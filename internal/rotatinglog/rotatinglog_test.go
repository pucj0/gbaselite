package rotatinglog

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRotatesBeforeConfiguredSizeAndPreservesWrites(t *testing.T) {
	directory := t.TempDir()
	file, err := Open(directory, "gbaselite.log", 10, 7)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("123456")); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("abcdef")); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	rotated, err := filepath.Glob(filepath.Join(directory, "gbaselite-*.log"))
	if err != nil || len(rotated) != 1 {
		t.Fatalf("rotated logs = %#v, %v", rotated, err)
	}
	assertFileContents(t, rotated[0], "123456")
	assertFileContents(t, filepath.Join(directory, "gbaselite.log"), "abcdef")
}

func TestRetentionPrunesOnlyExpiredRotatedLogs(t *testing.T) {
	directory := t.TempDir()
	oldPath := filepath.Join(directory, "gbaselite-20200101-000000.000000000Z.log")
	recentPath := filepath.Join(directory, "gbaselite-20990101-000000.000000000Z.log")
	unrelatedPath := filepath.Join(directory, "other.log")
	manualPath := filepath.Join(directory, "gbaselite-manual.log")
	for _, path := range []string{oldPath, recentPath, unrelatedPath, manualPath} {
		if err := os.WriteFile(path, []byte("log"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	if err := os.Chtimes(oldPath, now.AddDate(0, 0, -8), now.AddDate(0, 0, -8)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(recentPath, now.AddDate(0, 0, -1), now.AddDate(0, 0, -1)); err != nil {
		t.Fatal(err)
	}
	file, err := Open(directory, "gbaselite.log", 20, 7)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("expired rotated log still exists: %v", err)
	}
	for _, path := range []string{recentPath, unrelatedPath, manualPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("retained file %s: %v", path, err)
		}
	}
}

func TestActiveWriterPrunesExpiredRotationsAtDailyInterval(t *testing.T) {
	directory := t.TempDir()
	file, err := Open(directory, "gbaselite.log", 1024, 7)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	oldPath := filepath.Join(directory, "gbaselite-20200101-000000.000000000Z.log")
	if err := os.WriteFile(oldPath, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().AddDate(0, 0, -8)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(25 * time.Hour)
	file.now = func() time.Time { return future }
	if _, err := file.Write([]byte("trigger cleanup")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("active writer did not prune expired rotation: %v", err)
	}
}

func TestZeroRetentionKeepsExpiredRotatedLogs(t *testing.T) {
	directory := t.TempDir()
	oldPath := filepath.Join(directory, "gbaselite-20200101-000000.000000000Z.log")
	if err := os.WriteFile(oldPath, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().AddDate(-10, 0, 0)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	file, err := Open(directory, "gbaselite.log", 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("permanent retention removed rotated log: %v", err)
	}
}

func TestConcurrentWritesRemainCompleteAcrossRotations(t *testing.T) {
	directory := t.TempDir()
	file, err := Open(directory, "gbaselite.log", 128, 7)
	if err != nil {
		t.Fatal(err)
	}
	const writers = 12
	const writesPerWorker = 50
	const line = "worker-log-line\n"
	var wait sync.WaitGroup
	for range writers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for range writesPerWorker {
				if _, err := file.Write([]byte(line)); err != nil {
					t.Errorf("write: %v", err)
					return
				}
			}
		}()
	}
	wait.Wait()
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(directory, "gbaselite*.log"))
	if err != nil {
		t.Fatal(err)
	}
	var contents strings.Builder
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		contents.Write(data)
	}
	if got := strings.Count(contents.String(), line); got != writers*writesPerWorker {
		t.Fatalf("log line count = %d, want %d", got, writers*writesPerWorker)
	}
}

func assertFileContents(t *testing.T, path, want string) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != want {
		t.Fatalf("%s = %q, want %q", path, contents, want)
	}
}
