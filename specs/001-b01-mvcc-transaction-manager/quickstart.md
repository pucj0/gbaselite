# Quickstart: B01 MVCC Transaction Manager

## 1. 建议分支

```bash
git checkout -b 001-b01-mvcc-transaction-manager
```

## 2. Spec Kit 推荐执行顺序

```text
/speckit.specify
/speckit.clarify
/speckit.plan
/speckit.checklist
/speckit.tasks
/speckit.analyze
/speckit.implement
/speckit.converge
```

本目录已经预生成 specify/clarify/plan/tasks 所需的核心产物。

## 3. 实施前先跑基线

```bash
go test ./mvcc ./storageengine/... ./executor ./server ./replication
go test ./...
```

记录所有已有失败；B01 不应把无关旧失败误判为本功能引入。

## 4. 推荐实施顺序

1. `transaction_state.go`
2. `transaction_manager.go`
3. Begin / child / commit / rollback lifecycle
4. GC retention 接入
5. diagnostics counters
6. conflict validator 收口
7. commit path matrix tests
8. visibility contract tests
9. executor/savepoint lifecycle tests
10. overflow/cancel/restore edge tests

## 5. 实施时固定原则

```text
Snapshot Isolation 不变
StartTS == ReadTS == Begin Head
ordinary reads 不做全量 key-level validation
Guard/GuardRange 才是 dependency
child commit == MERGED
read-only commit 不推进 Head
publication marker == durable commit point
root transaction 才拥有 GC retention
```

## 6. 推荐单测命名

```text
TestTransactionManagerLifecycle
TestTransactionManagerRootRetention
TestTransactionManagerChildDoesNotPinSnapshotTwice
TestTransactionStateTransitions
TestReadTimestampStaysStable
TestReadOnlyCommitDoesNotAdvanceHead
TestChildCommitMarksMergedWithoutCommitTS
TestConflictValidationSameKey
TestConflictValidationDifferentKeys
TestGuardDetectsPointChange
TestGuardRangeDetectsPhantom
TestWriteSkewRemainsAllowedWithoutGuard
TestVisibilityPathsAgree
TestCancelBeforePublicationAborts
TestCancelAfterPublicationReportsCommit
TestSequenceExhaustionFailsClosed
TestRestoreInvalidatesActiveTransactions
TestTransactionDiagnosticsCleanup
```

## 7. 禁止通过 sleep 构造并发

错误：

```go
time.Sleep(100 * time.Millisecond)
```

推荐：

```go
ready := make(chan struct{})
proceed := make(chan struct{})
```

或测试 hook / barrier，把“snapshot 已建立”“publication 前”“publication 后”等状态明确同步。

## 8. 完成后

```bash
go test ./...
go vet ./...
```

若项目 CI 支持 race：

```bash
go test -race ./mvcc ./executor ./server
```

然后运行：

```text
/speckit.analyze
/speckit.converge
```
