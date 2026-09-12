# A04 MVCC SQL 能力矩阵

本表以 Repository HEAD 的实测行为为准（针对 HEAD 编写的 SQL probe 加现有回归），不是历史
legacy 执行器的能力承诺。MVCC 是唯一运行事务引擎。写入面已实现 INSERT SET/IGNORE、REPLACE、
ON DUPLICATE KEY UPDATE、子查询（标量/IN/EXISTS，含相关子查询），以及复合写入 UPDATE JOIN、
INSERT SELECT 与多表 DELETE。

| 能力 | Legacy 执行器 | Parser | MVCC 运行时 | 回归覆盖 |
|---|---|---|---|---|
| UNION / UNION ALL / DISTINCT | 支持 | 支持 | 支持 | 有 |
| 窗口函数 | 支持 | 支持 | 支持 | 有 |
| INNER/LEFT JOIN、聚合、分组、排序分页 | 支持 | 支持 | 支持 | 有 |
| UPDATE JOIN（连接输入为基表） | 支持 | 支持 | 支持（A04 增量） | 有（legacy 对拍） |
| INSERT SELECT（含 UNION ALL 源） | 支持 | 支持 | 支持（A04 增量） | 有（legacy 对拍） |
| 多表 DELETE（目标表须有主键） | 支持 | 支持 | 支持（A04 增量） | 有（legacy 对拍） |
| INSERT SET / IGNORE / REPLACE / ON DUPLICATE KEY | 支持 | 支持 | 支持（A04 增量） | 有（legacy 对拍） |
| 子查询（标量/IN/EXISTS，含相关子查询，SELECT 与写入） | 支持 | 支持 | 支持（A04 增量） | 有（legacy 对拍） |
| 派生表、CTE、视图 | 支持 | 支持 | 不支持 | 无 |
| RIGHT / CROSS JOIN | 支持 | 支持 | 不支持（仅 INNER/LEFT） | 无 |
| SAVEPOINT、外键级联/置空动作 | 支持 | 支持 | 不支持（外键仅 RESTRICT/NO ACTION） | 无 |
| 事务快照、语句原子性、断连回滚 | 支持 | - | 支持 | 有 |

要点：

- MVCC 语句在父事务快照上读、在 statement child transaction 上写；UPDATE JOIN 遵循同一边界，
  任意一行失败会回滚整条语句。
- UPDATE JOIN 目标行被多个连接输入命中时只更新一次并采用首个匹配，SET 列表从左到右生效、
  后续表达式可见前面的赋值结果，受影响行数按命中的目标行统计。上述语义由
  `executor/mvcc_update_join_test.go` 与 legacy 执行器对拍锁定。
- INSERT SELECT（含 UNION ALL 源）在语句快照上读取源数据、在 statement child 事务中写入目标表，
  自引用源不会重复读取本次插入的行，任一行 UNIQUE/CHECK/外键/类型转换失败会回滚整条语句。
  生成自增号的语句只在语句事务提交后才发布 LastInsertID，回滚或提交失败的语句不会把未提交的
  id 写回会话。上述语义由 `executor/mvcc_insert_select_test.go` 与 legacy 执行器对拍锁定。
- 多表 DELETE（`DELETE t1,t2 FROM …` 与 `DELETE FROM t1,t2 USING …`）在语句快照上读取连接关系，
  按主键去重，并按“先删引用方（子表）、后删被引用方（父表）”的顺序写入 statement child 事务，
  使同一条语句删除父表与子表时仍满足 RESTRICT 外键；不支持 LIMIT，目标表须有主键，目标表之间
  不允许循环外键。上述语义由 `executor/mvcc_multi_delete_test.go` 与 legacy 执行器对拍锁定。
- INSERT 冲突处理与 legacy 一致：IGNORE 只跳过重复键行（其他约束错误仍然失败）；REPLACE 删除主键或
  唯一键上的全部冲突行后再插入，受影响行数 = 删除行数 + 1，被引用行仍受 RESTRICT 保护；
  ON DUPLICATE KEY UPDATE 更新首个冲突行，普通列名读取被更新的行、VALUES(列) 读取待插入值，
  受影响行数为 1 且不发布 LastInsertID。上述语义由 `executor/mvcc_insert_modes_test.go`
  与 legacy 执行器对拍锁定。
- 子查询在语句的父快照事务上求值，不新开事务；相关子查询按外层行逐行求值，未限定列优先绑定内层
  作用域、显式外层限定名按外层行绑定，两层相关可用；`NOT IN` 子查询含 NULL 时遵循三值逻辑，
  标量子查询返回多行会报错并回滚整条语句。SELECT 与 UPDATE/DELETE/INSERT 的表达式同样适用。
  上述语义由 `executor/mvcc_subquery_test.go` 与 legacy 执行器对拍锁定。
- 表中“不支持”的行是 MVCC 运行时的真实缺口，不是 legacy parity 已完成。后续 A04 增量应
  按本表逐项补齐实现和回归，而不是重复审计。