# GBaseLite SQL 兼容参考

更新日期：2026-08-04

本文按语法类别说明当前 GBaseLite 源码已经实现的 MySQL 兼容范围。它是开发、迁移和
联调时的核对清单，不代表兼容任意 MySQL 语句。面向第一次使用数据库的人员，请先阅
读 [SQL 使用教程](sql-user-guide.zh-CN.md)；安装、配置、部署和备份见项目
[README](../README.md)。

部署后的实例可能仍运行旧二进制。开始验收前先执行：

```sql
SELECT VERSION();
SELECT NOW();
```

源码测试通过不等于运行实例已经部署对应修复。涉及 Prepared Statement、BOOLEAN、
元数据字段包或错误码时，还应通过实际 MySQL 驱动复测，不能只在 SQL 控制台测试字面
量。

## 阅读说明

| 标记 | 含义 |
|---|---|
| 支持 | 有解析、执行和回归测试覆盖 |
| 兼容子集 | 满足已知客户端或业务路径，但不是完整 MySQL 语义 |
| 接受但不执行 | 为导入或客户端探测接受语法，不提供对应数据库能力 |
| 不支持 | 会报错、返回空兼容元数据，或不能依赖其业务语义 |

语句中的数据库名、表名和列名均可按 MySQL 习惯使用反引号。业务 SQL 应优先显式写列
名，并在隔离测试库验证后再进入生产迁移。

## 数据库、表和视图

| 语法 | 状态 | 说明 |
|---|---|---|
| `CREATE DATABASE [IF NOT EXISTS]`、`DROP DATABASE [IF EXISTS]`、`USE` | 支持 | 数据库名大小写不敏感 |
| `CREATE TABLE [IF NOT EXISTS]` | 支持 | 接受常见 ENGINE、字符集、排序规则和 COMMENT 选项 |
| `CREATE TABLE ... AS SELECT` | 支持 | 推导列并复制查询结果，不复制来源索引和约束 |
| `CREATE TABLE ... LIKE source` | 支持 | 复制列、默认值、主键、普通/唯一索引、CHECK 和表注释，不复制外键 |
| `DROP TABLE [IF EXISTS] t1,t2` | 支持 | 可一次删除多张表 |
| `TRUNCATE TABLE` | 支持 | 重置自增；被非空子表引用时拒绝，不执行级联 |
| `RENAME TABLE old TO new,...` | 支持 | 同库单表或多表原子改名，并同步同库外键引用 |
| `CREATE/ALTER/CREATE OR REPLACE VIEW` | 支持 | 视图持久化但只读 |
| `DROP VIEW [IF EXISTS]` | 支持 | 支持一次删除多个视图 |

CTAS 和 LIKE 都按整条语句提交，失败不会留下半成品表。跨库表改名、跨库外键和可更新
视图不支持。

## ALTER TABLE

一条 ALTER 可以用逗号组合下面已支持的动作。动作按书写顺序执行；任一后置动作失败
时，前面动作也不会保留。

| 动作 | 说明 |
|---|---|
| `ADD [COLUMN] [IF NOT EXISTS]` | 支持 FIRST/AFTER；非空表新增 NOT NULL 列必须有可用默认值 |
| `DROP [COLUMN] [IF EXISTS]` | 列仍被主键、索引、外键或 CHECK 使用时拒绝 |
| `MODIFY [COLUMN]` | 原子转换历史值，可调整类型、默认值、注释和位置 |
| `CHANGE [COLUMN] old new` | 修改定义并改名，同步本表索引 |
| `RENAME COLUMN old TO new` | 同步本表索引及同库外键引用 |
| `ADD PRIMARY KEY` | 先检查历史 NULL 和重复值 |
| `ADD [UNIQUE] INDEX/KEY` | 支持命名或根据首列生成名称 |
| `ADD CONSTRAINT ... UNIQUE` | 先扫描历史重复值 |
| `DROP INDEX/KEY`、`RENAME INDEX` | 按索引名执行 |
| `ADD/DROP FOREIGN KEY` | 按约束名执行；新增前扫描历史孤儿行 |
| `ADD/DROP CHECK` | 按约束名执行；新增前扫描历史不合规行 |
| `ALTER COLUMN ... SET/DROP DEFAULT` | 只影响后续写入，不重写历史行 |
| `COMMENT='...'` | 修改并持久化表注释 |
| `RENAME TO new_name` | 必须作为单独 ALTER，不能与其他动作混写 |

示例：

```sql
ALTER TABLE users
  ADD COLUMN weekly_goal INT NOT NULL DEFAULT 3,
  ADD CONSTRAINT ck_weekly_goal CHECK (weekly_goal BETWEEN 1 AND 14),
  ADD INDEX (weekly_goal),
  ALTER COLUMN weekly_goal SET DEFAULT 4,
  RENAME INDEX weekly_goal TO idx_users_weekly_goal;
```

`ADD COLUMN` 中的内联 PRIMARY KEY、UNIQUE 或 CHECK 当前应拆成后续独立动作。升级不
会根据历史数据猜测旧版本已经丢失的约束元数据，`CREATE TABLE IF NOT EXISTS` 也不
会把新定义合并到已有表。

## INSERT 和 REPLACE

| 语法 | 状态 | 说明 |
|---|---|---|
| `INSERT [INTO] ... VALUES/VALUE` | 支持 | 支持多行和表达式值 |
| `INSERT ... SET col=expr` | 支持 | 支持 Prepared Statement |
| `INSERT ... SELECT` | 支持 | SELECT 输入可以是 UNION |
| `INSERT IGNORE` | 支持 | 只忽略主键或唯一键冲突 |
| `ON DUPLICATE KEY UPDATE` | 支持 | 结果按目标列正常转换 |
| `REPLACE ... VALUES/VALUE/SET/SELECT` | 支持 | 删除全部主键/唯一键冲突行后插入候选行 |

REPLACE 不是原地 UPDATE。它会执行冲突行删除触发的同库外键动作，影响行数为显式删
除数加插入数；任一候选行失败时，整条 REPLACE 不保留前面候选行的修改。

```sql
INSERT INTO users SET id=1, phone='13800000001', enabled=TRUE;

REPLACE INTO users (id, phone, enabled)
VALUES (1, '13800000002', TRUE);
```

## UPDATE 和 DELETE

| 语法 | 状态 | 说明 |
|---|---|---|
| 表达式 UPDATE | 支持 | 多个 SET 赋值按 MySQL 左到右顺序求值 |
| `UPDATE ... WHERE ... LIMIT n` | 支持 | 适用于客户端单行编辑 |
| `UPDATE target [alias] JOIN ...` | 支持 | 只修改 JOIN 前的第一个目标基表 |
| 单表 `DELETE ... LIMIT n` | 支持 | 条件为空时会删除全表 |
| `DELETE t1,t2 FROM ...` | 支持 | 各目标表按稳定行号去重 |
| `DELETE FROM t1,t2 USING ...` | 支持 | 与上一种多表 DELETE 形式等价 |
| 多表 DELETE 的 LIMIT | 不支持 | 应先缩小 WHERE 或拆分为单表操作 |

同一目标行被 UPDATE JOIN 多次命中时只更新一次。UPDATE JOIN、多表 DELETE、相关子查
询写入和表达式 UPDATE 都从隔离的语句快照执行并整句提交；求值、唯一键、外键或
CHECK 在任一行失败时不会留下部分修改。

```sql
UPDATE users AS u
JOIN adjustments AS a ON a.user_id=u.id
SET u.balance=u.balance+a.delta,
    u.label=CONCAT('balance-',u.balance)
WHERE a.approved=TRUE;

DELETE p,c
FROM parents AS p
JOIN children AS c ON c.parent_id=p.id
WHERE p.id=1;
```

## SELECT、UNION 和 CTE

| 能力 | 状态与边界 |
|---|---|
| WHERE | 比较、NULL 三值逻辑、LIKE、IN、BETWEEN、IS NULL、NOT、AND、OR |
| ORDER BY | 支持真实列、布尔表达式、`IS NULL`、比较表达式和 `CASE WHEN` |
| JOIN | INNER、LEFT、RIGHT、CROSS；纯列等值连接可走哈希路径 |
| 派生表 | 支持 SELECT 或 UNION 作为 FROM/JOIN 输入 |
| 聚合 | COUNT、SUM、AVG、MIN、MAX、GROUP BY、HAVING |
| UNION | `UNION`、`UNION DISTINCT`、`UNION ALL`；最终应用 ORDER BY/LIMIT |
| 非递归 CTE | 一条 WITH 可按顺序声明多个 CTE，后一个可引用前一个 |
| 递归 CTE | 单个 `WITH RECURSIVE ... UNION ALL`，最多 1000 轮 |
| 窗口函数 | ROW_NUMBER、RANK、DENSE_RANK、COUNT/SUM/AVG/MIN/MAX OVER |
| 分页 | LIMIT、LIMIT/OFFSET 和 MySQL `LIMIT offset,count` |
| EXPLAIN | 基础 `EXPLAIN SELECT/UNION`，输出访问类型、候选/实际索引和估算行数 |

UNION 可作为顶层查询、派生表、视图、INSERT SELECT、标量/IN/EXISTS 子查询和
EXPLAIN 输入。各分支必须返回相同列数。

窗口函数暂不支持显式 frame、命名窗口以及同层 GROUP BY 后直接窗口计算。复杂 JOIN、
OR、函数包裹索引列、派生表、窗口和分组路径仍可能扫描或物化。

## 子查询

支持位置：

- SELECT 投影，以及 SELECT、UPDATE 和 DELETE 的 WHERE 条件
- `JOIN ... ON`
- UPDATE 的 SET 表达式

SELECT 或 UNION 还可作为派生表、视图和 INSERT SELECT 输入；当前没有 LATERAL 相关
派生表语义。

支持形式：

- 单列或多列 `IN (SELECT ...)` / `NOT IN (SELECT ...)`
- `EXISTS` / `NOT EXISTS`
- 只返回一列、至多一行的标量子查询
- 相关和非相关子查询
- 行构造器，例如 `(user_id,status) IN (SELECT user_id,status ...)`

```sql
UPDATE users AS u
SET score=(
  SELECT s.score
  FROM sessions AS s
  WHERE s.user_id=u.id
  LIMIT 1
)
WHERE EXISTS (
  SELECT 1
  FROM sessions AS s
  WHERE s.user_id=u.id AND s.active=TRUE
);

DELETE FROM users AS u
WHERE u.id IN (
  SELECT s.user_id
  FROM sessions AS s
  WHERE s.user_id=u.id AND s.revoked_at IS NOT NULL
);
```

非相关子查询在外层扫描前执行一次；相关子查询按外层行求值。内层不存在同名列时，可
使用未限定外层列，但迁移 SQL 更建议写明确别名。标量子查询返回多行会报错，并保持整
条写入原子回滚。`NOT IN` 子查询包含 NULL 时遵循 SQL 三值逻辑，通常不会命中任何
行。

## 索引和 EXPLAIN

普通、唯一和复合索引维护有序内存访问路径。基础表 SELECT 可以使用：

- 单列主键或唯一索引等值定位
- 复合索引左前缀等值
- 紧随等值前缀后的单列范围
- 与索引一致的正序或倒序 ORDER BY
- 提前 LIMIT/OFFSET
- 简单 COUNT(*) 的有界扫描

```sql
CREATE INDEX idx_workouts_user_started
ON workouts(user_id,started_at);

EXPLAIN SELECT id
FROM workouts
WHERE user_id=10
  AND started_at>='2026-08-01 00:00:00'
ORDER BY started_at DESC
LIMIT 20;
```

当前 EXPLAIN 是基础计划说明，不是成熟 MySQL 成本模型；JOIN 两侧索引访问、OR、IN、
函数包裹索引列和复杂分组不保证使用索引。

## 约束和外键动作

新表和后续 ALTER 会执行 PRIMARY KEY、UNIQUE、同库 FOREIGN KEY、CHECK、NOT NULL、
ENUM 和 JSON 载荷校验。

同库外键支持：

- 默认 `RESTRICT` / `NO ACTION`
- `ON DELETE CASCADE`
- `ON UPDATE CASCADE`
- `ON DELETE SET NULL`
- `ON UPDATE SET NULL`

SET NULL 的全部子列必须可空。级联修改支持多层传播，并检测循环；TRUNCATE 不执行级
联。跨库外键不支持。

## 事务和并发语义

```sql
BEGIN;
UPDATE users SET balance=balance-1 WHERE id=1;
INSERT INTO course_ledger(user_id,change_amount) VALUES (1,-1);
COMMIT;
```

支持 `BEGIN`、`COMMIT` 和 `ROLLBACK`。显式事务使用实例级独占事务门：当前会话
读取自己的事务快照，其他会话的读取和写入等待事务结束。当前没有可选择的隔离级别、
行级锁、死锁检测或锁等待超时。

`SELECT ... FOR UPDATE` 语法可用于业务事务，但当前正确性来自实例级事务门，并不是
只锁命中行。SAVEPOINT、RELEASE SAVEPOINT 和 ROLLBACK TO 为客户端兼容命令，不能依
赖完整保存点语义。

事务中分配的 AUTO_INCREMENT ID 在回滚后不会复用；普通 DELETE 不重置计数，
TRUNCATE 会重置计数。

## SHOW 和 information_schema

常用 SHOW：

```sql
SHOW DATABASES;
SHOW FULL TABLES FROM app LIKE 'user%';
SHOW TABLE STATUS FROM app LIKE 'users';
SHOW FULL COLUMNS FROM users LIKE 'phone';
SHOW COLUMNS FROM users WHERE Field='phone';
SHOW INDEX FROM users;
SHOW CREATE TABLE users;
SHOW CREATE VIEW active_users;
SHOW GRANTS FOR 'app'@'%';
SHOW GLOBAL STATUS;
SHOW SESSION STATUS LIKE 'Ssl_%';
```

`SHOW COLUMNS/FIELDS` 支持表名前缀、第二个 `FROM/IN database`、LIKE 和 WHERE。
FULL 结果包含 Field、Type、Collation、Null、Key、Default、Extra、Privileges 和
Comment。

提供实际兼容数据的 information_schema 表包括：

- SCHEMATA、TABLES、COLUMNS、VIEWS、STATISTICS
- TABLE_CONSTRAINTS、KEY_COLUMN_USAGE
- REFERENTIAL_CONSTRAINTS、CHECK_CONSTRAINTS
- USER_PRIVILEGES、SCHEMA_PRIVILEGES、TABLE_PRIVILEGES

FILES、TABLESPACES、PARTITIONS、ROUTINES、PARAMETERS、TRIGGERS 和 EVENTS 等客户端
探测表只返回兼容列布局或空结果，不代表实现了对应数据库对象。information_schema 和
mysql 都不是可 USE 的持久化系统库。

```sql
SELECT COLUMN_NAME,COLUMN_TYPE,IS_NULLABLE,COLUMN_DEFAULT,COLUMN_COMMENT
FROM information_schema.COLUMNS
WHERE TABLE_SCHEMA='app'
  AND TABLE_NAME='users'
  AND COLUMN_NAME='phone';
```

## Prepared Statement 和驱动

MySQL Prepared Statement 协议支持参数绑定和二进制结果。BOOLEAN/TINYINT 条件会把
Go MySQL Driver 传入的 bool 或整数参数按数值语义比较：

```sql
SELECT id FROM workout_part_options WHERE enabled=?;
UPDATE workout_part_options SET enabled=? WHERE id=?;
DELETE FROM workout_part_options WHERE enabled=? LIMIT 1;
```

`LIMIT ? OFFSET ?`、动态重组后的 `IN (?,...)` 和常见 INSERT/UPDATE/DELETE 参数都
应通过实际驱动验收。SQL 中占位符数量必须与绑定参数一致。

## 常见 MySQL 错误码

| 错误码 | 场景 |
|---:|---|
| 1030 | 持久化失败后进入 fail-closed，数据库不可继续读写 |
| 1045 | 认证失败或来源账号被短暂阻断 |
| 1049 | 连接握手或客户端切库指定了不存在的数据库 |
| 1061 | 索引名称已存在 |
| 1062 | 主键或唯一索引冲突 |
| 1064 | SQL 解析错误或未单独映射的执行错误 |
| 1091 | 删除不存在的索引或约束 |
| 1142 | 当前账号缺少对象权限 |
| 1396 | CREATE/ALTER/DROP USER 的对象状态错误 |
| 1451 | 父行仍被引用，RESTRICT/NO ACTION 拒绝修改或删除 |
| 1452 | 子行引用了不存在的父行 |
| 1826 | 约束名称已存在 |
| 3159 | 服务要求 TLS，但客户端使用明文连接 |
| 3819 | CHECK、ENUM 或 JSON 载荷校验失败 |

收到 1030 且错误中包含 fail-closed 时，先停止业务重试，检查磁盘空间和服务账号对快
照目录的创建、写入及原子替换权限，保留完整数据目录后再恢复服务。

## 接受但不提供完整语义的兼容命令

以下命令用于 MySQL 工具导入或能力探测，不能据此推断对应引擎能力已经实现：

- SET 会话兼容项、FLUSH PRIVILEGES、FLUSH TABLES
- LOCK TABLES / UNLOCK TABLES
- SAVEPOINT / RELEASE SAVEPOINT / ROLLBACK TO
- ALTER TABLE ... ENABLE/DISABLE KEYS
- SHOW WARNINGS / SHOW ERRORS
- SHOW PROCEDURE/FUNCTION STATUS、SHOW TRIGGERS、SHOW EVENTS
- KILL 的兼容响应

业务正确性不能依赖这些命令产生 MySQL 完整副作用。

## 明确不支持或仍有限制

- 跨库外键和跨库 RENAME TABLE
- 触发器、存储过程、存储函数和事件
- 通过视图执行 INSERT、UPDATE 或 DELETE
- 多表 DELETE 尾部 LIMIT
- 完整 MySQL DECIMAL、UNSIGNED/ZEROFILL、TIME、二进制、JSON、SET 和空间类型语义
- 完整 information_schema/mysql 系统目录
- 可更新视图和 WITH CHECK OPTION 执行
- 命名窗口、显式窗口 frame 和完整优化器成本模型
- 行级锁、可配置事务隔离、死锁检测和锁等待超时
- 复制、集群、分片、自动故障转移、在线物理备份和物理 WAL
- 客户端证书认证与证书自动签发/轮换

迁移包含这些能力的 MySQL 应用时，应先改写设计或选择 MySQL/PostgreSQL 等成熟数据
库，不能通过忽略错误或只放开解析器规避。

## 上线前最小验收

1. `SELECT VERSION()` 与批准部署版本一致。
2. `NOW()` 和 DEFAULT CURRENT_TIMESTAMP 写入时间一致。
3. 通过实际驱动验证 Prepared BOOLEAN、LIMIT/OFFSET 和事务回滚。
4. 核对 SHOW CREATE TABLE 中的主键、唯一键、外键和 CHECK。
5. 验证 1062、1451、1452 和 3819 错误路径。
6. 验证多动作 ALTER 后置失败时整条结构不变。
7. 验证相关 UPDATE/DELETE 子查询和 JOIN ON 条件。
8. 重启后核对 AUTO_INCREMENT 不倒退或复用。
9. 在独立实例恢复逻辑备份并比对行数和关键业务统计。
10. 确认生产账号、TLS、网络访问范围、日志和备份保留策略。
