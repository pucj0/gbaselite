# Quickstart：A05 统一写入算子 Modify Pipeline

**功能**：005-unified-modify-pipeline
**规格**：`spec/005-unified-modify-pipeline/spec.md` ｜ **计划**：`plan.md` ｜ **任务**：`tasks.md`
**说明**：仓库实际目录是 `spec/`（单数），不存在 `specs/`。

本文件是 A05 的端到端验证入口：命令、关键 SQL 场景、预期结果、以及对应的回归测试。

------

## 1. 验证命令

```powershell
# 环境（沙箱下 Go build cache 需指到仓库内）
$env:GOCACHE='E:\code\gbaselite\.tmp\gocache'
$env:GOPATH='E:\code\gbaselite\.tmp\gopath'

# 格式 / 构建 / 静态检查
gofmt -l executor/ physical/
go build ./...
go vet ./...

# 全量回归
go test ./... -count=1
```

定向回归（按 A05 的不变量分组）：

```powershell
# INSERT（VALUES / SET / SELECT 统一）
go test ./executor/ -count=1 -run 'TestMVCCInsert' -v

# UPDATE（普通 + JOIN）
go test ./executor/ -count=1 -run 'TestMVCCUpdate' -v

# DELETE（普通 + 多表 + 无主键）
go test ./executor/ -count=1 -run 'TestMVCCDelete|TestMVCCMultiTableDelete' -v

# TargetRowDedup（稳定首次命中 / 跨表不去重 / spill / 资源释放）
go test ./physical/ -count=1 -run 'TestTargetRowDedup|TestDistinct' -v

# 跨 DML 事务、快照、回滚、资源不变量
go test ./executor/ -count=1 -run 'TestMVCCModifyPipelines|TestMVCCStatementSnapshot|TestMVCCIteratorContract|TestMVCCMutatingScanned|TestMVCCDownstreamError|TestMVCCStatementFailure|TestMVCCLastInsertID|TestMVCCCancellation|TestMVCCSpillableDedup' -v

# parity（legacy vs MVCC）
go test ./executor/ -count=1 -run 'MatchesLegacyEngine' -v

# 架构边界（physical 不依赖 executor、无回调往返、无具体后端 import）
go test ./executor/ -count=1 -run 'TestProductionArchitectureBoundaries' -v
```

**通过标准**：`gofmt -l` 无输出；`go build`/`go vet` 退出码 0；`go test ./...` 全部 `ok`，无 `FAIL`。

------

## 2. 关键 SQL 场景与预期结果

以下场景均可用 `gbaselite` CLI 或 `go test` 中的 `rangeTestEngine` 会话直接复现。除特别说明外，
预期结果在 legacy 与 MVCC 两个引擎上一致（由 parity 回归锁定）。

### 2.1 INSERT：VALUES 与 SELECT 共用同一写入实现

```sql
CREATE TABLE src(id INT PRIMARY KEY, v INT);
CREATE TABLE dst(id INT PRIMARY KEY, v INT);
INSERT INTO src VALUES(1,10),(2,20);
INSERT INTO dst VALUES(1,10),(2,20),(3,30);   -- affected 3
INSERT INTO dst SELECT id+10, v FROM src;      -- affected 2
```

- 两条语句都经 `InsertOperator`；全仓库 `writeInsertedRow` 只有 1 个调用点。
- 断言证据：`TestMVCCInsertValuesAndSelectShareOneOperator`（用 `insertApplyHook` 观察每行都经过该算子）。

```sql
-- self-reference：源只能看到语句开始时的快照
INSERT INTO dst SELECT id+100, v FROM dst;
```

- 预期：只复制语句开始前已存在的行，绝不读到本语句刚写入的行。
- 证据：`TestMVCCInsertSelfSourceSnapshotThroughOperator`、`TestMVCCInsertSelectSelfSourceSnapshot`。

```sql
-- auto_increment 跨路径连续 + 失败留 gap
CREATE TABLE a(id INT AUTO_INCREMENT PRIMARY KEY, v INT);
INSERT INTO a(v) VALUES(10),(20);   -- LastInsertID=1, affected 2
INSERT INTO a(v) SELECT v FROM src; -- LastInsertID=3, affected 2
INSERT INTO a(id,v) VALUES(100,10); -- LastInsertID=0（显式 id 不发布）
INSERT INTO a(v) VALUES(55);        -- LastInsertID=101
```

- 预期：生成 id 连续；显式 id 抬升 floor；失败后 reservation 不复用（允许 gap）。
- 证据：`TestMVCCInsertAutoIncrementAcrossValuesAndSelect`、`TestMVCCInsertAutoIncrementGapAfterRollbackIsPreserved`、`TestMVCCInsertLastInsertIDRequiresStatementCommit`。

### 2.2 UPDATE：顺序赋值、索引列、主键、Halloween

```sql
CREATE TABLE t(id INT PRIMARY KEY, a INT, b INT);
INSERT INTO t VALUES(1,10,0);
UPDATE t SET a=a+1, b=a;   -- 预期 a=11, b=11（b 读到新的 a）
```

- 证据：`TestMVCCUpdateSequentialAssignmentOrder`。

```sql
CREATE TABLE h(id INT PRIMARY KEY, k INT, v INT, KEY k_idx(k));
INSERT INTO h VALUES(1,1,10),(2,2,20),(3,99,30);
UPDATE h SET k=k+1 WHERE k<3;   -- affected 2，不是 3
```

- 预期：扫描固定在语句父快照，写入落在 child，因此更新后的 `k=2` 不会重新进入本次扫描。
- 证据：`TestMVCCUpdateIndexHalloweenProtection`、`TestMVCCMutatingScannedTableStaysAtomic`。

```sql
UPDATE h SET id=id+100;   -- 每个物理行移动一次，旧主键与旧唯一索引项被释放
```

- 证据：`TestMVCCUpdatePrimaryKeyKeepsOldPhysicalIdentity`。

```sql
-- 访问计划不得退化：UPDATE/DELETE 不支持 EXPLAIN，直接断言 mutationScan 的 PlanNode
CREATE TABLE t(id INT PRIMARY KEY, k INT, v INT, KEY k_idx(k));
UPDATE t SET v=v+1 WHERE k=2;    -- 仍走 k_idx 索引访问，未退化为全表扫描
DELETE FROM t WHERE k=2;         -- 同上
```

- 预期：索引等值条件仍选中 `k_idx`（`access != ALL`），不可服务的条件回退普通 Scan。
- 证据：`TestMVCCUpdateKeepsAccessPlan`、`TestMVCCDeleteKeepsAccessPlan`（两者直接检查 `mutationScan`
  返回的 `Limit → Filter → Scan` 链与 `PlanNode.Attributes`）。

### 2.3 UPDATE JOIN：多匹配只改一次、first match、LIMIT 在 dedup 之后

```sql
CREATE TABLE t(id INT PRIMARY KEY, v INT);
CREATE TABLE s(id INT PRIMARY KEY, tid INT, delta INT, KEY tid_idx(tid));
INSERT INTO t VALUES(1,10),(2,20);
INSERT INTO s VALUES(11,1,3),(12,1,7),(13,1,4),(14,2,5);
UPDATE t JOIN s ON s.tid=t.id SET t.v=t.v+s.delta;   -- affected 2
```

- 预期：`t` 为 `(1,13)`、`(2,25)` —— 每个 target 只改一次，取**首个**匹配 source（delta 3，不是 7/4）。
- 证据：`TestMVCCUpdateJoinFirstMatchWinsPerTarget`、`TestMVCCUpdateJoinDeduplicatesRepeatedTargetMatches`。

```sql
-- LIMIT 统计去重后的唯一 target
UPDATE t JOIN s ON s.tid=t.id SET t.v=0 LIMIT 2;   -- 命中 t.id=1 与 t.id=2
```

- 预期：affected = 2（若 LIMIT 位于 dedup 之前，扫过 2 条 join row 时只会碰到 target 1）。
- 证据：`TestMVCCUpdateJoinLimitCountsDedupedTargets`。

```sql
-- SET 表达式可读 joined source，且顺序赋值
UPDATE t JOIN s ON s.tid=t.id SET t.v=t.v+s.delta, t.note=CONCAT(s.label,'-',t.v);
```

- 证据：`TestMVCCUpdateJoinSetSeesJoinedSourceAndSequentialAssignments`。

### 2.4 DELETE：普通与多表

```sql
DELETE FROM t WHERE v>=20;   -- affected = 匹配行数
DELETE FROM t WHERE g=1 LIMIT 1;   -- LIMIT 计匹配行，WHERE 在 LIMIT 之前
```

- 证据：`TestMVCCDeleteUsesUnifiedDeleteOperator`、`TestMVCCDeleteWhereAndLimitSemantics`。

```sql
-- 多表 DELETE：重复 join 组合只删一次；无主键表按物理行区分
CREATE TABLE a(v INT);
CREATE TABLE b(v INT);
INSERT INTO a VALUES(1),(2),(2),(3);
INSERT INTO b VALUES(2),(3);
DELETE a FROM a JOIN b ON a.v=b.v;   -- a 中两行 v=2 都被删除（是两个不同物理行）
```

- 证据：`TestMVCCMultiTableDeleteMatchesLegacyEngine`、`TestMVCCMultiTableDeleteWithoutPrimaryKeyMatchesLegacy`、`TestMVCCMultiTableDeleteDeduplicatesTargetRows`、`TestMVCCMultiTableDeleteJoinedHeapTargetAffectedRows`。

```sql
-- LEFT JOIN 的空扩展目标不得产生假 target
DELETE e FROM e LEFT JOIN f ON e.v=f.v WHERE f.v IS NULL;
```

- 证据：`TestMVCCMultiTableDeleteLeftJoinSkipsNullExtendedTargets`、`TestMVCCMultiTableDeleteWithoutPrimaryKeyMatchesLegacy`。

```sql
-- derived / CTE / 视图不能作为删除目标
DELETE FROM (SELECT 1 AS v) d USING d JOIN a ON a.id=d.v;   -- 预期：报错（unknown DELETE target）
```

- 证据：`TestMVCCMultiTableDeleteRejectsDerivedTarget`、`TestMVCCMultiTableDeleteRejectsUnsupportedTargets`。

### 2.5 事务、回滚、FK、savepoint

```sql
-- 中途失败整条回滚（每个 DML 家族）
INSERT INTO dst SELECT id,v FROM src;   -- 第 N 行唯一冲突 → 前 N-1 行一起回滚
UPDATE t JOIN s ON s.tid=t.id SET t.score=-1;   -- CHECK 失败 → 整条回滚
DELETE FROM p;                          -- 第 3 行被 FK 引用 → 前 2 行一起回滚
```

- 证据：`TestMVCCInsertStatementRollbackThroughOperator`、`TestMVCCUpdateStatementRollbackIsAtomic`、`TestMVCCDeleteStatementRollbackIsAtomic`、`TestMVCCDownstreamErrorRollsBackChildTransaction`。

```sql
-- 用户事务内失败语句只回滚自己；savepoint 语义不变
BEGIN;
UPDATE t SET score=score+1 WHERE id=1;
UPDATE t SET score=-1;            -- 失败，不影响上面那条
SAVEPOINT sp1;
UPDATE t SET score=score+10 WHERE id=2;
ROLLBACK TO SAVEPOINT sp1;
COMMIT;
```

- 证据：`TestMVCCStatementFailureInsideUserTransactionKeepsSavepointSemantics`、`mvcc_savepoint_failure_test.go` 全部、`mvcc_savepoint_test.go`。

```sql
-- FK 动作仍走原有实现
CREATE TABLE casc(id INT PRIMARY KEY);
CREATE TABLE casc_child(id INT PRIMARY KEY, pid INT, CONSTRAINT fk FOREIGN KEY(pid) REFERENCES casc(id) ON DELETE CASCADE);
```

- 证据：`TestMVCCForeignKeyActionsMatchLegacyEngine`、`TestMVCCForeignKeySelfReferenceAndSetNullValidation`、`TestMVCCDeleteForeignKeyActions`、`TestMVCCUpdateForeignKeyActionsAndRollback`。

### 2.6 TargetRowDedup：有界、可落盘、首次命中

```sql
CREATE TABLE t(id INT PRIMARY KEY, v INT);
CREATE TABLE s(id INT PRIMARY KEY, tid INT, KEY tid_idx(tid));
-- 每个 target 匹配 8 条 source：开启小 sort budget 后 UPDATE JOIN 仍对每个 target 只改一次
```

- 预期：`SUM(v)` 等于 target 数（各加一次）；临时目录无残留。
- 证据：`TestMVCCSpillableDedupStaysBounded`、`physical.TestTargetRowDedupSpillsBeyondMemoryThresholdAndCleansUp`。

------

## 3. 关键 regression test 索引

| 主题 | 文件 | 代表测试 |
|---|---|---|
| INSERT 统一路径 | `executor/mvcc_insert_pipeline_test.go` | `TestMVCCInsertValuesAndSelectShareOneOperator`、`TestMVCCInsertAutoIncrementGapAfterRollbackIsPreserved` |
| INSERT SELECT parity | `executor/mvcc_insert_select_test.go` | `TestMVCCInsertSelectMatchesLegacyEngine`、`TestMVCCInsertSelectUsesStatementSnapshot` |
| INSERT 冲突策略 | `executor/mvcc_insert_modes_test.go` | `TestMVCCInsertModesMatchLegacyEngine`（含 `REPLACE … SELECT`、`… SELECT ON DUPLICATE KEY UPDATE`） |
| 普通 UPDATE / Halloween | `executor/mvcc_update_pipeline_test.go`、`executor/mvcc_halloween_test.go` | `TestMVCCUpdateSequentialAssignmentOrder`、`TestMVCCUpdateIndexHalloweenProtection` |
| UPDATE JOIN | `executor/mvcc_update_join_test.go`、`executor/mvcc_update_join_pipeline_test.go` | `TestMVCCUpdateJoinMatchesLegacyEngine`、`TestMVCCUpdateJoinLimitCountsDedupedTargets` |
| 普通 DELETE | `executor/mvcc_delete_pipeline_test.go` | `TestMVCCDeleteUsesUnifiedDeleteOperator`、`TestMVCCDeleteKeepsAccessPlan` |
| 多表 DELETE | `executor/mvcc_multi_delete_test.go` | `TestMVCCMultiTableDeleteMatchesLegacyEngine`、`TestMVCCMultiTableDeleteForeignKeyOrder` |
| 无主键多表 DELETE | `executor/mvcc_multi_delete_nopk_test.go` | `TestMVCCMultiTableDeleteWithoutPrimaryKeyMatchesLegacy`、`TestMVCCMultiTableDeleteJoinedHeapTargetAffectedRows` |
| FK 动作 | `executor/mvcc_fk_actions_test.go` | `TestMVCCForeignKeyActionsMatchLegacyEngine` |
| savepoint / 失败 | `executor/mvcc_savepoint_failure_test.go`、`executor/mvcc_savepoint_test.go` | `TestMVCCSavepointStatementCommitFailureAbortsTransaction` |
| LastInsertID 发布时序 | `executor/mvcc_last_insert_id_commit_test.go` | `TestMVCCInsertLastInsertIDRequiresStatementCommit` |
| 跨 DML 事务/快照/资源不变量 | `executor/mvcc_transaction_invariants_test.go` | `TestMVCCModifyPipelinesUseParentReadAndChildWrite`、`TestMVCCCancellationReleasesOperatorResources` |
| TargetRowDedup | `physical/target_dedup_test.go` | `TestTargetRowDedupKeepsFirstOccurrenceInSourceOrder`、`TestTargetRowDedupReleasesSortersOnEveryExitPath` |
| DISTINCT 未回归 | `physical/distinct_test.go` | `TestDistinctStableRepresentativeLifecycle`（`Distinct` 现委托共享 dedup 内核） |
| 架构边界 | `executor/architecture_test.go` | `TestProductionArchitectureBoundaries` |

------

## 4. 完成状态（A05 已全部收敛）

Phase 7（T070～T087）已于本轮完成，多表 DELETE 已接入统一 Modify Pipeline，
**spec 的 FR-001～FR-025 与 SC-001～SC-012 全部满足**，无剩余未完成项。

- `executor/mutation_multi_delete.go` 与 `executor/multi_delete_pipeline.go` 内
  `writeVersionedRow` / `writeInsertedRow` **零命中**；删除经 `DeleteOperator`。
- 去重使用 `physical.TargetRowDedup`（ledger 可落盘），已移除原先的行累积 map/slice
  （`resolveMultiDeleteTargets` 仅保留按语句请求的目标名去重的 `seen map`，
  其上界是目标名个数，与结果行数无关）。
- 多表 DELETE 的**可观察行为未变**：上表 2.4 与 2.3 的所有 parity 回归全部通过。

**诚实的已知限制**：去重 ledger 可落盘，但 survivor 的暂存切片本身在内存中，
上界为"不同目标行数"（非 join 结果行数）。

逐条 FR / SC 对照见 `tasks.md`。

------

## 5. 已确认的行为变更（验证时按此预期）

| 场景 | 变更 |
|---|---|
| `UPDATE ... JOIN ... LIMIT n` | LIMIT 统计去重后的唯一 target（可观察语义不变）；不再有"满足 LIMIT 后提前终止 target 扫描"的优化 |
| 同一列重复赋值（`SET c=1, c=2`） | 绑定阶段报 `duplicate update column`（原为静默取最后一次） |
| `UPDATE ... RIGHT JOIN ...` | 现由完整 JOIN 管线求值（`bindJoinsIdentified` 支持 INNER/LEFT/RIGHT/CROSS） |
