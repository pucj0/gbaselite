package journal

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const (
	MinRetentionDays     = 0
	DefaultRetentionDays = 7
	MaxRetentionDays     = 365
	retentionCheckPeriod = 24 * time.Hour
)

type retainedJSONL struct {
	path          string
	file          *os.File
	retentionDays int
	lastPrune     time.Time
	now           func() time.Time
}

func openRetainedJSONL(path string, retentionDays int) (*retainedJSONL, error) {
	if err := validateRetentionDays(retentionDays); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if retentionDays > 0 {
		if _, err := pruneJSONL(path, now.Add(-time.Duration(retentionDays)*24*time.Hour)); err != nil {
			return nil, err
		}
	}
	file, err := openAppendFile(path)
	if err != nil {
		return nil, err
	}
	return &retainedJSONL{path: path, file: file, retentionDays: retentionDays, lastPrune: now, now: time.Now}, nil
}

func validateRetentionDays(days int) error {
	if days < MinRetentionDays || days > MaxRetentionDays {
		return fmt.Errorf("retention days must be between %d and %d", MinRetentionDays, MaxRetentionDays)
	}
	return nil
}

func (l *retainedJSONL) currentTime() time.Time {
	return l.now().UTC()
}

func (l *retainedJSONL) pruneDue(now time.Time) bool {
	return l.retentionDays > 0 && now.Sub(l.lastPrune) >= retentionCheckPeriod
}

func (l *retainedJSONL) prepareAppend(now time.Time) error {
	if !l.pruneDue(now) {
		return nil
	}
	if err := l.file.Close(); err != nil {
		return err
	}
	_, pruneErr := pruneJSONL(l.path, now.Add(-time.Duration(l.retentionDays)*24*time.Hour))
	file, openErr := openAppendFile(l.path)
	if openErr != nil {
		if pruneErr != nil {
			return fmt.Errorf("prune log: %v; reopen log: %w", pruneErr, openErr)
		}
		return openErr
	}
	l.file = file
	l.lastPrune = now
	if pruneErr != nil {
		return fmt.Errorf("prune log: %w", pruneErr)
	}
	return nil
}

func (l *retainedJSONL) write(payload []byte) error {
	info, err := l.file.Stat()
	if err != nil {
		return err
	}
	start := info.Size()
	if _, err := l.file.Write(payload); err != nil {
		_ = l.file.Truncate(start)
		return err
	}
	if err := l.file.Sync(); err != nil {
		_ = l.file.Truncate(start)
		return err
	}
	return nil
}

func (l *retainedJSONL) close() error {
	return l.file.Close()
}

func pruneJSONL(path string, cutoff time.Time) (bool, error) {
	source, err := os.Open(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	sourceClosed := false
	defer func() {
		if !sourceClosed {
			_ = source.Close()
		}
	}()

	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".retention-*")
	if err != nil {
		return false, err
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		_ = temporary.Close()
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return false, err
	}

	decoder := json.NewDecoder(bufio.NewReader(source))
	writer := bufio.NewWriter(temporary)
	dropped := false
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			if err == io.EOF {
				break
			}
			return false, fmt.Errorf("decode retained log: %w", err)
		}
		var metadata struct {
			Timestamp time.Time `json:"timestamp"`
		}
		if err := json.Unmarshal(raw, &metadata); err != nil {
			return false, fmt.Errorf("decode retained log timestamp: %w", err)
		}
		if !metadata.Timestamp.IsZero() && metadata.Timestamp.Before(cutoff) {
			dropped = true
			continue
		}
		if _, err := writer.Write(raw); err != nil {
			return false, err
		}
		if err := writer.WriteByte('\n'); err != nil {
			return false, err
		}
	}
	if err := writer.Flush(); err != nil {
		return false, err
	}
	if err := temporary.Sync(); err != nil {
		return false, err
	}
	if err := temporary.Close(); err != nil {
		return false, err
	}
	if !dropped {
		return false, nil
	}
	if err := source.Close(); err != nil {
		return false, err
	}
	sourceClosed = true
	if err := replaceFile(temporaryPath, path); err != nil {
		return false, err
	}
	removeTemporary = false
	return true, nil
}
