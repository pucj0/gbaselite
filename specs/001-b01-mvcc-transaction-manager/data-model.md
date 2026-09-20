# Data Model: B01 MVCC Transaction Manager

## 1. TransactionState

internal 状态（`mvcc.TransactionState`，`mvcc/transaction_state.go`）：

```go
type TransactionState uint8

const (
    TransactionUnset TransactionState = iota // 0：未注册/无效，不是合法生命周期状态
    TransactionActive
    TransactionCommitting
    TransactionCommitted
    TransactionAborted
    TransactionMerged
)
```

`TransactionUnset` 是 0 值：一个从未经过 TransactionManager 注册的事务只能处于它，绝不
把它当作 `ACTIVE`。public diagnostics 层不使用这个枚举，而是使用
`storageengine.TransactionState`（字符串词表，含 fail-closed 的 `UNKNOWN`），两层映射见
contracts/transaction-diagnostics.md。

### 约束

- `Merged` 只用于 child transaction。
- `Committed` 只表示 durable root commit 或成功结束的 empty / read-only root transaction。
- read-only（A 类：无 Put/Delete/Guard/GuardRange）committed transaction 的 CommitTS 为空。
- dependency-only root（C 类：只有 Guard/GuardRange）committed transaction 的 CommitTS
  非空（`HasCommitTS=true`）、Head 前进，但没有数据 version 被安装：见 spec.md FR-004、§7。
- 终态：Committed / Aborted / Merged；终态只允许自我确认，不得回到 ACTIVE。
- 合法转换（状态机层）：root `ACTIVE -> COMMITTING`、`ACTIVE -> COMMITTED`、
  `ACTIVE -> ABORTED`、`COMMITTING -> COMMITTED`、`COMMITTING -> ABORTED`；child
  `ACTIVE -> MERGED`、`ACTIVE -> ABORTED`。
  状态表本身对**任意 root** 允许 `ACTIVE -> COMMITTED`（`transactionTransitionKind` 只校验
  root-only，不看写集）。"仅 A 类"是**生产调用约定**而非状态机约束：`commitRoot()` 只有在
  `!hasWriteSet()` 时才走这条直接转换，B/C 类必须经过 `COMMITTING` 以获得 durable commit
  revision。若要由状态机自身保证该约束，需另做设计变更。

## 2. TransactionInfo

```go
type TransactionInfo struct {
    ID         string
    ParentID   string

    StartTS    uint64
    ReadTS     uint64
    CommitTS   uint64
    HasCommitTS bool

    State      TransactionState
    Generation uint64

    StartedAt  time.Time

    PointReads       uint64
    RangeReads       uint64
    RowsObserved     uint64
    BytesObserved    uint64

    Writes           uint64
    WriteBytes       int64

    PointDependencies uint64
    RangeDependencies uint64

    AbortReason string
}
```

说明：

- `CommitTS=0` 不能单独表示 unset，因此使用 `HasCommitTS` 或 pointer/optional。
- `HasCommitTS=true` 只表示该 root transaction 有 durable commit revision，不表示它安装了
  数据 version（dependency-only root 即如此，见 §1 约束与 spec.md §7）。
- `StartTS` 不是独立存储的字段：`StartTS == ReadTS`，由 manager 从 ReadTS 派生
  （`mvcc/transaction_manager.go`）。child 继承 parent 的 ReadTS，因此 StartTS/ReadTS 与
  parent 相同。
- `RowsObserved`/`BytesObserved` 只在非零时累加；两者都是 bounded numeric counter，绝不保留
  read keys、row payload 或 SQL 文本。
- `Writes`/`WriteBytes` 描述**数据写集**（Put/Delete）：guard 是 dependency 而不是 write，
  因此不计入这两个计数器（`WriteBytes` 为 0 也不代表事务没有可发布工作）。
- `PointDependencies`/`RangeDependencies` 只对应显式 `Guard`/`GuardRange`；ordinary Get/Scan
  只影响 PointReads/RangeReads/RowsObserved/BytesObserved。
- `StartedAt` 只用于 age/diagnostics，不参与 visibility。

## 3. TransactionManager

```go
type TransactionManager struct {
    mu sync.RWMutex

    activeByID map[string]*transactionEntry

    // root transactions only
    snapshotRefs map[uint64]uint64

    // 终态 outcome 的 process-lifetime 计数器
    committed uint64
    aborted   uint64
    conflicts uint64
}
```

### transactionEntry

内部 entry 可以持有 mutable counters/state，不直接暴露给调用者。它保存 `id`、`parentID`、
`root`、`startTS`、`readTS`、`commitTS`、`hasCommitTS`、`state`、`generation`、`startedAt`、
`counters`、`abortReason`、`pinned`。

`TransactionStats` 不是存储字段：它由 `Stats()` 在读取时按 registry 与上面三个计数器聚合。

### snapshotRefs

只统计 root transaction 的 ReadTS，用于 oldest active snapshot / GC horizon。

retention 的释放时机：root 进入**终态**时（`Transition` 内部，commit/rollback/conflict
决定的同一步）立即释放一次 pin，`Unregister` 只负责清理仍存活的引用，因此释放恰好一次。
处于 `COMMITTING` 的 root 仍然 pin（它还要完成自己的校验与发布）。终态但尚未 unregister 的
极短窗口内不再 pin，因此 horizon 可以先行推进。

## 4. TransactionStats

```go
type TransactionStats struct {
    ActiveRoot     uint64
    ActiveChildren uint64
    Committed      uint64
    Aborted        uint64
    Conflicts      uint64
    OldestReadTS   uint64
    HasOldestReadTS bool
}
```

- `ActiveRoot`/`ActiveChildren` 只统计 **non-terminal** 的已注册事务。
- `OldestReadTS` 只看 root transaction 的 ReadTS；`ReadTS=0` 合法，所以必须用
  `HasOldestReadTS` 表示存在性，0 不能作为 unset sentinel。
- `Committed`/`Aborted`/`Conflicts` 是 process-lifetime、单调、不跨重启持久化的计数器。
- `ActiveTransactions()` 返回的是"当前仍注册在 registry 中的事务"，可能短暂包含已终态但
  尚未 unregister 的条目，因此允许 `len(ActiveTransactions()) > ActiveRoot+ActiveChildren`
  出现在终态转换与 unregister 之间的极短窗口（diagnostics snapshot 语义，不是 lifecycle bug）。

## 5. Tx

实现中 `Tx` 保留存储职责与生命周期元数据；manager 引用挂在 `Store` 上（`store.txns`），
`Tx` 不持有 manager 指针：

```go
type Tx struct {
    generation  uint64
    term        uint64
    store       *Store        // store.txns 是 TransactionManager
    proposer    Proposer
    parent      *Tx

    ID       string
    Snapshot uint64            // == ReadTS（兼容别名，无独立 startTS/readTS 字段）

    // 由 t.mu 保护的诊断/生命周期字段
    mu          sync.Mutex
    startedAt   time.Time
    state       TransactionState
    commitTS    uint64
    hasCommitTS bool
    abortReason string

    // 与 registry entry 共享，供 diagnostics 免锁读取
    counters *transactionCounters

    // existing stage / write set ...
}
```

`StartTS == ReadTS == Snapshot`：两个时间戳不单独存储，由 manager/Info() 派生。后续是否重命名
storageengine interface 不属于 B01 必须项。

## 6. Dependency Model

### Write Dependency

Put/Delete 自动属于 commit dependency，并且是数据 version 的来源。

### Point Dependency

`Guard(space,key)`。是 commit dependency，但不是数据写：不安装 version，也不计入
`Writes`/`WriteBytes`。

### Range Dependency

`GuardRange(space,bounds)`。

### Read Observation

Get/Scan 只更新 counters，不保存全部 key。

## 7. Root/Child Relationship

```text
Root ACTIVE
  ├─ A 类 empty / ordinary read-only（无 Put/Delete/Guard/GuardRange）
  │    └─ root.Commit() -> COMMITTED          HasCommitTS=false，Head 不变
  ├─ B 类 data-writing（有 Put/Delete）
  │    └─ root.Commit() -> COMMITTING -> COMMITTED
  │         HasCommitTS=true，Head 前进，安装数据 version
  ├─ C 类 dependency-only（只有 Guard/GuardRange）
  │    └─ root.Commit() -> COMMITTING -> COMMITTED
  │         HasCommitTS=true，Head 前进，不安装数据 version
  ├─ 任意 root 失败/取消/冲突 -> ABORTED（COMMITTING -> ABORTED 或 ACTIVE -> ABORTED）
  └─ Child ACTIVE
       ├─ child.Commit() -> MERGED（无独立 commitTS）
       └─ child 失败/回滚 -> ABORTED
```

三类 root 的完整语义见 `spec.md` FR-004、FR-005 与 §7 "事务结束"。

如果 root rollback：

- root -> ABORTED
- 其逻辑写全部丢弃；
- 已 MERGED child 的写也随 parent 丢弃。

### Savepoint layer chain

`executor/savepoint_mvcc.go` 的 savepoint 是 root 之上的一串嵌套 child layer：

```text
root (session.transaction)
  └─ layer1 (SAVEPOINT a)      ── 每个 layer 持有一个 storageengine.Txn child
       └─ layer2 (SAVEPOINT b)
            └─ layer3 (当前 statement 写入的 layer)
```

- `RELEASE SAVEPOINT` 只把 layer 的名字清空，layer 及其 child transaction **仍然存活**，
  成为匿名 boundary；同名 replacement 同样只让老 layer 匿名化。
- 资源上限是 **maximum live MVCC savepoint layers per user transaction**（`maxMVCCSavepoints`
  = 32），统计的是存活 layer 数（含匿名 boundary），不是 named savepoint 数；B01 不做匿名
  layer compaction。达到 ceiling 后即使 named 数为 0 也不能创建新 `SAVEPOINT`。
- `ROLLBACK TO SAVEPOINT` 丢弃目标 layer 及其之上的 layer（最内层优先），并以同一 parent 重新
  开一个同名 layer。被丢弃的每个 child 必须由 executor 显式 `Tx.Rollback()`：registry 的
  descendant 清理只移除 diagnostics 条目与 retention 引用，而 staging bbolt handle 与
  `<store>/transactions/<id>.tmp` 只在 `Tx.cleanup()` 中释放，两者不可互相替代；截断后还需
  清除 slice 尾部对这些 child 的引用，避免它们仍被底层数组持有。
  截断后、创建 fresh child 前，`session.transaction` 必须立即回到仍可达的 `layer.parent`：即使
  fresh child 创建失败（parent 关闭或 generation 被 `RESTORE MVCC FROM` / Raft snapshot 作废），
  session 也必须保留对 surviving parent/root 的引用，使后续 `ROLLBACK` / disconnect /
  `CloseSession` 仍能回收整个 remaining user transaction（含 root 的 staging 文件）。fresh child
  只能在被丢弃 layer 清理之后创建，否则在 ceiling 处会瞬时出现第 33 个 live child。
- `SAVEPOINT` 先创建 child 再改动名字：child 创建失败时不得把已存在的同名 savepoint 匿名化。
- `COMMIT` 自内向外把每个 layer merge 进它的 parent，最终只留下 root 的 durable commit；
  `ROLLBACK` 丢弃整条链。

## 8. Retention Ownership

```text
root Begin(ReadTS=100)
  snapshotRefs[100] += 1

child A
child B
savepoint child C
  不增加 snapshotRefs

root 进入终态（commit / rollback / conflict）
  snapshotRefs[100] -= 1        // 恰好一次；COMMITTING 期间仍然 pin
```

释放发生在终态转换内，而不是 unregister 时；`Unregister` 只清理仍存活的引用。

## 9. Version Visibility

对于逻辑 key K 和 transaction T：

1. T/ancestor write overlay 优先。
2. committed storage version：
   - version <= T.ReadTS
   - commit marker exists
3. tombstone => not exists。
4. unpublished version => skip。

## 10. Commit Validation

对于每个 write/dependency：

```text
latestCommittedChange > ReadTS
    => ErrConflict
```

Range dependency：

```text
max committed change in guarded range > ReadTS
    => ErrConflict
```

校验对象是**已提交数据版本**。dependency-only root 的 publication 不安装 version，因此它本身
不会使另一个事务的 guard 失败；反过来，dependency-only root 自己的 guard 仍按上面的规则校验
（其 guarded key 在 ReadTS 之后被写过就冲突）。
