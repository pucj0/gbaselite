package executor

import (
	"context"
	"errors"
	"sync"
	"testing"

	"gbaselite/storageengine"
)

// TestB03StreamingViewLifecycle (M39) tests that streaming results retain the read view
// until consumed, and early termination (LIMIT / EOF / error / Result.Close) properly releases the view.
func TestB03StreamingViewLifecycle(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	isoEngine, ok := e.Backend.(storageengine.IsolationEngine)
	if !ok {
		t.Fatalf("Backend does not implement IsolationEngine")
	}

	ctx := context.Background()

	// Seed data in test space
	txInit, err := isoEngine.BeginIsolated(ctx, storageengine.IsolationRepeatableRead)
	if err != nil {
		t.Fatal(err)
	}
	_ = txInit.Put("test_space", []byte("k1"), []byte("v1"))
	_ = txInit.Put("test_space", []byte("k2"), []byte("v2"))
	_ = txInit.Put("test_space", []byte("k3"), []byte("v3"))
	if _, err := txInit.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// 1. Full streaming consumption: view stays active during stream, closed on stream completion
	t.Run("FullConsumption", func(t *testing.T) {
		tx, err := isoEngine.BeginIsolated(ctx, storageengine.IsolationRepeatableRead)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()

		isoTxn := tx.(storageengine.IsolationTxn)
		view, err := isoTxn.BeginStatementView(ctx, storageengine.StatementViewKindStatement)
		if err != nil {
			t.Fatal(err)
		}

		viewClosed := false
		res := &Result{
			Columns: []Column{{Name: "id"}},
			cleanup: func() {
				_ = view.Close()
				viewClosed = true
			},
		}

		origStream := func(yield func([]any) error) error {
			it, err := view.NewIterator(ctx, storageengine.ScanRequest{Space: "test_space"})
			if err != nil {
				return err
			}
			defer it.Close()
			for it.Next() {
				if err := yield([]any{it.Key()}); err != nil {
					return err
				}
			}
			return it.Err()
		}

		// Wrap stream with managed lifecycle
		res.StreamRows = func(yield func([]any) error) error {
			defer res.Close()
			return origStream(yield)
		}

		if view.State() != storageengine.ViewStateActive {
			t.Fatalf("view should be active before streaming")
		}

		count := 0
		err = res.StreamRows(func(row []any) error {
			count++
			if view.State() != storageengine.ViewStateActive {
				t.Fatalf("view should be active during streaming")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("StreamRows error: %v", err)
		}
		if count != 3 {
			t.Fatalf("stream count = %d, want 3", count)
		}
		if !viewClosed || view.State() != storageengine.ViewStateClosed {
			t.Fatalf("view should be closed after stream completion")
		}
	})

	// 2. Early termination (simulating LIMIT / client disconnect)
	t.Run("EarlyTermination", func(t *testing.T) {
		tx, err := isoEngine.BeginIsolated(ctx, storageengine.IsolationRepeatableRead)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()

		isoTxn := tx.(storageengine.IsolationTxn)
		view, err := isoTxn.BeginStatementView(ctx, storageengine.StatementViewKindStatement)
		if err != nil {
			t.Fatal(err)
		}

		viewClosed := false
		res := &Result{
			Columns: []Column{{Name: "id"}},
			cleanup: func() {
				_ = view.Close()
				viewClosed = true
			},
		}

		origStream := func(yield func([]any) error) error {
			it, err := view.NewIterator(ctx, storageengine.ScanRequest{Space: "test_space"})
			if err != nil {
				return err
			}
			defer it.Close()
			for it.Next() {
				if err := yield([]any{it.Key()}); err != nil {
					return err
				}
			}
			return it.Err()
		}

		res.StreamRows = func(yield func([]any) error) error {
			defer res.Close()
			return origStream(yield)
		}

		stopErr := errors.New("client limit reached")
		earlyCount := 0
		err = res.StreamRows(func(row []any) error {
			earlyCount++
			return stopErr
		})
		if !errors.Is(err, stopErr) {
			t.Fatalf("expected stopErr, got: %v", err)
		}
		if earlyCount != 1 {
			t.Fatalf("earlyCount = %d, want 1", earlyCount)
		}
		if !viewClosed || view.State() != storageengine.ViewStateClosed {
			t.Fatalf("view should be closed after early stream termination")
		}
	})

	// 3. Result.Close() without streaming consumption
	t.Run("UnconsumedClose", func(t *testing.T) {
		tx, err := isoEngine.BeginIsolated(ctx, storageengine.IsolationRepeatableRead)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()

		isoTxn := tx.(storageengine.IsolationTxn)
		view, err := isoTxn.BeginStatementView(ctx, storageengine.StatementViewKindStatement)
		if err != nil {
			t.Fatal(err)
		}

		viewClosed := false
		res := &Result{
			Columns: []Column{{Name: "id"}},
			cleanup: func() {
				_ = view.Close()
				viewClosed = true
			},
		}

		res.Close()
		if !viewClosed || view.State() != storageengine.ViewStateClosed {
			t.Fatalf("view should be closed by Result.Close()")
		}

		// Double close must be safe
		res.Close()
	})
}

// TestB03ViewConcurrency (T027) verifies concurrent view token creation and release.
func TestB03ViewConcurrency(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	isoEngine, ok := e.Backend.(storageengine.IsolationEngine)
	if !ok {
		t.Fatalf("Backend does not implement IsolationEngine")
	}

	const concurrency = 20
	const iterations = 5

	var wg sync.WaitGroup
	wg.Add(concurrency)

	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			ctx := context.Background()
			for j := 0; j < iterations; j++ {
				tx, err := isoEngine.BeginIsolated(ctx, storageengine.IsolationRepeatableRead)
				if err != nil {
					t.Errorf("BeginIsolated: %v", err)
					return
				}
				isoTxn, ok := tx.(storageengine.IsolationTxn)
				if !ok {
					t.Errorf("Txn does not implement IsolationTxn")
					_ = tx.Rollback()
					return
				}
				view, err := isoTxn.BeginStatementView(ctx, storageengine.StatementViewKindStatement)
				if err != nil {
					t.Errorf("BeginStatementView: %v", err)
					_ = tx.Rollback()
					return
				}
				// Point read through view
				_, _, _ = view.Get("db/rows/t", []byte("1"))
				_ = view.Close()
				_ = tx.Rollback()
			}
		}()
	}

	wg.Wait()
}

// TestB03CommitValidationNonRegression (T028) verifies that reading in a view does NOT
// authorize overwriting concurrent changes without a covering lock (no lost updates).
func TestB03CommitValidationNonRegression(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := &Session{CurrentDatabase: "db"}
	sB := &Session{CurrentDatabase: "db"}

	// Seed row
	if _, err := e.Execute(sA, "INSERT INTO t VALUES (10, 'initial');"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	// Session A begins transaction
	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatalf("A BEGIN: %v", err)
	}
	// A reads row 10
	resA, err := e.Execute(sA, "SELECT v FROM t WHERE id = 10;")
	if err != nil {
		t.Fatalf("A SELECT: %v", err)
	}
	if len(resA.Rows) != 1 || resA.Rows[0][0] != "initial" {
		t.Fatalf("A read = %v, want 'initial'", resA.Rows)
	}

	// Session B updates row 10 and commits
	if _, err := e.Execute(sB, "UPDATE t SET v = 'updated_by_B' WHERE id = 10;"); err != nil {
		t.Fatalf("B UPDATE: %v", err)
	}

	// Session A attempts to update row 10 based on its stale snapshot
	_, err = e.Execute(sA, "UPDATE t SET v = 'overwrite_by_A' WHERE id = 10;")
	if err == nil {
		// If update succeeded, commit must fail on conflict validation
		_, err = e.Execute(sA, "COMMIT;")
	}

	// In GBaseLite, updating a stale row or committing a stale update must be detected and rejected
	if err == nil {
		t.Fatalf("A was allowed to overwrite B's committed update without serialization or conflict!")
	}

	// End A
	rollbackSessionTransaction(sA)

	// Verify row 10 in database is B's value
	resFinal, err := e.Execute(sB, "SELECT v FROM t WHERE id = 10;")
	if err != nil {
		t.Fatalf("B final SELECT: %v", err)
	}
	if len(resFinal.Rows) != 1 || resFinal.Rows[0][0] != "updated_by_B" {
		t.Fatalf("final value = %v, want 'updated_by_B'", resFinal.Rows)
	}
}
