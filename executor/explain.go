package executor

import (
	"fmt"
	"gbaselite/parser"
	"gbaselite/physical"
	"gbaselite/storage"
	"gbaselite/storageengine"
	"strings"
)

func executeSQLExplain(tx storageengine.Txn, session *Session, query parser.Query) (result *Result, err error) {
	if union, ok := query.(parser.Union); ok {
		var combined *Result
		for i, branch := range union.Queries {
			result, err := executeSQLExplain(tx, session, branch)
			if err != nil {
				return nil, err
			}
			for _, row := range result.Rows {
				row[0] = int64(i + 1)
				if i > 0 {
					row[1] = "UNION"
				}
			}
			if combined == nil {
				combined = result
			} else {
				combined.Rows = append(combined.Rows, result.Rows...)
			}
		}
		if combined == nil {
			return nil, fmt.Errorf("empty UNION")
		}
		return combined, nil
	}
	s, ok := query.(parser.Select)
	if !ok {
		return nil, fmt.Errorf("EXPLAIN supports a single SELECT")
	}
	if err := validateSQLSelectShape(s); err != nil {
		return nil, err
	}
	var bound *boundQuery
	if s.Table != "" {
		bound, err = bindPhysicalSelect(operatorContext(session), tx, session, s)
		if err != nil {
			return nil, err
		}
	}
	defer func() {
		if err == nil && result != nil && bound != nil {
			for _, r := range result.Rows {
				r[11] = fmt.Sprint(r[11]) + "; Pipeline: " + physical.Describe(bound.Input).String()
			}
		}
	}()
	columns := []Column{{Name: "id", Type: storage.TypeBigInt}, {Name: "select_type", Type: storage.TypeVarchar}, {Name: "table", Type: storage.TypeVarchar, Nullable: true}, {Name: "partitions", Type: storage.TypeVarchar, Nullable: true}, {Name: "type", Type: storage.TypeVarchar, Nullable: true}, {Name: "possible_keys", Type: storage.TypeVarchar, Nullable: true}, {Name: "key", Type: storage.TypeVarchar, Nullable: true}, {Name: "key_len", Type: storage.TypeVarchar, Nullable: true}, {Name: "ref", Type: storage.TypeVarchar, Nullable: true}, {Name: "rows", Type: storage.TypeBigInt, Nullable: true}, {Name: "filtered", Type: storage.TypeDouble, Nullable: true}, {Name: "Extra", Type: storage.TypeText}}
	if len(s.Joins) > 0 {
		inputs, err := bindJoins(tx, session, s)
		if err != nil {
			return nil, err
		}
		schema := inputs[len(inputs)-1].combined
		if err := bindSQLExplainClauses(s, schema); err != nil {
			return nil, err
		}
		for _, item := range s.Items {
			if item.Expression == "*" {
				continue
			}
			expr, err := parser.ParseExpression(item.Expression)
			if err != nil {
				return nil, err
			}
			if err = bindSQLExplainExpr(expr, schema); err != nil {
				return nil, err
			}
		}
		if err = bindSQLExplainExpr(s.Where, schema); err != nil {
			return nil, err
		}
		result = &Result{Columns: columns}
		for i, input := range inputs {
			name := input.join.TableAlias
			if name == "" {
				_, name = splitTableName(input.join.Table)
			}
			extra := "Physical nested loop; Statistics unavailable"
			if i > 0 {
				extra += "; integer equality index checked per outer row"
			}
			if len(s.GroupBy) > 0 {
				extra += "; Using temporary"
			}
			if len(s.OrderBy) > 0 {
				extra += "; Using filesort"
			}
			result.Rows = append(result.Rows, []any{int64(1), "SIMPLE", name, nil, "ALL", nil, nil, nil, nil, nil, nil, extra})
		}
		return result, nil
	}
	var schema *storage.Table
	if s.Table != "" {
		_, schema, _, err = loadVersionedTable(tx, session, s.Table)
		if err != nil {
			return nil, err
		}
		if s.TableAlias != "" {
			schema, err = qualifySchema(schema, s.TableAlias)
			if err != nil {
				return nil, err
			}
		}
	}
	aliases := make(map[string]bool)
	for _, item := range s.Items {
		if item.Alias != "" {
			aliases[strings.ToLower(item.Alias)] = true
		}
		if item.Expression == "*" {
			continue
		}
		expr, err := parser.ParseExpression(item.Expression)
		if err != nil {
			return nil, err
		}
		if err = bindSQLExplainExpr(expr, schema); err != nil {
			return nil, err
		}
	}
	if err = bindSQLExplainExpr(s.Where, schema); err != nil {
		return nil, err
	}
	for _, order := range s.OrderBy {
		if aliases[strings.ToLower(order.Column)] {
			continue
		}
		expr, err := parser.ParseExpression(order.Column)
		if err != nil {
			return nil, err
		}
		if err = bindSQLExplainExpr(expr, schema); err != nil {
			return nil, err
		}
	}
	if err := bindSQLExplainClauses(s, schema); err != nil {
		return nil, err
	}
	result = &Result{Columns: columns}
	if s.Table == "" {
		result.Rows = [][]any{{int64(1), "SIMPLE", nil, nil, nil, nil, nil, nil, nil, int64(1), nil, "No tables used"}}
		return result, nil
	}
	p := *bound.Access
	access := p.kind
	var selected, possible, rows any
	if p.index != "" {
		selected = p.index
	}
	if len(p.candidates) > 0 {
		possible = strings.Join(p.candidates, ",")
	} else if p.index != "" {
		possible = p.index
	}
	extra := []string{}
	if len(s.GroupBy) > 0 {
		extra = append(extra, "Using temporary")
	}
	if p.covering {
		extra = append(extra, "Using index")
	}
	if s.Where != nil {
		extra = append(extra, "Using where")
	}
	if len(s.OrderBy) > 0 && (!p.ordered || len(s.GroupBy) > 0) {
		extra = append(extra, "Using filesort")
	}
	if s.Distinct && !selectHasAggregate(s.Items) {
		extra = append(extra, "Using temporary")
	}
	if s.HasLimit && s.Limit == 0 {
		extra = append(extra, "Zero limit")
	}
	if p.kind == sqlAccessPoint || p.kind == sqlAccessUnique {
		access = "const"
		rows = int64(1)
		extra = append(extra, "rows is an upper bound")
	} else {
		extra = append(extra, "Statistics unavailable")
	}
	if p.bounds.Reverse {
		extra = append(extra, "Backward index scan")
	}
	name := s.TableAlias
	if name == "" {
		_, name = splitTableName(s.Table)
	}
	result.Rows = [][]any{{int64(1), "SIMPLE", name, nil, access, possible, selected, nil, nil, rows, nil, strings.Join(extra, "; ")}}
	return result, nil
}

// Resolve column references only. EXPLAIN must not evaluate SLEEP, assignments,
// user functions or row expressions to fabricate cardinality statistics.
func bindSQLExplainExpr(expr parser.Expr, schema *storage.Table, aliases ...map[string]bool) error {
	return bindSQLExplainExprSession(expr, schema, nil, aliases...)
}

// bindSQLExplainExprSession resolves column references, accepting names that the
// active correlation scope publishes so a correlated subquery can be bound while
// its statement evaluates an outer row. Subqueries are not descended into: they
// are bound and checked when they run.
func bindSQLExplainExprSession(expr parser.Expr, schema *storage.Table, session *Session, aliases ...map[string]bool) error {
	var children []parser.Expr
	switch v := expr.(type) {
	case nil, parser.LiteralExpr, parser.ScalarSubquery, parser.ExistsExpr:
		return nil
	case parser.Identifier:
		if len(aliases) > 0 && aliases[0][strings.ToLower(v.Name)] {
			return nil
		}
		if schema != nil {
			if _, ok := queryColumnIndex(schema, v.Name); ok {
				return nil
			}
		}
		if _, ok := correlationScopeValue(session, v.Name); ok {
			return nil
		}
		return fmt.Errorf("unknown column %s", v.Name)
	case parser.BinaryExpr:
		children = []parser.Expr{v.Left, v.Right}
	case parser.UnaryExpr:
		children = []parser.Expr{v.Value}
	case parser.RowExpr:
		children = v.Values
	case parser.FunctionExpr:
		children = v.Args
	case parser.WindowExpr:
		children = append(children, v.Function.Args...)
		children = append(children, v.PartitionBy...)
		for _, order := range v.OrderBy {
			children = append(children, order.Expression)
		}
	case parser.IntervalExpr:
		children = []parser.Expr{v.Value}
	case parser.InExpr:
		children = append([]parser.Expr{v.Value}, v.Values...)
	case parser.BetweenExpr:
		children = []parser.Expr{v.Value, v.Lower, v.Upper}
	case parser.IsExpr:
		children = []parser.Expr{v.Value, v.Target}
	case parser.CaseExpr:
		children = []parser.Expr{v.Operand, v.Else}
		for _, w := range v.Whens {
			children = append(children, w.When, w.Then)
		}
	default:
		return fmt.Errorf("unsupported EXPLAIN expression %T", expr)
	}
	for _, child := range children {
		if err := bindSQLExplainExprSession(child, schema, session, aliases...); err != nil {
			return err
		}
	}
	return nil
}

func bindSQLExplainClauses(s parser.Select, schema *storage.Table) error {
	aliases := make(map[string]bool)
	for _, item := range s.Items {
		if item.Alias != "" {
			aliases[strings.ToLower(item.Alias)] = true
		}
	}
	expressions := append([]string(nil), s.GroupBy...)
	for _, order := range s.OrderBy {
		expressions = append(expressions, order.Column)
	}
	for _, text := range expressions {
		expr, err := parser.ParseExpression(text)
		if err != nil {
			return err
		}
		if err = bindSQLExplainExpr(expr, schema, aliases); err != nil {
			return err
		}
	}
	return bindSQLExplainExpr(s.Having, schema, aliases)
}
