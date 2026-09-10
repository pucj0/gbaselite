package mvcc

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"gbaselite/internal/atomicfile"
	bolt "go.etcd.io/bbolt"
	"hash"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
)

const localWALMagic = "GBLWAL01"
const walBegin = 1
const walOp = 2
const walEnd = 3
const walSeal = 4
const maxWALFrame = MaxValueBytes + 10240

type localWALWriter struct {
	w      *bufio.Writer
	digest hash.Hash
}

func (w *localWALWriter) frame(p []byte) error {
	var h [8]byte
	binary.BigEndian.PutUint32(h[:4], uint32(len(p)))
	binary.BigEndian.PutUint32(h[4:], crc32.ChecksumIEEE(p))
	if len(p) > maxWALFrame {
		return errors.New("WAL frame exceeds budget")
	}
	if _, err := w.w.Write(h[:]); err != nil {
		return err
	}
	if _, err := w.w.Write(p); err != nil {
		return err
	}
	if p[0] != walSeal {
		w.digest.Write(h[:])
		w.digest.Write(p)
	}
	return nil
}
func (w *localWALWriter) begin(index uint64, id string) error {
	if len(id) == 0 || len(id) > 128 || index == 0 {
		return errors.New("invalid WAL transaction")
	}
	p := append([]byte{walBegin}, sequence(index)...)
	p = append(p, []byte(id)...)
	return w.frame(p)
}
func (w *localWALWriter) op(k, encoded []byte) error {
	if _, err := stagedOpHeader(k, encoded); err != nil {
		return err
	}
	p := make([]byte, 5+len(k)+len(encoded))
	p[0] = walOp
	binary.BigEndian.PutUint32(p[1:5], uint32(len(k)))
	copy(p[5:], k)
	copy(p[5+len(k):], encoded)
	return w.frame(p)
}
func (s *Store) walPath() string { return filepath.Join(filepath.Dir(s.path), "local.commit.wal") }
func (s *Store) writeLocalWAL(ctx context.Context, produce func(*localWALWriter) error) error {
	var identity []byte
	if err := s.db.View(func(tx *bolt.Tx) error {
		identity = bytes.Clone(tx.Bucket(metaBucket).Get([]byte("wal_identity")))
		return nil
	}); err != nil {
		return err
	}
	if len(identity) != 32 {
		return errors.New("missing WAL store identity")
	}
	tmp := s.walPath() + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { f.Close(); os.Remove(tmp) }()
	w := &localWALWriter{w: bufio.NewWriterSize(f, MaxChunkBytes), digest: sha256.New()}
	if _, err = w.w.WriteString(localWALMagic + string(identity)); err != nil {
		return err
	}
	if err = produce(w); err != nil {
		return err
	}
	if err = w.frame(append([]byte{walSeal}, w.digest.Sum(nil)...)); err != nil {
		return err
	}
	if err = w.w.Flush(); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	// Publication is the durable commit decision. Once this succeeds, callers
	// complete installation/recovery without treating cancellation as rollback.
	if err := atomicfile.Replace(tmp, s.walPath()); err != nil {
		s.failLocalWAL(err)
		return err
	}
	err = syncWALDirectory(s.walPath())
	s.failLocalWAL(err)
	return err
}
func walkLocalWAL(path string, identity []byte, visit func([]byte) error) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, MaxChunkBytes)
	header := make([]byte, 40)
	n, err := io.ReadFull(r, header)
	if n == 0 && err == io.EOF {
		return nil
	}
	if err != nil || string(header[:8]) != localWALMagic || !bytes.Equal(header[8:], identity) {
		return errors.New("invalid WAL header/store identity")
	}
	digest := sha256.New()
	active := false
	var previous uint64
	for {
		var h [8]byte
		if _, err = io.ReadFull(r, h[:]); err != nil {
			return fmt.Errorf("unsealed WAL: %w", err)
		}
		length := binary.BigEndian.Uint32(h[:4])
		if length < 1 || length > maxWALFrame {
			return errors.New("invalid WAL frame length")
		}
		p := make([]byte, int(length))
		if _, err = io.ReadFull(r, p); err != nil {
			return err
		}
		if crc32.ChecksumIEEE(p) != binary.BigEndian.Uint32(h[4:]) {
			return errors.New("WAL checksum mismatch")
		}
		switch p[0] {
		case walBegin:
			if active || len(p) < 10 || len(p) > 137 || number(p[1:9]) <= previous {
				return errors.New("invalid WAL begin")
			}
			previous = number(p[1:9])
			active = true
		case walOp:
			if !active || len(p) < 7 {
				return errors.New("invalid WAL operation")
			}
			n := int(binary.BigEndian.Uint32(p[1:5]))
			if n < 1 || n > len(p)-7 {
				return errors.New("invalid WAL key length")
			}
			if _, err = stagedOpHeader(p[5:5+n], p[5+n:]); err != nil {
				return err
			}
		case walEnd:
			if !active || len(p) != 1 {
				return errors.New("invalid WAL end")
			}
			active = false
		case walSeal:
			if active || len(p) != 33 || !bytes.Equal(p[1:], digest.Sum(nil)) {
				return errors.New("invalid WAL seal")
			}
			if _, err = r.ReadByte(); err != io.EOF {
				return errors.New("trailing WAL bytes")
			}
			return nil
		default:
			return errors.New("unknown WAL record")
		}
		digest.Write(h[:])
		digest.Write(p)
		if visit != nil {
			if err = visit(p); err != nil {
				return err
			}
		}
	}
}
func (s *Store) clearLocalWAL() error {
	f, err := os.OpenFile(s.walPath(), os.O_WRONLY|os.O_TRUNC, 0600)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	err = f.Sync()
	return errors.Join(err, f.Close())
}
func (s *Store) recoverLocalWAL() error {
	var identity []byte
	if err := s.db.View(func(tx *bolt.Tx) error {
		identity = bytes.Clone(tx.Bucket(metaBucket).Get([]byte("wal_identity")))
		return nil
	}); err != nil {
		return err
	}
	// Validate the complete seal before installing any record. A damaged suffix
	// must not allow a valid prefix to become committed.
	if err := walkLocalWAL(s.walPath(), identity, nil); err != nil {
		return err
	}
	type entry struct{ k, v []byte }
	var batch []entry
	size := 0
	var index uint64
	var id string
	skip, catalog := false, false
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := s.db.Update(func(tx *bolt.Tx) error {
			if err := tx.Bucket(metaBucket).Put([]byte("allocated"), sequence(max(index, number(tx.Bucket(metaBucket).Get([]byte("allocated")))))); err != nil {
				return err
			}
			for _, op := range batch {
				if op.v[1]&2 != 0 {
					continue
				}
				v := []byte{0}
				if op.v[1]&1 == 0 {
					v = append([]byte{1}, op.v[2:]...)
				}
				if err := putVersion(tx, op.k, sequence(index), v); err != nil {
					return err
				}
			}
			return nil
		})
		clear(batch)
		batch = batch[:0]
		size = 0
		return err
	}
	err := walkLocalWAL(s.walPath(), identity, func(p []byte) error {
		switch p[0] {
		case walBegin:
			index = number(p[1:9])
			id = string(p[9:])
			catalog = false
			skip = false
			s.localSeq = max(s.localSeq, index)
			return s.db.View(func(tx *bolt.Tx) error {
				existing := number(tx.Bucket(commitsBucket).Get([]byte(id)))
				if existing != 0 {
					if existing != index {
						return errors.New("WAL transaction identity mismatch")
					}
					skip = true
					return nil
				}
				if number(tx.Bucket(metaBucket).Get([]byte("head"))) >= index {
					return errors.New("stale WAL transaction")
				}
				return nil
			})
		case walOp:
			if skip {
				return nil
			}
			n := int(binary.BigEndian.Uint32(p[1:5]))
			k, v := p[5:5+n], p[5+n:]
			if size+len(p) > localInstallBytes {
				if err := flush(); err != nil {
					return err
				}
			}
			batch = append(batch, entry{k, v})
			size += len(p)
			catalog = catalog || bytes.HasPrefix(k, []byte("catalog\x00"))
		case walEnd:
			if skip {
				return nil
			}
			if err := flush(); err != nil {
				return err
			}
			return s.db.Update(func(tx *bolt.Tx) error { return publishCommit(tx, id, index, catalog) })
		}
		return nil
	})
	if err != nil {
		return err
	}
	return s.clearLocalWAL()
}
func (s *Store) failLocalWAL(err error) {
	if err != nil {
		s.failure.Lock()
		s.fatal = err
		s.failure.Unlock()
	}
}
func (s *Store) commitLocalWALGroup(group []*localCommitRequest) {
	seen := make(map[string]uint64)
	err := s.db.View(func(tx *bolt.Tx) error {
		reader := newVisibilityReader(tx, ^uint64(0))
		for _, r := range group {
			if r.err != nil {
				continue
			}
			if r.err = r.ctx.Err(); r.err != nil {
				continue
			}
			for _, op := range r.ops {
				k, _ := key(op.Space, op.Key)
				_, v, _ := reader.visible(k)
				v = max(v, seen[string(k)])
				if op.Space == rangeGuardSpace {
					for changed, index := range seen {
						if rangeDependencyContains(op.Key, []byte(changed)) {
							v = max(v, index)
						}
					}
				}
				if v > r.snapshot {
					r.err = ErrConflict
					break
				}
			}
			if r.err != nil {
				continue
			}
			s.localSeq++
			r.index = s.localSeq
			for _, op := range r.ops {
				if !op.Check {
					k, _ := key(op.Space, op.Key)
					seen[string(k)] = r.index
				}
			}
		}
		return nil
	})
	accepted := 0
	for _, r := range group {
		if r.err == nil {
			accepted++
		}
	}
	if err == nil && accepted > 0 {
		err = s.writeLocalWAL(context.Background(), func(w *localWALWriter) error {
			for _, r := range group {
				if r.err != nil {
					continue
				}
				if err := w.begin(r.index, r.id); err != nil {
					return err
				}
				for _, op := range r.ops {
					k, _ := key(op.Space, op.Key)
					if err := w.op(k, encodeStagedOp(op)); err != nil {
						return err
					}
				}
				if err := w.frame([]byte{walEnd}); err != nil {
					return err
				}
			}
			return nil
		})
		if err == nil {
			err = s.recoverLocalWAL()
		}
	}
	if err != nil {
		s.failLocalWAL(err)
		for _, r := range group {
			if r.err == nil {
				r.err = err
				r.index = 0
			}
		}
	}
}
