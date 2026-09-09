# JSON 函数兼容说明

GBaseLite 支持以下常用 MySQL JSON 函数子集。JSON 列继续保存文本，读取旧数据不需要格式迁移；这不是 MySQL 原生二进制 JSON 或完整 JSON 兼容实现。

## 函数范围

| 函数 | 当前行为 |
|---|---|
| `JSON_OBJECT(key,value,...)` | 创建对象，可无参数；重复键取最后值，NULL 键报错 |
| `JSON_ARRAY(value,...)` | 创建数组，可无参数；SQL NULL 值转为 JSON null |
| `JSON_QUOTE(text)` / `JSON_UNQUOTE(value)` | JSON 字符串编码、解码；SQL NULL 透传 |
| `JSON_EXTRACT(doc,path,...)` | 提取路径；多个路径或通配符结果包装为数组；没有匹配返回 SQL NULL |
| `JSON_SET(doc,path,value,...)` | 更新已有值，或向已有父对象/数组添加值 |
| `JSON_INSERT(doc,path,value,...)` | 只添加，不替换已有值 |
| `JSON_REPLACE(doc,path,value,...)` | 只替换，不创建缺失值 |
| `JSON_REMOVE(doc,path,...)` | 按参数顺序删除；缺失路径忽略，禁止删除根 |
| `JSON_VALID(doc)` | 有效且在当前深度限制内为 1，无效为 0；SQL NULL 返回 SQL NULL |
| `JSON_TYPE(doc)` | 返回 OBJECT、ARRAY、STRING、INTEGER、UNSIGNED INTEGER、DOUBLE、BOOLEAN、NULL |
| `JSON_LENGTH(doc[,path])` | 顶层成员/元素数量，标量为 1；路径缺失返回 SQL NULL |
| `JSON_DEPTH(doc)` | 最大值层数，标量和空容器为 1 |
| `JSON_KEYS(doc[,path])` | 对象键数组；非对象或路径缺失返回 SQL NULL |
| `JSON_CONTAINS_PATH(doc,'one'/'all',path,...)` | 判断任一/所有路径是否存在；值为 JSON null 仍算存在 |

构造和修改函数区分 JSON 值与普通 SQL 字符串。JSON 列和 JSON 函数的结果可嵌套；普通字符串即使看起来是 JSON，也作为字符串编码。JSON 文档中的整数通过十进制 token 解析，不先转成 float64，因此提取、修改、重新编码不会把 64 位整数舍入。

函数处理 NULL、重复键及路径修改的设计依据为 [MySQL JSON 构造函数](https://dev.mysql.com/doc/refman/8.0/en/json-creation-functions.html)、[查询函数](https://dev.mysql.com/doc/refman/8.0/en/json-search-functions.html)和[修改函数](https://dev.mysql.com/doc/refman/8.0/en/json-modification-functions.html)。当前范围以本页和仓库回归测试为准，尚未运行独立 MySQL 服务的差分测试。

## SQL 示例

```sql
CREATE TABLE json_documents(id INT PRIMARY KEY, body JSON);

INSERT INTO json_documents VALUES (
    1,
    JSON_OBJECT('name', '小明', 'tags', JSON_ARRAY('sql', 'go'), 'extra', NULL)
);

SELECT JSON_OBJECT('document', body) FROM json_documents WHERE id=1;
SELECT JSON_UNQUOTE(JSON_EXTRACT(body, '$.name')) FROM json_documents;
SELECT JSON_EXTRACT(body, '$.tags[*]') FROM json_documents;
SELECT JSON_TYPE(JSON_EXTRACT(body, '$.extra')) FROM json_documents;
-- 上一行返回字符串 NULL；缺失路径则返回 SQL NULL。

UPDATE json_documents
SET body=JSON_SET(body, '$.active', TRUE, '$.tags[10]', 'json')
WHERE id=1;

UPDATE json_documents SET body=JSON_REMOVE(body, '$.extra') WHERE id=1;
SELECT JSON_VALID(body), JSON_LENGTH(body), JSON_KEYS(body) FROM json_documents;
```

`JSON_SET('{}','$.x','[1,2]')` 写入字符串；要写数组，使用 `JSON_ARRAY(1,2)`，或 `JSON_EXTRACT('[1,2]','$')` 显式解析文本。

支持普通查询、MySQL Prepared Statement 参数、`INSERT ... VALUES/VALUE/SET/SELECT`、`REPLACE`、`ON DUPLICATE KEY UPDATE` 和 `UPDATE` 表达式。VALUES 中新增标量表达式支持；纯字面量 VALUES 不分配表达式映射。含表达式的多行 VALUES 和 INSERT SELECT 使用语句级快照，表达式/约束错误不会留下部分插入；普通字面量多行 INSERT 现在也使用语句快照，失败时整批回滚。自增号仍允许失败或回滚后留空洞。嵌套子查询读取仍需对应 SELECT 权限。

基础 JSON 列和直接 JSON 函数投影经过派生表、CTE、只读视图、标量子查询和 CREATE TABLE AS SELECT 时保留 JSON 标记。相关子查询在 JSON 函数内按当前行求值。CASE/混合类型表达式经过物化后的 JSON 类型推导、用户变量，以及所有聚合/窗口组合未实现完整 MySQL 类型传播；必要时使用 `JSON_EXTRACT(value,'$')` 显式解析文本。

## 路径与限制

- 支持根 `$`、成员 `$.name`、双引号成员 `$."a.b"`、Unicode 成员名和零基数组下标 `$[0]`，可以组合。
- `JSON_EXTRACT`、`JSON_CONTAINS_PATH` 支持 `.*` 和 `[*]`。不支持递归 `**`、`last`/`last-N`、数组范围、负数下标等扩展。
- 修改函数、JSON_KEYS 和 JSON_LENGTH 只支持单值路径，禁止通配符。JSON_LENGTH 的这个范围小于较新 MySQL 8.0 版本。
- 路径最多 4096 字节、100 段；JSON 最多 100 层容器嵌套。过深文档在函数中报错，JSON_VALID 返回 0。原有 JSON 列写入语法校验规则不变，因此旧的过深文本可能可存储但不能调用这些函数。
- 修改按参数从左到右进行，不自动创建缺失的中间对象。超过数组末尾的下标只追加一个元素，不创建稀疏数组；非数组按适用的数组下标规则自动包装。
- 暂不支持 `->`/`->>`、`CAST(... AS JSON)`、JSON_TABLE、JSON_CONTAINS、JSON_SEARCH、JSON_MERGE 系列、JSON_ARRAYAGG/JSON_OBJECTAGG、JSON Schema、JSON 路径索引及多值索引。
- 文本输出的空白、对象键顺序、Unicode/HTML 转义不承诺与 MySQL 逐字节相同。比较结果时应解析 JSON。路径成员名区分大小写。
- JSON 列/结果通过现有 TEXT 协议类型传输，未发布原生 MYSQL_TYPE_JSON 元数据；客户端可读取文本自行解码。
- JSON 比较、排序、混合类型转换及 SQL_MODE 并非完整 MySQL 实现；取字符串作条件时使用 JSON_UNQUOTE，数值标量可参与现有数值表达式，但算术仍有现有 float64 精度限制。超出 uint64 范围或极端指数、无效 Unicode 代理项的行为不承诺与 MySQL 一致。

JSON 函数错误返回相应 MySQL 错误号：1582 参数个数，3141 无效文档，3143 无效/不支持的路径，3149 禁用通配符，3153 删除根，3154 无效 one/all，3157 过深，3158 NULL 键。协议 SQLSTATE 仍沿用现有 HY000。JSON 列原有载荷校验失败仍为 3819。[MySQL 错误号参考](https://dev.mysql.com/doc/connector-j/en/connector-j-reference-error-sqlstates.html)

## 内存和性能

没有新增常驻 JSON 文档或路径缓存，也没有给每个存储单元增加字段。函数结果只保留不可变文本和表达式内的类型标记，树结构按需解析并在调用后释放；普通无子查询表达式跳过相关作用域分配。对象/数组构造通过原始 JSON 片段嵌入嵌套结果；修改函数每次调用解析一次输入文档，依次修改后统一编码。

这仍会产生与文档大小相关的临时内存，多个 JSON 函数调用可能重复解析。字段路径条件通常逐行扫描；修改按现有文本/快照方式写入，不是局部二进制更新。含表达式的多行 INSERT 和 INSERT SELECT 的原子快照也有额外内存、索引重建及独占语句锁成本，建议分批操作。

2026-09-07，Windows amd64、i5-12600KF、Go 1.26.5，三次 200ms 小文档函数微基准中位数：

| 调用 | 耗时 | 每次分配字节 | 分配次数 |
|---|---:|---:|---:|
| JSON_OBJECT，3 对键值 | 1.080 µs | 689 B | 15 |
| JSON_EXTRACT，约 40 字节对象 | 1.743 µs | 1707 B | 28 |
| JSON_SET，同样对象改数组元素 | 2.526 µs | 1988 B | 34 |

运行：`go test ./executor -run '^$' -bench '^BenchmarkJSONFunctions$' -benchmem -benchtime=200ms -count=3`。这是函数级耗时和分配量，不是完整 SQL 延迟、吞吐量或进程物理内存承诺。
