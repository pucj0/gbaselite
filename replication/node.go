package replication

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"gbaselite/mvcc"
	"gbaselite/storageengine"
	"github.com/hashicorp/raft"
	raftbolt "github.com/hashicorp/raft-boltdb/v2"
	bolt "go.etcd.io/bbolt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

var ErrNotLeader = storageengine.ErrNotLeader

type Peer struct {
	ID      string
	Address string
}
type Options struct {
	ID, Bind, Advertise, Directory string
	Peers                          []Peer
	Bootstrap                      bool
	TLSCert, TLSKey, TLSCA         string
}
type Node struct {
	fatalSignal chan struct{}
	stopped     chan struct{}
	closeOnce   sync.Once
	closeErr    error
	id          string
	raft        *raft.Raft
	logs        *raftbolt.BoltStore
	transport   *raft.NetworkTransport
	store       *mvcc.Store
	failure     sync.Mutex
	fatal       error
}
type Status struct {
	ID, State, Leader, LeaderID string
	Applied                     uint64
}

func Open(store *mvcc.Store, options Options) (*Node, error) {
	if options.ID == "" || options.Bind == "" || options.Directory == "" {
		return nil, errors.New("replication requires node ID, bind and directory")
	}
	if len(options.Peers) != 3 {
		return nil, errors.New("replication requires exactly three distinct configured voting peers")
	}
	ids, addresses := map[string]bool{}, map[string]bool{}
	found := false
	for _, peer := range options.Peers {
		if peer.ID == "" || ids[peer.ID] || addresses[peer.Address] {
			return nil, errors.New("duplicate or empty replication peer")
		}
		if _, _, err := net.SplitHostPort(peer.Address); err != nil {
			return nil, err
		}
		ids[peer.ID], addresses[peer.Address] = true, true
		if peer.ID == options.ID {
			found = true
			if options.Advertise == "" {
				options.Advertise = peer.Address
			} else if options.Advertise != peer.Address {
				return nil, errors.New("local peer address differs from advertised address")
			}
		}
	}
	if !found {
		return nil, errors.New("local node missing from configured peers")
	}
	if err := os.MkdirAll(options.Directory, 0700); err != nil {
		return nil, err
	}
	stream, err := openStream(options)
	if err != nil {
		return nil, err
	}
	transport := raft.NewNetworkTransport(stream, 2, time.Second, io.Discard)
	logs, err := raftbolt.New(raftbolt.Options{Path: filepath.Join(options.Directory, "raft.db"), BoltOptions: &bolt.Options{Timeout: time.Second, NoFreelistSync: true}})
	if err != nil {
		transport.Close()
		return nil, err
	}
	snapshots, err := raft.NewFileSnapshotStore(options.Directory, 2, io.Discard)
	if err != nil {
		logs.Close()
		transport.Close()
		return nil, err
	}
	config := raft.DefaultConfig()
	config.LocalID = raft.ServerID(options.ID)
	config.LogOutput = io.Discard
	config.HeartbeatTimeout = 500 * time.Millisecond
	config.ElectionTimeout = 750 * time.Millisecond
	config.LeaderLeaseTimeout = 250 * time.Millisecond
	config.CommitTimeout = 50 * time.Millisecond
	config.MaxAppendEntries = 16
	config.SnapshotThreshold = 2048
	config.TrailingLogs = 256
	node := &Node{fatalSignal: make(chan struct{}, 1), stopped: make(chan struct{}), id: options.ID, store: store, logs: logs, transport: transport}
	exists, err := raft.HasExistingState(logs, logs, snapshots)
	if err != nil {
		node.Close()
		return nil, err
	}
	if !exists {
		head, err := store.Head()
		if err != nil {
			node.Close()
			return nil, err
		}
		if head != 0 {
			node.Close()
			return nil, errors.New("initialize replication with an empty MVCC database; migrate through leader SQL")
		}
	}
	if err := validateIdentity(options); err != nil {
		node.Close()
		return nil, err
	}
	node.raft, err = raft.NewRaft(config, node, logs, logs, snapshots, transport)
	if err != nil {
		node.Close()
		return nil, err
	}
	go func() {
		select {
		case <-node.fatalSignal:
			_ = node.raft.Shutdown().Error()
		case <-node.stopped:
		}
	}()
	if options.Bootstrap && !exists {
		var peers []raft.Server
		for _, peer := range options.Peers {
			peers = append(peers, raft.Server{ID: raft.ServerID(peer.ID), Address: raft.ServerAddress(peer.Address), Suffrage: raft.Voter})
		}
		if err := node.raft.BootstrapCluster(raft.Configuration{Servers: peers}).Error(); err != nil {
			node.Close()
			return nil, err
		}
	}
	return node, nil
}
func (n *Node) Close() error { n.closeOnce.Do(func() { n.closeErr = n.close() }); return n.closeErr }
func (n *Node) close() error {
	if n.stopped != nil {
		close(n.stopped)
	}
	var err error
	if n.raft != nil {
		err = n.raft.Shutdown().Error()
	}
	if n.transport != nil {
		err = errors.Join(err, n.transport.Close())
	}
	if n.logs != nil {
		err = errors.Join(err, n.logs.Close())
	}
	return err
}
func (n *Node) Status() Status {
	address, id := n.raft.LeaderWithID()
	applied, _ := n.store.Applied()
	return Status{ID: n.id, State: n.raft.State().String(), Leader: string(address), LeaderID: string(id), Applied: applied}
}
func (n *Node) check() error {
	n.failure.Lock()
	defer n.failure.Unlock()
	if n.fatal != nil {
		return n.fatal
	}
	if n.raft.State() != raft.Leader {
		return fmt.Errorf("%w; raft leader=%s", ErrNotLeader, n.Status().Leader)
	}
	return nil
}
func wait(ctx context.Context, future raft.Future) error {
	done := make(chan error, 1)
	go func() { done <- future.Error() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
func timeout(ctx context.Context) time.Duration {
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining < time.Millisecond {
			return time.Millisecond
		}
		return remaining
	}
	return 5 * time.Second
}
func (n *Node) Barrier(ctx context.Context) error {
	if err := n.check(); err != nil {
		return err
	}
	if err := wait(ctx, n.raft.VerifyLeader()); err != nil {
		return err
	}
	return wait(ctx, n.raft.Barrier(timeout(ctx)))
}
func (n *Node) Propose(ctx context.Context, command mvcc.Command) (mvcc.Result, error) {
	if err := ctx.Err(); err != nil {
		return mvcc.Result{}, err
	}
	if err := n.check(); err != nil {
		return mvcc.Result{}, err
	}
	if command.Term != 0 && command.Term != n.Term() {
		return mvcc.Result{}, mvcc.ErrConflict
	}
	encoded, err := json.Marshal(command)
	if err != nil {
		return mvcc.Result{}, err
	}
	if len(encoded) > 128<<10 {
		return mvcc.Result{}, errors.New("replication command exceeds 128 KiB")
	}
	future := n.raft.Apply(encoded, timeout(ctx))
	if err = wait(ctx, future); err != nil {
		return mvcc.Result{}, err
	}
	response, ok := future.Response().(mvcc.Result)
	if !ok {
		return mvcc.Result{}, errors.New("invalid replication response")
	}
	return response, response.Err()
}
func (n *Node) Apply(log *raft.Log) interface{} {
	var command mvcc.Command
	if err := json.Unmarshal(log.Data, &command); err != nil {
		return n.fail(err)
	}
	n.failure.Lock()
	failure := n.fatal
	n.failure.Unlock()
	if failure != nil {
		return mvcc.Result{Error: failure.Error()}
	}
	if command.Term != 0 && command.Term != log.Term {
		return mvcc.Result{Error: mvcc.ErrConflict.Error()}
	}
	result, err := n.store.Apply(log.Index, command)
	if err != nil {
		return n.fail(err)
	}
	return result
}
func (n *Node) fail(err error) mvcc.Result {
	n.failure.Lock()
	n.fatal = fmt.Errorf("replicated storage unavailable: %w", err)
	n.failure.Unlock()
	select {
	case n.fatalSignal <- struct{}{}:
	default:
	}
	return mvcc.Result{Error: err.Error()}
}

type snapshot struct{ tx *mvcc.Snapshot }

func (n *Node) Snapshot() (raft.FSMSnapshot, error) {
	n.failure.Lock()
	failure := n.fatal
	n.failure.Unlock()
	if failure != nil {
		return nil, failure
	}
	tx, err := n.store.Snapshot()
	if err != nil {
		return nil, err
	}
	return &snapshot{tx: tx}, nil
}
func (s *snapshot) Persist(sink raft.SnapshotSink) error {
	if _, err := s.tx.WriteTo(sink); err != nil {
		sink.Cancel()
		return err
	}
	return sink.Close()
}
func (s *snapshot) Release() { s.tx.Rollback() }
func (n *Node) Restore(reader io.ReadCloser) error {
	defer reader.Close()
	if err := n.store.Restore(reader); err != nil {
		n.fail(err)
		return err
	}
	return nil
}

type streamLayer struct {
	net.Listener
	address net.Addr
	config  *tls.Config
}

func (s *streamLayer) Addr() net.Addr { return s.address }
func (s *streamLayer) Dial(address raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: timeout}
	if s.config == nil {
		return dialer.Dial("tcp", string(address))
	}
	return tls.DialWithDialer(dialer, "tcp", string(address), s.config)
}
func openStream(options Options) (*streamLayer, error) {
	host, _, err := net.SplitHostPort(options.Bind)
	if err != nil {
		return nil, err
	}
	plain := options.TLSCert == "" && options.TLSKey == "" && options.TLSCA == ""
	if plain {
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			return nil, errors.New("non-loopback replication requires mutual TLS")
		}
		for _, peer := range options.Peers {
			host, _, _ := net.SplitHostPort(peer.Address)
			if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
				return nil, errors.New("remote replication peers require mutual TLS")
			}
		}
	}
	var config *tls.Config
	if !plain {
		certificate, err := tls.LoadX509KeyPair(options.TLSCert, options.TLSKey)
		if err != nil {
			return nil, err
		}
		pem, err := os.ReadFile(options.TLSCA)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("invalid replication CA")
		}
		config = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}, RootCAs: pool, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert}
	}
	address, err := net.ResolveTCPAddr("tcp", options.Advertise)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", options.Bind)
	if err != nil {
		return nil, err
	}
	if config != nil {
		listener = tls.NewListener(listener, config)
	}
	return &streamLayer{Listener: listener, address: address, config: config}, nil
}

func validateIdentity(options Options) error {
	peers := append([]Peer(nil), options.Peers...)
	sort.Slice(peers, func(i, j int) bool { return peers[i].ID < peers[j].ID })
	encoded, err := json.Marshal(struct {
		ID, Address string
		Peers       []Peer
	}{options.ID, options.Advertise, peers})
	if err != nil {
		return err
	}
	path := filepath.Join(options.Directory, "identity.json")
	previous, err := os.ReadFile(path)
	if err == nil {
		if string(previous) != string(encoded) {
			return errors.New("replication node identity or fixed membership changed; refusing to reuse data")
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(encoded)
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}

func (n *Node) Term() uint64 {
	term, _ := strconv.ParseUint(n.raft.Stats()["term"], 10, 64)
	return term
}
