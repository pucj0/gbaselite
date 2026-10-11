package executor

import (
	"bytes"
	"fmt"
	"math"
	"testing"

	"gbaselite/parser"
	"gbaselite/storageengine"
)

// This file pins the single heap-key encoding shared by the write path and the lock path.
//
// A table with no primary key has no encoded primary-key identity, so both the writer and the
// row-lock path synthesise one from the write transaction id and the statement ordinal. If the
// two sides disagree by even one byte, the lock protects a resource the write never touches: two
// concurrent transactions can then write the same heap slot without ever meeting on a lock, which
// is exactly the "no-primary-key heap key inconsistency" defect this file covers.
//
// The authoritative rendering is fmt's "%020d": it zero-pads to at least 20 digits and never
// truncates, so a uint64 that needs more than 20 digits keeps them. The hand-rolled pow10 loop
// that used to live here overflowed int64 at pow10(19) and mis-rendered every ordinal from
// 10^19 upward, so the unit table below checks the boundary densely.

// TestHeapKeyStringMatchesAuthoritativeRendering compares the canonical heap-key encoder with the
// write path's own rendering for the ordinals that matter: the low values, the digit-count
// boundaries and the 20-digit zero-padding boundary.
func TestHeapKeyStringMatchesAuthoritativeRendering(t *testing.T) {
	ordinals := []uint64{
		0,
		1,
		9,
		10,
		12,
		99,
		100,
		999,
		1000,
		1 << 40,
		9_999_999_999_999_999_999, // 19 digits: the leftmost %020d padding zero lives here.
		math.MaxUint64,            // 20 digits: fmt must render all of them, not truncate.
	}
	for _, ordinal := range ordinals {
		want := fmt.Sprintf("%s/%020d", "tx-7", ordinal)
		if got := heapKeyString("tx-7", ordinal); got != want {
			t.Errorf("heapKeyString(%d) = %q, want %q", ordinal, got, want)
		}
	}
}

// TestHeapKeyMatchesAuthoritativeRendering pins the []byte form on one long transaction id, which
// is the shape the lock component receives.
func TestHeapKeyMatchesAuthoritativeRendering(t *testing.T) {
	write := &heapKeyTestTxn{id: "root/0f3a"}
	for _, ordinal := range []uint64{0, 12, 1 << 40, math.MaxUint64} {
		want := []byte(fmt.Sprintf("%s/%020d", write.ID(), ordinal))
		if got := heapKey(write, ordinal); !bytes.Equal(got, want) {
			t.Errorf("heapKey(%d) = %q, want %q", ordinal, got, want)
		}
	}
	// A nil write has no identity to lock; the encoder must keep returning nil rather than
	// inventing a key the write path would never touch.
	if got := heapKey(nil, 0); got != nil {
		t.Errorf("heapKey(nil, 0) = %q, want nil", got)
	}
}

// TestHeapKeyNoPrimaryKeyInsertLockIdentity drives one SQL INSERT into a table with no PRIMARY KEY
// and asserts that the record-lock component the statement acquires carries the byte-identical
// heap key the write path synthesises: txID + "/" + fmt.Sprintf("%020d", ordinal).
//
// The comparison is made against []byte(txID+"/"+fmt.Sprintf("%020d", ordinal)) rather than
// against heapKeyString, so the test fails if the canonical encoder itself drifts from the write
// path's rendering.
func TestHeapKeyNoPrimaryKeyInsertLockIdentity(t *testing.T) {
	e, err := OpenWithOptions(t.TempDir(), "root", "pw", OpenOptions{StorageMode: "mvcc"})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	var writeID string
	previousStatement := statementTxnHook
	statementTxnHook = func(statement parser.Statement, read, write storageengine.Txn) {
		if _, ok := statement.(parser.Insert); ok && write != nil {
			writeID = write.ID()
		}
	}
	defer func() { statementTxnHook = previousStatement }()

	var locked []storageengine.LockComponent
	previousRequest := dmlLockRequestHook
	dmlLockRequestHook = func(request storageengine.LockRequest) storageengine.LockRequest {
		locked = append(locked, request.Components...)
		return request
	}
	defer func() { dmlLockRequestHook = previousRequest }()

	session := &Session{}
	for _, sql := range []string{
		"CREATE DATABASE test",
		"USE test",
		// No PRIMARY KEY: the write path and the lock path must synthesise the same heap key.
		"CREATE TABLE nopk(a INT, b VARCHAR(10))",
		"INSERT INTO nopk VALUES(1,'x')",
	} {
		if _, err := e.Execute(session, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	if writeID == "" {
		t.Fatal("the INSERT did not report a write transaction id")
	}

	// The statement wrote one row at ordinal 0, so the heap identity is the one below. The
	// expected value is built here from the format string rather than from heapKeyString, so the
	// assertion is independent of the encoder it is testing.
	want := []byte(writeID + "/" + fmt.Sprintf("%020d", uint64(0)))
	keys := make([][]byte, 0, len(locked))
	for _, component := range locked {
		if component.Kind != storageengine.LockComponentRecord {
			continue
		}
		keys = append(keys, component.Key)
	}
	if len(keys) == 0 {
		t.Fatalf("the no-primary-key INSERT acquired no record lock; components = %+v", locked)
	}
	if !bytes.Equal(keys[0], want) {
		t.Fatalf("no-primary-key lock key = %q, want %q (all record keys: %q)", keys[0], want, keys)
	}
}

// heapKeyTestTxn is the smallest storageengine.Txn that can name itself: the heap encoder reads
// only ID(), so the remaining methods never run.
type heapKeyTestTxn struct {
	storageengine.Txn
	id string
}

func (t *heapKeyTestTxn) ID() string { return t.id }
