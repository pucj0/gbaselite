package journal

import "testing"

func TestForeignKeyChecksRequireVersion3(t *testing.T) {
	r := BinlogRecord{Version: 1, Statements: []BinlogStatement{{SQL: "INSERT INTO t VALUES(1)", ForeignKeyChecksDisabled: true}}}
	if validateBinlogRecord(r) == nil {
		t.Fatal("old record accepted new semantics")
	}
	r.Version = 3
	if err := validateBinlogRecord(r); err != nil {
		t.Fatal(err)
	}
}
