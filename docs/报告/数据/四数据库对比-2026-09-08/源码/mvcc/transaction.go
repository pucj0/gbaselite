package mvcc

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	bolt "go.etcd.io/bbolt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// ErrWriteSetLimit reports an explicitly configured logical write-set budget.
var ErrWriteSetLimit = errors.New("MVCC transaction write set exceeds configured budget")

type Tx struct {
	generation  uint64
	term        uint64
	store       *Store
	proposer    Proposer
	parent      *Tx
	Snapshot    uint64
	ID          string
	stage       *bolt.DB
	path        string
	closed      bool
	stagedBytes int64
	buffered    map[string][]byte
	bufferBytes int64
}

func randomID() string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(id[:])
}
func (s *Store) Begin(ctx context.Context, p Proposer) (*Tx, error) {
	if p == nil {
		p = s
	}
	if err := p.Barrier(ctx); err != nil {
		return nil, err
	}
	s.views.Lock()
	defer s.views.Unlock()
	snapshot, err := s.Head()
	if err != nil {
		return nil, err
	}
	tx, err := s.newTx(p, snapshot, nil)
	if err == nil {
		s.active[snapshot]++
	}
	return tx, err
}
func (s *Store) newTx(p Proposer, snapshot uint64, parent *Tx) (*Tx, error) {
	id := randomID()
	path := filepath.Join(filepath.Dir(s.path), "transactions", id+".tmp")
	var term uint64
	if source, ok := p.(interface{ Term() uint64 }); ok {
		term = source.Term()
	}
	return &Tx{generation: s.generation.Load(), term: term, store: s, proposer: p, parent: parent, Snapshot: snapshot, ID: id, path: path}, nil
}
func (t *Tx) ensureStage() error {
	if t.stage != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(t.path), 0700); err != nil {
		return err
	}
	db, err := bolt.Open(t.path, 0600, &bolt.Options{Timeout: time.Second, NoSync: true, NoFreelistSync: true})
	if err != nil {
		return err
	}
	if err = db.Update(func(tx *bolt.Tx) error { _, err := tx.CreateBucket([]byte("writes")); return err }); err != nil {
		db.Close()
		os.Remove(t.path)
		return err
	}
	t.stage = db
	return nil
}
func (t *Tx) Child() (*Tx, error) {
	if t.closed || t.generation != t.store.generation.Load() {
		return nil, ErrClosed
	}
	return t.store.newTx(t.proposer, t.Snapshot, t)
}
func (t *Tx) readOp(space string, rowKey []byte) (Op, bool, error) {
	if t.stage == nil && len(t.buffered) == 0 {
		return Op{}, false, validateKey(space, rowKey)
	}
	k, err := key(space, rowKey)
	if err != nil {
		return Op{}, false, err
	}
	v, err := t.stagedValue(k)
	if err != nil || v == nil {
		return Op{}, false, err
	}
	op, err := decodeStagedOp(k, v)
	return op, true, err
}
func (t *Tx) Get(space string, rowKey []byte) ([]byte, bool, error) {
	if t.closed || t.generation != t.store.generation.Load() {
		return nil, false, ErrClosed
	}
	op, found, err := t.readOp(space, rowKey)
	if err != nil {
		return nil, false, err
	}
	if found && !op.Check {
		return op.Value, !op.Delete, nil
	}
	if t.parent != nil {
		return t.parent.Get(space, rowKey)
	}
	v, _, ok, err := t.store.Get(t.Snapshot, space, rowKey)
	if t.generation != t.store.generation.Load() {
		return nil, false, ErrConflict
	}
	return v, ok, err
}
func (t *Tx) Put(space string, rowKey, value []byte) error {
	return t.write(Op{Space: space, Key: rowKey, Value: value})
}
func (t *Tx) Delete(space string, rowKey []byte) error {
	return t.write(Op{Space: space, Key: rowKey, Delete: true})
}
func (t *Tx) write(op Op) error {
	if t.closed || t.generation != t.store.generation.Load() {
		return ErrClosed
	}
	if err := validateCommand(Command{Ops: []Op{op}}); err != nil {
		return err
	}
	k, _ := key(op.Space, op.Key)
	return t.bufferWrite(op, k, encodeStagedOp(op))
}
func (t *Tx) Scan(ctx context.Context, space string, yield func([]byte, []byte) error) (scanErr error) {
	defer func() {
		if t.generation != t.store.generation.Load() {
			scanErr = ErrConflict
		}
	}()
	if t.closed || t.generation != t.store.generation.Load() {
		return ErrClosed
	}
	if err := t.flushWrites(); err != nil {
		return err
	}
	visit := func(k, v []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Most scan layers have no writes. Avoid building a namespace key for
		// every row; recheck each callback so writes made by a consumer still apply.
		if t.stage == nil && len(t.buffered) == 0 {
			return yield(k, v)
		}
		op, found, err := t.readOp(space, k)
		if err != nil {
			return err
		}
		if found && !op.Check {
			if op.Delete || op.Check {
				return nil
			}
			v = op.Value
		}
		return yield(k, v)
	}
	var err error
	if t.parent != nil {
		err = t.parent.Scan(ctx, space, visit)
	} else {
		err = t.store.Scan(ctx, t.Snapshot, space, visit)
	}
	if err != nil {
		return err
	}
	if t.stage == nil {
		return nil
	}
	prefix, _ := key(space, nil)
	return t.stage.View(func(tx *bolt.Tx) error {
		c := tx.Bucket([]byte("writes")).Cursor()
		for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			op, err := decodeStagedOp(k, v)
			if err != nil {
				return err
			}
			if op.Delete || op.Check {
				continue
			}
			var exists bool
			if t.parent != nil {
				_, exists, err = t.parent.Get(space, op.Key)
			} else {
				_, _, exists, err = t.store.Get(t.Snapshot, space, op.Key)
			}
			if err != nil {
				return err
			}
			if !exists {
				if err := yield(bytes.Clone(op.Key), op.Value); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
func (t *Tx) WalkWrites(ctx context.Context, yield func(Op) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if t.stage == nil {
		keys := make([]string, 0, len(t.buffered))
		for k := range t.buffered {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if err := ctx.Err(); err != nil {
				return err
			}
			op, err := decodeStagedOp([]byte(k), t.buffered[k])
			if err != nil {
				return err
			}
			if err := yield(op); err != nil {
				return err
			}
		}
		return nil
	}
	if err := t.flushWrites(); err != nil {
		return err
	}
	return t.stage.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("writes")).ForEach(func(k, v []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			op, err := decodeStagedOp(k, v)
			if err != nil {
				return err
			}
			return yield(op)
		})
	})
}
func (t *Tx) Commit(ctx context.Context) (uint64, error) {
	if t.generation != t.store.generation.Load() {
		t.Rollback()
		return 0, ErrConflict
	}
	if t.closed {
		return 0, ErrClosed
	}
	if t.parent != nil {
		err := t.mergeParent(ctx)
		t.Rollback()
		return t.Snapshot, err
	}
	defer t.Rollback()
	if sequence, handled, err := t.tryLocalCommit(ctx); handled {
		return sequence, err
	}
	completed := false
	defer func() {
		if !completed {
			cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, _ = t.proposer.Propose(cleanup, Command{Kind: "abort", ID: t.ID})
		}
	}()
	var batch []Op
	size := 0
	count := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		r, err := t.proposer.Propose(ctx, Command{Term: t.term, Kind: "stage", ID: t.ID, Snapshot: t.Snapshot, Ops: batch})
		if err == nil {
			err = r.Err()
		}
		batch = nil
		size = 0
		return err
	}
	err := t.WalkWrites(ctx, func(op Op) error {
		n := len(op.Space) + len(op.Key) + len(op.Value) + 64
		if size+n > MaxChunkBytes {
			if err := flush(); err != nil {
				return err
			}
		}
		batch = append(batch, op)
		size += n
		count++
		return nil
	})
	if err == nil {
		err = flush()
	}
	if err != nil {
		return 0, err
	}
	if count == 0 {
		completed = true
		return t.Snapshot, nil
	}
	result, err := t.proposer.Propose(ctx, Command{Term: t.term, Kind: "commit", ID: t.ID, Snapshot: t.Snapshot})
	if err != nil {
		return 0, err
	}
	completed = result.Err() == nil
	return result.Sequence, result.Err()
}
func (t *Tx) Rollback() error {
	if t.closed {
		return nil
	}
	t.closed = true
	t.buffered = nil
	t.bufferBytes = 0
	var err, removeErr error
	if t.stage != nil {
		err = t.stage.Close()
		removeErr = os.Remove(t.path)
	}
	if t.parent == nil {
		t.store.views.Lock()
		if t.generation == t.store.generation.Load() {
			t.store.active[t.Snapshot]--
			if t.store.active[t.Snapshot] == 0 {
				delete(t.store.active, t.Snapshot)
			}
		}
		t.store.views.Unlock()
	}
	return errors.Join(err, removeErr)
}

func (t *Tx) Guard(space string, rowKey []byte) error {
	return t.write(Op{Space: space, Key: rowKey, Check: true})
}
