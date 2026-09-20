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

在所有 version allocation 路径统一检查：

```text
localSeq == MaxUint64
```

禁止 `++` 回绕。

必须覆盖：

- local commit
- group commit
- streaming commit
- replicated apply / allocated high-water mark

## Design Decisions

### D1. 不把普通 read-set 变成 conflict set

普通 read 仅统计；Guard/GuardRange 才是 validation dependency。

### D2. child transaction 不产生 durable commitTS

child commit 是 MERGED。

### D3. read-only commit 不推进 Head

保持当前行为与性能。

### D4. root transaction 是 retention owner

savepoint/child 只做 diagnostics registration。

### D5. publication marker 是 commit point

cancel 和错误处理以 publication 是否 durable 为最终边界。

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
