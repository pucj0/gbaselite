# Java 业务兼容与 MVCC 维护验证

2026-09-08。本页为固定版本使用与验证说明，不是完整 MySQL 或若依认证。

## 已验证版本与结果

Java 17.0.17、Spring Boot 3.5.0、MyBatis-Plus 3.5.12（包含 MyBatis）、Connector/J 9.3.0、Spring Boot 管理版本的 HikariCP。独立测试工程 `scripts/java-compat`，五组测试全部通过，0 失败、0 错误、0 跳过：

| 测试 | 实际验证 |
| --- | --- |
| preparedAndGeneratedKeys | JDBC 服务端预处理、自增主键回填、超过双精度整数范围的 DECIMAL 精确传输 |
| springCommitRollback | TransactionTemplate 提交与业务异常回滚 |
| plusCrudPaginationAndOptimisticLock | MyBatis-Plus insert/update/select、分页 count、陈旧 version 更新失败、逻辑删除 |
| ruoyiStyleRoleMenuJoin | 用户角色表的 INNER/LEFT JOIN 与带参 count，仅为若依风格 SQL |
| annotatedTransactionAndPoolReuse | 真正由 Spring 代理的 @Transactional 提交/异常回滚，Hikari 归还未提交连接时回滚并恢复 autocommit |

本轮修复了真实驱动暴露的两个握手/协议问题：认证挑战值改为安全随机可打印 ASCII；未协商会话跟踪时省略 OK 包可选 info，避免 Connector/J 的长度编码解析失败。还补充 JDBC 当前隔离级别查询。连接状态位、受影响行数与生成主键保留。

## 重跑步骤（Windows）

需要 Go、JDK 17、Python 3、PyMySQL 和 Maven 3.9.9。脚本支持 `JAVA_HOME` 和 `MAVEN_CMD`；默认 Maven 位于 `.tmp/java-compat/apache-maven-3.9.9/bin/mvn.cmd`，依赖仓库位于 `.tmp/java-compat/m2`。如未安装 PyMySQL，可在选定 Python 环境中安装，或放入脚本已有的 `.tmp/db-comparison/packages`。

```powershell
New-Item -ItemType Directory -Force .tmp/java-compat
# JAVA_HOME 指向本机 JDK 17；MAVEN_CMD 指向本机 mvn.cmd
$env:JAVA_HOME = 'C:\Java\jdk-17'
$env:MAVEN_CMD = 'C:\Tools\apache-maven-3.9.9\bin\mvn.cmd'
go build -o .tmp/java-compat/gbaselite.exe ./cmd/gbaselite
python scripts/java-compat/run.py
```

脚本仅监听 127.0.0.1 的随机可用端口，使用独立临时数据目录和测试密码，结束后停止自己创建的进程并删除该数据目录；不会修改正式 `data/`。固定开启单机 WAL，Go 内存软目标 64 MiB、2 个执行线程。Maven 首次运行需下载依赖。端口选择与启动之间存在很短竞争窗口，遇到端口占用可重跑。

结果保留在 `.tmp/java-compat/integration.json`、`integration.log` 和 `target/surefire-reports/`；日志不是产品发布报告。不要把脚本中的测试密码用于业务部署。

## 应用接入边界

JDBC URL 使用 MySQL 方言并开启 `useServerPrepStmts=true`。生产连接的 TLS 与账号按部署配置，不照搬测试中的 useSSL=false。默认事务提供 MVCC 快照隔离，`@@tx_isolation` / `@@transaction_isolation` 返回 REPEATABLE-READ 用于 JDBC 查询；没有 InnoDB 间隙锁语义，仍可能写偏斜。隔离级别切换、保存点、锁定读和完整事务传播矩阵尚未支持或验收。DDL 使用原子事务目录切换，不仿真 MySQL 隐式提交。

尚未运行完整若依应用的初始化、登录、菜单、部门数据权限、导入导出流程；Druid、不同 Connector/J/Spring 版本、复杂分页 count 派生查询、批量部分失败、自增空洞、连接故障与事务超时仍须逐项验证。五组测试通过不能代表这些范围已完成。

## MVCC 管理 SQL

仅适用于单机 MVCC；要求 autocommit 开启、没有活动事务，及全局 FILE/SELECT/CREATE/DROP 权限。目标目录不得存在，父目录应预先准备：

```sql
BACKUP MVCC TO 'E:/backup/test-20260908';
GC MVCC;
COMPACT MVCC TO 'E:/migration/compact-20260908';
-- 在隔离恢复实例中演练；恢复会替换全部 MVCC 数据并使旧事务失效
RESTORE MVCC FROM 'E:/backup/test-20260908';
```

备份以同一物理快照保存 `mvcc.db` 与 `backup.json`（SHA-256、大小和头版本）。仅备份 MVCC 数据，用户、配置和日志需独立保留。备份允许业务写入，但 bbolt 映射增长可能等待；未完成标记、损坏校验拒绝恢复，校验失败保留健康事务。

GC 保留活动快照需要的版本。压缩导出只生成新平铺副本并保留源目录，必须在校验后停机切换，原文件不自动缩小；尚未清除全部废弃表空间。Raft 集群不能使用这些单机维护入口代替集群恢复流程。

## 架构与扩展

跨存储层、优化器、执行器、事务和兼容性的影响，以及每项能力的优先级、核心结构、改造位置和风险，见[MVCC 业务数据库演进设计](MVCC业务数据库演进设计.md)。不得把设计中的未来扩展当作已实现能力。
