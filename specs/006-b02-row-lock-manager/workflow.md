# B02 Spec Kit Workflow Evidence

**Date**: 2026-10-10（文档规划轮）+ 本轮（Phase 1 + Phase 2 实现轮）。
用户授权：先安装 Spec Kit 及 B02 文档规划；本轮明确授权执行 T001–T012。
源码基线：`140838c2d4f10971c010a9e4d643fd4b135634f2`，Git仍为master（未新建分支）。

## 本轮（Phase 1 + Phase 2）授权与变更边界

用户要求「开始执行 GBaseLite B02 的第一阶段：项目基线与架构准备」，逐项列出执行要求
（识别 Phase/任务编号、检查 B01/A05、梳理调用路径、确认现状行为、建立测试基线、
完成必要的接口契约测试与测试辅助设施、不得提前实现 Record/Gap/Next-Key/SQL 加锁、
发现不符先修订文档或报告阻断）。经 `ask_user_question` 二次确认范围为
**「Phase 1 + Phase 2（T001–T012）」**，并授权把每 root 组件预算由 65536 上调至 262144。

**已完成**：T001–T012（12/72）。
**新增文件**（11 个，全部为新增，无既有文件修改）：
```
storageengine/locking.go             storageengine/locking_contract_test.go
lockmanager/types.go                 lockmanager/resource.go
lockmanager/budget.go                lockmanager/manager.go
lockmanager/clock.go                 lockmanager/resource_test.go
lockmanager/budget_test.go           lockmanager/test_helpers_test.go
```
**文档修订**：`plan.md`、`research.md`、`data-model.md`、`contracts/lock-manager.md`（预算校准）、
`tasks.md`（勾选 + 执行注记）、`traceability.md`（执行映射 + 一致性核对附录）、
`spec.md`/`quickstart.md`/`workflow.md`（状态）。
**未做**：任何 Record/Gap/Next-Key/兼容矩阵/队列/死锁/诊断/SQL 加锁行为；未改
`mvcc`/`executor`/`parser`/`replication`/`config`/`cmd`/`server`；未改 `go.mod`/`go.sum`；
未改持久化格式、WAL framing、Raft 载荷；未接触 `data/`；未 build/部署。

**零改动是哈希证明**：对 `mvcc`、`storageengine`、`executor`、`replication`、`parser`、`config`、
`cmd`、`sqllayout`、`storage`、`server`、`enginefactory` 下 354 个既有生产 Go 文件逐个重算
SHA-256 并与 T003 基线比对：**ADDED 2 / CHANGED 0 / MISSING 0**（新增即
`storageengine/locking.go` 与其契约测试）。

**验证**：`gofmt -l` 无输出；`go build ./...` 退出 0；`go test ./... -count=1` 退出 0
（22 个有测试的包全 ok，含新增 `gbaselite/lockmanager`）；`go vet ./...` 退出 0。
证据 `.tmp/b02-validation.md`。

## Installation

官方github/spec-kit发布v1.1.3源码经isolated venv安装；不是自创的同名skill。
CLI `.tmp/spec-kit-venv/Scripts/specify.exe`，Codex官方skill位于`.agents/skills/speckit-*`。

```powershell
.tmp/spec-kit-venv/Scripts/python.exe -m pip install https://github.com/github/spec-kit/archive/refs/tags/v1.1.3.zip
.tmp/spec-kit-venv/Scripts/specify.exe init --here --force --non-interactive --integration codex --integration-options='--skills' --script ps --ignore-agent-tools
.tmp/spec-kit-venv/Scripts/specify.exe integration status
```

先在`.tmp/spec-kit-stage`检查产物，再初始化项目。初始化前后AGENTS.md与两个既有release
skill的hash一致。integration status结果OK：managed modified/missing/invalid/unchecked均0。
`.agents/`与`.codex/`沿仓库现有ignore规则，不强行加入版本控制；`.specify/`工作流可随文档审阅。
新skill通常由下一轮/会话加载，本轮已直接读取官方SKILL.md并按其指令执行。

## Document stages

| 官方工作流 | 已执行的文档活动 / 证据 |
|---|---|
| speckit-constitution | 从AGENTS/B01/A05/用户范围填充官方constitution template，1.0.0；无新授权 |
| speckit-specify | resolve-template，按现有001..005选006 feature；feature.json指向独立目录；spec与requirements16项审核 |
| speckit-clarify | check-prerequisites -Json -PathsOnly；已按分类复核，0问答，无伪造确认；见checklists/clarification.md |
| speckit-plan | setup-plan.ps1 -Json；源码研究、research/model/contracts/quickstart、两次constitution设计gate |
| speckit-tasks | setup-tasks.ps1 -Json并读取tasks-template；72项、9阶段、文件/依赖/验收/必做测试 |
| speckit-analyze | 全文档完成后按下述命令进入严格只读审核；结果返回会话，不在analyze中写文件 |

plan工作流明确要求研究agent，因此派发两个只读研究任务分别检查commit/current和锁/SQL接入。
两个研究任务都未编辑源码；源证据、可选方案与决策已由主agent整合进research.md。
无extensions.yml，所有阶段的before/after hooks按官方skill跳过。

```powershell
.specify/scripts/powershell/check-prerequisites.ps1 -Json -RequireSpec -RequireTasks -IncludeTasks
```

analyze核对32 FR/8 SC的design、实施与测试映射、72任务格式/顺序、可选后端边界及
constitution。分析是文档审核，既不是实施任务完成，也不是Go测试结果。

## Authorized change boundary

### 2026-10-10 文档规划轮
安装官方规划工具，新增`.specify/`和本feature文档、README规划状态说明。
未执行speckit-implement；未编辑生产Go/配置/WAL/Raft/二进制；未打开业务data/。
未运行Go runtime tests、build或deployment，因为该轮未改Go；tasks/quickstart中的命令当时均为
未来授权后的验证要求。全部implementation checkboxes当时保持未勾选。
记录测试设计不构成声称已运行测试；未定预算需T001/T004实测校准后再实施。

### 本轮（Phase 1 + Phase 2 实现轮）
用户明确授权后执行 T001–T012，边界见本文件顶部「本轮（Phase 1 + Phase 2）授权与变更边界」。
本轮**已**运行 `gofmt`/`go build`/`go test ./...`/`go vet ./...`，因为本轮新增了 Go 代码（新包
`lockmanager` 与 `storageengine/locking.go`）。仅勾选有真实证据的 12 项；其余 60 项保持未勾选。
预算默认值已由 T004 用实测校准（65536 → 262144），并同步四份文档；README 的限制矩阵登记
留至 T070（本轮无对外行为/SQL/CLI/持久化变更，但新增的锁资源错误属于新限制，已在
`.tmp/b02-resource-calibration.md` §3.3 与 tasks.md T004 记录为待办）。

## References

安装来源：[官方Spec Kit](https://github.com/github/spec-kit/tree/v1.1.3)。
feature文档入口：[spec](spec.md)、[plan](plan.md)、[tasks](tasks.md)、[traceability](traceability.md)。
后续变更须先获得用户明确实现指令；不以文档审核通过替代授权。
