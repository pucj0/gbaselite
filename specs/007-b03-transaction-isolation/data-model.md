# Data Model: B03 Isolation and Read-View Ownership

本文是**逻辑模型与安全不变量**；函数/字段名称为实现候选，须在 Phase 1 与现有 API 一起定稿。

## Entities

```text
ServerDefaultIsolation [RR, readonly in B03]
    └── SessionIsolationState (per connection)
            ├── sessionDefault: IsolationLevel = RR
            ├── pendingNext: optional IsolationLevel
            ├── activeRootID: optional opaque ID
            └── activeIsolation: optional IsolationLevel
                     │
                     ├── TransactionReadView (RR/SER: lazy then stable)
                     ├── StatementReadView (RC/RU: one SQL statement)
                     ├── SerializableReadClaims (root-held S + predicate)
                     ├── ValidationBaselines (per key/guard; B02 integrated)
                     └── RUUncommittedView (standalone capability only)
```

| Entity | Fields & ownership | Lifetime |
|---|---|---|
| `IsolationLevel` | READ_UNCOMMITTED/READ_COMMITTED/REPEATABLE_READ/SERIALIZABLE, validated enum | Compile/Session |
| `SessionIsolationState` | default/pending/active; session owner only | reset/disconnect |
| `RootTxnIsolation` | frozen level+root ID+generation+term | root BEGIN → terminal |
| `ReadViewToken` | published head/revision, generation, ref-count/owner/scope, `closed` | RR root or RC statement |
| `StatementViewContext` | one active token, SQL statement ID, child inheritance, no resource lock over waiting | statement execute & stream drain |
| `PendingStagedRow` | writer root, space, key, value/tombstone, completed merged-stage version, monotonically stable publication ordinal | stage flush → root finish |
| `UncommittedReadView` | copied sorted staged entries, owner liveness/epoch, published baseline, bounded memory bytes | one RU statement |
| `WriteValidationBaseline` | key/dependency, source (snapshot/current), observed committed version, grant validity, generation | root until finish |
| `PredicateClaims` | table/index identity, proven intervals and held S/gap/next-key grants | root until finish |

## State transitions

**Session:** `fresh(RR)` → `SET SESSION RC` → `sessionDefault=RC` → `SET TRANSACTION SERIALIZABLE` → `pendingNext=SER` → `BEGIN` success → `activeLevel=SER, pending=empty` → `COMMIT/ROLLBACK` → `active=empty, sessionDefault=RC` → next BEGIN active RC → connection reset RR.

**RR view:** root created (`NO_CONSISTENT_VIEW`) → first **nonlocking consistent SELECT** atomically capture+pin (`PINNED`) → further SELECT reuse same token → root terminal (`CLOSED`) release pin. `FOR UPDATE`/`FOR SHARE` before first ordinary SELECT does not create RR consistent view.

**RC view:** root may exist without view → statement open capture+pin exactly one published head → child/iterators/subqueries reuse token → statement complete/cancel/EOF/Close/error releases → next statement gets a new head.

**RU view:** statement open capture+pin committed baseline + acquire a stable copy/borrow of **complete merged stages** and owner epoch → read until Close; never return raw mutable stage references. If snapshot cannot be acquired within bounds, error before returning mixed results.

**SER view:** RR consistent view only for autocommit standalone optimization; explicit/implicit multi-statement transactions acquire root-held predicate claims for ordinary SELECT and serve *current* protected rows according to proven scan plan. Do not present a stale RR snapshot as a freshly locked serializable read.

## Core invariants

- INV-B03-01: Exactly one active level per root, immutable until root ends, independent of session `SET SESSION`.
- INV-B03-02: At most one pending next override; consume atomically only on successful transaction start.
- INV-B03-03: At most one active committed Read View per RC SQL statement, reused by every suboperator; no mid-statement refresh.
- INV-B03-04: RR root consistent view is immutable after first creation; when absent, no synthetic snapshot pinned solely to satisfy SQL reads.
- INV-B03-05: `Store.views` / manager lock order cannot be inverted; never hold bbolt iterator/view across B02 lock wait.
- INV-B03-06: A ref-held view always protects every committed version needed for its Head against GC; every terminal path removes exactly owned refs.
- INV-B03-07: No dirty version used as committed write-validation baseline; uncommitted stage from another owner cannot be committed by observer.
- INV-B03-08: Savepoint/child writes inherit the statement's read semantics but do not grant visibility of unmerged sibling/child writes to other roots.
- INV-B03-09: Serializable predicate protection covers every row/insert position whose change could invalidate the query; missing proof means fail closed.
- INV-B03-10: All visible tokens/grants validated against generation and term after Restore/leader change; cancel paths clean up.
- INV-B03-11: Pinned views and RU snapshot bytes have configurable/bounded capacity; budget rejection is safe and observable.
- INV-B03-12: B01 existing storage format + B02 lock ownership are intact; no current-read access bypassing grant validity.

## Transition failure outcomes

| Transition | Failure | Required state |
|---|---|---|
| Begin with pendingNext | backend Begin fails | pending preserved, no active txn |
| RC statement open | view pin allocation fails | statement rejected, root unaffected, no pin |
| RU staging view | stage owner rolled back during copy | restart bounded acquisition or safe failure; never partially visible copy |
| SER claim | wait timeout/deadlock/cancel | B02 cleanup (timeout statement, deadlock root victim), no mixed unlocked rows |
| Restore/epoch switch | generation mismatch | view invalid, ErrConflict/appropriate fail closed, no stale read |
| Statement read EOF/Close/cancel | normal/error end | all owned view pins released, root grants retained until root terminal |
