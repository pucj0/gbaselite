# Data Model: B01 MVCC Transaction Manager

## 1. TransactionState

```go
type TransactionState uint8

const (
    TransactionActive TransactionState = iota
    TransactionCommitting
    TransactionCommitted
    TransactionAborted
    TransactionMerged
)
```

### 约束

- `Merged` 只用于 child transaction。
- `Committed` 只表示 durable root commit 或成功结束的 read-only root transaction。
- read-only committed transaction 的 CommitTS 为空。
- 终态：Committed / Aborted / Merged。

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
    Writes           uint64
    WriteBytes       int64
    PointDependencies uint64
    RangeDependencies uint64

    AbortReason string
}
```

说明：

- `CommitTS=0` 不能单独表示 unset，因此使用 `HasCommitTS` 或 pointer/optional。
- `StartedAt` 只用于 age/diagnostics，不参与 visibility。

## 3. TransactionManager

```go
type TransactionManager struct {
    mu sync.RWMutex

    activeByID map[string]*transactionEntry

    // root transactions only
    snapshotRefs map[uint64]uint64

    stats TransactionStats
}
```

### transactionEntry

内部 entry 可以持有 mutable counters/state，不直接暴露给调用者。

### snapshotRefs

只统计 root transaction 的 ReadTS，用于 oldest active snapshot / GC horizon。

## 4. TransactionStats

候选：

```go
type TransactionStats struct {
    ActiveRoot     uint64
    ActiveChildren uint64
    Committed      uint64
    Aborted        uint64
    Conflicts      uint64
    OldestReadTS   uint64
}
```

## 5. Tx

现有 `Tx` 保留存储职责，并新增/迁移生命周期元数据：

```go
type Tx struct {
    // existing
    generation uint64
    term       uint64
    store      *Store
    proposer   Proposer
    parent     *Tx

    ID       string
    Snapshot uint64

    // B01
    startTS uint64
    readTS  uint64
    // commitTS only after durable root publish

    manager *TransactionManager

    // existing stage/write set...
}
```

兼容阶段可暂时保留 `Snapshot`，并使：

`Snapshot == ReadTS`

后续是否重命名 storageengine interface 不属于 B01 必须项。

## 6. Dependency Model

### Write Dependency

Put/Delete 自动属于 commit dependency。

### Point Dependency

`Guard(space,key)`。

### Range Dependency

`GuardRange(space,bounds)`。

### Read Observation

Get/Scan 只更新 counters，不保存全部 key。

## 7. Root/Child Relationship

```text
Root ACTIVE
  ├─ Child ACTIVE
  │    └─ child.Commit() -> MERGED
  └─ root.Commit() -> COMMITTING -> COMMITTED
```

如果 root rollback：

- root -> ABORTED
- 其逻辑写全部丢弃；
- 已 MERGED child 的写也随 parent 丢弃。

## 8. Retention Ownership

```text
root Begin(ReadTS=100)
  snapshotRefs[100] += 1

child A
child B
savepoint child C
  不增加 snapshotRefs

root end
  snapshotRefs[100] -= 1
```

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
