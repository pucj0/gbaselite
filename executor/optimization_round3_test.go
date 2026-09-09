package executor

import (
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"sort"
	"testing"
)

func TestLegacyBoundedSortMatchesFullSort(t *testing.T) {
	_, _, run := savepointEngine(t)
	run("BEGIN")
	for i := 0; i < 150; i++ {
		run(fmt.Sprintf("INSERT INTO items(value) VALUES(%d)", i))
	}
	for _, order := range []string{"value % 7 DESC,id DESC", "CASE WHEN value%3=0 THEN NULL ELSE value%5 END,id DESC", "value%7", "0-value"} {
		full := run("SELECT id,value FROM items ORDER BY " + order).Rows
		for _, page := range []struct{ offset, limit int }{{0, 0}, {0, 1}, {3, 8}, {4, 60}, {149, 10}, {200, 10}} {
			got := run(fmt.Sprintf("SELECT id,value FROM items ORDER BY %s LIMIT %d OFFSET %d", order, page.limit, page.offset)).Rows
			start := page.offset
			if start > len(full) {
				start = len(full)
			}
			end := start + page.limit
			if end > len(full) {
				end = len(full)
			}
			if !reflect.DeepEqual(got, full[start:end]) && !(len(got) == 0 && start == end) {
				t.Fatalf("%s %+v: results diverged", order, page)
			}
		}
	}
	// Exercise the plain-column sorter on a non-indexed column, ties and NULLs.
	run("CREATE TABLE plain(id INT, label VARCHAR(20))")
	for i := 0; i < 150; i++ {
		v := "'a'"
		if i%3 == 0 {
			v = "'A'"
		}
		if i%7 == 0 {
			v = "NULL"
		}
		run(fmt.Sprintf("INSERT INTO plain VALUES(%d,%s)", i, v))
	}
	full := run("SELECT id FROM plain ORDER BY label DESC").Rows
	got := run("SELECT id FROM plain ORDER BY label DESC LIMIT 10 OFFSET 3").Rows
	if !reflect.DeepEqual(got, full[3:13]) {
		t.Fatal("plain Top-K changed collation/null/tie order")
	}
	if got := run(fmt.Sprintf("SELECT id FROM plain ORDER BY label LIMIT %d OFFSET 1", int64(math.MaxInt64))).Rows; len(got) != 149 {
		t.Fatal("LIMIT overflow")
	}
	run("ROLLBACK")
}
func TestLegacyMutationIndexKeepsLimitOrderAndErrors(t *testing.T) {
	e, s, run := savepointEngine(t)
	run("INSERT INTO items(id,value) VALUES(30,3),(10,1),(20,2)")
	run("BEGIN")
	run("UPDATE items AS a SET value=value+10 WHERE a.id>=10 LIMIT 1")
	if got := run("SELECT id FROM items WHERE value=13").Rows; len(got) != 1 || got[0][0] != int64(30) {
		t.Fatal("UPDATE LIMIT changed physical selection order")
	}
	run("DELETE FROM items WHERE id>=10 LIMIT 1")
	if got := run("SELECT id FROM items ORDER BY id").Rows; !reflect.DeepEqual(got, [][]any{{int64(10)}, {int64(20)}}) {
		t.Fatal(got)
	}
	for _, q := range []string{"UPDATE items SET value=2 WHERE wrong.id=999", "DELETE FROM items WHERE missing=1 AND id=999"} {
		if _, err := e.Execute(s, q); err == nil {
			t.Fatalf("index hid error: %s", q)
		}
	}
	run("ROLLBACK")
}

func TestLegacyBoundedHeapRandomStableReference(t *testing.T) {
	random := rand.New(rand.NewSource(9718))
	type item struct{ key, ordinal int }
	for trial := 0; trial < 100; trial++ {
		limit := random.Intn(40)
		heap := boundedRows[item]{limit: limit, compare: func(a, b item) int { return a.key - b.key }}
		var full []item
		for i := 0; i < 200; i++ {
			v := item{random.Intn(13), i}
			full = append(full, v)
			heap.offer(v, func(v item) item { return v })
		}
		sort.SliceStable(full, func(i, j int) bool { return full[i].key < full[j].key })
		got := heap.sorted()
		if len(got) != limit || limit > 0 && !reflect.DeepEqual(got, full[:limit]) {
			t.Fatalf("heap reference mismatch trial %d", trial)
		}
	}
	if _, ok := boundedSortLimit(int(^uint(0)>>1), 1, 1000); ok {
		t.Fatal("overflow allowed")
	}
	if _, ok := boundedSortLimit(0, 65537, 1000000); ok {
		t.Fatal("large page used bounded heap")
	}
}

func TestLegacyMutationIndexPreservesLegacyNumericComparison(t *testing.T) {
	_, _, run := savepointEngine(t)
	run("CREATE TABLE numbers(id BIGINT PRIMARY KEY,v INT)")
	run("INSERT INTO numbers VALUES(9007199254740992,0),(9007199254740993,0)")
	run("BEGIN")
	result := run("UPDATE numbers SET v=1 WHERE id=9007199254740992")
	if result.AffectedRows != 2 {
		t.Fatal("exact index narrowed legacy float comparison")
	}
	run("CREATE TABLE texts(id VARCHAR(10) PRIMARY KEY)")
	run("INSERT INTO texts VALUES('01'),('1')")
	result = run("DELETE FROM texts WHERE id=1")
	if result.AffectedRows != 1 {
		t.Fatal("text comparison changed")
	}
	run("CREATE TABLE mixed(id INT PRIMARY KEY)")
	run("INSERT INTO mixed VALUES(1),(3),(100)")
	if result := run("DELETE FROM mixed WHERE id<'20'"); result.AffectedRows != 2 {
		t.Fatal("index narrowed mixed-type lexical comparison")
	}
	run("ROLLBACK")
}
