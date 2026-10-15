package mvcc

import (
	"errors"
	"testing"
)

// TestP7Fix02_DirtyRegistryAtomicReplacementAndBudgets covers P7-FIX-02:
// Verifies that DirtyRegistry.Publish validates budgets before mutating state,
// that an update failure preserves the existing published version without data loss,
// and that budget errors are returned rather than swallowed.
func TestP7Fix02_DirtyRegistryAtomicReplacementAndBudgets(t *testing.T) {
	reg := NewDirtyRegistry(1, 0)

	// 1. Publish initial version for rootA
	v1 := []DirtyVersion{
		{Space: "row/t1", Key: []byte("k1"), Value: []byte("val1")},
	}
	if err := reg.Publish("rootA", 1, 0, v1); err != nil {
		t.Fatalf("initial publish: %v", err)
	}

	snap1, err := reg.Sample("rootB")
	if err != nil {
		t.Fatal(err)
	}
	if d, ok := snap1.Get("row/t1", []byte("k1")); !ok || string(d.Value) != "val1" {
		t.Fatalf("sampled val = %s, %v, want val1", string(d.Value), ok)
	}

	// 2. Attempt to publish an oversized update exceeding per-root limit (8192)
	oversized := make([]DirtyVersion, 8193)
	for i := range oversized {
		oversized[i] = DirtyVersion{Space: "row/t1", Key: []byte{byte(i)}, Value: []byte("x")}
	}
	err = reg.Publish("rootA", 1, 0, oversized)
	if !errors.Is(err, ErrWriteSetLimit) {
		t.Fatalf("oversized publish returned %v, want ErrWriteSetLimit", err)
	}

	// 3. Atomicity check: failed replacement must NOT have deleted or lost the existing v1!
	snap2, err := reg.Sample("rootB")
	if err != nil {
		t.Fatal(err)
	}
	if d, ok := snap2.Get("row/t1", []byte("k1")); !ok || string(d.Value) != "val1" {
		t.Fatalf("existing version was lost after failed replacement! got %s, %v", string(d.Value), ok)
	}

	// 4. Stale generation/term must be rejected with ErrConflict and not reset epoch
	err = reg.Publish("rootA", 0, 0, v1)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("stale generation publish returned %v, want ErrConflict", err)
	}

	// 5. Withdraw removes rootA
	reg.Withdraw("rootA")
	snap3, _ := reg.Sample("rootB")
	if _, ok := snap3.Get("row/t1", []byte("k1")); ok {
		t.Fatal("rootA was not withdrawn from dirty registry")
	}
}
