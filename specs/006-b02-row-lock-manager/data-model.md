# B02 Data Model

所有新增实体为进程内状态；不改变 mvcc Op/Command、stage codec、持久库或 WAL/Raft。

## LockOwner
字段：RootID、RegistrationOrdinal、Generation、LeaderTerm、ReadTS（只引用不修改）、
State（ACTIVE/ABORT_REQUESTED/FINISHING/ENDED）、Scopes、GrantedIDs、PendingIDs、BudgetUsage。
RootID 与 B01 root ID 相同；child 通过祖先解析唯一 owner。Ordinal 独立单调，耗尽拒绝注册，
与 Head/commit sequence 无关。生命周期关联不能让 manager 掌握可并发 Rollback 的 Tx 指针。

## LockScope
字段：ScopeID、OwnerRootID、OriginTxnID、ParentScopeID、Kind（statement/child/savepoint）、
ContextDeadline、State（OPEN/CLOSING/CLOSED）。scope 关闭撤销自身 pending，不释放 grants。
同名 savepoint 的匿名 layer 仍具有独立 scope；源 scope 消失时 grants 的 root owner 不丢失。

## Resource / LockComponent
字段：Generation、Space、TableID、IndexID、Kind、Key/Lower/Upper、LowerInclusive、UpperInclusive。
Key 和 endpoints 是 owned copied bytes；Space 是实际逻辑 namespace，标签不改变 namespace 编码。
nil bound = infinity；empty non-nil = 有效字节边界；不可用 0 key 或空字符串代替 unbounded。
Kinds：TABLE_INTENT、RECORD、INTERVAL。TABLE_INTENT 使用 TableID；RECORD 使用 Space+Key，
所有 primary/secondary access 对 row 的保护汇合到 row space identity；INTERVAL 同 Space 相交判断。
Mode：IS/IX、S/X、GAP、INSERT_INTENTION。Next-Key 是多个 components 的 atomic bundle。
II 使用 exact insertion key，检测 interval.contains(key)；Gap 不依赖当前邻居是否存在。

## LockRequest / LockGrant
Request 字段：ID、OwnerRootID、ScopeID、Components、EnqueueOrdinal、RequestedAt、Deadline、
State、Blockers、ReservedBudget、**终态广播**（settled channel）、**origin 标记**（该 handle 是否属于
创建该 request 的调用者）。
转换：NEW → GRANTED 或 WAITING；WAITING → GRANTED/TIMED_OUT/CANCELED/
DEADLOCK_VICTIM/INVALIDATED/RESOURCE_REJECTED。所有终态唯一、不可重新 WAITING。
**一个 request 可有多个等待者**（创建者 + 加入者）：终态确定时关闭 settled channel 广播给全部等待者，
每个等待者从同一 decision 读取同一 status/grant；不设"单次通知 channel"（它只能唤醒一个等待者，
其余会为已决定的结果继续阻塞）。只有 origin handle 拥有撤销权：加入者的 context 结束只结束它自己的
等待，`Cancel`/scope 关闭对加入者无操作；创建者的 context 结束/scope 关闭/root 终态/epoch reset
撤销整个 request 并唤醒全部等待者。详见 contracts/lock-manager.md §共享等待。
Grant 字段：GrantID、OwnerRootID、OriginScopeID、Generation/Term、Components、GrantedAt。
Grant 身份属于**产生它的那个 request**：GrantID 是该 request 完成时的 id（重入请求为 0）、
OriginScopeID 是该 request 的 scope（不是 RequestID）、Components 恰好是该 request 取得或 adopt 的
组件（不得拼接同一 root 其他 request 的锁）；能力在终态确定时记录一次，同一 request 的所有调用者一致。
Request 的 claim 状态分 applied（属于 GRANTED 的 request）与 pending（属于 WAITING 的 request），
只有 applied claim 构成"已持有"，重入/adopt 判定见 contracts/lock-manager.md §Claim state and grant identity。
Next-Key/bundle 要么全部有效，要么没有新 grant；已有 S 的升级等待不能临时删除 S。
GrantToken 为 opaque handles，不是 durable version receipt；root 结束/epoch失效立即不可用。

## WaitEdge / DeadlockSummary
Edge 字段：FromRootID、ToRootID、RequestID、Reason（HOLDER/OLDER_WAITER）、ResourceID、GraphRevision。
同 owner 没有 self edge；一个 request 可有多个阻塞者。取消/授予/释放同步删边，不能遗留 dangling root。
DeadlockSummary 字段：DetectedOrdinal、GraphRevision、VictimRootID、CycleRootIDs、匿名 resource/mode、
Reason；只保留最近32条、每条最多32个诊断环节点；真实检测不因摘要截断而漏环。
Victim=max RegistrationOrdinal。遍历预算耗尽是 resource failure，不伪造 deadlock。

## CurrentObservation
字段：Value owned bytes、Exists bool、ChangeTS uint64、HasCommittedChange bool、Source
（COMMITTED/OWN_OVERLAY/ABSENT）、Tombstone bool。ChangeTS=0 可能是从未出现，必须用布尔区分。
即使最新为 tombstone，也返回 changeTS；OWN_OVERLAY 不伪造 durable commitTS，必要时另外保留
underlying committed change 元数据供 SI verification。Observation 不加入 B01 dependency set。

## Budget / DiagnosticsPage
Budget：MemoryLimit、RootRequestLimit、WaiterLimit、GraphWorkLimit；对 copied resource bytes、
components、grants、pending、edges、bounded ring 计量。request→grant 转移不重复计费；释放幂等归零。
默认64MiB metadata、262144 grant+pending components/root、4096waiters/engine、100000图边/work
（262144 为 T004 校准值：实测含 PK+UNIQUE 的 SQL UPDATE 为 3 components/行，20000 行 = 60000，
65536 只够约 21800 行；262144 × 最坏 216 B ≈ 54 MiB 仍受 64 MiB 封顶；
见 `.tmp/b02-resource-calibration.md`）；
预算单位、option范围和配置名称以contracts/lock-manager.md配置表为准，不将component数量当bundle数量。
DiagnosticsPage：SnapshotRevision、Rows（最多256）、Truncated、NextCursor；cursor仅同 snapshot有效，
不提供隐藏无限缓存。ResourceID为不可逆 opaque 随机/序号域标识，不编码或暴露真实 key。

## Lifecycle Relationship
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

## Validation Rules
非当前 generation/term、错误 owner/layer/token、非法 mode/kind、颠倒/空区间、预算 overflow、ordinal
wrap、未知 root/scope 都 fail closed。DDL 改变 metadata version时先冲突，不用旧schema解码新版本。
No dirty read、publication atomicity、root-only retention、savepoint32层、single consumer 保持原契约。
