package server

import (
	"strings"
	"testing"
)

func TestPreparedCacheLimitsReleaseAndWrap(t *testing.T) {
	cache := newPreparedCache(2, 32)
	id, _, _, err := cache.add("SELECT ?")
	if err != nil {
		t.Fatal(err)
	}
	second, _, _, err := cache.add("SELECT 2")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, code, err := cache.add("SELECT 3"); err == nil || code != 1461 {
		t.Fatal("count limit ignored")
	}
	cache.remove(id)
	cache.remove(id)
	if cache.bytes != 8 {
		t.Fatalf("bytes after close: %d", cache.bytes)
	}
	if _, _, code, err := cache.add(strings.Repeat("x", 25)); err == nil || code != 1461 {
		t.Fatal("byte limit ignored")
	}
	cache.nextID = second
	id, _, _, err = cache.add("SELECT 4")
	if err != nil || id == second {
		t.Fatal("statement ID collision")
	}
	cache.remove(id)
	cache.remove(second)
	cache.nextID = 0
	id, _, _, err = cache.add("SELECT 5")
	if err != nil || id == 0 {
		t.Fatal("invalid wrapped statement ID")
	}
	huge := newPreparedCache(1, 1<<20)
	if _, _, code, err := huge.add(strings.Repeat("?", 65536)); err == nil || code != 1390 {
		t.Fatal("parameter count overflow")
	}
}
