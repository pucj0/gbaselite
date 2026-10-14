# B03 Deterministic Concurrency and MySQL Differential Matrix

**Method**: each A/B/C is a distinct session connected to an isolated temporary instance. Gate by channels/barriers or existing B02 lock-wait hooks; `time.Sleep` may only be upper-limit watchdog, never establish happens-before. For MySQL 8.4 comparison, record actual server version + `@@transaction_isolation`, storage engine `InnoDB`, autocommit state, error category, rows, ordering, timeouts, SQLSTATE, GBase HEAD.

## Expected anomalies for *ordinary* SELECT

| Isolation | Dirty read of B staged data | A sees B's update committed between reads | A sees B's new row committed between same range reads | Critical note |
|---|---|---|---|---|
| RU | **Permitted** (B03 deterministic implementation demonstrates possible) | Permitted | Permitted | Write paths still locked; no dirty write |
| RC | Forbidden | Permitted | Permitted | Same statement uses one stable view |
| RR | Forbidden | Forbidden | Forbidden | First consistent SELECT fixes transaction view, own writes remain visible |
| SERIALIZABLE | Forbidden | Prevented by locking/serialization | Prevented by predicate protection | Long/explicit transaction; independent autocommit SELECT exception |

This matrix concerns **plain consistent SELECT**. `FOR UPDATE`/`FOR SHARE` use protected current reads with potentially different visibility, and RR current reads may observe newer rows than its plain snapshot.

## Executable scenario inventory

| ID | Scope / gate | Session A and B actions / evidence |
|---|---|---|
| M01 | session state | new → RR; A SET SESSION RC; B stays RR; introspection mirrors defaults |
| M02 | next only | A SET TRANSACTION SER; first BEGIN SER; second BEGIN session default; pending consumed exactly once |
| M03 | active restrictions | A BEGIN; SET TRANSACTION RU rejected; SET SESSION RC accepted but current level unchanged |
| M04 | implicit/autocommit | A pending RC then autocommit SELECT; next txn returns session default; `autocommit=0` implicit BEGIN |
| M05 | reset | SET SESSION RU and pending SER; reset/disconnect clears pending/default and rolls back active |
| M06 | negative syntax | GLOBAL/READ ONLY/multi-values/invalid level/START WITH SNAPSHOT: no partial state changes |
| M07 | protocol parity | raw Engine.Execute / ExecuteCompatible / prepared execution all honor exact same level |
| M08 | RR first-read timing | A BEGIN; B COMMIT v2; A first plain SELECT sees v2; repeat after B v3 remains v2 |
| M09 | RR consistent view | A first SELECT id=1; B UPDATE+COMMIT; A repeat sees previous committed version |
| M10 | RR phantom | A first SELECT range empty; B INSERT COMMIT; A repeat stays empty |
| M11 | RR own staged writes | A first SELECT, then own INSERT/UPDATE/DELETE; A sees own results despite fixed view |
| M12 | RR current vs plain | A pins plain RR; B COMMIT; A FOR UPDATE sees current after locking (safe conflict behavior) |
| M13 | RC nonrepeatable | A SELECT v1; B COMMIT v2; A SELECT v2; no B uncommitted visible |
| M14 | RC phantom | A SELECT empty range; B INSERT COMMIT; A repeat SELECT contains inserted row |
| M15 | RC single-statement consistency | Gate A after first row fetched; B UPDATE/COMMIT; A remaining rows stay one committed snapshot |
| M16 | RC complex one-view | JOIN/subquery/CTE/UNION/prepared/streaming paths either same statement view or documented unsupported |
| M17 | RC legal current write | B updates after A BEGIN; A gets correct current lock and updates safely, not false stale-root rejection |
| M18 | RC write conflict | two X writes to same key cannot both overwrite without serialization/validation |
| M19 | RC uniqueness & FK | unique conflict with active owner, foreign key/duplicate exception to gap relaxation |
| M20 | RC rollback/savepoint | child abort/ROLLBACK TO does not advance other statements' view or orphan baselines |
| M21 | SER S/S | two long read txns same row both progress; no needless database-wide serialization |
| M22 | SER S/X | A plain SELECT S; B UPDATE X waits; after A ends, B resumes |
| M23 | SER range phantom | A range/empty result S-predicate; B insert into interval waits/aborts |
| M24 | SER unrelated range | A protect `(10,100)`; B insert 150 proceeds; B insert 50 waits |
| M25 | SER write skew | two transactions read disjoint target rows but shared invariant predicate; at most one unsafe commit succeeds |
| M26 | SER S→X upgrade | two readers then writers cause legal wait/deadlock victim, not hang |
| M27 | SER unsupported query | complex shape with no proven protection rejected **before any rows emit**, no grants leak |
| M28 | SER autocommit read | one-shot pure SELECT nonlocking allowed, explicit and autocommit=0 still protected |
| M29 | RU dirty UPDATE | A staged UPDATE, B RU sees it before commit, A rollback, B sees old committed |
| M30 | RU dirty INSERT | A staged INSERT, B RU range sees row before commit, rollback row disappears |
| M31 | RU dirty DELETE | A staged DELETE, B RU cannot see row, rollback row returns |
| M32 | RU only visible | RC/RR/SER reader never sees A's uncommitted stage |
| M33 | RU no dirty write | RU writer still waits/conflicts on A's X, no broken unique/FK or lost update |
| M34 | RU stage lifecycle | child premerge/rollback savepoint invisible; root rollback/disconnect removes entries |
| M35 | RU concurrent rollback | gated snapshot acquisition and writer rollback don't produce torn/mixed stage values |
| M36 | RU replication | on unsupported Raft topology RU reject explicitly and no data committed |
| M37 | view GC | RC statement view / RR txn view vs CompactHistory/GC; no premature reclaimed versions |
| M38 | epoch/restore | generation reset invalidates stale tokens and grants; no stale data returned |
| M39 | cancel/stream | EOF, LIMIT, io error, cancellation, disconnect release view token and pending lock requests |
| M40 | resources | max statement views / RU staged bytes / lock memory exact bound and +1 fail closed |
| M41 | repeatability | 100 fixed interleavings under race detector; no timeout/leak or order-dependent assertions |
| M42 | regression | A05 parent-read/child-write, B01 conflicts+retention, B02 fairness/gap/diagnostics, old data fixture |
| M43 | replication | RC/RR/SER leader/follower barriers and invalid epoch; valid cases or explicit unsupported |
| M44 | DBA metadata | SHOW VARIABLES/SELECT @@tx_isolation / connection reset / CLI and driver responses match real state |

## Environment command patterns (implementation phase only)

```bash
gofmt -l $(git diff --name-only -- '*.go')
go test ./... -count=1
go vet ./...
go test -race ./... -count=1   # platform permitting; otherwise report exact reason
```

Use one isolated MySQL 8.4 InnoDB container and one isolated GBaseLite instance, clean temp databases, and output `b03-parity.md` with columns: `{ID, GBase result, MySQL result, classification, code evidence, test name, git SHA}`. Mark `UNSUPPORTED` explicitly, never color it PASS. Race detector may be expensive; run on supported platform and preserve failure output.
