# 实施计划：统一写入算子 Modify Pipeline

**分支**：`005-unified-modify-pipeline`
**日期**：2026-09-18
**规格**：`specs/005-unified-modify-pipeline/spec.md`

------

# 摘要

A05 将 GBaseLite 当前分散的 DML mutation 流程重构为：

```text
Parser / Binder
      ↓
Query Pipeline
      ↓
Mutation Candidate
      ↓
TargetRowDedup / Limit / Projection / Staging
      ↓
InsertOperator / UpdateOperator / DeleteOperator
      ↓
storageengine.Txn
```

核心边界：

```text
Query Pipeline：
    决定“修改谁”

Modify Operator：
    决定“怎么修改”
```

A05 是 Executor 架构重构，而不是 SQL 新功能开发。

必须保持 A04 已确认的所有可观察 SQL 语义。

------

# 技术上下文

**语言**：Go

**主要现有组件**：

```text
physical.Operator[T]
physical.Source
physical.Filter
physical.Limit
physical.Modify
physical.Distinct
storageengine.Engine
storageengine.Txn
executor binder/query pipeline
versionedTable
```

**存储**：

```text
storageengine
MVCC implementation behind storageengine.Txn
temporary spill storage
```

**测试框架**：

```text
Go testing
executor MVCC regression tests
legacy-vs-MVCC parity tests
```

------

# 强制技术约束

## 1. Read Txn 与 Write Txn 不得合并

必须继续：

```text
statement parent Txn
      │
      ├──── Query Pipeline
      │
      └──── Child Txn
                │
              Modify
```

即：

```text
查询：parent statement snapshot
写入：statement child transaction
```

禁止：

```text
scan + write 使用同一个 child Txn
```

否则可能导致：

- iterator mutation；
- self INSERT SELECT reread；
- Halloween Problem；
- UPDATE JOIN source 被自己修改污染。

------

## 2. 不直接依赖具体 MVCC Tx

新 Modify Operator 只能依赖：

```go
storageengine.Txn
```

不得：

```go
*mvcc.Tx
```

------

## 3. physical 不依赖 executor

推荐：

```text
physical/
    generic operator
    stable dedup infrastructure
    spill infrastructure

executor/
    RowIdentity
    InsertCandidate
    UpdateCandidate
    DeleteCandidate
    InsertOperator
    UpdateOperator
    DeleteOperator
```

不能形成：

```text
physical → executor
```

依赖。

------

# Phase 0：关键设计决策

## R1 — 现有 statement-level physical.Modify 不代表 A05 已完成

当前：

```go
physical.Modify[parser.Statement, *Result]
```

本质只是：

```text
Statement
→ mutateSQL()
```

并没有统一 row-level mutation。

A05 需要的是：

```text
Operator[InsertCandidate]
→ InsertOperator

Operator[UpdateCandidate]
→ UpdateOperator

Operator[DeleteCandidate]
→ DeleteOperator
```

------

## R2 — Row Identity 是核心一等模型

新增统一目标行身份：

```go
type RowIdentity struct {
    TableID string
    Key     []byte
}
```

建议增加：

```go
Valid bool
```

即：

```go
type RowIdentity struct {
    TableID string
    Key     []byte
    Valid   bool
}
```

含义：

```text
TableID + physical storage key
=
mutation target identity
```

不得以：

```text
row value
row hash
单独 PK value
```

作为统一 identity。

------

## R3 — retained key 必须拥有自己的生命周期

如果 Identity 会离开当前 Yield callback：

```go
Key
```

必须 clone。

禁止直接持有：

```go
scanner iterator
decoder buffer
temporary row buffer
```

返回的 borrowed slice。

------

# 数据模型

## RowIdentity

```go
type RowIdentity struct {
    TableID string
    Key     []byte
    Valid   bool
}
```

规则：

```text
TableID 相同
+
Key 相同
=
同一个 mutation target
```

------

## InsertCandidate

```go
type InsertCandidate struct {
    Values  []any
    Ordinal uint64
}
```

VALUES 和 SELECT 最终都转换为该模型。

------

## UpdateCandidate

```go
type UpdateCandidate struct {
    Identity RowIdentity
    OldRow   storage.Row
    EvalRow  storage.Row
}
```

### OldRow

目标表 statement snapshot 中的旧值。

### EvalRow

执行 SET expression 时使用的 evaluation context。

对于：

```sql
UPDATE t
JOIN s ON ...
SET t.v = s.v;
```

`EvalRow` 必须包含：

```text
t columns
+
s columns
```

------

## DeleteCandidate

```go
type DeleteCandidate struct {
    Identity RowIdentity
    OldRow   storage.Row
}
```

------

## ModifyResult

建议：

```go
type ModifyResult struct {
    AffectedRows uint64

    FirstGeneratedID uint64
    HasGeneratedID   bool
}
```

注意：

该结果首先属于 statement。

只有：

```text
child.Commit()
成功
```

后才能：

```text
session.LastInsertID = ...
```

------

# Operator 设计

## InsertOperator

建议文件：

```text
executor/modify_insert.go
```

概念：

```go
type InsertOperator struct {
    Input physical.Operator[InsertCandidate]

    Target ...
    Write  storageengine.Txn
    ...
}
```

职责：

- target column mapping；
- DEFAULT；
- generated expression；
- auto_increment；
- INSERT mode；
- CHECK；
- FK；
- UNIQUE；
- secondary indexes；
- 最终调用现有 `writeInsertedRow`；
- affected rows；
- statement-local LastInsertID。

不负责：

- SELECT 执行；
- transaction commit；
- session LastInsertID 提前发布。

------

# INSERT Pipeline

## INSERT VALUES

```text
VALUES
  ↓
Value Source
  ↓
InsertCandidate
  ↓
InsertOperator
```

## INSERT SELECT

```text
Query Pipeline
(parent Txn)
    ↓
InsertCandidate
    ↓
InsertOperator
(child Txn)
    ↓
storage
```

应以当前 `insertSelectSQL` 为首个迁移参考。

------

# UpdateOperator

建议：

```text
executor/modify_update.go
```

概念：

```go
type UpdateOperator struct {
    Input physical.Operator[UpdateCandidate]

    Target ...
    Assignments ...
    Write storageengine.Txn
}
```

处理流程：

```text
UpdateCandidate
      ↓
clone OldRow
      ↓
sequential SET evaluation
      ↓
ON UPDATE timestamp
      ↓
FK actions
      ↓
writeVersionedRow
      ↓
AffectedRows
```

------

# UPDATE JOIN 新执行结构

当前：

```text
scan target
    ↓
for each target
    ↓
run join
    ↓
first joined result
    ↓
update
```

A05 目标：

```text
JOIN
 ↓
WHERE
 ↓
project UpdateCandidate
 ↓
TargetRowDedup
 ↓
LIMIT
 ↓
UpdateOperator
```

顺序必须是：

```text
JOIN
→ WHERE
→ Candidate
→ Dedup
→ LIMIT
→ Update
```

不能：

```text
JOIN
→ LIMIT
→ Dedup
```

------

# UPDATE JOIN first-match

假设：

```text
T1 → S1
T1 → S2
T1 → S3
```

TargetRowDedup 输出：

```text
(T1, S1)
```

丢弃：

```text
(T1, S2)
(T1, S3)
```

然后执行 SET。

这保持当前：

```text
first joined match wins
```

行为。

------

# Assignment 顺序

必须维持：

```sql
UPDATE t
SET a = a + 1,
    b = a;
```

行为：

```text
assignment 1
a = old.a + 1

assignment 2
b = new.a
```

因此不能一次性基于 immutable old row 并行计算所有 assignment。

------

# Halloween Protection

例如：

```sql
UPDATE t
SET k = k + 1
WHERE k < 3;
```

扫描必须来自：

```text
parent statement snapshot
```

写入必须进入：

```text
child transaction
```

不能因为更新后的：

```text
k = 2
```

重新进入当前 scan。

------

# DeleteOperator

建议：

```text
executor/modify_delete.go
```

概念：

```go
type DeleteOperator struct {
    Input physical.Operator[DeleteCandidate]

    Target ...
    Write storageengine.Txn
}
```

职责：

```text
candidate validation
↓
FK actions
↓
writeVersionedRow(old → nil)
↓
AffectedRows
```

------

# 普通 DELETE

目标：

```text
planSQLAccess
 ↓
Scan
 ↓
WHERE
 ↓
DeleteCandidate
 ↓
DeleteOperator
```

必须保留：

```text
planSQLAccess
```

避免单表 DELETE 退化成全表扫描。

------

# 多表 DELETE

目标结构：

```text
JOIN
 ↓
WHERE
 ↓
project DeleteCandidates
 ↓
TargetRowDedup
 ↓
multi-target staging
 ↓
FK dependency ordering
 ↓
DeleteOperator
```

### 为什么允许 staging

多表 DELETE 需要：

```text
child tables before parent tables
```

因此可能必须先知道所有待删除 target。

所以 A05 中：

```text
“支持 streaming”
```

不等于：

```text
“每产生一个 candidate 就必须立即写入”
```

正确含义是：

> 不再把 query result 物化到 executor Result.Rows；但因为 SQL 全局语义需要，可以存在有界或 spillable operator barrier。

------

# TargetRowDedup

建议：

```text
physical/target_dedup.go
```

或者将现有：

```text
physical.Distinct
```

抽象出共享：

```text
StableDedup
```

再封装 TargetRowDedup。

概念：

```go
type TargetRowDedup[T any] struct {
    Input physical.Operator[T]

    Identity func(T) (RowIdentity, bool)
}
```

------

# TargetRowDedup 语义

输入：

```text
T1-S1
T2-S1
T1-S2
T3-S5
```

其中：

```text
T1-S1
T1-S2
```

属于同一个物理 target。

输出：

```text
T1-S1
T2-S1
T3-S5
```

必须：

```text
stable
first occurrence wins
```

------

# Dedup Key

逻辑 key：

```text
TableID
+
storage key
```

不能只有：

```text
storage key
```

否则：

```text
table A key = 01
table B key = 01
```

会错误合并。

------

# 资源模型

禁止最终实现长期使用：

```go
seen := make(map[string]bool)
```

保存无限 candidate。

应优先复用现有 `physical.Distinct` 使用的：

```text
bounded sort
+
spill
+
ordinal
```

思路：

### Pass 1

```text
按 Identity + Ordinal 排序
```

每个 identity 保留最低 Ordinal。

### Pass 2

```text
按 Ordinal 排序
```

恢复输入顺序。

这样可以得到：

```text
stable first-match dedup
```

------

# Outer Join

对于：

```sql
DELETE t1
FROM t1
LEFT JOIN t2 ...
```

NULL-extension 时：

```text
t2 target identity = invalid
```

不能通过：

```text
所有 target columns 都是 NULL
```

判断目标不存在。

因为真实 row 本身也可能包含 NULL。

只能依赖：

```text
physical provenance / RowIdentity.Valid
```

------

# 无主键表

必须继续使用：

```text
storage row key
```

作为 identity。

例如：

```text
row A = (1, "x")
row B = (1, "x")
```

虽然逻辑值完全相同：

```text
RowIdentity(A) != RowIdentity(B)
```

------

# Primary Key UPDATE

例如：

```sql
UPDATE t SET id = id + 100;
```

candidate：

```text
Identity = OLD storage identity
OldRow   = OLD row
```

UpdateOperator：

```text
old identity
↓
compute new row
↓
compute new physical key
↓
delete old indexes/row
↓
insert new indexes/row
```

不得提前把 Candidate Identity 改成 new key。

------

# Constraint / Index / FK 策略

A05 第一版不重新实现：

```text
CHECK
UNIQUE
PRIMARY KEY
secondary indexes
FK
CASCADE
SET NULL
```

继续进入已有：

```text
writeInsertedRow
writeVersionedRow
applyForeignKeyActions
```

A05 完成条件是：

```text
所有 DML 都从统一 Operator 到达这些底层写入函数
```

而不是把底层写函数也同时重构掉。

------

# 错误时序

应尽量：

```text
Binding Error
Invalid Assignment
Unknown target
Unsupported writable derived table
```

在：

```text
first mutation
```

之前返回。

但：

```text
duplicate key
CHECK violation
FK violation
data-dependent expression error
```

允许在 pipeline 运行中发生。

此时依赖：

```text
statement child transaction rollback
```

保证原子性。

------

# 推荐文件结构

```text
executor/
├── modify.go
├── modify_insert.go
├── modify_update.go
├── modify_delete.go
├── physical_binding.go
├── mutation.go
├── mutation_insert_select.go
├── mutation_join.go
└── mutation_multi_delete.go

physical/
├── operator.go
├── distinct.go
└── target_dedup.go
```

------

# 迁移步骤

## Phase A — 基础模型

建立：

```text
RowIdentity
InsertCandidate
UpdateCandidate
DeleteCandidate
ModifyResult
TargetRowDedup
```

不迁移 SQL。

目标：

```text
基础抽象 + unit tests
```

------

## Phase B — INSERT

迁移：

```text
INSERT VALUES
INSERT SELECT
```

成为：

```text
Source
→ InsertCandidate
→ InsertOperator
```

完成后删除 INSERT SELECT 中重复的最终写入逻辑。

------

## Phase C — UPDATE

先迁移：

```text
普通 UPDATE
```

再迁移：

```text
UPDATE JOIN
```

最终：

```text
simple update source
       ┐
       ├→ UpdateCandidate → UpdateOperator
       │
join source
       ┘
```

------

## Phase D — DELETE

先迁移：

```text
普通 DELETE
```

再：

```text
multi-table DELETE
```

最终：

```text
DeleteCandidate
→ TargetRowDedup
→ optional staging
→ DeleteOperator
```

------

## Phase E — 清理旧路径

```
mutation_insert_select.go
```

只保留：

```text
binding / pipeline construction
```

不能继续拥有独立的 row write。

```
mutation_join.go
```

只保留：

```text
UPDATE JOIN candidate pipeline construction
mutation_multi_delete.go
```

只保留：

```text
target resolution
candidate projection
dependency ordering
```

最终 row write 必须由 DeleteOperator 完成。

------

# Constitution Check — 设计后复核

GBaseLite 当前仓库没有 Spec Kit constitution，因此按当前项目约束复核：

- SQL compatibility：PASS
- statement atomicity：PASS
- storageengine abstraction：PASS
- parent read / child write：PASS
- bounded resource ownership：设计已覆盖
- no duplicate write path：完成旧路径清理后 PASS
- regression compatibility：实施后验证

最终 Gate：

```text
实施前：PASS
最终完成：需全部 regression tests 通过
```
