package failover

import (
	"context"
	"database/sql"
	"gbaselite/executor"
	"gbaselite/server"
	"io"
	"log"
	"net"
	"testing"
	"time"
)

func stateBackend(t *testing.T) string {
	t.Helper()
	e, err := executor.Open(t.TempDir(), "root", "pw")
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		e.Close()
		t.Fatal(err)
	}
	srv := &server.MySQLServer{Engine: e, Logger: log.New(io.Discard, "", 0)}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(l) }()
	t.Cleanup(func() {
		l.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Error(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-ctx.Done():
			t.Error("backend did not stop")
		}
	})
	return l.Addr().String()
}

// A single physical connection: sql.Conn cannot hide a disconnect by reconnecting.
func stateConnection(t *testing.T, r *Router) *sql.Conn {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		client, err := l.Accept()
		if err == nil {
			defer client.Close()
			r.route(ctx, client)
		}
	}()
	db, err := sql.Open("mysql", "root:pw@tcp("+l.Addr().String()+")/?timeout=2s&readTimeout=3s&writeTimeout=3s")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		db.Close()
		l.Close()
		r.changeLeader("")
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("proxy connection did not stop")
		}
	})
	c, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	stateQuery(t, c)
	return c
}
func stateQuery(t *testing.T, c *sql.Conn) {
	t.Helper()
	var one int
	if err := c.QueryRowContext(context.Background(), "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatal(one, err)
	}
}
func stateRouter(t *testing.T, a, b string) *Router {
	t.Helper()
	r, err := New(Options{Peers: []Peer{{"a", a}, {"b", b}, {"c", b}}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func TestProxyKeepsLeaderAcrossTransientProbeFailure(t *testing.T) {
	a := stateBackend(t)
	r := stateRouter(t, a, a)
	available := true
	r.probe = func(_ context.Context, _ *sql.DB, p Peer) bool { return available && p.ID == "a" }
	dbs := make([]*sql.DB, 3)
	r.discover(context.Background(), dbs)
	c := stateConnection(t, r)
	available = false
	r.discover(context.Background(), dbs)
	if r.Leader() != a {
		t.Fatalf("transient probe failure cleared confirmed leader: %q", r.Leader())
	}
	stateQuery(t, c)
	available = true
	r.discover(context.Background(), dbs)
	if r.Leader() != a {
		t.Fatal("confirmation did not recover")
	}
	stateQuery(t, c)
}
