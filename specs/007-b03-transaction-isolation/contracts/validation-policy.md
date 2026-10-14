# Contract: Write Validation and Current-Read Baselines

## Invariants inherited from B01/B02

B01 plain snapshot validation checks `latestCommittedChange(dep) > readTS`. B02 added a **per-key observation baseline** only after a valid current-read lock grant; unobserved keys continue to use their original readTS. The baseline must be validated again at commit, including the replicated apply policy. `Command.Snapshot` / WAL / Raft schema cannot be casually expanded.

## Required B03 behavior

| Scenario | Allowed result | Forbidden result |
|---|---|---|
| RR plain stale read then unguarded write with concurrent update | deterministic conflict or safe serialization according to current B01+locking contract | silently lost update |
| RC txn begins, B updates X and commits, A later UPDATE X with valid X lock | A can safely apply to latest committed X, or documented safe retry on real conflict | false conflict *solely* because A root began before B, ignoring valid current observation |
| RC A locks X and observes V2, B attempts write X before A commits | B blocks; A commit succeeds subject to other constraints | B overwrites X while A holds covering grant |
| RU A reads B uncommitted X, then A tries to modify X | write must acquire proper current/record X validation, wait/conflict as needed | dirty write or use B uncommitted X as committed baseline |
| RC two writers change same unique value / foreign key dependency | current/unique/FK claims and eventual conflict checking prevent corruption | both commit duplicate/invalid FK |
| SAVEPOINT child rollback | only surviving merged writes/guards affect root validation | orphan per-key baseline from rolled-back child authorizes overwrite |

## Approach gate

Define a neutral per-write/per-predicate validation evidence record containing `{dependency identity, baseline kind, committed version, grant id/epoch, layer/root ownership}`. Snapshot-origin dependencies retain original proof; locked-current-origin may use B02 per-key baseline only with valid root grant. For RC where statement epoch changes, choose a formally justified baseline policy **per dependency**, not by mutating root-wide `readTS`.

Write a validator truth table spanning RR/RC/RU/SER x point/range/unique/FK x locked/current/consistent x insert/update/delete x local/replicated. Reject states that cannot be proven safe. Test pre- and post-commit database state, not merely error string.

## Replication-specific gate

Review `mvcc/current_read.go` `errBaselineNeedsLocalValidation` and B02 T055 behavior on current HEAD before claiming replicated safety; do not assume B02 design note represents current runtime. If baseline cannot be carried safely through proposal/committed apply, refuse the relevant operation with an actionable Unsupported/Conflict error; no silent local-only commit on a replicated engine.

## Test hooks

Instrument chosen write baseline and latest published version in test-only neutral traces; use gated “after-lock-before-validation”, “after-stage-before-commit”, and “before-publish” to prove no lost update even under rollback, deadlock, and leadership changes.
