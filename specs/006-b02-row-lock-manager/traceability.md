# B02 Traceability — Final Acceptance Verdict

**Verdict: B02 is NOT complete. 32 / 72 tasks done; 40 unbuilt.**

The lock manager is a complete, tested component that is **largely unused in production**:

| Capability | Component | Reached from SQL? |
|---|---|---|
| Record S/X, IS/IX matrix | implemented + tested | **no** (no SQL path acquires a lock) |
| Gap / Next-Key / Insert Intention | implemented + tested | **no** (nothing requests GAP or II) |
| Wait queue, fairness, timeout, cancel | implemented + tested | **no** |
| Wait-for graph, deadlock victim | implemented + tested | **no driver** in production |
| Current read (locked read) | implemented + tested | **no** — `executor` never reads `LockMode` |
| Diagnostics | implemented + tested | **no** SQL entry point, no admin gate |
| INSERT lock | implemented | acquires, but **does not block** |
| UPDATE / DELETE lock | implemented | **deliberately disabled** (waiter not woken after holder commit) |
| Commit/rollback/savepoint lifecycle | implemented + tested | **yes** (in-process) |

Full acceptance record: `.tmp/b02-final-acceptance-review.md`.
Per-round evidence: `.tmp/b02-phase3-validation.md`, `.tmp/b02-phase7-validation.md`,
`.tmp/b02-phase9-lifecycle-validation.md`, `.tmp/b02-phase11-current-read-validation.md`,
`.tmp/b02-phase12-gap-validation.md`, `.tmp/b02-phase13-dml-validation.md`.

Commands: `gofmt` clean, `go vet ./...` exit 0, `go test ./...` exit 0 (22 packages, 0 FAIL).
`go test -race ./...` **did not run** (no C toolchain) — nothing is claimed for it, T067 unchecked.

---
# B02 Requirement Traceability

**Status**: Phase 1（T001–T004）、Phase 2（T005–T012）、Phase 3 US1 内核（T013–T017）、
Phase 5 的 T029/T030/T031/T033、Phase 7 的 T047/T048/T049/T050/T051/T053、
Phase 8 的 T058/T059 已执行并勾选；
T018/T019、T028、T032、T034–T036、Phase 4/6 全部、T052、T054–T057、T060–T064 仍为规划覆盖。
32 FR + 8 SC；其余 43 项任务待授权实施。

**架构修订（B02-1）**：`contracts/current-read.md` §Locked-read write baseline 与 `plan.md` §A3.1
把 INV-005 精化为**逐 key 观测 baseline**。原因：B01 的提交校验器只有事务级 `readTS`，
无法同时满足"锁下观测到新版本后写该 key 合法"与"未加锁写入必须冲突"。
修订只改变 baseline 的来源，不改变 `readTS`/`Snapshot`/GC horizon/`Command.Snapshot`，
也不改变未加锁路径的判定。

**范围提示**：T047/T049/T051/T053 的 `mvcc`（与 `lockmanager`）部分已完成并通过测试，
其 `executor` / `server` / `replication` / adapter 部分**未执行**，属于被排除的 SQL 面。
**因此锁管理器已正确集成到 B01 事务生命周期，但 SQL 层尚未使用它**：
`executor`、`parser`、`server`、`config`、`replication` 在本次改动中逐字节未变。
设计列中的A1..A6对应plan.md，LM/CR/SQL/DIAG对应contracts/lock-manager.md、
current-read.md、sql-locking.md、lock-diagnostics.md；实体与状态共同由data-model.md定义。
测试列是未来必执行任务，未声称测试已通过；本轮已执行的任务其映射见本文件末尾「本轮执行映射」。
每条实施任务的准确文件与验收见tasks.md。

## Functional requirements

| ID | 设计 | 实施任务 | 测试/审核任务 |
|---|---|---|---|
| FR-001 | A1/A2, LM Record | T015,T016,T019 | T013,T014,T063 |
| FR-002 | A1, LM Table | T015,T016 | T013,T027 |
| FR-003 | A1, LM Ranges | T022,T024,T025 | T009,T020,T027 |
| FR-004 | A1/A2, LM atomicity | T023 | T020,T021 |
| FR-005 | A1/A4, LM II, SQL DML | T023,T024,T042 | T021,T038,T045 |
| FR-006 | A1/A2, LM Matrix | T015,T022,T023 | T009,T013,T020,T021,T063 |
| FR-007 | A2, LM Queue | T016,T017,T023,T050 | T014,T048,T063 |
| FR-008 | A2, LM graph | T017,T050 | T048,T063 |
| FR-009 | A2/A5, SQL lifecycle | T017,T050,T051,T052,T056 | T047,T048,T049,T057,T063 |
| FR-010 | A2/A6, SQL Session, LM Configuration | T012,T035,T052,T056 | T049,T057,T063 |
| FR-011 | A3/A6, CR read ordering | T031,T032,T034,T054 | T028,T029,T036,T049,T064 |
| FR-012 | A3, SQL grammar | T033,T034,T035 | T030,T036 |
| FR-013 | A3/A5, CR matrix/retention | T031,T034,T051 | T029,T036,T055 |
| FR-014 | A4, SQL DML | T019,T024,T040,T041,T042,T043,T044 | T038,T039,T045,T046 |
| FR-015 | A1/A3/A4, SQL Lock plans | T025,T026,T032,T034,T043 | T009,T020,T027,T036,T038,T039 |
| FR-016 | A1/A4, SQL UNIQUE | T026,T042,T043 | T027,T038 |
| FR-017 | A4/A5, CR DML | T040,T041,T052 | T037,T039,T046,T064 |
| FR-018 | A3/A4/A5, CR validation matrix | T031,T040,T041,T042,T044,T051 | T029,T037,T038,T039,T055,T064 |
| FR-019 | A5/A6, CR Commit | T031,T051,T054 | T003,T049,T055,T068 |
| FR-020 | A5, LM Lifecycle, SQL lifecycle | T008,T051,T052 | T047 |
| FR-021 | A5, LM Lifecycle | T008,T051,T052,T053 | T047,T049,T063 |
| FR-022 | A5/A6, CR ordering, LM epoch | T053,T054 | T049,T055,T063,T068 |
| FR-023 | A6, DIAG | T005,T059,T060,T061 | T058,T062 |
| FR-024 | Testing gates, LM Acceptance | T012 | T014,T036,T046,T048,T049,T063,T064,T066,T067 |
| FR-025 | A2/A6, LM Configuration, DIAG bounds | T005,T011,T016,T050,T056,T059 | T010,T048,T057,T058,T063,T069 |
| FR-026 | Structure/A1/A3/A4/A6, all contracts | T005,T018,T019,T024,T026,T060 | T007,T027,T028,T036,T045,T057,T062,T065 |
| FR-027 | A1/A3, SQL plans, LM ranges | T022,T025,T026 | T009,T020,T027,T069 |
| FR-028 | A3/A6, SQL supported shapes | T025,T033,T034,T061 | T030,T036,T062 |
| FR-029 | A2/A3/A5/A6, LM discipline, CR ordering | T008,T016,T017,T018,T019,T024,T032,T050,T051,T053,T054 | T014,T036,T045,T048,T049,T063,T067 |
| FR-030 | A3/A4/A5/A6, CR original APIs/Commit | T019,T031,T041,T051,T052,T053,T070 | T029,T036,T037,T046,T047,T055,T066,T068,T069 |
| FR-031 | Constitution/Authorization, SQL limits | T002,T070,T072 | T057,T062,T065,T066,T071,T072 |
| FR-032 | A3/A4, CR metadata/DML | T031,T034,T040,T044 | T036,T037,T039,T064 |

## Success criteria

| ID | 设计 | 实施/交付任务 | 测试/审核任务 |
|---|---|---|---|
| SC-001 | A1/A2, LM matrices/intervals | T015,T016,T022,T023,T025,T026 | T009,T013,T014,T020,T021,T027,T063 |
| SC-002 | A2, deterministic gates | T012,T050 | T048,T063,T064,T067 |
| SC-003 | A2/A5, LM states/lifecycle | T017,T050,T051,T052,T053 | T014,T047,T049,T063,T064 |
| SC-004 | A3/A4/A5/A6, CR/SQL | T031,T034,T040,T041,T051,T054 | T029,T036,T037,T046,T047,T055,T064,T066 |
| SC-005 | Constitution/Testing gates, this matrix | T070,T071 | T007,T065,T066,T071,T072 |
| SC-006 | A2/A6, LM budgets, DIAG | T011,T056,T059 | T010,T048,T057,T058,T062,T069 |
| SC-007 | A5/A6, CR/LM nondurable | T031,T051,T054 | T003,T049,T055,T068 |
| SC-008 | Constitution V, workflow.md | T002,T072 | T003,T072 |

SC-005/008是文档交付与授权审计，实施栏指文档/审核任务，不要求为文档规则增加生产代码。
SC-008的「本轮」指2026-10-10文档规划，后续明确授权不会被该规则误解成永远禁止实施。
T002/T003/T072未来再次核对范围；本轮可从git diff与任务勾选状态验证。

## User story acceptance coverage

| Story | 场景 | 覆盖 |
|---|---|---|
| US1 | S/X、独立行、公平、升级意向 | T013,T014,T027,T063 |
| US2 | gap插入、II兼容、Next-Key、稳定边界 | T020,T021,T027,T038,T045 |
| US3 | 旧/新/旧、墓碑、current-onlyroot、早拒绝 | T028,T029,T030,T036,T055 |
| US4 | PK/UNIQUE、stale target、A05、NULL/collation | T037,T038,T039,T045,T046,T064 |
| US5 | cycle、timeout、layers、publication、epoch | T047,T048,T049,T055,T057,T063,T068 |
| US6 | 诊断、预算、100次固定调度 | T010,T058,T062,T063,T064,T069 |

所有task有FR/SC引用；setup用于授权/基线，foundation用于中立模型/测试设施，gates用于全局
验收。矩阵不把数量覆盖当成语义正确的证明；只读analyze另核对冲突、依赖及constitution。

## 本轮执行映射（T001–T012）

| 任务 | FR/SC | 证据文件 | 验证方式 |
|---|---|---|---|
| T001 | FR-024/030, SC-004/006 | `.tmp/b02-baseline.md` | `go test ./... -count=1` 退出 0 / 38.47s / 21 包 ok / 已存在失败 0；`go vet ./...` 退出 0 |
| T002 | FR-031, SC-008 | 本轮用户消息 + `ask_user_question` 答复 | 用户明确授权「Phase 1 + Phase 2（T001–T012）」 |
| T003 | FR-019/030, SC-007/008 | `.tmp/b02-format-baseline.md`、`.tmp/b02-src-hashes.txt` | 354 文件逐个 SHA-256；桶/codec/WAL/backup/Raft 逐项记录 |
| T004 | FR-025/030, SC-006 | `.tmp/b02-resource-calibration.md` | 实测 3 components/行；65536→262144；四文档同步 |
| T005 | FR-010/023/025/026 | `storageengine/locking.go` | 最小 Txn/Engine 未变；0=默认；无 mvcc 依赖（架构测试） |
| T006 | FR-001..006/020/022/025 | `lockmanager/types.go`、`resource.go` | 实体字段与 data-model 对应；owned bytes 由 OwnedBytes64 计量 |
| T007 | FR-026, SC-005 | `storageengine/locking_contract_test.go` | 3 个测试通过，含 `testkit.Run` 完整后端套件 |
| T008 | FR-020/021/026/029 | `lockmanager/manager.go` | child 唯一 root、无 `*Tx`、ordinal/预算 fail closed |
| T009 | FR-003/004/006/015/027, SC-001 | `lockmanager/resource_test.go` | 10 个测试；含区间端点在点相接的双向判定 |
| T010 | FR-025, SC-006 | `lockmanager/budget_test.go` | 10 个测试；exact/+1、幂等、单一真源 |
| T011 | FR-025/027 | `lockmanager/budget.go` | Reserve/Commit/Release/ClaimEdge/ChargeFixed 全路径归零 |
| T012 | FR-010/024, SC-002/003 | `lockmanager/clock.go`、`test_helpers_test.go`、`concurrency_safety_test.go` | 虚拟时钟 + 屏障 + 100 次无 sleep 单终态 + 8×8×200 并发终态归零 |

## 本轮执行映射（Phase 3 / T013–T017）

| 任务 | FR/SC | 证据文件 | 验证方式 |
|---|---|---|---|
| T013 | FR-001/002/006, SC-001 | `lockmanager/compatibility_test.go` | 9 个测试；Record/Table 有向对穷举、表级 S/X 与 SIX 明确拒绝、意向前置、升级判定 |
| T014 | FR-007/024/029, SC-001/003 | `lockmanager/queue_test.go` | 12 个测试；老 X 前新 S 不插队、无关资源推进、cancel/grant 单终态、grant 胜 cancel、升级保 S |
| T015 | FR-001/002/006 | `lockmanager/compatibility.go` | `modesCompatible` 为唯一矩阵判定点；`ValidateBundle` 校验意向前置；IS/IX 不等同 Table X |
| T016 | FR-001/002/007/025/029 | `lockmanager/manager.go` | `AcquireWith` 单临界区内 all-or-nothing；`bundleReadyLocked` 后一次性应用；无全局门（64 行 + 3 表测试）；无 latch 跨 wait |
| T017 | FR-007/008/009/029 | `lockmanager/queue.go` | FIFO `waiting`；`waitersAhead` + 按 `EnqueueOrdinal` 的 `blockingClaims`；`setStatus` 唯一终态；`Edges()` 去重边；升级不释放 S |
| T018 | FR-026/029 | — | **未执行**：`storageengine/mvccadapter/locking.go` 不存在，内核未被任何 engine 装配 |
| T019 | FR-014/026/029/030 | — | **未执行**：依赖 T018 |

## 本轮执行映射（Current Read 与 SQL Locking Read）

| 任务 | FR/SC | 证据文件 | 验证方式 |
|---|---|---|---|
| T029 | FR-011/013/018/019/030, SC-004/007 | `mvcc/current_read_test.go` | 9 个测试；latest published vs 固定快照、**锁下观测后写该 key 成功且 readTS 不变**、未加锁写入仍冲突、逐 key 隔离、absent/tombstone/own-overlay 三态、grant 失效即丢弃、GC horizon 与三类 root 不变 |
| T030 | FR-012/028 | `parser/locking_read_test.go` | 4 组测试；**7 条 unsupported 语句解析期拒绝**、mode 可区分、普通 SELECT 非锁定 |
| T031 | FR-011/013/018/019/030/032 | `mvcc/current_read.go` | 复用既有 `visible()` 做 latest published，不新建可见性路径；observation 携带 tombstone/changeTS/source；格式与 `Command` 未改 |
| T033 | FR-012/028 | `parser/ast.go`、`parser/parser.go` | `LockMode`+`LockingAlias`；`FOR SHARE` 为新增语法；`NOWAIT`/`SKIP LOCKED` 不再静默消费 |
| T028/T032/T034–T036 | FR-011..013/015/024/027/028/032 | — | **未执行**：adapter contract 测试、lock-aware iterator、executor pipeline、会话期限、两 session SQL 测试 |

**当前 SQL 可达性（重要）**：`executor` 不读取 `Select.Locking`/`LockMode`，
`mvccadapter`/`enginefactory` 不创建 lock manager。因此 **`SELECT ... FOR UPDATE` 仍不加锁**，
用户列出的并发测试 A–G 全部未实现。

## 上一轮执行映射（锁管理器 × B01 事务管理器生命周期集成）

| 任务 | FR/SC | 证据文件 | 验证方式 |
|---|---|---|---|
| T047 | FR-009/020/021/030, SC-003/004 | `mvcc/locking_lifecycle_test.go` | 14 个测试；root commit/rollback 释放、**child merge 保留 root 锁**、失败 statement 保留 grant、savepoint 创建/ROLLBACK TO/RELEASE、双清理幂等、victim 不得继续、timeout 只结束自己的请求、owner 与注册表一致 |
| T049 | FR-009/010/019/021/022/029, SC-003/004/007 | `mvcc/locking_publication_test.go`、`mvcc/locking_commit_test.go` | 12 个测试；**release 点在 durable publication 之后**、victim 不移动 head、**4 配置 × 3 root 形态的发布不变量**、restore 后镜像可用、未定 publish 不假 rollback、SI 冲突不变、group/local-WAL 路径不变 |
| T051 | FR-019/020/021/029/030 | `mvcc/locking.go`、`mvcc/transaction.go` | `finish` 的四段式清理顺序（定案 → 写集不可用 → 结束锁参与 → 注销）；child 继承 root owner、无自己的 owner；child scope 在 finish 关闭 |
| T053 | FR-021/022/029/030 | `mvcc/snapshot.go`、`mvcc/store.go` | `Restore` 与 `txns.Reset` 同步调用 `ResetLockEpoch`；`ResetEpoch` 先结束并注销 owner、再唤醒、最后释放 grant（顺序被测试锁定） |
| T052/T054–T057 | FR-009/010/017/022/025/026/031 | — | **未执行**：需要 `executor`/`server`/`config`/`replication` 接线 |

**锁顺序不变量**：`Store.views → Store.gate / TransactionManager.mu → lockmanager.Manager.mu`。
store 的锁先取、lock manager 永远是最内层；`lockmanager` 内部不回调事务，
事务路径也不在持有 lock manager 锁时去取 store 锁。
`TestB02LockOrderIsNeverInverted` 用 begin/commit/rollback/epoch reset/diagnostics 的并发压力验证；
**这不宣称等价于 race detector**（本机无 C 工具链）。

## 上一轮执行映射（Phase 7 死锁 T048/T050 + Phase 8 诊断 T058/T059）

| 任务 | FR/SC | 证据文件 | 验证方式 |
|---|---|---|---|
| T048 | FR-007/008/009/024/025, SC-002/006 | `lockmanager/deadlock_test.go` | 16 个测试；二/三环、升级死锁、多 holder、两类边、无悬空边、无虚假 deadlock、graphwork 耗尽不误杀、并发检测只挑一个 victim |
| T050 | FR-008/009/024/025/029 | `lockmanager/graph.go`、`manager.go` | 有界 DFS（`GraphWork`）、revision 跟踪、max-ordinal victim、已标记 victim 的环跳过；detector 只标记 abort + 撤销 victim pending，**不释放 grants、不调用 Txn** |
| T058 | FR-023/025, SC-006 | `lockmanager/diagnostics_test.go` | 8 个测试；page 256/257、cursor revision、ring 32/33、owned copies、隐私子串扫描、并发快照 |
| T059 | FR-023/025 | `lockmanager/diagnostics.go` | bounded 三组行 + 独立截断标志与完整总数；匿名 resource ID（本地序号）；一次短临界区复制后返回；等待时长取自注入时钟 |
| T047/T049/T051–T057 | FR-009/010/019/020/021/022 | — | **未执行**：需要 `mvcc`/`executor`/`server`/`config`/`replication` 接线，属被排除的 SQL 面 |
| T060–T064 | FR-023/026/028/031 | — | **未执行**：adapter 转换、`SHOW ENGINE` 语法、executor/server 接线与权限，全部未做 |

**生产可达性（重要）**：本轮交付的能力**没有生产调用点** —— 没有后台 detector goroutine、
没有自动超时驱动、没有 SQL 诊断入口。`DetectDeadlock`/`CheckTimeouts`/`Diagnostics` 只能由 Go 调用方
显式驱动。把这条链路接起来属于 T051/T052/T060/T061。

**race detector**：本机仍无 C 工具链（PATH 无 `gcc`/`clang`，`CGO_ENABLED=0`），
`go test -race` 无法运行。按 quickstart.md 与 T067 的约定**未伪报通过**；T067 保持未勾选，
真实 race 需在具备 cgo 工具链的平台/CI 执行。测试已按 race 兼容方式（无 sleep、channel/屏障同步）
编写。替代证据（不宣称等价）：`TestB02ConcurrentWaitingAndDetectionIsDataRaceFree`、
`TestB02DiagnosticsIsReadableConcurrently`、`TestB02ConcurrentDetectionNeverPicksTwoVictimsForOneCycle`、
`TestB02ConcurrentAcquireAndReleaseIsSafe`。详见 `.tmp/b02-phase7-validation.md` §6。

---

# 附录：与现有源码的一致性核对

**日期**：本轮（Phase 1 + Phase 2 执行轮）
**源码基线**：`140838c2d4f10971c010a9e4d643fd4b135634f2`（与 spec.md 第 165 行、research.md 第 3 行、
workflow.md 第 4 行一致）
**方法**：逐条比对 spec/plan/research/data-model/tasks/contracts 中可核验的**源码事实断言**
（文件是否存在、符号是否存在、当前行为是什么、数值是否为既有约束）与实际源码。

## A1. 核对结论总览

| 类别 | 数量 |
|---|---|
| 与源码一致、无需修订的断言 | 全部其余条目 |
| **与源码不符、已修订文档** | **1**（plan/research/data-model/contracts 的每 root 预算默认值） |
| 与源码不符但属**未来目标**、非缺陷 | 若干（`(new)` 标记文件尚未创建，符合规划语义） |
| 阻断项 | **0** |

**没有发现任何需要修改既有语义才能继续的冲突。** 未擅自改变任何既定语义。

## A2. 已修订的不符项（T004）

| 位置 | 规划值 | 实测/裁定 | 处理 |
|---|---|---|---|
| `plan.md` §Technical Context/Scale-Scope | 每 root grant+pending components **65536** | 含 PK+UNIQUE 的 SQL UPDATE 需 **3 components/行**：10000 行 = 30000，20000 行 = 60000；65536 只够约 21800 行，而仓库自带 10000 行 UPDATE benchmark 路径已占旧上限 46% | 上调至 **262144**，并在四处文档记录校准依据与兼容影响 |
| `research.md` §R8 | 同上 65536 | 同上 | 同上，追加 T004 校准结果段 |
| `data-model.md` §Budget | 同上 65536 | 同上 | 同上 |
| `contracts/lock-manager.md` §Locking discipline + Configuration 表 | 同上 65536 | 同上 | 同上 |

这是**规划文档自身的预测误差**（规划文档在 R8 中已明确要求「实现前基线任务必须覆盖现有大事务
数量，在保持 bounded metadata 的前提下调整这些规划默认值，并同步四类文档」），不是源码与
规格的语义冲突。64 MiB 内存预算经核算仍封顶，因此该调整**放宽**而非收紧，且不改变任何
SI/validator/提交协议语义。

## A3. 逐条核对：spec.md 关键事实断言

| 断言（spec/plan/research） | 源码事实 | 结论 |
|---|---|---|
| 基线 `140838c2d4f10971c010a9e4d643fd4b135634f2` | `git rev-parse HEAD` 相同 | ✅ |
| 「parser：Locking 目前只有 bool；FOR UPDATE/LOCK IN SHARE MODE；NOWAIT/SKIP LOCKED 被接受但策略丢弃；FOR SHARE 尚未支持」(research.md) | `parser/ast.go:189 Select.Locking bool`；`parser/parser.go:1010-1031` `FOR` 只 `expect("UPDATE")`，`NOWAIT`/`SKIP`+`LOCKED` 被 `accept` 后丢弃，`LOCK IN SHARE MODE` 置同一 bool | ✅ 精确 |
| 「当前锁定读明确按 snapshot 读取」(plan/research) | `executor/access_plan.go:31-38` `validateSQLSelectShape` 注释即「accepted and served from the statement snapshot」；`executor.Locking` 在整个 `executor` 包中只出现在注释与测试里，**没有任何执行路径分支** | ✅ 精确，且**比文档更强**：`Locking` 甚至未被读取 |
| 「`VerifyLeader` 不提供 FSM ordering；`Barrier` 才建立 applied ordering」 | `replication/node.go:212-231`：`VerifyLeader` = `raft.VerifyLeader()`；`Barrier` = `VerifyLeader()` + `raft.Barrier()` | ✅ |
| 「`Command.Snapshot` 保留 readTS」「无新 Command 字段」 | `mvcc/store.go:49-58` `Command{Term,Kind,ID,Snapshot,Ops,Counter,Floor,Count}`；`replication/node.go:252` `json.Marshal(command)` | ✅ |
| 「Guard 不是阻塞锁」「GuardRange 乐观依赖」 | `storageengine/engine.go:58-61` 文档；`executor/access_plan.go:32-33` 注释「never blocked」；`mvcc/conflict.go` 只做提交期校验 | ✅ |
| 「fixed readTS、三类 root、retention、五类 commit validation」 | `mvcc/transaction.go:388-419`（A/B/C 三类）、`mvcc/transaction_manager.go:264-287`（`RegisterRoot` pin `snapshotRefs`）、`mvcc/conflict.go`（共享规则）、`mvcc/{local_commit,group_commit,local_stream,local_wal,store}.go`（五路径） | ✅ |
| 「child 继承 parent snapshot，不新增 pin」 | `mvcc/transaction.go:99-107` `RegisterChild`；`mvcc/transaction_manager.go:292-323` `startTS = parent.readTS`，不写 `snapshotRefs` | ✅ |
| 「savepoint 32 层、RELEASE/同名替换只释放名字、ROLLBACK TO 安全重建」 | `executor/savepoint_mvcc.go:20` `maxMVCCSavepoints = 32`；`:61-63` 拒绝先于副作用；`:87-94` RELEASE 只置空名字；`:96-145` ROLLBACK TO 逐层 Rollback + 先指回 parent 再建新 child | ✅ |
| 「A05 parent source / child write、identity dedup、first-match、LIMIT、SET 顺序、spill」 | `executor/transaction_engine.go:270-365`（parent tx + `tx.Child()`）、`executor/physical_binding.go:93-157`（`mutationScan` 用 parent `tx`，`UpdateCandidate.Identity/OldRow/EvalRow`）、`executor/mutation_join_pipeline.go`、`executor/multi_delete_pipeline.go` | ✅ |
| 「`storageengine` 中立接口 + 可选能力」 | `storageengine/engine.go`：`Engine{Begin,Close}` + `Txn{...}`；可选 `RevisionReader`/`CounterAllocator`/`ReplicatedEngine`/`Availability`/`Maintenance`/`Diagnostics`/`TransactionDiagnostics`/`LeaderVerifier`/`FullEngine`；`storageengine/testkit` + `contract_test.go:32-38 coreFixture` 证明最小后端合法 | ✅ |
| 「UNIQUE NULL 不参与 unique reservation、collation 与编码必须复用」(research.md 引用 `sqllayout/keyspace.go`、`storage/versioned_values.go`) | 两文件存在于规划所述包中；B02 未改其任何行为 | ✅（B02 尚未使用，留待 US2/US4） |
| 「旧 `spec/` 路径说明与实际 `specs/` 不符」 | `specs/` 下为 `001-…`、`005-…`、`006-…`；无 `spec/` 目录 | ✅ |
| data-model §LockScope 字段（ScopeID/OwnerRootID/OriginTxnID/ParentScopeID/Kind/ContextDeadline/State） | 实现 `lockmanager.Scope{ID,OwnerID,OriginTxnID,ParentScopeID,Kind,State,OpenedAt,ContextDeadline}` | ✅ 一致（实现多一个 `OpenedAt`，属附加诊断字段，不改变语义） |
| data-model §LockRequest 字段 | 实现 `LockRequest{ID,OwnerID,ScopeID,Components,EnqueueOrdinal,RequestedAt,Deadline,Status,ShouldWait,BlockerIDs,Result,ReservedBytes,ReservationToken}` | ✅ 命名沿用 Go 惯例，字段集合语义对应；`ReservedBytes`/`ReservationToken` 是预算归属的显式句柄（data-model 的 `ReservedBudget`） |
| contracts/current-read.md 候选接口名（`BeginLockScope`/`Acquire`/`ReadCurrent`/`NewCurrentIterator`/`CloseLockScope`） | `storageengine.LockingTxn` 逐字采用这五个方法与同名语义 | ✅ 契约名未漂移 |
| contracts/lock-diagnostics.md 「每页最多 256」 | 实现 `storageengine.MaxLockDiagnosticRows = 256` + `lockmanager.ResolvePageLimit` | ✅ |
| spec.md §Clarifications / FR-028（unsupported 早期拒绝、不继承静默丢弃） | 与现状一致：现状是**静默忽略**，B02 要求改为**明确拒绝**；本轮未改 parser，因此这是**待实现的差异**而非文档错误 | ✅ 规划正确 |

## A4. 逐条核对：plan.md §Project Structure 的文件路径

plan.md 明确标注这些是「未来目标，不是本轮创建的生产文件」，故不存在缺失文件：
`lockmanager/{compatibility,queue,graph,interval,diagnostics}.go`、`storageengine/mvccadapter/locking.go`、
`mvcc/locking.go`、`mvcc/current_read.go`、`executor/locking_read.go`、`executor/locking_modify.go`、
`executor/lock_plan.go`、`executor/locking_diagnostics.go`、`parser/locking_read_test.go` 等尚未创建，
符合规划。

**已核验存在的既有路径（plan 中未加 `(new)` 的那些）全部存在**，逐项确认：

| plan 中列出的既有文件 | 存在 |
|---|---|
| `storageengine/engine.go` | ✅ |
| `storageengine/mvccadapter/adapter.go`、`iterator.go` | ✅ |
| `mvcc/transaction.go`、`merge.go`、`snapshot.go`、`store.go` | ✅ |
| `executor/access_plan.go`、`physical_binding.go`、`physical_select.go`、`range_access.go` | ✅ |
| `executor/transaction_engine.go`、`savepoint_mvcc.go`、`session_settings.go`、`open_options.go` | ✅ |
| `executor/modify_insert.go`、`modify_update.go`、`modify_delete.go`、`mutation.go` | ✅ |
| `executor/mutation_insert_modes.go`、`mutation_join_pipeline.go`、`multi_delete_pipeline.go` | ✅ |
| `executor/foreign_key.go`、`secondary_index.go`、`executor.go` | ✅ |
| `parser/ast.go`、`parser/parser.go` | ✅ |
| `server/handler.go`、`connection.go`、`metadata_browse.go` | ✅ |
| `config/config.go`、`enginefactory/default.go`、`cmd/gbaselite/main.go` | ✅ |
| `replication/node.go` | ✅ |

**发现的唯一路径级偏差（已记录，未修订 plan.md，因为它是描述性列表而非契约）**：
research.md 表格写作「`storageengine/engine.go`、`relation.go`、`mvccadapter/{adapter,iterator,diagnostics}.go`」，
其中 `diagnostics.go` 确实位于 `storageengine/mvccadapter/`，而该行行首的 `engine.go`、`relation.go`
属于 `storageengine/` 根。该行是简写，不是错误路径。

## A5. 逐条核对：data-model.md 的生命周期图

```text
B01 root (fixed ReadTS; owns GC retention)
  └── one LockOwner
        ├── statement / child / savepoint LockScopes
        ├── root-owned Grants (survive scope rollback)
        └── pending Requests → WaitEdges
root definitive end → prevent writes/merge → cleanup → release + remove graph
child MERGED/ABORTED → close scope only; no new commitTS/retention
generation/term reset → invalidate handles + wake; publication decides committed outcome
```

Phase 2 已落地的部分与图一致：`LockOwner` 一对一于 root（`manager.RegisterOwner` 以 `rootID` 唯一），
`ScopeTree` 承载 statement/child/savepoint，`CloseScope` 只返回需取消 pending 的 id、
**没有任何释放 grant 的路径**。未落地的是 grants 与 WaitEdges（US1/US5 范围），符合阶段划分。

## A6. 阻断项

**无。** 未发现需要修订既有语义、改变持久化格式或改动 WAL/Raft 协议才能继续的冲突。
Phase 3（US1）的前置条件已就绪：

- 中立 DTO、能力探测与预算计量可用（T005/T007/T010/T011）；
- owner/scope/绑定/中止意图可用（T006/T008）；
- 确定性时钟、屏障与无 sleep 的测试夹具可用（T012）；
- 兼容矩阵与公平队列**尚未**实现，且必须保持「scope 不释放 grant」「预算 fail closed」
  「同 owner 重入不自阻塞」三条本轮已用测试锁定的规则。
