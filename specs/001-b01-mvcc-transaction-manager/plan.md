# Implementation Plan: B01 完善 MVCC Transaction Manager

**Branch**: `001-b01-mvcc-transaction-manager`  
**Spec**: `spec.md`

## Summary

在不改变 GBaseLite Snapshot Isolation 和现有 MVCC 物理版本格式的前提下，引入显式 Transaction Manager，统一事务元数据、状态转换、active registry、snapshot retention、diagnostics 和 commit conflict 语义；保留现有 optimized visibility/commit paths，通过共享 validator 和 deterministic concurrency tests 固定行为。

## Technical Context

**Language**: Go  
**Storage**: bbolt-based MVCC  
**Transaction API**: `storageengine.Txn`  
**Key modules**:

- `mvcc/transaction.go`
- `mvcc/store.go`
- `mvcc/group_commit.go`
- `mvcc/local_commit.go`
- `mvcc/local_stream.go`
- `mvcc/local_wal.go`
- `mvcc/range_guard.go`
- `mvcc/maintenance.go`
- `storageengine/engine.go`
- `storageengine/mvccadapter/adapter.go`
- `executor/transaction_engine.go`
- `executor/savepoint_mvcc.go`

**Isolation**: Snapshot Isolation  
**Constraints**:

- 不改变现有 SQL isolation contract；
- 不建立无界普通 read-set；
- 不更改现有 on-disk version layout；
- 不降低 large transaction bounded-memory 特性；
- 不破坏 standalone、LocalWAL、Raft 三种耐久路径；
- 保持 A05 parent-read / child-write 语义；
- tests 必须确定性同步。

## Constitution Check

由于当前任务未提供项目 `.specify/memory/constitution.md` 内容，本计划按仓库现状采用以下临时 gates：

- [x] Backward-compatible storage semantics
- [x] Snapshot Isolation semantics preserved
- [x] Bounded memory for large transactions
- [x] Crash-safe publication preserved
- [x] Existing tests remain enabled
- [x] New concurrency tests deterministic
- [x] No new unbounded in-memory collections on read path

若仓库已有 constitution，实施前必须以其为最高优先级重新执行 gate。

## Project Structure

```text
specs/001-b01-mvcc-transaction-manager/
├── spec.md
├── research.md
├── data-model.md
├── plan.md
├── quickstart.md
├── contracts/
│   └── transaction-diagnostics.md
├── checklists/
│   └── requirements.md
└── tasks.md
```

## Source Changes

### Phase 1: Transaction metadata/state

新增建议文件：

```text
mvcc/transaction_manager.go
mvcc/transaction_state.go
```

修改：

```text
mvcc/transaction.go
mvcc/store.go
```

工作：

- 定义 TransactionState；
- 建立 TransactionInfo；
- root Begin 注册；
- child 注册 ParentID；
- 明确 startTS/readTS；
- 在 root durable commit 后设置 commitTS；
- read-only commit 不分配 sequence；
- state transition helper。

### Phase 2: Active registry + GC retention

修改：

```text
mvcc/maintenance.go
mvcc/transaction.go
mvcc/store.go
```

工作：

- 用 manager 替代直接操作 `Store.active`；
- retention refs 仅 root transaction 持有；
- `CompactHistory` 从 manager 获取 oldest ReadTS；
- restore/generation invalidation 安全清理。

### Phase 3: Read/write/dependency diagnostics

修改：

```text
mvcc/transaction.go
mvcc/write_buffer.go
mvcc/range_guard.go
```

工作：

- Get 增 point read counter；
- Scan/ScanRange 增 range read counter；
- Put/Delete 统计 write count/bytes；
- Guard/GuardRange 统计 dependency；
- 不保存普通 read keys。

### Phase 4: Unified conflict validation

新增建议：

```text
mvcc/conflict.go
```

迁移所有：

```text
latest version > snapshot/readTS
```

逻辑到共享 validator。

接入：

- group commit
- local bounded commit
- local stream
- local WAL
- Raft commitContext

注意：可以共享 validation helper，但仍允许每条路径拥有适合自己的 batch/transaction 生命周期。

### Phase 5: Visibility contract

不强制合并：

- `visible`
- `visibilityReader.visible`
- `flatVisible`

而是增加 contract tests，验证同一 fixture 在所有 reader path 返回完全一致结果。

必要时抽取语义辅助函数，但不得损害 scan cache / flat path。

### Phase 6: Diagnostics capability

修改：

```text
storageengine/engine.go
storageengine/mvccadapter/adapter.go
```

新增 optional interface：

```go
type TransactionDiagnostics interface {
    ActiveTransactions() []TransactionInfo
    TransactionStats() TransactionStats
}
```

为了避免 storageengine 直接 import `mvcc`，公共 DTO 应定义在 `storageengine` 或中性包。

### Phase 7: Executor lifecycle integration

审查：

```text
executor/transaction_engine.go
executor/savepoint_mvcc.go
server disconnect cleanup
```

确保：

- BEGIN root 注册；
- child/savepoint 状态正确；
- child commit -> MERGED；
- rollback -> ABORTED；
- failed statement commit 不遗留 orphan registry；
- disconnect cleanup。

### Phase 8: Sequence overflow hardening

sequence 规则分两处，二者合起来禁止 `uint64` 回绕：

```text
分配点（nextSequence，所有本地提交路径的唯一入口）：
    localSeq == MaxUint64 时拒绝分配（ErrSequenceExhausted）

apply 侧（replicated apply 携带日志 index）：
    sequence = 0 非法（0 从不是 version）
    MaxUint64 是合法的最后 revision，apply 侧不拒绝它
```

因此准确表述是：

- replicated apply 拒绝 `sequence=0`；
- `MaxUint64` 可以作为最后合法 revision（由日志给出），它把 high-water mark 推到
  `MaxUint64`；
- 此后任何本地 sequence allocation（local commit / group commit / streaming commit /
  local WAL）必须 fail closed，返回 `ErrSequenceExhausted`，不得 wrap 到 0。

覆盖测试：`mvcc/resource_boundary_test.go` 的 `TestLastLegalSequenceIsUsable`、
`TestSequenceExhaustionFailsClosedEveryPath`、`TestReplicatedApplyRejectsZeroSequence`、
`TestReplicatedApplyRejectsUsedSequence`、
`TestReplicatedMaxSequenceThenLocalAllocationFailsClosed`（经真实 `Apply` 入口把 high-water
mark 推到 MaxUint64，再验证下一次本地分配 fail closed）。

## Design Decisions

### D1. 不把普通 read-set 变成 conflict set

普通 read 仅统计；Guard/GuardRange 才是 validation dependency。

### D2. child transaction 不产生 durable commitTS

child commit 是 MERGED。

### D3. read-only commit 不推进 Head

保持当前行为与性能。此处的 read-only 指 §7 的 A 类：既无 Put/Delete 也无
Guard/GuardRange 的 empty / ordinary read-only root（`ACTIVE -> COMMITTED`、
`HasCommitTS=false`）。

### D4. root transaction 是 retention owner

savepoint/child 只做 diagnostics registration。

### D5. publication marker 是 commit point

cancel 和错误处理以 publication 是否 durable 为最终边界。

### D6. dependency-only root 仍然是可发布写事务

`Guard`/`GuardRange` 是显式 commit-time validation dependency，不是 ordinary read
observation，因此只有 Guard/GuardRange 的 root 走
`ACTIVE -> COMMITTING -> COMMITTED`，获得 durable commit revision（`HasCommitTS=true`）并
推进 Head，同时不安装任何数据 version。B01 保留该既有实现，不修改代码语义；结论与三类
root 的完整定义见 spec.md FR-004/FR-005/§7、research.md R6b、contracts/transaction-diagnostics.md。
对应测试：`mvcc/transaction_invariants_baseline_test.go` 的
`TestDependencyOnlyCommitPublishesRevision`（由原 characterization test
`TestBaselineGuardOnlyCommitCurrentlyAdvancesHead` 转成的正式 contract test）。

### D7. savepoint 资源上限约束实际存活的 layer 数

`executor/savepoint_mvcc.go` 的 resource budget 约束一个 user transaction 的**实际存活
MVCC savepoint child layer 数**（上限 `maxMVCCSavepoints = 32`），而不是"当前有名字的
savepoint 个数"：`RELEASE SAVEPOINT` 与同名 replacement 只把 layer 的名字清空，layer 本身
（及其 child transaction）继续占用名额。B01 明确不实现匿名 layer compaction，因此采用
bounded layer model：capacity check 必须在任何 state mutation 之前完成，达到 ceiling 后
任何新的 `SAVEPOINT`（新名字或已存在名字）都 fail closed 返回 resource limit，且失败无副作用
（不得先把被替换的同名 savepoint 匿名化）。

对应测试：`executor/mvcc_savepoint_limit_test.go` 的 `TestMVCCSavepointLayerLimit`
（exact boundary、replacement below ceiling、replacement at ceiling fail-closed 且名字仍可用、
repeated same-name 不能增长 layer 链、RELEASE 不能腾出名额、ROLLBACK 释放全部 layer）。

`ROLLBACK TO SAVEPOINT` 截断 layer 链时，executor 必须对**目标及其之上每个被丢弃的 child** 显式
调用 `Tx.Rollback()`（最内层优先）并 `clear` slice 尾部引用：registry 的 descendant 清理只覆盖
diagnostics 状态与 retention，只有 `Tx.cleanup()` 会释放 staging bbolt handle 与
`<store>/transactions/<id>.tmp`。单个 cleanup 失败不得跳过其余 layer（用 `errors.Join` 聚合后
上报）。对应测试：`executor/mvcc_savepoint_resource_test.go` 的四个用例（discarded layer 立即释放、
多层丢弃全部释放、同进程 close+reopen 成功、重复 ROLLBACK TO 不累积 stage）。

截断完成后、尝试创建 fresh target child 之前，`session.transaction` 必须立即恢复为仍可达的
`layer.parent`（fallback invariant）。fresh child 创建可能失败——parent 已关闭，或 generation 被
`RESTORE MVCC FROM`（`executor/maintenance.go` → `mvcc/snapshot.go` 的 generation bump）或副本应用
Raft snapshot（`replication/node.go` → `Store.Restore`）作废；此时若 session 仍指向已关闭的
discarded layer，`index == 0` 会让 root 从可达链中消失，`rollbackSessionTransaction` /
`CloseSession` 都无法再回收 root 的 staging handle 与 `.tmp`。因此失败路径的 contract 是：
`session.savepoints` 只保留 target 以下的 lower layer，`session.transaction` 指向 `layer.parent`
（不得为 nil、不得指向已关闭或被 clear 的 Tx），使后续 `ROLLBACK` / disconnect 仍能完成最终资源
清理。同时 **fresh child 只能在清理之后创建**，否则在 layer 数已达 32 时会瞬时产生第 33 个 live
child，违反 M-5 的 ceiling contract。对应测试：同文件的
`TestMVCCRollbackToSavepointAfterRestoreKeepsRootReclaimable`（双 session + 真实
BACKUP/RESTORE，分别用显式 ROLLBACK 与 `CloseSession` 证明 root 被回收、staging 归零、同进程
reopen 成功）与 `TestMVCCCreateSavepointAfterRestoreKeepsExistingName`（被拒绝的 `SAVEPOINT`
不得丢失已存在的同名 savepoint）。

## Testing Strategy

### Unit

- state machine transition
- manager register/unregister
- TxnID collision defensive behavior
- retention refs
- ReadTS=0
- CommitTS optional
- sequence exhaustion

### MVCC storage tests

- repeatable read
- dirty read prevention
- read-your-writes
- same/different key conflicts
- delete/update conflict
- point guard
- range guard phantom
- tombstone visibility
- child merge
- parent rollback
- restore invalidation

### Commit path matrix

同一 conflict fixture 在：

- local small
- group commit
- local stream
- local WAL
- replicated apply（可用现有 replication test harness）

产生一致结果。

### Executor integration

- BEGIN/COMMIT/ROLLBACK
- SAVEPOINT / ROLLBACK TO / RELEASE
- autocommit
- autocommit=0 implicit transaction
- statement child rollback
- session disconnect

### Concurrency

只使用 channel/barrier/hook，不依赖 sleep。

## Risk Analysis

### Risk 1: Manager 与 existing `closed bool` 双状态源

Mitigation：单一 transition helper；`closed` 逐步变成由 terminal state 派生或严格同步。

### Risk 2: child registry 泄漏

Mitigation：child merge/rollback 都必须 unregister；root cleanup 可做 defensive descendant cleanup/assertion。

### Risk 3: conflict helper 抽取造成 commit path 性能退化

Mitigation：统一规则，不强制统一物理迭代方式；保留 bounded batch。

### Risk 4: diagnostics 锁竞争

Mitigation：manager 使用短临界区；返回 snapshot copy；read counters 可用 atomic 或批量更新。

### Risk 5: GC horizon regression

Mitigation：专门测试 long root + many child/savepoint + CompactHistory。

## Migration / Compatibility

- 不改变 MVCC on-disk data format。
- 不需要 data migration。
- 不改变 public SQL isolation。
- 可保留 `Txn.Snapshot()` 作为 `ReadTS` compatibility alias。
- 可逐步新增 diagnostics optional interface，不破坏第三方 Engine 实现。

## Definition of Done

- spec 中 P1/P2 user stories 独立测试通过；
- `go test ./...` 通过；
- race-capable CI 执行相关 transaction tests；
- existing A05 / savepoint / group commit / local stream / WAL / maintenance tests 不跳过；
- spec/plan/tasks 经 `/speckit.analyze` 无 critical inconsistency；
- implementation 后 `/speckit.converge` 报告无剩余 required tasks。
