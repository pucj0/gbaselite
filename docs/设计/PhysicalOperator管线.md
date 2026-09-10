# Physical Operator 管线

## 边界

生产启动仍装配 MVCC/bbolt adapter。storageengine.Txn 是唯一事务契约，SQL 不做运行模式分派。
physical 包只依赖中立 Iterator 契约，不包含 SQL AST、行编码、目录布局或存储实现。
executor/physical_binding.go 负责访问路径与行解码，physical_select.go 和 physical_join.go 负责 SQL 绑定。

物理接口采用同步推送：Operator[T].Run(context.Context, Yield[T]) error。
不同节点可有不同类型：Scan/Filter/Join 传递 storage.Row，Projection 输出 []any，Modify 可以携带行键。
Run 在调用栈内传递背压；yield 返回错误立即向上传播，无后台生产 goroutine。
每次 Run 的打开和释放在同一调用中完成。下游只能在 yield 内借用行，保留行的组件必须复制。

## 算子组合

| 算子 | 行为与绑定职责 |
|---|---|
| Scan | 打开 Iterator，逐条解码，所有退出路径 Close；点查、索引回表和表扫描统一为此入口 |
| Filter | 对上游行求谓词，NULL/比较规则由 SQL 表达式绑定提供 |
| Projection | 从输入行计算输出列，可以改变行类型 |
| Join | 唯一嵌套循环实现；每个外侧行由工厂打开内侧索引探测或扫描；LEFT 未匹配时补 NULL |
| Aggregate | 创建局部状态，Add 消费行，Finish 产生结果，Close 释放；全局和分组共享生命周期 |
| Sort | 注入有预算的 externalRowSorter，维护稳定顺序、溢写与临时文件清理 |
| TopN | 共用 Sort，再执行 Offset/Limit；当前不使用堆优化，可能扫描全部输入 |
| Distinct | 通过 key+ordinal 选首个代表，再按 ordinal 恢复源顺序；两次排序均有预算且可溢写，SELECT/UNION 复用 |
| Union | 顺序连接输入流；SQL DISTINCT 组合现有有预算排序去重，不猜测类型合并 |
| Materialize | 先按预算收费，再复制行；每次运行独立物化，非跨事务缓存 |
| Window | 接收预算限制下的物化行，复用 SQL 分区、排序与默认 frame 求值 |
| Modify | 对每个逻辑输入调用绑定的写入函数；不提交事务 |

排序时投影保留不可见 ORDER BY 键，Sort/TopN 之后再丢弃。WHERE 在投影之前，HAVING 在分组之后。
UNION 各分支共享同一个 Txn，外层 ORDER/LIMIT 最后执行。窗口在过滤后的行集上求值，之后才应用 DISTINCT/LIMIT。

## 事务与资源

UPDATE/DELETE 的行键与原始值随扫描流进入 Filter/Limit/Modify；INSERT 的 VALUES 逐行进入 Modify。
写入绑定仍维护唯一索引、外键、CHECK 和自增预留。语句 child Txn 成功才合并到父事务，失败回滚 child。
Iterator 关闭、排序器关闭和消费错误均会传播；Limit 只移除自己产生的结束信号，不掩盖关闭失败。
查询取消和查询预算继续由上下文及 queryControl 检查。Materialize/Window 超内存预算报错，尚不支持窗口溢写。

## 支持与验证

支持现有 INNER/LEFT JOIN、全局和分组聚合、排序、分页，并接入 UNION/ALL 与 ROW_NUMBER/RANK/DENSE_RANK、COUNT/SUM/AVG/MIN/MAX 默认窗口。
不扩大到完整 MySQL：CTE、派生表、标量子查询、显式窗口 frame、窗口与 GROUP BY/HAVING 混用仍不支持；UNION 未实现完整类型合并。
现有持久化格式、旧数据迁移路径和默认打开方式不变。

physical/operator_test.go 覆盖提前结束、借用缓冲复制、取消、关闭失败、复合关系算子和预算失败。
executor/physical_pipeline_test.go 在生产 adapter 与独立内存后端上执行同一组 SQL，包括 JOIN、聚合、窗口、Union 和写入失败回滚。
导入边界回归包含 physical 包。旧整数核仅在 integer_batch_kernel_fixture_test.go 中保留作历史基准，SQL 不调用它。

## 加固后的输入边界

physical_select 与 physical_join 直接传递 Operator，Aggregate/Window/表达式排序使用 WithInput 绑定入口。
WithSource 仅为历史辅助调用的兼容入口；核心路径不经过 Operator→callback→Operator 转换。
Result 转算子用于协议或已物化结果边界。Distinct 键生成保留 SQL 排序规则和 NULL 语义，公共算子不解释 SQL 值。
Distinct.NewSort 必须返回拥有所保留数据的排序器：按 key/ordinal 去重，再按 ordinal 输出。
创建第二个排序器失败、输入取消、消费错误均释放已打开的排序器。
跨后端回归增加 DISTINCT 与 UNION DISTINCT、溢写、磁盘预算拒绝、执行中取消和迭代器/文件清理验证。
