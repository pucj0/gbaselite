# B03 Workflow and Authorization Gates

## Intended handoff

**Turn A（当前）**：交付 Spec Kit 文档包，检查边界和设计选择；用户决定是否调整文档，之后自行推送。此时所有任务未执行。

**Turn B（用户推送后）**：读用户确认的 branch/HEAD 和本仓库 `specs/007-b03-transaction-isolation/`，对比文档变化，再提供**每个 Phase 的解释、前置条件、中文 Codex/AI Prompt**。提示词必须以 HEAD、FR/SC 和代码现状为输入，不得套用旧版本代码；由用户决定授权哪个 Phase 的实现。

**Turn C（分阶段执行）**：每次只实现授权的 Phase 或任务段；用户推送本轮代码后进行 review，测试通过再进入下一阶段；不得默默连续执行完全部 Phase。

## Per-phase check-in template

```text
Phase / exact Task IDs:
Git branch / HEAD before & after:
Changed paths:
Tests added and tested:
Commands / exit codes:
Deterministic gate/parallel race results:
B01 / B02 / A05 regression:
README / compatibility changes:
Unresolved risk / UNKNOWN / FAIL:
User approval for next Phase? (not automatic):
```

## Blocking gates

- **Before Phase 3**: isolation state machine complete, `BEGIN` freeze/next-only semantics tested.
- **Before Phase 4**: read view pin ownership + GC + old public Txn compatibility proved.
- **Before Phase 5**: RR first-read and existing B02 current read remain coherent.
- **Before Phase 6**: RC validator and lock behavior proved safe for current DML.
- **Before Phase 7**: RU staging publication capacity, ownership, topology restriction specified; staged data never becomes commit baseline.
- **Before Phase 8/9**: all safety-critical tests and protocol parity pass; unstable/flaky tests are blockers, not 'mostly passed'.

## Reports storage convention

`.tmp/b03-{baseline,readview,validation,locking,dirty-read,parity,race,regression}.md` are **implementation-run evidence**, not shipped as fabricated prefilled results. If a persistent committed report is wanted, move redacted summary to `specs/007-b03-transaction-isolation/verification.md` only after real execution, with git SHA and command output.

## Red lines

No data/ writes, no silent downgrade from RU/SER, no global transaction mutex for serializability, no public minimal Txn breakage, no WAL/Command/Raft format drift, no pretending planning notes are runtime proof, no release/deployment until separate authorization.
