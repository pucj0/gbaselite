# Tasks: B01 完善 MVCC Transaction Manager

**Input**: `spec.md`, `plan.md`, `research.md`, `data-model.md`, `contracts/transaction-diagnostics.md`

## Format

`[ID] [P?] [Story] Description`

- `[P]`: 可以与同阶段其他任务并行。
- `[USx]`: 对应 spec user story。
- 每个任务包含明确文件路径。

---

## Phase 1: Setup / Baseline

- [ ] T001 记录 B01 实施前完整基线测试结果：`go test ./mvcc ./storageengine/... ./executor ./server ./replication` 和 `go test ./...`
      （基线已在 B01 实施过程中实际执行并全绿，但仓库中没有独立的历史 artifact；按 Spec Kit 的证据规则不勾选，也不补造日志。）
- [x] T002 [P] 审查仓库 `.specify/memory/constitution.md`（如存在），将 constitution gates 补回 `specs/001-b01-mvcc-transaction-manager/plan.md`
- [x] T003 [P] 为 B01 新增测试辅助同步 primitive/hook 约定，禁止使用 `time.Sleep()` 构造事务时序，目标文件 `mvcc/*_test.go`

---

## Phase 2: Foundational Transaction Manager

**Blocking prerequisite for all user stories**

- [x] T004 新增事务状态定义 `mvcc/transaction_state.go`，实现 ACTIVE/COMMITTING/COMMITTED/ABORTED/MERGED
- [x] T005 新增 `mvcc/transaction_manager.go`，实现 `activeByID`、root `snapshotRefs`、register/unregister、state transition
- [x] T006 [P] 在 `mvcc/transaction_manager_test.go` 为合法/非法状态转换补单测
- [x] T007 [P] 在 `mvcc/transaction_manager_test.go` 覆盖 TxnID duplicate defensive behavior
- [x] T008 在 `mvcc/store.go` 初始化并持有 TransactionManager，避免新的全局 singleton
- [x] T009 将现有 `Store.active map[uint64]int` 的职责迁入 TransactionManager，同时暂时保持 `CompactHistory` 行为不变
- [x] T010 在 `mvcc/maintenance.go` 改为通过 TransactionManager 获取 oldest active root ReadTS
- [x] T011 [P] 增加 GC retention 回归：一个 root + 多 child/savepoint 时 snapshot 只 pin 一次

**Checkpoint**: Transaction Manager 可独立注册/注销、计算 horizon，不改变现有 MVCC semantics。

---

## Phase 3: US1 - 生命周期与 Timestamp

**Goal**: 明确 TxnID/startTS/readTS/commitTS/state。

**Independent Test**: Begin/commit/rollback/read-only/child 生命周期测试不依赖 SQL executor 即可验证。

- [x] T012 [US1] 扩展 `mvcc.Tx` 生命周期元数据，保持 `Snapshot()` 兼容 alias = ReadTS，修改 `mvcc/transaction.go`
- [x] T013 [US1] root Begin 时设置 `StartTS=ReadTS=Head` 并注册 manager，修改 `mvcc/transaction.go`
- [x] T014 [US1] child 创建时继承 ReadTS、设置 ParentID、注册 diagnostics 但不 pin snapshot，修改 `mvcc/transaction.go`
- [x] T015 [US1] root Commit 进入 COMMITTING；durable publish 成功后记录 COMMITTED + CommitTS
- [x] T016 [US1] child Commit 成功只标记 MERGED，不设置 CommitTS
- [x] T017 [US1] read-only root Commit 标记 COMMITTED，但不分配新 sequence，不设置 CommitTS
- [x] T018 [US1] Rollback 标记 ABORTED，并保持 double rollback 幂等
- [x] T019 [US1] double Commit 返回 ErrClosed，terminal state 不可重新进入 ACTIVE
- [x] T020 [P] [US1] 新增 `mvcc/transaction_lifecycle_test.go` 覆盖 Head=0、read-only、write commit、child merge、double commit/rollback
- [x] T021 [P] [US1] 验证事务执行期间 ReadTS 固定，即使另一事务提交推进 Head

**Checkpoint**: US1 可独立通过。

---

## Phase 4: US2 - Active Registry 与 GC

**Goal**: 可诊断活动事务，并保证 history retention 正确。

- [x] T022 [US2] TransactionManager 提供 active snapshot copy API
- [x] T023 [US2] root commit/rollback 后从 registry 与 snapshotRefs 同时清理
- [x] T024 [US2] child merge/rollback 后从 child registry 清理，不修改 snapshotRefs
- [x] T025 [US2] restore/generation change 后确保旧 generation transaction 最终不能保留新 generation horizon
- [x] T026 [P] [US2] `mvcc/transaction_manager_test.go` 覆盖 root + nested child + savepoint retention
- [x] T027 [P] [US2] `mvcc/maintenance_test.go` 覆盖 active long transaction 阻止历史 anchor 被删除
- [x] T028 [P] [US2] 覆盖 commit/rollback/disconnect 后 registry 无泄漏

**Checkpoint**: US2 可独立通过。

---

## Phase 5: US3 - Unified Visibility Contract

**Goal**: point/scan/flat/child 路径语义一致。

- [x] T029 [US3] 编写统一 visibility fixture/helper，不先修改实现，新增 `mvcc/visibility_contract_test.go`
- [x] T030 [P] [US3] 覆盖 committed version `<= ReadTS` 可见、`> ReadTS` 不可见
- [x] T031 [P] [US3] 覆盖 tombstone 与 reinsert
- [x] T032 [P] [US3] 覆盖 unpublished installed version 无 commit marker 时不可见
- [x] T033 [P] [US3] 覆盖 transaction read-your-writes
- [x] T034 [P] [US3] 覆盖 child own write + parent write visibility
- [x] T035 [P] [US3] 覆盖 parent 不见 unmerged child
- [x] T036 [US3] 如 contract tests 暴露差异，仅在必要处调整 `mvcc/store.go` / `mvcc/flat_scan.go` / range reader；不得机械合并优化路径
- [x] T037 [US3] 补 repeatable-read 并发测试：T1 snapshot 后 T2 commit，T1 仍读取旧版本

**Checkpoint**: US3 可独立通过。

---

## Phase 6: US4 - Unified Conflict Validation

**Goal**: 所有 commit path 共享相同 conflict semantics。

- [x] T038 [US4] 新增 `mvcc/conflict.go`，抽取共享 point/range commit validation contract
- [x] T039 [US4] 将 `mvcc/group_commit.go` 接入共享 validator
- [x] T040 [US4] 将 `mvcc/local_commit.go` / bounded local path 接入共享 validator
- [x] T041 [US4] 将 `mvcc/local_stream.go` 接入共享 validator
- [x] T042 [US4] 将 `mvcc/local_wal.go` 接入共享 validator
- [x] T043 [US4] 将 `mvcc/store.go::commitContext` / replicated staged path 接入共享 validator
- [x] T044 [P] [US4] point conflict matrix：same key vs different key
- [x] T045 [P] [US4] insert/update/delete/tombstone conflict matrix
- [x] T046 [P] [US4] Guard point dependency tests
- [x] T047 [P] [US4] GuardRange insert/update/delete phantom tests，含 inclusive/exclusive bounds
- [x] T048 [P] [US4] 增加 write-skew regression：无 Guard 时两个不同 key 写均允许提交
- [x] T049 [P] [US4] 增加 guarded write-skew regression：显式 dependency 后发生 conflict
- [x] T050 [US4] 建立 commit-path matrix helper，使相同 fixture 可用于 bounded/group/stream/WAL/replicated path

**Checkpoint**: US4 可独立通过，并确认 isolation 未被提升。

---

## Phase 7: US5 - Read/Write/Dependency Diagnostics

**Goal**: bounded transaction diagnostics。

- [x] T051 [US5] 在 manager/entry 增 point reads、range reads、writes、write bytes、point/range dependency counters
- [x] T052 [US5] `mvcc/transaction.go::Get` 更新 point read counter，不保存 key
- [x] T053 [US5] `Scan/ScanRange` 更新 range read counter，避免逐 key read-set allocation
- [x] T054 [US5] `mvcc/write_buffer.go` 在逻辑 write set 变化时更新 write count/bytes
- [x] T055 [US5] `mvcc/range_guard.go` 与 Guard path 更新 dependency counters
- [x] T056 [P] [US5] 大 scan regression：读取大量 key 后 manager 内存结构不含与 key 数量线性增长的 read-key collection
- [x] T057 [P] [US5] write replacement/overwrite 时 diagnostics bytes 不重复累计错误

**Checkpoint**: US5 可独立通过。

---

## Phase 8: Diagnostics Contract

- [x] T058 [US5] 在 `storageengine/engine.go` 增 optional `TransactionDiagnostics` capability 与中性 DTO
- [x] T059 [US5] 在 `storageengine/mvccadapter/adapter.go` 实现 diagnostics adapter
- [x] T060 [P] [US5] contract tests 验证返回 snapshot copy，调用者修改不会影响 manager
- [x] T061 [P] [US5] 验证 diagnostics 不包含 key/value/SQL payload
- [x] T062 [P] [US5] 验证无 root 时 `HasOldestReadTS=false`

---

## Phase 9: US6 - Cancel / Restore / Disconnect / Overflow

**Goal**: 极端边界最终状态正确。

- [x] T063 [US6] 明确并实现 publication 前 cancellation -> ABORTED
- [x] T064 [US6] 明确并实现 publication 已 durable 后 cancellation 不得返回 rollback 语义
- [x] T065 [P] [US6] 为 local bounded/group commit 增 cancellation race deterministic tests
- [x] T066 [P] [US6] 为 streaming commit 增 install-before-publication cancel test，确认数据不可见
- [x] T067 [P] [US6] 为 replicated commit 增已有 commit replay/idempotency 兼容测试
- [x] T068 [US6] restore/generation invalidation 后旧 Tx Get/Put/Commit fail closed 且 registry cleanup
- [x] T069 [US6] 审查 `server` session disconnect cleanup，确保 active user transaction rollback/unregister
- [x] T070 [P] [US6] 添加 disconnect registry leak test
- [x] T071 [US6] 抽取/增加 commit sequence allocator overflow check，禁止 `uint64` wraparound
- [x] T072 [P] [US6] MaxUint64 边界测试：最后可用版本/耗尽后拒绝分配

**Checkpoint**: US6 可独立通过。

---

## Phase 10: Resource Boundaries

- [x] T073 [P] `mvcc/write_buffer_test.go` 覆盖 transaction write limit exact boundary
- [x] T074 [P] 覆盖 limit+1 返回 ErrWriteSetLimit
- [x] T075 [P] 覆盖 limit=0 仍为 unlimited total + bounded memory
- [x] T076 [P] 覆盖 stagedBytes accounting overflow fail closed
- [x] T077 [P] 验证单个大 op 直接 stage 行为保持
- [x] T078 验证 B01 未改变 MaxValueBytes / MaxChunkBytes / localInstallBytes 现有约束

---

## Phase 11: Executor / Savepoint Integration

- [x] T079 [US1] 审查 `executor/transaction_engine.go` BEGIN/COMMIT/ROLLBACK 对 manager state 的映射
- [x] T080 [US1] 审查 automatic transaction：statement child MERGED 后 root durable commit
- [x] T081 [US1] 审查 explicit transaction：statement child merge 不提前 durable commit
- [x] T082 [US1] 审查 `executor/savepoint_mvcc.go` CREATE/ROLLBACK TO/RELEASE/COMMIT 生命周期
- [x] T083 [P] explicit transaction 中 statement failure 只回滚 statement child 的既有语义回归
- [x] T084 [P] parent final rollback 必须丢弃所有已 merged statement/savepoint writes
- [x] T085 [P] autocommit=0 implicit root transaction registry 生命周期测试

---

## Phase 12: Polish & Cross-Cutting Validation

- [x] T086 运行 `gofmt` 和 `git diff --check`
- [x] T087 运行 `go vet ./...`
- [x] T088 运行 `go test ./...`
- [x] T089 [P] 在可用 CI/Linux 环境运行 `go test -race ./mvcc ./executor ./server`
- [x] T090 对照 `checklists/requirements.md` 完成 requirements-quality review
- [ ] T091 运行 `/speckit.analyze`，修复 spec/plan/tasks 的 critical/high inconsistency
- [ ] T092 运行 `/speckit.implement` 后执行 `/speckit.converge`
- [ ] T093 若 converge 追加 required tasks，重复 implement/converge 直到 Converged

---

## Dependencies

```text
Phase 1 Baseline
   ↓
Phase 2 Transaction Manager foundation
   ├─→ US1 Lifecycle
   │      ├─→ US2 Registry/GC
   │      └─→ Executor/Savepoint Integration
   ├─→ US3 Visibility
   ├─→ US4 Conflict Validation
   └─→ US5 Diagnostics
             ↓
       Diagnostics Contract

US1 + US2 + US3 + US4
        ↓
US6 Cancel/Restore/Overflow
        ↓
Cross-cutting Validation
```

## Parallel Execution Examples

### Foundation

T006 与 T007 可并行；T010 必须等待 T005/T009。

### Visibility

T030-T035 可在 fixture T029 完成后并行。

### Conflict

T044-T049 可以在 validator T038 完成后并行，T039-T043 可按不同文件并行修改但最终需统一审查。

### Diagnostics

T052-T055 修改不同职责文件，可以在 counters model T051 后并行。

### Edge tests

T065-T067、T070、T072 可在对应实现 seam 完成后并行。

## MVP

最小可交付建议完成：

- Phase 1-6
- 即 Transaction Manager + lifecycle + registry/GC + visibility contract + unified conflict validation

这已经满足 B01 P1 核心目标。

P2 diagnostics 与 cancel/overflow hardening 可随后增量完成，但最终 B01 DoD 要全部通过。

---

## Completion Status

状态更新于 2026-09-20。勾选表示该任务有仓库内代码/测试或 CI 证据；未勾选项在下方单独说明。

### 证据索引

- Foundational 与 US1：`mvcc/transaction_state.go`、`mvcc/transaction_manager.go`、`mvcc/transaction.go`、`mvcc/transaction_manager_test.go`（15 例）、`mvcc/transaction_lifecycle_test.go`（15 例，覆盖 T015–T018、double commit/rollback、终态不被 cleanup 改写）、`mvcc/transaction_invariants_baseline_test.go`（12 例）。
- US2 与 GC：`mvcc/maintenance.go` 经 `TransactionManager.OldestReadTS()` 取 horizon；`mvcc/history_retention_test.go`（6 例，含 disconnect rollback 与 generation change）；`TestTransactionManagerChildrenNeverPinSnapshot`（T011/T026/T034）。
- US3：`mvcc/visibility_contract_test.go`（11 例，含重复读与多读取路径一致性）。契约测试未暴露需要修改读取路径实现的差异，因此 T036 无实现改动，也未做机械合并；`visibilityReader`/`visible` 的实现在 `mvcc/store.go`（`newVisibilityReader`、`visibilityReader.visible`），flat path 在 `mvcc/flat_scan.go`。
- US4：`mvcc/conflict.go` 为唯一 validator；实际接入点为 `mvcc/group_commit.go`（`commitLocal` → `commitLocalGroup`，bounded + group 两条路径）、`mvcc/local_stream.go`、`mvcc/local_wal.go`、`mvcc/store.go` 的 `commitContext`（replicated/staged）；`mvcc/conflict_matrix_test.go`（5 例，含 write skew 与 commit-path matrix）。
- US5：`mvcc/observation.go`（`trackStagedOp`/`stagedOpKindOf` 是 point/range dependency 计数器的实际实现位置，经 `mvcc/write_buffer.go` 的 `bufferWrite` 调用；`mvcc/range_guard.go` 只负责把 `GuardRange` 编码成依赖，不含计数逻辑）、`mvcc/transaction_diagnostics_test.go`（11 例）、`mvcc/resource_boundary_test.go`（含 MaxUint64 与 write-limit 边界，T071–T078）；能力与 DTO 见 `storageengine/engine.go`、diagnostics adapter 位于 `storageengine/mvccadapter/diagnostics.go`（与 `adapter.go` 同包，故 T059 的实现位置以该文件为准）及其 11 个测试。
- US6：`mvcc/cancel_publication_test.go`（6 例）、`replication/commit_cancel_test.go`、`mvcc/transaction_reset_test.go`（含 stale Get/Commit/Rollback 与 stale Put/Delete fail-closed）、`storageengine/mvccadapter/diagnostics_internal_test.go::TestDiagnosticsExposeCommittingStateThroughTheAdapter`。
- Executor/Savepoint 与断连：`executor/mvcc_autocommit_registry_test.go`（autocommit=0 隐式 root 的 registry 生命周期，经 `storageengine.TransactionDiagnostics` 断言）、`executor/mvcc_savepoint_test.go`、`executor/mvcc_savepoint_failure_test.go`、`executor/mvcc_savepoint_limit_test.go`（MVCC savepoint layer 上限 32 与 replacement/RELEASE bypass 回归）、`executor/mvcc_transaction_invariants_test.go`、`server/mvcc_disconnect_registry_test.go`（协议层 KILL/断连后 registry 清空且未提交写入不可见）；断连清理路径为 `server/mysql_server.go` 的 `defer s.Engine.CloseSession(session)` → `executor.CloseSession` → `rollbackSessionTransaction`。
- T086–T088：`gofmt`、`git diff --check`、`go vet ./...`、`go test ./... -count=1` 全部通过。
- T090：本文件与 `checklists/requirements.md` 的 CHK001–CHK050 已逐条对照 `spec.md`/`plan.md` 审查通过（该清单按自身说明只表示需求质量，不代表代码完成）。

### 未完成项

- **T001**：按 Spec Kit 证据规则不勾选——B01 实施前的基线测试已在实施过程中执行（`go test ./mvcc ./storageengine/... ./executor ./server ./replication` 与 `go test ./...` 全绿，无既存失败），但仓库内没有该次运行的历史 artifact，本文件不为此补造日志。
- **T089 已完成**（见下方 CI 证据）：race 在 GitHub Linux runner 上验证通过。本机 `CGO_ENABLED=0` 且无 gcc/clang，`go test -race` 直接报 `-race requires cgo`，因此本机结果不作为完成依据。
- **T091–T093**：需要 Spec Kit 的 `/speckit.analyze`、`/speckit.implement`、`/speckit.converge` 命令执行环境，本仓库未执行这些命令，因此不勾选。

### `/speckit.analyze` 后续（H-1/H-2 与 M-4..M-8 已关闭）

`/speckit.analyze` 报出 CRITICAL 0 / HIGH 2 / MEDIUM 8 / LOW 8。两个 HIGH 以及 M-4/M-5/M-6/M-7/M-8 已关闭，未修改任何 MVCC 事务语义。

- **H-1（guard-only 提交语义未进入 spec）**：已正式决策并写入 `spec.md`（FR-004、FR-005、FR-010、INV-002 说明、§7 "事务结束" 的三类 root 表、§9 Clarifications 8/9、US1.5/US1.9）、`data-model.md`（§1/§2/§7/§8/§10）、`research.md`（R2/R5/R6/R6b/R11）、`plan.md`（D3 限定 + D6）、`contracts/transaction-diagnostics.md`。原 characterization test `TestBaselineGuardOnlyCommitCurrentlyAdvancesHead` 已转为正式 contract test `TestDependencyOnlyCommitPublishesRevision`。
- **H-2（TransactionDiagnostics contract 与实际公共 API 不一致）**：`contracts/transaction-diagnostics.md` 已按实现重写（完整 18 字段 DTO、`UNKNOWN` fail-closed 词表与映射、`ActiveTransactions` 的 registry 语义、`ActiveRoot`/`ActiveChildren` 只计 non-terminal、非 nil slice、计数器定义、`HasCommitTS` 不保证数据 version），并与 `storageengine/engine.go` 的公共注释同步；`data-model.md` §1–§5 的结构与状态图同步为实际实现。
- **M-4（FR-015 closed transaction API 覆盖）**：新增 `mvcc/transaction_lifecycle_test.go::TestClosedTransactionAPIContract`（committed root / rolled back root / merged child 三组子测试，逐项覆盖 Get/Put/Delete/Child/Commit 返回 `ErrClosed`，并固定"重复 Rollback 仍幂等返回 nil"这一既有契约）。
- **M-5（MVCC savepoint 最大层数）**：`executor/mvcc_savepoint_limit_test.go::TestMVCCSavepointLayerLimit` 固定 **maximum live MVCC savepoint layers per user transaction**（32）：exact boundary 成功、超限 fail closed 且事务仍可用、replacement below ceiling 保持原语义、**replacement at ceiling fail closed 且不匿名化原名字**、repeated same-name 不能增长 layer 链、RELEASE 不能腾出名额、COMMIT/ROLLBACK 后 registry 清空。该测试同时暴露并修复了一个真实 resource-boundary bug：原实现只统计 named savepoint 数，且先匿名化再计数，因此 `SAVEPOINT x` 重复执行或 `SAVEPOINT x; RELEASE x` 循环可让 `len(session.savepoints)` 无界增长到 33、34…；现改为在**任何 mutation 之前**检查 `len(session.savepoints) >= maxMVCCSavepoints`（`executor/savepoint_mvcc.go`）。
- **M-6（stale Put/Delete fail-closed）**：新增 `mvcc/transaction_reset_test.go::TestStaleTransactionRejectsWritesAfterReset`（stale root 不新建 stage、已有 stage 不被替换、head 与 retention 不变、数据不可见、stale child/parent 同样拒绝写入）。
- **M-7（证据引用修正）**：本文件证据索引已按真实实现位置改写（`visibilityReader`/`visible` 在 `mvcc/store.go`、flat path 在 `mvcc/flat_scan.go`、validator 接入点在 `mvcc/group_commit.go` 等、dependency counters 在 `mvcc/observation.go`、diagnostics adapter 在 `storageengine/mvccadapter/diagnostics.go`），并把 T001 改为按证据规则不勾选。
- **M-8（replicated MaxUint64 → local 分配）**：新增 `mvcc/resource_boundary_test.go::TestReplicatedMaxSequenceThenLocalAllocationFailsClosed`（经真实 `Apply` 入口把 high-water mark 推到 `MaxUint64`，随后的本地提交必须 `ErrSequenceExhausted`、不 wrap、无 version 0、无 marker、无 CommitTS）；`plan.md` Phase 8 的措辞已修正为"apply 侧只拒绝 sequence=0；MaxUint64 是最后合法 revision；后续本地分配必须 fail closed"。
- 其余 MEDIUM/LOW（M-2 部分结构漂移已在 H-2 同步中顺带修正，M-3 等其余项与 L 系列）尚未处理，留待后续按需安排。

### CI 证据（B01 race）

- Workflow：`.github/workflows/test.yml`，Linux 步骤 `MVCC transaction race regression`：
  `go test -race ./mvcc ./storageengine/... ./executor ./server -count=1`；既有 `Failover race regression`
  （`go test -race ./failover -count=10`）保留。
- Run：<https://github.com/pucj0/gbaselite/actions/runs/35496213448>（commit `d7292d3`）整体 `success`。
  覆盖范围含 TransactionManager registry、Tx lifecycle、State/Info、diagnostics counters、
  ActiveTransactions、commit/rollback、adapter diagnostics、executor 事务集成与 server 断连清理。
- 逐步骤结论：`quality (ubuntu-latest)` success（`Test` 44s、`Failover race regression` 70s、
  `MVCC transaction race regression` 50s，整 job 176s）；`quality (windows-latest)` success（两个 race 步骤按
  `if: runner.os == 'Linux'` skipped）；`build` linux/windows success；`mysql-8-client` success。
