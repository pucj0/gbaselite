package executor

import "testing"

func TestMVCCUpdateIndexHalloweenProtection(t *testing.T) {
	_, _, run := rangeTestEngine(t)
	run("CREATE TABLE halloween(id INT PRIMARY KEY, k INT, v INT, KEY k_idx(k))")
	run("INSERT INTO halloween VALUES (1,1,10),(2,2,20),(3,99,30)")
	res := run("UPDATE halloween SET k=k+1 WHERE k<3")
	if res.AffectedRows != 2 {
		t.Fatalf("affected rows=%d", res.AffectedRows)
	}
	rows := run("SELECT id,k FROM halloween ORDER BY id")
	if got := rows.Rows; len(got) != 3 || got[0][1] != int64(2) || got[1][1] != int64(3) || got[2][1] != int64(99) {
		t.Fatalf("rows=%v", got)
	}
}
