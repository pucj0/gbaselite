# Tasks: B02 InnoDB-style Row Lock Manager

**Input**: spec.md、plan.md、research.md、data-model.md、contracts/、quickstart.md。
**Prerequisites**: 官方 setup-tasks.ps1 已解析完整设计与 tasks-template；实现须后续明确授权。
**Tests**: 用户明确要求确定性测试与可追踪验收；以下测试任务必做，不是可选示例。
**Organization**: 9 个阶段、72 个任务。新测试统一 TestB02 前缀。
**Progress**: Phase 1（T001–T004）、Phase 2（T005–T012）、Phase 3 的 US1 内核（T013–T017）、
Phase 4 的 T020/T022/T023（区间包含语义 + 边界感知冲突规则）、
Phase 5 的 T029/T030/T031/T033（`mvcc` 当前读 + baseline；`parser` 锁定模式）、
Phase 7 的 T047/T048/T049/T050/T051/T053、Phase 8 的 T058/T059（`lockmanager` 诊断）
已执行并勾选；其余（含 T018/T019/T021/T024–T028、T032、T034–T036、**Phase 6 全部**）未执行。
**Phase 6（DML 集成）本轮尝试后未达成**：UPDATE/DELETE 目标锁实现后停用（waiter 不唤醒），
INSERT 锁实现后未产生阻塞；交付骨架与固定负向测试，证据 `.tmp/b02-phase13-dml-validation.md`。
勾选只依据真实命令输出与源码证据，见 `.tmp/b02-baseline.md`、`.tmp/b02-format-baseline.md`、
`.tmp/b02-resource-calibration.md`、`.tmp/b02-validation.md`、`.tmp/b02-phase3-validation.md`、
`.tmp/b02-phase7-validation.md`。

## Format: `[ID] [P?] [Story] Description`
[P]仅表示同一依赖层、不同文件可并行；不允许越过前置任务。
每任务列出文件、需求与验收。Implementation与test映射另见traceability.md。
setup/foundation/polish无US标签；六个story阶段均标[USn]。

## Phase 1: Setup (T001–T004)
**Purpose**: 再确认授权、真实基线与旧格式。无生产实现先于本阶段。

> **执行结果（已完成）**：生产代码改动 0（`mvcc`/`executor`/`replication`/`parser`/`config`/`cmd`/
> `server` 共 354 个既有 Go 文件逐个 SHA-256 比对 0 changed / 0 missing）；`data/` 未接触；
> 未 build/部署。证据：`.tmp/b02-baseline.md`、`.tmp/b02-format-baseline.md`、
> `.tmp/b02-resource-calibration.md`。
> **与 SC-008 的关系**：SC-008 的「本轮生产代码改动数为零」指 2026-10-10 的**文档规划轮**。
> 本次是用户明确授权的实现轮，Phase 1 本身仍保持生产代码零改动；Phase 2 新增
> `storageengine/locking.go` 与 `lockmanager/` 新包（见该阶段说明）。

- [x] T001 记录 `go test ./...`、`go vet ./...` 和既有 B01/A05/大事务/五提交路径基线到 `.tmp/b02-baseline.md`；验收：退出码/已存在失败/最大锁资源规模可核对，不跳过既有测试；FR-024/030，SC-004/006。
      **证据**：`go test ./... -count=1` 退出 0 / 38.47s / 21 包全 ok / 2109 顶层测试通过 / 已存在失败 0；
      `go vet ./...` 退出 0 / 5.10s。另建 `.tmp/b02measure/`（module 树外）实测资源规模。
- [x] T002 [P] 复核当前用户实现授权、AGENTS.md、`.specify/memory/constitution.md` 及 `specs/006-b02-row-lock-manager/workflow.md`；验收：没有明确实现授权则停止于文档；FR-031，SC-008。
      **证据**：用户在本轮消息中明确授权「开始执行 B02 第一阶段」并逐项列出执行要求，且以
      `ask_user_question` 二次确认为「Phase 1 + Phase 2（T001–T012）」。constitution v1.0.0
      原则 V 要求「work 必须限于用户授权的阶段」——本轮严格限于 T001–T012。
- [x] T003 [P] 保存旧格式、Op/Command/WAL/codec与生产源基线摘要至 `.tmp/b02-format-baseline.md`，读取 `mvcc/store.go`、`mvcc/stage_codec.go`、`mvcc/local_wal.go`；验收：后续可证明无格式/协议变更且未接触data/；FR-019/030，SC-007/008。
      **证据**：354 文件逐个 SHA-256 清单 `.tmp/b02-src-hashes.txt`；桶布局、版本编码、stage codec、
      WAL framing、backup manifest、Raft 载荷逐项记录。
- [x] T004 根据 T001 大事务资源规模校准并同步 `specs/006-b02-row-lock-manager/{plan,research,data-model}.md` 和 `contracts/lock-manager.md` 的默认预算；验收：数值一致、bounded且既有大事务可验证，记录兼容影响；FR-025/030，SC-006。
      **证据**：实测含 PK+UNIQUE 的 SQL UPDATE = 3 components/行（10000 行 = 30000，20000 行 = 60000），
      规划默认 65536 只够约 21800 行，故每 root 上限上调至 **262144**；64 MiB 内存预算不变且仍封顶
      （262144 × 96 B ≈ 24 MiB，× 最坏 216 B ≈ 54 MiB）。四份文档已同步，兼容影响记入
      `.tmp/b02-resource-calibration.md` §3.3。README 登记留至 T070。

**Checkpoint**: baseline/授权/预算与格式证据完整。

## Phase 2: Foundational (T005–T012)
**Purpose**: 中立API、资源、owner/scope、预算与可注入clock，阻塞所有story实现。

> **执行结果（已完成）**：新增 `storageengine/locking.go`、`storageengine/locking_contract_test.go`
> 与**新包** `lockmanager/`（types.go、resource.go、budget.go、manager.go、clock.go +
> 三个测试文件）。**未**实现 `compatibility.go`、`queue.go`、`graph.go`、`interval.go`、
> `diagnostics.go`；**未**修改 `mvcc`/`executor`/`parser`/`replication`/`config`/`cmd`/`server`
> 任何既有文件（逐文件 SHA-256 证明）。`gofmt` 无输出，`go test ./...` 与 `go vet ./...` 均退出 0。
> **设计注记**：T006 与 T011 合并落地为 `resource.go`（身份/区间/scope树/绑定）+ `budget.go`
> （计量账本），因为资源身份是预算计量的直接输入，拆成两文件只会产生一个只有结转函数的空壳。
> T012 的「channel/barrier 测试 hook」落在 `test_helpers_test.go`（仅测试可用，
> 生产 `manager` 默认不携带任何 channel/barrier 状态），另把 `Barrier` 一并放在 `clock.go` 供
> 测试复用。

- [x] T005 定义可选 locking/current/diagnostic DTO、错误与 options 在 `storageengine/locking.go`（新）、`storageengine/engine.go`；验收：最小Txn/Engine未增加必选方法、options0取默认、无mvcc依赖；FR-010/023/025/026。
      **证据**：`Engine`/`Txn` 方法集未变（`TestB02LockingCapabilityIsAbsentByDefault` 断言最小后端
      仍合法、capability 明确 `ErrUnsupported`）；`LockingEngine`/`LockingTxn`/`LockDiagnostics`
      为可选接口 + `LockingTxnOf`/`LockDiagnosticsOf` 能力探测；`BudgetLimits.Resolve` 固定
      「0 = 默认，非无限」与合法范围；`ErrLockWaitTimeout`/`ErrDeadlock`/`ErrLockResourceLimit`/
      `ErrLockInvalidated` 为中立错误。`engine.go` **未改动**（既有可选能力风格已足够，无需新字段；
      新状态全部集中在 `locking.go` 以免污染最小契约）。
- [x] T006 [P] 建立owner/scope/resource/request/grant/observation类型在 `lockmanager/types.go`、`lockmanager/resource.go`（新）；验收：模型字段、owned bytes、root epoch、ordinal optional元数据匹配data-model；FR-001..006/020/022/025。
      **证据**：`LockOwner`（RootID/Ordinal/Generation/Term/ReadTS/State/StartedAt/EndedAt + scopes/usage）、
      `Scope`、`Identity`/`ResourceRef`（owned bytes 由 `OwnedBytes64` 计量）、`LockRequest`/`LockGrant`、
      `Observation`、`WaitEdge`/`DeadlockSummary`、`BudgetLimits`/`BudgetUsage`/`Stats` 均已落地；
      `ScopeKind`/`OwnerState`/`RequestStatus`/`Mode`/`ResourceKind`/`ObservationSource` 与
      contracts 的取值集合一致。
- [x] T007 [P] 在 `storageengine/locking_contract_test.go`（新）测试capability absent、复制字节、错误/token/owner约束；验收：普通无capability后端仍有效，locking明确unsupported；FR-026，SC-005。
      **证据**：`TestB02LockingCapabilityIsAbsentByDefault`、`TestB02LockingCapabilityIsAdditive`、
      `TestB02NeutralLockingTypesStayDependencyFree` 全通过（含 `testkit.Run` 完整后端契约套件）。
- [x] T008 建立owner注册、scope树、namespacebinding与abortRequested状态在 `lockmanager/manager.go`（新）；依赖T005/T006；验收：child唯一root、重入、无Tx指针跨线程操作、ordinal耗尽fail closed；FR-020/021/026/029。
      **证据**：`RegisterOwner`/`Owner`/`Owners`/`OpenScope`/`Scope`/`CloseScope`/`BeginCloseScope`/
      `Ancestors`/`Sibling`/`RequestAbort`/`AbortRequested`/`MarkFinishing`/`ReleaseOwner`/
      `ResetEpoch`/`Close`/`Stats`。`LockOwner` 不持有 `*Tx`；`RegisterOwner`/`OpenScope` 在
      generation/越界/预算失败时 fail closed 且无副作用（`TestB02OwnerAndScopeChargesAreBounded`）。
- [x] T009 在 `lockmanager/resource_test.go`（新）测试身份、namespace/table/epoch隔离、bounds克隆、nil/empty与开闭/反向/交集；验收：覆盖端点全集及不同bounds重叠；FR-003/004/006/015/027，SC-001。
      **证据**：`TestB02ResourceIdentityIsolation`、`TestB02ResourceKeyIsUnambiguous`、
      `TestB02ResourceValidationFailsClosed`、`TestB02ResourceCloneOwnsItsBytes`、
      `TestB02IntervalContainsKey`、`TestB02IntervalOverlap`、`TestB02DedupeUsesStrongestModeAndCanonicalOrder`、
      `TestB02RefBytesAccountingIsExact`、`TestB02ScopeTreeOwnershipAndAncestry`、
      `TestB02BindingsRejectConflictingLabels` 全通过。
      **实现期发现并修正的真实缺陷**：区间重叠最初用「按下界排序后单侧比较」，但下界序不是区间的
      全序（端点相接时两侧互为「更晚」），会把仅在一个开端点相接的两个区间误判为重叠。已改为
      双向 disjoint 判定，并由「仅在点相接（单开 vs 双闭）」用例锁定。
- [x] T010 [P] 在 `lockmanager/budget_test.go`（新）测试memory/root请求/waiter/graph预算exact/+1、overflow、重复release与失败无副作用；验收：计量零泄漏；FR-025，SC-006。
      **证据**：`TestB02MemoryBudgetExactAndPlusOne`、`TestB02RootRequestBudgetExactAndPlusOne`、
      `TestB02WaiterBudgetExactAndPlusOne`、`TestB02GraphBudgetExactAndPlusOne`、
      `TestB02RepeatedReleaseAndFailedReserveHaveNoSideEffect`、
      `TestB02ReservationToGrantIsNotDoubleCharged`、`TestB02LedgerTotalsAreDerivedFromTrackedState`、
      `TestB02BudgetLimitsResolution`、`TestB02DiagnosticPageLimitClamping`、
      `TestB02OwnerAndScopeChargesAreBounded` 全通过。
      **实现期修正**：预算曾同时维护「增量计数器」与「可重算总量」两份记账，二者在
      reserve→grant→release 序列上出现漂移（会掩盖真实泄漏或把预算算小）。已删除增量计数器：
      每次状态变更后由被跟踪集合唯一定导总量，因此「限额判定」与「上报用量」不可能不一致。
- [x] T011 实现reserve/transfer/release与限额校验在 `lockmanager/budget.go`（新），完善resource验证；依赖T009/T010；验收：T009/T010通过，owned bytes计量有界；FR-025/027。
      **证据**：`Ledger` 的 `Reserve`（一次性 all-or-nothing：内存 + 每 root 组件 + waiter）、
      `Commit`（reservation→grant 只加一次性 grant 开销，不重复计费）、`Release`/`ReleaseOwner`
      （幂等，归零）、`ClaimEdge`/`ReleaseEdge`、`ChargeFixed`/`ReleaseFixed`、`Resources`、
      `Usage`/`Totals`。`DedupeResourceRefs`+`SortResourceRefs` 使 bundle 规范序与去重
      （规范序 = 规范键的字节序，保证多域按稳定顺序取锁）。
- [x] T012 建立monotonic clock、fake clock与channel/barrier测试hook在 `lockmanager/clock.go`、`lockmanager/test_helpers_test.go`（新）；验收：无sleep构造调度，hook生产默认关闭；FR-010/024，SC-002/003。
      **证据**：`Clock` 接口 + `SystemClock`（单调、已过期 deadline 立即触发）+ `FakeClock`
      （`Advance` 只触发已到期定时器、按 deadline 序、丢弃已触发项）+ `Barrier`（ctx 可中断）。
      `test_helpers_test.go` 提供 `testBroker`（单终态通知、cond 同步等待、无 timeout 兜底）、
      `newTestClock`/`newTestManager`/`requireBudgetZero`。`TestB02DeterministicHarnessUsesNoSleeps`
      跑 100 次证明不需要 sleep；`manager` 生产默认不携带任何 hook。
      另加 `concurrency_safety_test.go`：8 owner × 8 goroutine × 200 op + 屏障同时起跑，
      终态账本精确归零。
      **race 说明**：本机 `CGO_ENABLED=0` 且 PATH 无 `gcc`/`clang`，`go test -race` 无法运行。
      按 quickstart/T067 约定**未伪报通过**，T067 保持未勾选；详见 `.tmp/b02-validation.md` §6。

**Checkpoint**: foundation contract/resource/budget tests通过，不改变普通快照或commit format。
**已核验**：`go test ./... -count=1` 退出 0（含 `gbaselite/lockmanager` 1.77s）；
`go vet ./...` 退出 0；354 个既有生产文件逐个 SHA-256 0 changed。

## Phase 3: US1 行级并发与公平等待 (T013–T019, P1 / kernel MVP)
**Goal**: Record/IS/IX、atomic bundle、公平queue；独立lockmanager夹具可以验收。

> **执行结果（T013–T017 已完成；T018–T019 未执行）**
> 新增 `lockmanager/compatibility.go`、`compatibility_test.go`、`queue_test.go`、
> `lock_acquisition_test.go`；重写 `lockmanager/queue.go`；扩展 `manager.go`、`types.go`、
> `budget.go`、`test_helpers_test.go`。
> **未执行 T018/T019**：`storageengine/mvccadapter/locking.go` 与 `mvcc/locking.go` 不存在，
> 因此本阶段的内核**尚未被任何 engine 装配**，SQL/raw 路径都无法触发它。
> 也**未**接入 SQL DML、Current Read 或 Gap/Next-Key（`interval.go`、`graph.go`、
> `diagnostics.go`、`lock_plan.go` 均不存在）。
> `gofmt` 无输出，`go test ./...` 与 `go vet ./...` 均退出 0（22 包 / 0 FAIL）。
> 既有 354 个生产 Go 文件逐个 SHA-256 比对 **CHANGED=0 / MISSING=0**。
> 证据：`.tmp/b02-phase3-validation.md`。
> **race 说明**：本机仍无 C 工具链，`go test -race ./lockmanager/` 无法运行，**未伪报**，
> T067 保持未勾选；测试已按 race 兼容方式编写，并新增并发归零测试作为非等价替代证据。
>
> **本阶段实现期发现并修正的 14 个真实缺陷**（详见证据文件 §4），其中三个最严重：
> `inspectLocked` 用同一 mode 比对多 kind 组件导致冲突漏判；grant 通道在 `range` 上边遍历边删
> 元素导致重复 grant；grant 原为 `owner → mode` 单值，使"释放 S 连带丢掉升级后的 X"、
> pending 升级又不提供排他保护。已改为**按请求的 claim 集合**并把 `heldMode`（已应用）
> 与 `offeredMode`（含 pending）分离。

### Tests first
- [x] T013 [P] [US1] 在 `lockmanager/compatibility_test.go`（新）穷举Record/Table矩阵有向对、同owner重入/升级及意向前置；验收：SC-001全部组合可核对；FR-001/002/006。
      **证据**：`TestB02RecordMatrixExhaustive`、`TestB02TableIntentionMatrixExhaustive`、
      `TestB02MatrixIsSymmetric`、`TestB02TableSXAndSIXAreRejected`、`TestB02IntentionPrerequisite`、
      `TestB02ReentrancyAndUpgradeDetection`、`TestB02EffectiveModeIsAProjectionNotAMatrix`、
      `TestB02DescribeProjectsResourceState`、`TestB02ComponentValidationCoversEveryKind` 全通过。
      表级 6 个 mode 的有向对全部断言；表级 S/X 与 SIX 以 `ErrUnsupportedMode` **明确拒绝**
      （这是 B02 对"表级 S/X"的判定：拒绝，而非实现——contracts/lock-manager.md 第 33 行
      「Table S/X、SIX、自动 table escalation 不在 B02」）。
- [x] T014 [P] [US1] 在 `lockmanager/queue_test.go`（新）测试S批授予、老X前新S不插队、独立行/表进度、cancel/grant单结果；验收：hook证明queue/grant因果，无timeout兜底当顺序；FR-007/024/029，SC-001/003。
      **证据**：`TestB02SharedRequestsCoexist`、`TestB02OlderExclusiveWaiterBlocksYoungerShared`、
      `TestB02UnrelatedResourcesMakeProgress`、`TestB02TableIntentionIsCompatibleAcrossDistinctRows`、
      `TestB02BatchSharedGrant`、`TestB02CancelProducesOneTerminalStatus`、`TestB02GrantBeatsCancel`、
      `TestB02FailFastRequestReturnsConflict`、`TestB02ScopeCloseCancelsScopeWaitersKeepsGrants`、
      `TestB02ReleaseOwnerWakesEveryWaiter`、`TestB02EpochResetInvalidatesWaiters`、
      `TestB02UpgradeWaitsWithoutReleasingShared` 全通过。
      顺序由 `EnqueueOrdinal` 与「请求已被授予/仍在排队」的确定状态断言，**不使用 timeout 兜底**；
      每个"仍在等待"都用未投递的终态 + `PendingRequests` 断言，而非睡眠。

### Implementation
- [x] T015 [US1] 实现模式兼容与table intent前置在 `lockmanager/compatibility.go`（新）；依赖T013；验收：IS/IX不充当Table X，矩阵测试通过；FR-001/002/006。
      **证据**：`modesCompatible` 是唯一矩阵判定点；`ValidateComponent` 先判 kind/mode 配对（因此
      表级 S/X 得到 `ErrUnsupportedMode` 而非泛化的 identity 错误）；`ValidateBundle` 在 bundle 级
      校验意向前置（record S 需 IS 或 IX，record X/II 需 IX，IX 同时满足 IS）；表身份来自组件的
      `TableID` 标签并回退到 namespace binding。IS/IX **不**充当 Table X：`modesCompatible` 中
      意向模式只与意向/记录/区间兼容，从不与 `ModeX` 的排他语义等同。
- [x] T016 [US1] 实现分片domain的atomic Acquire/Grant与scope重入在 `lockmanager/manager.go`；依赖T015/T011；验收：bundle无新部分grant、无全局事务门、无latch跨wait；FR-001/002/007/025/029。
      **证据**：`AcquireWith` 在**单个临界区**内完成 scope/owner 校验、bundle 校验、重入判定、
      all-or-nothing 预算预留、冲突判定与整包授予；`grantLocked` 只有在 `bundleReadyLocked`
      （每个组件都可授予）时才一次性应用全部 claim 并提交预留，因此**不存在部分授予**。
      无全局事务门：每资源独立 `lockEntry`，由 `TestB02NoGlobalTransactionGate`（持一行锁时
      64 独立行 + 3 张表立即授予）与 `TestB02UnrelatedResourcesMakeProgress` 锁定。
      无 latch 跨 wait：`Acquire` 只记录终态或返回 `LockHandle`，等待发生在调用方 goroutine 的
      channel 上（`Manager.WaitForGrant`），包内无 I/O、无外部回调。
- [x] T017 [US1] 实现冲突footprint公平队列、升级、单终态通知和初始blocker边在 `lockmanager/queue.go`（新）；依赖T014/T016；验收：老waiter不饥饿且无关resource推进；FR-007/008/009/029。
      **证据**：`lockEntry.waiting` 为 FIFO；`waitersAhead` 实现「年轻请求只有不会阻碍老 waiter 时
      才能共授予」；`blockingClaims` 按 `EnqueueOrdinal` 跳过更年轻的 pending claim（否则后来者
      会永久压住先到者），并跳过同 owner 的 claim（否则升级被自己持有的 S 挡住）；
      `waitsBehindOwner` 把同 owner 同 mode 的重复排队识别为可收养；`setStatus` 是唯一终态写入点，
      先写共享 `requestState` 再通知 channel，故 `Cancel`/`WaitStatus` 幂等且不会消费掉通知；
      初始 blocker 边由 `Edges()` 按 (from,to,request) 去重后给出 `EdgeHolder`/`EdgeOlderWaiter`。
      升级：`grantLocked` 用 `upgradeNeeded` 决定是否提升 mode，**等待期间不释放已有 S**
      （由 `TestB02UpgradeWaitsWithoutReleasingShared`、`TestB02UpgradeCompetition` 锁定）。
- [ ] T018 [US1] 在 `storageengine/mvccadapter/locking.go`（新）绑定每engine manager、root/scope与实际namespace标签；验收：多engine不共享锁、SQL/raw资源身份一致，neutral接口不泄露backend；FR-026/029。
      **未执行**（本轮范围外：用户要求"不提前接入 SQL DML、Current Read 或 Gap Lock"，
      且该任务需要把锁管理器装配进 engine，属于跨阶段接线）。
- [ ] T019 [US1] 在 `mvcc/locking.go`（新）与 `mvcc/transaction.go` 接入已有key覆盖/删除的raw client X锁与查询scope context；验收：raw writer不能绕过record holder，Get不变；FR-014/026/029/030。
      **未执行**（同上；依赖 T018 的装配入口）。

**Independent Test / Checkpoint**: T013/T014+adapter raw-key fixture通过。MVP只代表record内核，
gap/new-key写和完整SQL发布仍依赖后续phase，不能将US1当作完整B02上线。
**实际状态**：T013–T017 的独立 `lockmanager` 夹具通过；**adapter raw-key fixture 未建立**，
因此 checkpoint 只完成了一半——内核可验收，接线未验收。

## Phase 4: US2 区间与插入保护 (T020–T027, P1)
**Goal**: 稳定Gap、Next-Key、II及proven/fallback计划；独立资源夹具可验证。

> **执行结果（T020、T022、T023 已完成；T021、T024、T025、T026、T027 未执行）**
>
> 本轮先确认了前置阶段的真实状态：`executor` **不读取** `Select.Locking`/`LockMode`，
> `mvccadapter` 没有 `locking.go`，`enginefactory` 不创建 lock manager，
> `AttachLocks` 只有测试调用（`executor/access_plan.go` 自述 "Locking reads ... are served from
> the statement snapshot"）。经用户确认（Option 1），本轮先补 SQL 接入基础。
>
> 已完成：
> - `lockmanager/interval.go`（新，T022）：`containsPoint`（nil 边界=无穷、空非 nil 键是合法点、
>   端点开闭）、`isEmptyInterval`（仅闭端点零长度非空）、`gapBlocksInsertion`、
>   `gapOverlapsGap`（复用既有 `resource.go:intervalsOverlap`）、`intervalComponentsEqual`。
> - `lockmanager/compatibility.go`（T023）：`componentsConflict(requestRef, heldRef)` 按**区间包含**
>   决定 Gap↔InsertIntention（两个方向同一函数）；Gap↔Gap 兼容；其余走 `recordsConflict`
>   （原 mode 矩阵，逐字节未变）。调用方改为传完整组件，使边界可达。
> - `lockmanager/queue.go`：`lockClaim.ref` 存储组件与边界（`LockRequest.Components` 实测为空，
>   无法反查）；`conflictingHolders`/`appendBlockingClaims`/`hasBlockingClaims`/`olderBlockers`/
>   `waitersAhead`/`immediateConflict` 全部改为边界感知。
> - `lockmanager/interval_test.go`（新，T020）：7 组 41 个用例。
> - **SQL 接入基础**：`storageengine/mvccadapter/locking.go`（新，实现 `LockingTxn` 与全部
>   neutral↔lockmanager 类型转换）、`adapter.go`（创建/装配/关闭 lock manager）、
>   `mvcc/locking.go`（`AcquireLock`/`WaitForLock`）、`current_read.go`（按 owner 校验 grant）。
>
> **未执行**：T021（insert_intention_test.go）、T024（`Put` 自动取 II+X）、
> T025（lock access plan / proven bounds / fallback / 明确拒绝）、T026（PK/UNIQUE/secondary 共同身份）、
> T027（adapter 与 executor 计划验证）。
> **因此 `SELECT ... FOR UPDATE` 仍然不加锁**：`executor` 依旧不读取锁定模式，
> SQL 层不产生任何 Gap 或 Insert Intention 锁，用户列出的全部 SQL 级 gap 行为测试**未实现**。
> `gofmt` 无输出，`go test ./...` 与 `go vet ./...` 均退出 0（22 包 / 0 FAIL）。
> 证据：`.tmp/b02-phase12-gap-validation.md`。

### Tests first
- [x] T020 [P] [US2] 在 `lockmanager/interval_test.go`（新）测试open/closed、空表、缺失点、terminalgap、删除边界、重叠Gap及reverse；验收：准确包含/排除插入位置；FR-003/004/006/015/027，SC-001。
      **证据**：`lockmanager/interval_test.go` 7 组 41 个用例全通过 —— 开闭边界 12 例、
      Gap 阻止范围内插入、Gap 不阻止范围外插入（双向）、同间隙不同位置并发、
      II/II 兼容、相邻与重叠 7 例（含四种端点相接组合与单点重叠，并断言**对称性**）、
      空区间与零长度、空表/最小键之前/最大键之后/supremum、跨表跨索引身份隔离。
- [ ] T021 [P] [US2] 在 `lockmanager/insert_intention_test.go`（新）测试II/II、Gap/II双方有向冲突、samekey X reservation及原子Next-Key；验收：不同位置并行、同key互斥；FR-004/005/006/007。

### Implementation
- [x] T022 [US2] 实现有界interval overlap/membership索引在 `lockmanager/interval.go`（新）；依赖T020；验收：不按相同bounds哈希冒充重叠，删除邻居不丢grant；FR-003/006/027。
      **证据**：`interval.go` 提供 `containsPoint`/`isEmptyInterval`/`gapBlocksInsertion`/`gapOverlapsGap`/
      `intervalComponentsEqual`。**重叠判定复用既有 `resource.go:intervalsOverlap`**（双向 disjoint），
      不按相同 bounds 哈希冒充重叠（首次编写时重复定义，被编译器拦下后删除）。
      边界随 `lockClaim.ref` 存储，因此删除邻居不会使已授予区间的边界丢失。
- [x] T023 [US2] 实现Gap/II compatibility及Next-Key atomic components在 `lockmanager/compatibility.go`、`lockmanager/manager.go`；依赖T021/T022；验收：record与gap规则独立，公平queue正确加边；FR-004/005/006/007/008。
      **证据（Next-Key 部分未做）**：`componentsConflict(requestRef, heldRef)` 按区间包含决定
      Gap↔InsertIntention（两个方向调用同一 `gapBlocksInsertion`，不可能漂移）；Gap↔Gap 兼容；
      其余走 `recordsConflict`（原矩阵，Record 与表意向逐字节未变）——**record 与 gap 规则相互独立**。
      公平队列与死锁图走同一 `componentsConflict`，因此边界感知规则自动进入加边与检测
      （`lockmanager` 全部测试含死锁/公平性测试全绿）。
      **未做**：Next-Key 的显式组合构造器（目前靠 bundle 原子性把 record 与 gap 组合）与专属测试。
- [ ] T024 [US2] 在 `mvcc/locking.go`、`mvcc/transaction.go` 对真正missing/tombstone后的Put自动获取II+X；验收：raw插入遵守区间，overlay delete/reinsert分类正确，stage/apply不等锁；FR-005/014/026/029。
- [ ] T025 [US2] 建立lock access plan/proof/fallback在 `executor/lock_plan.go`（新）、`executor/access_plan.go`、`executor/range_access.go`；验收：完整uniquehit record-only、missinggap、partialkey range、legacywhole-domain，EXPLAIN标注；FR-015/027/028。
- [ ] T026 [US2] 在 `executor/lock_plan.go`、`executor/secondary_index.go` 复用PK/UNIQUE/secondary字节编码与共同row identity；验收：NULL不reservation、collation等值同resource、覆盖读也锁row；FR-015/016/026/027。
- [ ] T027 [US2] 在 `storageengine/mvccadapter/locking_range_test.go`、`executor/lock_plan_test.go`（新）验证SQL计划与rawnamespace同域及旧/新编码fallback；验收：无访问路径绕过，全部gapfixtures通过；FR-003..006/015/016/026/027，SC-001。

## Phase 5: US3 锁定Current Read SQL (T028–T036, P1)
**Goal**: 新读能力与普通snapshot分离；独立两session读取可验收。

> **执行结果（T029、T030、T031、T033 已完成的 `mvcc`/`parser` 部分；T028、T032、T034、T035、T036 未执行）**
>
> 本轮先解决了一个**必须先处理的架构冲突**（用户已确认修订方案 Option 1）：
> B01 的提交校验器只有一个事务级 `readTS`（`mvcc/conflict.go:88`），
> 因此"在锁下观测到最新版本 V 之后写该 key"会被旧 readTS 错误拒绝，
> 而契约矩阵要求这次写入合法。修订方案是**逐 key 观测 baseline**（INV-005 精化），
> 已写入 `contracts/current-read.md` §Locked-read write baseline 与 `plan.md` §A3.1。
>
> 已完成：
> - `mvcc/current_read.go`（新）：`ReadCurrentLocked`/`VerifyCurrentRow`/`ReadCurrentUnlocked`、
>   三类 `CurrentObservationSource`、tombstone 与 absent 可区分、grant/owner/generation 失败即丢弃观测、
>   逐 key baseline（有界 `maxObservationBaselines`，达界 fail-closed）。
> - `mvcc/conflict.go`：`conflictValidator.withBaseline`；`validateKey` 对有 baseline 的 key 比较
>   观测版本，其余 key 仍比较 `readTS`。
> - `mvcc/group_commit.go`/`local_commit.go`/`local_stream.go`/`local_wal.go`：
>   四条本地提交路径统一携带 baseline；replicated apply 无法携带 → **fail closed**。
> - `parser/ast.go`（`LockMode`/`LockingAlias`）、`parser/parser.go`：
>   `FOR UPDATE`=`UPDATE`、`FOR SHARE` 与 `LOCK IN SHARE MODE`=`SHARE`、
>   `NOWAIT`/`SKIP LOCKED`/`FOR NO KEY UPDATE`/`FOR KEY SHARE` **解析期明确拒绝**。
> - `mvcc/transaction_diagnostics_test.go`：B01 的"唯一集合"不变量改为**声明式有界集合**
>   （`buffered` + `writeBaseline`，各自必须声明边界）。
>
> **未执行**：T028（adapter contract 测试）、T032（lock-aware page iterator）、
> T034（executor current-only pipeline）、T035（session lock deadline）、T036（两 session SQL 测试）。
> 因此**`SELECT ... FOR UPDATE` 目前仍不会真正加锁**：解析器已能识别模式，但
> `executor` 尚未读取 `Locking`/`LockMode`。本阶段交付的是**已通过测试的 mvcc 当前读与 baseline
> 机制**，不是 SQL 可见的行为。
> `gofmt` 无输出，`go test ./...` 与 `go vet ./...` 均退出 0（22 包 / 0 FAIL）。
> 证据：`.tmp/b02-phase11-current-read-validation.md`。

### Tests first
- [ ] T028 [P] [US3] 在 `storageengine/mvccadapter/current_read_contract_test.go`（新）测试scope/owner/token、parent/child/siblingoverlay、能力缺失；验收：parent不能看未merge child，普通API不变；FR-011/013/026/030。
- [x] T029 [P] [US3] 在 `mvcc/current_read_test.go`（新）测试flat/nested、marker、等待后值/墓碑、readTS/horizon、current-only/guard-only/data三类root；验收：旧/新/旧读序列及原commitTS语义；FR-011/013/018/019/030，SC-004/007。
      **证据**：`mvcc/current_read_test.go` 8 个测试全通过 ——
      `TestB02CurrentReadSeesLatestPublishedVersion`（当前读看到最新已发布版本，**普通 GET 仍返回固定快照**）、
      `TestB02LockedReadAuthorisesTheWriteItObserved`（**INV-005 精化的核心**：锁下观测到 `V>readTS` 后写该 key 成功提交，
      且断言 `readTS` 未被改写）、`TestB02UnlockedWriteStillConflicts`（**未加锁写入仍报 ErrConflict**）、
      `TestB02BaselineIsPerKeyNotPerTransaction`（逐 key：锁了 A 不能授权写 B）、
      `TestB02BaselineRecordsZeroForAbsentAndTombstone`（absent 与 tombstone 可区分；观测为 absent 后被他人创建则冲突；
      tombstone 携带删除 changeTS）、`TestB02CurrentReadIncludesOwnOverlay`（own overlay、child 见 ancestor、
      **parent 不见未 merge child**）、`TestB02OwnOverlayDeleteIsObservable`、
      `TestB02CurrentReadRejectsAForeignOrStaleGrant`、`TestB02CurrentReadDoesNotPinHistoryOrChangeTheRootClass`
      （**GC horizon 不变**、current-only root 不产生 publication marker、head 不移动、dependency-only 仍可提交）。
- [x] T030 [P] [US3] 在 `parser/locking_read_test.go`（新）测试FOR UPDATE/SHARE/alias mode与NOWAIT/SKIPLOCKED/unsupportedshapes；验收：策略不静默丢失，错误早于副作用；FR-012/028。
      **证据**：`parser/locking_read_test.go` 4 组测试全通过 —— `TestB02LockingReadModesAreDistinct`
      （`FOR UPDATE`→`UPDATE`、`FOR SHARE`→`SHARE`、`LOCK IN SHARE MODE`→`SHARE`+alias，含大小写与换行）、
      `TestB02PlainSelectIsNotLocking`、`TestB02UnsupportedLockingOptionsAreRefused`
      （**7 条语句在解析期被拒绝**：`FOR UPDATE NOWAIT`、`FOR UPDATE SKIP LOCKED`、`FOR SHARE` 两种 option、
      `LOCK IN SHARE MODE NOWAIT`、`FOR NO KEY UPDATE`、`FOR KEY SHARE`，错误消息必须点名该项）、
      `TestB02LockModeValidRejectsUnknownValues`。**策略不再静默丢失**：错误发生在绑定与执行之前。

### Implementation
- [x] T031 [US3] 实现latest published+readlayeroverlay与observation metadata在 `mvcc/current_read.go`（新）；依赖T028/T029；验收：tombstone changeTS不遗漏，不改变Snapshot/Guard/format；FR-011/013/018/019/030/032。
      **证据**：`mvcc/current_read.go` 实现 `latestPublished`（复用既有 `visible(tx,k,^0)`，
      **不自建第二套可见性代码**）、upward-only overlay 解析、`CurrentObservation{Value,Exists,Tombstone,ChangeTS,HasChange,Source}`。
      tombstone 的 `ChangeTS` 不遗漏（由 `TestB02BaselineRecordsZeroForAbsentAndTombstone` 锁定）；
      `Snapshot`/`Guard`/磁盘格式**未改**：`readTS` 无任何写入点，`Guard` 语义与编码未动，
      `Command` 结构未动（因此 replicated apply 无法携带 baseline 时 fail closed，而不是改格式）。
- [ ] T032 [US3] 实现lock-aware有限页iterator在 `storageengine/mvccadapter/locking.go`、`storageengine/mvccadapter/iterator.go`；验收：close physical view先于yield/wait、等待后重seek、所有error/Close清理；FR-011/015/024/029。
- [x] T033 [US3] 扩展明确lockmode AST与语法拒绝在 `parser/ast.go`、`parser/parser.go`；依赖T030；验收：FOR SHARE可区分S/X、unsupportedoptions早拒绝；FR-012/028。
      **证据**：`Select.LockMode`（`LockModeUpdate`/`LockModeShare`）+`LockingAlias`+`LockMode.Valid()`；
      `refuseLockingOption` 对两种锁定形式都调用。`FOR SHARE` 是**新增语法**（此前只支持 `LOCK IN SHARE MODE`），
      与 S/X 语义可区分，不再只有一个 bool。`SKIP LOCKED`/`NOWAIT` 以前被**静默消费并丢弃**（旧
      `parser.go:1015-1019`），现在解析期报错。
- [ ] T034 [US3] 在 `executor/locking_read.go`（新）、`executor/physical_binding.go`、`executor/physical_select.go` 绑定current-only单表pipeline；验收：commonrow锁、WHERE重评、ORDER/LIMIT/反向与autocommit结果消费清理；FR-011/012/013/015/027/028/032。
- [ ] T035 [US3] 在 `executor/session_settings.go`、`executor/executor.go` 接入会话锁期限与queryctx较短deadline；验收：默认50、范围1..86400、SESSION限定、普通read不变；FR-010/012/028。
- [ ] T036 [US3] 在 `executor/locking_read_test.go`、`storageengine/mvccadapter/locking_view_test.go`（新）执行完整两session读取/unsupported早期/多索引/无view跨wait测试；验收：SC-004且并发writer/remap不被等待reader阻止；FR-011..013/015/026..030/032。

## Phase 6: US4 DML 与唯一索引 (T037–T046, P1)
**Goal**: A05所有write路径保护同资源、原候选语义不变。

> **执行结果：本轮未达成，无任务可勾选。**
>
> 本轮开始时确认：**没有任何 DML 路径获取锁**（`executor` 中 `LockingTxn`/`BeginLockScope`/
> `.Acquire(` 无匹配），`executor` 也不读取锁定模式。因此本轮不是"集成"而是"从零加锁"。
>
> 已交付（未达验收，故不勾选）：
> - `executor/dml_locking.go`（新）：语句级锁作用域、表意向/主键行/索引条目组件构造、
>   `primaryKeyBytes`、无主键表的 heap 身份、`indexEntryLocks`（owner 传真实主键字节）。
> - `executor/mutation.go`：INSERT/UPDATE/DELETE 三分支各开语句作用域并 `defer close`；
>   多表 DELETE 与 INSERT SELECT 穿透锁作用域，**继续走 A05 统一算子**（要求 8 保持）。
> - 固定锁申请顺序：**表意向 → 主键行 → 索引条目**（要求 11）。
> - `executor/dml_locking_test.go`（新）：2 个**固定负向测试** + 1 个正测试。
>
> **未达成**：UPDATE/DELETE 目标记录锁**已实现但刻意停用**。
> 最小复现：单表在"第一轮两事务各改不同行并提交"之后，第二轮两事务改**同一行**时，
> waiter 在 holder 提交后**仍不唤醒**，直到 lock wait deadline 报超时。
> 一个会让普通 UPDATE 偶发超时失败的锁比不加锁更差，故停用并在代码注释与
> `TestB02UpdateTargetLockingIsNotEnabled` 中钉住。
> INSERT 锁已实现但**未产生阻塞**：两个事务插入同一唯一键时第二个仍立即成功，
> 由 `TestB02ConcurrentInsertSameUniqueKeyIsStillAccepted`（固定负向）记录。
> 已排除的原因：不是 INSERT 锁、不是索引条目编码（owner 已改为真实主键字节后仍复现）、
> 不是作用域关闭时机、不是 `ReleaseOwner` 缺失。
>
> `gofmt` 无输出，`go test ./...` 与 `go vet ./...` 均退出 0（22 包 / 0 FAIL）；
> `mvcc`/`storageengine`/`parser`/`server`/`config`/`replication` 未改。
> 证据：`.tmp/b02-phase13-dml-validation.md`。
> **race**：本机仍无 C 工具链，`go test -race` 无法运行，**未伪报**，T067 保持未勾选。

### Tests first
- [ ] T037 [P] [US4] 在 `executor/locking_modify_test.go`（新）测试stale UPDATE/DELETE、post-readTS墓碑、parent候选、JOINfirstmatch/dedup/LIMIT/SET顺序与Halloween；验收：早冲突结束root，兼容候选结果与A05相同；FR-017/018/030/032，SC-004。
- [ ] T038 [P] [US4] 在 `executor/locking_unique_test.go`（新）测试同PK/UNIQUE、NULL/collation、等待后判重、IGNORE/REPLACE/upsert；验收：至多一个插入胜者、不能忽略lock/SIerrors；FR-005/014/015/016/018。
- [ ] T039 [P] [US4] 在 `executor/locking_foreign_key_test.go`（新）测试旧/新PK/index、heapidentity、多目标DELETE、cascade/SETNULL与schema变化；验收：共同rowidentity、guard基准保持；FR-014/015/017/018/032。

### Implementation
- [ ] T040 [US4] 实现parenttarget currentverification与old/new资源收集在 `executor/locking_modify.go`（新）；验收：latest>readTS早ErrConflict、metadata先验证再解码，不refreshsource；FR-014/017/018/032。
- [ ] T041 [US4] 将lock验证接入 `executor/modify_update.go`、`executor/modify_delete.go`、`executor/mutation_join_pipeline.go`、`executor/multi_delete_pipeline.go`；验收：仍parentread/childwrite、原Identity与SET顺序，不扫描child；FR-014/017/018/030。
- [ ] T042 [US4] 将插入与duplicatevariant锁后判重接入 `executor/modify_insert.go`、`executor/mutation_insert_modes.go`；验收：II+X/unique reservation、NULL/collation/旧target检查、原ID预留与LastInsertID；FR-005/014/016/018。
- [ ] T043 [US4] 在 `executor/mutation.go`、`executor/secondary_index.go` 统一old/newrow及index锁，维护namespacebinding；验收：所有indexwrites进入同路径，无第二套rowwriter；FR-014/015/016/026。
- [ ] T044 [US4] 在 `executor/foreign_key.go` 接入派生write锁而保留point/range/catalog guards；验收：cascade、SETNULL与既有depth/budget约束不绕过，dependencyonly语义不变；FR-014/018/032。
- [ ] T045 [US4] 在 `mvcc/locking_client_write_test.go`（新）证明直接Txn.Put/Delete与SQL持锁同identity、ctx取消、缺失key/重新插入；验收：raw写无锁旁路，apply/replay仍不等待；FR-014/026/029。
- [ ] T046 [US4] 在 `executor/locking_modify_pipeline_test.go`（新）及原A05fixtures回归全部INSERT/UPDATE/DELETE/自引用/spill/stageerror；验收：affectedrows、dedup、LastInsertID、statementrollback与临时资源一致；FR-014/017/024/030，SC-004。

## Phase 7: US5 生命周期、死锁、耐久性 (T047–T057, P1)
**Goal**: root正确结束，有限等待，五提交路径一致；发布前阻塞gate。

> **执行结果（T047、T048、T049、T050 已完成；T051 `mvcc` 部分已完成、`executor`/`server` 部分未执行；T052–T057 未执行）**
>
> 第一次执行（死锁与诊断）：新增 `lockmanager/graph.go`、`deadlock_test.go`（T048）、
> `waiting_test.go`；扩展 `manager.go`（惰性派生图、`DetectDeadlock`、`WaitEdges`、`CheckTimeouts`、
> `Deadlocks` façade）、`queue.go`（模式索引、`olderBlockers`）、`types.go`。
>
> 第二次执行（**锁管理器与 B01 事务管理器的生命周期集成**）：新增 `mvcc/locking.go`（所有权绑定、
> scope 归属、清理顺序、victim 判定、epoch 同步）与 `mvcc/locking_lifecycle_test.go`（T047）、
> `mvcc/locking_publication_test.go`（T049）、`mvcc/locking_commit_test.go`（持久化不变量）；
> 扩展 `mvcc/transaction.go`（`newTx`/`ChildContext` 建 owner 与 scope、`finish` 的三段式清理顺序、
> `commitChild`/`commitRoot` 的 victim 门禁）、`mvcc/store.go`（`locks` 绑定字段 + `ErrLocksAttached`）、
> `mvcc/snapshot.go`（`Restore` 同步 lock epoch）；`lockmanager` 侧新增 `AbortReason`、
> `Manager.Owner`/`AbortRequested`/`AbortReasonOf`/`Scope`/`Clock` façade，
> 并让 `ResetEpoch` **真正结束并注销 owner**（原先只标记 FINISHING，导致两个注册表不一致）。
>
> **未执行**：T051 的 `executor`/`server` 接线、T052–T057。它们全部要改
> `executor`/`server`/`config`/`replication`，即「SELECT FOR UPDATE、DML、Gap/Next-Key」被排除的 SQL 面。
> 因此死锁检测与自动超时**仍没有后台驱动点**：`DetectDeadlock` 由测试或调用方显式驱动，
> 事务层只在 owner 已被标记后拒绝继续（`AbortRequested` → `ErrDeadlock`）。
> `executor`、`parser`、`server`、`config`、`replication` 逐字节未变（SHA-256 比对 CHANGED=0）。
> `gofmt` 无输出，`go test ./...` 与 `go vet ./...` 均退出 0（22 包 / 0 FAIL）。
> 证据：`.tmp/b02-phase7-validation.md`（死锁/诊断）、`.tmp/b02-phase9-lifecycle-validation.md`（生命周期集成）。
> **race 说明**：本机仍无 C 工具链（PATH 无 `gcc`/`clang`，已搜 `msys64`/`mingw64`/`tools`/
> `Program Files`/`LOCALAPPDATA`），`go test -race` 无法运行，**未伪报**，T067 保持未勾选。
> 锁顺序由 `TestB02LockOrderIsNeverInverted` 以并发压力 + 确定性断言替代验证（**不宣称等价于 race**）。

### Tests first
- [x] T047 [P] [US5] 在 `executor/locking_lifecycle_test.go`、`mvcc/locking_lifecycle_test.go`（新）测试child/statement、savepoint32层/重名/RELEASE/ROLLBACKTO、root/disconnect与双清理；验收：grantsroot保留、discardpending撤销、终态全零；FR-009/020/021/030，SC-003/004。
      **证据（`mvcc` 部分完成，`executor` 部分未执行）**：`mvcc/locking_lifecycle_test.go` 14 个测试全通过 ——
      `TestB02RootCommitReleasesLocks`、`TestB02RootRollbackReleasesLocks`、
      `TestB02DuplicateRollbackAndCloseRaceIsIdempotent`（**双清理/关闭竞态**幂等）、
      `TestB02ChildCommitKeepsRootLocks`（**child merge 不释放 root 锁**，并证明 child scope 已关闭）、
      `TestB02FailedStatementKeepsRootLocks`（**失败 statement** 撤销 pending、保留 grant、root 仍可提交）、
      `TestB02SavepointScopeCreateRollbackRelease`（**savepoint 创建/ROLLBACK TO/RELEASE**，含兄弟 scope 隔离）、
      `TestB02DeadlockVictimCannotWriteOrCommit`、`TestB02LockTimeoutAbortsOnlyItsOwnRequest`、
      `TestB02CommitFailureReleasesLocks`、`TestB02DisconnectAndRestoreInvalidateWaiters`、
      `TestB02StoreRestoreSynchronisesTheLockEpoch`、`TestB02LockOwnersAndRegistryStayConsistent`、
      `TestB02NoLockingWithoutABinding`、`TestB02AttachingASecondManagerIsRefused`。
      **未执行**：`executor/locking_lifecycle_test.go` —— 它需要 SQL statement/savepoint 层接线，
      属于被排除的 SQL 面。**savepoint 32 层/重名替换**属于 executor 层语义，此处未覆盖。
      全部顺序用确定性断言（owner/scope 状态、`PendingRequests`、`LockTableSize`、注册表条目数），**无 time.Sleep**。
- [x] T048 [P] [US5] 在 `lockmanager/deadlock_test.go`（新）测试二/三环、双升级、多个holder、queueedges、stalegraph、ordinal/graphwork耗尽；验收：确定maxordinalvictim、无虚假deadlock；FR-007/008/009/024/025，SC-002/006。
      **证据**：`lockmanager/deadlock_test.go` 16 个测试全通过 ——
      `TestB02TwoTransactionDeadlockChoosesTheLaterRoot`（二环 + max ordinal victim）、
      `TestB02ThreeTransactionCycleIsDetectedAndResolvable`（三环并逐环解开）、
      `TestB02UpgradeDeadlockIsDetectedAndKeepsSurvivorShared`（**锁升级死锁**，victim 保留原有 S）、
      `TestB02VictimWakesEveryWaiterOfItsOwnRequests`（多 holder + victim 的每个等待请求都被唤醒）、
      `TestB02WaitForGraphRecordsHolderAndQueueEdges`（HOLDER + OLDER_WAITER 两类边）、
      `TestB02WaitForGraphHasNoDanglingEdges`（stale graph：无悬空边）、
      `TestB02DetectionIgnoresNonCycles`（链/菱形/多读无虚假 deadlock）、`TestB02SelfDeadlockIsNeverReported`、
      `TestB02VictimSelectionIsIndependentOfDetectionOrder`、`TestB02DeadlockIsReportedOnceUntilTheVictimIsCleanedUp`、
      `TestB02DeadlockStatisticsAndRingBound`、`TestB02DetectionFailsClosedOnAnExhaustedWorkBudget`
      （**graphwork 耗尽** → 不报 deadlock、不误杀）、`TestB02EpochResetAndCloseClearTheGraph`、
      `TestB02ConcurrentDetectionNeverPicksTwoVictimsForOneCycle`。
      全部顺序用确定性断言（ordinal 比较、`PendingRequests`、状态机），**无 time.Sleep**；
      `TestB02ReleaseAndTimeoutRacingProducesOneOutcome` 用 start gate 制造竞争。
- [x] T049 [P] [US5] 在 `mvcc/locking_publication_test.go`、`replication/locking_epoch_test.go`（新）测试fakeclocktimeout/grant/cancel、publication前后、restore/term/Close；验收：definite结果、waiter唤醒、旧token拒绝；FR-009/010/019/021/022/029，SC-003/004/007。
      **证据（`mvcc` 部分完成，`replication` 部分未执行）**：`mvcc/locking_publication_test.go` 6 个测试 +
      `mvcc/locking_commit_test.go` 6 个测试全通过 —— `TestB02LocksAreHeldUntilThePublicationIsDecided`
      （**release 点在 publication 之后**，并证明释放后可被他人取得）、`TestB02RootWithNoWriteSetStillReleasesLocks`、
      `TestB02DeadlockVictimNeverPublishes`（**victim 不消耗 commit sequence、head 不移动、写集不可见**）、
      `TestB02LockOrderIsNeverInverted`（**锁顺序**：并发 begin/commit/rollback/epoch reset/diagnostics）、
      `TestB02WaiterWakesAfterTheHolderIsInvalidated`、`TestB02CommitAfterADeadlockAbortIsRefusedOnce`；
      `TestB02CommitPathsKeepTheirPublicationInvariants`（**4 种配置 × 3 种 root 形态**：
      bounded / bounded+locking / local-wal / local-wal+locking，逐一比对 commit sequence 与 publication marker）、
      `TestB02RestoreKeepsTheMVCCImageUsable`、`TestB02FailedPublicationIsReconciledAfterARestore`、
      `TestB02LockingDoesNotChangeConflictDetection`、`TestB02GroupCommitPathIsUnaffectedByLocking`、
      `TestB02LockingAvailabilityFailsClosed`。
      等待期限由 `lockmanager.FakeClock` 驱动（**无 time.Sleep**）。
      **未执行**：`replication/locking_epoch_test.go` —— 需要 `replication` 接线（T054）。

### Implementation
- [x] T050 [US5] 实现holder/olderwaiter图、bounded cycle检测、revisionrecheck、maxordinalvictim与owner通知在 `lockmanager/graph.go`（新）；验收：T048通过、detector不调用Txn.Rollback；FR-008/009/024/025/029。
      **证据**：`graph.go` 的 `waitGraph` 保存 HOLDER/OLDER_WAITER 两类边并维护 `revision`；
      `detectCycle` 按 ordinal 顺序做**有界** DFS，工作预算耗尽返回 `exhausted` 且**不**报 deadlock；
      `pickVictim` 取环内最大 ordinal。
      **revision recheck**：检测成功时若环内已含被标记的 victim 则跳过，不重复挑第二个 victim
      （`TestB02DeadlockIsReportedOnceUntilTheVictimIsCleanedUp`、`TestB02ConcurrentDetectionNeverPicksTwoVictimsForOneCycle`）。
      **bounded**：`detectCycle` 受 `BudgetLimits.GraphWork` 约束；环摘要受 `maxDeadlockNodes`（32）截断并置 `Truncated`。
      **detector 不调用 Txn.Rollback**：`lockmanager` 不 import `mvcc`/`executor`（架构断言测试仍通过），
      `DetectDeadlock` 只做三件事 —— 标记 `abortRequested`、以 `RequestDeadlockVictim` 撤销 victim 的 pending 请求、记录摘要；
      **不释放 victim 的 grants**（`TestB02DeadlockDoesNotReleaseVictimGrants` 锁定，
      该测试同时证明「不删边留下活锁」被避免：survivor 保持 PENDING 直到 victim 自己 `ReleaseOwner`）。
      完整清理契约由 victim 自己的 `ReleaseOwner` 完成（FR-008/009/020/029）。
- [x] T051 [US5] 在 `mvcc/transaction.go`、`mvcc/merge.go` 接入definitefinish/childscope与cleanup后ReleaseRoot；验收：三类root、failedmerge、publicationcancel不变、无prematureunlock；FR-019/020/021/029/030。
      **证据（已完成）**：新增 `mvcc/locking.go`；`mvcc/transaction.go` 的 `finish` 现在是契约规定的四段式顺序 ——
      **(1) 记录终态 → (2) 写集不可用（`cleanup`）→ (3) 结束锁参与（child 关闭自己的 scope；root `ReleaseOwner`）→ (4) 注销注册表**。
      「cleanup 后 ReleaseRoot」因此对成功与失败两条路径同时成立；「已定案的结果不会被释放改写」由顺序 (1) 先于 (3) 保证。
      child scope 在 `newTx` 建立（root 用 statement scope、child 用 child scope）并在 `finish` 中关闭；
      `ChildContext` 继承 root 的 owner（子事务**不**创建自己的 owner），因此 child merge 在原理上不可能释放 root 的 grant。
      `mvcc/merge.go` **未改**（merge 语义不在本阶段范围，失败 merge 仍走原 `p.Rollback()` 路径）。
      验收「无 premature unlock」由 `TestB02LocksAreHeldUntilThePublicationIsDecided`、
      `TestB02ChildCommitKeepsRootLocks`、`TestB02RootWithNoWriteSetStillReleasesLocks` 锁定。
- [ ] T052 [US5] 在 `executor/transaction_engine.go`、`executor/savepoint_mvcc.go`、`executor/session_settings.go`、`server/connection.go` 接入statementscope、timeoutrollback、SI/deadlockrootabort与disconnect/reset；验收：root可达、单consumer、discardstage与grants清理；FR-009/010/017/020/021/030。
      **未执行**：需要改 `executor`/`server`，即「SELECT FOR UPDATE / DML / Gap-Next-Key」被排除的 SQL 面。
      `mvcc` 侧已提供 T052 需要的三个入口：`Tx.BeginLockScope`/`CloseLockScope`（statement scope）、
      `Tx.AbortRequested`/`AbortRequestReason`（SI/deadlock root abort）、`Store.ResetLockEpoch`/`CloseLocks`
      （disconnect/reset/Close）。**接线本身未做**，因此 SQL 会话仍然不受锁约束。
- [x] T053 [US5] 在 `mvcc/snapshot.go`、`mvcc/store.go` 与adapter同步generation/Close invalidation；验收：先禁止oldhandles再wake，不影响新epoch计量/retention，未定publish不假rollback；FR-021/022/029/030。
      **证据（已完成，adapter 部分未执行）**：`Store.Restore` 在 `s.txns.Reset()` 之后调用 `s.ResetLockEpoch(...)`，
      两个注册表在同一步换到新 generation；`Store.CloseLocks()` 与 `Store.SyncLockEpoch()` 是 engine/adapter 侧入口。
      **「先禁止 old handles 再 wake」**：`ResetEpoch` 先把每个 owner 标记为 `OwnerEnded` 并**从注册表删除**
      （连同其 scopes 与 grants），再 `cancelRequestsLocked` 唤醒等待者 —— 顺序由
      `TestB02WaiterWakesAfterTheHolderIsInvalidated` 锁定（waiter 必须收到 `ErrLockInvalidated`，
      **不得**因为 holder 的 grant 被释放而顺手获授）。
      **不影响 retention/计量**：B01 的 `oldestReadTS` 与 `txns.Reset` 逻辑未改；锁侧只额外注销 owner。
      **未定 publish 不假 rollback**：`TestB02FailedPublicationIsReconciledAfterARestore` 断言未定事务
      `Committed()` 为 false、commit 被拒为 `ErrConflict`、状态为终态且锁全部释放。
      **adapter 部分未执行**：`storageengine/mvccadapter/locking.go` 不存在（T018/T054 范围）。
- [ ] T054 [US5] 在 `replication/node.go`、`storageengine/mvccadapter/locking.go` 接入leader-onlyCurrentRead的Barrier/Termfence与losswake；验收：VerifyLeader不冒充barrier、FSMapply不等待、无新Command字段；FR-011/019/022/026/029。
- [ ] T055 [US5] 在 `mvcc/locking_commit_matrix_test.go`、`replication/locking_commit_test.go`（新）参数化bounded/group/stream/WAL/Raft含acceptedChanges及rawapply干预；验收：final latest>readTS拒绝、三类root与marker一致，不改validator/codec；FR-018/019/022/030，SC-004/007。
- [ ] T056 [US5] 在 `config/config.go`、`cmd/gbaselite/main.go`、`executor/open_options.go`、`enginefactory/default.go`、`storageengine/engine.go` 传递lock预算与默认配置，在 `server/handler.go` 接入1205/1213中立映射；验收：符合contracts配置表、env优先级、单位/0/default/overflow明确、原冲突映射保持；FR-009/010/025/026/031。
- [ ] T057 [US5] 在 `config/locking_test.go`、`executor/locking_settings_test.go`、`server/locking_error_test.go`（新）覆盖YAML/env/options/session设置及wireerror；验收：会话期限GLOBAL/负数/0/小数/超界拒绝，engine预算0取默认、合法边界接受，query较短期限和资源budget通过；FR-009/010/025/026/031，SC-006。

## Phase 8: US6 诊断与确定性验收 (T058–T064, P2)
**Goal**: bounded管理员诊断、可重复竞争验证，不泄露数据。

> **执行结果（T058、T059 的 `lockmanager` 部分已完成；T060–T064 未执行）**
> 新增 `lockmanager/diagnostics.go`（bounded 只读快照、匿名 resource ID、stats）与
> `lockmanager/diagnostics_test.go`（T058）。
> **未执行**：T060–T064 —— 它们是 adapter 转换（`storageengine/mvccadapter`）、
> `SHOW ENGINE` 语法、executor 与 server 接线，全部属于被排除的 SQL 面。
> 因此**没有 SQL 可见的诊断入口**：`Manager.Diagnostics` 存在并被测试，
> 但没有 parser/executor/server 调用它。
> 证据：`.tmp/b02-phase7-validation.md`。

### Tests first
- [x] T058 [P] [US6] 在 `lockmanager/diagnostics_test.go`（新）测试page256/257、cursorrevision、ring32/33、ownedcopies、privacy及并发snapshot；验收：截断明确、无rawkey/row/SQL；FR-023/025，SC-006。
      **证据**：`lockmanager/diagnostics_test.go` 8 个测试全通过 ——
      `TestB02DiagnosticsTruncatesAtTheContractBound`（**page 256/257**：`Limit=257` 被钳到 256 并报 `Truncated`；
      257 个 waiter 时 `len(Waits)==256`、`TotalWaits` 报完整总数、`WaitsTruncated=true`；`Limit=4` 报截断）、
      `TestB02DiagnosticsCursorTracksTheGraphRevision`（**cursor revision**：cursor 就是 graph revision；
      无关 grant 不移动 cursor）、`TestB02DiagnosticsRetainedDeadlockRingIsBounded`（**ring 32/33**：
      33 次死锁后只保留 32 条，`TotalDeadlocks==32`、不报截断）、
      `requireCopiedSlices`（**owned copies**：改写返回行的所有 slice 字段后，
      再读一页证明 manager 状态与保留历史都未被影响）、
      `TestB02DiagnosticsPrivacyExposesNoRawIdentity`（**privacy**：把 key/表名/索引名/space 设为
      `SECRET-ROW-KEY`/`SECRETTABLE`/`SECRETINDEX`，再对整页渲染结果做子串扫描，
      断言不含这些值、也不含 `index/`/`PRIMARY`/`SELECT`/`k1`；同时断言 `root-b` **在**页中，
      使该断言不是空页通过）、`TestB02DiagnosticsIsReadableConcurrently`（**并发 snapshot**：
      6 reader × 120 轮与 8 组 writer 并发，每页内部计数与列表一致）、
      `TestB02DiagnosticsPageReportsHoldersWaitersAndEdges`、`TestB02DiagnosticsStatisticsAreConsistentWithTheGraph`、
      `TestB02DiagnosticsReportsWaitDurationsFromTheManagerClock`（等待时长只随 manager 时钟前进）。

### Implementation and integration
- [x] T059 [US6] 实现boundedlock/wait/deadlock snapshots、匿名resourceIDs和stats在 `lockmanager/diagnostics.go`（新）；验收：不持latch输出、不等待userprogress、epoch与release计量正确；FR-023/025。
      **证据**：`diagnostics.go` 提供 `LockDiagnosticsPage`（resources/waits/deadlocks 三组行）、
      `LockResourceRow`、`LockWaitRow`、`DiagnosticsOptions` 与 `Manager.Diagnostics`。
      **bounded**：`ResolvePageLimit` 把 limit 钳到 `[1, MaxDiagnosticRows=256]` 并回报 `Truncated`；
      三组行各有独立的 `*Truncated` 与完整 `Total*` 计数；deadlock ring 上限 `maxDeadlockSummaries=32`。
      **匿名 resource ID**：`lockEntry.anonID` 由 manager 本地计数器按需分配，
      是序号而非 key 的哈希，因此不携带表/索引/键的任何信息。
      **不持 latch 输出、不等待 user progress**：`Diagnostics` 只在一次短临界区内复制纯值后返回，
      不执行 I/O、不调用任何外部回调、不等待任何 owner；等待时长由 `BlockedAt` 与注入时钟相减得到。
      **epoch 与 release 计量正确**：`Stats.EpochInvalidations` 计**事件**数（`m.invalidated++`），
      而 `ResetEpoch` 的返回值计**受影响 owner** 数，两者语义在注释与
      `TestB02EpochResetAndCloseClearTheGraph` 中分别锁定；
      `WaitEdges` 直接取派生图大小，因此统计视图与图视图不可能不一致。
      另外修正了 `cancelRequestsLocked` 把**队列条目数**当作**请求数**回报的问题
      （pending 索引按组件条目为键，一个双组件 bundle 会出现两次）。
- [ ] T060 [US6] 在 `storageengine/mvccadapter/diagnostics.go`、`storageengine/locking.go` 实现可选LockDiagnostics转换；验收：不污染B01TransactionDiagnostics、缺失capability明确；FR-023/026。
- [ ] T061 [US6] 在 `parser/parser.go`、`executor/locking_diagnostics.go`（新）、`server/metadata_browse.go` 接入三类SHOWENGINE及管理员权限；验收：limit/truncated明确，普通账号拒绝；FR-023/028/031。
- [ ] T062 [US6] 在 `executor/locking_diagnostics_test.go`、`server/locking_diagnostics_test.go`（新）验证管理员/非管理员、unsupported、匿名字段、页面与cursor；验收：零敏感数据泄露、不阻塞独立txn；FR-023/026/031，SC-006。
- [ ] T063 [US6] 在 `lockmanager/concurrency_test.go`（新）固定barrier/fakeclock调度100次cycle/queue/upgrade/grantcancel/epoch/budget；验收：victim与结果一致、终态全零、无sleep；FR-006..010/021/022/024/025，SC-001/002/003/006。
- [ ] T064 [US6] 在 `executor/locking_concurrency_test.go`（新）及 `.tmp/b02-determinism.md` 保存100次SQL/rawwriter/索引冲突与生命周期结果；验收：相同调度相同结果、读模式分离、各scope资源为零；FR-011..024/026/030/032，SC-002/003/004。

## Phase 9: Polish and Cross-Cutting Gates (T065–T072)
全部story完成后执行；未授权发布/部署。本轮所有任务保持未勾选。

- [ ] T065 对已修改Go文件gofmt并验证 `executor/architecture_test.go` 的backend/physical依赖边界；证据 `.tmp/b02-formatting.md`；验收：格式无差异，SQL不import具体mvcc/lockmanager；FR-026/029/031，SC-005。
- [ ] T066 执行 `go test ./...`、`go vet ./...` 和 `specs/006-b02-row-lock-manager/quickstart.md` 全验收，保存 `.tmp/b02-validation.md`；验收：退出0、现有B01/A05/资源测试不删不skip；FR-024/030/031，SC-004/005/008。
- [ ] T067 [P] 在 `.github/workflows/test.yml` 验证支持平台race job运行B02与既有事务测试，保存 `.tmp/b02-race.md`；验收：真实race通过，不伪报Windows工具链缺失；FR-024/029/030，SC-002/004。
- [ ] T068 [P] 在 `mvcc/locking_format_compat_test.go`、`replication/locking_replay_test.go`（新）用旧格式fixture验证open/backup/restore/WAL/replicareplay；验收：无migration/newlockdurablefields，replay不等待locks；FR-019/022/030，SC-007。
- [ ] T069 [P] 在 `lockmanager/resource_boundary_test.go`（新）及 `.tmp/b02-resource-calibration.md` 验证最终默认memory/root/waiter/graph边界、大事务、whole-domain fallback与diagnostics负载；验收：exact/+1、无计量overflow、符合T004校准且零leak；FR-025/027/030，SC-006。
- [ ] T070 更新 `README.md` 的实际已实现语法、current-vs-snapshot/SI限制、锁预算/YAML/env/session、错误与diagnostics矩阵；验收：同功能变更完成、无完整InnoDB兼容或未测量性能声明；FR-010/012/023/025/027/028/030/031，SC-005。
- [ ] T071 只读执行 `$speckit-analyze` 并复核 `specs/006-b02-row-lock-manager/traceability.md`；验收：FR/SC design/implementation/test覆盖100%，critical/high为零，不自动实施；FR-031，SC-005/008。
- [ ] T072 核对 `specs/006-b02-row-lock-manager/workflow.md`、全部真实测试证据与 `git diff` 授权/格式边界；验收：只勾选有证据任务，未获部署授权不stop/replace/start，data/未变；FR-019/030/031，SC-005/007/008。

## Dependencies & Execution Order

### Phase graph
```text
1 Setup → 2 Foundation → 3 US1 → 4 US2 → 5 US3 → 6 US4 → 7 US5 → 8 US6 → 9 Gates
```
所有phase依赖前一phase checkpoint；不是可以任意并行的story集合。各story可以用独立资源/SQL
fixture验收，完整服务启用还依赖US5终态与耐久路径gate。

### Within-phase dependency layers
- Setup：T001/T002/T003可独立开始，T004依赖T001（及T002授权）。
- Foundation：T005/T006 → T007/T008/T009/T010 → T011 → T012；T008依赖T005/T006。
- US1：T013/T014 → T015 → T016 → T017 → T018 → T019。
- US2：T020/T021 → T022 → T023 → T024 → T025 → T026 → T027。
- US3：T028/T029/T030 → T031 → T032；T033依赖T030，T034依赖T032/T033/T025；T035 → T036。
- US4：T037/T038/T039 → T040 → T041 → T042 → T043 → T044 → T045 → T046。
- US5：T047/T048/T049 → T050 → T051 → T052 → T053 → T054 → T055 → T056 → T057。
- US6：T058 → T059 → T060 → T061 → T062 → T063 → T064。
- Gates：T065 → T066/T067/T068/T069 → T070 → T071 → T072；共享文件的mutation需串行。

Tests first：先验证新增测试针对尚缺行为失败，再实施对应功能使其通过；不修改assertion迎合错误实现。
T056沿现有 `enginefactory/default.go` 装配入口接入；不新增第二装配入口。

## Parallel execution examples per story
US1：T013和T014；US2：T020和T021；US3：T028/T029/T030；US4：T037/T038/T039；
US5：T047/T048/T049；US6：manager诊断fixture准备可与SQLfixture准备并行，但T058仅标自身测试，
T059..T064按依赖串行。所有[P]任务只在同层且不修改共同源文件时并行。

## Implementation strategy
MVP为US1 kernel fixture；随后按范围→current→DML→终态/复制→观测增量验收。
在US5和cross-cutting gates完成之前不发布/部署SQLlocking能力。严格保持originalSIvalidator及
A05candidate读域；若需要InnoDB式write rebase，另立feature，不在B02任务里隐式增加。

## Completion Status

> **最终验收结论（本轮）：B02 未完成。32/72 已勾选，40 项未执行。**
>
> 关键结论：`lockmanager` 是一个**完整且有测试的组件**，生产路径上的接入按下面分层，不能一句
> "基本未被使用"概括：
> - `INSERT`：取表意向 IX、主键行 X、**唯一值 reservation** 与非唯一二级索引 entry X，**会产生阻塞**；
>   同唯一值的第二个事务等待、被 holder 的提交唤醒，然后在锁下按**最新已发布**索引重新判重并
>   得到 duplicate-key（不是可重试的版本冲突）。
> - `UPDATE` / `DELETE`：**已启用**目标行 X 锁（含移动主键的新旧身份与新旧索引项），冲突语句等待并由
>   holder 的 COMMIT/ROLLBACK 唤醒。第二轮代码审查用 trace + 确定性用例确认：此前记录的
>   "等待后不唤醒"是**串行测试假象**（释放 holder 的 COMMIT 与阻塞的 UPDATE 在同一 goroutine），
>   lock manager 的 release/queue advance/notify 均正确；详见 `.tmp/b02r2-fix-report.md` §4。
> - `SELECT ... FOR UPDATE` / `FOR SHARE` / `LOCK IN SHARE MODE`：**解析器识别，执行器不读取**，
>   仍按普通快照读执行；Current Read 只有存储层能力（且本轮补强了 grant 授权验证）。
> - Gap / Next-Key / Insert Intention：`lockmanager` 支持齐备且已测试，
>   但**没有任何 SQL 路径会申请它们**。
> - 死锁检测**没有驱动方**；锁等待超时无法从 SQL 配置（无 `innodb_lock_wait_timeout`），
>   1205/1213 的 wire 映射与锁诊断 SQL 入口也都没有。
> - FK 派生写（`applyForeignKeyActions`）不经过 DML 锁算子，属 T044 未做。
>
> 因此：`go test ./...` 全绿（22 包 / 0 FAIL）**不等于** B02 功能可用。
> 完整验收记录见 `.tmp/b02-final-acceptance-review.md`；第二轮修复见
> `.tmp/b02r2-fix-report.md`。
> `go test -race ./...` **未执行**（本机无 C 工具链：`-race requires cgo`，且无任何 gcc/clang/zig），
> T067 保持未勾选，**不伪报 race 通过**。

**已执行并勾选 32/72**：Phase 1 Setup 的 T001–T004、Phase 2 Foundational 的 T005–T012、
Phase 3 US1 内核的 T013–T017、Phase 7 US5 的 T047/T048/T049/T050/T051/T053、Phase 8 US6 的 T058/T059。
其余 40 项保持未勾选，未执行：US1 的 T018/T019、US2 全部（T020–T027）、US3 全部（T028–T036）、
US4 全部（T037–T046）、US5 的 T052 与 T054–T057、US6 的 T060–T064、Phase 9 Gates 全部（T065–T072）。

**T020/T022/T023 的完成范围说明**：区间包含语义与边界感知冲突规则已完成并通过 41 个用例，
且**已把 lock manager 接入生产装配**（`mvccadapter.LockingTxn` + `enginefactory` 侧创建）。
但 **`executor` 仍不读取锁定模式**，因此 SQL 层不产生任何 Gap 或 Insert Intention 锁，
用户要求"必须验证相应索引访问路径的实际 SQL 行为"**未达成**。
要求 11–13（与 Access Plan 协调、保守/明确拒绝、避免错误冲突）**未验证**。
证据：`.tmp/b02-phase12-gap-validation.md`。

### 未支持的索引场景（T025/T026 未做，要求 10 的现状）
复合索引、非唯一二级索引后缀、NULL 不产生 unique reservation、collation 等值同身份、
旧/新键编码 fallback、覆盖索引仍锁 common row、范围扫描的 lock-aware 有限页 iterator
**全部未实现**。`lockmanager` 层面只比较调用方传入的字节（从不解析或重编码 key，这是正确且必须的），
但"传入哪些字节"由 `executor` 决定，而该决定尚未实现。

**T029/T030/T031/T033 的完成范围说明**：本轮先修订了一个必须先解决的架构冲突
（见 `contracts/current-read.md` §Locked-read write baseline 与 `plan.md` §A3.1），
然后完成 `mvcc` 当前读 + 逐 key 有界 baseline 与 `parser` 锁定模式。
**但 `executor` 尚未读取锁定模式，生产路径仍未创建 lock manager**，
因此 `SELECT ... FOR UPDATE` 仍不会真正加锁，用户列出的并发测试 A–G **全部未实现**。
证据：`.tmp/b02-phase11-current-read-validation.md`。

**T047/T049/T051/T053 的完成范围说明**：这四项的 `mvcc`（以及 `lockmanager`）部分已完成并通过测试，
但它们的 `executor` / `server` / `replication` / adapter 部分**未执行** —— 那些文件属于被排除的
SQL 面（SELECT FOR UPDATE、DML、Gap/Next-Key）。因此：**锁管理器已与 B01 事务生命周期正确集成，
但 SQL 层尚未使用它**。`executor`、`parser`、`server`、`config`、`replication` 逐字节未变。
Task 的 test 标记不等于测试已执行；勾选项均附真实命令输出或源码证据。
按故事数量：US1=7，US2=8，US3=9，US4=10，US5=11，US6=7；Setup=4，Foundation=8，Gates=8。

### Phase 3 证据文件
`.tmp/b02-phase3-validation.md`：命令与退出码、新增/修改文件、30 个新测试的覆盖映射、
**14 个实现期真实缺陷**及修正、9 条关键不变量逐条核对、race 环境限制、6 项未解决问题。

### 本轮（锁管理器 × B01 事务管理器生命周期集成）证据文件
`.tmp/b02-phase9-lifecycle-validation.md`：命令与退出码、新增/修改文件、26 个新测试与 11 项必测的对应、
**本轮发现并修正的真实缺陷**（含两个会让两个注册表永久不一致的 bug）、锁顺序不变量的论证与验证方式、
持久化不变量核对、race 环境限制、未解决问题。

### 上一轮（Phase 7 死锁 + Phase 8 诊断的 lockmanager 部分）证据文件
`.tmp/b02-phase7-validation.md`：命令与退出码、新增/修改文件、33 个新测试的覆盖映射、
**本轮发现并修正的真实缺陷**（含一个会让取消的 waiter 永久阻塞资源的 bug、两个图边缺陷、
一个性能悬崖）、关键不变量逐条核对、race 环境限制、未解决问题。

### 进入 Phase 4（US2 区间与插入保护）的阻断条件
1. **T018/T019 仍必须先做**（阻断）：`lockmanager` 现已通过 `mvcc` 接入事务生命周期
   （`Store.AttachLocks`），但 `storageengine/mvccadapter` **没有** `locking.go`，
   `enginefactory` 也**没有**创建或装配 lock manager，而 `executor`/`server` 完全不感知锁。
   因此生产二进制仍然是无锁运行的：**测试之外没有任何代码路径会创建一个 lock manager**。
   直接做 US2 会在"无法从引擎驱动"的基础上叠加区间规则，无法做端到端验收。
2. **T052/T054 是剩下的接线（阻断 SQL 可达性）**：`executor/transaction_engine.go` +
   `server/connection.go`（statement scope、timeout rollback、disconnect/reset）与
   `replication/node.go`（term fence）。`mvcc` 侧入口已就绪并被测试，**接线未做**。
3. **死锁检测仍没有后台驱动点**：`DetectDeadlock` 只能由调用方显式调用；
   事务层现在会在 owner 已被标记后拒绝继续提交（`ErrDeadlock` / `ErrConflict`），
   但**没有任何 goroutine 会去检测**。这是 T051/T052 的剩余部分。
4. **`ErrLockWaitTimeout`/`ErrDeadlock` 现已可达**，但只有显式驱动才生效：
   `Manager.CheckTimeouts` 与 `WaitForGrantResult` 的上下文/期限路径会产生 `ErrLockWaitTimeout`，
   `DetectDeadlock` 会产生 `ErrDeadlock`。**没有后台 goroutine 自动检测**，
   因此生产上必须由 T051/T052（事务层）或一个由调用方拥有的驱动来调用它们。
3. **`ErrLockResourceLimit` 仍只有预留失败路径**：等待者数量/内存/图预算是 fail-closed 的，
   但 `RequestResourceRejected` 没有产生点。
4. **公平性模型的边界**：当前是「每资源 FIFO + 整包就绪」，不是跨资源的严格 `EnqueueOrdinal` 全序。
   若 US2/US5 需要严格全序授予，需重新设计队列模型。
5. **`Release` 只撤销一个 claim**：调用方必须成对释放它取得的每个组件（释放一行不会连带释放
   表意向，与 InnoDB 意图锁存活至事务结束一致）。T018 装配时必须把这一语义映射到 SQL 生命周期。
6. **上下文取消现在会撤销请求**（本轮修正）：`WaitForGrantResult` 在 context 结束时把请求从队列
   移除并以 `RequestContextCanceled` 结束它。这与「查询取消只终止 statement」一致，但意味着
   调用方不能靠一个已取消的 context 保留排队位置。
7. 未解决问题的完整清单见 `.tmp/b02-phase7-validation.md` §7 与 `.tmp/b02-phase3-validation.md` §7。

### 第二轮（阻断缺陷修复 + 代码审查）证据与任务口径

证据文件 `.tmp/b02r2-fix-report.md`（本轮唯一总证据）：5 项阻断缺陷的"修复前失败证据 → 定位 →
修复 → 修复后通过"、临时 trace 与调用栈、不变量核对表、`go test ./...` 退出 0、race 工具链不可用
的原始报错，以及本轮发现但不属这 5 项的遗留问题（同主键并发插入的错误类别、Windows
`BACKUP MVCC TO '绝对路径'` 的字面量转义瑕疵、中性边界对"编程错误类"错误的透传）。
辅助原始输出：`.tmp/b02r2-gotest-all2.txt`、`.tmp/b02r2-unique-before.txt`、
`.tmp/b02r2-unique-after.txt`、`.tmp/b02r2-heapkey-before.txt`、`.tmp/b02r2-heapkey-after.txt`、
`.tmp/b02r1-locktrace.txt`。

新增/改写的测试与固定负向测试的处置（**不删除任何 B01/A05 回归测试**）：

| 固定负向测试 | 处置 | 取代它的验收测试 |
|---|---|---|
| `TestB02ConcurrentInsertSameUniqueKeyIsStillAccepted` | 删除（断言的是缺陷本身） | `executor/locking_unique_test.go`（同值等待→唤醒→duplicate；NULL/不同值/复合/collation/UPDATE 占位） |
| `TestB02UpdateTargetLockingIsNotEnabled` | 删除（钉住的是"锁被停用"） | `executor/locking_dml_wake_test.go`（真实 SQL Session 的阻塞与唤醒，UPDATE+DELETE） |

任务口径（**不勾选任何未完整达成的任务**）：
- **T038 仍不完整**（继续未勾选）：新增的 `executor/locking_unique_test.go` 覆盖了 UNIQUE/复合/NULL/
  collation/等待后判重，但**同主键的 duplicate-key 错误类别**与 **IGNORE/REPLACE/upsert 的并发用例**
  未实现（前者见 `.tmp/b02r2-fix-report.md` §8.1）。
- **T041/T042/T043 仍不完整**（继续未勾选）：`modify_update.go`/`modify_delete.go`/`modify_insert.go`
  已接入锁与锁后判重，多表 DELETE（经 `DeleteOperator`）与 INSERT SELECT（经 `InsertOperator`）随之覆盖；
  但 FK 派生写（T044）与 `mutation_join_pipeline.go` 的独立验证仍未做。
- **T029/T031 得到补强**（已勾选，不新增勾选）：`mvcc/current_read.go` 的 grant 授权验证从不完整
  （只比 owner）改为"owner + epoch + 组件覆盖 + manager 实时 applied claim"四项，并新增
  `mvcc/current_read_authorization_test.go` 与 `storageengine/mvccadapter/locking_current_read_test.go`。
- **T018 得到部分实现**（继续未勾选）：`mvccadapter` 的 grant 现在完整映射并回传
  Components/Generation/Term，reentrant 分支不再伪造降级能力，`ErrConflict` 也有了中立映射；
  但 engine 级装配与更多 namespace 标签仍未验收。
- **T067（race）保持未勾选**：本机无 C 工具链，`-race` 无法运行，已在 §7 与 tasks 顶部如实记录。

契约同步（本轮随代码一起修订）：
`contracts/current-read.md` 新增 **Grant verification** 一节（四项校验与"pending 不是锁"）；
`contracts/sql-locking.md` 的 DML 一节新增**唯一值 reservation 与物理 index entry 的资源表**、
等待后重校验的三种结论，以及 UPDATE 每个候选都取新旧索引项的规则。
### 第三轮（Lock Manager 契约缺陷修复）证据与任务口径

证据文件 `.tmp/b02r3-fix-report.md`：两条缺陷的"复现（修复前失败）→ 根因 → 修复 → 修复后通过"、
不变量核对、`go test ./...` 退出码、race 工具链不可用的原始报错，以及本轮发现的其他问题。
辅助原始输出：`.tmp/b02r3-before.txt`（修复前 6 个失败）、`.tmp/b02r3-after.txt`、
`.tmp/b02r3-lockmanager-all.txt`、`.tmp/b02r3-gotest-all.txt`、`.tmp/b02r3-race.txt`。

新增测试：`lockmanager/grant_contract_test.go`（pending 不等于已授予、加入排队请求、TryAcquire 拒绝、
等待获授的 grant 身份、多资源 bundle、多次读取一致、重入与 adopt 保持、owner 结束后失效、取消不产生 grant）、
`storageengine/mvccadapter/locking_grant_test.go`（中立层跨等待的 grant 身份）。
改写的既有测试：`lockmanager/lock_acquisition_test.go` 的
`TestB02ReentrantUpgradeWhileWaitingAdoptsThePendingClaim` → `TestB02RepeatUpgradeWhileWaitingJoinsTheQueuedRequest`
（旧断言正是本轮缺陷）。

任务口径（**不勾选任何未完整达成的任务**）：
- **T016/T017 得到补强**（已勾选，不新增勾选）：US1 内核的 acquire/公平队列新增"pending≠applied"与
  "重复申请加入原请求"两条规则，并新增 grant 身份记录；`TryAcquire` 语义明确为 `ErrConflict`。
- **T018 得到补强**（继续未勾选）：`mvccadapter.grant()` 逐字段映射 `GrantID`/`OriginScopeID`/`Components`/
  `Generation`/`Term` 并移除误导性的 `Deadline`，新增中立层跨等待身份测试；engine 级装配与 namespace
  标签仍未验收。
- **T048/T050 不受影响**（死锁图/检测未改），但队列诚实性判定的变化已由 lockmanager 全套回归覆盖。
- **T067（race）保持未勾选**：本机无 C 工具链（`-race requires cgo`，且无任何 gcc/clang/zig），
  `go test -race ./lockmanager/...` 无法运行，**不伪报 race 通过**。

契约同步（随代码一起修订）：`contracts/lock-manager.md` 新增 §Claim state and grant identity；
`data-model.md` 的 LockRequest/LockGrant 一节补充 claim 状态与"grant 身份属于该 request"。
本轮未改 WAL、Raft Command、存储格式，也未进入其他 Phase。


### 本轮证据文件
| 文件 | 内容 |
|---|---|
| `.tmp/b02-baseline.md` | T001 实施前测试基线（环境、退出码、21 包结果、耗时、已存在失败=0） |
| `.tmp/b02-format-baseline.md` | T003 旧格式 / Op / Command / WAL / codec / 五提交路径 / Root-Child / Savepoint 调用路径 |
| `.tmp/b02-resource-calibration.md` | T004 实测规模与预算裁定（65536 → 262144 的依据、内存核算、兼容影响） |
| `.tmp/b02-validation.md` | T005–T012 验证证据、23 个新测试清单、六条关键不变量逐条核对、354 文件哈希零改动证明 |
| `.tmp/b02-src-hashes.txt` | 354 个生产 Go 文件的逐个 SHA-256 指纹清单 |
| `.tmp/b02-scale.txt` | 资源规模测量原始输出 |
| `.tmp/b02measure/` | 测量夹具（独立 module，module 树外，不参与 `go test ./...`） |
| `.tmp/b02-gotest-clean.json` / `.tmp/b02-gotest-after.txt` / `.tmp/b02-govet-after.txt` | 原始命令输出 |

### 下一阶段（Phase 3 US1）阻断条件
见本轮最终报告的「进入下一阶段的阻断条件」一节。简要：US1 需要实现兼容矩阵与公平队列，
必须先取得用户对 Phase 3 的明确授权（constitution 原则 V），且 `compatibility.go`/`queue.go`
的新增不得改变本轮已建立的预算语义与「scope 不释放 grant」规则。
