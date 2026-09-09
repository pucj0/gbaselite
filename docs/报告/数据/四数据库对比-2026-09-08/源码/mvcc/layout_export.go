package mvcc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"gbaselite/internal/atomicfile"
	bolt "go.etcd.io/bbolt"
	"hash"
	"os"
	"path/filepath"
)

// ExportLayout creates an independently openable copy. It never replaces source.
// Writers are paused for the copy; callers must arrange an offline cutover.
func (s *Store) ExportLayout(ctx context.Context, target, layout string) error {
	if layout != "flat" && layout != "nested" {
		return fmt.Errorf("layout must be flat or nested")
	}
	s.apply.Lock()
	defer s.apply.Unlock()
	if err := s.AvailabilityError(); err != nil {
		return err
	}
	s.gate.RLock()
	defer s.gate.RUnlock()
	return s.db.View(func(src *bolt.Tx) error {
		if k, _ := src.Bucket(pendingBucket).Cursor().First(); k != nil {
			return fmt.Errorf("cannot migrate pending replicated transactions")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := os.Mkdir(target, 0700); err != nil {
			return err
		}
		if err := syncWALDirectory(target); err != nil {
			return err
		}
		marker := filepath.Join(target, "migration.incomplete")
		if err := os.WriteFile(marker, []byte("incomplete\n"), 0600); err != nil {
			return err
		}
		f, err := os.OpenFile(marker, os.O_RDWR, 0600)
		if err != nil {
			return err
		}
		err = f.Sync()
		f.Close()
		if err != nil {
			return err
		}
		if err = syncWALDirectory(marker); err != nil {
			return err
		}
		build := filepath.Join(target, "mvcc.db.build")
		dst, err := bolt.Open(build, 0600, &bolt.Options{NoFreelistSync: true})
		if err != nil {
			return err
		}
		defer dst.Close()
		err = dst.Update(func(tx *bolt.Tx) error {
			for _, name := range [][]byte{dataBucket, metaBucket, pendingBucket, commitsBucket, countersBucket} {
				if _, err := tx.CreateBucket(name); err != nil {
					return err
				}
			}
			if layout == "flat" {
				if _, err := tx.CreateBucket(flatVersionsBucket); err != nil {
					return err
				}
				return tx.Bucket(metaBucket).Put(layoutKey, []byte{1})
			}
			return nil
		})
		if err != nil {
			return err
		}
		type item struct{ bucket, key, version, value []byte }
		var batch []item
		size := 0
		flush := func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			err := dst.Update(func(tx *bolt.Tx) error {
				for _, v := range batch {
					var err error
					if v.version != nil {
						err = putVersion(tx, v.key, v.version, v.value)
					} else {
						err = tx.Bucket(v.bucket).Put(v.key, v.value)
					}
					if err != nil {
						return err
					}
				}
				return nil
			})
			batch = nil
			size = 0
			return err
		}
		err = walkLayout(src, func(bucket, key, version, value []byte) error {
			if size > 0 && size+len(key)+len(value)+len(version) > 256<<10 {
				if err := flush(); err != nil {
					return err
				}
			}
			batch = append(batch, item{bytes.Clone(bucket), bytes.Clone(key), bytes.Clone(version), bytes.Clone(value)})
			size += len(key) + len(value) + len(version) + 64
			return ctx.Err()
		})
		if err != nil {
			return err
		}
		if len(batch) > 0 {
			if err = flush(); err != nil {
				return err
			}
		}
		sourceHash := sha256.New()
		if err = hashLayout(ctx, src, sourceHash); err != nil {
			return err
		}
		targetHash := sha256.New()
		err = dst.View(func(tx *bolt.Tx) error {
			if err := validateVersionLayout(tx); err != nil {
				return err
			}
			return hashLayout(ctx, tx, targetHash)
		})
		if err != nil {
			return err
		}
		if !bytes.Equal(sourceHash.Sum(nil), targetHash.Sum(nil)) {
			return fmt.Errorf("migration content checksum mismatch")
		}
		if err = dst.Close(); err != nil {
			return err
		}
		if err = atomicfile.Replace(build, filepath.Join(target, "mvcc.db")); err != nil {
			return err
		}
		if err = syncWALDirectory(build); err != nil {
			return err
		}
		result := []byte(fmt.Sprintf("layout=%s\nsha256=%s\n", layout, hex.EncodeToString(sourceHash.Sum(nil))))
		if err = os.WriteFile(marker, result, 0600); err != nil {
			return err
		}
		f, err = os.OpenFile(marker, os.O_RDWR, 0600)
		if err != nil {
			return err
		}
		err = f.Sync()
		f.Close()
		if err != nil {
			return err
		}
		if err = atomicfile.Replace(marker, filepath.Join(target, "migration.txt")); err != nil {
			return err
		}
		return syncWALDirectory(marker)
	})
}

// Canonical traversal excludes only the physical-layout selector. All versions,
// including tombstones and unpublished versions, and logical metadata survive.
func walkLayout(tx *bolt.Tx, visit func([]byte, []byte, []byte, []byte) error) error {
	for _, name := range [][]byte{metaBucket, commitsBucket, countersBucket} {
		if err := tx.Bucket(name).ForEach(func(k, v []byte) error {
			if bytes.Equal(name, metaBucket) && bytes.Equal(k, layoutKey) {
				return nil
			}
			if v == nil {
				return fmt.Errorf("unexpected nested metadata bucket")
			}
			return visit(name, k, nil, v)
		}); err != nil {
			return err
		}
	}
	if flatLayout(tx) {
		// Traverse every physical version, not just keys reachable through the
		// row directory. Otherwise an orphan could silently disappear on copy.
		return tx.Bucket(flatVersionsBucket).ForEach(func(k, v []byte) error {
			row, seq, err := splitFlatVersionKey(k)
			if err != nil {
				return err
			}
			if !bytes.Equal(tx.Bucket(dataBucket).Get(row), []byte{1}) {
				return fmt.Errorf("flat version is missing its row directory entry")
			}
			if err = validateLayoutVersion(seq, v); err != nil {
				return err
			}
			return visit(dataBucket, row, seq, v)
		})
	}
	return tx.Bucket(dataBucket).ForEach(func(k, _ []byte) error {
		b := tx.Bucket(dataBucket).Bucket(k)
		if b == nil {
			return fmt.Errorf("invalid nested row directory")
		}
		return b.ForEach(func(seq, v []byte) error {
			if err := validateLayoutVersion(seq, v); err != nil {
				return err
			}
			return visit(dataBucket, k, seq, v)
		})
	})
}
func hashLayout(ctx context.Context, tx *bolt.Tx, h hash.Hash) error {
	return walkLayout(tx, func(b, k, s, v []byte) error {
		for _, p := range [][]byte{b, k, s, v} {
			var size [8]byte
			binary.BigEndian.PutUint64(size[:], uint64(len(p)))
			h.Write(size[:])
			h.Write(p)
		}
		return ctx.Err()
	})
}

func splitFlatVersionKey(k []byte) ([]byte, []byte, error) {
	row := make([]byte, 0, len(k))
	for i := 0; i < len(k); i++ {
		if k[i] != 0 {
			row = append(row, k[i])
			continue
		}
		i++
		if i >= len(k) {
			break
		}
		switch k[i] {
		case 255:
			row = append(row, 0)
		case 0:
			if len(k)-i-1 == 8 {
				return row, k[i+1:], nil
			}
			return nil, nil, fmt.Errorf("invalid flat version suffix")
		default:
			return nil, nil, fmt.Errorf("invalid flat key escape")
		}
	}
	return nil, nil, fmt.Errorf("unterminated flat version key")
}
func validateLayoutVersion(seq, v []byte) error {
	if len(seq) != 8 || number(seq) == 0 || len(v) < 1 || v[0] > 1 || len(v) > MaxValueBytes+1 || v[0] == 0 && len(v) != 1 {
		return fmt.Errorf("invalid MVCC version record")
	}
	return nil
}
