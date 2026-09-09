package server

import (
	"context"
	"database/sql"
	"io"
	"log"
	"net"
	"testing"
	"time"

	"gbaselite/executor"
	"gbaselite/storage"
)

func BenchmarkMySQLConcurrentPrimaryKeySelect(b *testing.B) {
	engine, err := executor.Open(b.TempDir(), "root", "123456")
	if err != nil {
		b.Fatal(err)
	}
	database, err := engine.Store.CreateDatabase("benchmark_lookup")
	if err != nil {
		b.Fatal(err)
	}
	table, err := database.CreateTable("records", []storage.Column{{Name: "id", Type: storage.TypeInt}, {Name: "value", Type: storage.TypeVarchar, Length: 32}})
	if err != nil {
		b.Fatal(err)
	}
	if err := table.AddPrimaryKey([]string{"id"}); err != nil {
		b.Fatal(err)
	}
	for index := 0; index < 10_000; index++ {
		if err := table.Insert(storage.NewRow(storage.MustValue(storage.TypeInt, index), storage.MustValue(storage.TypeVarchar, "value"))); err != nil {
			b.Fatal(err)
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	databaseServer := &MySQLServer{Engine: engine, Logger: log.New(io.Discard, "", 0), MaxConnections: 64, WriteBufferSize: 8 << 10}
	done := make(chan error, 1)
	go func() { done <- databaseServer.Serve(listener) }()
	client, err := sql.Open("mysql", "root:123456@tcp("+listener.Addr().String()+")/benchmark_lookup?timeout=3s")
	if err != nil {
		b.Fatal(err)
	}
	client.SetMaxOpenConns(64)
	client.SetMaxIdleConns(64)
	b.Cleanup(func() {
		_ = client.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = databaseServer.Shutdown(ctx)
		<-done
	})
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			var id int
			var value string
			if err := client.QueryRow("SELECT id,value FROM records WHERE id=9999").Scan(&id, &value); err != nil {
				b.Error(err)
				return
			}
			if id != 9999 || value != "value" {
				b.Errorf("row = %d %q", id, value)
				return
			}
		}
	})
}
