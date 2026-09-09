package mvcc

import (
	"fmt"
	"gbaselite/internal/atomicfile"
	bolt "go.etcd.io/bbolt"
	"io"
	"os"
)

// Snapshot holds a consistent disk view. Payload is streamed, never buffered as
// a whole database. Writes may wait for a bbolt mapping growth until Close.
type Snapshot struct {
	tx    *bolt.Tx
	store *Store
}

func (v *Snapshot) WriteTo(w io.Writer) (int64, error) { return v.tx.WriteTo(w) }
func (v *Snapshot) Rollback() error                    { err := v.tx.Rollback(); v.store.gate.RUnlock(); return err }
func (s *Store) Snapshot() (*Snapshot, error) {
	s.gate.RLock()
	tx, err := s.db.Begin(false)
	if err != nil {
		s.gate.RUnlock()
		return nil, err
	}
	return &Snapshot{tx: tx, store: s}, nil
}
func (s *Store) Restore(reader io.Reader) error {
	s.apply.Lock()
	defer s.apply.Unlock()
	if err := s.AvailabilityError(); err != nil {
		return err
	}
	s.views.Lock()
	defer s.views.Unlock()
	s.gate.Lock()
	defer s.gate.Unlock()
	temporary := s.path + ".restore"
	f, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer os.Remove(temporary)
	_, err = io.CopyBuffer(f, reader, make([]byte, 64<<10))
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	check, err := bolt.Open(temporary, 0600, &bolt.Options{ReadOnly: true})
	if err != nil {
		return err
	}
	err = check.View(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{dataBucket, metaBucket, pendingBucket, commitsBucket, countersBucket} {
			if tx.Bucket(name) == nil {
				return fmt.Errorf("invalid MVCC snapshot: missing %s", name)
			}
		}
		return validateVersionLayout(tx)
	})
	check.Close()
	if err != nil {
		return err
	}
	// Invalid input must not invalidate active transactions in the healthy store.
	// Invalidate only once the validated image is ready to replace the database.
	s.generation.Add(1)
	s.active = make(map[uint64]int)
	if err = s.db.Close(); err != nil {
		return err
	}
	replaceErr := atomicfile.Replace(temporary, s.path)
	if replaceErr == nil {
		replaceErr = syncWALDirectory(s.path)
	}
	reopened, openErr := bolt.Open(s.path, 0600, &bolt.Options{NoFreelistSync: true})
	err = openErr
	if err == nil {
		s.db = reopened
	}
	if err != nil {
		s.failLocalWAL(err)
		return err
	}
	if replaceErr != nil {
		s.failLocalWAL(replaceErr)
		return replaceErr
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		if s.localWAL && tx.Bucket(metaBucket).Get([]byte("wal_identity")) == nil {
			if err := tx.Bucket(metaBucket).Put([]byte("wal_identity"), []byte(randomID())); err != nil {
				return err
			}
		}
		s.localSeq = max(number(tx.Bucket(metaBucket).Get([]byte("applied"))), number(tx.Bucket(metaBucket).Get([]byte("allocated"))))
		return nil
	})
}
