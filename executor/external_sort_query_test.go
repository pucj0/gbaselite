package executor

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"gbaselite/storage"
)

func TestLegacyBudgetedOrderAndDistinctOperators(t *testing.T) {
	directory := t.TempDir()
	session := &Session{query: newQueryControl(context.Background(), QueryOptions{SortMemoryBytes: 128 << 10, ResultMemoryBytes: 1 << 20, TempDirectory: directory, MaxTempBytes: 8 << 20})}
	columns := []Column{{Name: "id", Type: storage.TypeBigInt}, {Name: "payload", Type: storage.TypeVarchar}}
	compare := func(a, b []any) int { return int(a[2].(int64) - b[2].(int64)) }
	source := func(yield func([]any) error) error {
		for i := 0; i < 3000; i++ {
			if err := yield([]any{int64(i), strings.Repeat("v", 60), int64(i % 13)}); err != nil {
				return err
			}
		}
		return nil
	}
	result, err := executeBudgetedOrder(session, columns, compare, source, 2, 4)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, row := range result.Rows {
		if len(row) != 2 {
			t.Fatal("hidden key exposed")
		}
		ids = append(ids, row[0].(int64))
	}
	if !reflect.DeepEqual(ids, []int64{26, 39, 52, 65}) {
		t.Fatalf("offset/tie order: %v", ids)
	}
	distinctSource := &Result{Columns: columns, StreamRows: func(yield func([]any) error) error {
		for i := 0; i < 6000; i++ {
			if err := yield([]any{int64((6000 - i) % 23), "v"}); err != nil {
				return err
			}
		}
		return nil
	}}
	distinct, err := executeBudgetedDistinct(session, distinctSource, 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	var got []int64
	for _, row := range distinct.Rows {
		got = append(got, row[0].(int64))
	}
	if !reflect.DeepEqual(got, []int64{19, 18, 17}) {
		t.Fatalf("distinct stable source order: %v", got)
	}
	entries, _ := os.ReadDir(directory)
	if len(entries) != 0 {
		t.Fatalf("leaked %v", entries)
	}
	if session.query.temporary.used != 0 {
		t.Fatalf("retained temporary bytes %d", session.query.temporary.used)
	}
}

func TestLegacyBudgetedSortSharedDiskBudgetAndLazyCleanup(t *testing.T) {
	directory := t.TempDir()
	q := newQueryControl(nil, QueryOptions{SortMemoryBytes: 128 << 10, MaxTempBytes: 100, TempDirectory: directory})
	first, err := newExternalRowSorter(q, func(a, b []any) int { return 0 })
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	copyControl := *q
	second, err := newExternalRowSorter(&copyControl, func(a, b []any) int { return 0 })
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	file, err := first.createRun()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = (sortRunWriter{first, file}).Write(make([]byte, 60)); err != nil {
		t.Fatal(err)
	}
	file.Close()
	file, err = second.createRun()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = (sortRunWriter{second, file}).Write(make([]byte, 60)); !errors.Is(err, ErrQueryResourceLimit) {
		t.Fatalf("nested sorter escaped disk quota: %v", err)
	}
	file.Close()
	first.Close()
	second.Close()
	if q.temporary.used != 0 {
		t.Fatal("temporary reservation leaked")
	}
	session := &Session{StreamResults: true, query: newQueryControl(nil, QueryOptions{SortMemoryBytes: 128 << 10, TempDirectory: directory})}
	started := false
	result, err := executeBudgetedOrder(session, nil, func(a, b []any) int { return 0 }, func(yield func([]any) error) error { started = true; return yield(nil) }, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	if started {
		t.Fatal("lazy result created runs before stream consumption")
	}
	sentinel := errors.New("write failed")
	if err = result.StreamRows(func([]any) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("got %v", err)
	}
	entries, _ := os.ReadDir(directory)
	if len(entries) != 0 {
		t.Fatalf("leaked %v", entries)
	}
}

func TestLegacyBudgetedDistinctRejectsTooSmallBudget(t *testing.T) {
	session := &Session{query: newQueryControl(nil, QueryOptions{SortMemoryBytes: 64 << 10, TempDirectory: t.TempDir()})}
	if _, err := executeBudgetedDistinct(session, &Result{}, 0, -1); !errors.Is(err, ErrQueryResourceLimit) {
		t.Fatalf("got %v", err)
	}
}

func TestLegacyQueryVisitCancelsEvenWhenPredicateRejectsEveryRow(t *testing.T) {
	_, _, table, _ := indexedJoinFixture(t, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := newQueryControl(ctx, QueryOptions{})
	tested, yielded := 0, 0
	err := visitQueryTable(q, table, func(storage.Row) bool {
		tested++
		if tested == 3 {
			cancel()
		}
		return false
	}, func(storage.Row) error { yielded++; return nil })
	if !errors.Is(err, ErrQueryCanceled) || tested != 3 || yielded != 0 {
		t.Fatalf("nonmatching scan did not stop: checked=%d yielded=%d err=%v", tested, yielded, err)
	}
}
