package executor

import (
	"fmt"
	"strings"
	"testing"
)

func TestMVCCJoinAndGroupSnapshot(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE users(id INT PRIMARY KEY,name VARCHAR(20))")
	run("CREATE TABLE orders(id INT PRIMARY KEY,uid INT,amount DECIMAL(12,2),KEY uid_idx(uid))")
	run("INSERT INTO users VALUES(1,'a'),(2,'b'),(3,'c')")
	run("INSERT INTO orders VALUES(10,1,1.25),(11,1,2.75),(12,2,NULL)")
	cases := []struct{ q, want string }{
		{"SELECT u.id,o.id FROM users u INNER JOIN orders o ON u.id=o.uid ORDER BY u.id,o.id", "[[1 10] [1 11] [2 12]]"},
		{"SELECT u.id,o.id FROM users u LEFT JOIN orders o ON u.id=o.uid AND o.id<12 ORDER BY u.id,o.id", "[[1 10] [1 11] [2 <nil>] [3 <nil>]]"},
		{"SELECT u.id,o.id FROM users u LEFT JOIN orders o ON u.id=o.uid WHERE o.id<12 ORDER BY u.id,o.id", "[[1 10] [1 11]]"},
		{"SELECT u.id,COUNT(o.id),SUM(o.amount) FROM users u LEFT JOIN orders o ON u.id=o.uid GROUP BY u.id ORDER BY u.id", "[[1 2 4.00] [2 1 <nil>] [3 0 <nil>]]"},
		{"SELECT uid,COUNT(*) AS n FROM orders GROUP BY uid HAVING n>1 ORDER BY uid LIMIT 1", "[[1 2]]"},
	}
	for _, c := range cases {
		r := run(c.q)
		if got := fmt.Sprint(r.Rows); got != c.want {
			t.Fatal(c.q, got, c.want)
		}
	}
	if r := run("EXPLAIN SELECT u.id,o.id FROM users u LEFT JOIN orders o ON u.id=o.uid"); len(r.Rows) != 2 {
		t.Fatal(r.Rows)
	}

	for _, q := range []string{
		"EXPLAIN SELECT uid,COUNT(*) AS n FROM orders GROUP BY missing",
		"EXPLAIN SELECT uid,COUNT(*) AS n FROM orders GROUP BY uid HAVING missing>0",
		"EXPLAIN SELECT u.id FROM users u LEFT JOIN orders o ON u.id=o.uid ORDER BY missing",
	} {
		if _, err := e.Execute(s, q); err == nil {
			t.Fatal("EXPLAIN accepted unknown column", q)
		}
	}
	if r := run("EXPLAIN SELECT uid,COUNT(*) AS n FROM orders GROUP BY uid HAVING n>0 ORDER BY n"); !strings.Contains(fmt.Sprint(r.Rows), "Using filesort") {
		t.Fatal(r.Rows)
	}
	if r := run("SELECT DISTINCT COUNT(*) AS n FROM users GROUP BY id ORDER BY n LIMIT 1 OFFSET 1"); len(r.Rows) != 0 {
		t.Fatal("distinct must precede limit", r.Rows)
	}
	run("BEGIN")
	run("SELECT COUNT(*) FROM orders")
	other := &Session{CurrentDatabase: "test"}
	if _, err := e.Execute(other, "INSERT INTO orders VALUES(13,3,5.00)"); err != nil {
		t.Fatal(err)
	}
	if r := run(cases[3].q); fmt.Sprint(r.Rows) != cases[3].want {
		t.Fatal("join changed snapshot", r.Rows)
	}
	run("INSERT INTO orders VALUES(14,3,6.00)")
	if r := run("SELECT u.id,COUNT(o.id) FROM users u LEFT JOIN orders o ON u.id=o.uid GROUP BY u.id ORDER BY u.id"); fmt.Sprint(r.Rows) != "[[1 2] [2 1] [3 1]]" {
		t.Fatal(r.Rows)
	}
	run("ROLLBACK")
	_ = s
}
