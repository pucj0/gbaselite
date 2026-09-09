package mvcc

import (
	"bytes"
	"context"
	"encoding/json"
	bolt "go.etcd.io/bbolt"
)

// CompactHistory retains the newest version at or before the oldest local
// snapshot, plus every newer version. It is only available in standalone mode:
// clustered retention needs a replicated snapshot horizon across leader terms.
func (s *Store) CompactHistory(ctx context.Context) error {
	var after []byte
	repeat := false
	generation := s.generation.Load()
	// Each physical batch releases both locks, allowing commits and new snapshots
	// to make progress. A restore invalidates the cursor rather than mixing stores.
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		done, err := func() (bool, error) {
			s.apply.Lock()
			defer s.apply.Unlock()
			s.views.Lock()
			defer s.views.Unlock()
			if generation != s.generation.Load() {
				return false, ErrConflict
			}
			if err := s.AvailabilityError(); err != nil {
				return false, err
			}
			horizon, err := s.Head()
			if err != nil {
				return false, err
			}
			for snapshot := range s.active {
				if snapshot < horizon {
					horizon = snapshot
				}
			}
			done := false
			err = s.db.Update(func(tx *bolt.Tx) error {
				root := tx.Bucket(dataBucket)
				c := root.Cursor()
				k, _ := c.First()
				if after != nil {
					k, _ = c.Seek(after)
					if bytes.Equal(k, after) && !repeat {
						k, _ = c.Next()
					}
				}
				work := 0
				for ; k != nil; k, _ = c.Next() {
					if err := ctx.Err(); err != nil {
						return err
					}
					after = bytes.Clone(k)
					repeat = false
					_, anchor, _ := visible(tx, k, horizon)
					var versions *bolt.Cursor
					var prefix, first, value []byte
					if flatLayout(tx) {
						prefix = flatPrefix(k)
						versions = tx.Bucket(flatVersionsBucket).Cursor()
						first, value = versions.Seek(prefix)
					} else {
						versions = root.Bucket(k).Cursor()
						first, value = versions.First()
					}
					for v, p := first, value; v != nil; v, p = versions.Next() {
						version := v
						if prefix != nil {
							if !bytes.HasPrefix(v, prefix) {
								break
							}
							version = v[len(prefix):]
						}
						if number(version) >= anchor {
							break
						}
						if err := ctx.Err(); err != nil {
							return err
						}
						work += len(v) + len(p)
						if err := versions.Delete(); err != nil {
							return err
						}
						if work >= MaxChunkBytes {
							repeat = true
							return nil
						}
					}
					work += len(k)
					if work >= MaxChunkBytes {
						return nil
					}
				}
				done = true
				return nil
			})
			return done, err
		}()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}

// PendingBytes is an inspection aid; it reads one staged operation at a time.
func (s *Store) PendingBytes() (int64, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	var size int64
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(pendingBucket).ForEach(func(k, v []byte) error {
			return tx.Bucket(pendingBucket).Bucket(k).ForEach(func(k, v []byte) error {
				var op Op
				if err := json.Unmarshal(v, &op); err != nil {
					return err
				}
				size += int64(len(op.Key) + len(op.Value))
				return nil
			})
		})
	})
	return size, err
}
