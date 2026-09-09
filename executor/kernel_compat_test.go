package executor

import (
	"fmt"
	"gbaselite/storage"
	"strings"
	"testing"
)

func TestLegacyConditionalDecimalMetadataAndScalarSubquery(t *testing.T) {
	engine, err := openLegacy(t.TempDir(), "root", "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	session := &Session{}
	for _, sql := range []string{"CREATE DATABASE precise", "USE precise", "CREATE TABLE numbers(id INT PRIMARY KEY, n DECIMAL(25,2), whole DECIMAL(25,0), f DOUBLE)", "INSERT INTO numbers VALUES (1,9007199254740993.01,9007199254740993,2.5),(2,0.25,2,3.5)"} {
		if _, err := engine.Execute(session, sql); err != nil {
			t.Fatal(sql, err)
		}
	}
	for _, expression := range []string{"IF(id=1,0,n)", "COALESCE(NULL,n,0)", "IFNULL(n,0)", "GREATEST(0,n)", "LEAST(n,100)", "CASE WHEN id=1 THEN 0 ELSE n END"} {
		result, err := engine.Execute(session, "SELECT "+expression+" FROM numbers ORDER BY id")
		if err != nil {
			t.Fatal(expression, err)
		}
		if result.Columns[0].Type != storage.TypeDecimal {
			t.Fatalf("%s metadata=%s", expression, result.Columns[0].Type)
		}
	}
	mixed, err := engine.Execute(session, "SELECT IF(id=1,n,f) FROM numbers ORDER BY id")
	if err != nil || mixed.Columns[0].Type != storage.TypeDouble {
		t.Fatalf("mixed %#v %v", mixed, err)
	}
	scalar, err := engine.Execute(session, "SELECT (SELECT whole FROM numbers WHERE id=1)+0.1 FROM numbers WHERE id=1")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(scalar.Rows[0][0]) != "9007199254740993.1" {
		t.Fatalf("scalar whole DECIMAL lost precision: %#v", scalar.Rows)
	}
}

func TestLegacyExplicitColumnCollationSQLAndPersistence(t *testing.T) {
	directory := t.TempDir()
	engine, err := openLegacy(directory, "root", "secret")
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{}
	execute := func(sql string) *Result {
		t.Helper()
		r, e := engine.Execute(session, sql)
		if e != nil {
			t.Fatal(sql, e)
		}
		return r
	}
	execute("CREATE DATABASE collated")
	execute("USE collated")
	execute("CREATE TABLE names(id INT PRIMARY KEY, ci VARCHAR(32) COLLATE utf8mb4_general_ci, bin VARCHAR(32) COLLATE utf8mb4_bin)")
	execute("INSERT INTO names VALUES(1,'Alpha','Alpha'),(2,'alpha','alpha'),(3,'Beta','Beta')")
	assertCount := func(sql string, want int) {
		t.Helper()
		result := execute(sql)
		rows, err := collectResultRows(result)
		if err != nil || len(rows) != want {
			t.Fatalf("%s = %#v, %v; want %d rows", sql, rows, err, want)
		}
	}
	for _, budget := range []int64{0, 256 << 10} {
		engine.QueryOptions = QueryOptions{SortMemoryBytes: budget, MaxTempBytes: 8 << 20, TempDirectory: t.TempDir()}
		assertCount("SELECT id FROM names WHERE ci='ALPHA'", 2)
		assertCount("SELECT id FROM names WHERE bin='ALPHA'", 0)
		assertCount("SELECT id FROM names WHERE ci LIKE 'AL%'", 2)
		assertCount("SELECT id FROM names WHERE bin LIKE 'AL%'", 0)
		assertCount("SELECT DISTINCT ci FROM names", 2)
		assertCount("SELECT DISTINCT bin FROM names", 3)
		assertCount("SELECT ci,COUNT(*) FROM names GROUP BY ci", 2)
		assertCount("SELECT bin,COUNT(*) FROM names GROUP BY bin", 3)
		assertCount("SELECT x.ci FROM (SELECT ci,id+1 AS extra FROM names) x WHERE x.ci='ALPHA'", 2)
		result := execute("SELECT bin FROM names ORDER BY bin")
		rows, e := collectResultRows(result)
		if e != nil {
			t.Fatal(e)
		}
		if fmt.Sprint(rows[0][0]) != "Alpha" || fmt.Sprint(rows[1][0]) != "Beta" {
			t.Fatalf("binary ORDER BY %#v", rows)
		}
	}
	execute("CREATE TABLE copy_names AS SELECT ci,id+1 AS extra FROM names")
	assertCount("SELECT ci FROM copy_names WHERE ci='ALPHA'", 2)
	execute("CREATE TABLE unique_names(id INT PRIMARY KEY, name VARCHAR(32) COLLATE utf8mb4_general_ci UNIQUE)")
	execute("INSERT INTO unique_names VALUES(1,'Alice')")
	if _, err := engine.Execute(session, "INSERT INTO unique_names VALUES(2,'ALICE')"); err == nil {
		t.Fatal("case-insensitive unique accepted duplicate")
	}
	if _, err := engine.Execute(session, "ALTER TABLE names ADD UNIQUE KEY uq_ci(ci)"); err == nil {
		t.Fatal("existing case-insensitive duplicates accepted")
	}
	ddl := execute("SHOW CREATE TABLE unique_names")
	if !strings.Contains(strings.ToLower(fmt.Sprint(ddl.Rows)), "collate utf8mb4_general_ci") {
		t.Fatalf("missing persisted DDL collation %#v", ddl.Rows)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	engine, err = openLegacy(directory, "root", "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	assertCount("SELECT id FROM names WHERE ci='ALPHA'", 2)
	assertCount("SELECT ci FROM copy_names WHERE ci='ALPHA'", 2)
	if _, err := engine.Execute(session, "INSERT INTO unique_names VALUES(2,'ALICE')"); err == nil {
		t.Fatal("reopened unique lost collation")
	}
}

func TestLegacyCollatedGroupKeyUsesLengthBoundaries(t *testing.T) {
	value := func(s string) any { return collatedText{s, "utf8mb4_bin"} }
	left := groupedRowKey([]any{value("a|string:b"), value("c")}, nil)
	right := groupedRowKey([]any{value("a"), value("b|string:c")}, nil)
	if left == right {
		t.Fatal("distinct tuples shared a group key")
	}
}
