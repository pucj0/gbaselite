# GBaseLite、MySQL、PostgreSQL、SQLite 对比报告

日期：2026-09-08。实测窗口：{{START}} 至 {{END}}。本报告使用当前工作区重新构建的GBaseLite，与其余三个数据库在同一轮重新测试；不沿用旧报告数值。

## 1. 结论与百万行摘要

本次完成 **4 个数据库 × 5 档行数 × 3 轮 = 60 个隔离测试**。全部 SQL 阶段成功；每次整表 UPDATE 的影响行数、更新后的 COUNT/SUM 通过校验，临时测试库全部清理。本次 GBaseLite 使用当前工作区构建的 **MVCC 单机后端**，百万行 UPDATE 三轮均完成，未触发历史 256 MiB 写集限制。

**GBaseLite 目前不能称为全面兼容 MySQL 的海量生产数据库，也没有全面超过 SQLite。** 百万行 UPDATE 耗时中位数是本次 SQLite 的约 {{G_UPDATE_RATIO}} 倍。吞吐、CPU、内存、磁盘和 SQL 能力需要分别评估，不能用某一项优势代替整体结论。

![百万行性能摘要](图片/百万行性能摘要.png)

以下均为三次独立运行的中位数。RSS 为采样工作集，Private 为私有提交内存；二者不能相加或互相替代。

| 数据库 | INSERT / 秒 | 原子 UPDATE / 秒 | RSS 峰值 / MiB | Private 峰值 / MiB | 更新后目录 / MiB |
| --- | ---: | ---: | ---: | ---: | ---: |
{{SUMMARY}}

百万行写入的三轮最小至最大值如下；三轮样本不能提供可靠的尾部稳定性或置信区间结论。

| 数据库 | INSERT 范围 / 秒 | UPDATE 范围 / 秒 |
| --- | ---: | ---: |
{{VARIABILITY}}

所有轮次均保留，没有剔除慢样本。结果受操作系统缓存、同步I/O和后台活动影响；本轮没有完整的缺页或设备跟踪，不能对波动强行归因。

{{CURRENT_FINDINGS}}

## 2. 环境、版本和公平性边界

{{HOST_ENV}}

{{VERSIONS}}

这些是本机实际测试版本，**不是各产品最新版本排名**。MySQL 能力部分引用 8.4 官方文档作为能力背景，性能数字仍来自本次 8.2 实例。操作系统文件缓存未清空、未限额；测试期间没有刻意停止其它用户进程。

| 数据库 | 主要资源与持久性配置 |
| --- | --- |
| GBaseLite | `storage.mode: mvcc`；默认 nested 布局；`local_wal=false`（默认），原同步提交路径；Go 内存软目标 64 MiB；Windows 工作集配置 64 MiB；`max_procs: 2`；排序 4 MiB、结果 16 MiB、查询临时盘 256 MiB；`transaction_write_mb: 0`；查询超时 120 秒；持久库同步提交 |
| MySQL / InnoDB | buffer pool 64 MiB；最多 16 连接；关闭 performance_schema 和 binlog；`innodb_flush_log_at_trx_commit=1`；redo 容量 100 MiB |
| PostgreSQL | shared_buffers 64 MiB；work_mem 4 MiB；最多 16 连接；fsync、synchronous_commit、full_page_writes 开启；禁用查询并行；语句超时 120 秒 |
| SQLite | WAL；synchronous=FULL；页缓存预算 64 MiB；mmap_size=0；进程内 Python sqlite3 调用 |

**这些不是等总内存、等 CPU 硬限额配置。** 64 MiB 分别约束运行时、工作集或某种缓存；数据库其它内存不受相同限制。仅 GBaseLite 设置了两路 Go 执行并行度。SQLite 没有 TCP/服务进程开销，其余三个通过本机 TCP 和驱动执行。因此本报告回答“这些配置和访问方式下表现如何”，不回答“在完全相同总资源下哪个内核必然更快”。本轮也未测试启用 binlog、复制或备份时的成本。

环境原始记录见[环境 JSON](数据/四数据库对比-2026-09-08/环境.json)，每个结果 JSON 含实际配置、版本及命令。

## 3. 测试方法与统计口径

- 规模为 10、1,000、10,000、100,000、1,000,000 行；每个组合三轮重新创建独立数据库。固定随机种子打乱每轮 20 个组合，串行运行，避免几个数据库测试互相抢资源。
- 表包含 `id` 主键、`v INT NOT NULL`、`payload VARCHAR(128) NOT NULL`；v 初始为 1，payload 为 128 个 ASCII 字符 x。SQLite 使用 `INTEGER PRIMARY KEY`，其它数据库使用 BIGINT 主键。只有主键，没有额外普通索引。
- INSERT 每批 250 行、自动提交；百万行共 4,000 次持久化事务。计时不包括建表和实例启动。吞吐为每轮行数除以该轮写入耗时，再取三轮中位数，不是并发事务 TPS。
- 装载后先核对 COUNT/SUM；主键点查预热 20 次，之后测 200 次固定随机主键。每轮计算 P50/P95/P99，再对三轮各自分位数取中位数；P95 沿用脚本的有序 200 样本第 190 项，P50 为中间两项的均值。P99 采用有序 200 样本中的第 198 项；200 次样本不足以推断生产尾延迟。
- 范围读取返回 `min(100, 行数)` 条有序 id/v 并核对内容；聚合为 `SELECT COUNT(*), SUM(v) FROM items`。两者各测三次取中位数，然后汇总三轮。读取属于装载后的热读，没有冷缓存测试。
- UPDATE 为单条 `UPDATE items SET v=v+1`，不拆事务。计时包含提交，随后核对影响行数 N、COUNT=N、SUM(v)=2N。计时不含后续验证。校验不等于全字段逐行一致性、崩溃恢复或断电持久性证明。
- 百万行的文本载荷合计为 128000000 字节（约 122.1 MiB），数据仍远小于主机约 64 GiB RAM。这不是数据超过物理内存的测试，也不是专门验证超过 256 MiB 写集的宽行探针；不能据此推导事务或数据库容量无限。
- CPU 用进程累计 user+system 时间差；INSERT 和 UPDATE 分开列示。全流程 CPU 包含建表、查询和校验等；平均单核等效 CPU%=全流程 CPU 秒/全流程墙钟秒×100，100% 表示占用一个逻辑核的等效时间，不是整台机器总占用。小任务可能低于约 15.625 ms 计时粒度，0 不表示零成本。
- 服务型数据库统计服务进程及其子进程，不包括 Python 客户端；SQLite 则包括 Python 进程。100 ms 采样可能漏掉短时峰值。PostgreSQL 进程树 RSS 相加可能重复统计共享页，不能当作去重后的物理内存。Private 是私有提交量，可能并未全部驻留物理内存。
- 磁盘数字为数据目录文件逻辑长度总和，包含目录内日志、预分配和保留版本；不是纯表大小、累计 I/O、设备实际分配量或临时盘峰值。相对空库增长只是辅助观察，不能完全消除引擎初始预分配差异。没有统一执行手工压缩/回收。

## 4. SQL 执行速度

![SQL 性能对比](图片/SQL性能对比.png)

### 写入与原子更新

{{WRITE_TABLE}}

### 查询延迟

{{READ_TABLE}}

图中使用对数坐标，阴影为三轮最小至最大。完整每轮数值和波动范围见[三轮指标 CSV](数据/四数据库对比-2026-09-08/三轮指标汇总.csv)。这些 SQL 没有覆盖 JOIN、分组、排序溢写、JSON 运算、并发冲突、宽行或大结果传输。

## 5. CPU、物理工作集与磁盘

![资源占用对比](图片/资源占用对比.png)

### CPU

{{CPU_TABLE}}

CPU 秒表示完成工作的处理器时间；耗时较长且平均 CPU 较低，也可能来自同步 I/O 等待，不能据此认定效率更好。跨数据库 CPU 口径含客户端的差异见前文。

### 内存

{{MEMORY_TABLE}}

本次 GBaseLite 百万行 RSS 峰值的三轮中位数为约 {{G_RSS}} MiB；64 MiB 配置不是采样峰值或整进程资源保证。工作集配置不等于整机物理内存承诺，实测峰值以表格为准；Go 软目标也不是硬上限。强行压低驻留页可能增加缺页、I/O 和延迟。若部署需要严格总物理内存约束，应在隔离虚拟机或 cgroup 配额下，包含操作系统缓存、客户端和故障恢复负载重新验证。

### 文件占用

{{DISK_TABLE}}

GBaseLite 的 bbolt 页分配、版本组织和旧版本保留仍是空间优化重点；整表更新还会产生临时写集。本表没有测临时写集峰值，容量规划不能只按“更新后目录”预留空间。

## 6. 功能、兼容性和高可用

![功能与适用性对比](图片/功能与适用性对比.png)

{{CAPABILITIES}}

成熟引擎的索引与并发背景见 [PostgreSQL 索引](https://www.postgresql.org/docs/17/indexes.html)、[PostgreSQL MVCC](https://www.postgresql.org/docs/17/mvcc.html)、[InnoDB 多版本](https://dev.mysql.com/doc/refman/8.4/en/innodb-multi-versioning.html)和 [SQLite 查询规划](https://www.sqlite.org/queryplanner.html)。这些资料说明产品机制，不提供本机性能排名。

GBaseLite 的功能判断依据为仓库[MVCC 功能清单](../使用文档/MVCC复制与高可用.md)、[SQL 兼容参考](../使用文档/SQL兼容性参考.md)和[JSON 兼容说明](../使用文档/JSON兼容性说明.md)。默认 snapshot/paged 的较广 SQL 支持不能套用到本次 MVCC 性能结果。当前 MVCC 已支持普通/唯一/联合索引、有限覆盖访问、EXPLAIN、INNER/LEFT JOIN、GROUP BY/HAVING、常用 ALTER 和后建索引、CHECK、RESTRICT/NO ACTION 外键，以及单机 GC、压缩副本、在线备份恢复。其余 JOIN、派生查询、CTE、窗口、保存点与锁定读仍有限制，不能等同完整 MySQL。普通二级索引以安全左前缀等值访问为主；优化器仍按规则选路，暂无统计成本模型。DDL 为影子重建，可能因并发写冲突失败；分组尚无外排，压缩需要显式切换副本才释放旧文件。详见[当前 MVCC 架构与限制](../使用文档/MVCC业务数据库演进设计.md)。

前序独立 Java 验证已通过 Java 17 / Spring Boot 3.5.0 / MyBatis-Plus 3.5.12 / Connector/J 9.3.0 的五组测试，含注解事务、连接池回滚、分页与乐观锁；它不是本轮四库测试结果，也未覆盖完整若依应用。[Java 验证范围](../使用文档/Java业务兼容验证.md)。

MySQL 的 Group Replication 可配置单主模式和自动选主，客户端切换仍需要 Router、连接器或其它入口；这些能力不是启动单个 MySQL 就自动获得。[MySQL Group Replication](https://dev.mysql.com/doc/refman/8.4/en/group-replication.html)

PostgreSQL 提供复制与备用服务器机制，自动故障切换还需合适的监测与编排；备份包括逻辑转储、文件系统备份和连续归档等路径。[PostgreSQL 故障切换](https://www.postgresql.org/docs/17/warm-standby-failover.html)、[备份与恢复](https://www.postgresql.org/docs/17/backup.html)

SQLite 面向嵌入式和本地存储，同一个数据库文件一次只有一个写入者；它不是“只能存少量数据”，但网络多客户端和大量并发写需要不同架构。其在线 Backup API 可在运行期间复制数据库。[SQLite 适用场景](https://www.sqlite.org/whentouse.html)、[Backup API](https://www.sqlite.org/backup.html)

### JSON_OBJECT 能否直接迁移

GBaseLite 已支持 JSON 对象构造子集，但 JSON 列仍按文本保存，路径、函数、错误和字符排序规则均有边界，不兼容所有 MySQL JSON 语义。

```sql
-- GBaseLite / MySQL
SELECT JSON_OBJECT('id', 1, 'name', 'demo');
-- PostgreSQL 的一种对应写法
SELECT jsonb_build_object('id', 1, 'name', 'demo');
-- SQLite
SELECT json_object('id', 1, 'name', 'demo');
```

这些是对应功能示例，本轮性能测试没有运行 JSON 差分测试。MySQL 原生 JSON、PostgreSQL json/jsonb、SQLite JSON/JSONB 和 GBaseLite 文本表示并不具备相同存储格式或完整类型语义。[MySQL JSON](https://dev.mysql.com/doc/refman/8.4/en/json.html)、[PostgreSQL JSON 函数](https://www.postgresql.org/docs/17/functions-json.html)、[SQLite JSON](https://www.sqlite.org/json1.html)

### GBaseLite 三节点意味着什么

三份数据副本、固定三个投票节点，多数派确认写入；可自动选主和重启追赶。它不分片，也不把三台机器容量相加。单机默认只启动一个 server，复制需显式配置。单个入口代理仍可能成为单点，切换时旧连接断开，客户端必须确认事务结果并重连；不会自动重放 SQL。账号文件不经过 Raft。当前没有本报告支持的 RPO/RTO、长期容灾或生产可用性承诺，本轮没有测试集群。

## 7. 是否满足“海量数据库”

**结论：GBaseLite 目前不满足“已验证的海量生产数据库”这一定位。** “海量”没有仅按行数划定的统一门槛，需要结合数据字节量、并发、延迟、可用性与恢复目标。

**本次只能证明给定配置下，四个数据库均完成最大 100 万行的指定单客户端工作负载。** 百万行文本载荷仅约 122 MiB，远低于本机约 64 GiB RAM，且未清空系统缓存；这不是数据超过内存的实验。128 字节文本的百万行测试，不能代替亿级行数、TB 数据、高并发、跨机复制和故障恢复认证。MySQL、PostgreSQL 有成熟的服务端关系数据库能力，但其产品容量上限也不是本次机器的可交付性能；SQLite 的单文件与单写者模型适合不同场景，不能因数据容量大就当作分布式系统。[InnoDB 限制](https://dev.mysql.com/doc/refman/8.4/en/innodb-limits.html)、[PostgreSQL 容量限制](https://www.postgresql.org/docs/17/limits.html)、[SQLite 适用范围](https://www.sqlite.org/whentouse.html)。GBaseLite 仍需补齐以下能力及证据。

| 验证维度 | 本次证据 | 后续验收建议 |
| --- | --- | --- |
| 数据规模 | 最大百万行，单表固定窄行 | 千万/亿行与宽行，数据明显超过 RAM；记录长期增长及维护窗口 |
| 内存与 CPU | 100 ms 采样，单客户端 | 相同总资源配额，多并发与长事务；包含缓存和临时文件；记录 OOM/取消行为 |
| 并发正确性 | UPDATE 影响行数和 COUNT/SUM | 冲突、唯一键、写偏差、长读与 GC 交错；按业务隔离语义验收 |
| 可靠性 | 正常 SQL 返回 | 强杀、断电、磁盘满、损坏、提交未知、重复请求，验证可见性和幂等 |
| 运维与恢复 | 本轮未测试 | 一致性备份、恢复演练、时间点恢复、升级回滚、空间回收 |
| 高可用 | 本轮未测试 | 三台机器分区、丢包、主节点故障、落后副本追赶、入口故障；量化 RPO/RTO |
| SQL 与生态 | 本轮基本单表 SQL；前序另有 Java 验证 | 为已实现的二级/复合索引、JOIN、分组建立四库差分与性能矩阵，补完整业务回归 |

## 8. GBaseLite 当前边界与优化优先级

本轮二进制包含只读目录解码缓存、flat 正向顺序扫描及 Navicat 虚拟库选择/元数据修复。测试采用默认 nested 布局，flat 优化没有启用；本轮单表负载也不是 Navicat UI 验收。报告不把前序读取器微基准的 11～14 倍收益当成端到端 SQL 提升。当前 GBaseLite 继续以 MySQL 业务语法和协议兼容为目标，借鉴 PostgreSQL 内部优化不意味着支持其 SQL 方言。


1. **以当前大事务实测剖析写放大。** 将临时写集、版本安装、fsync 与发布分别采样，评估覆盖索引维护成本。现有顺序 WAL 保留数据页同步，本轮未开启，不把新增 WAL 当成已验证的提速开关。
2. **扩展 GC 与空间回收。** 已有按活动快照保留历史的 GC 和平铺导出；继续补完整废弃表/索引空间回收、后台节流和可观测性，量化维护期间查询延迟。
3. **完善优化器与 JOIN/分组路径。** 已有规则选路和普通覆盖索引；下一步是分布统计、成本选择、二级范围及更高效 JOIN、高基数分组外排，用倾斜数据和复杂谓词检验。
4. **降低端到端 CPU 与真实工作集。** 原生整数批次路径已接入；仍需结合采样剖析 DECIMAL/文本/大结果等回退路径。对进程 RSS、私有提交量、缺页与总 CPU 分开设定目标，不将 Go 堆代替物理内存。
5. **补齐业务与运维验收。** 单机备份恢复和固定版本 Java 链路已实现并单独验证；还需完整若依、复制分区故障、磁盘故障、升级及备份恢复混合压力。先固定业务 SLA，再扩大千万/亿行规模。

默认 `transaction_write_mb=0` 只取消逻辑写集总量上限；约 128 KiB 的逐层写缓冲、单机大事务约 256 KiB 的物理安装预算及复制路径原预算仍保留。SQL 文本约 1 MiB、编码单行/表定义 48 KiB，以及磁盘、查询超时等限制仍然存在。更大的事务必须另测，不能从百万行成功推导为任意事务无限成功。

## 9. 原始数据与复现

- [三轮原始数据目录](数据/四数据库对比-2026-09-08/)、[执行顺序与程序哈希](数据/四数据库对比-2026-09-08/运行记录.json)、[结果校验](数据/四数据库对比-2026-09-08/结果校验.json)。
- [官方资料核对记录](数据/四数据库对比-2026-09-08/资料来源.json)。
- [三轮指标 CSV](数据/四数据库对比-2026-09-08/三轮指标汇总.csv)、[SHA-256 清单](SHA256SUMS.txt)。
- [调度脚本](../../scripts/benchmarks/run_comparison.py)、[测试脚本](../../scripts/benchmarks/compare_databases.py)、[报告生成器](../../scripts/benchmarks/render_comparison.py)、[交付校验器](../../scripts/benchmarks/validate_comparison.py)、[依赖](../../scripts/benchmarks/requirements.txt)。
- 配图同时保留 PNG 和 SVG：[百万行摘要 SVG](图片/百万行性能摘要.svg)、[SQL SVG](图片/SQL性能对比.svg)、[资源 SVG](图片/资源占用对比.svg)、[功能 SVG](图片/功能与适用性对比.svg)。

实测候选二进制 SHA-256：`{{BINARY_HASH}}`。执行时测试脚本 SHA-256：`{{HARNESS_HASH}}`。当前构建相关源文件摘要见[源码哈希清单](数据/四数据库对比-2026-09-08/源码/源码哈希.json)。执行时脚本副本保存在[源码目录](数据/四数据库对比-2026-09-08/源码/)，用于核对取证；本次取证记录只对应本报告的候选与测试窗口。源码副本依赖原仓库布局，不应直接在归档目录执行。

Windows 复测前准备 Python 依赖、Go、MySQL 和 PostgreSQL 独立运行文件。当前脚本使用本机路径：MySQL 位于 `D:/MySQL/MySQL Server 8.2/bin`，PostgreSQL 位于 `.tmp/db-comparison/pgsql/bin`，Python 包位于 `.tmp/db-comparison/packages`。其它机器需调整路径；Linux 还需调整进程资源采样与 Windows 工作集配置，不能直接套用本报告结果。

```powershell
# 从仓库根目录执行；使用新的输出目录，脚本拒绝覆盖已有测量
# python 应指向已安装依赖的解释器
python -m pip install --target .tmp/db-comparison/packages -r scripts/benchmarks/requirements.txt
go build -o .tmp/four-db-comparison/gbaselite.exe ./cmd/gbaselite
python scripts/benchmarks/run_comparison.py --binary .tmp/four-db-comparison/gbaselite.exe --output .tmp/new-comparison/数据/四数据库对比-2026-09-08
# 同时采集环境.json、源码及哈希，再渲染独立报告
python scripts/benchmarks/render_comparison.py --output .tmp/new-comparison
```

测试仅创建 `.tmp` 中的独立目录和临时监听端口，关闭自己启动的进程后清理；不连接或删除正式 `data/`。渲染器要求 60 组成功、三轮结束、影响行数正确且临时库清理成功才生成本报告；失败数据必须保留并单独解释，不能填成零或混入成功中位数。重新测试还需重新采集环境记录，历史环境与程序哈希不能直接沿用。
