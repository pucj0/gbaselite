# B02 Quickstart / Validation Guide

**Status**: Phase 1（Setup）与 Phase 2（Foundational）已实现并通过验证（`go test ./...` / `go vet ./...`
均退出 0）。以下 B02 SQL 与端到端场景是**后续阶段的**验收入口，当前**不可**据此宣称行锁、
Current Read、Gap/Next-Key 或锁定 SELECT 功能存在——它们尚未实现。证据见
`.tmp/b02-validation.md`。继续实施需要用户对相应 Phase 的明确授权。

## 1. Planning workflow
官方 Spec Kit v1.1.3 已安装至 `.agents/skills/speckit-*`，基础设施位于 `.specify/`。
Codex skills 形式对应原 `/speckit.*`：
```text
$speckit-specify  B02 InnoDB-style Row Lock Manager（使用原完整用户要求）
$speckit-clarify
$speckit-plan
$speckit-tasks
$speckit-analyze
```
这些是coding agent skills，不是终端子命令。真实执行记录见workflow.md。
本地 CLI：`.tmp/spec-kit-venv/Scripts/specify.exe`；`.tmp`清理后skills/模板仍存在，CLI需重新安装。
重建使用官方来源（有网络、uv环境时）：
```powershell
uv tool install specify-cli --from git+https://github.com/github/spec-kit.git@v1.1.3
specify init --here --force --non-interactive --integration codex --integration-options='--skills' --script ps --ignore-agent-tools
specify integration status
```
刷新前核对现有受管文件，避免覆盖自定义内容；`.agents`保持现有gitignore，不公开其他个人技能。
新安装技能将在下一轮自动发现；本轮通过直接读取安装后的SKILL.md执行。

## 2. Implementation verification (future only)
在隔离临时数据库和临时测试目录内运行；不得使用data/或当前服务。所有新fixture使用t.TempDir，
成功/失败/cancel都清理；large transaction需要足够的已记录lock预算，不得跳过B01/A05测试。
```powershell
$env:GOCACHE='E:\code\gbaselite\.tmp\gocache'
$env:GOPATH='E:\code\gbaselite\.tmp\gopath'
gofmt -l lockmanager storageengine mvcc executor parser config server replication enginefactory cmd
go test ./lockmanager ./storageengine/... ./mvcc ./executor ./parser ./config ./server ./replication -count=1
go test ./... -count=1
go vet ./...
```
修改Go文件必须先gofmt；-l无输出、测试/vet退出0。B02 targeted tests统一TestB02前缀：
```powershell
go test ./lockmanager ./mvcc ./executor ./replication -run TestB02 -count=100
go test -race ./lockmanager ./mvcc ./storageengine/... ./executor ./replication -run TestB02 -count=10
```
当前 `./lockmanager` 与 `./storageengine` 已有 TestB02 用例（26 个），其余包待后续 phase 增加。
race在支持平台/CI运行；Windows未配置race工具链时保留限制并在支持平台完成，不能伪报通过。
go test ./...保留现有B01/A05、WAL、GC、savepoint32层与staging资源用例。

## 3. Deterministic two-session scenarios
以下SQL由未来测试harness的channel/hooks协调。不要手工sleep作为验收因果证据。
Fixture：`t(id INT PRIMARY KEY, u INT UNIQUE, v INT)`，初始(10,10,1)、(20,20,1)。

| 场景 | 明确调度 | 预期 |
|---|---|---|
| Record | T1 FOR UPDATE id=10，hook确认granted；T2 UPDATE id=10 hook确认queued；T1ROLLBACK | T2等锁后完成或因原readTS过期冲突；id=20可并行 |
| Shared | T1/T2 FOR SHARE id=10均granted；T3UPDATE进入queue | S/S并行，X等待至两root结束 |
| Gap | T1 FOR UPDATE WHERE id>10 AND id<20；T2INSERT15queued；T3INSERT25 | 15等待，25推进；具体fallback范围须符合lockplan |
| Insert intention | 无Gap，T1INSERT12、T2INSERT18各自granted | 不同插入位置并行；同PK/UNIQUE只有一个胜者 |
| Missing point | T1 FOR SHARE WHERE id=15无结果；T2INSERT15 | 空结果仍保护插入位置 |
| Current vs snapshot | T1BEGIN并SELECTv=1；T2UPDATEv=2 COMMIT；T1FOR UPDATE；T1普通SELECT | 锁定值2，普通值1；readTS/horizon不变；T1UPDATE冲突需root重试 |
| Deadlock | T1锁10、T2锁20；T1请求20queued；T2请求10 | 后注册root为victim1213，另一root推进 |
| Timeout | T1持锁；T2期限50s入队；fakeclock推进至deadline | 1205、statement回滚，显式T2root可继续、已授予锁保留 |
| Savepoint | T1获锁→SAVEPOINT→新增锁→ROLLBACK TO/RELEASE | grants均root持有；pendingdiscard取消；rootROLLBACK后全零 |
| Epoch | waiterqueued后restore/leaderloss | waiter醒、oldtokens无效；不能影响新epoch |

排序/legacy编码可能fallbackwhole-domain，此时验收按EXPLAIN范围而非凭SQL值猜锁范围。
NEXT-KEY端点、Gap/Gap、II/II、upgrade与queueedges见lock-manager契约。

## 4. Durability and diagnostics
同fixtures参数化bounded、group、stream、local-WAL、Raft；assert原readTSvalidator仍生效。
故障hook分别暂停“versionsinstalled未published”“published后cancel”“termchange”，验证no dirty
read、definiteCOMMITTED、锁释放与原格式replay。Current-only/guard-only/data-write三类root的Head与
HasCommitTS完全按B01判断。
管理员运行plannedSHOWENGINELOCKS/LOCKWAITS/DEADLOCKS，核对root/scope/opaqueResource/edges。
非管理员拒绝；257条请求截断可见；不出现rawkeys、rows、SQL；root终态全部current计数零。

## 5. Acceptance evidence
traceability.md每条FR/SC有design、implementation、test task。未来保留命令退出码、
fixture参数、预算与100次调度结果至`.tmp/b02-validation.md`；不得勾选没有真实执行的任务。
发布前复核完整README、无格式/协议diff、gofmt/test/vet/race与critical/high问题为零。
本轮不build/deploy，也不执行服务stop/replace/start。
