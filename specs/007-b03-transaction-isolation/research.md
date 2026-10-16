# B03 Research — 当前仓库证据、MySQL 8.4 决策与风险

**代码基线**: `pucj0/gbaselite`，`master` HEAD `fd07a48eec8dc611080dc176708c35de03d2c5f7`（2026-10-14 检索）；实现前必须复核新 HEAD。  
**资料基线**: MySQL 8.4 官方文档 + 仓库 B01/B02/A05 源码与 Constitution；本报告是静态审计而非运行测试。  
**参考来源**:
- https://dev.mysql.com/doc/refman/8.4/en/innodb-transaction-isolation-levels.html
- https://dev.mysql.com/doc/refman/8.4/en/innodb-consistent-read.html
- https://dev.mysql.com/doc/refman/8.4/en/set-transaction.html
- https://dev.mysql.com/doc/refman/8.4/en/server-system-variables.html
- https://github.com/github/spec-kit
- `.specify/memory/constitution.md`；`specs/001-b01-mvcc-transaction-manager/`；`specs/006-b02-row-lock-manager/`

## 代码证据表（实施前检查行号随 HEAD 漂移）

| Existing surface | Observed contract | B03 impact |
|---|---|---|
| `storageengine/engine.go` | Txn.Snapshot/Get/Iterator 约定快照隔离；Engine.Begin 无 level 参数；Optional capability 模式已存在 | 不改最小接口；增加 optional isolation/statement view capability |
| `storageengine/mvccadapter/adapter.go` | Begin 调用 `store.Begin`；Get/Scan/Iterator 均传入 inner 固定 Snapshot | 新 SQL 语句视图必须穿过 adapter，而不是 executor 私自读 Store.Head |
| `mvcc/transaction.go` | Store.Begin 在 views 锁内 Head 并 RegisterRoot；`Tx.Snapshot` 固定；child 从 parent 继承；root Get 用 `store.Get(t.Snapshot)` | RR lazy 与 RC statement view 和已有 fixed-root pin 设计冲突，须设计视图 pin/validator 迁移 |
| `mvcc/transaction_manager.go` | root 注册时 `snapshotRefs[readTS]++`，child 从 parent 继承 readTS | 增加可引用的 statement/transaction view token，严格配对释放和 GC horizon |
| `mvcc/conflict.go` | 普通依赖 `latestCommittedChange > readTS`；B02 引入 per-key observation baseline | RC/RU 不能因旧 root readTS 误拒绝，也不能不经锁随意调高全事务 readTS |
| `mvcc/current_read.go` | 加锁后最新 published + own overlay；B02 per-key baseline；snapshot 与 current 分离 | 保留 B02 current-read path；RC DML 使用它与明确冲突基准 |
| `mvcc/locking.go` / `lockmanager/` | root owner+scope，公平队列，gap/next-key/II，epoch 验证，root grants 保留 | SER 普通 SELECT 必须取得真正的共享 record 与谓词保护，禁止改成仅 IS |
| `executor/transaction_engine.go` | Begin/implicit/autocommit 均调用 Backend.Begin；plain Select 子查询共享 tx；mutation 走 parent read + child write | 语句边界贯穿上下游，active level 在 root 创建时冻结；异常路径释放 view |
| `executor/locking_read.go` | B02 FOR UPDATE/SHARE current reader；一些复杂查询被 fail closed 拒绝 | SER 普通 SELECT 不能只调用一般 snapshot pipeline；要扩安全 shape 或拒绝 |
| `server/handler.go:60-63` | SET isolation 直接拒绝；兼容 SET 走分支路由 | 更换为 parser+单一 validator；防文本过滤误报及协议路径差异 |
| `server/session_settings.go:367-369` | SELECT @@transaction_isolation / tx_isolation 硬编码 REPEATABLE-READ | 必须使用 session 实际状态和只读默认；SHOW VARIABLES 同步 |
| `parser/parser.go` / `parser/ast.go` | BEGIN 是空结构；SET 主要处理 PASSWORD；未见 isolation AST | 增强 parser/AST 严格验证完整 token；支持事务 SET 与安全拒绝 |
| `executor/executor.go` | Session 有 `AutocommitDisabled`, transaction, savepoints, reset state | 加 sessionDefault/pendingNext/activeLevel；ResetConnection 清理 |
| `AGENTS.md` + Constitution | README 同提交、保数据、保持 B01/A05、测试确定性、planning-only 不实现 | 所有 gate 必须执行；B03 允许受控设计扩展需在 plan 标明 |

## Design decisions（D1–D14）

**D1 — 定义范围**：以 SQL 语句可观察语义对标 MySQL 8.4 InnoDB，默认 RR。不要只做配置变量；测试先行验证异常矩阵。  
**D2 — three-scope state**：`sessionDefault` / `pendingNext` / `activeLevel` 三层，active 在 Begin 冻结；pending 在实际创建下一 root transaction 成功时消费；失败创建不能误消费。  
**D3 — RR 首次一致性读**：MySQL RR 在首个 *consistent nonlocking read* 建视图而非 BEGIN；B01 fixed begin 的语义为需明确升级的核心改变，锁定读不提前固定视图（若读路径确需初始化，需要说明并对拍）。  
**D4 — RC per-statement**：取一次发布水位并生成**同一条 SQL**的 token；整个 statement 包括子查询、JOIN、stream/iterator 完成后释放，不是每个 iterator 自己取 Head。  
**D5 — 可见性/冲突分离**：展示哪个版本，与写入时是否能够合法提交，是不同问题。保留 B02 观察基准的锁证明，按键/谓词来源标注写入基准；不得用新的 statement Head 覆盖 `Command.Snapshot` 与 validator 从而制造 lost update。  
**D6 — SERIALIZABLE**：基于 B02 root-owned S + interval/next-key + write X。对于没有可证明基表与范围映射的 SQL，不得无锁运行；如果只支持部分形状，必须暴露限制并保证 fail closed。  
**D7 — RC gap**：一般范围写取消普通 gap 阻塞仅当 B02 可安全区分；duplicate/FK 检查仍需锁 gap/预留位；不能粗暴禁用 `Gap` 或关闭 Record X。  
**D8 — RU staged published view**：计划使用**内存/阶段性只读索引**或 stage 源，其他事务能读到 atomic-complete root/merged writes。读取不依赖持有 writer Tx 对象锁跨等待；禁止未合并 child 与临时 partial stage 曝光。测试稳定调度必须知道“stage 已发布供 dirty-read”的 hook。  
**D9 — RU 数据安全**：脏读仅 SELECT；写事务约束与 RC 一样；外键/唯一键/索引维护必须读 committed/current 并取锁；RU 不意味着脏写。  
**D10 — replication policy**：因未提交 stage、leader epoch 与日志没有跨 root dirty-read 协议，本期 RU 可明确只支持 standalone，复制模式直接拒绝选择 RU 或首次 RU 事务开始，不得谎称启用成功；其余级别要证明 Barrier/term/generation 条件。  
**D11 — format stability**：不改 bbolt bucket、old stage codec、Command payload、WAL 或 Raft 协议。需要持久化结构更动必须另立提案与迁移测试，当前 B03 fail closed。  
**D12 — ownership**：view token 强引用 + generation，iterator/statement 关闭与 rollback 清理；旧 root pin 生命周期迁移有无泄漏与保留水位证明。  
**D13 — SQL state reporting**：变量赋值/SHOW/SELECT 必须使用状态机单一实现；GLOBAL runtime 设置本期 Unsupported，不假装成功；变量 string 格式 `READ-COMMITTED`。  
**D14 — Verification**：两个 MySQL CLI 连接与一个进程内部 Go API 测试，都用 barrier/channel，不用 sleep 排序；阻塞用“已知等待状态 + 超时上限”证实。

## Critical architectural collisions（未解决前不准开发后续 Phase）

1. **B01 ReadTS = Begin Head、root retention** 与 **MySQL RR lazy/RC statement snapshots**：首先证明读视图迁移不会破坏 child/savepoint、GC、复制屏障及 B01 commit validation；见 `contracts/snapshot-lifecycle.md`。
2. **B02 observed baseline** 与 **RC/RU DML 在不同语句最新快照**：若只把 root Snapshot 改成 Head，则可能允许丢失更新；若完全不变又会频繁假冲突。见 `contracts/validation-policy.md`。
3. **SERIALIZABLE 普通 SELECT** 与 **B02 锁定 SELECT shape 限制**：简单把 `Locking=true` 会导致复杂查询错误或在层间绕过；需要逐个 planner 审核或在执行前拒绝。见 `contracts/lock-policy.md`。
4. **RU** 与 **当前 stage 只由拥有者读取**：没有跨 root 原子 staging 暴露能力，便做不出真正 dirty-read。不能把 `Store.Get(Head)` 称为 RU。见 `contracts/dirty-read.md`。

## Explicit non-goals / differences

- 不承诺 MySQL 所有 lock granularity、semi-consistent read 调度、binlog format 行为、XA/DDL 隐式 COMMIT 细节都完全一致；单独列差异。
- 不承诺 RU 每次都看到尚未提交的新值，只验证在受控可观测窗口中“能够看见”。
- 不通过全局互斥锁串行化所有事务来伪造 SER。
- 不在本轮自动创建分支、修改代码、运行测试、制作 Release、重启服务。

---

## Q01–Q10 最终技术决议与实施对照（Phase 9 RC 审计）

依据 Commit `a7e80a42e3fdf6ba01e996c9a18559109650c67f` 的实际落地实现，回写 Q01–Q10 的最终实施结果、设计差异、现存限制与测试依据：

| 问题编号 | 核心问题与设计初衷 | 最终落地实施结果 | 与早期设计的差异说明 | 现存限制与测试依据 |
|---|---|---|---|---|
| **Q01** | `storageengine.Txn` 旧 Go API 的 Snapshot 语义是否必须完全不变？ | **严格落实**。所有旧 Go API 契约保持原有快照隔离，`storageengine/isolation.go` 新增中立的可选扩展能力接口 `IsolationEngine` / `IsolationTxn` / `StatementView`。 | 实现完全吻合早期设计，未改动底层最小 Engine 接口。 | **限制**：非 SQL 调用方仍使用原生快照；测试依据：`storageengine/isolation_test.go:TestStorageEngineContract_PreserveExisting`。 |
| **Q02** | RR lazy view 的 pin 与 B01 root register 的 `readTS` 是否解耦？ | **严格落实**。`Store.Begin` 初始化事务结构但不预占 pin，首次 plain SELECT 通过 `BeginConsistentView` 建立 ViewToken 并增加引用计数；提交时根据依赖类型核验。 | 实现了延迟建立，但在首读前被 Guard 的键严格保留起始时间戳基线，防止并发写绕过冲突检测。 | **限制**：锁定读不确立普通快照；测试依据：`executor/isolation_rr_test.go:TestM08_RR_LazyFirstReadTiming`。 |
| **Q03** | RC per-statement Head 捕获时点？ | **严格落实**。在 Raft Barrier 之后、Statement Physical Bind 之前分配独立 `StatementView`，在 defer 中随语句结束调用 `view.Close()` 归还资源。 | 早期草案提议每个算子自取，最终改为单语句内所有算子（含子查询与 CTE）共享原子快照，消除了 Torn Read。 | **限制**：长复杂语句仍受单语句生命周期限制；测试依据：`executor/isolation_rc_test.go:TestM13_RC_NonRepeatableRead`。 |
| **Q04** | B02 observed baseline 的复制集群实现？ | **严格落实 Fail-Closed**。复制集群模式下不支持跨节点 Carrying Baseline 验证，保持本地安全核验，未改动 Raft Command / WAL 格式。 | 早期曾探讨跨节点传递 baseline，但受宪法持久化格式冻结约束，正式决议采用 Fail-Closed 拒绝。 | **限制**：复制集群下拒绝未共识 baseline 写入；测试依据：`storageengine/isolation_test.go:TestP7Fix01_CompatibilityMatrixAndReplicationGate`。 |
| **Q05** | RU 跨事务脏读的内存与锁序安全？ | **严格落实内存快照**。Writer 在语句完成时通过 `mvcc/dirty_registry.go` 发布不可变脏数据快照，Reader 进行深拷贝采样；Rollback/断连时通过代次失效条目。 | 早期曾探讨直接共享内存，最终确认为避免 Go map 并发 panic 和锁争用，采用只读发布与深拷贝。 | **限制**：极大事务可能触发 16MB 内存上限；测试依据：`executor/isolation_ru_test.go:TestM34_RU_StageLifecycleAndSavepoint`。 |
| **Q06** | SERIALIZABLE 复杂形态支持边界？ | **严格落实 Fail-Closed (Error 3108)**。单表点查与范围查施加 IS/S/Next-Key/Gap 锁；多表 JOIN、CTE、派生表、视图、集合 UNION、聚合直接拒绝。 | 早期曾考虑对 JOIN 驱动表加锁，后因无法保证被驱动表与谓词无幻读，正式实施保守 Fail-Closed 策略。 | **限制**：复杂分析查询在 SER 报 3108；测试依据：`executor/isolation_serializable_test.go:TestM27_SER_UnsupportedQueries`。 |
| **Q07** | RC 下是否保留超出 MySQL 的保守 Gap 锁？ | **严格落实**。对于唯一性检查和外键依赖校验，保留必要 Gap 锁以防并发插入导致重复键或孤儿行。普通非约束扫描放宽间隙锁。 | 比原生 MySQL InnoDB 更加保守，优先保证嵌入式存储的强一致性与无损性。 | **限制**：高频并发插入可能发生轻微锁等待；测试依据：`executor/isolation_rc_test.go:TestM19_RC_UniquenessAndForeignKey`。 |
| **Q08** | pendingNext 作用域状态消费时机？ | **严格落实**。在 `executor/transaction_engine.go` 的显式 BEGIN、隐式事务首条 DML 及 autocommit=1 真实启动点统一消费；纯元数据（SET/SHOW）不误消费。 | 解析阶段不消费，仅在事务真正创建并成功后消费，重试与失败时状态保持不丢。 | **限制**：跨连接不共享 pendingNext；测试依据：`executor/isolation_phase2_test.go:TestB03SetNextTransactionIsolation`。 |
| **Q09** | RU Stage 内存与条数配额限制？ | **严格落实配额硬边界**。根事务最大 10,000 键、16MB 内存；全局所有事务最大 64MB。超限返回 `storageengine.ErrViewBudgetLimit` 拒绝脏读。 | 从最初无限制设计收敛为硬上限防御，彻底防止并发脏读大事务导致进程 OOM。 | **限制**：超大批量导入在 RU 下无法提供完整脏读；测试依据：`mvcc/isolation_view_test.go:TestP3Fix05_HistoryHorizonAndBudgetLimit`。 |
| **Q10** | RC 合规写入是否误报旧版本冲突？ | **严格落实 Baseline 校验**。在 X 锁保护下获取最新已发布版本并写入 baseline，提交校验器命中 baseline 时对比 `latest > baseline`，消除假冲突。 | 彻底解决了 B01 时代使用 `latest > root.readTS` 导致长 RC 事务合法写操作被频繁误杀的问题。 | **限制**：无锁快照写仍受 `latest > readTS` 保护；测试依据：`executor/isolation_rc_test.go:TestM17_RC_LegalCurrentWrite`。 |
