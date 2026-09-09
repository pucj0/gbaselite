package mvcc

import (
	"context"
	"errors"
	bolt "go.etcd.io/bbolt"
	"testing"
)

func TestLocalStreamBatchIsBoundedUnpublishedAndHasNoPending(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seedLargeTest(t, s)
	update := largeUpdateTest(t, s)
	observed := false
	ctx := &afterInstalledContext{Context: context.Background(), store: s, action: func() {
		observed = true
		if err := s.db.View(func(tx *bolt.Tx) error {
			index := number(tx.Bucket(metaBucket).Get([]byte("allocated")))
			if tx.Bucket(commitsBucket).Get(sequence(index)) != nil {
				t.Fatal("published before all batches")
			}
			if k, _ := tx.Bucket(pendingBucket).Cursor().First(); k != nil {
				t.Fatal("standalone stream wrote persistent pending")
			}
			installed := 0
			err := tx.Bucket(dataBucket).ForEach(func(k, v []byte) error {
				if tx.Bucket(dataBucket).Bucket(k).Get(sequence(index)) != nil {
					installed++
				}
				return nil
			})
			if installed < 1 || installed >= 100 {
				t.Fatalf("first batch contains %d of 100 rows", installed)
			}
			if s.db.NoSync {
				t.Fatal("durability disabled")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}}
	if _, err = update.Commit(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if !observed {
		t.Fatal("no durable partial batch observed")
	}
	verifyLargeTest(t, s, false)
	if s.AvailabilityError() != nil {
		t.Fatal("cancellation poisoned store")
	}
}

type recordingLocalProposer struct {
	store *Store
	kinds []string
}

func (p *recordingLocalProposer) Barrier(ctx context.Context) error { return p.store.Barrier(ctx) }
func (p *recordingLocalProposer) Propose(ctx context.Context, c Command) (Result, error) {
	p.kinds = append(p.kinds, c.Kind)
	return p.store.Propose(ctx, c)
}
func TestExternalProposerRetainsReplayableStagePath(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seedLargeTest(t, s)
	update := largeUpdateTest(t, s)
	p := &recordingLocalProposer{store: s}
	update.proposer = p
	if _, err = update.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	stages := 0
	for _, kind := range p.kinds {
		if kind == "stage" {
			stages++
		}
	}
	if stages < 2 || p.kinds[len(p.kinds)-1] != "commit" {
		t.Fatal("proposer bypassed", p.kinds)
	}
	verifyLargeTest(t, s, true)
}
