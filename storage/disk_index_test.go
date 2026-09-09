package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestDiskIndexExternalSortBoundsAndReopen(t *testing.T) {
	directory := t.TempDir()
	const count = 20000
	definition := Index{Name: "pk", Columns: []string{"id"}, Unique: true}
	columns := []Column{{Name: "id", Type: TypeBigInt}}
	descriptor, err := BuildDiskIndex(directory, definition, columns, func(yield func(int, Row) error) error {
		for i := 0; i < count; i++ {
			key := int64((i * 7919) % count)
			if err := yield(i, Row{MustValue(TypeBigInt, key)}); err != nil {
				return err
			}
		}
		return nil
	}, 256<<10)
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.Height < 2 || descriptor.Entries != count {
		t.Fatalf("not a multilevel tree: %#v", descriptor)
	}
	index, err := OpenDiskIndex(directory, descriptor, 16<<10)
	if err != nil {
		t.Fatal(err)
	}
	lower, upper := IndexBound{Value: MustValue(TypeBigInt, int64(1234)), Inclusive: true}, IndexBound{Value: MustValue(TypeBigInt, int64(1567)), Inclusive: false}
	scan := IndexScan{Name: "pk", Lower: &lower, Upper: &upper}
	var positions []int
	if err = index.Scan(scan, func(position int) error { positions = append(positions, position); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(positions) != 333 {
		t.Fatalf("range has %d rows", len(positions))
	}
	for i, position := range positions {
		if got := (position * 7919) % count; got != 1234+i {
			t.Fatalf("range item %d = %d", i, got)
		}
	}
	n, err := index.Count(scan)
	if err != nil || n != len(positions) {
		t.Fatalf("count %d %v", n, err)
	}
	scan.Descending = true
	var reversed []int
	if err = index.Scan(scan, func(position int) error { reversed = append(reversed, position); return nil }); err != nil {
		t.Fatal(err)
	}
	slices.Reverse(positions)
	if !slices.Equal(positions, reversed) {
		t.Fatal("descending mismatch")
	}
	n, err = index.Count(IndexScan{Name: "pk"})
	if err != nil || n != count {
		t.Fatalf("fullcount %d %v", n, err)
	}
	equal := IndexScan{Name: "pk", EqualPrefix: []Value{MustValue(TypeBigInt, int64(9999))}}
	n, err = index.Count(equal)
	if err != nil || n != 1 {
		t.Fatalf("unique lookup %d %v", n, err)
	}
	stop := errors.New("test stop")
	if err = index.Scan(IndexScan{Name: "pk"}, func(int) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("stop error %v", err)
	}
	index.cache.mu.Lock()
	used := index.cache.used
	index.cache.mu.Unlock()
	if used > 16<<10 {
		t.Fatalf("cache grew to %d", used)
	}
	files, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Name() != DiskIndexFileName(descriptor) {
		t.Fatalf("temporary run files leaked: %v", files)
	}
	if err := verifyDiskIndexHash(filepath.Join(directory, DiskIndexFileName(descriptor)), descriptor.File); err != nil {
		t.Fatal(err)
	}
}
func TestDiskIndexCompositeDecimalCollationAndNulls(t *testing.T) {
	columns := []Column{{Name: "category", Type: TypeVarchar, Length: 32, Collation: "utf8mb4_general_ci"}, {Name: "amount", Type: TypeDecimal, SQLType: "decimal(25,2)"}}
	definition := Index{Name: "composite", Columns: []string{"category", "amount"}, Collations: []string{"utf8mb4_general_ci", ""}}
	rows := []Row{{MustValue(TypeVarchar, "A"), MustValue(TypeDecimal, "9007199254740993.01")}, {MustValue(TypeVarchar, "a"), MustValue(TypeDecimal, "9007199254740992.01")}, {MustValue(TypeVarchar, "b"), MustValue(TypeDecimal, "1.00")}, {NullValue(TypeVarchar), MustValue(TypeDecimal, "0")}}
	directory := t.TempDir()
	descriptor, err := BuildDiskIndex(directory, definition, columns, func(yield func(int, Row) error) error {
		for i, row := range rows {
			if err := yield(i, row); err != nil {
				return err
			}
		}
		return nil
	}, 256<<10)
	if err != nil {
		t.Fatal(err)
	}
	index, err := OpenDiskIndex(directory, descriptor, 0)
	if err != nil {
		t.Fatal(err)
	}
	scan := IndexScan{Name: "composite", EqualPrefix: []Value{MustValue(TypeVarchar, "a")}, Lower: &IndexBound{Value: MustValue(TypeDecimal, "9007199254740992.02"), Inclusive: true}}
	var got []int
	if err = index.Scan(scan, func(position int) error { got = append(got, position); return nil }); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []int{0}) {
		t.Fatalf("composite boundary %v", got)
	}
	scan = IndexScan{Name: "composite", EqualPrefix: []Value{NullValue(TypeVarchar)}}
	n, err := index.Count(scan)
	if err != nil || n != 1 {
		t.Fatalf("nullprefix %d %v", n, err)
	}
}
func TestDiskIndexUniqueValidationAndFailureCleanup(t *testing.T) {
	directory := t.TempDir()
	columns := []Column{{Name: "v", Type: TypeDecimal, SQLType: "decimal(10,2)"}}
	definition := Index{Name: "uniq", Columns: []string{"v"}, Unique: true}
	_, err := BuildDiskIndex(directory, definition, columns, func(yield func(int, Row) error) error {
		if err := yield(0, Row{MustValue(TypeDecimal, "1.0")}); err != nil {
			return err
		}
		return yield(1, Row{MustValue(TypeDecimal, "1.00")})
	}, 256<<10)
	if !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("unique precision = %v", err)
	}
	files, _ := os.ReadDir(directory)
	if len(files) != 0 {
		t.Fatalf("failedbuild leaked %v", files)
	}
	descriptor, err := BuildDiskIndex(directory, definition, columns, func(yield func(int, Row) error) error {
		for i := 0; i < 2; i++ {
			if err := yield(i, Row{NullValue(TypeDecimal)}); err != nil {
				return err
			}
		}
		return nil
	}, 256<<10)
	if err != nil {
		t.Fatal(err)
	}
	index, err := OpenDiskIndex(directory, descriptor, 0)
	if err != nil {
		t.Fatal(err)
	}
	n, err := index.Count(IndexScan{Name: "uniq"})
	if err != nil || n != 2 {
		t.Fatalf("nullable unique %d %v", n, err)
	}
}
func TestDiskIndexCorruptionAndReclamation(t *testing.T) {
	directory := t.TempDir()
	definition := Index{Name: "id", Columns: []string{"id"}}
	columns := []Column{{Name: "id", Type: TypeInt}}
	build := func(value int) DiskIndexDescriptor {
		t.Helper()
		d, err := BuildDiskIndex(directory, definition, columns, func(yield func(int, Row) error) error { return yield(0, Row{MustValue(TypeInt, value)}) }, 256<<10)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	first, second := build(1), build(2)
	userFile := filepath.Join(directory, "user.idx")
	if err := os.WriteFile(userFile, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	removed, err := RemoveUnusedDiskIndexes(directory, []DiskIndexDescriptor{second})
	if err != nil || removed != 1 {
		t.Fatalf("reclaim %d %v", removed, err)
	}
	if _, err := os.Stat(filepath.Join(directory, DiskIndexFileName(first))); !os.IsNotExist(err) {
		t.Fatal("orphan remains")
	}
	if _, err := os.Stat(userFile); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, DiskIndexFileName(second))
	file, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte{255}, second.Root+8); err != nil {
		t.Fatal(err)
	}
	file.Close()
	if _, err := OpenDiskIndex(directory, second, 0); err == nil {
		t.Fatal("corruptroot accepted")
	}
	_, err = BuildDiskIndex(directory, definition, columns, func(yield func(int, Row) error) error { return yield(0, Row{MustValue(TypeInt, 2)}) }, 256<<10)
	if err == nil {
		t.Fatal("reused corrupt hash file")
	}
}
func TestDiskIndexEmptyAndHugeKeys(t *testing.T) {
	directory := t.TempDir()
	definition := Index{Name: "k", Columns: []string{"k"}}
	columns := []Column{{Name: "k", Type: TypeText}}
	descriptor, err := BuildDiskIndex(directory, definition, columns, func(func(int, Row) error) error { return nil }, 0)
	if err != nil {
		t.Fatal(err)
	}
	index, err := OpenDiskIndex(directory, descriptor, 0)
	if err != nil {
		t.Fatal(err)
	}
	n, err := index.Count(IndexScan{Name: "k"})
	if err != nil || n != 0 {
		t.Fatalf("empty %d %v", n, err)
	}
	_, err = BuildDiskIndex(directory, definition, columns, func(yield func(int, Row) error) error {
		return yield(0, Row{MustValue(TypeText, fmt.Sprintf("%070000d", 1))})
	}, 0)
	if err == nil {
		t.Fatal("unbounded key accepted")
	}
}
