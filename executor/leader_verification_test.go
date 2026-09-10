package executor

import (
	"context"
	"errors"
	"gbaselite/storageengine"
	"reflect"
	"testing"
)

type statusOnlyReplica struct {
	status   storageengine.ReplicationStatus
	barriers int
}

func (r *statusOnlyReplica) Status() storageengine.ReplicationStatus { return r.status }
func (r *statusOnlyReplica) Barrier(context.Context) error {
	r.barriers++
	return errors.New("unexpected SQL barrier")
}

type verifiedStatusReplica struct {
	statusOnlyReplica
	calls  int
	verify func(context.Context) error
}

func (r *verifiedStatusReplica) VerifyLeader(ctx context.Context) error {
	r.calls++
	if r.verify != nil {
		return r.verify(ctx)
	}
	return nil
}
func TestVerifiedReplicationStatusOptionalCapability(t *testing.T) {
	leader := storageengine.ReplicationStatus{ID: "a", LeaderID: "a", State: "Leader", Leader: "raft:1", Applied: 7}
	r := &verifiedStatusReplica{statusOnlyReplica: statusOnlyReplica{status: leader}}
	e := &Engine{Replica: r}
	want := e.ReplicationStatus()
	got, err := e.VerifiedReplicationStatus(context.Background())
	if err != nil || !reflect.DeepEqual(got, want) || r.calls != 1 || r.barriers != 0 {
		t.Fatalf("%+v %v calls=%d barriers=%d", got, err, r.calls, r.barriers)
	}
	r.status.State = "Follower"
	if _, err = e.VerifiedReplicationStatus(context.Background()); err != nil || r.calls != 1 {
		t.Fatal("follower invoked verifier", err)
	}
	missing := &statusOnlyReplica{status: leader}
	e.Replica = missing
	if _, err = e.VerifiedReplicationStatus(context.Background()); !errors.Is(err, storageengine.ErrUnsupported) || missing.barriers != 0 {
		t.Fatal("optional verifier used barrier fallback", err)
	}
	e.Replica = nil
	got, err = e.VerifiedReplicationStatus(nil)
	if err != nil || !reflect.DeepEqual(got, e.ReplicationStatus()) {
		t.Fatal("standalone schema changed", err)
	}
}
func TestVerifiedReplicationStatusRejectsFailureAndRoleChange(t *testing.T) {
	r := &verifiedStatusReplica{statusOnlyReplica: statusOnlyReplica{status: storageengine.ReplicationStatus{ID: "a", LeaderID: "a", State: "Leader"}}}
	e := &Engine{Replica: r}
	failed := errors.New("quorum unavailable")
	r.verify = func(context.Context) error { return failed }
	if _, err := e.VerifiedReplicationStatus(context.Background()); !errors.Is(err, failed) {
		t.Fatal(err)
	}
	r.verify = func(context.Context) error { r.status.State = "Follower"; return nil }
	if _, err := e.VerifiedReplicationStatus(context.Background()); !errors.Is(err, storageengine.ErrNotLeader) {
		t.Fatal("stale role accepted", err)
	}
	r.status.State = "Leader"
	r.status.LeaderID = "b"
	calls := r.calls
	if _, err := e.VerifiedReplicationStatus(context.Background()); !errors.Is(err, storageengine.ErrNotLeader) || r.calls != calls {
		t.Fatal("mismatched identity verified", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.VerifiedReplicationStatus(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
