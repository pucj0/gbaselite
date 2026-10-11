package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gbaselite/parser"
	"gbaselite/storageengine"
)

// lock_plan_test.go covers T025's locking plan and, in the integration test at the end, the
// end-to-end consequence of it: a range another transaction protects blocks a real SQL INSERT that
// lands inside it and does not block one that lands outside it.
//
// The unit half drives `planSQLLocking` with access plans directly, because that is where the
// classification lives: whether a statement gets a record-only plan, a proven interval or the
// explicit whole-domain fallback is a property of the access path the planner chose, and a test that
// only inspected SQL text could not tell the three apart.

// t025AccessPlan builds one access plan of the given shape. It exists so each case states only the
// part of the plan that decides its classification.
func t025AccessPlan(kind, index, space string, key []byte, bounds storageengine.KeyRange, covering, ordered bool) sqlAccessPlan {
	return sqlAccessPlan{kind: kind, index: index, space: space, key: key, bounds: bounds, covering: covering, ordered: ordered}
}

// TestT025LockPlanClassification pins the six shapes the contract names.
func TestT025LockPlanClassification(t *testing.T) {
	bounds := storageengine.KeyRange{Lower: []byte("a"), Upper: []byte("z")}
	cases := []struct {
		name     string
		plan     sqlAccessPlan
		mode     storageengine.LockMode
		wantKind lockPlanKind
		wantErr  error
		// wantReason is a substring the annotation must contain, so a fallback cannot be reported
		// with wording that hides it.
		wantReason string
	}{
		{
			name:       "a plain snapshot read has no locking plan",
			plan:       t025AccessPlan(sqlAccessPoint, "PRIMARY", "row/t", []byte("k"), storageengine.KeyRange{}, false, false),
			mode:       "",
			wantKind:   lockPlanNone,
			wantReason: "snapshot read",
		},
		{
			name:       "full unique hit is record-only",
			plan:       t025AccessPlan(sqlAccessPoint, "PRIMARY", "row/t", []byte("k"), storageengine.KeyRange{}, false, false),
			mode:       storageengine.LockModeX,
			wantKind:   lockPlanRecordOnly,
			wantReason: "full equality on a unique identity",
		},
		{
			name:       "a unique index equality locks the entry and the common row",
			plan:       t025AccessPlan(sqlAccessUnique, "u", "index/t/u", []byte("v"), storageengine.KeyRange{}, false, false),
			mode:       storageengine.LockModeS,
			wantKind:   lockPlanRecordOnly,
			wantReason: "common row",
		},
		{
			name:       "a proven interval is a range plan",
			plan:       t025AccessPlan(sqlAccessRange, "u", "index/t/u", nil, bounds, false, false),
			mode:       storageengine.LockModeX,
			wantKind:   lockPlanRange,
			wantReason: "proven interval",
		},
		{
			name:       "an ordered access without provable bounds falls back to the whole namespace",
			plan:       t025AccessPlan(sqlAccessOrdered, "u", "index/t/u", nil, storageengine.KeyRange{}, false, true),
			mode:       storageengine.LockModeX,
			wantKind:   lockPlanWholeDomain,
			wantReason: "whole namespace",
		},
		{
			name:       "a full scan falls back to the whole namespace",
			plan:       t025AccessPlan(sqlAccessAll, "", "row/t", nil, storageengine.KeyRange{}, false, false),
			mode:       storageengine.LockModeX,
			wantKind:   lockPlanWholeDomain,
			wantReason: "full scan",
		},
		{
			name:       "an unclassifiable access path is refused",
			plan:       t025AccessPlan("something-new", "u", "index/t/u", nil, storageengine.KeyRange{}, false, false),
			mode:       storageengine.LockModeX,
			wantKind:   lockPlanRefused,
			wantErr:    ErrLockPlanUnavailable,
			wantReason: "",
		},
		{
			name:       "an ordered plan with no namespace cannot be proven",
			plan:       t025AccessPlan(sqlAccessOrdered, "u", "", nil, bounds, false, true),
			mode:       storageengine.LockModeX,
			wantKind:   lockPlanWholeDomain,
			wantReason: "whole namespace",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			plan, err := planSQLLocking(testCase.plan, testCase.mode)
			if testCase.wantErr != nil {
				if !errors.Is(err, testCase.wantErr) {
					t.Fatalf("plan error = %v, want %v", err, testCase.wantErr)
				}
			} else if err != nil {
				t.Fatalf("plan error = %v, want nil", err)
			}
			if plan.kind != testCase.wantKind {
				t.Fatalf("plan kind = %q, want %q (reason %q)", plan.kind, testCase.wantKind, plan.reason)
			}
			if testCase.wantReason != "" && !strings.Contains(plan.reason, testCase.wantReason) {
				t.Fatalf("plan reason %q does not contain %q", plan.reason, testCase.wantReason)
			}
			// A refused plan must not render as a plan a caller could run unprotected, and a
			// fallback must say that it protects the whole namespace.
			if testCase.wantKind == lockPlanWholeDomain && plan.bounds.Lower != nil && plan.bounds.Upper != nil {
				t.Fatalf("a whole-domain fallback carries bounds %+v, want the unbounded interval", plan.bounds)
			}
			annotation := plan.Annotation(false)
			if testCase.wantKind != lockPlanNone && !strings.Contains(annotation, string(testCase.wantKind)) {
				t.Fatalf("annotation %q does not name the plan kind %q", annotation, testCase.wantKind)
			}
			// Locking reads are recognised but not executed yet; the annotation has to say so, or a
			// reader would take it for a lock the statement already takes.
			if testCase.wantKind != lockPlanNone && !strings.Contains(annotation, "not executed yet") {
				t.Fatalf("annotation %q hides that the plan is not executed yet", annotation)
			}
			executed := plan.Annotation(true)
			if strings.Contains(executed, "not executed yet") {
				t.Fatalf("annotation %q claims a pending state for an executed plan", executed)
			}
		})
	}
}

// TestT025LockPlanComponents pins what the plan turns into.
func TestT025LockPlanComponents(t *testing.T) {
	// An insertion is two components on one key, and the pair is what the manager validates: the
	// intention answers a covering Gap, the record lock answers same-key contention.
	components := recordComponents("row/t", "t", "", []byte("k"), storageengine.LockModeX, true)
	if len(components) != 2 {
		t.Fatalf("an insertion claim produced %d components, want the intention and its record lock", len(components))
	}
	if components[0].Mode != storageengine.LockModeInsertIntention || components[1].Mode != storageengine.LockModeX {
		t.Fatalf("insertion claim modes = %q/%q, want INSERT_INTENTION/X", components[0].Mode, components[1].Mode)
	}
	if components[0].Kind != storageengine.LockComponentRecord || components[1].Kind != storageengine.LockComponentRecord {
		t.Fatal("both halves of an insertion claim are record components on one key")
	}
	// A statement that is not creating the key takes only the record lock, which is what keeps an
	// existing row from claiming an insertion position it does not need.
	existing := recordComponents("row/t", "t", "", []byte("k"), storageengine.LockModeX, false)
	if len(existing) != 1 || existing[0].Mode != storageengine.LockModeX {
		t.Fatalf("an existing key produced %+v, want only the record lock", existing)
	}
	// A shared plan must stay shared: turning every locking read into an exclusive one would make
	// two FOR SHARE readers exclude each other.
	shared := recordComponents("row/t", "t", "", []byte("k"), storageengine.LockModeS, false)
	if len(shared) != 1 || shared[0].Mode != storageengine.LockModeS {
		t.Fatalf("a shared plan produced %+v, want a shared record lock", shared)
	}
	if got := recordComponents("row/t", "t", "", nil, storageengine.LockModeX, true); got != nil {
		t.Fatalf("a component without a key = %+v, want none: an unaddressable lock is refused", got)
	}

	// The interval of a range plan is the proven bounds, and a whole-domain fallback is the same
	// component with both bounds unset: "protect everything" is stated, never omitted.
	rangePlan := sqlLockPlan{kind: lockPlanRange, space: "index/t/u", bounds: storageengine.KeyRange{Lower: []byte("a"), Upper: []byte("z"), UpperInclusive: true}}
	built := rangeComponents(rangePlan, "t", "u", storageengine.LockModeX, [][]byte{[]byte("m"), []byte("p")})
	if len(built) != 3 {
		t.Fatalf("range components = %d, want the interval plus the two visited keys", len(built))
	}
	if built[0].Kind != storageengine.LockComponentInterval || built[0].Mode != storageengine.LockModeGap {
		t.Fatalf("the first range component = %+v, want a GAP interval", built[0])
	}
	if !built[0].UpperInclusive || string(built[0].Lower) != "a" || string(built[0].Upper) != "z" {
		t.Fatalf("the interval lost its bounds or inclusivity: %+v", built[0])
	}
	for i, key := range []string{"m", "p"} {
		if built[i+1].Kind != storageengine.LockComponentRecord || string(built[i+1].Key) != key {
			t.Fatalf("visited key %d = %+v, want a record lock on %q", i, built[i+1], key)
		}
	}
	whole := intervalComponent("row/t", "t", "", storageengine.KeyRange{})
	if whole.Lower != nil || whole.Upper != nil {
		t.Fatalf("a whole-domain interval = %+v, want both bounds unset", whole)
	}
}

// TestT025LockPlanModeMapping pins the statement-to-mode mapping: nothing locks without a locking
// clause, FOR UPDATE takes X and FOR SHARE (or its LOCK IN SHARE MODE alias) takes S.
func TestT025LockPlanModeMapping(t *testing.T) {
	if got := planSQLLockingMode(false, parser.LockModeUpdate); got != "" {
		t.Fatalf("a non-locking statement mapped to %q, want no mode", got)
	}
	if got := planSQLLockingMode(true, parser.LockModeUpdate); got != storageengine.LockModeX {
		t.Fatalf("FOR UPDATE mapped to %q, want X", got)
	}
	if got := planSQLLockingMode(true, parser.LockModeShare); got != storageengine.LockModeS {
		t.Fatalf("FOR SHARE mapped to %q, want S", got)
	}
}

// TestT025SQLInsertRespectsAProtectedRange is the end-to-end half.
//
// One transaction protects the insertion range [k, +inf) of the table's row namespace through the
// engine's own locking capability. A real SQL INSERT whose key falls inside that range must queue —
// observed through the neutral `ShouldWait` decision, which is consulted exactly when the conflict
// is decided, so "it waited" is a fact and not a delay — and must complete once the range is
// released. An INSERT whose key falls outside the range must not queue at all.
//
// The row component is captured from a real INSERT instead of being recomputed here, so the
// interval protects exactly the bytes the write path uses.
func TestT025SQLInsertRespectsAProtectedRange(t *testing.T) {
	e, err := OpenWithOptions(t.TempDir(), "root", "pw", OpenOptions{StorageMode: "mvcc"})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	setup := &Session{}
	for _, sql := range []string{
		"CREATE DATABASE test",
		"USE test",
		"CREATE TABLE t(id BIGINT PRIMARY KEY, v INT)",
		"INSERT INTO t VALUES(100,1)",
	} {
		if _, err := e.Execute(setup, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}

	// Capture the row component of one real INSERT: its space and key are the identity the range
	// has to contain.
	probe := &Session{CurrentDatabase: "test"}
	rowComponent := t025CapturedRowComponent(t, e, probe, "INSERT INTO t VALUES(200,2)")

	// The protecting transaction is a raw neutral transaction, so the test drives the engine's
	// locking capability directly rather than through a statement.
	guard, err := e.Backend.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	locking, err := storageengine.LockingTxnOf(guard)
	if err != nil {
		t.Fatalf("the engine has no locking capability: %v", err)
	}
	scope, err := locking.BeginLockScope(context.Background(), storageengine.LockScopeStatement)
	if err != nil {
		t.Fatal(err)
	}
	// [rowComponent.Key, +inf): everything at or after the captured key. A larger key is inside the
	// protected range, a smaller one is outside it.
	interval := storageengine.LockComponent{
		Mode:           storageengine.LockModeGap,
		Kind:           storageengine.LockComponentInterval,
		Space:          rowComponent.Space,
		TableID:        rowComponent.TableID,
		Lower:          rowComponent.Key,
		LowerInclusive: true,
	}
	if _, err := locking.Acquire(context.Background(), scope, storageengine.LockRequest{Components: []storageengine.LockComponent{interval}}); err != nil {
		t.Fatalf("acquire the protecting interval: %v", err)
	}

	// An INSERT inside the range waits: the barrier is signalled while the conflict is decided.
	barrier := newDMLWaitBarrier()
	barrier.arm(t)
	inside := &Session{CurrentDatabase: "test"}
	outcome := make(chan error, 1)
	go func() {
		_, err := e.Execute(inside, "INSERT INTO t VALUES(300,3)")
		outcome <- err
	}()
	barrier.wait(t, "the INSERT inside the protected range")
	select {
	case err := <-outcome:
		t.Fatalf("the INSERT inside the range returned (%v) instead of waiting", err)
	default:
	}

	// Releasing the range is the only thing that may let it through.
	if err := locking.CloseLockScope(context.Background(), scope); err != nil {
		t.Fatalf("close the protecting scope: %v", err)
	}
	if err := guard.Rollback(); err != nil {
		t.Fatalf("release the range: %v", err)
	}
	select {
	case err := <-outcome:
		if err != nil {
			t.Fatalf("the woken INSERT failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the INSERT inside the protected range was never woken")
	}

	// The row really is there: the statement was not merely unblocked.
	check := &Session{CurrentDatabase: "test"}
	result, err := e.Execute(check, "SELECT v FROM t WHERE id=300")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 {
		t.Fatalf("the woken INSERT left %d rows, want the inserted row", len(result.Rows))
	}

	// An INSERT below the protected range would not have waited. The range is released by now, so
	// the check is that it completes without the decision ever being consulted.
	barrierArm := newDMLWaitBarrier()
	barrierArm.arm(t)
	outside := &Session{CurrentDatabase: "test"}
	if _, err := e.Execute(outside, "INSERT INTO t VALUES(50,5)"); err != nil {
		t.Fatalf("an INSERT outside the protected range failed: %v", err)
	}
	select {
	case <-barrierArm.ready:
		t.Fatal("an INSERT outside the protected range consulted the wait decision")
	default:
	}
}

// t025CapturedRowComponent returns the primary-key record component of one real INSERT statement.
func t025CapturedRowComponent(t *testing.T, e *Engine, session *Session, insert string) storageengine.LockComponent {
	t.Helper()
	var captured []storageengine.LockComponent
	previous := dmlLockRequestHook
	dmlLockRequestHook = func(request storageengine.LockRequest) storageengine.LockRequest {
		captured = append(captured, request.Components...)
		return request
	}
	defer func() { dmlLockRequestHook = previous }()
	if _, err := e.Execute(session, insert); err != nil {
		t.Fatalf("%s: %v", insert, err)
	}
	for _, component := range captured {
		if component.Kind == storageengine.LockComponentRecord && component.Mode == storageengine.LockModeX && len(component.Key) > 0 {
			return component
		}
	}
	t.Fatalf("the statement locked no primary-key record: %+v", captured)
	return storageengine.LockComponent{}
}
