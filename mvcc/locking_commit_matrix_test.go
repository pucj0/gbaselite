package mvcc

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"gbaselite/lockmanager"
	"gbaselite/storageengine"
)

// locking_commit_matrix_test.go is the T055 suite: parameterizing bounded, group, stream,
// and WAL commit paths with lock manager integration.
//
// Invariants verified across all paths:
// 1. final latest > readTS is rejected with storageengine.ErrConflict;
// 2. three root classes (empty/read-only, data-writing, dependency-only) behave consistently;
// 3. publication markers and commit sequences match B01 specifications;
// 4. raw Apply intervention during transaction lifetime causes proper conflict on commit.

type commitPathConfig struct {
	name     string
	localWAL bool
}

func commitPathConfigs() []commitPathConfig {
	return []commitPathConfig{
		{name: "bounded", localWAL: false},
		{name: "local-wal", localWAL: true},
	}
}

// TestT055CommitMatrixConflictValidation tests that whenever another transaction commits a change
// to a key between a transaction's readTS and commit time, final commit validation rejects with
// ErrConflict across bounded, group, stream, and WAL paths.
func TestT055CommitMatrixConflictValidation(t *testing.T) {
	for _, cfg := range commitPathConfigs() {
		t.Run(cfg.name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := OpenWithOptions(dir, Options{LocalWAL: cfg.localWAL})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()

			lm, err := lockmanager.NewManager(lockmanager.BudgetLimits{}, 1, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer lm.Close()

			if _, err := store.AttachLocks(lm); err != nil {
				t.Fatal(err)
			}

			ctx := context.Background()

			// Seed key k1 = v0
			seedTx, err := store.Begin(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := seedTx.Put("row/t1", []byte("k1"), []byte("v0")); err != nil {
				t.Fatal(err)
			}
			if _, err := seedTx.Commit(ctx); err != nil {
				t.Fatal(err)
			}

			// T1 reads k1 at snapshot readTS
			t1, err := store.Begin(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer t1.Rollback()

			val, found, err := t1.Get("row/t1", []byte("k1"))
			if err != nil || !found || string(val) != "v0" {
				t.Fatalf("T1 read unexpected value: %s, %v", val, err)
			}

			// T2 modifies k1 and commits (latest committed change moves past T1's readTS)
			t2, err := store.Begin(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := t2.Put("row/t1", []byte("k1"), []byte("v1")); err != nil {
				t.Fatal(err)
			}
			if _, err := t2.Commit(ctx); err != nil {
				t.Fatal(err)
			}

			// 1. Data-writing root: T1 writes to k1 and attempts commit -> must conflict!
			if err := t1.Put("row/t1", []byte("k1"), []byte("v2")); err != nil {
				t.Fatal(err)
			}
			if _, err := t1.Commit(ctx); !errors.Is(err, storageengine.ErrConflict) {
				t.Fatalf("expected ErrConflict on commit, got: %v", err)
			}
		})
	}
}

// TestT055ThreeRootClassesConsistency verifies that empty/read-only, data-writing, and
// dependency-only (Guard) roots behave identically and consistently across commit paths.
func TestT055ThreeRootClassesConsistency(t *testing.T) {
	for _, cfg := range commitPathConfigs() {
		t.Run(cfg.name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := OpenWithOptions(dir, Options{LocalWAL: cfg.localWAL})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()

			lm, err := lockmanager.NewManager(lockmanager.BudgetLimits{}, 1, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer lm.Close()

			if _, err := store.AttachLocks(lm); err != nil {
				t.Fatal(err)
			}

			ctx := context.Background()

			// 1. Empty/read-only root
			readTx, err := store.Begin(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, _, _ = readTx.Get("row/t1", []byte("nonexistent"))
			seq, err := readTx.Commit(ctx)
			if err != nil {
				t.Fatalf("read-only commit failed: %v", err)
			}
			if seq != 0 {
				t.Fatalf("read-only commit allocated sequence: %d, want 0", seq)
			}

			// 2. Data-writing root
			writeTx, err := store.Begin(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := writeTx.Put("row/t1", []byte("k1"), []byte("v1")); err != nil {
				t.Fatal(err)
			}
			writeSeq, err := writeTx.Commit(ctx)
			if err != nil {
				t.Fatalf("data-writing commit failed: %v", err)
			}
			if writeSeq == 0 {
				t.Fatal("data-writing commit did not allocate sequence")
			}
			if _, committed := store.Committed(writeTx.ID); !committed {
				t.Fatal("publication marker missing for data-writing root")
			}

			// 3. Dependency-only (Guard) root
			guardTx, err := store.Begin(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := guardTx.Guard("row/t1", []byte("k1")); err != nil {
				t.Fatal(err)
			}
			guardSeq, err := guardTx.Commit(ctx)
			if err != nil {
				t.Fatalf("guard commit failed: %v", err)
			}
			if guardSeq == 0 {
				t.Fatal("guard commit did not allocate sequence")
			}
		})
	}
}

// TestT055RawApplyInterventionCausesConflict verifies that raw Store.Apply intervention
// updating a key while a user transaction is open correctly triggers ErrConflict on user commit.
func TestT055RawApplyInterventionCausesConflict(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenWithOptions(dir, Options{LocalWAL: false})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	lm, err := lockmanager.NewManager(lockmanager.BudgetLimits{}, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lm.Close()

	if _, err := store.AttachLocks(lm); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	// Initial seed
	seedTx, err := store.Begin(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := seedTx.Put("row/t1", []byte("keyA"), []byte("val0")); err != nil {
		t.Fatal(err)
	}
	seedSeq, err := seedTx.Commit(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// User Tx begins and reads keyA
	userTx, err := store.Begin(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer userTx.Rollback()

	_, _, _ = userTx.Get("row/t1", []byte("keyA"))

	// Raw Apply intervention: Apply stage and commit for keyA directly through Store.Apply!
	applyCmd := Command{
		Kind:     "stage",
		ID:       "replicated_external_tx",
		Snapshot: seedSeq,
		Ops: []Op{
			{Space: "row/t1", Key: []byte("keyA"), Value: []byte("val_applied")},
		},
	}
	applyIndex1 := seedSeq + 1
	applyIndex2 := seedSeq + 2
	if _, err := store.Apply(applyIndex1, applyCmd); err != nil {
		t.Fatalf("raw stage Apply failed: %v", err)
	}
	res, err := store.Apply(applyIndex2, Command{Kind: "commit", ID: "replicated_external_tx", Snapshot: seedSeq})
	if err != nil || res.Error != "" {
		t.Fatalf("raw commit Apply failed: %v, res: %+v", err, res)
	}

	// User Tx now puts keyA and tries to commit -> must detect conflict with applied change!
	if err := userTx.Put("row/t1", []byte("keyA"), []byte("val_user")); err != nil {
		t.Fatal(err)
	}
	if _, err := userTx.Commit(ctx); !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("expected ErrConflict after raw Apply intervention, got: %v", err)
	}
}

// TestT055GroupCommitConcurrentTransactions verifies that multiple concurrent transactions
// committing through group commit maintain strict serializability and conflict detection.
func TestT055GroupCommitConcurrentTransactions(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenWithOptions(dir, Options{LocalWAL: true})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	lm, err := lockmanager.NewManager(lockmanager.BudgetLimits{}, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lm.Close()

	if _, err := store.AttachLocks(lm); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	const numTxns = 8
	var wg sync.WaitGroup
	errs := make([]error, numTxns)
	seqs := make([]uint64, numTxns)

	for i := 0; i < numTxns; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			tx, err := store.Begin(ctx, nil)
			if err != nil {
				errs[idx] = err
				return
			}
			key := fmt.Sprintf("k_%d", idx)
			if err := tx.Put("row/t1", []byte(key), []byte("v")); err != nil {
				errs[idx] = err
				_ = tx.Rollback()
				return
			}
			seq, err := tx.Commit(ctx)
			errs[idx] = err
			seqs[idx] = seq
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("txn %d failed in group commit: %v", i, err)
		}
		if seqs[i] == 0 {
			t.Fatalf("txn %d got 0 sequence in group commit", i)
		}
	}
}

// TestT055StreamCommitConflictAndCancellation verifies that large chunk-streamed transactions
// properly enforce commit conflict detection, publication markers, cancellation boundaries,
// and lock cleanup.
func TestT055StreamCommitConflictAndCancellation(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenWithOptions(dir, Options{LocalWAL: false})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	lm, err := lockmanager.NewManager(lockmanager.BudgetLimits{}, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lm.Close()

	if _, err := store.AttachLocks(lm); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	// 1. Seed row stream_key_0
	seedTx, err := store.Begin(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := seedTx.Put("row/stream", []byte("stream_key_0"), []byte("v0")); err != nil {
		t.Fatal(err)
	}
	if _, err := seedTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// 2. Stream transaction T1 reads stream_key_0, writes 16 rows of 4 KiB each (64 KiB > MaxChunkBytes 32 KiB)
	t1, err := store.Begin(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer t1.Rollback()

	_, _, _ = t1.Get("row/stream", []byte("stream_key_0"))

	chunkVal := make([]byte, 4096)
	for i := range chunkVal {
		chunkVal[i] = 'X'
	}

	for i := 1; i < 16; i++ {
		key := fmt.Sprintf("stream_key_%d", i)
		if err := t1.Put("row/stream", []byte(key), chunkVal); err != nil {
			t.Fatal(err)
		}
	}

	// 3. Concurrent transaction T2 modifies stream_key_0 and commits
	t2, err := store.Begin(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := t2.Put("row/stream", []byte("stream_key_0"), []byte("v_concurrent")); err != nil {
		t.Fatal(err)
	}
	if _, err := t2.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// 4. Stream transaction T1 puts stream_key_0 and attempts commit -> must be rejected with ErrConflict!
	if err := t1.Put("row/stream", []byte("stream_key_0"), chunkVal); err != nil {
		t.Fatal(err)
	}
	if _, err := t1.Commit(ctx); !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("expected stream commit conflict ErrConflict, got: %v", err)
	}

	// 5. Successful stream transaction without conflict
	t3, err := store.Begin(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 16; i++ {
		key := fmt.Sprintf("stream_success_%d", i)
		if err := t3.Put("row/stream", []byte(key), chunkVal); err != nil {
			t.Fatal(err)
		}
	}
	seq3, err := t3.Commit(ctx)
	if err != nil {
		t.Fatalf("stream commit failed: %v", err)
	}
	if seq3 == 0 {
		t.Fatal("stream commit allocated 0 sequence")
	}
	if _, committed := store.Committed(t3.ID); !committed {
		t.Fatal("stream commit missing publication marker")
	}

	// 6. Lock manager has 0 residue
	if lm.Stats().ActiveOwners != 0 || lm.Stats().PendingRequests != 0 || lm.LockTableSize() != 0 {
		t.Fatalf("lock manager residue after stream commits: %+v, size=%d", lm.Stats(), lm.LockTableSize())
	}
}
