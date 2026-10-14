# Clarifications: B03 Decisions and Defaults

**Status**: 主要设计决策已在 plan/research/contracts 给出建议；带 `[ ]` 的项目是实施前必须以源码或用户明确意见确认的架构门禁，不是“未考虑”。

## Adopted defaults

- [x] 默认 RR；SQL 显式支持 RU/RC/RR/SERIALIZABLE，SESSION 与 NEXT scopes。
- [x] 参考 MySQL 8.4 InnoDB：RR **首个一致性读**快照、RC **每语句**快照；锁定读为当前读。
- [x] RU 真正允许 staged dirty read，写安全采用 RC 保护；不能变更名字做伪支持。
- [x] 本期不支持动态 GLOBAL/PERSIST 变更；SQL 明确拒绝，global introspection 只读 RR。
- [x] SERIALIZABLE long txn 普通读加共享+谓词保护；无法证明安全 shape 在读前拒绝。
- [x] RC 锁 gap 放宽以正确性优先，不能安全完成的 MySQL 行为差异写进文档。
- [x] RU 复制拓扑可先显式 Unsupported，standalone 提供功能，但不得宣称复制 RU 已支持。
- [x] 不授权新 WAL/Raft/磁盘格式；需要时另立 B03.1 提案。

## Blocking engineering questions (answer with measured evidence)

- [ ] **Q01** `storageengine.Txn` 旧 Go API 的 Snapshot 语义是否必须完全不变？方案将新 SQL level 隔离为 optional capability；审查全部调用方后定稿。
- [ ] **Q02** RR lazy view 的 pin 与 B01 root register 的 `readTS` 是否可以分离且不改变三类 root commit 行为？需要测试证明。
- [ ] **Q03** RC per-statement Head 捕获时点相对 Raft Barrier/statement physical bind/stream Open 应在哪里？必须单语句一份并能正确 release。
- [ ] **Q04** B02 observed baseline 是否已有 replicated apply 的安全实现？检查 HEAD 源码与 T055 实测，拒绝不能证明的路径。
- [ ] **Q05** RU 完整 merged stage 如何成为另一个 root 可安全采样的原子 index？root/savepoint/epoch/GC 的锁序有无死锁？
- [ ] **Q06** SERIALIZABLE 对 JOIN/聚合/CTE/索引等 shape 的本期实际支持矩阵和错误文案如何确定？必须在代码前承认缺口。
- [ ] **Q07** 是否允许在 RC 中保留超出 MySQL 的保守 gap 锁？建议可接受但作为 KNOWN DIFFERENCE；性能基线记录。
- [ ] **Q08** 在 autocommit=0 `SET SESSION`/隐式事务、prepared 及连接 reset 上如何触发 pendingNext 消费？须端到端定位。
- [ ] **Q09** RU staged index 内存/对象数默认预算如何由 B02 并发/大事务基线测量后校准？禁止空想阈值。
- [ ] **Q10** 同一事务 READ COMMITTED 的合法 current update 是否在旧 `latest>readTS` 上误拒绝？用红测和具体 baselines 决策。

## Resolution convention

每条 Q 后续添加 `Decision / Alternatives / Evidence / Affected FR / Updated docs / Approved at HEAD`，不要在 code review 时静默修改 specs。若需要改变已 adopted default，先修 spec、plan、research、contracts 和 traceability，再请求实施授权。
