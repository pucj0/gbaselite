# Clarifications: B03 Decisions and Defaults

**Status**: Phase 1 架构审计已完成；Q01–Q10 已依据 `master` HEAD（Commit `5c845d4f92f29431e433633f85c90ddf21e312af`）的源码证据全部完成技术决议并关闭。

## Adopted defaults

- [x] 默认 RR；SQL 显式支持 RU/RC/RR/SERIALIZABLE，SESSION 与 NEXT scopes。
- [x] 参考 MySQL 8.4 InnoDB：RR **首个一致性读**快照、RC **每语句**快照；锁定读为当前读。
- [x] RU 真正允许 staged dirty read，写安全采用 RC 保护；不能变更名字做伪支持。
- [x] 本期不支持动态 GLOBAL/PERSIST 变更；SQL 明确拒绝，global introspection 只读 RR。
- [x] SERIALIZABLE long txn 普通读加共享+谓词保护；无法证明安全 shape 在读前拒绝。
- [x] RC 锁 gap 放宽以正确性优先，不能安全完成的 MySQL 行为差异写进文档。
- [x] RU 复制拓扑可先显式 Unsupported，standalone 提供功能，但不得宣称复制 RU 已支持。
- [x] 不授权新 WAL/Raft/磁盘格式；需要时另立 B03.1 提案。

## Resolved engineering questions (Phase 1 Audited)

### Q01: `storageengine.Txn` 旧 Go API 的 Snapshot 语义是否必须完全不变？
- **Decision**: 必须完全不变。所有旧 Go API（`Engine.Begin` 与 `Txn.Snapshot/Get/NewIterator`）维持原有 Snapshot Isolation 契约不变。新 SQL 隔离级别通过 `storageengine` 可选能力接口（`IsolationEngine` / `IsolationTxn` / `StatementView`）提供。
- **Alternatives**: (1) 动态修改 `Txn.Snapshot/Get`（被拒：破坏非 SQL 调用方与内部工具的快照假设）；(2) 强制所有调用方传入隔离级别（被拒：破坏最小 Engine 接口）。
- **Evidence**: `storageengine/engine.go:82-87`, `storageengine/mvccadapter/adapter.go:91-97`, Constitution Principle II & III.
- **Affected FR / SC**: FR-010, FR-024; SC-002, SC-010.
- **Updated docs**: `contracts/snapshot-lifecycle.md`, `.tmp/b03-validation.md`.
- **Approved at HEAD**: Commit `5c845d4f92f29431e433633f85c90ddf21e312af`.
- **Status**: **RESOLVED**

### Q02: RR lazy view 的 pin 与 B01 root register 的 `readTS` 是否可以分离且不改变三类 root commit 行为？
- **Decision**: 可以分离。解耦事务生命周期注册与一致性快照 Pin。Root `Begin` 时初始化事务和锁所有者但不预先锁定 snapshotRefs pin；首个普通一致性读（Plain SELECT）时通过 `BeginConsistentView` 捕获当前 `Head` 并 pin，实现 MySQL RR lazy view；提交校验时根据写入依赖类型分别采用 lazy view 或 baseline 校验。
- **Alternatives**: (1) Begin 即固定 Pin（被拒：无法实现 MySQL 8.4 RR lazy 首读新提交语义）；(2) 原地并发修改 `readTS` 与 `snapshotRefs`（被拒：数据竞争与下溢风险高）。
- **Evidence**: `mvcc/transaction.go:85-120`, `mvcc/transaction_manager.go:277-286`, `mvcc/maintenance.go:38-44`.
- **Affected FR / SC**: FR-009, FR-016; SC-003, SC-007.
- **Updated docs**: `contracts/snapshot-lifecycle.md`, `contracts/validation-policy.md`, `.tmp/b03-validation.md`.
- **Approved at HEAD**: Commit `5c845d4f92f29431e433633f85c90ddf21e312af`.
- **Status**: **RESOLVED**

### Q03: RC per-statement Head 捕获时点相对 Raft Barrier/statement physical bind/stream Open 应在哪里？
- **Decision**: 捕获时点严格设置在 **Raft Barrier 之后、Statement Physical Bind 之前**。在语句执行入口获取单一 `StatementView` Token，注入 Context/Session，供该语句的全部算子（Scan, HashJoin, Subquery, CTE）共享；语句结束/Stream 关闭时在 defer 中 `view.Close()` 释放。
- **Alternatives**: (1) 各算子打开迭代器时自取 Head（被拒：导致单语句多算子版本分裂 Torn Read）；(2) 跨语句持有 View（被拒：压死 GC horizon）。
- **Evidence**: `executor/transaction_engine.go:246-255, 336-350`, `executor/subquery_mvcc.go:14-25`.
- **Affected FR / SC**: FR-010, FR-016; SC-002, SC-007.
- **Updated docs**: `contracts/snapshot-lifecycle.md`, `.tmp/b03-session.md`, `.tmp/b03-validation.md`.
- **Approved at HEAD**: Commit `5c845d4f92f29431e433633f85c90ddf21e312af`.
- **Status**: **RESOLVED**

### Q04: B02 observed baseline 是否已有 replicated apply 的安全实现？
- **Decision**: 确认当前 HEAD 上的 Raft 复制路径尚未支持 carrying baseline 的安全验证，`mvcc/local_commit.go:21-23` 严格 fail-closed。在 Phase 1 明确维持该限制，复制集群若产生 baseline 写入显式返回 `errBaselineNeedsLocalValidation` 并报 Unsupported，不私自改动 Command/WAL 磁盘格式。
- **Alternatives**: (1) Follower 跳过 baseline 验证直接写（被拒：破坏数据一致性与丢失更新检查）；(2) 扩展 Command 结构体（被拒：违反不得修改持久化/Command 格式的宪法禁令）。
- **Evidence**: `mvcc/local_commit.go:15-25`, `mvcc/current_read.go:12-20`.
- **Affected FR / SC**: FR-014, FR-025, FR-026; SC-008, SC-010.
- **Updated docs**: `contracts/validation-policy.md`, `.tmp/b03-validation.md`.
- **Approved at HEAD**: Commit `5c845d4f92f29431e433633f85c90ddf21e312af`.
- **Status**: **RESOLVED** (as an intentional fail-closed boundary with documented limitation)

### Q05: RU 完整 merged stage 如何成为另一个 root 可安全采样的原子 index？root/savepoint/epoch/GC 的锁序有无死锁？
- **Decision**: 禁止外部 Reader 直接并发读取 Writer 的私有 `buffered` map 或 `stage` bbolt 文件。当 Writer 完成 Child Merge 并处于语句稳态时，原子公布只读的 Staged Snapshot 镜像；Reader 采样并拷贝所需字节（深拷贝），生命周期以 Reader View Token 保护。Writer 发生 Rollback / Disconnect 时，通过 generation/term 失效该 entry，Reader 迭代器不受影响。锁序遵循 views -> manager.mu，不产生死锁。
- **Alternatives**: (1) Reader 直接读 Writer 内存（被拒：导致 Go map 并发读写 panic 或 bbolt 锁冲突）；(2) 伪装成 RC 只读 committed 数据（被拒：不诚实，违反宪法）。
- **Evidence**: `mvcc/write_buffer.go:11-41`, `mvcc/merge.go:5-38`, `mvcc/transaction.go:440-475`.
- **Affected FR / SC**: FR-011, FR-017, FR-018; SC-004.
- **Updated docs**: `contracts/dirty-read.md`, `.tmp/b03-dirty-read.md`.
- **Approved at HEAD**: Commit `5c845d4f92f29431e433633f85c90ddf21e312af`.
- **Status**: **RESOLVED** (design proof completed; needs implementation proof in Phase 7)

### Q06: SERIALIZABLE 对 JOIN/聚合/CTE/索引等 shape 的本期实际支持矩阵和错误文案如何确定？
- **Decision**: 单表 PK 点查/范围查、唯一索引点查已具备安全加锁能力；全表扫描使用全域 Fallback Gap 保护；**对多表 JOIN、子查询、CTE、View、聚合/GROUP BY/DISTINCT、UNION 等当前 B02 无法安全端到端加锁的形态，在执行前显式拒绝并报错 `errLockedReadShape` / `ErrLockPlanUnavailable`**，绝不静默降级为快照读。
- **Alternatives**: (1) 仅锁主表行忽略子查询（被拒：子查询无保护，破坏串行化）；(2) 静默降级为无锁快照读（被拒：严重违背 SERIALIZABLE 承诺）。
- **Evidence**: `executor/locking_read.go:68-82`, `executor/lock_plan.go:125-170`.
- **Affected FR / SC**: FR-012, FR-020, FR-021, FR-027; SC-005, SC-008.
- **Updated docs**: `contracts/lock-policy.md`, `.tmp/b03-locking.md`.
- **Approved at HEAD**: Commit `5c845d4f92f29431e433633f85c90ddf21e312af`.
- **Status**: **RESOLVED**

### Q07: 是否允许在 RC 中保留超出 MySQL 的保守 gap 锁？
- **Decision**: 允许保留超出 MySQL 的保守 Gap 锁。在 B02 锁规划器能精准识别并保证唯一键与外键检查的前提下，普通 RC DML 尽量减少不必要的 Gap 锁；若无法在保持正确性的同时安全区分，优先保证正确性，保留保守 Gap 锁定，并在 README 作为已知差异（KNOWN DIFFERENCE）披露。
- **Alternatives**: (1) 全局彻底禁用所有 Gap 锁（被拒：导致唯一性约束和外键并发损坏）；(2) 强行模仿 MySQL 但留下并发漏洞（被拒：违反正确性第一原则）。
- **Evidence**: `lockmanager/compatibility.go:50-55`, `contracts/lock-policy.md`.
- **Affected FR / SC**: FR-019, FR-020; SC-006, SC-008.
- **Updated docs**: `contracts/lock-policy.md`, `.tmp/b03-locking.md`.
- **Approved at HEAD**: Commit `5c845d4f92f29431e433633f85c90ddf21e312af`.
- **Status**: **RESOLVED**

### Q08: 在 autocommit=0 `SET SESSION`/隐式事务、prepared 及连接 reset 上如何触发 pendingNext 消费？
- **Decision**: 在 `executor/transaction_engine.go:executeSQLStatement` 的事务创建点统一触发消费。覆盖 3 种情况：(1) 显式 `BEGIN/START TRANSACTION` 成功；(2) `autocommit=0` 时首条启动隐式事务的 SQL 成功启动事务；(3) 独立 autocommit=1 单语句临时事务成功启动。若 Begin 失败或为纯元数据语句（`SET`, `SHOW`），不消费 pendingNext。连接重置（`ResetConnection`）或断开时清除 pendingNext 与 activeIsolation，重置 sessionDefault 为 RR。
- **Alternatives**: (1) 在解析器层直接消费（被拒：执行失败会导致待应用设置丢失）；(2) 语句级跨连接继承（被拒：连接池污染）。
- **Evidence**: `executor/transaction_engine.go:264-322`, `executor/session_settings.go:242-251`.
- **Affected FR / SC**: FR-003, FR-007, FR-008; SC-001.
- **Updated docs**: `contracts/sql-isolation.md`, `.tmp/b03-session.md`.
- **Approved at HEAD**: Commit `5c845d4f92f29431e433633f85c90ddf21e312af`.
- **Status**: **RESOLVED**

### Q09: RU staged index 内存/对象数默认预算如何由 B02 并发/大事务基线测量后校准？
- **Decision**: 脏读 Stage 内存预算采用明确的上限控制：每个根事务最大公布 Stage 键条数 `MaxDirtyStagedKeys = 10,000`，最大脏读内存 `MaxDirtyStagedBytes = 16MB`，全局所有活跃事务公布脏读内存总和不超过 64MB。若超出限制，该事务后续脏读公布返回 `ErrWriteSetLimit` / `ErrLockResourceLimit`，RU 查询安全降级或 fail closed，防止内存耗尽。
- **Alternatives**: (1) 无上限自由缓冲（被拒：导致 OOM 崩溃）；(2) 随意的无依据阈值（被拒：需与 B02 的 `lockMemoryBytes` 预算对齐）。
- **Evidence**: `mvcc/write_buffer.go:71-76`, `storageengine/engine.go:156-163`, `contracts/dirty-read.md`.
- **Affected FR / SC**: FR-017, FR-023; SC-004, SC-007.
- **Updated docs**: `contracts/dirty-read.md`, `.tmp/b03-dirty-read.md`.
- **Approved at HEAD**: Commit `5c845d4f92f29431e433633f85c90ddf21e312af`.
- **Status**: **RESOLVED**

### Q10: 同一事务 READ COMMITTED 的合法 current update 是否在旧 `latest>readTS` 上误拒绝？
- **Decision**: 不会误拒绝。RC 下的合规写操作必须在持锁当前读下执行，通过 `ReadCurrentLocked` 获取最新已发布版本并记录 `writeBaseline`；提交校验器在 `v.baseline` 命中时比较 `latest > observed`，只要持锁期间该行未被外部修改，即允许提交，绝不因 `latest > root.readTS` 发生假冲突。未加锁的纯快照写依然受 `latest > readTS` 保护，防止丢失更新。
- **Alternatives**: (1) 直接将 root readTS 原地修改（被拒：会引发前述的丢失更新风险）；(2) 完全不引入 baseline（被拒：RC 写入将严重误报假冲突）。
- **Evidence**: `mvcc/conflict.go:110-124`, `mvcc/current_read.go:88-124`.
- **Affected FR / SC**: FR-014; SC-006.
- **Updated docs**: `contracts/validation-policy.md`, `.tmp/b03-readview.md`, `.tmp/b03-validation.md`.
- **Approved at HEAD**: Commit `5c845d4f92f29431e433633f85c90ddf21e312af`.
- **Status**: **RESOLVED**
