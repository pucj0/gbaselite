# Research: B01 MVCC Transaction Manager

## R1. 当前能力基线

当前 GBaseLite MVCC 已具备：

- `Tx.ID`
- `Tx.Snapshot`
- bounded write buffer + temporary bbolt stage
- optimistic write conflict validation
- `Guard` / `GuardRange`
- parent/child transaction
- savepoint layered child transaction
- group commit
- standalone streaming commit
- local WAL
- Raft staged commit
- publication marker
- active snapshot ref map
- history compaction based on oldest active snapshot

结论：B01 应做“语义显式化和统一管理”，而非重写版本存储。

## R2. startTS/readTS/commitTS 语义

### Decision

- `StartTS = Begin 时 Head`
- `ReadTS = StartTS`
- `CommitTS = durable publish 时的 commit/version sequence`
- child merge 不分配 CommitTS
- read-only commit 不分配 CommitTS

### Rationale

当前系统的 Snapshot 已经承担固定 read timestamp 的角色；commit sequence 已经承担 durable version timestamp 的角色。B01 应命名并记录这些语义，而不是引入第二套时间轴。

### Rejected

- BEGIN 时递增 global sequence：会导致只读事务改变版本时钟，并给复制路径增加不必要日志。
- wall clock 作为 TS：无法安全表达全序与恢复语义。

## R3. 隔离级别

### Decision

继续 Snapshot Isolation。

### Rationale

现有 `storageengine.Txn` 合同已定义 snapshot isolation。写冲突检查基于 `latest version > snapshot`。普通读不会自动转为 serializable dependency。

### Rejected

“读取过的所有 key 在 commit 时全部验证”。原因：

- 会改变隔离语义；
- 大 SELECT read-set 无界；
- 内存成本与结果规模线性增长；
- write skew 将被非预期禁止。

## R4. Read Set 模型

### Decision

拆成两类：

1. **Read Observation**：普通 Get/Scan，仅 counters/summary。
2. **Dependency Set**：Guard/GuardRange，参与 commit conflict。

### Rationale

既满足诊断需求，又不破坏 SI。

## R5. Transaction Manager 数据结构

### Decision

至少维护：

- `activeByID: TxnID -> TxnInfo`
- root snapshot retention refs：`ReadTS -> root count`

child transaction 可以在 `activeByID` 中出现，但不重复占用 root retention。

### Rationale

当前 `map[snapshot]count` 适合 GC，但无法诊断事务；只改成 `map[id]*Tx` 又会丢失高效 horizon 统计。

## R6. Transaction State Machine

### Decision

状态：

- ACTIVE
- COMMITTING
- COMMITTED
- ABORTED
- MERGED

失败原因单独放 AbortReason，不制造大量状态枚举。

## R7. Unified Visibility

### Decision

统一语义，不强制统一实现函数。

保留 point/scan/flat fast path，增加共享 contract tests / helper assertions。

### Rationale

visibility reader 当前包含缓存、flat layout 和 scan 优化；把所有路径机械合并可能引入性能回退。

## R8. Unified Conflict Validation

### Decision

提取逻辑层 validator，使所有 commit path 使用相同规则。

建议逻辑 API：

```go
type CommitValidationInput struct {
    ReadTS uint64
    Writes DependencyIterator
}
```

或内部 helper：

```go
func validateCommitDependencies(
    tx *bolt.Tx,
    readTS uint64,
    deps func(func(encodedKey []byte) error) error,
) error
```

具体签名允许实现阶段调整，但不可改变语义。

## R9. Durable publication 与 cancel

### Decision

publication marker 是 commit point。

- commit point 前 cancel：可 abort。
- publication marker 已 durable：必须报告已 committed 的最终结果，不能向上层声称 rollback。

现有 group commit 对该原则已有注释与等待机制，B01 必须把它变成跨路径不变量测试。

## R10. Sequence Exhaustion

### Decision

在分配下一 commit sequence 前检查 MaxUint64，禁止 overflow。

## R11. Diagnostics API

### Decision

作为 `storageengine` optional capability 添加，不扩张最小 `Txn` interface。

候选：

```go
type TransactionDiagnostics interface {
    ActiveTransactions() []TransactionInfo
    TransactionStats() TransactionStats
}
```

SQL SHOW 命令不是 B01 必须项。
