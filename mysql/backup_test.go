package mysql

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gbaselite/executor"
	"gbaselite/storage"
)

func TestLogicalBackupAndRestoreRoundTrip(t *testing.T) {
	sourceStore := storage.NewStore()
	db, err := sourceStore.CreateDatabase("backup_one")
	if err != nil {
		t.Fatal(err)
	}
	table, err := db.CreateTableWithIndexes("records", []storage.Column{{Name: "id", Type: storage.TypeInt}, {Name: "label", Type: storage.TypeVarchar, Length: 64}, {Name: "note", Type: storage.TypeText}}, nil, []storage.Index{{Name: "records_id", Columns: []string{"id"}, Unique: true}})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []storage.Row{{storage.MustValue(storage.TypeInt, int64(1)), storage.MustValue(storage.TypeVarchar, "O'Reilly\\path"), storage.NullValue(storage.TypeText)}, {storage.MustValue(storage.TypeInt, int64(2)), storage.MustValue(storage.TypeVarchar, "second"), storage.MustValue(storage.TypeText, "line;two")}} {
		if err = table.Insert(row); err != nil {
			t.Fatal(err)
		}
	}
	db, err = sourceStore.CreateDatabase("backup_two")
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.CreateTable("items", []storage.Column{{Name: "id", Type: storage.TypeInt}})
	if err != nil {
		t.Fatal(err)
	}
	if err = other.Insert(storage.Row{storage.MustValue(storage.TypeInt, int64(9))}); err != nil {
		t.Fatal(err)
	}
	backupPath := filepath.Join(t.TempDir(), "all.sql")
	if err := executor.BackupSQL(sourceStore, backupPath, executor.BackupOptions{AddDropDatabase: true}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	dump := string(content)
	if !strings.Contains(dump, "DROP DATABASE IF EXISTS `backup_one`") || !strings.Contains(dump, "INSERT INTO `records` VALUES") || !strings.Contains(dump, "UNIQUE KEY `records_id` (`id`)") {
		t.Fatalf("backup is missing expected MySQL statements:\n%s", dump)
	}

	target, err := openTestEngine(t, filepath.Join(t.TempDir(), "target"), "root", "secret")
	if err != nil {
		t.Fatal(err)
	}
	executed, err := RestoreSQL(target, backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if executed < 8 {
		t.Fatalf("restored only %d statements", executed)
	}
	result, err := target.Execute(&executor.Session{CurrentDatabase: "backup_one"}, "SELECT id,label,note FROM records ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 2 || result.Rows[0][1] != "O'Reilly\\path" || result.Rows[0][2] != nil || result.Rows[1][2] != "line;two" {
		t.Fatalf("restored rows = %#v", result.Rows)
	}
	indexes, err := target.Execute(&executor.Session{CurrentDatabase: "backup_one"}, "SHOW INDEX FROM records")
	if err != nil || len(indexes.Rows) != 1 || indexes.Rows[0][2] != "records_id" {
		t.Fatalf("restored indexes = %#v, %v", indexes, err)
	}
	second, err := target.Execute(&executor.Session{CurrentDatabase: "backup_two"}, "SELECT * FROM items")
	if err != nil || len(second.Rows) != 1 || second.Rows[0][0] != int64(9) {
		t.Fatalf("second database = %#v, %v", second, err)
	}
}

func TestBackupModesAndRestoreRollback(t *testing.T) {
	engine, err := openTestEngine(t, t.TempDir(), "root", "secret")
	if err != nil {
		t.Fatal(err)
	}
	session := &executor.Session{}
	for _, query := range []string{"CREATE DATABASE modes", "CREATE TABLE modes.items(id INT)", "INSERT INTO modes.items VALUES (1)"} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatal(err)
		}
	}
	legacy := storage.NewStore()
	db, err := legacy.CreateDatabase("modes")
	if err != nil {
		t.Fatal(err)
	}
	table, err := db.CreateTable("items", []storage.Column{{Name: "id", Type: storage.TypeInt}})
	if err != nil {
		t.Fatal(err)
	}
	if err = table.Insert(storage.Row{storage.MustValue(storage.TypeInt, int64(1))}); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	schemaPath := filepath.Join(directory, "schema.sql")
	dataPath := filepath.Join(directory, "data.sql")
	if err := executor.BackupSQL(legacy, schemaPath, executor.BackupOptions{Databases: []string{"modes"}, SchemaOnly: true}); err != nil {
		t.Fatal(err)
	}
	if err := executor.BackupSQL(legacy, dataPath, executor.BackupOptions{Databases: []string{"modes"}, DataOnly: true}); err != nil {
		t.Fatal(err)
	}
	schema, _ := os.ReadFile(schemaPath)
	data, _ := os.ReadFile(dataPath)
	if strings.Contains(string(schema), "INSERT INTO") || !strings.Contains(string(schema), "CREATE TABLE") {
		t.Fatalf("invalid schema-only backup:\n%s", schema)
	}
	if strings.Contains(string(data), "CREATE TABLE") || !strings.Contains(string(data), "INSERT INTO") {
		t.Fatalf("invalid data-only backup:\n%s", data)
	}
	if _, err := engine.Execute(session, "CREATE DATABASE excluded_from_backup"); err != nil {
		t.Fatal(err)
	}
	selectedPath := filepath.Join(directory, "selected.sql")
	if err := executor.BackupSQL(legacy, selectedPath, executor.BackupOptions{Databases: []string{"modes"}}); err != nil {
		t.Fatal(err)
	}
	selected, _ := os.ReadFile(selectedPath)
	if strings.Contains(string(selected), "excluded_from_backup") {
		t.Fatalf("single-database backup included another database:\n%s", selected)
	}

	brokenPath := filepath.Join(directory, "broken.sql")
	if err := os.WriteFile(brokenPath, []byte("CREATE DATABASE should_rollback; CREATE TABLE should_rollback.ok(id INT); INVALID SQL;"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreSQL(engine, brokenPath); err == nil {
		t.Fatal("expected restore failure")
	}
	if _, err := engine.Store.Database("should_rollback"); err == nil {
		t.Fatal("failed restore left a partially created database")
	}
}

func TestSplitSQLStatementsHandlesCommentsAndQuotedSemicolons(t *testing.T) {
	statements, err := SplitSQLStatements("-- header\nCREATE DATABASE test; /* comment; */ USE test; INSERT INTO t VALUES ('a;''b',\"c;d\");")
	if err != nil {
		t.Fatal(err)
	}
	if len(statements) != 3 || !strings.Contains(statements[2], "a;''b") {
		t.Fatalf("statements = %#v", statements)
	}
}

func TestRestoreExecutesMySQLVersionComments(t *testing.T) {
	engine, err := openTestEngine(t, t.TempDir(), "root", "secret")
	if err != nil {
		t.Fatal(err)
	}
	script := `
/*!40101 SET @OLD_CHARACTER_SET_CLIENT=@@CHARACTER_SET_CLIENT */;
CREATE DATABASE ` + "`version-comment-restore`" + `;
USE ` + "`version-comment-restore`" + `;
CREATE TABLE ` + "`items`" + ` (` + "`id`" + ` INT, ` + "`label`" + ` VARCHAR(32), PRIMARY KEY (` + "`id`" + `));
INSERT INTO ` + "`items`" + ` VALUES (1,'restored');
/*!50001 INSERT INTO items VALUES (2,'version comment') */;
`
	path := filepath.Join(t.TempDir(), "mysql-version-comments.sql")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreSQL(engine, path); err != nil {
		t.Fatal(err)
	}
	result, err := engine.Execute(&executor.Session{CurrentDatabase: "version-comment-restore"}, "SELECT id,label FROM items ORDER BY id")
	if err != nil || len(result.Rows) != 2 || result.Rows[0][0] != int64(1) || result.Rows[0][1] != "restored" {
		t.Fatalf("restored version-comment rows = %#v, %v", result, err)
	}
}
