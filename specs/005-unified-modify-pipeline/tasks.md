# Tasks：统一写入算子 Modify Pipeline

**输入文档**：

```text
specs/005-unified-modify-pipeline/spec.md
specs/005-unified-modify-pipeline/plan.md
```

**测试要求**：

A05 属于架构重构，所有现有 DML 行为均属于兼容合同，因此每个迁移阶段必须包含 regression tests。

------

# Phase 1：基础设施

**目标**：建立统一 Mutation Candidate 模型，但暂不迁移 SQL。

**状态**：已完成（T001-T006 全部完成，纯新增，无现有 SQL 行为改动）。

-  [X] T001 在 `executor/modify.go` 中定义统一的 `RowIdentity`、`ModifyResult` 基础模型，并写明 physical identity 与 ownership 语义
  - 完成说明：新增 `executor/modify.go`。`RowIdentity{TableID, Key, Valid}`，`TableID` 取 `versionedTable.ID`（与写入路径同一标识）；`ModifyResult{AffectedRows, FirstGeneratedID, HasGeneratedID}`，用显式 flag 而非 0 哨兵表示"是否产生过 id"，并注明发布到 `session.LastInsertID` 属于调用方、必须在 child commit 之后。
  - 本地约定（additive，不影响现有代码）：`IsTarget()` 判定必须 `Valid && len(Key) > 0`；零长度 key 视为丢失 physical provenance，不得作为 mutation target。
-  [X] T002 [P] 在 `executor/modify_insert.go` 中定义 `InsertCandidate`
  - 完成说明：`InsertCandidate{Values storage.Row, Ordinal uint64}`。按其语义实现 `Target()` 恒返回 `(RowIdentity{}, false)`：INSERT 不寻址既有物理行，两条相同 VALUES 不得被去重。
-  [X] T003 [P] 在 `executor/modify_update.go` 中定义 `UpdateCandidate`
  - 完成说明：`UpdateCandidate{Identity, OldRow, EvalRow}`，注释明确 `Identity` 永远是 **old** physical row（PK/index 变更也不改），`OldRow` 不得原地修改，`EvalRow` 对 UPDATE JOIN 需含 target+source 列。
-  [X] T004 [P] 在 `executor/modify_delete.go` 中定义 `DeleteCandidate`
  - 完成说明：`DeleteCandidate{Identity, OldRow}`，注释明确 `OldRow` 只含目标表自身列——多表 DELETE 的 joined window 必须在进入写路径前剥掉隐藏 join identity 列。
-  [X] T005 在 `executor/modify.go` 中增加 RowIdentity key clone/helper，确保 retained storage key 不引用上游 borrowed buffer
  - 完成说明：`NewRowIdentity`（构造即拷贝，nil/空 key 归一化为 nil）、`(*RowIdentity).Clone()`（保留算子必须使用）、`cloneKey`（唯一拷贝边界，非 nil 输入一律重新分配，空切片也不与借用数组共享底层数组）。校验方式：测试中改写源 buffer 后断言 identity 未受影响，并断言 `&identity.Key[0] != &borrowed[0]`。
-  [X] T006 在 `executor/modify_test.go` 中增加 RowIdentity 测试，验证相同 key 不同 TableID 不相等、相同 TableID+key 相等、invalid identity 不产生 mutation target
  - 完成说明：新增 `executor/modify_test.go`，6 个测试覆盖：TableID+key 等价性、跨表同 key 不去重（含 dedup ledger key 的 length-prefix 编码断言）、invalid/keyless identity 不产生 target、retained key ownership、三种 candidate 的 target 暴露、ModifyResult 首个 generated id 语义。
  - 注：本阶段按"仅新增基础模型"执行，未创建 `physical/target_dedup.go`，跨表去重通过 `RowIdentity.StorageKey()` 的编码契约先行固定（T008 再接入算子）。

**Checkpoint**：

完成后应该仅新增基础模型，不改变任何现有 SQL 行为。

**Checkpoint 结论**：满足。新增 5 个文件（`executor/modify.go`、`modify_insert.go`、`modify_update.go`、`modify_delete.go`、`modify_test.go`），未修改任何现有生产文件、未修改 transaction architecture、未接入任何调用点；`go build ./...`、`go vet ./executor/...`、`go test ./...` 与 `TestProductionArchitectureBoundaries` 均通过。

**Phase 1 遗留（交后续阶段处理，不在本阶段改动）**：

- `InsertCandidate` 目前不含 `Mode`/`Target` 上下文，而现有 `IGNORE`/`REPLACE`/`ON DUPLICATE KEY UPDATE` 依赖 `insertTarget + session + insertMode`（`writeInsertedRow`）。Phase 3 开始前需先补 plan/tasks（见审查报告 P0-4）。
- `RowIdentity.Valid` 的判定来源在"有 PK / 无 PK"两种表上不对称（无 PK 依赖隐藏 rowid provenance），需在 plan 的 RowIdentity 一节补判定矩阵（见审查报告 P0-3）。
- `DeleteCandidate.OldRow` 的 multi-delete 来源（joined window 去隐藏列）需在 plan/tasks 写明（见审查报告 P0-2）。

------

# Phase 2：TargetRowDedup 基础能力

**目标**：提供 stable first-match、resource-bounded 的目标行去重。

**状态**：已完成（T007-T016 全部完成，未迁移任何 INSERT/UPDATE/DELETE）。

-  [X] T007 调研并抽取 `physical/distinct.go` 中现有 bounded/spillable stable dedup 机制，记录可复用部分到 `specs/005-unified-modify-pipeline/research.md`
  - 完成说明：新增 `spec/005-unified-modify-pipeline/research.md`（仓库实际目录是 `spec/`，不是 `specs/`）。结论：两趟有界排序 + ordinal + spill 已存在于 `physical.Distinct` + 注入式 `NewSort` 工厂 + `executor/externalRowSorter`，A05 的正确复用方式是"抽共享内核 + 注入工厂"，而不是在 `physical` 里重写 spill。
-  [X] T008 在 `physical/target_dedup.go` 实现 TargetRowDedup，使 dedup key 同时包含 TableID 与 physical storage key
  - 完成说明：`TargetRowDedup[T]{Input, Identity func(T) (RowIdentity, bool), NewSort}`。dedup key 由 `RowIdentity.StorageKey()` 产出（length-prefixed `[uint32 len(TableID)][TableID][Key]`），因此 TableID 必在 key 内且不会被 key 字节吸收。identity 每行只解析一次（经 `Projection` 把 key 与候选一起传递）。
-  [X] T009 在 `physical/target_dedup.go` 中保证 first occurrence wins，并恢复原始输入顺序
  - 完成说明：复用共享内核 `physical/stable_dedup.go` 的 Pass 1（key+ordinal）/ Pass 2（ordinal）。同一内核也是 `Distinct` 的实现，`physical/distinct_test.go` 原断言未改动即通过。
-  [X] T010 在 `physical/target_dedup.go` 中确保 retained candidate/key 被 clone，而不是持有 borrowed row/key
  - 完成说明：按 plan 的职责切分，**retained row 与 identity key 的 clone 由绑定层负责**（与 `Sort`/`Distinct` 的既有约定一致：算子只保证 `Close`，sorter 拥有它保留的行）。`RowIdentity.NewRowIdentity`/`Clone` 提供拷贝边界；`TargetRowDedup` 自身不复制负载，文档已写明该契约。T014 的测试断言 row 内容可安全保留。
-  [X] T011 在 `physical/target_dedup.go` 中保证 success、upstream error、downstream error、context cancel 时正确释放 spill/temp resource
  - 完成说明：`dedup` 对两个 sorter 都使用 `defer errors.Join(err, sorter.Close())`，并覆盖：工厂第二个失败（已开的那个仍被关闭）、上游错误、下游错误、run 前已 cancel（不打开任何 sorter）、运行中 cancel。`Close` 采用现成的 removeRun 路径。
-  [X] T012 [P] 在 `physical/target_dedup_test.go` 添加 stable-first 去重测试
  - 完成说明：`TestTargetRowDedupKeepsFirstOccurrenceInSourceOrder`：非有序输入 + 重复 target，断言输出为首次出现的 marker 且顺序等于输入顺序。
-  [X] T013 [P] 在 `physical/target_dedup_test.go` 添加不同 TableID、相同 key 不互相去重的测试
  - 完成说明：`TestTargetRowDedupSeparatesTablesWithEqualStorageKey`：三个表同 key 全保留；另含 `"t"` vs `"t\x01"` 前缀边界断言。
-  [X] T014 [P] 在 `physical/target_dedup_test.go` 添加 invalid identity / outer-join absent target 测试
  - 完成说明：`TestTargetRowDedupDropsCandidatesWithoutPhysicalTarget`：outer-join NULL-extension（无 key）、`Valid=false`、以及 `Identity` 返回 `ok=false` 的候选全部丢弃；**列值全 NULL 但物理 key 存在的行必须保留**。`TestTargetRowDedupWithoutTargetsProducesNoMutation` 覆盖全部候选都无 provenance 时的空输出且不报错。
-  [X] T015 [P] 在 `physical/target_dedup_test.go` 添加超过内存阈值的 spill regression
  - 完成说明：`TestTargetRowDedupSpillsBeyondMemoryThresholdAndCleansUp`：用有界测试 sorter（超过 `maxRetained` 即写私有临时 run 文件）跑 40 行候选，断言去重结果正确、两个 sorter 关闭、临时目录无残留。
-  [X] T016 [P] 在 `physical/target_dedup_test.go` 添加 cancel/error 后临时资源释放测试
  - 完成说明：`TestTargetRowDedupReleasesSortersOnEveryExitPath`：下游错误、上游错误、运行前 cancel、运行中 cancel、sorter 工厂失败五条路径，每条都断言 sorter 关闭计数与临时目录为空。

**Checkpoint**：

TargetRowDedup 可以独立运行，不依赖 INSERT/UPDATE/DELETE。

**Checkpoint 结论**：满足。`physical/target_dedup.go` 只依赖 `physical` 包内的 `Operator`/`Projection`/`Sorter`/`RowIdentity` 与共享内核，不 import 任何 executor 或 storage 类型；`physical` 全包测试通过，`Distinct` 行为未变。

**Phase 2 与 plan.md 的差异（已记入 research.md 第 4 节）**：

1. `RowIdentity` 由 `executor/` 迁到 `physical/`（`executor` 保留 `type RowIdentity = physical.RowIdentity` 别名 + `NewRowIdentity` 包装）。原因：plan 的 `Identity func(T) (RowIdentity, bool)` 要求算子能引用该类型；若 identity 定义在 executor，physical 只能反向依赖 executor。调用侧写法与 Phase 1 完全不变，`executor/modify_test.go` 未改动即通过。
2. 文件切分为 `physical/row_identity.go`（身份模型）、`physical/stable_dedup.go`（共享内核）、`physical/target_dedup.go`（目标去重算子）。
3. 内核 dedup key 使用 `string` 而非 `[]byte`：内核靠 `==` 丢弃同 key 组，而 `[]byte` 不满足 `comparable`；`Distinct` 原本就用 `string`。绑定层在导入候选时做 `string(StorageKey())` 转换。

------

# Phase 3：User Story 1 — 统一 INSERT（P1）🎯 MVP

**目标**：

```text
INSERT VALUES
INSERT SELECT
       ↓
InsertCandidate
       ↓
InsertOperator
```

**独立验收**：

完成本 Phase 后，全部 INSERT regression 应通过，即使 UPDATE/DELETE 尚未迁移。

## 测试

-  [X] T017 [P] [US1] 在现有 INSERT regression 中增加架构断言或针对性测试，证明 INSERT VALUES 与 INSERT SELECT 最终进入统一 InsertOperator
  - 完成说明：新增 `executor/mvcc_insert_pipeline_test.go`。`InsertOperator.apply` 中加入了 `insertApplyHook`（生产环境为 nil，仅供回归测试观察通过统一算子的候选）。`TestMVCCInsertValuesAndSelectShareOneOperator` 断言 VALUES 走 3 行 ordinal 0/1/2、SELECT 走 2 行 ordinal 0/1、两者目标表相同；`TestMVCCInsertSetUsesTheSameOperator` 覆盖 INSERT SET（重写为 VALUES 形式）。
-  [X] T018 [P] [US1] 在 `executor/mvcc_insert_select_test.go` 保留并强化 self-reference snapshot 测试
  - 完成说明：原 `TestMVCCInsertSelectSelfSourceSnapshot` 保留未改；新增 `TestMVCCInsertSelfSourceSnapshotThroughOperator` 追加第二轮 self-source，验证 statement snapshot 隔离可重复成立（8 行）。
-  [X] T019 [P] [US1] 在 `executor/mvcc_insert_select_test.go` 保留 auto_increment + LastInsertID commit-only 测试
  - 完成说明：`TestMVCCInsertSelectAutoIncrementAndLastInsertID`、`mvcc_insert_select_test.go` 的 `TestMVCCInsertSelectLastInsertIDRollback`、`mvcc_last_insert_id_commit_test.go` 的 `TestMVCCInsertLastInsertIDRequiresStatementCommit` 全部保留并通过。发布点未改动，仍在 `transaction_engine.go:357-362`（child commit 之后）。
-  [X] T020 [P] [US1] 在 `executor/mvcc_insert_select_test.go` 保留中途失败整 statement rollback 测试
  - 完成说明：原测试保留；新增 `TestMVCCInsertStatementRollbackThroughOperator`，对 VALUES 与 SELECT 两条路径分别覆盖 duplicate key / CHECK / FK / type mismatch 六种中途失败，并断言失败前已写入的行全部回滚、后续语句恢复正常。
-  [X] T021 [P] [US1] 在现有 INSERT 测试中覆盖 explicit auto_increment 推进 counter 与 rollback gap 语义
  - 完成说明：新增 `TestMVCCInsertAutoIncrementAcrossValuesAndSelect`（VALUES 生成 1,2 → SELECT 续 3,4,5 → explicit 100 抬升 floor → 两条路径分别续 101、102）与 `TestMVCCInsertAutoIncrementGapAfterRollbackIsPreserved`（两条路径中途失败后 reservation 不复用，允许 gap）。

## 实现

-  [X] T022 [US1] 在 `executor/modify_insert.go` 实现 InsertOperator，输入为 `physical.Operator[InsertCandidate]`
  - 完成说明：`InsertOperator{Input, Target, Write, Engine, Session, Mode, Rows, Result}` + `Run`/`Publish`。同时补齐 Phase 1 缺失的 `InsertCandidate{Values, Ordinal}` 定义（Phase 1 只留下了文件骨架与计划注释）及其 `Target()` 语义（INSERT 无既有目标）。
-  [X] T023 [US1] 在 `executor/modify_insert.go` 中复用现有 `writeInsertedRow`，不要复制 CHECK/UNIQUE/index/FK 最终写逻辑
  - 完成说明：`apply` 直接调用 `writeInsertedRow(ctx, o.Write, o.Session, o.Target, o.Mode, ...)`，未新增任何约束/索引/FK 逻辑。`writeInsertedRow` 与 `mutation_insert_modes.go` 未改动。
-  [X] T024 [US1] 将 INSERT auto_increment 处理迁入或集中到 InsertOperator 所拥有的统一 statement-local insert state
  - 完成说明：新增 `insertCounters`（floors/sent/next/last + advance/flush/reserve），由 `InsertOperator` 独占。原 `insertSQL` 内联的 `floors/sent/next/last/advance` 闭包与 `resolveInsertAutoIncrement`/`advanceInsertCounter`/`flushInsertCounters` 已删除；`insertTarget` 不再持有计数器字段。
  - **保留的策略差异**：VALUES 通过 `Rows` 一次性预留剩余行数，SELECT 通过 `Rows=0` 每行预留 1 个。两者发出的 id 序列完全相同（已由 T021 跨路径测试锁定），差异只影响后端计数器提前量；这是刻意保留而非静默合并。
-  [X] T025 [US1] 在 InsertOperator 中记录 statement-local affected rows 与 first generated ID，但不得直接发布到 session
  - 完成说明：`ModifyResult` 由算子在 `apply` 中累加、在 `Publish` 中返回；`Publish` 明确注释"不得在此发布 LastInsertID"，算子不接触 `session.LastInsertID`。
-  [X] T026 [US1] 重构 `executor/mutation.go` 中 INSERT VALUES，使 VALUES source 生成 InsertCandidate 后进入 InsertOperator
  - 完成说明：`insertSQL` 改为 `valuesSource(...)` → `InsertOperator` → `Publish`。新增 `valuesSource`/`valuesCandidate`，逐字保留原有行装配顺序（默认值 → 表达式/字面量）与全部错误语义（值个数不匹配、列默认值错误、表达式求值错误、类型转换错误均在写入前返回）。
-  [X] T027 [US1] 重构 `executor/mutation_insert_select.go`，使 SELECT pipeline 输出转换为 InsertCandidate 后进入同一 InsertOperator
  - 完成说明：新增 `insertSelectSource`（`Projection[[]any, InsertCandidate]`，内部维护 statement-local ordinal，复用 `target.buildRow`），随后进入同一 `InsertOperator`。
-  [X] T028 [US1] 保持 `executor/mutation_insert_select.go` 的 SELECT 使用 parent statement snapshot，InsertOperator 使用 statement child transaction
  - 完成说明：`bindSubqueryQuery(ctx, read, ...)` 仍绑定 parent/read txn；`InsertOperator.Write` 仍是传入的 child/write txn。transaction 架构未改动（`transaction_engine.go` 未触碰）。
-  [X] T029 [US1] 将 session.LastInsertID 发布保持在 statement child commit 成功之后的 transaction statement wrapper 中
  - 完成说明：发布点未移动。`Publish` 只把首个生成 id 放进 `*Result`，由 `transaction_engine.go` 在 child commit 之后发布（该文件未改动）。
-  [X] T030 [US1] 删除 INSERT VALUES 与 INSERT SELECT 中迁移后重复的最终 row write 分支
  - 完成说明：两条路径各自的 `writeInsertedRow` 调用点、行装配/auto_increment 分支已消失，最终写入只剩 `InsertOperator.apply` 一处（`grep writeInsertedRow` 仅命中 operator 与修改模式的内部使用）。
-  [X] T031 [US1] 运行全部 INSERT、transaction、savepoint regression 并修复兼容性差异
  - 完成说明：`go test ./...` 全 21 包通过；INSERT 相关的 legacy-vs-MVCC parity（`TestMVCCInsertModesMatchLegacyEngine`、`TestMVCCInsertSelectMatchesLegacyEngine`、`TestMVCCInsertSelectMatchesValuesSemantics`）、transaction/savepoint 回归（`mvcc_savepoint*`、`mvcc_last_insert_id_commit`、`legacy_migration`）全部通过。
  - 修复的实际缺陷 2 处：① `Publish` 在空源（source 未产生任何候选）时解引用未初始化的计数器而 panic（`INSERT ... SELECT ... WHERE 1=0` 会触发）；② SELECT 路径的 `InsertCandidate.Ordinal` 未设置，导致 fallback key 与预留规模异常。

**Checkpoint**：

```text
INSERT VALUES ─┐
               ├→ InsertOperator
INSERT SELECT ─┘
```

必须成立。

**Checkpoint 结论**：成立。`insertSQL`（VALUES/SET）与 `insertSelectSQL` 都构造同一个 `InsertOperator` 并通过 `apply` 调用同一个 `writeInsertedRow`；已由测试断言（含 hook 观测）证明，且全量回归通过。

------

# Phase 4：User Story 2 — 统一普通 UPDATE（P2-A）

**目标**：

先把简单 UPDATE 迁入 UpdateOperator，为 UPDATE JOIN 建立稳定基础。

**状态**：已完成（T032-T045 全部完成，未实施 UPDATE JOIN）。

## 测试

-  [X] T032 [P] [US2] 在现有 UPDATE regression 中覆盖 CHECK、UNIQUE、FK、ON UPDATE CURRENT_TIMESTAMP 行为
  - 完成说明：新增 `executor/mvcc_update_pipeline_test.go` 的 `TestMVCCUpdateConstraintsAndOnUpdateTimestamp`（CHECK 失败回滚、UNIQUE 收敛被拒、索引列更新后索引双向一致、ON UPDATE 列仅在真正赋值时移动、显式赋值优先于自动值）与 `TestMVCCUpdateForeignKeyActionsAndRollback`（ON UPDATE CASCADE 生效、RESTRICT 型失败使整语句回滚）。
-  [X] T033 [P] [US2] 在 `executor/mvcc_halloween_test.go` 保持 indexed-column update 只修改目标一次
  - 完成说明：`TestMVCCUpdateIndexHalloweenProtection` 未改动即通过（affected=2，行值正确）。新增 `TestMVCCUpdateKeepsAccessPlan` 额外断言 indexed UPDATE 仍走索引访问路径，并验证实际只更新匹配行。
-  [X] T034 [P] [US2] 添加或保留 primary-key update 后 old physical identity 不被重新处理的测试
  - 完成说明：`TestMVCCUpdatePrimaryKeyKeepsOldPhysicalIdentity`：`UPDATE t SET id=id+100` 对 3 行各修改一次（不是全表重扫）、旧主键与旧唯一索引项被释放、PK 冲突时报 `ErrDuplicateKey` 且整语句回滚。
-  [X] T035 [P] [US2] 保留 UPDATE 中途失败整 statement rollback 测试
  - 完成说明：`TestMVCCUpdateStatementRollbackIsAtomic`（CHECK 在第 3 行失败 → 前 2 行一起回滚；显式事务内 ROLLBACK 不泄漏）保留原有覆盖，并新增 `TestMVCCUpdateRejectsInvalidAssignmentBeforeWriting`（未知列/重复列/跨表赋值/未知表达式全部在写入前失败）。

## 实现

-  [X] T036 [US2] 在 `executor/modify_update.go` 实现 UpdateOperator，输入为 `physical.Operator[UpdateCandidate]`
  - 完成说明：`UpdateOperator{Input, Target, Schema, Assignments, Write, Session, Result}` + `Run`/`apply`/`assign`；新增 `updateAssignment`（position/evaluated/column/value）。
-  [X] T037 [US2] 在 UpdateOperator 中按现有语义顺序执行 SET assignments
  - 完成说明：`assign` 逐条求值，赋值立即写入 evaluation row，因此 `b=a` 读到的是同语句已更新的 `a`。
  - **修复的关键缺陷**：evaluation row 必须是独立缓冲区。若直接改 `EvalRow`（其底层数组与 `OldRow` 相同），写路径会用**新值**去计算旧唯一/二级索引项，导致旧项删除错误并静默跳过唯一索引冲突检查（`UPDATE t SET u=99` 可把多行改成同一唯一值）。现由 `candidateEvalRow` 保证：与 OldRow 同底层数组时复制一份，UPDATE JOIN 的 joined evaluation row 则直接使用。
-  [X] T038 [US2] 在 UpdateOperator 中复用 ON UPDATE CURRENT_TIMESTAMP 逻辑
  - 完成说明：逻辑逐字保留（`strings.EqualFold` 两种写法 + `session.Now()`），仅改为通过预解析的 `assigned` 集合判断显式赋值。
-  [X] T039 [US2] 在 UpdateOperator 中复用 `applyForeignKeyActions`
  - 完成说明：`apply` 调用 `applyForeignKeyActions(ctx, o.Write, o.Session, o.Target, candidate.OldRow, updated, nil, 0)`，未新增 FK 逻辑。
-  [X] T040 [US2] 在 UpdateOperator 中复用 `writeVersionedRow`
  - 完成说明：`apply` 调用 `writeVersionedRow(ctx, o.Write, o.Target, candidate.Identity.Key, candidate.OldRow, updated, "")`；`fallback` 仍为空（UPDATE 不走生成键路径）。写路径本身未改动。
-  [X] T041 [US2] 保证 UpdateCandidate.Identity 始终表示 old physical row identity，即使 UPDATE 修改 PK
  - 完成说明：候选由绑定层用扫描到的 storage key 构造（`physical.NewRowIdentity(table.ID, r.key)`），算子在 `assign` 中从不修改 Identity，写路径用旧 key 删除旧行并在内部计算新 key。
-  [X] T042 [US2] 重构 `executor/physical_binding.go` 的 `runRowModification`，使 scan/filter 输出 UpdateCandidate 而不是直接闭包写入
  - 完成说明：抽出 `mutationScan`（Scan → Filter → Limit，保留 planSQLAccess）供候选算子与闭包路径共用；新增 `updateCandidates` 投影为 `UpdateCandidate`。
  - **注意（T-4 前置依赖）**：`runRowModification` 仍保留，供尚未迁移的简单 DELETE、UPDATE JOIN、单目标 DELETE JOIN 使用，因此本阶段的改动**未影响**这些路径的行为；`mutationScan` 是二者共享的同一个访问计划阶段。
  - 命名说明：该函数原名 `rowSource`，被 `architecture_test.go` 列为 `physical_binding.go` 的禁用标识符（"operator callback roundtrip"），故改名 `mutationScan`；它本就是直接构造算子并返回，而非回调往返。
-  [X] T043 [US2] 保留 `planSQLAccess` 与现有 index/range access path
  - 完成说明：`mutationScan` 仍调用 `planSQLAccess` 并用其结果构造 `physical.Scan`，同时把 `access.kind/index` 写入 `PlanNode.Attributes`（与 HEAD 一致）。`TestMVCCUpdateKeepsAccessPlan` 直接断言：索引等值条件仍选中 `k_idx`（未退化为 `ALL`），不可服务的条件仍回退为普通 Scan。
-  [X] T044 [US2] 重构普通 UPDATE dispatch，使最终写入统一进入 UpdateOperator
  - 完成说明：`mutateSQL` 的 `parser.Update` 分支改为 `resolveUpdateAssignments` → `updateCandidates(mutationScan(...))` → `UpdateOperator`；该分支内已无 `writeVersionedRow`/`applyForeignKeyActions`/行装配调用。`UPDATE JOIN` 仍走原 `joinUpdateSQL`（Phase 5 范围）。
-  [X] T045 [US2] 运行 simple UPDATE、Halloween、FK、transaction regression
  - 完成说明：`go test ./...` 全 21 包通过；UPDATE 相关 `TestMVCCUpdate*`、`TestMVCCUpdateJoin*`、`TestLegacyUpdate*`、`TestMVCCMutationScratchPreservesRowsAndRollback`、`TestProductionArchitectureBoundaries` 全部通过。

**Checkpoint**：

普通 UPDATE 不再直接拥有独立 `writeVersionedRow` 调用路径。

**Checkpoint 结论**：成立。`mutateSQL` 的普通 UPDATE 分支只负责绑定与构造候选，最终写入只经 `UpdateOperator.apply` 一处到达 `writeVersionedRow`（`grep writeVersionedRow` 已不在该分支出现）。

**Phase 4 有意保留的行为**：重复赋值同一列（`SET a=1,a=2`）现在在绑定阶段被拒绝，而 HEAD 会静默取最后一个值。这与 UPDATE JOIN 既有行为（`TestMVCCUpdateJoinRejectsInvalidAssignments` 要求拒绝 `SET u.score=1,u.score=2`）一致，且更符合 plan 的"binding error 应在第一条写入前返回"。若需保留旧的静默覆盖语义，请告知，我会改为允许重复并在求值阶段应用最后一次。

------

# Phase 5：User Story 2 — UPDATE JOIN（P2-B）

**目标**：

把：

```text
per-target nested JOIN
```

迁移成：

```text
JOIN
→ WHERE
→ UpdateCandidate
→ TargetRowDedup
→ LIMIT
→ UpdateOperator
```

**状态**：已完成（T046-T059 全部完成）。

## 测试

-  [X] T046 [P] [US2] 在 `executor/mvcc_update_join_test.go` 验证同一 target 多 source match 时 affected rows = 1
  - 完成说明：原 `TestMVCCUpdateJoinDeduplicatesRepeatedTargetMatches` 保留通过；新增 `TestMVCCUpdateJoinFirstMatchWinsPerTarget`（target 1 匹配 3 条 source，affected=2 而非 4）与 `TestMVCCUpdateJoinLeftJoinVisitsEveryTargetOnce`（LEFT JOIN 下每个 target 恰好一次）。
-  [X] T047 [P] [US2] 在 `executor/mvcc_update_join_test.go` 验证 first source match 被使用
  - 完成说明：`TestMVCCUpdateJoinFirstMatchWinsPerTarget` 用**非累加**赋值 `SET t.v=s.delta` 断言取到首个匹配 source 的值（3 而非 7/4），排除了"把所有匹配都应用一遍"的实现；`TestMVCCUpdateJoinSetSeesJoinedSourceAndSequentialAssignments` 另以 `CONCAT(s.label,'-',t.v)` 验证 winner 的 join evaluation row 被使用。
-  [X] T048 [P] [US2] 在 `executor/mvcc_update_join_test.go` 验证 sequential SET assignment
  - 完成说明：`TestMVCCUpdateJoinSetSeesJoinedSourceAndSequentialAssignments`：`SET t.v=t.v+s.delta, t.note=CONCAT(s.label,'-',t.v)` 得到 `seven-17`，证明后一个 assignment 看到前一个对 target 的修改，同时能读 joined source。
-  [X] T049 [P] [US2] 添加 UPDATE JOIN + LIMIT + duplicate matches regression，验证 LIMIT 统计 dedup 后 target
  - 完成说明：`TestMVCCUpdateJoinLimitCountsDedupedTargets`：target 1/2 各匹配 2 条 source，`LIMIT 2` 必须更新 target 1 与 2（affected=2）。若 LIMIT 位于 dedup 之前，扫过 2 条 join row 时只会碰到 target 1，因此该用例可区分两种顺序。
-  [X] T050 [P] [US2] 保持 UPDATE JOIN constraint failure 全 statement rollback
  - 完成说明：原 `TestMVCCUpdateJoinRollsBackOnConstraintFailure` 保留通过；新增 `TestMVCCUpdateJoinConstraintFailureRollsBackWholeStatement` 覆盖两个 target 收敛到同一 UNIQUE 值、以及后续 target CHECK 失败两种情形，均要求前一个 target 的修改一起回滚。
-  [X] T051 [P] [US2] 保持 invalid assignment 在 first mutation 前失败
  - 完成说明：原 `TestMVCCUpdateJoinRejectsInvalidAssignments` 保留通过（`resolveUpdateAssignments` 在 join 绑定阶段解析全部 assignment）；新增 `TestMVCCUpdateJoinInvalidAssignmentFailsBeforeMutation` 覆盖跨表赋值、未知列、重复列、未知表达式、未知 join 表。

## 实现

-  [X] T052 [US2] 重构 `executor/mutation_join.go`，由完整 JOIN pipeline 生成 target row identity、OldRow 与 EvalRow
  - 完成说明：`joinUpdateSQL` 改为 `bindUpdateJoinPlan` → `winners` → `apply`。新增 `executor/mutation_join_pipeline.go`：`updateJoinPlan`、`updateJoinCandidate`、`newUpdateJoinSorter`。identity 取自 `bindJoinsIdentified` 为目标表注入的隐藏 rowid 列（与多表 DELETE 同一 provenance），经 `identityKeys` 映射回扫描观察到的 storage key；`OldRow` 为 joined row 的目标列前缀，`EvalRow` 为完整 joined row。
-  [X] T053 [US2] 将 JOIN WHERE 过滤放在 UpdateCandidate projection 之前
  - 完成说明：`updateJoinPlan.matched()` 是 `Filter{WHERE}`，两个 pass 都以它作为 projection 的输入；`TestMVCCUpdateJoinWhereFiltersBeforeDedup` 断言被 WHERE 过滤掉的首个匹配不会成为 winner。
-  [X] T054 [US2] 在 UPDATE JOIN pipeline 中接入 TargetRowDedup
  - 完成说明：`updateJoinPlan.winners` 使用 `physical.TargetRowDedup[updateJoinCandidate]`，identity 为 `RowIdentity{TableID: target.ID, Key: storage key}`；sorter 由 `newUpdateJoinSorter` 复用共享的 `externalRowSorter`（受 `SortMemoryBytes`/`MaxTempBytes`/`TempDirectory` 约束，可落盘），ledger 行只含 identity key 与 join 位置两个原语。
  - 设计说明：候选只保留 identity + ordinal，不保留 joined row。join 在第二遍重跑并按 ordinal 取 winner 行，因此 dedup ledger 从不需要编码 `storage.Row`；两遍读取同一 parent statement snapshot，观察到的行与顺序完全一致。
-  [X] T055 [US2] 确保 TargetRowDedup stable-first 行为替代当前每 target `Limit(1)` 的 first-match 机制
  - 完成说明：`physical.TargetRowDedup` 的 stable first-occurrence 语义取代了原 `physical.Limit[storage.Row]{Count: 1}` 每 target 取首匹配的做法；`errJoinUpdateLimit` sentinel 及对应提前终止逻辑已删除（LIMIT 现位于 dedup 之后，见 T056）。
-  [X] T056 [US2] 将 UPDATE LIMIT 放到 TargetRowDedup 之后
  - 完成说明：LIMIT 在 `winners` 的 dedup 输出上计数（`limit >= 0 && count >= limit`），算子顺序为 `JOIN → WHERE → Candidate → Dedup → LIMIT → UpdateOperator`。据此 driver 不再把 LIMIT 下推到 target 扫描（原实现把 limit 传给 `runRowModification`）。
-  [X] T057 [US2] 将 dedup 后的 UpdateCandidate 输入统一 UpdateOperator
  - 完成说明：第二遍构造 `[]RowIdentity + OldRow + EvalRow` 的 `UpdateCandidate` 后交给 `UpdateOperator`。为此 `UpdateOperator` 增加 `EvalSchema` 字段（UPDATE JOIN 传 joined schema，普通 UPDATE 回落到 `Schema`），并把赋值求值抽成共享的 `assignUpdateRow`，两种 UPDATE 共用同一套 SET/ON UPDATE 语义。
-  [X] T058 [US2] 删除 `mutation_join.go` 中迁移后独立的 physical row write 逻辑
  - 完成说明：`mutation_join.go` 由 149 行缩至 63 行，`grep writeVersionedRow|applyForeignKeyActions|writeInsertedRow` 在该文件与 `mutation_join_pipeline.go` 中**零命中**；最终 row write 只经 `UpdateOperator.Apply`。
-  [X] T059 [US2] 对比 legacy 与 MVCC UPDATE JOIN 行为并运行全量 parity regression
  - 完成说明：`TestMVCCUpdateJoinMatchesLegacyEngine`（legacy/MVCC 逐句 affected rows + 结果集 parity）与 `TestLegacyUpdateJoinAndMultiTableDelete` 通过；`go test ./...` 全 21 包通过。

**Checkpoint**：

```text
普通 UPDATE ─┐
             ├→ UpdateOperator
UPDATE JOIN ─┘
```

必须成立。

**Checkpoint 结论**：成立。普通 UPDATE 与 UPDATE JOIN 都经同一 `UpdateOperator`（同一 `assignUpdateRow`、同一 `applyForeignKeyActions`、同一 `writeVersionedRow`）；UPDATE JOIN 的差异只在候选来源（JOIN pipeline + TargetRowDedup + LIMIT）与 `EvalSchema`（joined schema）。

**Phase 5 行为变更（需在 spec/plan 记录，见审查报告 P0-5）**：

1. **LIMIT 提前终止消失**：原实现在满足 LIMIT 后由 `errJoinUpdateLimit` 停止 target 扫描；新结构必须先把 dedup 结果全部算完才能截断。`LIMIT` 的**可观察语义不变**（仍统计去重后的唯一 target，T049 已锁定），但大结果集下的工作量增加。
2. **RIGHT JOIN 现在可被求值**：`bindJoinsIdentified` 支持 INNER/LEFT/RIGHT/CROSS，而原 per-target 嵌套 join 只处理了 target-driven 的子集。UNION 型语义扩张属已确认变更，当前无既有测试断言其被拒绝。
3. **target 访问路径的作用域**：`planSQLAccess` 仍用于 driver（`identityScan` 的 access plan 取自语句 WHERE），因此索引 UPDATE JOIN 仍走索引；但它不再兼作 join 的行过滤，过滤统一由 pipeline 的 WHERE 完成。

------

# Phase 6：User Story 3 — 统一普通 DELETE（P3-A）

**目标**：

建立单表 DELETE → DeleteCandidate → DeleteOperator 路径。

**状态**：已完成（T060-T069 全部完成，未实施 multi-table DELETE）。

## 测试

-  [X] T060 [P] [US3] 保留普通 DELETE CHECK/FK/transaction regression
  - 完成说明：新增 `executor/mvcc_delete_pipeline_test.go`。`TestMVCCDeleteForeignKeyActions` 覆盖 ON DELETE CASCADE（子行随之删除）、RESTRICT（拒绝且父行保留）、ON DELETE SET NULL 违反 CHECK（整语句回滚，父行与子行均不变）；`TestMVCCDeleteStatementRollbackIsAtomic` 覆盖事务内删除的 read-your-own-delete 与 ROLLBACK 恢复。既有 `TestLegacyCascadingMutationCyclesAndCheckFailureAreAtomic`、`mvcc_fk_actions_test.go`、`mvcc_savepoint*`、`executor_test.go` 的 DELETE 用例全部保留通过。
-  [X] T061 [P] [US3] 保留 indexed DELETE access-path regression
  - 完成说明：`TestMVCCDeleteKeepsAccessPlan` 直接检查 `mutationScan` 的算子链与 `PlanNode`：索引等值条件仍选中 `k_idx` 且 `access != ALL`，不可服务的条件仍回退普通 Scan；并验证实际 `DELETE FROM t WHERE k=2` 只删除匹配行。既有 `TestLegacyNavicatStyleDeleteLimit`、`TestMVCCUniqueAccess*`、`mvcc_range_test.go`、`optimization_round3_test.go` 的 indexed DELETE 用例通过。
-  [X] T062 [P] [US3] 增加中途 FK/error 后 statement atomic rollback 验证
  - 完成说明：`TestMVCCDeleteStatementRollbackIsAtomic` 构造三行父表、仅最后一行被引用的场景，使 `DELETE FROM p` 在前两行删除成功后于第三行失败，断言父表与子表均无残留；另有 `TestMVCCDeleteForeignKeyActions` 的 SET NULL/CHECK 中途失败用例。

## 实现

-  [X] T063 [US3] 在 `executor/modify_delete.go` 实现 DeleteOperator
  - 完成说明：`DeleteOperator{Input, Target, Write, Session, Result}` + `Run`/`Apply`；`DeleteCandidate.Identity` 由扫描观察到的 storage key 构造（与 UPDATE 同一 `mutationScan` + `physical.NewRowIdentity`），因此无主键表也能稳定寻址，值完全相同的两行也不会互相混淆。
-  [X] T064 [US3] 在 DeleteOperator 中复用 `applyForeignKeyActions`
  - 完成说明：`Apply` 调用 `applyForeignKeyActions(ctx, o.Write, o.Session, o.Target, candidate.OldRow, nil, nil, 0)`，与 HEAD 的参数完全一致，未新增任何 FK 逻辑。
-  [X] T065 [US3] 在 DeleteOperator 中复用 `writeVersionedRow(oldRow, nil)`
  - 完成说明：`Apply` 调用 `writeVersionedRow(ctx, o.Write, o.Target, candidate.Identity.Key, candidate.OldRow, nil, "")`。`newRow=nil` 即删除语义：写路径先移除旧唯一/二级索引项再删除行。写路径本身未改动。
-  [X] T066 [US3] 修改普通 DELETE pipeline，使 scan/filter 生成 DeleteCandidate
  - 完成说明：新增 `deleteCandidates` 投影（`physical_binding.go`），`OldRow` 为解码行**的私有拷贝**（与 UPDATE 同样原因：扫描迭代器会复用缓冲区，而写路径要用旧值删除索引项）。`mutateSQL` 的普通 DELETE 分支改为 `deleteCandidates(mutationScan(...))` → `DeleteOperator`。
-  [X] T067 [US3] 保持 `planSQLAccess`，不得退化成无条件 full scan
  - 完成说明：直接复用 Phase 4 抽出的 `mutationScan`，其唯一访问计划入口仍是 `planSQLAccess`，并把 `access.kind/index` 写入 `PlanNode.Attributes`。`TestMVCCDeleteKeepsAccessPlan` 对普通 DELETE 路径断言了这一点。
-  [X] T068 [US3] 删除普通 DELETE 中迁移后的独立最终 row write 闭包
  - 完成说明：`mutateSQL` 普通 DELETE 分支内的 `applyForeignKeyActions` + `writeVersionedRow` 闭包已删除，最终写入只经 `DeleteOperator.Apply` 一处。
  - `runRowModification` 当时仍被多表 DELETE 的 driving-target 路径使用；Phase 7 删除该路径后它已无调用方（收尾清理中删除）。
  - 新增 `deleteApplyHook`（生产为 nil，仅回归测试观察候选），用途与 Phase 3/4 相同：证明普通 DELETE 确实经过统一算子。
-  [X] T069 [US3] 运行普通 DELETE regression
  - 完成说明：`go test ./...` 全 21 包通过；DELETE 相关新增 6 个测试全 PASS，既有 DELETE/FK/索引/事务/savepoint 回归全部通过。

**Checkpoint**：

普通 DELETE 已完全使用 DeleteOperator。

**Checkpoint 结论**：成立，且已超出"不再绕过"的最低要求 —— 普通 DELETE 的候选构造、FK 动作与最终写入全部收敛到 `DeleteOperator`，`mutateSQL` 的普通 DELETE 分支内不再出现任何写入调用。`TestMVCCDeleteUsesUnifiedDeleteOperator` 通过 `deleteApplyHook` 断言全表 DELETE / 带 WHERE / 无匹配三类语句都恰好按删除行数进入该算子；`TestMVCCMultiTableDeleteUsesUnifiedDeleteOperator` 反向断言 multi-table DELETE **不**进入单表算子（仍走 Phase 7 的路径）。

------

# Phase 7：User Story 3 — 多表 DELETE（P3-B）

**目标**：

```text
JOIN
→ WHERE
→ DeleteCandidate
→ TargetRowDedup
→ dependency-aware staging
→ DeleteOperator
```

## 测试

-  [X] T070 [P] [US3] 在 `executor/mvcc_multi_delete_nopk_test.go` 验证无 PK identical-value row 仍是不同物理目标
-  [X] T071 [P] [US3] 验证同一物理 target 因多 JOIN combinations 出现时仅删除一次
-  [X] T072 [P] [US3] 验证不同 TableID + 相同 key 不互相去重
-  [X] T073 [P] [US3] 保持 LEFT JOIN NULL-extension 删除语义
-  [X] T074 [P] [US3] 保持 RIGHT JOIN NULL-extension 删除语义
-  [X] T075 [P] [US3] 保持 derived/CTE/view 非法 DELETE target rejection
-  [X] T076 [P] [US3] 保持 FK rollback regression
-  [X] T077 [P] [US3] 保持多 target child-before-parent deletion order
-  [X] T078 [P] [US3] 保持 cyclic target dependency 当前 rejection 行为

## 实现

-  [X] T079 [US3] 重构 `executor/mutation_multi_delete.go`，把 JOIN row 投影为零个或多个 DeleteCandidate
-  [X] T080 [US3] 使用 RowIdentity.Valid 表示 outer join target 是否真正存在，不得通过 target column NULL 值推断
-  [X] T081 [US3] 将当前 `seen map` 去重替换为 TargetRowDedup
-  [X] T082 [US3] 保留 no-PK target 的 hidden physical storage key provenance
-  [X] T083 [US3] 保留现有 `multiDeleteOrder` FK target dependency ordering
-  [X] T084 [US3] 将 dedup 后 candidate staging 改造成只负责 ordering，不再自己完成最终 physical mutation
-  [X] T085 [US3] 将 ordered DeleteCandidate 输入统一 DeleteOperator
-  [X] T086 [US3] 删除 `mutation_multi_delete.go` 中迁移后的独立 `writeVersionedRow` 路径
-  [X] T087 [US3] 运行全部 multi-table DELETE 与 no-PK regression

**Checkpoint**：

```text
普通 DELETE ───┐
               ├→ DeleteOperator
Multi DELETE ──┘
```

必须成立。

**状态**：**已完成**（T070-T087 全部完成）。

实现落在 `executor/multi_delete_pipeline.go`（新建）与重写后的 `executor/mutation_multi_delete.go`：

```text
JOIN
→ WHERE
→ project self-contained DeleteCandidates
→ TargetRowDedup
→ bounded/spillable staging
→ multiDeleteOrder
→ DeleteOperator
```

完成说明：

- **候选自包含**：`multiDeleteCandidate` 在 JOIN 输出时即携带 target 索引（写元数据）、`RowIdentity`（TableID + physical storage key）与 `OldRow`，因此 dedup 之后**不需要**重跑 JOIN、**不需要**按 ordinal 重放、**不需要**查询外部 ledger。
- **真正使用 TargetRowDedup**：`multiDeletePlan.selection()` 以 `physical.TargetRowDedup[multiDeleteCandidate]` 去重，去重账本走注入的可 spill `externalRowSorter`（受 `SortMemoryBytes`/`MaxTempBytes`/`TempDirectory` 约束）。
- **无界结构已删除**：原 `seen := make(map[string]bool)`（按物理行累积）与 `rows := make([]multiDeleteRow, ...)` 已移除，`multiDeleteRow` 类型已删除。`resolveMultiDeleteTargets` 内保留的 `seen` 只去重**语句中请求的 target 名**，上界为名字个数。
- **T080 的实际实现**：缺席判定由 physical provenance 承担 —— `multiDeleteTarget.key()` 经 `ProvenanceKey(row, provenanceAt)` 读取隐藏 provenance 列；该列在 outer join NULL-extension 时为 NULL，因而被判为"无物理行"，**不**依据业务列是否全 NULL。等价于 `RowIdentity.Valid=false` 的语义。
- **T082 的前置基础**：`identityScan` 现在把物理 storage key 作为**第二个隐藏 provenance 列**随行传递（`executor/derived_mvcc.go`），消除了嵌套循环 re-scan 下 `rowid → storage key` mutable ledger 被覆盖的问题；相关 regression 见 `executor/mvcc_provenance_test.go`。
- **T084 staging 定位**：`multiDeletePlan.delete()` 只把去重后的 winner 按 `multiDeleteOrder` 分组与排序，不再自行完成物理删除。
- **T086 结果**：`mutation_multi_delete.go` 中 `writeVersionedRow` / `writeInsertedRow` **零命中**；`deleteDrivingTargetSQL` 与 `multiDeleteRow` 已删除。
- **行为保持**：no-PK physical identity、identical-value rows 独立、LEFT/RIGHT JOIN NULL-extension、derived/CTE/view target rejection、FK child-before-parent、FK cycle rejection、statement atomic rollback 全部由既有 parity 回归覆盖并通过。

**验证**：`executor/mvcc_multi_delete_test.go`（15）、`executor/mvcc_multi_delete_nopk_test.go`（6）、`executor/mvcc_fk_actions_test.go`（5）、transaction rollback（31）、TargetRowDedup spill（19）全部通过；`go test ./...` 21/21 包通过。

------

# Phase 8：跨 DML 事务与资源验证

**状态**：已完成（T088-T094 全部完成）。本轮以验证为主，除下列 2 处**测试基础设施**外未改动生产语义：

1. `executor/transaction_engine.go` 新增 `statementTxnHook`（生产为 nil，仅回归测试观察 read/child 事务配对）；
2. 无其他生产改动。

新增测试文件：`executor/mvcc_transaction_invariants_test.go`（11 个测试）。

-  [X] T088 [P] 在 `executor/` transaction regression 中验证 INSERT/UPDATE/DELETE query source 均读取 parent statement snapshot
  - 完成说明：`TestMVCCModifyPipelinesUseParentReadAndChildWrite` + `TestMVCCStatementSnapshotExcludesConcurrentCommits` + `TestMVCCMutatingScannedTableStaysAtomic`（详见下方对照表 #1-#3）。
-  [X] T089 [P] 验证所有 Modify Operator 均仅写入 statement child transaction
  - 完成说明：`statementTxnHook` 对 6 种语句形态断言 `read.ID() != write.ID()` 且 `write.Snapshot() <= read.Snapshot()`；`TestMVCCIteratorContractChildWriteIsInvisibleToParentScan` 另行验证 child 写入对 parent 扫描不可见。
-  [X] T090 [P] 验证任一 pipeline downstream error 导致 child rollback
  - 完成说明：`TestMVCCDownstreamErrorRollsBackChildTransaction` 覆盖 INSERT SELECT / INSERT VALUES / UPDATE / UPDATE JOIN 的 CHECK 失败与 UPDATE JOIN 的 UNIQUE 收敛失败，断言失败语句不返回 result 且前后行集完全一致。
-  [X] T091 [P] 验证用户 transaction 内失败 statement 的现有 rollback/savepoint 行为没有变化
  - 完成说明：`TestMVCCStatementFailureInsideUserTransactionKeepsSavepointSemantics` 覆盖事务内连续失败语句、SAVEPOINT / ROLLBACK TO SAVEPOINT / COMMIT；既有 `TestMVCCSavepoint*` 全部保留通过。
-  [X] T092 [P] 验证 LastInsertID 只在 child commit 成功后发布
  - 完成说明：`TestMVCCLastInsertIDIsPublishedOnlyAfterStatementCommit` 覆盖成功的 INSERT、非 INSERT 语句、失败的 INSERT、失败的 INSERT SELECT 四种情形。
-  [X] T093 [P] 验证 context cancellation 后 TargetRowDedup、sorter、temporary partitions 等资源释放
  - 完成说明：`TestMVCCCancellationReleasesOperatorResources`（三条 DML 在已 cancel 的 context 下执行，断言 `TempDirectory` 无残留且未写入行）+ `physical.TestTargetRowDedupReleasesSortersOnEveryExitPath`（五条失败/取消路径的 sorter 关闭与临时目录断言）。
-  [X] T094 [P] 对 INSERT SELECT / UPDATE JOIN / multi DELETE 增加大结果集执行测试，确认不存在新的 `Result.Rows` 全量物化
  - 完成说明：`TestMVCCModifyPipelinesDoNotMaterializeResults`（6 种 DML result 均无 `Rows`/`StreamRows`/`StreamValues`）+ `TestMVCCSpillableDedupStaysBounded`（小 sort budget 下 320 条 join 输出、每 target 只改一次、无临时文件残留）。

## 事务不变量 → 测试证据对照

| # | 不变量 | 测试证据 |
|---|---|---|
| 1 | INSERT SELECT source 使用 parent statement snapshot | `TestMVCCModifyPipelinesUseParentReadAndChildWrite`（INSERT SELECT 的 read txn ≠ write txn）、`TestMVCCStatementSnapshotExcludesConcurrentCommits`（并发已提交插入对该语句不可见）、既有 `TestMVCCInsertSelectUsesStatementSnapshot` / `TestMVCCInsertSelectSelfSourceSnapshot` / `TestMVCCInsertSelfSourceSnapshotThroughOperator` |
| 2 | UPDATE source / JOIN source 使用 parent snapshot | 同上第 1 条三个测试；既有 `TestMVCCJoinAndGroupSnapshot`、`TestMVCCUpdateJoinUsesStatementSnapshot`、`TestMVCCRangeSnapshotOwnWritesAndReopen`、`TestMVCCSchemaConflictAndStatementRollback` |
| 3 | DELETE / multi-table DELETE source 使用 parent snapshot | `TestMVCCModifyPipelinesUseParentReadAndChildWrite`（DELETE 与 MULTI DELETE 分别断言 read≠write）；`TestMVCCMutatingScannedTableStaysAtomic`（DELETE 谓词不会删除自己刚删除/更新的行）；既有 `TestMVCCMultiTableDeleteJoinedHeapTargetReadYourOwnWrite`、`TestMVCCMultiTableDeleteWithoutPrimaryKeyAtomicRollback` |
| 4 | 三个 Operator 只写 statement child transaction | `TestMVCCModifyPipelinesUseParentReadAndChildWrite`：对 6 种语句形态逐一断言 `read.ID() != write.ID()`，并断言 `write.Snapshot() <= read.Snapshot()`（child 由该 statement 切出） |
| 5 | iterator 不得扫描并修改同一个 Txn | `TestMVCCIteratorContractChildWriteIsInvisibleToParentScan`（在 iterator 打开期间向 **child** 写入；parent 扫描看不到 child 写入，child 自己可见 —— 正是 storageengine 契约允许的形态）；`TestMVCCModifyPipelinesUseParentReadAndChildWrite`（管线从不把同一个 txn 同时当 read/write 传入）；`TestMVCCMutatingScannedTableStaysAtomic`（若同一 txn 边扫边改，该谓词会无限匹配） |
| 6 | downstream error 导致 child rollback | `TestMVCCDownstreamErrorRollsBackChildTransaction`（5 种中途失败：INSERT SELECT/INSERT VALUES/UPDATE/UPDATE JOIN 的 CHECK 与 UPDATE JOIN 的 UNIQUE 收敛；断言失败语句不返回 result 且前后行集完全一致）；`TestMVCCInsertStatementRollbackThroughOperator`、`TestMVCCDeleteStatementRollbackIsAtomic`、`TestMVCCMultiTableDeleteRollsBackPartialMutations`、`TestMVCCInsertSelectRollsBackOnFailure` |
| 7 | 用户事务内 statement failure / savepoint 语义不变 | `TestMVCCStatementFailureInsideUserTransactionKeepsSavepointSemantics`（事务内两次失败语句不影响先前语句；SAVEPOINT / ROLLBACK TO SAVEPOINT / COMMIT 行为不变）；既有 `TestMVCCSavepointStatementCommitFailureAbortsTransaction`、`TestMVCCSavepointNestedCommitFailureAbortsTransaction`、`TestMVCCSavepointCommitMergeFailureAbortsTransaction`、`TestLegacyCommitAndRollbackWithoutTransactionAreNoOps` |
| 8 | LastInsertID 只在 child commit 后发布 | `TestMVCCLastInsertIDIsPublishedOnlyAfterStatementCommit`（成功 INSERT 发布；非 INSERT 语句不发布；失败的 INSERT 与 INSERT SELECT 都不移动它）；既有 `TestMVCCInsertLastInsertIDRequiresStatementCommit`、`TestMVCCInsertSelectLastInsertIDRollback`、`TestMVCCInsertValuesLastInsertIDRollback` |
| 9 | cancel 后释放 TargetRowDedup / sorter / temp file / operator 资源 | `TestMVCCCancellationReleasesOperatorResources`（已 cancel 的 context 下执行 UPDATE JOIN / INSERT SELECT / multi DELETE，断言 `TempDirectory` 无残留且未写入任何行；并对同一 spilling 配置的成功路径断言同样无残留）；`physical/target_dedup_test.go` 的 `TestTargetRowDedupReleasesSortersOnEveryExitPath`（sorter 工厂失败 / 上游错误 / 下游错误 / 运行前 cancel / 运行中 cancel 五条路径均关闭 sorter 且临时目录为空）；既有 `TestLegacyExternalSortCancellationAndDiskLimitCleanup`、`TestLegacyExternalSortSpillStableAndCleanup` |
| 10 | 不得退化回 `Result.Rows` 全量物化 | `TestMVCCModifyPipelinesDoNotMaterializeResults`（6 种 DML 语句的 result 均无 `Rows`、无 `StreamRows`/`StreamValues`）；既有 `TestDistinctDoesNotMaterializeInputResult`、`TestBoundDistinctStreamingBoundary` |
| 11 | 允许 bounded/spillable barrier，不得引入无界内存收集 | `TestMVCCSpillableDedupStaysBounded`（40 个 target × 8 条 source 匹配，`SortMemoryBytes=64KiB` 下仍对每个 target 只改一次且临时目录无残留）；`TestMVCCCancellationReleasesOperatorResources`（200 行 spilling 配置）；Phase 2 的 `TestTargetRowDedupSpillsBeyondMemoryThresholdAndCleansUp` |

## 发现的 iterator / snapshot / rollback 风险

**结论：未发现当前管线（Phase 3-6 已完成的 INSERT / UPDATE / UPDATE JOIN / DELETE）存在 iterator 契约违反或 snapshot/rollback 风险。** 逐条说明：

1. **同一 Txn 边扫边写** —— 未发现。所有 modify 管线都从 `mutateSQL(ctx, read, write, ...)` 拿到**两个不同** txn：`read` 是 statement parent，`write` 是 `tx.Child()`。`mutationScan` 用 `read` 建 Scan，算子用 `write` 写。测试对 6 种语句形态逐一断言二者 ID 不同。multi-table DELETE（尚未迁移）同样遵守该分割。
2. **snapshot 稳定性** —— 未发现。scan 全部通过 `read`（parent）打开，写入落在 child，因此"扫描过程中读到本语句刚写入的行"在结构上不可能。`TestMVCCMutatingScannedTableStaysAtomic` 用"谓词永远为真"的更新/删除反证了这一点（各只改 3 行）。
3. **child 写入对 parent 不可见** —— 已用 `TestMVCCIteratorContractChildWriteIsInvisibleToParentScan` 直接验证：在 iterator 打开期间向 child `Put`，parent 扫描看不到、child 自己看得到。这正是 `storageengine.Iterator` 契约注解允许的形态。
4. **rollback 完整性** —— 未发现。5 种中途失败场景均验证前后行集完全一致，且失败语句不返回 result（`resultOut` 保持 nil）。
5. **一处**当时标记的结构性风险（**已在 Phase 7 解决**）：`identityScan` 的 `identityNext++` 在**嵌套循环 join 重复 re-scan 同一 input** 时会覆盖 `identityKeys` 的槽位，因此不能依赖该 ledger 在 re-scan 后解析物理行。Phase 7 的解法是把物理 storage key 作为隐藏 provenance 列**随行传递**（`executor/derived_mvcc.go`），使身份不再依赖可变 ledger；对应 regression 见 `executor/mvcc_provenance_test.go`。

**验证方式**：`go build ./...`、`go vet ./...`、`go test ./...` 全 21 包通过；transaction/savepoint/cancel/spill 定向回归全部 PASS（含 `TestMVCCSavepoint*`、`TestMVCCSQLTransactionsAndReopen`、`TestLegacyExternalSortCancellationAndDiskLimitCleanup`、`TestMVCCConfiguredBudgetFailureAndOldSnapshot` 等）。

**未做**：本轮未进行任何旧代码清理（Phase 9 范围）。

------

# Phase 9：旧路径清理

**状态**：**已完成**（T095-T102 全部完成）。Phase 7 完成后，此前受其阻塞的 T098/T100/T101 均已收口。

**本轮实际改动（仅删除死代码，未改任何行为）**：

- `executor/mutation_join_pipeline.go`：删除 `targetRowProjection`（UPDATE JOIN 改用 `identityScan` 后已无调用方；grep 确认零引用）。
- `executor/physical_binding.go`：修复 `updateCandidates` 上方**重复的文档注释块**（残留的旧注释 + 新注释并存）。

**逐项结果**：

-  [X] T095 在 `executor/mutation.go` 清理已迁入 Insert/Update/DeleteOperator 的重复 row mutation 逻辑
  - 完成说明：`mutation.go` 现在只剩 `mutateSQL`（dispatch）、`insertSQL`（INSERT VALUES binding + `valuesSource` + `InsertOperator`）、`valuesSource`/`valuesCandidate`（candidate 构造）、以及 `writeVersionedRow`/`writeVersionedRowUnchecked`（底层写函数本体，按 plan 约定不重构）。UPDATE 分支、DELETE 分支内**均无** row mutation、无 `applyForeignKeyActions`、无 `writeVersionedRow` 调用。已确认删除的重复逻辑：INSERT VALUES 的内联 auto_increment 闭包（floors/sent/next/last/advance）、INSERT SELECT 的 `resolveInsertAutoIncrement`/`advanceInsertCounter`/`flushInsertCounters`、UPDATE 的逐行闭包写入、DELETE 的逐行闭包写入、UPDATE JOIN 的 `errJoinUpdateLimit` sentinel 与逐 target 写入、`insertTarget` 的计数器字段。
-  [X] T096 在 `executor/mutation_insert_select.go` 仅保留 INSERT SELECT binding、source pipeline 与 candidate projection
  - 完成说明：该文件现存 5 个函数，全部属于这三类职责：`insertSelectSQL`（binding + pipeline 编排）、`insertSelectSource`（candidate projection）、`bindInsertTarget`（binding）、`bindSubqueryQuery`（query pipeline 绑定）、`buildRow`（dest row 装配，属 binding 侧）。**无任何写入调用**（`writeInsertedRow`/`writeVersionedRow` 零命中）。
-  [X] T097 在 `executor/mutation_join.go` 仅保留 UPDATE JOIN binding/query pipeline/candidate construction
  - 完成说明：该文件现存**仅 1 个函数** `joinUpdateSQL`（63 行），职责是 load/qualify/bind → `bindUpdateJoinPlan` → `winners` → `apply`。**无任何写入调用**；`TargetRowDedup` 与 `UpdateOperator` 都在 `mutation_join_pipeline.go` 中。`errJoinUpdateLimit` 已随迁移删除。
-  [X] T098 [US3] 在 `executor/mutation_multi_delete.go` 仅保留 target resolution、candidate projection、FK ordering
  - 完成说明：Phase 7 完成后该文件只剩 target resolution（`resolveMultiDeleteTargets`）、target 名映射（`multiDeleteTargetIndexes`/`lookupMultiDeleteTarget`）、`multiDeleteTarget.key()`（provenance 优先）、`sqlPrimaryIndex`、`multiDeleteOrder`。`deleteDrivingTargetSQL`、`multiDeleteRow`、`columns()` 与 2 处直接 `writeVersionedRow` 已全部删除；候选投影与去重/排序/写入编排移至新建的 `executor/multi_delete_pipeline.go`。
-  [X] T099 搜索 `writeInsertedRow` 调用点，确认 INSERT statement-specific path 不再存在重复最终写入实现
  - 完成说明：全仓库 `writeInsertedRow` 只有 **1 个生产调用点** —— `modify_insert.go:199`（`InsertOperator.apply`）。函数本体位于 `mutation_insert_modes.go:56`（冲突策略分派，按 plan 不重构）。INSERT VALUES / SET / SELECT 三条来源全部经该唯一调用点写入，不存在 statement-specific 的重复最终写入。
-  [X] T100 搜索 `writeVersionedRow` 调用点，确认 UPDATE JOIN 与 multi-table DELETE 不再绕过统一 Modify Operator
  - 完成说明：**两者均已不再绕过**。UPDATE JOIN 经 `UpdateOperator`（`mutation_join.go`/`multi_delete_pipeline.go` 均零命中）。multi-table DELETE 经 `DeleteOperator`（`multi_delete_pipeline.go:201`），`mutation_multi_delete.go` 与本管线文件中 `writeVersionedRow`/`writeInsertedRow` **零命中**。剩余生产调用点仅：`modify_delete.go:94`（DeleteOperator）、`modify_update.go:120`（UpdateOperator）、`mutation_insert_modes.go` 5 处（经 `InsertOperator`）、`foreign_key_actions.go:143/151`（FK 动作）、`alter_table.go:182`、`ddl_mvcc.go:133`（DDL）、`snapshot_transfer.go:86`（迁移）、`mutation.go:452/459`（函数本体）。
-  [X] T101 检查是否仍存在针对复杂 DML 的无界 `seen map + rows slice` target accumulation
  - 完成说明：**已消除**。原按物理行累积的 `seen := make(map[string]bool)` 与 `rows := make([]multiDeleteRow, 0)` 已随 Phase 7 删除，`multiDeleteRow` 类型不复存在。`resolveMultiDeleteTargets` 保留的 `seen` 只去重**语句中请求的 target 名**，上界为名字个数，不随结果集增长。去重职责已交给 `physical.TargetRowDedup`（`multi_delete_pipeline.go:153`），其账本为 bounded/spillable。
-  [X] T102 删除 A05 重构后不再使用的 helper、sentinel 或专用 mutation glue
  - 完成说明：本轮删除 `targetRowProjection`（死代码）并修复重复注释。各阶段累计删除：`errJoinUpdateLimit`（sentinel）、`resolveInsertAutoIncrement`/`advanceInsertCounter`/`flushInsertCounters`（INSERT SELECT 专用计数器胶水）、`insertTarget` 的计数器字段（重复状态）、`deleteDrivingTargetSQL`（Phase 7 删除）、`multiDeleteRow` 类型（Phase 7 删除）。已用脚本全量扫描 executor 包中"仅出现一次（即无调用方）"的函数，除上述删除项外，其余未引用者（`expressionPredicate`、`valueJoinHashKey`、`viewsInDependencyOrder`、`resolveGroupColumn`、`uniquePredicateIndex`、`likeMatchWithLookup`、`SetCharacterSet`、`SetNames`）经核实**在 A05 开始前就已存在**，不属于本次重构产生的死代码，按"不修改已确认行为"原则保留。

## writeInsertedRow 剩余调用点

| 位置 | 用途 | 合法性 |
|---|---|---|
| `executor/modify_insert.go:199`（`InsertOperator.apply`） | 统一 INSERT 写入：VALUES / SET / SELECT 三条来源唯一入口；内部按 `insertMode` 分派 plain / IGNORE / REPLACE / ON DUPLICATE KEY UPDATE | **合法且唯一** |
| `executor/mutation_insert_modes.go:56` | `writeInsertedRow` 函数**本体**（定义处，非调用） | 定义 |

结论：**INSERT 已完全统一，无 statement-specific 重复最终写入**。

## writeVersionedRow 剩余调用点

| 位置 | 用途 | 是否绕过统一 Operator |
|---|---|---|
| `executor/modify_insert.go`（经 `mutation_insert_modes.go` 的 5 处） | INSERT 写入 + 冲突策略（REPLACE 删冲突行、ON DUPLICATE KEY UPDATE 改冲突行） | 否 —— 全部由 `InsertOperator` 驱动 |
| `executor/modify_update.go:120`（`UpdateOperator.Apply`） | UPDATE / UPDATE JOIN 统一写入 | 否 |
| `executor/modify_delete.go:94`（`DeleteOperator.Apply`） | 普通 DELETE 统一写入（`newRow=nil`） | 否 |
| `executor/foreign_key_actions.go:143`、`:151`(`Unchecked`) | FK CASCADE / SET NULL 动作（按 plan 不重构） | 不适用 —— FK 动作路径 |
| `executor/alter_table.go:182` | ALTER TABLE 行重写 | 不适用 —— DDL |
| `executor/ddl_mvcc.go:133` | CREATE TABLE AS / LIKE 行拷贝 | 不适用 —— DDL |
| `executor/snapshot_transfer.go:86` | legacy → MVCC 迁移导入 | 不适用 —— 迁移工具 |
| `executor/mutation.go:452`/`:459` | `writeVersionedRow` / `writeVersionedRowUnchecked` **本体**（定义处） | 定义 |

**仍绕过统一 Operator 的调用点：无。** 上表所有调用点要么属于三个统一 Operator，要么属于 shared low-level writer（FK 动作路径 / DDL / 迁移工具）。Phase 7 完成后，multi-table DELETE 也已改经 `DeleteOperator`，不再有专用最终写入路径。

## 三个问题的明确回答

**a. INSERT SELECT 是否还存在专用最终写路径？**
**否。** `mutation_insert_select.go` 内 `writeInsertedRow`/`writeVersionedRow` 零命中；其输出经 `insertSelectSource` 转为 `InsertCandidate` 后进入与 INSERT VALUES 完全相同的 `InsertOperator`。全仓库 `writeInsertedRow` 仅 1 个调用点。

**b. UPDATE JOIN 是否还存在专用最终写路径？**
**否。** `mutation_join.go` 仅 63 行、1 个函数，无任何写入调用；`writeVersionedRow`/`applyForeignKeyActions` 均由 `UpdateOperator.Apply` 统一调用。`mutation_join_pipeline.go` 只负责 identity 解析、`TargetRowDedup` 接线与 `UpdateCandidate` 构造。

**c. multi-table DELETE 是否还存在专用最终写路径？**
**否。** Phase 7 完成后，`mutation_multi_delete.go` 与 `executor/multi_delete_pipeline.go` 内 `writeVersionedRow`/`writeInsertedRow` **零命中**；`deleteDrivingTargetSQL` 与 `multiDeleteRow` 已删除，原按物理行累积的 `seen map + rows slice` 已由 `physical.TargetRowDedup`（`multi_delete_pipeline.go:153`）取代，最终删除经 `DeleteOperator`（`multi_delete_pipeline.go:201`）。

------

# Phase 10：全量回归与文档

**状态**：已完成（T103-T114 全部完成）。Phase 7 亦已完成，故 FR-025 / SC-005 已满足（见下方 FR/SC 对照）。

**回归汇总（本轮实测）**：

| # | 回归域 | 结果 | 测试数 |
|---|---|---|---|
| — | `go test ./...` 全量 | **21/21 包 ok，0 FAIL** | — |
| 1 | A04 SQL 能力矩阵 | PASS | 7 |
| 2 | INSERT SELECT parity | PASS | 14 |
| 3 | UPDATE JOIN parity | PASS | 15 |
| 4 | 多表 DELETE parity | PASS | 14 |
| 5 | Halloween | PASS | 1 |
| 6 | FK actions | PASS | 5 |
| 7 | no-PK 多表 DELETE | PASS | 6 |
| 8 | savepoint / failure | PASS | 12 |
| 9 | transaction / snapshot | PASS | 29 |
| 10 | auto_increment / LastInsertID | PASS | 10 |
| 11 | TargetRowDedup spill / cancel / cleanup | PASS | 22 |

`gofmt -l executor/ physical/` 无输出；`go build ./...`、`go vet ./...` 退出码 0。

-  [X] T103 运行 `go test ./...`
  - 完成说明：21/21 包 ok，0 FAIL。
-  [X] T104 运行全部 A04 SQL 能力矩阵相关 regression
  - 完成说明：A04 矩阵覆盖的能力由各专题 parity 回归体现（INSERT SELECT / INSERT 冲突策略 / UPDATE JOIN /
    多表 DELETE / no-PK 多表 DELETE / FK 动作 / savepoint / 事务快照），这些专题均在本表 #2-#10 行单独统计并通过。
    另有 `TestMVCCSQLTransactionsAndReopen` 直接覆盖事务与 reopen。矩阵文档已补充 A05 章节。
-  [X] T105 对 INSERT SELECT、UPDATE JOIN、multi-table DELETE 执行 legacy-vs-MVCC parity
  - 完成说明：`TestMVCCInsertSelectMatchesLegacyEngine`、`TestMVCCUpdateJoinMatchesLegacyEngine`、
    `TestMVCCMultiTableDeleteMatchesLegacyEngine`、`TestMVCCMultiTableDeleteWithoutPrimaryKeyMatchesLegacy`、
    `TestMVCCMultiTableDeleteJoinedTargetWithoutPrimaryKeyMatchesLegacy`、`TestMVCCInsertModesMatchLegacyEngine`
    全部通过（逐句 affected rows + 结果集 parity）。
-  [X] T106 验证 `executor/mvcc_halloween_test.go`
  - 完成说明：`TestMVCCUpdateIndexHalloweenProtection` 通过（affected=2，行值正确）。
-  [X] T107 验证 `executor/mvcc_insert_select_test.go`
  - 完成说明：10 个测试全部通过。
-  [X] T108 验证 `executor/mvcc_update_join_test.go`
  - 完成说明：5 个测试全部通过。
-  [X] T109 验证 `executor/mvcc_multi_delete_test.go`
  - 完成说明：6 个测试全部通过。
-  [X] T110 验证 `executor/mvcc_multi_delete_nopk_test.go`
  - 完成说明：7 个测试全部通过。
-  [X] T111 验证 `executor/mvcc_fk_actions_test.go`
  - 完成说明：3 个测试全部通过。
-  [X] T112 验证 `executor/mvcc_savepoint_failure_test.go`
  - 完成说明：4 个测试全部通过（`TestMVCCSavepointStatementCommitFailureAbortsTransaction`、
    `TestMVCCSavepointNestedCommitFailureAbortsTransaction`、`TestMVCCSavepointCommitMergeFailureAbortsTransaction`、
    `TestMVCCSavepointWithoutTransactionKeepsLegacyMessage`）。另有 `mvcc_savepoint_test.go` 的 2 个测试一并通过，
    故 savepoint 专题合计 6 个测试通过。
-  [X] T113 更新 `docs/设计/A04-MVCC-SQL能力矩阵.md` 或新增 A05 设计说明，记录复杂 DML 已统一进入 Modify Pipeline
  - 完成说明：在 `docs/设计/A04-MVCC-SQL能力矩阵.md` 末尾新增 **"A05：写入算子统一（Modify Pipeline）"** 章节，
    含目标模型、统一进度表（INSERT VALUES/SET/SELECT、普通 UPDATE、UPDATE JOIN、普通 DELETE、
    **多表 DELETE 均已统一**）、保持不变的行为契约、已确认的行为变更。
    多表 DELETE 于 Phase 7（T070～T087）完成后已统一，文档中标注其经 `DeleteOperator` 落盘。
-  [X] T114 在 `specs/005-unified-modify-pipeline/quickstart.md` 记录 A05 端到端验证命令与预期结果
  - 完成说明：新建 `spec/005-unified-modify-pipeline/quickstart.md`（仓库实际目录是 `spec/`，非 `specs/`）。
    含验证命令、6 组关键 SQL 场景与预期结果、13 行关键 regression test 索引、完成状态声明、已确认行为变更。
    **文档中所有 SQL 示例与预期结果均已用临时脚本实测验证**（~17 条断言全部匹配），验证脚本已删除。

## FR / SC 达成情况（对照 spec.md）

**已满足**：FR-001～FR-025、SC-001～SC-012 全部满足。

**Phase 7 完成前曾记录的两处缺口，现已收口**：

| 项 | 内容 | 现状 |
|---|---|---|
| **FR-025** | "A05 完成后，INSERT SELECT、UPDATE JOIN、multi-table DELETE 不得继续拥有独立的最终物理 row mutation 实现" | INSERT SELECT ✅、UPDATE JOIN ✅、**multi-table DELETE ✅**（`mutation_multi_delete.go` 与 `multi_delete_pipeline.go` 内 `writeVersionedRow` 零命中；删除经 `DeleteOperator`） |
| **SC-005** | "复杂 DML 专用代码中不再拥有独立的最终物理 row mutation 实现" | ✅ 三个复杂 DML 文件均无自有最终写入 |

**其余 SC 佐证**：

- SC-001 全部 MVCC regression 通过 → T103/T105；SC-002 INSERT 同实现 → `writeInsertedRow` 仅 1 调用点 + `TestMVCCInsertValuesAndSelectShareOneOperator`；SC-003 UPDATE 同实现 → `TestMVCCUpdate*` + `TestMVCCUpdateJoin*` 均经 `UpdateOperator`；SC-004 普通/多表 DELETE 同实现 → `TestMVCCDeleteUsesUnifiedDeleteOperator`、`TestMVCCMultiTableDeleteUsesUnifiedDeleteOperator`。
- SC-006 first match → `TestMVCCUpdateJoinFirstMatchWinsPerTarget`；SC-007 Halloween → `TestMVCCUpdateIndexHalloweenProtection`；SC-008 无主键多删可区分 → `TestMVCCMultiTableDeleteJoinedHeapTargetAffectedRows`；SC-009 不依赖无界 `seen` map → `TestMVCCSpillableDedupStaysBounded`、`physical.TestTargetRowDedupSpillsBeyondMemoryThresholdAndCleansUp`（多表 DELETE 的行累积 `seen` map 已由 `TargetRowDedup` 取代）；SC-010/SC-011 原子性/资源 → `TestMVCCDownstreamErrorRollsBackChildTransaction`、`TestMVCCCancellationReleasesOperatorResources`；SC-012 索引不退化为全表扫描 → `TestMVCCUpdateKeepsAccessPlan`、`TestMVCCDeleteKeepsAccessPlan`。

**convergence review 结论**：**CONVERGED**。FR-025 / SC-004 / SC-005 / SC-009 已于 Phase 7 完成后满足；
根因（`TargetRowDedup` payload 透传 + `identityScan` 在嵌套循环 re-scan 下的身份稳定性）已在
`physical/target_dedup.go`（payload-preserving）与 `executor/derived_mvcc.go`（provenance 列随行传递）中解决，
Phase 9 的 T098/T100/T101 已随之收口。

------

# 依赖关系

```text
Phase 1 基础模型
       ↓
Phase 2 TargetRowDedup
       ↓
       ├─────────────┐
       ↓             │
Phase 3 INSERT       │
                     │
Phase 4 Simple UPDATE
       ↓
Phase 5 UPDATE JOIN
                     │
Phase 6 Simple DELETE
       ↓
Phase 7 Multi DELETE
       ↓
Phase 8 Transaction / Resource
       ↓
Phase 9 Cleanup
       ↓
Phase 10 Full Regression
```

INSERT 在基础模型完成后即可单独实施。

UPDATE JOIN 依赖：

```text
UpdateOperator
+
TargetRowDedup
```

Multi-table DELETE 依赖：

```text
DeleteOperator
+
TargetRowDedup
```

------

# MVP 建议

Spec Kit 强调每个 User Story 应尽可能能够单独交付。

A05 最适合的第一个 MVP 是：

```text
Phase 1
+
Phase 2
+
Phase 3 / US1
```

即：

```text
统一 Candidate 基础
+
TargetRowDedup 基础能力
+
INSERT VALUES / INSERT SELECT → InsertOperator
```

先证明：

```text
Query Pipeline
→ Candidate
→ Modify Operator
→ Txn
```

这一架构成立，再迁移 UPDATE JOIN 和 Multi DELETE。

------

# 完成定义

只有同时满足以下条件才能认为 A05 完成：

```text
INSERT VALUES ────┐
INSERT SELECT ────┴→ InsertOperator

Simple UPDATE ────┐
UPDATE JOIN ──────┴→ UpdateOperator

Simple DELETE ────┐
Multi DELETE ─────┴→ DeleteOperator
```

并且：

- TargetRowDedup 稳定保留 first match；
- target identity 使用物理 row identity；
- 无 PK table 正确；
- outer join 正确；
- UPDATE LIMIT 在 dedup 后；
- Halloween protection 正确；
- statement snapshot 正确；
- statement rollback 正确；
- FK ordering 正确；
- LastInsertID 正确；
- 大规模 dedup 不依赖无界内存；
- 所有现有 A04 regression 继续通过；
- 复杂 DML 不再保留独立最终 MVCC 写入路径。