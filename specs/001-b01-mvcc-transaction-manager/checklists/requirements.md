# Requirements Quality Checklist: B01 MVCC Transaction Manager

**Purpose**: 在实施前检查需求是否完整、一致、可验收。  
**Note**: 勾选表示“需求质量已审查通过”，不是“代码已经完成”。

## Scope

- [x] CHK001 明确说明 B01 不重新实现 MVCC 版本存储。
- [x] CHK002 明确 Snapshot Isolation 保持不变。
- [x] CHK003 明确不引入全量普通 read-set commit validation。
- [x] CHK004 明确不引入 blocking locks / Serializable。
- [x] CHK005 明确 A05 parent-read/child-write 模型保持兼容。

## Timestamp Semantics

- [x] CHK006 StartTS 定义明确。
- [x] CHK007 ReadTS 定义明确并说明事务内固定。
- [x] CHK008 CommitTS 只在 durable write commit 后设置。
- [x] CHK009 read-only commit 不推进 Head。
- [x] CHK010 child merge 不产生独立 CommitTS。
- [x] CHK011 Head=0 / ReadTS=0 合法。
- [x] CHK012 明确禁止 version sequence wraparound。

## Lifecycle

- [x] CHK013 ACTIVE/COMMITTING/COMMITTED/ABORTED/MERGED 状态完整。
- [x] CHK014 明确 double rollback 语义。
- [x] CHK015 明确 double commit 语义。
- [x] CHK016 failed commit 的终态和 CommitTS 语义明确。
- [x] CHK017 disconnect cleanup 有验收要求。
- [x] CHK018 generation invalidation 有验收要求。

## Visibility

- [x] CHK019 read-your-writes 明确。
- [x] CHK020 child-own-write visibility 明确。
- [x] CHK021 parent 不见 unmerged child write 明确。
- [x] CHK022 no dirty read 明确。
- [x] CHK023 tombstone visibility 明确。
- [x] CHK024 unpublished/crash version 不可见明确。
- [x] CHK025 point/scan/flat path 语义一致要求明确。

## Conflict Model

- [x] CHK026 same-key write conflict 明确。
- [x] CHK027 different-key concurrent commit 明确。
- [x] CHK028 Guard point dependency 明确。
- [x] CHK029 GuardRange phantom dependency 明确。
- [x] CHK030 write skew without guard 允许，符合 SI。
- [x] CHK031 所有 commit paths 必须一致。

## Active Registry / GC

- [x] CHK032 root 注册/注销明确。
- [x] CHK033 child diagnostics registration 与 retention ownership 分离。
- [x] CHK034 child 不重复 pin snapshot。
- [x] CHK035 GC horizon invariant 明确。
- [x] CHK036 registry leak/negative ref 有测试要求。

## Resource Boundaries

- [x] CHK037 exact write-set limit 可成功。
- [x] CHK038 limit+1 失败。
- [x] CHK039 limit=0 保持 unlimited-total/bounded-memory 语义。
- [x] CHK040 accounting overflow fail closed。
- [x] CHK041 普通大 SELECT 不产生无界 read-key collection。

## Concurrency & Durability

- [x] CHK042 cancel-before-publication 行为明确。
- [x] CHK043 cancel-after-publication 行为明确。
- [x] CHK044 publication marker 被定义为 commit point。
- [x] CHK045 并发测试禁止依赖 sleep。
- [x] CHK046 crash/recovery visibility 有验收要求。

## Diagnostics

- [x] CHK047 ActiveTransactions 字段完整。
- [x] CHK048 OldestReadTS 只看 root。
- [x] CHK049 diagnostics 不暴露 row values/keys。
- [x] CHK050 optional capability 不强制扩张最小 Engine/Txn 接口。
