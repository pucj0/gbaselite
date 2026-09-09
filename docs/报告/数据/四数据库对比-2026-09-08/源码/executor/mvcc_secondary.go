package executor

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"gbaselite/mvcc"
	"gbaselite/parser"
	"gbaselite/storage"
	"strings"
)

func secondaryColumns(table versionedTable, idx storage.Index) ([]int, bool) {
	if table.SecondaryEncoding != 1 || idx.Primary || idx.Unique {
		return nil, false
	}
	var positions []int
	for _, name := range idx.Columns {
		found := -1
		for p, col := range table.Definition.Columns {
			if strings.EqualFold(name, col.Name) {
				if strings.EqualFold(col.SQLType, "JSON") {
					return nil, false
				}
				switch col.Type {
				case storage.TypeInt, storage.TypeBigInt, storage.TypeText, storage.TypeVarchar:
					found = p
				}
				break
			}
		}
		if found < 0 {
			return nil, false
		}
		positions = append(positions, found)
	}
	return positions, len(positions) > 0
}
func secondaryPart(v storage.Value) []byte {
	if v.Null {
		return []byte{0}
	}
	if v.Type == storage.TypeInt || v.Type == storage.TypeBigInt {
		return append([]byte{1}, mvccIntegerKey(v.Int64)...)
	}
	p := []byte{1}
	for _, c := range []byte(v.Text) {
		p = append(p, c)
		if c == 0 {
			p = append(p, 255)
		}
	}
	return append(p, 0, 0)
}
func secondaryCover(table versionedTable, positions []int) []bool {
	cover := make([]bool, len(table.Definition.Columns))
	for _, p := range positions {
		cover[p] = true
	}
	for _, idx := range table.Definition.Indexes {
		if idx.Primary {
			for _, name := range idx.Columns {
				for p, c := range table.Definition.Columns {
					if strings.EqualFold(c.Name, name) {
						cover[p] = true
					}
				}
			}
		}
	}
	return cover
}
func secondaryEntry(table versionedTable, positions []int, owner []byte, row storage.Row) ([]byte, []byte, error) {
	var k []byte
	for _, p := range positions {
		k = append(k, secondaryPart(row[p])...)
	}
	k = append(k, owner...)
	cover := secondaryCover(table, positions)
	projected := make(storage.Row, len(row))
	for i, v := range row {
		if cover[i] {
			projected[i] = v
		} else {
			projected[i] = storage.Value{Type: v.Type, Null: true}
		}
	}
	encoded, err := encodeMVCCRow(table, projected)
	if err != nil {
		return nil, nil, err
	}
	value := make([]byte, 2+len(owner)+len(encoded))
	binary.BigEndian.PutUint16(value[:2], uint16(len(owner)))
	copy(value[2:], owner)
	copy(value[2+len(owner):], encoded)
	return k, value, nil
}
func writeSecondaryEntries(tx *mvcc.Tx, table versionedTable, oldKey, newKey []byte, oldRow, newRow storage.Row) error {
	for _, idx := range table.Definition.Indexes {
		positions, ok := secondaryColumns(table, idx)
		if !ok {
			continue
		}
		space := "secondary/" + table.ID + "/" + idx.Name
		var okKey, ov, nk, nv []byte
		var err error
		if oldRow != nil {
			okKey, ov, err = secondaryEntry(table, positions, oldKey, oldRow)
			if err != nil {
				return err
			}
		}
		if newRow != nil {
			nk, nv, err = secondaryEntry(table, positions, newKey, newRow)
			if err != nil {
				return err
			}
		}
		if oldRow != nil && newRow != nil && bytes.Equal(okKey, nk) && bytes.Equal(ov, nv) {
			continue
		}
		if oldRow != nil {
			if err = tx.Delete(space, okKey); err != nil {
				return err
			}
		}
		if newRow != nil {
			if err = tx.Put(space, nk, nv); err != nil {
				return err
			}
		}
	}
	return nil
}
func prefixSuccessor(p []byte) []byte {
	out := bytes.Clone(p)
	for i := len(out) - 1; i >= 0; i-- {
		if out[i] != 255 {
			out[i]++
			return out[:i+1]
		}
	}
	return nil
}
func planMVCCSecondary(s parser.Select, table versionedTable, schema *storage.Table, session *Session) (mvccAccessPlan, bool) {
	if table.SecondaryEncoding != 1 || !safeMutationIndexExpression(s.Where, schema) {
		return mvccAccessPlan{}, false
	}
	hasIndex := false
	for _, idx := range table.Definition.Indexes {
		if !idx.Primary && !idx.Unique {
			hasIndex = true
			break
		}
	}
	if !hasIndex {
		return mvccAccessPlan{}, false
	}
	equal := make(map[int]storage.Value)
	var visit func(parser.Expr)
	visit = func(e parser.Expr) {
		b, ok := e.(parser.BinaryExpr)
		if !ok {
			return
		}
		if b.Operator == "AND" {
			visit(b.Left)
			visit(b.Right)
			return
		}
		if b.Operator != "=" && b.Operator != "<=>" {
			return
		}
		id, ok := b.Left.(parser.Identifier)
		lit, lok := b.Right.(parser.LiteralExpr)
		if !ok || !lok {
			id, ok = b.Right.(parser.Identifier)
			lit, lok = b.Left.(parser.LiteralExpr)
		}
		if !ok || !lok {
			return
		}
		p, ok := queryColumnIndex(schema, id.Name)
		if !ok {
			return
		}
		col := table.Definition.Columns[p]
		if isTextColumn(col.Type) {
			coll := col.Collation
			if coll == "" {
				coll = session.CollationConnection
			}
			if coll != "binary" && !strings.HasSuffix(coll, "_bin") {
				return
			}
		}
		v, err := literalToValue(lit.Value, col)
		if err == nil && !v.Null {
			equal[p] = v
		}
	}
	visit(s.Where)
	needed := mvccProjectionMask(s, schema)
	bestPrefix := 0
	var best mvccAccessPlan
	for _, idx := range table.Definition.Indexes {
		positions, ok := secondaryColumns(table, idx)
		if !ok {
			continue
		}
		var prefix []byte
		n := 0
		for _, pos := range positions {
			v, ok := equal[pos]
			if !ok {
				break
			}
			prefix = append(prefix, secondaryPart(v)...)
			n++
		}
		if n == 0 {
			continue
		}
		cover := secondaryCover(table, positions)
		covered := len(s.Items) > 0
		for p := range cover {
			if (needed == nil || needed[p]) && !cover[p] {
				covered = false
			}
		}
		if n > bestPrefix || n == bestPrefix && covered && !best.covering {
			bestPrefix = n
			best = mvccAccessPlan{kind: "ref", index: idx.Name, space: "secondary/" + table.ID + "/" + idx.Name, bounds: mvcc.KeyRange{Lower: prefix, LowerInclusive: true, Upper: prefixSuccessor(prefix)}, covering: covered}
		}
	}
	return best, bestPrefix > 0
}
func (p mvccAccessPlan) scanSecondary(ctx context.Context, tx *mvcc.Tx, table versionedTable, yield func([]byte, []byte) error) error {
	return tx.ScanRange(ctx, p.space, p.bounds, func(k, v []byte) error {
		if len(v) < 2 {
			return fmt.Errorf("invalid covering index record")
		}
		n := int(binary.BigEndian.Uint16(v[:2]))
		if n > len(v)-2 {
			return fmt.Errorf("invalid covering index owner")
		}
		owner := v[2 : 2+n]
		if p.covering {
			return yield(owner, v[2+n:])
		}
		row, ok, err := tx.Get("row/"+table.ID, owner)
		if err != nil || !ok {
			return err
		}
		return yield(owner, row)
	})
}
