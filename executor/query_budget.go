package executor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// QueryOptions are optional execution safeguards. Zero values preserve existing
// behavior. Memory limits account for retained SQL values, not process RSS.
// Configure the engine before serving requests.
type QueryOptions struct {
	Timeout           time.Duration
	SortMemoryBytes   int64
	ResultMemoryBytes int64
	MaxTempBytes      int64
	TempDirectory     string
}

var (
	ErrQueryTimeout       = errors.New("query execution deadline exceeded")
	ErrQueryCanceled      = errors.New("query execution canceled")
	ErrQueryResourceLimit = errors.New("query resource limit exceeded")
)

type queryControl struct {
	context   context.Context
	deadline  time.Time
	options   QueryOptions
	temporary *queryTemporaryBudget
}

func newQueryControl(ctx context.Context, options QueryOptions) *queryControl {
	if ctx == nil {
		ctx = context.Background()
	}
	q := &queryControl{context: ctx, options: options, temporary: &queryTemporaryBudget{}}
	if options.Timeout > 0 {
		q.deadline = time.Now().Add(options.Timeout)
	}
	return q
}

func (q *queryControl) check() error {
	if q == nil {
		return nil
	}
	if err := q.context.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return ErrQueryTimeout
		}
		return ErrQueryCanceled
	}
	if !q.deadline.IsZero() && !time.Now().Before(q.deadline) {
		return ErrQueryTimeout
	}
	return nil
}

func (q *queryControl) hasDeadline() bool {
	return q != nil && (!q.deadline.IsZero() || q.context.Done() != nil)
}

// acquireQueryMutex never leaves a goroutine waiting on a lock after timeout.
// The ordinary blocking lock remains the fast path when safeguards are disabled.
func acquireQueryMutex(q *queryControl, mutex *sync.RWMutex, write bool) error {
	if err := q.check(); err != nil {
		return err
	}
	if !q.hasDeadline() {
		if write {
			mutex.Lock()
		} else {
			mutex.RLock()
		}
		return nil
	}
	try := mutex.TryRLock
	unlock := mutex.RUnlock
	if write {
		try, unlock = mutex.TryLock, mutex.Unlock
	}
	delay := time.Millisecond
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		if try() {
			if err := q.check(); err != nil {
				unlock()
				return err
			}
			return nil
		}
		select {
		case <-q.context.Done():
			return q.check()
		case <-timer.C:
			if err := q.check(); err != nil {
				return err
			}
			if delay < 8*time.Millisecond {
				delay *= 2
			}
			timer.Reset(delay)
		}
	}
}

// A conservative estimate includes interface and slice headers plus immutable
// variable-sized payloads. Sharing a string can make actual usage lower.
func queryRowBytes(row []any) int64 {
	n := int64(48 + 24*len(row))
	for _, value := range row {
		switch v := value.(type) {
		case string:
			n += int64(len(v))
		case jsonDocument:
			n += int64(len(v))
		case []byte:
			n += int64(len(v))
		case collatedText:
			n += int64(len(v.Text) + len(v.Collation))
		case time.Time:
			n += 32
		case interface{ String() string }:
			n += int64(len(v.String()))
		}
	}
	return n
}

func checkResultMemory(limit, used int64, row []any) (int64, error) {
	size := queryRowBytes(row)
	if used > int64(^uint64(0)>>1)-size || limit > 0 && size > limit-used {
		return used, fmt.Errorf("%w: materialized result exceeds %d bytes; use streaming or a smaller LIMIT", ErrQueryResourceLimit, limit)
	}
	return used + size, nil
}

type queryTemporaryBudget struct {
	mu   sync.Mutex
	used int64
}

func (q *queryControl) reserveTemporary(size int64) error {
	if q == nil || q.temporary == nil {
		return nil
	}
	q.temporary.mu.Lock()
	defer q.temporary.mu.Unlock()
	if q.options.MaxTempBytes > 0 && size > q.options.MaxTempBytes-q.temporary.used {
		return fmt.Errorf("%w: temporary query data exceeds %d bytes", ErrQueryResourceLimit, q.options.MaxTempBytes)
	}
	q.temporary.used += size
	return nil
}
func (q *queryControl) releaseTemporary(size int64) {
	if q == nil || q.temporary == nil {
		return
	}
	q.temporary.mu.Lock()
	q.temporary.used -= size
	q.temporary.mu.Unlock()
}
