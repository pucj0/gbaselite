# GBaseLite

GBaseLite 是一个使用 Go 编写的单机轻量级关系型数据库服务。它不依赖 MySQL 或
PostgreSQL，使用自己的存储文件持久化数据，并通过 MySQL 协议向 Navicat、DBeaver、
JDBC、Go MySQL 驱动等客户端提供服务。

当前版本：`1.0.002`

> GBaseLite 是 MySQL 兼容子集，不是 MySQL 的完整替代品。生产使用前请阅读
> [兼容范围与限制](#兼容范围与限制)，并使用真实业务 SQL 做完整验证。

## 目录

- [使用者入口](#使用者入口)
  - [SQL 使用教程](docs/sql-user-guide.zh-CN.md)
  - [SQL 兼容参考](docs/sql-compatibility-reference.zh-CN.md)
- [项目定位](#项目定位)
- [主要能力](#主要能力)
- [快速开始](#快速开始)
- [配置](#配置)
- [部署](#部署)
- [升级与回滚](#升级与回滚)
- [SQL 示例](#sql-示例)
- [兼容范围与限制](#兼容范围与限制)
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

本 README 同时说明安装、部署、备份和项目维护。若你的工作是在 Navicat、DBeaver 或
MySQL 客户端中建表、写查询和维护业务数据，请从
[SQL 使用教程](docs/sql-user-guide.zh-CN.md) 开始。教程使用独立练习库，覆盖连
接、建表、增删改查、统计、关联、事务、视图、索引、Navicat 传输和常见限制。

需要按语法类别确认兼容范围时，使用
[SQL 兼容参考](docs/sql-compatibility-reference.zh-CN.md)。它列出当前支持的 DDL、
DML、查询、元数据语句、事务行为、常见错误码和明确不支持的能力。两份文档都以当前
源码为准；已部署实例仍需先用 `SELECT VERSION()` 确认实际版本。

使用前请确认：

- 连接地址、端口、用户名和密码由管理员提供；默认端口为 `3307`
- 在生产库执行 `UPDATE`、`DELETE`、`TRUNCATE` 或 `DROP` 前，先用同一 `WHERE` 条
  件执行 `SELECT` 核对影响范围
- 业务 SQL 来自 MySQL 时，先在测试库验证；GBaseLite 只支持同库外键约束，不支持
  跨库外键、触发器、存储过程、存储函数和事件

## 项目定位

GBaseLite 面向以下场景：

- 需要 MySQL 客户端和驱动直连，但不希望部署完整 MySQL 的小型服务
- 开发、测试、演示和教学环境
- 低资源单机部署
- 数据库协议、解析器、执行器和存储引擎的学习与实验
- 需要针对 Navicat、DBeaver 或现有业务 SQL 做定向兼容的项目

对于纯嵌入式本地应用、强可靠生产系统或高并发写入场景，应优先评估 SQLite、MySQL、
PostgreSQL 等成熟数据库。

## 主要能力

- MySQL TCP 协议、密码认证、`COM_QUERY`、PING、QUIT
- Prepared Statement 协议和二进制结果集
- 数据库和表的 DDL、CRUD、事务和文件持久化
- 原子多动作 `ALTER TABLE`、CTAS/LIKE、`RENAME TABLE` 和同库外键级联动作
- `REPLACE`、`UPDATE ... JOIN`、多表 DELETE 以及相关/非相关子查询写入
- `WHERE`、三值 NULL 逻辑、聚合、分组、排序、分页和 DISTINCT
- 派生表以及 INNER、LEFT、RIGHT、CROSS JOIN
- 非递归多 CTE、递归 CTE、常用标量函数和窗口函数
- 只读持久化视图、嵌套视图和依赖检测
- 普通索引、唯一索引、复合索引及唯一性约束
- MySQL 风格用户、密码、授权和权限元数据
- Navicat/DBeaver 常用 `information_schema` 元数据兼容
- 逻辑备份、事务恢复、MySQL 数据导入和 SQL 导出
- Windows/Linux 后台启停、健康检查和 Docker Compose 部署
- Windows Service、WiX MSI、跨平台便携包和多架构容器发布

## 快速开始

### 环境要求

- Go 1.22 或更高版本
- 可选：Docker Engine 与 Docker Compose
- 可选：WiX Toolset 5 和 .NET 8 SDK，用于构建 Windows MSI
- 可选：Syft，用于生成 SPDX JSON SBOM
- 可选：MySQL 客户端，用于协议连接验证

### 从源码构建

Windows PowerShell：

```powershell
go mod download
New-Item -ItemType Directory -Force .\bin | Out-Null
go build -trimpath -o .\bin\gbaselite.exe .\cmd\gbaselite
.\bin\gbaselite.exe start --config .\config.yaml
.\bin\gbaselite.exe healthcheck --host 127.0.0.1 --port 3307
```

Linux/macOS：

```bash
go mod download
mkdir -p ./bin
go build -trimpath -o ./bin/gbaselite ./cmd/gbaselite
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
  write_buffer_kb: 8
  slow_query_ms: 100

storage:
  path: /app/data

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

binlog:
  enabled: false
  path: /app/data/binlog.jsonl
  retention_days: 7
```

Windows 本地运行时，`/app/data` 和 `/app/logs` 会自动映射为当前目录下的 `./data`
和 `./logs`。

支持以下环境变量覆盖：

| 环境变量 | 配置项 | 默认值 |
|---|---|---|
| `DB_USER` | 初始管理员用户名 | `root` |
| `DB_PASSWORD` | 初始管理员密码 | `change-this-password` |
| `DB_HOST` | 监听地址 | `127.0.0.1` |
| `DB_PORT` | 监听端口 | `3307` |
| `DB_MAX_CONNECTIONS` | 最大活动连接数；`0` 表示不限制 | `512` |
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
| `DB_BINLOG_ENABLED` | 是否启用可重放逻辑 binlog | `false` |
| `DB_BINLOG_PATH` | 逻辑 binlog 路径 | `<storage.path>/binlog.jsonl` |
| `DB_BINLOG_RETENTION_DAYS` | 逻辑 binlog 保留天数；`0` 永久保留，最大 `365` | `7` |

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

### 审计日志与逻辑 binlog

`audit.enabled` 与 `binlog.enabled` 相互独立且默认关闭。审计日志按每次认证、普通
SQL、Prepared Statement 执行和切库操作追加一行 JSON，包含 UTC 时间、连接 ID、认
证账号、真实来源 IP/端口、当前库、操作类型、成功或失败、MySQL 错误码、影响行数、
耗时和脱敏 SQL。字符串、数字字面量及 SQL 注释不会写入审计文件；反引号标识符和语
句结构保留，便于检索。

逻辑 binlog 只记录成功持久化的 DDL、DML、账号和授权变更。自动提交语句各占一条记
录；显式事务在 `COMMIT` 后将其中的变更合并为一个记录，`ROLLBACK`、执行失败和连接
断开回滚不会写入。记录保留原始 SQL、原连接会话、数据库、提交时间、影响行数和递增
序号，因此可能包含密码或业务数据；文件以 `0600` 创建，仍必须限制目录访问并纳入敏
感数据管理。

`audit.retention_days` 和 `binlog.retention_days` 分别控制保留期，默认 `7` 天；
`0` 表示永久保留，允许范围为 `0` 到 `365`。服务启动时会立即检查，持续运行时每 24
小时检查一次，按记录中的 UTC 时间原子压缩同一个 JSONL 文件，文件名不会变化。审计
/binlog 当前没有按文件大小轮转；上面的大小轮转仅用于 `gbaselite.log`。binlog 通
过同目录下的小型 `.sequence` 状态文件保持清理前后的递增序号。审计或 binlog 写入
失败会写入主日志；binlog 在数据已经持久化后、向客户端返回成功前同步写盘，若同步
失败会明确返回“变更已提交但 binlog 追加失败”。它是用于“基础备份 + 后续事务重放”
的逻辑日志，不是带页 LSN 的物理 WAL；进程在数据快照落盘与 binlog 落盘之间被强制
终止时，最后一个事务仍存在极小的缺失窗口。启用有限 binlog 保留期时，基础备份及其
起始序号必须位于保留窗口内；否则过期事务已被清理，无法组成连续恢复链。

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
MiB，并根据数据量、视图物化和并发查询继续提高。不要使用 `docker compose down -v`
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

`GBaseLite-windows-amd64.msi` 安装到 `%ProgramFiles%\GBaseLite`，将安装目录加入
系统 `PATH`，并注册 `GBaseLite` Windows Service。安装界面可以分别选择是否随
Windows 自动启动、是否在安装完成后立即启动。安装包产品语言及 WiX 标准、自定义对
话框均为简体中文；许可协议页显示中文许可文本，欢迎页、横幅和配置页使用统一的
GBaseLite 视觉样式。安装或升级完成后需要关闭并重新打开命令提示符、PowerShell 或
终端，新的进程才会继承系统 `PATH`；随后可直接执行 `gbaselite -u root -p`。安装器
使用独立 MSI 组件追加 `%ProgramFiles%\GBaseLite`，卸载时只移除该路径项，不覆盖系
统原有 `PATH`。配置、数据和日志的默认位置分别为：

```text
%ProgramData%\GBaseLite\config.yaml
%ProgramData%\GBaseLite\data
%ProgramData%\GBaseLite\logs
```

安装目录只包含程序、说明、许可和图标，不包含 `config.example.yaml` 或另一份
`config.yaml`。MSI 管理的服务只读取上述 `%ProgramData%` 中的正式配置，避免出现两
份配置文件而误改未生效的那一份。便携 ZIP 包仍保留 `config.example.yaml`，供其启
停脚本在首次运行时生成本地 `config.yaml`。

MSI 新安装默认只监听 `127.0.0.1`；升级或“更改”会保留已有 `server.host`，不会擅自
中断原有可信局域网连接。安装器会将正式配置文件、数据目录和日志目录的 Windows ACL
收紧为仅 `SYSTEM` 与本机管理员完全控制，后续创建的用户目录、审计日志和 binlog 继
承该权限。TLS 不在 MSI 交互页中自动生成或选择证书。管理员可在正式 `config.yaml`
中配置 `tls` 段；后续升级或“更改”重写配置时会原样保留 TLS 开关、证书路径和强制安
全传输设置。主日志大小阈值和保留期同样从正式配置中保留；安装器不删除已有主日志或
轮转文件。

交互安装界面允许设置端口、管理员用户名、数据目录、开机自启、是否立即启动，以及是
否创建桌面快捷方式。数据库配置页之后有独立的“审计与恢复日志”页，可分别勾选结构化
审计日志和可重放逻辑 binlog，并分别设置保留天数。默认 `7` 天，`0` 永久保留，最大
`365` 天；两项日志默认均不勾选。升级或“更改”安装时会从正式 `config.yaml` 回显已
有开关和保留天数。桌面快捷方式默认不勾选，只有用户主动选择时才创建。"重新初始化
数据"默认不勾选；只有手工勾选并进入第二个确认页面后才会创建新的空数据目录。首次
安装或选择重新初始化时必须输入管理员密码；升级并保留现有配置和数据时密码可留空，
此时安装器保留配置中已有值；已按上述流程清空的 bootstrap 密码也会保持为空。安装
器会从 64 位系统注册表自动回填上次选择的安装目录、数据目录和日志目录；这些目录记
录在卸载后仍会保留，重新安装或升级时无需重复选择。首次安装的数据目录默认显示
`%ProgramData%\GBaseLite\data`，既可直接输入绝对路径，也可使用 `Browse...` 打开
Windows 文件夹选择器。密码属性会被 MSI 隐藏，并通过进程内自定义操作写配置，不会
拼接到命令行。不要在 `msiexec` 命令行上传入密码；当前版本不支持无人值守的初次密
码配置。

选择"重新初始化数据"后，安装器会把原数据目录改名为同级的
`数据目录.gbaselite-backup-<随机标识>` 备份，并立即使用新的空数据目录；不会在安
装界面中递归删除该备份。因此大数据目录不会让安装长时间停在"正在删除备份文件"。确
认新服务可用且不再需要回退后，再由管理员手动删除这个明确的备份目录；不要删除当前
配置正在使用的数据目录。

通过 `gbaselite.exe start` 启动的是独立后台进程，不会出现在控制面板的卸载列表或
Windows 服务管理器中。MSI 安装时会先停止名为 `GBaseLite` 的现有 Windows
Service，然后检查所选端口的监听进程；只有监听 PID 的进程名准确为 `gbaselite`
时，安装程序才会关闭该独立实例并切换为 MSI 管理的 Windows Service。若端口由其他
程序占用，安装程序不会关闭它，服务启动失败会写入 MSI 日志但不会删除或回滚已有数
据。勾选“安装完成后立即启动”时，安装器只发起 Windows Service 启动请求，不等待服
务达到运行状态；端口被其他程序占用、配置错误或服务启动失败时，安装界面会立即完
成。此时请先释放或更换端口，再在 Windows 服务管理器中启动 `GBaseLite` 并检查服务
状态。在“更改”或升级配置页确认端口、管理员用户名或数据目录后，安装器会写回
`%ProgramData%\GBaseLite\config.yaml`；密码留空时会保留已有密码。因此将端口从
`3307` 改为 `3308` 后，服务会实际监听 `3308`，而不是继续使用旧配置中的 `3307`。
写回配置时，安装器按“审计与恢复日志”页的选择更新 `audit.enabled` 和
`binlog.enabled`，同时保留已有 `audit.path` 和 `binlog.path`。首次安装时分别使用
日志目录和数据目录下的默认 JSONL 路径，并使用 `7` 天保留期；静默安装或“修复”不经
过该页，不会意外改变已有开关和保留天数。点击配置页“下一步”时，安装器会检查所选端
口。端口被非 GBaseLite 程序占用会显示提示并阻止继续；端口被已有 GBaseLite
Windows Service 或独立 `gbaselite.exe` 占用时可以继续，安装器会停止旧实例、应用
新配置后再启动服务。

MSI 成功安装后，GBaseLite 会出现在 Windows“已安装的应用/程序和功能”中，也会在开
始菜单创建带 GBaseLite 图标的命令行入口，并在服务管理器中注册为 `GBaseLite`。安
装目录同时包含多尺寸 `gbaselite.ico`，“已安装的应用”、开始菜单和用户选择创建的桌
面快捷方式使用同一图标。之前从 ZIP 或源码直接运行的版本没有系统卸载项，停止对应
进程后删除原程序目录即可；数据目录应按备份与保留策略单独处理。

再次运行已安装版本的 MSI 时，“更改”和“修复”均可使用。“更改”会重新打开数据库配置
页，可调整服务配置并添加或移除桌面快捷方式；安装器会从注册表恢复当前快捷方式状
态。“修复”会恢复缺失或损坏的程序文件、服务、快捷方式和注册表项，但不会勾选 “重新
初始化数据”，也不会删除或覆盖现有 `config.yaml`、`data`、用户和日志。

MSI 的 WiX DTF 自定义操作显式使用 CLR v4，可在仅启用 .NET Framework 4.x 的受支持
Windows 系统上运行。若安装仍异常，可使用 `/L*V` 生成详细日志进行定位：

```powershell
msiexec.exe /i .\dist\GBaseLite-1.0.002\GBaseLite-windows-amd64.msi /L*V .\gbaselite-install.log
```

```powershell
# 安装
msiexec.exe /i .\dist\GBaseLite-1.0.002\GBaseLite-windows-amd64.msi

# 使用同一 MSI 修复程序文件
msiexec.exe /fa .\dist\GBaseLite-1.0.002\GBaseLite-windows-amd64.msi

# 升级：运行更高版本 MSI，UpgradeCode 保持不变
msiexec.exe /i .\dist\GBaseLite-1.0.002\GBaseLite-windows-amd64.msi

# 卸载；默认保留 config.yaml、data、users 和 logs
msiexec.exe /x .\dist\GBaseLite-1.0.002\GBaseLite-windows-amd64.msi
```

升级会显示同一配置页，停止旧服务、复用已有配置，并在安装成功后恢复服务。默认不勾
选“重新初始化数据”，因此无需重新输入原密码且只覆盖程序文件；已有 `config.yaml`、
`data`、`users` 和 `logs` 不会被 MSI 覆盖或删除。“重新初始化数据”默认关闭，选中
后还必须进入第二个确认页面。删除发生在安装提交阶段，执行前旧目录会先改名备份；失
败时自定义操作会尝试恢复原目录和配置。

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
- 按合规与恢复窗口设置审计/binlog 保留天数；超大日志还应监控磁盘并规划外部归档
- 按审计和恢复要求决定是否启用 `audit` / `binlog`，并限制 JSONL 文件访问权限
- 配置定期逻辑备份和完整数据目录备份
- 使用 healthcheck 或进程管理器监控服务
- 定期执行 `gbaselite diagnose --config <正式配置>`，并通过已认证连接检查
  `SHOW GLOBAL STATUS`
- 上线前使用真实客户端、业务 SQL 和恢复演练验证兼容性
- 保留当前二进制和数据备份，确保升级失败时可以回滚

## 升级与回滚

升级前先做业务库逻辑备份，并在停服后备份整个数据目录。逻辑备份不包含账号、密码哈
希和授权，完整数据目录备份才包含整个实例。

Windows 示例：

```powershell
.\bin\gbaselite.exe backup --all-databases --output .\backup-before-upgrade.sql
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
和 Navicat 使用说明见 [SQL 使用教程](docs/sql-user-guide.zh-CN.md)。

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
INSERT INTO users SET id=2, name='李四', phone='13800000002', age=18+7, enabled=TRUE;
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

视图：

```sql
CREATE VIEW adult_users AS
SELECT id, name FROM users WHERE age >= 18;

CREATE OR REPLACE VIEW adult_users AS
SELECT id, name FROM users WHERE age >= 21;

SHOW FULL TABLES;
SHOW CREATE VIEW adult_users;
DROP VIEW IF EXISTS adult_users;
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

开始编写业务 SQL 前，建议先阅读
[SQL 使用教程的支持边界](docs/sql-user-guide.zh-CN.md#支持边界和写入安全)，特别
是视图只读、类型语义和不支持功能的说明。需要查某一类语法时，直接查看
[SQL 兼容参考](docs/sql-compatibility-reference.zh-CN.md)。

| 分类 | 当前支持 |
|---|---|
| 数据类型 | 保留 MySQL 8 常见整数、定点/浮点、字符/二进制、TEXT/BLOB、`ENUM/SET/JSON`、日期时间及空间类型声明；写入时校验 `ENUM` 和 `JSON` 载荷 |
| DDL | 数据库、表、`CREATE TABLE ... AS SELECT/LIKE`、只读视图、主键、普通/唯一/复合索引、同库外键、`CHECK`、列的 ADD/DROP/MODIFY/CHANGE/RENAME、索引的 ADD/DROP/RENAME、列默认值的 SET/DROP、命名 `UNIQUE/FOREIGN KEY/CHECK`、逗号分隔的原子多动作 `ALTER TABLE`、`RENAME TABLE`、用户和授权的常用语法 |
| DML | `INSERT ... VALUES/VALUE/SET/SELECT`、`REPLACE ... VALUES/VALUE/SET/SELECT`、`ON DUPLICATE KEY UPDATE`、表达式 `UPDATE`、`UPDATE ... JOIN`、两种 MySQL 多表 `DELETE`、`SELECT`、行构造器与相关/非相关 `IN/EXISTS`/标量子查询、单表 `UPDATE/DELETE ... LIMIT`、`TRUNCATE` |
| 条件与表达式 | 比较、数值加减除/取模、时间值加减 `INTERVAL`、`CASE`、`LIKE`、`IN`、`BETWEEN`、`IS NULL`、`NOT`、`AND`、`OR` 和 NULL 三值逻辑 |
| 查询 | JOIN、派生表、标量及 EXISTS 子查询、可用于顶层/派生表/视图/INSERT SELECT/子查询/EXPLAIN 的 `UNION/UNION ALL`、表达式 GROUP BY、HAVING、支持普通表达式和 `CASE WHEN ... END` 排序键的 `ORDER BY`、LIMIT/OFFSET、DISTINCT、常用聚合、聚合外层标量函数及基础 `EXPLAIN SELECT` |
| CTE | 按顺序物化的非递归多 CTE；单个 `WITH RECURSIVE ... UNION ALL` 最多 1000 轮 |
| 窗口函数 | `ROW_NUMBER`、`RANK`、`DENSE_RANK`、`COUNT/SUM/AVG/MIN/MAX OVER(...)` |
| 函数 | 常用条件、字符串、数值和日期函数子集 |
| 事务 | `BEGIN`、`COMMIT`、`ROLLBACK`；SAVEPOINT 系列为客户端兼容命令 |
| 元数据 | Navicat/DBeaver 常用 `SHOW` 与 `information_schema` 查询子集，包括 `SHOW [FULL] COLUMNS ... LIKE/WHERE` 和按列名过滤 `information_schema.COLUMNS` |
| 协议 | MySQL 认证、查询、Prepared Statement、二进制结果及 INSERT OK 包生成 ID |

明确限制：

- 不是完整 MySQL 实现，不保证任意 MySQL SQL 可以直接运行
- 非引号标识符可以数字开头但不能全部是数字。查询会先精确解析 `1ssssd`；精确对象
  不存在时，可兼容回退到同库旧视图 `ssssd`，因此 `SELECT * FROM 1ssssd` 可直接执
  行且不会改名旧视图；纯数字名称仍需使用反引号
- 单机存储，不支持复制、集群、分片和高可用
- 不支持跨库外键，也不支持存储过程、存储函数、触发器和事件。同库外键会在子表
  `INSERT`/`UPDATE` 时检查父行，并执行 `ON DELETE/UPDATE CASCADE` 或 `SET NULL`；
  `SET NULL` 的全部子列必须可空。未声明动作或声明 `RESTRICT/NO ACTION` 时，更新父
  键以及删除仍被引用的父表记录会返回 MySQL 1451。`TRUNCATE` 不执行级联，仍会拒绝
  截断被非空子表引用的父表
- 新建表会持久化并执行 `PRIMARY KEY`、`UNIQUE`、同库 `FOREIGN KEY`、`CHECK`、
  `NULL/NOT NULL`、`ENUM`/`JSON` 载荷校验、字面量默认值和常用
  `CURRENT_TIMESTAMP` 默认值；`AUTO_INCREMENT` 会生成递增整数，INSERT OK 包和
  `SELECT LAST_INSERT_ID()` 返回本连接最近一次自动生成的 ID。无括号的
  `CURRENT_TIMESTAMP` 与 `CURRENT_TIMESTAMP()` 等价；DATETIME 默认值与 `NOW()`
  使用同一服务本地墙钟，不进行 UTC 墙钟转换。
  `ALTER TABLE ... ADD COLUMN` 会原子回填已有行并执行 `FIRST/AFTER` 列位置；非空
  新列在非空表上必须提供默认值。内联 `PRIMARY KEY`、`UNIQUE` 或 `CHECK` 应拆成后
  续独立约束语句。`ADD FOREIGN KEY/CHECK` 会先扫描已有行，不合规时不修改元数据；
  `DROP FOREIGN KEY/CHECK` 按约束名执行。`MODIFY/CHANGE COLUMN` 会原子转换已有值并
  同步改名列上的索引，`RENAME COLUMN` 还会同步本表及外部外键引用；`DROP COLUMN`
  在列仍被主键、索引、外键或 CHECK 使用时拒绝，必须在同一多动作语句中先删除依赖。
  `ADD PRIMARY KEY` 和 `ADD CONSTRAINT ... UNIQUE` 会在修改元数据前检查已有 NULL 或
  重复值；`ADD INDEX (column)` 可按首列生成索引名，`RENAME INDEX` 保留原索引内容。
  `ALTER COLUMN ... SET/DROP DEFAULT` 只改变后续写入的默认值，不重写历史业务值。
  同一条 `ALTER TABLE` 可以用逗号组合上述受支持的列、索引、约束和表注释动作；动作
  按书写顺序执行，任一动作失败时整条语句不保留任何修改。`RENAME TABLE` 支持同库单
  表和多表原子改名，并同步同库外键引用；暂不支持跨库改名，也不能把 `RENAME` 与其
  他 `ALTER TABLE` 动作混写
- 升级不会从旧表的行或建表历史中猜测已经丢失的 `UNIQUE`、`FOREIGN KEY` 或
  `CHECK` 元数据，也不会让 `CREATE TABLE IF NOT EXISTS` 合并结构。受早期版本影响的
  表必须先清理或重新关联无效行，再通过 `ALTER TABLE ... ADD UNIQUE INDEX` 或
  `ALTER TABLE ... ADD CONSTRAINT` 明确补齐；新增外键或 CHECK 的历史行扫描失败时
  应先修复数据
- 为兼容 Navicat 表格数据编辑和单行删除，`UPDATE`、`DELETE` 支持尾部 `LIMIT`，条
  件表达式支持 MySQL NULL 安全比较符 `<=>`；`SHOW COLUMNS/INDEX/CREATE TABLE`、
  带库名及 `LIKE`/`WHERE` 过滤的 `SHOW [FULL] COLUMNS FROM db.table` 以及
  `information_schema.COLUMNS`、`STATISTICS`、`TABLE_CONSTRAINTS`、
  `KEY_COLUMN_USAGE`、`REFERENTIAL_CONSTRAINTS` 和 `CHECK_CONSTRAINTS` 会返回主
  键、外键、CHECK、可空性、声明长度、列注释、默认值和附加属性。普通 `SELECT` 的 MySQL
  字段包会携带来源库、来源表、原字段名、声明长度及主键标志，使带主键的基础表可被
  Navicat 识别为可编辑；`DATE/DATETIME` 存储值可以和 MySQL 日期字符串在普通比较
  及 `<=>` 条件中正确匹配
- 行构造器支持 `(a,b) IN ((1,2),(3,4))`，`SELECT`、`UPDATE`、`DELETE` 的 WHERE
  条件以及 `JOIN ... ON` 支持单列或多列 `IN (SELECT ...)` / `NOT IN (SELECT ...)`、
  `EXISTS/NOT EXISTS` 和标量子查询。非相关子查询在外层扫描前执行一次并物化结果；
  相关子查询按外层行求值，可引用目标表别名，也可在内层没有同名列时使用未限定外
  层列。`UPDATE SET` 表达式同
  样支持相关标量子查询。写入使用快照行号收集变更并整句提交，后续行求值或约束失败
  时不保留前面行的修改；空结果和包含 NULL 时遵循现有三值逻辑
- 链式 `UNION`、`UNION DISTINCT` 和 `UNION ALL` 在最终结果上应用 `ORDER BY`、
  `LIMIT/OFFSET`，并可作为派生表、视图、`INSERT ... SELECT`、标量/IN/EXISTS 子查询
  和 `EXPLAIN` 输入。各 UNION 分支必须返回相同列数
- `CREATE TABLE target AS SELECT ...` 从查询结果推导列并复制数据，不复制来源表索引
  或约束；`CREATE TABLE target LIKE source` 复制列、默认值、主键/普通/唯一索引、
  CHECK 和表注释，但按 MySQL 语义不复制外键。两种语句失败时都不会留下半成品表
- `REPLACE` 支持 VALUES/VALUE/SET/SELECT 输入，会先删除与候选行任一主键或唯一索引
  冲突的全部行，再插入候选行，因此会执行这些删除触发的外键动作；影响行数为显式删
  除数加插入数。整条 REPLACE 在任一候选行失败时不保留前面候选行的修改
- `UPDATE target [alias] JOIN ...` 只允许修改第一个目标基表；同一目标行被 JOIN 多次
  命中时只更新一次，多个 SET 赋值按 MySQL 的左到右顺序求值。多表 DELETE 支持
  `DELETE t1,t2 FROM ...` 和 `DELETE FROM t1,t2 USING ...`，会对每个目标表按行去重并
  一次提交；多表 DELETE 不支持尾部 `LIMIT`
- Navicat 复制表时支持其“先建结构，再执行
  `INSERT INTO target SELECT * FROM source`” 的流程。连接先读取
  `SHOW CREATE TABLE/VIEW` 后，如果目标与已有对象撞名，或者服务端直接识别到
  Navicat 默认的 `原名_copy` / `原名_copyN`，实际副本命名为
  `原名_copy_YYMMDDNN`；`NN` 是同一原对象当天从 `01` 开始的两位递增序号，表和视
  图使用相同规则，每个原对象当天可保留 `01` 到 `99` 共 99 个顺序副本；不再生成分
  钟时间或 `_2`、`_3` 冲突后缀，也不会覆盖现有表或视图。普通建表和建视图不启用该
  改名
- Navicat 复制视图时发送的带 `ALGORITHM`、`DEFINER` 或 `SQL SECURITY` 选项的同名
  `CREATE VIEW` 也会生成 `原名_copy_YYMMDDNN`，原视图保持不变；普通同名
  `CREATE VIEW` 仍返回对象已存在，`CREATE OR REPLACE VIEW` 和 `ALTER VIEW` 仍按
  标准语义执行。发生服务端改名时，OK 包会直接携带最终对象名并设置 MySQL
  `SERVER_STATUS_METADATA_CHANGED`；同一连接后续以 `_copy` / `_copyN` 查询元数据
  或插入数据时也会解析到最终对象。占位名称不会写入存储，也不会由 `SHOW TABLES` /
  `SHOW FULL TABLES` 暂时暴露后再消失。Navicat 17 仍可能在复制完成后短暂保留其本
  地乐观创建的占位项，按 `F5` 后会按服务端元数据移除
- GBaseLite 到 GBaseLite 的 Navicat 数据传输保持 MySQL 重复键语义：普通 `INSERT`
  写入已存在的主键会返回 1062，不会静默覆盖目标数据。需要完整镜像时应在 Navicat
  中选择删除目标记录或删除目标表后再传输；需要保留目标已有数据时可使用
  `INSERT IGNORE`，GBaseLite 会跳过重复的唯一键或主键行，并只统计实际新增行。重
  新传输前可先比较源、目标行数，避免对已经完成的数据再次写入。数据传输执行
  `SHOW CREATE` 后再删除并原名重建目标表或视图时，会清除复制对象上下文，不会误生
  成 `_copy_YYMMDDNN` 对象或让并行传输任务串用最后一个对象名。Navicat 并行传输表
  及其视图时，带 `ALGORITHM`、`DEFINER` 或 `SQL SECURITY` 的视图定义可以先于依赖
  表登记；依赖表随后建立后视图即可查询。该兼容只延迟“依赖对象尚不存在”的校验，普
  通 `CREATE VIEW`、错误字段和循环引用仍立即失败
- `SHOW TABLE STATUS [FROM database] LIKE 'table'` 会严格按 MySQL `LIKE` 条件过
  滤。Navicat 检查目标对象时不会再收到库中全部表并把所选的 `auth_app` /
  `auth_code` 错误映射为最后一张 `auth_code_equ`，多个数据传输 worker 也不会因此
  同时删除和创建同一个目标表
- `SHOW TABLE STATUS` 和 `information_schema.TABLES` 会返回行数、估算数据长度、
  表注释、创建时间和更新时间。表注释及时间会持久化；旧存储格式没有时间字段时，首
  次加载使用存储快照修改时间作为保守回退，查询时按服务本地时区显示并精确到秒。数
  据长度是逻辑载荷估算值，不是磁盘占用量；长度在写入时增量维护，元数据读取不再扫
  描整表。元数据兼容查询支持单列 `COUNT(*)` 投影，无匹配行时返回一行 `0`
- Navicat 17 的“转储 SQL 文件 -> 结构和数据”使用已选数据库作为脚本上下文，其原生
  输出通常不包含 `CREATE DATABASE` 或 `USE`。应以文件中的 `CREATE TABLE`、
  `INSERT` 和结尾的 `SET FOREIGN_KEY_CHECKS = 1` 判断转储是否完整；需要包含建库
  与切库语句的自包含脚本时，使用下文的
  `gbaselite backup --database ... --output ...`
- 导入 Navicat 转储时，文件头块注释或行注释可以与紧随其后的 `SET NAMES`、
  `SET FOREIGN_KEY_CHECKS` 一起通过 MySQL 协议发送；兼容层会忽略这些前导普通注
  释，不会把会话设置误判为 `SET PASSWORD`
- Navicat 的数据库转储会通过服务端预处理的 `SHOW FULL TABLES` 和
  `information_schema` 查询枚举对象；带 `-` 的库名可作为已选数据库正常导出。
  `CREATE TABLE` 中声明的命名 `UNIQUE KEY` 和普通 `KEY` 会持久化，并出现在
  `SHOW CREATE TABLE` 和逻辑备份中；引用不存在字段的内联索引会让整条建表失败，不
  会留下缺索引的半成品表
- 升级前已经存在且没有主键的业务表不会被自动修改。Navicat 仍会把这类表判定为只
  读；应先确认某列非空且唯一，再显式执行
  `ALTER TABLE table_name ADD PRIMARY KEY(column)`
- 字段元数据会持久化长度、`DECIMAL(p,s)`、日期时间精度、`UNSIGNED/ZEROFILL` 和
  `ENUM/SET` 参数；覆盖 `BIT/TINYINT/SMALLINT/MEDIUMINT/INT/BIGINT`、
  `DECIMAL/NUMERIC/FLOAT/DOUBLE`、`CHAR/VARCHAR/BINARY/VARBINARY`、四类
  `TEXT/BLOB`、`ENUM/SET/JSON`、`DATE/TIME/DATETIME/TIMESTAMP/YEAR` 以及 MySQL
  空间类型名称
- 类型声明兼容不等于完整 MySQL 类型语义：`DECIMAL` 当前映射为浮点数，
  `UNSIGNED/ZEROFILL` 主要保留元数据但未完整执行范围与显示规则，`TIME`、二进制、
  JSON、ENUM/SET 和空间类型当前以文本载荷保存；JSON 只校验语法，ENUM 只校验成员，
  SET 和空间类型尚未执行 MySQL 的全部校验、运算、排序、字符集和空间函数语义
- 视图只读；`WITH CHECK OPTION` 只接受语法，不执行可更新视图检查
- `LEFT/RIGHT JOIN` 的未匹配侧在临时结果中按可空列处理，即使来源列是主键或
  `NOT NULL`，也能按 SQL 外连接语义返回 `NULL`
- 窗口函数暂不支持显式 frame、命名窗口和同层 GROUP BY 后直接窗口计算
- 普通、唯一和复合索引会维护有序内存访问路径。基础表 `SELECT` 可使用复合索引左前
  缀等值、紧随其后的单列范围、匹配索引顺序的正序/倒序输出及提前 `LIMIT/OFFSET`；
  简单 `COUNT(*)` 也会复用相同的有界扫描。单列主键和唯一索引的等值条件仍使用紧凑
  映射直接定位
- `EXPLAIN SELECT` 会输出访问类型、候选索引、实际索引、估算行数和额外排序/索引条
  件。当前索引计划不覆盖 `OR`、`IN`、函数包裹索引列、窗口/分组表达式、派生表和
  JOIN 两侧访问；这些路径仍可能扫描、物化或排序。内存索引会在写入时同步重建，不能
  等同于成熟数据库的磁盘 B-tree 成本模型
- 两表之间的纯列等值 `INNER/LEFT/RIGHT JOIN` 使用哈希连接；复合 `ON` 条件、范围
  连接和函数连接仍使用通用嵌套扫描
- 并发写使用同步组提交：同一落盘周期中的请求共享快照编码和 `fsync`，每个请求仍在
  覆盖自身变更的持久化完成后返回；每次快照后会把协调权交给等待写入，避免持续写入
  时单个 leader 请求被后续批次长期占用。无序结果通过不可变行快照流式发送，不复制
  整批行头；文本与 Prepared Statement 二进制协议行使用有上限的共享缓冲池，超过
  64 KiB 的大缓冲不会长期留在池中。重复 SQL 使用最多 512 条、单条不超过 8 KiB 的
  解析缓存，缓存满后整代淘汰；字段布局以不可变快照发布，使查询无需复制字段定义或
  持有结构锁
- 事务快照共享不可变行载荷，首次修改时才复制受影响的行；表结构、行头和唯一索引映
  射仍按事务独立保存。显式事务采用实例级独占事务门，同一会话读取自己的事务快照，
  其他会话的读取和写入都等待 `COMMIT`、`ROLLBACK` 或断连自动回滚；当前没有可选择
  的隔离级别，该行为比 MySQL 默认 `REPEATABLE READ` 更保守，长事务会阻塞其他会
  话。事务中已经分配的 `AUTO_INCREMENT` ID 在回滚后不会复用，普通 `DELETE` 也不
  会重置计数；`TRUNCATE` 会重置计数
- 大视图当前会物化结果，尚未实现完整谓词下推
- `SHOW DATABASES` / `SHOW SCHEMAS` 只列出当前账户可进入的持久化业务库；
  `information_schema` 元数据查询仍为客户端兼容子集，但它和 `mysql` 都不是可
  `USE` 的系统库，因此不会出现在该列表
- TLS 仅支持服务器证书和 TLS 1.2 以上传输加密，尚无自动签发/轮换和客户端证书认
  证；当前也没有在线物理备份和按文件大小日志轮转

## 备份与恢复

### 内置逻辑备份

```powershell
# 单库
gbaselite backup --database test --output test.sql

# 所有持久化业务库
gbaselite backup --all-databases --output all.sql

# 仅结构或仅数据
gbaselite backup --database test --no-data --output test-schema.sql
gbaselite backup --database test --no-create-info --output test-data.sql

# 恢复时先删除同名数据库
gbaselite backup --database test --add-drop-database --output test-replace.sql
```

备份文件包含表、索引、数据和按依赖排序的视图，并通过临时文件原子替换输出。

### 恢复

恢复是离线操作。所有 SQL 在一个事务中执行，任一语句失败会回滚整个恢复过程。

```powershell
gbaselite stop --config config.yaml
gbaselite restore --config config.yaml --input test.sql
gbaselite start --config config.yaml
```

### 逻辑 binlog 重放

先把早于目标时间的逻辑备份恢复到独立、已停服的数据目录，再按序重放备份之后的记
录：

```powershell
# 恢复前先只读校验文件；显式指定 --input 时无需读取配置或停止服务
gbaselite replay-binlog --input D:\backup\binlog.jsonl --check-only `
  --after-sequence 120 --until 2026-07-30T12:00:00+08:00

gbaselite replay-binlog --config config.yaml --input D:\backup\binlog.jsonl --after-sequence 120

# 只恢复到指定 UTC/带时区时间
gbaselite replay-binlog --config config.yaml --input D:\backup\binlog.jsonl `
  --after-sequence 120 --until 2026-07-30T12:00:00+08:00
```

`--check-only` 会完整解析文件，检查格式版本和序号是否严格递增，并报告筛选范围内
的事务数与最后序号；它不执行 SQL、不打开数据目录，也不要求停止正在运行的服务。显
式提供 `--input` 时无需读取 `--config`；省略 `--input` 时仍从配置定位 binlog。
`--after-sequence` 是基础备份已包含的最后序号；省略时从第一条记录开始。重放按事
务执行，任一记录失败会回滚该记录并停止，之前成功重放的记录保留。命令拒绝在 PID
文件指向运行中服务时执行，也不会把重放操作再次写入 binlog。不要直接对当前生产数
据从序号 0 重放，否则可能遇到对象已存在或重复键。有限保留期会永久移除过期 binlog
记录；基础备份早于当前保留窗口时，必须先取得更完整的 binlog 归档或重新制作基础备
份。

### 完整实例备份

用户、密码哈希和授权保存在数据目录中，不属于逻辑备份。完整实例备份必须先停止服
务，再复制整个 `data` 目录。恢复演练应至少核对表、数据、主键与普通/唯一索引、视
图、账号和授权；逻辑 SQL 备份默认不输出 `CREATE USER`、密码哈希或配置中的密码。

复制完成后可以在不启动副本的情况下做只读结构检查：

```powershell
gbaselite inspect-instance --directory D:\backup\data
```

命令解码 `databases\store.gob` 和 `users\users.gob`，报告文件大小、UTC 修改时
间、SHA-256，以及数据库、表、索引、视图、行、账号、授权和权限项的聚合数量；不输
出任何对象名、账号名、主机、SQL、行值、密码或密码哈希，也不修改副本。它发现
`store.gob.tmp` 或 `users.gob.tmp` 恢复候选时会拒绝把目录判定为干净副本，并要求
单独检查候选。该检查只能证明文件可解码和结构自洽，不能证明备份时服务已经停止，也
不能替代在另一个隔离实例中的实际恢复演练。

### 外部 mysqldump

MySQL 8.2 `mysqldump` 可以连接 GBaseLite 并导出数据库、基础表、数据和只读视图：

```powershell
mysqldump --protocol=tcp --host=127.0.0.1 --port=3307 `
  --user=root --password --no-tablespaces --skip-triggers `
  --single-transaction --set-gtid-purged=OFF test > mysql-dump.sql
```

普通 `SHOW TABLES` 与 MySQL 一样同时列出基础表和视图；需要对象类型时使用
`SHOW FULL TABLES`。`mysqldump` 不会导出 GBaseLite 账号和授权，完整实例迁移仍应
使用停服后的数据目录副本；恢复前应在独立实例验证表、数据、索引和视图。触发器、存
储过程和事件当前不支持，即使传入相应 `mysqldump` 选项也不会生成这些对象。

服务端接受 `mysqldump` 读取表数据时使用的版本注释选择修饰符，例如
`SELECT /*!40001 SQL_NO_CACHE */ * FROM table`；这只提供导出工作流所需的语法兼
容，不表示支持完整 MySQL 优化器提示语义。

导入前先在独立实例验证文件；MySQL 8 生成的 `/*!版本 SQL */` 可执行注释会按其中的
SQL 执行，因此视图定义不会在导入时被当成普通注释跳过：

```powershell
mysql --protocol=tcp --host=127.0.0.1 --port=3307 --user=root --password < mysql-dump.sql
```

## CLI

| 命令 | 用途 |
|---|---|
| `gbaselite server` | 前台运行服务 |
| `gbaselite service` | Windows SCM 专用服务入口，不应在终端直接运行 |
| `gbaselite start` | 后台启动；已运行时自动重启 |
| `gbaselite stop` | 停止后台服务 |
| `gbaselite restart` | 重启服务 |
| `gbaselite shell` | 打开内置交互 Shell |
| `gbaselite client` / `gbaselite connect` | 使用 MySQL 风格参数连接正在运行的 GBaseLite 服务 |
| `gbaselite import mysql` | 从外部 MySQL 导入数据库，执行前应停服 |
| `gbaselite export mysql` | 导出 MySQL 可执行 SQL |
| `gbaselite backup` | 逻辑备份单库或全部业务库 |
| `gbaselite restore` | 离线事务恢复 |
| `gbaselite replay-binlog` | 只读校验或将逻辑 binlog 按事务离线重放到基础备份 |
| `gbaselite healthcheck` | 检查 TCP 监听状态 |
| `gbaselite diagnose` | 只读检查配置、监听、数据/日志路径、持久化文件、TLS 和日志开关 |
| `gbaselite inspect-snapshot` | 只读解码数据库快照并输出含索引的结构计数、时间和 SHA-256，可比较恢复候选 |
| `gbaselite inspect-instance` | 只读检查停服数据目录副本中的数据库快照、用户目录和授权聚合信息 |
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
常需要在管理员终端中执行。报告包含程序版本、配置地址、实际探测地址、快照和用户目
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
├── databases/store.gob
├── tables/
├── users/users.gob
├── indexes/
├── binlog.jsonl             # 可选；启用逻辑 binlog 时
└── gbaselite.pid

logs/
├── gbaselite.log
├── gbaselite-*.log          # 已轮转主日志；按 log.retention_days 清理
└── audit.jsonl              # 可选；启用审计时
```

`databases/store.gob` 当前使用持久化格式版本 `3`，`users/users.gob` 使用版本 `1`。
版本标记会随文件保存，启动时会先通过各自的迁移注册表升级内存表示；没有版本标记的
历史文件按版本 `0` 读取。数据库快照从版本 `1` 升级到版本 `2` 时，会按已有行的最大
自增值初始化每张表的下一 ID；从版本 `2` 升级到版本 `3` 时，会把旧 CHECK 表达式迁
移为带稳定名称的约束元数据，并为旧的未命名外键分配稳定名称，不改变业务行。用户目
录迁移只增加版本标记。未知的更高版本会 fail-closed 并拒绝启动，不会把数据当成空库或静默
丢弃字段。`inspect-snapshot` 和 `inspect-instance` 只读检查并显示当前版本和文件来
源版本，不会改写文件。旧文件在下一次正常成功保存时才会写成当前版本，正式升级前仍
应先复制并验证完整数据目录；格式升级属于数据迁移，不要在未授权的业务目录上直接试
验。

数据库写操作和事务提交后会原子保存。不要在服务运行时编辑、替换或删除这些文件。
`store.gob`、`users.gob` 和逻辑备份都先写入同目录临时文件，完成编码与同步后再使
用操作系统原子替换；替换失败不会先删除上一份可用文件。若异常退出发生在首次快照替
换前，可能遗留 `store.gob.tmp` 或 `users.gob.tmp`。主文件缺失但存在该恢复候选
时，服务会明确拒绝创建空库或重新初始化管理员。此时应保持停服，先复制整个数据目
录，再在副本中验证候选文件或恢复已知可用的完整数据目录/逻辑备份；不要直接删除、
改名或覆盖现场文件。损坏的 gob 主文件同样会在启动错误和 `gbaselite.log` 中给出实
际路径与恢复建议，不会静默回退为空实例。

在保持服务停止并复制整个数据目录后，可以只对副本执行快照检查：

```powershell
gbaselite inspect-snapshot --file D:\recovery-copy\databases\store.gob `
  --compare D:\recovery-copy\databases\store.gob.tmp
```

命令会完整解码并校验每个指定文件，输出绝对路径、字节数、UTC 修改时间、SHA-256 以
及数据库、表、索引、视图和行的总数；不会输出对象名、视图 SQL 或行值，也不会写入
文件。摘要相同只能证明两个文件内容相同，修改时间和对象数量也不能单独证明哪个快照
应被恢复。应结合已验证备份、binlog 序号和业务核对决定恢复来源，工具不会自动删
除、改名、覆盖或提升 `.tmp`。

如果运行中的数据库快照落盘失败（例如磁盘已满、权限被修改或文件系统故障），引擎会
立即进入 fail-closed 状态：当前组提交中尚未完成的写入和后续所有 SQL 均返回 MySQL
错误 1030，关闭服务时也不会用不确定的内存状态重试覆盖最后一份成功快照。管理员应
先修复存储问题并复制保留现场，再重启 GBaseLite；重启只加载最后成功落盘的快照。所
有已返回错误的写入都应视为未提交，核对数据后再由业务重试。

## 项目结构

```text
catalog/       用户、授权和权限元数据
cmd/           gbaselite CLI 入口与进程管理
config/        配置加载与环境变量覆盖
docs/          SQL 使用教程等面向使用者的文档
docker/        Dockerfile、Compose 与环境示例
executor/      SQL 执行器、查询、备份
index/         索引数据结构接口
journal/       审计日志与可重放逻辑 binlog
mysql/         MySQL 导入、导出和恢复
parser/        SQL lexer、AST 和 parser
protocol/      MySQL 协议包与结果编码
server/        MySQL TCP 服务和客户端兼容层
storage/       表、数据库、行和值及持久化
transaction/   事务相关组件
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

可重复性能基准使用项目临时目录，不连接已安装服务或业务数据。以下命令覆盖点查、范
围查询、每批 100 行持久化写入、持久化更新、等值 JOIN 和多连接 MySQL 协议查询：

```powershell
$env:GOCACHE="$PWD\.tmp\gocache"
D:\env\Go\bin\go.exe test ./executor ./server -run '^$' `
  -bench 'Benchmark(PrimaryKeyLookupTenThousandRows|RangeQueryTenThousandRows|BulkInsertHundredRows|PersistentUpdate|JoinOneThousandRows|MySQLConcurrentPrimaryKeySelect)$' `
  -benchtime=1s -benchmem -count=3
```

比较结果时固定 Go 版本、CPU、电源模式、`-count` 和 `-benchtime`，并保留完整命令
与原始输出；这些微基准用于同一机器上的版本回归，不代表生产容量或完整 MySQL 性
能。

`server/mysql_server_test.go` 使用标准 MySQL 驱动验证真实 TCP 握手、认证、权限、
CRUD 和元数据兼容。涉及客户端兼容的改动还应使用真实 MySQL 客户端和对应 GUI 客户
端验证。

`executor/recovery_test.go` 使用独立子进程验证已经返回成功的写入在未执行 `Close`
就退出后仍可恢复，并使用固定随机种子执行 250 次增删改、每 25 次关闭重开后与内存
模型逐行核对。测试只使用 `t.TempDir()`，不会连接本机服务或正式数据目录；它覆盖已
确认提交后的恢复，不模拟磁盘控制器谎报落盘成功或操作系统无法提供的断电保证。

GitHub Actions 会在 Ubuntu 和 Windows 上分别执行 `gofmt` 检查、完整 Go 测试与
`go vet`。独立的 Linux CI 任务还会使用官方 `mysql:8.4` 容器中的真实 `mysql` 和
`mysqldump`，连接仅使用 CI `.tmp` 数据目录的临时 GBaseLite 实例，覆盖带连字符数
据库、表/视图/索引、`SHOW DATABASES`、自包含导出、删除后导入和恢复结果核对。该任
务不访问开发机或部署机器的数据，也不替代 Navicat GUI 的版本专项验证。

## 本地发布打包

Windows PowerShell 会先检查 `gofmt`，执行完整测试和静态检查，然后以
`CGO_ENABLED=0` 交叉编译 Windows amd64、Linux amd64 和 Linux arm64。检测到 WiX、
.NET SDK 和 Syft 时，还会生成 MSI 与 SPDX JSON SBOM。脚本不包含 GitHub、Git
push、GHCR push 或其他上传操作。

Windows 可直接双击项目根目录的 `release.bat` 一键发布。脚本从源码当前版本自动计
算下一版本：修订号从 `001` 到 `999` 使用三位补零，例如 `1.0.123` 的下一版本是
`1.0.124`；达到 `1.0.999` 后进位到 `1.1.0`，再继续为 `1.1.001`。它会同步源码、
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
工具路径。双击执行结束后窗口会保留；自动化调用可使用：

```powershell
.\release.bat --no-pause
.\release.bat --preview --no-pause
.\release.bat --self-test --no-pause
```

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\build-release.ps1 `
  -Version 1.0.002 -GoExecutable D:\env\Go\bin\go.exe
```

兼容入口 `scripts/package.ps1` 会转发到同一脚本。Linux 构建机可以使用：

```bash
VERSION=1.0.002 ./scripts/build-release.sh
```

MSI 单独构建：

```powershell
dotnet tool install --global wix --version 5.0.2
wix extension add --global WixToolset.UI.wixext/5.0.2
.\scripts\build-msi.ps1 -Version 1.0.002 `
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
ELF、执行权限、禁入内容和 SHA-256 校验。产物仍复制到
`dist/GBaseLite-<VERSION>`，开发分支中的版本文件保持不变。

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
.\publish-release.bat -Version 1.0.002 -DryRun -ReplaceArtifacts
.\publish-release.bat -Version 1.0.002 -PrepareOnly -ReplaceArtifacts
.\publish-release.bat -Version 1.0.002 -Publish -ReplaceArtifacts
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
- 增加审计/binlog 轮转、指标和更完整的可观测性
- 继续维护持久化格式迁移和升级恢复演练
- 增加 MSI 安装器自动化测试和签名流程
