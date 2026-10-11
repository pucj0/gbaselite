# Feature Specification: B02 InnoDB-style Row Lock Manager

**Feature Branch**: 未创建 Git 分支；Spec Kit feature 为 `006-b02-row-lock-manager`，与分支独立。

**Created**: 2026-10-10

**Status**: 部分实现。Phase 1（Setup，T001–T004）与 Phase 2（Foundational，T005–T012）已执行：
`go test ./...` 与 `go vet ./...` 退出 0，354 个既有生产 Go 文件逐个 SHA-256 零改动。
**Phase 3 起（Record/Gap/Next-Key/公平队列/SQL 锁定读/DML 加锁/诊断/发布 gate）仍未实现**，
不得据此宣称功能存在。证据：`.tmp/b02-{baseline,format-baseline,resource-calibration,validation}.md`。

**Input**: 在 GBaseLite 上规划 Record S/X、Table IS/IX、Gap、Next-Key、Insert Intention、
兼容矩阵、等待队列、死锁、超时、Current Read、锁定 SELECT、DML/唯一键、事务生命周期及诊断。
保持 B01/A05 既有语义，不改持久化、WAL、Raft 协议，不声称完整 InnoDB 兼容。

## User Scenarios & Testing *(mandatory)*

### User Story 1 - 行级并发与公平等待 (Priority: P1)
数据库使用者希望共享读取可同时进行，互斥修改按资源等待，互不相干的行仍能推进。

**Why this priority**: 所有 SQL 加锁的基础；没有它就无法验证行锁与全局事务门的差异。

**Independent Test**: 独立锁资源夹具验证模式矩阵、等待和升级，无须 SQL。

**Acceptance Scenarios**:
1. **Given** 两个 root 请求同一 record，**When** 都请求 S，**Then** 同时获得；任一请求 X 则等待。
2. **Given** A 持有行 1 的 X，**When** B 写行 2 或其他表，**Then** B 在 A 结束前可完成。
3. **Given** 老 X waiter 排队，**When** 新 S 请求到达，**Then** 新请求不能延长老请求等待。
4. **Given** IS/IX 意向与记录锁，**When** 请求入队、升级或取消，**Then** 按已公布矩阵与唯一请求状态处理。

### User Story 2 - 区间保护与插入并发 (Priority: P1)
使用者希望范围保护阻止落入受保护区间的插入，允许其他区间以及同空隙不同位置的插入并行。

**Why this priority**: 避免把乐观范围依赖误当成 gap lock，解决未命中、范围与索引访问冲突。

**Independent Test**: 空表、边界行、部分组合键和删除边界夹具验证区间成员及插入意向。

**Acceptance Scenarios**:
1. **Given** 保护空隙 (10,20)，**When** 插入 15，**Then** 等待；插入 25 可推进。
2. **Given** 仅存在插入意向，**When** 分别插入 12、18，**Then** 两者兼容；同一主键仍互斥。
3. **Given** 行 20 的 Next-Key，**When** 修改 20 或插入前置空隙，**Then** 分别受 record/gap 部分阻挡。
4. **Given** 两个重叠 Gap，**When** 第二个 Gap 请求，**Then** 兼容；删除边界行不会撤销区间保护。

### User Story 3 - 明确锁定读与快照读 (Priority: P1)
使用者希望 FOR UPDATE/FOR SHARE 实际获得锁并读取锁后的已提交值，同时普通 SELECT 保持可重复读。

**Why this priority**: 当前锁定语法只按快照读取；不能只增加等待而继续返回等待前旧值。

**Independent Test**: 两个会话中先 BEGIN、再由另一会话提交，再交替普通读与锁定读。

**Acceptance Scenarios**:
1. **Given** T1 的固定 readTS=R、T2 已发布 R+1，**When** T1 普通读/锁定读/普通读，
   **Then** 依次看到旧值/锁后新值/旧值；readTS、历史保留 horizon 不变。
2. **Given** T1 等待 T2，**When** T2 提交删除，**Then** T1 获锁后得到不存在及最新删除事实。
3. **Given** 仅进行锁定读的 root，**When** 提交，**Then** 释放锁但不推进 Head、不产生 commitTS。
4. **Given** 不支持的锁定查询形状或选项，**When** 执行，**Then** 在获得锁或写入之前明确拒绝。

### User Story 4 - DML 与索引约束一致 (Priority: P1)
使用者希望 INSERT/UPDATE/DELETE、主键/唯一索引及既有变体遵循统一加锁路径，避免访问路径绕过。

**Why this priority**: 仅锁定 SELECT 无法阻挡普通写者；约束和索引必须与物理目标一致。

**Independent Test**: 相同/不同主键与唯一值、多表目标、主键迁移、IGNORE/REPLACE/upsert。

**Acceptance Scenarios**:
1. **Given** 两个 root 插入相同 PK 或 UNIQUE，**When** 都提交，**Then** 至多一个新行成功；另一个等待后报 duplicate 或旧快照冲突，绝不覆盖。
2. **Given** UPDATE/DELETE 的父快照候选，**When** 获锁后的目标变化晚于 readTS，
   **Then** 返回需重试整个 root 的冲突，不用新值重写旧快照事务。
3. **Given** 自引用 INSERT SELECT 或移动主键 UPDATE，**When** child 写入，
   **Then** 新写不会重新进入 parent 候选流，dedup、first-match、LIMIT 和语句原子性保持。
4. **Given** UNIQUE 的 NULL 或排序规则等价值，**When** 并发插入，**Then** 与现有唯一性定义一致。

### User Story 5 - 事务结束、超时与死锁 (Priority: P1)
使用者希望等待有期限、死锁有确定受害者，回滚/断连/恢复/领导权变化不留下资源或错误提交结果。

**Why this priority**: 防止无限挂起、提前释放与 publication/cancel 竞争造成错误结果。

**Independent Test**: 构造二/三 root 环、双升级、虚拟时钟超时及完整生命周期夹具。

**Acceptance Scenarios**:
1. **Given** 两个 root 相互等待，**When** 完成等待边登记，**Then** 确定选择后注册 root，终止其全部事务并让幸存者推进。
2. **Given** 等待超时，**When** 期限到达，**Then** 取消该 statement 的等待并回滚 statement 工作；显式 root 保持可用，已授予锁保留。
3. **Given** child merge/rollback、ROLLBACK TO 或 RELEASE，**When** 操作成功，**Then** root 的已授予锁保留，丢弃层的 pending 请求撤销。
4. **Given** durable publication 已完成后取消，**When** 清理，**Then** 保持 COMMITTED 并释放锁；不得伪装 ABORTED。
5. **Given** 断连、恢复或 term 变化，**When** 失效旧事务，**Then** waiter 被唤醒，旧 owner 不得读写新世代。

### User Story 6 - 可诊断与可重复验收 (Priority: P2)
维护人员希望定位等待资源、阻塞者与死锁原因，同时诊断不泄露行内容、不阻塞事务推进。

**Why this priority**: 让并发行为可观测，避免靠 sleep 和日志偶然时序验收。

**Independent Test**: 从诊断快照核对 owner、grants、queue、edges 与终态清理，重复运行固定调度。

**Acceptance Scenarios**:
1. **Given** 活动 waiter，**When** 读取诊断，**Then** 获得模式、匿名资源标识、root/child、阻塞者与期限，不含 SQL、key 原文或 row。
2. **Given** 资源预算边界，**When** 恰好达到/超过预算，**Then** 分别允许/明确拒绝，失败不遗留部分请求。
3. **Given** 同一并发调度重复 100 次，**When** 验收，**Then** victim、错误类别、结果一致，终态 grants/queue/edges 为零。

### Edge Cases
- 未命中唯一点、空表、删除/再插入、负无穷/正无穷、空非 nil 字节界、开闭端点、反向扫描。
- S→X 双升级；年轻兼容读者；多阻塞者；公平队列造成的等待边；授予与取消/超时同时发生。
- 同 root 的祖先/child 重入；32 个 savepoint layer、同名替换、匿名 layer；child merge 失败。
- 二级索引覆盖读与回表、主键变更、唯一键等值排序规则、多个 NULL、FK 递归动作。
- 大事务 stage/spill、窗口与 JOIN 锁定查询拒绝、旧主键编码不能推导 SQL 有序范围。
- restore、Close、term/generation 变化、未发布版本、发布后取消、资源清理失败。

## Requirements *(mandatory)*

### Functional Requirements
- **FR-001**: Record S/S 必须兼容，涉及 X 必须互斥；同 root 重入不自阻塞，S→X 保留已持 S 直到升级或 root 结束。
- **FR-002**: 每表 Table IS/IX 必须反映子资源 S/X 意向；IS/IS、IS/IX、IX/IX 兼容；不支持 Table S/X 或锁升级成整表互斥。
- **FR-003**: Gap 必须按稳定逻辑区间阻止区间内插入，Gap 与 Gap 兼容，不阻止既有记录值更新。
- **FR-004**: Next-Key 必须原子组合记录 S/X 与前置 Gap；边界开闭和末端无穷保护必须明确定义。
- **FR-005**: Insert Intention 必须与覆盖插入位置的他 root Gap 冲突，不同插入位置可并行；同键竞争由 record X/唯一键 reservation 决定。
- **FR-006**: 必须公布记录、意向、Gap/Insert Intention 与组合锁兼容矩阵；包含同 owner 例外、区间交集与边界用例。
- **FR-007**: 等待队列必须按冲突资源防止新请求越过旧不兼容 waiter；取消、授予、超时只产生一个结果；无关资源不受阻塞。
- **FR-008**: 死锁图必须包含 holder 边和队列公平性边，支持升级、二/三节点环；选择环内注册序号最大的 root 作为受害者。
- **FR-009**: 死锁必须终止整个 victim root；等待超时/查询取消只终止 statement，显式 root 不自动结束；隐式 autocommit root 随 statement 结束。
- **FR-010**: 必须支持会话锁等待期限，默认 50 秒、合法整数 1..86400 秒；实际等待受更短查询/context 期限限制，事务顺序不依赖墙钟。
- **FR-011**: Current Read 必须先锁后读最新已发布数据与指定 read layer 的既有写；不得读取其他 root 未提交写、修改 root readTS 或绕过 publication marker。
- **FR-012**: FOR UPDATE 使用 X/IX，FOR SHARE 与 LOCK IN SHARE MODE 使用 S/IS；锁持有至 root 结束，autocommit 持有至语句读结果完成。
- **FR-013**: 普通读保持 B01 固定快照；锁定读不自动成为提交依赖。仅持锁 root 仍是 B01 empty/read-only 类，Head/commitTS 不变。
- **FR-014**: INSERT 必须保护目标 row、非 NULL 唯一键与插入位置；UPDATE 必须保护旧/新 row、旧/新索引项；DELETE 必须保护 row 与被移除索引项。所有既有 DML 变体与 FK 派生写不得绕过。
- **FR-015**: 完整存在的 PK/UNIQUE 等值查询只需 record；未命中保护空隙；非唯一/部分组合键使用区间；不同索引访问相同行必须汇合到共同 row 资源。
- **FR-016**: UNIQUE reservation 必须复用现有 NULL、排序规则和编码语义；锁后重新检查唯一性，不得凭等待前缓存判重。
- **FR-017**: DML 候选及 JOIN/INSERT SELECT 源保持 parent 固定快照；child 仅写入。锁后重新验证目标，不引入快照外新候选，不改变去重、首次命中、LIMIT、SET 顺序及 Halloween 保护。
- **FR-018**: 所有写/显式依赖继续按 B01 的 latest committed change > readTS 规则冲突；锁后发现过期目标可早期冲突并要求 root 重试，最终提交仍全量验证，无 locked-write 豁免。
- **FR-019**: bounded/group/stream/local-WAL/Raft 的验证及 publication 结果必须一致；锁、Current Read 元数据不进入持久化、备份、WAL、Raft 命令。
- **FR-020**: root 是全部 grants 的唯一 owner；child merge/rollback、statement failure、savepoint rollback/release/同名替换均保留已授予锁，只撤销丢弃 scope 的 pending 请求。
- **FR-021**: root 最终 commit/rollback/死锁中止/断连后必须释放全部 grants、请求和图边；释放必须在最终结果已决定、写集不可继续使用之后，且幂等。
- **FR-022**: restore/generation、Close 与 leader term 失效必须唤醒 waiter 并拒绝旧 owner 新操作；锁只协调当前 leader，恢复/apply 不等待用户锁。
- **FR-023**: 必须提供有界 diagnostics：owner、scope、模式、匿名资源、holder/waiter、阻塞边、等待时长/期限及死锁摘要；权限限定管理员，截断明确可见，不泄露原始 key/value/SQL。
- **FR-024**: 并发验收必须用屏障、通道、hook 和虚拟时钟；禁止 sleep 构造因果顺序；覆盖全部模式、队列、环、期限与释放竞争。
- **FR-025**: 锁请求、持锁、图遍历和诊断必须有明确内存/数量预算；超限 fail closed，不通过提前释放锁或全局事务互斥降低资源消耗。
- **FR-026**: SQL 通过可选后端能力获得锁与 Current Read；能力缺失时锁定 SELECT 明确不支持；所有默认后端写入口必须参与同资源协调，最终验证仍是旁路/复制变化的防线。
- **FR-027**: 不可证明 SQL 范围与现有字节排序一致时，必须使用对应索引全域的保守区间保护并标注锁范围，不能伪造精确范围或改变持久化编码。
- **FR-028**: 锁定查询初版只支持单个基表的点/范围/全扫描、WHERE、投影、ORDER/LIMIT；JOIN、派生表、CTE、视图、聚合、DISTINCT、窗口、子查询、UNION 锁定形状及 NOWAIT/SKIP LOCKED 必须在副作用前拒绝。
- **FR-029**: 阻塞等待不得持有存储视图、游标、提交/恢复门或事务管理器内部互斥锁；单 Txn 仍是单调用者，检测线程不得并发操作事务写集。
- **FR-030**: 必须保持 B01 的 snapshot read API、root retention、child merge、三类 root 与 publication/cancel 不变量，并保持既有 A05 回归；明确 Current Read 是新增读模式而不是重定义快照可见性。
- **FR-031**: 未授权实现前只生成/审核规划文档；实施时同变更更新 README 的语法、限制、错误、预算与部署说明，不宣称完整 InnoDB 兼容。
- **FR-032**: 锁后 row 解码和写入必须验证 catalog/索引定义仍符合 parent 元数据；版本过期报冲突；既有 catalog/FK Guard/GuardRange 保留原始验证基准。

### Key Entities *(include if feature involves data)*
- **Lock Owner**: root 身份、注册序号、generation/term、结束状态及预算；scope 记录发起 child/statement。
- **Lock Resource**: 表、索引或通用命名空间内的 record/稳定区间，与现有键身份一致。
- **Lock Request / Grant**: 模式、scope、排队次序、期限、状态；grant 属于 root。
- **Wait Edge / Deadlock Summary**: holder/公平阻塞边、graph revision、确定 victim、匿名环摘要。
- **Current Read Observation**: 值、存在/墓碑、已提交变更版本、自有写标志；不改变快照或提交基准。

## Success Criteria *(mandatory)*

### Measurable Outcomes
- **SC-001**: 公布兼容矩阵全部有向组合、端点和同 owner 情况均可验证；无关行在竞争者持锁期间完成。
- **SC-002**: 固定调度运行 100 次，二/三 root 环和双升级的 victim、结果均相同，无永久等待。
- **SC-003**: 超时/取消/授予竞争只有一次结果；每个 root 结束后 grants、waiters、edges 计数为零。
- **SC-004**: 普通/锁定/普通读序列、三类 root 结束、五类提交路径与 B01/A05 全部相关场景一致。
- **SC-005**: 每条 FR 和 SC 都有设计、实施及测试映射，覆盖率 100%，审核无 critical/high 不一致。
- **SC-006**: 预算精确边界允许、超出一个单位拒绝；诊断截断可见且原始行/键/SQL 泄露数为零。
- **SC-007**: 旧格式数据库、备份、WAL 与复制重放无需迁移且行为一致；新增锁状态持久化字段数量为零。
- **SC-008**: 本轮生产代码、data/、部署二进制改动数量为零，全部实现任务保持未完成。

## Assumptions
- B02 是 InnoDB-style 的有限能力，不升级为 Serializable，也不支持隔离级别切换、Table S/X、锁升级、谓词锁或分布式锁服务。
- 保留 B01 INV-005 意味着旧事务等待后不能更新 readTS 之后的提交版本并成功提交；必须 root 重试。此限制优先于模拟完整 InnoDB 行为。
- B01 INV-003 的原快照 API 保持；新增 Current Read 独立契约允许读到更晚已提交版本，不更改原 API 的可见性判定。
- 初版保守保留 child/savepoint 已授予锁至 root 结束，不实现 InnoDB 插入记录的特殊 savepoint 解锁。
- timeout 默认采用常见 50 秒；资源预算、具体接口和索引区间算法由计划明确。测试使用隔离临时库。
- B01/A05 文档中历史路径/完成状态有偏差；本 feature 使用实际 `specs/` 与源码基线 `140838c2d4f10971c010a9e4d643fd4b135634f2`。

## Clarifications

### Session 2026-10-10
本轮未伪造用户问答。以下是明确约束推导及已写入需求的默认边界；clarify 复核见 checklists/clarification.md。
- Record 与意向模式由 FR-001/002/006 固定，Gap 无 S/X 强度之分；Next-Key 是组合而非另一条相互冲突规则。
- Current Read 与固定 readTS 分离；最终验证完全保留 B01（FR-011/013/018/030）。
- parent 候选不改变；等待后目标过期报冲突，避免 silently rebase（FR-017/018）。
- root 保守持锁；timeout 终止 statement、deadlock 终止 root；检测器只发中止信号（FR-008/009/020/029）。
- unsupported locking shapes/options 早期拒绝；不继承现有 NOWAIT/SKIP LOCKED 的静默忽略（FR-028）。
