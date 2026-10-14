# B03 Requirements Quality Checklist

**Review mode**: 本清单评估文档是否可执行；勾选不代表软件已实现。  
**Status**: Draft ready for review; implementation/tests NOT RUN.

- [x] 描述了用户目标、默认 RR 和全部四级，并将 SQL Scope 与 transaction state 分开。
- [x] 列出 RU/RC/RR/SER 对 dirty/nonrepeatable/phantom 的精确可观察差异。
- [x] 规范 RC 单语句一致性 vs RR 首个一致性读，而非将快照时间写成普通 BEGIN。
- [x] 分离普通快照读和 B02 FOR UPDATE/SHARE 当前读。
- [x] 披露 B01 fixed readTS、B02 per-key baseline、GC retention 和 A05 child 的结构冲突。
- [x] SERIALIZABLE 包含空范围谓词保护、写偏斜与复杂 shape fail-closed。
- [x] RU 要求真实脏读且禁止脏写、复制未支持不伪称可用。
- [x] 提供不改变最小 storageengine API / durable schema 的明确边界。
- [x] 每类异常都有确定性并发、回滚和语法状态负测试。
- [x] README/协议/不同拓扑/资源泄漏/GC/复制都有验收 gate。
- [x] Phase 按依赖、先红后绿测试，任务全部初始未勾选。
- [ ] Q01–Q10 的代码级阻塞问题关闭并同步设计文件（须下一轮）。
- [ ] 运行基础 `go test ./...`/`go vet ./...` 并记录真实证据（仅实施授权后）。
- [ ] 每项 FR/SC 对应的真实测试和代码证据齐备（仅实施完成后）。
- [ ] MySQL 8.4 对拍/100 次调度/race 运行结果审计通过（仅实施完成后）。

**Review outcome**: 文档结构可进入人工设计审查；**不是** B03 实现验收。
