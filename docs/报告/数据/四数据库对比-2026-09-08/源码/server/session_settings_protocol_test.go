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

	_ "github.com/go-sql-driver/mysql"
)

func TestProtocolSessionInheritsTimeZoneAndAcceptsDSNCharset(t *testing.T) {
	engine, err := executor.Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	databaseServer := &MySQLServer{Engine: engine, Logger: log.New(io.Discard, "", 0), DefaultTimeZone: "+08:00"}
	done := make(chan error, 1)
	go func() { done <- databaseServer.Serve(listener) }()

	client, err := sql.Open("mysql", "root:123456@tcp("+listener.Addr().String()+")/?charset=latin1&timeout=3s")
	if err != nil {
		t.Fatal(err)
	}
	var zone, globalZone, charset, collation string
	if err := client.QueryRow("SELECT @@session.time_zone,@@global.time_zone,@@character_set_client,@@collation_connection").Scan(&zone, &globalZone, &charset, &collation); err != nil {
		t.Fatal(err)
	}
	if zone != "+08:00" || globalZone != "+08:00" || charset != "latin1" || collation != "latin1_swedish_ci" {
		t.Fatalf("protocol settings = zone %q global %q charset %q collation %q", zone, globalZone, charset, collation)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := databaseServer.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
