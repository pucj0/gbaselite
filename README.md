# GBaseLite

GBaseLite 是一个使用 Go 编写、默认单机运行的轻量级关系型数据库服务。它不依赖 MySQL 或
PostgreSQL，使用自己的存储文件持久化数据，并通过 MySQL 协议向 Navicat、DBeaver、
JDBC、Go MySQL 驱动等客户端提供服务。

当前版本：`1.1.2`（工作区开发改造；历史发布包以对应发布说明为准）

**MVCC 是唯一运行事务引擎。snapshot/paged 仅作为离线迁移源格式保留。**

> GBaseLite 是 MySQL 兼容子集，不是 MySQL 的完整替代品。生产使用前请阅读
> [兼容范围与限制](#兼容范围与限制)，并使用真实业务 SQL 做完整验证。

## 目录

- [使用者入口](#使用者入口)
  - [SQL 使用教程](docs/使用文档/SQL使用教程.md)
  - [SQL 兼容参考](docs/使用文档/SQL兼容性参考.md)
- [项目定位](#项目定位)
- [MVCC 范围与写入优化](#mvcc-范围与写入优化)
- [数据库实测对比](#数据库实测对比)
- [主要能力](#主要能力)
- [快速开始](#快速开始)
- [配置](#配置)
- [部署](#部署)
- [升级与回滚](#升级与回滚)
- [SQL 示例](#sql-示例)
- [兼容范围与限制](#兼容范围与限制)
- [旧数据迁移](#旧数据迁移)
- [备份与恢复](#备份与恢复)
- [CLI](#cli)
- [持久化布局](#持久化布局)
- [项目结构](#项目结构)
- [开发与测试](#开发与测试)
- [本地发布打包](#本地发布打包)
- [GitHub 与 Docker Hub 发布](#github-与-docker-hub-发布)
- [贡献规范](#贡献规范)
- [开源许可证](#开源许可证)
- [路线图](#路线图)

## 使用者入口

当前 SQL 范围以本页[兼容矩阵](#兼容范围与限制)和 [MVCC 文档](docs/使用文档/MVCC复制与高可用.md)为准。
[SQL 教程](docs/使用文档/SQL使用教程.md)与[旧兼容参考](docs/使用文档/SQL兼容性参考.md)
包含历史功能，使用前需核对当前矩阵。旧实例请先阅读[旧数据迁移](#旧数据迁移)。

## 项目定位

GBaseLite 面向以下场景：

- 需要 MySQL 客户端和驱动直连，但不希望部署完整 MySQL 的小型服务
- 开发、测试、演示和教学环境
- 低资源单机部署
- 数据库协议、解析器、执行器和存储引擎的学习与实验
- 需要针对 Navicat、DBeaver 或现有业务 SQL 做定向兼容的项目

对于纯嵌入式本地应用、强可靠生产系统或高并发写入场景，应优先评估 SQLite、MySQL、
PostgreSQL 等成熟数据库。

## MVCC 与高可用入口

新建实例和默认 Open 均使用 MVCC：有界写缓冲、磁盘暂存、行级快照和冲突检测。
可选三节点 Raft 及连接代理仍属实验能力。旧目录不会自动迁移，必须显式转换到新目录。
完整配置见 [MVCC 与复制](docs/使用文档/MVCC复制与高可用.md)。

## MVCC 范围与写入优化

开发版本的新建单列 INT/BIGINT 主键表使用带格式标识的8字节有序主键，支持安全整数
比较、AND与BETWEEN的范围访问，以及主键正反向排序和LIMIT提前结束。查询仍检查
剩余WHERE条件，并按快照合并未提交写集和删除。文本主键、复合主键、危险隐式转换
和旧键格式继续使用原有路径。已有单列/复合唯一索引支持安全整数全键等值定位；
普通二级索引仍未实现范围访问。

新MVCC表采用紧凑行编码，投影可跳过不需要的文本构建；每层写集有约128 KiB编码
缓冲，超过后批量暂存磁盘。完整命令计量不超过64 KiB的小型单机事务一次原子同步
提交，持久库仍保持同步；单机大事务省去持久pending复制和清理，Raft仍分块暂存再发布。原256 MiB写集硬上限
已取消，新增 `resources.transaction_write_mb`，默认0不限制逻辑写集总量；内存缓冲不随之放大。
这些缓冲预算不是Go堆或物理内存的总硬上限。

旧表不会自动重写，新版本保留旧键及Gob行读写。新格式表不能交给旧版本二进制打开，
不支持直接降级或混合版本Raft滚动升级；所有节点须支持相同格式。迁移需通过受支持
SQL写入独立新库并校验，目前没有MVCC格式在线迁移CLI。本次未改动业务实例。

最新实现使用紧凑临时写集和语句写集所有权转移，减少编码与重复复制；大事务未发布
版本的序号持久保留，防止崩溃后复用。临时编码不改变历史行格式及Raft JSON协议。
复制SQL不再受隐藏30秒整句时限约束，仍受用户查询期限限制，多数派探测最多5秒。
目前仍未全面超过SQLite；存储空间、索引与版本回收仍需继续优化。

扫描路径在每个物理读事务内复用B+树根桶与固定16槽提交标记缓存，减少重复查找；
缓存不跨物理事务保留，未发布版本仍不可见。空写集层跳过逐行临时键构造，分页游标
每批只复制一次续读键。无分组聚合复用一条解码行缓冲，普通结果行仍独立持有值。
行/日志格式、同步落盘、逻辑快照和每批读取预算保持原有语义；不提高内存配置。
速度、CPU和内存实测见下方当前四数据库对比报告。

开发者可剖析独立临时库的插入、五次聚合与原子更新：

```powershell
go run ./scripts/internal/mvccperfprobe -rows 100000 -profiles -output .tmp/mvcc-perf/probe.json
```

探针使用Windows工作集64 MiB、Go软目标64 MiB和GOMAXPROCS=2，记录CPU profile及累计
分配量；累计分配量不是驻留内存。跨平台使用前需调整工作集配置。结果放在`.tmp`，
统一对照结果仍归入下方报告，不另建重复测试报告。

单机大事务提交：先在提交锁内校验全部行、唯一键与表结构冲突，再从有界
临时写集分批同步写入不可见版本，最后原子发布。省去单机持久pending的重复写入、
JSON编解码和清理；不增加写缓冲、不关闭fsync、不改变行格式或Raft协议。每批同时
持久保留版本序号，发布前取消或崩溃后不能因序号复用误显示部分更新。崩溃后未发布
版本仍可能占磁盘，此改造不等于自动GC。该快路径仅用于直接单机提交，Raft照旧。
基准对照脚本 `scripts/benchmarks/compare_scan_optimization.py` 支持 `--before`、`--after`
和 `--output` 指定两份候选程序与独立结果目录；检测到已有结果时拒绝覆盖。

减少写入分配：单机大事务在冻结的临时写集只读事务内借用编码字节，
全量校验后按原预算提交，尾批也在只读事务关闭前消费完毕；不把临时映射切片传出其
有效期。UPDATE/DELETE复用扫描行缓冲，UPDATE的新旧行仍分开，编码写集后才复用。
持久化格式、同步落盘、原子发布、冲突检测和内存配置不变。累计分配减少不等于
物理内存同比减少，实际速度及资源表现以最新四数据库对比报告为准。

进一步减少读写路径重复工作：键校验不构造临时键，实际键按精确长度一次分配，空写集
点查不重复构造键。无分组聚合的直接列引用在执行前解析位置，仍使用原JSON、NULL、
DECIMAL与排序规则转换，复杂表达式继续走通用求值。分页预算、同步落盘和文件格式
不变；版本游标维持原有定位方式，性能需按工作负载验证。
最新四库报告对应其记录的测试程序哈希，本次后续代码改动不自动计入该报告；独立
增量对照保存在`.tmp/mvcc-perf4`与`.tmp/mvcc-perf4b`，不新增重复报告，也不覆盖四库原始测量。

修订版在同配置三轮交错对照中，百万行全流程CPU约降3.1%、INSERT耗时约降1.7%；
十万行UPDATE约降4.3%。百万行UPDATE约慢0.8%、RSS仍约71.8 MiB，Private峰值约升5.1%；
点查和范围读取也有波动，不能宣称所有SQL或内存指标均改善。曾尝试的最新版本优先
读取已因综合收益不足撤回，其原始对照仍保留；以上数字仅对应`.tmp/mvcc-perf4b`修订版。

## 数据库实测对比

[GBaseLite、MySQL、PostgreSQL、SQLite 四数据库对比报告](docs/报告/四数据库对比报告.md)
包含10、1,000、1万、10万、100万行的五档三轮、共60组独立实测，覆盖SQL速度、
CPU、工作集、私有内存和磁盘。当前报告使用 2026-09-08 19:49—20:00 新跑的60组测量；百万行摘要、SQL、资源、功能四张图同时提供 PNG/SVG，保留原始数据、源码与哈希。GBaseLite使用MVCC单机后端；
不能将其性能与snapshot/paged的较广SQL能力混用，百万行成功不代表海量生产认证。

文档按用途分开：[使用文档](docs/使用文档/README.md)保存教程与配置说明；
[报告](docs/报告/四数据库对比报告.md)保存当前统一对比，图片与数据置于其子目录。
上一版报告和 PDF 已归档到 `.tmp/four-db-current/上一版报告`，不与当前 MD/配图混用。
四库调度脚本支持`--binary`与`--output`，报告生成器支持`--output`；可先在`.tmp`
生成并核验完整报告，再替换报告目录。已有测量不会被调度脚本覆盖。
运行 `python scripts/benchmarks/validate_comparison.py --output docs/报告` 可核对60组原始数据、360项汇总、图片、链接及哈希。
本轮百万行 GBaseLite 插入/原子更新中位数约23.73/12.35秒，RSS峰值中位数约71.77 MiB；
存在明确波动与访问方式差异，不表示全面超过SQLite或已完成海量生产认证。

## 主要能力

- MySQL 协议、认证、TLS、Prepared Statement 与常用元数据
- MVCC 快照隔离、原子提交、断连与语句失败回滚
- 表/索引 DDL、受支持 CRUD、聚合、分组、INNER/LEFT JOIN
- 精确 DECIMAL、JSON、约束与单机账号授权
- 单机 MVCC 备份恢复、历史 GC、实验性复制与代理
- Windows/Linux 部署、服务管理、Docker 与 MSI

限制与未支持语法见[当前矩阵](#兼容范围与限制)。

## 快速开始

### 环境要求

- Go 1.23 或更高版本
- 可选：Docker Engine 与 Docker Compose
- 可选：WiX Toolset 5 和 .NET 8 SDK，用于构建 Windows MSI
- 可选：Syft，用于生成 SPDX JSON SBOM
- 可选：MySQL 客户端，用于协议连接验证

### 从源码构建

Windows PowerShell：

```powershell
go mod download
New-Item -ItemType Directory -Force .\.tmp, .\bin | Out-Null
go build -trimpath -o .\.tmp\gbaselite.exe .\cmd\gbaselite
# 替换前先停止已有服务；新安装可直接复制
Copy-Item .\.tmp\gbaselite.exe .\bin\gbaselite.exe
.\bin\gbaselite.exe start --config .\config.yaml
.\bin\gbaselite.exe healthcheck --host 127.0.0.1 --port 3307
```

Linux/macOS：

```bash
go mod download
mkdir -p ./.tmp ./bin
go build -trimpath -o ./.tmp/gbaselite ./cmd/gbaselite
# 替换前先停止已有服务
cp ./.tmp/gbaselite ./bin/gbaselite
./bin/gbaselite start --config ./config.yaml
./bin/gbaselite healthcheck --host 127.0.0.1 --port 3307
```

不传子命令等同于执行 `start`。如果 PID 文件对应的服务已经运行，`start` 会先停止
旧进程再启动新进程，而不是直接报告端口占用错误。

```powershell
.\bin\gbaselite.exe
```

### 连接数据库

默认连接参数：

```text
Host:     127.0.0.1
Port:     3307
Username: root
Password: change-this-password
```

MySQL 客户端：

```bash
mysql --protocol=tcp -h 127.0.0.1 -P 3307 -u root -p
```

GBaseLite 自带客户端使用兼容 MySQL 的连接参数；`-p` 不带密码时会无回显提示输入，
默认连接 `127.0.0.1:3307`：

```powershell
gbaselite -u root -p
gbaselite -h 127.0.0.1 -P 3307 -u root -p -D yuanma-auth1
gbaselite -u root -p -e "SELECT VERSION()"
```

也可以显式使用 `gbaselite client` 或 `gbaselite connect`。支持 `-u/--user`、
`-p/--password`、`-h/--host`、`-P/--port`、`-D/--database` 和 `-e/--execute`；为
避免密码出现在命令历史和进程参数中，不建议使用 `-p密码` 或 `--password=密码`。
Windows 下客户端启动时会将当前控制台的输入和输出代码页切换为 UTF-8，中文库名、表
名、字段名、查询结果和交互输入可在 CMD、PowerShell 与 Windows Terminal 中正常显
示。交互模式使用 MySQL 风格的对齐表格，提示符显示当前数据库；每条成功或失败的
SQL 都会显示包含客户端取数时间在内的毫秒/秒耗时，`USE database` 成功后显示
`Database changed`。

JDBC：

```text
jdbc:mysql://127.0.0.1:3307/test?useUnicode=true&characterEncoding=utf8
```

Go DSN：

```text
root:change-this-password@tcp(127.0.0.1:3307)/test?charset=utf8mb4
```

## 配置

默认配置文件为 `config.yaml`：

```yaml
server:
  host: 127.0.0.1
  port: 3307
  max_connections: 512
  max_prepared_statements: 128
  max_prepared_memory_kb: 1024
  write_buffer_kb: 8
  slow_query_ms: 100
  time_zone: SYSTEM

resources:
  memory_limit_mb: 0
  max_procs: 0
  query_timeout_ms: 0
  sort_memory_mb: 0
  query_result_memory_mb: 0
  query_temp_mb: 0
  transaction_write_mb: 0  # 仅MVCC；0不限制逻辑写集总量，内存仍有界
  query_temp_path: ""

storage:
  path: /app/data
  local_wal: false  # 仅单机；不改变 MVCC 事务语义

auth:
  username: root
  password: change-this-password

security:
  login_failure_limit: 5
  login_failure_window_seconds: 60
  login_failure_block_seconds: 30

tls:
  enabled: false
  cert_file: ""
  key_file: ""
  require_secure_transport: false

log:
  path: /app/logs
  max_size_mb: 20
  retention_days: 7

audit:
  enabled: false
  path: /app/logs/audit.jsonl
  retention_days: 7

```

Windows 本地运行时，`/app/data` 和 `/app/logs` 会自动映射为当前目录下的 `./data`
和 `./logs`。

`server.time_zone` 是新连接的默认 SQL 时区，默认 `SYSTEM`，也可使用 `UTC`、
`+08:00`/`-05:00` 这类偏移或 `Asia/Shanghai` 等 IANA 时区名；配置无效时服务拒绝
启动。连接建立后可按 MySQL 会话语法覆盖，不影响其他连接：

```sql
SET time_zone = '+08:00';
SET SESSION time_zone = 'Asia/Shanghai';
SELECT @@session.time_zone, NOW();
```

`NOW()`、`CURRENT_TIMESTAMP`、`CURDATE()`、`DEFAULT CURRENT_TIMESTAMP` 都使用当前会话时区。`DATETIME` 仍保存不带时区的墙钟字段；当前
`TIMESTAMP` 也映射到同一存储类型，尚未实现 MySQL `TIMESTAMP` 独立的 UTC 存储/会
话时区转换。`SET GLOBAL time_zone` 暂不支持，应修改配置并重启以更改新连接默认值。

已移除分页缓存、冷读、optimistic_transactions 与 binlog 启用配置；旧非零/true 值会报错，
请删除这些配置和对应环境变量。支持以下环境变量覆盖：

| 环境变量 | 配置项 | 默认值 |
|---|---|---|
| `DB_USER` | 初始管理员用户名 | `root` |
| `DB_PASSWORD` | 初始管理员密码 | `change-this-password` |
| `DB_HOST` | 监听地址 | `127.0.0.1` |
| `DB_PORT` | 监听端口 | `3307` |
| `DB_MAX_CONNECTIONS` | 最大活动连接数；`0` 表示不限制 | `512` |
| `DB_MEMORY_LIMIT_MB` | Go 运行时内存软上限（MiB）；`0` 保留 `GOMEMLIMIT`/Go 默认策略 | `0` |
| `DB_MAX_PROCS` | Go 代码最大 CPU 并行度；`0` 保留 `GOMAXPROCS`/Go 默认策略 | `0` |
| `DB_QUERY_TIMEOUT_MS` | SQL 执行及事务锁等待期限；0 关闭，最大 86400000 | `0` |
| `DB_SORT_MEMORY_MB` | ORDER BY / DISTINCT 外部排序预算；0 使用 MVCC 默认 4 MiB | `0` |
| `DB_QUERY_RESULT_MEMORY_MB` | 物化结果与中间状态预算；0 使用 MVCC 默认 16 MiB | `0` |
| `DB_QUERY_TEMP_MB` | 单查询排序临时文件总量预算；0 使用 MVCC 默认 256 MiB | `0` |
| `DB_TRANSACTION_WRITE_MB` | 仅MVCC的逻辑写集预算（MiB）；0不设总量上限，正数最大1048576；不是内存或总磁盘上限 | `0` |
| `DB_QUERY_TEMP_PATH` | 已存在且可写的排序临时目录；空值使用系统临时目录 | 空 |
| `DB_STORAGE_MODE` | 已弃用的兼容配置，仅允许 `mvcc`；其他值拒绝启动 | `mvcc` |
| `DB_MAX_PREPARED_STATEMENTS` | 每连接预编译语句数量上限，范围 1–65535 | `128` |
| `DB_MAX_PREPARED_MEMORY_KB` | 每连接保留的预编译 SQL 与参数类型预算（KiB），范围 1–1048576 | `1024` |
| `DB_TIME_ZONE` | 新连接默认 SQL 时区；支持 `SYSTEM`、UTC 偏移和 IANA 名称 | `SYSTEM` |
| `DB_SLOW_QUERY_MS` | 慢查询阈值，包含结果发送时间；`0` 表示关闭 | `100` |
| `DB_DATA_PATH` | 数据目录 | 配置文件中的值 |
| `DB_LOGIN_FAILURE_LIMIT` | 同一来源 IP 与账号在窗口内允许的认证失败次数；`0` 关闭限速 | `5` |
| `DB_LOGIN_FAILURE_WINDOW_SECONDS` | 认证失败计数窗口秒数；`0` 关闭限速 | `60` |
| `DB_LOGIN_FAILURE_BLOCK_SECONDS` | 达到阈值后的阻断秒数；`0` 关闭限速 | `30` |
| `DB_TLS_ENABLED` | 是否启用 MySQL 协议 TLS | `false` |
| `DB_TLS_CERT_FILE` | PEM 服务器证书路径 | 空 |
| `DB_TLS_KEY_FILE` | PEM 私钥路径 | 空 |
| `DB_REQUIRE_SECURE_TRANSPORT` | 是否拒绝非 TLS 连接 | `false` |
| `DB_LOG_MAX_SIZE_MB` | 主服务日志单文件轮转阈值（MiB），范围 1-1024 | `20` |
| `DB_LOG_RETENTION_DAYS` | 已轮转主日志保留天数；`0` 永久保留，最大 `365` | `7` |
| `DB_AUDIT_ENABLED` | 是否启用 JSONL 审计日志 | `false` |
| `DB_AUDIT_PATH` | 审计日志路径 | `<log.path>/audit.jsonl` |
| `DB_AUDIT_RETENTION_DAYS` | 审计日志保留天数；`0` 永久保留，最大 `365` | `7` |

新实例默认只监听 `127.0.0.1`。可信局域网确需远程连接时，可把 `server.host` 改为
指定的局域网网卡地址；只有容器端口发布等明确场景才应使用 `0.0.0.0`，并必须通过主
机防火墙限制来源。Docker 镜像和 Compose 因端口映射需要，会显式把容器内 `DB_HOST`
设置为 `0.0.0.0`。公开部署前必须修改默认密码。TLS 默认关闭；需要加密连接时，配置
PEM 证书和私钥并启用 `tls.enabled`，服务支持 TLS 1.2 及以上版本。
`tls.require_secure_transport: true` 会拒绝明文认证并返回 MySQL 错误 3159，而且
必须与 `tls.enabled: true` 同时使用。证书或私钥缺失、不可读或不匹配时服务会在监
听端口前明确拒绝启动。MySQL 8 CLI 可使用 `--ssl-mode=REQUIRED`，需要校验证书时再
提供 CA 并选择 `VERIFY_CA`/`VERIFY_IDENTITY`；Navicat 和 DBeaver 应在连接的
SSL/TLS 页面启用并按证书签发方式配置 CA。TLS 不代替防火墙，MySQL 协议端口仍不应
直接暴露到公网。

证书和私钥由管理员在 GBaseLite 外部签发、续期和轮换；私钥文件只应允许服务账号读
取。Docker 启用 TLS 时还需把证书目录只读挂载到容器，并让 `DB_TLS_CERT_FILE` 与
`DB_TLS_KEY_FILE` 指向容器内路径。当前不自动签发证书，也不要求或验证客户端证书。

认证失败限速按“真实来源 IP + 用户名”独立计数。默认在 `60` 秒内失败 `5` 次后阻断
该组合 `30` 秒，客户端仍收到通用的 `1045 Access denied`，避免泄漏账号是否存在；
启用审计时，后续受限请求以 `AUTHENTICATE`/`blocked` 记录且不包含密码。三个安全参
数中任意一个设为 `0` 都会关闭限速，通常只应用于隔离的兼容性测试环境。

`auth.password` 是仅用于首次创建 `data/users/users.gob` 的 bootstrap 密码，不是
已有账号的持续认证来源。用户目录成功初始化后，可先停服务并备份数据目录，确认
`users/users.gob` 存在，再将配置改为 `password: ''` 后启动并用原账号密码验证；程
序会继续使用已保存的 `mysql_native_password` 哈希，不会修改或尝试反推密码。全新
或丢失用户目录时，空的 bootstrap 密码会让启动明确失败，避免意外创建无密码管理
员。修改配置中的 bootstrap 密码不会更改已存在账号，应使用 `ALTER USER` 或
`SET PASSWORD` 正常修改账号密码。

`max_connections` 对活动连接施加背压，达到上限后新连接在操作系统监听队列中等待，
避免无限创建 goroutine 和连接缓冲。`write_buffer_kb` 是每连接协议写缓冲；小结果
和高连接数场景可使用 `4` 或 `8`，大结果吞吐优先时可提高到 `16` 或 `32`。总连接内
存还包含客户端状态、Prepared Statement 和正在执行的查询，不应只按写缓冲计算。
`slow_query_ms` 同时覆盖普通查询和 Prepared Statement，从开始执行到结果写入连接
缓冲结束计时；达到阈值时会在 `gbaselite.log` 写入 `slow query`。它用于区分服务端
耗时和客户端取数、表格渲染及客户端附加元数据查询耗时。后台 `start` 若在监听、用
户目录、审计/binlog 或 PID 初始化阶段失败，会把具体原因写入 `gbaselite.log` 并在
父进程错误中返回本次新增的最后一条诊断，不再只提示检查空日志。主日志及新建 JSONL
文件使用仅所有者可读写的 `0600` 模式；Windows MSI 安装目录使用前述 ACL 控制。

主服务日志当前写入 `gbaselite.log`。当下一条记录会使文件超过 `log.max_size_mb`
时，现有文件会改名为带 UTC 纳秒时间戳的 `gbaselite-*.log`，随后继续写新的
`gbaselite.log`；默认阈值为 `20` MiB，允许范围为 `1` 到 `1024` MiB。
`log.retention_days` 默认 `7` 天，`0` 表示永久保留，最大 `365` 天。服务启动、发
生轮转以及持续写日志期间每 24 小时会清理一次超过保留期的已轮转主日志，不会删除当
前 `gbaselite.log`、审计日志、binlog 或其他文件。轮转或清理失败会写到标准错误，
并继续尝试保留当前主日志，不会为清理日志而覆盖业务数据。

### 低资源运行与 SQL 性能

64 位平台的每个存储 Value 从 88 字节压缩到 80 字节，仅重排字段来减少填充；已验证
旧/新 gob 字段布局双向解码，未改变类型语义或存储格式版本。

MVCC 的行、索引和事务持久化由版本存储负责，不再使用实例级事务门和整库快照克隆。
查询和维护范围以当前兼容矩阵为准。

每连接默认最多保留 128 条 Prepared Statement，SQL 文本加预留参数类型向量最多
1024 KiB；达到任一限制返回 MySQL 1461，关闭语句后释放预算，连接仍可继续查询。
单条语句超过 65535 个占位符返回 1390。该预算不包含连接对象、AST、执行结果或临时
参数，因此不是连接总内存硬上限。应用应及时关闭不再使用的 Prepared Statement。

低资源配置项见[查询资源预算](docs/使用文档/查询资源预算.md)，合并到自己的配置后重启。
`resources.memory_limit_mb` 范围 0–1048576；`resources.max_procs` 范围 0–1024。
正数覆盖相应 Go 环境设置，零保留运行时原设置。不定时调用强制 GC 或工作集清理。
Go 内存限制是软目标，**不是 Windows 工作集/RSS 或物理内存上限**；数据、索引和
运行时开销超过目标时仍可能超出，过低会增加 GC CPU 和 SQL 延迟。`max_procs` 限制
并行执行的 Go 代码数量，不是 CPU 百分比限速。原理见
[Go 内存限制文档](https://pkg.go.dev/runtime/debug#SetMemoryLimit)。

使用 `SHOW GLOBAL STATUS LIKE 'Gbaselite_%'` 可按需读取 Go 堆对象字节、运行时管理
字节、已归还堆内存、内存软上限、GC 次数/累计 CPU 秒、goroutine 数及 CPU 并行度。
这些是实例级 Go 指标，即使通过 SESSION STATUS 查询也不代表单连接用量；没有额外
后台采样线程。物理内存仍应查看 Windows 工作集/Private Bytes 或 Linux RSS。

性能与进程内存的复现工具是 [measure-resources.ps1](scripts/measure-resources.ps1)：

```powershell
.\scripts\measure-resources.ps1 `
  -BaselineExecutable E:\bench\before.exe `
  -CandidateExecutable E:\bench\after.exe
```

它只启动使用新建 `.tmp` 目录的隔离实例，覆盖一万行准备、点查、范围分页、JOIN、
持久化插入和更新，核对行数并输出 SQL 延迟、累计 CPU 秒及峰值工作集。测试结束停止
自己创建的进程、清理测试数据并保留 JSON/日志。可加 `-MemoryLimitMB 64 -MaxProcs 2`
仅对候选实例启用调优；评估配置影响时将两个路径都指向候选程序。测试驱动需要 Go，
不连接已安装服务。该脚本用于自助诊断；当前MVCC四数据库实测见[统一对比报告](docs/报告/四数据库对比报告.md)。

### 审计日志

`audit.enabled` 默认关闭。审计记录认证、普通 SQL、预处理执行、操作结果、耗时和脱敏 SQL。
`audit.retention_days` 默认 7，0 永久保留，最大 365。事务持久化由 MVCC 负责，
`binlog.enabled: true` 拒绝启动。旧 JSONL 文件保留供离线检查和迁移前恢复。

## 部署

### 方式一：内置后台进程

适合 Windows、本地开发和单机测试：

```powershell
.\bin\gbaselite.exe start --config .\config.yaml
.\bin\gbaselite.exe stop --config .\config.yaml
.\bin\gbaselite.exe restart --config .\config.yaml
.\bin\gbaselite.exe healthcheck --host 127.0.0.1 --port 3307
```

`server` 是前台模式，可以使用 `Ctrl+C` 停止；`start` 是后台模式，PID 写入数据目
录下的 `gbaselite.pid`。

Windows 可直接双击 `scripts\windows` 下的 `start.bat`、`stop.bat` 和
`restart.bat`。脚本会自动识别源码布局（项目根目录中的 `bin\gbaselite.exe`）与
Windows ZIP 便携包布局（脚本同目录的 `gbaselite.exe`），并从对应目录读取
`config.yaml`。双击运行会在结束前保留窗口以显示结果；从已打开的 `cmd.exe` 直接调
用不会等待。PowerShell 和自动化调用应显式传入 `--no-pause`。

```powershell
.\bin\gbaselite.exe server --config .\config.yaml
```

### 方式二：Docker Compose

标准镜像 Compose 直接拉取 `pucj/gbaselite:latest`；启动前在
`docker/docker-compose.yml` 中把 `DB_PASSWORD` 改为强密码。源码构建 Compose 使用
被 Git 忽略的 `docker/temp.env`：

```bash
cp docker/temp.env.example docker/temp.env
# 修改 docker/temp.env 中的 DB_PASSWORD
```

三种启动方式分别为：

```bash
# 默认拉取 pucj/gbaselite:latest（Docker Hub）
docker compose -f docker/docker-compose.yml up -d

# 开发者：使用当前源码构建镜像
docker compose --env-file docker/temp.env -f docker/docker-compose.build.yml up -d --build

# Alpine 3.21：挂载宿主机 Linux 静态二进制
chmod +x dist/gbaselite-linux-amd64
sudo chown -R 65532:65532 data logs
docker compose -f docker/docker-compose.binary.yml up -d
```

`docker-compose.binary.yml` 不构建业务镜像，直接以 `alpine:3.21` 运行挂载到
`/home/bin/gbaselite` 的静态二进制。该文件不使用 `${...}` 插值：默认挂载当前版本
目录的 `gbaselite-linux-amd64` 并固定映射 `3307:3307`。ARM64 主机将 volume 源文
件名改为 `gbaselite-linux-arm64`；自定义端口时，同时修改 `ports`、`DB_PORT` 和健
康检查中的三个 `3307`。

发布前使用 `uname -m` 确认宿主机架构，并用 `test -x` 检查执行权限。架构不匹配会
返回 `exec format error`，缺少执行权限会返回 `permission denied`。

普通镜像已经包含并执行 `/app/gbaselite`，不要再把宿主机二进制挂载到
`/home/bin/gbaselite`；需要直接挂载裸二进制时，应改用
`docker-compose.binary.yml`。普通镜像入口与 MySQL 官方镜像采用相同的降权模式：入口
短暂以 root 创建 `/app/data` 和 `/app/logs`，只修复所有者不匹配的挂载内容，随后通过
`su-exec` 以固定的 `10001:10001` 执行数据库进程。因此标准镜像和源码构建模式首次启动
不需要手工 `chown`，且数据库进程本身不是 root。不要在 Compose 中设置 `user:` 或覆盖
`entrypoint`，否则会跳过自动初始化。

数据库前台进程在容器内是 PID 1。容器被强制终止时，挂载的数据目录可能保留内容为
`1` 的 `gbaselite.pid`；下次启动会把这个与当前容器进程同 PID 的文件作为残留状态重新
认领，不需要手工删除 PID 文件。指向其他仍存活进程的 PID 文件仍会阻止重复启动。

裸二进制 `docker-compose.binary.yml` 没有镜像入口辅助，仍固定以 `65532:65532` 运行，
首次使用时必须按示例手工准备目录权限；两种模式的宿主机目录所有者不能混用。普通镜像
启动时会递归修正已有数据和日志文件的所有者，因此两个挂载目录必须专供这个 GBaseLite
实例使用，不能与其他容器共享。启用 SELinux 且 `getenforce` 返回 `Enforcing` 时，还要
给两个 bind mount 添加 `:Z`，例如 `../data:/app/data:Z`；容器内 root 也不能绕过宿主机
SELinux 标签或只读文件系统，这些情况会返回带目录所有者和模式的明确错误。

普通镜像读取镜像内的 `/app/config.yaml`，源码构建 Compose 挂载
`docker/config.example.yaml`；两种模式都再使用非空的 `DB_*` 环境变量覆盖同名配置。
二进制 Compose 不挂载配置文件，`/app/config.yaml` 不存在时程序使用内置默认值。它
不依赖 `temp.env`：在 `environment:` 中直接把必需的 `DB_PASSWORD` 改为强密码，并
固定容器外访问所需的 `DB_HOST=0.0.0.0`；其余 `DB_*` 均已注释，按需取消注释后才覆
盖默认值。二进制 Compose 不使用 `${...}` 或默认值插值。启动前必须把示例
`DB_PASSWORD` 改为强密码；由于密码会明文保存在 Compose 文件中，勿将填写密码后的
文件提交、分享或复制到不可信位置。

二进制部署目录不需要提供 `docker/config.example.yaml`。如果旧版短语法挂载曾在宿
主机创建同名目录，应先确认它确实是空目录，再使用 `rmdir config.example.yaml` 删
除；不要对 `data` 或 `logs` 执行该操作。

标准镜像 Compose 持久化 `./gbaselite/data` 与 `./gbaselite/logs`；源码构建和裸二进制
Compose 持久化仓库根目录的 `./data` 与 `./logs`。容器日志使用 `json-file`，单文件
上限 20 MiB，最多保留 3 个；这是容器标准输出日志，与 `/app/logs/gbaselite.log` 自身的
大小轮转和按天保留相互独立。健康检查直接执行 GBaseLite 的 TCP `healthcheck`，不
依赖 HTTP。二进制 Compose 的端口映射和健康检查会随 `DB_PORT` 同步变化。

Compose 的 `mem_limit: 128m` 只适合小数据量、低并发环境。生产环境建议至少 256
MiB，并根据数据量和并发查询继续提高。不要使用 `docker compose down -v`
或手工删除 `data`，除非明确需要清空实例。

验证和停止：

```bash
docker compose --env-file docker/temp.env -f docker/docker-compose.yml ps
docker inspect --format '{{json .State.Health}}' gbaselite
docker compose --env-file docker/temp.env -f docker/docker-compose.yml down

# 二进制 Compose 使用相同的两个环境文件
docker compose -f docker/docker-compose.binary.yml ps
docker compose -f docker/docker-compose.binary.yml down
```

### 方式三：Windows MSI

MSI 安装程序到 `%ProgramFiles%\GBaseLite`，注册 Windows Service 和系统 PATH；
正式配置、数据、日志位于 `%ProgramData%\GBaseLite`，由 SYSTEM 与管理员访问。
可选择端口、账号、目录、自启、安装后启动与桌面快捷方式。审计页仅提供脱敏审计配置，
不再提供 legacy binlog 开关；旧日志文件不删除。

旧数据必须先停服、备份并执行 `migrate-legacy`，再将服务配置指向新 MVCC 目录。
升级不能将旧 snapshot/paged 目录直接作为运行目录。重新初始化仍需手工选择和第二次确认，
它不等于迁移；不要用重新初始化替代迁移。安装后重开终端以继承 PATH，使用
`gbaselite -u root -p` 连接。TLS 证书需自行配置，安装器不会生成证书。

### 方式四：Linux systemd

生产环境应让 systemd 管理前台 `server` 进程，而不是使用内置 `start` 创建二级后台
进程。以下示例目录可以按实际环境调整：

```bash
sudo useradd --system --home /var/lib/gbaselite --shell /usr/sbin/nologin gbaselite
sudo install -d -o gbaselite -g gbaselite /opt/gbaselite /var/lib/gbaselite /var/log/gbaselite
sudo install -m 0755 ./bin/gbaselite /opt/gbaselite/gbaselite
sudo install -d -m 0750 /etc/gbaselite
sudo install -m 0640 config.yaml /etc/gbaselite/config.yaml
sudo chown root:gbaselite /etc/gbaselite/config.yaml
```

生产配置建议使用绝对路径：

```yaml
server:
  host: 127.0.0.1
  port: 3307
storage:
  path: /var/lib/gbaselite
auth:
  username: root
  password: replace-this-password
log:
  path: /var/log/gbaselite
```

创建 `/etc/systemd/system/gbaselite.service`：

```ini
[Unit]
Description=GBaseLite database server
After=network.target

[Service]
Type=simple
User=gbaselite
Group=gbaselite
ExecStart=/opt/gbaselite/gbaselite server --config /etc/gbaselite/config.yaml
Restart=on-failure
RestartSec=2
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
```

启用服务：

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now gbaselite
sudo systemctl status gbaselite
sudo -u gbaselite /opt/gbaselite/gbaselite healthcheck --host 127.0.0.1 --port 3307
sudo journalctl -u gbaselite -f
```

### 生产部署检查清单

- 修改默认管理员密码
- 将数据目录放在持久化磁盘，并限制目录访问权限
- 限制 3307 端口来源，不直接暴露到公网
- 跨主机连接时配置 TLS、限制私钥权限，并在客户端验证受信任的 CA/主机名
- 按需设置审计保留天数；超大日志还应监控磁盘并规划外部归档
- 按审计和恢复要求决定是否启用 `audit`，并限制 JSONL 文件访问权限
- 配置定期逻辑备份和完整数据目录备份
- 使用 healthcheck 或进程管理器监控服务
- 定期执行 `gbaselite diagnose --config <正式配置>`，并通过已认证连接检查
  `SHOW GLOBAL STATUS`
- 上线前使用真实客户端、业务 SQL 和恢复演练验证兼容性
- 保留当前二进制和数据备份，确保升级失败时可以回滚

## 升级与回滚

MVCC 实例可先通过 SQL 做 MVCC 备份，停服后备份整个数据目录以保留账号与授权。
旧 snapshot/paged 实例必须按[迁移步骤](#旧数据迁移)转换，不能直接执行下面的二进制替换流程。

Windows 示例：

```powershell
.\bin\gbaselite.exe stop --config .\config.yaml
Copy-Item .\data .\data-before-upgrade -Recurse
Copy-Item .\bin\gbaselite.exe .\bin\gbaselite.previous.exe
Copy-Item .\gbaselite-new.exe .\bin\gbaselite.exe
.\bin\gbaselite.exe start --config .\config.yaml
.\bin\gbaselite.exe healthcheck --host 127.0.0.1 --port 3307
```

回滚时先停止新版本，恢复旧二进制；如果新版本已经改变了持久化数据，则同时恢复升级
前的数据目录。不要在服务运行时直接覆盖 `data`。

## SQL 示例

本节只保留快速确认服务可用的 SQL。面向业务使用者的循序教程、可直接运行的练习脚本
和 Navicat 使用说明见 [SQL 使用教程](docs/使用文档/SQL使用教程.md)。

```sql
CREATE DATABASE test;
USE test;

CREATE TABLE users (
  id INT,
  name VARCHAR(50),
  phone VARCHAR(30),
  age INT,
  enabled BOOLEAN,
  created DATE
);

ALTER TABLE users ADD UNIQUE INDEX users_id(id);
CREATE INDEX users_name ON users(name);
ALTER TABLE users
  ADD COLUMN course_balance INT NOT NULL DEFAULT 0 AFTER age,
  ADD COLUMN weekly_goal INT NOT NULL DEFAULT 3,
  ADD CONSTRAINT ck_course_balance CHECK (course_balance >= 0),
  MODIFY COLUMN created DATETIME;
ALTER TABLE users CHANGE COLUMN created updated_at DATETIME;
ALTER TABLE users
  ADD CONSTRAINT uq_users_phone UNIQUE (phone),
  ADD INDEX (weekly_goal),
  ALTER COLUMN weekly_goal SET DEFAULT 4,
  RENAME INDEX weekly_goal TO idx_users_weekly_goal;

INSERT INTO users (id, name, phone, age, enabled, updated_at)
VALUES (1, '张三', '13800000001', 20, TRUE, '2026-07-27');
INSERT INTO users(id,name,phone,age,enabled) VALUES(2,'李四','13800000002',18+7,TRUE);
SELECT name, age FROM users
WHERE age >= 18 AND enabled = TRUE
ORDER BY age DESC LIMIT 10;

UPDATE users SET age = 21 WHERE id = 1;
UPDATE users SET age = 22 WHERE id = 1 LIMIT 1;
DELETE FROM users WHERE id = 1;
DELETE FROM users WHERE id = 1 LIMIT 1;

SHOW INDEX FROM users;
SHOW CREATE TABLE users;
SHOW FULL COLUMNS FROM users LIKE 'phone';
SHOW COLUMNS FROM users WHERE Field='weekly_goal';
SELECT COLUMN_NAME, COLUMN_TYPE, COLUMN_COMMENT
FROM information_schema.COLUMNS
WHERE TABLE_SCHEMA='test' AND TABLE_NAME='users' AND COLUMN_NAME='phone';
EXPLAIN SELECT id FROM users WHERE name = '张三';
```

事务：

```sql
BEGIN;
INSERT INTO users (id, name, phone, age, enabled, updated_at)
VALUES (3, '王五', '13800000003', 30, TRUE, '2026-07-27');
ROLLBACK;
```

用户和授权：

```sql
CREATE USER IF NOT EXISTS 'app'@'%' IDENTIFIED BY 'secret';
ALTER USER 'app'@'%' IDENTIFIED BY 'new-secret';

GRANT SELECT, INSERT, UPDATE ON `test`.* TO 'app'@'%';
GRANT SELECT ON `test`.`users` TO 'app'@'%' WITH GRANT OPTION;
REVOKE INSERT ON `test`.* FROM 'app'@'%';

SHOW GRANTS FOR 'app'@'%';
SHOW CREATE USER 'app'@'%';
DROP USER IF EXISTS 'app'@'%';
```

## 兼容范围与限制

MVCC 是唯一运行事务引擎。`snapshot`、`paged` 不再作为服务模式；旧文档、历史测试和
已发布旧版本的 SQL 清单不能作为本开发版本的能力承诺。GBaseLite 只实现 MySQL 子集。

| 范围 | 当前支持 | 限制 |
|---|---|---|
| 连接 | MySQL TCP、认证、TLS、COM_QUERY、Prepared Statement、二进制结果 | 不是完整 MySQL 协议实现 |
| DDL | 数据库/表创建删除、TRUNCATE、常用 ALTER、主键/唯一/普通索引 | 不支持视图、CTAS/LIKE、RENAME TABLE；DDL 在 MVCC 事务内，无 MySQL 隐式提交 |
| 写入 | INSERT VALUES/表达式/参数、单表 UPDATE/DELETE | 不支持 INSERT SELECT/SET/IGNORE、REPLACE、ON DUPLICATE KEY、JOIN 写入、子查询写入 |
| 查询 | 投影、WHERE、排序、分页、DISTINCT、聚合、GROUP BY/HAVING、INNER/LEFT JOIN、UNION/UNION ALL、排名与聚合窗口 | 不支持 CTE、派生表、子查询及锁定读；窗口不与 GROUP BY/HAVING 混用，不支持显式窗口 frame；UNION 要求列数一致，未实现完整 MySQL 类型合并 |
| 事务 | BEGIN/COMMIT/ROLLBACK、SET autocommit=0/1、断连回滚、语句失败回滚 | 快照隔离；不支持 SAVEPOINT、LOCK TABLES、隔离级别切换或串行化保证 |
| 约束 | PRIMARY KEY、UNIQUE、CHECK、同库 RESTRICT/NO ACTION 外键 | 不支持级联、自引用、跨库外键；受引用表 ALTER 有限制 |
| 类型 | INT/BIGINT、文本、日期时间、BOOLEAN、精确 DECIMAL、JSON 列及现有标量函数 | 不支持 ON UPDATE 列表达式；TIMESTAMP 尚无独立 UTC 存储语义 |
| 账号 | 单机用户、密码、授权及权限元数据 | 用户目录独立持久化，不参与业务事务，不经过 Raft；复制节点拒绝账号 SQL |
| 元数据 | 已提交表结构、索引、约束、information_schema、SHOW STATUS/REPLICATION STATUS | SHOW 的行数、大小不是实时业务统计，准确计数使用 SELECT COUNT(*) |
| 维护 | 单机 BACKUP/RESTORE/GC/COMPACT MVCC | 必须在事务外且开启 autocommit；复制节点不支持这些在线维护命令 |
| 复制 | 实验性固定三节点 Raft、选主、连接代理 | 无分片、动态成员、混合版本滚动升级或生产容灾保证 |

同一行、唯一键或依赖表结构的并发变更可能导致提交返回 MySQL 1213，应重试整个事务。
不同行更新可独立提交。自增号持久预留，回滚后允许空洞。SHOW 读取已提交元数据，不能
用它判断当前事务内尚未提交的 DDL。单条 SQL 文本限制为 1 MiB。

MVCC 默认查询预算：排序内存 4 MiB、结果内存 16 MiB、查询临时文件 256 MiB。
配置相应预算为 0 时使用这些默认值；`transaction_write_mb: 0` 不限制逻辑写集总量。
它们不是进程物理内存或整机磁盘总量上限。

## 旧数据迁移

先用原版本停止旧实例，保留完整源目录；不要直接把旧配置的 mode 改成 mvcc 后打开
同一目录。源目录必须已有 `users/users.gob`，且无 PID 文件。异常退出残留 PID 时先确认
原进程已停止，并在恢复副本处理残留标记。目标父目录需已存在，目标目录必须不存在。

```powershell
gbaselite migrate-legacy --source D:\backup\old-data --target D:\backup\mvcc-data
```

迁移自动识别 snapshot/paged，复制源目录到隔离暂存区，读取旧格式及用户目录，重建
MVCC 表、行、索引、约束与自增计数。提交并关闭后重新打开目标，核对行数和逐行编码，
成功后才发布目标目录。源目录不写入；失败不会发布目标库，暂存区会清理。

旧视图、ON UPDATE 列表达式、MVCC 不支持的外键及无效数据会明确报错，不丢弃结构或
静默放宽约束。迁移是离线操作，旧库加载需要内存容纳数据，磁盘需容纳源副本和新库。
逻辑 binlog 不自动重放；如需基于旧日志恢复，先使用原版本在恢复副本完成恢复，再迁移。

成功后将配置的 `storage.path` 指向新目录，删除旧分页/冷读/optimistic 配置，并保持
`binlog.enabled: false`（或删除该段）。使用新版本启动并运行健康检查与业务 SQL 校验。
回退需要原版本及保留的原目录；迁移后在新库产生的写入不会自动回到原目录。

## 备份与恢复

单机通过已认证的 MySQL 连接执行：

```sql
BACKUP MVCC TO 'D:/backup/mvcc-backup';
RESTORE MVCC FROM 'D:/backup/mvcc-backup';
GC MVCC;
COMPACT MVCC TO 'D:/backup/mvcc-compact';
```

路径位于服务端。备份带长度与 SHA-256 校验；RESTORE 替换业务存储并使旧事务失效。
COMPACT 生成独立副本，切换需显式停服处理。在线 MVCC 备份不包含 `users/users.gob`；
要备份账号、授权和配置，应停服后复制完整实例目录。不要在线直接复制 `mvcc.db`。

旧离线 `shell/import/export/backup/restore` 不再打开运行库；通过 MySQL 连接导入
受支持的 SQL。`replay-binlog --check-only --input <文件>` 保留只读日志验证，写入回放
已禁用。`inspect-snapshot` / `inspect-instance` 仅检查旧格式迁移源，不能校验 MVCC 库。
运行实例的 `Engine.Store` 是表结构镜像，不能用它导出业务行；备份使用 MVCC 维护接口。
## CLI

| 命令 | 用途 |
|---|---|
| `gbaselite server` | 前台运行服务 |
| `gbaselite service` | Windows SCM 专用服务入口，不应在终端直接运行 |
| `gbaselite start` | 后台启动；已运行时自动重启 |
| `gbaselite stop` | 停止后台服务 |
| `gbaselite restart` | 重启服务 |
| `gbaselite shell` | 旧离线入口已禁用；使用 MySQL 客户端 |
| `gbaselite client` / `gbaselite connect` | 使用 MySQL 风格参数连接正在运行的 GBaseLite 服务 |
| `gbaselite backup` / `restore` | 旧离线入口已禁用；使用 SQL BACKUP/RESTORE MVCC |
| `gbaselite replay-binlog --check-only` | 只读校验旧逻辑日志；写入回放已禁用 |
| `gbaselite healthcheck` | 检查 TCP 监听状态 |
| `gbaselite diagnose` | 只读检查配置、监听、数据/日志路径、持久化文件、TLS 和日志开关 |
| `gbaselite inspect-snapshot` | 只读解码数据库快照并输出含索引的结构计数、时间和 SHA-256，可比较恢复候选 |
| `gbaselite inspect-instance` | 只读检查停服数据目录副本中的数据库快照、用户目录和授权聚合信息 |
| `gbaselite migrate-legacy` | 将停服旧目录转换为新的独立 MVCC 目录 |
| `gbaselite migrate-layout` | 在新目录导出已有 MVCC 文件布局 |
| `gbaselite version` | 输出版本号 |

完整帮助：

```bash
gbaselite help
```

### 健康检查与诊断

`healthcheck` 只验证目标 TCP 端口可连接，适合容器和进程管理器的高频探针；它不执
行认证、SQL 或数据文件解码。管理员排障时应使用正式配置运行更完整的只读诊断：

```powershell
gbaselite diagnose --config C:\ProgramData\GBaseLite\config.yaml
```

Windows MSI 会限制正式配置和数据目录仅由 `SYSTEM` 与本机管理员读取，因此该命令通
常需要在管理员终端中执行。报告包含程序版本、配置地址、实际探测地址、MVCC 数据库和用户目
录文件状态、数据卷和日志卷的总字节数/当前账号可用字节数、主日志与轮转日志占用、
TLS 证书能否加载，以及审计/binlog 路径、当前文件大小和保留期。即使审计或 binlog
已关闭，报告仍会显示配置路径及遗留文件状态，便于管理员判断磁盘占用；不会输出用户
名、密码、密码哈希、SQL 或数据内容，也不会打开、解码或改写正在使用的持久化文件。
端口不可达、关键目录/文件不可用、TLS 材料无效或发现 `.tmp` 恢复候选时返回非零退
出码；卷空间查询失败会显示 `unavailable`，但不会单独改变退出码。

已认证的 MySQL 连接可查看服务进程内指标：

```sql
SHOW GLOBAL STATUS;
SHOW SESSION STATUS LIKE 'Ssl_%';
SHOW STATUS LIKE 'Threads_%';
```

当前提供累计连接数、当前/峰值连接数、查询数、活动查询数、中止连接数、运行秒数、
TLS 连接累计值和当前会话的 TLS 版本/密码套件。计数从本次服务进程启动开始；TCP
healthcheck 这类未完成 MySQL 认证的连接会计入 `Connections` 和
`Aborted_connects`。存储进入 fail-closed 后仍保持“所有后续 SQL 返回 1030”的契
约，不允许用状态查询绕过故障状态。

## 持久化布局

```text
data/
├── versioned/mvcc.db        # 唯一运行事务存储
├── users/users.gob          # 本地用户、密码哈希、授权
├── replication/            # 仅配置 Raft 时存在
└── gbaselite.pid
logs/
├── gbaselite.log
├── gbaselite-*.log
└── audit.jsonl              # 可选脱敏审计
```

MVCC 的 local WAL、临时写集及备份格式详见 [MVCC 文档](docs/使用文档/MVCC复制与高可用.md)。
历史 MVCC Gob 行/旧键编码继续按格式标识读取；这属于文件编码兼容，不是 snapshot/paged
运行模式。新格式不支持直接降级给旧二进制。

`databases/store.gob`、`store.pages`、`store.wal`、`store.checkpoint` 及恢复候选只由
旧格式迁移/检查工具使用。正常打开遇到这些标记会拒绝启动，避免把旧库误识别为空库。
不要删除标记绕过检查，也不要在运行时编辑或替换持久化文件。

## Storage Engine 接口

SQL 执行、访问计划和事务编排依赖 `storageengine.Engine/Txn/Iterator/Table/Index/ScanRequest`，
不直接导入 bbolt、具体 MVCC 或复制实现。默认由 `enginefactory` 装配现有 MVCC/bbolt adapter；
`OpenOptions.BackendFactory` 或 `NewWithStorage` 可注入替代后端，SQL 算子无需改动。
正常启动仍不需要 mode，数据格式保持不变；当前 SQL 支持范围以功能矩阵为准。

`executor.Engine.Backend` 替代具体 `MVCC` 句柄。自增号通过中立接口持久预留，维护为可选能力；
不支持的能力明确报错。迭代器字节只借用到下一次读取，批量算子显式复制保留的数据。
详情、生命周期约定和测试边界见[接口设计](docs/设计/StorageEngine接口.md)。

已有独立内存后端的 SQL 回归测试与导入边界检查；该后端仅用于测试，不是第二个生产引擎。

## 统一 Physical Operator

查询统一组合 physical.Operator[T].Run(ctx, yield)：Scan、Filter、Projection、Join、
Aggregate、Sort、TopN、Union、Materialize、Window、Modify。关系算子只消费上游行，
Scan 通过 Iterator 读数据；SQL 绑定层把 Txn、访问计划、行解码、表达式和约束写入接到算子。
公共算子不导入 MVCC、bbolt 或具体后端，索引探测与全表扫描共用同一个 Join。

Sort 和 TopN 共用有预算、可溢写的排序器；TopN 当前为排序后分页，未采用独立堆优化。
GROUP BY 和 Window 保留内存预算，超限报错；窗口物化尚不溢写。
支持 ROW_NUMBER/RANK/DENSE_RANK，以及 COUNT/SUM/AVG/MIN/MAX 的默认窗口：
无排序时整个分区，有排序时累积至当前同值组。窗口与 GROUP BY/HAVING 混用、显式 frame
仍不支持。UNION DISTINCT 复用排序去重，混合 ALL/DISTINCT 按分支顺序组合。

增删改使用行级 Modify，提交仍由语句子事务及外层事务负责，失败不留下部分语句写入。
独立整数 Executor 已移除，旧向量内核只留在测试基准中；当前 SQL 不选择独立整数执行路径。
LIMIT 提前结束、消费错误和取消均释放已打开的迭代器/排序临时文件。
详见[物理算子设计](docs/设计/PhysicalOperator管线.md)。

### A01–A03 架构防回归

生产源码检查禁止 SQL/Physical 导入具体存储实现、测试后端或调用 legacy persistence；
迁移读取器仅允许在指定迁移函数中使用，snapshot/paged 启动选项持续拒绝。
检查所有平台源码，并以负例测试验证别名导入、mode 分支和迁移入口绕用会被发现。

storageengine/testkit.Run 是后端通用 Contract Suite，当前覆盖 MVCC、local-WAL 与独立 memory 夹具。
新后端应运行同一套事务、所有权、扫描、取消、计数器和冲突测试，再运行 SQL regression matrix。
memory 仅供测试；生产装配仍返回 storageengine.Engine。

Txn.GuardRange(space, KeyRange) 和 Table/Index.GuardRange(KeyRange) 支持明确上下界及开闭区间。
零 KeyRange 表示整个空间；nil 无界，空但非 nil 是有效端点。守卫复制端点，提交时检测范围内
插入、更新和删除，子事务回滚会丢弃其守卫。现有 DDL/外键仍传全范围，不改变 SQL 隔离语义。
这预留了范围依赖表达能力，尚未实现阻塞式 Gap/Next-Key Lock；后续锁管理可复用边界语义。

核心 Scan/Join 到 Filter、Projection、Aggregate、Window、Sort 的输入直接传递 Operator；
协议 Result 和历史查询辅助入口保留适配边界。SELECT DISTINCT 与 UNION DISTINCT 复用
physical.Distinct，维持排序规则、NULL、首行代表、输出顺序、分页和溢写预算。

## 单引擎改造与验证

| 原分支位置 | 当前处理 |
|---|---|
| config/config.go、DB_STORAGE_MODE | 默认 mvcc；旧模式拒绝，mvcc 仅为兼容配置别名 |
| executor/open_options.go | 公共 Open/OpenWithOptions 只构造 MVCC Engine |
| executor/executor.go、mvcc_*.go（旧 transaction/ 包已删除） | 公共 SQL、提交、回滚、关闭仅走 MVCC；旧事务执行器已移出生产构建 |
| executor/legacy_reader.go、legacy_migration.go、storage/persistence.go、page_persistence.go | snapshot/paged 格式识别及持久化恢复只在离线迁移副本内使用 |
| server/handler.go、mysql_server.go、resources.go、session_settings.go | 无后端选择；固定 MVCC 事务语义，删除分页状态指标 |
| cmd/gbaselite/replication.go、main.go、diagnose.go | 移除离线旧运行入口和 binlog 写入回放；诊断 versioned/mvcc.db |
| Docker/MSI/探针 | 不再提供 legacy binlog；MVCC 探针使用默认 Open，冷读运行探针明确退出 |

旧事务执行器仅存在于 `executor/legacy_*_fixture_test.go`，不进入生产二进制。
旧 SQL/持久化行为测试以 `TestLegacy...` 标识，仅用于迁移组件的历史特征验证，不能
作为当前运行引擎的支持声明。MVCC、默认打开、协议、迁移与资源预算有独立回归覆盖。
迁移测试只使用 `t.TempDir()`，验证源文件不变、拒绝覆盖、失败清理和持久化重开结果。
## 项目结构

```text
catalog/       用户、授权和权限元数据
cmd/           gbaselite CLI 入口与进程管理
config/        配置加载与环境变量覆盖
docs/使用文档/ SQL教程、兼容性、配置与存储说明
docs/报告/     当前四数据库对比报告，图片/与数据/存放配套证据
docker/        Dockerfile、Compose 与环境示例
executor/      SQL 执行器、查询、备份
index/         索引数据结构接口
journal/       审计及旧日志迁移/只读检查
mysql/         MySQL 导入、导出和恢复
parser/        SQL lexer、AST 和 parser
protocol/      MySQL 协议包与结果编码
server/        MySQL TCP 服务和客户端兼容层
storage/       表、数据库、行和值及持久化
installer/wix/ WiX MSI 清单和安全配置自定义操作
scripts/       PowerShell、Shell、便携包和 MSI 构建脚本
.github/       测试、Release 和多架构 Docker 工作流
```

## 开发与测试

```bash
go fmt ./...
go test ./...
go vet ./...
```

可重复性能基准使用项目临时目录，不连接已安装服务或业务数据。以下命令覆盖历史整数内核对比、表结构读取及多连接 MySQL 协议查询；整数内核基准不代表当前统一管线的端到端性能：

```powershell
$env:GOCACHE="$PWD\.tmp\gocache"
D:\env\Go\bin\go.exe test ./executor ./server -run '^$' `
  -bench 'Benchmark(MVCCIntegerBatchKernel|MVCCReadTableMetadata|MySQLConcurrentPrimaryKeySelect)$' `
  -benchtime=1s -benchmem -count=3
```

比较结果时固定 Go 版本、CPU、电源模式、`-count` 和 `-benchtime`，并保留完整命令
与原始输出；这些微基准用于同一机器上的版本回归，不代表生产容量或完整 MySQL 性
能。

`server/mysql_server_test.go` 使用标准 MySQL 驱动验证真实 TCP 握手、认证、权限、
CRUD 和元数据兼容。涉及客户端兼容的改动还应使用真实 MySQL 客户端和对应 GUI 客户
端验证。

历史夹具 `executor/recovery_test.go` 使用独立子进程验证旧快照引擎已经返回成功的写入在未执行 `Close`
就退出后仍可恢复，并使用固定随机种子执行 250 次增删改、每 25 次关闭重开后与内存
模型逐行核对。测试只使用 `t.TempDir()`，不会连接本机服务或正式数据目录；它覆盖已
确认提交后的恢复，不模拟磁盘控制器谎报落盘成功或操作系统无法提供的断电保证。

GitHub Actions 会在 Ubuntu 和 Windows 上分别执行 `gofmt` 检查、完整 Go 测试与
`go vet`。独立的 Linux CI 任务还会使用官方 `mysql:8.4` 容器中的真实 `mysql` 和
`mysqldump`，连接仅使用 CI `.tmp` 数据目录的临时 GBaseLite 实例，覆盖带连字符数
据库、表/索引、事务回滚、视图拒绝、默认 MVCC、`SHOW DATABASES`、自包含导出、删除后导入和恢复结果核对。该任
务不访问开发机或部署机器的数据，也不替代 Navicat GUI 的版本专项验证。

## 本地发布打包

Windows PowerShell 会先检查 `gofmt`，执行完整测试和静态检查，然后以
`CGO_ENABLED=0` 交叉编译 Windows amd64、Linux amd64 和 Linux arm64。检测到 WiX、
.NET SDK 和 Syft 时，还会生成 MSI 与 SPDX JSON SBOM。脚本不包含 GitHub、Git
push、GHCR push 或其他上传操作。

Windows 可直接双击项目根目录的 `release.bat` 一键发布。脚本从源码当前版本自动计
算下一版本：patch 使用单个数字，从 `x.y.0` 递增到 `x.y.9`；例如 `1.1.8` 的下一版
本是 `1.1.9`，随后进位到 `1.2.0`。它会同步源码、
README、版本化裸二进制 Compose 路径、环境示例、工作流默认版本和 CHANGELOG，
在独立 `.tmp` 候选目录完成测试、Compose 配置检查、三平台构建、中文 MSI 与归档校验，
全部成功后才创建
`dist/GBaseLite-<VERSION>` 独立目录；失败会恢复原版本文件。每个版本目录都包含自
己的 `checksums.txt`、归档、MSI 和裸 Linux 二进制，旧版本目录不会被覆盖；
`docker-compose.binary.yml` 同步更新版本化裸二进制路径，标准镜像 Compose 始终使用
`latest`。Docker CLI 或 Compose 不可用时会给出警告并跳过该项检查。

首次运行缺少 .NET SDK、WiX 或中文 UI 扩展时，脚本会下载到 `.tmp` 或用户 WiX 扩展
缓存，不修改系统 `PATH`；即使手工清空 `.tmp`，下次发布也会重新下载并重建缓存。发
布入口和 PowerShell 构建脚本统一使用 UTF-8 控制台编码，`.NET`、WiX 及中文警告不
会在 CMD 或 Windows Terminal 中显示为乱码。安装器输出只显示在终端，不会混入后续
工具路径。格式检查只遍历项目源码，跳过 `.tmp`、隐藏目录、`data`、`logs`、`dist`、`bin`、`release`、`vendor`、`node_modules` 和重解析点；不会因下载工具缓存中的第三方 Go 源码报错。新增且尚未提交的包、Windows/Linux 平台源码仍会检查，项目源码格式不合格仍会阻止发布。`--self-test` 同时验证此检查范围。双击执行结束后窗口会保留；自动化调用可使用：

```powershell
.\release.bat --no-pause
.\release.bat --preview --no-pause
.\release.bat --self-test --no-pause
```

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\build-release.ps1 `
  -Version 1.1.2 -GoExecutable D:\env\Go\bin\go.exe
```

兼容入口 `scripts/package.ps1` 会转发到同一脚本。Linux 构建机可以使用：

```bash
VERSION=1.1.2 ./scripts/build-release.sh
```

MSI 单独构建：

```powershell
dotnet tool install --global wix --version 5.0.2
wix extension add --global WixToolset.UI.wixext/5.0.2
.\scripts\build-msi.ps1 -Version 1.1.2 `
  -SourceDirectory .\.tmp\windows-package `
  -OutputPath .\dist\GBaseLite-windows-amd64.msi
```

一键发布的最终目录结构：

```text
dist/
└── GBaseLite-<VERSION>/
    ├── GBaseLite-windows-amd64.msi                 # WiX 可用时
    ├── gbaselite-windows-amd64.zip
    ├── gbaselite-linux-amd64.tar.gz
    ├── gbaselite-linux-arm64.tar.gz
    ├── gbaselite-linux-amd64                       # Compose 直接挂载
    ├── gbaselite-linux-arm64                       # Compose 直接挂载
    ├── checksums.txt
    └── sbom.spdx.json                              # Syft 可用时
```

版本号只保留在 `GBaseLite-<VERSION>` 目录名中，目录内所有发布文件名均不重复携带
版本号。其中两个裸 Linux 二进制专供 `docker-compose.binary.yml` 直接挂载，并纳入
同目录的 `checksums.txt`。Windows ZIP 包含 `gbaselite.exe`、
`config.example.yaml`、README、LICENSE 和三个 `.bat`；Linux 包含静态
`gbaselite`、相同文档和三个带执行权限的 `.sh`。归档不包含 `data`、`logs`、业务备
份、`config.yaml`、密码、`.tmp`、`bin` 或开发缓存。

## GitHub 与 Docker Hub 发布

正式发布位置固定为：

- GitHub：[`pucj0/gbaselite`](https://github.com/pucj0/gbaselite)
- Docker Hub：[`pucj/gbaselite`](https://hub.docker.com/r/pucj/gbaselite)
- GHCR：`ghcr.io/pucj0/gbaselite`

`release.bat` 只负责在当前工作目录计算下一版本并完成本地打包，不执行远程发布。
正式发布使用独立入口 `publish-release.bat`，版本必须显式传入。脚本不会切换或修改
当前开发分支，而是在 `.tmp` 中基于 `HEAD` 创建
`release/v<VERSION>` Git worktree，并只在该发布分支中同步运行时版本、
README、版本化裸二进制 Compose 路径、环境模板、GitHub Release 默认版本和
CHANGELOG。

发布分支随后调用现有 `scripts/one-click-release.ps1 -TargetVersion <VERSION>`，
完整执行 Compose 检查、`gofmt`、`go test ./... -count=1`、
`go vet ./...`、Windows amd64/Linux amd64/Linux arm64 构建、MSI、归档、
ELF、执行权限、禁入内容和 SHA-256 校验；校验和通过 .NET 标准加密库计算，不依赖
可选的 `Get-FileHash` cmdlet。产物仍复制到
`dist/GBaseLite-<VERSION>`，开发分支中的版本文件保持不变。

GitHub tag 工作流也会用同一源码一次构建 `linux/amd64`、`linux/arm64`，同时推送
GHCR 和 Docker Hub 的精确版本、`major.minor` 与 `latest` 标签。GHCR 使用
`GITHUB_TOKEN`；Docker Hub 必须在仓库 Actions Secrets 中配置
`DOCKERHUB_USERNAME` 和仅具备目标仓库读写权限的 `DOCKERHUB_TOKEN`。Secrets
不会写入镜像、日志或发布产物。该路径可用于 CI 发布；本地
`publish-release.bat -Publish` 仍会在推送 Git tag 前直接构建并推送 Docker Hub，
因此只应选择其中一条 Docker Hub 发布路径，避免重复推送同一版本。

远程发布前必须满足：

- 当前目录是 `https://github.com/pucj0/gbaselite.git` 的干净 Git 工作树
- Git 已配置 `user.name`、`user.email` 和 GitHub 推送凭据
- Docker Desktop/Engine、Buildx 可用，并已执行 `docker login` 登录有权推送
  `pucj/gbaselite` 的 Docker Hub 账号
- `release/v<VERSION>` 分支和 `v<VERSION>` tag 在本地及远程均不存在
- 对已有同版本产物的重建必须显式加 `-ReplaceArtifacts`

推荐先后执行：

```powershell
.\publish-release.bat -SelfTest
.\publish-release.bat -Version 1.1.2 -DryRun -ReplaceArtifacts
.\publish-release.bat -Version 1.1.2 -PrepareOnly -ReplaceArtifacts
.\publish-release.bat -Version 1.1.2 -Publish -ReplaceArtifacts
```

`publish-release.bat` 默认在完成或失败后暂停，双击运行时可以看到完整输出；自动化或已打开的
终端中可加 `--no-pause`（或 `-NoPause`）避免等待按键。发布失败时先阅读 PowerShell
错误正文，末尾的退出码只用于脚本调用方判断成功或失败。

三种模式互斥：

- `-DryRun`：只检查 Git、远程引用、Docker/Buildx 和目标产物冲突，不创建任何内容
- `-PrepareOnly`：创建本地发布分支、tag 和完整产物，不推送 GitHub 或 Docker Hub
- `-Publish`：先构建 amd64 临时镜像并运行容器健康检查，再把 amd64/arm64 的
  `<VERSION>`、`major.minor`、`latest` 三组标签推送到
  `pucj/gbaselite`，读取远端 manifest 确认同时包含 `linux/amd64` 和
  `linux/arm64`，最后使用一次原子 Git push 推送发布分支和 tag

Dockerfile 必须直接使用 Buildx 自动提供的 `TARGETOS` 和 `TARGETARCH`，不能为它们
设置固定架构默认值；发布脚本会在自检和远端 manifest 验收中阻止架构不完整的发布。

tag 推送后，`.github/workflows/release.yml` 会异步创建 GitHub Release 并上传
本次版本附件，`.github/workflows/docker.yml` 会发布 GHCR 多架构镜像；Docker
Hub 仅由专用发布脚本推送，避免同一 tag 被本地和 GitHub Actions 重复构建覆盖。

Docker 推送失败时不会推送 GitHub 分支或 tag。若 Docker 已成功而最后的原子 Git push
失败，脚本会保留隔离 worktree、本地发布分支、tag 和产物供人工核对后重试。成功后默认
删除隔离 worktree；调试时可加 `-KeepWorktree`。


## 贡献规范

提交变更时请遵循以下要求：

1. 先添加或更新与风险相匹配的测试。
2. 运行 `gofmt`、`go test ./...` 和 `go vet ./...`。
3. SQL 兼容功能必须说明支持范围，不能宣称完整 MySQL 兼容。
4. 不得提交 `data`、`logs`、`.tmp`、本地密码或业务备份。
5. 不得使用业务数据库执行破坏性测试；写测试应使用独立临时数据库并在完成后清理。
6. **任何功能、SQL、CLI、配置、持久化格式、部署方式或兼容边界的变更，都必须在同
   一个变更中同步更新本 README。**

## 开源许可证

GBaseLite 使用 [MIT License](LICENSE) 发布。分发源码或二进制时必须保留许可证和版
权声明。安全问题请按 [SECURITY.md](SECURITY.md) 私下报告，版本变化记录在
[CHANGELOG.md](CHANGELOG.md)。

## 路线图

- 扩展索引计划到 `IN`、多范围、JOIN、分组聚合和游标分页
- 完善算术表达式和更多 MySQL 函数
- 扩展 `information_schema` 客户端兼容范围
- 优化大视图物化和谓词下推
- 增加审计轮转、指标和更完整的可观测性
- 继续维护持久化格式迁移和升级恢复演练
- 增加 MSI 安装器自动化测试和签名流程

### 历史性能优化记录

以下记录对应各优化当时的验证，不代表本次单引擎改造重新跑过这些性能负载。

#### 有界写入批次优化

单机 MVCC 大事务的数据版本安装批次采用 256 KiB 逻辑预算；每批仍同步磁盘，全部安装完成后才同步发布提交标记。事务总写集仍可落盘增长，复制命令与扫描批次保持原预算。256 KiB 是键、值及估算开销的批次预算，不是进程物理内存硬上限。顺序 WAL 与布局迁移的后续实现及限制见 MVCC 文档。

MVCC SELECT、UPDATE、DELETE 可利用已有单列或复合唯一索引进行全键整数等值定位，再回表验证完整 WHERE。该路径只接受安全整数比较范围内的非 NULL 常量；部分复合键、文本、浮点和不安全转换保持扫描。普通覆盖索引的实现见 MVCC 文档；CREATE INDEX 现走原子影子重建，具体边界见 MVCC 文档。

简单 MVCC SELECT 过滤条件在每次查询开始时绑定列位置，逐行求值仍复用原 SQL 比较器，保留 NULL、数值与排序规则语义。绑定计划不跨查询缓存，复杂表达式使用原执行路径；这尚不是完整向量化执行器。

单机历史版本清理按有界批次释放提交和快照锁，每批重新计算最老活跃快照；同一行版本过多时从该行继续，不再重新遍历之前的所有行。恢复快照会使正在进行的清理退出。清理释放可复用页，不承诺立即缩小数据库文件；集群仍需统一保留边界。SQL 解析缓存维持 512 条、单条 SQL 不超过 8192 字节，满载时按插入顺序逐条替换，避免整表清空引起集中重新解析；缓存仅保存语法树，不保存执行计划。

单机小事务支持有界组提交：首个请求立即同步，在其执行期间到达的请求可合并下一次物理提交，不设置等待定时器。排队最多 32 个请求、512 KiB 逻辑预算，每组最多 256 KiB；队列满时使用串行提交。每个事务分别验证版本冲突，组内后续事务能够看到前序写入的提交标记，全部成功事务共享磁盘同步；复制路径保持原协议。取消与落盘竞争时等待确定结果，不将已提交事务报告为回滚。

版本布局另有仅用于测试的平铺键原型：验证带零字节行键、历史快照、删除和未发布版本的可见性，并提供 `go test ./mvcc -run TestFlatVersionLayoutPrototype -bench BenchmarkVersionLayoutPrototype -benchmem` 对比。后续已接入平铺布局实际读写及离线双向迁移，默认新库仍使用嵌套布局，具体命令见 MVCC 文档。

主键点查直接使用单行访问路径，不额外创建扫描过滤绑定或计算未使用的范围边界。

在本机双 Go 执行线程、8 个并发工作者的同步持久化微基准中，三轮组提交摊销时间中位数为 0.486 ms/op，逐事务物理提交为 1.925 ms/op，吞吐约为 3.96 倍；该数值不是单请求延迟。平铺布局的 1 万行、每行 3 版本合成测试中，逻辑文件占用约从 1384 降到 1050 字节/行，点读中位数从 1159 到 1178 ns/op，没有点读提速证据，尚不支持据此自动迁移业务库。

本轮最终候选在相同 TCP、同步持久化、Go 64 MiB 软目标、Windows 64 MiB 工作集配置、双执行线程下，按 AB/BA/AB 完成三轮十万/百万行对照，中位数如下。该结果不替换原四库报告的历史测量。

| 数据量 | 原子 UPDATE 原版→候选 | UPDATE CPU 原版→候选 | 工作集峰值原版→候选 | 私有内存峰值原版→候选 |
| --- | --- | --- | --- | --- |
| 10 万行 | 1.835→1.312 秒（-28.5%） | 1.359→1.172 秒 | 71.75→71.79 MiB | 54.13→63.53 MiB |
| 100 万行 | 17.904→13.300 秒（-25.7%） | 12.766→12.219 秒 | 71.84→71.80 MiB | 68.32→70.13 MiB |

百万行点查 P95 为 0.346→0.330 ms，聚合 1.345→1.331 秒；INSERT 25.819→26.082 秒，整套负载 CPU 39.375→39.469 秒，数据库目录仍约 1 GiB。私有内存存在回退，不能把更新提速表述为所有性能或内存指标均改善。首个候选曾出现点查回退，其结果也保留在 `.tmp/priority-perf/对照`；最终数据为 `.tmp/priority-perf/最终对照`，程序和源码哈希、检查日志、原型基准与结果汇总均位于 `.tmp/priority-perf`。

最终候选已通过 `gofmt`、`go test ./...`、`go vet ./...`，百万行全内容/旧快照/重启校验，以及10万宽行、有效载荷307200000字节（超过256 MiB）的原子 UPDATE 校验；两种数据集重启后 pending 为0，临时库均已清理。验证没有替换正式服务或改写 `data/`。

## MVCC 业务兼容与维护

当前状态以本页矩阵为准；详细设计和历史演进见 [MVCC 文档](docs/使用文档/MVCC复制与高可用.md)。

A01–A03 Deep Hardening：SELECT 与 DML 的表/范围扫描直接使用后端 Iterator 接入 Physical Scan；历史 callback 适配单独隔离于 compat_sources，复杂索引访问暂保留一个字节访问适配边界。此清理保持现有 SQL 与事务语义。

Deep Hardening 的输出绑定使用 boundQuery（列描述 + Operator），SELECT/GROUP/Window/UNION 可直接进入 Distinct，只有最终结果边界收集 Result.Rows；最终结果预算、Distinct/Sort 的溢写预算仍生效。事务查询在事务结束前完成消费，暂不让惰性协议流持有已关闭的 Txn；外部已物化输入仍支持 StreamRows 边界。

Physical Join3[L,R,O] 支持不同的左右输入及输出类型；现有 Join[T] 作为同类型兼容外壳委托同一个执行算法。INNER/LEFT、索引探测和表扫描的语义保持不变。

Engine 最小接口为 Begin(ctx)/Close；RevisionReader、CounterAllocator、ReplicatedEngine、Availability、Maintenance、Diagnostics 均独立。缺失 RevisionReader 时通过普通 Txn 快照刷新元数据；自增/维护等显式能力缺失返回 ErrUnsupported。FullEngine 保留原完整方法集合用于嵌入方的兼容迁移。已有完整后端无需修改实现。

SQL namespace 与有序整数主键基础编码集中到 sqllayout：row/index/secondary/catalog 及计数器键只在此定义。storageengine 的 Table/Index 兼容便利句柄委托该布局模块，Txn 的 opaque namespace/bytes 语义保持不变；所有既有编码逐字节兼容。

执行层按职责命名：transaction_engine、access_plan、row_layout 等模块只依赖通用存储接口；原 SetMVCCAutocommit Go API 保留为 SetAutocommit 的兼容转发。协议消息与持久化编码不变，审计见 docs/architecture/executor-dependency-audit.md。

EXPLAIN 的 Extra 追加 Pipeline 算子树，由同一次 SQL 绑定生成；索引访问类型与运行计划共享绑定结果。计划不执行 Scan 或 Join 探测，动态内侧扫描明确标注 DynamicScan。PlanNode 为成本、基数和实际行数预留可选字段，当前未实现代价优化器或 EXPLAIN ANALYZE。

Aggregate 的 Accumulator 与 Window 的 PartitionStore/EvaluateStore 为外部聚合和分区落盘预留接口，统一每次运行的资源所有权与 Close。当前 SQL 聚合和窗口仍按原预算保存在内存；窗口表达式保留切片兼容边界，未宣称已实现窗口落盘。设计边界见 docs/architecture/spill-stores.md。

离线迁移编排与 snapshot/paged reader 已移至 migration/legacy，生产 Executor 不再读取旧存储格式。migrate-legacy 命令及源目录只读、目标重开验证语义保持不变。Go 调用入口改为 legacy.Migrate(ctx, source, target, opener)，由命令装配 executor.Open；逻辑 ImportSnapshot/VerifySnapshot 只面向隔离目标，导入失败必须丢弃目标（包含独立计数器变化）。
