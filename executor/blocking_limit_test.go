package executor

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestGroupedZeroLimitPreservesResourceErrors(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE drain_groups(v INT)")
	var values []string
	for i := 0; i < 40; i++ {
		values = append(values, fmt.Sprintf("(%d)", i))
	}
	run("INSERT INTO drain_groups VALUES" + strings.Join(values, ","))
	e.QueryOptions.ResultMemoryBytes = 1024
	for _, q := range []string{"SELECT v,COUNT(*) FROM drain_groups GROUP BY v LIMIT 0", "SELECT v,COUNT(*) FROM drain_groups GROUP BY v ORDER BY v LIMIT 0"} {
		if _, err := e.Execute(s, q); !errors.Is(err, ErrQueryResourceLimit) {
			t.Fatal(q, err)
		}
	}
}
