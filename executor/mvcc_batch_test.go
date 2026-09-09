package executor

import (
	"context"
	"fmt"
	"gbaselite/parser"
	"strings"
	"testing"
)

func TestMVCCBatchBoundsAndOrder(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE bat(id INT PRIMARY KEY,v VARCHAR(2000))")
	values := make([]string, 400)
	for i := range values {
		values[i] = fmt.Sprintf("(%d,'%s')", i, strings.Repeat("x", 1000))
	}
	run("INSERT INTO bat VALUES" + strings.Join(values, ","))
	tx, err := e.Backend.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	table, schema, _, err := loadVersionedTable(tx, s, "bat")
	if err != nil {
		t.Fatal(err)
	}
	plan := planMVCCAccess(parser.Select{}, table, schema, s)
	count, batches := 0, 0
	err = plan.scanBatches(context.Background(), tx, table, 128, func(batch []mvccBatchEntry) error {
		size := 0
		for _, v := range batch {
			size += len(v.value) + 24
		}
		if size > mvccBatchBytes || len(batch) > 128 {
			t.Fatal("unbounded batch", size)
		}
		count += len(batch)
		batches++
		return nil
	})
	if err != nil || count != 400 || batches < 2 {
		t.Fatal(count, batches, err)
	}
	r := run("SELECT id FROM bat ORDER BY id DESC LIMIT 3")
	if fmt.Sprint(r.Rows) != "[[399] [398] [397]]" {
		t.Fatal(r.Rows)
	}
	r = run("SELECT COUNT(*),SUM(id) FROM bat WHERE id>=0")
	if fmt.Sprint(r.Rows) != "[[400 79800]]" {
		t.Fatal(r.Rows)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = plan.scanBatches(ctx, tx, table, 128, func([]mvccBatchEntry) error { t.Fatal("consumed canceled batch"); return nil }); err == nil {
		t.Fatal("missing cancellation")
	}
}
