# GBaseLite SQL 使用教程

这是一份给业务使用者、数据录入人员、报表人员和使用 Navicat/DBeaver 的用户的教
程。它只讲如何连接数据库和编写 SQL，不要求了解 Go、编译、发布或存储实现。

GBaseLite 使用 MySQL 协议，大多数 MySQL 客户端都可以连接；但它是 MySQL 兼容子
集。请在测试库先验证新 SQL，再用于业务库。需要按语法类别查当前兼容范围时，配合
[SQL 兼容参考](sql-compatibility-reference.zh-CN.md) 使用。

## 开始前

请向管理员确认以下连接信息：

| 项目 | 示例 | 说明 |
|---|---|---|
| 主机 | `127.0.0.1` | 本机服务使用这个地址；远程服务使用管理员提供的 IP 或域名 |
| 端口 | `3307` | 默认 MySQL 协议端口 |
| 用户名 | `root` | 首次安装可由管理员修改 |
| 密码 | 管理员设置的密码 | 不要把密码写进 SQL 文件或截图 |
| 数据库 | `sales_demo` | 连接后可用 `USE` 切换 |

使用 MySQL 命令行客户端连接：

```bash
mysql --protocol=tcp -h 127.0.0.1 -P 3307 -u root -p
```

输入密码后，可以先执行：

```sql
SELECT VERSION();
SHOW DATABASES;
```

在 Navicat 或 DBeaver 中新建 **MySQL** 连接，填入相同的主机、端口、用户名和密码
即可。不需要选择 PostgreSQL、SQLite 或 HTTP 连接类型。

新安装默认只接受本机 `127.0.0.1` 连接。需要从可信局域网连接时，由管理员把监听地
址改为指定的局域网网卡地址，并在防火墙中只放行需要的来源。管理员启用 TLS 后，
MySQL 8 CLI 可增加 `--ssl-mode=REQUIRED`，Navicat/DBeaver 则在连接的 SSL/TLS 页
面启用加密；如果服务要求安全传输，未启用 TLS 的客户端会收到错误 3159。即使启用
TLS，也不应把端口直接暴露到公网。连续输错密码达到管理员设置的阈值后，同一来源 IP
和账号会被短暂阻断，等待阻断时间结束再重试，不要通过不断重连规避限制。

## 一次完整练习

以下示例会创建名为 `gbaselite_training` 的练习库。不要把它替换为正在使用的业务库
名称。

### 1. 创建并进入数据库

```sql
CREATE DATABASE gbaselite_training;
USE gbaselite_training;

SHOW TABLES;
```

`USE` 只切换当前窗口或当前连接使用的数据库，不会修改其他人的连接。

### 2. 创建三张表

主键用于唯一标识每一行。为需要在客户端编辑、关联或长期维护的表建立主键。

```sql
CREATE TABLE customers (
  customer_id BIGINT PRIMARY KEY AUTO_INCREMENT,
  customer_name VARCHAR(100) NOT NULL,
  phone VARCHAR(30),
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE products (
  product_id BIGINT PRIMARY KEY AUTO_INCREMENT,
  product_name VARCHAR(100) NOT NULL,
  unit_price DECIMAL(10, 2) NOT NULL,
  active BOOLEAN NOT NULL DEFAULT TRUE
);

CREATE TABLE orders (
  order_id BIGINT PRIMARY KEY AUTO_INCREMENT,
  customer_id BIGINT NOT NULL,
  product_id BIGINT NOT NULL,
  quantity INT NOT NULL,
  ordered_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
```

查看建表结果：

```sql
SHOW TABLES;
SHOW COLUMNS FROM customers;
SHOW CREATE TABLE orders;
```

#### 一条 ALTER 修改多个字段

受支持的列、索引、约束和表注释动作可以按 MySQL 写法用逗号放在同一条
`ALTER TABLE` 中：

```sql
ALTER TABLE products
  ADD COLUMN stock_count INT NOT NULL DEFAULT 0,
  ADD COLUMN sku VARCHAR(40),
  MODIFY COLUMN product_name VARCHAR(150),
  ADD CONSTRAINT products_sku_uq UNIQUE (sku),
  ADD INDEX (stock_count),
  ALTER COLUMN stock_count SET DEFAULT 10,
  RENAME INDEX stock_count TO products_stock_idx;
```

动作按书写顺序执行。任一动作失败时，整条语句中的前置修改也会撤销，不会留下部分列
或索引。列还可以使用 `DROP COLUMN [IF EXISTS]` 和 `RENAME COLUMN old TO new`；删除
仍被索引、外键或 CHECK 使用的列前，必须在同一条 ALTER 中先删除依赖。表重命名必须
使用单独的 `ALTER TABLE ... RENAME TO ...` 或 `RENAME TABLE`，当前不能与其他 ALTER
动作混写。

#### 查看字段和兼容元数据

`SHOW COLUMNS` 支持 MySQL 的 `LIKE` 和 `WHERE` 过滤；`FULL` 形式还会返回排序
规则、权限和列注释。需要给管理工具或迁移程序精确查询字段时，也可以过滤
`information_schema.COLUMNS.COLUMN_NAME`：

```sql
SHOW FULL COLUMNS FROM products LIKE 'stock%';
SHOW COLUMNS FROM products WHERE Field='sku';

SELECT COLUMN_NAME, COLUMN_TYPE, IS_NULLABLE, COLUMN_DEFAULT, COLUMN_COMMENT
FROM information_schema.COLUMNS
WHERE TABLE_SCHEMA='gbaselite_training'
  AND TABLE_NAME='products'
  AND COLUMN_NAME='stock_count';
```

`information_schema` 是客户端兼容目录，不是完整 MySQL 系统库，不能执行
`USE information_schema`。应用只应查询实际需要的兼容字段。

需要从查询结果建表或复制表结构时，可以使用：

```sql
CREATE TABLE active_products AS
SELECT product_id, product_name, unit_price
FROM products
WHERE active = TRUE;

CREATE TABLE products_template LIKE products;
```

CTAS 会推导列并复制查询结果，但不复制来源索引和约束；LIKE 会复制列、默认值、索引、
CHECK 和表注释，但不复制外键。任一语句失败都不会留下半成品表。

`AUTO_INCREMENT` 适合生成连续的业务内部编号。`DECIMAL(p,s)` 的声明会被保留，但当
前数值计算不是完整 MySQL DECIMAL 语义；金额需要严格财务精度时，应先进行业务验
证。

### 3. 录入数据

优先写出列名。这样即使以后增加字段，原有 SQL 也更不容易错位。

```sql
INSERT INTO customers (customer_name, phone)
VALUES ('张三', '13800000001');

INSERT INTO customers (customer_name, phone)
VALUES ('李四', '13800000002');

INSERT INTO products (product_name, unit_price, active)
VALUES ('无线键盘', 199.00, TRUE);

INSERT INTO products (product_name, unit_price, active)
VALUES ('显示器支架', 289.00, TRUE);

INSERT INTO orders (customer_id, product_id, quantity)
VALUES (1, 1, 2);

INSERT INTO orders (customer_id, product_id, quantity)
VALUES (1, 2, 1);

INSERT INTO products
SET product_name='瑜伽垫', unit_price=80+19, active=TRUE;
```

录入后立即核对：

```sql
SELECT * FROM customers;
SELECT * FROM products;
SELECT * FROM orders;
```

重复写入主键或唯一索引会返回 MySQL 风格的 `1062` 错误，不会静默覆盖已有数据。导
入时明确需要跳过重复数据，可使用 `INSERT IGNORE`；不要把它当作日常写入的默认方
式。

```sql
INSERT IGNORE INTO customers (customer_id, customer_name, phone)
VALUES (1, '张三', '13800000001');
```

需要 MySQL 的“删除唯一键冲突行后重新插入”语义时可使用 `REPLACE`，支持
VALUES/VALUE/SET/SELECT 输入。它与更新原行不同，会执行删除触发的外键动作；影响行
数是删除数加插入数，应先确认关联数据的级联策略。

```sql
REPLACE INTO products (product_id, product_name, unit_price, active)
VALUES (1, '无线键盘', 219.00, TRUE);
```

## 查询数据

### 条件、排序和分页

```sql
SELECT customer_id, customer_name, phone
FROM customers
WHERE customer_name LIKE '张%'
ORDER BY customer_id DESC
LIMIT 20;
```

常用条件包括：

```sql
SELECT * FROM products WHERE active = TRUE;
SELECT * FROM products WHERE unit_price BETWEEN 100 AND 300;
SELECT * FROM customers WHERE customer_id IN (1, 2, 3);
SELECT * FROM customers WHERE phone IS NULL;
SELECT * FROM customers WHERE phone IS NOT NULL;
```

SQL 中 `NULL` 不是空字符串，也不能用 `= NULL` 判断。请使用 `IS NULL` 或
`IS NOT NULL`。

`ORDER BY` 可以使用普通列、布尔表达式和 `CASE WHEN`。下面的查询把有手机号的客
户排在前面，再按名称排序：

```sql
SELECT customer_id, customer_name, phone
FROM customers
ORDER BY phone IS NULL,
  CASE WHEN customer_name LIKE '张%' THEN 0 ELSE 1 END,
  customer_name,
  customer_id;
```

### 统计和分组

```sql
SELECT customer_id, COUNT(*) AS order_count, SUM(quantity) AS item_count
FROM orders
GROUP BY customer_id
HAVING COUNT(*) >= 1
ORDER BY item_count DESC;
```

常用聚合函数有 `COUNT`、`SUM`、`AVG`、`MIN` 和 `MAX`。按业务维度统计时，非聚合字
段应写入 `GROUP BY`。

`UNION/UNION ALL` 可以用于顶层查询、派生表、视图、INSERT SELECT、子查询和 EXPLAIN。
非递归 WITH 可以依次声明多个 CTE，后一个 CTE 可以引用前一个：

```sql
WITH priced_products AS (
  SELECT product_id, active
  FROM products
  WHERE unit_price >= 100
), active_ids AS (
  SELECT product_id
  FROM priced_products
  WHERE active = TRUE
)
SELECT product_id FROM active_ids ORDER BY product_id;

SELECT product_id, 'active' AS source
FROM products
WHERE active = TRUE
UNION ALL
SELECT product_id, 'inactive' AS source
FROM products
WHERE active = FALSE
ORDER BY product_id;
```

### 关联查询

GBaseLite 支持 `INNER JOIN`、`LEFT JOIN`、`RIGHT JOIN` 和 `CROSS JOIN`。订单表中
的 `customer_id`、`product_id` 可以建立同库外键；子行写入会检查父行，删除仍被引用
的父行会按默认 `RESTRICT` 拒绝。同库外键支持 `ON DELETE/UPDATE CASCADE` 和
`SET NULL`；`SET NULL` 的子列必须允许 NULL。跨库外键仍不支持，`TRUNCATE` 也不会
执行级联动作。

```sql
SELECT
  o.order_id,
  c.customer_name,
  p.product_name,
  o.quantity,
  o.ordered_at
FROM orders AS o
INNER JOIN customers AS c ON o.customer_id = c.customer_id
INNER JOIN products AS p ON o.product_id = p.product_id
ORDER BY o.order_id;
```

想保留没有订单的客户时使用 `LEFT JOIN`：

```sql
SELECT c.customer_name, o.order_id
FROM customers AS c
LEFT JOIN orders AS o ON c.customer_id = o.customer_id
ORDER BY c.customer_id, o.order_id;
```

### 子查询

`IN`、`NOT IN`、`EXISTS/NOT EXISTS` 和只返回一列一行的标量子查询可以是非相关
或相关子查询。相关子查询可以在 WHERE、`JOIN ... ON` 和 UPDATE 的 SET 表达式中
引用当前外层行：

```sql
SELECT c.customer_id, c.customer_name
FROM customers AS c
WHERE EXISTS (
  SELECT 1
  FROM orders AS o
  WHERE o.customer_id = c.customer_id
)
ORDER BY c.customer_id;

SELECT o.order_id, o.customer_id, o.product_id
FROM orders AS o
WHERE (o.customer_id, o.product_id) IN (
  SELECT candidate.customer_id, candidate.product_id
  FROM orders AS candidate
  WHERE candidate.customer_id = o.customer_id
)
ORDER BY o.order_id;
```

非相关子查询在外层扫描前执行一次，相关子查询按外层行求值。标量子查询返回多行会让
整条语句失败；写入语句不会保留错误发生前已经计算的部分修改。

## 修改和删除前的安全步骤

`UPDATE`、`DELETE`、`TRUNCATE`、`DROP TABLE` 和 `DROP DATABASE` 都会改变或删除数
据。业务库操作前，应先执行同条件的 `SELECT`，确认行数和内容。

```sql
-- 第一步：先看将要修改的行
SELECT * FROM products WHERE product_id = 1;

-- 第二步：确认后再修改
UPDATE products
SET unit_price = 209.00
WHERE product_id = 1;

-- 第一步：先看将要删除的行
SELECT * FROM orders WHERE order_id = 2;

-- 第二步：确认后再删除
DELETE FROM orders WHERE order_id = 2;
```

需要按关联表更新或同时删除多个表时，可以使用 MySQL JOIN 写法：

```sql
UPDATE products AS p
JOIN orders AS o ON o.product_id = p.product_id
SET p.active = FALSE
WHERE o.quantity >= 2;

DELETE o, p
FROM orders AS o
JOIN products AS p ON p.product_id = o.product_id
WHERE o.order_id = 2;

DELETE FROM o, p
USING orders AS o
JOIN products AS p ON p.product_id = o.product_id
WHERE o.order_id = 2;
```

`UPDATE ... JOIN` 只修改 JOIN 前的第一个目标表；同一目标行重复命中时只修改一次。
多表 DELETE 会对各目标表行去重，且不支持尾部 `LIMIT`。上面两条 DELETE 是同一操
作的两种 MySQL 语法，只选择其中一种执行。这些复杂写入会整条提交或整条回滚。

`SELECT`、`UPDATE` 和 `DELETE` 的 WHERE 条件及 `JOIN ... ON` 支持相关与非相关
`IN`、`EXISTS` 和标量子查询；UPDATE 的 SET 表达式也可使用相关标量子查询。非相
关子查询在扫描前执行一次，相关子查询按当前外层行求值。写入使用稳定的快照行号收集
变更，任一行求值或约束失败时整条语句回滚。

```sql
UPDATE products AS p
SET p.stock_count = COALESCE((
  SELECT SUM(o.quantity)
  FROM orders AS o
  WHERE o.product_id = p.product_id
), 0)
WHERE EXISTS (
  SELECT 1 FROM orders AS o WHERE o.product_id = p.product_id
);

DELETE FROM orders AS o
WHERE o.order_id IN (
  SELECT candidate.order_id
  FROM orders AS candidate
  WHERE candidate.order_id = o.order_id
    AND candidate.quantity <= 0
);
```

多个 SET 赋值按 MySQL 的左到右顺序求值，后面的表达式能看到前一个赋值产生的新值。

需要限制单次修改或删除量时，`UPDATE`、`DELETE` 支持尾部 `LIMIT`：

```sql
UPDATE products
SET active = FALSE
WHERE product_name LIKE '已下架%'
LIMIT 10;

DELETE FROM products
WHERE product_name LIKE '已下架%'
LIMIT 1;
```

没有 `WHERE` 的 `UPDATE` 或 `DELETE` 会影响整张表。除非这正是你的目的，否则不要
执行。

## 用事务完成一组操作

事务把多条 SQL 作为一组处理：全部成功后提交，发现问题时回滚。事务应尽量短，避免
长时间占用连接并阻塞其他写入。

```sql
BEGIN;

INSERT INTO orders (customer_id, product_id, quantity)
VALUES (2, 1, 1);

UPDATE products
SET active = FALSE
WHERE product_id = 2;

COMMIT;
```

执行后发现条件写错，在 `COMMIT` 前使用：

```sql
ROLLBACK;
```

GBaseLite 支持 `BEGIN`、`COMMIT` 和 `ROLLBACK`。显式事务会取得实例级独占事务门，
同一会话读取自己的事务快照；其他会话的读取和写入都等待到 `COMMIT`、`ROLLBACK` 或
连接断开自动回滚后继续。当前没有可选择的隔离级别，这一行为比 MySQL 默认的
`REPEATABLE READ` 更保守，因此事务必须保持短小。不要依赖复杂的保存点流程作为业务
逻辑的一部分。

## 索引和视图

### 索引

唯一索引会阻止重复值。普通/复合索引可用于基础表查询的左前缀等值、后一列范围和匹配
索引顺序的排序；用 `EXPLAIN SELECT ...` 核对实际索引。函数包裹索引列、OR、复杂分组
和 JOIN 访问仍可能扫描，应继续用真实数据量验证。

```sql
CREATE TABLE customer_contacts (
  id BIGINT NOT NULL,
  phone VARCHAR(32) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY customer_contacts_phone_uq (phone)
);

CREATE UNIQUE INDEX customers_phone_uq ON customers(phone);
CREATE INDEX orders_customer_idx ON orders(customer_id);

SHOW INDEX FROM customers;
EXPLAIN SELECT * FROM orders WHERE customer_id = 1;
```

### 视图

视图适合保存经常使用的查询。GBaseLite 中的视图是只读的，应向基础表写入数据。

```sql
CREATE VIEW customer_order_summary AS
SELECT customer_id, COUNT(*) AS order_count, SUM(quantity) AS item_count
FROM orders
GROUP BY customer_id;

SELECT * FROM customer_order_summary;
SHOW CREATE VIEW customer_order_summary;

CREATE OR REPLACE VIEW customer_order_summary AS
SELECT customer_id, COUNT(*) AS order_count
FROM orders
GROUP BY customer_id;
```

## Navicat 使用要点

1. 新建 MySQL 连接，端口填 `3307`，然后测试连接。
2. 在左侧选择数据库，右键打开“查询”窗口，先执行 `USE 数据库名;` 或直接在对应库下
   运行 SQL。
3. 表需要在 Navicat 网格中编辑或删除单行时，建议定义主键；客户端生成的
   `UPDATE/DELETE ... WHERE ... LIMIT 1` 可直接执行。没有主键的旧表可能被客户端
   识别为只读。
4. 数据传输只勾选需要的表或视图。要完整覆盖目标内容，应在向导中选择删除目标记录
   或删除目标对象，并确认目标库正确。
5. 复制同名表或视图时，GBaseLite 会保留原对象，并将副本命名为
   `原名_copy_YYMMDDNN`，例如 `orders_copy_26072901`。同一原对象当天按 `01` 到
   `99` 递增；不要依赖 Navicat 界面中临时出现的 `原名_copy` 或 `原名_copy1` 名
   称。
6. GBaseLite 到 GBaseLite 的数据传输若执行“读取结构、删除目标、按原名重建”，会保
   留原名；多任务不会把选中的表错误映射为库中的最后一张表。
7. “转储 SQL 文件 -> 结构和数据”应至少包含 `CREATE TABLE`、`INSERT` 和结尾的外键
   检查恢复语句。Navicat 17 通常不会写 `CREATE DATABASE` 或 `USE`；需要可直接创
   建数据库的完整脚本时，使用 `gbaselite backup --database ... --output ...`。

## 管理员操作：账号、备份和恢复

以下操作需要管理员权限，并应在维护窗口或测试环境先演练。

创建只读报表用户：

```sql
CREATE USER IF NOT EXISTS 'reporter'@'%' IDENTIFIED BY 'choose-a-strong-password';
GRANT SELECT ON `gbaselite_training`.* TO 'reporter'@'%';
SHOW GRANTS FOR 'reporter'@'%';
```

备份单个数据库：

```powershell
gbaselite backup --database gbaselite_training --output gbaselite-training.sql
```

包含视图时优先使用内置 `gbaselite backup`。恢复是离线操作，必须先停服务；恢复命
令、完整实例备份和升级注意事项见项目 [README](../README.md#备份与恢复)。

停服复制完整数据目录后，可以先对副本执行只读检查；该命令会核对数据库快照、索引、
用户目录和授权聚合数量，但不会显示对象名、账号、密码、SQL 或行内容：

```powershell
gbaselite inspect-instance --directory D:\backup\data
```

检查通过不等于恢复演练通过；仍应在另一个隔离实例中实际启动副本并核对业务结果。

如果启动错误提示 `store.gob.tmp` 或 `users.gob.tmp` 是恢复候选，不要继续初始化、
删除或覆盖文件。保持停服并先复制整个数据目录，只在副本上验证候选；无法确认时恢复
最近一次已验证的完整数据目录备份，再按需要重放仍在保留窗口内的逻辑 binlog。

`databases/store.gob` 当前是格式版本 `3`，`users/users.gob` 是格式版本 `1`。没有
版本标记的旧文件按版本 `0` 读取并通过各自的迁移注册表转换到当前内存表示；未知的更高版本会
拒绝启动，避免静默丢失字段。`inspect-snapshot` 和 `inspect-instance` 会报告文件
来源版本和当前解析版本，但不会改写现场文件。旧文件只有在下一次正常成功保存时才会
写入版本标记，正式升级前仍须先复制并验证完整数据目录，并按数据迁移流程取得授权。

数据库快照副本可以用以下只读命令解码和比较；不要把现场文件改名成主快照后直接试启
动：

```powershell
gbaselite inspect-snapshot --file D:\recovery-copy\databases\store.gob `
  --compare D:\recovery-copy\databases\store.gob.tmp
```

输出只包含文件路径、大小、UTC 修改时间、SHA-256 和对象总数（包括索引），不包含数
据库名、表名、SQL 或行内容，也不会修改文件。即使候选可以成功解码或修改时间较新，
也不能据此自动认定它是应恢复版本；还要结合完整备份、binlog 连续性和业务数据核
对。

如果服务运行中返回 MySQL 1030 且提示 `fail-closed`，表示快照落盘失败。此后服务会
拒绝所有 SQL，也不会在关闭时用当前内存状态覆盖最后成功快照。先检查磁盘空间、目录
权限和文件系统，复制保留整个数据目录后再重启；把所有已返回错误的写入按“未提交”处
理，核对重启后的数据再决定是否重试。

管理员可在本机管理员终端执行以下只读检查，不需要进入业务库：

```powershell
gbaselite diagnose --config C:\ProgramData\GBaseLite\config.yaml
```

该报告检查监听端口、数据/日志目录、快照、用户目录、TLS、审计和 binlog 状态，不读
取数据内容，也不会显示账号密码。通过已认证的 Navicat、DBeaver 或 MySQL CLI 连
接，还可以执行 `SHOW GLOBAL STATUS` 查看当前/累计连接、查询和 TLS 指标，或用
`SHOW SESSION STATUS LIKE 'Ssl_%'` 确认当前连接实际使用的 TLS 版本与密码套件。

主服务日志位于日志目录的 `gbaselite.log`，默认达到 20 MiB 时轮转，并只保留最近 7
天的 `gbaselite-*.log`；管理员可用 `log.max_size_mb` 和 `log.retention_days` 调
整，`0` 表示永久保留已轮转主日志。该清理不会删除当前主日志、审计日志或 binlog。
审计和 binlog 有各自的保留天数，不要把三项设置混为同一个开关。

## 支持边界和写入安全

以下能力当前不支持：

- 跨库外键
- 触发器
- 存储过程和存储函数
- 事件
- 集群、复制、分片和高可用

还应注意：

- 视图只读，不能通过视图执行写入
- 递归 CTE 当前只支持单个 `WITH RECURSIVE ... UNION ALL`；非递归 CTE 可以声明多个
- `UPDATE ... JOIN` 只修改第一个目标基表；多表 DELETE 不支持 `LIMIT`
- `DECIMAL`、`JSON`、`ENUM/SET`、二进制、空间类型等可接受常见 MySQL 类型声明，但
  并不具备完整 MySQL 类型校验、运算和排序语义
- `information_schema` 和某些 `SHOW` 查询用于客户端兼容，不是完整 MySQL 系统目录
- 长事务会阻塞其他写入；查询和批量导入请拆分处理
- MySQL 8.2 `mysqldump` 可导出数据库、基础表、数据和只读视图，但不会导出
  GBaseLite 账号、授权或当前不支持的触发器、存储过程和事件；导入时会执行
  `/*!版本 SQL */` 中的视图 DDL，普通块注释仍会忽略
- TLS 支持服务器证书和 TLS 1.2 以上加密，但证书签发、续期、CA 信任和客户端 SSL
  设置需要管理员维护；当前不支持客户端证书认证

遇到 `1064` 语法错误时，不要通过反复修改生产 SQL 猜测语法。先在独立测试库中缩小
为最小可复现语句，再对照
[SQL 兼容参考](sql-compatibility-reference.zh-CN.md) 和项目
[README 的兼容范围与限制](../README.md#兼容范围与限制)。

## 清理练习库

确认不再需要练习数据后执行：

```sql
DROP DATABASE gbaselite_training;
```

这会永久删除该库中的表、数据、索引和视图。执行前再次确认当前连接不是业务库。
