# B02 Locked Current Read Contract

**Related**: FR-011..019/026/029/030/032；plan A3/A4/A5。

## Capability surface (planned)
storageengine新增可选LockingTxn，原Txn/Engine最小接口保持。
- `BeginLockScope(ctx, kind)`：返回属于root的scope；ctx只决定该语句的取消/期限。
- `Acquire(ctx, scope, request)`：申请opaque GrantToken；request用中立Space/Key/Range。
- `ReadCurrent(ctx, grant, space, key)`：验证token覆盖record及epoch，返回CurrentObservation。
- `NewCurrentIterator(ctx, scope, scanRequest, lockPlan)`：只接受已证明bounds或显式fallback，
  每visited entry加锁后再current reread；Iterator须Close、borrowed bytes规则不变。

### 第十二轮修正（P1/P2：共享范围读的表意向、缺席键 Gap 边界、Statement Scope 生命周期）

- **P1 表意向与 interval**：带 TableID 标签的 interval 需要**声明**表意向，而**共享（IS）即可**——
  interval 锁的是插入位置而不是行本身，`FOR SHARE` 范围读按合同声明 IS、`FOR UPDATE` 声明 IX，两者都是
  合法声明（IX 在 pinned-mode 记账里已满足 IS）。原先要求 IX 会让**每一个 `FOR SHARE` 范围读被拒绝**
  （"record lock requires a table intention in the same bundle"）；完全不带意向的 bundle 仍被拒绝。
  验证：`TestP1ForShareRangeReadIsAccepted`、`TestP1TwoForShareRangeReadsCoexist`（两个共享范围读互不排斥）、
  `TestP1ForShareRangeReadConflictsWithWriters`（区间内 INSERT/UPDATE 等待、区间外 UPDATE 完成）。
- **P2a 缺席键的 Gap 边界**：`LockingTxn.KeysAround(ctx, space, key)`（新增中立能力，adapter 用
  `ScanLatestRange` 做两次单键探测）返回该键的**前驱与后继**已发布键；点路径与唯一索引路径都用它们把
  Gap 限定为 `(前驱, 后继)`（无前驱/后继则相应一侧无界）。旧实现依赖一个永不成立的类型断言，退化成
  `[key, +∞)`，会把**所有** ≥ key 的插入都挡掉；现在表内 10/100 时对缺席 42 的保护是 `(10,100)`，
  插入 150 不被阻塞，而插入 42/50 仍等待 ✓（与 next-key gap lock 同形）。
  验证：`TestP2AbsentKeyGapDoesNotBlockDistantInserts`、`TestP2AbsentKeyGapBoundaries`（最小键之下、
  最大键之上、空表）。
- **P2b Statement Scope 生命周期**：scope 改为在 **Operator `Open` 内**创建（而不是在绑定期），
  `Open` 失败即关闭；`lockedSource` 在 EOF/Close/LIMIT/取消时关闭 scope；关闭 scope **只撤回本语句的
  pending 请求、绝不释放 root grants**（显式事务继续持锁至 COMMIT/ROLLBACK）。
  验证：`TestP2bLockingReadScopeLifecycle` 七种终态（绑定失败、Open 失败、范围取锁超时、迭代中取消、
  LIMIT 提前结束、正常 EOF、显式事务继续后续语句）+ `TestP2bRefusedPlanStillLeavesNothingBehind`。
- **同时修正**：范围扫描**逐行 claim** 的错误现在也经 `neutralLockError` 映射，超时等错误不再把
  lock-manager sentinel 泄漏到中立边界（此前 `errors.Is(err, storageengine.ErrLockWaitTimeout)` 为 false）。

### Locking scan 的实施补充（T032，本轮）
`CurrentScanRequest` 增加显式 **`Mode`（S/X）**：它描述迭代器为每个访问到的键取得的**记录**claim，
而 `Plan.Components` 描述的是**访问区间**（Gap/插入声明/whole-domain 回退）。空模式或区间模式一律拒绝
（不猜强度）。迭代器的实现纪律：
1. 先用 `Plan.Components` 取得区间（可能等待，`LockOpWait` 必须等句柄），此刻不持有任何 storage view；
2. 分页列键（≤64 行/1MiB）：底层 `ScanRange` 在每次 yield 前释放 bbolt 读事务，因此"等待期间不持有
   view/cursor/gate"是结构性保证；
3. 逐键按访问顺序取 record claim（可能等待），获锁后 `ReadCurrent` 读取**最新有效版本**并记录 baseline；
   观察为 absent 的键**跳过**，绝不返回列表时的旧值；
4. 进度稳定：下一页严格从上一个已消费键之后继续（正向 Lower 排他、反向 Upper 排他），删除/改写不重复输出；
5. Close/ctx cancel/epoch reset 均以定义好的终态结束且无锁资源残留。

### 键列表语义（T032 修正，本轮更新）
键列表**不再是快照枚举**：它走 `mvcc.Tx.ScanLatestRange`——以 store head 解析发布层并合并本事务/祖先
stage，且**不记录 range dependency**（当前读不是关于快照的证据，保护它的是它持有的 claim）。因此
**本事务快照之后别的事务提交的键会被枚举到**；普通 `ScanRange` 未改动，仍在固定 readTS 解析键。
SQL 侧的对应可见性由 `TestT034LockingScanDiscoversRowsCommittedAfterTheSnapshot` /
`TestT036...` 端到端锁定（同一事务的普通扫描看不到该行，锁定扫描看到）。

### 会话锁等待预算（T035 修正，本轮更新）
`innodb_lock_wait_timeout` 是**每次等待**的期限：`Scope.WaitBudget`（duration）由
`LockingTxn.BeginLockScopeBudget` 传入，`requestDeadlineLocked` 取
`min(该请求开始等待的时刻 + budget, scope 的 context deadline)`。语句/查询 context 是整条语句的上限，
因此一条语句内多次短等待共享的是 context 上限而不是会话预算。
**scope 归属**：scope id 每个 owner 从 1 开始，等待期限必须按"该请求所属 owner 的 scope 树"解析
（`requestDeadlineLocked(rootID, scopeID)`）；按 id 在全部 owner 中查找会把同号他人 scope 的预算当成
自己的（已修复，`TestB09BudgetIsReadFromTheRequestingOwnersScope`）。超时/取消/scope 关闭/root 回滚/
epoch reset 都必须终止等待且不残留请求（`TestB09...`、`TestT036CleanupPaths`）。

### SQL 锁定读路径（T034，本轮实现）
`executor/locking_read.go`：`bindPhysicalSelect` 通过 `readSourceOperator` 选行源——普通 SELECT 仍是快照
扫描，锁定读走：
- **点路径**（`sqlAccessPoint`）：先取 `[表意向, 记录 claim]`，再 `ReadCurrent`；缺失时补取"包含插入位置的
  Gap"（上界由 `ScanLatestRange` 找到的后继键给出，无后继则到 +∞）并重读；
- **唯一索引路径**（`sqlAccessUnique`）：先取该唯一值的 reservation（与写入路径同一资源），读出的 entry
  value 是**拥有该值的主键**，再取**共同主键行**并重读该行；行已被删除则返回空；
- **范围/whole-domain 路径**：`NewCurrentIterator` + 区间组件；主键范围计划里 planner 未显式给出 namespace
  （行命名空间只有读取方知道），`provenPrimaryInterval` 负责把它解析为已证明区间，避免把已证明区间降级为
  整表回退；
- 表意向模式：X ⇒ IX、S ⇒ IS。WHERE/ORDER BY/LIMIT/反向沿用原管线，因此 WHERE 在**当前行**上重评。
- **明确拒绝**（fail-closed）：JOIN/派生表/CTE/视图/聚合/DISTINCT/标量查询、以及经**二级索引**的锁定读
  （其 entry 集合可有多条，尚未接线）；NOWAIT/SKIP LOCKED 由 parser 拒绝。

- `CloseLockScope(scope)`：取消pending，不解root grants。
上述名称为接口契约候选，实施只能一致调整所有设计/任务，不能偷偷把语义塞入Get。
Current read receiver就是read layer；child可读自己的overlay与ancestor，parent不能读未merge child。
SQL DML验证target使用parent receiver；unique/FK写中检查用child receiver，避免读到别的sibling。

## Grant verification（本轮实现，B02-1 补强）

`ReadCurrent(grant, space, key)` 必须在**仍持有覆盖锁**时验证四件事，缺一即
`ErrConflict`（fail closed），且不得记录 observation baseline：

1. `grant.OwnerID` 等于本事务的 root owner；跨 root 的 grant 不授权。
2. 该 owner 仍在 lock registry 中（root 终态后 `ReleaseOwner` 已移除，旧 grant 立即失效）。
3. `grant.Generation`/`Term` 与该 owner 当前登记值**精确相等**；epoch 变化后的旧 grant 拒绝。
4. grant 自带的 RECORD 组件精确覆盖被读的 space+key（身份含 TableID/IndexID，以 grant 的组件为
   身份来源），**并且** manager 实时表里该 owner 在该资源上有 **applied** 的 S/X claim
   （以 manager 为事实来源）。仍等待中的请求其 claim 是 pending，**不是锁**，不能授权。

因此"OwnerID + GrantID"或"调用方自己拼的组件列表"都不构成能力：`LockGrant` 必须携带
Components/Generation/Term 并原样回传（中性层 `storageengine.LockGrant` 同样字段）。
`ReadCurrentUnlocked` 继续可用但永不记录 baseline，其观测不得作为提交基准。
读前与读后各验证一次；两次之间存在 epoch/grant 失效时丢弃已读到的值。

## Snapshot / Current / Validation Matrix
| 操作 | 读取基准 | 是否写回ReadTS | 是否新增dependency | post-readTS变更 |
|---|---|---|---|---|
| Txn.Get/Scan/NewIterator / ordinary SELECT | Begin固定readTS+自己/ancestor写 | 否 | 否 | 快照隐藏 |
| SELECT FOR UPDATE/SHARE | 获锁后最新published+read layer overlay | 否 | 否 | 可返回新版本 |
| UPDATE/DELETE目标验证 | parent候选后lock+current observation | 否 | 写入仍是原write set | 早期ErrConflict，root重试 |
| INSERT/唯一检查 | child overlay+lock后最新已发布索引 | 否 | 保留原写/guard | duplicate或SI conflict，不覆盖 |
| Guard/GuardRange最终验证 | latest committed change 对原readTS | 否 | 是 | ErrConflict |

B01 INV-003原snapshot API的规则保留；新增显式Current Read不是snapshot读。
**INV-005 精化（B02-1，见 §Locked-read write baseline）**：写入的 baseline 对"在锁下观测过的 key"是
观测到的版本，对其余 key 仍是 check readTS；任何 write/dependency 的最新版本超过其 baseline 仍不能提交。
不能用观察到的version替代全事务/逐key的commit baseline，也不能修改Command.Snapshot。

### Locked-read write baseline (B02-1)
本节是对 INV-005 的**精化**，不是放宽。它解决的是一个真实的架构冲突：B01 的 validator 只有
一个事务级 `readTS`（`mvcc/conflict.go` 的 `latestCommittedChange(dep) > readTS`），
因此"在锁下观测到最新版本 V 之后写该 key"会被旧 readTS 错误拒绝，而 §Snapshot/Current 矩阵
第 22 行要求的正是这种情况能够**早期冲突或正常写入**。两者不可能同时靠一个事务级 readTS 满足。

规则：

1. 锁定当前读在**仍持有覆盖锁**时观测到 key `k` 的已发布版本 `V`（含 tombstone 的 changeTS；
   不存在记为 0），并为 `k` 记录 baseline `V`。
2. 提交校验时，`k` 的 baseline 若存在，则比较 `latestCommittedChange(k) > baseline(k)`；
   否则仍比较 `> readTS`（原规则不变）。
3. baseline 的**边界**：只为本事务成功加锁的 key 记录，因此天然受锁预算约束；另有显式上限
   `maxObservationBaselines`。达到上限后**不再记录新 baseline**（fail-closed：未记录的 key
   退回 readTS 比较，只会更容易冲突，绝不会更宽松）。baseline 随事务终态释放。
4. **安全性论证**：`V` 在观测时刻就是最新已发布版本，覆盖锁在此 root 进入终态前阻止任何其他
   事务修改该 key。因此 `latest > V` 仍然只可能意味着"锁丢失/行在锁下被改"，丢失更新的防护
   没有被削弱；被放松的只是"用早于观测的 readTS 去否定一次已获锁的观测"。
5. **不变量保持**：`readTS`/`Snapshot`/GC horizon/`Command.Snapshot` 均不变；三个 root 类
   （empty/read-only、data-writing、dependency-only）的 Head 与 commitTS 语义不变；
   普通（未加锁）Put/Delete/Guard 的冲突检测逐字节不变，由
   `TestB02UnlockedWriteStillConflicts` 与 `TestB02BaselineIsPerKeyNotPerTransaction` 锁定。
6. **复制路径**：replicated apply 的 validator 只能看到 `Command`，无法携带该 baseline。
   因此带 baseline 的事务**不得**走 replicated apply，而是 fail closed
   （`errBaselineNeedsLocalValidation`）。禁止"只在SQL早冲突而durable层豁免"。
   后续若要让锁定读在复制拓扑下可用，必须先把 baseline 纳入 staged/pending 记录
   （不改 `Command` 字段），见 tasks.md T055。

## Read ordering and observation
1. 校验query shape、能力、metadata版本、owner/scope。
2. table intention与record/interval bundle获得；等待前无bbolt/iterator/stage views。
3. Raft情况下完成Barrier并重检term/generation。
4. 在grant仍有效时获取短storage view，仅选择有效publication marker的latest版本。
5. 叠加指定layer写，复制返回字节；包含不存在/tombstone的change metadata。
6. 关闭view后才把value传回executor或yield给可能等待的下游。

不存在从未发布与最新为tombstone必须可区分。OWN_OVERLAY没有独立commitTS；底层committed
change仍用于SI检查。等待前snapshot缓存或statement metadata cache不能代替步骤4。
generation/term/grant再次检查若失败必须丢弃value；不能把失效后结果作为当前读返回。

## Locking scan
完整PK/UNIQUE点查询使用专用点路径：短view探测后关闭view，先取得索引entry及common row
record grant，再重读；若值已不存在，定位并取得包含该插入位置的Gap，重新检查后才能返回
missing。发现身份改变则重新绑定/获锁重读；旧grants仍归root。已存在全键命中不得套用下面的
范围预锁路径而加无关Gap。点路径竞争与删除/再插入由T027/T036确定性夹具验证。

先保护整个access interval，再seek有限页；每页关闭storage view，随后按访问顺序Acquire record。
获锁后重读/重评WHERE、索引owner与metadata；删除或键迁移须重新seek稳定进度，不重复输出。
secondary/covering访问也要common primary row lock；不可因覆盖查询没有回表而省略row保护。
Reverse保留同一bounds成员；LIMIT可以多锁access interval/visited records，不能保证只锁结果行。
对于不能证明ordered编码的范围，whole-index gap与全部visited records是明确fallback。
普通snapshot iterator不改为current；所有stream/spill保持有界、Close/error/cancel清理。

## A05 DML validation
parent候选物理Identity不改变。newest target/catalog change >readTS包括delete/reinsert都报冲突，
因此不需要在旧快照下用新row重新计算SET或JOIN。compatible目标继续使用OldRow/EvalRow；
FK/UNIQUE使用原writer，old/new row/index资源都保护。source在parent，不扫描child。
无PK表使用旧heap physical key；old PK变更仍以旧Identity去重，new key作为新的reservation。
no-op UPDATE也持有target保护并遵守是否有原实际write的行为，不伪造数据version。

## Commit coordination and retention
current-onlyroot既无Put/Delete/Guard/GuardRange时，Commit走B01 A类，不分配revision。
dependency-only仍为C类，hasCommitTS=true但不安装数据version。写root仍走B类。
current observation不额外pin历史；root固定readTS继续决定GC horizon。latestview读时存储view本身
提供短时安全，不注册一个新root或提高horizon。
五路径都保留原validator；只在SQL早冲突而durable层豁免是禁止实现。

## Required validation
普通/锁定/普通序列，own/parent/sibling overlay，等待后值/墓碑，raw writer参与，页view先关闭，
late schema变更，三类root，五commitpaths含WALacceptedChanges，publication/cancel与GC契约。
