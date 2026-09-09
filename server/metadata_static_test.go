package server

import (
	"gbaselite/executor"
	"testing"
)

func TestNavicatStaticMetadataAndDescriptions(t *testing.T) {
	for _, mode := range []string{"", "mvcc"} {
		t.Run("mode="+mode, func(t *testing.T) {
			engine, err := openTestEngineWithOptions(t, t.TempDir(), "root", "secret", executor.OpenOptions{StorageMode: mode})
			if err != nil {
				t.Fatal(err)
			}
			defer engine.Close()
			session := &executor.Session{CurrentDatabase: "information_schema", Username: "root", Host: "%"}
			cases := []struct {
				query         string
				columns, rows int
			}{
				{"SELECT CHARACTER_SET_NAME,MAXLEN FROM information_schema.CHARACTER_SETS WHERE CHARACTER_SET_NAME='utf8mb4'", 2, 1},
				{"SELECT COLLATION_NAME FROM COLLATIONS WHERE CHARACTER_SET_NAME='utf8mb4' ORDER BY ID LIMIT 2", 1, 2},
				{"SELECT c.ID AS collation_id FROM information_schema.COLLATIONS c WHERE c.COLLATION_NAME='utf8mb4_general_ci'", 1, 1},
				{"SELECT * FROM CHARACTER_SETS WHERE CHARACTER_SET_NAME='not_a_charset'", 4, 0},
				{"SHOW FULL COLUMNS FROM information_schema.TABLES", 9, 21},
				{"SHOW COLUMNS FROM `COLUMNS` FROM `information_schema` LIKE 'TABLE_NAME'", 6, 1},
				{"SHOW COLUMNS FROM CHARACTER_SETS WHERE Field='MAXLEN'", 6, 1},
				{"SHOW INDEX FROM information_schema.TABLES", 13, 0},
				{"DESCRIBE information_schema.CHARACTER_SETS", 6, 4},
			}
			for _, tc := range cases {
				result, err := ExecuteCompatible(engine, session, tc.query)
				if err != nil {
					t.Errorf("%s: %v", tc.query, err)
					continue
				}
				if len(result.Columns) != tc.columns || len(result.Rows) != tc.rows {
					t.Errorf("%s: columns=%d rows=%d; want %d/%d", tc.query, len(result.Columns), len(result.Rows), tc.columns, tc.rows)
				}
			}
			if session.CurrentDatabase != "information_schema" {
				t.Fatal("changed current database")
			}
			if _, err := engine.Store.Database("information_schema"); err == nil {
				t.Fatal("persisted virtual schema")
			}
			if _, err := ExecuteCompatible(engine, session, "SHOW COLUMNS FROM information_schema.not_implemented"); err == nil {
				t.Fatal("unknown system table accepted")
			}
		})
	}
}
