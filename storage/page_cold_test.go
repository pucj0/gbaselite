package storage

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestColdLoadStreamsMoreDataThanCacheWithoutResidentRows(t *testing.T) {
	directory := t.TempDir()
	store, _ := pagedTestStore(t, 2048)
	p := NewPagedPersistence(directory, 4096)
	if err := p.Save(store); err != nil {
		t.Fatal(err)
	}
	cold, err := p.LoadCold()
	if err != nil {
		t.Fatal(err)
	}
	database, _ := cold.Database("paged")
	table, _ := database.Table("items")
	if !table.IsCold() || len(table.rows) != 0 || table.RowCount() != 2048 {
		t.Fatalf("cold table contains resident rows or wrong count")
	}
	if len(table.indexRows["primary"]) != 0 || len(table.uniqueRows["primary"]) != 0 {
		t.Fatal("cold load materialized index entries")
	}
	count := 0
	if err := table.Stream(nil, 1000, 3, func(row Row) error {
		if row[0].Int64 != int64(1000+count) {
			t.Fatalf("wrong row: %v", row[0])
		}
		count++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatal(count)
	}
	if len(table.rows) != 0 || !table.IsCold() {
		t.Fatal("stream materialized cold table")
	}
	if stats := p.PagedStats(); stats.CacheBytes > 4096 {
		t.Fatalf("cache bytes %d", stats.CacheBytes)
	}
	if err := cold.Materialize(1024); !errors.Is(err, ErrColdMaterializationLimit) {
		t.Fatalf("materialization budget error=%v", err)
	}
	if !table.IsCold() || len(table.rows) != 0 {
		t.Fatal("refused fallback changed cold table")
	}
	if err := p.Save(cold); err != nil {
		t.Fatal(err)
	}
	if got := len(pagedTestRows(t, NewPagedPersistence(directory, 0))); got != 2048 {
		t.Fatalf("saving cold store lost rows: %d", got)
	}
	if err := cold.Materialize(16 << 20); err != nil {
		t.Fatal(err)
	}
	if table.IsCold() || len(table.rows) != 2048 {
		t.Fatal("explicit materialization did not load rows")
	}
	if row, found, indexed := table.LookupUnique("id", MustValue(TypeInt, 1700)); !found || !indexed || row[0].Int64 != 1700 {
		t.Fatal("materialization did not rebuild unique index")
	}
}

func TestColdReadCorruptionPropagatesWithoutSilentEmptyResult(t *testing.T) {
	directory := t.TempDir()
	store, _ := pagedTestStore(t, 10)
	p := NewPagedPersistence(directory, 0)
	if err := p.Save(store); err != nil {
		t.Fatal(err)
	}
	cold, err := p.LoadCold()
	if err != nil {
		t.Fatal(err)
	}
	database, _ := cold.Database("paged")
	table, _ := database.Table("items")
	ref := p.pages.manifest.Databases[0].Tables[0].Pages[0]
	if err := os.WriteFile(p.pagePath(ref.Hash), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := table.Visit(nil, func(Row) error { return nil }); err == nil || !strings.Contains(err.Error(), "cold row page") {
		t.Fatalf("read error=%v", err)
	}
	if err := cold.Materialize(1 << 20); err == nil {
		t.Fatal("corrupt table materialized")
	}
	if !table.IsCold() || len(table.rows) != 0 {
		t.Fatal("failed materialization installed empty rows")
	}
}

func TestColdDiskIndexReadsOnlyMatchingRowPage(t *testing.T) {
	directory := t.TempDir()
	store, table := pagedTestStore(t, 4096)
	p := NewPagedPersistence(directory, 0)
	if err := p.Save(store); err != nil {
		t.Fatal(err)
	}
	first := p.PagedStats()
	if first.DiskIndexBuilds != 1 {
		t.Fatalf("index builds=%d", first.DiskIndexBuilds)
	}
	if _, err := table.Update(func(row Row) bool { return row[0].Int64 == 2000 }, map[string]Value{"value": MustValue(TypeText, "payload only")}); err != nil {
		t.Fatal(err)
	}
	if err := p.Save(store); err != nil {
		t.Fatal(err)
	}
	if p.PagedStats().DiskIndexBuilds != first.DiskIndexBuilds {
		t.Fatal("non-index field update rebuilt disk index")
	}
	cold, err := p.LoadCold()
	if err != nil {
		t.Fatal(err)
	}
	database, _ := cold.Database("paged")
	coldTable, _ := database.Table("items")
	before := p.PagedStats()
	row, found, indexed, err := coldTable.LookupUniqueChecked("id", MustValue(TypeInt, 2000))
	if err != nil || !found || !indexed || row[1].Text != "payload only" {
		t.Fatalf("lookup=%v %v %v %v", row, found, indexed, err)
	}
	if misses := p.PagedStats().CacheMisses - before.CacheMisses; misses != 1 {
		t.Fatalf("point lookup read %d row pages", misses)
	}
	var ids []int64
	err = coldTable.StreamIndex(IndexScan{Name: "PRIMARY", Lower: &IndexBound{Value: MustValue(TypeInt, 4000), Inclusive: true}, Descending: true}, nil, 1, 3, func(row Row) error { ids = append(ids, row[0].Int64); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 || ids[0] != 4094 || ids[2] != 4092 {
		t.Fatalf("descending range=%v", ids)
	}
	count, err := coldTable.CountIndex(IndexScan{Name: "PRIMARY", Lower: &IndexBound{Value: MustValue(TypeInt, 4000), Inclusive: true}}, nil)
	if err != nil || count != 96 {
		t.Fatalf("range count=%d %v", count, err)
	}
	if len(coldTable.rows) != 0 {
		t.Fatal("disk index materialized table")
	}
}
