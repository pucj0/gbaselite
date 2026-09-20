# Feature Specification: B01 完善 MVCC Transaction Manager

**Feature Branch**: `001-b01-mvcc-transaction-manager`  
**Created**: 2026-09-20  
**Status**: Draft  
**Input**: 完善 GBaseLite MVCC Transaction Manager：补齐 startTS/readTS/commitTS、活动事务表、读写集、提交/回滚状态和统一可见性判断，并补并发事务测试。

## 1. 背景

GBaseLite 当前已经具备可工作的 MVCC 基础：事务拥有唯一 ID 和固定 Snapshot；写入通过内存缓冲与临时 bbolt stage 管理；提交阶段执行乐观写冲突检查；已存在 point/range guard、父子事务、savepoint、group commit、streaming commit、local WAL、Raft 提交路径以及历史版本 GC。

B01 不重新实现 MVCC 存储，也不改变现有 SQL 能力。B01 的目标是把当前分散在 `Tx`、`Store`、commit path、active snapshot 计数和 visibility helpers 中的隐式事务语义，收束为明确、可诊断、可验证的 Transaction Manager 生命周期模型。

## 2. 目标

建立成熟、统一且可诊断的事务生命周期与版本可见性模型，使并发事务下的以下行为可重复验证：

- 固定快照读取与可重复读；
- read-your-writes；
- 未提交数据隔离；
- 写写冲突；
- point/range dependency 冲突；
- 提交、回滚、savepoint/child merge；
- active transaction 生命周期；
- 历史版本保留 horizon；
- commit/cancel 竞争；
- transaction state diagnostics。

## 3. 非目标

本功能明确不包含：

- 将 Snapshot Isolation 升级为 Serializable；
- 为普通 SELECT 建立无界全量 read-set 并在提交时逐项校验；
- 引入行锁、gap lock、next-key lock 或死锁检测；
- 改变现有 `Guard` / `GuardRange` 的乐观 dependency 语义；
- 为每次 BEGIN 推进全局 commit/version sequence；
- 使用 wall-clock time 参与 MVCC 可见性判断；
- 重写现有 bbolt 版本布局、local WAL 或 Raft 协议；
- 让单个 `Txn` 支持多个 goroutine 并发调用；
- 改变 A05 已建立的 parent-read / child-write DML 执行模型。

## 4. 用户故事与优先级

### US1 - 明确事务生命周期与时间戳语义（P1）

作为数据库内核开发者，我希望每个事务都有明确的 TxnID、startTS、readTS、commitTS 和状态，这样事务生命周期不再依赖隐含字段和约定。

**独立验收**：

1. BEGIN 时创建唯一 TxnID。
2. root transaction 的 `startTS == readTS == Begin 时观察到的 committed Head`。
3. 事务执行期间 `readTS` 固定。
4. 成功写事务提交后生成 `commitTS`。
5. 只读事务提交不推进 Head，`commitTS` 保持 unset。此处的 "只读" 指 §7 "事务结束" 的 A 类（既无 Put/Delete 也无 Guard/GuardRange）；只有 Guard/GuardRange 的 C 类仍会推进 Head 并设置 `commitTS`（见 FR-004 与 §7）。
6. child transaction commit 只进入 `MERGED`，不产生独立 durable commitTS。
7. rollback 后状态为 `ABORTED`。
8. double commit 返回明确 closed error；double rollback 幂等。
9. 仅含 Guard/GuardRange 的 root transaction 成功提交后：状态 `COMMITTED`、`HasCommitTS=true`、Head 前进，但没有数据 version 被安装，普通读取者不会因此看到任何新数据。

### US2 - 活动事务注册表与 GC Horizon（P1）

作为存储层维护者，我希望 Transaction Manager 能按 TxnID 观察活动事务，同时继续准确计算 oldest readTS，使 GC 不会删除仍被长事务需要的版本。

**独立验收**：

1. root transaction BEGIN 后出现在 active transaction registry。
2. commit/rollback/disconnect 后从 active registry 移除。
3. savepoint/child 可被诊断，但不重复增加 root snapshot retention 引用。
4. GC horizon 始终不高于任一活动 root transaction 的 readTS。
5. 活动事务全部结束后 horizon 可继续推进。
6. registry 不出现负引用、重复清理或泄漏。

### US3 - 统一可见性语义（P1）

作为 SQL 执行器，我希望 point read、scan、flat layout、child transaction 都遵循同一套 MVCC 可见性规则，从而避免不同读取路径出现语义偏差。

**独立验收**：

统一语义必须满足：

1. 当前事务自己的未提交写优先可见。
2. child 看见自己的 writes。
3. child 看见 parent 已存在的 writes。
4. parent 看不见尚未 merge 的 child writes。
5. 其他事务看不见未提交 writes。
6. committed version 仅当其 commit version `<= readTS` 时可见。
7. tombstone 在对应 snapshot 后隐藏更旧版本。
8. 未发布 publication marker 的 crash/interrupted version 永不可见。
9. 同一事务两次读取不因其他事务中途提交而变化。

### US4 - 统一提交冲突验证（P1）

作为事务管理器，我希望 standalone、group commit、streaming commit、local WAL 与 Raft 提交路径使用一致的冲突语义，使相同事务无论走哪条提交路径结果一致。

**独立验收**：

1. 对 write/dependency key，若 `latestCommittedVersion > readTS`，提交失败并返回 `ErrConflict`。
2. 同 snapshot 修改不同 key 可同时成功。
3. 同 snapshot 修改同 key，后提交者冲突。
4. delete/update、insert/delete 等同 key 组合 obey 同一规则。
5. `Guard` 检测 point dependency 变化。
6. `GuardRange` 检测范围内 insert/update/delete phantom 变化。
7. 普通 read observation 默认不参与 commit conflict。
8. write skew 在未声明 Guard/GuardRange 时保持 SI 语义，不被自动禁止。

### US5 - Read/Write/Dependency 可诊断模型（P2）

作为调试人员，我希望能够看到事务的读写活动规模和 dependency 情况，而不会因为大 SELECT 建立无界内存 read-set。

**独立验收**：

每个活动事务至少可诊断：

- TxnID；
- ParentID（如有）；
- State；
- startTS；
- readTS；
- commitTS（如有）；
- age / StartedAt；
- point read count；
- range read count；
- write count；
- staged/write bytes；
- point dependency count；
- range dependency count；
- abort reason（如有）。

普通读只记录 bounded counters/summary，不要求保存全部 read keys。

### US6 - 并发、取消与恢复边界（P2）

作为数据库使用者，我希望事务在 cancel、restore、断连和并发提交边界下仍保持一致的最终状态。

**独立验收**：

1. publication 之前 cancel：事务不得对其他事务可见，最终 ABORTED。
2. durable publication 已完成后再发生 context cancel：不得向调用者伪装成 rollback。
3. restore/generation change 后旧事务失效。
4. session disconnect 释放事务并从 registry 移除。
5. failed commit 不产生 commitTS。
6. crash 未发布的版本不会被新事务看到。
7. timestamp/version sequence 不允许 uint64 wraparound。

## 5. 功能需求

### FR-001 Transaction Metadata

系统必须为每个事务维护：

- `TxnID`
- `ParentID`（child 时）
- `StartTS`
- `ReadTS`
- `CommitTS`（optional）
- `State`
- `Generation`
- `StartedAt`
- `AbortReason`（optional）

### FR-002 TxnID

- TxnID 必须唯一。
- 当前随机 128-bit ID 方案可以保留。
- 注册表检测到碰撞时不得覆盖现有事务。
- Command ID 的既有限制必须保持兼容。

### FR-003 startTS/readTS

- root transaction Begin 时读取 committed Head。
- `StartTS = ReadTS = Head at Begin`。
- 当前 B01 只实现 Snapshot Isolation，因此事务生命周期内 ReadTS 不更新。
- Begin 不得单独推进 commit sequence。

### FR-004 commitTS

- 仅在 root 写事务 durable publication 成功后设置。
- 值等于真正发布该事务版本的逻辑 commit/version sequence。
- child merge 不设置 durable commitTS。
- read-only commit 不分配新 version，commitTS 为 unset。

"root 写事务" 的判定是 **是否持有可发布的事务工作**，即是否持有 staged 写集
（`Put`/`Delete` 数据写，或 `Guard`/`GuardRange` 依赖），而 **不是** 是否安装了数据
version。因此：

- Put/Delete（数据写）成功提交：设置 commitTS，并安装数据 version。
- 仅 Guard/GuardRange（dependency-only，见 §7 "事务结束" C 类）成功提交：同样设置
  commitTS 并推进 Head，但 **不安装任何数据 version**。
- 既无 Put/Delete 也无 Guard/GuardRange（empty / ordinary read-only，A 类）：commitTS 为
  unset，不推进 Head。

因此 `HasCommitTS=true` 只表示 **"该 root transaction 获得了 durable commit revision"**，
它 **不保证** 该事务安装了数据 version。判定"是否有数据变更"必须看数据写集（例如
`TransactionInfo.Writes`/`WriteBytes`），不能看 `HasCommitTS`。

### FR-005 Transaction State Machine

至少支持：

- `ACTIVE`
- `COMMITTING`
- `COMMITTED`
- `ABORTED`
- `MERGED`（child only）

有效转换（root）：

- ACTIVE -> COMMITTING（有可发布工作需要 durable publication）
- ACTIVE -> COMMITTED（无 Put/Delete/Guard/GuardRange 的 empty / ordinary read-only root）
- ACTIVE -> ABORTED
- COMMITTING -> COMMITTED
- COMMITTING -> ABORTED

有效转换（child）：

- ACTIVE -> MERGED
- ACTIVE -> ABORTED

`ACTIVE -> COMMITTED` 只用于 A 类（empty / ordinary read-only root）；dependency-only
root（C 类）走 `ACTIVE -> COMMITTING -> COMMITTED`，因为它有可发布的依赖集。终态不得
重新进入 ACTIVE，终态只允许自我确认（重复 rollback / 重复终态上报为幂等 no-op）。

### FR-006 Transaction Manager

引入明确的 Transaction Manager，负责：

- root transaction 注册与注销；
- child transaction diagnostic registration；
- snapshot retention ref；
- state transition；
- transaction metadata snapshot；
- active transaction enumeration；
- oldest readTS/horizon 计算；
- transaction counters/statistics。

### FR-007 Retention Ownership

- 只有 root transaction 持有 MVCC history retention。
- child/savepoint 继承 root ReadTS，不重复增加 retention 引用。
- child diagnostics 与 retention ownership 必须解耦。

### FR-008 Write Set

保留当前 bounded memory + disk stage 的 write-set 机制。

- 写集大小必须继续受到现有资源限制约束。
- 配置 limit=0 的现有“unlimited total write set but bounded memory buffer”语义保持不变。
- 精确等于 configured limit 时允许写入；超过 1 byte 时失败。
- accounting overflow 必须 fail closed。

### FR-009 Read Observation

普通读取不建立无界 key-level read-set。

系统仅需以 bounded counters/summary 记录普通读取活动，例如：

- point reads；
- range scans；
- rows/keys observed（可选）；
- bytes observed（可选）。

### FR-010 Dependency Set

真正参与 commit-time validation 的读取依赖继续由：

- `Guard`
- `GuardRange`

表示，并形成逻辑 Dependency Set。

Dependency Set 是"可发布事务工作"的一部分（FR-004、§7 C 类）：持有 Guard/GuardRange 的事务
在提交时需要 durable publication 与 validation，因此即使没有 Put/Delete 也会获得 commit
revision 并推进 Head，但不安装数据 version。ordinary Get/Scan 只更新 bounded counters
（FR-009），既不进入 Dependency Set，也不使事务成为可发布写事务。

### FR-011 Visibility Rule

所有读取路径必须遵循同一语义定义，但可以保留不同性能优化实现：

- `visible`
- `visibilityReader`
- `flatVisible`
- range/scan fast path

不得为了“统一函数”而强制合并导致性能退化。

### FR-012 Conflict Validation

提供一个逻辑统一的 commit validation 入口/层，使所有提交路径遵循：

`latestCommittedVersion(dependencyOrWriteKey) > readTS => ErrConflict`

range dependency 使用等价的 latest change 规则。

### FR-013 Commit Path Consistency

以下路径必须共享同一冲突语义：

- local bounded commit；
- group commit；
- local streaming commit；
- local WAL commit；
- replicated/Raft commit。

### FR-014 Diagnostics Capability

通过 storageengine optional capability 暴露 transaction diagnostics，不强制污染最小 `Txn` 接口。

建议：

- `ActiveTransactions()`
- `TransactionStats()`

SQL 层 `SHOW MVCC TRANSACTIONS` 可作为后续或本功能增强项，但底层 capability 必须完成。

### FR-015 Closed Transaction Behavior

- Commit 成功后事务关闭。
- Commit 失败并终止事务时状态可诊断。
- Rollback 关闭事务。
- double rollback 幂等。
- closed transaction 的 Get/Put/Delete/Child/Commit 返回 `ErrClosed` 或既有兼容错误。

### FR-016 Generation Invalidation

Store restore / generation change 后：

- 旧事务不可继续读取或提交；
- registry 必须最终清理；
- 不得影响新 generation 的 snapshot retention。

### FR-017 Sequence Exhaustion

版本 sequence 接近 `math.MaxUint64` 时必须显式拒绝新 commit sequence 分配，禁止 wraparound 到 0。

## 6. 核心不变量

### INV-001 Snapshot Stability

对 root transaction T：

`T.ReadTS` 自 Begin 到终止保持不变。

### INV-002 Commit Ordering

所有 durable committed write version 拥有严格单调、不可回绕的 commit sequence。

说明：INV-002 约束的是**数据 version** 的 sequence 单调性。dependency-only root（§7 C 类）
的 publication 会消费一个 commit revision 并推进 Head，但不安装数据 version，因此
commit sequence 序列中允许存在"没有对应数据 version 的 revision"。这不违反 INV-002，也不
影响可见性：可见性只按 `version <= ReadTS` 且存在 publication marker 判定（INV-003/INV-008），
不存在 version 的 revision 不会产生任何可见行。

### INV-003 Visibility

某事务只能看到：

- 自己/ancestor transaction 的逻辑 writes；
- 或 commit version `<= ReadTS` 的 committed version。

### INV-004 No Dirty Reads

一个 root transaction 的未提交 writes 不得被另一个 root transaction看到。

### INV-005 Write Conflict

同一逻辑 key 在 ReadTS 后被其他事务成功提交，则包含该 key write/dependency 的事务不得成功提交。

### INV-006 Child Isolation

child write 在 merge 前只对 child 可见；merge 后对 parent 可见；parent rollback 时最终全部消失。

### INV-007 Retention

只要存在 ReadTS=R 的活动 root transaction，GC 不得删除该 transaction 可能需要的 anchor version。

### INV-008 Publication Atomicity

未存在有效 publication marker 的版本不得被 visibility reader 视为 committed。

## 7. 边界条件

### 时间戳

- Head=0 时可以 Begin，ReadTS=0 合法。
- 0 不可用作 ReadTS 的 unset sentinel。
- CommitTS 必须使用 optional/has-value 语义。
- MaxUint64 前必须阻止下一次 sequence allocation。

### 事务结束

root transaction 按"是否持有可发布工作 / 是否安装数据 version"分为三类，三类语义都已正式
决策并作为 contract 固定：

| 类别 | 持有 | 成功结束的转换 | HasCommitTS | Head | 数据 version |
|---|---|---|---|---|---|
| A. empty / ordinary read-only root | 无 Put/Delete/Guard/GuardRange | ACTIVE -> COMMITTED | false | 不变 | 无 |
| B. data-writing root | Put/Delete（可同时含 Guard/GuardRange） | ACTIVE -> COMMITTING -> COMMITTED | true | 前进 | 发布 |
| C. dependency-only root | 只有 Guard/GuardRange，无 Put/Delete | ACTIVE -> COMMITTING -> COMMITTED | true | 前进 | **不安装** |

C 类存在的原因：`Guard`/`GuardRange` 是 **显式 commit-time validation dependency**，不是
ordinary read observation（FR-009/FR-010）。因此在当前 GBaseLite 事务模型中：

```text
has publishable transaction work  !=  has data mutation
```

dependency-only transaction 仍然通过 durable transaction publication 获得 commit revision，
只是这次 publication 携带的是依赖集而不是数据版本；commit-time validation 对依赖集的校验
照常执行（`latestCommittedChange > ReadTS => ErrConflict`）。又因为该 publication 不安装
version，它本身不会使另一个事务的 guard 失败（guard 校验的是已提交数据版本，而不是
revision 计数）。

`HasCommitTS=true` 不保证该事务一定安装了数据 version；它只表示"该 root transaction 有
durable commit revision"。

其余结束语义：

- 空事务 Commit：成功，不推进 Head（A 类）。
- ordinary read-only transaction Commit：成功，不推进 Head（A 类）。
- failed commit：不得设置 CommitTS。
- rollback before writes：正常 ABORTED。
- double rollback：成功/幂等。
- double commit：ErrClosed。

### 写冲突

- same snapshot / same key；
- same snapshot / different key；
- insert vs insert；
- insert vs delete；
- update vs delete；
- tombstone vs reinsert；
- range guard + insert；
- range guard + delete；
- range guard + boundary-inclusive/exclusive keys。

### Write Set Resource

- 0-byte/empty logical write set；
- exact limit；
- limit + 1；
- single op larger than memory buffer but within value/transaction limit；
- spill；
- accounting near MaxInt64。

### Parent/Child

- child commit then parent commit；
- child commit then parent rollback；
- child rollback；
- multiple nested children；
- savepoint replacement；
- rollback to savepoint；
- max savepoint count。

### Cancel/Failure

- cancel before stage；
- cancel during stage；
- cancel after versions installed but before publication；
- cancel racing publication；
- cancellation after durable publication；
- storage fatal error；
- generation invalidation；
- disconnect rollback。

## 8. 成功标准

### SC-001

全部新增事务状态机测试、visibility 测试、conflict 测试和 concurrency regression 在 `go test ./...` 中稳定通过。

### SC-002

并发测试必须使用 channel/barrier/hook 等确定性同步，不依赖 `time.Sleep()` 判断时序。

### SC-003

现有 A05 parent-read / child-write、savepoint、large transaction、group commit、local stream、local WAL、range guard、maintenance/GC 回归全部继续通过。

### SC-004

普通大 SELECT 不因 B01 建立与返回行数线性增长的 key-level read-set。

### SC-005

Transaction diagnostics 能准确列出 active root/child transaction，并在 commit/rollback/disconnect 后无泄漏。

### SC-006

对相同 workload，standalone bounded/group/stream/WAL/Raft 路径的 conflict 结果一致。

## 9. Clarifications 已决策

1. **隔离级别**：保持 Snapshot Isolation。
2. **startTS/readTS**：当前版本中相等，均来自 Begin 时 committed Head。
3. **普通 read-set**：不做全量 key retention，仅记录统计；显式 dependency 由 Guard/GuardRange 表示。
4. **child commit**：状态为 MERGED，不产生独立 commitTS。
5. **read-only commit**：不分配 commit version。此处的 read-only 指 A 类（无 Put/Delete/Guard/GuardRange）。
6. **Transaction Manager registry**：root retention 与 child diagnostics 分离。
7. **时间来源**：MVCC ordering 只使用逻辑 revision；wall clock 仅用于 diagnostics。
8. **dependency-only commit（B01 决策）**：仅含 `Guard`/`GuardRange` 的 root transaction 仍是一个可发布写事务，走 `ACTIVE -> COMMITTING -> COMMITTED`，设置 `HasCommitTS=true` 并推进 Head，但不安装数据 version。理由：Guard/GuardRange 是显式 commit-time validation dependency，属于"可发布事务工作"，不是 ordinary read observation。因此 `has publishable transaction work != has data mutation`，且 `HasCommitTS=true` 只表示存在 durable commit revision。实现的现有行为即为最终行为，B01 不修改该语义。
9. **状态词表分层**：internal 状态（`mvcc.TransactionState`）与 public diagnostics 状态（`storageengine.TransactionState`）是两层概念：internal 含 `TransactionUnset`（未注册/无效，0 值），public 用 `UNKNOWN` 作为 fail-closed 分类，二者不是同一个枚举，映射见 contracts/transaction-diagnostics.md。
