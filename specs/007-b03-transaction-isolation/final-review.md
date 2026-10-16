# GBaseLite B03 — Phase 9 Final Review & Release Candidate Audit Report

- **Feature Specification**: `specs/007-b03-transaction-isolation` (B03)
- **Phase**: Phase 9: Documentation, Traceability & RC Gate (T077–T083)
- **Status**: **Release Candidate Ready (待用户最终授权)**
- **Branch**: `master`
- **Git Baseline HEAD**: `87192c505830603e4da4b5d433d24ad0ffed5f73`
- **Date**: 2026-10-16

---

## 1. 真实支持矩阵 (Genuine Support Matrix)

GBaseLite B03 实现了完整的 SQL 事务隔离级别状态机、多版本并发控制（MVCC）及行级锁管理器：

| 领域 / 功能项 | 支持状态 | 行为与机制说明 | 验证测试与依据 |
|---|---|---|---|
| **REPEATABLE READ (RR)** | **完整支持** (默认) | 首次普通 SELECT 确立快照 `ViewToken` 并复用；自身写优先可见；锁定读为 Current Read 并加锁，不刷新普通一致快照。 | `TestM08_RR_LazyFirstReadTiming` .. `TestM12_RR_CurrentReadSeparation` (M08–M12) |
| **READ COMMITTED (RC)** | **完整支持** | 每条语句分配全新一致快照 `StatementView`；单语句内（含聚合、子查询、JOIN）共享原子快照；锁保护下的写基于最新已提交版本递增更新；SAVEPOINT 撤回不冻结快照。 | `TestM13_RC_NonRepeatableRead` .. `TestM20_RC_SavepointDoesNotFreezeView` (M13–M20) |
| **SERIALIZABLE (SER)** | **受控安全支持** | 长事务普通 SELECT 隐式获取共享锁（IS/S/Next-Key/Gap），防止 Write Skew 与 Phantom；单语句 `autocommit=1` 只读快速路径免锁；复杂无谓词形态直接 Fail-Closed 拒绝报错 3108。 | `TestM21_SER_SSCompatibility` .. `TestM28_SER_AutocommitOptimization` (M21–M28) |
| **READ UNCOMMITTED (RU)** | **单机完整支持** | 单机模式支持读取未提交阶段性脏数据（通过内存态 `DirtyRegistry` 原子发布与清理）；写操作强制申请 X 锁校验已提交基线，**严格杜绝脏写与丢失更新**。 | `TestM29_RU_DirtyUpdate` .. `TestM35_RU_RollbackVsReaderConcurrency` (M29–M35) |
| **作用域状态机 (Scopes)** | **完整支持** | `SET [SESSION] ...` 修改会话默认；`SET TRANSACTION ...` 修改下一事务（pendingNext）；事务真正启动时消费并回退；活跃事务内修改报错拒绝。 | `TestB03SessionIsolationDefault` .. `TestB03IsolationAutocommit` (M01–M04) |
| **协议与内省兼容** | **完整支持** | 支持 `COM_RESET_CONNECTION (0x1f)` 二进制重置；支持 Prepared Statement；支持标准变量 `@@transaction_isolation` 与 5.7 遗留别名 `@@tx_isolation`。 | `TestB03IsolationCOMResetConnectionProtocol`, `TestB03IsolationVariables` (M05, M07, M44) |
| **锁管理器与死锁消解** | **完整支持** | 支持 IS/IX、S/X、Gap、Next-Key、Insert Intention；`innodb_lock_wait_timeout` 超时（Error 1205）；闭环等待图周期检测自动回滚受害者（Error 1213）。 | `TestM26_SER_DualSXUpgradeDeadlock`, `lockmanager/*` |
| **多版本 GC 与恢复栅栏** | **完整支持** | 活跃 `ViewToken` 严格 Pin 住底层版本阻止 `CompactHistory` 误回收；Restore 递增 Store Generation 强刷旧代次持锁与视图。 | `TestViewToken_GCAndCompactHistory`, `TestViewToken_EpochResetOnRestore` (M37–M38) |

---

## 2. 已知架构差异与 Fail-Closed 清单 (Disclosures)

为确保工程严谨性，**GBaseLite 绝不宣传为完全兼容 MySQL 8.4**，以下为已披露的架构差异与安全拒绝清单：

### 2.1 已知架构差异 (KNOWN DIFFERENCE)
1. **M06 (SET GLOBAL 拒绝)**:
   - GBaseLite 显式拒绝 `SET GLOBAL TRANSACTION ISOLATION LEVEL`（返回 Error 1064）；
   - **设计理由**：GBaseLite 定位为嵌入式或轻量级多租户服务，杜绝单连接意外修改全局配置影响同一进程内其他独立业务会话；
2. **M44 (5.7 遗留别名兼容支持)**:
   - GBaseLite 支持 `SELECT @@tx_isolation` 并返回当前隔离级别；
   - **设计理由**：向后兼容未适配 MySQL 8.0 的旧版 JDBC / ORM 驱动（MySQL 8.0+ 官方已移除该变量）。

### 2.2 架构 Fail-Closed 拒绝清单 (UNSUPPORTED)
1. **M27 (SERIALIZABLE 复杂形态拒绝)**:
   - 在 SERIALIZABLE 级别下，多表 JOIN、CTE、派生表、视图、集合 UNION、聚合查询直接报错 Error 3108（`this statement shape does not support row locking yet`）；
   - **设计理由**：保守的 Fail-Closed 策略，杜绝因范围谓词无法锁定而造成幻读；
2. **M36 (Raft 复制模式禁用 RU)**:
   - 在三节点 Raft 复制集群模式下，显式执行 `SET SESSION TRANSACTION ISOLATION LEVEL READ UNCOMMITTED` 返回 `storageengine.ErrUnsupported`；
   - **设计理由**：未共识的脏数据无法跨副本同步，为防止副本状态发散，强制禁止集群脏读。

---

## 3. 全量 CI 运行证据 (GitHub Actions 真实数据)

基线提交 Commit `87192c505830603e4da4b5d433d24ad0ffed5f73` 的远程自动化构建与测试结果：

- **Test 工作流**: [Run 38876080043](https://github.com/pucj0/gbaselite/actions/runs/38876080043) (**100% SUCCESS**)
  - `build (ubuntu-latest, linux, gbaselite)`: **SUCCESS**
  - `build (windows-latest, windows, gbaselite.exe)`: **SUCCESS**
  - `quality (ubuntu-latest)` (Job: `116891346299`): **SUCCESS**
    - Step 7 `Failover race regression`: **SUCCESS** (10 轮 race 全部通过)
    - Step 8 `MVCC transaction race regression`: **SUCCESS**
    - Step 9 `Lock manager race regression`: **SUCCESS** (2 轮 race 全部通过)
  - `quality (windows-latest)` (Job: `116891345896`): **SUCCESS**
  - `mysql-8-client` (Job: `116894600583`): **SUCCESS**
  - `mysql-84-parity` (Job: `116894600579`): **SUCCESS**
    - Step 5 `Run MySQL 8.4 M01-M44 Parity Suite`: **SUCCESS**（反向注入失败校验与 44 项全量对拍全部通过）
    - Step 6 `Upload Parity Report Artifact`: **SUCCESS**（产物 `b03-parity-report`）
- **Docker 构建工作流**: [Run 38876080024](https://github.com/pucj0/gbaselite/actions/runs/38876080024) (**SUCCESS**)

---

## 4. 本地工程质量与回归验证

| 验证项 | 执行命令 | 退出码 | 结果详情 |
|---|---|---|---|
| 代码格式规范 | `gofmt -l scripts/ .` | 0 | 100% clean，无任何代码格式差异 |
| 静态分析检查 | `go vet ./...` | 0 | 0 warnings, 0 errors |
| 补丁白名单检查 | `git diff --check` | 0 | 0 whitespace or merge conflicts |
| 100 轮确定性并发压测 | `go test ./mvcc -run ^TestP7R2_ScenarioA_DelayedLeaderLossCannotDisruptNewTerm$ -count=100` | 0 | **100/100 轮全部通过** (6.57s)，零锁残留，零悬挂边 |
| 历史兼容 Fixture | `go test ./mvcc -run ^TestT068GenuineB01FixtureOpenAndRestoreWithLockInvalidation$ -count=10` | 0 | **10/10 轮全部通过** (1.56s)，确证未修改底层磁盘与 WAL 格式 |
| 生产目录安全检查 | `git status -- data/` | 0 | **生产 `data/` 目录完全保持原始状态，零触碰，零污染** |

---

## 5. 任务清单完成状态 (T001–T083 100% Complete)

- **Phase 1: Setup & Architectural Proof (T001–T008)**: 8/8 Completed [x]
- **Phase 2: SQL Grammar & Session State (T009–T018)**: 10/10 Completed [x]
- **Phase 3: Read View Foundation & GC (T019–T029)**: 11/11 Completed [x]
- **Phase 4: REPEATABLE READ (T030–T036)**: 7/7 Completed [x]
- **Phase 5: READ COMMITTED (T037–T046)**: 10/10 Completed [x]
- **Phase 6: SERIALIZABLE (T047–T056)**: 10/10 Completed [x]
- **Phase 7: READ UNCOMMITTED (T057–T065)**: 9/9 Completed [x]
- **Phase 8: End-to-End, Replication & MySQL Parity (T066–T076)**: 11/11 Completed [x]
- **Phase 9: Documentation, Traceability & RC Gate (T077–T083)**: 7/7 Completed [x]

---

## 6. 残余风险与 Release Candidate 资格审查

### 6.1 残余风险评估
1. **SERIALIZABLE 复杂查询**: 业务若在 SERIALIZABLE 级别执行多表 JOIN、聚合或视图查询，将收到 Error 3108。**规避建议**：对此类分析型只读查询，显式设置会话隔离级别为 REPEATABLE READ 或 READ COMMITTED；
2. **长事务内存配额**: 若事务在 RU 模式下写入超过 10,000 键或 16MB 暂存数据，将收到 `storageengine.ErrViewBudgetLimit`。**规避建议**：拆分大批量写入或在 RC/RR 下执行；
3. **复制模式限制**: 集群模式禁用 RU，客户端需配置默认 RR 或 RC。

### 6.2 Release Candidate 资格审查结论
- **功能完整性**: 36 项功能需求（FR-001–036）全部具备完备的代码实现与测试覆盖；
- **验收合格性**: 12 项成功标准（SC-001–012）全部真实达成；
- **测试真实性**: 双端对拍与内部架构测试分别审计，所有用例均由真实网络与 Go 子测试执行产生；
- **门禁合规性**: 遵守门禁限制，未自动打 Tag、Release、替换 bin 或部署。

**最终结论**: **GBaseLite B03 满足 Release Candidate 资格，正式进入待发布候选状态，停止自动操作，等待用户最终审查授权。**
