package executor

import (
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"sort"
	"testing"
)

const (
	crashRecoveryHelperEnvironment = "GBASELITE_CRASH_RECOVERY_HELPER"
	crashRecoveryDataEnvironment   = "GBASELITE_CRASH_RECOVERY_DATA"
)

func TestAcknowledgedWritesSurviveAbruptProcessExit(t *testing.T) {
	if os.Getenv(crashRecoveryHelperEnvironment) == "1" {
		if err := writeCrashRecoveryFixture(os.Getenv(crashRecoveryDataEnvironment)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		os.Exit(0)
	}

	directory := t.TempDir()
	command := exec.Command(os.Args[0], "-test.run=^TestAcknowledgedWritesSurviveAbruptProcessExit$")
	command.Env = append(os.Environ(),
		crashRecoveryHelperEnvironment+"=1",
		crashRecoveryDataEnvironment+"="+directory,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("abrupt-exit helper: %v\n%s", err, output)
	}

	engine, err := Open(directory, "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	session := &Session{CurrentDatabase: "crash_recovery"}
	result, err := engine.Execute(session, "SELECT id,label FROM items ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 2 || result.Rows[0][0] != int64(1) || result.Rows[0][1] != "one" || result.Rows[1][0] != int64(2) || result.Rows[1][1] != "acknowledged" {
		t.Fatalf("recovered rows = %#v", result.Rows)
	}
	indexes, err := engine.Execute(session, "SHOW INDEX FROM items")
	if err != nil || len(indexes.Rows) != 2 {
		t.Fatalf("recovered indexes = %#v, %v", indexes, err)
	}
	view, err := engine.Execute(session, "SELECT COUNT(*) FROM active_items")
	if err != nil || len(view.Rows) != 1 || view.Rows[0][0] != int64(1) {
		t.Fatalf("recovered view = %#v, %v", view, err)
	}
}

func writeCrashRecoveryFixture(directory string) error {
	engine, err := Open(directory, "root", "123456")
	if err != nil {
		return err
	}
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE crash_recovery",
		"USE crash_recovery",
		"CREATE TABLE items(id INT NOT NULL,label VARCHAR(32),PRIMARY KEY(id),KEY idx_label(label))",
		"INSERT INTO items VALUES(1,'one'),(2,'two')",
		"UPDATE items SET label='acknowledged' WHERE id=2",
		"CREATE VIEW active_items AS SELECT id,label FROM items WHERE id >= 2",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			return fmt.Errorf("%s: %w", query, err)
		}
	}
	return nil
}

func TestDeterministicRandomWritesMatchModelAcrossRestarts(t *testing.T) {
	directory := t.TempDir()
	engine, err := Open(directory, "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE random_recovery",
		"USE random_recovery",
		"CREATE TABLE items(id INT NOT NULL,value VARCHAR(32),PRIMARY KEY(id))",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}

	random := rand.New(rand.NewSource(20260731))
	model := make(map[int]string)
	for iteration := 0; iteration < 250; iteration++ {
		id := random.Intn(40)
		switch random.Intn(3) {
		case 0:
			if _, exists := model[id]; exists {
				result, err := engine.Execute(session, fmt.Sprintf("INSERT IGNORE INTO items VALUES(%d,'duplicate-%d')", id, iteration))
				if err != nil || result.AffectedRows != 0 {
					t.Fatalf("duplicate insert %d = %#v, %v", iteration, result, err)
				}
				break
			}
			value := fmt.Sprintf("insert-%d", iteration)
			result, err := engine.Execute(session, fmt.Sprintf("INSERT INTO items VALUES(%d,'%s')", id, value))
			if err != nil || result.AffectedRows != 1 {
				t.Fatalf("insert %d = %#v, %v", iteration, result, err)
			}
			model[id] = value
		case 1:
			value := fmt.Sprintf("update-%d", iteration)
			result, err := engine.Execute(session, fmt.Sprintf("UPDATE items SET value='%s' WHERE id=%d", value, id))
			if err != nil {
				t.Fatalf("update %d: %v", iteration, err)
			}
			if _, exists := model[id]; exists {
				if result.AffectedRows != 1 {
					t.Fatalf("update %d affected %d rows", iteration, result.AffectedRows)
				}
				model[id] = value
			} else if result.AffectedRows != 0 {
				t.Fatalf("missing-row update %d affected %d rows", iteration, result.AffectedRows)
			}
		case 2:
			result, err := engine.Execute(session, fmt.Sprintf("DELETE FROM items WHERE id=%d", id))
			if err != nil {
				t.Fatalf("delete %d: %v", iteration, err)
			}
			if _, exists := model[id]; exists {
				if result.AffectedRows != 1 {
					t.Fatalf("delete %d affected %d rows", iteration, result.AffectedRows)
				}
				delete(model, id)
			} else if result.AffectedRows != 0 {
				t.Fatalf("missing-row delete %d affected %d rows", iteration, result.AffectedRows)
			}
		}

		if (iteration+1)%25 == 0 {
			assertRowsMatchModel(t, engine, session, model)
			if err := engine.Close(); err != nil {
				t.Fatal(err)
			}
			engine, err = Open(directory, "root", "123456")
			if err != nil {
				t.Fatal(err)
			}
			session = &Session{CurrentDatabase: "random_recovery"}
			assertRowsMatchModel(t, engine, session, model)
		}
	}
	assertRowsMatchModel(t, engine, session, model)
}

func assertRowsMatchModel(t testing.TB, engine *Engine, session *Session, model map[int]string) {
	t.Helper()
	result, err := engine.Execute(session, "SELECT id,value FROM items ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int, 0, len(model))
	for id := range model {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	if len(result.Rows) != len(ids) {
		t.Fatalf("rows = %#v, model = %#v", result.Rows, model)
	}
	for index, id := range ids {
		if result.Rows[index][0] != int64(id) || result.Rows[index][1] != model[id] {
			t.Fatalf("row %d = %#v, want %d %q", index, result.Rows[index], id, model[id])
		}
	}
}
