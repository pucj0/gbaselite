# Contract: Transaction Diagnostics Capability

## Purpose

为上层调试、维护命令或未来 SQL `SHOW MVCC TRANSACTIONS` 提供只读事务诊断能力，不改变最小 `storageengine.Engine` / `Txn` 契约。

本文件是 `storageengine.TransactionDiagnostics` 的正式契约，字段与语义与
`storageengine/engine.go`、`storageengine/mvccadapter/diagnostics.go` 的实际实现逐项一致。
SQL 层 `SHOW MVCC TRANSACTIONS` 仍属后续增强项，本契约不引入任何 SQL 行为。

## DTO

```go
package storageengine

type TransactionState string

const (
    TransactionActive     TransactionState = "ACTIVE"
    TransactionCommitting TransactionState = "COMMITTING"
    TransactionCommitted  TransactionState = "COMMITTED"
    TransactionAborted    TransactionState = "ABORTED"
    TransactionMerged     TransactionState = "MERGED"

    // UNKNOWN 不是 MVCC lifecycle state：它是 diagnostics 层的 fail-closed 分类。
    TransactionUnknown TransactionState = "UNKNOWN"
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

    PointReads    uint64
    RangeReads    uint64
    RowsObserved  uint64
    BytesObserved uint64

    Writes     uint64
    WriteBytes int64

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

任何后端要满足该契约，都必须提供上述完整字段；contract 不得少于实际 public DTO。

## State vocabulary

| 状态 | 含义 |
|---|---|
| `ACTIVE` | 接受读写、尚未结束。 |
| `COMMITTING` | root 的 durable publication 正在进行；尚未到达 commit point，因此不得当作已提交，也不得读取 commit sequence。 |
| `COMMITTED` | root 成功结束。写事务（Put/Delete）已安装数据 version 并携带 durable publication marker；empty / ordinary read-only root（无 Put/Delete/Guard/GuardRange）`HasCommitTS=false`、不推进 Head；dependency-only root（只有 Guard/GuardRange）`HasCommitTS=true`、推进 Head，但**不安装数据 version**。 |
| `ABORTED` | 已结束且写被丢弃：rollback、冲突、取消或提交失败。 |
| `MERGED` | child 的写已 merge 进 parent；没有自己的 durable commit sequence。 |
| `UNKNOWN` | fail-closed 分类，见下。 |

`UNKNOWN` 不是正常的 MVCC lifecycle transition target。它是 adapter / diagnostics 层的
fail-closed classification：当 backend internal state 未知、未映射、是 future state、或
invalid/unset（例如 `mvcc.TransactionUnset`，即从未注册的事务）时，adapter 必须映射为

```text
-> UNKNOWN
```

而不能映射为

```text
-> ACTIVE
```

这样可以防止 diagnostics 把无法判断的事务错误声明为可用的 ACTIVE transaction。两层状态不是
同一个枚举：internal `mvcc.TransactionState` 是 `uint8`（含 0 值 `TransactionUnset`），public
`storageengine.TransactionState` 是字符串词表（含 `UNKNOWN`），映射由 adapter 负责。

## Behavioral Contract

### ActiveTransactions

- 返回调用时刻的 snapshot copy；slice 归调用者所有，调用者修改返回的 slice/struct 不影响
  engine（也不影响后续调用）。
- 无事务时返回非 nil 的空切片（`[]TransactionInfo{}`），不是 `nil`。
- 不暴露内部 `*Tx` 或 bbolt handle。
- 语义是"**当前仍注册在 TransactionManager registry 中的事务**"，不是严格的
  `State == ACTIVE`。因此返回值可能短暂包含已进入终态（`COMMITTED`/`ABORTED`/`MERGED`）但
  尚未 unregister 的条目。终态历史不要求无限保存。
- child transaction 可显示 ParentID。
- 顺序不作为 correctness contract；测试如需稳定输出应按 ID 排序。

### TransactionStats

- 必须并发安全；可以在事务提交/回滚的同时调用。
- `ActiveRoot`/`ActiveChildren` 只统计 **non-terminal** 的已注册事务，因此允许

  ```text
  len(ActiveTransactions()) > ActiveRoot + ActiveChildren
  ```

  出现在终态 transition 与 unregister 之间的极短窗口。这是 diagnostics snapshot semantics，
  不是 lifecycle bug。
- `OldestReadTS` 只基于 active root transactions（child/savepoint 不 pin retention）。处于
  `COMMITTING` 的 root 仍然 pin；进入终态的同一步释放 pin。
- 无 root transaction 时 `HasOldestReadTS=false`。`ReadTS=0` 合法，所以必须用该布尔表示存在性，
  0 不能作为 unset sentinel。
- `Committed`/`Aborted`/`Conflicts` 是 monotonic counters；不要求跨重启持久化。`Conflicts`
  统计真实的冲突中止（该中止同时计入 `Aborted`，但 `Conflicts` 只对应冲突原因）。

## Field semantics

### Timestamps

- `StartTS == ReadTS`（root 为 Begin 时观察到的 committed Head；child 继承 parent 的值），
  两者不单独存储。
- `HasCommitTS` 表示该 root 是否获得 durable commit revision。`CommitTS=0` 不能表示 unset。
- `HasCommitTS=true` **不保证该事务安装了数据 version**：dependency-only root 会得到
  `HasCommitTS=true`、Head 前进，但没有数据 version。判定"有数据变更"必须看
  `Writes`/`WriteBytes`。

### Observation and dependency counters

| 字段 | 语义 |
|---|---|
| `PointReads` | ordinary point read（`Get`）的调用计数。 |
| `RangeReads` | ordinary range read（`Scan`/`ScanRange`）的调用计数。 |
| `RowsObserved` | 读取到的行数累计（bounded numeric counter，只在非零时累加）。 |
| `BytesObserved` | 读取到的字节数累计（bounded numeric counter，只在非零时累加）。 |
| `Writes` | **当前 logical write set** 中的 Put/Delete key 数，不是 API 调用历史次数。例如 `Put A=1; Put A=2` 最终仍是一个 logical write key。 |
| `WriteBytes` | 当前 staged logical write-set accounting（含 spill 到 stage 的字节）；guard 是 dependency 而不是 write，因此贡献 0，`WriteBytes=0` 也不代表事务没有可发布工作。 |
| `PointDependencies` | 只对应显式 `Guard`。 |
| `RangeDependencies` | 只对应显式 `GuardRange`。 |

ordinary Get/Scan **不进入** dependency set，也不会让事务成为可发布写事务；只有
`Guard`/`GuardRange` 参与 commit-time validation。

`RowsObserved`/`BytesObserved`（以及所有其它计数器）都是 bounded numeric counters：契约禁止
保存 read keys、row payload、SQL 文本或 key list，也不允许 diagnostics state 随读取键数线性增长。

## Privacy / Security

- 不暴露 SQL 文本、row values、keys 或敏感 payload。
- `AbortReason` 为错误类别/短消息（实现取值：`conflict`、`sequence exhausted`、
  `write set limit`、`canceled`、`closed`、`commit failed`，以及 lifecycle 自己的
  `generation change`、`merge failed`、`commit incomplete`），不包含写集内容。
