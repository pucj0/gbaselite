package server

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gbaselite/executor"
	"gbaselite/storage"
)

// serverConnection tracks one authenticated protocol connection so that
// CONNECTION_ID(), SHOW PROCESSLIST and KILL can observe and interrupt it.
//
// Only the owning connection goroutine reads executor session state; every value
// another goroutine needs is mirrored here under the mutex, so a client killing a
// busy connection can never race with the statements running on it.
type serverConnection struct {
	id        uint32
	username  string
	host      string
	remote    string
	connected time.Time
	closeConn func()

	mu        sync.Mutex
	database  string
	command   string
	statement string
	started   time.Time
	cancel    context.CancelFunc
	// killRequested records a KILL CONNECTION issued by this connection itself, so
	// the response is written before the loop closes the socket.
	killRequested bool
}

func (s *MySQLServer) registerConnection(id uint32, session *executor.Session, remote string, closeConn func()) *serverConnection {
	connection := &serverConnection{
		id:        id,
		username:  session.Username,
		host:      session.Host,
		remote:    remote,
		connected: time.Now(),
		closeConn: closeConn,
		database:  session.CurrentDatabase,
		command:   "Sleep",
	}
	s.connectionMu.Lock()
	if s.connections == nil {
		s.connections = make(map[uint32]*serverConnection)
	}
	s.connections[id] = connection
	s.connectionMu.Unlock()
	return connection
}

func (s *MySQLServer) unregisterConnection(id uint32) {
	s.connectionMu.Lock()
	delete(s.connections, id)
	s.connectionMu.Unlock()
}

func (s *MySQLServer) connectionByID(id uint32) *serverConnection {
	s.connectionMu.Lock()
	defer s.connectionMu.Unlock()
	return s.connections[id]
}

// connectionsVisibleTo lists the connections the caller may observe: every
// connection for accounts allowed to inspect all threads (PROCESS on *.*),
// otherwise only its own user@host sessions, matching MySQL's SHOW PROCESSLIST.
func (s *MySQLServer) connectionsVisibleTo(session *executor.Session) []*serverConnection {
	all := s.Engine == nil || session == nil || strings.TrimSpace(session.Username) == "" ||
		s.Engine.Users.Allowed(session.Username, session.Host, "PROCESS", "*", "*")
	s.connectionMu.Lock()
	connections := make([]*serverConnection, 0, len(s.connections))
	for _, connection := range s.connections {
		if all || (strings.EqualFold(connection.username, session.Username) && strings.EqualFold(connection.host, session.Host)) {
			connections = append(connections, connection)
		}
	}
	s.connectionMu.Unlock()
	sort.Slice(connections, func(i, j int) bool { return connections[i].id < connections[j].id })
	return connections
}

// beginStatement marks the connection busy and records how to interrupt the
// statement that is about to run. It is always called by the owning goroutine.
func (c *serverConnection) beginStatement(database, query string, cancel context.CancelFunc) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.database = database
	c.command = "Query"
	c.statement = query
	c.started = time.Now()
	c.cancel = cancel
	c.mu.Unlock()
}

func (c *serverConnection) finishStatement() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.command = "Sleep"
	c.statement = ""
	c.started = time.Time{}
	c.cancel = nil
	c.mu.Unlock()
}

// refreshDatabase mirrors a session database change so SHOW PROCESSLIST reports it.
func (c *serverConnection) refreshDatabase(database string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.database = database
	c.mu.Unlock()
}

// processRow renders one SHOW PROCESSLIST row: id, user, host:port, database,
// command, elapsed seconds, state and the statement currently running.
func (c *serverConnection) processRow(now time.Time) []any {
	c.mu.Lock()
	defer c.mu.Unlock()
	started := c.started
	if started.IsZero() {
		started = c.connected
	}
	elapsed := int64(now.Sub(started) / time.Second)
	if elapsed < 0 {
		elapsed = 0
	}
	var info any
	if c.command != "Sleep" && c.statement != "" {
		info = c.statement
	}
	return []any{int64(c.id), c.username, c.remote, c.database, c.command, elapsed, "", info}
}

// cancelStatement interrupts the statement currently running on the connection.
// Canceling an idle connection is a no-op, exactly like MySQL.
func (c *serverConnection) cancelStatement() {
	if c == nil {
		return
	}
	c.mu.Lock()
	cancel := c.cancel
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (c *serverConnection) requestSelfClose() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.killRequested = true
	c.mu.Unlock()
}

// consumeKillRequest reports whether the connection must stop after the response
// that answered KILL CONNECTION for its own id.
func (c *serverConnection) consumeKillRequest() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	requested := c.killRequested
	c.killRequested = false
	return requested
}

// closeNow closes the socket from another connection's goroutine, which also
// unblocks the victim's pending read.
func (c *serverConnection) closeNow() {
	if c == nil || c.closeConn == nil {
		return
	}
	c.closeConn()
}

func isKillStatement(upper string) bool {
	return strings.HasPrefix(upper, "KILL ") || upper == "KILL"
}

func isProcessListStatement(upper string) bool {
	return upper == "SHOW PROCESSLIST" || upper == "SHOW FULL PROCESSLIST"
}

// executeKill implements MySQL's connection management: KILL [CONNECTION] <id>
// interrupts the target statement and closes that connection, while KILL QUERY
// <id> only interrupts the statement and leaves the connection usable.
func (s *MySQLServer) executeKill(session *executor.Session, query string) (*executor.Result, error) {
	queryOnly, id, err := parseKillStatement(query)
	if err != nil {
		return nil, err
	}
	target := s.connectionByID(id)
	if target == nil {
		return nil, &killError{code: 1094, message: fmt.Sprintf("Unknown thread id: %d", id)}
	}
	if !s.mayKill(session, target) {
		return nil, &killError{code: 1095, message: fmt.Sprintf("You are not owner of thread %d", id)}
	}
	self := session != nil && session.ConnectionID == id
	if self {
		// The only statement running here is this KILL itself.
		if !queryOnly {
			target.requestSelfClose()
		}
		return &executor.Result{}, nil
	}
	target.cancelStatement()
	if !queryOnly {
		target.closeNow()
	}
	return &executor.Result{}, nil
}

// mayKill allows a session to manage its own connections; managing another
// account's connection requires the PROCESS privilege like MySQL.
func (s *MySQLServer) mayKill(session *executor.Session, target *serverConnection) bool {
	if session == nil || strings.TrimSpace(session.Username) == "" || s.Engine == nil {
		return true
	}
	if strings.EqualFold(target.username, session.Username) && strings.EqualFold(target.host, session.Host) {
		return true
	}
	return s.Engine.Users.Allowed(session.Username, session.Host, "PROCESS", "*", "*")
}

// killError carries the MySQL error code for KILL failures so the protocol layer
// answers with 1094/1095 instead of a generic execution error.
type killError struct {
	code    uint16
	message string
}

func (e *killError) Error() string { return e.message }

func parseKillStatement(query string) (queryOnly bool, id uint32, err error) {
	fields := strings.Fields(query)
	kind := "CONNECTION"
	var idField string
	switch len(fields) {
	case 2:
		idField = fields[1]
	case 3:
		kind = strings.ToUpper(fields[1])
		idField = fields[2]
	default:
		return false, 0, &killError{code: 1064, message: "KILL requires a connection id"}
	}
	switch kind {
	case "QUERY":
		queryOnly = true
	case "CONNECTION":
	default:
		return false, 0, &killError{code: 1064, message: "KILL supports QUERY or CONNECTION"}
	}
	parsed, parseErr := strconv.ParseUint(idField, 10, 32)
	if parseErr != nil {
		return false, 0, &killError{code: 1094, message: "Unknown thread id: " + idField}
	}
	return queryOnly, uint32(parsed), nil
}

// processListResult renders SHOW [FULL] PROCESSLIST from the live registry.
func (s *MySQLServer) processListResult(session *executor.Session, _ string) (*executor.Result, error) {
	result := &executor.Result{Columns: []executor.Column{
		{Name: "Id", Type: storage.TypeBigInt},
		{Name: "User", Type: storage.TypeVarchar},
		{Name: "Host", Type: storage.TypeVarchar},
		{Name: "db", Type: storage.TypeVarchar},
		{Name: "Command", Type: storage.TypeVarchar},
		{Name: "Time", Type: storage.TypeBigInt},
		{Name: "State", Type: storage.TypeVarchar},
		{Name: "Info", Type: storage.TypeText},
	}}
	now := time.Now()
	for _, connection := range s.connectionsVisibleTo(session) {
		result.Rows = append(result.Rows, connection.processRow(now))
	}
	return result, nil
}
