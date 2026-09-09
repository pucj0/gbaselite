//go:build windows

package atomicfile

import (
	"errors"
	"fmt"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWindowsReplaceRetryBoundaries(t *testing.T) {
	for _, test := range []struct {
		name                             string
		failure                          error
		successAt, wantCalls, wantPauses int
	}{
		{"immediate", nil, 1, 1, 0},
		{"transient", fmt.Errorf("wrapped: %w", windows.ERROR_ACCESS_DENIED), 3, 3, 2},
		{"persistent", windows.ERROR_SHARING_VIOLATION, 0, 6, 5},
		{"permission", windows.ERROR_ACCESS_DENIED, 0, 6, 5},
		{"missing", windows.ERROR_FILE_NOT_FOUND, 0, 1, 0},
		{"diskfull", windows.ERROR_DISK_FULL, 0, 1, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls, pauses := 0, 0
			err := retryWindowsReplace(func() error {
				calls++
				if calls == test.successAt {
					return nil
				}
				return test.failure
			}, func(delay time.Duration) {
				if delay != time.Millisecond<<pauses {
					t.Fatal(delay)
				}
				pauses++
			})
			if calls != test.wantCalls || pauses != test.wantPauses {
				t.Fatalf("calls %d pauses %d", calls, pauses)
			}
			if test.successAt > 0 {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, test.failure) {
				t.Fatal(err)
			}
		})
	}
}
func TestWindowsReplaceLockedDestinationKeepsBothFiles(t *testing.T) {
	dir := t.TempDir()
	source, target := filepath.Join(dir, "new.tmp"), filepath.Join(dir, "old.dat")
	for path, value := range map[string]string{source: "new", target: "old"} {
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	path, err := windows.UTF16PtrFromString(target)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(path, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := Replace(source, target); err == nil {
		windows.CloseHandle(handle)
		t.Fatal("locked destination replaced")
	}
	if err := windows.CloseHandle(handle); err != nil {
		t.Fatal(err)
	}
	for path, value := range map[string]string{source: "new", target: "old"} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != value {
			t.Fatalf("%s: %q %v", path, got, err)
		}
	}
	if err := Replace(source, target); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "new" {
		t.Fatalf("retry after releasing handle: %q %v", got, err)
	}
}
