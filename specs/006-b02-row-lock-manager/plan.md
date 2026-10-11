# Implementation Plan: B02 InnoDB-style Row Lock Manager

**Branch**: Git `master` 未切换；feature `006-b02-row-lock-manager` 与分支独立。
**Date**: 2026-10-10 | **Spec**: [spec.md](spec.md)
**Input**: 用户 B02 请求；仅规划，不授权实现。

## Summary
每 engine 新增内存 Lock Manager，以 root 为 owner、statement/child 为 scope，采用实际逻辑
namespace/key 与稳定字节区间。提供 Record S/X、Table IS/IX、Gap、Next-Key、Insert Intention、
公平队列、有界等待图与确定 victim。storageengine optional capabilities 向 SQL 提供 Acquire、
locked-current 与诊断，默认 adapter/MVCC 所有 client 写入口参与协调。

普通快照 API 与 fixed readTS 不变；Current Read 是显式的新读模式。DML 从 parent snapshot
获取候选，child 写入，锁后最新变更大于 readTS 仍冲突。最终全部提交路径仍使用 B01 validator。
不会向现有 Command、stage codec、版本布局、WAL framing、Raft snapshot 添加字段。

## Technical Context
**Language/Version**: Go 1.23（go.mod）；Spec Kit 1.1.3、PowerShell scripts。
**Primary Dependencies**: 现有 bbolt 1.3.12、hashicorp/raft 1.7.3；锁管理器优先 Go 标准库，不引入外部锁服务。
**Storage**: 当前 MVCC nested/flat 布局、local WAL 与 replicated pending；锁元数据仅内存。
**Testing**: Go unit/contract/integration/race；现有 storageengine/testkit、MVCC/replication harness。
**Target Platform**: 当前 Windows/Linux amd64/arm64；race 在支持的 CI 平台运行，不增加平台承诺。
**Project Type**: 数据库服务与嵌入式 engine。
**Performance Goals**: 不相关资源在 held lock 期间推进；不承诺未测量吞吐提升。
**Constraints**: bounded lock metadata、无视图跨等待、单 Txn 单调用者、B01/A05/协议不变。
**Scale/Scope**: 默认 memory 64 MiB、root grant+pending components 262144、engine waiters 4096、
graph edge容量/遍历work各100000、diagnostic page 256；0 options 表示默认。预算按持有的 copied key/bounds 计实际字节。
已有大事务回归必须纳入预算基线；若默认不够，在实施前统一调整规划值，而不是修改兼容承诺或跳过测试。
**T004 校准（已完成）**: 实测含 PK+UNIQUE 的 SQL UPDATE 为 3 components/行（10000 行 = 30000，
20000 行 = 60000），而 65536 上限只支持约 21800 行；仓库自带 10000 行 UPDATE benchmark
路径已占用旧上限 46%。故 root 上限上调至 262144（≈8.7× 实测最大语句）。64 MiB 内存预算不变
且仍封顶：262144 × 96 B ≈ 24 MiB、× 216 B ≈ 54 MiB。证据 `.tmp/b02-resource-calibration.md`、
`.tmp/b02-baseline.md`。上调为放宽，不改变任何 SI/validator 语义；超限仍是 fail closed 的
`ErrLockResourceLimit`，需在 README 限制矩阵登记（T070）。

## Constitution Check
Gate 在 Phase 0 前、Phase 1 设计后各复核，结果均为文档设计 PASS；运行结果待实施。

| 原则 | 设计证据 | Gate |
|---|---|---|
| I 文档准确 | README 区分 planned、现有锁定读快照语义及 future limits | PASS |
| II 事务/格式 | §Current Read、§Commit；B01 snapshot API/INV-005 不变；无格式字段 | PASS |
| III 边界/预算 | 中立 capability、per-index latch、预算拒绝与归属 | PASS |
| IV 验证 | traceability.md、确定性场景、Go/race/full regressions tasks | PASS |
| V 授权 | 全部 implementation task 未勾选；本轮仅文档与官方技能工具 | PASS |

## Project Structure

### Documentation (this feature)
```text
specs/006-b02-row-lock-manager/
  spec.md  plan.md  research.md  data-model.md  quickstart.md  tasks.md
  traceability.md  workflow.md
  checklists/requirements.md  checklists/clarification.md
  contracts/lock-manager.md  contracts/current-read.md
  contracts/sql-locking.md  contracts/lock-diagnostics.md
```

### Source Code (repository root)
以下为未来目标，不是本轮创建的生产文件。`(new)` 与既有文件明确区分。
```text
lockmanager/ (new) types.go resource.go interval.go compatibility.go manager.go
                   queue.go graph.go budget.go diagnostics.go clock.go
storageengine/locking.go (new), engine.go options
storageengine/mvccadapter/locking.go (new), adapter.go iterator.go
mvcc/locking.go current_read.go (new), transaction.go merge.go snapshot.go store.go
executor/locking_read.go locking_modify.go lock_plan.go locking_diagnostics.go (new)
executor/access_plan.go physical_binding.go physical_select.go transaction_engine.go
executor/modify_insert.go modify_update.go modify_delete.go mutation.go
executor/mutation_insert_modes.go mutation_join_pipeline.go multi_delete_pipeline.go
executor/foreign_key.go secondary_index.go range_access.go savepoint_mvcc.go executor.go
executor/session_settings.go open_options.go
parser/ast.go parser.go
server/handler.go connection.go metadata_browse.go
config/config.go enginefactory/default.go cmd/gbaselite/main.go
replication/node.go
```
**Structure Decision**: 新中立 `lockmanager` 包不 import executor/mvcc/bbolt；SQL 不 import
lockmanager 具体 manager，只用 storageengine DTO/capabilities。backend adapter 组合 manager 与 MVCC。
新增文件附近放测试；不能用任务中的建议文件名声称已有实现。

## Architecture and Requirement Allocation

### A1 — Resource Identity and Compatibility (FR-001..006/015/027)
Resource 核心为 generation + namespace + copied key/bounds；TableID/IndexID 是 SQL 绑定的
中立标签，不重编码持久化 key。主记录资源始终使用 Rows(TableID)+旧 physical Key，二级访问
必须额外锁这个 row；UNIQUE reservation 是对应 UniqueIndex namespace 的既有 encoded key。
table intentions 为同 TableID 的 IS/IX；所有矩阵及 interval membership 在 contracts/lock-manager.md。

Gap 使用稳定开闭区间。Next-Key 常规 (previous,key] 分解成 (previous,key) Gap + key Record；
terminal gap 为 (last,+∞)，空域 (-∞,+∞)。范围边界/未命中 predicate 可以保守覆盖更宽区间。
域内所有 insertion 对 raw Put 和 SQL index Put 都先检查 II membership。旧 encoding 或复杂 predicate
无法证明正确 bounds 时，whole-index Gap + 扫描 records；不设置 Table X/global engine gate。

### A2 — Manager, Queue and Wait Graph (FR-007..010/024/025/029)
per-engine manager 按 namespace/index domain 分片。多域 bundle 按 domain 的稳定顺序取短 latch，
检查兼容、fairness 与预算，再整体 grant 或登记一个 pending request。不得先授予半个 Next-Key；
已有同 owner grant/升级前的 S 保留。记录/区间索引初版可采用有界有序集合，扫描复杂度由预算限制；
不额外承诺区间树性能。没有 user wait 持有 domain/owner/graph latch。

holder edges 与 older incompatible waiter edges 都进入 graph，root 级去重。入队/取消/释放/升级
更新 graph revision；Cycle 选择 root registration ordinal 最大者，检测完成后核对 revision 和边。
稳定 ordinal 与 commit sequence 无关，不推进 Head，溢出 fail closed。遍历超过预算回滚新请求并报
resource limit，不声称发现死锁。检测器仅标记 abortRequested + wake；owner goroutine 完成 root cleanup。

Request 状态单一终态：GRANTED/TIMED_OUT/CANCELED/DEADLOCK_VICTIM/INVALIDATED/RESOURCE_REJECTED。
grant/cancel/deadline 同时发生由 latch 内转换线性化；任何 grant-then-cancel 也必须通过 owner 释放规则，
不得把 granted 请求偷偷从 map 删除。deadline 来源由可注入 monotonic clock；query deadline 更短时优先。

### A3 — Current Read (FR-011..013/018/026/028/029/030/032)
原 Txn.Get/Snapshot/NewIterator 代码语义不变。optional LockingTxn 的 ReadCurrent 验证 grant、
generation/term 和 read layer 属于 owner；overlay 只读指定 layer 自己及 ancestor，不能看 sibling。
在 granted 之后开短 storage view，使用 latest published reader（含 flat/nested），返回 tombstone/changeTS。
不把 observation 变成 Guard、commit baseline、全事务 read-set 或 durable receipt。

**A3.1 — Locked-read write baseline（INV-005 精化，见 contracts/current-read.md §Locked-read write baseline）**

实施期发现必须先解决的架构冲突：B01 的 validator 只有一个事务级 `readTS`
（`mvcc/conflict.go`：`latestCommittedChange(dep) > readTS`）。锁定当前读在锁下观测到版本 `V > readTS`
之后写同一 key，会被旧 `readTS` 拒绝；而 §Snapshot/Current 矩阵与用户要求都要求这次写入是**合法**的
（锁已阻止丢失更新）。二者无法同时用一个事务级 readTS 满足。

修订（不改变 `readTS`/`Snapshot`/GC horizon/`Command.Snapshot`，不放宽未加锁路径）：

1. 锁下观测到 key `k` 的版本 `V` 时，记录**逐 key** baseline `k→V`（tombstone 记 changeTS，
   不存在记 0）。只为本事务**成功加锁**的 key 记录。
2. 提交校验时，`k` 有 baseline 则比较 `> baseline(k)`；否则仍比较 `> readTS`（原规则）。
3. baseline 有界：受锁预算 + `maxObservationBaselines`；到上限后不再记录新 baseline
   （fail-closed，未记录的 key 退回 readTS 比较）。
4. 未加锁的 Put/Delete/Guard 冲突检测逐字节不变。
5. **五条提交路径都必须携带同一 baseline**：`commitLocalWith`（bounded/group）、
   `commitLocalStream`、`commitLocalWALGroup` 通过 `localCommitRequest.baseline` 传入。
   replicated apply 只能看到 `Command`，无法携带，因此带 baseline 的事务**fail closed**
   （`errBaselineNeedsLocalValidation`），禁止 durable 层豁免。

锁定 scan 先取得 proven/fallback access interval protection，再 seek 当前 index，复制有限页并关闭
storage view；对每个 visited record（和回表 row）取得 S/X，然后重新读并重评 WHERE，再输出。
等待后重新 seek/resume、检查删除/键迁移，避免 stale cache；保持 no-skip/no-duplicate 对受保护域的
候选观察。普通 SELECT 仍走原 iterator。LIMIT 可保守锁整个 access interval，ORDER 可锁更多 examined
records；EXPLAIN/diagnostics 标注 fallback/范围，不承诺只锁输出行。

初版仅单基表锁定查询；SQL shape、mode 与 options 在绑定前检查；FOR SHARE 是新增语法，
LOCK IN SHARE MODE 别名，NOWAIT/SKIP LOCKED 不再静默忽略。autocommit 锁持有到结果消费完成，
读-only root 不额外提交 revision；explicit transaction 则持至终态。

### A4 — A05 DML and Raw Writes (FR-014..018/026/029/032)
parent fixed snapshot scan → stable candidate/dedup/LIMIT → lock bundle → latest target verification
→ existing ModifyOperator evaluation → existing FK/index writer → child merge。
按旧 Identity 锁定，current changeTS > readTS（含不存在/墓碑）则早期 ErrConflict，终止整个 root；
compatible target 使用原 parent OldRow/EvalRow。JOIN source/子查询/INSERT SELECT 都维持 parent readTS。
不会让修改后的 key 再加入候选。多目标 DELETE 保持现有 FK ordering/barrier/spill。

每 row mutation 保护 old/new row 以及 old/new unique/secondary entry；key-changing update 删除
旧 entry 和创建新 entry，后者有 II。INSERT 判重在 grants 后基于 child overlay+latest committed，
NULL UNIQUE 无 reservation；IGNORE/REPLACE/upsert 仍调用统一 writer并遵守 stale-target conflict。
FK check 保留 Guard/GuardRange，cascade/SET NULL 进入相同锁路径和深度/资源预算。

low-level MVCC Tx.Put/Delete 作为默认后端 client seam 自动获取同 namespace/key X，真正创建 key
取得 II；SQL child 先用 statement ctx 显式预取，low-level 重入同 owner。stage 写/merge 没有新 grants
等锁；child commit 不重复入队。普通 Get 不参与锁，Guard 原义不变。Raw Apply/WAL recovery 是
受控耐久入口，不属于阻塞锁 API，绕过 lock 不意味着绕过 final validator。

### A5 — Commit and Lifecycle (FR-009/019..022/029/030)
Lock owner 是 outermost root，不是当前 savepoint layer。statement scope 在所有退出路径关闭，
取消 pending；grants 记录源 scope 供诊断，但 child/savepoint rollback 不解锁。RELEASE/同名替换
仍占用现有 layer budget；ROLLBACK TO 依现有顺序 cleanup discarded child并重建，不改 root retention。

本地 bounded/group/stream/WAL 与 replicated `Command.Snapshot` 保留 readTS；所有 Put/Delete/Guard
共享现有 validator。后续 raw/apply 变化仍会在 final validation 导致冲突，不能仅信任锁前检查。
三个 root 类（empty/read-only、data-writing、dependency-only）的 Head/commitTS 不变。
只有 publication marker durable 后才终态 COMMITTED；未定结果保留 grants 直到 publication-aware
reconciliation 决定，不允许 context cancel 把已提交结果改为 rollback。

root finish：确定终态 → 原 Txn 禁止进一步读写/merge → 所有 child stage cleanup → cancel scopes /
release grants/edges → registry/horizon 清理。cleanup error 聚合上报，但 closed Txn 不能因错误继续写。
释放幂等；新的 engine generation manager epoch 不与旧 owner混用。deadlock root 中止不能从 detector
并发触碰 stage；所有环内成员都是 waiters，由 wake 路径在 owner safe point 结束 victim。

### A6 — Leadership, Diagnostics and Configuration (FR-010/022..027/031)
leader-only Current Read 在可能等待后执行 Barrier + Term/Generation 校验，并且在读 view 前再次
检查 owner epoch；VerifyLeader 单独不够。leader loss/restore/Close 使旧 epoch pending 进入 INVALIDATED，
授予不能再使用；replicated publish继续 existing Term fence/marker 判断；FSM Apply、日志重放永不等用户锁。
不可撤销已开始的 durable publication；失效期间的 outcome 以 marker/现有恢复协议为准。

`SET SESSION innodb_lock_wait_timeout` 使用整数秒，默认50，1..86400；GLOBAL/负数/0/小数报错。
engine options 提供 lock memory/root count/waiter/graph bounds，config YAML/env 映射需要 T056/T057
联动 README，不能复用 transaction_write_mb=0 的含义。预算原子计量所有 cloned bytes及图边，
失败无部分 bundle；full cleanup 计量归零。大事务预算调整在基线 gate 中完成。

配置完整名称、单位、范围、env 优先级与装配入口由 contracts/lock-manager.md 的 Configuration
表固定；CLI 启动时通过既有 config → open options → enginefactory → adapter 链传递，不增加新子命令。

管理员 SHOW ENGINE LOCKS / LOCK WAITS / DEADLOCKS 调用 optional diagnostics，最多256 rows，
snapshot revision + truncated/next cursor 明确；匿名 opaque resource ID 不直接 hash 可猜的 PK。
限长 ring buffer 保存最近32 deadlock summaries。普通账号无权读，不存 row/key/SQL；调用不等待事务。

## Testing and Acceptance Gates
- Unit：全部 compatibility 有向组合、边界、atomic bundle、重入/升级、budget exact/+1。
- Queue/graph：FIFO batch、无关资源、older-waiter edges、二/三 cycle、upgrade、ordinal overflow。
- Deterministic：hooks/barriers/fake clock；100 次固定调度，grant/timeout/cancel/epoch races。
- Contract：optional capability absent、copied bytes、record/interval identity、current tombstone/overlay。
- SQL：FOR UPDATE/SHARE/alias、shape reject、PK/UNIQUE/secondary/missing/range/reverse/LIMIT、模式分离。
- DML：所有 A05 变体、FK、PK迁移、NULL/collation、stale targets、child resource/spill cleanup。
- Durability：五类 commit path、WAL acceptedChanges、unpublished/cancel、restore/term；旧格式无 migration。
- Regression：B01 三类 root/GC/readTS + A05/large transaction tests保留；不能通过 skip 修改不变量。
- 实施后：gofmt、go test ./...、go vet ./...、支持平台 race，命令/证据见 quickstart.md。

## Implementation Phases
按 tasks.md 的依赖：Setup → Foundation → US1 Record/queue → US2 ranges → US3 locked current SQL
→ US4 DML/unique → US5 lifecycle/durability/deadlock → US6 diagnostics → cross-cutting validation。
Core 里先构建取消/owner基础以允许各 US 独立夹具测试；未完成 US5 之前不能对外发布 SQL 行锁。
MVP 仅 US1 的中立 engine 内核验收，不宣称 B02 全部交付。

## Risks and Mitigations
| 风险 | 应对 / 验收 |
|---|---|
| 把 Current Read 混入原快照 | 独立 API、三次读测试、原 Snapshot 字段不变 |
| locked-write rebase 造成复制偏差 | 早 conflict + 五路径 final validator，无 codec/command 变化 |
| Iterator 在 wait 中持 view | page close-before-yield 测试；并发 writer/remap hook |
| 旧编码/索引与 gap 不匹配 | planner proof 或 whole-domain fallback；NULL/collation/heap fixture |
| queue graph stale、victim 并发操作 Tx | revision recheck、owner safe point、race fixture |
| restore/term 未定提交释放过早 | epoch invalidation + publication reconciliation |
| root 保守持锁/预算限制大事务 | 显式限制、基线和资源校准；不提前 unlock |

## Complexity Tracking
无 constitution 例外。区间索引与 root graph 带来复杂度是指定锁能力所需；没有新增持久化系统或锁日志。

## Design Completion and Authorization
research/model/contracts/quickstart 的规划完成不等于实现完成。全部功能任务等待后续明确实现指令。
没有必须由外部资料或用户回答才能决定的设计项；性能优化与预算调校作为实施验收任务，而非伪造已通过结果。
