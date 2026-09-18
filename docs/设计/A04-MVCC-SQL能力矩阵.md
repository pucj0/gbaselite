# A04 MVCC SQL 能力矩阵

本表以 Repository HEAD 的实测行为为准（针对 HEAD 编写的 legacy↔MVCC 对拍脚本加回归），一项一行，
不合并独立能力。MVCC 是唯一运行事务引擎；“范围限制”记录当前实现的真实边界。

| 能力 | Legacy | Parser | MVCC runtime | 回归 | 范围限制 |
|---|---|---|---|---|---|
| UNION / UNION ALL / DISTINCT | 支持 | 支持 | 支持 | 有 | 未实现完整 MySQL 类型合并 |
| 窗口函数 | 支持 | 支持 | 支持 | 有 | 不支持 named window / 显式 frame |
| INNER / LEFT JOIN | 支持 | 支持 | 支持 | 有 | 16 路输入上限 |
| RIGHT JOIN | 支持 | 支持 | 支持 | 有（对拍） | SELECT 交换驱动侧；UPDATE JOIN 取目标匹配子集；多表 DELETE 保留完整 RIGHT JOIN 语义（未匹配的右表行仍可作为删除目标，未匹配的左表行因空扩展不删） |
| CROSS JOIN | 支持 | 支持 | 支持 | 有（对拍） | 无 ON 时笛卡尔积 |
| Derived table in FROM | 支持 | 支持 | 支持 | 有（对拍） | 必须带别名；别名列在相关作用域可见（legacy 不发布） |
| Derived table in JOIN | 支持 | 支持 | 支持 | 有（对拍） | 同上；受结果内存预算约束 |
| 非递归 CTE | 支持 | 支持 | 支持 | 有（对拍） | 语句级作用域，支持列清单/多 CTE/遮蔽同名表 |
| 递归 CTE（WITH RECURSIVE） | 支持 | 支持（需 UNION ALL） | 支持 | 有（对拍） | 列清单语法 parser 不支持；深度上限 1000；越界/列数不符报错回滚 |
| 标量/IN/NOT IN/EXISTS 子查询（含相关、写入中） | 支持 | 支持 | 支持 | 有（对拍） | 无 FROM 的 `SELECT (SELECT …)` 两引擎均不支持 |
| UPDATE JOIN（连接输入为基表） | 支持 | 支持 | 支持 | 有（对拍） | 目标行多命中只更新一次，取首个匹配 |
| INSERT SELECT（含 UNION ALL 源） | 支持 | 支持 | 支持 | 有（对拍） | 源使用语句父快照 |
| INSERT SET / IGNORE / REPLACE / ON DUPLICATE KEY UPDATE | 支持 | 支持 | 支持 | 有（对拍） | REPLACE 删除全部冲突行；被引用行返回 parent change |
| 多表 DELETE（目标表有主键） | 支持 | 支持 | 支持 | 有（对拍） | 不支持 LIMIT；同一物理行被多个 join 组合命中只删除一次，去重按行 identity（无主键表用 storage row key） |
| 多表 DELETE（目标表无主键） | 支持 | 支持 | 支持 | 有（对拍） | 基表扫描在语句快照上携带 storage row key 作为行 identity，驱动目标与 joined 目标都支持，相同值的重复行按物理行区分；derived/CTE/视图没有可删除 identity，作为目标时报 unknown DELETE target（同 legacy） |
| CREATE VIEW | 支持 | 支持 | 支持 | 有（对拍） | 定义以可重新 parser.Parse 的 SQL 存入统一 MVCC catalog（`view/<db>/<name>`），CREATE 前先绑定校验，失败/回滚不留半个 view；OR REPLACE 同 legacy；parser 无 CREATE VIEW IF NOT EXISTS 形式；视图列名必须唯一（`SELECT *` 覆盖同名 join 列被拒绝，同 MySQL），legacy 允许重复列名属其宽松差异 |
| Query VIEW（SELECT ... FROM view） | 支持 | 支持 | 支持 | 有（对拍） | 绑定顺序 CTE → derived → view → base table；读取语句快照；支持 view over 基表/JOIN/聚合/UNION/派生表/子查询、嵌套 view、别名与限定列、INSERT SELECT 源；v1→v2→v1 循环返回稳定错误不递归；子查询 FROM 为视图时外层相关引用两引擎都报 unknown column（legacy 同） |
| DROP VIEW | 支持 | 支持 | 支持 | 有（对拍） | 支持多视图与 IF EXISTS；走 statement child 事务，reopen 后一致 |
| DROP DATABASE（含视图清理） | 支持 | 支持 | 支持 | 有（对拍） | 同一条 statement child 事务内删除 `db/<db>`、`table/<db>/…` 与 `view/<db>/…`；数据库内父子外键不因 catalog key order 阻止删除；rollback 恢复整库；DROP + recreate 不会复活旧 view，也不残留 orphan view entry；同时清空会话当前库（同 legacy） |
| VIEW 元数据（SHOW TABLES / SHOW FULL TABLES / SHOW COLUMNS / DESCRIBE / SHOW CREATE VIEW） | 支持 | 支持 | 支持 | 有（对拍） | 视图发布进元数据镜像，与基表同一命名空间；表/视图重名的 CREATE TABLE、CREATE TABLE LIKE、CTAS、RENAME TABLE 按 legacy 拒绝（IF NOT EXISTS 静默跳过） |
| EXPORT DATABASE … TO 'path' | 支持 | 支持 | 支持 | 有（对拍） | 在语句快照上物化整库快照（表定义 + 行 + 视图）后复用 legacy 逻辑备份渲染器，输出格式一致；导出只读语句快照，不修改业务数据 |
| CREATE TABLE AS SELECT | 支持 | 支持 | 支持 | 有（对拍） | 查询快照 + 同语句 child 事务建表插数，失败不留空表 |
| CREATE TABLE LIKE | 支持 | 支持 | 支持 | 有（对拍） | 复制列/主键/索引/CHECK/注释，不复制行与外键（同 legacy） |
| RENAME TABLE | 支持 | 支持 | 支持 | 有（对拍） | 保留表 ID/数据命名空间，同步 catalog 名、FK RefTable/Referrers/fkref；源必须是基表，目标名不得是视图 |
| SAVEPOINT / ROLLBACK TO / RELEASE SAVEPOINT | 支持 | 支持 | 支持 | 有（对拍） | 用 `Txn.Child()` 分层实现，无独立顶层事务；RELEASE 只摘名字 |
| FK ON DELETE/UPDATE CASCADE / SET NULL（含自引用） | 支持 | 支持 | 支持 | 有（对拍） | 级联在同一 statement child 事务内，深度上限 32，失败整条回滚 |
| 锁定读（SELECT … FOR UPDATE / LOCK IN SHARE MODE） | 支持 | 支持 | 支持 | 有（对拍） | 与 legacy 一致按语句快照读取，不做加锁 |
| ON UPDATE 列表达式（ON UPDATE CURRENT_TIMESTAMP） | 支持 | 支持 | 支持 | 有（对拍） | 未显式赋值列在 UPDATE/UPDATE JOIN 时写入当前时间 |
| 自引用外键（含自引用级联） | 支持 | 支持 | 支持 | 有（对拍） | 见上一行 |
| 事务快照、语句原子性、断连回滚 | 支持 | - | 支持 | 有 | 快照隔离 |
| metadata refresh（non-RevisionReader backend） | 支持 | - | 支持 | 有 | 修复 `head = tx.Snapshot()` 后连续 revision 正确 |

要点：

- 查询、派生表、CTE、子查询都在语句父快照事务上求值，不新开事务；mutation 写入 statement child
  transaction，失败整条回滚；`LastInsertID` 只在语句事务提交成功后发布。
- 派生表/CTE 通过 `physical.Materialize` + transient schema 进入统一 pipeline，不经过旧 Result 物化管线。
- parser 的每个 statement（含 `CreateView/Query VIEW/DropView/ExportDatabase`）都有 MVCC runtime 实现，
  上表“范围限制”列只记录真实边界（例如 multi-table DELETE 不支持 LIMIT、递归 CTE 深度上限 1000、
  跨库外键不支持）。
- 已知 **legacy 比 MVCC 宽松**的语义差异（statement 级无缺口）：视图投影出现重名列时 legacy 接受
  （`SELECT * FROM a JOIN b` 且 a/b 都有 `id`），MVCC 与 MySQL 一致地在 CREATE VIEW 阶段拒绝
  （error 1060 duplicate column name）。该差异由 `server` 测试
  `TestMVCCRejectsAmbiguousViewColumnsBeforeChangingMetadata` 固定；若要求与 legacy 完全一致，
  需要放宽视图列名唯一性，属独立评估项。
- 旧 snapshot/paged 数据迁移（`migrate-legacy`）同样导入视图定义；视图与基表共用命名空间，重名按 legacy 拒绝。
- 窄范围 catalog lifecycle audit（本轮）：`CREATE DATABASE`、`DROP DATABASE`、`CREATE VIEW`、`DROP VIEW`、
  SHOW 元数据、`EXPORT DATABASE`、`BACKUP/RESTORE MVCC`、`migrate-legacy`、reopen 均已覆盖 `view/` 命名空间，
  未再发现 view catalog lifecycle 遗漏。

------

# A05：写入算子统一（Modify Pipeline）

A05 是 executor 内部重构，**不新增任何 DML SQL 语法**、不改变上表任何一行的可观察行为。上表仍是
行为契约；本节只记录"这些行为现在由哪条代码路径产生"。

## 目标模型

```text
Parser / Binder
      ↓
Query Pipeline（读 statement parent 快照）
      ↓
Mutation Candidate
      ↓
TargetRowDedup / LIMIT / Staging
      ↓
InsertOperator / UpdateOperator / DeleteOperator
      ↓
storageengine.Txn（statement child 事务）
```

核心边界：查询管线决定"修改谁"，Modify Operator 决定"怎么修改"。

## 统一进度

| DML 家族 | 统一写入算子 | 状态 | 最终 row write 唯一入口 |
|---|---|---|---|
| INSERT VALUES / SET | `InsertOperator` | ✅ 已统一 | `modify_insert.go` → `writeInsertedRow` |
| INSERT SELECT | `InsertOperator` | ✅ 已统一（与 VALUES 同一个算子） | 同上 |
| 普通 UPDATE | `UpdateOperator` | ✅ 已统一 | `modify_update.go` → `writeVersionedRow` |
| UPDATE JOIN | `UpdateOperator` | ✅ 已统一（`TargetRowDedup` 去重 + LIMIT 在 dedup 之后） | 同上 |
| 普通 DELETE | `DeleteOperator` | ✅ 已统一 | `modify_delete.go` → `writeVersionedRow(old → nil)` |
| 多表 DELETE | `DeleteOperator` | ✅ 已统一（`TargetRowDedup` 去重 + winner 暂存） | `multi_delete_pipeline.go` → `delete` → `modify_delete.go` |

## 统一后仍保持不变的行为契约

- INSERT SELECT / UPDATE JOIN / 多表 DELETE 的源查询一律在 **statement parent 快照**上求值；
  写入一律经 **statement child 事务**；失败整条回滚；`LastInsertID` 只在语句事务提交成功后发布。
- `statement snapshot` 与 `child transaction` 的分割是 Halloween protection 的实现基础：扫描期间
  写入落在 child，父快照看不到本语句的写入，因此"谓词永远为真"的 UPDATE/DELETE 仍对每个物理行只改一次。
- CHECK / UNIQUE / PRIMARY KEY / 二级索引 / FK / CASCADE / SET NULL / auto_increment 的底层实现
  **未重构**：统一后的算子仍然调用原有的 `writeInsertedRow`、`writeVersionedRow`、
  `applyForeignKeyActions`。
- UPDATE JOIN 的 target 多次匹配仍只更新一次并取首个匹配（由 `physical.TargetRowDedup` 的
  stable first-occurrence 语义实现，取代原"每 target 取 `Limit(1)`"）。
- UPDATE 的 SET 赋值仍按语句顺序求值（`SET a=a+1, b=a` 中 `b` 读到新的 `a`）。
- 主键 UPDATE 的 `RowIdentity` 仍是**旧**物理行标识：写路径用旧 key 删除旧行并在内部计算新 key。
- auto_increment 允许失败后留 gap；child 提交前不发布 `LastInsertID`。
- 多表 DELETE 仍不支持 `LIMIT`；重复 JOIN 组合命中同一物理行只删除一次；无主键表用 storage row key
  作为行 identity，值完全相同的两行仍可区分；derived/CTE/视图作为删除目标仍被拒绝；多目标仍按
  child-before-parent 排序，循环目标外键仍被拒绝。

## 已确认的行为差异（A05 引入，均为有意变更）

| 场景 | 变更 |
|---|---|
| `UPDATE ... JOIN ... LIMIT n` | LIMIT 现在统计**去重后的唯一 target**（原先由每 target 取首匹配间接等价）。可观察语义不变，但不再有"满足 LIMIT 后提前终止 target 扫描"的优化，需先算完 dedup 才能截断。 |
| `UPDATE t SET col=..., col=...`（同列重复赋值） | 现在在绑定阶段报 `duplicate update column`；原实现静默取最后一次。与 UPDATE JOIN 既有行为一致，也符合"binding error 应在第一条写入前返回"。 |
| `UPDATE ... RIGHT JOIN ...` | 现由完整 JOIN 管线求值，`bindJoinsIdentified` 支持 INNER/LEFT/RIGHT/CROSS；原 per-target 嵌套 join 只覆盖 target-driven 子集。 |

## 多表 DELETE 的统一实现（Phase 7 / T070～T087 完成）

`DELETE t1,t2 FROM ... JOIN ...` 与 `DELETE FROM t1,t2 USING ...` 已接入统一 Modify Pipeline：

- `executor/mutation_multi_delete.go` 内**不再有任何** `writeVersionedRow` / `writeInsertedRow` 调用；
  语句绑定一次 join 计划（`bindMultiDeletePlan`），交由 `executor/multi_delete_pipeline.go` 求值。
- 去重改用 `physical.TargetRowDedup`（ledger 可落盘），不再是 `seen map + rows slice` 的无界收集；
  同时移除了行累积用的 map/slice。
- 最终删除**经过 `DeleteOperator`**：每个目标按 `multiDeleteOrder` 顺序构造一个 `DeleteOperator`
  （`multi_delete_pipeline.go`），由它调用 `applyForeignKeyActions` 与 `writeVersionedRow(old → nil)`。
- 行 identity 不再依赖主键探测：已识别 join 会为每个基表输入附加
  `alias.__rowid`（identity 标记）与 `alias.__rowkey`（物理存储键）两列隐藏列，
  外连接 NULL 扩展的判定改由 provenance 的 NULL 语义决定，而非业务列取值。

先前记录的阻塞原因（`TargetRowDedup` 不携带 payload、`identityScan` 账本被嵌套循环 re-scan 覆盖）
均已解决：`TargetRowDedup` 改为 payload-preserving（`runPayloadDedup` 按 ledger key 暂存 winner），
identity 改由行携带的 provenance 列决定，不再依赖被覆盖的账本。

**因此 A05 的 FR-025 / SC-005 已完全满足**。

已知的诚实限制：去重 ledger 可落盘，但 survivor 的**暂存切片本身在内存中**，其规模以
"不同目标行数"为上界（非 join 结果行数）。
