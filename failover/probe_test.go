package failover

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	mysql "github.com/go-sql-driver/mysql"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

type probeConnector struct {
	mutex                          sync.Mutex
	acquireErr, statusErr, pingErr error
	state                          string
	opens                          int
	connections                    []int
}

func (d *probeConnector) Connect(context.Context) (driver.Conn, error) {
	d.mutex.Lock()
	defer d.mutex.Unlock()
	if d.acquireErr != nil {
		return nil, d.acquireErr
	}
	d.opens++
	return &probeConn{owner: d, id: d.opens}, nil
}
func (d *probeConnector) Driver() driver.Driver { return probeDriver{} }

type probeDriver struct{}

func (probeDriver) Open(string) (driver.Conn, error) { return nil, errors.New("use connector") }

type probeConn struct {
	owner *probeConnector
	id    int
}

func (*probeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unexpected Prepare") }
func (*probeConn) Close() error                        { return nil }
func (*probeConn) Begin() (driver.Tx, error)           { return nil, errors.New("unexpected Begin") }
func (c *probeConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	d := c.owner
	d.mutex.Lock()
	defer d.mutex.Unlock()
	d.connections = append(d.connections, c.id)
	switch q {
	case "SHOW REPLICATION STATUS":
		if d.statusErr != nil {
			return nil, d.statusErr
		}
		return &probeRows{values: []driver.Value{"0", d.state, "0", "raft:1", int64(7)}}, nil
	case "SELECT 1":
		if d.pingErr != nil {
			return nil, d.pingErr
		}
		return &probeRows{values: []driver.Value{int64(1)}}, nil
	}
	return nil, fmt.Errorf("unexpected query %q", q)
}

type probeRows struct {
	values []driver.Value
	done   bool
}

func (r *probeRows) Columns() []string { return make([]string, len(r.values)) }
func (*probeRows) Close() error        { return nil }
func (r *probeRows) Next(dst []driver.Value) error {
	if r.done {
		return io.EOF
	}
	copy(dst, r.values)
	r.done = true
	return nil
}
func TestProbeLeaderDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, stage, class string
		connector          *probeConnector
	}{
		{"acquire", "acquire-conn", "context-deadline", &probeConnector{acquireErr: context.DeadlineExceeded}},
		{"status", "show-replication-status", "unexpected-eof", &probeConnector{statusErr: io.ErrUnexpectedEOF}},
		{"role", "role-mismatch", "", &probeConnector{state: "Follower"}},
		{"verification", "show-replication-status", "sql-error", &probeConnector{state: "Leader", statusErr: &mysql.MySQLError{Number: 1105, Message: "leadership unavailable"}}},
		{"success", "confirmed", "", &probeConnector{state: "Leader"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := sql.OpenDB(tc.connector)
			defer db.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
			defer cancel()
			result := probeLeader(ctx, db, Peer{"0", "sql:1"})
			if result.Stage != tc.stage || result.ErrClass != tc.class || result.Confirmed != (tc.stage == "confirmed") {
				t.Fatalf("%+v", result)
			}
			if result.PeerID != "0" || result.Address != "sql:1" || result.TotalDuration < result.AcquireDuration+result.StatusDuration {
				t.Fatalf("invalid diagnostics: %+v", result)
			}
			if tc.stage == "role-mismatch" && (result.State != "Follower" || result.Leader != "0" || result.Applied != 7) {
				t.Fatal(result)
			}
			if tc.stage == "confirmed" && result.RemainingBudget <= 0 {
				t.Fatal(result)
			}
		})
	}
}
func TestProbeUsesSingleConnection(t *testing.T) {
	d := &probeConnector{state: "Leader"}
	db := sql.OpenDB(d)
	defer db.Close()
	// One probe must perform exactly one SQL roundtrip and one checkout.
	db.SetMaxIdleConns(0)
	if result := probeLeader(context.Background(), db, Peer{"0", "sql:1"}); !result.Confirmed {
		t.Fatal(result)
	}
	if d.opens != 1 || len(d.connections) != 1 {
		t.Fatalf("opens=%d queries=%v", d.opens, d.connections)
	}
}
func TestProbeErrorClasses(t *testing.T) {
	for _, tc := range []struct {
		err   error
		class string
	}{
		{context.DeadlineExceeded, "context-deadline"}, {context.Canceled, "context-canceled"},
		{driver.ErrBadConn, "driver-bad-connection"}, {&net.DNSError{IsTimeout: true}, "network-timeout"},
		{net.ErrClosed, "connection-closed"}, {io.ErrUnexpectedEOF, "unexpected-eof"},
		{&mysql.MySQLError{Number: 1105}, "sql-error"}, {errors.New("other"), "unknown"},
	} {
		if got := probeErrorClass(fmt.Errorf("wrapped: %w", tc.err)); got != tc.class {
			t.Fatalf("%v: %s", tc.err, got)
		}
	}
}
func TestDiscoveryRoundDiagnostics(t *testing.T) {
	r := stateRouter(t, "127.0.0.1:1", "127.0.0.1:2")
	var events []discoveryRound
	r.discoveryHook = func(e discoveryRound) { events = append(events, e) }
	r.probe = func(_ context.Context, _ *sql.DB, p Peer) probeResult {
		return probeResult{PeerID: p.ID, Address: p.Address, Stage: "role-mismatch", State: "Follower"}
	}
	r.discover(context.Background(), make([]*sql.DB, 3))
	if len(events) != 2 || events[0].Result != "start" || events[1].Result != "miss" || events[1].ID != 1 || len(events[1].Probes) != 3 || events[1].End.Before(events[1].Start) {
		t.Fatal(events)
	}
	for i, p := range events[1].Probes {
		if p.PeerID != r.options.Peers[i].ID {
			t.Fatal(events)
		}
	}
}
