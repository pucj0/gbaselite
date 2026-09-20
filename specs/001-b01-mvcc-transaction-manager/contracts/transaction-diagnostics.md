# Contract: Transaction Diagnostics Capability

## Purpose

为上层调试、维护命令或未来 SQL `SHOW MVCC TRANSACTIONS` 提供只读事务诊断能力，不改变最小 `storageengine.Engine` / `Txn` 契约。

## Proposed DTO

```go
package storageengine

type TransactionState string

const (
    TransactionActive     TransactionState = "ACTIVE"
    TransactionCommitting TransactionState = "COMMITTING"
    TransactionCommitted  TransactionState = "COMMITTED"
    TransactionAborted    TransactionState = "ABORTED"
    TransactionMerged     TransactionState = "MERGED"
)

type TransactionInfo struct {
    ID       string
    ParentID string

    StartTS uint64
    ReadTS  uint64

    CommitTS    uint64
    HasCommitTS bool

    State      TransactionState
    Generation uint64

    StartedAt time.Time

    PointReads        uint64
    RangeReads        uint64
    Writes            uint64
    WriteBytes        int64
    PointDependencies uint64
    RangeDependencies uint64

    AbortReason string
}

type TransactionStats struct {
    ActiveRoot     uint64
    ActiveChildren uint64
    Committed      uint64
    Aborted        uint64
    Conflicts      uint64

    OldestReadTS    uint64
    HasOldestReadTS bool
}

type TransactionDiagnostics interface {
    ActiveTransactions() []TransactionInfo
    TransactionStats() TransactionStats
}
```

## Behavioral Contract

### ActiveTransactions

- 返回调用时刻的 snapshot copy。
- 调用者修改返回 slice/struct 不影响 engine。
- 不暴露内部 `*Tx` 或 bbolt handle。
- 只要求活动事务；终态历史不要求无限保存。
- child transaction 可显示 ParentID。
- 顺序不作为 correctness contract；测试如需稳定输出应按 ID 排序。

### TransactionStats

- 必须并发安全。
- `OldestReadTS` 只基于 active root transactions。
- 无 root transaction 时 `HasOldestReadTS=false`。
- 统计值可以是 monotonic counters；不要求跨重启持久化。

## Privacy / Security

- 不暴露 SQL 文本、row values、keys 或敏感 payload。
- AbortReason 应为错误类别/短消息，不包含写集内容。
