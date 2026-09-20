# Research：TargetRowDedup 的 bounded / spillable stable dedup 基础设施

**功能**：005-unified-modify-pipeline（A05）
**阶段**：Phase 2（T007）
**日期**：2026-09-18
**结论**：现有 `physical.Distinct` 已经实现了 plan 需要的"两趟有界排序 + spill + ordinal"算法。A05 **不需要新写一套**，只需要把算法抽成共享内核，并让 `TargetRowDedup` 复用同一内核。

------

## 1. 调研对象

| 位置 | 作用 |
|---|---|
| `physical/distinct.go` | SQL DISTINCT 的两趟 stable dedup 算子 |
| `physical/operator.go` | `Sorter[T]`、`Sort[T]`、`Operator[T]`、`Yield[T]` 契约 |
| `executor/external_sort_query.go` | `Distinct` 的真实 sorter 工厂（把 `queryControl` 预算切成两半） |
| `executor/external_sort.go` | `externalRowSorter`：有界批次 + 私有临时 run + 多路归并 + `Close` 清理 |

------

## 2. 现有机制（可直接复用部分）

### 2.1 两趟算法

`physical.Distinct.Run` 的原始实现就是 plan 的 "Pass 1 / Pass 2"：

```text
Pass 1（NewSort(true)）：按 Key + Ordinal 排序
      → 每个 Key 组的第一条即"首次出现"，用 previous == Key 丢掉同组其余
Pass 2（NewSort(false)）：按 Ordinal 排序
      → 恢复输入顺序（stable first occurrence wins）
```

关键点：**没有 `seen map`**。去重靠"已排序数组里相邻 key 比较"，所以内存占用由 sorter 决定，而不是由候选数量决定。

### 2.2 ordinal 的语义

`Ordinal` 是**进入算子时的自增序号**，不是 key 的一部分。Pass 1 用它做组内 tie-break（保证"首次出现"就是组内第一条），Pass 2 用它恢复原序。任何"跳过某些输入行"的实现都必须让 ordinal 反映**进入内核之后**的顺序，否则 Pass 2 的输出顺序会和输入顺序不一致。

### 2.3 有界 / spill

`physical` 包**不含**任何 spill 实现（全包只 import `gbaselite/storageengine`，无 `os`）。spill 由 sorter 工厂注入：

```go
NewSort func(byKey bool) (Sorter[DistinctRow[T]], error)
```

`executor` 侧的真实工厂是 `bindDistinct`：深拷贝 `queryControl`，把 `SortMemoryBytes` 半分给两趟排序，并给出 64 KiB 下限，然后 `newExternalRowSorter`。`externalRowSorter` 提供：

- 批次超过 `memory/2` 即 `flush` 成私有临时 run（`os.CreateTemp` + `TempDirectory`）；
- run 数超过 64 就 `compact`（8 路归并）；
- 每次写入都走 `queryControl.reserveTemporary`，所以 `MaxTempBytes` 能真正拒绝；
- `Close` 只删除自己创建的路径，成功 / 上游错误 / 下游错误 / cancel 全部走同一条清理路径（`Distinct` 用 `defer errors.Join(err, sorter.Close())`）。

**结论**：Plan 中 "bounded sort + spill + ordinal" 三个要素在仓库里都已存在，A05 的复用方式是"泛化内核 + 注入工厂"，不是"在 physical 里实现 spill"。

------

## 3. Phase 2 实际采用的复用方式

1. **抽出内核**：`physical/stable_dedup.go` 定义 `dedup[T]`，参数为 `input`、`include`、`identity`、`newSort`、`yield`，函数体是原 `Distinct.Run` 的两趟逻辑 + 逐路径 `Close`。
2. **`Distinct` 改为委托**：`physical/distinct.go` 只保留 `DistinctRow` / `Distinct` 对外契约，内部把 `Sorter[DistinctRow[T]]` 适配成 `Sorter[dedupRow[T]]`，再调用 `dedup`。对外 API 与行为完全不变（`physical/distinct_test.go` 原测试未改一行，继续通过）。
3. **`TargetRowDedup` 复用同一内核**：`physical/target_dedup.go` 注入 sorter 工厂并把候选投影为"key + 候选"，`include` 丢弃没有物理 provenance 的候选。
4. **dedup key 类型统一为 `string`**：内核需要 `==` 才能丢弃同 key 组，而 `[]byte` 不满足 `comparable`。`Distinct` 原本就用 `string`，因此 `TargetRowDedup` 也用 `string`，由 `RowIdentity.StorageKey()` 产出字节后转 `string`。

------

## 4. 与 plan.md 的差异（需记录）

| 项 | plan 建议 | 实际实现 | 原因 |
|---|---|---|---|
| `RowIdentity` 所在包 | `executor/` | `physical/`（`executor` 提供别名） | plan 的 `TargetRowDedup` 签名是 `Identity func(T) (RowIdentity, bool)`；若 identity 在 executor，physical 无法引用它，除非 executor 反向依赖被判为"稳定去重基础设施"的算子。把身份模型放到消费它的算子旁，`executor` 用 alias 保持调用侧写法不变。 |
| 文件切分 | `physical/target_dedup.go` 单文件 | 另有 `physical/row_identity.go`、`physical/stable_dedup.go` | 身份模型、共享内核、目标去重算子三者职责不同；单文件会让 Phase 3/5/7 的改动都指向同一文件。 |
| `Identity` 返回类型 | `RowIdentity`（`Key []byte`） | 同 plan，但内核 key 为 `string` | `[]byte` 不可 `comparable`，无法在内核里用 `==` 丢弃同 key 组。 |

------

## 5. 对后续阶段的输入

- **Phase 3（INSERT）**：Phase 3 不需要 `TargetRowDedup`（INSERT 不寻址既有物理行），`InsertCandidate.Target()` 恒为无效身份。
- **Phase 5（UPDATE JOIN）/ Phase 7（多表 DELETE）**：绑定层必须提供 `NewSort` 工厂（复用 `bindDistinct` 的预算切分方式），并负责 clone 被保留的 row 与 identity key；算子只保证 `Close`。
- **Phase 8（资源验证）**：现有 `physical_pipeline_test.go:88-99` 已给出"DISTINCT spill 后临时目录为空"的断言范式，A05 可直接照抄验证 `TargetRowDedup` 的真实 spill 路径。
