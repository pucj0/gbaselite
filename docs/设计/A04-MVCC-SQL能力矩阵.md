# A04 MVCC SQL 能力矩阵

本表以 Repository HEAD 的实测行为为准（针对 HEAD 编写的 SQL probe 加现有回归），不是历史
legacy 执行器的能力承诺。MVCC 是唯一运行事务引擎。

| 能力 | Legacy 执行器 | Parser | MVCC 运行时 | 回归覆盖 |
|---|---|---|---|---|
| UNION / UNION ALL / DISTINCT | 支持 | 支持 | 支持 | 有 |
| 窗口函数 | 支持 | 支持 | 支持 | 有 |
| INNER/LEFT JOIN、聚合、分组、排序分页 | 支持 | 支持 | 支持 | 有 |
| UPDATE JOIN（连接输入为基表） | 支持 | 支持 | 支持（本轮实现） | 有（legacy 对拍） |
| 多表 DELETE | 支持 | 支持 | 不支持 | 无 |
| INSERT SELECT / INSERT SET / IGNORE / REPLACE | 支持 | 支持 | 不支持 | 无 |
| 派生表、CTE、视图、子查询（EXISTS/IN/标量） | 支持 | 支持 | 不支持 | 无 |
| RIGHT / CROSS JOIN | 支持 | 支持 | 不支持（仅 INNER/LEFT） | 无 |
| SAVEPOINT、外键级联/置空动作 | 支持 | 支持 | 不支持（外键仅 RESTRICT/NO ACTION） | 无 |
| 事务快照、语句原子性、断连回滚 | 支持 | - | 支持 | 有 |

要点：

- MVCC 语句在父事务快照上读、在 statement child transaction 上写；UPDATE JOIN 遵循同一边界，
  任意一行失败会回滚整条语句。
- UPDATE JOIN 目标行被多个连接输入命中时只更新一次并采用首个匹配，SET 列表从左到右生效、
  后续表达式可见前面的赋值结果，受影响行数按命中的目标行统计。上述语义由
  `executor/mvcc_update_join_test.go` 与 legacy 执行器对拍锁定。
- 表中“不支持”的行是 MVCC 运行时的真实缺口，不是 legacy parity 已完成。后续 A04 增量应
  按本表逐项补齐实现和回归，而不是重复审计。