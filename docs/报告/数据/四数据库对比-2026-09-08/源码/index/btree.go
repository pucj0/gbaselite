package index

import (
	"sort"
	"sync"
)

// BTree is a compact ordered index facade. The first release uses sorted keys;
// its API allows a page-based B+Tree implementation without changing callers.
type BTree struct {
	mu     sync.RWMutex
	values map[string][]int
}

func New() *BTree { return &BTree{values: map[string][]int{}} }
func (b *BTree) Insert(key string, row int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.values[key] = append(b.values[key], row)
}
func (b *BTree) Find(key string) []int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return append([]int(nil), b.values[key]...)
}
func (b *BTree) Delete(key string, row int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	values := b.values[key]
	for i, value := range values {
		if value == row {
			values = append(values[:i], values[i+1:]...)
			break
		}
	}
	if len(values) == 0 {
		delete(b.values, key)
	} else {
		b.values[key] = values
	}
}
func (b *BTree) Keys() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	keys := make([]string, 0, len(b.values))
	for key := range b.values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
