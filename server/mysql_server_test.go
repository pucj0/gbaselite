package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gbaselite/executor"
	"gbaselite/journal"
	"gbaselite/storage"

	_ "github.com/go-sql-driver/mysql"
)

func TestProtocolAuditLogIncludesIdentityAndRedactsSQL(t *testing.T) {
	engine, err := executor.Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	audit, err := journal.OpenAudit(auditPath, journal.DefaultRetentionDays)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	databaseServer := &MySQLServer{Engine: engine, Logger: log.New(io.Discard, "", 0), Audit: audit}
	done := make(chan error, 1)
	go func() { done <- databaseServer.Serve(listener) }()
	client, err := sql.Open("mysql", "root:123456@tcp("+listener.Addr().String()+")/?charset=utf8mb4&timeout=3s")
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"CREATE DATABASE audit_test", "CREATE TABLE audit_test.items(id INT,label VARCHAR(32))", "INSERT INTO audit_test.items VALUES (17,'private value')", "/*!50001 CREATE VIEW audit_test.item_view AS SELECT id FROM audit_test.items */"} {
		if _, err := client.Exec(query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	_ = client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := databaseServer.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := audit.Close(); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	if !strings.Contains(text, `"username":"root"`) || !strings.Contains(text, `"remote_ip":"127.0.0.1"`) || !strings.Contains(text, `"operation":"INSERT"`) || !strings.Contains(text, `"operation":"CREATE VIEW"`) || !strings.Contains(text, `"affected_rows":1`) {
		t.Fatalf("audit log is missing required fields:\n%s", text)
	}
	if strings.Contains(text, "private value") || strings.Contains(text, "VALUES (17") {
		t.Fatalf("audit log contains unredacted values:\n%s", text)
	}
}

func BenchmarkMySQLSelect60Rows(b *testing.B) {
	engine, err := executor.Open(b.TempDir(), "root", "123456")
	if err != nil {
		b.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	databaseServer := &MySQLServer{Engine: engine, Logger: log.New(io.Discard, "", 0)}
	done := make(chan error, 1)
	go func() { done <- databaseServer.Serve(listener) }()
	client, err := sql.Open("mysql", "root:123456@tcp("+listener.Addr().String()+")/?charset=utf8mb4&timeout=3s")
	if err != nil {
		b.Fatal(err)
	}
	for _, query := range []string{
		"CREATE DATABASE benchmark",
		"USE benchmark",
		"CREATE TABLE records(id INT,c1 VARCHAR(64),c2 VARCHAR(64),c3 VARCHAR(64),c4 VARCHAR(64),c5 VARCHAR(64),c6 VARCHAR(64),c7 VARCHAR(64),c8 VARCHAR(64),c9 VARCHAR(64))",
	} {
		if _, err := client.Exec(query); err != nil {
			b.Fatal(err)
		}
	}
	var insert strings.Builder
	insert.WriteString("INSERT INTO records VALUES ")
	for i := 0; i < 60; i++ {
		if i > 0 {
			insert.WriteByte(',')
		}
		fmt.Fprintf(&insert, "(%d,'a','b','c','d','e','f','g','h','i')", i)
	}
	if _, err := client.Exec(insert.String()); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := client.Query("SELECT * FROM records")
		if err != nil {
			b.Fatal(err)
		}
		for rows.Next() {
			values := make([]sql.RawBytes, 10)
			destinations := make([]any, len(values))
			for index := range values {
				destinations[index] = &values[index]
			}
			if err := rows.Scan(destinations...); err != nil {
				b.Fatal(err)
			}
		}
		if err := rows.Close(); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	if err := client.Close(); err != nil {
		b.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := databaseServer.Shutdown(ctx); err != nil {
		b.Fatal(err)
	}
	if err := <-done; err != nil {
		b.Fatal(err)
	}
}

func TestSlowQueryLogging(t *testing.T) {
	var output strings.Builder
	databaseServer := &MySQLServer{Logger: log.New(&output, "", 0), SlowQuery: time.Microsecond}
	databaseServer.logSlowQuery("SELECT 1", time.Now().Add(-time.Millisecond))
	if logOutput := output.String(); !strings.Contains(logOutput, "slow query") || !strings.Contains(logOutput, "SELECT 1") {
		t.Fatalf("slow query log = %q", logOutput)
	}
	output.Reset()
	databaseServer.SlowQuery = 0
	databaseServer.logSlowQuery("SELECT 2", time.Now().Add(-time.Second))
	if output.Len() != 0 {
		t.Fatalf("disabled slow query log = %q", output.String())
	}
}

func TestShowDatabasesListsOnlyAccessiblePersistentDatabases(t *testing.T) {
	engine, err := executor.Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Execute(&executor.Session{}, "CREATE DATABASE application"); err != nil {
		t.Fatal(err)
	}

	for _, query := range []string{"SHOW DATABASES", "SHOW SCHEMAS"} {
		result, err := ExecuteCompatible(engine, &executor.Session{Username: "root", Host: "%"}, query)
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		if len(result.Rows) != 1 || result.Rows[0][0] != "application" {
			t.Fatalf("%s = %#v, want only application", query, result.Rows)
		}
	}
}

func TestNavicatDatabaseDumpMetadataOverPreparedProtocol(t *testing.T) {
	engine, err := executor.Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	databaseServer := &MySQLServer{Engine: engine, Logger: log.New(io.Discard, "", 0)}
	done := make(chan error, 1)
	go func() { done <- databaseServer.Serve(listener) }()

	client, err := sql.Open("mysql", "root:123456@tcp("+listener.Addr().String()+")/?charset=utf8mb4&timeout=3s")
	if err != nil {
		t.Fatal(err)
	}
	client.SetMaxOpenConns(1)
	defer func() {
		_ = client.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := databaseServer.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	}()

	for _, query := range []string{
		"/* Navicat Premium Dump SQL\n Source Schema: navicat-export-test\n*/\nSET NAMES utf8mb4",
		"-- Navicat session setting\nSET FOREIGN_KEY_CHECKS = 0",
		"/*!40101 SET @OLD_CHARACTER_SET_CLIENT=@@CHARACTER_SET_CLIENT */",
		"CREATE DATABASE `navicat-export-test`",
		"USE `navicat-export-test`",
		"CREATE TABLE `order-items` (`id` BIGINT NOT NULL AUTO_INCREMENT, `sku` VARCHAR(32) NOT NULL, `qty` INT NOT NULL DEFAULT 0, PRIMARY KEY (`id`), UNIQUE KEY `uq_sku` (`sku`), KEY `idx_qty` (`qty`))",
		"INSERT INTO `order-items` (`sku`,`qty`) VALUES ('SKU-001',2),('SKU-002',0)",
		"/*!50001 CREATE VIEW `active-items` AS SELECT `id`,`sku`,`qty` FROM `order-items` WHERE `qty` > 0 */",
	} {
		if _, err := client.Exec(query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}

	databaseRows, err := client.Query("SHOW DATABASES")
	if err != nil {
		t.Fatal(err)
	}
	var databases []string
	for databaseRows.Next() {
		var name string
		if err := databaseRows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		databases = append(databases, name)
	}
	if err := databaseRows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(databases) != 1 || databases[0] != "navicat-export-test" {
		t.Fatalf("SHOW DATABASES = %#v", databases)
	}
	var shownDatabase, createDatabase string
	if err := client.QueryRow("SHOW CREATE DATABASE IF NOT EXISTS `navicat-export-test`").Scan(&shownDatabase, &createDatabase); err != nil {
		t.Fatal(err)
	}
	if shownDatabase != "navicat-export-test" || createDatabase != "CREATE DATABASE IF NOT EXISTS `navicat-export-test`" {
		t.Fatalf("SHOW CREATE DATABASE IF NOT EXISTS = %q %q", shownDatabase, createDatabase)
	}

	relationRows, err := client.Query("SHOW TABLES")
	if err != nil {
		t.Fatal(err)
	}
	var relationNames []string
	for relationRows.Next() {
		var name string
		if err := relationRows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		relationNames = append(relationNames, name)
	}
	if err := relationRows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(relationNames) != 2 || relationNames[0] != "active-items" || relationNames[1] != "order-items" {
		t.Fatalf("mysqldump SHOW TABLES enumeration = %#v", relationNames)
	}

	tableStatement, err := client.Prepare("SHOW FULL TABLES WHERE Table_type != ?")
	if err != nil {
		t.Fatal(err)
	}
	tableRows, err := tableStatement.Query("VIEW")
	if err != nil {
		t.Fatal(err)
	}
	var tableName, tableType string
	if !tableRows.Next() || tableRows.Scan(&tableName, &tableType) != nil {
		t.Fatalf("Navicat table enumeration returned no readable row: %v", tableRows.Err())
	}
	if tableName != "order-items" || tableType != "BASE TABLE" || tableRows.Next() {
		t.Fatalf("Navicat table enumeration = %q %q", tableName, tableType)
	}
	if err := tableRows.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tableStatement.Close(); err != nil {
		t.Fatal(err)
	}

	metadataStatement, err := client.Prepare("SELECT TABLE_SCHEMA, TABLE_NAME, TABLE_TYPE FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? ORDER BY TABLE_SCHEMA, TABLE_TYPE")
	if err != nil {
		t.Fatal(err)
	}
	metadataRows, err := metadataStatement.Query("navicat-export-test")
	if err != nil {
		t.Fatal(err)
	}
	var relations [][2]string
	for metadataRows.Next() {
		var schema, name, relationType string
		if err := metadataRows.Scan(&schema, &name, &relationType); err != nil {
			t.Fatal(err)
		}
		if schema != "navicat-export-test" {
			t.Fatalf("metadata schema = %q", schema)
		}
		relations = append(relations, [2]string{name, relationType})
	}
	if err := metadataRows.Close(); err != nil {
		t.Fatal(err)
	}
	if err := metadataStatement.Close(); err != nil {
		t.Fatal(err)
	}
	if len(relations) != 2 || relations[0] != [2]string{"order-items", "BASE TABLE"} || relations[1] != [2]string{"active-items", "VIEW"} {
		t.Fatalf("information_schema.TABLES = %#v", relations)
	}

	var createTable string
	if err := client.QueryRow("SHOW CREATE TABLE `navicat-export-test`.`order-items`").Scan(&tableName, &createTable); err != nil {
		t.Fatal(err)
	}
	if tableName != "order-items" || !strings.Contains(createTable, "PRIMARY KEY (`id`)") || !strings.Contains(createTable, "UNIQUE KEY `uq_sku` (`sku`)") || !strings.Contains(createTable, "KEY `idx_qty` (`qty`)") {
		t.Fatalf("SHOW CREATE TABLE = %q %q", tableName, createTable)
	}
	var id, quantity int64
	var sku string
	if err := client.QueryRow("SELECT `id`,`sku`,`qty` FROM `navicat-export-test`.`order-items` ORDER BY `id` LIMIT 1").Scan(&id, &sku, &quantity); err != nil {
		t.Fatal(err)
	}
	if id != 1 || sku != "SKU-001" || quantity != 2 {
		t.Fatalf("dump data row = %d %q %d", id, sku, quantity)
	}
}

func TestNavicatCommonWorkflowsOverMySQLProtocol(t *testing.T) {
	engine, err := executor.Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	databaseServer := &MySQLServer{Engine: engine, Logger: log.New(io.Discard, "", 0)}
	done := make(chan error, 1)
	go func() { done <- databaseServer.Serve(listener) }()

	client, err := sql.Open("mysql", "root:123456@tcp("+listener.Addr().String()+")/?charset=utf8mb4&timeout=3s")
	if err != nil {
		t.Fatal(err)
	}
	client.SetMaxOpenConns(3)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connection, err := client.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = connection.Close()
		_ = client.Close()
		shutdownContext, shutdownCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer shutdownCancel()
		if err := databaseServer.Shutdown(shutdownContext); err != nil {
			t.Errorf("shutdown: %v", err)
		}
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	}()

	exec := func(query string) sql.Result {
		t.Helper()
		result, execErr := connection.ExecContext(ctx, query)
		if execErr != nil {
			t.Fatalf("%s: %v", query, execErr)
		}
		return result
	}
	listRelations := func(database string) map[string]string {
		t.Helper()
		rows, queryErr := client.QueryContext(ctx, "SHOW FULL TABLES FROM `"+database+"`")
		if queryErr != nil {
			t.Fatalf("list relations in %s: %v", database, queryErr)
		}
		defer rows.Close()
		relations := make(map[string]string)
		for rows.Next() {
			var name, relationType string
			if scanErr := rows.Scan(&name, &relationType); scanErr != nil {
				t.Fatal(scanErr)
			}
			relations[name] = relationType
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			t.Fatal(rowsErr)
		}
		return relations
	}
	findCopy := func(relations map[string]string, source, relationType string, sequence int, days ...string) string {
		t.Helper()
		for name, actualType := range relations {
			if actualType != relationType {
				continue
			}
			for _, day := range days {
				if name == fmt.Sprintf("%s_copy_%s%02d", source, day, sequence) {
					return name
				}
			}
		}
		t.Fatalf("copy %s sequence %02d not found in %#v", source, sequence, relations)
		return ""
	}

	for _, query := range []string{
		"CREATE DATABASE `navicat-workflow-source`",
		"USE `navicat-workflow-source`",
		"CREATE TABLE `items` (`id` INT NOT NULL, `label` VARCHAR(32) NOT NULL, `note` TEXT NULL, PRIMARY KEY (`id`), UNIQUE KEY `uq_label` (`label`), KEY `idx_note` (`note`))",
		"INSERT INTO `items` VALUES (1,'first',NULL),(2,'second',NULL),(3,'third','keep')",
		"CREATE VIEW `active-items` AS SELECT `id`,`label` FROM `items` WHERE `id` >= 1",
	} {
		exec(query)
	}
	updated := exec("UPDATE `items` SET `label`='edited' WHERE `id` <=> 1 AND `note` <=> NULL LIMIT 1")
	if affected, err := updated.RowsAffected(); err != nil || affected != 1 {
		t.Fatalf("Navicat UPDATE affected %d rows: %v", affected, err)
	}
	deleted := exec("DELETE FROM `items` WHERE `id` <=> 2 AND `note` <=> NULL LIMIT 1")
	if affected, err := deleted.RowsAffected(); err != nil || affected != 1 {
		t.Fatalf("Navicat DELETE affected %d rows: %v", affected, err)
	}
	var remaining int
	if err := connection.QueryRowContext(ctx, "SELECT COUNT(*) FROM `items`").Scan(&remaining); err != nil || remaining != 2 {
		t.Fatalf("rows after grid edits = %d, %v", remaining, err)
	}

	var tableName, tableDDL string
	if err := connection.QueryRowContext(ctx, "SHOW CREATE TABLE `items`").Scan(&tableName, &tableDDL); err != nil {
		t.Fatal(err)
	}
	if tableName != "items" || !strings.Contains(tableDDL, "UNIQUE KEY `uq_label`") || !strings.Contains(tableDDL, "KEY `idx_note`") {
		t.Fatalf("copy source DDL = %q %q", tableName, tableDDL)
	}
	firstDay := time.Now().Format("060102")
	exec("CREATE TABLE `items_copy` (`id` INT NOT NULL, `label` VARCHAR(32) NOT NULL, `note` TEXT NULL, PRIMARY KEY (`id`), UNIQUE KEY `uq_label` (`label`), KEY `idx_note` (`note`))")
	secondDay := time.Now().Format("060102")
	relations := listRelations("navicat-workflow-source")
	firstTableCopy := findCopy(relations, "items", "BASE TABLE", 1, firstDay, secondDay)
	if _, exposed := relations["items_copy"]; exposed {
		t.Fatalf("temporary items_copy was exposed: %#v", relations)
	}
	exec("INSERT INTO `items_copy` SELECT * FROM `items`")
	if err := client.QueryRowContext(ctx, "SELECT COUNT(*) FROM `navicat-workflow-source`.`"+firstTableCopy+"`").Scan(&remaining); err != nil || remaining != 2 {
		t.Fatalf("first copied table rows = %d, %v", remaining, err)
	}

	if err := connection.QueryRowContext(ctx, "SHOW CREATE TABLE `items`").Scan(&tableName, &tableDDL); err != nil {
		t.Fatal(err)
	}
	firstDay = time.Now().Format("060102")
	exec("CREATE TABLE `items_copy1` (`id` INT NOT NULL, `label` VARCHAR(32) NOT NULL, `note` TEXT NULL, PRIMARY KEY (`id`), UNIQUE KEY `uq_label` (`label`), KEY `idx_note` (`note`))")
	secondDay = time.Now().Format("060102")
	relations = listRelations("navicat-workflow-source")
	secondTableCopy := findCopy(relations, "items", "BASE TABLE", 2, firstDay, secondDay)
	if _, exposed := relations["items_copy1"]; exposed {
		t.Fatalf("temporary items_copy1 was exposed: %#v", relations)
	}
	exec("INSERT INTO `items_copy1` SELECT * FROM `items`")
	if err := client.QueryRowContext(ctx, "SELECT COUNT(*) FROM `navicat-workflow-source`.`"+secondTableCopy+"`").Scan(&remaining); err != nil || remaining != 2 {
		t.Fatalf("second copied table rows = %d, %v", remaining, err)
	}

	var viewName, viewDDL, characterSet, collation string
	if err := connection.QueryRowContext(ctx, "SHOW CREATE VIEW `active-items`").Scan(&viewName, &viewDDL, &characterSet, &collation); err != nil {
		t.Fatal(err)
	}
	firstDay = time.Now().Format("060102")
	exec("CREATE VIEW `active-items_copy` AS SELECT `id`,`label` FROM `items` WHERE `id` >= 1")
	secondDay = time.Now().Format("060102")
	relations = listRelations("navicat-workflow-source")
	viewCopy := findCopy(relations, "active-items", "VIEW", 1, firstDay, secondDay)
	if _, exposed := relations["active-items_copy"]; exposed {
		t.Fatalf("temporary view copy was exposed: %#v", relations)
	}
	if err := client.QueryRowContext(ctx, "SELECT COUNT(*) FROM `navicat-workflow-source`.`"+viewCopy+"`").Scan(&remaining); err != nil || remaining != 2 {
		t.Fatalf("copied view rows = %d, %v", remaining, err)
	}

	exec("CREATE DATABASE `navicat-workflow-target`")
	if err := connection.QueryRowContext(ctx, "SHOW CREATE TABLE `navicat-workflow-source`.`items`").Scan(&tableName, &tableDDL); err != nil {
		t.Fatal(err)
	}
	exec("CREATE TABLE `navicat-workflow-target`.`items` (`id` INT NOT NULL, `label` VARCHAR(32) NOT NULL, `note` TEXT NULL, PRIMARY KEY (`id`), UNIQUE KEY `uq_label` (`label`), KEY `idx_note` (`note`))")
	exec("INSERT INTO `navicat-workflow-target`.`items` SELECT * FROM `navicat-workflow-source`.`items`")
	exec("USE `navicat-workflow-target`")
	if err := connection.QueryRowContext(ctx, "SHOW CREATE TABLE `items`").Scan(&tableName, &tableDDL); err != nil {
		t.Fatal(err)
	}
	exec("DROP TABLE `items`")
	exec(tableDDL)
	exec("INSERT INTO `items` SELECT * FROM `navicat-workflow-source`.`items`")
	exec("CREATE VIEW `active-items` AS SELECT `id`,`label` FROM `items` WHERE `id` >= 1")
	if err := connection.QueryRowContext(ctx, "SHOW CREATE VIEW `active-items`").Scan(&viewName, &viewDDL, &characterSet, &collation); err != nil {
		t.Fatal(err)
	}
	exec("DROP VIEW `active-items`")
	exec(viewDDL)
	targetRelations := listRelations("navicat-workflow-target")
	if targetRelations["items"] != "BASE TABLE" || targetRelations["active-items"] != "VIEW" {
		t.Fatalf("transferred relations = %#v", targetRelations)
	}
	for name := range targetRelations {
		if strings.Contains(name, "_copy_") {
			t.Fatalf("GBaseLite transfer was misclassified as a copy: %#v", targetRelations)
		}
	}
	if err := client.QueryRowContext(ctx, "SELECT COUNT(*) FROM `navicat-workflow-target`.`active-items`").Scan(&remaining); err != nil || remaining != 2 {
		t.Fatalf("transferred view rows = %d, %v", remaining, err)
	}

	exec("USE `navicat-workflow-source`")
	statement, err := connection.PrepareContext(ctx, "SHOW FULL TABLES WHERE Table_type != ?")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := statement.QueryContext(ctx, "VIEW")
	if err != nil {
		t.Fatal(err)
	}
	baseTables := 0
	for rows.Next() {
		var name, relationType string
		if err := rows.Scan(&name, &relationType); err != nil {
			t.Fatal(err)
		}
		if relationType != "BASE TABLE" {
			t.Fatalf("export table enumeration returned %q %q", name, relationType)
		}
		baseTables++
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if err := statement.Close(); err != nil {
		t.Fatal(err)
	}
	if baseTables != 3 {
		t.Fatalf("export enumerated %d base tables, want 3", baseTables)
	}
}

func TestMySQLClientCRUD(t *testing.T) {
	engine, err := executor.Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	databaseServer := &MySQLServer{Engine: engine, Logger: log.New(io.Discard, "", 0)}
	done := make(chan error, 1)
	go func() { done <- databaseServer.Serve(listener) }()

	dsn := "root:123456@tcp(" + listener.Addr().String() + ")/?charset=utf8mb4&timeout=3s"
	client, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Ping(); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"CREATE DATABASE test", "USE test", "CREATE TABLE users(id INT,name VARCHAR(50),age INT)",
		"INSERT INTO users VALUES (1,'张三',20)", "UPDATE users SET age=21 WHERE id=1",
	} {
		if _, err := client.Exec(query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	var name string
	var age int
	if err := client.QueryRow("SELECT name,age FROM users WHERE id=1").Scan(&name, &age); err != nil {
		t.Fatal(err)
	}
	if name != "张三" || age != 21 {
		t.Fatalf("got (%s,%d)", name, age)
	}
	if _, err := client.Exec("DELETE FROM users WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"CREATE TABLE drop_batch_a(id INT)",
		"CREATE TABLE drop_batch_b(id INT)",
		"DROP TABLE IF EXISTS `test`.`drop_batch_a`, `test`.`missing`, `test`.`drop_batch_b`",
	} {
		if _, err := client.Exec(query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	for _, table := range []string{"drop_batch_a", "drop_batch_b"} {
		rows, queryErr := client.Query("SELECT * FROM `test`.`" + table + "`")
		if queryErr == nil {
			_ = rows.Close()
			t.Fatalf("table %s still exists", table)
		}
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := databaseServer.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRejectsWrongPassword(t *testing.T) {
	engine, err := executor.Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	databaseServer := &MySQLServer{Engine: engine, Logger: log.New(io.Discard, "", 0)}
	go databaseServer.Serve(listener)
	client, err := sql.Open("mysql", "root:wrong@tcp("+listener.Addr().String()+")/?timeout=2s")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Ping(); err == nil {
		t.Fatal("expected authentication failure")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = databaseServer.Shutdown(ctx)
}

func TestAuthenticationFailureThrottleBlocksRepeatedLogin(t *testing.T) {
	engine, err := executor.Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	audit, err := journal.OpenAudit(auditPath, journal.DefaultRetentionDays)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	databaseServer := &MySQLServer{
		Engine:            engine,
		Logger:            log.New(io.Discard, "", 0),
		Audit:             audit,
		AuthFailureLimit:  2,
		AuthFailureWindow: time.Minute,
		AuthFailureBlock:  2 * time.Second,
	}
	done := make(chan error, 1)
	go func() { done <- databaseServer.Serve(listener) }()
	address := listener.Addr().String()
	ping := func(password string) error {
		client, openErr := sql.Open("mysql", "root:"+password+"@tcp("+address+")/?timeout=2s")
		if openErr != nil {
			return openErr
		}
		client.SetMaxOpenConns(1)
		defer client.Close()
		return client.Ping()
	}
	if err := ping("wrong-one"); err == nil {
		t.Fatal("first invalid password was accepted")
	}
	if err := ping("wrong-two"); err == nil {
		t.Fatal("second invalid password was accepted")
	}
	if err := ping("123456"); err == nil {
		t.Fatal("correct password bypassed the active authentication block")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := databaseServer.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := audit.Close(); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(contents)
	if !strings.Contains(text, `"result":"blocked"`) || strings.Contains(text, "wrong-one") || strings.Contains(text, "wrong-two") {
		t.Fatalf("authentication throttle audit = %s", text)
	}
}

func TestAuthenticationFailureThrottleExpiresAndClears(t *testing.T) {
	server := &MySQLServer{AuthFailureLimit: 2, AuthFailureWindow: time.Minute, AuthFailureBlock: 30 * time.Second}
	started := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)
	server.recordAuthenticationFailure("127.0.0.1\x00root", started)
	server.recordAuthenticationFailure("127.0.0.1\x00root", started.Add(time.Second))
	if !server.authenticationBlocked("127.0.0.1\x00root", started.Add(2*time.Second)) {
		t.Fatal("authentication failure threshold did not block the account and IP")
	}
	if server.authenticationBlocked("127.0.0.1\x00root", started.Add(32*time.Second)) {
		t.Fatal("authentication block did not expire")
	}
	server.clearAuthenticationFailures("127.0.0.1\x00root")
	if server.authenticationBlocked("127.0.0.1\x00root", started.Add(33*time.Second)) {
		t.Fatal("successful authentication did not clear failures")
	}
}

func TestNavicatEmptyDatabaseMetadata(t *testing.T) {
	engine, err := executor.Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &executor.Session{}
	if _, err := engine.Execute(session, "CREATE DATABASE `111` CHARACTER SET 'utf8mb4' COLLATE 'utf8mb4_general_ci'"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Execute(session, "CREATE DATABASE `yuanma-auth`"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Execute(session, "USE `yuanma-auth`"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Execute(session, "CREATE TABLE `auth_code` (`id` varchar(64) NOT NULL, `use_info` tinytext NULL, `create_time` datetime NULL, PRIMARY KEY (`id`) USING BTREE) ENGINE=InnoDB CHARACTER SET=utf8mb4"); err != nil {
		t.Fatal(err)
	}
	queries := []string{
		"SELECT SCHEMA_NAME FROM `INFORMATION_SCHEMA`.`SCHEMATA`",
		"SELECT TABLE_SCHEMA, TABLE_NAME, COLUMN_NAME, COLUMN_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = '111' ORDER BY TABLE_SCHEMA, TABLE_NAME",
		"SELECT DISTINCT ROUTINE_SCHEMA, ROUTINE_NAME FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA = '111'",
		"SHOW PROCEDURE STATUS WHERE Db = 'yuanma-auth'",
		"SHOW FUNCTION STATUS WHERE Db = 'yuanma-auth'",
		"SHOW TRIGGERS FROM `yuanma-auth`",
		"SHOW EVENTS FROM `yuanma-auth`",
		"SELECT TABLE_NAME, CHECK_OPTION, IS_UPDATABLE, SECURITY_TYPE, DEFINER FROM information_schema.VIEWS WHERE TABLE_SCHEMA = '111'",
		"SHOW INDEX FROM `missing_table`",
		"ALTER TABLE `yuanma-auth`.`auth_code` ADD UNIQUE INDEX `auth_code_id`(`id`)",
		"SELECT TABLE_NAME, PARTITION_NAME, SUBPARTITION_NAME, PARTITION_METHOD, SUBPARTITION_METHOD, PARTITION_EXPRESSION, SUBPARTITION_EXPRESSION, PARTITION_DESCRIPTION, PARTITION_COMMENT, NODEGROUP, TABLESPACE_NAME FROM information_schema.PARTITIONS WHERE NOT ISNULL(PARTITION_NAME) AND TABLE_SCHEMA LIKE BINARY 'yuanma-auth' AND TABLE_NAME LIKE BINARY 'auth_code' ORDER BY TABLE_NAME, PARTITION_NAME, PARTITION_ORDINAL_POSITION, SUBPARTITION_ORDINAL_POSITION",
		"SELECT DISTINCT(TABLESPACE_NAME) AS TABLESPACE_NAME FROM information_schema.FILES WHERE NOT ISNULL(TABLESPACE_NAME) LIMIT 10000",
		"LOCK TABLES `auth_code` WRITE",
		"UNLOCK TABLES",
		"FLUSH TABLES",
		"SAVEPOINT dump_snapshot",
		"ROLLBACK TO SAVEPOINT dump_snapshot",
		"RELEASE SAVEPOINT dump_snapshot",
		"KILL 123",
	}
	for _, query := range queries {
		if _, err := ExecuteCompatible(engine, session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	indexes, err := ExecuteCompatible(engine, session, "SHOW INDEX FROM `auth_code` FROM `yuanma-auth`")
	if err != nil || len(indexes.Rows) != 2 || indexes.Rows[0][1] != int64(0) || indexes.Rows[1][1] != int64(0) {
		t.Fatalf("Navicat SHOW INDEX = %#v, %v", indexes, err)
	}
}

func TestMySQLIndexErrorCodes(t *testing.T) {
	if code := mysqlExecutionErrorCode(executor.ErrPersistenceUnavailable); code != 1030 {
		t.Fatalf("persistence failure code = %d, want 1030", code)
	}
	if code := mysqlExecutionErrorCode(fmt.Errorf("wrapped: %w", storage.ErrDuplicateKey)); code != 1062 {
		t.Fatalf("duplicate key code = %d", code)
	}
	if code := mysqlExecutionErrorCode(storage.ErrIndexExists); code != 1061 {
		t.Fatalf("duplicate index code = %d", code)
	}
	if code := mysqlExecutionErrorCode(storage.ErrIndexNotFound); code != 1091 {
		t.Fatalf("missing index code = %d", code)
	}
	if code := mysqlExecutionErrorCode(storage.ErrForeignKeyReferenced); code != 1451 {
		t.Fatalf("referenced parent code = %d, want 1451", code)
	}
}

func TestConstraintInformationSchemaMetadata(t *testing.T) {
	engine, err := executor.Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	session := &executor.Session{}
	for _, query := range []string{
		"CREATE DATABASE constraint_metadata_test",
		"USE constraint_metadata_test",
		"CREATE TABLE parents(id INT,PRIMARY KEY(id))",
		"CREATE TABLE children(id INT,parent_id INT,value INT,CONSTRAINT fk_parent FOREIGN KEY(parent_id) REFERENCES parents(id),CONSTRAINT ck_value CHECK(value >= 0))",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatal(err)
		}
	}
	tableConstraints, err := tableConstraintInformation(engine, session, "SELECT * FROM information_schema.TABLE_CONSTRAINTS WHERE TABLE_SCHEMA='constraint_metadata_test' AND TABLE_NAME='children'")
	if err != nil {
		t.Fatal(err)
	}
	foundForeign, foundCheck := false, false
	for _, row := range tableConstraints.Rows {
		foundForeign = foundForeign || row[2] == "fk_parent" && row[5] == "FOREIGN KEY"
		foundCheck = foundCheck || row[2] == "ck_value" && row[5] == "CHECK"
	}
	if !foundForeign || !foundCheck {
		t.Fatalf("TABLE_CONSTRAINTS rows = %#v", tableConstraints.Rows)
	}
	referential, err := referentialConstraintInformation(engine, session, "SELECT * FROM information_schema.REFERENTIAL_CONSTRAINTS WHERE CONSTRAINT_SCHEMA='constraint_metadata_test'")
	if err != nil || len(referential.Rows) != 1 || referential.Rows[0][1] != "fk_parent" || referential.Rows[0][6] != "RESTRICT" {
		t.Fatalf("REFERENTIAL_CONSTRAINTS rows = %#v, %v", referential.Rows, err)
	}
	checks, err := checkConstraintInformation(engine, session, "SELECT * FROM information_schema.CHECK_CONSTRAINTS WHERE CONSTRAINT_SCHEMA='constraint_metadata_test'")
	if err != nil || len(checks.Rows) != 1 || checks.Rows[0][1] != "ck_value" {
		t.Fatalf("CHECK_CONSTRAINTS rows = %#v, %v", checks.Rows, err)
	}
}

func TestCompatibilityQueriesFailClosedAfterPersistenceFailure(t *testing.T) {
	directory := t.TempDir()
	engine, err := executor.Open(directory, "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &executor.Session{CurrentDatabase: "failure_test"}
	if _, err := engine.Execute(session, "CREATE DATABASE failure_test"); err != nil {
		t.Fatal(err)
	}
	temporary := engine.Persistence.Path() + ".tmp"
	if err := os.Mkdir(temporary, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Execute(session, "CREATE TABLE items(id INT)"); !errors.Is(err, executor.ErrPersistenceUnavailable) {
		t.Fatalf("persistence failure = %v", err)
	}
	if err := os.Remove(temporary); err != nil {
		t.Fatal(err)
	}
	if _, err := ExecuteCompatible(engine, session, "SHOW DATABASES"); !errors.Is(err, executor.ErrPersistenceUnavailable) {
		t.Fatalf("compatibility query after persistence failure = %v", err)
	}
}

func TestNavicatEditableTableMetadata(t *testing.T) {
	engine, err := executor.Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &executor.Session{}
	for _, query := range []string{
		"CREATE DATABASE navicat_metadata",
		"USE navicat_metadata",
		"CREATE TABLE items(id INT NOT NULL COMMENT 'identifier',name VARCHAR(64) NULL DEFAULT NULL,PRIMARY KEY(id)) COMMENT='editable items'",
		"INSERT INTO items VALUES(1,'first')",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	columns, err := ExecuteCompatible(engine, session, "SELECT COLUMN_NAME,IS_NULLABLE,COLUMN_KEY,COLUMN_DEFAULT,EXTRA FROM information_schema.COLUMNS WHERE TABLE_SCHEMA='navicat_metadata' AND TABLE_NAME='items'")
	if err != nil || len(columns.Rows) != 2 || columns.Rows[0][1] != "NO" || columns.Rows[0][2] != "PRI" {
		t.Fatalf("column metadata = %#v, %v", columns, err)
	}
	constraints, err := ExecuteCompatible(engine, session, "SELECT CONSTRAINT_NAME,CONSTRAINT_TYPE FROM information_schema.TABLE_CONSTRAINTS WHERE TABLE_SCHEMA='navicat_metadata' AND TABLE_NAME='items'")
	if err != nil || len(constraints.Rows) != 1 || constraints.Rows[0][0] != "PRIMARY" || constraints.Rows[0][1] != "PRIMARY KEY" {
		t.Fatalf("constraint metadata = %#v, %v", constraints, err)
	}
	usage, err := ExecuteCompatible(engine, session, "SELECT CONSTRAINT_NAME,COLUMN_NAME,ORDINAL_POSITION FROM information_schema.KEY_COLUMN_USAGE WHERE TABLE_SCHEMA='navicat_metadata' AND TABLE_NAME='items'")
	if err != nil || len(usage.Rows) != 1 || usage.Rows[0][0] != "PRIMARY" || usage.Rows[0][1] != "id" {
		t.Fatalf("key usage metadata = %#v, %v", usage, err)
	}
	statistics, err := ExecuteCompatible(engine, session, "SELECT INDEX_NAME,NON_UNIQUE,COLUMN_NAME FROM information_schema.STATISTICS WHERE TABLE_SCHEMA='navicat_metadata' AND TABLE_NAME='items'")
	if err != nil || len(statistics.Rows) != 1 || statistics.Rows[0][0] != "PRIMARY" || statistics.Rows[0][1] != int64(0) {
		t.Fatalf("statistics metadata = %#v, %v", statistics, err)
	}
	status, err := ExecuteCompatible(engine, session, "SHOW TABLE STATUS FROM `navicat_metadata`")
	if err != nil || len(status.Rows) != 1 {
		t.Fatalf("table status = %#v, %v", status, err)
	}
	fullColumns, err := ExecuteCompatible(engine, &executor.Session{}, "SHOW FULL COLUMNS FROM `items` FROM `navicat_metadata`")
	if err != nil || len(fullColumns.Rows) != 2 || fullColumns.Rows[0][4] != "PRI" || fullColumns.Rows[0][8] != "identifier" {
		t.Fatalf("full columns = %#v, %v", fullColumns, err)
	}
	filteredColumns, err := ExecuteCompatible(engine, session, "SHOW FULL COLUMNS FROM items LIKE 'name'")
	if err != nil || len(filteredColumns.Rows) != 1 || filteredColumns.Rows[0][0] != "name" {
		t.Fatalf("SHOW FULL COLUMNS LIKE = %#v, %v", filteredColumns, err)
	}
	columnByName, err := ExecuteCompatible(engine, session, "SELECT COLUMN_NAME,COLUMN_COMMENT FROM information_schema.COLUMNS WHERE TABLE_SCHEMA='navicat_metadata' AND TABLE_NAME='items' AND COLUMN_NAME='name'")
	if err != nil || len(columnByName.Rows) != 1 || columnByName.Rows[0][0] != "name" {
		t.Fatalf("information_schema.COLUMNS COLUMN_NAME = %#v, %v", columnByName, err)
	}
	qualifiedColumns, err := ExecuteCompatible(engine, &executor.Session{}, "SHOW COLUMNS FROM `navicat_metadata`.`items`")
	if err != nil || len(qualifiedColumns.Rows) != 2 || qualifiedColumns.Rows[1][1] != "varchar(64)" {
		t.Fatalf("qualified columns = %#v, %v", qualifiedColumns, err)
	}
	status, err = ExecuteCompatible(engine, session, "SHOW TABLE STATUS FROM `navicat_metadata`")
	if err != nil || len(status.Columns) != 18 || status.Rows[0][6].(int64) <= 0 || status.Rows[0][11] == nil || status.Rows[0][12] == nil || status.Rows[0][17] != "editable items" {
		t.Fatalf("full table status = %#v, %v", status, err)
	}
	if status.Rows[0][11].(time.Time).Nanosecond() != 0 || status.Rows[0][12].(time.Time).Nanosecond() != 0 {
		t.Fatalf("table status timestamps must be second precision: %#v", status.Rows[0])
	}
	tableInfo, err := ExecuteCompatible(engine, session, "SELECT TABLE_NAME,DATA_LENGTH,CREATE_TIME,UPDATE_TIME,TABLE_COMMENT FROM information_schema.TABLES WHERE TABLE_SCHEMA='navicat_metadata'")
	if err != nil || len(tableInfo.Rows) != 1 || tableInfo.Rows[0][1].(int64) <= 0 || tableInfo.Rows[0][4] != "editable items" {
		t.Fatalf("table information = %#v, %v", tableInfo, err)
	}
}

func TestShowTableStatusLikeFiltersNavicatTargetLookup(t *testing.T) {
	engine, err := executor.Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &executor.Session{CurrentDatabase: "navicat_status_filter"}
	for _, query := range []string{
		"CREATE DATABASE navicat_status_filter",
		"CREATE TABLE auth_app(id INT NOT NULL, PRIMARY KEY(id))",
		"CREATE TABLE auth_code(id INT NOT NULL, PRIMARY KEY(id))",
		"CREATE TABLE auth_code_equ(id INT NOT NULL, PRIMARY KEY(id))",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}

	exact, err := ExecuteCompatible(engine, session, "SHOW TABLE STATUS FROM `navicat_status_filter` LIKE 'auth_app'")
	if err != nil || len(exact.Rows) != 1 || exact.Rows[0][0] != "auth_app" {
		t.Fatalf("exact table status = %#v, %v", exact, err)
	}
	wildcard, err := ExecuteCompatible(engine, session, "SHOW TABLE STATUS LIKE 'auth_code%'")
	if err != nil || len(wildcard.Rows) != 2 || wildcard.Rows[0][0] != "auth_code" || wildcard.Rows[1][0] != "auth_code_equ" {
		t.Fatalf("wildcard table status = %#v, %v", wildcard, err)
	}
	missing, err := ExecuteCompatible(engine, session, "SHOW TABLE STATUS IN navicat_status_filter LIKE 'missing'")
	if err != nil || len(missing.Rows) != 0 {
		t.Fatalf("missing table status = %#v, %v", missing, err)
	}
}

func TestNavicatDatabaseColumnsIncludeLegacyViewWithDuplicateSourceNames(t *testing.T) {
	engine, err := executor.Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &executor.Session{CurrentDatabase: "metadata_views"}
	for _, query := range []string{
		"CREATE DATABASE metadata_views",
		"CREATE TABLE left_items(id INT,name VARCHAR(16))",
		"CREATE TABLE right_items(id INT,left_id INT)",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	database, err := engine.Store.Database("metadata_views")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CreateView("legacy_join", "SELECT * FROM left_items t1 LEFT JOIN right_items t2 ON t1.id=t2.left_id", nil, false); err != nil {
		t.Fatal(err)
	}
	columns, err := ExecuteCompatible(engine, session, "SELECT TABLE_SCHEMA,TABLE_NAME,COLUMN_NAME,COLUMN_TYPE,COLUMN_COMMENT FROM information_schema.COLUMNS WHERE TABLE_SCHEMA='metadata_views' ORDER BY TABLE_SCHEMA,TABLE_NAME")
	if err != nil || len(columns.Rows) != 8 || columns.Rows[0][0] != "metadata_views" {
		t.Fatalf("database columns = %#v, %v", columns, err)
	}
	viewID2 := false
	for _, row := range columns.Rows {
		if row[1] == "legacy_join" && row[2] == "id_2" {
			viewID2 = true
		}
	}
	if !viewID2 {
		t.Fatalf("legacy view duplicate column was not disambiguated: %#v", columns.Rows)
	}
}

func TestNavicatViewMetadata(t *testing.T) {
	engine, err := executor.Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &executor.Session{CurrentDatabase: "view_metadata"}
	for _, query := range []string{
		"CREATE DATABASE view_metadata",
		"CREATE TABLE view_metadata.users(id INT,name VARCHAR(32))",
		"CREATE VIEW view_metadata.user_names AS SELECT id,name FROM users",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	full, err := ExecuteCompatible(engine, session, "SHOW FULL TABLES")
	if err != nil || len(full.Columns) != 2 || len(full.Rows) != 2 {
		t.Fatalf("SHOW FULL TABLES = %#v, %v", full, err)
	}
	views, err := ExecuteCompatible(engine, session, "SELECT TABLE_NAME,CHECK_OPTION,IS_UPDATABLE FROM information_schema.VIEWS WHERE TABLE_SCHEMA='view_metadata'")
	if err != nil || len(views.Columns) != 3 || len(views.Rows) != 1 || views.Rows[0][0] != "user_names" {
		t.Fatalf("information_schema.VIEWS = %#v, %v", views, err)
	}
	tables, err := ExecuteCompatible(engine, session, "SELECT TABLE_SCHEMA,TABLE_NAME,TABLE_TYPE FROM information_schema.TABLES WHERE TABLE_SCHEMA='view_metadata' AND TABLE_TYPE='BASE TABLE'")
	if err != nil || len(tables.Columns) != 3 || len(tables.Rows) != 1 || tables.Rows[0][0] != "view_metadata" || tables.Rows[0][1] != "users" || tables.Rows[0][2] != "BASE TABLE" {
		t.Fatalf("base table metadata = %#v, %v", tables, err)
	}
	viewTables, err := ExecuteCompatible(engine, session, "SELECT TABLE_SCHEMA,TABLE_NAME,TABLE_TYPE FROM information_schema.TABLES WHERE TABLE_SCHEMA='view_metadata' AND TABLE_TYPE='VIEW'")
	if err != nil || len(viewTables.Rows) != 1 || viewTables.Rows[0][1] != "user_names" || viewTables.Rows[0][2] != "VIEW" {
		t.Fatalf("view table metadata = %#v, %v", viewTables, err)
	}
	baseOnly, err := ExecuteCompatible(engine, session, "SHOW FULL TABLES FROM `view_metadata` WHERE Table_type = 'BASE TABLE'")
	if err != nil || len(baseOnly.Rows) != 1 || baseOnly.Rows[0][0] != "users" || baseOnly.Rows[0][1] != "BASE TABLE" {
		t.Fatalf("SHOW FULL TABLES base table filter = %#v, %v", baseOnly, err)
	}
	viewsOnly, err := ExecuteCompatible(engine, session, "SHOW FULL TABLES IN view_metadata WHERE `Table_type` = 'VIEW'")
	if err != nil || len(viewsOnly.Rows) != 1 || viewsOnly.Rows[0][0] != "user_names" || viewsOnly.Rows[0][1] != "VIEW" {
		t.Fatalf("SHOW FULL TABLES view filter = %#v, %v", viewsOnly, err)
	}
	viewStatus, err := ExecuteCompatible(engine, session, "SHOW TABLE STATUS LIKE 'user_names'")
	if err != nil || len(viewStatus.Rows) != 1 || viewStatus.Rows[0][0] != "user_names" || viewStatus.Rows[0][1] != nil || viewStatus.Rows[0][17] != "VIEW" {
		t.Fatalf("SHOW TABLE STATUS view row = %#v, %v", viewStatus, err)
	}
	relations, err := ExecuteCompatible(engine, &executor.Session{}, "SHOW TABLES FROM `view_metadata` LIKE 'user%'")
	if err != nil || len(relations.Rows) != 2 || relations.Rows[0][0] != "user_names" || relations.Rows[1][0] != "users" {
		t.Fatalf("SHOW TABLES database and LIKE filter = %#v, %v", relations, err)
	}
	columns, err := ExecuteCompatible(engine, session, "SELECT COLUMN_NAME,DATA_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA='view_metadata' AND TABLE_NAME='user_names'")
	if err != nil || len(columns.Rows) != 2 || columns.Rows[1][0] != "name" {
		t.Fatalf("view columns = %#v, %v", columns, err)
	}
	source, err := ExecuteCompatible(engine, session, "SELECT CONVERT(load_file(concat(@@datadir, 'view_metadata', '/', 'user_names', '.frm')) USING utf8) AS source")
	if err != nil || len(source.Rows) != 1 || !strings.Contains(source.Rows[0][0].(string), "query=SELECT id,name FROM users") {
		t.Fatalf("Navicat view source = %#v, %v", source, err)
	}
}

func TestMySQLProtocolUserPrivileges(t *testing.T) {
	engine, err := executor.Open(t.TempDir(), "root", "root-secret")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	databaseServer := &MySQLServer{Engine: engine, Logger: log.New(io.Discard, "", 0)}
	done := make(chan error, 1)
	go func() { done <- databaseServer.Serve(listener) }()
	address := listener.Addr().String()
	root, err := sql.Open("mysql", "root:root-secret@tcp("+address+")/?charset=utf8mb4&timeout=3s")
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"CREATE DATABASE privilege_protocol",
		"CREATE TABLE privilege_protocol.visible_records(id INT,label VARCHAR(32))",
		"CREATE TABLE privilege_protocol.hidden_records(id INT)",
		"INSERT INTO privilege_protocol.visible_records VALUES (1,'visible')",
		"CREATE USER 'protocol_reader'@'127.0.0.1' IDENTIFIED BY 'reader-secret'",
		"GRANT SELECT ON privilege_protocol.visible_records TO 'protocol_reader'@'127.0.0.1'",
	} {
		if _, err := root.Exec(query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	reader, err := sql.Open("mysql", "protocol_reader:reader-secret@tcp("+address+")/privilege_protocol?charset=utf8mb4&timeout=3s")
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Ping(); err != nil {
		t.Fatal(err)
	}
	var label string
	if err := reader.QueryRow("SELECT label FROM visible_records WHERE id=1").Scan(&label); err != nil || label != "visible" {
		t.Fatalf("authorized protocol SELECT = %q, %v", label, err)
	}
	if _, err := reader.Exec("SELECT * FROM hidden_records"); err == nil {
		t.Fatal("expected hidden table SELECT to be denied")
	}
	if _, err := reader.Exec("INSERT INTO visible_records VALUES (2,'denied')"); err == nil {
		t.Fatal("expected INSERT to be denied")
	}
	accountRows, err := root.Query("SELECT User,Host FROM mysql.user")
	if err != nil {
		t.Fatal(err)
	}
	foundReader := false
	for accountRows.Next() {
		var username, host string
		if err := accountRows.Scan(&username, &host); err != nil {
			t.Fatal(err)
		}
		if username == "protocol_reader" && host == "127.0.0.1" {
			foundReader = true
		}
	}
	_ = accountRows.Close()
	if !foundReader {
		t.Fatal("mysql.user did not expose the created account to root")
	}
	if _, err := reader.Exec("SELECT User,Host FROM mysql.user"); err == nil {
		t.Fatal("restricted account could read mysql.user")
	}
	privilegeRows, err := reader.Query("SELECT GRANTEE,TABLE_SCHEMA,TABLE_NAME,PRIVILEGE_TYPE FROM information_schema.TABLE_PRIVILEGES")
	if err != nil {
		t.Fatal(err)
	}
	var grantee, schema, table, privilege string
	if !privilegeRows.Next() || privilegeRows.Scan(&grantee, &schema, &table, &privilege) != nil {
		t.Fatal("table privilege metadata did not return the reader grant")
	}
	_ = privilegeRows.Close()
	if schema != "privilege_protocol" || table != "visible_records" || privilege != "SELECT" || !strings.Contains(grantee, "protocol_reader") {
		t.Fatalf("unexpected table privilege metadata: %q %q %q %q", grantee, schema, table, privilege)
	}
	rows, err := reader.Query("SHOW TABLES")
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, table)
	}
	_ = rows.Close()
	if len(tables) != 1 || tables[0] != "visible_records" {
		t.Fatalf("filtered SHOW TABLES = %#v", tables)
	}
	if _, err := root.Exec("GRANT INSERT ON privilege_protocol.visible_records TO 'protocol_reader'@'127.0.0.1'"); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Exec("INSERT INTO visible_records VALUES (2,'allowed')"); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Exec("REVOKE SELECT ON privilege_protocol.visible_records FROM 'protocol_reader'@'127.0.0.1'"); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Exec("SELECT * FROM visible_records"); err == nil {
		t.Fatal("revoked protocol SELECT remained active")
	}
	_ = reader.Close()
	_ = root.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := databaseServer.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
