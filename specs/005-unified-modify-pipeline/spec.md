# 功能规格：统一写入算子 Modify Pipeline

**功能分支**：`005-unified-modify-pipeline`
**创建日期**：2026-09-18
**状态**：Draft
**输入需求**：统一 INSERT / UPDATE / DELETE 写入模型。查询管线负责输出目标行，随后统一由 InsertOperator / UpdateOperator / DeleteOperator 调用事务写入；优先完成 INSERT SELECT、UPDATE JOIN、多表 DELETE 的统一。

------

## 用户场景与测试

### 用户故事 1 — 统一 INSERT 执行路径（优先级：P1）

作为数据库使用者，我希望 `INSERT VALUES` 和 `INSERT SELECT` 使用同一套 INSERT 写入机制，从而使不同数据来源下的 INSERT 行为保持一致。

**优先级原因**：

INSERT 是当前三个 DML 家族中最容易统一的部分，并且现有 `INSERT SELECT` 已经具备较完整的：

```text
Query Pipeline
→ physical.Modify
→ writeInsertedRow
```

结构，适合作为统一 Modify Pipeline 的第一阶段。

**独立测试方式**：

只完成该阶段后，应能够独立运行：

- INSERT VALUES
- INSERT SELECT
- INSERT SELECT + UNION ALL
- DEFAULT
- auto_increment
- explicit auto_increment
- self-reference INSERT SELECT
- duplicate / constraint failure
- LastInsertID
- statement rollback

相关回归测试，并保持原有行为。

**验收场景**：

1. **Given** 一个合法的 `INSERT VALUES`，**When** 执行插入，**Then** 所有输入行均通过统一 InsertOperator 写入。
2. **Given** 一个 `INSERT SELECT`，**When** SELECT 持续产生结果，**Then** 结果可以逐行进入统一 InsertOperator，而不需要先转换为完整 `Result.Rows`。
3. **Given** `INSERT INTO t SELECT ... FROM t`，**When** 执行语句，**Then** SELECT 只能看到 statement 开始时的读取快照，不得读取本语句刚写入的新行。
4. **Given** 前几行插入成功但后续一行违反约束，**When** statement 失败，**Then** 本 statement 已产生的全部行修改必须回滚。
5. **Given** 插入过程中产生 auto_increment ID，**When** statement 成功提交，**Then** session 才能看到正确的首个生成 ID。
6. **Given** statement 在保留 auto_increment ID 后失败，**When** 回滚，**Then** 数据不得残留，但允许 ID 出现间隙。

------

### 用户故事 2 — 统一 UPDATE 执行路径（优先级：P2）

作为数据库使用者，我希望普通 UPDATE 与 UPDATE JOIN 使用同一个 UpdateOperator，从而统一目标行身份、表达式计算、约束、索引、事务与 affected rows 行为。

**优先级原因**：

UPDATE JOIN 是当前最明显仍包含专用 mutation 流程的能力之一。

完成该阶段可以证明：

```text
Join Query
→ Target Candidate
→ Target Dedup
→ UpdateOperator
```

这一模型能够替代当前针对 UPDATE JOIN 的特殊 MVCC 写入流程。

**独立测试方式**：

独立运行普通 UPDATE 与 UPDATE JOIN 测试，包括：

- 单表 UPDATE
- UPDATE JOIN
- 一个 target 匹配多个 source
- first-match
- sequential SET
- UPDATE LIMIT
- indexed-column update
- primary-key update
- Halloween protection
- CHECK / UNIQUE / FK failure
- transaction rollback

**验收场景**：

1. **Given** 普通 UPDATE，**When** 多行满足 WHERE，**Then** 每个目标物理行最多修改一次。

2. **Given** UPDATE JOIN 中一个 target 匹配多个 source，**When** 执行语句，**Then** target 只修改一次，并继续保持当前的 first-match 行为。

3. **Given**：

   ```sql
   UPDATE t SET a = a + 1, b = a;
   ```

   **When** 执行 UPDATE，**Then** 后一个 assignment 应看到前一个 assignment 已修改后的 `a`。

4. **Given** UPDATE 修改了用于扫描的索引列，**When** 新值仍满足扫描范围，**Then** 同一物理行不得再次被处理。

5. **Given** UPDATE JOIN 带 LIMIT 且一个 target 有多个 JOIN match，**When** 执行语句，**Then** LIMIT 统计的是唯一 target mutation 数量，而不是原始 JOIN row 数量。

6. **Given** 第 N 个 target 更新时发生约束失败，**When** statement 失败，**Then** 前 N-1 个修改必须一起回滚。

------

### 用户故事 3 — 统一 DELETE 执行路径（优先级：P3）

作为数据库使用者，我希望普通 DELETE 和多表 DELETE 使用统一 DeleteOperator，使所有 DELETE 共用物理目标行身份与最终删除逻辑。

**优先级原因**：

多表 DELETE 涉及：

- 多目标表；
- JOIN；
- 重复匹配；
- 无主键表；
- outer join；
- FK target ordering；

是三个 DML 中边界最多的一种，因此在 INSERT 和 UPDATE 模型稳定后迁移。

**独立测试方式**：

运行：

- single-table DELETE
- multi-table DELETE
- duplicate JOIN matches
- no-PK table
- identical-value rows
- LEFT JOIN
- RIGHT JOIN
- FK action
- FK rollback
- transaction read-your-write
- derived target rejection

相关测试。

**验收场景**：

1. **Given** 普通 DELETE，**When** WHERE 匹配目标行，**Then** 每个物理目标通过统一 DeleteOperator 删除。
2. **Given** 多表 DELETE 中一个物理行出现在多个 JOIN 组合中，**When** 执行语句，**Then** 该物理行只删除一次。
3. **Given** 不同目标表具有相同 storage key bytes，**When** 执行多表 DELETE，**Then** 两行仍然必须被视为两个不同目标。
4. **Given** 无主键表中存在两个值完全相同的物理行，**When** 仅其中一个被选中，**Then** 不得误删另一个。
5. **Given** outer join 产生 NULL-extension row，**When** 目标表实际上不存在对应物理行，**Then** 不得生成有效 DELETE target。
6. **Given** 多个 DELETE target 存在 FK 依赖，**When** 执行删除，**Then** 必须维持当前 child-before-parent 的安全执行顺序。
7. **Given** 多 DELETE target 形成当前实现无法安全处理的 FK cycle，**When** 执行语句，**Then** 继续维持现有拒绝行为。

------

### 用户故事 4 — 有界的目标行去重（优先级：P4）

作为 GBaseLite 的运行维护者，我希望 UPDATE JOIN 与多表 DELETE 的目标去重不会依赖无限增长的内存 map，从而保证大规模重复 JOIN 结果不会导致无界内存占用。

**优先级原因**：

当前多表 DELETE 使用类似：

```go
seen := map[string]bool{}
rows := []multiDeleteRow{}
```

的方式保存全部结果。

这在大结果集下属于无界内存结构。

A05 最终必须把 TargetRowDedup 建立为可控资源的通用操作。

**独立测试方式**：

构造大量重复 mutation candidates，使数量明显超过内存阈值，验证：

- 去重结果正确；
- first-match 保持；
- 能使用临时存储；
- success/error/cancel 后资源均正确释放。

**验收场景**：

1. 相同 target identity 出现多次时只输出第一次出现的 candidate。
2. target table 不同时，即使 storage key 相同也不得互相去重。
3. candidate 超过内存阈值时允许 spill 到临时文件。
4. upstream error、downstream error 和 context cancel 后临时文件必须释放。

------

# 边界情况

- 无主键表必须依赖物理 storage row identity，而不能使用 row values。
- 两条内容完全相同的 heap row 必须仍然是两个不同物理目标。
- Row Identity 必须包含 target table identity。
- UPDATE JOIN 重复匹配必须保持 first-match-wins。
- UPDATE LIMIT 必须位于 target dedup 之后。
- Outer Join 必须能够区别：
  - 一个真实存在、字段值为 NULL 的 target row；
  - 一个因为 outer join 补出的“目标不存在” row。
- UPDATE 修改 PK 或用于访问计划的索引列后，不得再次扫描到同一物理行。
- INSERT SELECT 不得读取本 statement 已插入的新数据。
- 多表 DELETE 可以在最终删除前存在 staging barrier。
- staging / dedup 中持有的 row/key 不得引用上游 Yield 生命周期结束后的临时内存。
- CHECK / FK / UNIQUE / PK / index 任一失败必须保证 statement 原子回滚。
- auto_increment reservation 可以在失败后留下 gap。
- LastInsertID 不得在 child transaction commit 之前发布到 session。
- Derived Table / CTE / View 如果无法产生有效物理目标身份，必须维持现有不可写行为。
- 可以在执行前判断的 binding/static error 应尽可能在第一条物理写入发生前返回。

------

# 需求

## 功能需求

- **FR-001**：系统必须为 INSERT、UPDATE、DELETE 提供统一的逻辑 Modify Pipeline。
- **FR-002**：查询阶段必须负责确定“哪些行需要被修改”，然后再进入 mutation operator。
- **FR-003**：INSERT VALUES 与 INSERT SELECT 必须共用最终 INSERT 写入实现。
- **FR-004**：普通 UPDATE 与 UPDATE JOIN 必须共用最终 UPDATE 写入实现。
- **FR-005**：普通 DELETE 与多表 DELETE 必须共用最终 DELETE 写入实现。
- **FR-006**：每个需要 UPDATE/DELETE 的物理目标行必须拥有稳定 Row Identity。
- **FR-007**：Row Identity 必须支持无主键表。
- **FR-008**：需要去重的 DML 中，相同物理 target 最多执行一次 mutation。
- **FR-009**：TargetRowDedup 必须稳定保留第一次出现的 candidate。
- **FR-010**：TargetRowDedup 不得要求结果集整体永久驻留内存。
- **FR-011**：DML source query 必须继续从 statement read snapshot 读取。
- **FR-012**：statement 失败后不得暴露部分 mutation。
- **FR-013**：必须保持现有 CHECK、FK、UNIQUE、PK、secondary index、auto_increment、ON UPDATE timestamp、affected rows、LastInsertID 行为。
- **FR-014**：UPDATE assignment 必须维持顺序求值。
- **FR-015**：UPDATE JOIN 必须维持 target 多次匹配时 first-match 行为。
- **FR-016**：UPDATE LIMIT 必须统计去重后的目标 mutation。
- **FR-017**：多表 DELETE 必须保持 FK-safe target ordering。
- **FR-018**：无主键表中值相同的不同物理 row 必须可以独立删除。
- **FR-019**：outer join 的 NULL-extension 不得生成虚假的 mutation target。
- **FR-020**：View / CTE / Derived Table 的可写范围不得因为 A05 被意外扩大。
- **FR-021**：除必须存在的全局语义 barrier 外，query output 应能够以 pipeline 方式消费。
- **FR-022**：单表 UPDATE/DELETE 不得因为 A05 退化为无条件全表扫描。
- **FR-023**：现有 A04 mutation regression tests 必须全部继续通过。
- **FR-024**：cancel/error 后 mutation pipeline 拥有的所有临时资源必须释放。
- **FR-025**：A05 完成后，INSERT SELECT、UPDATE JOIN、多表 DELETE 不得继续拥有独立的最终物理 row mutation 实现。

------

## 关键实体

### Mutation Candidate

Query Pipeline 输出的逻辑 mutation 输入。

### Target Row Identity

一个目标表中某一物理 row 的稳定身份。

### Insert Candidate

描述一个待插入 logical row 的输入。

### Update Candidate

包含：

- target identity；
- target old row；
- SET expression 所需 evaluation context。

### Delete Candidate

包含：

- target identity；
- old row。

### Modify Result

记录 statement mutation 结果，例如：

- affected rows；
- first generated ID。

### Target Deduplication

基于物理 target identity 去重，并稳定保留第一次出现结果的操作。

------

# 成功标准

- **SC-001**：现有 INSERT / UPDATE / DELETE / INSERT SELECT / UPDATE JOIN / multi-table DELETE MVCC regression tests 100% 通过。
- **SC-002**：INSERT VALUES 与 INSERT SELECT 最终调用同一套 INSERT mutation implementation。
- **SC-003**：普通 UPDATE 与 UPDATE JOIN 最终调用同一套 UPDATE mutation implementation。
- **SC-004**：普通 DELETE 与 multi-table DELETE 最终调用同一套 DELETE mutation implementation。
- **SC-005**：复杂 DML 专用代码中不再拥有独立的最终物理 row mutation 实现。
- **SC-006**：UPDATE JOIN 多 match target 仍然只修改一次并保留 first match。
- **SC-007**：Halloween regression 中同一物理 row 最多修改一次。
- **SC-008**：无主键 multi-delete 可以正确区别值相同的物理 row。
- **SC-009**：大量重复 candidate 测试不依赖无界内存 `seen` map。
- **SC-010**：任意中途 failure 后均看不到部分 statement 修改。
- **SC-011**：cancel/error 后没有遗留 mutation 临时资源。
- **SC-012**：已有 indexed UPDATE/DELETE 不因架构重构退化成强制 full scan。

------

# 假设

- A04 已确认的 SQL 行为作为 A05 的兼容基线。
- 当前 `storageengine.Txn` transaction snapshot 与 child transaction 语义保持不变。
- 首版继续复用现有 row write、constraint、index、FK 与 auto_increment 实现。
- A05 不新增 DML SQL 语法。
- A05 不负责实现通用 writable view / writable CTE / writable derived table。
- stable dedup、FK ordering 等全局语义允许使用有界或可 spill 的 staging。