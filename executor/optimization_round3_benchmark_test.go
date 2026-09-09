package executor

import (
	"fmt"
	"gbaselite/storage"
	"testing"
)

func round3BenchmarkEngine(b *testing.B) (*legacyEngine, *Session) {
	b.Helper()
	e, err := openLegacy(b.TempDir(), "root", "secret")
	if err != nil {
		b.Fatal(err)
	}
	db, err := e.Store.CreateDatabase("bench")
	if err != nil {
		b.Fatal(err)
	}
	table, err := db.CreateTable("items", []storage.Column{{Name: "id", Type: storage.TypeInt}, {Name: "score", Type: storage.TypeInt}, {Name: "payload", Type: storage.TypeVarchar, Length: 40}})
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 10000; i++ {
		if err := table.Insert(storage.NewRow(storage.MustValue(storage.TypeInt, i), storage.MustValue(storage.TypeInt, (i*97)%10000), storage.MustValue(storage.TypeVarchar, "payload"))); err != nil {
			b.Fatal(err)
		}
	}
	if err := table.AddPrimaryKey([]string{"id"}); err != nil {
		b.Fatal(err)
	}
	if err := table.AddIndex("scores", []string{"score"}, false); err != nil {
		b.Fatal(err)
	}
	s := &Session{CurrentDatabase: "bench"}
	if _, err := e.Execute(s, "BEGIN"); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { e.CloseSession(s); e.Close() })
	return e, s
}
func BenchmarkLegacyRound3PlainUpdate(b *testing.B) {
	e, s := round3BenchmarkEngine(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := e.Execute(s, fmt.Sprintf("UPDATE items SET payload='changed' WHERE id=%d", i%10000)); err != nil {
			b.Fatal(err)
		}
	}
}
func BenchmarkLegacyRound3IndexedUpdate(b *testing.B) {
	e, s := round3BenchmarkEngine(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := e.Execute(s, fmt.Sprintf("UPDATE items SET score=score+1 WHERE id=%d", i%10000)); err != nil {
			b.Fatal(err)
		}
	}
}
func BenchmarkLegacyRound3TopK(b *testing.B) {
	e, s := round3BenchmarkEngine(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r, err := e.Execute(s, "SELECT id FROM items ORDER BY payload,score DESC LIMIT 20 OFFSET 5")
		if err != nil || len(r.Rows) != 20 {
			b.Fatalf("result %v %v", r, err)
		}
	}
}
