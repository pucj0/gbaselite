package executor

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// indexCommentScript pins the MySQL index comment (index_option COMMENT 'text')
// that Navicat and mysqldump emit for every dumped index: both runtimes accept it,
// keep it in the table definition and echo it through SHOW CREATE TABLE, and the
// index still enforces its uniqueness.
var indexCommentScript = []parityStep{
	{Query: "CREATE DATABASE ic", SkipAffect: true},
	{Query: "USE ic", SkipAffect: true},
	{Query: "CREATE TABLE portal_dic_type (id varchar(64) NOT NULL, class_key varchar(64) NULL, parent_id varchar(64) NULL, sort int NULL DEFAULT 0, PRIMARY KEY (id) USING BTREE, UNIQUE INDEX unique_class_key(class_key, parent_id) USING BTREE COMMENT '分类key不重复') ENGINE = InnoDB CHARACTER SET = utf8mb4 COLLATE = utf8mb4_0900_ai_ci COMMENT = '字典类型表'", SkipAffect: true},
	{Query: "SHOW CREATE TABLE portal_dic_type", Rows: true},
	{Query: "INSERT INTO portal_dic_type (id, class_key, parent_id) VALUES ('1','a','p1')"},
	// The commented unique index still rejects duplicates.
	{Query: "INSERT INTO portal_dic_type (id, class_key, parent_id) VALUES ('2','a','p1')", Fail: true},
	{Query: "INSERT INTO portal_dic_type (id, class_key, parent_id) VALUES ('3','a','p2')"},
	{Query: "SELECT id FROM portal_dic_type ORDER BY id", Rows: true},
	{Query: "CREATE INDEX idx_sort ON portal_dic_type (sort) COMMENT '排序索引'", SkipAffect: true},
	{Query: "SHOW CREATE TABLE portal_dic_type", Rows: true},
	{Query: "ALTER TABLE portal_dic_type ADD UNIQUE KEY uq_id_sort (id, sort) COMMENT 'id+排序'", SkipAffect: true},
	{Query: "SHOW CREATE TABLE portal_dic_type", Rows: true},
}

func TestMVCCIndexCommentsMatchLegacyEngine(t *testing.T) {
	legacy, err := openLegacy(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	legacySession := &Session{}
	legacyOutcomes := runParityScript(t, func(q string) (*Result, error) { return legacy.Execute(legacySession, q) }, indexCommentScript)

	e, session, _ := rangeTestEngine(t)
	mvccOutcomes := runParityScript(t, func(q string) (*Result, error) { return e.Execute(session, q) }, indexCommentScript)

	if !reflect.DeepEqual(legacyOutcomes, mvccOutcomes) {
		for i, step := range indexCommentScript {
			if legacyOutcomes[i] != mvccOutcomes[i] {
				t.Errorf("%s\nlegacy=%+v\nmvcc=%+v", step.Query, legacyOutcomes[i], mvccOutcomes[i])
			}
		}
	}
}

func TestMVCCIndexCommentSurvivesReopenAndMaintenance(t *testing.T) {
	dir := t.TempDir()
	e, err := OpenWithOptions(dir, "root", "pw", OpenOptions{StorageMode: "mvcc"})
	if err != nil {
		t.Fatal(err)
	}
	s := &Session{}
	run := func(query string) *Result {
		t.Helper()
		result, err := e.Execute(s, query)
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return result
	}
	run("CREATE DATABASE icr")
	run("USE icr")
	run("CREATE TABLE t (id INT PRIMARY KEY, sku VARCHAR(16), UNIQUE KEY uq_sku (sku) COMMENT '商品编码唯一')")
	show := func() string { return fmt.Sprint(run("SHOW CREATE TABLE t").Rows[0][1]) }
	if got := show(); !strings.Contains(got, "COMMENT '商品编码唯一'") {
		t.Fatalf("index comment missing after CREATE: %s", got)
	}
	// The comment is part of the persisted definition.
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err = OpenWithOptions(dir, "root", "pw", OpenOptions{StorageMode: "mvcc"})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	s = &Session{CurrentDatabase: "icr"}
	if got := show(); !strings.Contains(got, "COMMENT '商品编码唯一'") {
		t.Fatalf("index comment missing after reopen: %s", got)
	}
	// An ALTER that rebuilds the table keeps the comment, and CREATE INDEX adds one.
	run("ALTER TABLE t ADD COLUMN note VARCHAR(16) NULL")
	if got := show(); !strings.Contains(got, "COMMENT '商品编码唯一'") {
		t.Fatalf("index comment missing after ALTER: %s", got)
	}
	run("CREATE INDEX idx_note ON t (note) COMMENT '备注索引'")
	if got := show(); !strings.Contains(got, "COMMENT '备注索引'") {
		t.Fatalf("created index comment missing: %s", got)
	}
	// The commented unique index still enforces uniqueness.
	run("INSERT INTO t (id, sku) VALUES (1, 'A')")
	if _, err := e.Execute(s, "INSERT INTO t (id, sku) VALUES (2, 'A')"); err == nil {
		t.Fatal("commented unique index stopped enforcing uniqueness")
	}
}
