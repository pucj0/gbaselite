package mvcc

import (
	"bytes"
	"fmt"
	"sort"
	"sync"
)

const (
	maxGlobalDirtyEntries  = 65536
	maxGlobalDirtyBytes    = 64 * 1024 * 1024 // 64 MB
	maxDirtyEntriesPerRoot = 8192
)

// DirtyVersion represents one uncommitted operation published from an active transaction.
type DirtyVersion struct {
	RootID       string
	Generation   uint64
	Term         uint64
	MergeOrdinal uint64
	Space        string
	Key          []byte
	Value        []byte
	Deleted      bool
}

// DirtySnapshot is an immutable, point-in-time sample of all active foreign uncommitted writes.
type DirtySnapshot struct {
	Generation   uint64
	Term         uint64
	entries      map[string]DirtyVersion   // composite key -> DirtyVersion
	spaceEntries map[string][]DirtyVersion // space -> sorted slice of DirtyVersion
}

// Get looks up a foreign dirty version for (space, key).
func (s *DirtySnapshot) Get(space string, key []byte) (DirtyVersion, bool) {
	if s == nil || s.entries == nil {
		return DirtyVersion{}, false
	}
	v, ok := s.entries[space+"\x00"+string(key)]
	return v, ok
}

// SpaceEntries returns the pre-sorted slice of dirty versions in one space.
func (s *DirtySnapshot) SpaceEntries(space string) []DirtyVersion {
	if s == nil || s.spaceEntries == nil {
		return nil
	}
	return s.spaceEntries[space]
}

// DirtyRegistry manages the in-memory visibility of active uncommitted root transactions.
type DirtyRegistry struct {
	mu           sync.RWMutex
	generation   uint64
	term         uint64
	byRoot       map[string]map[string]DirtyVersion // rootID -> (compositeKey -> DirtyVersion)
	globalCount  int
	globalBytes  int64
	mergeCounter uint64
}

// NewDirtyRegistry constructs a clean DirtyRegistry.
func NewDirtyRegistry(generation, term uint64) *DirtyRegistry {
	return &DirtyRegistry{
		generation: generation,
		term:       term,
		byRoot:     make(map[string]map[string]DirtyVersion),
	}
}

// Publish atomically updates the published dirty versions for one root.
func (r *DirtyRegistry) Publish(rootID string, generation, term uint64, versions []DirtyVersion) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// 1. Generation/Term check:
	// A transaction from an older epoch must be rejected and must not overwrite current epoch.
	if generation < r.generation || (generation == r.generation && term < r.term) {
		return fmt.Errorf("%w: stale generation/term (%d/%d < %d/%d)", ErrConflict, generation, term, r.generation, r.term)
	}

	// If moving to a strictly newer epoch, reset registry to the new epoch
	if generation > r.generation || (generation == r.generation && term > r.term) {
		r.generation = generation
		r.term = term
		r.byRoot = make(map[string]map[string]DirtyVersion)
		r.globalCount = 0
		r.globalBytes = 0
	}

	// 2. Validate per-root budget BEFORE mutating existing state
	if len(versions) > maxDirtyEntriesPerRoot {
		return fmt.Errorf("%w: root %s exceeded dirty entries limit (%d > %d)", ErrWriteSetLimit, rootID, len(versions), maxDirtyEntriesPerRoot)
	}

	// 3. Compute net changes BEFORE modifying existing state
	var oldCount int
	var oldBytes int64
	if old, ok := r.byRoot[rootID]; ok {
		oldCount = len(old)
		for _, ver := range old {
			oldBytes += int64(len(ver.Key) + len(ver.Value) + len(ver.Space) + 64)
		}
	}

	var newBytes int64
	for _, ver := range versions {
		newBytes += int64(len(ver.Key) + len(ver.Value) + len(ver.Space) + 64)
	}

	netCount := r.globalCount - oldCount + len(versions)
	netBytes := r.globalBytes - oldBytes + newBytes

	if netCount > maxGlobalDirtyEntries {
		return fmt.Errorf("%w: global dirty entries limit exceeded (%d > %d)", ErrWriteSetLimit, netCount, maxGlobalDirtyEntries)
	}
	if netBytes > maxGlobalDirtyBytes {
		return fmt.Errorf("%w: global dirty bytes limit exceeded (%d > %d)", ErrWriteSetLimit, netBytes, maxGlobalDirtyBytes)
	}

	// 4. All checks passed: perform atomic update
	if len(versions) == 0 {
		delete(r.byRoot, rootID)
		r.globalCount = netCount
		r.globalBytes = netBytes
		return nil
	}

	r.mergeCounter++
	rootMap := make(map[string]DirtyVersion, len(versions))
	for _, ver := range versions {
		ver.RootID = rootID
		ver.Generation = generation
		ver.Term = term
		ver.MergeOrdinal = r.mergeCounter
		ver.Key = bytes.Clone(ver.Key)
		ver.Value = bytes.Clone(ver.Value)
		compKey := ver.Space + "\x00" + string(ver.Key)
		rootMap[compKey] = ver
	}

	r.byRoot[rootID] = rootMap
	r.globalCount = netCount
	r.globalBytes = netBytes
	return nil
}

// Withdraw removes all published dirty versions for one root.
func (r *DirtyRegistry) Withdraw(rootID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if old, ok := r.byRoot[rootID]; ok {
		r.globalCount -= len(old)
		for _, ver := range old {
			r.globalBytes -= int64(len(ver.Key) + len(ver.Value) + len(ver.Space) + 64)
		}
		delete(r.byRoot, rootID)
	}
}

// Reset clears all dirty versions upon generation or term reset.
func (r *DirtyRegistry) Reset(generation, term uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.generation = generation
	r.term = term
	r.byRoot = make(map[string]map[string]DirtyVersion)
	r.globalCount = 0
	r.globalBytes = 0
}

// Sample captures an immutable point-in-time DirtySnapshot excluding readerRootID.
func (r *DirtyRegistry) Sample(readerRootID string) (*DirtySnapshot, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	snap := &DirtySnapshot{
		Generation:   r.generation,
		Term:         r.term,
		entries:      make(map[string]DirtyVersion),
		spaceEntries: make(map[string][]DirtyVersion),
	}

	// Merge candidate dirty versions across all other active roots
	for rootID, rootMap := range r.byRoot {
		if rootID == readerRootID {
			continue // reader's own writes take precedence in reader's own transaction overlay
		}
		for compKey, ver := range rootMap {
			if existing, ok := snap.entries[compKey]; ok {
				// Deterministic choice: highest MergeOrdinal wins
				if ver.MergeOrdinal > existing.MergeOrdinal {
					snap.entries[compKey] = ver
				}
			} else {
				snap.entries[compKey] = ver
			}
		}
	}

	for _, ver := range snap.entries {
		snap.spaceEntries[ver.Space] = append(snap.spaceEntries[ver.Space], ver)
	}

	// Sort each space's dirty entries by key for linear range scan merging
	for space := range snap.spaceEntries {
		sort.Slice(snap.spaceEntries[space], func(i, j int) bool {
			return bytes.Compare(snap.spaceEntries[space][i].Key, snap.spaceEntries[space][j].Key) < 0
		})
	}

	return snap, nil
}
