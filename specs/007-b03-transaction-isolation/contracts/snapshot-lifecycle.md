# Contract: RR Lazy, RC Statement Views, GC and Lifetime

**Status**: 最终已实现与闭环行为契约 (Phase 9 Release Candidate Verified)

## Read routing matrix

| isolation | Plain consistent SELECT | Explicit `FOR UPDATE` / `FOR SHARE` | DML target / constraint reads |
|---|---|---|---|
| RU | atomically sampled committed + *visible completed uncommitted root stages* (standalone) | B02 protected current read | RC-style protected latest committed, never dirty integrity decision |
| RC | fresh **per-statement** committed Read View | B02 protected current read | current read and lock; statement view for nonlocking source only where safe |
| RR | once **at first consistent SELECT**, reused until txn end | B02 protected current read, even if newer than plain snapshot | B02 current read and conflict guard |
| SERIALIZABLE | explicit/long txn: protected S + predicate read of valid current committed state; autocommit single read may optimize as nonlocking consistent read | existing B02 current read | B02 protected latest, with predicate protection where needed |

> Snapshot by itself does not eliminate write-skew. For RR, repeatable **plain SELECT** does not imply every DML uses old snapshot.

## Optional capability proposal (names illustrative)

```go
// IsolationEngine is optional: existing storageengine.Engine.Begin stays unchanged.
type IsolationEngine interface { BeginIsolated(context.Context, IsolationLevel) (Txn, error) }
// Root-owned read view token, not a raw bolt.Tx/cursor.
type IsolationTxn interface {
  BeginStatementView(context.Context, StatementViewKind) (StatementView, error)
  BeginConsistentView(context.Context) (StatementView, error) // RR lazy, idempotent
}
type StatementView interface {
  Get(space string, key []byte) ([]byte, bool, error)
  NewIterator(context.Context, ScanRequest) (Iterator, error)
  Close() error
}
```

Go interface shape is not final: phase 1 must decide how `Child`, `Guard` and subquery runner inherit token, how read-your-writes overlays are selected, and whether `BeginIsolated` risks duplicating `Engine.Begin`. Crucial: **do not modify existing public `Txn.Get` semantics for raw Go callers without explicit compatibility study**.

## Lock order / view allocation

1. Wait for Raft barrier (when applicable) and check current generation/term.
2. Under short `Store.views` protection atomically read published head, allocate registered root/statement view token and pin history. Never open long bbolt read tx while taking a B02 potentially blocking lock.
3. Share this token with all operators, scalar subqueries, CTEs, joins, prepared execution and stream until statement completion; child write-stage reads include parent+own staged overlay but never unmerged sibling.
4. On `Close`/EOF/LIMIT/cancel/error release token, iterators and borrowed buffers, then check generation mismatch and surface error if changed.
5. After statement, RC next statement may advance. RR holds the first consistent token until root terminal, and locked reads before it do not pin it.

## GC semantics

- Current `TransactionManager.snapshotRefs` pins root `ReadTS` immediately. B03 introduces view owners (root RR/statement RC) so `oldestReadTS` must incorporate **all live valid tokens**; do not migrate GC separately from view creation.
- No historical committed version can be purged while referenced by active read view. An unneeded B01 root begin pin must not remain forever under long RC root; exact fallback scope must be documented.
- Even empty SELECT should create the RR first consistent read view (to prevent future phantom). Metadata SHOW and Current Read are not consistent-read triggers without an explicit documented decision.
- RU stage overlay does not belong to `snapshotRefs`; its copy/borrow lifecycle is separately counted, with generation invalidation.

## Savepoint & Child View Inheritance

1. Root owns the RR consistent read view.
2. A Child transaction created by `t.Child()` (e.g. for a SQL SAVEPOINT) inherits the root's isolation level and shares the root's consistent read view revision.
3. Reading in a Child walks from the Child's staged writes through ancestors to root, ensuring own writes and ancestor writes take precedence over the consistent snapshot.
4. If a consistent view was not yet established prior to SAVEPOINT, the first plain consistent read in the Child establishes the root's consistent view.
5. Child `Commit` (savepoint release/merge) and Child `Rollback` (`ROLLBACK TO`) do not release the root's consistent view pin. Only root `Commit`, root `Rollback`, or terminal transition releases the root's consistent view pin.

## Non-negotiable close scenarios

`COMMIT`, `ROLLBACK`, `ROLLBACK TO`, failed child MERGE, deadlock victim, lock wait timeout, context cancel, statement parse/bind error, iterator EOF, LIMIT early exit, stream sender error, connection reset/disconnect, store Restore/Close, Raft epoch/term change.

## Tests / instrumentation

Each transition asserts owned view tokens and epoch, retained bytes/horizon, open cursors, root lock owner and pending queue entries. Use gate hook to race GC against statement BEGIN/Close; use exact before/after counters, not sleeps. Legacy B01 fixed begin behavior is an intentionally changed SQL semantic only as specified; raw API migration is separately gated.
