package executor

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestMVCCMaintenanceRoundTrip(t *testing.T) {
	_, _, run := rangeTestEngine(t)
	run("CREATE TABLE saved(id INT PRIMARY KEY,v INT)")
	run("INSERT INTO saved VALUES(1,10)")
	root := t.TempDir()
	path := func(name string) string { return strings.ReplaceAll(filepath.Join(root, name), "\\", "/") }
	run("BACKUP MVCC TO '" + path("backup") + "'")
	run("UPDATE saved SET v=20 WHERE id=1")
	run("RESTORE MVCC FROM '" + path("backup") + "'")
	if r := run("SELECT v FROM saved"); fmt.Sprint(r.Rows) != "[[10]]" {
		t.Fatal(r.Rows)
	}
	run("GC MVCC")
	run("COMPACT MVCC TO '" + path("compact") + "'")
}
