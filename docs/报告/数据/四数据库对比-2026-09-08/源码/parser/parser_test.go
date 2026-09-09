package parser

import (
	"strings"
	"testing"
)

func TestParseCoreStatements(t *testing.T) {
	queries := []string{
		"CREATE DATABASE test;",
		"CREATE TABLE user(id INT, name VARCHAR(50), age INT);",
		"INSERT INTO user VALUES (1, '张三', 20), (2, '李四', 21);",
		"SELECT name, age FROM user WHERE age >= 20 AND name != 'x' ORDER BY age DESC LIMIT 10;",
		"SELECT auth_code, COUNT(*) AS total FROM auth_code_equ WHERE auth_code LIKE '%000%' GROUP BY auth_code ORDER BY total DESC LIMIT 10;",
		"SELECT * FROM user WHERE (age BETWEEN 18 AND 30 OR id IN (1, 2, 3)) AND name NOT LIKE '%test%' AND deleted_at IS NULL;",
		"SELECT * FROM user WHERE id NOT IN (4, 5) AND age NOT BETWEEN 60 AND 80 AND enabled IS NOT FALSE;",
		"SELECT * FROM workout_sets WHERE workout_exercise_id IN (SELECT id FROM workout_exercises WHERE workout_id = 10);",
		"SELECT region area, category, COUNT(*) total, SUM(amount) sum_amount, AVG(amount) avg_amount, MIN(amount) min_amount, MAX(amount) max_amount FROM sales WHERE amount IS NOT NULL GROUP BY 1, category HAVING total > 1 ORDER BY total DESC, area ASC LIMIT 10 OFFSET 2;",
		"SELECT region, COUNT(id) AS total FROM sales GROUP BY region ORDER BY COUNT(id) DESC, 1 ASC;",
		"SELECT region, COUNT(*) AS total FROM sales GROUP BY region HAVING COUNT(*) > 1 ORDER BY SUM(amount) DESC;",
		"SELECT SUM(v1.count1) FROM (SELECT auth_code, COUNT(*) AS count1 FROM auth_code_equ GROUP BY auth_code) v1;",
		"SELECT v.id FROM users AS v WHERE v.age >= 18 ORDER BY v.id LIMIT 10;",
		"SELECT id, COALESCE(name, ''), IFNULL(note, 'none'), CONCAT(LOWER(name), '-', UPPER(city)), CHAR_LENGTH(name) AS name_length FROM users WHERE id = 1 LIMIT 1 FOR UPDATE;",
		"SELECT id FROM users WHERE id = 1 LOCK IN SHARE MODE;",
		"SELECT c.id, COALESCE(a.app_name, ''), COALESCE(e.used_count, 0) FROM auth_code c LEFT JOIN auth_app a ON a.id = c.app_id LEFT JOIN (SELECT code_id, COUNT(*) AS used_count FROM auth_code_equ GROUP BY code_id) e ON e.code_id = c.id WHERE c.status = '10' ORDER BY c.create_time DESC LIMIT 10;",
		"SELECT a.id, c.id FROM auth_app a INNER JOIN auth_code c ON c.app_id = a.id;",
		"SELECT a.id, c.id FROM auth_app a RIGHT JOIN auth_code c ON c.app_id = a.id;",
		"SELECT a.id, c.id FROM auth_app a CROSS JOIN auth_code c LIMIT 5;",
		"SELECT DISTINCT city FROM users ORDER BY city LIMIT 10;",
		"SELECT * FROM 1ssssd;",
		"CREATE TABLE IF NOT EXISTS users(id INT);",
		"INSERT IGNORE INTO users VALUES (1);",
		"INSERT INTO users SELECT * FROM source_users;",
		"INSERT INTO users(id, name) SELECT source_id, source_name FROM source_users WHERE enabled = 1;",
		"DESCRIBE users;",
		"DESC users;",
		"TRUNCATE TABLE users;",
		"UPDATE user SET age=21 WHERE id=1;",
		"DELETE FROM user WHERE id=1;",
		"SHOW DATABASES;", "SHOW TABLES;", "USE test;", "BEGIN;", "COMMIT;", "ROLLBACK;",
		"DROP DATABASE IF EXISTS test;", "DROP TABLE IF EXISTS users;",
	}
	for _, query := range queries {
		if _, err := Parse(query); err != nil {
			t.Errorf("Parse(%q): %v", query, err)
		}
	}
}

func TestParseJoinUpdateAndMultiTableDelete(t *testing.T) {
	statement, err := Parse("UPDATE users AS u JOIN adjustments a ON a.user_id=u.id SET u.balance=u.balance+a.delta,u.label=a.label WHERE a.enabled=1")
	if err != nil {
		t.Fatal(err)
	}
	update, ok := statement.(Update)
	if !ok || update.Table != "users" || update.TableAlias != "u" || len(update.Joins) != 1 || len(update.Assignments) != 2 {
		t.Fatalf("UPDATE JOIN = %#v", statement)
	}

	statement, err = Parse("DELETE p.*,c FROM parents p JOIN children c ON c.parent_id=p.id WHERE p.id=1")
	if err != nil {
		t.Fatal(err)
	}
	deletion, ok := statement.(Delete)
	if !ok || len(deletion.Targets) != 2 || deletion.Targets[0] != "p" || deletion.TableAlias != "p" || len(deletion.Joins) != 1 {
		t.Fatalf("DELETE targets FROM = %#v", statement)
	}

	statement, err = Parse("DELETE FROM c,p USING parents p JOIN children c ON c.parent_id=p.id WHERE p.id=2")
	if err != nil {
		t.Fatal(err)
	}
	deletion, ok = statement.(Delete)
	if !ok || len(deletion.Targets) != 2 || deletion.Targets[0] != "c" || deletion.Table != "parents" || deletion.TableAlias != "p" {
		t.Fatalf("DELETE FROM USING = %#v", statement)
	}
}

func TestParseReplaceFormsAndCreateTableFromQuery(t *testing.T) {
	for _, query := range []string{
		"REPLACE INTO items VALUES (1,'one')",
		"REPLACE LOW_PRIORITY INTO items VALUE (1,'one')",
		"REPLACE DELAYED items SET id=1,label='one'",
		"REPLACE INTO items(id,label) SELECT id,label FROM archived_items UNION ALL SELECT id,label FROM pending_items",
		"CREATE TABLE copied LIKE items",
		"CREATE TABLE selected AS SELECT id,label FROM items UNION ALL SELECT id,label FROM archived_items",
	} {
		statement, err := Parse(query)
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		switch {
		case strings.HasPrefix(query, "REPLACE"):
			if insert, ok := statement.(Insert); !ok || !insert.Replace {
				t.Fatalf("%s = %#v", query, statement)
			}
		case strings.Contains(query, " LIKE "):
			if _, ok := statement.(CreateTableLike); !ok {
				t.Fatalf("%s = %#v", query, statement)
			}
		default:
			if _, ok := statement.(CreateTableAs); !ok {
				t.Fatalf("%s = %#v", query, statement)
			}
		}
	}
}

func TestParseShowColumnsFilters(t *testing.T) {
	statement, err := Parse("SHOW FULL COLUMNS FROM users LIKE 'weight_%'")
	if err != nil {
		t.Fatal(err)
	}
	show, ok := statement.(Show)
	if !ok || show.What != "COLUMNS" || !show.Full || show.Name != "users" || show.Pattern != "weight_%" {
		t.Fatalf("SHOW FULL COLUMNS = %#v", statement)
	}
	statement, err = Parse("SHOW COLUMNS FROM users FROM fitness WHERE Field='weight_unit'")
	if err != nil {
		t.Fatal(err)
	}
	show, ok = statement.(Show)
	if !ok || show.Name != "fitness.users" || show.Where == nil {
		t.Fatalf("SHOW COLUMNS WHERE = %#v", statement)
	}
}

func TestParseTopLevelUnion(t *testing.T) {
	statement, err := Parse("SELECT id,name FROM users UNION ALL SELECT id,name FROM archived_users UNION DISTINCT SELECT id,name FROM invited_users ORDER BY name DESC LIMIT 2 OFFSET 1")
	if err != nil {
		t.Fatal(err)
	}
	union, ok := statement.(Union)
	if !ok || len(union.Queries) != 3 || len(union.All) != 2 || !union.All[0] || union.All[1] || len(union.OrderBy) != 1 || !union.OrderBy[0].Desc || !union.HasLimit || union.Limit != 2 || union.Offset != 1 {
		t.Fatalf("statement = %#v", statement)
	}
	if len(union.Queries[2].OrderBy) != 0 || union.Queries[2].HasLimit {
		t.Fatalf("global UNION clauses remained on last query: %#v", union.Queries[2])
	}
}

func TestParseInSubquery(t *testing.T) {
	statement, err := Parse("DELETE FROM workout_sets WHERE workout_exercise_id NOT IN (SELECT id FROM workout_exercises WHERE workout_id=10)")
	if err != nil {
		t.Fatal(err)
	}
	deletion, ok := statement.(Delete)
	if !ok {
		t.Fatalf("statement = %#v", statement)
	}
	predicate, ok := deletion.Where.(InExpr)
	subquery, subqueryOK := predicate.Subquery.(Select)
	if !ok || !predicate.Not || !subqueryOK || subquery.Table != "workout_exercises" {
		t.Fatalf("predicate = %#v", deletion.Where)
	}
}

func TestParseRowConstructorInPredicate(t *testing.T) {
	statement, err := Parse("SELECT id FROM memberships WHERE (user_id,status) IN (SELECT user_id,status FROM archived_memberships) OR (user_id,status) IN ((1,'active'),(2,'paused'))")
	if err != nil {
		t.Fatal(err)
	}
	query := statement.(Select)
	disjunction, ok := query.Where.(BinaryExpr)
	if !ok {
		t.Fatalf("predicate = %#v", query.Where)
	}
	left, ok := disjunction.Left.(InExpr)
	if !ok || left.Subquery == nil {
		t.Fatalf("subquery row IN = %#v", disjunction.Left)
	}
	if row, ok := left.Value.(RowExpr); !ok || len(row.Values) != 2 {
		t.Fatalf("row value = %#v", left.Value)
	}
}

func TestParseExistsAndScalarSubqueryPredicates(t *testing.T) {
	statement, err := Parse("SELECT id FROM users WHERE EXISTS (SELECT id FROM sessions WHERE revoked_at IS NULL) AND id=(SELECT user_id FROM sessions WHERE id=1)")
	if err != nil {
		t.Fatal(err)
	}
	query := statement.(Select)
	conjunction, ok := query.Where.(BinaryExpr)
	if !ok || conjunction.Operator != "AND" {
		t.Fatalf("predicate = %#v", query.Where)
	}
	if _, ok := conjunction.Left.(ExistsExpr); !ok {
		t.Fatalf("EXISTS predicate = %#v", conjunction.Left)
	}
	equality, ok := conjunction.Right.(BinaryExpr)
	if !ok {
		t.Fatalf("scalar equality = %#v", conjunction.Right)
	}
	if _, ok := equality.Right.(ScalarSubquery); !ok {
		t.Fatalf("scalar subquery = %#v", equality.Right)
	}
	if _, err := Parse("SELECT id FROM users WHERE NOT EXISTS (SELECT id FROM sessions)"); err != nil {
		t.Fatal(err)
	}
}

func TestParseMySQLMigrationDDLAndExplain(t *testing.T) {
	addition, err := Parse("ALTER TABLE users ADD COLUMN enabled BOOLEAN NOT NULL DEFAULT TRUE AFTER name")
	if err != nil {
		t.Fatal(err)
	}
	column, ok := addition.(AddColumn)
	if !ok || column.Table != "users" || column.Column.Name != "enabled" || column.After != "name" {
		t.Fatalf("unexpected ADD COLUMN: %#v", addition)
	}

	foreign, err := Parse("ALTER TABLE subscriptions ADD CONSTRAINT fk_subscription_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE RESTRICT")
	if err != nil {
		t.Fatal(err)
	}
	foreignKey, ok := foreign.(AlterForeignKey)
	if !ok || foreignKey.ForeignKey.Name != "fk_subscription_user" || foreignKey.ForeignKey.OnDelete != "RESTRICT" {
		t.Fatalf("unexpected ADD FOREIGN KEY: %#v", foreign)
	}

	check, err := Parse("ALTER TABLE users ADD CONSTRAINT ck_balance CHECK (course_balance >= 0)")
	if err != nil {
		t.Fatal(err)
	}
	checkConstraint, ok := check.(AlterCheck)
	if !ok || checkConstraint.Check.Name != "ck_balance" || checkConstraint.Check.Expression != "course_balance >= 0" {
		t.Fatalf("unexpected ADD CHECK: %#v", check)
	}

	rename, err := Parse("RENAME TABLE users TO members, subscriptions TO plans")
	if err != nil {
		t.Fatal(err)
	}
	if statement, ok := rename.(RenameTable); !ok || len(statement.Pairs) != 2 || statement.Pairs[1].To != "plans" {
		t.Fatalf("unexpected RENAME TABLE: %#v", rename)
	}

	explain, err := Parse("EXPLAIN SELECT id FROM users WHERE id=1")
	if err != nil {
		t.Fatal(err)
	}
	statement, ok := explain.(Explain)
	query, queryOK := statement.Query.(Select)
	if !ok || !queryOK || query.Table != "users" {
		t.Fatalf("unexpected EXPLAIN: %#v", explain)
	}
}

func TestParseMySQLMultiActionAlterTable(t *testing.T) {
	statement, err := Parse(`ALTER TABLE items
		ADD COLUMN quantity INT NOT NULL DEFAULT 0,
		ADD COLUMN label VARCHAR(20),
		MODIFY COLUMN name VARCHAR(50),
		CHANGE COLUMN label status VARCHAR(30),
		ADD INDEX idx_status(status),
		DROP INDEX idx_old`)
	if err != nil {
		t.Fatal(err)
	}
	batch, ok := statement.(AlterTableBatch)
	if !ok || batch.Table != "items" || len(batch.Actions) != 6 {
		t.Fatalf("unexpected multi-action ALTER TABLE: %#v", statement)
	}
	if first, ok := batch.Actions[0].(AddColumn); !ok || first.Column.Name != "quantity" || first.Table != "items" {
		t.Fatalf("unexpected first ALTER action: %#v", batch.Actions[0])
	}
	if changed, ok := batch.Actions[3].(AlterColumn); !ok || changed.OldName != "label" || changed.Column.Name != "status" {
		t.Fatalf("unexpected CHANGE action: %#v", batch.Actions[3])
	}
	if index, ok := batch.Actions[4].(CreateIndex); !ok || index.Name != "idx_status" || index.Table != "items" {
		t.Fatalf("unexpected ADD INDEX action: %#v", batch.Actions[4])
	}
	if _, ok := batch.Actions[5].(DropIndex); !ok {
		t.Fatalf("unexpected DROP INDEX action: %#v", batch.Actions[5])
	}

	single, err := Parse("ALTER TABLE items ADD COLUMN enabled BOOLEAN")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := single.(AddColumn); !ok {
		t.Fatalf("single ALTER action changed shape: %#v", single)
	}
	if _, err := Parse("ALTER TABLE items RENAME TO archived_items, ADD COLUMN note TEXT"); err == nil {
		t.Fatal("mixed RENAME and ALTER actions unexpectedly succeeded")
	}
}

func TestParseColumnLifecycleAlterTable(t *testing.T) {
	statement, err := Parse(`ALTER TABLE items
		ADD COLUMN IF NOT EXISTS note VARCHAR(100),
		RENAME COLUMN note TO description,
		DROP COLUMN IF EXISTS obsolete`)
	if err != nil {
		t.Fatal(err)
	}
	batch, ok := statement.(AlterTableBatch)
	if !ok || len(batch.Actions) != 3 {
		t.Fatalf("statement = %#v", statement)
	}
	addition, ok := batch.Actions[0].(AddColumn)
	if !ok || !addition.IfNotExists || addition.Column.Name != "note" {
		t.Fatalf("ADD COLUMN = %#v", batch.Actions[0])
	}
	rename, ok := batch.Actions[1].(RenameColumn)
	if !ok || rename.OldName != "note" || rename.NewName != "description" {
		t.Fatalf("RENAME COLUMN = %#v", batch.Actions[1])
	}
	drop, ok := batch.Actions[2].(DropColumn)
	if !ok || !drop.IfExists || drop.Name != "obsolete" {
		t.Fatalf("DROP COLUMN = %#v", batch.Actions[2])
	}
	withoutKeyword, err := Parse("ALTER TABLE items DROP legacy_value")
	if err != nil {
		t.Fatal(err)
	}
	if drop, ok := withoutKeyword.(DropColumn); !ok || drop.Name != "legacy_value" {
		t.Fatalf("DROP without COLUMN = %#v", withoutKeyword)
	}
}

func TestParseInsertSelect(t *testing.T) {
	statement, err := Parse("INSERT INTO `items_copy1` SELECT * FROM `application`.`items`")
	if err != nil {
		t.Fatal(err)
	}
	insert, ok := statement.(Insert)
	query, queryOK := insert.Select.(Select)
	if !ok || insert.Table != "items_copy1" || !queryOK || query.Table != "application.items" || len(insert.Values) != 0 {
		t.Fatalf("unexpected INSERT SELECT AST: %#v", statement)
	}
}

func TestParseNumericLeadingIdentifier(t *testing.T) {
	statement, err := Parse("SELECT * from 1ssssd")
	if err != nil {
		t.Fatal(err)
	}
	query, ok := statement.(Select)
	if !ok || query.Table != "1ssssd" {
		t.Fatalf("numeric-leading identifier AST = %#v", statement)
	}
}

func TestParseMySQLIndexStatements(t *testing.T) {
	queries := []string{
		"CREATE INDEX idx_name ON users(name)",
		"CREATE UNIQUE INDEX idx_identity ON users(tenant_id,id) USING BTREE",
		"ALTER TABLE `yuanma-auth1`.`sss` ADD INDEX `id`(`id`)",
		"ALTER TABLE `yuanma-auth1`.`sss` ADD UNIQUE INDEX `id`(`id`)",
		"ALTER TABLE users ADD INDEX (name)",
		"ALTER TABLE users ADD CONSTRAINT uq_users_phone UNIQUE (phone)",
		"ALTER TABLE users RENAME INDEX idx_name TO idx_display_name",
		"ALTER TABLE users DROP INDEX idx_name",
		"DROP INDEX idx_identity ON users",
		"SHOW INDEX FROM users",
		"SHOW KEYS FROM users FROM application",
	}
	for _, query := range queries {
		if _, err := Parse(query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	statement, err := Parse("ALTER TABLE users ADD UNIQUE INDEX idx_identity(tenant_id,id)")
	if err != nil {
		t.Fatal(err)
	}
	index, ok := statement.(CreateIndex)
	if !ok || !index.Unique || index.Name != "idx_identity" || index.Table != "users" || len(index.Columns) != 2 {
		t.Fatalf("unexpected index AST: %#v", statement)
	}
	statement, err = Parse("ALTER TABLE users ADD CONSTRAINT uq_users_phone UNIQUE KEY (phone)")
	if err != nil {
		t.Fatal(err)
	}
	index, ok = statement.(CreateIndex)
	if !ok || !index.Unique || index.Name != "uq_users_phone" || len(index.Columns) != 1 || index.Columns[0] != "phone" {
		t.Fatalf("unexpected UNIQUE constraint AST: %#v", statement)
	}
}

func TestParseMySQLAlterColumnStatements(t *testing.T) {
	tests := []struct {
		query   string
		oldName string
		newName string
		kind    string
		length  int
	}{
		{"ALTER TABLE items MODIFY COLUMN last_time DATETIME", "last_time", "last_time", "DATETIME", 0},
		{"ALTER TABLE app.items MODIFY last_time DATETIME NULL DEFAULT NULL COMMENT 'last access' AFTER expires_at", "last_time", "last_time", "DATETIME", 0},
		{"ALTER TABLE items CHANGE COLUMN old_name new_name VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci", "old_name", "new_name", "VARCHAR", 128},
	}
	for _, test := range tests {
		statement, err := Parse(test.query)
		if err != nil {
			t.Fatalf("%s: %v", test.query, err)
		}
		alter, ok := statement.(AlterColumn)
		if !ok || alter.OldName != test.oldName || alter.Column.Name != test.newName || !strings.EqualFold(alter.Column.Type, test.kind) || alter.Column.Length != test.length {
			t.Fatalf("unexpected ALTER COLUMN AST for %q: %#v", test.query, statement)
		}
	}

	for _, query := range []string{
		"ALTER TABLE items MODIFY COLUMN",
		"ALTER TABLE items MODIFY COLUMN last_time",
		"ALTER TABLE items CHANGE COLUMN old_name",
	} {
		if _, err := Parse(query); err == nil {
			t.Fatalf("expected %q to fail", query)
		}
	}

	setDefault, err := Parse("ALTER TABLE items ALTER COLUMN retries SET DEFAULT 3")
	if err != nil {
		t.Fatal(err)
	}
	setAction, ok := setDefault.(AlterColumnDefault)
	if !ok || setAction.Drop || setAction.Name != "retries" || setAction.Default.Kind != LiteralNumber || setAction.Default.Text != "3" {
		t.Fatalf("unexpected SET DEFAULT AST: %#v", setDefault)
	}
	dropDefault, err := Parse("ALTER TABLE items ALTER retries DROP DEFAULT")
	if err != nil {
		t.Fatal(err)
	}
	dropAction, ok := dropDefault.(AlterColumnDefault)
	if !ok || !dropAction.Drop || dropAction.Name != "retries" {
		t.Fatalf("unexpected DROP DEFAULT AST: %#v", dropDefault)
	}
}

func TestParseMySQLInsertSetAndValue(t *testing.T) {
	statement, err := Parse("INSERT IGNORE users SET name='Alice',score=1+2,updated_at=NOW() ON DUPLICATE KEY UPDATE score=score+1")
	if err != nil {
		t.Fatal(err)
	}
	insert, ok := statement.(Insert)
	if !ok || !insert.Ignore || len(insert.Columns) != 3 || len(insert.SetValues) != 3 || len(insert.OnDuplicate) != 1 {
		t.Fatalf("unexpected INSERT SET AST: %#v", statement)
	}
	if expression, ok := insert.SetValues[1].(BinaryExpr); !ok || expression.Operator != "+" {
		t.Fatalf("unexpected INSERT SET expression: %#v", insert.SetValues[1])
	}
	if _, err := Parse("INSERT INTO users(name) VALUE ('Bob')"); err != nil {
		t.Fatal(err)
	}
}

func TestParseNavicatUpdateLimitAndNullSafeEquality(t *testing.T) {
	statement, err := Parse("UPDATE `application`.`items` SET `label`='changed' WHERE `note` <=> NULL LIMIT 1")
	if err != nil {
		t.Fatal(err)
	}
	update, ok := statement.(Update)
	if !ok || update.Table != "application.items" || !update.HasLimit || update.Limit != 1 {
		t.Fatalf("unexpected UPDATE AST: %#v", statement)
	}
	comparison, ok := update.Where.(BinaryExpr)
	if !ok || comparison.Operator != "<=>" {
		t.Fatalf("unexpected UPDATE predicate: %#v", update.Where)
	}
}

func TestParseUpdateExpressions(t *testing.T) {
	statement, err := Parse(`UPDATE users
		SET balance=balance+5,
		    doubled=balance*2,
		    updated_at=CURRENT_TIMESTAMP,
		    status=CASE WHEN balance>0 THEN 'active' ELSE 'empty' END
		WHERE id=1 LIMIT 1`)
	if err != nil {
		t.Fatal(err)
	}
	update, ok := statement.(Update)
	if !ok || len(update.Assignments) != 4 || !update.HasLimit || update.Limit != 1 {
		t.Fatalf("statement = %#v", statement)
	}
	if expression, ok := update.Assignments[0].Value.(BinaryExpr); !ok || expression.Operator != "+" {
		t.Fatalf("first assignment = %#v", update.Assignments[0])
	}
	if expression, ok := update.Assignments[1].Value.(BinaryExpr); !ok || expression.Operator != "*" {
		t.Fatalf("second assignment = %#v", update.Assignments[1])
	}
}

func TestParseNavicatDeleteLimitAndNullSafeEquality(t *testing.T) {
	statement, err := Parse("DELETE FROM `application`.`items` WHERE `id` <=> 7 LIMIT 1")
	if err != nil {
		t.Fatal(err)
	}
	deleteStatement, ok := statement.(Delete)
	if !ok || deleteStatement.Table != "application.items" || !deleteStatement.HasLimit || deleteStatement.Limit != 1 {
		t.Fatalf("unexpected DELETE AST: %#v", statement)
	}
	comparison, ok := deleteStatement.Where.(BinaryExpr)
	if !ok || comparison.Operator != "<=>" {
		t.Fatalf("unexpected DELETE predicate: %#v", deleteStatement.Where)
	}
}

func TestParseMySQLAccountAndPrivilegeStatements(t *testing.T) {
	queries := []string{
		"CREATE USER IF NOT EXISTS 'app'@'localhost' IDENTIFIED WITH mysql_native_password BY 'secret', 'reader'@'%' IDENTIFIED BY 'read-secret'",
		"ALTER USER IF EXISTS 'app'@'localhost' IDENTIFIED BY 'new-secret'",
		"DROP USER IF EXISTS 'app'@'localhost', 'reader'@'%'",
		"RENAME USER 'app'@'localhost' TO 'service'@'%'",
		"SET PASSWORD FOR 'service'@'%' = 'changed'",
		"GRANT SELECT, INSERT, CREATE VIEW ON `application`.* TO 'service'@'%' WITH GRANT OPTION",
		"REVOKE GRANT OPTION FOR SELECT ON `application`.* FROM 'service'@'%'",
		"REVOKE INSERT ON `application`.`records` FROM 'service'@'%'",
		"SHOW GRANTS FOR 'service'@'%'",
		"SHOW CREATE USER 'service'@'%'",
	}
	for _, query := range queries {
		if _, err := Parse(query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	statement, err := Parse("GRANT SELECT, UPDATE ON `application`.`records` TO 'service'@'localhost' WITH GRANT OPTION")
	if err != nil {
		t.Fatal(err)
	}
	grant, ok := statement.(Grant)
	if !ok || grant.Database != "application" || grant.Table != "records" || len(grant.Accounts) != 1 || grant.Accounts[0].Username != "service" || grant.Accounts[0].Host != "localhost" || !grant.GrantOption || len(grant.Privileges) != 2 {
		t.Fatalf("unexpected GRANT AST: %#v", statement)
	}
}

func TestDerivedTableRequiresAlias(t *testing.T) {
	if _, err := Parse("SELECT * FROM (SELECT id FROM users)"); err == nil {
		t.Fatal("expected a missing derived table alias error")
	}
}

func TestRejectsUnsupportedSQL(t *testing.T) {
	if _, err := Parse("MERGE user"); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestParseCommentOnlyQuery(t *testing.T) {
	for _, query := range []string{"-- client comment", "# mysql comment", "/* import comment */", ";"} {
		statement, err := Parse(query)
		if err != nil {
			t.Fatalf("Parse(%q): %v", query, err)
		}
		if _, ok := statement.(Empty); !ok {
			t.Fatalf("Parse(%q) returned %T", query, statement)
		}
	}
}

func TestMySQLExecutableCommentsAreExpandedOutsideQuotedText(t *testing.T) {
	statement, err := Parse("/*!50001 CREATE VIEW `active-items` AS SELECT `id` FROM `items` */;")
	if err != nil {
		t.Fatal(err)
	}
	view, ok := statement.(CreateView)
	if !ok || view.Name != "active-items" || !strings.Contains(view.Definition, "FROM `items`") {
		t.Fatalf("executable comment parsed as %#v", statement)
	}

	statement, err = Parse("SELECT '/*!50001 DROP TABLE private_data */'")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := statement.(Select); !ok {
		t.Fatalf("quoted executable comment parsed as %T", statement)
	}
	commentedSQL := "-- /*!50001 DROP TABLE line_comment */\n/* ordinary */ SELECT 1"
	expanded, err := ExpandMySQLExecutableComments(commentedSQL)
	if err != nil || !strings.Contains(expanded, "-- /*!50001 DROP TABLE line_comment */") || !strings.Contains(expanded, "/* ordinary */") {
		t.Fatalf("comment expansion = %q, %v", expanded, err)
	}
	statement, err = Parse(commentedSQL)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := statement.(Select); !ok {
		t.Fatalf("line-comment executable SQL parsed as %T", statement)
	}
	if _, err := ExpandMySQLExecutableComments("/*!50001 SELECT 1"); err == nil {
		t.Fatal("unterminated executable comment was accepted")
	}
}

func TestParseMySQLSelectModifiers(t *testing.T) {
	statement, err := Parse("SELECT /*!40001 SQL_NO_CACHE */ * FROM `order-items`")
	if err != nil {
		t.Fatal(err)
	}
	selectStatement, ok := statement.(Select)
	if !ok || len(selectStatement.Items) != 1 || selectStatement.Items[0].Expression != "*" || selectStatement.Table != "order-items" {
		t.Fatalf("mysqldump SELECT parsed as %#v", statement)
	}

	statement, err = Parse("SELECT DISTINCTROW HIGH_PRIORITY SQL_BIG_RESULT id FROM items")
	if err != nil {
		t.Fatal(err)
	}
	selectStatement, ok = statement.(Select)
	if !ok || !selectStatement.Distinct || len(selectStatement.Items) != 1 || selectStatement.Items[0].Expression != "id" {
		t.Fatalf("modified SELECT parsed as %#v", statement)
	}
}

func TestParseNavicatCreateDatabase(t *testing.T) {
	queries := []string{
		"CREATE DATABASE `11` CHARACTER SET 'utf8mb4'",
		"CREATE DATABASE `yuanma-auth`",
		"CREATE DATABASE `minisql` DEFAULT CHARACTER SET = utf8mb4 COLLATE = 'utf8mb4_general_ci'",
		"CREATE DATABASE IF NOT EXISTS test CHARSET utf8mb4",
	}
	for _, query := range queries {
		statement, err := Parse(query)
		if err != nil {
			t.Fatalf("Parse(%q): %v", query, err)
		}
		if _, ok := statement.(CreateDatabase); !ok {
			t.Fatalf("Parse(%q) returned %T", query, statement)
		}
	}
}

func TestParseNavicatCreateTable(t *testing.T) {
	query := "CREATE TABLE `auth_app` (`id` varchar(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci NOT NULL COMMENT 'id', `app_desc` text CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci COMMENT 'desc', PRIMARY KEY (`id`) USING BTREE, UNIQUE KEY `uq_desc` (`app_desc`) USING BTREE, KEY `idx_desc` USING BTREE (`app_desc`)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='table'"
	statement, err := Parse(query)
	if err != nil {
		t.Fatal(err)
	}
	create, ok := statement.(CreateTable)
	if !ok {
		t.Fatalf("got %T", statement)
	}
	if len(create.Columns) != 2 || create.Columns[0].Name != "id" || create.Columns[1].Name != "app_desc" {
		t.Fatalf("unexpected columns: %#v", create.Columns)
	}
	if len(create.PrimaryKey) != 1 || create.PrimaryKey[0] != "id" || create.Columns[0].Nullable || create.Columns[0].Comment != "id" || create.Comment != "table" {
		t.Fatalf("unexpected constraints: %#v", create)
	}
	if len(create.Indexes) != 2 || create.Indexes[0].Name != "uq_desc" || !create.Indexes[0].Unique || create.Indexes[1].Name != "idx_desc" || create.Indexes[1].Unique {
		t.Fatalf("unexpected indexes: %#v", create.Indexes)
	}
	alterStatement, err := Parse("ALTER TABLE auth_app COMMENT='updated table'")
	if err != nil {
		t.Fatal(err)
	}
	alter, ok := alterStatement.(AlterTableComment)
	if !ok || alter.Table != "auth_app" || alter.Comment != "updated table" {
		t.Fatalf("unexpected ALTER TABLE COMMENT: %#v", alterStatement)
	}
}

func TestParseColumnConstraintsAndAlterPrimaryKey(t *testing.T) {
	statement, err := Parse("CREATE TABLE items(id BIGINT NOT NULL AUTO_INCREMENT,name VARCHAR(32) NULL DEFAULT NULL,updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,PRIMARY KEY(id))")
	if err != nil {
		t.Fatal(err)
	}
	create := statement.(CreateTable)
	if len(create.PrimaryKey) != 1 || !create.Columns[0].AutoIncrement || create.Columns[0].Nullable || !create.Columns[1].HasDefault || create.Columns[1].Default.Kind != LiteralNull || create.Columns[2].DefaultExpression != "current_timestamp" || create.Columns[2].OnUpdate != "current_timestamp" {
		t.Fatalf("unexpected CREATE TABLE: %#v", create)
	}
	statement, err = Parse("ALTER TABLE items ADD PRIMARY KEY(id)")
	if err != nil {
		t.Fatal(err)
	}
	index := statement.(CreateIndex)
	if !index.Primary || !index.Unique || index.Name != "PRIMARY" {
		t.Fatalf("unexpected ALTER TABLE: %#v", index)
	}
}

func TestParseNavicatDropMultipleTables(t *testing.T) {
	query := "DROP TABLE IF EXISTS `navicat_e2e`.`auth_app`, `navicat_e2e`.`auth_app_navicat`, `navicat_e2e`.`auth_app_navicat_fixed`;"
	statement, err := Parse(query)
	if err != nil {
		t.Fatal(err)
	}
	drop, ok := statement.(DropTable)
	if !ok {
		t.Fatalf("got %T", statement)
	}
	if !drop.IfExists || len(drop.Names) != 3 || drop.Names[0] != "navicat_e2e.auth_app" {
		t.Fatalf("unexpected DROP TABLE: %#v", drop)
	}
}

func TestParseMySQLImportTypes(t *testing.T) {
	query := "CREATE TABLE `auth_code` (`id` varchar(64) NOT NULL, `use_info` tinytext NULL, `create_time` datetime NULL, `amount` decimal(10,2), PRIMARY KEY (`id`) USING BTREE) ENGINE=InnoDB CHARACTER SET=utf8mb4"
	statement, err := Parse(query)
	if err != nil {
		t.Fatal(err)
	}
	create, ok := statement.(CreateTable)
	if !ok || len(create.Columns) != 4 {
		t.Fatalf("unexpected statement: %#v", statement)
	}
}

func TestParsePreservesMySQLColumnTypeDeclarations(t *testing.T) {
	query := "CREATE TABLE type_matrix(" +
		"tiny TINYINT(1) UNSIGNED," +
		"amount DECIMAL(20,6) UNSIGNED ZEROFILL," +
		"state ENUM('new','in progress','done')," +
		"flags SET('a','b')," +
		"payload VARBINARY(16)," +
		"occurred TIME(6)," +
		"created TIMESTAMP(6)," +
		"shape POINT SRID 4326)"
	statement, err := Parse(query)
	if err != nil {
		t.Fatal(err)
	}
	create, ok := statement.(CreateTable)
	if !ok || len(create.Columns) != 8 {
		t.Fatalf("unexpected statement: %#v", statement)
	}
	want := []string{
		"tinyint(1) unsigned",
		"decimal(20,6) unsigned zerofill",
		"enum('new','in progress','done')",
		"set('a','b')",
		"varbinary(16)",
		"time(6)",
		"timestamp(6)",
		"point",
	}
	for index, column := range create.Columns {
		if column.SQLType != want[index] {
			t.Fatalf("column %s SQL type = %q, want %q", column.Name, column.SQLType, want[index])
		}
	}
}

func TestParseRecursiveDateCTE(t *testing.T) {
	query := `WITH RECURSIVE date_range AS (
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
	ORDER BY dr.dt`
	statement, err := Parse(query)
	if err != nil {
		t.Fatal(err)
	}
	cte, ok := statement.(WithRecursive)
	if !ok || cte.Name != "date_range" || cte.Recursive.Table != "date_range" || len(cte.Query.Joins) != 1 {
		t.Fatalf("unexpected recursive CTE: %#v", statement)
	}
}

func TestParseWindowExpressions(t *testing.T) {
	for _, expression := range []string{
		"ROW_NUMBER() OVER (PARTITION BY region ORDER BY amount DESC)",
		"RANK() OVER (ORDER BY score)",
		"COUNT(*) OVER (PARTITION BY region)",
		"SUM(amount) OVER (PARTITION BY region ORDER BY id)",
	} {
		parsed, err := ParseExpression(expression)
		if err != nil {
			t.Fatalf("ParseExpression(%q): %v", expression, err)
		}
		if _, ok := parsed.(WindowExpr); !ok {
			t.Fatalf("ParseExpression(%q) returned %T", expression, parsed)
		}
	}
}

func TestParseMySQLViewStatements(t *testing.T) {
	queries := []string{
		"CREATE VIEW active_users AS SELECT id,name FROM users WHERE enabled=TRUE",
		"CREATE OR REPLACE VIEW active_users(user_id,user_name) AS SELECT id,name FROM users WITH CASCADED CHECK OPTION",
		"CREATE ALGORITHM=UNDEFINED DEFINER=`root`@`%` SQL SECURITY DEFINER VIEW user_totals AS SELECT region,COUNT(*) AS total FROM users GROUP BY region",
		"ALTER VIEW active_users AS SELECT id,name FROM users WHERE enabled=TRUE",
	}
	for _, query := range queries {
		statement, err := Parse(query)
		if err != nil {
			t.Fatalf("Parse(%q): %v", query, err)
		}
		view, ok := statement.(CreateView)
		if !ok || view.Name == "" || view.Definition == "" {
			t.Fatalf("unexpected view statement: %#v", statement)
		}
		if strings.Contains(query, "ALGORITHM") != view.HasCreateOptions {
			t.Fatalf("CREATE options marker for %q = %v", query, view.HasCreateOptions)
		}
	}
	statement, err := Parse("DROP VIEW IF EXISTS active_users,user_totals")
	if err != nil {
		t.Fatal(err)
	}
	drop, ok := statement.(DropView)
	if !ok || !drop.IfExists || len(drop.Names) != 2 {
		t.Fatalf("unexpected DROP VIEW: %#v", statement)
	}
	if _, err := Parse("SHOW CREATE VIEW active_users"); err != nil {
		t.Fatal(err)
	}
	statement, err = Parse("SHOW CREATE DATABASE IF NOT EXISTS `export-db`")
	if err != nil {
		t.Fatal(err)
	}
	showDatabase, ok := statement.(Show)
	if !ok || showDatabase.What != "CREATE DATABASE IF NOT EXISTS" || showDatabase.Name != "export-db" {
		t.Fatalf("unexpected SHOW CREATE DATABASE: %#v", statement)
	}
	if _, err := Parse("SHOW FULL TABLES"); err != nil {
		t.Fatal(err)
	}
}
