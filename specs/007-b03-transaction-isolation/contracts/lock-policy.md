# Contract: Isolation-Aware B02 Lock Behavior

**Status**: 最终已实现与闭环行为契约 (Phase 9 Release Candidate Verified)

## Ordinary reads / locking reads / DML

| Level | Plain SELECT | SELECT FOR UPDATE / SHARE | DML locked writes | Gap / phantom expectations |
|---|---|---|---|---|
| RU | no blocking consistent-read row locks; may read stable staged data | B02 current lock | X + necessary unique/FK protections | phantom allowed; no dirty write |
| RC | one statement committed view | B02 current lock | record X, duplicate/FK exceptions | ordinary phantom allowed, reduce unnecessary gap locks where provable |
| RR | txn committed consistent view | B02 current lock | B02 range/next-key according to existing plan | ordinary SELECT phantom-free by view; locking range protects insert gap |
| SERIALIZABLE | explicit/long txn: **shared current record + predicate** claims; root retains grants | B02 current lock | X, interval protections | predicates must not exhibit illegal phantom/write-skew |

## Serializable execution rules

1. Explicit `BEGIN` or `autocommit=0` gives long transaction; plain SELECT is locking S-equivalent even without `FOR SHARE` token.
2. Build/validate a complete base-table access/lock plan before streaming any result. For joins/subqueries/CTEs/view/aggregation/UNION: lock **every relevant base relation and predicate** or pre-execution fail closed; a protected parent row with an unlocked subquery does NOT establish serializability.
3. For PK point: intention IS + Record S; absent point protects insertion position with a bounded Gap, not `[key,+∞)` unless that is true successor region.
4. For range: S record on visited keys + proven Next-Key/Gap/whole-domain predicate; insert within predicate waits, unrelated ranges can advance.
5. If a read visits zero records, it still needs predicate protection; otherwise empty-range phantom is possible.
6. IS/IX are intention declarations, **not** table S/X; may not use an IS lock alone as blanket protection for a table scan.
7. If planner shape cannot be proven safe, return `ErrLockPlanUnavailable` or equivalent with no rows emitted and no residual pending claims. Whole-domain fallback is acceptable only if strong enough to block all relevant future INSERT/UPDATE/DELETE and bounds/persistence of grant are provable, not a global database mutex.
8. Mixed reads and writes, S→X upgrades, two readers later writing disjoint records but violating a predicate must produce serialization by waiting or a deterministic deadlock/abort path; cannot allow both invalid commits.
9. `autocommit=1` one-shot pure SELECT may be served as nonlocking consistent read (MySQL allowance); must not reuse this exception in long/explicit transactions.

## RC special cases

MySQL generally disables gap locking for RC locking read/DML except FK and duplicate checking. Implement only where B02 lock resource planner safely distinguishes these paths. **If not implemented**, document conservative over-locking as a KNOWN DIFFERENCE and add non-regression tests; do not remove correctness-critical gap reservations globally.

## Lock wait lifecycle

B02 statement scope Close removes pending requests only, root grants persist until COMMIT/ROLLBACK. Deadlock victim, timeout, cancellation, resource exhaustion retain the B02 error/rollback discipline; test owned claim counts and resource queue cleanup. No bbolt read txn, iterator or stage views held while waiting.

## Shape inventory (classification must be updated during Phase 6)

| Query form | Expected treatment |
|---|---|
| SELECT by PK, PK range, empty range | MUST support and prove isolation |
| SELECT by UNIQUE; secondary index lookup | MUST test actual plan; safe support OR transparent unsupported, never silent fallback |
| Full-table scan | MUST use protected whole-domain (or safely fail closed with documented limit); basic SERIALIZABLE claim depends on useful simple table scans |
| JOIN / scalar correlated subquery / CTE / VIEW / UNION / GROUP BY | extend predicate protection end-to-end OR fail closed before visible output; excluded shapes listed README |
| SELECT FOR SHARE, FOR UPDATE | preserve B02 current read logic; NOWAIT/SKIP LOCKED behavior remains B02 explicit support policy |

## Test matrix

Deterministic S/S same row concurrent, S/X blocking, independent-row progress, absent-key gap, empty range, interval boundary adjacent, write skew, reader DML same transaction, two readers upgrading, lock timeout/deadlock, query cancel, full-scan fallback, SELECT plan rejection, replication epoch reset. Never assert a fixed lock wait wall-clock duration as the ordering proof.
