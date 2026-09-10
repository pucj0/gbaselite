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
type Router struct {
	probe       func(context.Context, *sql.DB, Peer) bool
	options     Options
	mutex       sync.Mutex
	leader      string
	connections map[net.Conn]string
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
func (r *Router) changeLeader(address string) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if r.leader == address {
		return
	}
	r.leader = address
	for c, backend := range r.connections {
		if backend != address {
			c.Close()
		}
	}
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
	current := r.Leader()
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
			r.changeLeader(r.options.Peers[i].Address)
			return
		}
		cancel()
		if ctx.Err() != nil {
			break
		}
	}
	r.changeLeader("")
}
func (r *Router) route(ctx context.Context, client net.Conn) {
	address := r.Leader()
	if address == "" {
		return
	}
	backend, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", address)
	if err != nil {
		return
	}
	defer backend.Close()
	r.mutex.Lock()
	if r.leader != address {
		r.mutex.Unlock()
		return
	}
	r.connections[client] = address
	r.mutex.Unlock()
	defer func() { r.mutex.Lock(); delete(r.connections, client); r.mutex.Unlock() }()
	done := make(chan struct{})
	go func() { io.CopyBuffer(backend, client, make([]byte, 32<<10)); backend.Close(); close(done) }()
	io.CopyBuffer(client, backend, make([]byte, 32<<10))
	client.Close()
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
