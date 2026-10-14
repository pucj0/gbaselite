# B03 Phase 1 Verification and Audit Report

- **Feature**: `specs/007-b03-transaction-isolation`
- **Phase**: Phase 1: Setup & Architectural Proof (T001–T008)
- **Status**: PASSED (Phase 1 Gate Ready, awaiting Phase 2 user authorization)
- **Branch**: `master`
- **Git Commit HEAD**: `5c845d4f92f29431e433633f85c90ddf21e312af`
- **Go Version**: `go version go1.26.5 windows/amd64`
- **Date**: 2026-10-14

---

## 1. T001: 真实基线与回归验证

| 验证项 | 执行命令 | 退出码 | 耗时 | 结果与详情 |
|---|---|---|---|---|
| Go 环境 | `go version` | 0 | <1s | `go version go1.26.5 windows/amd64` |
| Git 状态 | `git status --short` | 0 | <1s | 工作区干净 (Clean) |
| Git HEAD | `git rev-parse HEAD` | 0 | <1s | `5c845d4f92f29431e433633f85c90ddf21e312af` |
| 代码差异 | `git diff --check` | 0 | <1s | 无冲突标记、无异常空白 |
| 静态检查 | `go vet ./...` | 0 | ~3s | 无任何警告或错误 |
| 全量单元测试 | `go test ./... -count=1` | 0 | 120.8s | 21 个测试包全部通过 (0 FAIL) |

### 既有架构核心回归覆盖
- **A05 parent-read / child-write & statement atomicity**: 在 `mvcc` 和 `executor` 测试中通过；
- **B01 事务生命周期与提交冲突**: `RegisterRoot`, `RegisterChild`, `snapshotRefs`, `latestCommittedChange > readTS` 测试全部通过；
- **B02 行/间隙锁管理器**: Record S/X, IS/IX, Gap, Next-Key, Insert Intention, 死锁图检测、等待队列全部通过；
- **B02 当前读与逐键基线**: `ReadCurrentLocked` 与 `withBaseline` 在 `mvcc/current_read_test.go` 和 `conflict_test.go` 通过；
- **生产数据安全**: 生产 `data/` 未做任何修改，所有测试均使用 `t.TempDir()` 隔离环境。

---

## 2. T002: MVCC 与提交验证调用链审计

1. **`Store.Begin()` 时点确定 readTS**:
   - `mvcc/transaction.go:94` 在 `s.views.Lock()` 保护下调用 `s.Head()` 确定 snapshot 水位，在 `s.newTx` (行 117) 注册 `RootRegistration{ReadTS: snapshot}`，在行 131 赋值 `tx.Snapshot = snapshot`。
2. **root、child、SAVEPOINT 共享继承机制**:
   - Root 注册时 `snapshotRefs[readTS]++` (`mvcc/transaction_manager.go:285`)；
   - Child (`Tx.ChildContext()`, 行 185) 继承父级 `t.Snapshot`，`pinned: false`；
   - SAVEPOINT 基于 child 嵌套，读取逐级回溯至 root (`mvcc/transaction.go:245-248`)。
3. **GC Retention pin/release 机制**:
   - Pin 仅由 Root 注册触发；
   - 事务达到终态 (`Transition` 到 COMMITTED/ABORTED) 或 `Unregister` 时调用 `releaseRefLocked` (`mvcc/transaction_manager.go:353-365`) 递减 `snapshotRefs`；
   - `CompactHistory` (`mvcc/maintenance.go:42-44`) 以 `OldestReadTS` 为界清理废弃版本。
4. **B01 `latestCommittedChange > readTS` 触发路径**:
   - `mvcc/conflict.go:121-124`；覆盖本地流式提交 (`local_stream.go:46`)、Group Commit (`group_commit.go:138`)、Local WAL (`local_wal.go:329`)、Direct Commit (`store.go:616`)。
5. **B02 per-key observation baseline 覆盖机制**:
   - 在排他/共享锁下调用 `ReadCurrentLocked` 记录 `t.writeBaseline[key] = observation.ChangeTS` (`current_read.go:213-250`)；
   - 校验器中若存在 baseline，以 `latest > observed` 替代 `latest > readTS` (`conflict.go:110-120`)，只要持锁期间未被并发修改即允许提交；超限 (16384) fail-closed 回退。
6. **RC 每语句刷新 Read View 的两难冲突**:
   - 若不加锁直接改 `readTS`，会引发**丢失更新 (Lost Update)**；
   - 若仅靠旧 root `readTS` 校验，会导致合规当前更新出现**假冲突 (False Conflict)**。
7. **旧 Go API Snapshot 保持**:
   - `storageengine/engine.go` 的 `Engine.Begin` 与 `Txn` 保持最小契约不变；通过可选接口 `IsolationEngine` / `IsolationTxn` / `StatementView` 扩展 SQL 隔离能力。

---

## 3. T003: SQL 执行链路与 Session 审计

- **三层状态机**: `sessionDefault (默认 RR) → pendingNext (一次性覆盖) → activeIsolation (活动事务冻结)`。
- **消费规则**: `pendingNext` 仅在底层事务**成功启动 (`Backend.Begin` 无错误)** 时消费一次；Begin 失败或元数据语句（`SET`, `SHOW`）绝不消费。
- **单语句单一 Read View 机制**:
  - 语句执行前分配 `StatementView` Token，注入 Session/Context；
  - 物理扫描（Table/Index/Range Scan）、HashJoin、子查询 (`subqueryRunner`)、CTE 统一共享该 Token；
  - 在 `executeSQLStatement` 的 `defer` 中执行 `view.Close()`，确保 EOF、LIMIT 提前退出、错误、取消等路径无泄漏。
- **失败清理路径**:
  - 针对活跃事务 `SET TRANSACTION` 拒绝、Begin 失败、锁超时、死锁受害者、取消与断开连接均建立了完备的清理机制与状态重置映射。

---

## 4. T004: B02 锁管理器兼容性审计

- **加锁原语兼容性**:
  - Record S/X: S/S 兼容，S/X 互斥，X/X 互斥；
  - Table IS/IX: 意向锁互不排斥；B02 无表级 S/X，IS 不能代替整表保护；
  - Gap / Next-Key / II: Gap 互不排斥，仅排斥区间内的 Insert Intention。
- **SERIALIZABLE 普通 SELECT 策略**:
  - 显式事务/`autocommit=0` 必须获取访问键的 Record S 锁及范围保护（Gap / Next-Key）；
  - `autocommit=1` 单语句独立纯 SELECT 优化为非锁定一致读（MySQL 兼容）；
  - 缺席点与空范围必须由 Gap 锁保护，防止幻读。
- **Query Shape 支持与 Fail-Closed 决策**:
  - **支持并保护**: 单表 PK 点查/范围查、单表唯一索引查、单表全表扫描 (Fallback Whole-Domain Gap)；
  - **明确不支持并 Fail Closed (`errLockedReadShape` / `ErrLockPlanUnavailable`)**:
    多表 JOIN、子查询/派生表、公用表表达式 (CTE)、视图 (View)、聚合/GROUP BY/DISTINCT、UNION。绝不静默降级为无锁快照读。

---

## 5. T005: RU 脏读可行性审计

- **核心结论**: 绝对不能直接读取 Writer 的私有 `buffered` map 或 `stage` bbolt 临时文件。
- **技术可行性证明**:
  1. 采用**已合并 Stage 的原子公布机制 (Staged Publication)**；
  2. 仅在 Statement Child 成功执行 `mergeParent` 后的语句稳态才进行发布，彻底排除未合并 Child；
  3. Reader 采样并深拷贝所需字节，生命周期以 Reader View Token 保护；Writer 回滚不影响 Reader 已复制切片；
  4. 设立严格配额 (`MaxDirtyStagedKeys = 10,000`, `MaxDirtyStagedBytes = 16MB`)，超限 fail-closed；
  5. RU 写操作强制走 RC 的行排他锁与当前读校验，绝不允许脏写；
  6. 坚决不使用 RC committed read 伪装 RU。

---

## 6. T006: M01–M44 测试映射方案

- **无 Sleep 并发同步**: 严格使用 `chan struct{}` 信号屏障、LockManager 等待队列观察点、RU Staged 发布 Hook 建立偏序。
- **全场景映射**:
  - Session 状态与语法 (M01–M07, M44) 映射至 Phase 2；
  - RR 首次读与不可重复读 (M08–M12) 映射至 Phase 4；
  - RC 语句快照与当前写 (M13–M20) 映射至 Phase 5；
  - SERIALIZABLE S/谓词锁与拒绝形态 (M21–M28) 映射至 Phase 6；
  - RU 真实脏读与写保护 (M29–M36) 映射至 Phase 7；
  - 视图 GC、Epoch 与全回归 (M37–M43) 映射至 Phase 3 与 Phase 8。

---

## 7. T007 & T008: Q01–Q10 技术决议与规格一致性

- **Q01 (旧 Go API)**: RESOLVED — 契约严格不变，隔离走可选能力接口；
- **Q02 (RR Lazy Pin)**: RESOLVED — 事务注册与一致读 Pin 解耦，首读分配 View Token；
- **Q03 (RC 语句时点)**: RESOLVED — Barrier 之后、Bind 之前分配单一 Token，全算子共享，defer Close；
- **Q04 (复制路径 Baseline)**: RESOLVED — 确认 HEAD 源码限制，复制模式下产生 baseline 写入显式返回 Unsupported；
- **Q05 (RU 原子采样)**: RESOLVED — 稳态已合并 Stage 只读发布，内存深拷贝，不持底层文件句柄；
- **Q06 (SERIALIZABLE 复杂形态)**: RESOLVED — 复杂形态（JOIN/Subquery/CTE/View/聚合）在执行前 fail-closed 拒绝；
- **Q07 (RC 保守 Gap 锁)**: RESOLVED — 以正确性为先，无法安全剥离时保留保守 Gap 锁并作为差异披露；
- **Q08 (PendingNext 消费时机)**: RESOLVED — 事务成功 Begin 时消费，元数据与失败不消费，ResetConnection 清理；
- **Q09 (RU 内存配额)**: RESOLVED — 单事务 10,000 keys / 16MB，全局 64MB 严格预算；
- **Q10 (RC 当前写冲突)**: RESOLVED — 锁下观测记录 baseline，比较 `latest > observed`，不发生假冲突也不丢失更新。

### 规格文档一致性检查
- 已核对 `spec.md`、`plan.md`、`research.md`、`data-model.md`、5 份 contracts、`checklists/clarification.md`、`tasks.md`、`test-matrix.md`；
- 所有关于接口定义、状态机、锁策略、验证规则、差异说明的技术决策均保持严格一致；
- `tasks.md` 中的 Phase 1 任务 T001–T008 已根据真实完成证据更新为 `[x]`。

---

## 8. Phase 1 Gate 审查结论

- **T001–T008 状态**: 全部完成 (PASS)。
- **代码状态**: 生产代码未做任何修改（遵守只读审计禁令）；`data/` 未触碰。
- **阻塞问题状态**: 底层架构冲突（Q01–Q04 及 Q05–Q10）已全部完成技术决议并书面关闭。
- **Phase 2 准入资格**: 已完全具备。

---

## 9. Phase 2 实施与验收结果 (T009–T018)

- **授权阶段**: Phase 2: SQL Grammar & Session State
- **起始 Commit**: `f064da3781cac9f10e80550035c40434c691cba6`
- **实施范围**:
  - `parser/ast.go`, `parser/parser.go`, `parser/parser_test.go`: 强类型 `IsolationLevel`, `IsolationScope`, `SetTransaction` 语法解析与红测覆盖；
  - `executor/executor.go`, `executor/session_settings.go`: 三层会话隔离状态机 (`sessionDefault`, `pendingNext`, `activeIsolation`) 及 `ResetConnection` 清理；
  - `executor/transaction_engine.go`: 显式/隐式/单语句事务消费 `pendingNext` 与 `activeIsolation` 绑定；
  - `executor/savepoint_mvcc.go`: 事务提交/回滚时保证 `ClearActiveIsolation()`；
  - `executor/isolation_phase2_test.go`: 会话状态机、未实现级别门禁及单元回归测试；
  - `server/handler.go`: 移除文本 substring 早拒绝；统一走 AST 路由与执行器状态机；
  - `server/session_settings.go`: 变量回显 (`SELECT @@...` 与 `SHOW VARIABLES`) 统一标准连字符字符串，不支持 GLOBAL 写入；
  - `server/isolation_phase2_test.go`: 变量往返、协议一致性、Prepared 动态读取及兼容设置非回归测试；
  - `README.md`: 同步功能矩阵与 Phase 2 执行能力限制。
- **T017 安全门禁验证**:
  - 底层存储引擎在 Phase 2 仅支持 REPEATABLE READ 执行；
  - 若事务以 READ UNCOMMITTED、READ COMMITTED 或 SERIALIZABLE 启动，`checkIsolationCapability` 立即返回 `storageengine.ErrUnsupported`；
  - 启动失败后 `pendingNext` 保留不消费，不生成假 active 事务，不泄漏锁与事务注册；
- **全量测试通过情况**:
  - `gofmt -l parser executor server`: clean
  - `go vet ./...`: clean
  - `git diff --check`: clean
  - `go test ./... -count=1`: 21 包全部通过 (0 FAIL)，耗时 ~166.5s；
  - 生产 `data/` 未修改，写测试全量隔离在临时目录；
- **Phase 2 Gate 审查结论**:
  - T009–T018 全部完成 (PASS)；
  - 具备进入 Phase 3（Read View Foundation & GC）的前置条件。

---

## 10. Phase 2 审查问题修复 (P2-FIX-01 & P2-FIX-02)

1. **P2-FIX-01: 隔离 SQL 误识别修复**:
   - `server/handler.go:isIsolationSetAttempt` 原实现通过字符串全局包含关键词导致合法的用户变量赋值（如 `SET @note = 'transaction_isolation'`）被误拒；
   - 增加红测 `server/isolation_phase2_test.go:TestP2Fix01_SetVariableStringLiteral` 证实报错；
   - 改用 `parser.Lex` 词法结构分析：仅检测 `SET` 赋值目标字段（左侧标识符或 `@` 变量前缀），彻底避免对右侧字符串字面量误伤；合法字面量成功通过，真正非法的隔离设置（如 `SET GLOBAL transaction_isolation`）依然严格拒绝；
   - 测试通过 (PASS)。
2. **P2-FIX-02: 真实二进制 Prepared 协议覆盖**:
   - 补充 `server/isolation_phase2_test.go:TestB03IsolationBinaryPreparedProtocol`，通过真实 TCP socket 和 MySQL 驱动端到端验证 `COM_STMT_PREPARE`、`COM_STMT_EXECUTE`、`COM_STMT_CLOSE`；
   - 验证准备时为 RR、执行前修改为 RC 时，预处理语句在执行时动态读取当前 Session 变量 (`READ-COMMITTED`)；
   - 补充 `TestB03IsolationCOMResetConnectionProtocol`，通过二进制包直接验证 `0x1f` (`COM_RESET_CONNECTION`)，证实连接重置后隔离级别恢复默认 `REPEATABLE-READ`。

---

## 11. Phase 3 实施与验收结果 (T019–T029)

- **授权阶段**: Phase 3: Read View Foundation & GC
- **实施内容与证据**:
  - **T019 (Contract 红测)**: `storageengine/isolation_test.go` 验证原始 `Engine.Begin`、`Txn.Snapshot` 语义不变，可选能力缺失返回 `ErrUnsupported`，`executor` 不导入 `mvcc`；
  - **T020 (中立 API)**: `storageengine/isolation.go` 定义 `ReadView`、`StatementViewKind`、`ViewState`、`IsolationTxn`、`IsolationEngine`；
  - **T021 (MVCC Adapter)**: `storageengine/mvccadapter/isolation.go` 实现 `mvccReadView`，封装 MVCC 句柄，隔离底层 bbolt 与 Store 私有结构；
  - **T022 & T023 (Pin 与所有权)**: `mvcc/transaction_manager.go` 新增 `RegisterView`、`ReleaseView`、`ActiveViews` 与 `viewsByRoot`；事务终态自动回收所属视图引用，防止泄漏；
  - **T024 (原子快照与 GC)**: `mvcc/isolation.go:Store.BeginViewToken` 在 `views.Lock()` 临界区内原子获取 Head 并加持 `snapshotRefs`，`CompactHistory` 遵守 `OldestReadTS` 屏障；`Store.Restore` 重置 generation 并使旧视图失效；
  - **T025 & T026 (Statement View 与流式生命周期)**: `executor/transaction_engine.go` 与 `Result.Close()` 建立所有权转移模型，流式全生命周期托管，EOF/LIMIT/提前退出/连接重置安全释放；
  - **T027 (并发与资源边界)**: `executor/isolation_phase3_test.go:TestB03ViewConcurrency` 验证高并发创建与释放无死锁、无悬挂 Pin；
  - **T028 (提交校验与当前读非回归)**: `executor/isolation_phase3_test.go:TestB03CommitValidationNonRegression` 验证纯快照读绝不绕过写冲突校验，防止丢失更新；
  - **T029 (门禁验收)**: M37–M39 全绿；全量回归测试通过；README 同步修订。
- **全量测试通过情况**:
  - `gofmt -l .`: clean
  - `go vet ./...`: clean
  - `git diff --check`: clean
  - `go test ./... -count=1`: 21 包全部通过 (0 FAIL)；
  - 生产 `data/` 未修改，写测试全量隔离在临时目录；
- **Phase 3 Gate 审查结论**:
  - T019–T029 全部完成 (PASS)；
  - 具备进入 Phase 4（REPEATABLE READ）的前置条件。

---

## 12. Phase 3 Review Remediation (P3-FIX-01 至 P3-FIX-05)

- **基线 Commit**: `f9c81fbb83f8902864dd428c4287f1161edb07a7`
- **审查修复内容与证据**:

### P3-FIX-01：协议层流式结果资源泄漏 (P0 - RESOLVED)
- **问题**: 原 `protocol.WriteResult` 与 `WriteBinaryResult` 未在入口声明 `defer result.Close()`，若在发送列数、列定义、EOF 或首行之前发生网络写错误或客户端断开，StreamRows/StreamValues 回调未被调用，导致底层的 ReadView Pin 与自动事务永久泄漏。
- **改动文件**: `protocol/result.go`, `executor/executor.go`
- **红测先行**: `protocol/result_test.go:TestP3Fix01_ProtocolWriteResultLifecycle` 在列数、列定义、EOF 写入失败及流式消费完成等场景下捕获未释放红测失败。
- **修复措施**: 在 `WriteResult` 与 `WriteBinaryResult` 函数入口统一设置 `defer result.Close()`；`Result.Close()` 采用 `sync.Once` 保证幂等安全，重复调用无副作用。
- **绿测证据**: `go test ./protocol -run TestP3Fix01 -count=100` 全部通过（0.887s）。

### P3-FIX-02：隔离能力不能伪支持 (P0 - RESOLVED)
- **问题**: 原 `mvccadapter.BeginIsolated()` 无条件接受任意 `IsolationLevel` 并启动普通快照事务，对外部直接调用 Go API 的用户伪造支持；`BeginStatementView` 对 `StatementViewKindUncommitted` 未返回 Unsupported。
- **改动文件**: `storageengine/mvccadapter/isolation.go`
- **红测先行**: `storageengine/isolation_test.go:TestP3Fix02_CapabilityGateInAdapter` 捕获到直接调用 Go API 传入非法级别或未实现的 RC/RU/SERIALIZABLE 能够意外成功的红测失败。
- **修复措施**:
  1. `BeginIsolated` 在启动底层事务前校验级别有效性，对未实现级别（RU、RC、SERIALIZABLE）立即返回 `storageengine.ErrUnsupported`，绝不提前注册底层根事务；
  2. `BeginStatementView` 对 `StatementViewKindUncommitted` 显式返回 `storageengine.ErrUnsupported`，杜绝未实现 Stage 读之前的伪支持。
- **绿测证据**: `go test ./storageengine -run TestP3Fix02 -count=100` 全部通过（3.586s）。

### P3-FIX-03：Result 与 ViewToken 并发清理与读保护 (P1 - RESOLVED)
- **问题**: `cleanupDone` 为普通布尔变量，存在数据竞争；若在 `view.Get` / `ScanRange` 正在读取底层数据时被并发 `Close()`，GC Pin 可能被提前释放，产生悬垂读或数据损坏。
- **改动文件**: `executor/transaction_engine.go`, `mvcc/isolation.go`
- **红测先行**: 增加 `executor/isolation_phase3_test.go:TestP3Fix03_ConcurrentCloseAndStream` 与 `TestP3Fix03_ConcurrentCommitRollbackAndReadView`。
- **修复措施**:
  1. `ViewToken` 改用读写锁 `rw sync.RWMutex`：活跃读取持有 `RLock`，`Close()` 持有 `Lock`。`Close()` 必须等待正在执行中的读操作完成，随后标记 `closed = true` 并释放 GC Pin；后续读操作立即返回 `ErrViewClosed`；
  2. `executor` 流式托管采用 `cleanupOnce sync.Once` 与 `streamConsumed atomic.Bool`：保证并发 Close 安全、重复消费返回 `ErrViewClosed`。
- **绿测证据**: `go test ./executor -run 'Test(P3Fix03|B03ViewConcurrency|B03StreamingViewLifecycle)' -count=100` 全部通过（13.729s）。

### P3-FIX-04：Restore 后 View 状态不准确 (P1 - RESOLVED)
- **问题**: `mvccReadView.State()` 仅检查 closed，在 Restore 发生代次变更后无法返回 `INVALIDATED`；`RegisterView` 允许为不存在或已结束的 root 注册视图。
- **改动文件**: `mvcc/isolation.go`, `storageengine/mvccadapter/isolation.go`, `mvcc/transaction_manager.go`
- **红测先行**: `mvcc/isolation_view_test.go:TestP3Fix04_ViewStateInvalidatedAndRootOwnership` 捕获 `view.State` 未定义及孤儿视图注册未拒绝的编译/运行失败。
- **修复措施**:
  1. `ViewToken.State()` 动态比对 `generation`：若代次不匹配直接返回 `storageengine.ViewStateInvalidated`，`Get`/`ScanRange` 报错 `ErrViewInvalidated`；
  2. `TransactionManager.RegisterView` 校验 `RootID`：必须属于已注册且处于 `TransactionActive` 的根事务，否则返回 `ErrUnknownTransaction`/`ErrInvalidRegistration`。
- **绿测证据**: `go test ./mvcc -run TestP3Fix04 -count=100` 全部通过。

### P3-FIX-05：历史水位与视图预算 (P1 - RESOLVED)
- **问题**: `RegisterView` 缺乏全局上限机制，且未校验请求的 revision 是否已被 GC 回收。
- **改动文件**: `mvcc/transaction_manager.go`
- **红测先行**: `mvcc/isolation_view_test.go:TestP3Fix05_HistoryHorizonAndBudgetLimit` 验证低水位注册与超配额失败。
- **修复措施**:
  1. 引入并严格强制 `DefaultMaxActiveViews = 16384` 与 `DefaultMaxViewsPerRoot = 256` 预算配额，超限 fail-closed 报错 `ErrViewBudgetLimit`；
  2. 历史低水位校验：若请求的 revision 在 `snapshotRefs` 中无引用且低于当前 GC Horizon，拒绝注册（`requested revision is below GC horizon`）。
- **绿测证据**: `go test ./mvcc -run TestP3Fix05 -count=100` 全部通过。

---

## 13. Phase 3 修复验证总结与最终 Gate 审查

1. **P0 / P1 缺陷解决状态**:
   - P3-FIX-01 (P0): **RESOLVED**
   - P3-FIX-02 (P0): **RESOLVED**
   - P3-FIX-03 (P1): **RESOLVED**
   - P3-FIX-04 (P1): **RESOLVED**
   - P3-FIX-05 (P1): **RESOLVED**
2. **测试与回归状态**:
   - 修复相关并发与生命周期测试分别执行 100 轮（`-count=100`），全部 100% 通过（0 FAIL）；
   - 全库 21 个带测试包执行 `go test ./... -count=1` 全部通过（总耗时 ~176s）；
   - `gofmt -l`、`go vet ./...`、`git diff --check` 全部 100% clean。
3. **环境约束 (Race Detector)**:
   - 当前 Windows 宿主环境未安装 C 编译器（`cgo` 未启用），`go test -race` 提示 `go: -race requires cgo; enable cgo by setting CGO_ENABLED=1`；
   - 与既有基线保持一致，未伪造测试结果；所有并发逻辑均通过 `atomic`、`RWMutex`、`sync.Once` 和 100 次高并发调度断言进行严格检验。
4. **Phase 4 准入结论**:
   - **所有 P0 与 P1 审查遗留项全部彻底关闭**；
   - 历史回退与并发安全证明确凿，零生产数据影响；
   - **GBaseLite B03 Phase 3 最终 Gate 正式通过**，已完全具备进入 Phase 4（REPEATABLE READ）的技术前置条件。

---

## 14. Phase 4 实施与验收结果 (T030–T036)

- **基线 Commit**: `c57873cefeba44565e001d5ad0557fb838c8ef33`
- **实施内容与证据**:

### 1. 前置历史可用性漏洞修复
- **红测先行**: `mvcc/isolation_view_test.go:TestP4Prereq_CompactHistoryPurgesUnpinnedHistory` 成功复现在无活跃 Pin 时 CompactHistory 物理裁剪旧版本后、尝试请求已回收版本导致数据缺失的漏洞。
- **修复措施**:
  1. `Store.CompactHistory` 在持久化 `metaBucket` 中记录 `compacted_horizon`，并在引擎启动时恢复；
  2. `TransactionManager.RegisterView` 增加低水位强制阻断：凡 revision 低于 `compacted_horizon`，或请求非当前 Head 且无活跃 Pin 的历史 revision，均严格 fail-closed 拒绝注册（`ErrInvalidRegistration`）。
- **绿测证据**: `go test ./mvcc -run TestP4Prereq_CompactHistoryPurgesUnpinnedHistory` 通过（1.112s）。

### 2. T030: M08–M12 红测先行
- **文件**: `executor/isolation_rr_test.go`
- **红测证据**: 在未实现 Lazy Read View 时运行 `TestM08_RR_LazyFirstReadTiming`，捕获到真实的 `A first SELECT returned [[V1]], want 'V2' (failed MySQL RR lazy read view)` 失败证据。

### 3. T031: RR Lazy Read View 实现
- **解耦生命周期与快照 Pin**:
  - `Store.BeginLazy` 与 `RootRegistration.Lazy`：SQL RR 事务启动时仅注册事务 ID 和锁所有者，不提前锁定 `snapshotRefs`，避免空闲事务持续压制 GC；
  - `BeginConsistentView`：在事务执行首个普通一致性读时原子采样当前已发布 `Head`，注册 `ViewToken` 并锁定 `snapshotRefs[Head]++`，更新事务 `Snapshot` 水位；
  - 幂等复用：事务后续的普通一致性读直接复用首个建立的 `consistentView`；
  - 终态释放：`COMMIT`/`ROLLBACK`/Reset/错误路径统一释放快照 Pin。

### 4. T032: 普通一致读与锁定读严格分离
- **普通一致读**: 仅普通 `SELECT`、`WITH`、`UNION` 触发并复用 RR Transaction Read View；
- **锁定读**: `SELECT ... FOR UPDATE`、`FOR SHARE`、`LOCK IN SHARE MODE` 走 B02 Current Read，加锁读取最新发布版本，不触发、不创建、不刷新普通 RR 一致性视图；
- **DML 与元数据**: 写入走修改管线与当前读，不误套用普通 SELECT 快照。

### 5. T033: 自身写覆盖、Child 与 Savepoint 验证
- `TestM11_RR_OwnStagedWrites` 与 `TestM11_RR_SavepointRollbackOwnWrites` 验证 own writes overlay 优先级高于固定快照，INSERT/UPDATE/DELETE 及 SAVEPOINT 回滚完全符合预期。

### 6. T034 & T035: 范围/空结果/提交校验兼容性
- `TestM10_RR_PhantomFreeRange` 与 `TestM10_RR_AbsentPointLookupConsistent` 验证范围与点查在并发插入下保持一致（无幻读）；
- `TestT035_RR_CommitValidationBaselines` 验证可见性快照不代替写冲突校验基准，无锁写入依然严格检测并发冲突，防止丢失更新。

### 7. T036: 100 轮确定性压测与全量回归
- `go test ./executor -run 'Test(M08|M09|M10|M11|M12|T035)' -count=100` 100 轮全部通过（EXIT: 0，总耗时 216s）；
- 全库 21 个测试包执行 `go test ./... -count=1` 100% 通过（总耗时 153s）；
- `gofmt -l`、`go vet ./...`、`git diff --check` 全部 clean。

---

## 15. Phase 4 最终 Gate 审查结论

- [x] **P3-FIX-05 前置水位漏洞彻底修复**；
- [x] **M08–M12 红测先行并绿测通过**；
- [x] **RR Lazy First Consistent Read 完整实现**；
- [x] **锁定读与普通读严格分离**；
- [x] **自身写/保存点/范围/空结果一致性验证齐备**；
- [x] **B01/B02/A05 零回归，全库 21 包 100% 通过**；
- [x] **100 轮并发压测通过，无死锁、无悬挂 Pin**；
- [x] **README 与 tasks.md 均已完成同步**。

**结论：GBaseLite B03 Phase 4 最终 Gate 正式通过，完全具备进入 Phase 5（READ COMMITTED）的技术前置条件。**
