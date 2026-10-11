# B02 Lock Manager Contract

**Related**: FR-001..010/020..022/025..027/029；plan A1/A2/A5。
本文件定义未来接口语义，不是新增生产实现。

## Identity and atomicity
Record identity = `(Generation, Space, Key)`；interval conflict domain = `(Generation, Space)`。
Table intent identity = `(Generation, immutable TableID)`。SQL TableID/IndexID 标签来自现有 metadata；
manager 不解析 SQL key，不能把 label 加入 record identity 导致 raw Write 与 SQL Lock 是两个资源。
未绑定的 raw namespace 使用 namespace intent，绑定的 row/index namespace沿用 TableID；中立
NamespaceBinding 在 adapter 发布/验证，冲突绑定报错误，计入预算，generation reset时清理。

Acquire 接收 root handle、scope、多个 LockComponents、ctx；返回 GrantToken 或中立错误。
NewOwner 绑定 B01 rootID/readTS/epoch；NewScope/CloseScope 管理请求来源。ReleaseRoot 仅由 owner
终态路径调用且幂等。BindNamespace 不允许使用变化的 SQL 表名字充当稳定 TableID。
Next-Key/多键 bundle 要么全授予、要么一个 pending，不留下新部分 grants；已持的同root grant保留。

## Compatibility Matrix
表格中的 Y 表示不同 owner、同资源且无 older-waiter 公平性阻塞时可以并存。

### Record：请求 / 已持有
| Request \ Held | S | X |
|---|---|---|
| S | Y | N |
| X | N | N |

### Table intention：请求 / 已持有
| Request \ Held | IS | IX |
|---|---|---|
| IS | Y | Y |
| IX | Y | Y |

Table S/X、SIX、自动 table escalation 不在 B02；不能由此表宣称支持 LOCK TABLES。
申请 record S 前必须已拥有或在 bundle 中申请 IS，record X/II 前必须 IX；IX 同时满足本 root 的 IS。

### Interval / insertion：请求 / 已持有
| Request \ Held | Gap | Insert Intention |
|---|---|---|
| Gap | Y | N 若区间含插入点；否则 Y |
| Insert Intention | N 若持有区间含插入点；否则 Y | Y |

II 同位置也兼容；实际同键 INSERT 必须再申请 record X reservation，不能仅凭 II 插入。
Gap 与已存在 row Record S/X 兼容：Gap 抑制创建 key，不保护现有值。缺失 key 的 X reservation
要创建 key 时必须 II；不允许通过 `Put` 先拿 X 再绕过 Gap。
Next-Key S/X 的 record 部分遵守 Record matrix，gap 部分遵守 interval matrix，整体按最严格结果。
同 root 的祖先/child 重入兼容，S→X升级按其他 root 的 grants及公平性判定，等待时不释放 S。

## Ranges and scan coverage
所有比较使用现有 unsigned byte lexicographic order。nil是±∞，empty non-nil是有效边界。
Gap 区间可有显式开闭端点，普通 Next-Key=(prev,key)Gap+Record(key)。空域全区间，terminal gap
(last,+∞)。Missing point保护包含该插入位置的 stable区间；可使用更宽 fallback，不能产生空保护。
交集算法必须覆盖不同 endpoints 的 overlap，不能仅按相同 bounds 查 map。
对 lower/upper 边界的插入是否等待以 contains 为准；reverse 不反转 interval成员语义。
删除邻居、GC/页分裂不使已授予区间消失；grant终态才撤销保护。

## Queue and graph
每个 request 有单调 EnqueueOrdinal。请求被 held incompatible components 或老 waiter 阻塞。
年轻请求只有不会阻碍老 waiter时才能共授予；对不相交resource可立即推进。批量S grant允许，但
不能不断向老X waiter前插队。每条边按 rootID去重、记录 HOLDER/OLDER_WAITER原因。
升级保留 S并入图；双升级是合法可检测环。Victim=环内 RegistrationOrdinal最大root。
graph revision变化须重新验证环与victim；检测预算超限报告 ErrLockResourceLimit并移除新请求，
不得报告虚假死锁。Victim收到 ErrDeadlock并在自身调用栈终止整个root，检测线程不调用Txn方法。

## Claim state and grant identity（本轮补齐的两条契约）

**Applied vs Pending**：claim 有两种状态——**applied**（属于已 GRANTED 的 request）与 **pending**
（属于仍 WAITING 的 request）。只有 applied claim 代表"已持有"：

- 重入判定（`LockOpReentrant`）只看 applied claim。pending claim **绝不**产生 REENTRANT/GRANTED：
  一个已持有 S 并在排队升级 X 的 owner，重复申请 X 时必须继续等待，而不能被告知"你已经有了"。
- 对同一 owner 已排队保护的重复申请**加入该请求**：返回 `LockOpWait` 且 `RequestID` 与原请求相同，
  两个调用者共享同一终态与同一次通知；不创建第二个 request、不产生第二份 claim、不重复计费。
  拒绝等待的调用（`TryAcquire`）在此情况返回 `ErrConflict`，不得返回 REENTRANT。
- 公平队列的"同 owner 较早请求"规则只在较早 claim 已 applied 时才 adopt，且 adopt 时必须撤销本请求
  排队时登记的那条 claim（否则会留下幽灵 claim，导致释放真正持有的锁后资源仍被占）。
  较早请求仍在排队时，本 entry 继续等待——它保留自己的 claim，因此不受较早请求被取消的影响。
- 只有排在**本 entry 之前**的队列项参与该判定（FIFO 顺序）：较年轻的同 owner 请求不得阻挡较年长者。

**Grant identity**：等待获授后返回的 `LockGrant` 必须是**该 request 自己的**能力：

| 字段 | 取值 |
|---|---|
| `GrantID` | 该 request 完成时分配的 grant id；重入请求为 0（它没有取得新锁） |
| `OriginScopeID` | 该 request 的 origin scope（**不是** RequestID） |
| `Components` | 该 request 取得或合法 adopt 的组件，恰好是它的 bundle 内容 |
| `OwnerID` / `Generation` / `Term` | 该 owner 的登记值；owner 结束或被 epoch 替换后该能力失效 |

禁止用"同一 root 的全量锁清单"（如 `ledger.GrantedResources`）重建 grant：那会把该 root 其他
request 的锁拼进本次 grant。能力在 request 终态确定时记录一次，同一 request 的每个调用者读到同一身份，
每次读取返回独立的组件副本。

### 共享等待：终态广播与取消所有权（本轮补齐）

一个 request 可以由同一 root 的多个调用者等待：**创建者**（`Acquire` 返回 WAIT 的那个调用，origin
handle）以及所有**加入者**（`origin=false`，见上文"重复申请加入该请求"）。由此确定两条规则：

**终态广播**：request 的终态是**广播**，不是交给单个接收者的值。

- 终态确定时关闭该 request 的 settled channel（在状态写入之后关闭，只关闭一次）。所有等待者同时被
  唤醒，然后各自从同一 decision 读取**同一个** status 与**同一个** grant（`GrantID` 相同）。
- 禁止用"容量为 1 的 channel + 一次发送"承载终态：它只能唤醒一个等待者，其余等待者会为一个已经
  决定的结果继续阻塞。
- 终态可被任意次重复读取：`Status`、`Wait`、`WaitStatus`、`WaitForGrantResult`/`WaitForGrant`
  对同一 handle 必须给出一致答案，不存在"已被消费"的终态，也不存在第二份通知。
- 唤醒来源与终态一一对应：grant、显式 cancel、等待期限、deadlock victim、epoch reset、root 终态、
  scope 关闭都必须唤醒**全部**等待者。

**取消所有权**：只有创建该 request 的调用者拥有撤销权。

| 事件 | 对共享 request 的影响 |
|---|---|
| 创建者 context 结束 / `Cancel(root, origin handle)` | **撤销整个 request**，全部等待者（含加入者）被同一终态唤醒 |
| 创建者 scope 关闭 | 撤销该 request（scope 只撤销自己发起的请求），全部等待者被唤醒 |
| 加入者 context 结束 | 只结束**它自己**的等待并返回它自己的 ctx 错误；request 保持排队，创建者与其他加入者不受影响 |
| 加入者 `Cancel(root, joined handle)` | 无操作，返回 request 当前状态（加入者没有自己的 request 可撤销） |
| 加入者 scope 关闭 | 不影响它未创建的 request |
| root 终态 / epoch reset / deadlock victim | 撤销该 root（或该 epoch 的）全部 request，全部等待者被唤醒 |

加入不产生第二份 claim，也不重复扣预算：加入路径在 `Reserve` 之前返回，且不创建 request。

## Deadline and errors
等待期限是会话lock期限与ctx期限的较早者；未来可注入clock，生产使用monotonic elapsed time。
Acquire返回：ErrLockWaitTimeout、ErrDeadlock、ErrLockResourceLimit、ErrClosed/ErrNotLeader/
ErrConflict(epoch/schema/SI)或ctx取消。错误类型在storageengine中立层，adapter可转换backend错误。
GRANTED与TIMED_OUT/CANCELED等终态由单一latch内转换决定，通知一次。scope关闭仅撤销pending，
已授予锁始终归root，timeout后显式root还持这些grants。

## Locking discipline and budgets
domain latches按稳定namespace顺序，owner/graph latch短期；无latch跨ctx等待或bbolt view。
默认64MiB copied metadata、262144 root grant+pending components、4096waiters、100000graph edges/work；0 options
取默认，负数/overflow拒绝。每次reserve后再改状态，失败无部分bundle；ID/ordinal耗尽fail closed。
release归还计量一次；diagnostics不得反向调用storage或waiter。无关domain可以并行操作。

## Configuration

以下为待实施字段，嵌入式options均使用int64；YAML放在既有resources节，env沿现有配置覆盖顺序优先于YAML。

| YAML resources字段 | 环境变量 | 中立engine option | 默认 / 单位 |
|---|---|---|---|
| lock_memory_mb | DB_LOCK_MEMORY_MB | LockMemoryBytes | 64 MiB，配置按MiB转换成bytes |
| lock_max_requests_per_txn | DB_LOCK_MAX_REQUESTS_PER_TXN | LockMaxRequestsPerRoot | 262144 个grants+pending components |
| lock_max_waiters | DB_LOCK_MAX_WAITERS | LockMaxWaiters | 4096 个pending requests / engine |
| lock_deadlock_work_limit | DB_LOCK_DEADLOCK_WORK_LIMIT | LockGraphWorkLimit | 100000 条图边容量与单次遍历work上限 |

每个配置/option的0或缺省取对应默认，不代表无限；负数与非整数拒绝。memory_mb合法正值
1..1048576，按1024*1024转换并校验overflow；LockMemoryBytes合法正值1..1099511627776。
其余合法正值1..2147483647，不依赖平台int宽度。设置非常小预算允许，超过时返回资源错误，
不提前释放grants。配置变更只在engine Open生效；不引入运行期重载/SET GLOBAL预算。
服务启动和嵌入式Open均转到同一Options→enginefactory→adapter装配链。会话超时独立，
详见sql-locking.md；lock预算不复用transaction_write_mb。以上数值已由T004与大事务基线共同校准
（lock_max_requests_per_txn 由 65536 上调至 262144，证据 `.tmp/b02-resource-calibration.md`；
64 MiB 内存预算不变且仍封顶）。若后续再调整默认，必须同步本表、plan/research/data-model、
README及exact/+1测试。

## Lifecycle
root定案并不能再使用写集后ReleaseRoot；child/savepoint/statement关闭只CloseScope。
epoch reset唤醒所有pending并使旧token无效；Close唤醒并清理；旧epoch late notification不能更改
新owner或新generation计量。grants释放不得掩盖COMMITTED或改判已发布结果。
锁不写日志、不随backup/restore/replica传播；apply/replay不使用Acquire。

## Acceptance contract
全部矩阵有向对、同owner、interval端点、atomic bundle、FIFO、多blocker、二/三环、双升级、
grant/cancel/deadline/epoch race及exact/+1预算都需要确定性测试，见traceability.md。
