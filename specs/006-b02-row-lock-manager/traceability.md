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

## A7. 第二轮修订（阻断缺陷修复）

本节记录第二轮代码审查对既有追溯结论的更正，避免上面的"尚未使用"表述被继续引用：

| 上一版结论 | 现状（`.tmp/b02r2-fix-report.md`） |
|---|---|
| A4 表：「UNIQUE NULL 不参与 unique reservation …B02 尚未使用，留待 US2/US4」 | **已部分落地**：唯一索引现在锁住 `UniqueIndex` 命名空间下 `IndexValueKey` 的**值本身**（不含 owner 后缀），非唯一二级索引锁 `SecondaryIndex` 命名空间下含 owner 后缀的物理 entry；NULL/复合/collation 语义直接复用写入方编码器。见 `executor/modify_insert.go:indexEntryLocks`、`executor/dml_locking.go`、`executor/dml_unique.go`、`executor/locking_unique_test.go`（覆盖）与 `contracts/sql-locking.md` §唯一值 reservation。 |
| 「`ReadCurrent` 验证 grant、epoch」仅为规划 | **已实现且强于规划**：owner + epoch（Generation/Term 精确相等）+ grant 自带组件对 space/key 的覆盖 + manager 实时 applied S/X claim 四项校验，读前后各一次；裸 owner ID 入口 `ReadCurrentLockedForOwner` 已删除。见 `mvcc/current_read.go`、`storageengine/mvccadapter/locking.go`、`contracts/current-read.md` §Grant verification。 |
| 「DML 不取行锁 / INSERT 不产生阻塞」 | **已变更**：UPDATE/DELETE 目标行 X 锁与 INSERT 的唯一值 reservation 均已启用，冲突语句等待并由 holder 终态唤醒（`executor/locking_dml_wake_test.go`、`executor/locking_unique_test.go`）。与此相关，两处**旧断言依赖"不阻塞"**的测试已按新语义改写：`executor/mvcc_engine_test.go`（第二个 UPDATE 改为并发驱动）与 `server/mvcc_autocommit_test.go`（同行并发 UPDATE 改为同表 DDL 触发提交冲突）。 |
| `LockHandle` 的状态读取 | **已同步化**：`requestState` 用自带 mutex + compare-and-set 保证唯一终态、无回退、并发查询安全（`lockmanager/handle_state_test.go`）。**真实 `-race` 证据仍缺失**（本机无 C 工具链），T067 未勾选。 |
| 「queue advance 遗漏导致等待后不唤醒」（第一轮记录） | **更正为测试串行假象**：释放 holder 的 COMMIT 与阻塞的 UPDATE 在同一个 goroutine，`ReleaseOwner` 从未执行。trace 与确定性用例见报告 §4。 |

## A8. 第三轮修订（Lock Manager 两条契约缺陷）

| 上一版结论 | 现状（`.tmp/b02r3-fix-report.md`） |
|---|---|
| `claimSatisfiedByOwner`/`claimSatisfiesOrQueued`「计数 applied grants 与自身排队请求的 claim」 | **已更正**：重入/已持有只看 **applied** claim；排队中的 claim 不再是"已持有"。重复申请同一排队保护改为**加入该 request**（WAIT、同 RequestID、同一终态），不再返回 REENTRANT；`TryAcquire` 在该情况返回 `ErrConflict`。见 `lockmanager/manager.go`（`claimSatisfiedByOwner`、`joinableRequestLocked`、`bundleCoversRequest`）与 `contracts/lock-manager.md` §Claim state and grant identity。 |
| 公平队列 `waitsBehindOwner`「同 owner 较早的相同 mode 排队即 adopt」 | **已更正**：只在较早 claim 已 applied 时 adopt；较早请求仍在排队时本 entry 继续等待（保留自身 claim，故不受其取消影响），且 adopt 时撤销本请求排队登记的 claim（消除幽灵 claim）。判定只看本 entry **之前**的队列项。见 `lockmanager/queue.go`（`adoptableOwnClaim`、`claimAppliedFor`）与 `grantLocked` 的 adopted 分支。 |
| `grantForHandle`「从 owner 的 granted resources 重建 Grant」 | **已更正**：等待获授的 grant 现在从该 request 自己记录的终态能力读出（`GrantID`/`OriginScopeID`/`Components`/`OwnerID`/`Generation`/`Term` 全部属于该 request），不再拼接同一 root 的其他锁、不再用 RequestID 充当 OriginScopeID；owner 结束或被 epoch 替换后 fail closed。见 `lockmanager/manager.go`（`requestState.recordGrant`、`completeBundleLocked`、`grantForHandle`）与 `storageengine/mvccadapter/locking.go`。中立 `LockGrant` 顺带移除了从未被读取、且被填成"发放时刻"的 `Deadline` 字段（data-model 的 Grant 字段列表本就没有它）。 |
| `TestB02ReentrantUpgradeWhileWaitingAdoptsThePendingClaim` | **改写为** `TestB02RepeatUpgradeWhileWaitingJoinsTheQueuedRequest`：旧断言（重复升级返回 REENTRANT）正是本轮修复的缺陷，现断言返回 WAIT 且加入同一请求，同时保留"只产生一个请求、两次 release 后交给下一个 owner"的原有约束。 |

## A9. 第四轮修订（共享等待请求的终态广播与取消隔离）

| 上一版结论 | 现状（`.tmp/b02r4-fix-report.md`） |
|---|---|
| `LockHandle.result`（容量 1 的 channel）+ `LockRequest.Notify` 单次发送 | **已移除**：一个 request 可有多个等待者，单值 channel 只能唤醒其中一个，其余会为已决定的结果继续阻塞（复现：两个等待者同时阻塞后只有创建者返回，加入者 5s 看门狗超时）。现终态由 request 自带的 `requestState` 广播：状态写入后关闭 settled channel，**全部**等待者被唤醒并各自读取同一 status/grant；`Status`/`Wait`/`WaitStatus`/`WaitForGrant` 可重复读取且一致。见 `lockmanager/manager.go`（`requestState.settled`/`wait`）、`queue.go`（`setStatus`）、`types.go`（删除 `Result`/`Notify`）。 |
| `waitWithDeadline` 在 context 结束时无条件 `withdrawOnContextEnd` | **已更正**：只有 **origin handle**（创建该 request 的调用者）的 context 结束才撤销共享 request；加入者的 context 结束只结束它自己的等待并返回其 ctx 错误。`Cancel` 对加入者 handle 无操作（返回 request 当前状态），加入者 scope 关闭不影响它未创建的 request。复现（守卫关闭时）：加入者取消导致共享 request 变为 `CONTEXT_CANCELED`。见 `contracts/lock-manager.md` §共享等待。 |
| 「多次等待同一 handle 由 ctx 取消兜底」 | 该兜底已不再需要：`TestB02HandleWaitAndStatusAgreeUnderConcurrency` 的注释与断言随之更新为广播语义（grant 与 ctx 同时到达时仍必须报告 GRANTED）。 |
| 新建测试 | `lockmanager/shared_wait_test.go`：2/10 个等待者同时获授、加入者取消隔离、创建者取消撤销、加入者 `Cancel` 无操作、五种终态（grant/cancel/timeout/deadlock/epoch）全部唤醒全部等待者、终态不被消费、三种 scope/root 生命周期；每个用例都断言无 goroutine 遗留（全部 outcome 被接收）与无锁资源/预算残留。 |

## A10. 第五轮修订（Joiner Scope 生命周期 + T018/T019）

| 上一版结论 | 现状（`.tmp/b02r5-fix-report.md`） |
|---|---|
| 加入者 scope 关闭只影响 `Cancel`/context，scope 关闭"不影响它未创建的 request" | **已补全**：只结束该加入者自己的等待（`ErrScopeUnknown` → 中立 `ErrConflict`），request 与其它等待者不受影响；新增 `requestBundle.joins`+`Manager.closeJoinsLocked`+`LockHandle.scopeDone`，等待路径对"已决定终态"优先。复现：加入者 scope 关闭后其调用者永不返回（`.tmp/b02r5-before-scope.txt`）。 |
| T018 未执行（`mvccadapter/locking.go` 不存在、`enginefactory` 未装配） | **已完成**：每 engine 一个 manager 装配、可选能力、root/child/scope 映射（子 scope 现建在父 scope 子树内）、身份与写路径一致、engine 隔离、中立接口不泄漏 backend 类型、grant 完整、Close/Rollback/EpochReset 无残留（`Manager.Closed()` 已导出可断言）。 |
| T019 未执行（raw client 不参与锁） | **已完成**：`mvcc.Tx.Put/Delete` 在暂存前取该 key 的 Record X 并等待；`Get` 保持快照读；身份收敛为 `(Generation, Space, Key)`（TableID/IndexID 是标签不参与身份），故 raw 写与 SQL 锁互为排他。T024 的 Gap/II 与 T026 的 binding 登记仍明确未做。 |
| `ResetEpoch` 归零 owner/图/请求 | **发现并修复**：它从不退还 ledger 计费（退款只在 `ReleaseOwner`），epoch 变更后仍计着已释放组件；现对每个 ended owner 调 `ledger.ReleaseOwner`（解锁后调用，避免嵌套 registry 锁）。 |
| 既有测试只覆盖"无锁 raw 写" | 四处既有测试的**断言对象**随新语义更新（不是放宽）：absent/tombstone 的 current read、`TestB02LockingDoesNotChangeConflictDetection`、diagnostics 冲突计数、`testkit` `WriteAndPointGuards`；提交期乐观冲突的断言全部保留，只是现在发生在等待之后。 |

## A11. 第六轮修订（US2 区间与插入保护：T021/T024/T025/T026/T027）

| 上一版结论 | 现状（`.tmp/b02r6-fix-report.md`） |
|---|---|
| 「Gap/II 兼容模型已实现（T023 已完成）」 | **实现与契约不符（已修复）**：`componentsConflict` 只是纯函数；`lockTable` 以 `ResourceKey` 为键且该键包含 bounds，`inspectLocked` 只查请求自身的表项，因此一个 `(a,z)` 的 Gap 与"在 m 插入"的 II 落在两个表项、**永不比较** ⇒ FR-003/004/005 在生产路径上无效。修复：interval 型身份（Gap 与 II）以 `(Generation, Space)` 域为 `ResourceKey`，同 namespace 的 Gap/II 共用队列、逐 claim 按 bounds 判定包含。 |
| 「II 是 KindRecord+ModeII 的独立组件」 | **插入声明改为结构约束**：II 必须与同键 record X 同 bundle（`ErrInsertIntentionRequiresRecord`/`MissingRowClaims`）。II 答"插入位置是否被 Gap 保护"，record X 答"键是否被占用"（FR-005）。 |
| T024 未执行（raw 只取精确键记录锁） | **已完成**：键缺失/tombstone（含本事务 Delete 后重插）取 II+X；存在键只取 X；取锁先于暂存、等待后重校验存活（generation 变化即拒绝暂存且无 staged 残留）。 |
| T025/T026/T027 未执行 | **T025**：`executor/lock_plan.go` 六类访问计划（record-only/insertion/range/whole-domain/refused/none）+ `rangeIsProven` + 拒绝而非静默不加锁 + `EXPLAIN Extra` 标注；INSERT 侧实际申请插入声明。**T026**：PK/UNIQUE/secondary/复合/NULL/collation 身份与写路径字节一致（unique 无 owner 后缀、secondary 有 owner 后缀）。**T027**：8 项逐条覆盖（见 tasks.md §第六轮），含真实 SQL 的区间内等待/区间外不等待、Next-Key 原子 bundle、并行插入、取消零残留、失效事务不再暂存、失效 scope 写入被拒。 |
| 未实现范围（README） | 已同步：说明插入声明与 EXPLAIN 标注**已生效**；同时明确**没有任何 SQL 语句会申请 Gap/Next-Key**（锁定读属 Phase 5，DML 范围候选仍只锁实际改动的行），EXPLAIN 对锁定读只标注计划并注明尚未执行。 |

## A12. 第七轮修订（Gap / II 的资源身份、去重、重入、请求合并）

| 上一版结论 | 现状（`.tmp/b07-fix-report.md`） |
|---|---|
| 「interval 型以 domain 为 `ResourceKey`」（第六轮） | **方向对但混用了两种身份**：域键只应回答"哪些 claim 必须互相比较"，却被同时当作"这是不是同一把锁"。后果：两个不同 Gap 被去重成一把；`[Gap,II,X]` 因 modeRank 丢掉 Gap；持有 `Gap(a,m)` 后申请 `Gap(n,z)` 被答 REENTRANT；排队的 `Gap(a,m)` 被新申请 `Gap(n,z)` 错误 Join/adopt；`Release` 一个 Gap 释放整个域；`HeldMode` 无法回答单把锁。 |
| （本轮新增）三层身份 | **已分离**：Conflict Domain（`ResourceKey`，锁表键，interval 型 = `(Generation, Space)`）／Claim Identity（`ClaimKey`/`SameClaim`，interval 型含 mode 与 bounds/键）／Coverage（同一 ClaimKey + `upgradeNeeded` 为假）。去重、重入、请求合并、adoption、单资源释放、`HeldMode`/`OfferedMode`、ledger 计费键统一改用 ClaimKey；兼容性仍由 `componentsConflict` 按 bounds 判定（兼容 ≠ 覆盖）。 |
| （本轮第二个缺陷）同一 request 在域内两个 claim | **已修复**：`applyClaim`/`dropClaim`/`claimAppliedFor` 原本只按 request 指针配对，导致第二个组件永远 pending（grant 报成功但 `HeldMode` 为假；排队路径 request 报 GRANTED 而 claim 未应用；`Release` 漏退款）；现按 (request, 精确 ref) 配对。 |
| （本轮第三个缺陷）同 bundle 兄弟被当作 older waiter | **已修复**：`waitersAhead`/`olderBlockers` 会把同一 bundle 的 `Gap(a,m)` 当作 `II(b)` 的 older waiter（包含判定为真），使"同域两组件"的请求永久排队且图上无边；现按 `sameBundleRequest` 跳过自身兄弟。 |
| 证据 | 新增 `lockmanager/claim_identity_test.go`（A–H 八项）与 `resource_test.go: TestB07ClaimIdentityIsExactInsideOneDomain`；**修复前 6/8 失败**（`.tmp/b07-before.txt`），**修复后全部通过**（`.tmp/b07-after.txt`）。`executor/lock_plan_test.go: TestT025SQLInsertRespectsAProtectedRange` 按用户要求重写为"T1 持锁期间 T3 必须完成"，替换原先在释放后才跑范围外插入的时序。T021–T027 的既有验收在修复后仍全部通过，完成标记保持。 |

## A19. 第十七轮（Linux CI 竞争时序修复、补锁公平性与死锁环、Phase 5 主清单闭环）

| 上一版结论 | 现状（`.tmp/b17-phase5-report.md`、`.tmp/b17-acceptance.txt`） |
|---|---|
| Linux CI 在 `TestP5AbsentKeyProtectionUnderConcurrency` 报 `claim k042: lock wait timeout exceeded` | **根因确证并修复**：Inserter 在 goroutine 中先行抢占 k042 的 II+X，主 goroutine 的 Scanner 请求 Record X 被阻塞入队等待 Inserter；而 Inserter 的 Rollback 原被置于 Scanner 之后，造成测试自身的循环等待导致 5s 超时。修复：① 重写并发竞争测试，Inserter 抢占成功后执行实际 Put+Commit 正常终结事务并释放锁，Scanner 获授后读取最新提交行；② 补充三组确定性 barrier 测试：`ScannerFirst`、`InserterFirst_AcquisitionOnly_Rollback`（仅申请后回滚，Scanner 读到 absent 并保护 Gap）、`InserterFirst_PutAndCommit`（真正提交，Scanner 读到已提交行不取 Gap）；③ 区分“Acquisition 成功”与“真正 Put/Commit 成功”；④ 断言实际数据、锁状态、等待图与 `t018RequireNoResidue` 零残留。多次执行 `-count=20` 100% 稳定通过。 |
| Lock Manager 补锁公平性与等待图 | **确证并修复图边混淆**：Root A 持有 X(R)，Root B 申请原子 bundle X(R)+X(I) 因 R 等待；A 继续申请 X(I)。经测试确认：① B 对 I 的 claim 是 `pending: true`，且持有 ledger reservation，非 applied；② A 被 B 的较早 waiter 阻塞（`LockOpWait`）；③ `rebuildGraphNowLocked` 原在 `ownerBlocksWith` 中把 pending claim 也当成阻塞 claim，导致 A 对 B 在 I 上错误生成了 `EdgeHolder` 边（B 实际只是 waiter 未持有 I）。修复 `ownerBlocksWith` 过滤 pending claim，等待图精确生成：B→A (HOLDER on R) 与 A→B (OLDER_WAITER on I)，构成完整死锁环；④ `DetectDeadlock` 精确识别环并以 registration ordinal 最大者（B）为 victim 终止（`ErrDeadlock`），A 立即解除等待获授 X(I)，且 A 原持有的 X(R) 绝不释放；⑤ 补充升级死锁、同事务 reentrant、新索引插入意图（Gap vs II）与多资源 Bundle 测试，全部通过。 |
| Phase 5 tasks.md 主复选框长期未同步 | **主清单已全部勾选闭环**：T028、T032、T034、T035、T036 主复选框全部更新为 `[x]` 并补充实际测试证据；清理 Phase 6 中过时的第一轮无锁描述，明确当前 DML 三分支已全量接入 `dmlLockScope` 与锁后版本校验；Phase 6 中已完全验收的 T039、T042、T044 勾选 `[x]`，其余未全量覆盖项如实保持 `[ ]`。 |

## A18. 第十六轮（锁后 DML 目标的版本验证：ABA、级联目标删除、主键/唯一 owner 变化）

| 上一版结论 | 现状（`.tmp/b16-phase5-report.md`、`.tmp/b16-acceptance.txt`） |
|---|---|
| 锁后目标校验用 `bytes.Equal(快照行, 当前行)` | **改为版本校验**：新增 `staleTargetObservation`，按 `Source / HasChange / ChangeTS vs Txn.Snapshot()` 判定；Overlay（本事务/祖先暂存）永不判过期以保留 read-your-writes。**修复前**：ABA（`(1,7)→8→7`）的 upsert 返回 `<nil>`、主键删除后重插返回 `<nil>`、唯一值 ABA 放行、级联目标被删除时**跳过校验**（可能复活已删除行）、级联 ABA 放行。**修复后**五者均返回可重试 `ErrConflict` 且终态无覆盖/无复活；`TestP2UpsertAcceptsItsOwnStagedOverlay` 证明显式保留自身 Overlay 的合法更新。 |
| `lockCascadeTarget` 使用 `context.Background()` | **改为语句 Context**（取消可及时终止），并复用同一版本校验。 |
| 预期外的仓库状态 | 本轮开始时工作树**丢失了 274 个已跟踪测试文件**（仅 `executor/` 下 101 个测试文件只剩 1 个）；已用 `git restore .` 恢复（仅测试文件受影响、内容未改动），随后才执行验收。 |
| CI（Ubuntu MVCC transaction race regression） | **无法读取 CI 日志/堆栈**（本机无网络）⇒ 不区分"既有不稳定"与"真实数据竞争"，也不声明通过；可执行替代：`./mvcc -count=5`、`./executor/... -count=5`、`./lockmanager/... -count=5` **全部通过且未复现**；`go test -race` 因无 C 工具链**未执行**。 |

## A17. 第十五轮（P1：过期目标不 Rebase + 唯一 reservation 释放语义 + 真正等待后唤起）

| 上一版结论 | 现状（`.tmp/b15-phase5-report.md`、`.tmp/b15-acceptance.txt`） |
|---|---|
| T042 的 upsert 用 `ReadCurrent` 的最新行继续计算 | **两个 P1 均修复**。① 过期目标**静默 rebase**：修复前 `INSERT items VALUES(1,7) ON DUPLICATE KEY UPDATE v=9`（T2 readTS 早于 T1 提交，T1 已改 v=8）**成功并覆盖** T1 的提交值——违反 FR-017/FR-018 与 current-read.md §A05（"newest target change > readTS 报冲突，不用新 row 重算 SET"），并靠 `ReadCurrent` 记下的 baseline 绕过旧快照 DML 验证。修复后锁下只**比对**快照行与当前值：不同 ⇒ 可重试 `ErrConflict`，T1 的值保留。② **唯一 reservation 已释放而快照仍有旧冲突**：修复前 T2 等待 T1（v 7→8）提交后把 `id=1` 改成 `v=9`（终态 `1,9;`），v=7 丢失。修复后**冲突是否存在**按当前 reservation owner 重算（释放或换主 ⇒ 可重试 `ErrConflict`，非永久 1062、非错误更新 id=1），重试后正确插入 `(2,7)` 且唯一索引一致。 |
| T044 的"等待后重验"只有超时→重试形态 | **已补真正唤起**：`TestP1CascadeGrantedAfterTheWaitRevalidates` 用 `newDMLWaitBarrier` 证明同一条 SQL **入队后被受让**，受让后的派生写因目标过期返回可重试 `ErrConflict`（修复前是**永久**外键错误），重试后级联成功且保留他人对该行的修改。 |
| CI 失败（MVCC transaction race regression） | **无法读取 CI 状态**（本机无网络，取不到 Actions 日志），因此不区分"既有不稳定"与"真实数据竞争"；可执行的替代证据：`go test ./mvcc/ -count=5`、`./storageengine/... ./server -count=3`、`./executor/... -count=5`、`./lockmanager/... -count=5` **全部通过**，本机重复运行**未能复现**该失败；`go test -race` 仍因无 C 工具链**未执行**。 |

## A16. 第十四轮（ON DUPLICATE 外键级联、新旧索引锁、等待后重验）

| 上一版结论 | 现状（`.tmp/b14-phase5-report.md`、`.tmp/b14-acceptance.txt`） |
|---|---|
| T042 的 ON DUPLICATE KEY UPDATE 只锁旧行后直接写入 | **三个缺陷已修**：① 未调用 `applyForeignKeyActions` ⇒ `INSERT parent(id=1) ON DUPLICATE KEY UPDATE id=2` **不触发** `ON UPDATE CASCADE`（等价 UPDATE 会触发）；② 只申报旧行/旧索引资源（组件级证据：捕获到的请求只有旧唯一值 `uq_v` 的 II+X）；③ 用**等待前**的行求值并写入 ⇒ 覆盖等待期间他人提交的修改。修法：先锁行 → 在锁下用 `readLockedValue` 重读当前行 → 再求值 → `lockCascadeTarget(..., currentRow, updated)` 申报新主键与全部新索引项（`indexEntryLocks` 负责 II 与 Record X 成对）→ 执行参照动作 → 写入；等待期间目标行消失时返回 `errUpsertTargetGone`，调用方按普通插入处理。 |
| T039/T044 的派生写等待后重验无测试 | **已补测**：`TestT044CascadeRevalidatesTheChildItWaitedFor`（等待后不覆盖他人的另一列修改）、`TestT044CascadeRespectsTheCurrentChildState`（子行被删除/改指向后派生写不得复活或覆盖）、`TestT044MultiLevelFailureRollsBackTheStatement`（多层任一步失败 ⇒ 语句整体回滚，父/子/孙均不变）、`TestT042DuplicateVariantsDoNotSwallowLockErrors`（REPLACE/IGNORE/ON DUPLICATE 不得吞掉锁等待超时）。 |
| T042 的参照动作覆盖（ON UPDATE SET NULL / RESTRICT / 多层） | **已覆盖**：`TestT042OnDuplicateUpdateHonoursEveryUpdateAction`、`TestT042OnDuplicateUpdateCascadesAcrossLevels`、`TestT042OnDuplicateKeyUpdateRunsForeignKeyActions`（与等价 UPDATE 对照）。**未覆盖**：显式 `NO ACTION` 与**自引用**外键。 |
| Phase 6 历史文字"尚未接入任何 DML 锁" | 已按用户要求保留历史记录并在同段明确**当前实现状态**（三条写入路径均开语句级 `dmlLockScope`、派生写经 `lockCascadeTarget` 复用既有加锁器）。 |

## A15. 第十三轮（Phase 5 质量门禁 + Phase 6 部分实施）

| 上一版结论 | 现状（`.tmp/b13-phase5-report.md`、`.tmp/b13-acceptance.txt`） |
|---|---|
| Phase 5 尚缺"KeysAround 两次探测之间并发 INSERT/DELETE"与"Scope Close 后无残留"两项门禁 | **已补齐并通过**：`TestP5StaleBoundsAfterAConcurrentInsert/Delete`（陈旧边界只会更宽 ⇒ 保守安全；缺席键位置恒受保护；不会悄悄扩展到整个命名空间）、`TestP5AbsentKeyProtectionUnderConcurrency`（8 轮安全不变量：插入成功 ⇔ 读者已观测到该行）、`TestP5ScopeCloseLeavesNoPendingRequestOrLedgerCharge`（真实计数：PendingRequests/WaitEdges/Ledger.Waiters/Edges 归零、账本回基线、锁表不增长、**无幽灵等待者**）。 |
| tasks.md 的 Phase 6 段落记录"DML 无锁、目标锁刻意停用、INSERT 不阻塞" | **该描述已过时**：两条固定负向测试（`TestB02UpdateTargetLockingIsNotEnabled`、`TestB02ConcurrentInsertSameUniqueKeyIsStillAccepted`）已不存在，目标锁已启用并被 `locking_unique_test.go` 正向用例覆盖；段落已改为"历史记录（已被后续提交取代）"并保留原文。 |
| T044「FK CASCADE/SET NULL 派生写入不得绕开锁管理器」 | **已实现**：`applyForeignKeyActions`/`cascadeForeignKeyAction` 接收语句锁作用域，`lockCascadeTarget` 复用 `indexEntryLocks`/`acquireRowLocksForKey`（旧身份 + 替换的 unique reservation），无第二套加锁器；`TestT039CascadeDeleteClaimsTheChildRowAndItsIndexes` 用生产 seam `dmlLockRequestHook` 断言派生写真的申报了子行主键与唯一/二级索引项（改动前该处**没有任何**被捕获的请求），`TestT039CascadeDeleteWaitsForAConcurrentLockingRead`、`TestT039SetNullDerivedWriteWaitsForAConcurrentLockingRead` 用真实双 Session 断言派生写等待并核对最终数据。 |
| T042「REPLACE / ON DUPLICATE KEY UPDATE 锁后判重」 | **已补齐取锁**：`replaceDuplicateInsertRow`/`updateDuplicateInsertRow` 经语句作用域申报被删除/重写的行及其索引项；`TestT042ReplaceClaimsTheRowItRemoves`（组件级）与既有的 `locking_unique_test.go` 正向用例共同覆盖。 |
| Phase 5 的 CI / race 结论 | **仍无可报告的 CI 结果**：本机无网络（取不到 Actions 结果）、无 C 工具链、无 WSL 访问权限、Docker 守护进程未运行 ⇒ `go test -race` **未执行**（不写成通过）。 |

## A14. 第十一轮（Phase 5 收尾：T034/T035/T036 验收）

| 上一版结论 | 现状（`.tmp/b11-phase5-report.md`、`.tmp/b11-acceptance.txt`） |
|---|---|
| T035「预算粒度是语句级而非每次等待」（A13 遗留） | **已修复**：`Scope.WaitBudget`（duration）+ `LockingTxn.BeginLockScopeBudget` + `requestDeadlineLocked = min(请求开始等待时刻 + budget, scope context deadline)`。**同时修掉一个真实缺陷**：scope id 每 owner 从 1 开始，旧查找遍历全部 owner 的 scope 树，会把同号他人 scope 的预算/期限当成自己的（按 owner 解析，`TestB09BudgetIsReadFromTheRequestingOwnersScope` 回归）。`TestB09LockWaitBudgetAppliesToEachWaitNotToTheStatement` 的 Skip 已删除，虚拟时钟下 0.6s+0.6s 在 1s 预算内均成功，超预算仍超时。 |
| T032「键列表取自本事务快照，快照后提交的新键不会被枚举」 | **已修复（`09b3be2`）**：键列表走 `mvcc.Tx.ScanLatestRange`（store head 发布层 + 本事务/祖先 stage，不记 range dependency），SQL 侧 `TestT034LockingScanDiscoversRowsCommittedAfterTheSnapshot` 端到端锁定；`iterator.go` 中过时的快照枚举注释已同步删除。 |
| T034「SQL 路径未接线」 | **已实现**：`executor/locking_read.go`（点路径 / 唯一索引路径 / 范围与 whole-domain 路径），X/IX 与 S/IS，获锁后重读并重评 WHERE，删除行不返回，主键范围不再被降级为整表回退（`provenPrimaryInterval`），显式事务持锁至 COMMIT/ROLLBACK、autocommit 随 root 结束释放；**明确拒绝** JOIN/派生表/CTE/视图/聚合/DISTINCT/标量查询与二级索引访问，NOWAIT/SKIP LOCKED 由 parser 拒绝。 |
| T034「多索引访问、共同主键行锁」 | **已实现并验收**：唯一索引路径先取该值的 reservation（与写入路径同一资源），再由 entry value（拥有该值的主键）取**共同主键行**并重读；`TestT034UniqueIndexLockingReadSharesTheWritersResources`（不同唯一值不被阻塞、同值被阻塞、提交后为永久 duplicate）与 `TestT036LockIdentityMatchesTheDMLPath`（组件与 DML 逐字段相同）。**二级索引**（非唯一）仍 fail-closed 拒绝。 |
| T036 双 Session 矩阵 | **本轮补齐并验收**：20 项 SQL 级测试（`executor/locking_read_test.go` + `locking_read_matrix_test.go`），含等待后最新值、共享读兼容/排斥写、快照不变、快照后新行可见、删除行不返回、缺失键插入位置保护、区间内插入等待而区间外完成、per-key baseline、清理路径（timeout/cancel/rollback/disconnect/savepoint）、等待期间无关写入可提交、锁身份与 DML 一致；不依赖 Sleep（wait hook 或短预算）。 |

## A13. 第八轮（Phase 5/US3：T028、T032、T035 完成；T034、T036 未完成）

| 上一版结论 | 现状（`.tmp/b08-phase5-report.md`） |
|---|---|
| `NewCurrentIterator` 返回 "not implemented"（fail-closed） | **已实现（T032）**：lock-aware 有限页迭代器——先取访问区间，再分页列键（底层 range scan 每次 yield 前释放 bbolt 读事务），逐键取 record claim 后重读最新有效版本；列表后被删除的键跳过；进度按上次列出的键排他续扫；Close/cancel/epoch 无残留。中立层新增 `CurrentScanRequest.Mode`（S/X，空或区间模式拒绝）。已知限制：键列表取自本事务快照，快照之后提交的新键不会被枚举（受保护范围内的插入仍被区间 claim 阻止）。 |
| Current Read 适配器契约无独立验收 | **T028 完成**：`current_read_contract_test.go` 9 个黑盒用例覆盖 7 项要求（身份/伪造 grant/父子 overlay/普通读不取锁/能力缺失/observation meta+baseline/epoch 失效）。 |
| 「1205 已在协议层映射」 | **此前不存在**（会落到 1064 兜底）：本轮在 `server/mysql_server.go` 增加 `ErrLockWaitTimeout` → **1205**。 |
| 「会话锁期限可在 1..86400 生效」 | **此前只能缩短到 ≤50s**（`requestDeadlineLocked` 把 scope deadline 夹到默认值）：本轮取消夹取，`mvccadapter.BeginLockScope` 传入 ctx deadline ⇒ 1..86400 双向生效；服务端 `SET SESSION innodb_lock_wait_timeout` 与 `@@innodb_lock_wait_timeout` 也已接线（此前仅 executor 路径）。 |
| SELECT FOR UPDATE/SHARE | **仍未接入（T034 未完成）**：接入点与所需改动见报告 §5；不做半接线（避免"旧快照候选直接返回旧结果"或降低既有可用行为）。T036 的 A–L 依赖它，同样未开始。 |


## A14. Phase 6（US4 DML 与唯一索引）完成：T037–T046 全部勾选

| 任务 | 设计 | 实施位置 | 测试 | 证据 |
|---|---|---|---|---|
| T037 | 锁后按版本校验 DML 候选 | `executor/locking_modify.go` | `executor/locking_modify_test.go`（新，15 例）；`executor/locking_dml_wake_test.go`、`executor/mvcc_engine_test.go`（断言改为早冲突） | `go test ./executor/ -run TestT037` 全通过 |
| T038 | 主键与 UNIQUE reservation 的锁后重判 | `executor/dml_unique.go`（新增主键分支）、`executor/modify_insert.go` | `executor/locking_unique_test.go`（＋6 例） | `go test ./executor/ -run TestT038` 全通过 |
| T040 | parent 目标 current verification、old/new 资源收集 | `executor/locking_modify.go`（`claimModifyTarget`/`claimModifyReplacement`/`verifyReplacementUniqueValues`） | 同上 T037/T038 用例 | 过期目标返回 `ErrConflict`；`!Exists` 墓碑同样冲突 |
| T041 | 锁验证接入四条 DML 路径 | `executor/modify_update.go`、`modify_delete.go`、`mutation.go`、`mutation_join_pipeline.go`、`multi_delete_pipeline.go` | `TestT037JoinUpdateTargetIsVerified`、`TestT037MultiTableDeleteTargetIsVerified` | UPDATE JOIN 与多表 DELETE 均申报并被校验 |
| T043 | 统一 old/new row 与 index 锁编码 | `executor/modify_insert.go`（`indexEntryLocks`）、`executor/secondary_index.go`（写路径，未改） | `executor/locking_index_claims_test.go`（新） | 语句实际 claim 与 `storage.IndexValueKey`/`secondaryEntry` 复算键逐项相等 |
| T045 | raw 与 SQL 同资源、无旁路、replay 不等待 | `mvcc/locking.go`（未改） | `mvcc/locking_client_write_test.go`（新） | raw Delete 等待共享读；`Store.Apply` 期间注入时钟未 armed |
| T046 | A05 全形状回归 | `executor/mutation.go`（未改语义） | `executor/locking_modify_pipeline_test.go`（新） | 各形状均申报表意向；affected/LastInsertID/dedup 不变 |
| Part 5 | 死锁回归补强（不改生产逻辑） | — | `server/mysql_deadlock_error_code_test.go`、`lockmanager/deadlock_edges_test.go`（新） | 1213/1205 映射含 `errors.Join` 包装；两 root 混合环 `EdgeHolder`+`EdgeOlderWaiter` 且 victim 退出后归零 |

**FR 覆盖增量**：FR-005/014/015/016/017/018/026/029/030/032 在本轮获得 SQL 端到端验收。

**仍未覆盖**：FR-019/022（T055 复制与提交矩阵）、FR-028 的 server/diagnostics 面（T060–T064）、
FR-033 的质量门（T065–T072）。`UPDATE`/`DELETE` 的范围候选仍不锁整个范围（T025/T027 未完成）。
## A15. Phase 6 并发边界复审（P1/P2 确认为缺陷并修复，基线 032b2bc）

| 编号 | 结论 | 实施位置 | 测试 | 证据 |
|---|---|---|---|---|
| P1 过期目标的 Root 重试 | **缺陷已修复**：错误说"retry transaction"但事务仍可提交，会发布同一快照算出的其余写入 | `executor/stale_target.go`（新）、`executor/transaction_engine.go`、`executor/savepoint_mvcc.go`、8 处过期判定点 | `executor/stale_target_retry_test.go`（新，8 例）；`executor/locking_dml_wake_test.go`、`executor/mvcc_engine_test.go`、`executor/locking_modify_test.go` 断言更新 | 修复前 `id=2=200` 被提交；修复后 COMMIT 被同一冲突拒绝、`id=2=20`；连续操作被拒与 BEGIN 重置正常 |
| P2 既有索引项的 II | **缺陷已修复**：身份未变的索引项也申报 Insert Intention，被只挡新键的 Gap 错误阻塞 | `executor/modify_insert.go`（`indexEntryIdentity`/`indexEntryClaim`/`indexReplacementLocks`）、`executor/locking_modify.go`、`executor/foreign_key_actions.go` | `executor/locking_replacement_entries_test.go`（新，3 组 13 例） | 修复前 2s 超时；修复后 18ms，且迁入被保护区间的项仍被阻塞 |
| P3 外键级联子表主键漏行 II | **缺陷已修复**：子表主键迁移时仅申报 Record X 漏申报 II，绕过覆盖子表新键的 Gap 锁 | `executor/foreign_key_actions.go`（`lockCascadeTarget` 直接复用 `claimModifyReplacement`） | `executor/locking_cascade_gap_test.go`（新，2 例） | 修复前子表行空间漏 II 绕过 Gap；修复后子表主键迁移真实被 Gap 阻断，WaitEdges 确认等待，放开后正确提交 |

**FR 影响**：FR-017/FR-018 的"锁后过期目标要求 root 重试"至此有可执行语义；FR-003/FR-005 的
"Gap 只阻挡新键"在 UPDATE 路径上成立（此前在 UPDATE 上被违反）。

**仍未覆盖**：T060–T064（Phase 8 诊断与确定性验收）、T065–T072（Phase 9 质量门禁与发布验收）；
`UPDATE`/`DELETE` 的范围候选仍不锁整个范围（T025/T027）。

## A16. Phase 7（US5 生命周期、复制与耐久性）实施与闭环（T052–T057 全部完成）

| 任务 | 目标 | 实施位置 | 自动化测试 | 证据与验收结论 |
|---|---|---|---|---|
| **T052** | Statement Scope、超时回滚、会话断连与零残留 | `executor/savepoint_mvcc.go`、`executor/transaction_engine.go`、`server/mysql_server.go` | `executor/locking_session_lifecycle_test.go`（新） | 会话异常断连（CloseSession）立即释放持有的锁并唤醒等待者；SAVEPOINT 回滚释放语句层但在 COMMIT 完整保留 root 锁；RELEASE SAVEPOINT 合并并在 ROLLBACK 清理，LockTableSize 与 WaitEdges 归零 |
| **T054** | Leader Barrier、Term Fence 与 Loss Wake | `mvcc/locking.go`、`replication/node.go`、`storageengine/mvccadapter/locking.go`、`storageengine/mvccadapter/iterator.go` | `mvcc/locking_epoch_sync_test.go`（新）、`replication/locking_epoch_test.go`（新） | `LockBinding` 跟踪 `storeTerm`；`SyncLockEpoch` 在任期变化时主动触发 `ResetLockEpoch`；`node.raft.LeaderCh()` 与 `check()` 在失去领导权时主动触发 `onLeaderLoss` 唤醒等待者并失效旧 grant（loss-wake）；`ReadCurrent` 与 `NewCurrentIterator` 接入 Leader Barrier、Term fence，并在 `currentIterator` 迭代过程中按页/行执行有界 term fence，任期移动或失主即刻中断并返回 `ErrLockInvalidated`；Command 格式无新字段 |
| **T055** | Bounded / WAL / Group / Stream / Raft 提交矩阵 | `mvcc/locking.go`、`replication/node.go` | `mvcc/locking_commit_matrix_test.go`（新）、`replication/locking_commit_test.go`（新） | 覆盖全部五条提交路径（bounded、local-wal、group、stream、Raft）；覆盖 read-only、data-writing、guard 三类 root；验证 latest > readTS 在提交期被 `ErrConflict` 拒绝，raw Apply 干预导致冲突，大事务 chunk stream 提交与取消，且不改变 B01 validator/codec |
| **T056** | 锁预算与默认配置传递、协议 1205/1213 映射 | `config/config.go`、`cmd/gbaselite/replication.go`、`executor/open_options.go`、`storageengine/engine.go`、`mvccadapter/adapter.go` | `config/locking_test.go`、`server/mysql_deadlock_error_code_test.go`、`server/locking_error_test.go` | YAML/ENV 锁内存（64MiB）与请求/等待/图边预算穿透至 `lockmanager.BudgetLimits`；0 采用默认值；MySQL 协议 1205/1213 错误包编码合规 |
| **T057** | 配置、会话变量边界与协议错误测试 | `executor/session_settings.go`、`config/config.go` | `config/locking_test.go`、`executor/locking_settings_test.go`、`server/locking_error_test.go` | 会话 `innodb_lock_wait_timeout` 边界 1..86400 校验、GLOBAL/负数/小数拒绝；Query 上下文较短 deadline 覆盖会话超时；引擎自定义预算生效；协议错误包二进制编码正确 |

**FR 覆盖增量**：FR-009/010/011/017/019/020/021/022/025/026/029/030/031 全部闭环。

**仍未覆盖**：Phase 8（T060–T064 诊断与确定性验收）与 Phase 9（T065–T072 质量门禁与发布验收）。