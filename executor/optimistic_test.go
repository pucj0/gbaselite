package executor

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestLegacyOptimisticTransactionsReadSnapshotAndConflict(t *testing.T) {
	e, s, run := savepointEngine(t)
	e.OptimisticTransactions = true
	run(`INSERT INTO items(value) VALUES(10)`)
	a := &Session{CurrentDatabase: "sp"}
	b := &Session{CurrentDatabase: "sp"}
	defer e.CloseSession(a)
	defer e.CloseSession(b)
	for _, session := range []*Session{a, b} {
		if _, err := e.Execute(session, "BEGIN"); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 1)
	go func() { _, err := e.Execute(s, `UPDATE items SET value=20 WHERE id=1`); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("snapshot transaction blocked outside write")
	}
	result, err := e.Execute(a, `SELECT value FROM items WHERE id=1`)
	if err != nil || result.Rows[0][0] != int64(10) {
		t.Fatalf("snapshot changed: %+v %v", result, err)
	}
	if _, err := e.Execute(a, "COMMIT"); err != nil {
		t.Fatal(err)
	}
	if run(`SELECT value FROM items WHERE id=1`).Rows[0][0] != int64(20) {
		t.Fatal("read-only commit reverted write")
	}
	if _, err := e.Execute(b, `UPDATE items SET value=30 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(b, "COMMIT"); !errors.Is(err, ErrSerializationConflict) {
		t.Fatalf("expected conflict got %v", err)
	}
	if e.legacyState(b).transaction != nil {
		t.Fatal("conflicted transaction retained")
	}
	if run(`SELECT value FROM items WHERE id=1`).Rows[0][0] != int64(20) {
		t.Fatal("conflict overwrote committed row")
	}
	if _, err := e.Execute(b, "BEGIN"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(b, `UPDATE items SET value=30 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(b, "COMMIT"); err != nil {
		t.Fatal(err)
	}
	if run(`SELECT value FROM items WHERE id=1`).Rows[0][0] != int64(30) {
		t.Fatal("retry not committed")
	}
}

func TestLegacyLiteralBatchInsertAtomicFailure(t *testing.T) {
	e, s, run := savepointEngine(t)
	if _, err := e.Execute(s, `INSERT INTO items(id,value) VALUES(1,10),(2,10)`); err == nil {
		t.Fatal("expected duplicate unique")
	}
	if run(`SELECT COUNT(*) FROM items`).Rows[0][0] != int64(0) {
		t.Fatal("partial batch persisted")
	}
	run("BEGIN")
	if _, err := e.Execute(s, `INSERT INTO items(id,value) VALUES(1,10),(1,20)`); err == nil {
		t.Fatal("expected duplicate primary")
	}
	if run(`SELECT COUNT(*) FROM items`).Rows[0][0] != int64(0) {
		t.Fatal("partial batch in transaction")
	}
	run("COMMIT")
}

func TestLegacyOptimisticConcurrentAutoIncrementReservationsSurviveRollback(t *testing.T) {
	e, _, run := savepointEngine(t)
	e.OptimisticTransactions = true
	const count = 8
	sessions := make([]*Session, count)
	for i := range sessions {
		sessions[i] = &Session{CurrentDatabase: "sp"}
		if _, err := e.Execute(sessions[i], "BEGIN"); err != nil {
			t.Fatal(err)
		}
		defer e.CloseSession(sessions[i])
	}
	type outcome struct {
		id  uint64
		err error
	}
	results := make(chan outcome, count)
	for i, session := range sessions {
		go func(i int, s *Session) {
			r, err := e.Execute(s, fmt.Sprintf("INSERT INTO items(value) VALUES(%d)", i+100))
			if err != nil {
				results <- outcome{err: err}
				return
			}
			results <- outcome{id: r.LastInsertID}
		}(i, session)
	}
	seen := map[uint64]bool{}
	var max uint64
	for i := 0; i < count; i++ {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.id == 0 || seen[result.id] {
			t.Fatalf("duplicate/zero reservation %d", result.id)
		}
		seen[result.id] = true
		if result.id > max {
			max = result.id
		}
	}
	for _, session := range sessions {
		if _, err := e.Execute(session, "ROLLBACK"); err != nil {
			t.Fatal(err)
		}
	}
	result := run("INSERT INTO items(value) VALUES(999)")
	if result.LastInsertID <= max {
		t.Fatalf("reservation reused: next=%d previous=%d", result.LastInsertID, max)
	}
}

func TestLegacyOptimisticDMLWaitsForCommitGateAndCanTimeout(t *testing.T) {
	e, _, _ := savepointEngine(t)
	e.OptimisticTransactions = true
	session := &Session{CurrentDatabase: "sp"}
	if _, err := e.Execute(session, "BEGIN"); err != nil {
		t.Fatal(err)
	}
	defer e.CloseSession(session)
	e.QueryOptions.Timeout = 20 * time.Millisecond
	e.txGate.Lock()
	done := make(chan error, 1)
	go func() { _, err := e.Execute(session, "INSERT INTO items(value) VALUES(123)"); done <- err }()
	select {
	case err := <-done:
		e.txGate.Unlock()
		if !errors.Is(err, ErrQueryTimeout) {
			t.Fatalf("mutation escaped commit gate: %v", err)
		}
	case <-time.After(2 * time.Second):
		e.txGate.Unlock()
		<-done
		t.Fatal("mutation wait did not cancel")
	}
	e.QueryOptions = QueryOptions{}
	if _, err := e.Execute(session, "INSERT INTO items(value) VALUES(123)"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(session, "COMMIT"); err != nil {
		t.Fatal(err)
	}
}
