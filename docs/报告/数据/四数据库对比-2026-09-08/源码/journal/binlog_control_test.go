package journal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestBinlogControlValidation(t *testing.T) {
	good := AutoIncrementState{Database: "db", Table: "t", Column: "id", Next: 3}
	for _, record := range []BinlogRecord{
		{Version: 1, Statements: []BinlogStatement{{AutoIncrement: []AutoIncrementState{good}}}},
		{Version: 2, Statements: []BinlogStatement{{SQL: "SELECT 1", AutoIncrement: []AutoIncrementState{good}}}},
		{Version: 2, Statements: []BinlogStatement{{AutoIncrement: []AutoIncrementState{{Database: "db", Table: "t", Column: "id", Next: 0}}}}},
	} {
		record.Sequence = 1
		content, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "bad.jsonl")
		if err := os.WriteFile(path, content, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LastBinlogSequence(path); err == nil {
			t.Fatal("invalid log accepted at startup")
		}
		if _, _, err := ReadBinlog(path, ReplayOptions{}, func(BinlogRecord) error { t.Fatal("invalid log applied"); return nil }); err == nil {
			t.Fatal("invalid log accepted at replay")
		}
	}
}
