package storage

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gbaselite/internal/atomicfile"
)

func pagedTestStore(t *testing.T, count int) (*Store, *Table) {
	t.Helper()
	store := NewStore()
	database, err := store.CreateDatabase("paged")
	if err != nil {
		t.Fatal(err)
	}
	table, err := database.CreateTableWithIndexes("items", []Column{{Name: "id", Type: TypeInt}, {Name: "value", Type: TypeText}}, []string{"id"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < count; index++ {
		if err := table.Insert(NewRow(MustValue(TypeInt, index), MustValue(TypeText, strings.Repeat("x", 128)))); err != nil {
			t.Fatal(err)
		}
	}
	return store, table
}

func pagedTestRows(t *testing.T, p *Persistence) []Row {
	t.Helper()
	store, err := p.Load()
	if err != nil {
		t.Fatal(err)
	}
	database, err := store.Database("paged")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := database.Select("items", nil)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestPagedPersistenceReusesUnchangedPagesAndReopens(t *testing.T) {
	directory := t.TempDir()
	store, table := pagedTestStore(t, 1024)
	p := NewPagedPersistence(directory, 8<<10)
	if err := p.Save(store); err != nil {
		t.Fatal(err)
	}
	first := p.PagedStats()
	if first.PagesWritten < 4 {
		t.Fatalf("not split into pages: %#v", first)
	}
	if _, err := table.Update(func(row Row) bool { return row[0].Int64 == 510 }, map[string]Value{"value": MustValue(TypeText, "changed")}); err != nil {
		t.Fatal(err)
	}
	if err := p.Save(store); err != nil {
		t.Fatal(err)
	}
	second := p.PagedStats()
	if delta := second.PagesWritten - first.PagesWritten; delta != 1 {
		t.Fatalf("point update wrote %d pages", delta)
	}
	if err := p.Save(store); err != nil {
		t.Fatal(err)
	}
	if p.PagedStats().PagesWritten != second.PagesWritten {
		t.Fatal("unchanged save rewrote pages")
	}
	reopened := NewPagedPersistence(directory, 8<<10)
	rows := pagedTestRows(t, reopened)
	if len(rows) != 1024 || rows[510][1].Text != "changed" {
		t.Fatalf("recovered unexpected rows")
	}
	stats := reopened.PagedStats()
	if stats.CacheBytes > stats.CacheBudgetBytes {
		t.Fatalf("cache exceeded budget: %#v", stats)
	}
	if stats.Generation != 3 {
		t.Fatalf("generation = %d", stats.Generation)
	}
}

func TestPagedMigrationPreservesLegacyAndExportsPortableSnapshot(t *testing.T) {
	directory := t.TempDir()
	store, table := pagedTestStore(t, 3)
	legacy := NewPersistence(directory)
	if err := legacy.Save(store); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(legacy.Path())
	if err != nil {
		t.Fatal(err)
	}
	p := NewPagedPersistence(directory, 0)
	loaded, err := p.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Save(loaded); err != nil {
		t.Fatal(err)
	}
	if _, err := table.Update(nil, map[string]Value{"value": MustValue(TypeText, "new")}); err != nil {
		t.Fatal(err)
	}
	if err := p.Save(store); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(legacy.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("migration modified legacy snapshot")
	}
	exported := filepath.Join(t.TempDir(), "portable.gob")
	if err := p.ExportSnapshot(exported); err != nil {
		t.Fatal(err)
	}
	inspection, err := InspectSnapshot(exported)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Rows != 3 {
		t.Fatalf("export rows = %d", inspection.Rows)
	}
	exportReader := NewPersistence(t.TempDir())
	exportReader.path = exported
	if got := pagedTestRows(t, exportReader)[0][1].Text; got != "new" {
		t.Fatalf("export value = %q", got)
	}
	if err := p.ExportSnapshot(p.Path()); err == nil {
		t.Fatal("allowed overwriting active manifest")
	}
}

func TestPagedFailedRootPublicationDoesNotReplayPrepare(t *testing.T) {
	directory := t.TempDir()
	store, table := pagedTestStore(t, 5)
	p := NewPagedPersistence(directory, 0)
	if err := p.Save(store); err != nil {
		t.Fatal(err)
	}
	if err := table.Insert(NewRow(MustValue(TypeInt, 5), MustValue(TypeText, "new"))); err != nil {
		t.Fatal(err)
	}
	p.replaceFile = func(from, to string) error {
		if to == p.Path() {
			return errors.New("injected manifest replacement failure")
		}
		return atomicfile.Replace(from, to)
	}
	if err := p.Save(store); err == nil {
		t.Fatal("save unexpectedly succeeded")
	}
	if _, err := os.Stat(p.Path() + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary manifest left: %v", err)
	}
	if got := len(pagedTestRows(t, NewPagedPersistence(directory, 0))); got != 5 {
		t.Fatalf("replayed uncommitted prepare: %d rows", got)
	}
	// Even when the head is unavailable, only the earlier COMMIT is recoverable.
	if err := os.Remove(p.Path()); err != nil {
		t.Fatal(err)
	}
	if got := len(pagedTestRows(t, NewPagedPersistence(directory, 0))); got != 5 {
		t.Fatalf("WAL recovery replayed prepare: %d", got)
	}
}

func TestPagedWALRecoversCommittedMissingHead(t *testing.T) {
	directory := t.TempDir()
	store, _ := pagedTestStore(t, 8)
	p := NewPagedPersistence(directory, 0)
	if err := p.Save(store); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p.Path()); err != nil {
		t.Fatal(err)
	}
	recovered := NewPagedPersistence(directory, 0)
	if len(pagedTestRows(t, recovered)) != 8 {
		t.Fatal("missing recovered rows")
	}
	if err := recovered.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.Path()); err != nil {
		t.Fatal("checkpoint did not restore head")
	}
	info, err := os.Stat(p.pages.walPath)
	if err != nil || info.Size() != 0 {
		t.Fatalf("WAL not rotated: %v", err)
	}
	if len(pagedTestRows(t, NewPagedPersistence(directory, 0))) != 8 {
		t.Fatal("checkpoint not recoverable")
	}
}

func TestPagedWALTornTailAndChecksumCorruption(t *testing.T) {
	for _, cut := range []int{1, 8, 16, 23} {
		t.Run(strconv.Itoa(cut), func(t *testing.T) {
			directory := t.TempDir()
			store, _ := pagedTestStore(t, 2)
			p := NewPagedPersistence(directory, 0)
			if err := p.Save(store); err != nil {
				t.Fatal(err)
			}
			payload, err := pageGob(pageWALRecord{Version: pageFormatVersion, Kind: walPrepare, Generation: 2, Manifest: []byte("interrupted")})
			if err != nil {
				t.Fatal(err)
			}
			frame := pageFrame(pageWALMagic, payload)
			file, err := os.OpenFile(p.pages.walPath, os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.Write(frame[:cut]); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			recovered := NewPagedPersistence(directory, 0)
			loaded, err := recovered.Load()
			if err != nil {
				t.Fatal(err)
			}
			if err := recovered.Save(loaded); err != nil {
				t.Fatalf("append after torn tail: %v", err)
			}
			if len(pagedTestRows(t, NewPagedPersistence(directory, 0))) != 2 {
				t.Fatal("torn tail changed data")
			}
		})
	}
	directory := t.TempDir()
	store, _ := pagedTestStore(t, 2)
	p := NewPagedPersistence(directory, 0)
	if err := p.Save(store); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p.pages.walPath)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 0x40
	if err := os.WriteFile(p.pages.walPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPagedPersistence(directory, 0).Load(); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("WAL corruption error = %v", err)
	}
}

func TestPagedRejectsPageAndManifestCorruption(t *testing.T) {
	for _, target := range []string{"page", "manifest"} {
		t.Run(target, func(t *testing.T) {
			directory := t.TempDir()
			store, _ := pagedTestStore(t, 3)
			p := NewPagedPersistence(directory, 0)
			if err := p.Save(store); err != nil {
				t.Fatal(err)
			}
			path := p.Path()
			if target == "page" {
				path = p.pagePath(p.pages.manifest.Databases[0].Tables[0].Pages[0].Hash)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			data[len(data)-1] ^= 1
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := NewPagedPersistence(directory, 0).Load(); err == nil || !strings.Contains(err.Error(), "checksum") {
				t.Fatalf("corruption accepted: %v", err)
			}
		})
	}
}

func TestPagedIteratorPinsOldPagesThroughCheckpoint(t *testing.T) {
	directory := t.TempDir()
	store, table := pagedTestStore(t, 512)
	p := NewPagedPersistence(directory, 2<<10)
	if err := p.Save(store); err != nil {
		t.Fatal(err)
	}
	reader, err := p.OpenRowPages("PAGED", "ITEMS")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	oldRef := p.pages.manifest.Databases[0].Tables[0].Pages[0]
	if _, err := table.Update(nil, map[string]Value{"value": MustValue(TypeText, "updated")}); err != nil {
		t.Fatal(err)
	}
	if err := p.Save(store); err != nil {
		t.Fatal(err)
	}
	if err := p.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.pagePath(oldRef.Hash)); err != nil {
		t.Fatal("checkpoint removed pinned page")
	}
	count := 0
	for {
		row, ok, err := reader.Next()
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		if row[1].Text != strings.Repeat("x", 128) {
			t.Fatal("iterator observed newer transaction")
		}
		count++
	}
	if count != 512 {
		t.Fatalf("iterator rows = %d", count)
	}
	if err := p.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.pagePath(oldRef.Hash)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unused page not reclaimed: %v", err)
	}
	if p.PagedStats().CacheBytes > 2<<10 {
		t.Fatal("cache exceeded its budget")
	}
}

func TestRowPageCacheEvictsAndRejectsOversize(t *testing.T) {
	cache := newRowPageCache(10)
	first, second, third := [32]byte{1}, [32]byte{2}, [32]byte{3}
	cache.put(first, make([]byte, 6))
	cache.put(second, make([]byte, 4))
	if _, ok := cache.get(first); !ok {
		t.Fatal("missing first page")
	}
	cache.put(third, make([]byte, 5))
	if cache.used != 5 {
		t.Fatalf("retained = %d", cache.used)
	}
	cache.put(first, make([]byte, 11))
	if cache.used != 5 {
		t.Fatal("oversize page entered cache")
	}
}

func TestPagedCrashRecoveryAtCommitBoundaries(t *testing.T) {
	for _, phase := range []string{"before-root", "after-root"} {
		t.Run(phase, func(t *testing.T) {
			directory := t.TempDir()
			store, _ := pagedTestStore(t, 1)
			p := NewPagedPersistence(directory, 0)
			if err := p.Save(store); err != nil {
				t.Fatal(err)
			}
			child := exec.Command(os.Args[0], "-test.run=^TestPagedCrashHelper$")
			child.Env = append(os.Environ(), "GBL_PAGE_CRASH_PHASE="+phase, "GBL_PAGE_CRASH_DIR="+directory)
			output, err := child.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 37 {
				t.Fatalf("crash helper = %v, %s", err, output)
			}
			rows := pagedTestRows(t, NewPagedPersistence(directory, 0))
			want := 1
			if phase == "after-root" {
				want = 2
			}
			if len(rows) != want {
				t.Fatalf("phase %s recovered %d rows; want %d", phase, len(rows), want)
			}
		})
	}
}

func TestPagedCrashHelper(t *testing.T) {
	phase := os.Getenv("GBL_PAGE_CRASH_PHASE")
	if phase == "" {
		return
	}
	p := NewPagedPersistence(os.Getenv("GBL_PAGE_CRASH_DIR"), 0)
	store, err := p.Load()
	if err != nil {
		t.Fatal(err)
	}
	database, err := store.Database("paged")
	if err != nil {
		t.Fatal(err)
	}
	table, err := database.Table("items")
	if err != nil {
		t.Fatal(err)
	}
	if err := table.Insert(NewRow(MustValue(TypeInt, 1), MustValue(TypeText, "crash-test"))); err != nil {
		t.Fatal(err)
	}
	p.replaceFile = func(from, to string) error {
		if to == p.Path() && phase == "before-root" {
			os.Exit(37)
		}
		err := atomicfile.Replace(from, to)
		if err == nil && to == p.Path() && phase == "after-root" {
			os.Exit(37)
		}
		return err
	}
	if err := p.Save(store); err != nil {
		t.Fatal(err)
	}
	t.Fatal("crash hook was not reached")
}

func TestSnapshotModeRefusesPagedDirectory(t *testing.T) {
	directory := t.TempDir()
	store, _ := pagedTestStore(t, 1)
	legacy := NewPersistence(directory)
	if err := legacy.Save(store); err != nil {
		t.Fatal(err)
	}
	p := NewPagedPersistence(directory, 0)
	if err := p.Save(store); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Load(); err == nil || !strings.Contains(err.Error(), "use migrate-legacy") {
		t.Fatalf("downgrade load = %v", err)
	}
	if err := legacy.Save(store); err == nil || !strings.Contains(err.Error(), "use migrate-legacy") {
		t.Fatalf("downgrade save = %v", err)
	}
}
