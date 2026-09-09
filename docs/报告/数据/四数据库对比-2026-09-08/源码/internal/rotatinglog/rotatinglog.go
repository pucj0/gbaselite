package rotatinglog

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type File struct {
	mu            sync.Mutex
	directory     string
	path          string
	basePrefix    string
	rotatedName   *regexp.Regexp
	file          *os.File
	size          int64
	maxSize       int64
	retentionDays int
	now           func() time.Time
	nextCleanup   time.Time
	closed        bool
}

func Open(directory, name string, maxSize int64, retentionDays int) (*File, error) {
	if maxSize < 1 {
		return nil, fmt.Errorf("log maximum size must be positive")
	}
	if retentionDays < 0 || retentionDays > 365 {
		return nil, fmt.Errorf("log retention days must be 0 or between 1 and 365")
	}
	if filepath.Base(name) != name || name == "." || name == "" {
		return nil, fmt.Errorf("log name must be a file name")
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(directory, name)
	file, size, err := openCurrent(path)
	if err != nil {
		return nil, err
	}
	basePrefix := strings.TrimSuffix(name, filepath.Ext(name))
	rotatedName := regexp.MustCompile("^" + regexp.QuoteMeta(basePrefix) + `-\d{8}-\d{6}\.\d{9}Z(?:-\d+)?` + regexp.QuoteMeta(filepath.Ext(name)) + "$")
	logFile := &File{
		directory:     directory,
		path:          path,
		basePrefix:    basePrefix,
		rotatedName:   rotatedName,
		file:          file,
		size:          size,
		maxSize:       maxSize,
		retentionDays: retentionDays,
		now:           time.Now,
	}
	now := logFile.now()
	if err := logFile.pruneLocked(now); err != nil {
		fmt.Fprintf(os.Stderr, "GBaseLite log cleanup warning: %v\n", err)
	}
	logFile.nextCleanup = now.Add(24 * time.Hour)
	return logFile, nil
}

func (f *File) Write(data []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed || f.file == nil {
		return 0, os.ErrClosed
	}
	now := f.now()
	if f.retentionDays > 0 && !now.Before(f.nextCleanup) {
		if err := f.pruneLocked(now); err != nil {
			fmt.Fprintf(os.Stderr, "GBaseLite log cleanup warning: %v\n", err)
		}
		f.nextCleanup = now.Add(24 * time.Hour)
	}
	if f.size > 0 && f.size+int64(len(data)) > f.maxSize {
		if err := f.rotateLocked(now); err != nil {
			if f.file == nil {
				return 0, err
			}
			fmt.Fprintf(os.Stderr, "GBaseLite log rotation warning: %v\n", err)
		}
	}
	written, err := f.file.Write(data)
	f.size += int64(written)
	return written, err
}

func (f *File) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	if f.file == nil {
		return nil
	}
	err := f.file.Close()
	f.file = nil
	return err
}

func (f *File) rotateLocked(now time.Time) error {
	if err := f.file.Close(); err != nil {
		f.file = nil
		return errors.Join(fmt.Errorf("close current log before rotation: %w", err), f.reopenCurrent())
	}
	f.file = nil
	rotatedPath, pathErr := f.rotatedPath(now)
	if pathErr != nil {
		return errors.Join(pathErr, f.reopenCurrent())
	}
	if err := os.Rename(f.path, rotatedPath); err != nil {
		reopenErr := f.reopenCurrent()
		return errors.Join(fmt.Errorf("rename current log for rotation: %w", err), reopenErr)
	}
	newFile, size, openErr := openCurrent(f.path)
	if openErr != nil {
		rollbackErr := os.Rename(rotatedPath, f.path)
		reopenErr := f.reopenCurrent()
		return errors.Join(fmt.Errorf("open new current log: %w", openErr), rollbackErr, reopenErr)
	}
	f.file = newFile
	f.size = size
	if err := f.pruneLocked(now); err != nil {
		fmt.Fprintf(os.Stderr, "GBaseLite log cleanup warning: %v\n", err)
	}
	return nil
}

func (f *File) reopenCurrent() error {
	file, size, err := openCurrent(f.path)
	if err != nil {
		f.file = nil
		return fmt.Errorf("reopen current log after failed rotation: %w", err)
	}
	f.file = file
	f.size = size
	return nil
}

func (f *File) rotatedPath(now time.Time) (string, error) {
	extension := filepath.Ext(f.path)
	stamp := now.UTC().Format("20060102-150405.000000000Z")
	base := filepath.Join(f.directory, f.basePrefix+"-"+stamp)
	path := base + extension
	for suffix := 1; ; suffix++ {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return path, nil
		} else if err != nil {
			return "", fmt.Errorf("inspect rotated log destination: %w", err)
		}
		path = fmt.Sprintf("%s-%d%s", base, suffix, extension)
	}
}

func (f *File) pruneLocked(now time.Time) error {
	if f.retentionDays == 0 {
		return nil
	}
	pattern := filepath.Join(f.directory, f.basePrefix+"-*"+filepath.Ext(f.path))
	paths, err := filepath.Glob(pattern)
	if err != nil {
		return err
	}
	cutoff := now.AddDate(0, 0, -f.retentionDays)
	var cleanupErr error
	for _, path := range paths {
		if !f.rotatedName.MatchString(filepath.Base(path)) {
			continue
		}
		info, statErr := os.Stat(path)
		if statErr != nil {
			cleanupErr = errors.Join(cleanupErr, statErr)
			continue
		}
		if !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
			continue
		}
		if removeErr := os.Remove(path); removeErr != nil {
			cleanupErr = errors.Join(cleanupErr, removeErr)
		}
	}
	return cleanupErr
}

func openCurrent(path string) (*os.File, int64, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, 0, err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, 0, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, 0, err
	}
	return file, info.Size(), nil
}

var _ io.WriteCloser = (*File)(nil)
