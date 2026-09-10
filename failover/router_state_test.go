package failover

import (
	"context"
	"database/sql"
	"gbaselite/executor"
	"gbaselite/server"
	"io"
	"log"
	"net"
	"sync"
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
		// Join Serve before Shutdown reads its listener and waits for handlers.
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-ctx.Done():
			t.Error("backend did not stop")
			return
		}
		if err := srv.Shutdown(ctx); err != nil {
			t.Error(err)
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

func TestProxyConfirmsLeaderLossAfterConsecutiveMisses(t *testing.T) {
	a := stateBackend(t)
	r := stateRouter(t, a, a)
	available := true
	r.probe = func(_ context.Context, _ *sql.DB, p Peer) bool { return available && p.ID == "a" }
	dbs := make([]*sql.DB, 3)
	r.discover(context.Background(), dbs)
	c := stateConnection(t, r)
	for cycle := 0; cycle < 2; cycle++ {
		available = false
		for i := 1; i < leaderMissThreshold; i++ {
			r.discover(context.Background(), dbs)
			if r.Leader() != a {
				t.Fatal("cleared before confirmation")
			}
			stateQuery(t, c)
		}
		if cycle == 0 {
			available = true
			r.discover(context.Background(), dbs)
		}
	}
	r.discover(context.Background(), dbs)
	if r.Leader() != "" {
		t.Fatal("unavailable leader retained")
	}
	if err := c.PingContext(context.Background()); err == nil {
		t.Fatal("old connection survived confirmed loss")
	}
}
func TestProxyConfirmedLeaderChangeClosesOldConnection(t *testing.T) {
	a, b := stateBackend(t), stateBackend(t)
	r := stateRouter(t, a, b)
	leader := "a"
	r.probe = func(_ context.Context, _ *sql.DB, p Peer) bool { return p.ID == leader }
	dbs := make([]*sql.DB, 3)
	r.discover(context.Background(), dbs)
	old := stateConnection(t, r)
	leader = "b"
	r.discover(context.Background(), dbs)
	if r.Leader() != b {
		t.Fatal("confirmed successor not selected immediately")
	}
	if err := old.PingContext(context.Background()); err == nil {
		t.Fatal("old leader connection still usable")
	}
	stateQuery(t, stateConnection(t, r))
}
func TestProxyRejectsStaleDiscovery(t *testing.T) {
	r := stateRouter(t, "127.0.0.1:1", "127.0.0.1:2")
	r.changeLeader("127.0.0.1:1")
	old := r.generation
	r.changeLeader("127.0.0.1:2")
	r.recordDiscovery("127.0.0.1:1", old)
	if r.Leader() != "127.0.0.1:2" {
		t.Fatal("stale probe undid transition")
	}
}
func TestProxyTransitionUnblocksBackendWrite(t *testing.T) {
	r := stateRouter(t, "127.0.0.1:1", "127.0.0.1:2")
	r.changeLeader("127.0.0.1:1")
	client, remote := net.Pipe()
	backend, server := net.Pipe()
	defer remote.Close()
	defer server.Close()
	c := &routedConnection{Conn: client, backend: backend}
	defer c.Close()
	r.connections[c] = "127.0.0.1:1"
	done := make(chan error, 1)
	go func() { _, err := backend.Write([]byte("blocked")); done <- err }()
	r.changeLeader("127.0.0.1:2")
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("write succeeded with no receiver")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("backend writer leaked after leader change")
	}
}
func TestRouterConcurrentDiscoveryAndConnections(t *testing.T) {
	a := stateBackend(t)
	r := stateRouter(t, a, a)
	r.probe = func(_ context.Context, _ *sql.DB, p Peer) bool { return p.ID == "a" }
	r.discover(context.Background(), make([]*sql.DB, 3))
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Serve(ctx, l) }()
	var workers sync.WaitGroup
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for j := 0; j < 8; j++ {
				c, err := net.DialTimeout("tcp", l.Addr().String(), time.Second)
				if err == nil {
					c.Close()
				}
				r.Leader()
			}
		}()
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		for i := 0; i < 32; i++ {
			r.changeLeader("")
			r.discover(ctx, make([]*sql.DB, 3))
		}
	}()
	workers.Wait()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not close active transports")
	}
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if len(r.connections) != 0 || r.leader != "" {
		t.Fatal("shutdown retained connections or leader")
	}
}

func TestRouterTransitionDiagnostics(t *testing.T) {
	r := stateRouter(t, "127.0.0.1:1", "127.0.0.1:2")
	var events []leaderTransition
	r.transitionHook = func(e leaderTransition) { events = append(events, e) }
	r.recordDiscovery("127.0.0.1:1", 0)
	r.recordDiscovery("127.0.0.1:1", 1)
	r.recordDiscovery("127.0.0.1:2", 1)
	for i := 0; i < leaderMissThreshold; i++ {
		r.recordDiscovery("", 2)
	}
	r.recordDiscovery("127.0.0.1:1", 1)
	r.changeLeader("")
	want := []string{"initial-discovery", "confirmed-current-leader", "confirmed-new-leader", "probe-miss", "probe-miss", "confirmed-loss", "stale-result-rejected", "shutdown"}
	if len(events) != len(want) {
		t.Fatal(events)
	}
	for i, reason := range want {
		if events[i].Reason != reason {
			t.Fatalf("event %d: %+v", i, events[i])
		}
	}
	if e := events[5]; e.From != "127.0.0.1:2" || e.To != "" || e.Misses != 3 || e.Generation != 3 {
		t.Fatal(e)
	}
}
func TestRouterConnectionCloseDiagnosticsOnce(t *testing.T) {
	client, remote := net.Pipe()
	backend, server := net.Pipe()
	defer remote.Close()
	defer server.Close()
	var reasons []string
	c := &routedConnection{Conn: client, backend: backend, onClose: func(reason string, _ error) { reasons = append(reasons, reason) }}
	c.closeBecause("confirmed-new-leader", nil)
	c.closeBecause("backend-to-client-copy-ended", net.ErrClosed)
	c.Close()
	if len(reasons) != 1 || reasons[0] != "confirmed-new-leader" {
		t.Fatal(reasons)
	}
}
