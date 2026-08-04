package executor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"gbaselite/storage"
)

func TestSQLCRUDAndPersistence(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{}
	queries := []string{"CREATE DATABASE test", "USE test", "CREATE TABLE user(id INT,name VARCHAR(50),age INT)", "INSERT INTO user VALUES (1,'张三',20),(2,'李四',25)", "UPDATE user SET age=21 WHERE id=1", "DELETE FROM user WHERE id=2"}
	for _, query := range queries {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	result, err := engine.Execute(session, "SELECT name,age FROM user WHERE age>=20 ORDER BY age DESC LIMIT 10")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 || result.Rows[0][0] != "张三" || result.Rows[0][1] != int64(21) {
		t.Fatalf("unexpected result: %#v", result.Rows)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLMigrationDDLForeignKeyRestrictAndPersistence(t *testing.T) {
	directory := t.TempDir()
	engine, err := Open(directory, "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE migration_ddl_test",
		"USE migration_ddl_test",
		"CREATE TABLE users(id BIGINT NOT NULL,phone VARCHAR(32),PRIMARY KEY(id))",
		"CREATE TABLE subscriptions(id BIGINT NOT NULL,user_id BIGINT NOT NULL,PRIMARY KEY(id))",
		"INSERT INTO users VALUES (1,'13800000000')",
		"INSERT INTO subscriptions VALUES (10,1)",
		"ALTER TABLE users ADD COLUMN course_balance INT NOT NULL DEFAULT 0 AFTER phone",
		"ALTER TABLE users ADD CONSTRAINT ck_course_balance CHECK (course_balance >= 0)",
		"ALTER TABLE subscriptions ADD CONSTRAINT fk_subscription_user FOREIGN KEY (user_id) REFERENCES users(id)",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	row, err := engine.Execute(session, "SELECT id,phone,course_balance FROM users")
	if err != nil || len(row.Rows) != 1 || row.Rows[0][2] != int64(0) {
		t.Fatalf("ADD COLUMN backfill = %#v, %v", row, err)
	}
	if _, err := engine.Execute(session, "ALTER TABLE users ADD COLUMN required_value INT NOT NULL"); err == nil {
		t.Fatal("ADD NOT NULL column without a default succeeded on a populated table")
	}
	describe, err := engine.Execute(session, "DESCRIBE users")
	if err != nil || len(describe.Rows) != 3 {
		t.Fatalf("failed ADD COLUMN changed metadata: %#v, %v", describe, err)
	}
	if _, err := engine.Execute(session, "UPDATE users SET course_balance=-1 WHERE id=1"); !errors.Is(err, storage.ErrCheckConstraint) {
		t.Fatalf("CHECK update error = %v", err)
	}
	if _, err := engine.Execute(session, "DELETE FROM users WHERE id=1"); !errors.Is(err, storage.ErrForeignKeyReferenced) {
		t.Fatalf("parent DELETE error = %v", err)
	}
	if _, err := engine.Execute(session, "UPDATE users SET id=2 WHERE id=1"); !errors.Is(err, storage.ErrForeignKeyReferenced) {
		t.Fatalf("parent UPDATE error = %v", err)
	}
	if _, err := engine.Execute(session, "RENAME TABLE users TO members"); err != nil {
		t.Fatal(err)
	}
	show, err := engine.Execute(session, "SHOW CREATE TABLE subscriptions")
	if err != nil || !strings.Contains(show.Rows[0][1].(string), "CONSTRAINT `fk_subscription_user`") || !strings.Contains(show.Rows[0][1].(string), "REFERENCES `members`") {
		t.Fatalf("renamed foreign key metadata = %#v, %v", show, err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(directory, "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	session = &Session{CurrentDatabase: "migration_ddl_test"}
	show, err = reopened.Execute(session, "SHOW CREATE TABLE members")
	if err != nil || !strings.Contains(show.Rows[0][1].(string), "CONSTRAINT `ck_course_balance` CHECK") {
		t.Fatalf("persisted CHECK metadata = %#v, %v", show, err)
	}
	if _, err := reopened.Execute(session, "ALTER TABLE subscriptions DROP FOREIGN KEY fk_subscription_user"); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Execute(session, "DELETE FROM members WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Execute(session, "ALTER TABLE members DROP CHECK ck_course_balance"); err != nil {
		t.Fatal(err)
	}
}

func TestMultiActionAlterTableIsAtomicAndPersistent(t *testing.T) {
	directory := t.TempDir()
	engine, err := Open(directory, "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE multi_alter_test",
		"USE multi_alter_test",
		"CREATE TABLE items(id INT,name VARCHAR(10),obsolete INT)",
		"CREATE INDEX idx_obsolete ON items(obsolete)",
		"INSERT INTO items VALUES (1,'seed',9)",
		`ALTER TABLE items
			ADD COLUMN quantity INT NOT NULL DEFAULT 7 AFTER name,
			ADD COLUMN label VARCHAR(20) DEFAULT 'ready',
			MODIFY COLUMN name VARCHAR(50),
			CHANGE COLUMN label status VARCHAR(30),
			ADD INDEX idx_status(status),
			DROP INDEX idx_obsolete`,
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}

	describe, err := engine.Execute(session, "DESCRIBE items")
	if err != nil {
		t.Fatal(err)
	}
	wantColumns := []string{"id", "name", "quantity", "obsolete", "status"}
	if len(describe.Rows) != len(wantColumns) {
		t.Fatalf("DESCRIBE row count = %d, want %d: %#v", len(describe.Rows), len(wantColumns), describe.Rows)
	}
	for index, name := range wantColumns {
		if describe.Rows[index][0] != name {
			t.Fatalf("column %d = %#v, want %q", index, describe.Rows[index][0], name)
		}
	}
	row, err := engine.Execute(session, "SELECT id,name,quantity,status FROM items")
	if err != nil || len(row.Rows) != 1 || row.Rows[0][2] != int64(7) || row.Rows[0][3] != "ready" {
		t.Fatalf("multi-action ALTER backfill = %#v, %v", row, err)
	}
	indexes, err := engine.Execute(session, "SHOW INDEX FROM items")
	if err != nil {
		t.Fatal(err)
	}
	indexNames := make(map[string]bool)
	for _, indexRow := range indexes.Rows {
		indexNames[indexRow[2].(string)] = true
	}
	if !indexNames["idx_status"] || indexNames["idx_obsolete"] {
		t.Fatalf("unexpected indexes after batch ALTER: %#v", indexes.Rows)
	}

	if _, err := engine.Execute(session, "ALTER TABLE items ADD COLUMN rolled_back INT DEFAULT 1, ADD COLUMN rejected INT NOT NULL"); err == nil {
		t.Fatal("multi-action ALTER with an invalid later action succeeded")
	}
	describe, err = engine.Execute(session, "DESCRIBE items")
	if err != nil || len(describe.Rows) != len(wantColumns) {
		t.Fatalf("failed batch ALTER changed column count: %#v, %v", describe, err)
	}
	for _, columnRow := range describe.Rows {
		if columnRow[0] == "rolled_back" || columnRow[0] == "rejected" {
			t.Fatalf("failed batch ALTER left a column behind: %#v", describe.Rows)
		}
	}

	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(directory, "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	session = &Session{CurrentDatabase: "multi_alter_test"}
	describe, err = reopened.Execute(session, "DESCRIBE items")
	if err != nil || len(describe.Rows) != len(wantColumns) || describe.Rows[4][0] != "status" {
		t.Fatalf("persisted multi-action ALTER = %#v, %v", describe, err)
	}
}

func TestColumnLifecycleDDLIsAtomicAndPersistent(t *testing.T) {
	directory := t.TempDir()
	engine, err := Open(directory, "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE column_lifecycle_test",
		"USE column_lifecycle_test",
		"CREATE TABLE items(id BIGINT AUTO_INCREMENT,name VARCHAR(30),obsolete INT,PRIMARY KEY(id),INDEX idx_obsolete(obsolete))",
		"INSERT INTO items(name,obsolete) VALUES ('first',9)",
		"ALTER TABLE items DROP INDEX idx_obsolete,DROP COLUMN obsolete,RENAME COLUMN id TO item_id,ADD COLUMN IF NOT EXISTS note VARCHAR(30) DEFAULT 'ready'",
		"ALTER TABLE items ADD COLUMN IF NOT EXISTS note VARCHAR(200)",
		"ALTER TABLE items DROP COLUMN IF EXISTS missing_column",
		"INSERT INTO items(name) VALUES ('second')",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	rows, err := engine.Execute(session, "SELECT item_id,name,note FROM items ORDER BY item_id")
	if err != nil || len(rows.Rows) != 2 || rows.Rows[0][0] != int64(1) || rows.Rows[0][2] != "ready" || rows.Rows[1][0] != int64(2) {
		t.Fatalf("column lifecycle rows = %#v, %v", rows, err)
	}
	if _, err := engine.Execute(session, "ALTER TABLE items DROP COLUMN item_id"); err == nil {
		t.Fatal("dropping indexed primary-key column succeeded")
	}
	if _, err := engine.Execute(session, "ALTER TABLE items ADD COLUMN rolled_back INT DEFAULT 1,DROP COLUMN item_id"); err == nil {
		t.Fatal("invalid multi-action column lifecycle succeeded")
	}
	if description, err := engine.Execute(session, "DESCRIBE items"); err != nil || len(description.Rows) != 3 {
		t.Fatalf("failed batch changed schema = %#v, %v", description, err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(directory, "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	rows, err = reopened.Execute(&Session{CurrentDatabase: "column_lifecycle_test"}, "SELECT item_id,name,note FROM items ORDER BY item_id")
	if err != nil || len(rows.Rows) != 2 || rows.Rows[1][0] != int64(2) {
		t.Fatalf("persisted column lifecycle rows = %#v, %v", rows, err)
	}
}

func TestRenameReferencedColumnAndRejectDependentDrop(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE rename_fk_column_test",
		"USE rename_fk_column_test",
		"CREATE TABLE parents(code INT,PRIMARY KEY(code))",
		"CREATE TABLE children(id INT,parent_code INT,CONSTRAINT fk_parent_code FOREIGN KEY(parent_code) REFERENCES parents(code))",
		"INSERT INTO parents VALUES (10)",
		"INSERT INTO children VALUES (1,10)",
		"ALTER TABLE parents RENAME COLUMN code TO external_code",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	show, err := engine.Execute(session, "SHOW CREATE TABLE children")
	if err != nil || !strings.Contains(show.Rows[0][1].(string), "REFERENCES `parents` (`external_code`)") {
		t.Fatalf("renamed referenced column metadata = %#v, %v", show, err)
	}
	if _, err := engine.Execute(session, "ALTER TABLE parents DROP COLUMN external_code"); !errors.Is(err, storage.ErrForeignKeyReferenced) {
		t.Fatalf("dependent DROP COLUMN error = %v", err)
	}
}

func TestAlterConstraintRejectsInvalidHistoryAtomically(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE invalid_constraint_test",
		"USE invalid_constraint_test",
		"CREATE TABLE parents(id INT,PRIMARY KEY(id))",
		"CREATE TABLE children(id INT,parent_id INT,value INT)",
		"INSERT INTO children VALUES (1,99,-1)",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := engine.Execute(session, "ALTER TABLE children ADD CONSTRAINT fk_parent FOREIGN KEY(parent_id) REFERENCES parents(id)"); !errors.Is(err, storage.ErrForeignKey) {
		t.Fatalf("invalid historical foreign key error = %v", err)
	}
	if _, err := engine.Execute(session, "ALTER TABLE children ADD CONSTRAINT ck_value CHECK(value >= 0)"); !errors.Is(err, storage.ErrCheckConstraint) {
		t.Fatalf("invalid historical CHECK error = %v", err)
	}
	show, err := engine.Execute(session, "SHOW CREATE TABLE children")
	if err != nil || strings.Contains(show.Rows[0][1].(string), "fk_parent") || strings.Contains(show.Rows[0][1].(string), "ck_value") {
		t.Fatalf("failed constraint ALTER changed metadata: %#v, %v", show, err)
	}
}

func TestCompositeIndexRangeOrderAndExplain(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE index_range_test",
		"USE index_range_test",
		"CREATE TABLE workouts(id INT,user_id INT,started_at DATETIME,PRIMARY KEY(id),KEY idx_user_started(user_id,started_at))",
		"INSERT INTO workouts VALUES (1,1,'2026-07-01 10:00:00'),(2,1,'2026-07-03 10:00:00'),(3,2,'2026-07-04 10:00:00'),(4,1,'2026-07-05 10:00:00')",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := engine.Execute(session, "SELECT id FROM workouts WHERE user_id=1 AND started_at>='2026-07-02' AND started_at<'2026-07-06' ORDER BY started_at DESC LIMIT 2")
	if err != nil || len(rows.Rows) != 2 || rows.Rows[0][0] != int64(4) || rows.Rows[1][0] != int64(2) {
		t.Fatalf("range index rows = %#v, %v", rows, err)
	}
	explain, err := engine.Execute(session, "EXPLAIN SELECT id FROM workouts WHERE user_id=1 AND started_at>='2026-07-02' ORDER BY started_at DESC LIMIT 2")
	if err != nil || len(explain.Rows) != 1 || explain.Rows[0][4] != "range" || explain.Rows[0][6] != "idx_user_started" || strings.Contains(fmt.Sprint(explain.Rows[0][11]), "filesort") {
		t.Fatalf("EXPLAIN = %#v, %v", explain, err)
	}
	count, err := engine.Execute(session, "SELECT COUNT(*) FROM workouts WHERE user_id=1 AND started_at>='2026-07-02'")
	if err != nil || count.Rows[0][0] != int64(2) {
		t.Fatalf("indexed COUNT = %#v, %v", count, err)
	}
}

func TestNavicatTableAndViewCopyUsesDailySequenceName(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE navicat_copy_test",
		"USE navicat_copy_test",
		"CREATE TABLE items(id INT NOT NULL,label VARCHAR(32),PRIMARY KEY(id)) COMMENT='source table'",
		"INSERT INTO items VALUES (1,'one'),(2,'two')",
		"CREATE VIEW active_items AS SELECT id,label FROM items WHERE id >= 2",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}

	if _, err := engine.Execute(session, "SHOW CREATE TABLE items"); err != nil {
		t.Fatal(err)
	}
	createdTable, err := engine.Execute(session, "CREATE TABLE items_copy(id INT NOT NULL,label VARCHAR(32),PRIMARY KEY(id)) COMMENT='source table'")
	if err != nil {
		t.Fatal(err)
	}
	if !createdTable.MetadataChanged {
		t.Fatal("renamed Navicat table copy should request a client metadata refresh")
	}
	date := time.Now().Format("060102")
	firstTableCopy := "items_copy_" + date + "01"
	if createdTable.Message != "table created as `"+firstTableCopy+"`" {
		t.Fatalf("create table message = %q", createdTable.Message)
	}
	shownTableCopy, err := engine.Execute(session, "SHOW CREATE TABLE items_copy")
	if err != nil || len(shownTableCopy.Rows) != 1 || shownTableCopy.Rows[0][0] != firstTableCopy {
		t.Fatalf("SHOW CREATE placeholder = %#v, %v", shownTableCopy, err)
	}
	insert, err := engine.Execute(session, "INSERT INTO items_copy SELECT * FROM navicat_copy_test.items")
	if err != nil || insert.AffectedRows != 2 {
		t.Fatalf("INSERT SELECT = %#v, %v", insert, err)
	}

	database, err := engine.Store.Database("navicat_copy_test")
	if err != nil {
		t.Fatal(err)
	}
	backupTable := findBackupRelation(t, database.ListTables(), "items_copy_")
	if backupTable != firstTableCopy {
		t.Fatalf("first table copy = %q, want %q", backupTable, firstTableCopy)
	}
	if _, err := database.Table("items_copy"); !errors.Is(err, storage.ErrTableNotFound) {
		t.Fatalf("Navicat placeholder table should not exist: %v", err)
	}
	rows, err := engine.Execute(session, "SELECT * FROM "+backupTable+" ORDER BY id")
	if err != nil || len(rows.Rows) != 2 || rows.Rows[1][1] != "two" {
		t.Fatalf("copied table rows = %#v, %v", rows, err)
	}

	separateConnection := &Session{CurrentDatabase: "navicat_copy_test"}
	secondTable, err := engine.Execute(separateConnection, "CREATE TABLE items_copy1(id INT NOT NULL,label VARCHAR(32),PRIMARY KEY(id)) COMMENT='source table'")
	if err != nil {
		t.Fatal(err)
	}
	secondTableCopy := "items_copy_" + date + "02"
	if secondTable.Message != "table created as `"+secondTableCopy+"`" {
		t.Fatalf("second create table message = %q", secondTable.Message)
	}
	if _, err := engine.Execute(separateConnection, "INSERT INTO items_copy1 SELECT * FROM items"); err != nil {
		t.Fatal(err)
	}
	backupTables := 0
	for _, name := range database.ListTables() {
		if strings.HasPrefix(name, "items_copy_") {
			backupTables++
		}
	}
	if backupTables != 2 {
		t.Fatalf("backup table count = %d, tables = %#v", backupTables, database.ListTables())
	}

	if _, err := engine.Execute(session, "SHOW CREATE VIEW active_items"); err != nil {
		t.Fatal(err)
	}
	createdView, err := engine.Execute(session, "CREATE VIEW active_items_copy AS SELECT id,label FROM items WHERE id >= 2")
	if err != nil {
		t.Fatal(err)
	}
	if !createdView.MetadataChanged {
		t.Fatal("renamed Navicat view copy should request a client metadata refresh")
	}
	firstViewCopy := "active_items_copy_" + date + "01"
	if createdView.Message != "view created as `"+firstViewCopy+"`" {
		t.Fatalf("create view message = %q", createdView.Message)
	}
	shownViewCopy, err := engine.Execute(session, "SHOW CREATE VIEW active_items_copy")
	if err != nil || len(shownViewCopy.Rows) != 1 || shownViewCopy.Rows[0][0] != firstViewCopy {
		t.Fatalf("SHOW CREATE view placeholder = %#v, %v", shownViewCopy, err)
	}
	backupView := findBackupRelation(t, database.ListViews(), "active_items_copy_")
	if backupView != firstViewCopy {
		t.Fatalf("first view copy = %q, want %q", backupView, firstViewCopy)
	}
	viewRows, err := engine.Execute(session, "SELECT * FROM "+backupView)
	if err != nil || len(viewRows.Rows) != 1 || viewRows.Rows[0][0] != int64(2) {
		t.Fatalf("copied view rows = %#v, %v", viewRows, err)
	}

	if _, err := engine.Execute(session, "CREATE ALGORITHM=UNDEFINED DEFINER=`root`@`%` VIEW active_items AS SELECT id,label FROM items WHERE id >= 1"); err != nil {
		t.Fatal(err)
	}
	decoratedBackup := findBackupRelationWithRows(t, engine, session, database.ListViews(), "active_items_copy_", 2)
	secondViewCopy := "active_items_copy_" + date + "02"
	if decoratedBackup != secondViewCopy {
		t.Fatalf("second view copy = %q, want %q", decoratedBackup, secondViewCopy)
	}
	originalRows, err := engine.Execute(session, "SELECT * FROM active_items")
	if err != nil || len(originalRows.Rows) != 1 || originalRows.Rows[0][0] != int64(2) {
		t.Fatalf("original view changed after copy: %#v, %v", originalRows, err)
	}
	if _, err := engine.Execute(session, "SELECT * FROM "+decoratedBackup); err != nil {
		t.Fatalf("query decorated view backup: %v", err)
	}
	if _, err := engine.Execute(session, "CREATE VIEW active_items AS SELECT id,label FROM items"); err == nil {
		t.Fatal("plain CREATE VIEW should still reject an existing view")
	}
}

func TestNavicatTransferDropThenCreatePreservesOriginalNames(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	setup := &Session{}
	for _, query := range []string{
		"CREATE DATABASE navicat_transfer_test",
		"USE navicat_transfer_test",
		"CREATE TABLE transfer_a(id INT NOT NULL, label VARCHAR(16), PRIMARY KEY(id))",
		"CREATE TABLE transfer_b(id INT NOT NULL, amount BIGINT, PRIMARY KEY(id))",
		"CREATE TABLE transfer_c(id INT NOT NULL, created_at DATETIME, PRIMARY KEY(id))",
	} {
		if _, err := engine.Execute(setup, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}

	tableNames := []string{"transfer_a", "transfer_b", "transfer_c"}
	errorsFound := make(chan error, len(tableNames))
	var transfers sync.WaitGroup
	for _, tableName := range tableNames {
		tableName := tableName
		transfers.Add(1)
		go func() {
			defer transfers.Done()
			session := &Session{CurrentDatabase: "navicat_transfer_test"}
			shown, showErr := engine.Execute(session, "SHOW CREATE TABLE `"+tableName+"`")
			if showErr != nil {
				errorsFound <- fmt.Errorf("show %s: %w", tableName, showErr)
				return
			}
			if len(shown.Rows) != 1 || shown.Rows[0][0] != tableName {
				errorsFound <- fmt.Errorf("show %s returned %#v", tableName, shown.Rows)
				return
			}
			ddl, ok := shown.Rows[0][1].(string)
			if !ok {
				errorsFound <- fmt.Errorf("show %s returned non-string DDL %#v", tableName, shown.Rows[0][1])
				return
			}
			if _, dropErr := engine.Execute(session, "DROP TABLE `"+tableName+"`"); dropErr != nil {
				errorsFound <- fmt.Errorf("drop %s: %w", tableName, dropErr)
				return
			}
			created, createErr := engine.Execute(session, ddl)
			if createErr != nil {
				errorsFound <- fmt.Errorf("recreate %s: %w", tableName, createErr)
				return
			}
			if created.MetadataChanged {
				errorsFound <- fmt.Errorf("recreate %s was incorrectly treated as a copy", tableName)
			}
		}()
	}
	transfers.Wait()
	close(errorsFound)
	for transferErr := range errorsFound {
		t.Error(transferErr)
	}

	database, err := engine.Store.Database("navicat_transfer_test")
	if err != nil {
		t.Fatal(err)
	}
	if names := database.ListTables(); !equalStrings(names, tableNames) {
		t.Fatalf("transferred table names = %#v, want %#v", names, tableNames)
	}

	viewSession := &Session{CurrentDatabase: "navicat_transfer_test"}
	if _, err := engine.Execute(viewSession, "CREATE VIEW transfer_view AS SELECT id,label FROM transfer_a"); err != nil {
		t.Fatal(err)
	}
	shownView, err := engine.Execute(viewSession, "SHOW CREATE VIEW transfer_view")
	if err != nil {
		t.Fatal(err)
	}
	viewDDL := shownView.Rows[0][1].(string)
	if _, err := engine.Execute(viewSession, "DROP VIEW transfer_view"); err != nil {
		t.Fatal(err)
	}
	createdView, err := engine.Execute(viewSession, viewDDL)
	if err != nil {
		t.Fatal(err)
	}
	if createdView.MetadataChanged {
		t.Fatal("recreated view was incorrectly treated as a copy")
	}
	if views := database.ListViews(); len(views) != 1 || views[0] != "transfer_view" {
		t.Fatalf("transferred view names = %#v", views)
	}
}

func TestNavicatParallelTransferDefersDecoratedViewDependencyValidation(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE navicat_parallel_transfer_test",
		"USE navicat_parallel_transfer_test",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}

	if _, err := engine.Execute(session, "CREATE VIEW strict_missing AS SELECT id FROM orders"); err == nil {
		t.Fatal("plain CREATE VIEW should reject a missing dependency")
	}
	if _, err := engine.Execute(session, "CREATE ALGORITHM=UNDEFINED SQL SECURITY DEFINER VIEW large_orders AS SELECT id,amount FROM orders WHERE amount >= 50"); err != nil {
		t.Fatalf("decorated transfer view before its table: %v", err)
	}
	if _, err := engine.Execute(session, "SELECT * FROM large_orders"); !errors.Is(err, errRelationNotFound) {
		t.Fatalf("pending view query error = %v", err)
	}
	if _, err := engine.Execute(session, "CREATE TABLE orders(id INT NOT NULL, amount DECIMAL(10,2) NOT NULL, PRIMARY KEY(id))"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Execute(session, "INSERT INTO orders VALUES (1,25),(2,75)"); err != nil {
		t.Fatal(err)
	}
	rows, err := engine.Execute(session, "SELECT id,amount FROM large_orders")
	if err != nil || len(rows.Rows) != 1 || rows.Rows[0][0] != int64(2) {
		t.Fatalf("resolved transferred view = %#v, %v", rows, err)
	}

	if _, err := engine.Execute(session, "CREATE ALGORITHM=UNDEFINED VIEW invalid_columns AS SELECT missing FROM orders"); err == nil {
		t.Fatal("decorated CREATE VIEW should still reject an invalid column")
	}
	database, err := engine.Store.Database("navicat_parallel_transfer_test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.View("invalid_columns"); !errors.Is(err, storage.ErrViewNotFound) {
		t.Fatalf("invalid decorated view should be rolled back: %v", err)
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func findBackupRelation(t *testing.T, names []string, prefix string) string {
	t.Helper()
	for _, name := range names {
		matched, err := regexp.MatchString("^"+regexp.QuoteMeta(prefix)+`\d{8}$`, name)
		if err == nil && matched {
			return name
		}
	}
	t.Fatalf("backup relation with prefix %q not found in %#v", prefix, names)
	return ""
}

func findBackupRelationWithRows(t *testing.T, engine *Engine, session *Session, names []string, prefix string, wantRows int) string {
	t.Helper()
	for _, name := range names {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		result, err := engine.Execute(session, "SELECT * FROM "+name)
		if err == nil && len(result.Rows) == wantRows {
			return name
		}
	}
	t.Fatalf("backup relation with prefix %q and %d rows not found in %#v", prefix, wantRows, names)
	return ""
}

func TestCommentOnlyQueryIsNoOp(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"-- source comment", "# mysql comment", "/* import comment */"} {
		if _, err := engine.Execute(&Session{}, query); err != nil {
			t.Fatalf("%q: %v", query, err)
		}
	}
}

func TestIndexDDLMetadataAndPersistence(t *testing.T) {
	directory := t.TempDir()
	engine, err := Open(directory, "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE index_test",
		"CREATE TABLE index_test.items(id INT,label VARCHAR(32))",
		"INSERT INTO index_test.items VALUES (1,'one'),(2,'two')",
		"ALTER TABLE index_test.items ADD UNIQUE INDEX item_id(id)",
		"CREATE INDEX item_label ON index_test.items(label)",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	result, err := engine.Execute(session, "SHOW INDEX FROM index_test.items")
	if err != nil || len(result.Rows) != 2 {
		t.Fatalf("SHOW INDEX = %#v, %v", result, err)
	}
	if _, err := engine.Execute(session, "INSERT INTO index_test.items VALUES (1,'duplicate')"); !errors.Is(err, storage.ErrDuplicateKey) {
		t.Fatalf("duplicate insert error = %v", err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(directory, "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	result, err = reopened.Execute(session, "SHOW INDEX FROM index_test.items")
	if err != nil || len(result.Rows) != 2 {
		t.Fatalf("persisted SHOW INDEX = %#v, %v", result, err)
	}
	if _, err := reopened.Execute(session, "ALTER TABLE index_test.items DROP INDEX item_label"); err != nil {
		t.Fatal(err)
	}
}

func TestCreateTableInlineIndexesPersistAndExport(t *testing.T) {
	directory := t.TempDir()
	engine, err := Open(directory, "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{CurrentDatabase: "inline_index_test"}
	for _, query := range []string{
		"CREATE DATABASE inline_index_test",
		"CREATE TABLE items(id INT NOT NULL,sku VARCHAR(32) NOT NULL,quantity INT,PRIMARY KEY(id),UNIQUE KEY uq_sku(sku),KEY idx_quantity(quantity))",
		"INSERT INTO items VALUES (1,'SKU-001',2)",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	if _, err := engine.Execute(session, "INSERT INTO items VALUES (2,'SKU-001',3)"); !errors.Is(err, storage.ErrDuplicateKey) {
		t.Fatalf("inline unique index duplicate error = %v", err)
	}
	indexes, err := engine.Execute(session, "SHOW INDEX FROM items")
	if err != nil || len(indexes.Rows) != 3 {
		t.Fatalf("inline SHOW INDEX = %#v, %v", indexes, err)
	}
	create, err := engine.Execute(session, "SHOW CREATE TABLE items")
	if err != nil {
		t.Fatal(err)
	}
	definition := create.Rows[0][1].(string)
	for _, expected := range []string{"PRIMARY KEY (`id`)", "UNIQUE KEY `uq_sku` (`sku`)", "KEY `idx_quantity` (`quantity`)"} {
		if !strings.Contains(definition, expected) {
			t.Fatalf("SHOW CREATE TABLE is missing %q:\n%s", expected, definition)
		}
	}
	output := filepath.Join(t.TempDir(), "inline-index.sql")
	if err := ExportSQL(engine.Store, "inline_index_test", output); err != nil {
		t.Fatal(err)
	}
	exported, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(exported), "UNIQUE KEY `uq_sku` (`sku`)") || !strings.Contains(string(exported), "KEY `idx_quantity` (`quantity`)") {
		t.Fatalf("export omitted inline indexes:\n%s", exported)
	}
	if _, err := engine.Execute(session, "CREATE TABLE invalid_index(id INT,KEY idx_missing(missing))"); err == nil {
		t.Fatal("expected invalid inline index column to fail")
	}
	database, err := engine.Store.Database("inline_index_test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Table("invalid_index"); !errors.Is(err, storage.ErrTableNotFound) {
		t.Fatalf("failed CREATE TABLE left a partial table: %v", err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(directory, "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	indexes, err = reopened.Execute(session, "SHOW INDEX FROM items")
	if err != nil || len(indexes.Rows) != 3 {
		t.Fatalf("persisted inline SHOW INDEX = %#v, %v", indexes, err)
	}
}

func TestPrimaryKeyColumnMetadataDefaultsAndPersistence(t *testing.T) {
	directory := t.TempDir()
	engine, err := Open(directory, "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE navicat_edit_test",
		"USE navicat_edit_test",
		"CREATE TABLE items(id INT NOT NULL AUTO_INCREMENT,name VARCHAR(64) NULL DEFAULT NULL,updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,PRIMARY KEY(id))",
		"INSERT INTO items(name) VALUES ('first')",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	describe, err := engine.Execute(session, "SHOW COLUMNS FROM items")
	if err != nil {
		t.Fatal(err)
	}
	if len(describe.Rows) != 3 || describe.Rows[0][2] != "NO" || describe.Rows[0][3] != "PRI" || describe.Rows[0][5] != "auto_increment" {
		t.Fatalf("unexpected column metadata: %#v", describe.Rows)
	}
	indexes, err := engine.Execute(session, "SHOW INDEX FROM items")
	if err != nil || len(indexes.Rows) != 1 || indexes.Rows[0][2] != "PRIMARY" {
		t.Fatalf("unexpected primary index: %#v, %v", indexes.Rows, err)
	}
	rows, err := engine.Execute(session, "SELECT * FROM items")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Rows) != 1 || rows.Rows[0][0] != int64(1) || !rows.Columns[0].PrimaryKey || rows.Columns[0].Nullable || rows.Columns[0].Table != "items" || rows.Columns[0].Schema != "navicat_edit_test" {
		t.Fatalf("unexpected editable result metadata: %#v rows=%#v", rows.Columns, rows.Rows)
	}
	create, err := engine.Execute(session, "SHOW CREATE TABLE items")
	if err != nil || !strings.Contains(create.Rows[0][1].(string), "PRIMARY KEY (`id`)") || !strings.Contains(create.Rows[0][1].(string), "`id` int NOT NULL AUTO_INCREMENT") {
		t.Fatalf("unexpected create table: %#v, %v", create, err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(directory, "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	describe, err = reopened.Execute(session, "SHOW COLUMNS FROM items")
	if err != nil || describe.Rows[0][3] != "PRI" || describe.Rows[0][2] != "NO" {
		t.Fatalf("persisted metadata: %#v, %v", describe, err)
	}
}

func TestAlterTableAddPrimaryKeyIsAtomic(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{}
	for _, query := range []string{"CREATE DATABASE primary_alter_test", "USE primary_alter_test", "CREATE TABLE items(id INT,name VARCHAR(20))", "INSERT INTO items VALUES (1,'a'),(1,'b')"} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	if _, err := engine.Execute(session, "ALTER TABLE items ADD PRIMARY KEY(id)"); !errors.Is(err, storage.ErrDuplicateKey) {
		t.Fatalf("duplicate primary key error = %v", err)
	}
	indexes, err := engine.Execute(session, "SHOW INDEX FROM items")
	if err != nil || len(indexes.Rows) != 0 {
		t.Fatalf("failed ALTER changed indexes: %#v, %v", indexes.Rows, err)
	}
}

func TestAlterColumnDateTimeMetadataAndPersistence(t *testing.T) {
	directory := t.TempDir()
	engine, err := Open(directory, "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE alter_column_test",
		"USE alter_column_test",
		"CREATE TABLE events(id INT,last_time DATE)",
		"INSERT INTO events VALUES (1,'2026-07-28')",
		"CREATE INDEX events_last_time ON events(last_time)",
		"ALTER TABLE events MODIFY COLUMN last_time DATETIME NULL DEFAULT NULL COMMENT 'last access' AFTER id",
		"UPDATE events SET last_time='2026-07-28 15:16:17' WHERE id=1",
		"ALTER TABLE events CHANGE COLUMN last_time updated_at DATETIME",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	describe, err := engine.Execute(session, "DESCRIBE events")
	if err != nil {
		t.Fatal(err)
	}
	if len(describe.Rows) != 2 || describe.Rows[1][0] != "updated_at" || describe.Rows[1][1] != "datetime" {
		t.Fatalf("unexpected DESCRIBE: %#v", describe.Rows)
	}
	show, err := engine.Execute(session, "SHOW CREATE TABLE events")
	if err != nil {
		t.Fatal(err)
	}
	ddl := show.Rows[0][1].(string)
	if !strings.Contains(ddl, "`updated_at` datetime") || !strings.Contains(ddl, "`events_last_time` (`updated_at`)") {
		t.Fatalf("unexpected SHOW CREATE TABLE: %s", ddl)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(directory, "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	database, err := reopened.Store.Database("alter_column_test")
	if err != nil {
		t.Fatal(err)
	}
	table, err := database.Table("events")
	if err != nil {
		t.Fatal(err)
	}
	columns, rows := table.Columns(), table.Select(nil)
	if columns[1].Type != storage.TypeDateTime || rows[0][1].String() != "2026-07-28 15:16:17" {
		t.Fatalf("unexpected persisted datetime: column=%#v row=%#v", columns[1], rows[0][1])
	}
}

func TestNavicatStyleUpdateLimit(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE navicat_update_test",
		"CREATE TABLE navicat_update_test.items(id INT,label VARCHAR(32),note TEXT)",
		"INSERT INTO navicat_update_test.items VALUES (1,'first',NULL),(2,'second',NULL)",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	result, err := engine.Execute(session, "UPDATE `navicat_update_test`.`items` SET `label`='changed' WHERE `note` <=> NULL LIMIT 1")
	if err != nil {
		t.Fatal(err)
	}
	if result.AffectedRows != 1 {
		t.Fatalf("affected rows = %d, want 1", result.AffectedRows)
	}
	database, err := engine.Store.Database("navicat_update_test")
	if err != nil {
		t.Fatal(err)
	}
	table, err := database.Table("items")
	if err != nil {
		t.Fatal(err)
	}
	rows := table.Select(nil)
	if rows[0][1].Text != "changed" || rows[1][1].Text != "second" {
		t.Fatalf("LIMIT update changed unexpected rows: %#v", rows)
	}
}

func TestUpdateExpressionsAreRowAwareAndAtomic(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE update_expression_test",
		"USE update_expression_test",
		"CREATE TABLE accounts(id INT,balance INT,snapshot INT,enabled BOOLEAN,note VARCHAR(20),updated_at DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,PRIMARY KEY(id),CHECK(balance>=0))",
		"INSERT INTO accounts(id,balance,snapshot,enabled,note) VALUES (1,10,0,TRUE,'old'),(2,20,0,FALSE,'old')",
		"UPDATE accounts SET balance=balance+5,snapshot=balance*2,enabled=NOT enabled,note=CASE WHEN balance>=15 THEN 'funded' ELSE 'empty' END WHERE id=1",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	rows, err := engine.Execute(session, "SELECT balance,snapshot,enabled,note,updated_at FROM accounts WHERE id=1")
	if err != nil || len(rows.Rows) != 1 || rows.Rows[0][0] != int64(15) || rows.Rows[0][1] != int64(30) || rows.Rows[0][2] != false || rows.Rows[0][3] != "funded" || rows.Rows[0][4] == nil {
		t.Fatalf("expression UPDATE row = %#v, %v", rows, err)
	}
	if _, err := engine.Execute(session, "UPDATE accounts SET note=NULL WHERE id=2"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Execute(session, "UPDATE accounts SET balance=balance-100 WHERE id IN (1,2)"); !errors.Is(err, storage.ErrCheckConstraint) {
		t.Fatalf("atomic CHECK error = %v", err)
	}
	rows, err = engine.Execute(session, "SELECT id,balance,note FROM accounts ORDER BY id")
	if err != nil || len(rows.Rows) != 2 || rows.Rows[0][1] != int64(15) || rows.Rows[1][1] != int64(20) || rows.Rows[1][2] != nil {
		t.Fatalf("rows after failed expression UPDATE = %#v, %v", rows, err)
	}
	if _, err := engine.Execute(session, "UPDATE accounts SET balance=1,balance=2 WHERE id=1"); err == nil {
		t.Fatal("duplicate UPDATE assignment succeeded")
	}
}

func TestNavicatStyleDeleteLimit(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE navicat_delete_test",
		"CREATE TABLE navicat_delete_test.items(id INT,label VARCHAR(32),note TEXT)",
		"INSERT INTO navicat_delete_test.items VALUES (1,'first',NULL),(2,'second',NULL)",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	result, err := engine.Execute(session, "DELETE FROM `navicat_delete_test`.`items` WHERE `note` <=> NULL LIMIT 1")
	if err != nil {
		t.Fatal(err)
	}
	if result.AffectedRows != 1 {
		t.Fatalf("affected rows = %d, want 1", result.AffectedRows)
	}
	database, err := engine.Store.Database("navicat_delete_test")
	if err != nil {
		t.Fatal(err)
	}
	table, err := database.Table("items")
	if err != nil {
		t.Fatal(err)
	}
	rows := table.Select(nil)
	if len(rows) != 1 || rows[0][0].Int64 != 2 {
		t.Fatalf("LIMIT delete removed unexpected rows: %#v", rows)
	}
}

func TestNavicatStyleDateUpdateMatchesStoredDate(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE navicat_date_update_test",
		"CREATE TABLE navicat_date_update_test.items(id INT,happened DATE,updated_at DATETIME)",
		"INSERT INTO navicat_date_update_test.items VALUES (1,'2026-07-28','2026-07-28 15:16:17')",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	result, err := engine.Execute(session, "UPDATE navicat_date_update_test.items SET happened='2026-07-29',updated_at='2026-07-29 16:17:18' WHERE id <=> 1 AND happened <=> '2026-07-28' AND updated_at <=> '2026-07-28 15:16:17' LIMIT 1")
	if err != nil {
		t.Fatal(err)
	}
	if result.AffectedRows != 1 {
		t.Fatalf("affected rows = %d, want 1", result.AffectedRows)
	}
	database, err := engine.Store.Database("navicat_date_update_test")
	if err != nil {
		t.Fatal(err)
	}
	table, err := database.Table("items")
	if err != nil {
		t.Fatal(err)
	}
	row := table.Select(nil)[0]
	if row[1].String() != "2026-07-29" || row[2].String() != "2026-07-29 16:17:18" {
		t.Fatalf("unexpected updated dates: %#v", row)
	}
}

func TestSelectProjectionOrderByUnselectedColumn(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE projection_test",
		"USE projection_test",
		"CREATE TABLE users(id INT, name VARCHAR(20), age INT)",
		"INSERT INTO users VALUES (1, 'Alice', 30), (2, 'Bob', 20), (3, 'Carol', 25)",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatal(err)
		}
	}
	result, err := engine.Execute(session, "SELECT name FROM users ORDER BY age DESC LIMIT 2")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 2 || len(result.Rows[0]) != 1 || result.Rows[0][0] != "Alice" || result.Rows[1][0] != "Carol" {
		t.Fatalf("unexpected result: %#v", result.Rows)
	}
}

func TestOrderByPredicateExpressions(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE order_expression_test",
		"USE order_expression_test",
		"CREATE TABLE items(id INT,owner_user_id INT NULL,visibility VARCHAR(16),revoked_at DATETIME NULL)",
		"INSERT INTO items VALUES (1,NULL,'private',NULL),(2,2,'global','2026-08-01 10:00:00'),(3,NULL,'global','2026-08-01 11:00:00'),(4,3,'private',NULL)",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	result, err := engine.Execute(session, `SELECT id FROM items
		ORDER BY owner_user_id IS NULL DESC, visibility='global' DESC, revoked_at IS NULL DESC, id`)
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{3, 1, 2, 4}
	if len(result.Rows) != len(want) {
		t.Fatalf("ORDER BY predicate row count = %#v", result.Rows)
	}
	for index, row := range result.Rows {
		if row[0] != want[index] {
			t.Fatalf("ORDER BY predicate row %d = %#v, want %d", index, row, want[index])
		}
	}
	caseResult, err := engine.Execute(session, `SELECT id FROM items
		ORDER BY CASE WHEN owner_user_id IS NULL THEN 1 ELSE 0 END DESC,
		         CASE WHEN visibility='global' THEN 1 ELSE 0 END DESC, id`)
	if err != nil {
		t.Fatalf("ORDER BY CASE expression: %v", err)
	}
	caseWant := []int64{3, 1, 2, 4}
	if len(caseResult.Rows) != len(caseWant) {
		t.Fatalf("ORDER BY CASE row count = %#v", caseResult.Rows)
	}
	for index, row := range caseResult.Rows {
		if row[0] != caseWant[index] {
			t.Fatalf("ORDER BY CASE row %d = %#v, want %d", index, row, caseWant[index])
		}
	}
}

func TestSelectLikeAndGroupBy(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE aggregate_test",
		"USE aggregate_test",
		"CREATE TABLE codes(id INT, auth_code VARCHAR(64))",
		"INSERT INTO codes VALUES (1, 'abc-000'), (2, 'abc-000'), (3, 'ABC-100'), (4, 'other')",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatal(err)
		}
	}
	result, err := engine.Execute(session, "SELECT auth_code, COUNT(*) AS total FROM codes WHERE auth_code LIKE '%000%' GROUP BY auth_code ORDER BY total DESC")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 || result.Rows[0][0] != "abc-000" || result.Rows[0][1] != int64(2) {
		t.Fatalf("unexpected grouped result: %#v", result.Rows)
	}
	like, err := engine.Execute(session, "SELECT auth_code FROM codes WHERE auth_code LIKE 'abc-___' ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(like.Rows) != 3 {
		t.Fatalf("unexpected LIKE result: %#v", like.Rows)
	}
}

func TestAggregateGroupHavingAndOrdering(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE aggregate_full_test",
		"USE aggregate_full_test",
		"CREATE TABLE sales(id INT, region VARCHAR(20), category VARCHAR(20), amount INT, score DOUBLE, note TEXT)",
		"INSERT INTO sales VALUES (1,'east','a',10,1.5,'x'),(2,'east','a',20,2.5,NULL),(3,'east','b',5,3.5,'z'),(4,'west','a',30,4.5,NULL),(5,'west','a',NULL,5.5,'w'),(6,'north','c',7,1.0,'n')",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}

	grouped, err := engine.Execute(session, "SELECT region, COUNT(*) AS rows_count, COUNT(amount) AS valued_count, SUM(amount) AS total, AVG(amount) AS average, MIN(amount) AS minimum, MAX(amount) AS maximum FROM sales WHERE region != 'north' GROUP BY region HAVING total >= 30 ORDER BY total DESC, region ASC LIMIT 10")
	if err != nil {
		t.Fatal(err)
	}
	want := [][]any{
		{"east", int64(3), int64(3), int64(35), float64(35) / 3, int64(5), int64(20)},
		{"west", int64(2), int64(1), int64(30), float64(30), int64(30), int64(30)},
	}
	if len(grouped.Rows) != len(want) {
		t.Fatalf("unexpected grouped rows: %#v", grouped.Rows)
	}
	for rowIndex := range want {
		for columnIndex := range want[rowIndex] {
			if compareAny(grouped.Rows[rowIndex][columnIndex], want[rowIndex][columnIndex]) != 0 {
				t.Fatalf("grouped row %d column %d = %#v, want %#v", rowIndex, columnIndex, grouped.Rows[rowIndex][columnIndex], want[rowIndex][columnIndex])
			}
		}
	}
	functionHaving, err := engine.Execute(session, "SELECT region, COUNT(*) AS total, SUM(amount) AS amount_total FROM sales GROUP BY region HAVING COUNT(*) > 1 AND SUM(amount) >= 30 ORDER BY SUM(amount) DESC")
	if err != nil {
		t.Fatal(err)
	}
	if len(functionHaving.Rows) != 2 || functionHaving.Rows[0][0] != "east" || functionHaving.Rows[1][0] != "west" {
		t.Fatalf("unexpected function HAVING result: %#v", functionHaving.Rows)
	}

	byOrdinal, err := engine.Execute(session, "SELECT region area, category, COUNT(1) total FROM sales GROUP BY 1, category ORDER BY total DESC, area ASC LIMIT 2")
	if err != nil {
		t.Fatal(err)
	}
	if len(byOrdinal.Rows) != 2 || byOrdinal.Rows[0][0] != "east" || byOrdinal.Rows[0][1] != "a" || byOrdinal.Rows[0][2] != int64(2) || byOrdinal.Rows[1][0] != "west" || byOrdinal.Rows[1][2] != int64(2) {
		t.Fatalf("unexpected ordinal grouping: %#v", byOrdinal.Rows)
	}
}

func TestScalarAggregatesAndAliasOrdering(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE aggregate_scalar_test",
		"USE aggregate_scalar_test",
		"CREATE TABLE sales(id INT, region VARCHAR(20), amount INT, note TEXT)",
		"INSERT INTO sales VALUES (1,'east',10,'x'),(2,'east',20,NULL),(3,'east',5,'z'),(4,'west',30,NULL),(5,'west',NULL,'w'),(6,'north',7,'n')",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}

	aggregates, err := engine.Execute(session, "SELECT COUNT(*) total_rows, COUNT(note) notes, SUM(amount) total, AVG(amount) average, MIN(amount) minimum, MAX(amount) maximum FROM sales")
	if err != nil {
		t.Fatal(err)
	}
	row := aggregates.Rows[0]
	if row[0] != int64(6) || row[1] != int64(4) || row[2] != int64(72) || row[3] != float64(14.4) || row[4] != int64(5) || row[5] != int64(30) {
		t.Fatalf("unexpected scalar aggregates: %#v", row)
	}

	ordered, err := engine.Execute(session, "SELECT region AS area, amount AS value FROM sales WHERE amount IS NOT NULL ORDER BY value DESC, area ASC LIMIT 2 OFFSET 1")
	if err != nil {
		t.Fatal(err)
	}
	if len(ordered.Rows) != 2 || ordered.Rows[0][0] != "east" || ordered.Rows[0][1] != int64(20) || ordered.Rows[1][1] != int64(10) {
		t.Fatalf("unexpected alias ordering: %#v", ordered.Rows)
	}

	ordinal, err := engine.Execute(session, "SELECT region, amount FROM sales WHERE amount IS NOT NULL ORDER BY 2 DESC, 1 ASC LIMIT 2")
	if err != nil {
		t.Fatal(err)
	}
	if len(ordinal.Rows) != 2 || ordinal.Rows[0][0] != "west" || ordinal.Rows[0][1] != int64(30) || ordinal.Rows[1][1] != int64(20) {
		t.Fatalf("unexpected ordinal ordering: %#v", ordinal.Rows)
	}

	limitedCount, err := engine.Execute(session, "SELECT COUNT(*) FROM sales LIMIT 10")
	if err != nil || len(limitedCount.Rows) != 1 || limitedCount.Rows[0][0] != int64(6) {
		t.Fatalf("unexpected limited count: %#v, %v", limitedCount.Rows, err)
	}
	zeroCount, err := engine.Execute(session, "SELECT COUNT(*) FROM sales LIMIT 0")
	if err != nil || len(zeroCount.Rows) != 0 {
		t.Fatalf("unexpected LIMIT 0 count: %#v, %v", zeroCount.Rows, err)
	}
}

func TestDerivedTableAggregationFilteringAndStreaming(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{StreamResults: true}
	for _, query := range []string{
		"CREATE DATABASE derived_test",
		"USE derived_test",
		"CREATE TABLE auth_code_equ(id INT, auth_code VARCHAR(64))",
		"INSERT INTO auth_code_equ VALUES (1,'a'),(2,'a'),(3,'b'),(4,'c'),(5,'c'),(6,'c')",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}

	result, err := engine.Execute(session, "SELECT SUM(v1.count1) FROM (SELECT auth_code,COUNT(*) AS count1 FROM auth_code_equ GROUP BY auth_code) v1")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 || result.Rows[0][0] != int64(6) {
		t.Fatalf("unexpected derived SUM: %#v", result.Rows)
	}

	filtered, err := engine.Execute(session, "SELECT v1.auth_code,v1.count1 FROM (SELECT auth_code,COUNT(*) AS count1 FROM auth_code_equ GROUP BY auth_code) AS v1 WHERE v1.count1 >= 2 ORDER BY v1.count1 DESC,v1.auth_code ASC LIMIT 2")
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Rows) != 2 || filtered.Rows[0][0] != "c" || filtered.Rows[0][1] != int64(3) || filtered.Rows[1][0] != "a" || filtered.Rows[1][1] != int64(2) {
		t.Fatalf("unexpected filtered derived rows: %#v", filtered.Rows)
	}

	streamed, err := engine.Execute(session, "SELECT v.id FROM (SELECT id FROM auth_code_equ WHERE id >= 3) v ORDER BY v.id DESC LIMIT 2")
	if err != nil {
		t.Fatal(err)
	}
	if len(streamed.Rows) != 2 || streamed.Rows[0][0] != int64(6) || streamed.Rows[1][0] != int64(5) {
		t.Fatalf("unexpected streamed derived rows: %#v", streamed.Rows)
	}

	aliased, err := engine.Execute(session, "SELECT e.id FROM auth_code_equ AS e WHERE e.auth_code = 'b'")
	if err != nil {
		t.Fatal(err)
	}
	var aliasRows [][]any
	if err := aliased.StreamRows(func(row []any) error {
		aliasRows = append(aliasRows, row)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(aliasRows) != 1 || aliasRows[0][0] != int64(3) {
		t.Fatalf("unexpected aliased table rows: %#v", aliasRows)
	}
}

func TestScalarFunctionProjectionAndLockingRead(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{StreamResults: true}
	for _, query := range []string{
		"CREATE DATABASE function_test",
		"USE function_test",
		"CREATE TABLE users(id INT,name VARCHAR(64),city VARCHAR(64),note TEXT)",
		"INSERT INTO users VALUES (1,'Alice','Shanghai',NULL),(2,NULL,'Beijing','ready')",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}

	result, err := engine.Execute(session, "SELECT id,COALESCE(name,''),IFNULL(note,'none'),CONCAT(LOWER(COALESCE(name,'unknown')),'-',UPPER(city)),CHAR_LENGTH(COALESCE(name,'')) AS name_length FROM users ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	want := [][]any{{int64(1), "Alice", "none", "alice-SHANGHAI", int64(5)}, {int64(2), "", "ready", "unknown-BEIJING", int64(0)}}
	if len(result.Rows) != len(want) {
		t.Fatalf("unexpected function rows: %#v", result.Rows)
	}
	for rowIndex := range want {
		for columnIndex := range want[rowIndex] {
			if compareAny(result.Rows[rowIndex][columnIndex], want[rowIndex][columnIndex]) != 0 {
				t.Fatalf("row %d column %d = %#v, want %#v", rowIndex, columnIndex, result.Rows[rowIndex][columnIndex], want[rowIndex][columnIndex])
			}
		}
	}

	locking, err := engine.Execute(session, "SELECT id,COALESCE(note,'') FROM users WHERE id=1 LIMIT 1 FOR UPDATE")
	if err != nil {
		t.Fatal(err)
	}
	var rows [][]any
	if err := locking.StreamRows(func(row []any) error {
		rows = append(rows, row)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0][0] != int64(1) || rows[0][1] != "" {
		t.Fatalf("unexpected locking read: %#v", rows)
	}
}

func TestCommonMySQLScalarFunctions(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{}

	assertRow := func(query string, expected []any) {
		t.Helper()
		result, executeErr := engine.Execute(session, query)
		if executeErr != nil {
			t.Fatalf("%s: %v", query, executeErr)
		}
		if len(result.Rows) != 1 || len(result.Rows[0]) != len(expected) {
			t.Fatalf("%s returned %#v", query, result.Rows)
		}
		for index, want := range expected {
			if compareAny(result.Rows[0][index], want) != 0 {
				t.Fatalf("%s column %d = %#v, want %#v", query, index, result.Rows[0][index], want)
			}
		}
	}

	assertRow(
		"SELECT IF(0,'yes','no'),GREATEST(3,7,5),LEAST(3,7,5),LEFT('你好世界',2),RIGHT('你好世界',2),SUBSTRING('你好世界',2,2),REPLACE('hello','ll','pp'),REVERSE('abc'),REPEAT('ab',2),LPAD('7',3,'0'),RPAD('7',3,'0'),LTRIM('  x '),RTRIM(' x  '),LOCATE('世界','你好世界'),INSTR('你好世界','世界'),ASCII('A'),LENGTH(SPACE(3))",
		[]any{"no", int64(7), int64(3), "你好", "世界", "好世", "heppo", "cba", "abab", "007", "700", "x ", " x", int64(3), int64(3), int64(65), int64(3)},
	)
	assertRow(
		"SELECT ABS(-5),CEIL(1.2),FLOOR(1.8),ROUND(12.345,2),TRUNCATE(12.345,2),MOD(7,4),POW(2,3),SQRT(9),SIGN(-5),LOG(2,8)",
		[]any{float64(5), float64(2), float64(1), float64(12.35), float64(12.34), float64(3), float64(8), float64(3), int64(-1), float64(3)},
	)
	assertRow(
		"SELECT YEAR('2026-07-28'),MONTH('2026-07-28'),DAYOFMONTH('2026-07-28'),DAYOFWEEK('2026-07-28'),WEEKDAY('2026-07-28'),QUARTER('2026-07-28'),HOUR('2026-07-28 13:14:15'),MINUTE('2026-07-28 13:14:15'),SECOND('2026-07-28 13:14:15'),DATEDIFF('2026-08-03','2026-07-28'),DATE_FORMAT('2026-07-28 13:14:15','%Y/%m/%d %H:%i:%s'),MONTHNAME('2026-07-28'),DAYNAME('2026-07-28')",
		[]any{int64(2026), int64(7), int64(28), int64(3), int64(1), int64(3), int64(13), int64(14), int64(15), int64(6), "2026/07/28 13:14:15", "July", "Tuesday"},
	)
	assertRow("SELECT LEFT(NULL,2),GREATEST(1,NULL),SQRT(-1),MOD(1,0)", []any{nil, nil, nil, nil})

	lastDay, err := engine.Execute(session, "SELECT LAST_DAY('2026-02-10')")
	if err != nil {
		t.Fatal(err)
	}
	date, ok := lastDay.Rows[0][0].(time.Time)
	if !ok || date.Format("2006-01-02") != "2026-02-28" {
		t.Fatalf("LAST_DAY returned %#v", lastDay.Rows)
	}

	for _, query := range []string{
		"CREATE DATABASE mysql_function_rows",
		"USE mysql_function_rows",
		"CREATE TABLE samples(n INT,label VARCHAR(32),happened DATE)",
		"INSERT INTO samples VALUES (-5,'你好世界','2026-07-28')",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	rowFunctions, err := engine.Execute(session, "SELECT ABS(n),LEFT(label,2),IF(n<0,'negative','positive'),YEAR(happened),DATE_FORMAT(happened,'%Y%m%d'),LAST_DAY(happened) FROM samples")
	if err != nil {
		t.Fatal(err)
	}
	if len(rowFunctions.Rows) != 1 || compareAny(rowFunctions.Rows[0][0], float64(5)) != 0 || rowFunctions.Rows[0][1] != "你好" || rowFunctions.Rows[0][2] != "negative" || rowFunctions.Rows[0][3] != int64(2026) || rowFunctions.Rows[0][4] != "20260728" {
		t.Fatalf("row function projection returned %#v", rowFunctions.Rows)
	}
	rowLastDay, ok := rowFunctions.Rows[0][5].(time.Time)
	if !ok || rowLastDay.Format("2006-01-02") != "2026-07-31" {
		t.Fatalf("row LAST_DAY returned %#v", rowFunctions.Rows[0][5])
	}
}

func TestInnerLeftRightAndDerivedJoins(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{StreamResults: true}
	for _, query := range []string{
		"CREATE DATABASE join_test",
		"USE join_test",
		"CREATE TABLE auth_app(id VARCHAR(32) NOT NULL,app_name VARCHAR(64),PRIMARY KEY(id))",
		"CREATE TABLE auth_code(id VARCHAR(32) NOT NULL,app_id VARCHAR(32),auth_code VARCHAR(64),status VARCHAR(10),create_time DATE,PRIMARY KEY(id))",
		"CREATE TABLE auth_code_equ(id VARCHAR(32) NOT NULL,code_id VARCHAR(32),PRIMARY KEY(id))",
		"INSERT INTO auth_app VALUES ('app-1','Desktop'),('app-2','Server')",
		"INSERT INTO auth_code VALUES ('code-1','app-1','AAA','10','2026-03-01'),('code-2','missing','BBB','10','2026-03-02'),('code-3','app-2','CCC','-1','2026-03-03')",
		"INSERT INTO auth_code_equ VALUES ('equ-1','code-1'),('equ-2','code-1'),('equ-3','code-3')",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}

	query := `SELECT c.id,COALESCE(a.app_name,''),COALESCE(e.used_count,0)
		FROM auth_code c
		LEFT JOIN auth_app a ON a.id=c.app_id
		LEFT JOIN (SELECT code_id,COUNT(*) AS used_count FROM auth_code_equ GROUP BY code_id) e ON e.code_id=c.id
		WHERE c.status='10' ORDER BY c.create_time DESC`
	result, err := engine.Execute(session, query)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]any{{"code-2", "", int64(0)}, {"code-1", "Desktop", int64(2)}}
	if len(result.Rows) != len(want) {
		t.Fatalf("unexpected LEFT JOIN rows: %#v", result.Rows)
	}
	for rowIndex := range want {
		for columnIndex := range want[rowIndex] {
			if compareAny(result.Rows[rowIndex][columnIndex], want[rowIndex][columnIndex]) != 0 {
				t.Fatalf("row %d column %d = %#v, want %#v", rowIndex, columnIndex, result.Rows[rowIndex][columnIndex], want[rowIndex][columnIndex])
			}
		}
	}

	inner, err := engine.Execute(session, "SELECT c.id,a.app_name FROM auth_code c INNER JOIN auth_app a ON a.id=c.app_id ORDER BY c.id")
	if err != nil {
		t.Fatal(err)
	}
	if len(inner.Rows) != 2 || inner.Rows[0][0] != "code-1" || inner.Rows[1][0] != "code-3" {
		t.Fatalf("unexpected INNER JOIN rows: %#v", inner.Rows)
	}

	right, err := engine.Execute(session, "SELECT a.app_name,c.id FROM auth_app a RIGHT JOIN auth_code c ON c.app_id=a.id ORDER BY c.id")
	if err != nil {
		t.Fatal(err)
	}
	if len(right.Rows) != 3 || right.Rows[1][0] != nil || right.Rows[1][1] != "code-2" {
		t.Fatalf("unexpected RIGHT JOIN rows: %#v", right.Rows)
	}

	cross, err := engine.Execute(session, "SELECT COUNT(*) FROM auth_app a CROSS JOIN auth_code c")
	if err != nil {
		t.Fatal(err)
	}
	if len(cross.Rows) != 1 || cross.Rows[0][0] != int64(6) {
		t.Fatalf("unexpected CROSS JOIN count: %#v", cross.Rows)
	}
}

func TestDistinctCreateIfNotExistsDescribeAndTruncate(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{StreamResults: true}
	for _, query := range []string{
		"CREATE DATABASE mysql_syntax_test",
		"USE mysql_syntax_test",
		"CREATE TABLE IF NOT EXISTS users(id INT,city VARCHAR(32))",
		"INSERT IGNORE INTO users VALUES (1,'Shanghai'),(2,'Beijing'),(3,'Shanghai'),(4,NULL)",
		"CREATE TABLE IF NOT EXISTS users(id INT,wrong VARCHAR(10))",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}

	distinct, err := engine.Execute(session, "SELECT DISTINCT city FROM users ORDER BY city ASC LIMIT 2")
	if err != nil {
		t.Fatal(err)
	}
	if len(distinct.Rows) != 2 || distinct.Rows[0][0] != nil || distinct.Rows[1][0] != "Beijing" {
		t.Fatalf("unexpected DISTINCT rows: %#v", distinct.Rows)
	}

	describe, err := engine.Execute(session, "DESCRIBE users")
	if err != nil {
		t.Fatal(err)
	}
	if len(describe.Rows) != 2 || describe.Rows[1][0] != "city" {
		t.Fatalf("unexpected DESCRIBE rows: %#v", describe.Rows)
	}

	truncated, err := engine.Execute(session, "TRUNCATE TABLE users")
	if err != nil {
		t.Fatal(err)
	}
	if truncated.AffectedRows != 4 {
		t.Fatalf("TRUNCATE affected %d rows", truncated.AffectedRows)
	}
	count, err := engine.Execute(session, "SELECT COUNT(*) FROM users")
	if err != nil || count.Rows[0][0] != int64(0) {
		t.Fatalf("unexpected count after TRUNCATE: %#v, %v", count.Rows, err)
	}
}

func TestTopLevelUnionAllDistinctOrderAndLimit(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE union_test",
		"USE union_test",
		"CREATE TABLE current_users(id INT,name VARCHAR(20))",
		"CREATE TABLE archived_users(id INT,name VARCHAR(20))",
		"INSERT INTO current_users VALUES (1,'Alice'),(2,'Bob')",
		"INSERT INTO archived_users VALUES (2,'Bob'),(3,'Carol')",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	distinct, err := engine.Execute(session, "SELECT id,name FROM current_users UNION SELECT id,name FROM archived_users ORDER BY id DESC LIMIT 2")
	if err != nil || len(distinct.Rows) != 2 || distinct.Rows[0][0] != int64(3) || distinct.Rows[1][0] != int64(2) {
		t.Fatalf("UNION rows = %#v, %v", distinct, err)
	}
	all, err := engine.Execute(session, "SELECT id FROM current_users UNION ALL SELECT id FROM archived_users ORDER BY 1")
	if err != nil || len(all.Rows) != 4 || all.Rows[0][0] != int64(1) || all.Rows[1][0] != int64(2) || all.Rows[2][0] != int64(2) || all.Rows[3][0] != int64(3) {
		t.Fatalf("UNION ALL rows = %#v, %v", all, err)
	}
	if _, err := engine.Execute(session, "SELECT id FROM current_users UNION SELECT id,name FROM archived_users"); !errors.Is(err, storage.ErrColumnCount) {
		t.Fatalf("UNION column-count error = %v", err)
	}
}

func TestUnionQueryInputsAndMultipleCTEs(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE union_inputs_test",
		"USE union_inputs_test",
		"CREATE TABLE left_items(id INT,label VARCHAR(20))",
		"CREATE TABLE right_items(id INT,label VARCHAR(20))",
		"CREATE TABLE copied_items(id INT,label VARCHAR(20))",
		"INSERT INTO left_items VALUES (1,'left')",
		"INSERT INTO right_items VALUES (2,'right')",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}

	derived, err := engine.Execute(session, "SELECT u.id,u.label FROM (SELECT id,label FROM left_items UNION ALL SELECT id,label FROM right_items) AS u ORDER BY u.id")
	if err != nil || len(derived.Rows) != 2 || derived.Rows[1][1] != "right" {
		t.Fatalf("UNION derived table = %#v, %v", derived, err)
	}
	if _, err := engine.Execute(session, "CREATE VIEW all_items AS SELECT id,label FROM left_items UNION ALL SELECT id,label FROM right_items"); err != nil {
		t.Fatal(err)
	}
	view, err := engine.Execute(session, "SELECT id,label FROM all_items ORDER BY id")
	if err != nil || len(view.Rows) != 2 || view.Rows[0][0] != int64(1) {
		t.Fatalf("UNION view = %#v, %v", view, err)
	}
	insert, err := engine.Execute(session, "INSERT INTO copied_items SELECT id,label FROM left_items UNION ALL SELECT id,label FROM right_items")
	if err != nil || insert.AffectedRows != 2 {
		t.Fatalf("INSERT SELECT UNION = %#v, %v", insert, err)
	}
	explain, err := engine.Execute(session, "EXPLAIN SELECT id FROM left_items UNION ALL SELECT id FROM right_items")
	if err != nil || len(explain.Rows) != 2 || explain.Rows[1][1] != "UNION" {
		t.Fatalf("EXPLAIN UNION = %#v, %v", explain, err)
	}
	cte, err := engine.Execute(session, "WITH first_ids AS (SELECT id FROM left_items UNION ALL SELECT id FROM right_items), labeled AS (SELECT id FROM first_ids WHERE id >= 2) SELECT id FROM labeled")
	if err != nil || len(cte.Rows) != 1 || cte.Rows[0][0] != int64(2) {
		t.Fatalf("multiple CTEs = %#v, %v", cte, err)
	}
}

func TestCreateTableAsSelectAndLike(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE create_from_query_test",
		"USE create_from_query_test",
		"CREATE TABLE parents(id INT NOT NULL,PRIMARY KEY(id))",
		"CREATE TABLE source_items(id INT NOT NULL,label VARCHAR(20) DEFAULT 'new',parent_id INT,UNIQUE KEY uq_label(label),CONSTRAINT fk_source_parent FOREIGN KEY(parent_id) REFERENCES parents(id),CONSTRAINT ck_id CHECK(id > 0)) COMMENT='source'",
		"INSERT INTO parents VALUES (1)",
		"INSERT INTO source_items VALUES (1,'one',1),(2,'two',1)",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}

	created, err := engine.Execute(session, "CREATE TABLE selected_items AS SELECT id,label FROM source_items WHERE id=1 UNION ALL SELECT id,label FROM source_items WHERE id=2")
	if err != nil || created.AffectedRows != 2 {
		t.Fatalf("CTAS = %#v, %v", created, err)
	}
	rows, err := engine.Execute(session, "SELECT id,label FROM selected_items ORDER BY id")
	if err != nil || len(rows.Rows) != 2 || rows.Rows[1][1] != "two" {
		t.Fatalf("CTAS rows = %#v, %v", rows, err)
	}
	if _, err := engine.Execute(session, "CREATE TABLE nullable_result AS SELECT NULL AS optional_value"); err != nil {
		t.Fatal(err)
	}
	nullable, err := engine.Execute(session, "SELECT optional_value FROM nullable_result")
	if err != nil || len(nullable.Rows) != 1 || nullable.Rows[0][0] != nil {
		t.Fatalf("nullable CTAS = %#v, %v", nullable, err)
	}
	if _, err := engine.Execute(session, "CREATE TABLE broken_items AS SELECT missing FROM source_items"); !errors.Is(err, storage.ErrColumnNotFound) {
		t.Fatalf("failed CTAS error = %v", err)
	}
	if _, err := engine.Execute(session, "SHOW CREATE TABLE broken_items"); err == nil {
		t.Fatal("failed CTAS left a target table")
	}

	if _, err := engine.Execute(session, "CREATE TABLE copied_structure LIKE source_items"); err != nil {
		t.Fatal(err)
	}
	definition, err := engine.Execute(session, "SHOW CREATE TABLE copied_structure")
	if err != nil {
		t.Fatal(err)
	}
	ddl := definition.Rows[0][1].(string)
	if !strings.Contains(ddl, "UNIQUE KEY `uq_label`") || !strings.Contains(ddl, "CONSTRAINT `ck_id` CHECK") || !strings.Contains(ddl, "DEFAULT 'new'") {
		t.Fatalf("LIKE metadata = %s", ddl)
	}
	if strings.Contains(ddl, "FOREIGN KEY") {
		t.Fatalf("LIKE copied a foreign key: %s", ddl)
	}
	if _, err := engine.Execute(session, "CREATE TABLE IF NOT EXISTS copied_structure AS SELECT missing FROM source_items"); err != nil {
		t.Fatalf("IF NOT EXISTS evaluated ignored CTAS query: %v", err)
	}
}

func TestShowFullColumnsLikeAndWhere(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE show_columns_test",
		"USE show_columns_test",
		"CREATE TABLE users(id INT NOT NULL,weight_unit VARCHAR(8) DEFAULT 'kg' COMMENT 'weight display unit',weight_value DOUBLE)",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	like, err := engine.Execute(session, "SHOW FULL COLUMNS FROM users LIKE 'weight_unit'")
	if err != nil || len(like.Rows) != 1 || len(like.Columns) != 9 || like.Rows[0][0] != "weight_unit" || like.Rows[0][8] != "weight display unit" {
		t.Fatalf("SHOW FULL COLUMNS LIKE = %#v, %v", like, err)
	}
	where, err := engine.Execute(session, "SHOW COLUMNS FROM users WHERE Field='weight_value'")
	if err != nil || len(where.Rows) != 1 || where.Rows[0][0] != "weight_value" {
		t.Fatalf("SHOW COLUMNS WHERE = %#v, %v", where, err)
	}
}

func TestReplaceValuesSetSelectAndStatementAtomicity(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE replace_test",
		"USE replace_test",
		"CREATE TABLE accounts(id INT NOT NULL,email VARCHAR(40) NOT NULL,label VARCHAR(20),PRIMARY KEY(id),UNIQUE KEY uq_email(email))",
		"CREATE TABLE account_source(id INT,email VARCHAR(40),label VARCHAR(20))",
		"INSERT INTO accounts VALUES (1,'a@example.com','one'),(2,'b@example.com','two')",
		"INSERT INTO account_source VALUES (4,'d@example.com','four')",
		"CREATE TABLE parents(id INT NOT NULL,PRIMARY KEY(id))",
		"CREATE TABLE children(id INT NOT NULL,parent_id INT NOT NULL,label VARCHAR(20),PRIMARY KEY(id),CONSTRAINT fk_child_parent FOREIGN KEY(parent_id) REFERENCES parents(id))",
		"INSERT INTO parents VALUES (1)",
		"INSERT INTO children VALUES (10,1,'original')",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}

	values, err := engine.Execute(session, "REPLACE INTO accounts VALUES (1,'b@example.com','merged')")
	if err != nil || values.AffectedRows != 3 {
		t.Fatalf("REPLACE VALUES = %#v, %v", values, err)
	}
	if _, err := engine.Execute(session, "REPLACE accounts SET id=3,email='c@example.com',label='three'"); err != nil {
		t.Fatal(err)
	}
	selected, err := engine.Execute(session, "REPLACE INTO accounts(id,email,label) SELECT id,email,label FROM account_source")
	if err != nil || selected.AffectedRows != 1 {
		t.Fatalf("REPLACE SELECT = %#v, %v", selected, err)
	}
	rows, err := engine.Execute(session, "SELECT id,email,label FROM accounts ORDER BY id")
	if err != nil || len(rows.Rows) != 3 || rows.Rows[0][2] != "merged" || rows.Rows[2][0] != int64(4) {
		t.Fatalf("REPLACE rows = %#v, %v", rows, err)
	}

	if _, err := engine.Execute(session, "REPLACE INTO children VALUES (10,1,'changed'),(11,999,'invalid')"); !errors.Is(err, storage.ErrForeignKey) {
		t.Fatalf("REPLACE atomic failure = %v", err)
	}
	child, err := engine.Execute(session, "SELECT id,parent_id,label FROM children")
	if err != nil || len(child.Rows) != 1 || child.Rows[0][2] != "original" {
		t.Fatalf("failed REPLACE changed rows = %#v, %v", child, err)
	}
}

func TestUpdateJoinAndMultiTableDelete(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE joined_mutation_test",
		"USE joined_mutation_test",
		"CREATE TABLE users(id INT NOT NULL,balance INT NOT NULL,label VARCHAR(20),email VARCHAR(40),PRIMARY KEY(id),UNIQUE KEY uq_email(email))",
		"CREATE TABLE adjustments(id INT,user_id INT,delta INT)",
		"INSERT INTO users VALUES (1,10,'start','one@example.com'),(2,20,'second','two@example.com')",
		"INSERT INTO adjustments VALUES (1,1,3),(2,1,7),(3,2,5)",
		"CREATE TABLE parents(id INT NOT NULL,PRIMARY KEY(id))",
		"CREATE TABLE children(id INT NOT NULL,parent_id INT NOT NULL,PRIMARY KEY(id),CONSTRAINT fk_child_parent FOREIGN KEY(parent_id) REFERENCES parents(id))",
		"INSERT INTO parents VALUES (1),(2)",
		"INSERT INTO children VALUES (10,1),(11,1),(20,2)",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}

	updated, err := engine.Execute(session, "UPDATE users u JOIN adjustments a ON a.user_id=u.id SET u.balance=u.balance+a.delta,u.label=CONCAT('balance-',u.balance) WHERE u.id=1")
	if err != nil || updated.AffectedRows != 1 {
		t.Fatalf("UPDATE JOIN = %#v, %v", updated, err)
	}
	user, err := engine.Execute(session, "SELECT balance,label FROM users WHERE id=1")
	if err != nil || user.Rows[0][0] != int64(13) || user.Rows[0][1] != "balance-13" {
		t.Fatalf("UPDATE JOIN row = %#v, %v", user, err)
	}
	if _, err := engine.Execute(session, "UPDATE users u JOIN adjustments a ON a.user_id=u.id SET a.delta=0 WHERE u.id=1"); err == nil {
		t.Fatal("UPDATE JOIN changed a non-target table")
	}
	if _, err := engine.Execute(session, "UPDATE users u JOIN adjustments a ON a.user_id=u.id SET u.email='same@example.com'"); !errors.Is(err, storage.ErrDuplicateKey) {
		t.Fatalf("atomic UPDATE JOIN error = %v", err)
	}
	emails, err := engine.Execute(session, "SELECT email FROM users ORDER BY id")
	if err != nil || emails.Rows[0][0] != "one@example.com" || emails.Rows[1][0] != "two@example.com" {
		t.Fatalf("failed UPDATE JOIN changed rows = %#v, %v", emails, err)
	}

	deleted, err := engine.Execute(session, "DELETE p,c FROM parents p JOIN children c ON c.parent_id=p.id WHERE p.id=1")
	if err != nil || deleted.AffectedRows != 3 {
		t.Fatalf("multi DELETE targets FROM = %#v, %v", deleted, err)
	}
	deleted, err = engine.Execute(session, "DELETE FROM c,p USING parents p JOIN children c ON c.parent_id=p.id WHERE p.id=2")
	if err != nil || deleted.AffectedRows != 2 {
		t.Fatalf("multi DELETE FROM USING = %#v, %v", deleted, err)
	}
	parentCount, err := engine.Execute(session, "SELECT COUNT(*) FROM parents")
	if err != nil || parentCount.Rows[0][0] != int64(0) {
		t.Fatalf("multi DELETE parent count = %#v, %v", parentCount, err)
	}
	childCount, err := engine.Execute(session, "SELECT COUNT(*) FROM children")
	if err != nil || childCount.Rows[0][0] != int64(0) {
		t.Fatalf("multi DELETE child count = %#v, %v", childCount, err)
	}
}

func TestForeignKeyCascadeAndSetNullActions(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE cascade_test",
		"USE cascade_test",
		"CREATE TABLE parents(id INT NOT NULL,label VARCHAR(20),PRIMARY KEY(id))",
		"CREATE TABLE children(id INT NOT NULL,parent_id INT,label VARCHAR(20),PRIMARY KEY(id),CONSTRAINT fk_child_parent FOREIGN KEY(parent_id) REFERENCES parents(id) ON DELETE CASCADE ON UPDATE CASCADE)",
		"CREATE TABLE grandchildren(id INT NOT NULL,child_id INT,PRIMARY KEY(id),CONSTRAINT fk_grandchild_child FOREIGN KEY(child_id) REFERENCES children(id) ON DELETE CASCADE ON UPDATE CASCADE)",
		"CREATE TABLE nullable_delete(id INT NOT NULL,parent_id INT,PRIMARY KEY(id),CONSTRAINT fk_nullable_delete FOREIGN KEY(parent_id) REFERENCES parents(id) ON DELETE SET NULL)",
		"CREATE TABLE nullable_update(id INT NOT NULL,parent_id INT,PRIMARY KEY(id),CONSTRAINT fk_nullable_update FOREIGN KEY(parent_id) REFERENCES parents(id) ON UPDATE SET NULL)",
		"INSERT INTO parents VALUES (1,'one'),(2,'two'),(3,'three')",
		"INSERT INTO children VALUES (10,1,'child-one'),(20,2,'child-two')",
		"INSERT INTO grandchildren VALUES (100,10),(200,20)",
		"INSERT INTO nullable_delete VALUES (1,1)",
		"INSERT INTO nullable_update VALUES (1,2)",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}

	if _, err := engine.Execute(session, "UPDATE parents SET id=22 WHERE id=2"); err != nil {
		t.Fatal(err)
	}
	child, err := engine.Execute(session, "SELECT parent_id FROM children WHERE id=20")
	if err != nil || child.Rows[0][0] != int64(22) {
		t.Fatalf("ON UPDATE CASCADE = %#v, %v", child, err)
	}
	nullableUpdate, err := engine.Execute(session, "SELECT parent_id FROM nullable_update WHERE id=1")
	if err != nil || nullableUpdate.Rows[0][0] != nil {
		t.Fatalf("ON UPDATE SET NULL = %#v, %v", nullableUpdate, err)
	}

	if _, err := engine.Execute(session, "DELETE FROM parents WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	childCount, _ := engine.Execute(session, "SELECT COUNT(*) FROM children WHERE id=10")
	grandchildCount, _ := engine.Execute(session, "SELECT COUNT(*) FROM grandchildren WHERE id=100")
	setNull, _ := engine.Execute(session, "SELECT parent_id FROM nullable_delete WHERE id=1")
	if childCount.Rows[0][0] != int64(0) || grandchildCount.Rows[0][0] != int64(0) || setNull.Rows[0][0] != nil {
		t.Fatalf("ON DELETE actions: child=%#v grandchild=%#v set-null=%#v", childCount.Rows, grandchildCount.Rows, setNull.Rows)
	}

	replaced, err := engine.Execute(session, "REPLACE INTO parents VALUES (22,'replacement')")
	if err != nil || replaced.AffectedRows != 2 {
		t.Fatalf("REPLACE parent = %#v, %v", replaced, err)
	}
	childCount, _ = engine.Execute(session, "SELECT COUNT(*) FROM children WHERE id=20")
	grandchildCount, _ = engine.Execute(session, "SELECT COUNT(*) FROM grandchildren WHERE id=200")
	if childCount.Rows[0][0] != int64(0) || grandchildCount.Rows[0][0] != int64(0) {
		t.Fatalf("REPLACE did not cascade deletes: child=%#v grandchild=%#v", childCount.Rows, grandchildCount.Rows)
	}

	if _, err := engine.Execute(session, "CREATE TABLE invalid_set_null(id INT NOT NULL,parent_id INT NOT NULL,PRIMARY KEY(id),CONSTRAINT fk_invalid_set_null FOREIGN KEY(parent_id) REFERENCES parents(id) ON DELETE SET NULL)"); !errors.Is(err, storage.ErrForeignKey) {
		t.Fatalf("non-nullable SET NULL definition error = %v", err)
	}
}

func TestCascadingMutationCyclesAndCheckFailureAreAtomic(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE cascade_atomic_test",
		"USE cascade_atomic_test",
		"CREATE TABLE nodes(id INT NOT NULL,parent_id INT,PRIMARY KEY(id),CONSTRAINT fk_node_parent FOREIGN KEY(parent_id) REFERENCES nodes(id) ON DELETE CASCADE ON UPDATE CASCADE)",
		"INSERT INTO nodes VALUES (1,NULL),(2,1)",
		"UPDATE nodes SET parent_id=2 WHERE id=1",
		"CREATE TABLE guarded_parents(id INT NOT NULL,PRIMARY KEY(id))",
		"CREATE TABLE guarded_children(id INT NOT NULL,parent_id INT,PRIMARY KEY(id),CONSTRAINT ck_parent_present CHECK(parent_id IS NOT NULL),CONSTRAINT fk_guarded_parent FOREIGN KEY(parent_id) REFERENCES guarded_parents(id) ON DELETE SET NULL)",
		"INSERT INTO guarded_parents VALUES (1)",
		"INSERT INTO guarded_children VALUES (1,1)",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	if _, err := engine.Execute(session, "DELETE FROM nodes WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	count, _ := engine.Execute(session, "SELECT COUNT(*) FROM nodes")
	if count.Rows[0][0] != int64(0) {
		t.Fatalf("cyclic cascade rows = %#v", count.Rows)
	}
	if _, err := engine.Execute(session, "DELETE FROM guarded_parents WHERE id=1"); !errors.Is(err, storage.ErrCheckConstraint) {
		t.Fatalf("SET NULL CHECK error = %v", err)
	}
	parent, _ := engine.Execute(session, "SELECT COUNT(*) FROM guarded_parents")
	child, _ := engine.Execute(session, "SELECT parent_id FROM guarded_children WHERE id=1")
	if parent.Rows[0][0] != int64(1) || child.Rows[0][0] != int64(1) {
		t.Fatalf("failed cascading statement was not atomic: parent=%#v child=%#v", parent.Rows, child.Rows)
	}
}

func TestInsertIgnoreSkipsDuplicateKeys(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE insert_ignore_test",
		"USE insert_ignore_test",
		"CREATE TABLE items(id INT NOT NULL,label VARCHAR(32),PRIMARY KEY(id))",
		"INSERT INTO items VALUES (1,'existing')",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	result, err := engine.Execute(session, "INSERT IGNORE INTO items VALUES (1,'duplicate'),(2,'new')")
	if err != nil {
		t.Fatal(err)
	}
	if result.AffectedRows != 1 {
		t.Fatalf("affected rows = %d", result.AffectedRows)
	}
	rows, err := engine.Execute(session, "SELECT id,label FROM items ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Rows) != 2 || rows.Rows[0][1] != "existing" || rows.Rows[1][1] != "new" {
		t.Fatalf("unexpected rows: %#v", rows.Rows)
	}
}

func TestCommonWherePredicates(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE filter_test",
		"USE filter_test",
		"CREATE TABLE users(id INT, name VARCHAR(20), age INT, note TEXT, enabled BOOLEAN)",
		"INSERT INTO users VALUES (1, 'Alice', 20, NULL, TRUE), (2, 'Bob', 30, 'x', FALSE), (3, 'Tester', 70, NULL, TRUE), (4, 'Carol', 40, NULL, TRUE)",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		query string
		ids   []int64
	}{
		{"SELECT id FROM users WHERE id IN (1, 3, 9) ORDER BY id", []int64{1, 3}},
		{"SELECT id FROM users WHERE id NOT IN (1, 2, 3) ORDER BY id", []int64{4}},
		{"SELECT id FROM users WHERE age BETWEEN 20 AND 40 ORDER BY id", []int64{1, 2, 4}},
		{"SELECT id FROM users WHERE age NOT BETWEEN 20 AND 40 ORDER BY id", []int64{3}},
		{"SELECT id FROM users WHERE note IS NULL ORDER BY id", []int64{1, 3, 4}},
		{"SELECT id FROM users WHERE note IS NOT NULL ORDER BY id", []int64{2}},
		{"SELECT id FROM users WHERE NOT (name LIKE '%test%') AND enabled IS TRUE ORDER BY id", []int64{1, 4}},
		{"SELECT id FROM users WHERE NOT name LIKE '%test%' AND enabled IS TRUE ORDER BY id", []int64{1, 4}},
		{"SELECT id FROM users WHERE note = NULL", nil},
		{"SELECT id FROM users WHERE NOT (note = NULL)", nil},
		{"SELECT id FROM users WHERE note = NULL OR id = 2 ORDER BY id", []int64{2}},
		{"SELECT id FROM users WHERE note = NULL AND id = 2", nil},
		{"SELECT id FROM users WHERE id IN (1, NULL) ORDER BY id", []int64{1}},
		{"SELECT id FROM users WHERE id NOT IN (1, NULL)", nil},
	}
	for _, test := range tests {
		result, err := engine.Execute(session, test.query)
		if err != nil {
			t.Fatalf("%s: %v", test.query, err)
		}
		if len(result.Rows) != len(test.ids) {
			t.Fatalf("%s returned %#v, want %v", test.query, result.Rows, test.ids)
		}
		for index, id := range test.ids {
			if result.Rows[index][0] != id {
				t.Fatalf("%s returned %#v, want %v", test.query, result.Rows, test.ids)
			}
		}
	}
}

func TestInSubqueryForSelectUpdateAndDelete(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE in_subquery_test",
		"USE in_subquery_test",
		"CREATE TABLE workout_exercises(id BIGINT,workout_id BIGINT,PRIMARY KEY(id))",
		"CREATE TABLE workout_sets(id BIGINT,workout_exercise_id BIGINT,note VARCHAR(20),PRIMARY KEY(id))",
		"INSERT INTO workout_exercises VALUES (1,10),(2,10),(3,20)",
		"INSERT INTO workout_sets VALUES (101,1,'keep'),(102,2,'keep'),(103,3,'keep'),(104,NULL,'keep')",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}

	selected, err := engine.Execute(session, "SELECT id FROM workout_sets WHERE workout_exercise_id IN (SELECT id FROM workout_exercises WHERE workout_id=10) ORDER BY id")
	if err != nil || len(selected.Rows) != 2 || selected.Rows[0][0] != int64(101) || selected.Rows[1][0] != int64(102) {
		t.Fatalf("IN subquery SELECT = %#v, %v", selected, err)
	}
	multiColumn, err := engine.Execute(session, "SELECT id FROM workout_sets WHERE (id,workout_exercise_id) IN (SELECT id+100,id FROM workout_exercises WHERE workout_id=10) ORDER BY id")
	if err != nil || len(multiColumn.Rows) != 2 || multiColumn.Rows[0][0] != int64(101) || multiColumn.Rows[1][0] != int64(102) {
		t.Fatalf("multi-column IN subquery = %#v, %v", multiColumn, err)
	}
	literalRows, err := engine.Execute(session, "SELECT id FROM workout_sets WHERE (id,workout_exercise_id) IN ((101,1),(999,9))")
	if err != nil || len(literalRows.Rows) != 1 || literalRows.Rows[0][0] != int64(101) {
		t.Fatalf("row-constructor IN list = %#v, %v", literalRows, err)
	}
	updated, err := engine.Execute(session, "UPDATE workout_sets SET note='selected' WHERE workout_exercise_id IN (SELECT id FROM workout_exercises WHERE workout_id=10)")
	if err != nil || updated.AffectedRows != 2 {
		t.Fatalf("IN subquery UPDATE = %#v, %v", updated, err)
	}
	deleted, err := engine.Execute(session, "DELETE FROM workout_sets WHERE workout_exercise_id IN (SELECT id FROM workout_exercises WHERE workout_id=10)")
	if err != nil || deleted.AffectedRows != 2 {
		t.Fatalf("IN subquery DELETE = %#v, %v", deleted, err)
	}
	remaining, err := engine.Execute(session, "SELECT id FROM workout_sets ORDER BY id")
	if err != nil || len(remaining.Rows) != 2 || remaining.Rows[0][0] != int64(103) || remaining.Rows[1][0] != int64(104) {
		t.Fatalf("remaining rows = %#v, %v", remaining, err)
	}
	notInWithNull, err := engine.Execute(session, "SELECT id FROM workout_exercises WHERE id NOT IN (SELECT workout_exercise_id FROM workout_sets)")
	if err != nil || len(notInWithNull.Rows) != 0 {
		t.Fatalf("NOT IN subquery with NULL = %#v, %v", notInWithNull, err)
	}
	if _, err := engine.Execute(session, "SELECT id FROM workout_sets WHERE id IN (SELECT id,workout_id FROM workout_exercises)"); err == nil || !strings.Contains(err.Error(), "column count mismatch") {
		t.Fatalf("IN row arity error = %v", err)
	}
}

func TestExistsAndNestedScalarSubqueries(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE common_subquery_test",
		"USE common_subquery_test",
		"CREATE TABLE users(id INT,balance INT,snapshot INT,PRIMARY KEY(id))",
		"CREATE TABLE sessions(id INT,user_id INT,revoked_at DATETIME)",
		"INSERT INTO users VALUES (1,10,0),(2,20,0)",
		"INSERT INTO sessions VALUES (100,2,NULL)",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	rows, err := engine.Execute(session, "SELECT id FROM users WHERE EXISTS (SELECT id FROM sessions WHERE revoked_at IS NULL) AND id=(SELECT user_id FROM sessions WHERE id=100)")
	if err != nil || len(rows.Rows) != 1 || rows.Rows[0][0] != int64(2) {
		t.Fatalf("EXISTS/scalar rows = %#v, %v", rows, err)
	}
	empty, err := engine.Execute(session, "SELECT id FROM users WHERE NOT EXISTS (SELECT id FROM sessions WHERE id=999)")
	if err != nil || len(empty.Rows) != 2 {
		t.Fatalf("NOT EXISTS rows = %#v, %v", empty, err)
	}
	if _, err := engine.Execute(session, "UPDATE users SET snapshot=(SELECT MAX(balance) FROM users)"); err != nil {
		t.Fatal(err)
	}
	rows, err = engine.Execute(session, "SELECT snapshot FROM users ORDER BY id")
	if err != nil || len(rows.Rows) != 2 || rows.Rows[0][0] != int64(20) || rows.Rows[1][0] != int64(20) {
		t.Fatalf("scalar assignment rows = %#v, %v", rows, err)
	}
	if _, err := engine.Execute(session, "SELECT id FROM users WHERE id=(SELECT user_id FROM sessions)"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Execute(session, "INSERT INTO sessions VALUES (101,1,NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Execute(session, "SELECT id FROM users WHERE id=(SELECT user_id FROM sessions)"); err == nil || !strings.Contains(err.Error(), "more than one row") {
		t.Fatalf("multi-row scalar subquery error = %v", err)
	}
	correlated, err := engine.Execute(session, "SELECT u.id FROM users u WHERE EXISTS (SELECT s.id FROM sessions s WHERE s.user_id=u.id AND s.revoked_at IS NULL) ORDER BY u.id")
	if err != nil || len(correlated.Rows) != 2 || correlated.Rows[0][0] != int64(1) || correlated.Rows[1][0] != int64(2) {
		t.Fatalf("correlated EXISTS rows = %#v, %v", correlated, err)
	}
	correlated, err = engine.Execute(session, "SELECT u.id FROM users u WHERE u.id IN (SELECT s.user_id FROM sessions s WHERE s.user_id=u.id) ORDER BY u.id")
	if err != nil || len(correlated.Rows) != 2 {
		t.Fatalf("correlated IN rows = %#v, %v", correlated, err)
	}
	if _, err := engine.Execute(session, "SELECT u.id FROM users u WHERE EXISTS (SELECT s.id FROM sessions s WHERE s.missing=u.id)"); err == nil || !strings.Contains(err.Error(), "unknown column") {
		t.Fatalf("correlated subquery evaluation error = %v", err)
	}
}

func TestCorrelatedSubqueriesForUpdateAndDelete(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE correlated_write_test",
		"USE correlated_write_test",
		"CREATE TABLE users(id INT NOT NULL,score INT NOT NULL,marker INT NOT NULL,PRIMARY KEY(id))",
		"CREATE TABLE sessions(id INT NOT NULL,user_id INT NOT NULL,score INT NOT NULL,active BOOLEAN NOT NULL,revoked_at DATETIME,PRIMARY KEY(id))",
		"CREATE TABLE adjustments(id INT NOT NULL,user_id INT NOT NULL,delta INT NOT NULL,PRIMARY KEY(id))",
		"INSERT INTO users VALUES (1,10,0),(2,20,0),(3,30,0),(4,40,0),(5,50,0)",
		"INSERT INTO sessions VALUES (10,1,31,TRUE,NULL),(20,2,41,FALSE,'2026-08-01 10:00:00'),(21,2,51,TRUE,'2026-08-02 10:00:00'),(40,4,61,TRUE,NULL),(50,5,71,TRUE,NULL),(51,5,81,TRUE,NULL)",
		"INSERT INTO adjustments VALUES (1,1,101),(2,2,102),(3,3,103)",
		"CREATE TABLE delete_groups(id INT NOT NULL,PRIMARY KEY(id))",
		"CREATE TABLE delete_members(id INT NOT NULL,group_id INT NOT NULL,PRIMARY KEY(id))",
		"CREATE TABLE delete_flags(group_id INT NOT NULL,member_id INT NOT NULL)",
		"INSERT INTO delete_groups VALUES (1),(2)",
		"INSERT INTO delete_members VALUES (10,1),(11,1),(20,2)",
		"INSERT INTO delete_flags VALUES (1,10)",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}

	updated, err := engine.Execute(session, `UPDATE users AS u
		SET score=(SELECT s.score+u.marker FROM sessions AS s WHERE s.user_id=u.id AND s.active=TRUE LIMIT 1),marker=score+1
		WHERE EXISTS (SELECT 1 FROM sessions AS s WHERE s.user_id=u.id AND s.active=TRUE) AND u.id<=2`)
	if err != nil || updated.AffectedRows != 2 {
		t.Fatalf("correlated UPDATE = %#v, %v", updated, err)
	}
	rows, err := engine.Execute(session, "SELECT id,score,marker FROM users WHERE id<=2 ORDER BY id")
	if err != nil || len(rows.Rows) != 2 || rows.Rows[0][1] != int64(31) || rows.Rows[0][2] != int64(32) || rows.Rows[1][1] != int64(51) || rows.Rows[1][2] != int64(52) {
		t.Fatalf("correlated UPDATE rows = %#v, %v", rows, err)
	}

	updated, err = engine.Execute(session, `UPDATE users AS u SET u.marker=marker+1
		WHERE EXISTS (SELECT 1 FROM sessions AS s WHERE s.user_id=u.id AND marker>=0) AND u.id<=2`)
	if err != nil || updated.AffectedRows != 2 {
		t.Fatalf("unqualified correlated UPDATE = %#v, %v", updated, err)
	}

	joinedRows, err := engine.Execute(session, `SELECT u.id FROM users AS u JOIN adjustments AS a
		ON a.user_id=u.id AND EXISTS (SELECT 1 FROM sessions AS s WHERE s.user_id=u.id AND s.active=TRUE) ORDER BY u.id`)
	if err != nil || len(joinedRows.Rows) != 2 || joinedRows.Rows[0][0] != int64(1) || joinedRows.Rows[1][0] != int64(2) {
		t.Fatalf("correlated SELECT JOIN = %#v, %v", joinedRows, err)
	}

	joined, err := engine.Execute(session, `UPDATE users AS u JOIN adjustments AS a
		ON a.user_id=u.id AND EXISTS (SELECT 1 FROM sessions AS s WHERE s.user_id=u.id AND s.active=TRUE)
		SET u.marker=a.delta`)
	if err != nil || joined.AffectedRows != 2 {
		t.Fatalf("correlated UPDATE JOIN = %#v, %v", joined, err)
	}
	updated, err = engine.Execute(session, "UPDATE users AS u SET u.marker=(SELECT u.score+1) WHERE u.id=3")
	if err != nil || updated.AffectedRows != 1 {
		t.Fatalf("correlated scalar SELECT without FROM = %#v, %v", updated, err)
	}

	if _, err := engine.Execute(session, `UPDATE users AS u SET marker=(SELECT s.score FROM sessions AS s WHERE s.user_id=u.id)
		WHERE u.id IN (4,5)`); err == nil || !strings.Contains(err.Error(), "more than one row") {
		t.Fatalf("correlated scalar atomic error = %v", err)
	}
	rows, err = engine.Execute(session, "SELECT id,marker FROM users WHERE id IN (4,5) ORDER BY id")
	if err != nil || len(rows.Rows) != 2 || rows.Rows[0][1] != int64(0) || rows.Rows[1][1] != int64(0) {
		t.Fatalf("failed correlated UPDATE changed rows = %#v, %v", rows, err)
	}

	deleted, err := engine.Execute(session, `DELETE FROM users AS u WHERE u.id IN (
		SELECT s.user_id FROM sessions AS s WHERE s.user_id=u.id AND s.revoked_at IS NOT NULL
	)`)
	if err != nil || deleted.AffectedRows != 1 {
		t.Fatalf("correlated DELETE = %#v, %v", deleted, err)
	}
	rows, err = engine.Execute(session, "SELECT id FROM users ORDER BY id")
	if err != nil || len(rows.Rows) != 4 || rows.Rows[0][0] != int64(1) || rows.Rows[1][0] != int64(3) {
		t.Fatalf("correlated DELETE rows = %#v, %v", rows, err)
	}

	deleted, err = engine.Execute(session, `DELETE g,m FROM delete_groups AS g JOIN delete_members AS m
		ON m.group_id=g.id AND EXISTS (SELECT 1 FROM delete_flags AS f WHERE f.group_id=g.id AND f.member_id=m.id)`)
	if err != nil || deleted.AffectedRows != 2 {
		t.Fatalf("correlated multi-table DELETE = %#v, %v", deleted, err)
	}
	groups, err := engine.Execute(session, "SELECT id FROM delete_groups ORDER BY id")
	if err != nil || len(groups.Rows) != 1 || groups.Rows[0][0] != int64(2) {
		t.Fatalf("correlated multi-table DELETE groups = %#v, %v", groups, err)
	}
	members, err := engine.Execute(session, "SELECT id FROM delete_members ORDER BY id")
	if err != nil || len(members.Rows) != 2 || members.Rows[0][0] != int64(11) || members.Rows[1][0] != int64(20) {
		t.Fatalf("correlated multi-table DELETE members = %#v, %v", members, err)
	}

	if _, err := engine.Execute(session, "UPDATE users AS u SET marker=1 WHERE EXISTS (SELECT 1 FROM sessions AS s WHERE s.missing=u.id)"); err == nil || !strings.Contains(err.Error(), "unknown column") {
		t.Fatalf("invalid correlated UPDATE error = %v", err)
	}
}

func TestFitnessStyleSchemaDDL(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	session := &Session{}
	queries := []string{
		"CREATE DATABASE IF NOT EXISTS fitness_ddl CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci",
		"USE fitness_ddl",
		"CREATE TABLE users(id BIGINT PRIMARY KEY AUTO_INCREMENT,phone VARCHAR(30) NOT NULL,UNIQUE KEY uq_users_phone(phone)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='users'",
		`CREATE TABLE workouts(
			id BIGINT PRIMARY KEY AUTO_INCREMENT,
			user_id BIGINT NOT NULL,
			weekdays JSON NOT NULL,
			enabled BOOLEAN NOT NULL DEFAULT TRUE,
			started_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			CONSTRAINT fk_workouts_user FOREIGN KEY(user_id) REFERENCES users(id),
			INDEX idx_workouts_user_started(user_id,started_at DESC),
			CHECK(enabled IN (TRUE,FALSE))
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='workouts'`,
	}
	for _, query := range queries {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	indexes, err := engine.Execute(session, "SHOW INDEX FROM workouts")
	if err != nil || len(indexes.Rows) < 2 {
		t.Fatalf("Fitness-style index metadata = %#v, %v", indexes, err)
	}
}

func TestCommonMySQLMigrationDDLAndInsertSet(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE migration_compat",
		"USE migration_compat",
		"CREATE TABLE profiles(id BIGINT PRIMARY KEY AUTO_INCREMENT,phone VARCHAR(32) NOT NULL,name VARCHAR(64) NOT NULL,score INT NOT NULL DEFAULT 0,updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,UNIQUE KEY uq_profiles_phone(phone),CHECK(score BETWEEN 0 AND 10))",
		"INSERT INTO profiles SET phone='100',name='Alice',score=2+3,updated_at=NOW()",
		"ALTER TABLE profiles ADD CONSTRAINT uq_profiles_name UNIQUE (name), ADD INDEX (score), ALTER COLUMN score SET DEFAULT 7, RENAME INDEX score TO idx_profiles_score",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	first, err := engine.Execute(session, "SELECT id,score,updated_at FROM profiles WHERE phone='100'")
	if err != nil || len(first.Rows) != 1 || first.Rows[0][0] != int64(1) || first.Rows[0][1] != int64(5) || first.Rows[0][2] == nil {
		t.Fatalf("INSERT SET row = %#v, %v", first, err)
	}
	if _, err := engine.Execute(session, "INSERT profiles SET phone='200',name='Bob'"); err != nil {
		t.Fatal(err)
	}
	second, err := engine.Execute(session, "SELECT score FROM profiles WHERE phone='200'")
	if err != nil || len(second.Rows) != 1 || second.Rows[0][0] != int64(7) {
		t.Fatalf("SET DEFAULT insert = %#v, %v", second, err)
	}
	if _, err := engine.Execute(session, "INSERT profiles SET phone='200',name='Bob',score=9 ON DUPLICATE KEY UPDATE score=score+1"); err != nil {
		t.Fatal(err)
	}
	second, err = engine.Execute(session, "SELECT score FROM profiles WHERE phone='200'")
	if err != nil || second.Rows[0][0] != int64(8) {
		t.Fatalf("INSERT SET duplicate update = %#v, %v", second, err)
	}
	if _, err := engine.Execute(session, "INSERT profiles SET phone='200',name='Bob',score=9 ON DUPLICATE KEY UPDATE score=score+10"); err == nil || !errors.Is(err, storage.ErrCheckConstraint) {
		t.Fatalf("ON DUPLICATE CHECK error = %v", err)
	}
	second, err = engine.Execute(session, "SELECT score FROM profiles WHERE phone='200'")
	if err != nil || second.Rows[0][0] != int64(8) {
		t.Fatalf("failed duplicate update changed row = %#v, %v", second, err)
	}
	indexes, err := engine.Execute(session, "SHOW INDEX FROM profiles")
	if err != nil {
		t.Fatal(err)
	}
	foundRenamed, foundUnique := false, false
	for _, row := range indexes.Rows {
		foundRenamed = foundRenamed || row[2] == "idx_profiles_score"
		foundUnique = foundUnique || row[2] == "uq_profiles_name"
	}
	if !foundRenamed || !foundUnique {
		t.Fatalf("renamed/unique indexes missing: %#v", indexes.Rows)
	}
	if _, err := engine.Execute(session, "INSERT profiles SET phone='300',name='Alice'"); err == nil || !errors.Is(err, storage.ErrDuplicateKey) {
		t.Fatalf("UNIQUE constraint error = %v", err)
	}
	if _, err := engine.Execute(session, "ALTER TABLE profiles ALTER COLUMN score DROP DEFAULT"); err != nil {
		t.Fatal(err)
	}
	columns, err := engine.Execute(session, "SHOW COLUMNS FROM profiles")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range columns.Rows {
		if row[0] == "score" && row[4] != nil {
			t.Fatalf("score default was not dropped: %#v", row)
		}
	}
}

func TestSelectStreamsUnorderedResults(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{StreamResults: true}
	for _, query := range []string{
		"CREATE DATABASE stream_test",
		"USE stream_test",
		"CREATE TABLE users(id INT, name VARCHAR(20))",
		"INSERT INTO users VALUES (1, 'Alice'), (2, 'Bob'), (3, 'Carol')",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatal(err)
		}
	}
	result, err := engine.Execute(session, "SELECT name FROM users WHERE id >= 2 LIMIT 2")
	if err != nil {
		t.Fatal(err)
	}
	if result.StreamRows == nil || len(result.Rows) != 0 {
		t.Fatalf("expected streaming result, got %#v", result)
	}
	var rows [][]any
	if err := result.StreamRows(func(row []any) error {
		rows = append(rows, row)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0][0] != "Bob" || rows[1][0] != "Carol" {
		t.Fatalf("unexpected streamed rows: %#v", rows)
	}
}
func TestTransactionRollback(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{}
	for _, query := range []string{"CREATE DATABASE test", "USE test", "CREATE TABLE t(id INT)", "BEGIN", "INSERT INTO t VALUES (1)", "ROLLBACK"} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	result, err := engine.Execute(session, "SELECT * FROM t")
	if err != nil || len(result.Rows) != 0 {
		t.Fatalf("rollback result=%#v err=%v", result, err)
	}
}

func TestExplicitTransactionProvidesExclusiveIsolationAndDisconnectRollback(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	first := &Session{CurrentDatabase: "isolation_test"}
	second := &Session{CurrentDatabase: "isolation_test"}
	for _, query := range []string{
		"CREATE DATABASE isolation_test",
		"CREATE TABLE isolation_test.items(id INT NOT NULL,value VARCHAR(16),PRIMARY KEY(id))",
		"INSERT INTO isolation_test.items VALUES(1,'before')",
		"BEGIN",
		"UPDATE items SET value='committed' WHERE id=1",
	} {
		if _, err := engine.Execute(first, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	inside, err := engine.Execute(first, "SELECT value FROM items WHERE id=1")
	if err != nil || len(inside.Rows) != 1 || inside.Rows[0][0] != "committed" {
		t.Fatalf("transaction read = %#v, %v", inside, err)
	}

	type queryResult struct {
		result *Result
		err    error
	}
	blockedRead := make(chan queryResult, 1)
	go func() {
		result, err := engine.Execute(second, "SELECT value FROM items WHERE id=1")
		blockedRead <- queryResult{result: result, err: err}
	}()
	select {
	case outcome := <-blockedRead:
		t.Fatalf("other session read did not wait for commit: %#v, %v", outcome.result, outcome.err)
	case <-time.After(30 * time.Millisecond):
	}
	if _, err := engine.Execute(first, "COMMIT"); err != nil {
		t.Fatal(err)
	}
	select {
	case outcome := <-blockedRead:
		if outcome.err != nil || len(outcome.result.Rows) != 1 || outcome.result.Rows[0][0] != "committed" {
			t.Fatalf("read after commit = %#v, %v", outcome.result, outcome.err)
		}
	case <-time.After(time.Second):
		t.Fatal("other session remained blocked after commit")
	}

	for _, query := range []string{"BEGIN", "UPDATE items SET value='discarded' WHERE id=1"} {
		if _, err := engine.Execute(first, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	blockedRead = make(chan queryResult, 1)
	go func() {
		result, err := engine.Execute(second, "SELECT value FROM items WHERE id=1")
		blockedRead <- queryResult{result: result, err: err}
	}()
	select {
	case outcome := <-blockedRead:
		t.Fatalf("other session read did not wait for disconnect rollback: %#v, %v", outcome.result, outcome.err)
	case <-time.After(30 * time.Millisecond):
	}
	engine.CloseSession(first)
	select {
	case outcome := <-blockedRead:
		if outcome.err != nil || len(outcome.result.Rows) != 1 || outcome.result.Rows[0][0] != "committed" {
			t.Fatalf("read after disconnect rollback = %#v, %v", outcome.result, outcome.err)
		}
	case <-time.After(time.Second):
		t.Fatal("other session remained blocked after disconnect rollback")
	}
}

func TestPersistenceFailureFailsClosedWithoutRetryingDivergentMemory(t *testing.T) {
	directory := t.TempDir()
	engine, err := Open(directory, "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	setup := &Session{CurrentDatabase: "failure_test"}
	for _, query := range []string{
		"CREATE DATABASE failure_test",
		"CREATE TABLE failure_test.items(id INT NOT NULL,PRIMARY KEY(id))",
		"INSERT INTO failure_test.items VALUES(1)",
	} {
		if _, err := engine.Execute(setup, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}

	started := make(chan struct{})
	release := make(chan struct{})
	saveCalls := 0
	engine.persistSave = func(*storage.Store) error {
		saveCalls++
		if saveCalls == 1 {
			close(started)
		}
		<-release
		return errors.New("injected durable replace failure")
	}
	engine.persistMu.Lock()
	baselineGeneration := engine.persistNext
	engine.persistMu.Unlock()

	writeErrors := make(chan error, 2)
	go func() {
		_, executeErr := engine.Execute(&Session{CurrentDatabase: "failure_test"}, "INSERT INTO items VALUES(2)")
		writeErrors <- executeErr
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first persistence attempt did not start")
	}
	go func() {
		_, executeErr := engine.Execute(&Session{CurrentDatabase: "failure_test"}, "INSERT INTO items VALUES(3)")
		writeErrors <- executeErr
	}()
	deadline := time.Now().Add(time.Second)
	for {
		engine.persistMu.Lock()
		bothWaiting := engine.persistNext >= baselineGeneration+2
		engine.persistMu.Unlock()
		if bothWaiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second write did not join the pending persistence batch")
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	for range 2 {
		if err := <-writeErrors; !errors.Is(err, ErrPersistenceUnavailable) {
			t.Fatalf("write error = %v", err)
		}
	}
	if saveCalls != 1 {
		t.Fatalf("persistence retried after fatal failure: %d calls", saveCalls)
	}
	if _, err := engine.Execute(&Session{CurrentDatabase: "failure_test"}, "SELECT * FROM items"); !errors.Is(err, ErrPersistenceUnavailable) {
		t.Fatalf("query after persistence failure = %v", err)
	}
	if err := engine.Close(); !errors.Is(err, ErrPersistenceUnavailable) {
		t.Fatalf("close after persistence failure = %v", err)
	}

	reopened, err := Open(directory, "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	result, err := reopened.Execute(&Session{CurrentDatabase: "failure_test"}, "SELECT id FROM items ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 || result.Rows[0][0] != int64(1) {
		t.Fatalf("durable snapshot contains failed writes: %#v", result.Rows)
	}
}

func TestCommitAndRollbackWithoutTransactionAreNoOps(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{}
	for _, query := range []string{"COMMIT", "ROLLBACK"} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s returned an error: %v", query, err)
		}
	}
}

func TestMySQLUserManagementAndPrivilegeEnforcement(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "root-secret")
	if err != nil {
		t.Fatal(err)
	}
	trusted := &Session{}
	for _, query := range []string{
		"CREATE DATABASE security_test",
		"CREATE TABLE security_test.records(id INT,label VARCHAR(32))",
		"CREATE TABLE security_test.private_records(id INT)",
		"INSERT INTO security_test.records VALUES (1,'visible')",
	} {
		if _, err := engine.Execute(trusted, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	root := &Session{CurrentDatabase: "security_test", Username: "root", Host: "%"}
	for _, query := range []string{
		"CREATE USER 'reader'@'%' IDENTIFIED BY 'secret'",
		"GRANT SELECT ON security_test.records TO 'reader'@'%'",
	} {
		if _, err := engine.Execute(root, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	reader := &Session{CurrentDatabase: "security_test", Username: "reader", Host: "%"}
	result, err := engine.Execute(reader, "SELECT id,label FROM records")
	if err != nil || len(result.Rows) != 1 || result.Rows[0][1] != "visible" {
		t.Fatalf("authorized SELECT = %#v, %v", result, err)
	}
	for _, query := range []string{
		"SELECT * FROM private_records",
		"INSERT INTO records VALUES (2,'denied')",
		"CREATE TABLE denied(id INT)",
	} {
		if _, err := engine.Execute(reader, query); err == nil || !strings.Contains(err.Error(), "access denied") {
			t.Fatalf("expected %s to be denied, got %v", query, err)
		}
	}
	grants, err := engine.Execute(reader, "SHOW GRANTS")
	if err != nil || len(grants.Rows) < 2 || !strings.Contains(grants.Rows[1][0].(string), "SELECT") {
		t.Fatalf("SHOW GRANTS = %#v, %v", grants, err)
	}
	if _, err := engine.Execute(root, "GRANT INSERT ON security_test.records TO 'reader'@'%'"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Execute(reader, "INSERT INTO records VALUES (2,'allowed')"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Execute(root, "REVOKE SELECT ON security_test.records FROM 'reader'@'%'"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Execute(reader, "SELECT * FROM records"); err == nil {
		t.Fatal("revoked SELECT remained active")
	}
	if _, err := engine.Execute(reader, "SET PASSWORD = 'changed'"); err != nil {
		t.Fatal(err)
	}
	if !engine.Users.VerifyPassword("reader", "changed") {
		t.Fatal("self-service password change was not persisted")
	}
	if _, err := engine.Execute(root, "RENAME USER 'reader'@'%' TO 'reporter'@'%'"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Execute(root, "DROP USER 'reporter'@'%'"); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentTransactionsDoNotLoseTables(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	setup := &Session{}
	if _, err := engine.Execute(setup, "CREATE DATABASE import_test"); err != nil {
		t.Fatal(err)
	}
	first := &Session{CurrentDatabase: "import_test"}
	second := &Session{CurrentDatabase: "import_test"}
	if _, err := engine.Execute(first, "BEGIN"); err != nil {
		t.Fatal(err)
	}
	secondBegin := make(chan error, 1)
	go func() {
		_, beginErr := engine.Execute(second, "BEGIN")
		secondBegin <- beginErr
	}()
	time.Sleep(20 * time.Millisecond)
	if _, err := engine.Execute(first, "CREATE TABLE auth_code(id INT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Execute(first, "COMMIT"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-secondBegin:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("second transaction remained blocked after first commit")
	}
	if _, err := engine.Execute(second, "CREATE TABLE auth_code_equ(id INT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Execute(second, "COMMIT"); err != nil {
		t.Fatal(err)
	}
	database, err := engine.Store.Database("import_test")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"auth_code", "auth_code_equ"} {
		if _, err := database.Table(name); err != nil {
			t.Fatalf("table %s was lost after concurrent commits: %v", name, err)
		}
	}
}

func TestDropMultipleQualifiedTables(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE navicat_e2e",
		"USE navicat_e2e",
		"CREATE TABLE auth_app(id INT)",
		"CREATE TABLE auth_app_navicat(id INT)",
		"CREATE TABLE auth_app_navicat_fixed(id INT)",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	result, err := engine.Execute(session, "DROP TABLE IF EXISTS `navicat_e2e`.`auth_app`, `navicat_e2e`.`missing`, `navicat_e2e`.`auth_app_navicat`, `navicat_e2e`.`auth_app_navicat_fixed`")
	if err != nil {
		t.Fatal(err)
	}
	if result.AffectedRows != 3 {
		t.Fatalf("affected rows = %d, want 3", result.AffectedRows)
	}
	database, err := engine.Store.Database("navicat_e2e")
	if err != nil {
		t.Fatal(err)
	}
	if tables := database.ListTables(); len(tables) != 0 {
		t.Fatalf("tables still present: %v", tables)
	}
}

func TestCreateQualifiedTableWithoutSelectedDatabase(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{}
	if _, err := engine.Execute(session, "CREATE DATABASE qualified"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Execute(session, "CREATE TABLE qualified.items(id INT)"); err != nil {
		t.Fatal(err)
	}
	database, err := engine.Store.Database("qualified")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Table("items"); err != nil {
		t.Fatal(err)
	}
}

func TestShowTableTypesUseMySQLLowercase(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE test",
		"USE test",
		"CREATE TABLE imported(id VARCHAR(64), description TINYTEXT, created_at DATETIME)",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	columns, err := engine.Execute(session, "SHOW COLUMNS FROM imported")
	if err != nil {
		t.Fatal(err)
	}
	if columns.Rows[0][1] != "varchar(64)" || columns.Rows[1][1] != "tinytext" || columns.Rows[2][1] != "datetime" {
		t.Fatalf("unexpected displayed types: %#v", columns.Rows)
	}
	create, err := engine.Execute(session, "SHOW CREATE TABLE imported")
	if err != nil {
		t.Fatal(err)
	}
	ddl := create.Rows[0][1].(string)
	if strings.Contains(ddl, "VARCHAR") || strings.Contains(ddl, "TEXT") || strings.Contains(ddl, "DATE") {
		t.Fatalf("SHOW CREATE TABLE contains uppercase types: %s", ddl)
	}
}

func TestShowColumnsReportsIndexKeys(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE column_key_test",
		"USE column_key_test",
		"CREATE TABLE items(id INT,label VARCHAR(32),category VARCHAR(32))",
		"CREATE UNIQUE INDEX items_id ON items(id)",
		"CREATE INDEX items_category ON items(category)",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatal(err)
		}
	}
	columns, err := engine.Execute(session, "SHOW COLUMNS FROM items")
	if err != nil {
		t.Fatal(err)
	}
	if columns.Rows[0][3] != "UNI" || columns.Rows[1][3] != "" || columns.Rows[2][3] != "MUL" {
		t.Fatalf("unexpected column keys: %#v", columns.Rows)
	}
}

func TestMySQLColumnTypeMetadataPersists(t *testing.T) {
	directory := t.TempDir()
	engine, err := Open(directory, "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{}
	query := "CREATE TABLE type_metadata.types(" +
		"tiny TINYINT(1) UNSIGNED," +
		"small SMALLINT UNSIGNED," +
		"amount DECIMAL(20,6) UNSIGNED," +
		"fixed_code CHAR(8)," +
		"payload LONGBLOB," +
		"state ENUM('new','in progress','done')," +
		"flags SET('a','b')," +
		"document JSON," +
		"clock TIME(6)," +
		"created TIMESTAMP(6)," +
		"year_value YEAR(4)," +
		"shape GEOMETRY)"
	if _, err := engine.Execute(session, "CREATE DATABASE type_metadata"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Execute(session, query); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Execute(session, "ALTER TABLE type_metadata.types MODIFY COLUMN amount DECIMAL(30,8) UNSIGNED"); err != nil {
		t.Fatal(err)
	}
	show, err := engine.Execute(session, "SHOW CREATE TABLE type_metadata.types")
	if err != nil {
		t.Fatal(err)
	}
	ddl := show.Rows[0][1].(string)
	for _, declaration := range []string{
		"`tiny` tinyint(1) unsigned",
		"`amount` decimal(30,8) unsigned",
		"`state` enum('new','in progress','done')",
		"`clock` time(6)",
		"`created` timestamp(6)",
		"`shape` geometry",
	} {
		if !strings.Contains(ddl, declaration) {
			t.Fatalf("SHOW CREATE TABLE missing %q: %s", declaration, ddl)
		}
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(directory, "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	describe, err := reopened.Execute(session, "SHOW COLUMNS FROM type_metadata.types")
	if err != nil {
		t.Fatal(err)
	}
	if describe.Rows[2][1] != "decimal(30,8) unsigned" || describe.Rows[5][1] != "enum('new','in progress','done')" || describe.Rows[9][1] != "timestamp(6)" {
		t.Fatalf("unexpected persisted types: %#v", describe.Rows)
	}
}
func TestExport(t *testing.T) {
	dir := t.TempDir()
	engine, err := Open(dir, "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{}
	for _, query := range []string{"CREATE DATABASE test", "USE test", "CREATE TABLE t(id INT)", "INSERT INTO t VALUES (1)", "CREATE VIEW z_base AS SELECT id FROM t", "CREATE VIEW a_nested AS SELECT id FROM z_base", "CREATE USER 'backup_reader'@'%' IDENTIFIED BY 'must-not-leak'"} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatal(err)
		}
	}
	output := filepath.Join(dir, "backup.sql")
	if err := os.WriteFile(output, []byte("previous backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ExportSQL(engine.Store, "test", output); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	basePosition := strings.Index(string(content), "CREATE VIEW `z_base`")
	nestedPosition := strings.Index(string(content), "CREATE VIEW `a_nested`")
	if basePosition < 0 || nestedPosition < basePosition {
		t.Fatalf("views not exported in dependency order:\n%s", content)
	}
	if strings.Contains(string(content), "must-not-leak") || strings.Contains(strings.ToUpper(string(content)), "CREATE USER") {
		t.Fatalf("logical backup leaked account credentials:\n%s", content)
	}
}

func TestStoppedDataDirectoryCopyRestoresDataObjectsAndPrivileges(t *testing.T) {
	sourceDirectory := t.TempDir()
	engine, err := Open(sourceDirectory, "root", "root-secret")
	if err != nil {
		t.Fatal(err)
	}
	root := &Session{Username: "root", Host: "%"}
	for _, query := range []string{
		"CREATE DATABASE recovery_test",
		"CREATE TABLE recovery_test.items(id INT NOT NULL,label VARCHAR(32),PRIMARY KEY(id),KEY idx_label(label))",
		"INSERT INTO recovery_test.items VALUES(1,'restored')",
		"CREATE VIEW recovery_test.item_view AS SELECT id,label FROM recovery_test.items",
		"CREATE USER 'reader'@'%' IDENTIFIED BY 'reader-secret'",
		"GRANT SELECT ON recovery_test.* TO 'reader'@'%'",
	} {
		if _, err := engine.Execute(root, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}

	restoredDirectory := t.TempDir()
	copyDirectoryTree(t, sourceDirectory, restoredDirectory)
	restored, err := Open(restoredDirectory, "root", "")
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	reader := &Session{CurrentDatabase: "recovery_test", Username: "reader", Host: "%"}
	result, err := restored.Execute(reader, "SELECT id,label FROM item_view")
	if err != nil || len(result.Rows) != 1 || result.Rows[0][0] != int64(1) || result.Rows[0][1] != "restored" {
		t.Fatalf("restored reader SELECT = %#v, %v", result, err)
	}
	if _, err := restored.Execute(reader, "INSERT INTO items VALUES(2,'denied')"); err == nil || !strings.Contains(strings.ToLower(err.Error()), "access denied") {
		t.Fatalf("restored write privilege error = %v", err)
	}
	indexes, err := restored.Execute(root, "SHOW INDEX FROM recovery_test.items")
	if err != nil || len(indexes.Rows) != 2 {
		t.Fatalf("restored indexes = %#v, %v", indexes, err)
	}
	grants, err := restored.Execute(root, "SHOW GRANTS FOR 'reader'@'%'")
	hasSelectGrant := false
	if err == nil {
		for _, row := range grants.Rows {
			if strings.Contains(row[0].(string), "SELECT") {
				hasSelectGrant = true
				break
			}
		}
	}
	if err != nil || !hasSelectGrant {
		t.Fatalf("restored grants = %#v, %v", grants, err)
	}
}

func copyDirectoryTree(t *testing.T, source, destination string) {
	t.Helper()
	err := filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		return os.WriteFile(target, contents, info.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRecursiveDateRangeCTEWithLeftJoinAggregation(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{StreamResults: true}
	today := normalizeSQLDate(time.Now())
	queries := []string{
		"CREATE DATABASE recursive_cte_test",
		"USE recursive_cte_test",
		"CREATE TABLE chat_order(id INT, create_time DATE)",
		fmt.Sprintf("INSERT INTO chat_order VALUES (1,'%s'),(2,'%s'),(3,'%s')",
			today.Format("2006-01-02"), today.Format("2006-01-02"), today.AddDate(0, 0, -3).Format("2006-01-02")),
	}
	for _, query := range queries {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}

	result, err := engine.Execute(session, `WITH RECURSIVE date_range AS (
		SELECT CURDATE() AS dt
		UNION ALL
		SELECT DATE_SUB(dt, INTERVAL 1 DAY)
		FROM date_range
		WHERE DATE_SUB(dt, INTERVAL 1 DAY) >= DATE_SUB(CURDATE(), INTERVAL 6 DAY)
	)
	SELECT dr.dt AS date, COUNT(co.id) AS consultCount
	FROM date_range dr
	LEFT JOIN chat_order co ON DATE(co.create_time) = dr.dt
	GROUP BY dr.dt
	ORDER BY dr.dt`)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := collectResultRows(result)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 7 {
		t.Fatalf("returned %d date rows: %#v", len(rows), rows)
	}
	for index, row := range rows {
		wantDate := today.AddDate(0, 0, index-6)
		if compareAny(row[0], wantDate) != 0 {
			t.Fatalf("row %d date = %v, want %v", index, row[0], wantDate)
		}
		wantCount := int64(0)
		if index == 3 {
			wantCount = 1
		}
		if index == 6 {
			wantCount = 2
		}
		if row[1] != wantCount {
			t.Fatalf("row %d count = %v, want %d", index, row[1], wantCount)
		}
	}
}

func TestRankingAndAggregateWindowFunctions(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE window_test",
		"USE window_test",
		"CREATE TABLE sales(id INT, region VARCHAR(16), amount INT)",
		"INSERT INTO sales VALUES (1,'east',10),(2,'east',20),(3,'east',20),(4,'west',5),(5,'west',15)",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	result, err := engine.Execute(session, `SELECT id,region,amount,
		ROW_NUMBER() OVER (PARTITION BY region ORDER BY amount DESC,id) AS row_num,
		RANK() OVER (PARTITION BY region ORDER BY amount DESC) AS rank_num,
		DENSE_RANK() OVER (PARTITION BY region ORDER BY amount DESC) AS dense_num,
		COUNT(*) OVER (PARTITION BY region) AS region_count,
		SUM(amount) OVER (PARTITION BY region ORDER BY id) AS running_total,
		SUM(amount) OVER (PARTITION BY region ORDER BY amount) AS peer_total
		FROM sales ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]any{
		{int64(1), "east", int64(10), int64(3), int64(3), int64(2), int64(3), int64(10), int64(10)},
		{int64(2), "east", int64(20), int64(1), int64(1), int64(1), int64(3), int64(30), int64(50)},
		{int64(3), "east", int64(20), int64(2), int64(1), int64(1), int64(3), int64(50), int64(50)},
		{int64(4), "west", int64(5), int64(2), int64(2), int64(2), int64(2), int64(5), int64(5)},
		{int64(5), "west", int64(15), int64(1), int64(1), int64(1), int64(2), int64(20), int64(20)},
	}
	if len(result.Rows) != len(want) {
		t.Fatalf("window rows = %#v", result.Rows)
	}
	for rowIndex := range want {
		for columnIndex := range want[rowIndex] {
			if compareAny(result.Rows[rowIndex][columnIndex], want[rowIndex][columnIndex]) != 0 {
				t.Fatalf("row %d column %d = %#v, want %#v", rowIndex, columnIndex, result.Rows[rowIndex][columnIndex], want[rowIndex][columnIndex])
			}
		}
	}
	star, err := engine.Execute(session, "SELECT *,ROW_NUMBER() OVER (ORDER BY id) AS rn FROM sales ORDER BY id LIMIT 1")
	if err != nil || len(star.Rows) != 1 || len(star.Rows[0]) != 4 || star.Rows[0][3] != int64(1) {
		t.Fatalf("unexpected star window result: %#v, %v", star, err)
	}
}

func TestPersistentViewsNestedViewsAndMetadata(t *testing.T) {
	dataDir := t.TempDir()
	engine, err := Open(dataDir, "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{StreamResults: true}
	for _, query := range []string{
		"CREATE DATABASE view_test",
		"USE view_test",
		"CREATE TABLE users(id INT,name VARCHAR(32),age INT,region VARCHAR(16))",
		"INSERT INTO users VALUES (1,'Alice',20,'east'),(2,'Bob',16,'west'),(3,'Carol',30,'east')",
		"CREATE VIEW adult_users AS SELECT id,name,age,region FROM users WHERE age >= 18",
		"CREATE VIEW east_adults(user_id,display_name) AS SELECT id,name FROM adult_users WHERE region='east'",
		"CREATE VIEW region_totals AS SELECT region,COUNT(*) AS total FROM adult_users GROUP BY region",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}

	result, err := engine.Execute(session, "SELECT e.user_id,e.display_name FROM east_adults e ORDER BY e.user_id")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := collectResultRows(result)
	if err != nil || len(rows) != 2 || rows[0][1] != "Alice" || rows[1][1] != "Carol" {
		t.Fatalf("nested view rows = %#v, %v", rows, err)
	}
	if _, err := engine.Execute(session, "INSERT INTO users VALUES (4,'Dave',40,'east')"); err != nil {
		t.Fatal(err)
	}
	count, err := engine.Execute(session, "SELECT COUNT(*) FROM east_adults")
	if err != nil || count.Rows[0][0] != int64(3) {
		t.Fatalf("live view count = %#v, %v", count, err)
	}
	if _, err := engine.Execute(session, "CREATE OR REPLACE VIEW region_totals AS SELECT region,SUM(age) AS total FROM adult_users GROUP BY region"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Execute(session, "ALTER VIEW region_totals AS SELECT region,SUM(age) AS total FROM adult_users GROUP BY region"); err != nil {
		t.Fatal(err)
	}
	totals, err := engine.Execute(session, "SELECT total FROM region_totals WHERE region='east'")
	if err != nil {
		t.Fatal(err)
	}
	totalRows, err := collectResultRows(totals)
	if err != nil || len(totalRows) != 1 || totalRows[0][0] != int64(90) {
		t.Fatalf("replaced view rows = %#v, %v", totalRows, err)
	}
	full, err := engine.Execute(session, "SHOW FULL TABLES")
	if err != nil || len(full.Rows) != 4 {
		t.Fatalf("SHOW FULL TABLES = %#v, %v", full, err)
	}
	viewTypeFound := false
	for _, row := range full.Rows {
		if row[0] == "east_adults" && row[1] == "VIEW" {
			viewTypeFound = true
		}
	}
	if !viewTypeFound {
		t.Fatalf("view type missing: %#v", full.Rows)
	}
	allRelations, err := engine.Execute(session, "SHOW TABLES")
	if err != nil || len(allRelations.Rows) != 4 {
		t.Fatalf("SHOW TABLES should contain base tables and views: %#v, %v", allRelations, err)
	}
	relationNames := make(map[string]bool, len(allRelations.Rows))
	for _, row := range allRelations.Rows {
		relationNames[row[0].(string)] = true
	}
	if !relationNames["users"] || !relationNames["east_adults"] || !relationNames["adult_users"] || !relationNames["region_totals"] {
		t.Fatalf("SHOW TABLES relations = %#v", allRelations.Rows)
	}
	create, err := engine.Execute(session, "SHOW CREATE VIEW east_adults")
	if err != nil || !strings.Contains(create.Rows[0][1].(string), "CREATE VIEW `east_adults`") {
		t.Fatalf("SHOW CREATE VIEW = %#v, %v", create, err)
	}
	if _, err := engine.Execute(session, "CREATE VIEW recursive_view AS SELECT * FROM recursive_view"); err == nil || !strings.Contains(err.Error(), "circular view") {
		t.Fatalf("expected circular view error, got %v", err)
	}
	if _, err := engine.Execute(session, "ALTER VIEW missing_view AS SELECT id FROM users"); !errors.Is(err, storage.ErrViewNotFound) {
		t.Fatalf("expected ALTER VIEW to require an existing view, got %v", err)
	}
	if _, err := engine.Execute(session, "INSERT INTO east_adults VALUES (5,'Eve')"); err == nil {
		t.Fatal("expected a view to reject INSERT")
	}

	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dataDir, "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	reopenedSession := &Session{CurrentDatabase: "view_test"}
	persisted, err := reopened.Execute(reopenedSession, "SELECT COUNT(*) FROM east_adults")
	if err != nil || persisted.Rows[0][0] != int64(3) {
		t.Fatalf("persisted view = %#v, %v", persisted, err)
	}
	if _, err := reopened.Execute(reopenedSession, "DROP VIEW IF EXISTS east_adults,region_totals"); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyViewWithDuplicateJoinColumnNames(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	database, err := engine.Store.CreateDatabase("duplicate_view_test")
	if err != nil {
		t.Fatal(err)
	}
	left, err := database.CreateTableWithPrimary("left_items", []storage.Column{{Name: "id", Type: storage.TypeInt}, {Name: "label", Type: storage.TypeVarchar, Length: 16}}, []string{"id"})
	if err != nil {
		t.Fatal(err)
	}
	right, err := database.CreateTableWithPrimary("right_items", []storage.Column{{Name: "id", Type: storage.TypeInt}, {Name: "left_id", Type: storage.TypeInt}}, []string{"id"})
	if err != nil {
		t.Fatal(err)
	}
	if err := left.Insert(storage.NewRow(storage.MustValue(storage.TypeInt, 1), storage.MustValue(storage.TypeVarchar, "one"))); err != nil {
		t.Fatal(err)
	}
	if err := right.Insert(storage.NewRow(storage.MustValue(storage.TypeInt, 10), storage.MustValue(storage.TypeInt, 99))); err != nil {
		t.Fatal(err)
	}
	definition := "SELECT * FROM left_items l LEFT JOIN right_items r ON l.id=r.left_id"
	if err := database.CreateView("legacy_join", definition, nil, false); err != nil {
		t.Fatal(err)
	}
	result, err := engine.Execute(&Session{CurrentDatabase: database.Name()}, "SELECT * FROM legacy_join")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Columns) != 4 || result.Columns[0].Name != "id" || result.Columns[2].Name != "id_2" {
		t.Fatalf("view columns = %#v", result.Columns)
	}
	if len(result.Rows) != 1 || result.Rows[0][0] != int64(1) || result.Rows[0][2] != nil {
		t.Fatalf("view rows = %#v", result.Rows)
	}
	if err := database.CreateView("ssssd", definition, nil, false); err != nil {
		t.Fatal(err)
	}
	legacyNumeric, err := engine.Execute(&Session{CurrentDatabase: database.Name()}, "SELECT * from 1ssssd")
	if err != nil || len(legacyNumeric.Rows) != 1 {
		t.Fatalf("numeric legacy view query = %#v, %v", legacyNumeric, err)
	}
}
