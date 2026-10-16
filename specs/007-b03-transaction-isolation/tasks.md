# Tasks: B03 Four Transaction Isolation Levels

**Feature**: `007-b03-transaction-isolation`  
**Status**: 65 tasks completed (Phase 1 Setup & Proof, Phase 2 Grammar & Session, Phase 3 Read View & GC, Phase 4 REPEATABLE READ, Phase 5 READ COMMITTED, Phase 6 SERIALIZABLE, and Phase 7 READ UNCOMMITTED completed). Phase 8–9 pending user authorization.
**Format**: `[TID] [P?] [USn] text — FR/SC`. `[P]` 只表示文件独立、前置依赖满足时可并行；并不授权 AI 跳过 Phase。
**Tests**: 必须先写失败测试，再实现；不能拿静态阅读等同运行验收。

## Phase sequence

| Phase | Title | Task IDs | Gate |
|---|---|---|---|
| P1 | Phase 1: Setup & Architectural Proof | T001–T008 | 每 Phase 的尾任务验收，不跨阶段 |
| P2 | Phase 2: SQL Grammar & Session State | T009–T018 | 每 Phase 的尾任务验收，不跨阶段 |
| P3 | Phase 3: Read View Foundation & GC | T019–T029 | 每 Phase 的尾任务验收，不跨阶段 |
| P4 | Phase 4: REPEATABLE READ | T030–T036 | 每 Phase 的尾任务验收，不跨阶段 |
| P5 | Phase 5: READ COMMITTED | T037–T046 | 每 Phase 的尾任务验收，不跨阶段 |
| P6 | Phase 6: SERIALIZABLE | T047–T056 | 每 Phase 的尾任务验收，不跨阶段 |
| P7 | Phase 7: READ UNCOMMITTED | T057–T065 | 每 Phase 的尾任务验收，不跨阶段 |
| P8 | Phase 8: End-to-End, Replication & MySQL Parity | T066–T076 | 每 Phase 的尾任务验收，不跨阶段 |
| P9 | Phase 9: Documentation, Traceability & RC Gate | T077–T083 | 每 Phase 的尾任务验收，不跨阶段 |

**Total**: 83 tasks / 9 phases；当前全部 `[ ]`。

---

## Phase 1: Setup & Architectural Proof

**Goal**: 发现并书面关闭底层架构冲突；先证明 safety，再写实现。

- [x] **T001** 记录 master HEAD、用户本轮授权边界和 B01/B02/A05 baseline；在 `.tmp/b03-baseline.md` 记真实 `go test ./... -count=1`、`go vet ./...` 与已有失败（未授权不得运行）。  _Trace: FR-030/031; SC-010/012_
- [x] **T002** 审计 `mvcc/{transaction.go,transaction_manager.go,conflict.go,current_read.go}` 的 root/child/readTS/obs baseline 真正调用图；写 `.tmp/b03-readview.md`。  _Trace: FR-009/010/014/016; SC-006_
- [x] **T003** 审计 `executor/{transaction_engine.go,physical_select.go,subquery_mvcc.go}` 及 `server/{handler.go,session_settings.go}` 的所有 Begin/stream/SET 路径。  _Trace: FR-006/008/010/035; SC-001/003_
- [x] **T004** 审计 `lockmanager/`、`executor/{locking_read.go,dml_locking.go,lock_plan.go}` 的 S/gap/next-key、锁等待及不支持查询 shape。  _Trace: FR-012/019/021/022; SC-005/008_
- [x] **T005** 审计 `mvcc/` stage merge/write buffer/transaction_state、GC/Restore、replication term，证明 RU 原子只读索引插入点可行。  _Trace: FR-017/018/026; SC-004/008_
- [x] **T006** 建立 `test-matrix.md` M01–M44 的 Go test 命名映射和 barrier hook 方案，禁止 sleep 作为同步。  _Trace: FR-028/029/036; SC-007/009_
- [x] **T007** 针对 B01 root 固定 pin 与 MySQL RR lazy 的改变提交决策记录 Q01–Q04，形成可核验 validator/retention 真值表。  _Trace: FR-009/014/016/024; SC-003/006_
- [x] **T008** 关闭 `checklists/clarification.md` Q01–Q10 或标记明确 blocker；静态运行 Spec Kit analyze 审核 spec/plan/contracts 一致性。  _Trace: FR-027/031; SC-011/012_

**Checkpoint P1**: 只有本阶段真实证据齐备、阻塞问题关闭且用户授权下一阶段后方可继续。

---

## Phase 2: SQL Grammar & Session State

**Goal**: 可用且不撒谎的 SQL 配置与状态。

- [x] **T009** 先在 `parser/parser_test.go` 编写支持/拒绝 `SET [SESSION] TRANSACTION ISOLATION LEVEL` 语法红测，覆盖非法 token、GLOBAL、多特征。  _Trace: FR-002/003/006/034; SC-001_
- [x] **T010** 在 `parser/ast.go`、`parser/parser.go` 增 IsolationLevel 和 scope AST，并让完整 parse 校验，不用字符串包含判断。  _Trace: FR-001/006/027; SC-001_
- [x] **T011** 在 `executor/session_settings.go`、`executor/executor.go` 添加 sessionDefault/pendingNext/activeLevel 状态与 ResetConnection 清理，单元红测先行。  _Trace: FR-007/008; SC-001_
- [x] **T012** 在 `executor/transaction_engine.go` 对显式 BEGIN、隐式 BEGIN、autocommit root 绑定 active level，失败不消费 pending，保留旧事务当前级别。  _Trace: FR-003/008; SC-001_
- [x] **T013** 在 `executor/transaction_engine.go`/session helpers 定义事务活跃期 `SET SESSION` 与 `SET TRANSACTION` 差异，禁止 root 内 NEXT。  _Trace: FR-002/003/004; SC-001_
- [x] **T014** 在 `server/handler.go` 把隔离 SET 走唯一 AST/state 路由，移除盲目 `TRANSACTION_ISOLATION` 文本早拒绝。  _Trace: FR-006/027; SC-001_
- [x] **T015** 在 `server/session_settings.go` 使 `SELECT @@SESSION/@@GLOBAL/@@tx_isolation` 和 SHOW VARIABLES 来自真实 state/default。  _Trace: FR-005; SC-001_
- [x] **T016** 在 `server/` prepared、COM_QUERY、重置连接路径补跨连接与执行时隔离配置测试；非法 SET 不可部分修改。  _Trace: FR-006/007/027; SC-001_
- [x] **T017** 确认暂未具备级别执行 capability 时不能让已设置 RU/SER 的新事务成功开始后默默执行 RR：增加 gated explicit unsupported。  _Trace: FR-027; SC-008_
- [x] **T018** Phase 2 gate：M01–M07、M44 和 B02 session locking 相关回归；记录真实命令、changed files、README 描述。  _Trace: FR-001..008/030/031; SC-001/010_

**Checkpoint P2**: 只有本阶段真实证据齐备、阻塞问题关闭且用户授权下一阶段后方可继续。

---

## Phase 3: Read View Foundation & GC

**Goal**: 可重用、可释放、不会被 GC 破坏的隔离 read-view 能力。

- [x] **T019** 先写 `storageengine` contract 红测：旧 `Engine/Txn` 不变；没有 IsolationEngine capability 返回 Unsupported；一个 SQL statement 一个 view。  _Trace: FR-010/016/024; SC-002/010_
- [x] **T020** 在 `storageengine/isolation.go`（新）定义必要最小 optional level/read-view contract；避免 import mvcc 到 executor。  _Trace: FR-024; SC-010_
- [x] **T021** 在 `storageengine/mvccadapter/isolation.go` 绑定事务/语句 view，把 Go minimal `txn.Get` 旧调用保持不变。  _Trace: FR-009/010/024; SC-003/010_
- [x] **T022** 写 `mvcc/transaction_manager_test.go` 测试 root pin / statement pin / child/rollback/GC horizon、成功与错误路径。  _Trace: FR-016/032; SC-007_
- [x] **T023** 在 `mvcc/{transaction_manager.go,transaction.go}` 实现 generation-aware 视图 token 与 ownership/retain/release（先保留旧 API）。  _Trace: FR-009/010/016; SC-003/007_
- [x] **T024** 在 `mvcc/{store.go,range.go,maintenance.go}` 确保 view sampling atomic+GC 识别所有 live pins，Restore/epoch 使旧 view 无效。  _Trace: FR-016/026/032; SC-007_
- [x] **T025** 在 `executor/transaction_engine.go` 引入 statement context/view owner，所有 child/subquery、iterators 与 stream 使用同一个 token。  _Trace: FR-010/015/035; SC-002_
- [x] **T026** 在 `executor/{physical_select.go,subquery_mvcc.go,with_mvcc.go,derived_mvcc.go}` 清点并改接所有读入口；拒绝无法保证单语句一致的 shape。  _Trace: FR-010/027; SC-002/008_
- [x] **T027** 补视图在 LIMIT/EOF/error/cancel/disconnect/ResetConnection 下计数精确归零测试，不允许持底层视图跨锁等待。  _Trace: FR-016/022/032; SC-007_
- [x] **T028** 验证 `mvcc/conflict.go`/B02 observation 与新的 token 不混用，提交校验仍在安全边界中（详见 `contracts/validation-policy.md`）。  _Trace: FR-014/018/030; SC-006_
- [x] **T029** Phase 3 gate：M37–M39，B01 history retention/A05 parent child/B02 lock current 全过；README 同步能力/限制。  _Trace: FR-030/031; SC-007/010_

**Checkpoint P3**: 只有本阶段真实证据齐备、阻塞问题关闭且用户授权下一阶段后方可继续。

---

## Phase 4: REPEATABLE READ

**Goal**: 默认 RR 首次一致读固定视图。

- [x] **T030** 先写 M08–M12 红测，特别是 BEGIN 后首次 plain SELECT 前另一个事务已经 COMMIT 与首次空结果范围。  _Trace: FR-009/035; SC-003_
- [x] **T031** 在 `mvcc/transaction.go` + optional adapter/SQL begin/view 里实现 RR 首个 consistent read 才 pin，且第二次复用同一个 view。  _Trace: FR-009/016; SC-003_
- [x] **T032** 在 `executor/transaction_engine.go` 将普通 SELECT/read source 与 B02 `FOR UPDATE/SHARE` current 分离；锁定读不提前创建 RR consistent view。  _Trace: FR-009/013/035; SC-003_
- [x] **T033** 给 RR own writes/tombstones/savepoint/child-inherited view 加回归，保证 A05 parent-read/child-write。  _Trace: FR-015/016; SC-003/006_
- [x] **T034** 补 RR range/empty predicates、rollback/commit、GC/Restore 和长事务占用的确定性测试。  _Trace: FR-009/016/032; SC-003/007_
- [x] **T035** 核对旧 public storageengine.Txn Snapshot、`Command.Snapshot`、B01 readTS validator 兼容或有明确安全迁移证明。  _Trace: FR-014/024/025/030; SC-010_
- [x] **T036** Phase 4 gate：M08–M12+M37 通过，B02 FOR UPDATE/SHARE/CurrentRead、旧备份恢复测试通过并归档。  _Trace: FR-013/030/031; SC-003/010_

**Checkpoint P4**: 只有本阶段真实证据齐备、阻塞问题关闭且用户授权下一阶段后方可继续。

---

## Phase 5: READ COMMITTED

**Goal**: 真正 RC 语句级快照与安全写入。

- [x] **T037** 先编 M13–M20 红测：两条 SELECT 可见性变化、单条扫描中途 commit 不混水位、子查询共用 view。  _Trace: FR-010/028; SC-002_
- [x] **T038** 在 `storageengine/mvccadapter/isolation.go`、`mvcc/transaction.go` 实现 RC 每条 statement 捕获一个发布 head，closed 后重新获取。  _Trace: FR-010/016; SC-002_
- [x] **T039** 在 `executor/{physical_select.go,subquery_mvcc.go,with_mvcc.go}` 共享 view 到各 query source；streaming 完全 drain 才释放。  _Trace: FR-010/032; SC-002/007_
- [x] **T040** 先写 M17–M20 的冲突与 current-read baseline 红测，枚举当前版本/根读 TS/unique & FK 分支。  _Trace: FR-014/019; SC-006_
- [x] **T041** 在 `mvcc/{conflict.go,current_read.go}` 和 adapter 增 per-dependency proof/validation，RC 后续更新不得因旧根 TS 假冲突也不能丢失更新。  _Trace: FR-014/018; SC-006_
- [x] **T042** 在 `executor/{locking_modify.go,dml_locking.go,modify.go}` 审核 UPDATE/DELETE/UPSERT/INSERT SELECT 的 parent read、child write 与 RC 当前锁/谓词访问。  _Trace: FR-014/015/019; SC-006_
- [x] **T043** 为 RC 普通 gap 放宽路径做安全判定；必须保留 duplicate/FK 所需 gap/unique reserve；无法完成则记录保守过锁差异。  _Trace: FR-019/020; SC-006/008_
- [x] **T044** 补 RC rollback SAVEPOINT、stale target、deadlock/timeout、zero row、范围/二级索引与 autocommit 测试。  _Trace: FR-015/019/022; SC-002/006_
- [x] **T045** 确认 RC root 不因早期 Head pin 长期压住 GC，并进行长事务内 100 次 statement view acquire/release 计量。  _Trace: FR-016/023/032; SC-007_
- [x] **T046** Phase 5 gate：M13–M20 + M37/39 全过，B01/B02/A05 全量回归，README+docs 更新。  _Trace: FR-030/031; SC-002/006/010_

**Checkpoint P5**: 只有本阶段真实证据齐备、阻塞问题关闭且用户授权下一阶段后方可继续。

---

## Phase 6: SERIALIZABLE

**Goal**: SER root-held 共享读锁与谓词保护。

- [x] **T047** 先写 M21–M28 SER S/S、S/X、absent point、empty range、write skew、upgrade/deadlock 与 unsupported 查询红测。  _Trace: FR-012/020/021/033; SC-005_
- [x] **T048** 在 `executor/transaction_engine.go`、`executor/locking_read.go` 将 long transaction 普通 SELECT 绑定 S-equivalent 模式；禁止返回普通 RR snapshot 代替。  _Trace: FR-012/013/033; SC-005_
- [x] **T049** 在 `executor/{lock_plan.go,physical_binding.go,locking_read.go}` 给点、唯一、范围、全表读证明 predicate lock 边界。  _Trace: FR-020/021; SC-005_
- [x] **T050** 在 `lockmanager` 使用既有 Record S + gap/next-key root grants 覆盖空结果与插入位置，不改 B02 等待公平契约。  _Trace: FR-020/021/022; SC-005_
- [x] **T051** 为 JOIN/CTE/VIEW/UNION/聚合/子查询做 base-read plan 审计，能安全扩展则加锁，否则在执行前 Unsupported；测试无泄漏。  _Trace: FR-012/027; SC-005/008_
- [x] **T052** 验证 FULL SCAN / secondary index 与唯一键使用的谓词 lock plan 不漏插入、不扩大成不必要的数据库全局锁。  _Trace: FR-020/021; SC-005_
- [x] **T053** 核对 S→X、write skew、当前读与自身写的串行性，处理死锁 victim/超时后的正确 session rollback state。  _Trace: FR-015/022; SC-005_
- [x] **T054** 验证 autocommit=1 独立纯 SELECT 优化与 BEGIN/autocommit=0 的边界，遇到 long txn 不可放松加锁。  _Trace: FR-008/033; SC-005_
- [x] **T055** 在 server/adapter 对 unsupported backend、missing LockingTxn、unsupported shapes 返回可预测错误与 SQLSTATE 类别。  _Trace: FR-024/027; SC-008_
- [x] **T056** Phase 6 gate：M21–M28 100 轮确定性调度、B02 gap/fairness/diagnostics/相邻范围回归与 README shape matrix。  _Trace: FR-030/031; SC-005/007/010_

**Checkpoint P6**: 只有本阶段真实证据齐备、阻塞问题关闭且用户授权下一阶段后方可继续。

---

## Phase 7: READ UNCOMMITTED

**Goal**: RU 真实 staged dirty-read 与不脏写。

- [x] **T057** 先写 M29–M36 的 staging 可见性红测，明确 writer 已完成 merge、reader 可采样的信号点。  _Trace: FR-011/017/028; SC-004_
- [x] **T058** 在 `storageengine/isolation.go` 提供可选 read-uncommitted capability，未实现或 Raft 模式必须报 Unsupported。  _Trace: FR-011/024/026; SC-004/008_
- [x] **T059** 在 `mvcc/{transaction.go,write_buffer.go,transaction_state.go}` 实现 root completed-merge staged-view 原子公布（不改 WAL/stage codec）。  _Trace: FR-017/018/025; SC-004_
- [x] **T060** 在 mvcc RU reader 实现 atomic sampling、tombstone/insert/update、reader own-overlay 优先以及取消安全字节复制。  _Trace: FR-011/017; SC-004/007_
- [x] **T061** 在 `storageengine/mvccadapter/isolation.go`、executor ordinary SELECT 整合 RU stage committed overlay，按 query plan 安全重算 predicate/索引。  _Trace: FR-011/017/027; SC-004/008_
- [x] **T062** 在 RU DML/unique/FK/current-read 验证上强制 RC 同等锁与 committed baseline，测量无 dirty write/lost update。  _Trace: FR-018/019; SC-004/006_
- [x] **T063** 给 RU stage memory/row budget、owner generation/term、rollback/restore/GC/scan concurrency 加 bounded/fail-closed gate。  _Trace: FR-017/023/026/032; SC-004/007_
- [x] **T064** 在 replica 初始化或事务 level 切换位置拒绝无安全 dirty-read 能力的拓扑；不输出 RU OK 但执行 RC。  _Trace: FR-026/027; SC-008_
- [x] **T065** Phase 7 gate：M29–M36 每种 stage 三类都可观察且 rollback 安全，100 次 race，并更新 README RU 限制。  _Trace: FR-030/031; SC-004/007/010_

**Checkpoint P7**: 只有本阶段真实证据齐备、阻塞问题关闭且用户授权下一阶段后方可继续。

---

## Phase 8: End-to-End, Replication & MySQL Parity

**Goal**: 协议/复制/资源/完整对拍与回归。

- [x] **T066** 准备两连接 GBaseLite 和 MySQL 8.4 InnoDB 隔离 test runner，数据库名称/端口/版本完全隔离，不读取生产 data/。  _Trace: FR-028/029; SC-009_
- [x] **T067** 执行 M01–M44 两端相同 SQL/bind/order gates，把 PASS/KNOWN DIFFERENCE/UNSUPPORTED/FAIL 写入 `.tmp/b03-parity.md`。  _Trace: FR-029; SC-009_
- [x] **T068** 对每个隔离级别跑 text query + binary prepared、SHOW/SELECT session variables、RESET CONNECTION、驱动断连重连。  _Trace: FR-005/006/007; SC-001_
- [x] **T069** 执行 read view/GC/Compact/Restore + cancelled iterator、LIMIT/early close 组合，证明无已发布版本误回收。  _Trace: FR-016/032; SC-007_
- [x] **T070** 执行 replica leader barrier、term/generation 切换；RU unsupported，其余保护/validator 必须有证据或明确失败。  _Trace: FR-026/027; SC-008_
- [x] **T071** 执行 B01 三类 root + 5 种 validation / A05 statement atomicity / B02 record/gap/next-key/II/fairness/deadlock 全回归。  _Trace: FR-030; SC-010_
- [x] **T072** 运行并保存 `gofmt -l`、`go test ./... -count=1`、`go vet ./...` 与退出码；任何失败处理后重跑。  _Trace: FR-028/030; SC-010_
- [x] **T073** 在支持 race 的平台运行 `go test -race ./... -count=1` 或明确更窄分包方案并解释约束；记录结果。  _Trace: FR-028/032; SC-007_
- [x] **T074** 重复固定并发调度 100 轮，测试断言包括无残留 view pins / pending locks / staging refs，不能靠任意 Sleep。  _Trace: FR-023/028/032; SC-007_
- [x] **T075** 对比 B02 HEAD 既有 data format、Command/codec/WAL/Raft schema 的 hash/diff 与备份恢复旧 fixture。  _Trace: FR-025/030; SC-010_
- [x] **T076** Phase 8 gate：修复 FAIL、文档披露差异、汇总所有真实测试证据和静态安全审查；不能把 unsupported 计成 PASS。  _Trace: FR-029/030/031; SC-008/009/010_

**Checkpoint P8**: 只有本阶段真实证据齐备、阻塞问题关闭且用户授权下一阶段后方可继续。

---

## Phase 9: Documentation, Traceability & RC Gate

**Goal**: 文档同步与发布候选审计。

- [x] **T077** 根据真实支持矩阵同时更新仓库 `README.md`：四级作用域、RU standby 限制、SER shape 与 MySQL 细粒度差异。  _Trace: FR-031/034; SC-008/010_
- [x] **T078** 更新 `docs/使用文档/事务与存储说明.md`、`SQL兼容性参考.md`：示例 SQL、事务/锁/异常边界。  _Trace: FR-031; SC-011_
- [x] **T079** 补 `traceability.md` 实际测试名/文件/Commit SHA/结果，对每个 FR/SC 建立证据，不凭勾选总数验收。  _Trace: FR-029/036; SC-011_
- [x] **T080** 将 Q01–Q10 的真实答案、选择与代价回写 research/plan/contracts，补 B03 KNOWN DIFFERENCE/UNSUPPORTED 表。  _Trace: FR-027/031; SC-008/011_
- [x] **T081** 做最后一次 go test/vet/race/固定调度/源格式/diff 检查，逐项校验 data/ 未碰并出 `specs/007-b03-transaction-isolation/final-review.md`。  _Trace: FR-025/028/030; SC-007/010_
- [x] **T082** 审核任务完成状态，只勾选有命令与测试证据者；产生可审查 PR 摘要及 Release candidate 清单。  _Trace: FR-031; SC-011_
- [x] **T083** 最终 Gate：向用户报告可发布候选资格及限制，产出 `specs/007-b03-transaction-isolation/final-review.md`；**不得**未获单独授权实际打 tag、Release、替换 bin 或部署。  _Trace: FR-031; SC-012_

**Checkpoint P9**: 只有本阶段真实证据齐备、阻塞问题关闭且用户授权下一阶段后方可继续。

---

## Dependencies, Parallelism & Acceptance

- Phase 1 → 2 → 3 → 4 → 5 是严格串行前置；Phase 6(SER) 和 7(RU) 可在完成 P5 后按依赖分开实现，但不能跳过各自安全 gate；P8/P9 均需前面全部完成。
- `spec.md` US1 对应 Phase 2；US2 对应 Phase 3+5；US3 对应 Phase 3+4；US4 对应 Phase 6；US5 对应 Phase 7；US6 对应 Phase 1+8+9。
- 所有代码变更遵守 Constitution 和 AGENTS.md；改 SQL 行为**同一提交**更新 README，不许等 P9 才第一次更新。P9 为总审核整理。
- T 中称“红测”的任务需要先观察对照失败；无权限运行时保持未勾选并标注 blocker。
- 测试证据应包含具体 Go Test 函数、路径、返回码、HEAD/commit、并发顺序 gate 和内存/GC/owner 后状态；不拿通过截图替代日志。
- **禁止**无用户授权执行 `speckit.implement`、全阶段代码变更、编译部署、Release。
