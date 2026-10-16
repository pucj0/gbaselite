# Contract: RU Uncommitted Visibility Without Dirty Writes

**Status**: 最终已实现与闭环行为契约 (Phase 9 Release Candidate Verified)

**Core requirement**: RU must be capable of observing another active root's **completed, merged, not-yet-committed** stage changes. A committed `Store.Get(Head)` is RC, not RU. **No unsafe direct read of writable stage file/memory**.

## Source and visibility model

A writer owns statement child stages and merges them into its root using A05 rules. Only **successfully merged root-visible writes**, after an atomic staging publication point, can enter a reader's dirty-read index. The writer's *unmerged current child*, temporarily partial stage/buffer, and rolled-back savepoint layers are invisible to other roots. A dirty-read entry carries `{rootID, generation, term, mergeOrdinal, space, key, value/tombstone, complete=true}` and is copied/referenced safely under a bounded ownership guard.

When several writers stage the same key: define a deterministic candidate choice (e.g., latest *complete* mergeOrdinal among active roots), but do **not** claim it mirrors every InnoDB read path. Never combine bytes of two partial records. For a range iterator use one sampled atomic collection with stable ordering and bounds for the statement; note RU need not be transaction-consistent, but **must** be memory safe.

## Precedence (one RU SELECT statement)

1. Reader's own merged root/ancestor writes (read-your-writes) always win.
2. Stable sampled foreign active-root completed staged version may be returned, including tombstone/insert.
3. Otherwise fallback to statement's committed published version; determine `exists` and `tombstone` from correct visibility layer.
4. Query filters / ORDER BY / JOIN over dirty-staged overlays use a coherent row set, not an independently dirty index tuple combined with an unrelated row. If safe reconstruction unavailable, reject that query shape in RU, document it and test rejection.

## Lifecycle and security

- Owner status must be ACTIVE/appropriate COMMITTING **before publication**; root terminal drops its dirty-visibility entries. Do not read already COMMITTED entries via dirty source after commit: they are now fetched from durable published head.
- All pointers/byte slices returned to executor are owned copies or pin-protected until iterator Close; no refs to mutable writer buffers.
- Commit/rollback/restore/epoch invalidate or replace dirty index atomically; generation/term must agree before and after snapshot acquisition.
- Uncommitted entries never affect `Command`, WAL, backup, replication visibility, unique/FK constraint decisions, or commit validation baselines.
- Limit total dirty index rows/bytes/per statement copy, trace owner/key counts only (not row content); use bounded error rather than OOM.
- Raft/leader-follower read topology: RU unavailable by default in B03 (explicit Unsupported); **do not advertise** RU when active topology cannot support it safely.

## SQL test schedules

**UPDATE dirty read**:
```text
A: BEGIN; UPDATE t SET v='staged' WHERE id=1; [signal merged-stage visible; do not commit]
B: SET SESSION TRANSACTION ISOLATION LEVEL READ UNCOMMITTED;
B: BEGIN; SELECT v FROM t WHERE id=1; --> staged (controlled window)
A: ROLLBACK;
B: SELECT v FROM t WHERE id=1; --> original committed v
B: ROLLBACK;
```
**INSERT dirty phantom**: A stages INSERT matching range; B RU SELECT range sees row before A commit; rollback makes it disappear, with correct PK/secondary predicates.  
**DELETE**: A stages DELETE; B RU SELECT no longer sees row; rollback restores.  
**Visibility barrier**: C (RC/RR/SER) never sees A stage before commit.  
**Dirty write prevention**: B RU attempts UPDATE same key while A holds X; blocks/timeout/conflicts per B02 and cannot commit an invalid result.  
**Snapshot-vs-rollback race**: A rollback while B acquires snapshot; B sees either fully valid pre-rollback staged snapshot or post-rollback committed state according to precise atomic sample, never torn bytes/panic.  
**Reset**: root rollback, savepoint rollback, deadlock victim, Restore all remove ghost entries.

**Acceptance warning**: MySQL's RU may choose older versions under some timings; parity tests must assert *allowed phenomena* and safety invariants, not that every MySQL SELECT must return the dirty value.
