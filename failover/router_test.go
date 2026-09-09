package failover_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"gbaselite/executor"
	"gbaselite/failover"
	"gbaselite/server"
	"gbaselite/storageengine"
	"github.com/go-sql-driver/mysql"
	"io"
	"log"
	"net"
	"testing"
	"time"
)

func TestProxyRoutesAfterLeaderFailure(t *testing.T) {
	var peers []storageengine.Peer
	for i := 0; i < 3; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		peers = append(peers, storageengine.Peer{ID: fmt.Sprint(i), Address: l.Addr().String()})
		l.Close()
	}
	engines := make([]*executor.Engine, 3)
	servers := make([]*server.MySQLServer, 3)
	listeners := make([]net.Listener, 3)
	done := make([]chan error, 3)
	var sqlPeers []failover.Peer
	defer func() {
		for i, s := range servers {
			if s != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				s.Shutdown(ctx)
				cancel()
				<-done[i]
			}
		}
	}()
	for i := 0; i < 3; i++ {
		e, err := executor.OpenWithOptions(t.TempDir(), "root", "pw", executor.OpenOptions{StorageMode: "mvcc", Replication: &storageengine.ReplicationOptions{ID: peers[i].ID, Bind: peers[i].Address, Peers: peers, Bootstrap: i == 0}})
		if err != nil {
			t.Fatal(err)
		}
		engines[i] = e
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		listeners[i] = l
		srv := &server.MySQLServer{Engine: e, Logger: log.New(io.Discard, "", 0)}
		servers[i] = srv
		done[i] = make(chan error, 1)
		go func(i int) { done[i] <- servers[i].Serve(listeners[i]) }(i)
		sqlPeers = append(sqlPeers, failover.Peer{ID: peers[i].ID, Address: l.Addr().String()})
	}
	router, err := failover.New(failover.Options{Peers: sqlPeers, Username: "root", Password: "pw", MaxConnections: 8})
	if err != nil {
		t.Fatal(err)
	}
	front, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	proxyDone := make(chan error, 1)
	go func() { proxyDone <- router.Serve(ctx, front) }()
	defer func() {
		cancel()
		if err := <-proxyDone; err != nil {
			t.Error(err)
		}
	}()
	await := func(previous string) string {
		deadline := time.Now().Add(12 * time.Second)
		for time.Now().Before(deadline) {
			address := router.Leader()
			if address != "" && address != previous {
				return address
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatal("proxy did not find new leader")
		return ""
	}
	first := await("")
	open := func() *sql.DB {
		db, err := sql.Open("mysql", "root:pw@tcp("+front.Addr().String()+")/?timeout=2s&readTimeout=3s&writeTimeout=3s")
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		return db
	}
	db := open()
	defer db.Close()
	// Leader discovery may close a connection while the initial cluster is
	// converging, especially on a busy CI host. Retry only these idempotent
	// fixture statements using a fresh connection; the proxy never replays SQL.
	execFixture := func(query string) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for {
			if _, err = db.Exec(query); err == nil {
				return
			}
			// A disconnected INSERT may already have committed. The fixed primary
			// keys make retry duplicates safe; the final count and sum verify values.
			var sqlErr *mysql.MySQLError
			if errors.As(err, &sqlErr) && sqlErr.Number == 1062 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("fixture %s: %v", query, err)
			}
			db.Close()
			time.Sleep(100 * time.Millisecond)
			db = open()
		}
	}
	defer func() { db.Close() }()
	for _, q := range []string{"CREATE DATABASE IF NOT EXISTS test", "CREATE TABLE IF NOT EXISTS test.items(id INT PRIMARY KEY,v INT)", "INSERT INTO test.items VALUES(1,10)"} {
		execFixture(q)
	}
	for i, p := range sqlPeers {
		if p.Address == first {
			listeners[i].Close()
			if err = engines[i].Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
	await(first)
	db.Close()
	db = open()
	defer db.Close()
	execFixture("INSERT INTO test.items VALUES(2,20)")
	var count, sum int
	if err = db.QueryRow("SELECT COUNT(*),SUM(v) FROM test.items").Scan(&count, &sum); err != nil {
		t.Fatal(err)
	}
	if count != 2 || sum != 30 {
		t.Fatalf("lost data: %d %d", count, sum)
	}
}
