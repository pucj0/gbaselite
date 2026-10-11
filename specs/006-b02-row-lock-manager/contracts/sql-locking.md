# B02 SQL Locking Contract

**Related**: FR-010/012/014..018/020..022/026..028/031/032。
以下SQL都是规划能力；当前生产代码仍按README现有矩阵运行。

## SELECT grammar and supported shape
```sql
SELECT ... FROM base_table [WHERE ...] [ORDER BY ...] [LIMIT ...] FOR UPDATE;
SELECT ... FROM base_table [WHERE ...] [ORDER BY ...] [LIMIT ...] FOR SHARE;
SELECT ... FROM base_table [WHERE ...] [ORDER BY ...] [LIMIT ...] LOCK IN SHARE MODE;
```
FOR UPDATE=X/IX；FOR SHARE/LOCK IN SHARE MODE=S/IS。AST必须保存实际mode，不能只用Locking bool。
首次实现只支持单个真实基表，允许alias、scalar projection和WHERE，不允许JOIN、CTE、派生表、
view、子查询、UNION、聚合、GROUP/HAVING、DISTINCT、window。FOR UPDATE NOWAIT / SKIP LOCKED
在解析/绑定阶段报明确unsupported，不能沿用现有丢弃策略。EXPLAIN不获得user grants。
无row输出的locking query仍可能持Gap；不存在目标不能静默取消区间保护。

## Session options
```sql
SET SESSION innodb_lock_wait_timeout = 50;
```
默认50秒，只允许整数1..86400；0、负值、小数、超界、GLOBAL设置报错；这是锁等待子集，
不意味着完整innodb变量支持。嵌入式options能独立配置预算，查询ctx可能更早结束等待。

## Lock plans
| 访问 | 锁范围 |
|---|---|
| 完整存在PK/UNIQUE等值 | table意向、索引entry需要的record及共同row S/X；不加无关gap |
| 未命中完整等值 | 含目标插入位置的stable Gap/reservation；不因无row漏锁 |
| ordered range / nonunique / partial composite | proven interval + visited Next-Key/row；边界保护覆盖predicate |
| unordered/legacy encoding/full scan | 对应index/rowspace whole-domain Gap + visited records；显式fallback |
| secondary/covering | entry/interval + common primary physical row锁 |

ORDER/LIMIT只规定结果，实际examined records和access interval可能锁得更宽。反向扫描不改变
interval成员。通过EXPLAIN/diagnostics可辨别record-only、range与fallback。

## DML behavior
INSERT VALUES/SET/SELECT均经InsertOperator；INSERT SELECT source仍parent snapshot。
目标heap/PK、新unique/secondaryentries先获II+X再检查latest/overlay约束。UNIQUE含NULL不reservation，
collation等价值必须使用同锁identity。生成ID持久预留在事务外，允许回滚后的自增gap，与II不同。
IGNORE保留既有可忽略错误集合，不能忽略timeout/deadlock/SIconflict；REPLACE/upsert必须锁冲突row及
所有old/new entries，不利用等待前duplicate缓存。任一post-readTStarget变更都不能被upsert覆盖。

UPDATE/DELETE：parent snapshot→dedup/first-match/LIMIT→锁后target/catalog验证→ModifyOperator→
childwrites。目标过期ErrConflict终止整个root，提示retry；兼容目标继续原SET顺序/旧索引移除逻辑。
PK/索引值迁移锁旧/新identity，multi DELETE沿既有FKorder执行，cascade/SET NULL统一底层锁入口。
catalog及FKguards保持乐观验证，DDL变化可以使current/DML在解码前冲突；不新增DDL隐式提交。

## Lifecycle/error table
| 事件 | 事务结果 | 已授予锁 |
|---|---|---|
| child merge / statement success | child MERGED，无durable revision | root保留 |
| statement error / lock timeout / querycancel | child rollback；显式root可继续 | root保留 |
| savepoint rollback/release/replacement | 原layer模型不变；pending discarded scopes取消 | root保留 |
| deadlock victim / SI conflict | 整root终止，需retry | 终态cleanup后全释放 |
| autocommit读写结束 | root结束，按原三类commit语义 | 全释放 |
| explicit COMMIT/ROLLBACK / disconnect | definite终态 | 全释放 |
| restore/term/Close | epoch无效，pending唤醒；未定publish须reconcile | oldhandles不可用，定案cleanup |

LockWaitTimeout=1205/HY000；Deadlock=1213/40001；既有ErrConflict映射不变，diagnostic reason可区分。
ctxcancel保持现有取消映射；unsupported/资源/leader错误复用项目中立错误映射并明确消息。
错误不能留下部分statementwrites、错误的LastInsertID、零散grants owner或阻塞goroutine。
result是collector消费完再关闭autocommitroot，不在提前结束/Close失败时遗漏清理。

## Explicit compatibility limits
旧root锁定读可见新committed版本，但写入该新版本仍必须SIconflict。DML候选不自动变成最新集合。
TableS/X、LOCKTABLES、isolationlevelswitch、NOWAIT、SKIPLOCKED、复杂lockingSELECT以及完整
performance_schema不在范围。粗范围锁与savepoint锁保留可能比InnoDB更保守。
