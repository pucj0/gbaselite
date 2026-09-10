package sqllayout

import (
	"bytes"
	"testing"
)

func TestPersistentNamespaceGolden(t *testing.T) {
	pairs := [][2]string{{Rows("T"), "row/T"}, {UniqueIndex("T", "I"), "index/T/I"}, {SecondaryIndex("T", "I"), "secondary/T/I"}, {string(DatabaseKey("db")), "db/db"}, {string(TableKey("db", "t")), "table/db/t"}, {Counter("T", "id"), "T/id"}}
	for _, p := range pairs {
		if p[0] != p[1] {
			t.Fatal(p)
		}
	}
	if !bytes.Equal(SignedInteger(0), []byte{128, 0, 0, 0, 0, 0, 0, 0}) || bytes.Compare(SignedInteger(-1), SignedInteger(0)) >= 0 {
		t.Fatal("integer key format changed")
	}
}
