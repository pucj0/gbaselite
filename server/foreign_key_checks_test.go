package server

import (
	"fmt"
	"gbaselite/executor"
	"sync"
	"testing"
)

func TestParallelForeignKeyImport(t *testing.T) {
	for _, mode := range []string{"", "mvcc"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			e, err := executor.OpenWithOptions(dir, "root", "secret", executor.OpenOptions{StorageMode: mode})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { e.Close() }()
			base := &executor.Session{CurrentDatabase: "tew"}
			run := func(s *executor.Session, q string) {
				t.Helper()
				if _, err := ExecuteCompatible(e, s, q); err != nil {
					t.Fatalf("%s: %v", q, err)
				}
			}
			run(base, "CREATE DATABASE tew")
			var wg sync.WaitGroup
			failures := make(chan error, 8)
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					s := &executor.Session{CurrentDatabase: "tew"}
					defer e.CloseSession(s)
					for _, q := range []string{"SET SESSION foreign_key_checks=0", fmt.Sprintf("CREATE TABLE audit_%d(id BIGINT PRIMARY KEY,actor_id BIGINT NOT NULL, CONSTRAINT fk_actor FOREIGN KEY(actor_id) REFERENCES tew.users(id))", i), fmt.Sprintf("INSERT INTO audit_%d VALUES(1,42)", i), "SET foreign_key_checks=1"} {
						if _, err := ExecuteCompatible(e, s, q); err != nil {
							failures <- fmt.Errorf("%s: %w", q, err)
							return
						}
					}
				}(i)
			}
			wg.Wait()
			close(failures)
			for err := range failures {
				t.Error(err)
			}
			if t.Failed() {
				return
			}
			if base.ForeignKeyChecksDisabled {
				t.Fatal("setting leaked")
			}
			if _, err := ExecuteCompatible(e, base, "INSERT INTO audit_0 VALUES(2,99)"); err == nil {
				t.Fatal("default checks disabled")
			}
			run(base, "CREATE TABLE users(id BIGINT PRIMARY KEY)")
			run(base, "INSERT INTO users VALUES(42)")
			if _, err := ExecuteCompatible(e, base, "DELETE FROM users WHERE id=42"); err == nil {
				t.Fatal("missing reverse dependency")
			}
			run(base, "SET @saved=@@foreign_key_checks, foreign_key_checks=0")
			run(base, "UPDATE audit_0 SET actor_id=99 WHERE id=1")
			run(base, "SET foreign_key_checks=@saved")
			if base.ForeignKeyChecksDisabled {
				t.Fatal("restore failed")
			}
			run(base, "INSERT INTO users VALUES(43)")
			run(base, "INSERT INTO audit_0 VALUES(2,42)")
			run(base, "UPDATE audit_0 SET actor_id=43 WHERE id=2") // Do not rescan the old orphan row.
			if _, err := ExecuteCompatible(e, base, "DROP TABLE users"); err == nil {
				t.Fatal("enabled checks allowed dropping a referenced parent")
			}
			run(base, "SET foreign_key_checks=0")
			run(base, "BEGIN")
			run(base, "DELETE FROM users WHERE id=42")
			run(base, "ROLLBACK")
			e.ResetConnection(base)
			if base.ForeignKeyChecksDisabled {
				t.Fatal("reset retained setting")
			}
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			e, err = executor.OpenWithOptions(dir, "root", "secret", executor.OpenOptions{StorageMode: mode})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ExecuteCompatible(e, base, "INSERT INTO audit_1 VALUES(3,77)"); err == nil {
				t.Fatal("restart lost constraint")
			}
			if _, err := ExecuteCompatible(e, base, "DELETE FROM users WHERE id=42"); err == nil {
				t.Fatal("restart lost reverse dependencies")
			}
			run(base, "SET foreign_key_checks=0")
			run(base, "TRUNCATE TABLE users")
			run(base, "DROP TABLE users")
		})
	}
}
