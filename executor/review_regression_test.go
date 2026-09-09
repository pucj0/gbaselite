package executor

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"gbaselite/storage"
)

// A transaction must not advance the autoincrement counter of a table that a
// concurrent commit has already replaced in the live store.
func TestReviewOptimisticCommitPreservesConcurrentReservations(t *testing.T) {
	engine, _, run := savepointEngine(t)
	engine.OptimisticTransactions = true
	run("INSERT INTO items(value) VALUES(10)")
	first, second := &Session{CurrentDatabase: "sp"}, &Session{CurrentDatabase: "sp"}
	defer engine.CloseSession(first)
	defer engine.CloseSession(second)
	for _, session := range []*Session{first, second} {
		if _, err := engine.Execute(session, "BEGIN"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := engine.Execute(first, "UPDATE items SET value=20 WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	db, _ := second.transaction.Database("sp")
	table, _ := db.Table("items")
	locked, release := make(chan struct{}), make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- table.Visit(nil, func(storage.Row) error {
			select {
			case <-locked:
			default:
				close(locked)
			}
			<-release
			return nil
		})
	}()
	<-locked
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	type outcome struct {
		result *Result
		err    error
	}
	inserted := make(chan outcome, 1)
	go func() {
		result, err := engine.Execute(second, "INSERT INTO items(value) VALUES(30)")
		inserted <- outcome{result, err}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		stack := make([]byte, 1<<16)
		size := runtime.Stack(stack, true)
		if strings.Contains(string(stack[:size]), "gbaselite/storage.(*Table).NextAutoIncrement") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("insert did not reach its private auto-increment lock")
		}
		runtime.Gosched()
	}
	// The hook runs after ReplaceShared; no filesystem timing is involved.
	replaced := make(chan struct{}, 1)
	previousSave := engine.persistSave
	engine.persistSave = func(*storage.Store) error {
		select {
		case replaced <- struct{}{}:
		default:
		}
		return nil
	}
	defer func() { engine.persistSave = previousSave }()
	committed := make(chan error, 1)
	go func() { _, err := engine.Execute(first, "COMMIT"); committed <- err }()
	select {
	case <-replaced:
		// Without the transaction read gate, ReplaceShared wins before the
		// in-flight writer can publish its reservation into the old table.
	case <-time.After(100 * time.Millisecond):
		// With the read gate the writer owns it and COMMIT waits safely.
	}
	close(release)
	released = true
	if err := <-holderDone; err != nil {
		t.Fatal(err)
	}
	outcome2 := <-inserted
	if outcome2.err != nil {
		t.Fatal(outcome2.err)
	}
	if err := <-committed; err != nil {
		t.Fatal(err)
	}
	abandoned := outcome2.result.LastInsertID
	if _, err := engine.Execute(second, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	fresh := run("INSERT INTO items(value) VALUES(40)")
	if fresh.LastInsertID <= abandoned {
		t.Fatal(fmt.Sprintf("concurrent commit lost reservation: rolled-back id=%d next committed id=%d", abandoned, fresh.LastInsertID))
	}
}
