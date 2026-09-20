package mvcc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

// Conflict matrix: one fixture per case, published through every physical commit
// path, so a conflict decision can never depend on which path was taken.
//
// Every case states both dependency directions:
//
//	leftConflicts  left must fail when right committed first
//	rightConflicts right must fail when left committed first
//
// Both orders are always run. A guard is deliberately one-way: its holder becomes
// sensitive to later changes, but a guard installs no version, so the guarded
// writer does not become sensitive to the guard.
const conflictSpace = visibilitySpace

// conflictPadSpace holds streaming-path padding. It is a separate namespace with
// per-transaction keys, so padding can never create or hide a dependency of the
// case under test.
const conflictPadSpace = "__conflict_pad"

type conflictScript func(t *testing.T, tx *Tx)

func putConflict(key, value string) conflictScript {
	return func(t *testing.T, tx *Tx) {
		t.Helper()
		putVisible(t, tx, key, value)
	}
}

func deleteConflict(key string) conflictScript {
	return func(t *testing.T, tx *Tx) {
		t.Helper()
		deleteVisible(t, tx, key)
	}
}

func guardConflict(key string) conflictScript {
	return func(t *testing.T, tx *Tx) {
		t.Helper()
		if err := tx.Guard(conflictSpace, []byte(key)); err != nil {
			t.Fatal(err)
		}
	}
}

func rangeGuardConflict(bounds KeyRange) conflictScript {
	return func(t *testing.T, tx *Tx) {
		t.Helper()
		if err := tx.GuardRange(conflictSpace, bounds); err != nil {
			t.Fatal(err)
		}
	}
}

// observeConflict is an ordinary read: it must never become a commit dependency.
func observeConflict(key string) conflictScript {
	return func(t *testing.T, tx *Tx) {
		t.Helper()
		if _, _, err := tx.Get(conflictSpace, []byte(key)); err != nil {
			t.Fatal(err)
		}
	}
}

func scripted(scripts ...conflictScript) conflictScript {
	return func(t *testing.T, tx *Tx) {
		t.Helper()
		for _, script := range scripts {
			script(t, tx)
		}
	}
}

// within builds an inclusive/exclusive byte range.
func within(lower, upper string, lowerInclusive, upperInclusive bool) KeyRange {
	return KeyRange{Lower: []byte(lower), Upper: []byte(upper), LowerInclusive: lowerInclusive, UpperInclusive: upperInclusive}
}

const conflictRangeName = "k"

type conflictCase struct {
	name           string
	seed           conflictScript
	left           conflictScript
	right          conflictScript
	leftConflicts  bool
	rightConflicts bool
}

var conflictCases = []conflictCase{
	// Write set: insert/update/delete combinations on one logical key.
	{
		name: "insert/insert", left: putConflict(conflictRangeName, "left"), right: putConflict(conflictRangeName, "right"),
		leftConflicts: true, rightConflicts: true,
	},
	{
		name: "update/update", seed: putConflict(conflictRangeName, "seed"),
		left: putConflict(conflictRangeName, "left"), right: putConflict(conflictRangeName, "right"),
		leftConflicts: true, rightConflicts: true,
	},
	{
		name: "update/delete", seed: putConflict(conflictRangeName, "seed"),
		left: putConflict(conflictRangeName, "left"), right: deleteConflict(conflictRangeName),
		leftConflicts: true, rightConflicts: true,
	},
	{
		name: "delete/update", seed: putConflict(conflictRangeName, "seed"),
		left: deleteConflict(conflictRangeName), right: putConflict(conflictRangeName, "right"),
		leftConflicts: true, rightConflicts: true,
	},
	{
		name: "insert/delete", left: putConflict(conflictRangeName, "left"), right: deleteConflict(conflictRangeName),
		leftConflicts: true, rightConflicts: true,
	},
	{
		name: "delete/reinsert", seed: putConflict(conflictRangeName, "seed"),
		left: deleteConflict(conflictRangeName), right: putConflict(conflictRangeName, "again"),
		leftConflicts: true, rightConflicts: true,
	},
	{
		name: "delete/delete", seed: putConflict(conflictRangeName, "seed"),
		left: deleteConflict(conflictRangeName), right: deleteConflict(conflictRangeName),
		leftConflicts: true, rightConflicts: true,
	},
	// Disjoint keys from the same snapshot both commit.
	{
		name: "different-keys", seed: scripted(putConflict("a", "1"), putConflict("b", "2")),
		left: putConflict("a", "left"), right: putConflict("b", "right"),
	},
	{
		name: "delete-and-unrelated-insert", seed: putConflict("a", "1"),
		left: deleteConflict("a"), right: putConflict("b", "2"),
	},
	// Point guards.
	{
		name: "point-guard vs update", seed: putConflict("a", "1"),
		left: guardConflict("a"), right: putConflict("a", "right"),
		leftConflicts: true,
	},
	{
		name: "point-guard vs insert", left: guardConflict("new"), right: putConflict("new", "right"),
		leftConflicts: true,
	},
	{
		name: "point-guard vs delete", seed: putConflict("a", "1"),
		left: guardConflict("a"), right: deleteConflict("a"),
		leftConflicts: true,
	},
	{
		name: "point-guard vs unrelated", seed: putConflict("a", "1"),
		left: guardConflict("a"), right: putConflict("b", "2"),
	},
	{
		name: "point-guard vs other-namespace", left: guardConflict("a"), right: putConflict("pad-key", "v"),
	},
	// Bounded range guards: every mutation kind, both endpoints, both opennesses.
	{
		name: "range-guard vs insert inside", left: rangeGuardConflict(within("b", "d", true, true)),
		right: putConflict("c", "v"), leftConflicts: true,
	},
	{
		name: "range-guard vs update inside", seed: putConflict("c", "old"),
		left: rangeGuardConflict(within("b", "d", true, true)), right: putConflict("c", "new"),
		leftConflicts: true,
	},
	{
		name: "range-guard vs delete inside", seed: putConflict("c", "old"),
		left: rangeGuardConflict(within("b", "d", true, true)), right: deleteConflict("c"),
		leftConflicts: true,
	},
	{
		name: "range-guard vs insert below", left: rangeGuardConflict(within("b", "d", true, true)),
		right: putConflict("a", "v"),
	},
	{
		name: "range-guard vs insert above", left: rangeGuardConflict(within("b", "d", true, true)),
		right: putConflict("e", "v"),
	},
	{
		name: "range-guard lower inclusive", left: rangeGuardConflict(within("b", "d", true, true)),
		right: putConflict("b", "v"), leftConflicts: true,
	},
	{
		name: "range-guard lower exclusive", left: rangeGuardConflict(within("b", "d", false, true)),
		right: putConflict("b", "v"),
	},
	{
		name: "range-guard upper inclusive", left: rangeGuardConflict(within("b", "d", true, true)),
		right: putConflict("d", "v"), leftConflicts: true,
	},
	{
		name: "range-guard upper exclusive", left: rangeGuardConflict(within("b", "d", true, false)),
		right: putConflict("d", "v"),
	},
	{
		name: "range-guard single key", left: rangeGuardConflict(within("c", "c", true, true)),
		right: putConflict("c", "v"), leftConflicts: true,
	},
	{
		name: "range-guard single key exclusive upper", left: rangeGuardConflict(within("c", "c", true, false)),
		right: putConflict("c", "v"),
	},
	{
		name: "range-guard empty interval", left: rangeGuardConflict(within("d", "b", true, true)),
		right: putConflict("c", "v"),
	},
	{
		name: "range-guard unbounded lower", left: rangeGuardConflict(KeyRange{Upper: []byte("d"), UpperInclusive: true}),
		right: putConflict("a", "v"), leftConflicts: true,
	},
	{
		name: "range-guard unbounded upper", left: rangeGuardConflict(KeyRange{Lower: []byte("b"), LowerInclusive: true}),
		right: putConflict("z", "v"), leftConflicts: true,
	},
	{
		name: "range-guard whole space", left: rangeGuardConflict(KeyRange{}),
		right: putConflict("anything", "v"), leftConflicts: true,
	},
	{
		name: "range-guard whole space vs other namespace", left: rangeGuardConflict(KeyRange{}),
		right: func(t *testing.T, tx *Tx) {
			t.Helper()
			if err := tx.Put(conflictPadSpace, []byte("elsewhere"), []byte("v")); err != nil {
				t.Fatal(err)
			}
		},
	},
	// Write skew: the Snapshot Isolation boundary that must survive this change.
	{
		name:  "write skew without guards",
		seed:  scripted(putConflict("on-a", "on"), putConflict("on-b", "on")),
		left:  scripted(observeConflict("on-a"), observeConflict("on-b"), putConflict("on-a", "off")),
		right: scripted(observeConflict("on-a"), observeConflict("on-b"), putConflict("on-b", "off")),
	},
	{
		name:          "write skew with point guards",
		seed:          scripted(putConflict("on-a", "on"), putConflict("on-b", "on")),
		left:          scripted(observeConflict("on-a"), observeConflict("on-b"), guardConflict("on-b"), putConflict("on-a", "off")),
		right:         scripted(observeConflict("on-a"), observeConflict("on-b"), guardConflict("on-a"), putConflict("on-b", "off")),
		leftConflicts: true, rightConflicts: true,
	},
	{
		name:          "write skew with range guards",
		seed:          scripted(putConflict("on-a", "on"), putConflict("on-b", "on")),
		left:          scripted(observeConflict("on-a"), observeConflict("on-b"), rangeGuardConflict(KeyRange{}), putConflict("on-a", "off")),
		right:         scripted(observeConflict("on-a"), observeConflict("on-b"), rangeGuardConflict(KeyRange{}), putConflict("on-b", "off")),
		leftConflicts: true, rightConflicts: true,
	},
	// Ordinary reads are never dependencies.
	{
		name: "read only vs write", seed: putConflict("a", "1"),
		left: observeConflict("a"), right: putConflict("a", "2"),
	},
}

// conflictPath publishes one side of a case through one physical commit path.
type conflictPath struct {
	name     string
	localWAL bool
	// prepare captures one side before either side publishes, so both sides share
	// the same read timestamp. It returns the publish step.
	prepare func(t *testing.T, s *Store, id string, snapshot uint64, script conflictScript) func() error
}

func newConflictStore(t *testing.T, localWAL bool) *Store {
	t.Helper()
	s, err := OpenWithOptions(t.TempDir(), Options{LocalWAL: localWAL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// captureConflictOps encodes one scripted write set by running it on a scratch
// transaction, which is never published.
func captureConflictOps(t *testing.T, s *Store, script conflictScript) []Op {
	t.Helper()
	tx := beginBaseline(t, s)
	script(t, tx)
	var ops []Op
	if err := tx.WalkWrites(context.Background(), func(op Op) error {
		ops = append(ops, op)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return ops
}

func prepareBounded(t *testing.T, s *Store, id string, snapshot uint64, script conflictScript) func() error {
	ops := captureConflictOps(t, s, script)
	return func() error {
		_, err := s.commitLocal(context.Background(), s.generation.Load(), snapshot, id, ops)
		return err
	}
}

func prepareGroup(t *testing.T, s *Store, id string, snapshot uint64, script conflictScript) func() error {
	ops := captureConflictOps(t, s, script)
	request := &localCommitRequest{ctx: context.Background(), generation: s.generation.Load(), snapshot: snapshot, id: id, ops: ops, done: make(chan struct{})}
	return func() error {
		s.commitLocalGroup([]*localCommitRequest{request})
		return request.err
	}
}

// prepareStream pads the write set past MaxChunkBytes, which is the condition
// Tx.Commit uses to select the streaming path.
func prepareStream(t *testing.T, s *Store, id string, snapshot uint64, script conflictScript) func() error {
	t.Helper()
	ctx := context.Background()
	tx, err := s.Begin(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tx.Snapshot != snapshot {
		t.Fatalf("streaming path began at %d, want %d", tx.Snapshot, snapshot)
	}
	script(t, tx)
	padding := bytes.Repeat([]byte{0x5a}, 4096)
	for i := 0; i < 20; i++ {
		if err := tx.Put(conflictPadSpace, []byte(fmt.Sprintf("pad-%s-%02d", id, i)), padding); err != nil {
			t.Fatal(err)
		}
	}
	size := 0
	if err := tx.WalkWrites(ctx, func(op Op) error {
		size += len(op.Space) + len(op.Key) + len(op.Value) + 64
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if size <= MaxChunkBytes {
		t.Fatalf("write set is %d bytes; the streaming path needs more than %d", size, MaxChunkBytes)
	}
	return func() error {
		_, err := tx.Commit(ctx)
		return err
	}
}

// prepareReplicated stages and commits exactly as a replicated FSM does: through
// Store.Apply with an explicit log index.
func prepareReplicated(t *testing.T, s *Store, id string, snapshot uint64, script conflictScript) func() error {
	ops := captureConflictOps(t, s, script)
	return func() error {
		stageIndex := s.localSeq + 1
		if _, err := s.Apply(stageIndex, Command{Kind: "stage", ID: id, Snapshot: snapshot, Ops: ops}); err != nil {
			return err
		}
		commitIndex := s.localSeq + 1
		result, err := s.Apply(commitIndex, Command{Kind: "commit", ID: id, Snapshot: snapshot})
		if err != nil {
			return err
		}
		return result.Err()
	}
}

var conflictPaths = []conflictPath{
	{name: "local-bounded", prepare: prepareBounded},
	{name: "local-group", prepare: prepareGroup},
	{name: "local-streaming", prepare: prepareStream},
	{name: "local-wal", localWAL: true, prepare: prepareBounded},
	{name: "local-wal-streaming", localWAL: true, prepare: prepareStream},
	{name: "replicated-staged", prepare: prepareReplicated},
}

func seedConflictStore(t *testing.T, s *Store, seed conflictScript) uint64 {
	t.Helper()
	if seed != nil {
		tx := beginBaseline(t, s)
		seed(t, tx)
		if _, err := tx.Commit(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	return mustHead(t, s)
}

// runConflictPair publishes first, then second, from the same read timestamp.
func runConflictPair(t *testing.T, path conflictPath, c conflictCase, first, second conflictScript, secondConflicts bool) {
	t.Helper()
	s := newConflictStore(t, path.localWAL)
	snapshot := seedConflictStore(t, s, c.seed)

	firstPublish := path.prepare(t, s, "first", snapshot, first)
	secondPublish := path.prepare(t, s, "second", snapshot, second)

	if err := firstPublish(); err != nil {
		t.Fatalf("%s: first side failed: %v", c.name, err)
	}
	if head := mustHead(t, s); head <= snapshot {
		t.Fatalf("%s: first side published head %d, want more than %d", c.name, head, snapshot)
	}
	published := mustHead(t, s)

	err := secondPublish()
	if !secondConflicts {
		if err != nil {
			t.Fatalf("%s: second side error = %v, want success", c.name, err)
		}
		if head := mustHead(t, s); head <= published {
			t.Fatalf("%s: second side published head %d, want more than %d", c.name, head, published)
		}
		return
	}
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("%s: second side error = %v, want ErrConflict", c.name, err)
	}
	// A rejected transaction must publish nothing at all.
	if head := mustHead(t, s); head != published {
		t.Fatalf("%s: conflicted commit moved head %d -> %d", c.name, published, head)
	}
	if err := s.AvailabilityError(); err != nil {
		t.Fatalf("%s: conflict poisoned the store: %v", c.name, err)
	}
}

func TestConflictMatrixAcrossCommitPaths(t *testing.T) {
	for _, path := range conflictPaths {
		t.Run(path.name, func(t *testing.T) {
			for _, c := range conflictCases {
				t.Run(c.name, func(t *testing.T) {
					t.Run("left-first", func(t *testing.T) {
						runConflictPair(t, path, c, c.left, c.right, c.rightConflicts)
					})
					t.Run("right-first", func(t *testing.T) {
						runConflictPair(t, path, c, c.right, c.left, c.leftConflicts)
					})
				})
			}
		})
	}
}

// groupCase publishes several dependency sets inside one physical commit group.
type groupCase struct {
	name         string
	scripts      []conflictScript
	wantConflict []bool
}

var groupConflictCases = []groupCase{
	{
		name: "same key twice", scripts: []conflictScript{putConflict(conflictRangeName, "1"), putConflict(conflictRangeName, "2")},
		wantConflict: []bool{false, true},
	},
	{
		name: "different keys", scripts: []conflictScript{putConflict("a", "1"), putConflict("b", "2")},
		wantConflict: []bool{false, false},
	},
	{
		name: "delete then write same key", scripts: []conflictScript{deleteConflict(conflictRangeName), putConflict(conflictRangeName, "r")},
		wantConflict: []bool{false, true},
	},
	{
		name: "write then point guard same key", scripts: []conflictScript{putConflict(conflictRangeName, "l"), guardConflict(conflictRangeName)},
		wantConflict: []bool{false, true},
	},
	{
		name: "point guard then write same key", scripts: []conflictScript{guardConflict(conflictRangeName), putConflict(conflictRangeName, "r")},
		wantConflict: []bool{false, false},
	},
	{
		name: "write then range guard inside", scripts: []conflictScript{putConflict("c", "l"), rangeGuardConflict(within("b", "d", true, true))},
		wantConflict: []bool{false, true},
	},
	{
		name: "range guard inside then write", scripts: []conflictScript{rangeGuardConflict(within("b", "d", true, true)), putConflict("c", "r")},
		wantConflict: []bool{false, false},
	},
	{
		name: "write then range guard outside", scripts: []conflictScript{putConflict("z", "l"), rangeGuardConflict(within("b", "d", true, true))},
		wantConflict: []bool{false, false},
	},
	{
		name: "three requests", scripts: []conflictScript{putConflict("c", "1"), rangeGuardConflict(within("b", "d", true, true)), putConflict("c", "2")},
		wantConflict: []bool{false, true, true},
	},
	{
		name: "accepted write then whole-space guard", scripts: []conflictScript{putConflict("anything", "1"), rangeGuardConflict(KeyRange{})},
		wantConflict: []bool{false, true},
	},
	{
		name: "accepted write in another namespace then whole-space guard",
		scripts: []conflictScript{
			func(t *testing.T, tx *Tx) {
				t.Helper()
				if err := tx.Put(conflictPadSpace, []byte("elsewhere"), []byte("v")); err != nil {
					t.Fatal(err)
				}
			},
			rangeGuardConflict(KeyRange{}),
		},
		wantConflict: []bool{false, false},
	},
	{
		name: "write skew inside one group stays allowed",
		scripts: []conflictScript{
			scripted(observeConflict("a"), putConflict("a", "off")),
			scripted(observeConflict("b"), putConflict("b", "off")),
		},
		wantConflict: []bool{false, false},
	},
}

// TestConflictMatrixWithinOneCommitGroup pins the group's own serialization order
// in both durability modes. Changes accepted earlier in a group are visible to the
// requests that follow them, and a request that arrives before a change does not
// see it -- the same order the single-transaction install path has.
func TestConflictMatrixWithinOneCommitGroup(t *testing.T) {
	for _, localWAL := range []bool{false, true} {
		name := "group"
		if localWAL {
			name = "wal-group"
		}
		t.Run(name, func(t *testing.T) {
			for _, c := range groupConflictCases {
				t.Run(c.name, func(t *testing.T) {
					s := newConflictStore(t, localWAL)
					snapshot := mustHead(t, s)
					requests := make([]*localCommitRequest, 0, len(c.scripts))
					for i, script := range c.scripts {
						requests = append(requests, &localCommitRequest{
							ctx:        context.Background(),
							generation: s.generation.Load(),
							snapshot:   snapshot,
							id:         fmt.Sprintf("request-%d", i),
							ops:        captureConflictOps(t, s, script),
							done:       make(chan struct{}),
						})
					}
					s.commitLocalGroup(requests)
					for i, request := range requests {
						if c.wantConflict[i] && !errors.Is(request.err, ErrConflict) {
							t.Fatalf("request %d error = %v, want ErrConflict", i, request.err)
						}
						if !c.wantConflict[i] && request.err != nil {
							t.Fatalf("request %d error = %v, want success", i, request.err)
						}
					}
					if err := s.AvailabilityError(); err != nil {
						t.Fatalf("conflict poisoned the store: %v", err)
					}
				})
			}
		})
	}
}

// TestConflictWriteSkewKeepsSnapshotIsolation is the isolation boundary in its
// most direct form: two transactions read the same rows and write disjoint keys.
// Without a declared dependency both commit -- that is Snapshot Isolation, not a
// bug -- and declaring a Guard/GuardRange turns the same workload into a conflict.
func TestConflictWriteSkewKeepsSnapshotIsolation(t *testing.T) {
	ctx := context.Background()
	for _, localWAL := range []bool{false, true} {
		name := "bounded"
		if localWAL {
			name = "local-wal"
		}
		t.Run(name, func(t *testing.T) {
			seed := func(t *testing.T, s *Store) {
				t.Helper()
				tx := beginBaseline(t, s)
				putVisible(t, tx, "A", "on")
				putVisible(t, tx, "B", "on")
				mustCommit(t, tx)
			}
			open := func(t *testing.T, s *Store, left, right conflictScript) (*Tx, *Tx) {
				t.Helper()
				first, second := beginBaseline(t, s), beginBaseline(t, s)
				if first.Snapshot != second.Snapshot {
					t.Fatalf("skew transactions began at %d and %d", first.Snapshot, second.Snapshot)
				}
				left(t, first)
				right(t, second)
				return first, second
			}

			t.Run("without guards both commit", func(t *testing.T) {
				s := newConflictStore(t, localWAL)
				seed(t, s)
				first, second := open(t, s, putConflict("A", "off"), putConflict("B", "off"))
				if _, err := first.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				if _, err := second.Commit(ctx); err != nil {
					t.Fatalf("disjoint write skew must commit under Snapshot Isolation: %v", err)
				}
			})

			t.Run("with point guards the second committer conflicts", func(t *testing.T) {
				s := newConflictStore(t, localWAL)
				seed(t, s)
				first, second := open(t, s, scripted(guardConflict("B"), putConflict("A", "off")), scripted(guardConflict("A"), putConflict("B", "off")))
				if _, err := first.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				if _, err := second.Commit(ctx); !errors.Is(err, ErrConflict) {
					t.Fatalf("declared dependency error = %v, want ErrConflict", err)
				}
			})

			t.Run("with range guards the second committer conflicts", func(t *testing.T) {
				s := newConflictStore(t, localWAL)
				seed(t, s)
				first, second := open(t, s, scripted(rangeGuardConflict(KeyRange{}), putConflict("A", "off")), scripted(rangeGuardConflict(KeyRange{}), putConflict("B", "off")))
				if _, err := first.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				if _, err := second.Commit(ctx); !errors.Is(err, ErrConflict) {
					t.Fatalf("declared range dependency error = %v, want ErrConflict", err)
				}
			})

			t.Run("ordinary reads never conflict", func(t *testing.T) {
				s := newConflictStore(t, localWAL)
				seed(t, s)
				reader, writer := beginBaseline(t, s), beginBaseline(t, s)
				if value := conflictRead(t, reader, "A"); value != "on" {
					t.Fatalf("read A = %q, want on", value)
				}
				if err := reader.Scan(ctx, conflictSpace, func([]byte, []byte) error { return nil }); err != nil {
					t.Fatal(err)
				}
				putVisible(t, writer, "A", "changed")
				if _, err := writer.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				if _, err := reader.Commit(ctx); err != nil {
					t.Fatalf("a transaction that only read must still commit: %v", err)
				}
			})
		})
	}
}

func conflictRead(t *testing.T, tx *Tx, key string) string {
	t.Helper()
	value, _, err := tx.Get(conflictSpace, []byte(key))
	if err != nil {
		t.Fatal(err)
	}
	return string(value)
}

// TestGroupChangeLookupLayersAcceptedChanges covers the one piece of the unified
// rule that is not a plain database lookup, in isolation from any commit path.
func TestGroupChangeLookupLayersAcceptedChanges(t *testing.T) {
	const baseAnswer = 3
	base := func([]byte) uint64 { return baseAnswer }
	accepted := map[string]uint64{}
	lookup := newGroupChangeLookup(base, accepted)

	exact, err := key(conflictSpace, []byte("k"))
	if err != nil {
		t.Fatal(err)
	}
	if got := lookup(exact); got != baseAnswer {
		t.Fatalf("base answer = %d, want %d", got, baseAnswer)
	}
	accepted[string(exact)] = 7
	if got := lookup(exact); got != 7 {
		t.Fatalf("exact-key overlay = %d, want 7", got)
	}
	accepted[string(exact)] = 1
	if got := lookup(exact); got != baseAnswer {
		t.Fatalf("overlay lowered the base answer to %d", got)
	}

	bounds := within("b", "d", true, true)
	payload, err := json.Marshal(rangeDependency{Space: conflictSpace, Bounds: bounds})
	if err != nil {
		t.Fatal(err)
	}
	guard, err := key(rangeGuardSpace, append([]byte{0, 1}, payload...))
	if err != nil {
		t.Fatal(err)
	}
	if got := lookup(guard); got != baseAnswer {
		t.Fatalf("range guard base = %d, want %d", got, baseAnswer)
	}
	inside, err := key(conflictSpace, []byte("c"))
	if err != nil {
		t.Fatal(err)
	}
	accepted[string(inside)] = 9
	if got := lookup(guard); got != 9 {
		t.Fatalf("range guard missed an accepted change inside its bounds: %d", got)
	}
	outside, err := key(conflictSpace, []byte("z"))
	if err != nil {
		t.Fatal(err)
	}
	accepted[string(outside)] = 11
	if got := lookup(guard); got != 9 {
		t.Fatalf("range guard counted an accepted change outside its bounds: %d", got)
	}
	elsewhere, err := key("other", []byte("c"))
	if err != nil {
		t.Fatal(err)
	}
	accepted[string(elsewhere)] = 12
	if got := lookup(guard); got != 9 {
		t.Fatalf("range guard counted an accepted change in another namespace: %d", got)
	}

	whole, err := key(rangeGuardSpace, []byte(conflictSpace))
	if err != nil {
		t.Fatal(err)
	}
	if got := lookup(whole); got != 11 {
		t.Fatalf("whole-space guard = %d, want 11 (newest accepted change in the space)", got)
	}

	point, err := key(conflictSpace, []byte("p"))
	if err != nil {
		t.Fatal(err)
	}
	if got := lookup(point); got != baseAnswer {
		t.Fatalf("point dependency = %d, want %d", got, baseAnswer)
	}
	accepted[string(point)] = 13
	if got := lookup(point); got != 13 {
		t.Fatalf("point dependency missed an accepted change: %d", got)
	}
}

// TestConflictValidatorRule checks the rule itself, independent of any store.
func TestConflictValidatorRule(t *testing.T) {
	latest := uint64(20)
	validator := newConflictValidator(20, func([]byte) uint64 { return latest })
	key := []byte("k")
	if err := validator.validateKey(key); err != nil {
		t.Fatalf("equal version must not conflict: %v", err)
	}
	latest = 21
	if err := validator.validateKey(key); !errors.Is(err, ErrConflict) {
		t.Fatalf("newer version error = %v, want ErrConflict", err)
	}
	latest = 19
	if err := validator.validateKey(key); err != nil {
		t.Fatalf("older version must not conflict: %v", err)
	}
	// Read timestamp 0 is a legal snapshot: any committed change is newer.
	zero := newConflictValidator(0, func([]byte) uint64 { return 1 })
	if err := zero.validateKey(key); !errors.Is(err, ErrConflict) {
		t.Fatalf("snapshot 0 error = %v, want ErrConflict", err)
	}
	none := newConflictValidator(0, func([]byte) uint64 { return 0 })
	if err := none.validateKey(key); err != nil {
		t.Fatalf("empty store at snapshot 0 must not conflict: %v", err)
	}
	// validateOps stops at the first conflicting dependency and reports it.
	order := 0
	sequence := newConflictValidator(5, func([]byte) uint64 {
		order++
		if order == 2 {
			return 9
		}
		return 1
	})
	ops := []Op{{Space: conflictSpace, Key: []byte("a")}, {Space: conflictSpace, Key: []byte("b")}, {Space: conflictSpace, Key: []byte("c")}}
	if err := sequence.validateOps(ops); !errors.Is(err, ErrConflict) {
		t.Fatalf("validateOps error = %v, want ErrConflict", err)
	}
	if order != 2 {
		t.Fatalf("validateOps checked %d dependencies, want it to stop at 2", order)
	}
}
