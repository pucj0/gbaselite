package failover

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"errors"
	mysql "github.com/go-sql-driver/mysql"
	"io"
	"net"
	"time"
)

type probeResult struct {
	Confirmed                                      bool
	PeerID, Address, Stage                         string
	ReportedID, State, Leader, RaftAddress         string
	Applied                                        uint64
	AcquireDuration, StatusDuration, TotalDuration time.Duration
	RemainingBudget                                time.Duration
	Err                                            error
	ErrClass                                       string
}
type discoveryRound struct {
	ID         uint64
	Start, End time.Time
	Result     string
	Probes     []probeResult
}

// One confirmation owns one SQL connection. The original shared 750 ms context
// covers acquisition and the single status request, including server-side
// lightweight quorum verification. No ordinary SQL or FSM Barrier is probed.
func probeLeader(ctx context.Context, db *sql.DB, peer Peer) (result probeResult) {
	started := time.Now()
	result.PeerID, result.Address, result.Stage = peer.ID, peer.Address, "acquire-conn"
	defer func() {
		result.TotalDuration = time.Since(started)
		result.ErrClass = probeErrorClass(result.Err)
		// A driver can surface ErrInvalidConn after closing on context expiration.
		// Preserve that error while recording the observable context cause too.
		if result.Err != nil && ctx.Err() != nil {
			result.ErrClass = probeErrorClass(ctx.Err())
		}
	}()
	conn, err := db.Conn(ctx)
	result.AcquireDuration = time.Since(started)
	if err != nil {
		result.Err = err
		return
	}
	defer conn.Close()
	result.Stage = "show-replication-status"
	if deadline, ok := ctx.Deadline(); ok {
		result.RemainingBudget = time.Until(deadline)
	}
	startedStatus := time.Now()
	err = conn.QueryRowContext(ctx, "SHOW REPLICATION STATUS").Scan(&result.ReportedID, &result.State, &result.Leader, &result.RaftAddress, &result.Applied)
	result.StatusDuration = time.Since(startedStatus)
	if err != nil {
		result.Err = err
		return
	}
	if result.State != "Leader" || result.ReportedID != peer.ID || result.Leader != peer.ID {
		result.Stage = "role-mismatch"
		return
	}
	result.Confirmed, result.Stage = true, "confirmed"
	return
}
func probeErrorClass(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "context-deadline"
	case errors.Is(err, context.Canceled):
		return "context-canceled"
	case errors.Is(err, sqldriver.ErrBadConn):
		return "driver-bad-connection"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected-eof"
	case errors.Is(err, net.ErrClosed), errors.Is(err, sql.ErrConnDone), errors.Is(err, io.EOF), errors.Is(err, mysql.ErrInvalidConn):
		return "connection-closed"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "network-timeout"
	}
	var sqlErr *mysql.MySQLError
	if errors.As(err, &sqlErr) {
		return "sql-error"
	}
	return "unknown"
}
