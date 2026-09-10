package executor

import (
	"strings"
	"testing"
)

func TestExplainDescribesBoundPipeline(t *testing.T) {
	_, _, run := rangeTestEngine(t)
	run("CREATE TABLE plan_tree(id INT PRIMARY KEY,v INT)")
	for _, c := range []struct {
		sql   string
		nodes []string
	}{
		{"SELECT v FROM plan_tree", []string{"Projection(", "Scan"}},
		{"SELECT v FROM plan_tree WHERE v>0 ORDER BY v LIMIT 2", []string{"Filter(", "TopN(", "Projection("}},
		{"SELECT DISTINCT v FROM plan_tree LIMIT 2", []string{"Distinct(", "Limit("}},
		{"SELECT v,COUNT(*) FROM plan_tree GROUP BY v", []string{"Aggregate(", "Projection("}},
		{"SELECT a.id FROM plan_tree a JOIN plan_tree b ON a.id=b.id", []string{"Join(", "DynamicScan"}},
		{"SELECT ROW_NUMBER() OVER(ORDER BY v) FROM plan_tree", []string{"Window("}},
	} {
		r := run("EXPLAIN " + c.sql)
		for _, name := range c.nodes {
			if !strings.Contains(r.Rows[0][11].(string), name) {
				t.Fatal(c.sql, r.Rows)
			}
		}
	}
}
