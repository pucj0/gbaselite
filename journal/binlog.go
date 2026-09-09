package journal

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const BinlogVersion = 2

// AutoIncrementState preserves reservations consumed by rolled-back inserts.
type AutoIncrementState struct {
	Database string `json:"database"`
	Table    string `json:"table"`
	Column   string `json:"column"`
	Next     int64  `json:"next"`
}

type BinlogStatement struct {
	ForeignKeyChecksDisabled bool                 `json:"foreign_key_checks_disabled,omitempty"`
	AutoIncrement            []AutoIncrementState `json:"auto_increment,omitempty"`
	Database                 string               `json:"database,omitempty"`
	SQL                      string               `json:"sql"`
	AffectedRows             uint64               `json:"affected_rows"`
}

type BinlogRecord struct {
	Version      int               `json:"version"`
	Sequence     uint64            `json:"sequence"`
	Timestamp    time.Time         `json:"timestamp"`
	Transaction  string            `json:"transaction"`
	SessionID    string            `json:"session_id,omitempty"`
	ConnectionID uint32            `json:"connection_id,omitempty"`
	Username     string            `json:"username,omitempty"`
	RemoteIP     string            `json:"remote_ip,omitempty"`
	Statements   []BinlogStatement `json:"statements"`
}

type Binlog struct {
	log      *retainedJSONL
	mu       sync.Mutex
	sequence uint64
}

func OpenBinlog(path string, retentionDays int) (*Binlog, error) {
	if err := validateRetentionDays(retentionDays); err != nil {
		return nil, err
	}
	sequence, err := LastBinlogSequence(path)
	if err != nil {
		return nil, err
	}
	if err := writeSequenceState(path, sequence); err != nil {
		return nil, err
	}
	retained, err := openRetainedJSONL(path, retentionDays)
	if err != nil {
		return nil, err
	}
	return &Binlog{log: retained, sequence: sequence}, nil
}

func (l *Binlog) Append(record BinlogRecord) error {
	if l == nil || len(record.Statements) == 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.log.currentTime()
	if l.log.pruneDue(now) {
		if err := writeSequenceState(l.log.path, l.sequence); err != nil {
			return err
		}
	}
	if err := l.log.prepareAppend(now); err != nil {
		return err
	}
	l.sequence++
	record.Version = 1
	for _, statement := range record.Statements {
		if len(statement.AutoIncrement) > 0 {
			record.Version = BinlogVersion
			break
		}
	}
	for _, statement := range record.Statements {
		if statement.ForeignKeyChecksDisabled {
			record.Version = 3
		}
	}
	if err := validateBinlogRecord(record); err != nil {
		l.sequence--
		return err
	}
	record.Sequence = l.sequence
	if record.Timestamp.IsZero() {
		record.Timestamp = time.Now().UTC()
	}
	if record.Transaction == "" {
		record.Transaction = fmt.Sprintf("%d-%d", record.Timestamp.UnixNano(), record.Sequence)
	}
	payload, err := json.Marshal(record)
	if err != nil {
		l.sequence--
		return err
	}
	payload = append(payload, '\n')
	if err := l.log.write(payload); err != nil {
		l.sequence--
		return err
	}
	return nil
}

func (l *Binlog) Sequence() uint64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sequence
}

func (l *Binlog) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.log.close()
}

func LastBinlogSequence(path string) (uint64, error) {
	stateSequence, err := readSequenceState(path)
	if err != nil {
		return 0, err
	}
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return stateSequence, nil
	}
	if err != nil {
		return 0, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	last := stateSequence
	var previous uint64
	for {
		var record BinlogRecord
		if err := decoder.Decode(&record); err != nil {
			if err == io.EOF {
				return last, nil
			}
			return 0, fmt.Errorf("decode binlog: %w", err)
		}
		if err := validateBinlogRecord(record); err != nil {
			return 0, err
		}
		if record.Sequence <= previous {
			return 0, fmt.Errorf("binlog sequence %d is not greater than %d", record.Sequence, previous)
		}
		previous = record.Sequence
		if record.Sequence > last {
			last = record.Sequence
		}
	}
}

type ReplayOptions struct {
	AfterSequence uint64
	Until         time.Time
}

func ReadBinlog(path string, options ReplayOptions, apply func(BinlogRecord) error) (int, uint64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	count := 0
	var lastSeen uint64
	var lastApplied uint64
	for {
		var record BinlogRecord
		if err := decoder.Decode(&record); err != nil {
			if err == io.EOF {
				return count, lastApplied, nil
			}
			return count, lastApplied, fmt.Errorf("decode binlog: %w", err)
		}
		if err := validateBinlogRecord(record); err != nil {
			return count, lastApplied, err
		}
		if record.Sequence <= lastSeen {
			return count, lastApplied, fmt.Errorf("binlog sequence %d is not greater than %d", record.Sequence, lastSeen)
		}
		lastSeen = record.Sequence
		if record.Sequence <= options.AfterSequence || !options.Until.IsZero() && record.Timestamp.After(options.Until) {
			continue
		}
		if err := apply(record); err != nil {
			return count, lastApplied, fmt.Errorf("replay binlog sequence %d: %w", record.Sequence, err)
		}
		count++
		lastApplied = record.Sequence
	}
}

func readSequenceState(path string) (uint64, error) {
	content, err := os.ReadFile(path + ".sequence")
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	sequence, err := strconv.ParseUint(strings.TrimSpace(string(content)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("decode binlog sequence state: %w", err)
	}
	return sequence, nil
}

func writeSequenceState(path string, sequence uint64) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("log path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".sequence-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := fmt.Fprintf(temporary, "%d\n", sequence); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return replaceFile(temporaryPath, path+".sequence")
}

func validateBinlogRecord(record BinlogRecord) error {
	if record.Version != 1 && record.Version != BinlogVersion && record.Version != 3 {
		return fmt.Errorf("unsupported binlog version %d", record.Version)
	}
	for _, statement := range record.Statements {
		if statement.ForeignKeyChecksDisabled && record.Version < 3 {
			return fmt.Errorf("foreign key session state requires binlog version 3")
		}
		if len(statement.AutoIncrement) == 0 {
			continue
		}
		if record.Version < 2 || statement.SQL != "" {
			return fmt.Errorf("invalid auto increment control record")
		}
		for _, state := range statement.AutoIncrement {
			if state.Next < 1 || strings.TrimSpace(state.Database) == "" || strings.TrimSpace(state.Table) == "" || strings.TrimSpace(state.Column) == "" {
				return fmt.Errorf("invalid auto increment control state")
			}
		}
	}
	return nil
}
