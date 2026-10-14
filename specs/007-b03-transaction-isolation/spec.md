# Feature Specification: B03 四种事务隔离级别

**Feature directory**: `specs/007-b03-transaction-isolation`  
**Suggested branch**: `007-b03-transaction-isolation`（文档尚未提交/未新建分支）  
**Created**: 2026-10-14  
**Status**: DRAFT — documentation only; no code/test/deployment executed  
**Input**: 为 GBaseLite 增加 RU/RC/RR/SERIALIZABLE，默认 RR；支持 session/next-transaction 配置；验证脏读、不可重复读、幻读及锁行为。

## User Scenarios & Testing

### US1 — 默认 RR 不回退、SQL 设置准确 (P1)
用户连接后看到 `REPEATABLE-READ`；`SET SESSION TRANSACTION ISOLATION LEVEL READ COMMITTED` 仅影响同连接后续事务；`SET TRANSACTION ISOLATION LEVEL SERIALIZABLE` 只影响下一事务，且不能用于已经开始的事务。

**Independent test**: 两连接独立设置；`BEGIN`/`COMMIT`/`ROLLBACK`、autocommit、disconnect/reconnect/ResetConnection，查询 `@@session.transaction_isolation`；默认 RR 在未启用新设置时原测试不回归。

**Acceptance**:
1. Given 新会话，When 查询会话和全局已公布默认值，Then 均为 `REPEATABLE-READ`（B03 不支持全局运行时修改）。
2. Given 会话配置 RC、下一事务覆盖为 SERIALIZABLE，When 开始并结束一个事务，Then 仅该事务使用 SERIALIZABLE，下一次恢复 RC。
3. Given 存在活动事务，When `SET TRANSACTION ISOLATION LEVEL READ UNCOMMITTED`，Then 原子拒绝，活动事务不受影响；`SET SESSION TRANSACTION ...` 可以修改后续事务默认，不影响当前事务。
4. Given 两个会话，When A 配置不同级别，Then B 的级别、锁和读视图不变化。

### US2 — RC 每个语句一份一致 Read View (P1)
同一 RC 事务的普通 SELECT 可读到两次 SELECT 之间的新提交；单条复杂查询在执行期间不能混用两个时点的已提交数据；能看到自己的写。

**Independent test**: 会话 A 事务中 SELECT 两次，B 在两次之间提交 UPDATE/INSERT；另外在一次多行扫描中 B 提交，A 的整条语句仍使用同一 Read View。

**Acceptance**:
1. A 第一条普通 SELECT 读 V1，B COMMIT V2，A 第二条普通 SELECT 读 V2（nonrepeatable read）；B 未提交时 A 仍看 V1（无脏读）。
2. RC 范围 SELECT 第二次可以出现已提交的新行（phantom）；同一条扫描不在中途出现不一致的“半新半旧”。
3. 语句级 read view 生命周期经 EOF、LIMIT、错误、cancel、savepoint rollback 等路径正确释放，GC 不抹去活跃读者所需版本。

### US3 — RR 固定一致视图；锁定读仍是 Current Read (P1)
重复的普通 SELECT（含不存在的行及范围）看到同一已提交视图；第一次一致性读决定 RR Read View，不把简单 BEGIN 误认为一定提前创建 Read View；本事务写入可见，B02 FOR UPDATE/FOR SHARE 保持当前读。

**Independent test**: A BEGIN、B COMMIT、A 第一次 SELECT；A 第一次 SELECT、B COMMIT、A 第二次 SELECT；A SELECT FOR UPDATE 与普通 SELECT 对照。

**Acceptance**:
1. 在 BEGIN 后、首个一致性读前 B COMMIT 的值可被首个 SELECT 看到（遵循 MySQL RR lazy read view；相应 B01 fixed readTS 是 B03 授权后的受控扩展）。
2. 首次一致性读后 B 提交 UPDATE/INSERT，A 再次普通 SELECT 不可重复读与普通 SELECT phantom 均不发生；自身 stage writes 仍可见。
3. 显式锁定读保持 B02 最新已提交版本+own overlay，不能错误读取陈旧普通快照，也不能绕开已实现的锁保护。

### US4 — SERIALIZABLE 防止读写/谓词异常 (P1)
在显式事务或 autocommit=0 的事务中，普通 SELECT 必须获得足够的共享记录锁及谓词/间隙保护；写者不可跨越受保护谓词提交。B02 无法安全锁定的查询形状必须在执行前可预期地拒绝，不能悄悄快照读取。

**Independent test**: 点查 SELECT、范围 SELECT 与另一个会话 UPDATE/INSERT；不同点/不同范围并发；并发读后升级为写触发正常等待/死锁恢复。

**Acceptance**:
1. 两个 SERIALIZABLE 读事务可同时 S 读；写入同一记录等待或按超时/死锁规则失败，直到 reader 结束。
2. 空范围、缺席键和区间边界被适当保护；范围内 INSERT 不能越过，区间外无关 INSERT 不被过度阻塞（受实际 lock plan 边界证明限制）。
3. 经典 write-skew/phantom 并发调度不能让双方在违反串行执行结果的前提下都提交。
4. autocommit=1 的独立只读 SELECT 可采用符合 MySQL 行为的非锁定一致读；显式事务不准错误套用此优化。
5. JOIN/CTE/视图/聚合/子查询等当前 B02 不能安全锁定的形状：要么新增可信计划与锁，要么在读前拒绝；不计作已兼容通过。

### US5 — RU 真实允许脏读，但绝不允许脏写 (P2)
在 READ UNCOMMITTED 下普通 SELECT 可观察另外一个尚未提交且已完整 staged 的事务更改；在对方回滚后后续 SELECT 恢复旧值；该读不能污染普通 RC/RR/SERIALIZABLE、提交、持久化、复制及外键/唯一键约束。

**Independent test**: A 先写已完整 stage 的行但不提交，B 在 RU 中读到该值；A ROLLBACK，B 再读回已提交状态；B 在其他隔离级别看不到 staged value。

**Acceptance**:
1. RU 可能读到未提交 UPDATE/INSERT/DELETE；至少每种操作有可控窗口内可重复的测试观察，不承诺任意调度都必然读到。
2. 读源不得看到半写记录、未合并 child、回滚 savepoint 的残留或失效 generation/term 的 staging；不允许脏写、双重唯一键占用或未提交数据进入 WAL/备份/复制结果。
3. RU 写路径遵循 RC 的锁/当前读/唯一键/FK 安全校验，而不是“所有读和写都不加锁”。
4. 当前体系不能安全提供 dirty read 的 backend/复制模式必须明确返回 Unsupported；禁止退化成 RC 却报告 RU 生效。

### US6 — 跨路径、资源、故障与 MySQL 对拍 (P2)
命令行、MySQL 文本协议、prepared SQL、隐式事务、保存点、复制/restore/GC 的行为与文档契约一致，错误不泄漏资源；有确定性双连接对拍。

**Independent test**: 对 MySQL 8.4 InnoDB 及 GBaseLite 运行同源测试脚本，记录状态/可见性/阻塞/错误类别差异，并持续回归既有 B01/B02/A05 测试。

## Edge Cases（必须设计与测试）

- `SET TRANSACTION` 待应用期间发生无显式 BEGIN 的第一条 autocommit SELECT/DML、`autocommit=0` 隐式 BEGIN、DDL/SHOW：记录哪些命令视为事务边界，不误消费、重复消费或遗漏一次性设置。
- 多语句 `SET`、变量别名/大小写/连字符/引号/注释/多条分号；不能靠 `strings.Contains()` 处理可见兼容值；非法值、重复隔离级别、GLOBAL/READ ONLY 语法必须清晰报错。
- `SET SESSION` 在事务中修改：当前活动事务的 `activeIsolation` 不变；`SET TRANSACTION` 无作用域仅在下一事务之前有效。
- RC 语句内部跨点/范围、JOIN/CTE/子查询/`INSERT SELECT`、WHERE/ORDER/LIMIT / prepared / stream 共享**一个**已发布 Read View；不能每算子刷新一次。
- RR 首次一致性读前/后执行 `UPDATE`、`SELECT FOR UPDATE`、SAVEPOINT、ROLLBACK TO、空结果 SELECT：明确定义首个 read view 触发与自身写覆盖。
- SERIALIZABLE 空表与不存在键、边界相交、唯一与非唯一索引、二级索引、全扫计划降级、锁升级/公平队列/锁超时/死锁；不能拿 IS 当做表级 S 锁。
- RU 正在写入 stage、未合并 statement child、半写二级索引、deleted/tombstone、owner 提前结束、stage 文件失效/压缩、读者查询正在扫描时 writer rollback，需稳定复制和生命周期保护。
- Snapshot GC、backup/restore、generation bump、leader change、replicated apply、epoch reset、cursor Close、cancel/disconnect；不得产生永久 pin/owner/iterator 泄漏。
- B01 `latestCommittedChange > readTS` 检查及 B02 per-key observed baseline，必须与 RC/RU statement epoch 重构一起证明不会误拒绝合法更新，也不会允许 lost update。
- DML/DDL 与 SERIALIZABLE READ 组合、写偏斜、父读子写、SAVEPOINT 回滚、事务 COMMIT 失败与语句失败回滚级别的差异。

## Functional Requirements

| ID | Requirement |
|---|---|
| FR-001 | 提供 RU、RC、RR、SERIALIZABLE 明确枚举，服务器与新会话默认 RR；不依赖全局用户可变开关才能启用四级。 |
| FR-002 | `SET SESSION TRANSACTION ISOLATION LEVEL x` 修改本 session 后续事务；当前事务不变。 |
| FR-003 | `SET TRANSACTION ISOLATION LEVEL x` 设置下一事务一次性覆盖；活动事务内拒绝。 |
| FR-004 | `SET SESSION transaction_isolation='READ-COMMITTED'`、`SET @@SESSION.transaction_isolation=...` 会话作用域与 `SET @@transaction_isolation=...` 下一事务作用域严格区分。 |
| FR-005 | `@@session.transaction_isolation`、`@@global.transaction_isolation`（只读默认）、`@@tx_isolation` 兼容别名及 SHOW VARIABLES 必须报告已实现真实会话/全局默认，不能硬编码欺骗。 |
| FR-006 | SQL parser/AST 正式支持事务隔离 SET 类型并拒绝不支持语法；直连 Go `Engine.Execute` 与 MySQL `ExecuteCompatible` 语义一致。 |
| FR-007 | 会话重置/断开/重连/connection pool reset 清除 pending override 和会话设置，隔离不同连接。 |
| FR-008 | 事务创建时冻结 activeIsolation，并根据语义在显式和隐式事务中准确应用；COMMIT/ROLLBACK 后待应用设置消费一次。 |
| FR-009 | RR 普通一致性读采用事务首个一致性读建立的 Read View；保持自身写可见且直到事务终态固定。 |
| FR-010 | RC 每条语句分配固定 Read View，单语句所有源、子查询、流迭代器共享，语句间可刷新。 |
| FR-011 | RU 普通 SELECT 提供受控可用的 uncommitted read 能力而非伪装成 RC；写遵守 RC 安全语义。 |
| FR-012 | SERIALIZABLE 显式/长事务中的普通 SELECT 安全获得 S/predicate 锁保护；无安全 lock plan 必须 fail closed。 |
| FR-013 | 显式锁定读 FOR UPDATE/SHARE 在所有级别保持 B02 Current Read、grant/epoch/owner 验证规则。 |
| FR-014 | RC 的 DML/current-read/唯一键/FK 不受陈旧 root `readTS` 假冲突影响，同时保持严格提交冲突保护。 |
| FR-015 | 自身写、父/statement child、SAVEPOINT、ROLLBACK TO 与所有隔离级别组合语义一致。 |
| FR-016 | RR 视图+RC/RU 语句视图被正确 pin/release，活跃 view 不被 GC 丢弃；无永久 pin。 |
| FR-017 | 对 dirty read 的 staged 数据构建原子只读发布快照，排除未合并 child 与中间状态；读者与 writer 结束安全并发。 |
| FR-018 | 绝不让未提交版本进入复制可见状态、durable publication、快照备份，也不让 RU 读取改变事务校验。 |
| FR-019 | RC/RU 正确使用 B02 行锁/唯一占位与必要的 FK/duplicate gap 例外；不能简单关闭所有 gap locks。 |
| FR-020 | RR/SERIALIZABLE 的范围锁、当前读、插入意向与 B02 兼容矩阵一致；SERIALIZABLE 具有防 phantom/write-skew 的完整锁计划。 |
| FR-021 | 缺席点、空范围、全域扫描均有可证明的边界保护；无法证明必须拒绝或安全加粗粒度谓词锁，禁止假设 IS/IX 等于表 S/X。 |
| FR-022 | 锁等待/死锁/超时/cancel 行为沿用 B02，资源释放含 statement scope 与 root owner。 |
| FR-023 | 读视图、锁、staging 的预算有上限与可观察诊断；预算耗尽 fail closed。 |
| FR-024 | 所有新能力经 `storageengine` optional capability 提供，原最小 `Engine`/`Txn` 公共接口兼容；不泄露 mvcc 到 executor。 |
| FR-025 | 不改变旧磁盘格式、Command/WAL framing/Raft payload，除非未来重新立项并明确授权。 |
| FR-026 | 复制/leader change/restore generation 不安全的 RU 等级显式拒绝；非 RU 的已支持场景不得隐式跳过 barrier。 |
| FR-027 | 语法错误、不支持形状、缺少 backend capability 的 SET/SELECT 必须返回错误，不能返回 OK 或改回 RR。 |
| FR-028 | 支持跨点查/范围/缺席键/重复读/回滚的稳定并发测试，不以 sleep 决定同步先后。 |
| FR-029 | 有两连接四级异常矩阵及与 MySQL 8.4 InnoDB 的差异报告；允许非确定性的调度差异但必须验证约束。 |
| FR-030 | B01/B02/A05/事务保存点/历史格式/资源限额回归不可退化。 |
| FR-031 | 同一代码变更中更新 README/SQL 兼容文档、限制说明、部署矩阵；不能宣称完整 MySQL 兼容。 |
| FR-032 | 结束事务/异常路径必须无 session pending / pin / staging borrow / lock wait / owner leak。 |
| FR-033 | Serializable autocommit=1 独立纯 SELECT 的 MySQL 优化行为明确；显式事务/`autocommit=0` 不允许用它逃避 S/predicate 锁。 |
| FR-034 | `START TRANSACTION WITH CONSISTENT SNAPSHOT`、READ ONLY/READ WRITE、多特征 SET、GLOBAL 等未实现命令必须显式拒绝并在兼容矩阵标记。 |
| FR-035 | 记录 RR 首次一致性读触发边界：locked/current read 与纯 metadata 命令不会不经设计地建立固定 view。 |
| FR-036 | 测试可对执行时的 active level、Read View 生命周期、锁授予与错误类别做中立层观测，不泄露敏感 SQL/行数据。 |

## Key Entities

- **IsolationLevel**：RU、RC、RR、SERIALIZABLE 的有界值。
- **SessionIsolationState**：sessionDefault、pendingNext（可空）、activeIsolation（仅活动事务）；connection reset 规则。
- **StatementReadView**：提交水位/head、generation/term、所有权、pin token、begin/end；只服务一个完整 SQL statement。
- **TransactionReadView**：RR/SERIALIZABLE 首读后固定水位及 retention ownership。
- **UncommittedReadView**：RU 使用的原子已合并 stage 可见性集合；不属于 durable head；生命周期受 owner registry 保护。
- **CommitValidationBaseline**：为每个写依赖选择校验基准，兼容 B02 observed-key baseline；不能整体改写 Command.Snapshot。
- **SerializablePredicateProtection**：已证明扫描边界对应的 record/gap/next-key/whole-domain 保护集合，根事务持有。

## Success Criteria

| ID | Gate |
|---|---|
| SC-001 | 新会话默认 RR；所有设置/查询/隐式事务/ResetConnection 等语义测试通过。 |
| SC-002 | RC 两语句 visibility 变化正确；单语句扫描中途 writer 提交时结果保持同一已提交 view。 |
| SC-003 | RR 首次一致读的建立与固定、own-writes、plain-vs-locked read 全部正确。 |
| SC-004 | RU 至少 INSERT/UPDATE/DELETE staged 读取与 rollback 消失三个确定性场景，且其他级别未见脏读。 |
| SC-005 | SERIALIZABLE record/range/empty predicate/写偏斜测试中，无不合法的双方提交；独立读者可并发。 |
| SC-006 | RC/RU DML 的合法当前更新与冲突拒绝都通过；A05 parent-read/child-write 未破坏。 |
| SC-007 | 100 次固定并发调度无卡死、无视图/锁/owner/iterator 泄露；race/gofmt/vet/test 门禁完成。 |
| SC-008 | 若复制/复杂 query 等未覆盖，测试确认明确报 Unsupported，README 准确说明，不以静默回退代替功能。 |
| SC-009 | MySQL 8.4 双连接对拍报告分类每项 PASS/KNOWN DIFFERENCE/UNSUPPORTED，主要可见性场景 PASS。 |
| SC-010 | 既有完整 `go test ./... -count=1`、`go vet ./...`、gofmt 通过；没有格式/协议非授权变更，data/ 未接触。 |
| SC-011 | spec FR/SC→design→task→test 的可追溯关系完整且所有任务有实测证据才勾选。 |
| SC-012 | 本轮交付为文档；未执行代码实现、发布或部署。 |

## Assumptions, Scope & Limits

1. B03 对标 MySQL 8.4 InnoDB 的**隔离语义**而非整个 SQL/InnoDB 实现；SERIALIZABLE 不等价于单纯 snapshot isolation。
2. 会话/next transaction 是必须项；GLOBAL 动态修改、SET PERSIST、READ ONLY/WRITE 等为单独扩展，不作本次通过前提，但语法必须报不支持。
3. 原已公开最小 Go `Txn` 仍维持旧 snapshot 合约；SQL 新级别经独立可选 capability/statement read view 实现，明确受控修改 B01 内部固定 readTS 注册设计。
4. 允许根据性能、GC/replication 约束**显式拒绝**特定 SQL shape / RU 复制模式；对外宣传时须披露，否则功能不算完成。
5. 新接口的最终 Go 名称需实施 Phase 1 经代码审计决定，文档中的候选接口仅是能力合同，不能跳过能力边界。
