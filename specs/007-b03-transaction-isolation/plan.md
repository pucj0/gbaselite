# Implementation Plan: B03 Four Transaction Isolation Levels

**Branch**: `master` (Baseline Commit `a7e80a42e3fdf6ba01e996c9a18559109650c67f`)
**Date**: 2026-10-16
**Spec**: [`spec.md`](spec.md)
**Status**: IMPLEMENTED & VERIFIED — Release Candidate Audit (Phase 9 Complete)

## Summary

在不破坏 B01 持久化与 B02 行/间隙锁核心合同的前提下，为 SQL 层建立 `sessionDefault → pendingNext → active` 事务隔离状态机；以 `storageengine` 可选能力提供 `StatementReadView` / `TransactionReadView` / `UncommittedView`；RC 每语句、RR 首次一致读、RU standalone staged dirty read、SER root-held S/predicate locks。任何无法证明安全的 SQL query shape 或拓扑模式明确拒绝而不是伪称兼容。

## Technical Context

| Attribute | Current / target |
|---|---|
| Language | Go，准确版本以实施时 `go.mod` 为准 |
| Storage | mvcc + bbolt，stage/write buffer，publication marker，local WAL，optional Raft |
| SQL | 自有 `parser`/AST、`executor`、`physical`，MySQL text/binary protocol in `server` |
| Lock | B02 `lockmanager` + `storageengine` optional LockingTxn (S/X, Gap, Next-Key, II, fair queue) |
| Testing | Go `testing`，固定 channel/barrier 并发 gate，MySQL 8.4 双连接对拍 |
| Platforms | 现有 Windows/Linux 构建；无需新桌面组件 |
| Constraints | 不迁移数据、不改 WAL/Raft 格式、不触碰 data/，不得全局互斥代替锁管理 |
| Performance | 语句级 view 不随访问每行新增 Head/Barrier；baseline 与 B02 比较，无拍脑袋 benchmark 通过值 |
| Resource limits | 视图数、staged dirty-read 索引字节、锁/GC 水位均 bounded；禁止无限期 pin |

## Constitution Check

| Principle | Check |
|---|---|
| I Accurate Compatibility | 提供 SET/SHOW 真值和 README/SQL 兼容矩阵；不声称完整 InnoDB |
| II Preserve Transaction | A05 parent-read/child-write、B01 三类 commit root 和基线验证、B02 current read 无回归；受控替换 B01 的“root 开始即固定 RR 视图”，只适用于已授权 B03 新语义 |
| III Backend & Resources | capability 在 `storageengine`，executor 无 mvcc import，计量/释放每一种 read view |
| IV Verifiability | 先红后绿测试、100 轮确定性调度、race/vet/gofmt/all tests + 对拍；文档与真实运行证据分开 |
| V Authorized Phases | 本交付文档包；后续任何 Phase 的代码变更需要新的明确授权 |

## Project Structure

```text
specs/007-b03-transaction-isolation/
├── spec.md
├── plan.md
├── research.md
├── data-model.md
├── quickstart.md
├── test-matrix.md
├── tasks.md
├── traceability.md
├── workflow.md
├── contracts/
│   ├── sql-isolation.md
│   ├── snapshot-lifecycle.md
│   ├── validation-policy.md
│   ├── lock-policy.md
│   └── dirty-read.md
└── checklists/
    ├── requirements.md
    └── clarification.md
```

**Code touchpoint inventory** (候选，须以实施时 source 为准):

```text
parser/{ast.go,parser.go,parser_test.go}
executor/{executor.go,transaction_engine.go,session_settings.go,locking_read.go,
  locking_modify.go,dml_locking.go,physical_select.go,subquery_mvcc.go,...}
server/{handler.go,session_settings.go,mysql_server.go,prepared.go,...}
storageengine/{engine.go, [new] isolation.go}
storageengine/mvccadapter/{adapter.go,iterator.go, [new] isolation.go}
mvcc/{transaction.go,transaction_manager.go,conflict.go,locking.go,current_read.go,
  store.go,range.go,maintenance.go,transaction_state.go, [new] isolation.go}
lockmanager/{types.go,resource.go,manager.go,interval.go,...}
config/{config.go,config_test.go} (only if explicit budget setting adopted)
README.md + docs/使用文档/{事务与存储说明.md,SQL兼容性参考.md}
```

## Execution phases and gates

| Phase | Work | Gate before next phase |
|---|---|---|
| 1 Foundation audit | B02 HEAD/真实运行基线、接口、读写路径/锁计划审计，先写将失败的核心测试 | P1：readTS、validation、GC、stage 路径得到签署决策 |
| 2 SQL/state | Isolation enum+parser+server+session/next state，变量真实回显 | 设置作用域、reset、隐式/显式事务全通过；未实现级别不能假 OK |
| 3 Read-view foundation | neutral optional API、view pin/GC/epoch、语句生命周期 | B01/A05 不退化；根/child 读视图保持；取消无泄漏 |
| 4 RR | lazy first read、own writes、普通/锁定读分离 | RR 三类首读时机/幻读/历史回归全通过 |
| 5 RC | 每语句 view、operator/subquery、DML validation 与锁差异 | RC 连读变化、单语句一致、写冲突和 safe lock matrix |
| 6 SERIALIZABLE | S+predicate 计划、复杂 shape 拒绝/扩展、死锁恢复 | 记录/范围/空区间/write-skew，锁策略安全 |
| 7 RU | atomic complete staged dirty-read standalone、budget、fail closed replication | 三类 dirty read + rollback + no dirty write + no leakage |
| 8 Integration & parity | text/prepared/protocol、GC/Raft/restore、100 次调度、MySQL 对拍 | 严格通过 all tests + race+vet+data unchanged |
| 9 Documentation & release candidate | README + traceability + reports；审计并回归 | 不发布、不部署，直到用户另行授权 |

### Dependency graph

`P1 → P2 → P3 → P4 → P5 → (P6 & P7: 分阶段可独立开展，均依赖 P5) → P8 → P9`。
**RU 不可由 RC “更宽松无锁”代替**，单独 gate 完成。SER 依赖 B02 完整锁计划而不是只依赖 RR 快照。

## Architecture decisions requiring Phase 1 proof

1. **Root Begin**: 现有 Engine.Begin/Tx.Snapshot 对旧 Go API 的语义保持；如何让 SQL root 的 RC statement view/RR lazy 分离，建议 optional `BeginIsolated(level)` + read view token，而不修改所有通用 Txn 方法的含义。
2. **Retention**: 固定 root `ReadTS` 对 RR lazy/RC 的 ref-count 策略升级。明确根/子/语句 pin ownership 和 GC horizon；无任何游标跨加锁等待持视图。
3. **Commit validation**: B02 observed per-key baseline、B01 root readTS validator、RC epoch-specific writes 的相互作用，详见 validation-policy；本阶段禁止仅凭“换 Snapshot”实现。
4. **RU staged visibility**: 需要跨 owner 只读 stage publish 层；控制 stage 原子性/事务级写一致/内存预算/独立执行模式；不可访问未合并 child 和内存中部分写集。
5. **SER shape**: 逐物理计划列出支持形状、predicates/keys/intervals、fallback；无法证明时拒绝；不许在获得完整保护前向外返回行。

## Complexity Tracking

| Tradeoff | Chosen reason | Rejected alternative |
|---|---|---|
| Optional isolation capability | 保持 `storageengine.Engine` 和 `Txn` 源兼容 | 把 isolation 类型塞进所有既有 Txn 方法 |
| Explicit read view token | 解决 RC statement head、pin 与 streaming 生命周期 | 每行取 `Head()` / 每个子查询重新建 view |
| Per-key validation evidence | 保持写安全并允许合法 current update | 把 root `Snapshot=latest` 导致 lost update |
| RU stable staged observer | 真实支持 dirty read 且跨 root 可观测 | 将普通 committed view 命名为 RU |
| SER fail-closed unsupported shapes | 防止写偏斜/幻读的虚假保证 | 仅靠 RC/RR MVCC 快照/表 IS 锁 |

## Delivery rules

- 仅在获用户明确授权后开始当前 Phase。每轮交付列本轮 TIDs、真实命令及退出码、关联测试、文件 diff、风险；不能直接跨 Phase。
- Go 源变更同批更新 README；此前 README 的待办信息与实现中状态清晰标识。
- 不因历史 B02 `tasks.md` 已勾选便跳过 B02 当前 HEAD 的独立 baseline。
- 不对 data/ 进行写/迁移/部署；所有集成测试 temp dir；禁止自动发布 Release。

---

## B03 实施收敛与决议闭环 (Q01–Q10 Final Status)

在 Phase 2 至 Phase 9 各阶段实施过程中，Q01–Q10 核心架构决策均已严格落地闭环：

1. **Q01 (旧 Go API 保持不变)**: `storageengine/isolation.go` 新增 `IsolationEngine` / `IsolationTxn` / `StatementView` 扩展接口，旧 API 语义 100% 保持；
2. **Q02 (RR lazy view 解耦)**: `mvcc/transaction.go` 延迟到首个普通 SELECT 建立 ViewToken 并 pin，首读前 Guard 仍受起始 TS 保护；
3. **Q03 (RC per-statement 捕获)**: 单条语句所有算子（Scan/Join/Subquery/CTE）共享一个 `StatementView`，语句结束在 defer 中安全归还；
4. **Q04 (B02 baseline 复制限制)**: 维持本地单机校验，复制集群下不支持未共识 baseline 写入，严格 Fail-Closed 保证集群一致性；
5. **Q05 (RU 跨事务安全脏读)**: `mvcc/dirty_registry.go` 发布只读快照镜像，并发 Reader 深拷贝采样，杜绝 Go map 竞态 panic；
6. **Q06 (SER 复杂形态 Fail-Closed)**: 单表加锁保护，多表 JOIN/CTE/视图/集合/聚合直接返回 Error 3108 Fail-Closed，拒绝隐式降级；
7. **Q07 (RC 保守 Gap 锁)**: 唯一键与外键校验保留必要 Gap 锁，优先保证数据完整性；
8. **Q08 (pendingNext 消费时机)**: 在显式 BEGIN、隐式事务首条 DML 及 autocommit=1 真正启动点统一消费，纯元数据不误消费；
9. **Q09 (RU 内存配额硬限制)**: 根事务最大 10,000 键、16MB 内存，全局最大 64MB，超限返回 `storageengine.ErrViewBudgetLimit`，杜绝 OOM；
10. **Q10 (RC 合规写 Baseline 校验)**: 锁下观测最新版本并更新 baseline，提交对比 `latest > baseline`，消除假冲突与丢失更新。
