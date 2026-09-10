# A01–A03 Deep Hardening delivery and Finalization-2

Earlier OPEN statements below are historical acceptance snapshots. The final lightweight-verification section records the current decision and supersedes those snapshots.

Original implementation baseline: master dd44f57. Finalization-2 starts from f10e5a2, fetched and confirmed equal to origin/master (0 ahead / 0 behind). Existing data/ was not used for write tests; no deployment was performed.

| Task | Delivered |
| --- | --- |
| 1 | Direct Table/KeyRange Iterator → Scan for SELECT and DML; callback compatibility boundary isolated; AST regression negatives |
| 2 | boundQuery composes projection/group/window/union with Distinct without intermediate Result.Rows; final collector retains transaction ownership and result budget |
| 3 | Join3 with independent left/right/output types; Join forwards to the sole algorithm |
| 4 | Begin/Close Engine core; independent optional capabilities; core-only test backend and contract fixture |
| 5 | sqllayout namespace and signed integer key ownership, byte golden tests, compatibility Table handles |
| 6 | Dependency-audited SQL names/files and private transaction/catalog fields; public autocommit forwarding alias |
| 7 | PlanNode derived from actual operators, no input execution; EXPLAIN Extra adds bound pipeline while preserving twelve columns and access metadata |
| 8 | PartitionStore, GroupStore and Window EvaluateStore; per-run ownership, borrowing contract, error/close tests and spill TODO specification |
| 9 | migration/legacy owns offline reader and orchestration; command injects a logical snapshot target; executor has no legacy persistence dependency |
| 10 | File sync, Unix directory sync, Linux atomic no-replace publish, Windows write-through publish; five failure points on both source formats and target recovery tests |

Compatibility review found that removing eager grouped/union results could suppress later computation under LIMIT. Drain limits preserve that evaluation without retaining a result slice; ordinary scan limits still stop early. Aggregate state is initialized by its run factory. Window slice compatibility requires explicit row retention when using a custom store.

## Validation on 2026-09-10

- Windows Go 1.26.5 and Go 1.24.6 both pass go test ./... -count=1; Go 1.24.6 also passes go vet ./.... Logs: .tmp/deep-task1-test.log through deep-task10-test.log, .tmp/deep-final2-test.log, .tmp/deep-final2-vet.log, .tmp/go124-test.log and .tmp/go124-vet.log.
- All tracked Go files pass gofmt; git diff --check passes. Local ignored .tmp probe sources are not part of the clean-checkout formatting gate.
- Windows amd64 and Linux amd64 static command builds passed. Linux migration test binary compilation and Linux go vet ./... passed.
- Existing cross-backend SQL matrix and storageengine contract suite pass. New tests cover core-only capability fallback, Distinct input/result budget separation, heterogeneous Join, plan non-execution, Window store failures/cancel/cleanup, grouped LIMIT 0 resource errors, drain-limit late errors, and migration write/before-verify/after-verify/before-rename/after-rename recovery.

## Finalization-2: historical failure, root cause and fix

Historical Windows Actions run [34442648257](https://github.com/pucj0/gbaselite/actions/runs/34442648257), commit 974a903939ffeab858fc2dcc172108944dc78857, failed quality job 102760726406. The previous report could only observe the exit-code annotation because raw logs required authentication. The failure excerpt subsequently supplied with Finalization-2 identifies failover/TestProxyRoutesAfterLeaderFailure: fixture CREATE DATABASE IF NOT EXISTS test returned driver: bad connection. Executor, migration/legacy, mvcc, physical, server, storageengine and mvccadapter passed that historical run. A later successful rerun did not establish that the intermittent failure was fixed.

Root cause: Router treated one unsuccessful discovery round as confirmed leader loss, set its address to empty, and closed all current proxy connections. A healthy leader can temporarily fail the combined status/SELECT 1 probe under scheduling or Raft propagation delays. Each peer still has a 750 ms probe deadline; the discovery ticker remains 500 ms (a round can probe three peers sequentially). Windows runner scheduling can expose this window; it is not a Windows-specific SQL or persistence defect. The historical runner timing itself was not captured. The causal disconnect path was reproduced deterministically using an injected probe and a pinned sql.Conn: the pre-fix test failed with 'transient probe failure cleared confirmed leader'. Commit dd9e28c records that reproduction before the production fix.

The state change is deliberately small:

- A successful probe resets the miss count. One or two unsuccessful whole rounds retain the last confirmed leader and its connections; the third consecutive unsuccessful round clears it. Initial discovery still has no leader until a probe succeeds.
- A newly confirmed B replaces A immediately, without waiting for the miss threshold. Existing A transports close; subsequent connections dial B.
- Discovery rounds are serialized. A generation check rejects stale probe results and connections dialed across an A → B → A transition.
- Shutdown forces a transition without grace. Closing a routed connection closes both client and backend exactly once, outside the state mutex, so a copier blocked on a backend write can exit.
- Router does not replay SQL. No new SQL retry, larger probe deadline, sleep, Windows skip, or ignored bad-connection error was added. Existing fixture retries and duplicate-key tolerance were removed.

Production fix: 8d70628. Changes are confined to failover, its tests, README, the CI race step and this report. Engine/Txn, Physical Operators, Distinct, Join3, PlanNode, sqllayout, migration, SQL behavior, persisted bytes and MySQL wire framing are unchanged in this round. Existing architecture guards remain enabled and unweakened.

Tests added: TestProxyKeepsLeaderAcrossTransientProbeFailure; TestProxyConfirmsLeaderLossAfterConsecutiveMisses (including success resetting misses); TestProxyConfirmedLeaderChangeClosesOldConnection; TestProxyRejectsStaleDiscovery; TestProxyTransitionUnblocksBackendWrite; TestRouterConcurrentDiscoveryAndConnections. The real three-node TestProxyRoutesAfterLeaderFailure still shuts A down, discovers B, opens a new client, writes once, and checks COUNT = 2 and SUM = 30. Test servers join Serve before Shutdown to keep fixture lifecycle synchronization explicit.

## Finalization-2 acceptance

Status: OPEN pending Linux race execution and remote CI. Automatic approval review rejected pushing to the shared origin/master branch without explicit push authorization; authorization has been requested. All local work and required local checks are complete. No architecture freeze is claimed until all required jobs pass.

| Check | Result |
| --- | --- |
| go test ./failover -count=20 | PASS, final version: 71.839 s |
| go test ./failover -count=50 | PASS, final version: 166.355 s |
| go test -race ./failover -count=10 (Linux) | PENDING remote quality race step |
| go test ./... -count=1 | PASS, final version |
| go vet ./... | PASS, final version |
| quality (ubuntu-latest) | PENDING |
| quality (windows-latest) | PENDING |
| build (ubuntu-latest) | PENDING |
| build (windows-latest) | PENDING |
| mysql-8-client, including dump/import | PENDING |

Local logs use .tmp/finalization2-count20.log, .tmp/finalization2-count50.log, .tmp/deep-finalization2-test.log and .tmp/deep-finalization2-vet.log. Local validation uses Windows Go 1.24.6. Both stability commands finished with zero failures; all tracked Go sources pass gofmt and git diff --check passes. Linux race is an explicit, non-skipped Ubuntu quality step; Windows has no local C compiler for race. Remote CI must execute the MySQL 8 smoke step rather than accept a skipped job.

A01 = OPEN (final acceptance pending); A02 = OPEN (final acceptance pending); A03 = OPEN (final acceptance pending); Deep Hardening = OPEN. Local tested code baseline: ff1789d5da472324c7c057abd4fd5f50eed6bd47. Final remotely validated baseline: pending.

## Frozen architecture rules for A04+

These existing boundaries remain mandatory while final acceptance is pending and after closure:

1. No direct bbolt dependency from SQL/Executor/Physical.
2. No direct concrete MVCC backend dependency from SQL/Executor/Physical.
3. No legacy runtime.
4. Legacy persistence only exists in offline migration modules.
5. New SQL features must compose common Physical Operators.
6. Core SELECT must not introduce Operator → callback → Operator roundtrips.
7. Core SELECT must not introduce unnecessary Result.Rows materialization.
8. SQL keyspace encoding belongs to sqllayout.
9. New storage backend must satisfy storageengine contracts.
10. Optional Engine features must remain optional capabilities.
11. A04 SQL capability migration must not bypass storageengine.
12. Any architecture exception must have an explicit compatibility boundary.

## Remaining technical debt and API effects

- External hash aggregation and disk-backed SQL window frames remain deferred as explicitly allowed by Task 8. Current SQL retains its memory limits; Window still uses the compatibility slice evaluator. See spill-stores.md for the staged implementation contract.
- Multi-step index probes retain one byte callback/iterator bridge. Historical helper and external/materialized Result adapters remain; core SELECT/JOIN/group/window binding does not round-trip through them.
- Core SQL consumes its Txn before closing it. Lazy protocol streaming that owns a live transaction is not introduced.
- Table convenience handles still use a compatibility bridge to sqllayout. No logical namespace bytes or persisted table/row metadata fields were renamed.
- Plan estimates remain unknown and dynamic join probes are described as dynamic. There is no optimizer, EXPLAIN ANALYZE or new isolation/locking behavior.
- Residual Executor names and user-facing errors that described the concrete backend were changed to SQL/storage-neutral names; actual MVCC maintenance/parser messages and the deprecated SetMVCCAutocommit compatibility API remain intentionally unchanged.
- Go callers needing former full Engine capabilities may use FullEngine or assert individual capabilities. Missing explicit optional features return ErrUnsupported. The public autocommit alias remains; offline migration Go callers move to legacy.Migrate with TargetOpener. CLI migration flags, SQL messages and MySQL wire framing remain unchanged; EXPLAIN Extra intentionally gains descriptive text.
- Failure injection verifies publication/recovery invariants, not a physical power-loss experiment. Final Linux durability runtime verification is part of the pending Ubuntu full test job. Windows cannot offer Unix directory fsync semantics.
- Deferred: Txn.Table compatibility bridge removal; transaction read-view redesign; four isolation levels; record/gap/next-key locks; deadlock detector; optimizer; cardinality statistics; EXPLAIN ANALYZE; Pebble backend. No B-stage isolation/locking or new HA architecture is introduced.

## Finalization-2.2 diagnostics (2026-09-10)

Baseline c90abd4 was fetched and confirmed equal to origin/master before this round. The complete Windows log for [Test run 34454567559, job 102797842614](https://github.com/pucj0/gbaselite/actions/runs/34454567559/job/102797842614) establishes: initial discovery of 127.0.0.1:50258 at generation 1; probe-miss counts 1 and 2; generation 2 confirmed-loss with misses=3; connection close reason=confirmed-loss; fixture CREATE TABLE returned invalid connection. There was no confirmed A → B transition. This supersedes the earlier uncertainty about who closed the connection, but does not identify why the probes failed.

Observed three discovery misses:

| Round | Peer 0 | Peer 1 | Peer 2 |
| --- | --- | --- | --- |
| First miss | Stage/duration/state/error unavailable in historical log | Unavailable | Unavailable |
| Second miss | Stage/duration/state/error unavailable in historical log | Unavailable | Unavailable |
| Third miss | Stage/duration/state/error unavailable in historical log | Unavailable | Unavailable |

No timeout, role mismatch, broken pool, real no-leader or readiness root cause is claimed from these missing data. Production bug / test readiness bug / probe design bug: NOT YET DETERMINED. Source inspection shows SHOW REPLICATION STATUS reads status directly through the server handler; both confirmation statements shared one 750 ms context. Neither fact alone proves where the historical deadline or failure occurred.

Diagnostic implementation c32d0bb adds private probeResult and discoveryRound records. Every attempted peer records its acquisition/status/read stage, replication identity/state/leader/address/applied index, acquire/status/ping/total durations, remaining shared budget before SELECT 1, original error and classified cause. Whole rounds record monotonic IDs and start/end timestamps. Existing transition and first connection-close diagnostics remain enabled only by tests. No public HA API, production log stream or wire change is added.

One probe now owns one sql.Conn for both confirmation statements, as requested; this is a structural guarantee, not a claim that the previous pool caused the failure. The strict three-node integration fixture waits for three consecutive actual confirmations of the same leader and generation. A miss or transition resets readiness. It checks the fixture generation, then shuts down the actual leader, awaits a stable successor, reconnects and verifies COUNT=2 and SUM=30. No fixture SQL retry or duplicate-key tolerance is introduced.

New tests: TestProbeLeaderDiagnostics (acquire/status/role/read/success), TestProbeUsesSingleConnection (zero idle pool forces separate checkouts to have different identities), TestProbeErrorClasses, TestDiscoveryRoundDiagnostics and TestProxyWaitsForStableInitialLeader. Existing transient-miss, loss, transition, stale-generation, blocked-writer, concurrent-shutdown and real failover tests remain.

Timeout changed: NO (750 ms); ticker changed: NO (500 ms); leaderMissThreshold changed: NO (3). There is no evidence supporting a parameter change. No Storage Engine, MVCC, Physical Operator, sqllayout, migration, WAL, locking or optimizer architecture is modified.

Validation on Windows Go 1.24.13: go test ./failover -count=20 PASS (110.310 s); go test ./failover -count=50 PASS (278.892 s); go test ./failover -run TestProxyRoutesAfterLeaderFailure -count=100 PASS (520.726 s). All completed with zero failures. Full go test ./... -count=1, go vet ./... and formatting checks PASS. Logs are .tmp/finalization22-count20.log, .tmp/finalization22-count50.log, .tmp/finalization22-real100.log, .tmp/deep-finalization22-test.log and .tmp/deep-finalization22-vet.log. The local Docker Linux daemon is unavailable, so no local Linux race pass is claimed. New remote Windows probe-stage evidence, Linux race x10, quality/build/MySQL 8/Docker acceptance remain pending explicit authorization to push to shared origin/master. Previous baseline Linux quality/race, both builds and Docker passed; Windows failed and MySQL 8 was skipped. Previous passes are not final validation of this change.

A01 = OPEN pending final validation; A02 = OPEN pending final validation; A03 = OPEN pending final validation; Deep Hardening = OPEN. All previously listed deferred work and A04+ architecture rules remain unchanged.

## Final lightweight-verification acceptance (2026-09-10)

Code baseline: e1ba8da973badc341319d98300cbe7cb9c05e075. This round started from master 7652ebb1ba0969f8ed90ce5858e655fd7081bc6a, fetched and confirmed equal to origin/master. Only failover leadership verification and its test lifecycle were changed; no B-stage isolation/locking, core Engine/Txn or Physical Operator redesign was performed. Existing data/ was untouched; all write tests used isolated temporary databases.

### Historical evidence and confirmed root cause

The complete Windows raw log from [run 34462470465 (Run #56), job 102823248533](https://github.com/pucj0/gbaselite/actions/runs/34462470465/job/102823248533) resolves the previously unknown per-peer failure stages. Node 0 remained Leader, and both followers kept reporting Leader_ID=0. Successful SELECT 1 probe latency rose through approximately 284, 299, 446 and 657 ms before the following failures:

| Discovery round | Node 0 status | Node 0 SELECT 1 | Node 1 | Node 2 |
| --- | --- | --- | --- | --- |
| 6 | Leader/0; 0.359 ms | 750.472 ms; context-deadline | Follower; leader=0 | Follower; leader=0 |
| 7 | Leader/0; 0.673 ms | 749.418 ms; context-deadline | Follower; leader=0 | Follower; leader=0 |
| 8 | Leader/0; below timer resolution | 749.928 ms; context-deadline | Follower; leader=0 | Follower; leader=0 |

After round 8, Router changed generation 1 → 2, cleared the leader with confirmed-loss/misses=3, and actively closed the existing connection. CREATE TABLE then returned invalid connection. Stable readiness had already completed; the cause was not initial readiness or an A → B election.

Root cause: Router used ordinary SELECT 1 as a liveness/quorum check. The current replicated SQL path enters Replica.Barrier, which invokes raft.VerifyLeader plus raft.Barrier and waits for FSM ordering. Repeated discovery therefore introduced full read barriers alongside real replicated work. A full SQL consistency barrier is the wrong leadership heartbeat. Probe design bug: YES. Production failover bug: YES. There is no evidence of an election defect or actual leader loss; connection pooling and startup were not this root cause. Increasing the probe timeout or miss threshold would not address the design defect.

### Actual fix and compatibility boundary

- replication.Node.VerifyLeader checks cancellation and local Leader state, then waits for raft.VerifyLeader. It never calls raft.Barrier. The existing Barrier implementation and ordinary SQL pre-barriers remain unchanged.
- storageengine.LeaderVerifier is an independent optional Replica capability, forwarded by mvccadapter. Engine core, Replica's existing required contract and backend mocks are not expanded. SQL/server/failover do not import concrete replication or Raft packages.
- SHOW REPLICATION STATUS validates a self-consistent Leader candidate through the optional verifier and rechecks identity/state before returning it. Follower and Standalone reporting retain the same behavior. All five result columns/types and MySQL framing are preserved. Missing optional verification returns ErrUnsupported; it never falls back to the full Barrier.
- Router makes one status SQL roundtrip, no ordinary SELECT and no FSM Barrier. The original 750 ms overall probe ceiling, 500 ms ticker and three-miss hysteresis remain. Server-side verification is bounded by the same 750 ms ceiling because MySQL does not carry the client's context. Transient failure retains the connection; confirmed loss or a confirmed successor still closes stale transports immediately. Generation and two-sided close protections remain.
- Diagnostics remain: per-round IDs/timestamps, each attempted peer, status-and-verification duration/budget/errors, transition history and the first connection-close reason. The removed SELECT-stage duration is replaced by the single status-and-verification roundtrip duration; no extra wire columns or production logging are introduced.

A second, distinct failure was found in [run 34469358580, Windows job 102845373754](https://github.com/pucj0/gbaselite/actions/runs/34469358580/job/102845373754) after the production repair. The log shows INSERT hit the integration client's 3-second socket read timeout. Router was still generation 1, with no confirmed-loss; close reason was client-to-backend-copy-ended. Confirmations were mostly below timer resolution to 132 ms, with only two misses before the client timed out. This was not another healthy-leader teardown.

The fixture now bounds each whole statement with a 15-second context, covering its existing two 5-second read-barrier stages plus replicated apply instead of asserting an unrelated 3-second SQL latency SLA. Its pinned sql.Conn prevents database/sql from replaying a fixture statement. No retry, ignored error, duplicate-key tolerance or production timeout change was added. Query duration/error history and COUNT=2, SUM=30, MIN(id)=1, MAX(id)=2 assertions preserve data-loss/duplication detection.

Files modified in this round: replication/node.go and node_test.go; storageengine/engine.go and mvccadapter/adapter.go; executor/transaction_engine.go and leader_verification_test.go; server/handler.go; failover/probe.go, probe_test.go, router_state_test.go, router_test.go and leader_verification_test.go; README.md; this report.

### Deterministic tests and final validation

New protocol test TestProbeDoesNotUseSQLBarrier makes ordinary Barrier fail while VerifyLeader succeeds. More than three discovery rounds still retain the Leader without any Barrier calls. An injected VerifyLeader failure leaves the same generation and pinned client alive; subsequent success recovers, while ordinary SELECT continues to invoke Barrier. Executor tests cover the optional capability, unchanged status schema, failure/cancellation, inconsistent identity and role change during verification. The real replication test verifies that leadership confirmation appends no Raft log entry and rejects follower, canceled context and minority cases. Existing transition, stale generation, blocked writer, concurrent shutdown, contract, migration and architecture tests remain enabled.

| Validation | Result / evidence |
| --- | --- |
| Windows Go 1.24.13: go test ./failover -count=20 | PASS; 107.316 s |
| Windows Go 1.24.13: go test ./failover -count=50 | PASS; 268.831 s |
| Windows Go 1.24.13: go test ./failover -run TestProxyRoutesAfterLeaderFailure -count=100 | PASS; 511.901 s |
| go test ./... -count=1 | PASS locally, Ubuntu CI and Windows CI |
| go vet ./... | PASS locally, Ubuntu CI and Windows CI |
| gofmt and git diff --check | PASS |
| Ubuntu: go test -race ./failover -count=10 | PASS; 45.834 s; quality job 102847159542 |
| quality (ubuntu-latest) | PASS; job 102847159542 |
| quality (windows-latest) | PASS; job 102847159703; failover package 11.432 s |
| build (ubuntu-latest) | PASS; job 102847159767 |
| build (windows-latest) | PASS; job 102847159696 |
| mysql-8-client | PASS; job 102847882351; dump/import smoke explicitly executed |
| Docker (linux/amd64 and linux/arm64) | PASS; job 102847159990 |

Remote evidence: [Test run 34469918569](https://github.com/pucj0/gbaselite/actions/runs/34469918569) and [Docker run 34469918674](https://github.com/pucj0/gbaselite/actions/runs/34469918674), both for e1ba8da. No failed run was rerun until green; each subsequent run tested a documented, evidence-driven code or fixture change. Local final-version logs: .tmp/lightweight-final-count20.log, .tmp/lightweight-final-count50.log, .tmp/lightweight-final-real100.log, .tmp/deep-lightweight-final-test.log and .tmp/deep-lightweight-final-vet.log.

Final decision: A01 Single MVCC Runtime = CLOSED; A02 Storage Engine Abstraction = CLOSED; A03 Physical Operator Framework = CLOSED; A01-A03 Deep Hardening = CLOSED. All required checks and the additional 100-run real failover check passed with zero failures. The frozen implementation baseline is e1ba8da973badc341319d98300cbe7cb9c05e075, validated by the runs linked above; this documentation-only closure commit preserves that code baseline. The twelve A04+ rules above remain mandatory. Deferred work also includes optimizing ordinary replicated SQL barrier placement/read consistency; this fix deliberately does not introduce ReadIndex, lease/follower/stale reads, a new consensus protocol or any B-stage transaction/lock design. Existing external aggregate/window spill, Txn.Table bridge removal, read-view/isolation/locks/deadlock, optimizer/statistics/EXPLAIN ANALYZE and Pebble debt remain deferred. After closure, stop A01-A03 changes; A04 Legacy SQL Capability Migration is the next stage.
