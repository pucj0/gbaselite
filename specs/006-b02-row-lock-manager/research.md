# B02 Research

**Date**: 2026-10-10 · **Baseline**: `140838c2d4f10971c010a9e4d643fd4b135634f2`
**Scope**: 只读源码调研、官方资料及规划决策；无生产实现。

## 已检查的现有设计与源码

| 证据 | 事实 / B02 影响 |
|---|---|
| AGENTS.md、README.md | README 同变更、保护 data/、Go 改动全量验证、部署顺序、有限兼容 |
| specs/001-b01-mvcc-transaction-manager/{spec,plan,tasks}.md | fixed readTS、三类 root、retention、五类 commit validation；历史 Spec Kit 状态不能当作本轮证据 |
| specs/005-unified-modify-pipeline/{spec,plan,tasks,research,quickstart}.md | parent source / child write、identity dedup、first-match、LIMIT、SET 顺序、spill；历史 `spec/` 路径说明与实际 `specs/` 不符 |
| docs/设计/StorageEngine接口.md、PhysicalOperator管线.md | 中立接口、可选能力、Iterator 借用字节及同步 yield |
| mvcc/transaction_manager.go、transaction_state.go | root 注册与 retention、child diagnostic、短 manager mutex、原 Txn 仍单调用者 |
| mvcc/transaction.go、merge.go | Get 先当前/祖先写再固定 Snapshot；child 只 MERGED；publication-aware finish 与 cleanup 独立 |
| mvcc/conflict.go | Put/Delete/Guard/GuardRange 按 latest > readTS 拒绝；普通读不参与验证 |
| mvcc/group_commit.go、local_commit.go、local_stream.go、local_wal.go、store.go | 小批/group、大事务 stream、WAL acceptedChanges、replicated pending 均遵守 shared validator |
| mvcc/store.go、flat_scan.go、snapshot.go、maintenance.go | publication markers、布局快路径、generation reset、固定 horizon 约束 |
| storageengine/engine.go、relation.go、mvccadapter/{adapter,iterator,diagnostics}.go | Capability seam；Guard 不是阻塞锁；不能污染最小 Txn 或泄露 mvcc |
| parser/ast.go、parser/parser.go | Locking 目前只有 bool；FOR UPDATE/LOCK IN SHARE MODE；NOWAIT/SKIP LOCKED 被接受但策略丢弃；FOR SHARE 尚未支持 |
| executor/access_plan.go、physical_binding.go、physical_select.go | 当前锁定读明确按 snapshot 读取；有统一点/范围/全扫描边界 |
| executor/transaction_engine.go、savepoint_mvcc.go | parent query、statement child、autocommit、32 层、RELEASE 匿名层、ROLLBACK TO 安全重建 |
| executor/modify_{insert,update,delete}.go、mutation*.go | 统一最终写；candidate OldRow 与 Identity 必須在等待后验证 |
| executor/foreign_key.go、secondary_index.go、range_access.go | FK guards、索引回表/row identity、旧编码不一定保留 SQL 值顺序 |
| sqllayout/keyspace.go、storage/versioned_values.go | 现有 namespace/row/index 字节身份；UNIQUE NULL 不参与唯一 reservation，collation 与编码必须复用 |
| replication/node.go | VerifyLeader 不提供 FSM ordering；Barrier 才建立 applied ordering；Term 字段已有 |

## R1 — 语义参考与有限兼容
**Decision**: 采用 Record、意向、Gap、Next-Key、Insert Intention 的逻辑语义；明确 B02 的
快照写冲突、保守 savepoint、锁定查询形状与粗粒度 fallback 差异。
**Rationale**: 核心锁能力与完整 InnoDB 隔离、页锁实现、优化器行为是不同承诺。
**Alternatives**: 宣称完整 InnoDB 兼容不符合项目；用 GuardRange 代替 Gap 没有阻塞语义。
**Sources**: [MySQL 8.4 InnoDB Locking](https://dev.mysql.com/doc/refman/8.4/en/innodb-locking.html)、
[SAVEPOINT](https://dev.mysql.com/doc/refman/8.4/en/savepoint.html)、
[Deadlock Detection](https://dev.mysql.com/doc/refman/8.4/en/innodb-deadlock-detection.html)。
这里采用语义模型，不移植其 B-tree 页格式或声称相同 victim 算法。

## R2 — Current Read 与 B01 验证
**Decision**: 新增显式 locked-current 能力；原 Get/Scan/Iterator/readTS 不变。持有锁后才取
短 committed view，叠加指定 parent/read layer 的既有写。不存在仍返回最新 change/tombstone
version。对 write/dependency 保留 latest > root.readTS 冲突；DML 早期检查不替代最终验证。
**Rationale**: 同 root 可锁定读取 R+1，普通读继续 R；但不能用更晚数据隐式授权 SI 写入。
**Alternatives**: 修改 Snapshot、per-key rebase、锁后直接豁免 validator 会破坏 INV-005；
向 Command/WAL 加 receipt 则违反协议限制；只在 executor 豁免会造成复制不一致。
**Consequence**: 无法承诺老事务等待后更新新提交版本成功。单纯锁定读不创建 Guard，仍为 B01 A 类。

## R3 — A05 集成与元数据
**Decision**: 保留 parent snapshot candidate/source、child write。UPDATE/DELETE 锁后按旧物理
identity 校验最新版本；任何 post-readTS 变更（含墓碑、迁移）报 ErrConflict，终止 root 并提示重试。
兼容版本使用原 OldRow/EvalRow、SET 顺序与 first-match；不重新扫描 child，不纳入快照外新候选。
INSERT 判重用 child 当前 overlay + committed 最新观察；目录最新版本比 readTS 新时先冲突再解码。
**Rationale**: 保护 Halloween、JOIN source、去重、LIMIT 及旧索引删除正确性。
**Alternatives**: 将 parent 扫描变为 current scan 会改变候选集合；刷新后重算 JOIN/LIMIT 是新 DML 语义，超出范围。

## R4 — 稳定区间，而非跟随物理邻居
**Decision**: 用现有字节命名空间内的显式区间维护 Gap，endpoint 固定，不绑定可删除的邻居对象。
Next-Key 的 (prev,key) Gap 与 key Record 原子授予；末端 Gap 为 (last,+∞)。SQL predicate
保护可包含边界，必要时表示为 interval + boundary reservation；缺失唯一点可按稳定 predicate
区间覆盖该 key，保守范围允许比 InnoDB 邻居 gap 更宽。
**Rationale**: 页分裂/边界删除不影响内存区间；部分/非有序编码不可凭 SQL 文本推断字节范围。
**Alternatives**: 哈希 `(lower,upper)` 不能检测不同边界的重叠；改持久化编码被禁止；动态邻居重绑复杂且不必要。
**Scope**: ordered PK 范围复用 planner 证明；UNIQUE 等值用现有编码；无法证明时全 index-domain Gap。

## R5 — 兼容、排队、升级
**Decision**: Gap 无 S/X 强弱；II/II 兼容，Gap/II 在插入位置相交时冲突；重复键另由 Record X
reservation 决定。所有 IS/IX 组合兼容。一个 atomic bundle 请求包含必要 table intent、record、gap。
同 owner 跳过自阻塞；升级保留 S。队列按 conflict footprint 公平，年轻兼容者可共同授予，但不得
越过会被其阻塞的老 waiter。图记录 holders 与旧 waiter 两种原因。
**Rationale**: 既防饥饿也防对不相关资源全局排队；全 engine FIFO 会把行锁变成事务门。
**Alternatives**: 只记录 holder 图漏掉 queue cycle；释放 S 再要 X 破坏已获得的保护。

## R6 — 同步与存储视图
**Decision**: 每 engine 独立 manager；table/index domain 内用分片短元数据 latch。
多域按稳定 domain 顺序一次检查/登记 bundle；owner/graph latch 仅短期持有。等待前全部释放。
lock-aware scan 每页复制必要 key/value 并关闭 bbolt view 后才 yield/Acquire；等待期间也不持
Store.apply、restore gate、Tx stage view、manager mutex。页上界/临时 spool 复用现有预算。
**Rationale**: 当前同步 Iterator yield 可能跨越 bbolt view；直接在 Apply 等待有 remap/commit 死锁风险。
**Alternatives**: 全局 mutex 持有到 txn 结束禁止；开启每扫描后台 goroutine 破坏现有生命周期；
先无限物化候选破坏资源模型。

## R7 — 生命周期与 deadlock
**Decision**: root 是 grants owner，scope/child 仅记录请求来源；全部 child/savepoint 终态保留已授予锁。
root 定案、不能再执行读写、清理 stage 后幂等释放。Cycle victim 为环内注册序号最大 root；
graph revision 验证环仍有效，置 abortRequested，再通知 owner safe point。检测器不调用 Tx.Rollback。
**Rationale**: 单 Txn 不支持跨 goroutine 操作；所有 cycle 成员都在等待，victim 被唤醒后可自行清理。
**Alternatives**: 用随机 ID/墙钟选 victim 无法确定性重放；child rollback 全解锁会泄露 root 已读保护；
提前释放 grants 允许仍可 merge 的 child 继续写。

## R8 — timeout、取消及预算
**Decision**: `SET SESSION innodb_lock_wait_timeout=N`，1..86400 整数秒，默认 50。
期限=min(lock deadline, query/context deadline)。Lock timeout 错误 1205/HY000；deadlock
1213/40001；既有 ErrConflict 映射保留，错误类别在 diagnostics 中可区分。
timeout/普通 cancel 回滚 statement 工作，grants root 保留；deadlock/早期 SI conflict 结束 root。
Engine 默认 lock memory 64 MiB、每 root grant+pending components 262144、全 engine waiter 4096、
图边容量与一次遍历work均100000；可选 capability options 可提高，0 使用默认，非“无限”。
具体YAML/env/option名称及范围见contracts/lock-manager.md配置表。diagnostics 每页最多 256。
**Rationale**: locks 必须有独立预算；不能从 write-set unlimited=0 推导 lock metadata unlimited。
**Alternatives**: 忽略 queue budget 或错误当 deadlock 不准确；overflow accounting 必须 fail closed。
**Compatibility risk**: 新默认锁预算可能限制大事务；实现前基线任务必须覆盖现有大事务数量，
在保持 bounded metadata 的前提下调整这些规划默认值并同步四类文档，不能跳过回归。
**T004 校准结果（已完成）**: 独立夹具实测（`.tmp/b02-resource-calibration.md`）显示含 PK+UNIQUE
的 SQL UPDATE/DELETE 为 3 components/行，20000 行 = 60000 components，而规划默认 65536 只够
约 21800 行；仓库自带 10000 行 UPDATE benchmark 路径已是 30000（占旧上限 46%）。
故每 root 上限由 65536 上调至 **262144**（≈8.7× 实测最大语句）。64 MiB 内存预算保持并仍封顶
（262144 × 96 B ≈ 24 MiB，× 最坏 216 B ≈ 54 MiB）。waiter 4096、图 100000、diagnostics 256 不变。
该调整只放宽每 root 上限，不改变锁兼容、SI 验证、提交协议或任何持久化格式；超限仍为 fail closed
的 `ErrLockResourceLimit`。

## R9 — 所有写入口与复制
**Decision**: storageengine 中立 resource 使用实际 `(Space,Key)` 身份；SQL 加入 table/index 标签，
manager 不解析 SQL 编码。默认 MVCC Tx.Put/Delete 从保存的 caller context/当前 statement lock scope
参加同一 key 资源协调；缺失键 Put 获 II+X，存在键覆盖/删除获 X，所有真实插入必须检查区间。
SQL 在 child 写前显式 Acquire(ctx) 带查询期限；low-level 默认 context 是 Begin 的生命周期 context。
Guard/GuardRange 原义不变。Raw command apply/WAL replay 不等待 lock；仍保留 commit validator。
**Rationale**: 只在 SELECT 加锁或只在 SQL DML 加锁都会被底层写绕过。
**Replication**: Current Read leader-only，每次可能等待后 Barrier + generation/term recheck；
generation/term 变更撤销 pending、失效 owner，不保留跨 term locks。FSM 不把本地队列带入日志。
**Alternatives**: follower 使用独立 manager 无法给锁定读承诺；只有 VerifyLeader 不能保证 applied current view。

## R10 — SQL 覆盖与观测
**Decision**: 锁定 SELECT 初版单基表形状，显式 S/X mode；NOWAIT/SKIP LOCKED 早拒绝。
可选 LockingTxn/LockDiagnostics 由 adapter 提供；第三方后端普通事务能力不变，
锁定查询缺失能力返回 ErrUnsupported。`SHOW ENGINE LOCKS`/`SHOW ENGINE LOCK WAITS` 与
`SHOW ENGINE DEADLOCKS` 只向管理员展示 bounded snapshot，不含 key/SQL。
**Rationale**: 接口能力与 SQL 语义分开；将 Locking bool 当完整语法无法区分 FOR SHARE。
**Alternatives**: 仿完整 performance_schema 表扩大范围；静默退回 snapshot 读违背 FR-012。

## 研究状态
Phase 0 研究完成；架构选项均有决策和替代解释，无需要外部权限/用户答复的未决定项。
资源默认值与性能阈值是可验收的规划值，尚未测量；调优不能改变锁兼容或 SI 验证语义。
两个 Spec Kit 要求的研究子任务均只读，已完成提交路径和锁/生命周期独立核查。
