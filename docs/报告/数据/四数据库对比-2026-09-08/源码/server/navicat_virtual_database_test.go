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
			e, err := executor.OpenWithOptions(t.TempDir(), "root", "secret", executor.OpenOptions{StorageMode: mode})
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
			for _, query := range []string{"SELECT SCHEMA_NAME FROM information_schema.SCHEMATA", "SELECT TABLE_SCHEMA,TABLE_NAME FROM information_schema.TABLES", "SELECT COLUMN_NAME FROM information_schema.COLUMNS"} {
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
			for _, query := range []string{"SELECT TABLE_SCHEMA,TABLE_NAME FROM information_schema.TABLES", "SELECT TABLE_SCHEMA,TABLE_NAME FROM information_schema.COLUMNS", "SELECT TABLE_SCHEMA,TABLE_NAME FROM information_schema.COLUMNS WHERE TABLE_NAME='items'"} {
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
