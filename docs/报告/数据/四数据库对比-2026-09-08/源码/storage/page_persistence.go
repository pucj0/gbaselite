package storage

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"gbaselite/internal/atomicfile"
)

type pagePersistence struct {
	directory       string
	legacyPath      string
	checkpointPath  string
	walPath         string
	pageDirectory   string
	cache           *rowPageCache
	initialized     bool
	verified        bool
	manifest        *pageManifest
	manifestBytes   []byte
	walBytes        int64
	sinceCheckpoint int
	readers         int
	coldPins        atomic.Int64
	stats           PagePersistenceStats
}

// PagePersistenceStats describes this process's paged I/O. CacheBytes is the
// retained encoded payload, not total process memory or decoded table memory.
type PagePersistenceStats struct {
	Enabled               bool
	Generation            uint64
	PagesWritten          uint64
	DiskIndexBuilds       uint64
	DiskIndexBytesWritten uint64
	PageBytesWritten      uint64
	WALBytesWritten       uint64
	CacheBytes            int64
	CacheBudgetBytes      int64
	CacheEntries          int
	CacheHits             uint64
	CacheMisses           uint64
	Checkpoints           uint64
	PagesReclaimed        uint64
}

// NewPagedPersistence opts into immutable row pages and WAL-backed manifests.
// Existing store.gob is read on first use and preserved during migration.
// cacheBytes limits encoded page payload retained by the LRU; zero disables it.
func NewPagedPersistence(dataDir string, cacheBytes int64) *Persistence {
	directory := filepath.Join(dataDir, "databases")
	return &Persistence{
		path:        filepath.Join(directory, "store.pages"),
		replaceFile: atomicfile.Replace,
		pages: &pagePersistence{
			directory:      directory,
			legacyPath:     filepath.Join(directory, "store.gob"),
			checkpointPath: filepath.Join(directory, "store.checkpoint"),
			walPath:        filepath.Join(directory, "store.wal"),
			pageDirectory:  filepath.Join(directory, "pages"),
			cache:          newRowPageCache(cacheBytes),
		},
	}
}

func (p *Persistence) PagedStats() PagePersistenceStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pages == nil {
		return PagePersistenceStats{}
	}
	stats := p.pages.stats
	stats.Enabled = true
	if p.pages.manifest != nil {
		stats.Generation = p.pages.manifest.Generation
	}
	cache := p.pages.cache
	cache.mu.Lock()
	stats.CacheBytes, stats.CacheBudgetBytes = cache.used, cache.budget
	stats.CacheEntries, stats.CacheHits, stats.CacheMisses = len(cache.entries), cache.hits, cache.misses
	cache.mu.Unlock()
	return stats
}

func decodePageManifest(data []byte) (*pageManifest, error) {
	var manifest pageManifest
	if err := decodePageGob(data, &manifest); err != nil {
		return nil, fmt.Errorf("decode page manifest: %w", err)
	}
	if manifest.Version != pageFormatVersion {
		return nil, fmt.Errorf("unsupported page format version %d", manifest.Version)
	}
	if manifest.Generation == 0 {
		return nil, fmt.Errorf("invalid zero page generation")
	}
	// Page manifests always contain an already migrated schema. Migrations which
	// require rows must run on legacy input before pages are written.
	if manifest.Snapshot.FormatVersion != CurrentSnapshotFormatVersion {
		return nil, fmt.Errorf("paged snapshot format version %d does not match supported version %d", manifest.Snapshot.FormatVersion, CurrentSnapshotFormatVersion)
	}
	if len(manifest.Databases) != len(manifest.Snapshot.Databases) {
		return nil, fmt.Errorf("page database metadata count mismatch")
	}
	for databaseIndex, database := range manifest.Snapshot.Databases {
		if len(database.Tables) != len(manifest.Databases[databaseIndex].Tables) {
			return nil, fmt.Errorf("page table metadata count mismatch")
		}
		for tableIndex, table := range database.Tables {
			if len(table.Rows) != 0 {
				return nil, fmt.Errorf("page manifest unexpectedly embeds rows")
			}
			for _, ref := range manifest.Databases[databaseIndex].Tables[tableIndex].Pages {
				if ref.Rows <= 0 || ref.Rows > pageMaxRows || ref.Bytes <= 0 || ref.Bytes > pageMaxPayload {
					return nil, fmt.Errorf("invalid row page reference")
				}
			}
		}
	}
	return &manifest, nil
}

func (p *Persistence) initializePages() error {
	if p.pages.initialized {
		return nil
	}
	var current *pageManifest
	var currentBytes []byte
	for _, path := range []string{p.pages.checkpointPath, p.path} {
		data, err := readPageFile(path, pageHeadMagic)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read durable page manifest %s: %w; preserve the directory and restore a known-good backup", path, err)
		}
		manifest, err := decodePageManifest(data)
		if err != nil {
			return err
		}
		if current == nil || manifest.Generation > current.Generation {
			current, currentBytes = manifest, data
		} else if manifest.Generation == current.Generation && sha256.Sum256(data) != sha256.Sum256(currentBytes) {
			return fmt.Errorf("conflicting page manifests at generation %d", manifest.Generation)
		}
	}
	wal, err := readPageWAL(p.pages.walPath)
	if err != nil {
		return err
	}
	if wal.committed != nil {
		if current == nil || wal.committed.Generation > current.Generation {
			current, currentBytes = wal.committed, wal.committedBytes
		} else if wal.committed.Generation == current.Generation && sha256.Sum256(wal.committedBytes) != sha256.Sum256(currentBytes) {
			return fmt.Errorf("WAL conflicts with durable page manifest")
		}
	}
	if current == nil {
		if _, err := os.Stat(p.path + ".tmp"); err == nil {
			return fmt.Errorf("page manifest missing but recovery candidate %s exists; do not delete or overwrite it", p.path+".tmp")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if wal.hasRecords {
			if _, err := os.Stat(p.pages.legacyPath); errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("WAL recovery candidate contains no committed database; preserve the directory for recovery")
			} else if err != nil {
				return err
			}
		}
	}
	p.pages.manifest, p.pages.manifestBytes, p.pages.walBytes = current, currentBytes, wal.validBytes
	p.pages.initialized = true
	return nil
}

func (p *Persistence) pagePath(hash [32]byte) string {
	encoded := fmt.Sprintf("%x", hash)
	return filepath.Join(p.pages.pageDirectory, encoded[:2], encoded+".page")
}

func (p *Persistence) writePageAtomic(path, magic string, payload []byte) error {
	temporary := path + ".tmp"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		_ = file.Close()
		if !committed {
			_ = os.Remove(temporary)
		}
	}()
	if _, err := file.Write(pageFrame(magic, payload)); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := p.replaceFile(temporary, path); err != nil {
		return fmt.Errorf("replace persistence data without deleting the previous snapshot: %w", err)
	}
	committed = true
	return syncPageDirectory(filepath.Dir(path))
}

func (p *Persistence) ensurePage(data []byte, rows int, known map[[32]byte]rowPageRef) (rowPageRef, error) {
	ref := rowPageRef{Hash: sha256.Sum256(data), Rows: rows, Bytes: len(data)}
	if old, ok := known[ref.Hash]; ok && old.Hash == ref.Hash && old.Rows == ref.Rows && old.Bytes == ref.Bytes {
		return ref, nil
	}
	path := p.pagePath(ref.Hash)
	if _, err := os.Stat(path); err == nil {
		// Existing content-addressed files must still be checked. A truncated or
		// corrupted page must never be silently reused in a new committed manifest.
		old, err := readPageFile(path, pageDataMagic)
		if err != nil {
			return rowPageRef{}, err
		}
		if sha256.Sum256(old) != ref.Hash {
			return rowPageRef{}, fmt.Errorf("existing row page checksum mismatch")
		}
		return ref, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return rowPageRef{}, err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return rowPageRef{}, err
	}
	if err := syncPageDirectory(p.pages.pageDirectory); err != nil {
		return rowPageRef{}, err
	}
	if err := p.writePageAtomic(path, pageDataMagic, data); err != nil {
		return rowPageRef{}, err
	}
	p.pages.stats.PagesWritten++
	p.pages.stats.PageBytesWritten += uint64(len(data) + pageFrameSize)
	return ref, nil
}

func pageRowEstimate(row Row) int64 {
	size := int64(len(row))*80 + 24
	for _, value := range row {
		size += int64(len(value.Text))
	}
	return size
}

func (p *Persistence) savePages(store *Store) error {
	if err := p.initializePages(); err != nil {
		return err
	}
	if err := os.MkdirAll(p.pages.pageDirectory, 0o700); err != nil {
		return err
	}
	if err := syncPageDirectory(p.pages.directory); err != nil {
		return err
	}
	if err := syncPageDirectory(filepath.Dir(p.pages.directory)); err != nil {
		return err
	}
	manifest := &pageManifest{Version: pageFormatVersion, Generation: 1, Snapshot: store.persistenceSnapshot()}
	if p.pages.manifest != nil {
		if p.pages.manifest.Generation == ^uint64(0) {
			return fmt.Errorf("page generation exhausted")
		}
		manifest.Generation = p.pages.manifest.Generation + 1
	}
	known := make(map[[32]byte]rowPageRef)
	if p.pages.manifest != nil && p.pages.verified {
		for _, database := range p.pages.manifest.Databases {
			for _, table := range database.Tables {
				for _, ref := range table.Pages {
					known[ref.Hash] = ref
				}
			}
		}
	}
	manifest.Databases = make([]databasePageRefs, len(manifest.Snapshot.Databases))
	for databaseIndex := range manifest.Snapshot.Databases {
		database := &manifest.Snapshot.Databases[databaseIndex]
		manifest.Databases[databaseIndex].Tables = make([]tablePageRefs, len(database.Tables))
		for tableIndex := range database.Tables {
			table := &database.Tables[tableIndex]
			refs := &manifest.Databases[databaseIndex].Tables[tableIndex]
			if cold := table.coldRows; cold != nil {
				if cold.persistence != p {
					return fmt.Errorf("saving cold tables into another persistence directory requires materialization")
				}
				refs.Pages = append([]rowPageRef(nil), cold.refs...)
				refs.Indexes = append([]DiskIndexDescriptor(nil), cold.indexes...)
				refs.IndexFingerprints = append([][32]byte(nil), cold.indexFingerprints...)
				table.coldRows = nil
				continue
			}
			for start := 0; start < len(table.Rows); {
				end := start
				var estimated int64
				blockEnd := (start/pageMaxRows + 1) * pageMaxRows
				if blockEnd > len(table.Rows) {
					blockEnd = len(table.Rows)
				}
				for end < blockEnd {
					size := pageRowEstimate(table.Rows[end])
					if end > start && estimated+size > pageTargetBytes {
						break
					}
					estimated += size
					end++
				}
				if estimated > pageMaxPayload {
					return fmt.Errorf("table %q row exceeds paged storage limit of %d bytes", table.Name, pageMaxPayload)
				}
				encoded, err := pageGob(table.Rows[start:end])
				if err != nil {
					return err
				}
				ref, err := p.ensurePage(encoded, end-start, known)
				if err != nil {
					return err
				}
				ref.MemoryBytes = estimated
				refs.Pages = append(refs.Pages, ref)
				start = end
			}
			if err := p.buildTableDiskIndexes(database.Name, *table, refs); err != nil {
				return err
			}
			table.Rows = nil
		}
	}
	encoded, err := pageGob(manifest)
	if err != nil {
		return err
	}
	if err := p.appendPageWAL(walPrepare, manifest, encoded); err != nil {
		return err
	}
	if err := p.writePageAtomic(p.path, pageHeadMagic, encoded); err != nil {
		return err
	}
	// Atomic manifest publication is the commit point. A later error is an
	// indeterminate commit and the executor's existing fail-closed path applies.
	p.pages.manifest, p.pages.manifestBytes = manifest, encoded
	p.pages.verified = true
	if err := p.appendPageWAL(walCommit, manifest, encoded); err != nil {
		return err
	}
	p.pages.sinceCheckpoint++
	if p.pages.sinceCheckpoint >= pageCheckpointCommits || p.pages.walBytes >= pageCheckpointWALBytes {
		return p.checkpointPages()
	}
	return nil
}

func (p *Persistence) loadRowPage(ref rowPageRef) ([]Row, error) {
	data, ok := p.pages.cache.get(ref.Hash)
	if !ok {
		var err error
		data, err = readPageFile(p.pagePath(ref.Hash), pageDataMagic)
		if err != nil {
			return nil, err
		}
	}
	rows, err := decodeRowPage(data, ref)
	if err != nil {
		return nil, err
	}
	if !ok {
		p.pages.cache.put(ref.Hash, data)
	}
	return rows, nil
}

func (p *Persistence) loadPages() (*Store, error) {
	if err := p.initializePages(); err != nil {
		return nil, err
	}
	if p.pages.manifest == nil {
		legacy := NewPersistence(filepath.Dir(p.pages.directory))
		legacy.allowPagedDirectory = true
		return legacy.Load()
	}
	// Decode metadata afresh so populating rows cannot mutate the committed
	// manifest or a concurrent page iterator's immutable snapshot.
	manifest, err := decodePageManifest(p.pages.manifestBytes)
	if err != nil {
		return nil, err
	}
	for databaseIndex := range manifest.Snapshot.Databases {
		for tableIndex := range manifest.Snapshot.Databases[databaseIndex].Tables {
			table := &manifest.Snapshot.Databases[databaseIndex].Tables[tableIndex]
			refs := manifest.Databases[databaseIndex].Tables[tableIndex].Pages
			for _, ref := range refs {
				rows, err := p.loadRowPage(ref)
				if err != nil {
					return nil, fmt.Errorf("load table %q row page: %w", table.Name, err)
				}
				table.Rows = append(table.Rows, rows...)
			}
		}
	}
	store, err := newStoreFromSnapshot(manifest.Snapshot, true)
	if err == nil {
		p.pages.verified = true
	}
	return store, err
}

// Checkpoint durably records the current committed root before rotating the
// WAL. Unreferenced page files are reclaimed only when no iterator pins them.
func (p *Persistence) Checkpoint() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pages == nil {
		return nil
	}
	if err := p.initializePages(); err != nil {
		return err
	}
	return p.checkpointPages()
}

func (p *Persistence) checkpointPages() error {
	if p.pages.manifest == nil {
		return nil
	}
	if err := p.writePageAtomic(p.pages.checkpointPath, pageHeadMagic, p.pages.manifestBytes); err != nil {
		return err
	}
	// Keep the public head present even after recovery solely from the WAL.
	if err := p.writePageAtomic(p.path, pageHeadMagic, p.pages.manifestBytes); err != nil {
		return err
	}
	temporary := p.pages.walPath + ".tmp"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(temporary)
		return err
	}
	if err = file.Close(); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	if err = p.replaceFile(temporary, p.pages.walPath); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	if err := syncPageDirectory(p.pages.directory); err != nil {
		return err
	}
	p.pages.walBytes, p.pages.sinceCheckpoint = 0, 0
	p.pages.stats.Checkpoints++
	if p.pages.readers != 0 || p.pages.coldPins.Load() != 0 {
		return nil
	}
	var liveIndexes []DiskIndexDescriptor
	live := make(map[string]struct{})
	for _, database := range p.pages.manifest.Databases {
		for _, table := range database.Tables {
			liveIndexes = append(liveIndexes, table.Indexes...)
			for _, ref := range table.Pages {
				live[p.pagePath(ref.Hash)] = struct{}{}
			}
		}
	}
	// Walk only this backend's validated immutable page directory; never touch
	// legacy snapshots, user data directories, or arbitrary paths from metadata.
	if _, err := RemoveUnusedDiskIndexes(filepath.Join(p.pages.directory, "disk-indexes"), liveIndexes); err != nil {
		return err
	}
	return filepath.WalkDir(p.pages.pageDirectory, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".page") {
			return nil
		}
		if _, keep := live[path]; keep {
			return nil
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		p.pages.stats.PagesReclaimed++
		return nil
	})
}
