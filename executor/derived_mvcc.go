package executor

import (
	"context"
	"fmt"
	"gbaselite/parser"
	"gbaselite/physical"
	"gbaselite/storage"
	"gbaselite/storageengine"
	"strings"
)

// cteRelation is a statement-local common table expression: the transient schema
// its alias publishes plus the rows the statement snapshot produced for it.
type cteRelation struct {
	schema *storage.Table
	rows   []storage.Row
}

// derivedRelation binds and materializes a derived table or CTE query into a
// transient relation. Rows are buffered through physical.Materialize so the
// statement result-memory budget applies, and every row is converted with the
// same column conversion the rest of the pipeline uses.
func derivedRelation(ctx context.Context, tx storageengine.Txn, session *Session, query parser.Query, alias string, columnNames []string) (*storage.Table, []storage.Row, error) {
	bound, err := bindSubqueryQuery(ctx, tx, session, query)
	if err != nil {
		return nil, nil, err
	}
	if len(columnNames) > 0 && len(columnNames) != len(bound.Columns) {
		return nil, nil, fmt.Errorf("derived table %q column count mismatch", alias)
	}
	definitions := derivedColumns(bound.Columns, alias, columnNames)
	table, err := storage.NewTransientTable(alias, definitions)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid derived table %q: %w", alias, err)
	}
	limit := int64(16 << 20)
	if session != nil && session.query != nil && session.query.options.ResultMemoryBytes > 0 {
		limit = session.query.options.ResultMemoryBytes
	}
	used := int64(0)
	rows := make([]storage.Row, 0, len(definitions))
	buffer := physical.Materialize[[]any]{Input: bound.Input, Clone: func(row []any) []any {
		return append([]any(nil), row...)
	}, Charge: func(row []any) error {
		var chargeErr error
		used, chargeErr = checkResultMemory(limit, used, row)
		return chargeErr
	}}
	err = buffer.Run(ctx, func(values []any) error {
		row := make(storage.Row, len(definitions))
		for index := range definitions {
			converted, conversionErr := interfaceToColumnValue(values[index], definitions[index])
			if conversionErr != nil {
				return conversionErr
			}
			row[index] = converted
		}
		rows = append(rows, row)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return table, rows, nil
}

func derivedColumns(columns []Column, alias string, names []string) []storage.Column {
	definitions := make([]storage.Column, len(columns))
	for index, column := range columns {
		name := column.Name
		if index < len(names) {
			name = names[index]
		}
		name = stripQualifier(name)
		length := 0
		if column.Type == storage.TypeVarchar {
			length = 65535
		}
		definitions[index] = storage.Column{Name: name, Type: column.Type, SQLType: column.SQLType, Collation: column.Collation, Length: length, MetadataVersion: 1, Nullable: true}
		if column.jsonValue {
			definitions[index].SQLType = "JSON"
		}
		if alias != "" {
			definitions[index].Name = alias + "." + name
		}
	}
	return definitions
}

// derivedRows exposes materialized derived/CTE rows as a read-only physical
// relation so the enclosing plan composes it like any other input.
func derivedRows(rows []storage.Row) physical.Operator[storage.Row] {
	return physical.Materialized[storage.Row]{Rows: rows}
}

func (r *subqueryRunner) registerCTE(name string, schema *storage.Table, rows []storage.Row) {
	if r == nil || name == "" {
		return
	}
	if r.ctes == nil {
		r.ctes = make(map[string]cteRelation)
	}
	r.ctes[strings.ToLower(stripQualifier(name))] = cteRelation{schema: schema, rows: rows}
}

// cteFor resolves a statement-local CTE reference. Qualified names never match a
// CTE, and a CTE shadows a base table with the same name.
func cteFor(session *Session, name string) (cteRelation, bool) {
	if session == nil || session.subqueries == nil || name == "" || strings.Contains(name, ".") {
		return cteRelation{}, false
	}
	relation, ok := session.subqueries.ctes[strings.ToLower(stripQualifier(name))]
	return relation, ok
}

// cteSource returns the schema and row source of a CTE reference, applying the
// statement WHERE clause the same way a base-table source does.
func cteSource(session *Session, name string) (*storage.Table, []storage.Row, bool) {
	relation, ok := cteFor(session, name)
	if !ok {
		return nil, nil, false
	}
	return relation.schema, relation.rows, true
}

// bufferedRelation buffers an operator once, charging the statement result
// budget, and replays the rows for every later run. RIGHT JOIN probes its left
// relation once per driving row, so that input must be repeatable.
type bufferedRelation struct {
	input  physical.Operator[storage.Row]
	charge func(storage.Row) error
	rows   []storage.Row
	loaded bool
}

func (b *bufferedRelation) Run(ctx context.Context, yield physical.Yield[storage.Row]) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !b.loaded {
		if err := b.input.Run(ctx, func(row storage.Row) error {
			if err := b.charge(row); err != nil {
				return err
			}
			b.rows = append(b.rows, append(storage.Row(nil), row...))
			return nil
		}); err != nil {
			return err
		}
		b.loaded = true
	}
	for _, row := range b.rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := yield(row); err != nil {
			return err
		}
	}
	return nil
}

// bufferedRows makes an operator repeatable under the statement result budget.
func bufferedRows(session *Session, input physical.Operator[storage.Row]) physical.Operator[storage.Row] {
	limit := int64(16 << 20)
	if session != nil && session.query != nil && session.query.options.ResultMemoryBytes > 0 {
		limit = session.query.options.ResultMemoryBytes
	}
	used := int64(0)
	return &bufferedRelation{input: input, charge: func(row storage.Row) error {
		size := storageRowBytes(row)
		if limit > 0 && used+size > limit {
			return fmt.Errorf("%w: materialized join rows exceed %d bytes", ErrQueryResourceLimit, limit)
		}
		used += size
		return nil
	}}
}

func storageRowBytes(row storage.Row) int64 {
	size := int64(len(row)) * 16
	for _, value := range row {
		if !value.Null {
			size += int64(len(value.Text)) + 32
		}
	}
	return size
}

// identityScan scans a base table for a mutation join and appends the hidden row
// identity value, mapping it back to the storage row key so DELETE can dedupe and
// address rows without a primary key.
func identityScan(tx storageengine.Txn, input *joinInput, access sqlAccessPlan, session *Session) physical.Operator[storage.Row] {
	return physical.Scan[storage.Row]{Plan: &physical.PlanNode{Kind: scanKind(access), Attributes: map[string]string{"table": input.definition.CatalogName, "access": access.kind, "index": access.index}}, Open: func(ctx context.Context) (storageengine.Iterator, error) {
		return openAccessIterator(ctx, tx, input.definition, access)
	}, Decode: func(key, value []byte) (storage.Row, error) {
		if err := checkQuery(session); err != nil {
			return nil, err
		}
		row, err := decodeSQLRow(input.definition, value)
		if err != nil {
			return nil, err
		}
		input.identityNext++
		identity := input.identityNext
		input.identityKeys[identity] = append([]byte(nil), key...)
		idValue, err := storage.NewValue(storage.TypeBigInt, identity)
		if err != nil {
			return nil, err
		}
		return append(row, idValue), nil
	}}
}
