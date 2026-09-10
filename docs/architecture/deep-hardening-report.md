# A01–A03 Deep Hardening delivery

Implementation baseline: master dd44f57, fetched and confirmed equal to origin/master before edits. Local commits implement Tasks 1–10 independently, followed by a compatibility fix. No push or deployment was performed; existing data/ was not used for write tests.

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

- Each task passed Windows go test ./... -count=1 and go vet ./...; final checks passed after the compatibility follow-up (Go 1.26.5). Logs: .tmp/deep-task1-test.log through deep-task10-test.log and .tmp/deep-final2-test.log / deep-final2-vet.log.
- All tracked Go files pass gofmt; git diff --check passes. Local ignored .tmp probe sources are not part of the clean-checkout formatting gate.
- Windows amd64 and Linux amd64 static command builds passed. Linux migration test binary compilation and Linux go vet ./... passed.
- Existing cross-backend SQL matrix and storageengine contract suite pass. New tests cover core-only capability fallback, Distinct input/result budget separation, heterogeneous Join, plan non-execution, Window store failures/cancel/cleanup, grouped LIMIT 0 resource errors, drain-limit late errors, and migration write/before-verify/after-verify/before-rename/after-rename recovery.
- Full Linux runtime tests and MySQL 8 dump/import smoke are NOT verified in this round. Docker Desktop fails during startup at its local dockerInference socket before the Linux engine is available. The repository CI workflow was not changed or remotely triggered. This environment blocker means the full CI acceptance criterion remains open.

## Remaining technical debt and API effects

- External hash aggregation and disk-backed SQL window frames remain deferred as explicitly allowed by Task 8. Current SQL retains its memory limits; Window still uses the compatibility slice evaluator. See spill-stores.md for the staged implementation contract.
- Multi-step index probes retain one byte callback/iterator bridge. Historical helper and external/materialized Result adapters remain; core SELECT/JOIN/group/window binding does not round-trip through them.
- Core SQL consumes its Txn before closing it. Lazy protocol streaming that owns a live transaction is not introduced.
- Table convenience handles still use a compatibility bridge to sqllayout. No logical namespace bytes or persisted table/row metadata fields were renamed.
- Plan estimates remain unknown and dynamic join probes are described as dynamic. There is no optimizer, EXPLAIN ANALYZE or new isolation/locking behavior.
- Go callers needing former full Engine capabilities may use FullEngine or assert individual capabilities. Missing explicit optional features return ErrUnsupported. The public autocommit alias remains; offline migration Go callers move to legacy.Migrate with TargetOpener. CLI migration flags, SQL messages and MySQL wire framing remain unchanged; EXPLAIN Extra intentionally gains descriptive text.
- Failure injection verifies publication/recovery invariants, not a physical power-loss experiment. Linux durability paths are cross-compiled and vetted, but await runtime verification after Docker is repaired. Windows cannot offer Unix directory fsync semantics.
