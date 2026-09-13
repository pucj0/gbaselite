# A04 MVCC SQL 能力矩阵

本表以 Repository HEAD 的实测行为为准（针对 HEAD 编写的 legacy↔MVCC 对拍脚本加回归），一项一行，
不合并独立能力。MVCC 是唯一运行事务引擎；“范围限制”记录当前实现的真实边界。

| 能力 | Legacy | Parser | MVCC runtime | 回归 | 范围限制 |
|---|---|---|---|---|---|
| UNION / UNION ALL / DISTINCT | 支持 | 支持 | 支持 | 有 | 未实现完整 MySQL 类型合并 |
| 窗口函数 | 支持 | 支持 | 支持 | 有 | 不支持 named window / 显式 frame |
| INNER / LEFT JOIN | 支持 | 支持 | 支持 | 有 | 16 路输入上限 |
| RIGHT JOIN | 支持 | 支持 | 支持 | 有（对拍） | SELECT 交换驱动侧；DML 取目标匹配子集 |
| CROSS JOIN | 支持 | 支持 | 支持 | 有（对拍） | 无 ON 时笛卡尔积 |
| Derived table in FROM | 支持 | 支持 | 支持 | 有（对拍） | 必须带别名；别名列在相关作用域可见（legacy 不发布） |
| Derived table in JOIN | 支持 | 支持 | 支持 | 有（对拍） | 同上；受结果内存预算约束 |
| 非递归 CTE | 支持 | 支持 | 支持 | 有（对拍） | 语句级作用域，支持列清单/多 CTE/遮蔽同名表 |
| 递归 CTE（WITH RECURSIVE） | 支持 | 支持（需 UNION ALL） | 支持 | 有（对拍） | 列清单语法 parser 不支持；深度上限 1000；越界/列数不符报错回滚 |
| 标量/IN/NOT IN/EXISTS 子查询（含相关、写入中） | 支持 | 支持 | 支持 | 有（对拍） | 无 FROM 的 `SELECT (SELECT …)` 两引擎均不支持 |
| UPDATE JOIN（连接输入为基表） | 支持 | 支持 | 支持 | 有（对拍） | 目标行多命中只更新一次，取首个匹配 |
| INSERT SELECT（含 UNION ALL 源） | 支持 | 支持 | 支持 | 有（对拍） | 源使用语句父快照 |
| INSERT SET / IGNORE / REPLACE / ON DUPLICATE KEY UPDATE | 支持 | 支持 | 支持 | 有（对拍） | REPLACE 删除全部冲突行；被引用行返回 parent change |
| 多表 DELETE（目标表有主键） | 支持 | 支持 | 支持 | 有（对拍） | 不支持 LIMIT；目标表须有主键 |
| 多表 DELETE（目标表无主键） | 支持 | 支持 | 支持（驱动目标） | 有（对拍） | 驱动目标用 storage row key 作 identity；joined 目标仍需主键 |
| CREATE VIEW | 支持 | 支持 | 不支持 | 无 | `executor/mutation.go` mutateSQL 默认分支（本轮未实现） |
| Query VIEW（SELECT ... FROM view） | 支持 | 支持 | 不支持 | 无 | 查询绑定不解析视图定义 |
| DROP VIEW | 支持 | 支持 | 不支持 | 无 | `executor/mutation.go` mutateSQL 默认分支 |
| CREATE TABLE AS SELECT | 支持 | 支持 | 支持 | 有（对拍） | 查询快照 + 同语句 child 事务建表插数，失败不留空表 |
| CREATE TABLE LIKE | 支持 | 支持 | 支持 | 有（对拍） | 复制列/主键/索引/CHECK/注释，不复制行与外键（同 legacy） |
| RENAME TABLE | 支持 | 支持 | 支持 | 有（对拍） | 保留表 ID/数据命名空间，同步 catalog 名、FK RefTable/Referrers/fkref |
| SAVEPOINT / ROLLBACK TO / RELEASE SAVEPOINT | 支持 | 支持 | 支持 | 有（对拍） | 用 `Txn.Child()` 分层实现，无独立顶层事务；RELEASE 只摘名字 |
| FK ON DELETE/UPDATE CASCADE / SET NULL（含自引用） | 支持 | 支持 | 支持 | 有（对拍） | 级联在同一 statement child 事务内，深度上限 32，失败整条回滚 |
| 锁定读（SELECT … FOR UPDATE / LOCK IN SHARE MODE） | 支持 | 支持 | 支持 | 有（对拍） | 与 legacy 一致按语句快照读取，不做加锁 |
| ON UPDATE 列表达式（ON UPDATE CURRENT_TIMESTAMP） | 支持 | 支持 | 支持 | 有（对拍） | 未显式赋值列在 UPDATE/UPDATE JOIN 时写入当前时间 |
| 自引用外键（含自引用级联） | 支持 | 支持 | 支持 | 有（对拍） | 见上一行 |
| 事务快照、语句原子性、断连回滚 | 支持 | - | 支持 | 有 | 快照隔离 |
| metadata refresh（non-RevisionReader backend） | 支持 | - | 支持 | 有 | 修复 `head = tx.Snapshot()` 后连续 revision 正确 |

要点：

- 查询、派生表、CTE、子查询都在语句父快照事务上求值，不新开事务；mutation 写入 statement child
  transaction，失败整条回滚；`LastInsertID` 只在语句事务提交成功后发布。
- 派生表/CTE 通过 `physical.Materialize` + transient schema 进入统一 pipeline，不经过旧 Result 物化管线。
- 仍未支持的项目（视图 CREATE/Query/DROP；其余原缺口已迁移）
  都是 legacy 支持、parser 支持的 A04 真实 gap，源码位置见上表“范围限制”列。
