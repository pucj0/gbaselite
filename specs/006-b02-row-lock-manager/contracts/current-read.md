# B02 Locked Current Read Contract

**Related**: FR-011..019/026/029/030/032；plan A3/A4/A5。

## Capability surface (planned)
storageengine新增可选LockingTxn，原Txn/Engine最小接口保持。
- `BeginLockScope(ctx, kind)`：返回属于root的scope；ctx只决定该语句的取消/期限。
- `Acquire(ctx, scope, request)`：申请opaque GrantToken；request用中立Space/Key/Range。
- `ReadCurrent(ctx, grant, space, key)`：验证token覆盖record及epoch，返回CurrentObservation。
- `NewCurrentIterator(ctx, scope, scanRequest, lockPlan)`：只接受已证明bounds或显式fallback，
  每visited entry加锁后再current reread；Iterator须Close、borrowed bytes规则不变。
- `CloseLockScope(scope)`：取消pending，不解root grants。
上述名称为接口契约候选，实施只能一致调整所有设计/任务，不能偷偷把语义塞入Get。
Current read receiver就是read layer；child可读自己的overlay与ancestor，parent不能读未merge child。
SQL DML验证target使用parent receiver；unique/FK写中检查用child receiver，避免读到别的sibling。

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
