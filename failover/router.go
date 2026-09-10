// Package failover routes new MySQL connections to the quorum-confirmed leader.
// It never replays SQL and closes old backend connections after a role change.
package failover

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	driver "github.com/go-sql-driver/mysql"
	"io"
	"net"
	"sync"
	"time"
)

type Peer struct{ ID, Address string }
type Options struct {
	Peers              []Peer
	Username, Password string
	TLS                *tls.Config
	MaxConnections     int
}

// Three consecutive unsuccessful rounds confirm loss; a new confirmed leader
// replaces the old one immediately. No timer extends a failed round.
const leaderMissThreshold = 3

// Test hooks are installed before Serve, must not re-enter Router, and are nil
// in production. Transition hooks run under mutex to preserve event ordering.
type leaderTransition struct {
	From, To, Reason string
	Misses           int
	Generation       uint64
}
type connectionClose struct {
	Backend, Reason string
	Generation      uint64
	Err             error
}

type Router struct {
	transitionHook func(leaderTransition)
	connectionHook func(connectionClose)
	discovery      sync.Mutex
	misses         int
	generation     uint64
	probe          func(context.Context, *sql.DB, Peer) bool
	options        Options
	mutex          sync.Mutex
	leader         string
	connections    map[net.Conn]string
}

func New(options Options) (*Router, error) {
	if len(options.Peers) != 3 {
		return nil, fmt.Errorf("proxy requires three SQL peers")
	}
	seen := map[string]bool{}
	for _, p := range options.Peers {
		if p.ID == "" || seen[p.ID] {
			return nil, fmt.Errorf("duplicate or empty SQL peer ID")
		}
		seen[p.ID] = true
		host, _, err := net.SplitHostPort(p.Address)
		if err != nil {
			return nil, err
		}
		if options.TLS == nil {
			ip := net.ParseIP(host)
			if ip == nil || !ip.IsLoopback() {
				return nil, fmt.Errorf("remote SQL leader discovery requires TLS")
			}
		}
	}
	if options.MaxConnections == 0 {
		options.MaxConnections = 64
	}
	if options.MaxConnections < 1 || options.MaxConnections > 4096 {
		return nil, fmt.Errorf("proxy max connections must be 1..4096")
	}
	return &Router{probe: probeLeader, options: options, connections: make(map[net.Conn]string)}, nil
}
func (r *Router) Leader() string { r.mutex.Lock(); defer r.mutex.Unlock(); return r.leader }

// changeLeader forces a transition (including shutdown), bypassing miss grace.
func (r *Router) changeLeader(address string) {
	r.mutex.Lock()
	r.misses = 0
	reason := "confirmed-new-leader"
	if address == "" {
		reason = "shutdown"
	}
	stale := r.changeLeaderLocked(address, reason)
	r.mutex.Unlock()
	closeTransitionConnections(stale, reason)
}
func (r *Router) changeLeaderLocked(address, reason string) []net.Conn {
	from := r.leader
	if r.leader == address {
		r.traceTransition(from, address, reason)
		return nil
	}
	r.leader = address
	r.generation++
	r.traceTransition(from, address, reason)
	var stale []net.Conn
	for c, backend := range r.connections {
		if backend != address {
			stale = append(stale, c)
		}
	}
	return stale
}
func (r *Router) recordDiscovery(address string, generation uint64) {
	r.mutex.Lock()
	// An older probe must not undo a newer transition or shutdown.
	if r.generation != generation {
		r.traceTransition(r.leader, address, "stale-result-rejected")
		r.mutex.Unlock()
		return
	}
	if address != "" {
		r.misses = 0
	} else if r.leader != "" {
		r.misses++
		if r.misses < leaderMissThreshold {
			r.traceTransition(r.leader, r.leader, "probe-miss")
			r.mutex.Unlock()
			return
		}
	}
	reason := "confirmed-new-leader"
	if address == "" {
		reason = "confirmed-loss"
	} else if address == r.leader {
		reason = "confirmed-current-leader"
	} else if r.leader == "" {
		reason = "initial-discovery"
	}
	stale := r.changeLeaderLocked(address, reason)
	r.mutex.Unlock()
	closeTransitionConnections(stale, reason)
}

func (r *Router) traceTransition(from, to, reason string) {
	if r.transitionHook != nil {
		r.transitionHook(leaderTransition{from, to, reason, r.misses, r.generation})
	}
}
func closeTransitionConnections(connections []net.Conn, reason string) {
	for _, c := range connections {
		if routed, ok := c.(*routedConnection); ok {
			routed.closeBecause(reason, nil)
		} else {
			c.Close()
		}
	}
}

// Close both transport directions, including a copier blocked writing to the
// backend. Closing only the client can leave route and Serve waiting forever.
type routedConnection struct {
	net.Conn
	backend net.Conn
	once    sync.Once
	onClose func(string, error)
}

func (c *routedConnection) Close() error {
	return c.closeBecause("route-cleanup", nil)
}
func (c *routedConnection) closeBecause(reason string, cause error) error {
	var err error
	c.once.Do(func() {
		if c.onClose != nil {
			c.onClose(reason, cause)
		}
		err = errors.Join(c.Conn.Close(), c.backend.Close())
	})
	return err
}
func (r *Router) Serve(ctx context.Context, listener net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var databases []*sql.DB
	for _, peer := range r.options.Peers {
		config := driver.NewConfig()
		config.User = r.options.Username
		config.Passwd = r.options.Password
		config.Net = "tcp"
		config.Addr = peer.Address
		config.Timeout = time.Second
		config.ReadTimeout = time.Second
		config.WriteTimeout = time.Second
		if r.options.TLS != nil {
			config.TLS = r.options.TLS.Clone()
		}
		connector, err := driver.NewConnector(config)
		if err != nil {
			return err
		}
		db := sql.OpenDB(connector)
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		databases = append(databases, db)
		defer db.Close()
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		defer listener.Close()
		defer r.changeLeader("")
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			r.discover(ctx, databases)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	var connections sync.WaitGroup
	defer func() { cancel(); <-stopped; connections.Wait() }()
	slots := make(chan struct{}, r.options.MaxConnections)
	for {
		client, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		select {
		case slots <- struct{}{}:
		default:
			client.Close()
			continue
		}
		connections.Add(1)
		go func() {
			defer connections.Done()
			defer func() { <-slots }()
			defer client.Close()
			r.route(ctx, client)
		}()
	}
}
func (r *Router) discover(ctx context.Context, databases []*sql.DB) {
	r.discovery.Lock()
	defer r.discovery.Unlock()
	r.mutex.Lock()
	current, generation := r.leader, r.generation
	r.mutex.Unlock()
	order := make([]int, 0, 3)
	for i, p := range r.options.Peers {
		if p.Address == current {
			order = append(order, i)
		}
	}
	for i, p := range r.options.Peers {
		if p.Address != current {
			order = append(order, i)
		}
	}
	for _, i := range order {
		probe, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
		confirmed := r.probe(probe, databases[i], r.options.Peers[i])
		if confirmed {
			cancel()
			if ctx.Err() == nil {
				r.recordDiscovery(r.options.Peers[i].Address, generation)
			}
			return
		}
		cancel()
		if ctx.Err() != nil {
			break
		}
	}
	if ctx.Err() == nil {
		r.recordDiscovery("", generation)
	}
}
func (r *Router) route(ctx context.Context, client net.Conn) {
	r.mutex.Lock()
	address, generation := r.leader, r.generation
	r.mutex.Unlock()
	if address == "" {
		return
	}
	backend, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", address)
	if err != nil {
		return
	}
	connection := &routedConnection{Conn: client, backend: backend}
	if r.connectionHook != nil {
		connection.onClose = func(reason string, err error) { r.connectionHook(connectionClose{address, reason, generation, err}) }
	}
	defer connection.Close()
	r.mutex.Lock()
	if r.leader != address || r.generation != generation || ctx.Err() != nil {
		r.mutex.Unlock()
		return
	}
	r.connections[connection] = address
	r.mutex.Unlock()
	defer func() { r.mutex.Lock(); delete(r.connections, connection); r.mutex.Unlock() }()
	done := make(chan struct{})
	go func() {
		_, err := io.CopyBuffer(backend, client, make([]byte, 32<<10))
		reason := "client-to-backend-copy-ended"
		if ctx.Err() != nil {
			reason = "context-cancel"
		}
		connection.closeBecause(reason, err)
		close(done)
	}()
	_, copyErr := io.CopyBuffer(client, backend, make([]byte, 32<<10))
	reason := "backend-to-client-copy-ended"
	if ctx.Err() != nil {
		reason = "context-cancel"
	}
	connection.closeBecause(reason, copyErr)
	<-done
}

// probeLeader requires both role identity and a quorum-confirmed SQL read.
func probeLeader(ctx context.Context, db *sql.DB, peer Peer) bool {
	var id, state, leader, raftAddress string
	var applied uint64
	if err := db.QueryRowContext(ctx, "SHOW REPLICATION STATUS").Scan(&id, &state, &leader, &raftAddress, &applied); err != nil || state != "Leader" || id != peer.ID {
		return false
	}
	var one int
	return db.QueryRowContext(ctx, "SELECT 1").Scan(&one) == nil && one == 1
}
