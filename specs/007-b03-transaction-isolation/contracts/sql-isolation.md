# Contract: SQL Syntax, Session Scope and Introspection

**Model**: MySQL 8.4 `SET TRANSACTION` as normative comparator；B03 范围**会话/下个事务**，不含 GLOBAL 可写配置。

## Supported commands (must work end-to-end)

```sql
SET SESSION TRANSACTION ISOLATION LEVEL READ UNCOMMITTED;
SET SESSION TRANSACTION ISOLATION LEVEL READ COMMITTED;
SET SESSION TRANSACTION ISOLATION LEVEL REPEATABLE READ;
SET SESSION TRANSACTION ISOLATION LEVEL SERIALIZABLE;
SET TRANSACTION ISOLATION LEVEL READ COMMITTED;
SET SESSION transaction_isolation = 'READ-COMMITTED';
SET @@SESSION.transaction_isolation = 'SERIALIZABLE';
SET @@transaction_isolation = 'READ-UNCOMMITTED'; -- next txn ONLY
SELECT @@session.transaction_isolation;
SELECT @@global.transaction_isolation;
SELECT @@tx_isolation; -- legacy alias read path
SHOW SESSION VARIABLES LIKE 'transaction_isolation';
SHOW GLOBAL VARIABLES LIKE 'transaction_isolation';
BEGIN;
COMMIT;
ROLLBACK;
```

`READ UNCOMMITTED`, `READ COMMITTED`, `REPEATABLE READ`, `SERIALIZABLE` 为 SET TRANSACTION 语法值；变量赋值用 `READ-UNCOMMITTED` 等带连字符的字符串，变量显示也带连字符。

## Scope table

| SQL | Affects | Valid during active txn? | Introspection |
|---|---|---|---|
| `SET SESSION TRANSACTION ISOLATION LEVEL RC` | 会话所有**之后创建**的 txn | Yes，当前 active 不改变 | `@@session...` 报新 sessionDefault |
| `SET TRANSACTION ISOLATION LEVEL RU` | 下一**一个** root txn | No，报 transaction-active error | `@@session...` 继续报告 sessionDefault 而不是 pendingNext |
| `SET SESSION transaction_isolation='RR'` | 后续事务，清除先前 pending override | Yes，当前 active 不变 | sessionDefault |
| `SET @@SESSION.transaction_isolation='SERIALIZABLE'` | 后续事务，清 pending | Yes，当前 active 不变 | sessionDefault |
| `SET @@transaction_isolation='RU'` | 下一 txn | No | `@@session` 不伪称下一事务值 |
| `SELECT @@global.transaction_isolation` | 只读服务器默认 | Yes | `REPEATABLE-READ` in B03 |

### Concrete state trace

```text
new connection: sessionDefault=RR, pending=nil, active=nil
SET SESSION ... RC: default=RC
SET TRANSACTION ... SER: default=RC, pending=SER
BEGIN success: active=SER, pending=nil
SET SESSION ... RU: default=RU, active still SER
COMMIT: active=nil
BEGIN: active=RU
ROLLBACK: active=nil
ResetConnection: default=RR, pending=nil, active=nil
```

If Begin fails (resource/leader/connection), retain pending until a real root starts. Treat autocommit single-statement SQL as a real root for pending consumption; `SET`/`SHOW` metadata-only statements that do not start an engine root must not silently consume pending. Distinguish MySQL implicit DDL commit semantics as separately documented incompatibility if not implemented.

## Fail closed / excluded syntax

- `SET GLOBAL TRANSACTION ...`, `SET GLOBAL transaction_isolation=...`, `SET PERSIST ...`: explicit unsupported error; do not claim accepted.
- `SET TRANSACTION READ ONLY/READ WRITE` 或与隔离特征组合：本期 unsupported，除非另行实现；不能忽略一部分后回 OK。
- `START TRANSACTION WITH CONSISTENT SNAPSHOT`、`START TRANSACTION READ ONLY`、`BEGIN` 附加非法 token：明确错误，不静默接受。
- 多重 `ISOLATION LEVEL`、无效 level、部分字符串匹配、SET 多赋值无法原子验证的语法：整个语句失败且 state 原样。
- `SET TRANSACTION` 无作用域时如果事务已经活动，ERROR（理想 SQLSTATE `25001` / ER_CANT_CHANGE_TX_CHARACTERISTICS）；支持的协议映射要对拍验证。
- 不允许宽松 server literal substring 拦截所有含隔离单词的合法字符串字面量或注释。

## Invocation equality

`executor.Engine.Execute`、`server.ExecuteCompatible`、MySQL text/binary prepared 路径在支持的语法上共享 parse AST/state transition，不允许某一路只更新兼容变量而引擎仍 RR。已准备的 prepared statement 每次执行应读取**执行时** session state，而不是 prepare 时 level。

## Observability

`SHOW VARIABLES LIKE ...`、`SELECT @@...` 输出同一个状态源；对外只展示 scope，不泄露 `activeLevel` 内部诊断作为 session default。重置连接后默认恢复 RR。
