package mvcc

import (
	"context"
	bolt "go.etcd.io/bbolt"
)

type localCommitRequest struct {
	ctx                  context.Context
	generation, snapshot uint64
	id                   string
	ops                  []Op
	bytes                int
	index                uint64
	err                  error
	done                 chan struct{}
}

// A lone caller commits immediately on its own goroutine. Arrivals during that
// sync may share the next physical transaction, with no batching timer.
func (s *Store) commitLocal(ctx context.Context, generation, snapshot uint64, id string, ops []Op) (uint64, error) {
	if err := validateCommand(Command{ID: id, Ops: ops}); err != nil {
		return 0, err
	}
	r := &localCommitRequest{ctx: ctx, generation: generation, snapshot: snapshot, id: id, ops: ops, done: make(chan struct{})}
	for _, op := range ops {
		r.bytes += len(op.Space) + len(op.Key) + len(op.Value) + 64
	}
	s.groupMu.Lock()
	if !s.groupRunning {
		s.groupRunning = true
		s.groupMu.Unlock()
		s.commitLocalGroup([]*localCommitRequest{r})
		next := s.nextLocalGroup()
		if len(next) > 0 {
			go s.drainLocalGroups(next)
		}
	} else if len(s.groupQueue) < 32 && s.groupBytes+r.bytes <= 2*localInstallBytes {
		s.groupQueue = append(s.groupQueue, r)
		s.groupBytes += r.bytes
		s.groupMu.Unlock()
		// Await the definitive outcome: cancellation racing a durable commit must
		// never be reported as if the committed transaction had rolled back.
		<-r.done
	} else {
		s.groupMu.Unlock()
		s.commitLocalGroup([]*localCommitRequest{r})
	}
	return r.index, r.err
}
func (s *Store) nextLocalGroup() []*localCommitRequest {
	s.groupMu.Lock()
	defer s.groupMu.Unlock()
	if len(s.groupQueue) == 0 {
		s.groupRunning = false
		return nil
	}
	n, size := 0, 0
	for n < len(s.groupQueue) && (n == 0 || size+s.groupQueue[n].bytes <= localInstallBytes) {
		size += s.groupQueue[n].bytes
		n++
	}
	group := append([]*localCommitRequest(nil), s.groupQueue[:n]...)
	copy(s.groupQueue, s.groupQueue[n:])
	clear(s.groupQueue[len(s.groupQueue)-n:])
	s.groupQueue = s.groupQueue[:len(s.groupQueue)-n]
	s.groupBytes -= size
	return group
}
func (s *Store) drainLocalGroups(group []*localCommitRequest) {
	for len(group) > 0 {
		s.commitLocalGroup(group)
		group = s.nextLocalGroup()
	}
}
func (s *Store) commitLocalGroup(group []*localCommitRequest) {
	s.apply.Lock()
	defer s.apply.Unlock()
	defer func() {
		for _, r := range group {
			close(r.done)
		}
	}()
	fatal := s.AvailabilityError()
	eligible := 0
	for _, r := range group {
		r.err = fatal
		if r.err == nil {
			r.err = r.ctx.Err()
		}
		if r.err == nil && r.generation != s.generation.Load() {
			r.err = ErrConflict
		}
		if r.err == nil {
			eligible++
		}
	}
	if eligible == 0 {
		return
	}
	if s.localWAL {
		s.commitLocalWALGroup(group)
		return
	}
	err := s.db.Update(func(tx *bolt.Tx) error {
		for _, r := range group {
			if r.err != nil {
				continue
			}
			if r.err = r.ctx.Err(); r.err != nil {
				continue
			}
			// A fresh reader observes markers published by earlier requests in this
			// same physical transaction, including conflicting writes within the group.
			reader := newVisibilityReader(tx, ^uint64(0))
			for _, op := range r.ops {
				k, _ := key(op.Space, op.Key)
				_, version, _ := reader.visible(k)
				if version > r.snapshot {
					r.err = ErrConflict
					break
				}
			}
			if r.err != nil {
				continue
			}
			s.localSeq++
			r.index = s.localSeq
			version := sequence(r.index)
			catalogChanged := false
			for _, op := range r.ops {
				if op.Check {
					continue
				}
				k, _ := key(op.Space, op.Key)
				value := []byte{0}
				if !op.Delete {
					value = make([]byte, 1+len(op.Value))
					value[0] = 1
					copy(value[1:], op.Value)
				}
				if err := putVersion(tx, k, version, value); err != nil {
					return err
				}
				catalogChanged = catalogChanged || op.Space == "catalog"
			}
			if err := publishCommit(tx, r.id, r.index, catalogChanged); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		s.failure.Lock()
		s.fatal = err
		s.failure.Unlock()
		for _, r := range group {
			if r.err == nil {
				r.err = err
				r.index = 0
			}
		}
	}
}
