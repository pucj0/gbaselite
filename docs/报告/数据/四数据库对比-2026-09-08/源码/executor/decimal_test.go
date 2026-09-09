package executor

import (
	"fmt"
	"gbaselite/storage"
	"testing"
)

func TestDecimalSQLExactOperationsAndPersistence(t *testing.T) {
	directory := t.TempDir()
	engine, err := Open(directory, "root", "password")
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{}
	execute := func(sql string) *Result {
		t.Helper()
		result, err := engine.Execute(session, sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return result
	}
	execute("CREATE DATABASE decimal_exact")
	execute("USE decimal_exact")
	execute("CREATE TABLE money(id INT PRIMARY KEY, amount DECIMAL(25,2) NOT NULL, UNIQUE KEY uq_amount(amount))")
	execute("INSERT INTO money VALUES(1,9007199254740993.01),(2,0.10),(3,0.20)")
	scalar := execute("SELECT 0.1 + 0.2,9007199254740993.01 + 0.01, ROUND(-1.235,2), TRUNCATE(12.349,2), 0.1 + 0.2 = 0.3")
	for i, want := range []any{storage.Decimal("0.3"), storage.Decimal("9007199254740993.02"), storage.Decimal("-1.24"), storage.Decimal("12.34"), true} {
		if scalar.Rows[0][i] != want {
			t.Fatalf("scalar %d = %#v, want %#v", i, scalar.Rows[0][i], want)
		}
	}
	for i := 0; i < 4; i++ {
		if scalar.Columns[i].Type != storage.TypeDecimal {
			t.Fatalf("scalar %d metadata %s", i, scalar.Columns[i].Type)
		}
	}
	sum := execute("SELECT SUM(amount),AVG(amount),MIN(amount),MAX(amount) FROM money")
	wants := []string{"9007199254740993.31", "3002399751580331.103333", "0.10", "9007199254740993.01"}
	for i, want := range wants {
		if fmt.Sprint(sum.Rows[0][i]) != want || sum.Columns[i].Type != storage.TypeDecimal {
			t.Fatalf("aggregate %d = %#v (%s), want %s", i, sum.Rows[0][i], sum.Columns[i].Type, want)
		}
	}
	exact := execute("SELECT id FROM money WHERE amount=9007199254740993.01")
	if len(exact.Rows) != 1 || exact.Rows[0][0] != int64(1) {
		t.Fatalf("exact index lookup %#v", exact.Rows)
	}
	absent := execute("SELECT COUNT(*) FROM money WHERE amount=0.104")
	if absent.Rows[0][0] != int64(0) {
		t.Fatalf("predicate was rounded %#v", absent.Rows)
	}
	rangeResult := execute("SELECT id FROM money WHERE amount<0.105 ORDER BY amount LIMIT 1")
	if len(rangeResult.Rows) != 1 || rangeResult.Rows[0][0] != int64(2) {
		t.Fatalf("range index %#v", rangeResult.Rows)
	}
	derived := execute("SELECT x.amount + 0.01 FROM (SELECT amount FROM money WHERE id=1) x")
	if fmt.Sprint(derived.Rows[0][0]) != "9007199254740993.02" {
		t.Fatalf("derived %#v", derived.Rows)
	}
	computed := execute("SELECT x.n FROM (SELECT 9007199254740993.01 + 0.01 AS n) x")
	if fmt.Sprint(computed.Rows[0][0]) != "9007199254740993.02" {
		t.Fatalf("computed derived %#v", computed.Rows)
	}
	execute("CREATE TABLE money_copy AS SELECT amount+0.01 AS amount FROM money")
	copyResult := execute("SELECT amount FROM money_copy ORDER BY amount DESC LIMIT 1")
	if fmt.Sprint(copyResult.Rows[0][0]) != "9007199254740993.02" {
		t.Fatalf("CTAS %#v", copyResult.Rows)
	}
	execute("UPDATE money SET amount=amount+0.01 WHERE id=1")
	if _, err := engine.Execute(session, "INSERT INTO money VALUES(4,0.1000)"); err == nil {
		t.Fatal("duplicate decimal key allowed")
	}
	if _, err := engine.Execute(session, "UPDATE money SET amount=99999999999999999999999999.99 WHERE id=2"); err == nil {
		t.Fatal("decimal overflow accepted")
	}
	execute("CREATE TABLE small(d DECIMAL(3,2))")
	execute("INSERT INTO small VALUES(1.235),(-1.235)")
	rounding := execute("SELECT d FROM small ORDER BY d")
	if fmt.Sprint(rounding.Rows) != "[[-1.24] [1.24]]" {
		t.Fatalf("rounding %#v", rounding.Rows)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	engine, err = Open(directory, "root", "password")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	restored := execute("SELECT amount FROM money WHERE id=1")
	if restored.Rows[0][0] != storage.Decimal("9007199254740993.02") {
		t.Fatalf("persisted %#v", restored.Rows)
	}
}
func TestDecimalFunctionsGroupingAndWindow(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "password")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	session := &Session{}
	for _, sql := range []string{"CREATE DATABASE dec_window", "USE dec_window", "CREATE TABLE t(id INT,d DECIMAL(10,2))", "INSERT INTO t VALUES(1,0.10),(2,0.20),(3,0.30)"} {
		if _, err := engine.Execute(session, sql); err != nil {
			t.Fatal(err)
		}
	}
	result, err := engine.Execute(session, "SELECT id,SUM(d) OVER (ORDER BY id),AVG(d) OVER () FROM t ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(result.Rows) != "[[1 0.10 0.200000] [2 0.30 0.200000] [3 0.60 0.200000]]" {
		t.Fatalf("window %#v", result.Rows)
	}
	result, err = engine.Execute(session, "SELECT SUM(d+0.01)+0.01 FROM t")
	if err != nil {
		t.Fatal(err)
	}
	if result.Rows[0][0] != storage.Decimal("0.64") {
		t.Fatalf("aggregate expression %#v", result.Rows)
	}
	result, err = engine.Execute(session, "SELECT JSON_OBJECT('amount',0.1+0.2),CEIL(-1.2),FLOOR(-1.2),MOD(12.34,2.0),0.1/0.0")
	if err != nil {
		t.Fatal(err)
	}
	wants := []string{`{"amount":0.3}`, "-1", "-2", "0.34", "<nil>"}
	for i, want := range wants {
		if fmt.Sprint(result.Rows[0][i]) != want {
			t.Fatalf("function %d %#v", i, result.Rows[0][i])
		}
	}
}
