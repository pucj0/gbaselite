package failover

import (
	"context"
	"database/sql"
	"fmt"
	"gbaselite/executor"
	"gbaselite/server"
	"gbaselite/storageengine"
	"io"
	"log"
	"net"
	"strings"
	"sync"
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
	var sqlPeers []Peer
	defer func() {
		for i, s := range servers {
			if s != nil {
				listeners[i].Close()
				<-done[i]
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				s.Shutdown(ctx)
				cancel()
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
		sqlPeers = append(sqlPeers, Peer{ID: peers[i].ID, Address: l.Addr().String()})
	}
	router, err := New(Options{Peers: sqlPeers, Username: "root", Password: "pw", MaxConnections: 8})
	if err != nil {
		t.Fatal(err)
	}
	history := &routerHistory{}
	stable := newStableLeader()
	router.transitionHook = func(e leaderTransition) { history.transition(e); stable.observe(e) }
	router.discoveryHook = history.round
	router.connectionHook = history.connection
	t.Cleanup(func() {
		if t.Failed() {
			t.Log("router diagnostics:\n" + history.String())
		}
	})
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
	first, firstGeneration := awaitStableLeader(t, ctx, router, stable, "")
	history.add(fmt.Sprintf("phase=fixture leader=%q gen=%d", first, firstGeneration))
	open := func() *sql.Conn {
		db, err := sql.Open("mysql", "root:pw@tcp("+front.Addr().String()+")/?timeout=2s&writeTimeout=3s")
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(0)
		t.Cleanup(func() { db.Close() })
		connectCtx, connectCancel := context.WithTimeout(ctx, 2*time.Second)
		defer connectCancel()
		connection, err := db.Conn(connectCtx)
		if err != nil {
			t.Fatal(err)
		}
		return connection
	}
	db := open()
	defer db.Close()
	// Do not retry fixture SQL or accept duplicate-key errors: every write below
	// must succeed once, including on a busy Windows runner.
	// The integration test is not a 3-second SQL latency SLA. A statement can
	// pass through two existing 5-second read barriers and replicated apply.
	// Bound each whole statement, separately from the unchanged 750ms probe;
	// use a pinned connection so database/sql cannot retry a fixture statement.
	const fixtureSQLTimeout = 15 * time.Second
	execFixture := func(query string) {
		t.Helper()
		queryCtx, queryCancel := context.WithTimeout(ctx, fixtureSQLTimeout)
		defer queryCancel()
		started := time.Now()
		_, err := db.ExecContext(queryCtx, query)
		history.add(fmt.Sprintf("fixture=%q duration=%s err=%v", query, time.Since(started), err))
		if err != nil {
			t.Fatalf("fixture %s: %v", query, err)
		}
	}
	for _, q := range []string{"CREATE DATABASE IF NOT EXISTS test", "CREATE TABLE IF NOT EXISTS test.items(id INT PRIMARY KEY,v INT)", "INSERT INTO test.items VALUES(1,10)"} {
		execFixture(q)
	}
	router.mutex.Lock()
	fixtureGeneration := router.generation
	router.mutex.Unlock()
	if fixtureGeneration != firstGeneration {
		t.Fatalf("fixture generation changed: %d -> %d", firstGeneration, fixtureGeneration)
	}
	history.add("phase=real-leader-shutdown")
	for i, p := range sqlPeers {
		if p.Address == first {
			listeners[i].Close()
			if err = engines[i].Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
	awaitStableLeader(t, ctx, router, stable, first)
	history.add("phase=reconnect-and-verify")
	db.Close()
	db = open()
	defer db.Close()
	execFixture("INSERT INTO test.items VALUES(2,20)")
	var count, sum, minID, maxID int
	queryCtx, queryCancel := context.WithTimeout(ctx, fixtureSQLTimeout)
	defer queryCancel()
	if err = db.QueryRowContext(queryCtx, "SELECT COUNT(*),SUM(v),MIN(id),MAX(id) FROM test.items").Scan(&count, &sum, &minID, &maxID); err != nil {
		t.Fatal(err)
	}
	if count != 2 || sum != 30 || minID != 1 || maxID != 2 {
		t.Fatalf("lost or duplicated data: count=%d sum=%d keys=%d..%d", count, sum, minID, maxID)
	}
}

// A bounded, synchronized history only used by tests; no production logging.
type routerHistory struct {
	mutex  sync.Mutex
	events []string
}

func (h *routerHistory) add(s string) {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	if len(h.events) == 256 {
		h.events = h.events[1:]
	}
	h.events = append(h.events, s)
}
func (h *routerHistory) transition(e leaderTransition) {
	h.add(fmt.Sprintf("gen=%d %q -> %q reason=%s misses=%d", e.Generation, e.From, e.To, e.Reason, e.Misses))
}
func (h *routerHistory) connection(e connectionClose) {
	h.add(fmt.Sprintf("connection gen=%d backend=%q close=%s err=%v", e.Generation, e.Backend, e.Reason, e.Err))
}
func (h *routerHistory) String() string {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	return strings.Join(h.events, "\n")
}

func (h *routerHistory) round(e discoveryRound) {
	if e.Result == "start" {
		h.add(fmt.Sprintf("round=%d start=%s", e.ID, e.Start.Format(time.RFC3339Nano)))
		return
	}
	for _, p := range e.Probes {
		h.add(fmt.Sprintf("round=%d peer=%s address=%s stage=%s reportedID=%s state=%s leader=%s raft=%s applied=%d acquire=%s status-and-verify=%s remaining=%s total=%s class=%s err=%v", e.ID, p.PeerID, p.Address, p.Stage, p.ReportedID, p.State, p.Leader, p.RaftAddress, p.Applied, p.AcquireDuration, p.StatusDuration, p.RemainingBudget, p.TotalDuration, p.ErrClass, p.Err))
	}
	h.add(fmt.Sprintf("round=%d end=%s result=%s duration=%s", e.ID, e.End.Format(time.RFC3339Nano), e.Result, e.End.Sub(e.Start)))
}
