package mvcc

import (
	"context"
	"sync"

	"gbaselite/storageengine"
)

// ViewToken represents an atomic, pinned snapshot view token owned by a transaction or statement.
type ViewToken struct {
	id         string
	rootID     string
	revision   uint64
	generation uint64
	store      *Store
	tx         *Tx
	mu         sync.Mutex
	closed     bool
}

// BeginViewToken captures the current published Head under views lock and pins it in TransactionManager.
// The lock order (views -> txns.mu) ensures atomic sampling where no GC compaction can run between
// reading Head and pinning the snapshotRefs reference.
func (s *Store) BeginViewToken(ctx context.Context, tx *Tx) (*ViewToken, error) {
	if err := s.AvailabilityError(); err != nil {
		return nil, err
	}
	s.views.Lock()
	defer s.views.Unlock()
	generation := s.generation.Load()
	revision, err := s.Head()
	if err != nil {
		return nil, err
	}
	id := randomID()
	rootID := ""
	if tx != nil {
		rootID = tx.ID
	}
	err = s.txns.RegisterView(ViewRegistration{
		ID:         id,
		RootID:     rootID,
		Revision:   revision,
		Generation: generation,
	})
	if err != nil {
		return nil, err
	}
	return &ViewToken{
		id:         id,
		rootID:     rootID,
		revision:   revision,
		generation: generation,
		store:      s,
		tx:         tx,
	}, nil
}

// BeginViewTokenAt captures a view token at an explicit snapshot revision.
func (s *Store) BeginViewTokenAt(ctx context.Context, tx *Tx, revision uint64) (*ViewToken, error) {
	if err := s.AvailabilityError(); err != nil {
		return nil, err
	}
	s.views.Lock()
	defer s.views.Unlock()
	generation := s.generation.Load()
	id := randomID()
	rootID := ""
	if tx != nil {
		rootID = tx.ID
	}
	err := s.txns.RegisterView(ViewRegistration{
		ID:         id,
		RootID:     rootID,
		Revision:   revision,
		Generation: generation,
	})
	if err != nil {
		return nil, err
	}
	return &ViewToken{
		id:         id,
		rootID:     rootID,
		revision:   revision,
		generation: generation,
		store:      s,
		tx:         tx,
	}, nil
}

func (v *ViewToken) ID() string         { return v.id }
func (v *ViewToken) Revision() uint64   { return v.revision }
func (v *ViewToken) Generation() uint64 { return v.generation }
func (v *ViewToken) RootID() string     { return v.rootID }

func (v *ViewToken) Closed() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.closed
}

func (v *ViewToken) checkOpen() error {
	v.mu.Lock()
	closed := v.closed
	v.mu.Unlock()
	if closed {
		return storageengine.ErrViewClosed
	}
	if v.generation != v.store.generation.Load() {
		return storageengine.ErrViewInvalidated
	}
	return nil
}

func (v *ViewToken) Get(space string, rowKey []byte) ([]byte, bool, error) {
	if err := v.checkOpen(); err != nil {
		return nil, false, err
	}
	if v.tx != nil {
		return v.tx.readAt(v.revision, space, rowKey)
	}
	val, _, ok, err := v.store.Get(v.revision, space, rowKey)
	if v.generation != v.store.generation.Load() {
		return nil, false, ErrConflict
	}
	return val, ok, err
}

func (v *ViewToken) ScanRange(ctx context.Context, space string, r KeyRange, yield func([]byte, []byte) error) error {
	if err := v.checkOpen(); err != nil {
		return err
	}
	if v.tx != nil {
		return v.tx.scanRangeAt(ctx, space, r, v.revision, false, yield)
	}
	return v.store.ScanRange(ctx, v.revision, space, r, yield)
}

func (v *ViewToken) Close() error {
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return nil
	}
	v.closed = true
	v.mu.Unlock()
	v.store.txns.ReleaseView(v.id)
	return nil
}
