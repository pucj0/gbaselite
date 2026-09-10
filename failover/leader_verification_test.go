package failover

import (
	"context"
	"database/sql"
	"errors"
	"gbaselite/storageengine"
	"sync"
	"testing"
)

type probeReplicaSpy struct {
	mutex                 sync.Mutex
	verifies, barriers    int
	verifyErr, barrierErr error
}

func (r *probeReplicaSpy) Status() storageengine.ReplicationStatus {
	return storageengine.ReplicationStatus{ID: "a", LeaderID: "a", State: "Leader", Leader: "raft:1"}
}
func (r *probeReplicaSpy) VerifyLeader(ctx context.Context) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.verifies++
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return r.verifyErr
}
func (r *probeReplicaSpy) Barrier(context.Context) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.barriers++
	return r.barrierErr
}
func TestProbeDoesNotUseSQLBarrier(t *testing.T) {
	replica := &probeReplicaSpy{}
	address := stateBackendWithReplica(t, replica)
	db, err := sql.Open("mysql", "root:pw@tcp("+address+")/?timeout=2s&readTimeout=3s&writeTimeout=3s")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := stateRouter(t, address, address)
	databases := []*sql.DB{db, db, db}
	r.discover(context.Background(), databases)
	connection := stateConnection(t, r)
	generation := r.generation
	// If discovery enters ordinary SQL, the original read barrier now fails.
	replica.mutex.Lock()
	replica.barrierErr = errors.New("SQL barrier deliberately unavailable")
	before := replica.barriers
	verifications := replica.verifies
	replica.mutex.Unlock()
	for i := 0; i < leaderMissThreshold+1; i++ {
		r.discover(context.Background(), databases)
	}
	if r.Leader() != address || r.generation != generation {
		t.Fatal("healthy leader lost because of SQL barrier")
	}
	replica.mutex.Lock()
	got, verified := replica.barriers, replica.verifies
	replica.mutex.Unlock()
	if got != before || verified != verifications+leaderMissThreshold+1 {
		t.Fatalf("barriers=%d -> %d verifies=%d -> %d", before, got, verifications, verified)
	}
	replica.mutex.Lock()
	replica.verifyErr = errors.New("transient quorum verification failure")
	replica.mutex.Unlock()
	r.discover(context.Background(), databases)
	if r.Leader() != address || r.generation != generation {
		t.Fatal("transient verification failure closed leader")
	}
	replica.mutex.Lock()
	replica.verifyErr = nil
	replica.mutex.Unlock()
	r.discover(context.Background(), databases)
	// Ordinary SELECT must still use Barrier; it is not exempted by this fix.
	if err = connection.QueryRowContext(context.Background(), "SELECT 1").Scan(new(int)); err == nil {
		t.Fatal("ordinary SELECT bypassed SQL barrier")
	}
	replica.mutex.Lock()
	replica.barrierErr = nil
	replica.mutex.Unlock()
	stateQuery(t, connection) // Same pinned physical connection remains usable.
}
