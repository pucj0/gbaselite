# A04 MVCC SQL 能力矩阵

| 能力 | Legacy | Parser | Unified executor | MVCC | 回归覆盖 | 当前边界 |
|---|---|---|---|---|---|---|
| UNION / UNION ALL / DISTINCT | 支持 | 支持 | 支持 | 支持 | 有 | 未实现完整 MySQL 类型合并 |
| 窗口函数 | 支持 | 支持 | 支持 | 支持 | 有 | 不支持 named window / 显式 frame |
| 派生表、CTE、视图、子查询 | 支持 | 支持 | 支持 | 支持 | 有 | 不支持 LATERAL |
| INSERT SELECT | 支持 | 支持 | 支持 | 支持 | 有 | source 使用 statement snapshot |
| UPDATE JOIN / 多表 DELETE | 支持 | 支持 | 支持 | 支持 | 有 | 稳定目标身份去重 |
| RIGHT / CROSS JOIN | 支持 | 支持 | 支持 | 支持 | 有 | 复用统一 Join operator |
| CTAS、LIKE、INSERT SET、IGNORE、REPLACE | 支持 | 支持 | 支持 | 支持 | 有 | 遵循现有兼容边界 |
| SAVEPOINT、外键动作 | 支持 | 支持 | 支持 | 支持 | 有 | 受当前事务协议限制 |

该矩阵基于 parser、统一 executor/physical pipeline、MVCC 与历史兼容测试审计。复合查询共享 `boundQuery`、relation binding 与同一 statement transaction；复合 DML 在 child transaction 中保持原子性。
