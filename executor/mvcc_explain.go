package executor

import (
	"fmt"
	"gbaselite/parser"
	"gbaselite/storage"
	"gbaselite/storageengine"
	"strings"
)

func executeMVCCExplain(tx storageengine.Txn, session *Session, query parser.Query) (*Result, error) {
	s, ok := query.(parser.Select)
	if !ok {
		return nil, fmt.Errorf("MVCC EXPLAIN supports a single SELECT")
	}
	if err := validateMVCCSelectShape(s); err != nil {
		return nil, err
	}
	columns := []Column{{Name: "id", Type: storage.TypeBigInt}, {Name: "select_type", Type: storage.TypeVarchar}, {Name: "table", Type: storage.TypeVarchar, Nullable: true}, {Name: "partitions", Type: storage.TypeVarchar, Nullable: true}, {Name: "type", Type: storage.TypeVarchar, Nullable: true}, {Name: "possible_keys", Type: storage.TypeVarchar, Nullable: true}, {Name: "key", Type: storage.TypeVarchar, Nullable: true}, {Name: "key_len", Type: storage.TypeVarchar, Nullable: true}, {Name: "ref", Type: storage.TypeVarchar, Nullable: true}, {Name: "rows", Type: storage.TypeBigInt, Nullable: true}, {Name: "filtered", Type: storage.TypeDouble, Nullable: true}, {Name: "Extra", Type: storage.TypeText}}
	if len(s.Joins) > 0 {
		inputs, err := bindMVCCJoins(tx, session, s)
		if err != nil {
			return nil, err
		}
		schema := inputs[len(inputs)-1].combined
		if err := bindMVCCExplainClauses(s, schema); err != nil {
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
			if err = bindMVCCExplainExpr(expr, schema); err != nil {
				return nil, err
			}
		}
		if err = bindMVCCExplainExpr(s.Where, schema); err != nil {
			return nil, err
		}
		result := &Result{Columns: columns}
		for i, input := range inputs {
			name := input.join.TableAlias
			if name == "" {
				_, name = splitTableName(input.join.Table)
			}
			extra := "MVCC nested loop; Statistics unavailable"
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
	var table versionedTable
	var schema *storage.Table
	var err error
	if s.Table != "" {
		table, schema, _, err = loadVersionedTable(tx, session, s.Table)
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
		if err = bindMVCCExplainExpr(expr, schema); err != nil {
			return nil, err
		}
	}
	if err = bindMVCCExplainExpr(s.Where, schema); err != nil {
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
		if err = bindMVCCExplainExpr(expr, schema); err != nil {
			return nil, err
		}
	}
	if err := bindMVCCExplainClauses(s, schema); err != nil {
		return nil, err
	}
	result := &Result{Columns: columns}
	if s.Table == "" {
		result.Rows = [][]any{{int64(1), "SIMPLE", nil, nil, nil, nil, nil, nil, nil, int64(1), nil, "No tables used"}}
		return result, nil
	}
	p := planMVCCAccess(s, table, schema, session)
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
	if p.kind == mvccAccessPoint || p.kind == mvccAccessUnique {
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
func bindMVCCExplainExpr(expr parser.Expr, schema *storage.Table, aliases ...map[string]bool) error {
	var children []parser.Expr
	switch v := expr.(type) {
	case nil, parser.LiteralExpr:
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
		return fmt.Errorf("unknown column %s", v.Name)
	case parser.BinaryExpr:
		children = []parser.Expr{v.Left, v.Right}
	case parser.UnaryExpr:
		children = []parser.Expr{v.Value}
	case parser.RowExpr:
		children = v.Values
	case parser.FunctionExpr:
		children = v.Args
	case parser.IntervalExpr:
		children = []parser.Expr{v.Value}
	case parser.InExpr:
		if v.Subquery != nil {
			return fmt.Errorf("MVCC scalar subqueries are not supported")
		}
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
		return fmt.Errorf("unsupported MVCC EXPLAIN expression %T", expr)
	}
	for _, child := range children {
		if err := bindMVCCExplainExpr(child, schema, aliases...); err != nil {
			return err
		}
	}
	return nil
}

func bindMVCCExplainClauses(s parser.Select, schema *storage.Table) error {
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
		if err = bindMVCCExplainExpr(expr, schema, aliases); err != nil {
			return err
		}
	}
	return bindMVCCExplainExpr(s.Having, schema, aliases)
}
