# B02 Lock Diagnostics Contract

**Related**: FR-023..025/031；plan A6。DTO位于storageengine，backend提供optional capability。

## Planned surface
`LockDiagnostics`返回LocksPage(filter,cursor,limit)、WaitsPage(...)、DeadlocksPage(...)及LockStats。
每页最多256条；请求更大limit截断到256并标记；cursor只对返回的SnapshotRevision有效。
不支持此能力返回ErrUnsupported，不能把空数组当作“没有锁”。
```sql
SHOW ENGINE LOCKS;
SHOW ENGINE LOCK WAITS;
SHOW ENGINE DEADLOCKS;
```
这些命令只允许管理员。返回有界snapshot，明确SnapshotRevision、Truncated、NextCursor；
不得持managerlatch执行SQL输出/网络写，读取不等待usertransactions。

## Columns
| View | 字段 |
|---|---|
| Locks | GrantID、RootID、OriginTxnID/ScopeID、Generation、Term、Mode、ResourceKind、OpaqueResourceID、GrantedAt |
| Waits | RequestID、RootID、ScopeID、RequestedMode、OpaqueResourceID、BlockerRootIDs、Reason、WaitMilliseconds、Deadline |
| Deadlocks | DetectedOrdinal、GraphRevision、VictimRootID、CycleRootIDs、匿名资源/模式摘要、Truncated |
| Stats | ActiveOwners、GrantedComponents、PendingRequests、WaitEdges、MemoryBytes、Timeouts、Deadlocks、ResourceRejections、EpochInvalidations |

不输出rawSpace/key/bounds/value/SQL，不用可枚举PK的直接hash伪匿名化。OpaqueResourceID只在
当前engineepoch有效，由随机/独立序号映射产生；复制、重启、恢复后变化不影响事务语义。
Deadline/time只诊断，不参与MVCCversionorder；已关闭child的OriginScope可显示historicalID，
不能被误认为activechildregistryentry。

## Retention / snapshots
只保存最近32deadlocks，每摘要最多32nodes；Graph检测本身不受摘要截断影响。
分页无无限snapshot缓存；cursor失效时明确要求重新获取，不返回混合两版的holder/edge。
Aggregate计数process-local，restore后epoch相关当前计数归零、累计计数按engine lifetime保留。
bytes/request计量不会因重复release下溢；snapshotcopy不暴露可变内部引用。

## Acceptance
权限正反例，不存在capability，256/257截断、invalidcursor、匿名数据检查、诊断并发Acquire/
Release/epochreset、ring32/33、boundedcopy与终态当前计数归零，全部用隔离fixtures。
