# 精确 DECIMAL 兼容范围

GBaseLite 的 `DECIMAL`、`NUMERIC`、`DEC`、`FIXED` 现在使用精确十进制值，
不再把新写入的数据映射为 `DOUBLE`。这是一组明确限定的 MySQL 兼容能力，
不代表完整 MySQL 数值语义。

## 已实现

- `DECIMAL(P,S)`：`1 <= P <= 65`，`0 <= S <= 30`，且 `S <= P`；省略时默认为
  `DECIMAL(10,0)`。声明不合法或整数部分超出范围会报错。
- 插入、更新、默认值按列的标度舍入，恰好一半时向远离零的方向舍入。
  例如 `DECIMAL(5,2)` 中 `1.235` 写为 `1.24`，`-1.235` 写为 `-1.24`。
  列值保留尾部零；`UNSIGNED` 不接受舍入后仍为负的值。
- 非指数的小数字面量和超出 `int64` 的整数字面量使用精确值。
  至少一个操作数是 DECIMAL，且没有混入 FLOAT/DOUBLE 时，`+ - * / %` 使用十进制计算。
  与浮点操作数混用时采用近似计算。
- DECIMAL 的比较、排序、分组、唯一键和索引范围访问按数值处理。
  `1.0` 与 `1.00` 是相同的唯一键。WHERE 的常量不会先按列标度舍入，
  因此 `amount = 0.104` 不会误匹配已存储的 `0.10`。
- DECIMAL 的 `SUM`、`AVG`、`MIN`、`MAX`，以及对应窗口聚合使用精确值。
  `SUM` 的中间结果超出 65 位会报错。普通整数聚合保留整数加法路径。
- `ABS`、`CEIL` / `CEILING`、`FLOOR`、`ROUND`、`TRUNCATE` 和 `MOD` 的
  DECIMAL 输入支持精确计算；`ROUND` / `TRUNCATE` 位数限定在 `-65..30`。
- 除法固定增加 4 位标度，最多 30 位，最后一位舍入；DECIMAL 除零返回 SQL NULL。
  例如 `5.05 / 0.014` 为 `360.714286`。`div_precision_increment` 暂不可配置。
- 派生表、标量结果、`CREATE TABLE AS SELECT`、JSON 构造器及持久化保留精确值。
  JSON 构造器把 DECIMAL 当作 JSON 数字，不转成带引号的字符串。
- MySQL 文本和二进制结果集使用 `NEWDECIMAL` 类型及精确文本编码。来源列提供
  声明的标度；计算表达式可以使用未指定标度的元数据。DECIMAL 类型的预编译参数
  也保留精确数值，不先经过 `float64`。客户端以字符串传入 DECIMAL 列同样支持；
  客户端已先转换成浮点数的精度损失无法恢复。

```sql
CREATE TABLE payments (
  id INT PRIMARY KEY,
  amount DECIMAL(25,2) NOT NULL,
  UNIQUE KEY uq_amount(amount)
);
INSERT INTO payments VALUES (1,9007199254740993.01);
UPDATE payments SET amount=amount+0.01 WHERE id=1;
SELECT amount FROM payments;       -- 9007199254740993.02
SELECT 0.1+0.2 = 0.3;              -- true
SELECT JSON_OBJECT('amount',0.1+0.2); -- {"amount":0.3}
```

参数范围和舍入方向参考 MySQL 官方的
[DECIMAL 特性](https://dev.mysql.com/doc/refman/8.4/en/precision-math-decimal-characteristics.html)、
[舍入规则](https://dev.mysql.com/doc/refman/8.4/en/precision-math-rounding.html)和
[算术运算](https://dev.mysql.com/doc/refman/8.4/en/arithmetic-functions.html)。

## 内存与持久化

每个值复用现有 `storage.Value.Text` 存放规范化的十进制文本，不新增每行字段。
64 位平台的 `storage.Value` 仍为 80 字节。比较直接扫描字符串，不创建大整数；
算术运算才临时使用 `math/big`，结果不保留计算树或常驻大整数缓存。

单个十进制文本最多接受 256 字节，精度和标度在构造大整数前校验，避免巨大
指数、长字符串或舍入位数导致不受控分配。十进制文本比 InnoDB 的压缩二进制
DECIMAL 编码占用更多有效载荷空间，这是当前实现的取舍。

2026-09-08 在 Windows amd64 / i5-12600KF / Go 1.26.5 上的短基准：
`CompareDecimal` 约 31.8 ns/op、0 B/op、0 次分配；两项定点加法约 657 ns/op、
392 B/op、17 次分配。这些是内存内函数微基准，不代表磁盘 SQL 的吞吐或延迟。
可用 `go test ./storage -run '^TestDecimal' -bench 'Benchmark(CompareDecimal|DecimalAdd)' -benchmem`
复测。

旧快照中 `SQLType` 为 DECIMAL、底层类型为 DOUBLE 的列，加载时会在复制后的
快照上转换为十进制，并按原声明标度舍入；默认值也转换。历史浮点运算已丢失的
数字无法补回。如果旧值超出声明范围，加载失败并报告错误，不静默截断。
加载转换本身不改写原快照；后续持久化会存储新类型。新格式含 DECIMAL 类型，
旧版本不应直接打开，应保留升级前备份。

## 尚未覆盖的差异

- 不实现 MySQL 的全部隐式转换、`CAST` / `CONVERT` 数值目标、SQL mode 对
  警告/截断/溢出的完整控制、全部 `UNSIGNED` 运算规则或 `ZEROFILL` 输出补零。
- `FLOAT` / `DOUBLE`、普通整数的所有算术、字符串数值转换以及所有数学函数，
  尚未统一为完整 MySQL 精确运算规则。不要把这一实现理解为所有 SQL 运算都无损。
- 当前 SQL 词法器仍限制指数数字面量与以点开头的数字写法；使用 `0.1`，不要依赖 `.1`。
  存入 DECIMAL 列的合法数字字符串可包含有界指数。
- 乘法结果超过 30 位小数会舍入；固定列、表达式和聚合最多保存 65 位精度。
  中间结果溢出会立即报错，后续加减不能抵消这个错误。
- `CREATE TABLE AS SELECT` 的计算 DECIMAL 列根据结果推断精度/标度；若不同结果
  的整数位和小数位合并需要超过 65 位，则拒绝建表。
- 不提供 MySQL InnoDB 的 DECIMAL 二进制文件格式兼容或物理文件互通。

CASE、IF、COALESCE/IFNULL、GREATEST/LEAST 的结果元数据会合并数字分支：
浮点优先于DECIMAL，DECIMAL优先于整数，避免Prepared Statement把实际十进制值
误编码为整数0。NULLIF保持第一参数的类型；混合文本和数字仍属于简化转换范围。
结果类型合并参考 [MySQL控制流函数](https://dev.mysql.com/doc/refman/8.4/en/flow-control-functions.html)。
