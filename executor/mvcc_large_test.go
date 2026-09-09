package executor

import (
	"context"
	"errors"
	"fmt"
	"gbaselite/storageengine"
	"strings"
	"testing"
	"time"
)

func TestMVCCConfiguredBudgetFailureAndOldSnapshot(t *testing.T) {
	e, err := OpenWithOptions(t.TempDir(), "root", "pw", OpenOptions{StorageMode: "mvcc", TransactionWriteBytes: 12 << 10})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	s := &Session{}
	run := func(s *Session, q string) *Result {
		t.Helper()
		r, err := e.Execute(s, q)
		if err != nil {
			t.Fatal(q, err)
		}
		return r
	}
	run(s, "CREATE DATABASE test")
	run(s, "USE test")
	run(s, "CREATE TABLE items(id BIGINT PRIMARY KEY,v INT,payload VARCHAR(512))")
	for i := 0; i < 100; i++ {
		run(s, fmt.Sprintf("INSERT INTO items VALUES(%d,1,'%s')", i, strings.Repeat("x", 256)))
	}
	old := &Session{CurrentDatabase: "test"}
	defer e.CloseSession(old)
	run(old, "BEGIN")
	if _, err = e.Execute(s, "UPDATE items SET v=v+1"); !errors.Is(err, storageengine.ErrWriteSetLimit) {
		t.Fatal("budget error", err)
	}
	if r := run(s, "SELECT COUNT(*),SUM(v) FROM items"); fmt.Sprint(r.Rows) != "[[100 100]]" {
		t.Fatal(r.Rows)
	}
	run(s, "UPDATE items SET v=2 WHERE id<10")
	if r := run(s, "SELECT COUNT(*),SUM(v) FROM items"); fmt.Sprint(r.Rows) != "[[100 110]]" {
		t.Fatal(r.Rows)
	}
	if r := run(old, "SELECT COUNT(*),SUM(v) FROM items"); fmt.Sprint(r.Rows) != "[[100 100]]" {
		t.Fatal("old snapshot", r.Rows)
	}
}

type deadlineRecordingBackend struct {
	storageengine.Engine
	deadlines []time.Duration
}

func (p *deadlineRecordingBackend) record(ctx context.Context) {
	if d, ok := ctx.Deadline(); ok {
		p.deadlines = append(p.deadlines, time.Until(d))
	} else {
		p.deadlines = append(p.deadlines, 0)
	}
}
func (p *deadlineRecordingBackend) Barrier(ctx context.Context) error {
	p.record(ctx)
	return p.Engine.Barrier(ctx)
}
func (p *deadlineRecordingBackend) Begin(ctx context.Context) (storageengine.Txn, error) {
	p.record(ctx)
	return p.Engine.Begin(ctx)
}
func (p *deadlineRecordingBackend) Status() storageengine.ReplicationStatus {
	return storageengine.ReplicationStatus{}
}
func TestMVCCReplicationDeadlineOnlyBoundsQuorum(t *testing.T) {
	e, err := OpenWithOptions(t.TempDir(), "root", "pw", OpenOptions{StorageMode: "mvcc"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { e.Replica = nil; e.Close() }()
	p := &deadlineRecordingBackend{Engine: e.Backend}
	e.Backend = p
	// Marker enables the replicated SQL branch; a recording proposer avoids any
	// real sleeping or dependency on network timing in this deadline policy test.
	e.Replica = p
	for _, limit := range []time.Duration{0, 90 * time.Second} {
		e.QueryOptions.Timeout = limit
		p.deadlines = nil
		if _, err = e.Execute(&Session{}, "SELECT 1"); err != nil {
			t.Fatal(err)
		}
		if len(p.deadlines) != 2 || p.deadlines[0] <= 0 || p.deadlines[0] > 5*time.Second {
			t.Fatal("quorum budget", p.deadlines)
		}
		if limit == 0 && p.deadlines[1] != 0 {
			t.Fatal("hidden statement deadline", p.deadlines)
		}
		if limit > 0 && (p.deadlines[1] < 88*time.Second || p.deadlines[1] > limit) {
			t.Fatal("user deadline shortened", p.deadlines)
		}
	}
}
