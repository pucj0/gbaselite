package server

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gbaselite/executor"
	"gbaselite/journal"
	"gbaselite/parser"
	"gbaselite/protocol"
	"gbaselite/storage"
)

type MySQLServer struct {
	Address                 string
	Engine                  *executor.Engine
	Logger                  *log.Logger
	MaxConnections          int
	WriteBufferSize         int
	SlowQuery               time.Duration
	DefaultTimeZone         string
	Audit                   *journal.AuditLog
	AuthFailureLimit        int
	AuthFailureWindow       time.Duration
	AuthFailureBlock        time.Duration
	TLSConfig               *tls.Config
	RequireSecureTransport  bool
	listener                net.Listener
	nextID                  atomic.Uint32
	wg                      sync.WaitGroup
	connectionSlots         chan struct{}
	connectionSlotsM        sync.Once
	stopping                atomic.Bool
	startedAt               time.Time
	activeConnections       atomic.Int64
	maxUsedConnections      atomic.Int64
	totalConnections        atomic.Uint64
	questions               atomic.Uint64
	activeQueries           atomic.Int64
	abortedConnections      atomic.Uint64
	tlsConnections          atomic.Uint64
	authFailuresMu          sync.Mutex
	authFailures            map[string]authenticationFailure
	authFailuresLastCleanup time.Time
}

type authenticationFailure struct {
	windowStarted time.Time
	blockedUntil  time.Time
	lastSeen      time.Time
	count         int
}

const maxTrackedAuthenticationFailures = 4096

func (s *MySQLServer) ListenAndServe() error {
	listener, err := net.Listen("tcp", s.Address)
	if err != nil {
		return err
	}
	return s.Serve(listener)
}

func (s *MySQLServer) Serve(listener net.Listener) error {
	s.listener = listener
	if s.startedAt.IsZero() {
		s.startedAt = time.Now()
	}
	s.connectionSlotsM.Do(func() {
		if s.MaxConnections > 0 {
			s.connectionSlots = make(chan struct{}, s.MaxConnections)
		}
	})
	s.Logger.Printf("MySQL protocol listening on %s", listener.Addr())
	for {
		if s.connectionSlots != nil {
			for !s.stopping.Load() {
				select {
				case s.connectionSlots <- struct{}{}:
					goto slotAcquired
				case <-time.After(100 * time.Millisecond):
				}
			}
			return nil
		}
	slotAcquired:
		connection, err := listener.Accept()
		if err != nil {
			if s.connectionSlots != nil {
				<-s.connectionSlots
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			s.Logger.Printf("accept error: %v", err)
			continue
		}
		s.totalConnections.Add(1)
		activeConnections := s.activeConnections.Add(1)
		updateAtomicMaximum(&s.maxUsedConnections, activeConnections)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.activeConnections.Add(-1)
			if s.connectionSlots != nil {
				defer func() { <-s.connectionSlots }()
			}
			s.handleConnection(connection)
		}()
	}
}
func (s *MySQLServer) Shutdown(ctx context.Context) error {
	s.stopping.Store(true)
	if s.listener != nil {
		_ = s.listener.Close()
	}
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return s.Engine.Close()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *MySQLServer) handleConnection(raw net.Conn) {
	defer raw.Close()
	authenticatedConnection := false
	defer func() {
		if !authenticatedConnection {
			s.abortedConnections.Add(1)
		}
	}()
	id := s.nextID.Add(1)
	remote := raw.RemoteAddr().String()
	remoteHost, remotePort, splitErr := net.SplitHostPort(remote)
	if splitErr != nil {
		remoteHost = remote
		remotePort = ""
	}
	s.Logger.Printf("client connected id=%d remote=%s", id, remote)
	defer s.Logger.Printf("client disconnected id=%d remote=%s", id, remote)
	packet := &protocol.PacketConn{Conn: raw}
	writeBufferSize := s.WriteBufferSize
	if writeBufferSize <= 0 {
		writeBufferSize = 16 << 10
	}
	packet.EnableWriteBuffer(writeBufferSize)
	defer func() { _ = packet.Flush() }()
	seed, err := protocol.NewSeed()
	if err != nil {
		return
	}
	capabilities := uint32(protocol.ServerCapabilities)
	if s.TLSConfig != nil {
		capabilities |= protocol.ClientSSL
	}
	if err = packet.WritePacket(protocol.HandshakePacketWithCapabilities(id, seed, capabilities)); err != nil {
		return
	}
	if err = packet.Flush(); err != nil {
		return
	}
	responseData, err := packet.ReadPacket()
	if err != nil {
		return
	}
	secureTransport := false
	tlsVersion := ""
	tlsCipher := ""
	if len(responseData) == 32 && binary.LittleEndian.Uint32(responseData[:4])&protocol.ClientSSL != 0 {
		if s.TLSConfig == nil {
			_ = packet.WritePacket(protocol.ErrorPacket(3159, "Connections using insecure transport are prohibited while --require_secure_transport=ON"))
			return
		}
		tlsConnection := tls.Server(raw, s.TLSConfig)
		if err := raw.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
			return
		}
		if err := tlsConnection.Handshake(); err != nil {
			if s.Logger != nil {
				s.Logger.Printf("TLS handshake failed id=%d remote=%s error=%v", id, remote, err)
			}
			return
		}
		if err := raw.SetDeadline(time.Time{}); err != nil {
			return
		}
		packet = &protocol.PacketConn{Conn: tlsConnection, Sequence: packet.Sequence}
		packet.EnableWriteBuffer(writeBufferSize)
		responseData, err = packet.ReadPacket()
		if err != nil {
			return
		}
		secureTransport = true
		state := tlsConnection.ConnectionState()
		tlsVersion = tls.VersionName(state.Version)
		tlsCipher = tls.CipherSuiteName(state.CipherSuite)
		s.tlsConnections.Add(1)
	}
	response, err := protocol.ParseHandshakeResponse(responseData)
	if err != nil {
		_ = packet.WritePacket(protocol.ErrorPacket(1043, err.Error()))
		return
	}
	if s.RequireSecureTransport && !secureTransport {
		s.writeAudit(journal.AuditEvent{ConnectionID: id, Username: response.Username, RemoteIP: remoteHost, RemotePort: remotePort, Database: response.Database, Operation: "AUTHENTICATE", Result: "error", ErrorCode: 3159})
		_ = packet.WritePacket(protocol.ErrorPacket(3159, "Connections using insecure transport are prohibited while --require_secure_transport=ON"))
		return
	}
	authenticationKey := remoteHost + "\x00" + response.Username
	if s.authenticationBlocked(authenticationKey, time.Now()) {
		s.writeAudit(journal.AuditEvent{ConnectionID: id, Username: response.Username, RemoteIP: remoteHost, RemotePort: remotePort, Database: response.Database, Operation: "AUTHENTICATE", Result: "blocked", ErrorCode: 1045})
		if s.Logger != nil {
			s.Logger.Printf("authentication throttled id=%d user=%s remote=%s", id, response.Username, remoteHost)
		}
		_ = packet.WritePacket(protocol.ErrorPacket(1045, "Access denied for user '"+response.Username+"'"))
		return
	}
	account, authenticated := s.Engine.Users.AuthenticateNativePassword(response.Username, remoteHost, seed, response.AuthResponse)
	if !authenticated {
		s.recordAuthenticationFailure(authenticationKey, time.Now())
		s.writeAudit(journal.AuditEvent{ConnectionID: id, Username: response.Username, RemoteIP: remoteHost, RemotePort: remotePort, Database: response.Database, Operation: "AUTHENTICATE", Result: "error", ErrorCode: 1045})
		_ = packet.WritePacket(protocol.ErrorPacket(1045, "Access denied for user '"+response.Username+"'"))
		return
	}
	s.clearAuthenticationFailures(authenticationKey)
	authenticatedConnection = true
	packet.Capabilities = response.Capabilities & capabilities
	s.Logger.Printf("client authenticated id=%d user=%s capabilities=0x%08x database=%s", id, response.Username, packet.Capabilities, response.Database)
	session := &executor.Session{CurrentDatabase: response.Database, StreamResults: true, Username: account.Username, Host: account.Host, RemoteIP: remoteHost, RemotePort: remotePort, ConnectionID: id, SecureTransport: secureTransport, TLSVersion: tlsVersion, TLSCipher: tlsCipher, JournalSessionID: fmt.Sprintf("%d-%d", time.Now().UnixNano(), id)}
	session.ServerTimeZone = s.DefaultTimeZone
	if s.DefaultTimeZone != "" {
		if err := session.SetTimeZone(s.DefaultTimeZone); err != nil {
			_ = packet.WritePacket(protocol.ErrorPacket(1064, err.Error()))
			return
		}
	}
	initializeHandshakeCharacterSet(session, response.CharacterSet)
	s.writeAudit(journal.AuditEvent{ConnectionID: id, Username: account.Username, RemoteIP: remoteHost, RemotePort: remotePort, Database: response.Database, Operation: "AUTHENTICATE", Result: "success"})
	preparedStatements := make(map[uint32]*preparedStatement)
	var nextStatementID uint32 = 1
	defer s.Engine.CloseSession(session)
	if response.Database != "" && !virtualDatabase(response.Database) {
		if _, err := s.Engine.Store.Database(response.Database); err != nil {
			_ = packet.WritePacket(protocol.ErrorPacket(1049, "Unknown database '"+response.Database+"'"))
			return
		}
		if !s.Engine.Users.HasDatabaseAccess(session.Username, session.Host, response.Database) {
			_ = packet.WritePacket(protocol.ErrorPacket(1044, "Access denied for user '"+response.Username+"' to database '"+response.Database+"'"))
			return
		}
	}
	if err = packet.WritePacket(protocol.OKPacketWithCapabilities(0, "", packet.Capabilities)); err != nil {
		return
	}
	if err = packet.Flush(); err != nil {
		return
	}
	for {
		packet.ResetSequence()
		commandData, err := packet.ReadPacket()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				s.Logger.Printf("connection id=%d read error: %v", id, err)
			}
			return
		}
		if len(commandData) == 0 {
			continue
		}
		switch commandData[0] {
		case 0x01:
			return
		case 0x0e:
			_ = packet.WritePacket(protocol.OKPacketWithCapabilities(0, "", packet.Capabilities))
		case 0x02:
			started := time.Now()
			database := string(commandData[1:])
			var commandErr error
			if availabilityErr := s.Engine.AvailabilityError(); availabilityErr != nil {
				commandErr = availabilityErr
				_ = packet.WritePacket(protocol.ErrorPacket(mysqlExecutionErrorCode(availabilityErr), availabilityErr.Error()))
			} else if _, err := s.Engine.Store.Database(database); err != nil && !virtualDatabase(database) {
				commandErr = err
				_ = packet.WritePacket(protocol.ErrorPacket(1049, err.Error()))
			} else if !virtualDatabase(database) && !s.Engine.Users.HasDatabaseAccess(session.Username, session.Host, database) {
				commandErr = fmt.Errorf("access denied")
				_ = packet.WritePacket(protocol.ErrorPacket(1044, "Access denied for user '"+session.Username+"' to database '"+database+"'"))
			} else {
				session.CurrentDatabase = database
				_ = packet.WritePacket(protocol.OKPacketWithCapabilities(0, "", packet.Capabilities))
			}
			s.auditQuery(session, remotePort, "USE `"+strings.ReplaceAll(database, "`", "``")+"`", started, &executor.Result{}, commandErr)
		case 0x03:
			s.handleQuery(packet, session, string(commandData[1:]))
		case 0x04:
			s.handleFieldList(packet, session, string(commandData[1:]))
		case 0x16:
			query := string(commandData[1:])
			statementID := nextStatementID
			nextStatementID++
			parameterCount := countPlaceholders(query)
			preparedStatements[statementID] = &preparedStatement{query: query, parameterCount: parameterCount}
			_ = packet.WritePacket(protocol.PrepareOKPacket(statementID, uint16(parameterCount)))
			if parameterCount > 0 {
				for range parameterCount {
					_ = packet.WritePacket(protocol.ColumnDefinition(executor.Column{Name: "?", Type: storage.TypeVarchar}, session.CurrentDatabase, ""))
				}
				_ = packet.WritePacket(protocol.EOFPacket())
			}
		case 0x17:
			started := time.Now()
			if len(commandData) < 5 {
				s.auditQuery(session, remotePort, "EXECUTE", started, nil, errors.New("invalid prepared statement execute packet"))
				_ = packet.WritePacket(protocol.ErrorPacket(1243, "invalid prepared statement execute packet"))
				break
			}
			statementID := binary.LittleEndian.Uint32(commandData[1:5])
			statement := preparedStatements[statementID]
			if statement == nil {
				s.auditQuery(session, remotePort, "EXECUTE", started, nil, errors.New("unknown prepared statement"))
				_ = packet.WritePacket(protocol.ErrorPacket(1243, "unknown prepared statement"))
				break
			}
			_, values, parameterTypes, decodeErr := protocol.DecodeStmtExecute(commandData[1:], statement.parameterCount, statement.parameterTypes)
			if decodeErr != nil {
				s.auditQuery(session, remotePort, statement.query, started, nil, decodeErr)
				_ = packet.WritePacket(protocol.ErrorPacket(1210, decodeErr.Error()))
				break
			}
			statement.parameterTypes = parameterTypes
			query, bindErr := bindPreparedQuery(statement.query, values)
			if bindErr != nil {
				s.auditQuery(session, remotePort, statement.query, started, nil, bindErr)
				_ = packet.WritePacket(protocol.ErrorPacket(1210, bindErr.Error()))
				break
			}
			result, executeErr := s.executeCompatible(session, query)
			if executeErr != nil {
				s.logSlowQuery(query, started)
				s.Logger.Printf("SQL error query=%q error=%v", queryForLog(query), executeErr)
				s.auditQuery(session, remotePort, query, started, nil, executeErr)
				_ = packet.WritePacket(protocol.ErrorPacket(mysqlExecutionErrorCode(executeErr), executeErr.Error()))
				break
			}
			if writeErr := protocol.WriteBinaryResult(packet, result, session.CurrentDatabase, ""); writeErr != nil {
				s.Logger.Printf("write prepared result error: %v", writeErr)
				s.logSlowQuery(query, started)
				s.auditQuery(session, remotePort, query, started, result, writeErr)
			} else {
				s.logSlowQuery(query, started)
				s.auditQuery(session, remotePort, query, started, result, nil)
			}
		case 0x19:
			if len(commandData) >= 5 {
				delete(preparedStatements, binary.LittleEndian.Uint32(commandData[1:5]))
			}
		case 0x1a:
			_ = packet.WritePacket(protocol.OKPacketWithCapabilities(0, "", packet.Capabilities))
		default:
			_ = packet.WritePacket(protocol.ErrorPacket(1047, fmt.Sprintf("unsupported command 0x%x", commandData[0])))
		}
		if err := packet.Flush(); err != nil {
			s.Logger.Printf("connection id=%d flush error: %v", id, err)
			return
		}
	}
}

func (s *MySQLServer) authenticationThrottleEnabled() bool {
	return s.AuthFailureLimit > 0 && s.AuthFailureWindow > 0 && s.AuthFailureBlock > 0
}

func (s *MySQLServer) authenticationBlocked(key string, now time.Time) bool {
	if !s.authenticationThrottleEnabled() {
		return false
	}
	s.authFailuresMu.Lock()
	defer s.authFailuresMu.Unlock()
	s.cleanupAuthenticationFailuresLocked(now)
	state, ok := s.authFailures[key]
	if !ok {
		return false
	}
	if state.blockedUntil.After(now) {
		state.lastSeen = now
		s.authFailures[key] = state
		return true
	}
	if !state.windowStarted.Add(s.AuthFailureWindow).After(now) {
		delete(s.authFailures, key)
	}
	return false
}

func (s *MySQLServer) recordAuthenticationFailure(key string, now time.Time) {
	if !s.authenticationThrottleEnabled() {
		return
	}
	s.authFailuresMu.Lock()
	defer s.authFailuresMu.Unlock()
	if s.authFailures == nil {
		s.authFailures = make(map[string]authenticationFailure)
	}
	s.cleanupAuthenticationFailuresLocked(now)
	state := s.authFailures[key]
	if state.windowStarted.IsZero() || !state.windowStarted.Add(s.AuthFailureWindow).After(now) {
		state = authenticationFailure{windowStarted: now}
	}
	state.count++
	state.lastSeen = now
	if state.count >= s.AuthFailureLimit {
		state.blockedUntil = now.Add(s.AuthFailureBlock)
	}
	s.authFailures[key] = state
}

func (s *MySQLServer) clearAuthenticationFailures(key string) {
	if !s.authenticationThrottleEnabled() {
		return
	}
	s.authFailuresMu.Lock()
	delete(s.authFailures, key)
	s.authFailuresMu.Unlock()
}

func (s *MySQLServer) cleanupAuthenticationFailuresLocked(now time.Time) {
	cleanupInterval := s.AuthFailureWindow
	if s.AuthFailureBlock > cleanupInterval {
		cleanupInterval = s.AuthFailureBlock
	}
	if len(s.authFailures) < maxTrackedAuthenticationFailures && !s.authFailuresLastCleanup.IsZero() && now.Sub(s.authFailuresLastCleanup) < cleanupInterval {
		return
	}
	for key, state := range s.authFailures {
		expires := state.windowStarted.Add(s.AuthFailureWindow)
		if state.blockedUntil.After(expires) {
			expires = state.blockedUntil
		}
		if !expires.After(now) {
			delete(s.authFailures, key)
		}
	}
	if len(s.authFailures) >= maxTrackedAuthenticationFailures {
		var oldestKey string
		var oldestTime time.Time
		for key, state := range s.authFailures {
			if oldestKey == "" || state.lastSeen.Before(oldestTime) {
				oldestKey = key
				oldestTime = state.lastSeen
			}
		}
		delete(s.authFailures, oldestKey)
	}
	s.authFailuresLastCleanup = now
}

func virtualDatabase(name string) bool {
	return strings.EqualFold(name, "information_schema") || strings.EqualFold(name, "mysql")
}

func (s *MySQLServer) handleQuery(packet *protocol.PacketConn, session *executor.Session, query string) {
	started := time.Now()
	result, err := s.executeCompatible(session, query)
	if err != nil {
		if !strings.EqualFold(strings.TrimSpace(query), "select $$") {
			s.Logger.Printf("SQL error duration=%s query=%q error=%v", time.Since(started), queryForLog(query), err)
		}
		s.auditQuery(session, session.RemotePort, query, started, nil, err)
		s.logSlowQuery(query, started)
		_ = packet.WritePacket(protocol.ErrorPacket(mysqlExecutionErrorCode(err), err.Error()))
		return
	}
	if err := protocol.WriteResult(packet, result, session.CurrentDatabase, ""); err != nil {
		s.Logger.Printf("write result error: %v", err)
		s.logSlowQuery(query, started)
		s.auditQuery(session, session.RemotePort, query, started, result, err)
	} else {
		s.logSlowQuery(query, started)
		s.auditQuery(session, session.RemotePort, query, started, result, nil)
	}
}

func (s *MySQLServer) executeCompatible(session *executor.Session, query string) (*executor.Result, error) {
	s.questions.Add(1)
	s.activeQueries.Add(1)
	defer s.activeQueries.Add(-1)
	if s.Engine != nil {
		if err := s.Engine.AvailabilityError(); err != nil {
			return nil, err
		}
	}
	trimmed := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(query), ";"))
	if isShowStatusQuery(strings.ToUpper(trimmed)) {
		status := s.runtimeStatus()
		if status.StorageState != "available" {
			return nil, s.Engine.AvailabilityError()
		}
		return statusRows(session, trimmed, status)
	}
	return ExecuteCompatible(s.Engine, session, query)
}

func (s *MySQLServer) runtimeStatus() runtimeStatus {
	uptime := uint64(0)
	if !s.startedAt.IsZero() {
		uptime = uint64(time.Since(s.startedAt) / time.Second)
	}
	storageState := "available"
	if s.Engine != nil && s.Engine.AvailabilityError() != nil {
		storageState = "fail-closed"
	}
	return runtimeStatus{
		Uptime:             uptime,
		Connections:        s.totalConnections.Load(),
		ActiveConnections:  s.activeConnections.Load(),
		MaxUsedConnections: s.maxUsedConnections.Load(),
		Questions:          s.questions.Load(),
		ActiveQueries:      s.activeQueries.Load(),
		AbortedConnections: s.abortedConnections.Load(),
		TLSConnections:     s.tlsConnections.Load(),
		StorageState:       storageState,
	}
}

func updateAtomicMaximum(target *atomic.Int64, value int64) {
	for current := target.Load(); value > current; current = target.Load() {
		if target.CompareAndSwap(current, value) {
			return
		}
	}
}

func (s *MySQLServer) auditQuery(session *executor.Session, remotePort, query string, started time.Time, result *executor.Result, queryErr error) {
	if s.Audit == nil {
		return
	}
	if expanded, err := parser.ExpandMySQLExecutableComments(query); err == nil {
		query = expanded
	}
	event := journal.AuditEvent{
		Timestamp:    time.Now().UTC(),
		ConnectionID: session.ConnectionID,
		Username:     session.Username,
		RemoteIP:     session.RemoteIP,
		RemotePort:   remotePort,
		Database:     session.CurrentDatabase,
		Operation:    journal.Operation(query),
		Result:       "success",
		DurationMS:   float64(time.Since(started).Microseconds()) / 1000,
		SQL:          journal.RedactSQL(query),
	}
	if result != nil {
		event.AffectedRows = result.AffectedRows
	}
	if queryErr != nil {
		event.Result = "error"
		event.ErrorCode = mysqlExecutionErrorCode(queryErr)
	}
	s.writeAudit(event)
}

func (s *MySQLServer) writeAudit(event journal.AuditEvent) {
	if s.Audit == nil {
		return
	}
	if err := s.Audit.Append(event); err != nil && s.Logger != nil {
		s.Logger.Printf("audit log write error: %v", err)
	}
}

func (s *MySQLServer) logSlowQuery(query string, started time.Time) {
	duration := time.Since(started)
	if s.SlowQuery > 0 && duration >= s.SlowQuery {
		s.Logger.Printf("slow query duration=%s query=%q", duration, queryForLog(query))
	}
}

func mysqlExecutionErrorCode(err error) uint16 {
	message := strings.ToLower(err.Error())
	switch {
	case errors.Is(err, executor.ErrPersistenceUnavailable):
		return 1030
	case errors.Is(err, storage.ErrDuplicateKey):
		return 1062
	case errors.Is(err, storage.ErrForeignKeyReferenced):
		return 1451
	case errors.Is(err, storage.ErrForeignKey):
		return 1452
	case errors.Is(err, storage.ErrCheckConstraint), errors.Is(err, storage.ErrInvalidJSON), errors.Is(err, storage.ErrInvalidEnum):
		return 3819
	case errors.Is(err, storage.ErrIndexExists):
		return 1061
	case errors.Is(err, storage.ErrIndexNotFound):
		return 1091
	case errors.Is(err, storage.ErrConstraintNotFound):
		return 1091
	case errors.Is(err, storage.ErrConstraintExists):
		return 1826
	case strings.Contains(message, "access denied"):
		return 1142
	case strings.Contains(message, "user") && (strings.Contains(message, "already exists") || strings.Contains(message, "not found")):
		return 1396
	default:
		return 1064
	}
}

func queryForLog(query string) string {
	const maxRunes = 1024
	runes := []rune(query)
	if len(runes) <= maxRunes {
		return query
	}
	return string(runes[:maxRunes]) + fmt.Sprintf("... [truncated, %d runes total]", len(runes))
}
func (s *MySQLServer) handleFieldList(packet *protocol.PacketConn, session *executor.Session, data string) {
	tableName := strings.SplitN(data, "\x00", 2)[0]
	result, err := s.Engine.Execute(session, "SHOW COLUMNS FROM `"+strings.ReplaceAll(tableName, "`", "``")+"`")
	if err != nil {
		_ = packet.WritePacket(protocol.ErrorPacket(1146, err.Error()))
		return
	}
	for _, row := range result.Rows {
		columnType := storage.TypeVarchar
		columnLength := 0
		if len(row) > 1 {
			declared := fmt.Sprint(row[1])
			if open := strings.IndexByte(declared, '('); open >= 0 {
				if close := strings.IndexByte(declared[open+1:], ')'); close >= 0 {
					lengthText := strings.TrimSpace(declared[open+1 : open+1+close])
					columnLength, _ = strconv.Atoi(lengthText)
				}
				declared = declared[:open]
			}
			if fields := strings.Fields(declared); len(fields) > 0 {
				if parsed, parseErr := storage.ParseDataType(fields[0]); parseErr == nil {
					columnType = parsed
				}
			}
		}
		column := executor.Column{Name: fmt.Sprint(row[0]), OriginalName: fmt.Sprint(row[0]), Type: columnType, Length: columnLength, Schema: session.CurrentDatabase, Table: tableName, Nullable: len(row) < 3 || strings.EqualFold(fmt.Sprint(row[2]), "YES")}
		if len(row) > 3 {
			switch strings.ToUpper(fmt.Sprint(row[3])) {
			case "PRI":
				column.PrimaryKey = true
			case "UNI":
				column.UniqueKey = true
			case "MUL":
				column.MultipleKey = true
			}
		}
		if len(row) > 5 {
			column.AutoIncrement = strings.Contains(strings.ToLower(fmt.Sprint(row[5])), "auto_increment")
		}
		_ = packet.WritePacket(protocol.ColumnDefinition(column, session.CurrentDatabase, tableName))
	}
	_ = packet.WritePacket(protocol.EOFPacket())
}
