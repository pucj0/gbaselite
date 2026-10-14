# B03 Quickstart — 文档安装、Spec Kit 工作流、验收演示

> 现在的交付是 Spec Kit 文档包，未运行测试、未推送到 GitHub、未执行生产代码。文档设计的候选接口不是已经实现的功能。

## Install documentation locally (repository root)

把 ZIP **解压到仓库根目录**（ZIP 内已包含 `specs/007-b03-transaction-isolation/...`），确认不覆盖已有 `specs/006-b02-row-lock-manager` 或 `.specify` 文件。推荐新建 feature 分支：

```bash
git checkout master
git pull --ff-only
git switch -c 007-b03-transaction-isolation
# 解压文件到仓库根目录（不要多套一层 b03_pack）
git status --short
git add specs/007-b03-transaction-isolation
# 若将 ZIP 根目录 B03-README.md 也复制了，可选择 add 它；不影响 speckit
git commit -m "docs: plan B03 transaction isolation levels"
git push -u origin 007-b03-transaction-isolation
```

PowerShell（Windows）解压示例：

```powershell
Expand-Archive -LiteralPath .\GBaseLite-B03-SpecKit.zip -DestinationPath . -Force
git status --short
```

注意：请在干净目录执行，包内 `B03-README.md` 是离线包使用说明，不需要放进仓库也可直接删除。

## Spec Kit order (repo has .specify/ installed)

当前仓库已包含 `.specify/templates`、`.specify/scripts/powershell`、Constitution、AGENTS 与 B02 的完整文档。本包遵循其规格路径，不会重新初始化或覆盖它们。通常工作流是：

1. `speckit.specify` 确认用户场景与验收；当前已有 `spec.md` 草案。
2. `speckit.clarify` 关闭 checklists/clarification 的关键歧义（不能靠任意默认把不可实现的 RU 标为完成）。
3. `speckit.plan` 阅读 `plan.md`/`research.md`/`data-model.md` 和 contracts；当前已有设计草案，不要让工具不经审查覆盖。
4. `speckit.tasks` 阅读 `tasks.md`，映射 FR/SC/测试，检查任务粒度及依赖；当前已有**未执行**任务列表。
5. `speckit.analyze` 审核文档一致性与完整性（只读）。
6. **仅用户明确授权某 Phase 后** `speckit.implement` 或向 Codex/AI 投递对应任务提示词，按序执行并回传实测证据。

你的 Codex 集成/命令语法要以仓库现有 `.specify/integrations` 和 agent 实际暴露的命令名称为准；上面为典型 Spec Kit 工作流标识，不保证每个终端均有同名全局 shell 命令。

## First dry review (no code)

- 对照当前 `master` HEAD，逐项验证 `research.md` 代码观察仍成立。
- 核对 `contracts/validation-policy.md` 与 `mvcc/conflict.go` + B02 per-key observation；没有证明不要开始 RC/RU。
- 用 `test-matrix.md` 的 M08/M13/M23/M29 确认目标行为存在可控测试方案。
- 审核未支持 SQL 形状/复制 RU 的错误必须清晰，并与 README 后续说明一致。
- `tasks.md` 全部初始 `[ ]`；只能凭真实测试+代码证据逐项勾选。

## Post-implementation SQL demonstration (only after authorized implementation)

```sql
SELECT @@session.transaction_isolation; -- REPEATABLE-READ
SET SESSION TRANSACTION ISOLATION LEVEL READ COMMITTED;
SELECT @@session.transaction_isolation; -- READ-COMMITTED
SET TRANSACTION ISOLATION LEVEL SERIALIZABLE;
START TRANSACTION;
SELECT * FROM t WHERE id BETWEEN 1 AND 100; -- shared + predicate protection
COMMIT;
START TRANSACTION; -- now RC
SELECT * FROM t;
COMMIT;
```

真实异常现象与双连接运行顺序详见 `test-matrix.md`；SQL 片段只是目标示例，不是已成功运行的记录。
