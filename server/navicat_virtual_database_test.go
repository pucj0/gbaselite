package server

import (
	"context"
	"database/sql"
	"io"
	"log"
	"net"
	"testing"
	"time"

	"gbaselite/executor"
)

func TestNavicatUseVirtualDatabase(t *testing.T) {
	for _, mode := range []string{"", "mvcc"} {
		t.Run("mode="+mode, func(t *testing.T) {
			e, err := openTestEngineWithOptions(t, t.TempDir(), "root", "secret", executor.OpenOptions{StorageMode: mode})
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			srv := &MySQLServer{Engine: e, Logger: log.New(io.Discard, "", 0)}
			done := make(chan error, 1)
			go func() { done <- srv.Serve(listener) }()
			db, err := sql.Open("mysql", "root:secret@tcp("+listener.Addr().String()+")/information_schema?timeout=3s")
			if err != nil {
				t.Fatal(err)
			}
			db.SetMaxOpenConns(1)
			defer func() {
				db.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				srv.Shutdown(ctx)
				<-done
			}()
			// Exercise SET and variable reads over actual TCP; failures must be atomic.
			if _, err := db.Exec("SET @old_fk=@@foreign_key_checks, SESSION foreign_key_checks=0"); err != nil {
				t.Fatal(err)
			}
			var fkState int
			if err := db.QueryRow("SELECT @@session.foreign_key_checks").Scan(&fkState); err != nil || fkState != 0 {
				t.Fatalf("foreign checks=%d err=%v", fkState, err)
			}
			if _, err := db.Exec("SET foreign_key_checks=1, foreign_key_checks=2"); err == nil {
				t.Fatal("invalid setting accepted")
			}
			if err := db.QueryRow("SELECT @@foreign_key_checks").Scan(&fkState); err != nil || fkState != 0 {
				t.Fatal("failed SET changed state")
			}
			if _, err := db.Exec("SET foreign_key_checks=@old_fk"); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow("SELECT @@foreign_key_checks").Scan(&fkState); err != nil || fkState != 1 {
				t.Fatal("saved state not restored")
			}
			// Navicat retries this probe as separate statements after its initial
			// multi-statement request is rejected. Verify both wire protocols.
			for _, engineName := range []string{"NDBCLUSTER", "ndbcluster", "InnoDB", "GBaseLite"} {
				want := 0
				if engineName == "GBaseLite" {
					want = 1
				}
				var count int
				query := "SELECT COUNT(*) AS support_ndb FROM information_schema.ENGINES WHERE Engine = ?"
				if err := db.QueryRow(query, engineName).Scan(&count); err != nil || count != want {
					t.Fatalf("prepared engine probe %s: count=%d err=%v", engineName, count, err)
				}
				if err := db.QueryRow("SELECT COUNT(*) AS support_ndb FROM information_schema.ENGINES WHERE Engine = '" + engineName + "'").Scan(&count); err != nil || count != want {
					t.Fatalf("text engine probe %s: count=%d err=%v", engineName, count, err)
				}
			}
			for _, query := range []string{"CREATE DATABASE navicat_visible", "CREATE DATABASE navicat_hidden", "CREATE TABLE navicat_visible.items(id INT)", "CREATE TABLE navicat_hidden.secrets(id INT)"} {
				if _, err = db.Exec(query); err != nil {
					t.Fatalf("setup %s: %v", query, err)
				}
			}
			// Provision through the catalog: MVCC SQL account DDL is not supported.
			if _, err = e.Users.CreateAccount("navicat_reader", "%", "reader_secret", false); err != nil {
				t.Fatal(err)
			}
			if err = e.Users.GrantPrivileges("navicat_reader", "%", []string{"SELECT"}, "navicat_visible", "*", false); err != nil {
				t.Fatal(err)
			}
			for _, query := range []string{"USE information_schema", "use `INFORMATION_SCHEMA` ;", "/* Navicat */ USE\n`information_schema`", "USE mysql", "USE `information_schema`"} {
				if _, err = db.Exec(query); err != nil {
					t.Fatalf("%s: %v", query, err)
				}
			}
			var name string
			if err = db.QueryRow("SELECT DATABASE()").Scan(&name); err != nil || name != "information_schema" {
				t.Fatalf("database %q: %v", name, err)
			}
			for _, query := range []string{"SHOW FULL COLUMNS FROM information_schema.TABLES", "SHOW INDEX FROM information_schema.TABLES", "SELECT * FROM information_schema.CHARACTER_SETS", "SELECT * FROM information_schema.COLLATIONS", "SELECT SCHEMA_NAME FROM information_schema.SCHEMATA", "SELECT TABLE_SCHEMA,TABLE_NAME FROM information_schema.TABLES", "SELECT COLUMN_NAME FROM information_schema.COLUMNS", "SHOW TABLES", "SHOW FULL TABLES FROM information_schema", "SHOW TABLE STATUS FROM information_schema", "SELECT TABLE_NAME FROM information_schema.VIEWS", "SELECT TABLE_NAME FROM information_schema.STATISTICS", "SELECT TABLE_NAME FROM TABLES", "SELECT TABLE_NAME FROM\ninformation_schema.TABLES", "SELECT TABLE_NAME FROM information_schema.TABLE_CONSTRAINTS", "SELECT TABLE_NAME FROM information_schema.KEY_COLUMN_USAGE", "SELECT CONSTRAINT_NAME FROM information_schema.CHECK_CONSTRAINTS", "SELECT CONSTRAINT_NAME FROM information_schema.REFERENTIAL_CONSTRAINTS"} {
				rows, err := db.Query(query)
				if err != nil {
					t.Fatalf("%s: %v", query, err)
				}
				rows.Close()
			}
			for _, query := range []string{"USE missing_navicat_database", "USE information_schema garbage", "USE information_schema; DROP DATABASE mysql"} {
				if _, err = db.Exec(query); err == nil {
					t.Fatalf("accepted invalid USE: %s", query)
				}
			}
			if err = db.QueryRow("SELECT DATABASE()").Scan(&name); err != nil || name != "information_schema" {
				t.Fatalf("failed USE changed database %q: %v", name, err)
			}
			reader, err := sql.Open("mysql", "navicat_reader:reader_secret@tcp("+listener.Addr().String()+")/information_schema?timeout=3s")
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			reader.SetMaxOpenConns(1)
			if _, err = reader.Exec("USE information_schema"); err != nil {
				t.Fatal(err)
			}
			for _, query := range []string{"SELECT TABLE_SCHEMA,TABLE_NAME FROM information_schema.TABLES", "SELECT TABLE_SCHEMA,TABLE_NAME FROM information_schema.COLUMNS", "SELECT TABLE_SCHEMA,TABLE_NAME FROM information_schema.COLUMNS WHERE TABLE_NAME='items'", "SELECT TABLE_SCHEMA,TABLE_NAME FROM TABLES", "SELECT TABLE_SCHEMA,TABLE_NAME FROM\ninformation_schema.TABLES"} {
				rows, err := reader.Query(query)
				if err != nil {
					t.Fatal(err)
				}
				count := 0
				for rows.Next() {
					var schema, table string
					if err := rows.Scan(&schema, &table); err != nil {
						t.Fatal(err)
					}
					if schema != "navicat_visible" || table != "items" {
						t.Fatalf("metadata leaked %s.%s", schema, table)
					}
					count++
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				rows.Close()
				if count != 1 {
					t.Fatalf("visible metadata rows=%d", count)
				}
			}
			if _, err = reader.Exec("USE navicat_hidden"); err == nil {
				t.Fatal("unauthorized USE accepted")
			}
			if _, err = e.Store.Database("information_schema"); err == nil {
				t.Fatal("virtual schema persisted as real database")
			}
		})
	}
}
