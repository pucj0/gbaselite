package mvcc

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"gbaselite/storageengine"
	bolt "go.etcd.io/bbolt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const MaxChunkBytes = 64 << 10
const MaxValueBytes = storageengine.MaxValueBytes

var ErrConflict = storageengine.ErrConflict
var ErrClosed = storageengine.ErrClosed
var dataBucket = []byte("versions")
var metaBucket = []byte("meta")
var pendingBucket = []byte("pending")
var commitsBucket = []byte("commits")
var countersBucket = []byte("counters")

type Op struct {
	Space  string
	Key    []byte
	Value  []byte
	Delete bool
	Check  bool
}
type Command struct {
	Term     uint64
	Kind     string
	ID       string
	Snapshot uint64
	Ops      []Op
	Counter  string
	Floor    uint64
	Count    uint64
}
type Result struct {
	Sequence uint64
	Number   uint64
	Error    string
}

func (r Result) Err() error {
	if r.Error == ErrConflict.Error() {
		return ErrConflict
	}
	if r.Error != "" {
		return errors.New(r.Error)
	}
	return nil
}

type Proposer interface {
	Propose(context.Context, Command) (Result, error)
	Barrier(context.Context) error
}
type Store struct {
	failure       sync.Mutex
	fatal         error
	generation    atomic.Uint64
	gate          sync.RWMutex
	db            *bolt.DB
	path          string
	groupMu       sync.Mutex
	groupRunning  bool
	groupQueue    []*localCommitRequest
	groupBytes    int
	apply         sync.Mutex
	views         sync.Mutex
	active        map[uint64]int
	localSeq      uint64
	localWAL      bool
	writeSetLimit int64
}

type Options struct {
	LocalWAL bool
	// WriteSetLimitBytes limits logical staged key/encoded-value bytes. Zero
	// leaves total transaction size uncapped; the memory buffer stays bounded.
	WriteSetLimitBytes int64
}

func Open(directory string) (*Store, error) { return OpenWithOptions(directory, Options{}) }
func OpenWithOptions(directory string, options Options) (*Store, error) {
	if options.WriteSetLimitBytes < 0 {
		return nil, errors.New("negative MVCC write-set budget")
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(directory, "migration.incomplete")); err == nil {
		return nil, errors.New("incomplete layout migration; use the original source or a verified destination")
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(directory, "backup.incomplete")); err == nil {
		return nil, errors.New("incomplete MVCC backup")
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	path := filepath.Join(directory, "mvcc.db")
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: time.Second, NoFreelistSync: true})
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, path: path, active: make(map[uint64]int), writeSetLimit: options.WriteSetLimitBytes, localWAL: options.LocalWAL}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{dataBucket, metaBucket, pendingBucket, commitsBucket, countersBucket} {
			if _, e := tx.CreateBucketIfNotExists(name); e != nil {
				return e
			}
		}
		if err := validateVersionLayout(tx); err != nil {
			return err
		}
		if options.LocalWAL && tx.Bucket(metaBucket).Get([]byte("wal_identity")) == nil {
			if err := tx.Bucket(metaBucket).Put([]byte("wal_identity"), []byte(randomID())); err != nil {
				return err
			}
		}
		s.localSeq = max(number(tx.Bucket(metaBucket).Get([]byte("applied"))), number(tx.Bucket(metaBucket).Get([]byte("allocated"))))
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	if err = s.recoverLocalWAL(); err != nil {
		db.Close()
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(directory, "transactions"))
	if err != nil && !os.IsNotExist(err) {
		db.Close()
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".tmp") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".tmp")
		if raw, err := hex.DecodeString(id); err == nil && len(raw) == 16 {
			if err = os.Remove(filepath.Join(directory, "transactions", entry.Name())); err != nil {
				db.Close()
				return nil, err
			}
		}
	}
	return s, nil
}
func (s *Store) Close() error {
	s.apply.Lock()
	defer s.apply.Unlock()
	s.gate.Lock()
	defer s.gate.Unlock()
	return s.db.Close()
}
func (s *Store) Head() (uint64, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	var n uint64
	err := s.db.View(func(tx *bolt.Tx) error { n = number(tx.Bucket(metaBucket).Get([]byte("head"))); return nil })
	return n, err
}
func number(value []byte) uint64 {
	if len(value) != 8 {
		return 0
	}
	return binary.BigEndian.Uint64(value)
}
func sequence(n uint64) []byte {
	var value [8]byte
	binary.BigEndian.PutUint64(value[:], n)
	return value[:]
}
func validateKey(space string, rowKey []byte) error {
	keyLimit := 8192
	if space == rangeGuardSpace {
		keyLimit = 32700
	} // Encodes two valid endpoints; check-only, never a data row.
	if space == "" || bytes.IndexByte([]byte(space), 0) >= 0 || len(space) > 1024 || len(rowKey) > keyLimit {
		return errors.New("invalid MVCC namespace/key")
	}
	return nil
}
func key(space string, rowKey []byte) ([]byte, error) {
	if err := validateKey(space, rowKey); err != nil {
		return nil, err
	}
	out := make([]byte, len(space)+1+len(rowKey))
	copy(out, space)
	copy(out[len(space)+1:], rowKey)
	return out, nil
}

// visibilityReader is scoped to one bbolt read/write transaction. Its small
// direct-mapped cache must never survive that transaction: publication markers
// can change between views, even for the same logical snapshot.
type visibilityReader struct {
	tx            *bolt.Tx
	flat          bool
	data, commits *bolt.Bucket
	snapshot      uint64
	seek          [8]byte
	cache         [16]struct {
		version          uint64
		known, committed bool
	}
}

func newVisibilityReader(tx *bolt.Tx, snapshot uint64) visibilityReader {
	r := visibilityReader{tx: tx, flat: flatLayout(tx), data: tx.Bucket(dataBucket), commits: tx.Bucket(commitsBucket), snapshot: snapshot}
	copy(r.seek[:], sequence(snapshot))
	return r
}
func (r *visibilityReader) visible(k []byte) ([]byte, uint64, bool) {
	if bytes.HasPrefix(k, rangeGuardPrefix) {
		return nil, r.rangeVersion(k), false
	}
	if r.flat {
		return flatVisible(r.tx, k, r.snapshot)
	}
	rows := r.data.Bucket(k)
	if rows == nil {
		return nil, 0, false
	}
	cursor := rows.Cursor()
	v, p := cursor.Seek(r.seek[:])
	if v == nil {
		v, p = cursor.Last()
	} else if number(v) > r.snapshot {
		v, p = cursor.Prev()
	}
	for v != nil {
		version := number(v)
		entry := &r.cache[version%uint64(len(r.cache))]
		if !entry.known || entry.version != version {
			entry.version = version
			entry.known = true
			entry.committed = r.commits.Get(v) != nil
		}
		if entry.committed {
			if len(p) == 0 || p[0] == 0 {
				return nil, version, false
			}
			return p[1:], version, true
		}
		v, p = cursor.Prev()
	}
	return nil, 0, false
}

// Point reads do not allocate a scan cache.
func visible(tx *bolt.Tx, k []byte, snapshot uint64) ([]byte, uint64, bool) {
	if bytes.HasPrefix(k, rangeGuardPrefix) {
		r := newVisibilityReader(tx, snapshot)
		return nil, r.rangeVersion(k), false
	}
	if flatLayout(tx) {
		return flatVisible(tx, k, snapshot)
	}
	rows := tx.Bucket(dataBucket).Bucket(k)
	if rows == nil {
		return nil, 0, false
	}
	cursor := rows.Cursor()
	v, p := cursor.Seek(sequence(snapshot))
	if v == nil {
		v, p = cursor.Last()
	} else if number(v) > snapshot {
		v, p = cursor.Prev()
	}
	for v != nil {
		version := number(v)
		if tx.Bucket(commitsBucket).Get(v) != nil {
			if len(p) == 0 || p[0] == 0 {
				return nil, version, false
			}
			return p[1:], version, true
		}
		v, p = cursor.Prev()
	}
	return nil, 0, false
}
func (s *Store) Get(snapshot uint64, space string, rowKey []byte) ([]byte, uint64, bool, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	k, err := key(space, rowKey)
	if err != nil {
		return nil, 0, false, err
	}
	var value []byte
	var version uint64
	var ok bool
	err = s.db.View(func(tx *bolt.Tx) error {
		v, n, exists := visible(tx, k, snapshot)
		value = bytes.Clone(v)
		version, ok = n, exists
		return nil
	})
	return value, version, ok, err
}
func (s *Store) Scan(ctx context.Context, snapshot uint64, space string, yield func([]byte, []byte) error) error {
	prefix, err := key(space, nil)
	if err != nil {
		return err
	}
	var after []byte
	// Release the bbolt read transaction between bounded pages. A slow consumer
	// must not prevent the writer from growing its file mapping indefinitely.
	for {
		var keys, values [][]byte
		done := false
		s.gate.RLock()
		err = s.db.View(func(tx *bolt.Tx) error {
			reader := newVisibilityReader(tx, snapshot)
			c := reader.data.Cursor()
			k, _ := c.Seek(prefix)
			if after != nil {
				k, _ = c.Seek(after)
				if bytes.Equal(k, after) {
					k, _ = c.Next()
				}
			}
			size := 0
			examined := 0
			for ; k != nil && bytes.HasPrefix(k, prefix); k, _ = c.Next() {
				if err := ctx.Err(); err != nil {
					return err
				}
				v, _, ok := reader.visible(k)
				examined++
				if ok {
					keys = append(keys, bytes.Clone(k[len(prefix):]))
					values = append(values, bytes.Clone(v))
					size += len(k) + len(v)
				}
				if size >= MaxChunkBytes || examined >= 256 {
					after = bytes.Clone(k)
					return nil
				}
			}
			done = true
			return nil
		})
		s.gate.RUnlock()
		if err != nil {
			return err
		}
		for i, k := range keys {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := yield(k, values[i]); err != nil {
				return err
			}
		}
		if done {
			return nil
		}
	}
}
func validateCommand(command Command) error {
	if len(command.ID) > 128 {
		return errors.New("transaction ID too long")
	}
	size := 0
	for _, op := range command.Ops {
		if err := validateKey(op.Space, op.Key); err != nil {
			return err
		}
		if len(op.Value) > MaxValueBytes {
			return fmt.Errorf("MVCC value exceeds %d bytes", MaxValueBytes)
		}
		size += len(op.Space) + len(op.Key) + len(op.Value) + 64
	}
	if size > MaxChunkBytes {
		return errors.New("MVCC write chunk exceeds memory budget")
	}
	return nil
}
func (s *Store) Propose(ctx context.Context, command Command) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	s.apply.Lock()
	defer s.apply.Unlock()
	s.localSeq++
	if err := s.AvailabilityError(); err != nil {
		return Result{}, err
	}
	var result Result
	var err error
	if command.Kind == "commit" {
		err = validateCommand(command)
		if err == nil {
			result, err = s.commitContext(ctx, s.localSeq, command)
		}
	} else {
		result, err = s.applyCommand(s.localSeq, command)
	}
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		s.failure.Lock()
		s.fatal = err
		s.failure.Unlock()
	}
	return result, err
}
func (s *Store) Barrier(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.AvailabilityError()
}
func (s *Store) Apply(index uint64, command Command) (Result, error) {
	s.apply.Lock()
	defer s.apply.Unlock()
	if index > s.localSeq {
		s.localSeq = index
	}
	return s.applyCommand(index, command)
}
func (s *Store) applyCommand(index uint64, command Command) (Result, error) {
	if err := validateCommand(command); err != nil {
		return Result{}, err
	}
	var result Result
	if command.Kind == "abort" {
		if err := s.clearPending(command.ID); err != nil {
			return Result{}, err
		}
		return Result{}, s.db.Update(func(tx *bolt.Tx) error {
			b := tx.Bucket(metaBucket)
			if number(b.Get([]byte("applied"))) >= index {
				return nil
			}
			return b.Put([]byte("applied"), sequence(index))
		})
	}
	if command.Kind == "commit" {
		return s.commit(index, command)
	}
	err := s.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket(metaBucket)
		if number(meta.Get([]byte("applied"))) >= index {
			return nil
		}
		switch command.Kind {
		case "stage":
			if command.ID == "" {
				return errors.New("missing transaction ID")
			}
			if tx.Bucket(commitsBucket).Get([]byte(command.ID)) != nil {
				return nil
			}
			bucket, err := tx.Bucket(pendingBucket).CreateBucketIfNotExists([]byte(command.ID))
			if err != nil {
				return err
			}
			for _, op := range command.Ops {
				k, _ := key(op.Space, op.Key)
				v, err := json.Marshal(op)
				if err != nil {
					return err
				}
				if err := bucket.Put(k, v); err != nil {
					return err
				}
			}
		case "abort":
			if tx.Bucket(pendingBucket).Bucket([]byte(command.ID)) != nil {
				if err := tx.Bucket(pendingBucket).DeleteBucket([]byte(command.ID)); err != nil {
					return err
				}
			}
		case "advance":
			b := tx.Bucket(countersBucket)
			if command.Floor > uint64(^uint64(0)>>1) {
				result.Error = "auto increment exhausted"
				return meta.Put([]byte("applied"), sequence(index))
			}
			if number(b.Get([]byte(command.Counter))) < command.Floor {
				if err := b.Put([]byte(command.Counter), sequence(command.Floor)); err != nil {
					return err
				}
			}
		case "reserve":
			if command.Counter == "" || command.Count == 0 || command.Count > uint64(^uint64(0)>>1) {
				return errors.New("invalid counter reservation")
			}
			b := tx.Bucket(countersBucket)
			current := number(b.Get([]byte(command.Counter)))
			if current > uint64(^uint64(0)>>1)-command.Count {
				result.Error = "auto increment exhausted"
				return meta.Put([]byte("applied"), sequence(index))
			}
			result.Number = current + 1
			if err := b.Put([]byte(command.Counter), sequence(current+command.Count)); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown MVCC command %q", command.Kind)
		}
		return meta.Put([]byte("applied"), sequence(index))
	})
	return result, err
}
func (s *Store) commit(index uint64, command Command) (Result, error) {
	// Replicated application must be deterministic, independent of a client's
	// cancellation or a follower's local resource configuration.
	return s.commitContext(context.Background(), index, command)
}
func (s *Store) commitContext(ctx context.Context, index uint64, command Command) (Result, error) {
	var result Result
	// Validate every key before modifying visible state. Different rows from the
	// same snapshot may commit; a concurrent change to the same row must conflict.
	err := s.db.View(func(tx *bolt.Tx) error {
		if done := tx.Bucket(commitsBucket).Get([]byte(command.ID)); done != nil {
			result.Sequence = number(done)
			return nil
		}
		if number(tx.Bucket(metaBucket).Get([]byte("applied"))) >= index {
			return ErrConflict
		}
		pending := tx.Bucket(pendingBucket).Bucket([]byte(command.ID))
		if pending == nil {
			return errors.New("missing staged transaction")
		}
		reader := newVisibilityReader(tx, ^uint64(0))
		return pending.ForEach(func(k, v []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			_, version, _ := reader.visible(k)
			if version > command.Snapshot {
				return ErrConflict
			}
			return nil
		})
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return Result{}, err
		}
		failure := err
		if err := s.clearPending(command.ID); err != nil {
			return Result{}, err
		}
		err = s.db.Update(func(tx *bolt.Tx) error {
			b := tx.Bucket(metaBucket)
			if number(b.Get([]byte("applied"))) < index {
				return b.Put([]byte("applied"), sequence(index))
			}
			return nil
		})
		return Result{Error: failure.Error()}, err
	}
	if result.Sequence != 0 {
		return result, s.clearPending(command.ID)
	}
	// Apply bounded batches with an unpublished version. Until the final marker,
	// readers ignore every row of this transaction, including after a crash.
	var after []byte
	catalogChanged := false
	for {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		var batch []Op
		var keys [][]byte
		size := 0
		err = s.db.View(func(tx *bolt.Tx) error {
			b := tx.Bucket(pendingBucket).Bucket([]byte(command.ID))
			c := b.Cursor()
			k, v := c.First()
			if after != nil {
				k, v = c.Seek(after)
				if bytes.Equal(k, after) {
					k, v = c.Next()
				}
			}
			for k != nil {
				var op Op
				if err := json.Unmarshal(v, &op); err != nil {
					return err
				}
				batch = append(batch, op)
				keys = append(keys, bytes.Clone(k))
				size += len(v)
				after = bytes.Clone(k)
				if size >= MaxChunkBytes {
					break
				}
				k, v = c.Next()
			}
			return nil
		})
		if err != nil {
			return Result{}, err
		}
		if len(batch) == 0 {
			break
		}
		err = s.db.Update(func(tx *bolt.Tx) error {
			// Reserve the unpublished version durably in the same batch. A standalone
			// restart must never reuse it and accidentally publish a crashed write set.
			meta := tx.Bucket(metaBucket)
			if number(meta.Get([]byte("allocated"))) < index {
				if err := meta.Put([]byte("allocated"), sequence(index)); err != nil {
					return err
				}
			}
			for i, op := range batch {
				if op.Space == "catalog" && !op.Check {
					catalogChanged = true
				}
				if op.Check {
					continue
				}
				value := []byte{1}
				if op.Delete {
					value[0] = 0
				} else {
					value = append(value, op.Value...)
				}
				if e := putVersion(tx, keys[i], sequence(index), value); e != nil {
					return e
				}
			}
			return nil
		})
		if err != nil {
			return Result{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	err = s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(commitsBucket)
		if err := b.Put(sequence(index), []byte{1}); err != nil {
			return err
		}
		if err := b.Put([]byte(command.ID), sequence(index)); err != nil {
			return err
		}
		m := tx.Bucket(metaBucket)
		if catalogChanged {
			if err := m.Put([]byte("catalog_head"), sequence(index)); err != nil {
				return err
			}
		}
		if err := m.Put([]byte("head"), sequence(index)); err != nil {
			return err
		}
		return m.Put([]byte("applied"), sequence(index))
	})
	if err != nil {
		return Result{}, err
	}
	if err := s.clearPending(command.ID); err != nil {
		return Result{}, err
	}
	return Result{Sequence: index}, nil
}

// clearPending removes bounded batches so large commits do not retain a
// second copy of their entire write set or delete it in one dirty transaction.
func (s *Store) clearPending(id string) error {
	for {
		done := false
		err := s.db.Update(func(tx *bolt.Tx) error {
			root := tx.Bucket(pendingBucket)
			b := root.Bucket([]byte(id))
			if b == nil {
				done = true
				return nil
			}
			c := b.Cursor()
			size := 0
			for k, v := c.First(); k != nil; k, v = c.Next() {
				size += len(k) + len(v)
				if err := c.Delete(); err != nil {
					return err
				}
				if size >= MaxChunkBytes {
					return nil
				}
			}
			done = true
			return root.DeleteBucket([]byte(id))
		})
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}

func (s *Store) CatalogHead() (uint64, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	var head uint64
	err := s.db.View(func(tx *bolt.Tx) error { head = number(tx.Bucket(metaBucket).Get([]byte("catalog_head"))); return nil })
	return head, err
}

func (s *Store) AvailabilityError() error { s.failure.Lock(); defer s.failure.Unlock(); return s.fatal }

func (s *Store) Applied() (uint64, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	var index uint64
	err := s.db.View(func(tx *bolt.Tx) error { index = number(tx.Bucket(metaBucket).Get([]byte("applied"))); return nil })
	return index, err
}
