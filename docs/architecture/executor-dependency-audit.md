# Executor dependency audit

The fourteen former executor/mvcc_*.go production files consume storageengine.Txn, logical storage schema/value types, parser expressions and resource policies. None consumes mvcc transaction internals or bbolt buckets. They now use responsibility-based file names and SQL-prefixed private declarations. An AST identifier rewrite preserves string literals, wire messages, metadata field names and row bytes. Actual mvcc/ and mvccadapter/ implementations and parser.MVCCMaintenance remain unchanged. SetMVCCAutocommit remains a deprecated forwarding API.

transaction_engine owns statement transactions/catalog caching; mutation, alter_table and foreign_key bind SQL writes and constraints; access_plan, range_access, unique_access and secondary_index select logical key access; scan_batch bounds borrowed buffers; row_layout owns SQL value encoding; projection_binding/filter_binding/explain bind SQL expressions; maintenance delegates optional engine capabilities. No file implements a second transaction engine.

Remaining historical test names describe the backend fixture under test and are intentionally retained. The legacy reader and offline orchestration are isolated separately. sqllayout owns namespace bytes; storageengine/relation_layout_compat.go retains the existing Table convenience API.

Audited declaration map:

- SetMVCCAutocommit → SetAutocommit
- alterMVCC → alterSQL
- bindMVCCExplainClauses → bindSQLExplainClauses
- bindMVCCExplainExpr → bindSQLExplainExpr
- bindMVCCFilter → bindSQLFilter
- decodeMVCCRow → decodeSQLRow
- decodeMVCCRowInto → decodeSQLRowInto
- decodeMVCCRowProjected → decodeSQLRowProjected
- encodeMVCCRow → encodeSQLRow
- errMVCCRowEncoding → errSQLRowEncoding
- executeMVCCExplain → executeSQLExplain
- executeMVCCMaintenance → executeSQLMaintenance
- executeMVCCStatement → executeSQLStatement
- insertMVCC → insertSQL
- mutateMVCC → mutateSQL
- mvccAccessAll → sqlAccessAll
- mvccAccessOrdered → sqlAccessOrdered
- mvccAccessPlan → sqlAccessPlan
- mvccAccessPoint → sqlAccessPoint
- mvccAccessRange → sqlAccessRange
- mvccAccessUnique → sqlAccessUnique
- mvccAlterTarget → sqlAlterTarget
- mvccBatchBytes → sqlBatchBytes
- mvccBatchEntry → sqlBatchEntry
- mvccBatchRows → sqlBatchRows
- mvccColumnPosition → sqlColumnPosition
- mvccCompactRowEncoding → sqlCompactRowEncoding
- mvccFKIndex → sqlFKIndex
- mvccFKParentRow → sqlFKParentRow
- mvccForeignReferrers → sqlForeignReferrers
- mvccIntegerKey → sqlIntegerKey
- mvccIntegerKeyEncoding → sqlIntegerKeyEncoding
- mvccIntegerPrimary → sqlIntegerPrimary
- mvccPrimaryKey → sqlPrimaryKey
- mvccPrimaryOrder → sqlPrimaryOrder
- mvccPrimaryRange → sqlPrimaryRange
- mvccProjectionMask → sqlProjectionMask
- mvccReadTableCache → sqlReadTableCache
- mvccRowMagic → sqlRowMagic
- mvccRowReader → sqlRowReader
- mvccSafeRangeExpression → sqlSafeRangeExpression
- mvccStartsImplicitTransaction → sqlStartsImplicitTransaction
- mvccUniqueAccess → sqlUniqueAccess
- mvccUniqueCandidate → sqlUniqueCandidate
- mvccUniqueCandidates → sqlUniqueCandidates
- planMVCCAccess → planSQLAccess
- planMVCCSecondary → planSQLSecondary
- prepareMVCCForeignKeys → prepareSQLForeignKeys
- refreshMVCCMetadata → refreshSQLMetadata
- rejectMVCCReferencedDrop → rejectSQLReferencedDrop
- scanMVCCUnique → scanSQLUnique
- validateMVCCCheckDefinition → validateSQLCheckDefinition
- validateMVCCKeyEncoding → validateSQLKeyEncoding
- validateMVCCReferences → validateSQLReferences
- validateMVCCSelectShape → validateSQLSelectShape

Private session/engine fields were audited separately: mvccTransaction (storageengine.Txn) becomes transaction; mvccReadCache becomes tableReadCache; mvccMetadata (mutex) becomes catalogMutex; mvccMetadataVersion becomes catalogRevision. They carry no backend-specific state.
