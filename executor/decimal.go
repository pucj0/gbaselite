package executor

import (
	"fmt"
	"strconv"
	"strings"

	"gbaselite/parser"
	"gbaselite/storage"
)

// Decimal operands retain exact arithmetic unless explicitly mixed with FLOAT
// or DOUBLE, whose approximate semantics remain unchanged.
func decimalArithmetic(operator string, left, right any) (any, bool, error) {
	_, ld := left.(storage.Decimal)
	_, rd := right.(storage.Decimal)
	if !ld && !rd {
		return nil, false, nil
	}
	switch left.(type) {
	case float32, float64:
		return nil, false, nil
	}
	switch right.(type) {
	case float32, float64:
		return nil, false, nil
	}
	a, err := storage.DecimalFrom(left)
	if err != nil {
		return nil, true, err
	}
	b, err := storage.DecimalFrom(right)
	if err != nil {
		return nil, true, err
	}
	if (operator == "/" || operator == "%") && b.IsZero() {
		return nil, true, nil
	}
	value, err := storage.DecimalBinary(operator, a, b)
	return value, true, err
}
func decimalCompare(left, right any) (int, bool) {
	_, ld := left.(storage.Decimal)
	_, rd := right.(storage.Decimal)
	if !ld && !rd {
		return 0, false
	}
	switch left.(type) {
	case float32, float64:
		return 0, false
	}
	switch right.(type) {
	case float32, float64:
		return 0, false
	}
	a, err := storage.DecimalFrom(left)
	if err != nil {
		return 0, false
	}
	b, err := storage.DecimalFrom(right)
	if err != nil {
		return 0, false
	}
	return storage.CompareDecimal(a, b), true
}
func decimalLiteral(text string) (any, error) {
	if strings.ContainsAny(text, "eE") {
		return strconv.ParseFloat(text, 64)
	}
	if !strings.Contains(text, ".") {
		if n, err := strconv.ParseInt(text, 10, 64); err == nil {
			return n, nil
		}
	}
	return storage.ParseDecimal(text)
}
func decimalFunction(name string, args []any) (any, bool, error) {
	if len(args) == 0 {
		return nil, false, nil
	}
	d, ok := args[0].(storage.Decimal)
	if !ok {
		return nil, false, nil
	}
	switch name {
	case "ABS":
		if len(args) != 1 {
			return nil, true, fmt.Errorf("ABS expects 1 argument")
		}
		return storage.Decimal(strings.TrimPrefix(string(d), "-")), true, nil
	case "CEIL", "CEILING", "FLOOR":
		if len(args) != 1 {
			return nil, true, fmt.Errorf("%s expects 1 argument", name)
		}
		integral, err := d.Round(0, true)
		if err != nil {
			return nil, true, err
		}
		cmp := storage.CompareDecimal(d, integral)
		if name == "FLOOR" && cmp < 0 {
			integral, err = storage.DecimalBinary("-", integral, "1")
		} else if name != "FLOOR" && cmp > 0 {
			integral, err = storage.DecimalBinary("+", integral, "1")
		}
		return integral, true, err
	case "ROUND", "TRUNCATE":
		if len(args) < 1 || len(args) > 2 || (name == "TRUNCATE" && len(args) != 2) {
			return nil, true, fmt.Errorf("invalid argument count for %s", name)
		}
		digits := int64(0)
		if len(args) == 2 {
			if args[1] == nil {
				return nil, true, nil
			}
			p, err := storage.DecimalFrom(args[1])
			if err != nil {
				return nil, true, err
			}
			digits, err = p.Int64()
			if err != nil {
				return nil, true, err
			}
		}
		if digits < -storage.MaxDecimalPrecision || digits > storage.MaxDecimalScale {
			return nil, true, storage.ErrDecimalRange
		}
		value, err := d.Round(int(digits), name == "TRUNCATE")
		return value, true, err
	case "MOD":
		if len(args) != 2 {
			return nil, true, fmt.Errorf("MOD expects 2 arguments")
		}
		if args[1] == nil {
			return nil, true, nil
		}
		return decimalArithmetic("%", d, args[1])
	}
	return nil, false, nil
}

// decimalAggregate is value-copy-safe, including when aggregate state is kept
// in a map. It holds at most one 65-digit immutable string per group.
type decimalAggregate struct {
	sum storage.Decimal
	has bool
	err error
}

func (a *decimalAggregate) add(candidate storage.Decimal) {
	if a.err != nil {
		return
	}
	if !a.has {
		a.sum = candidate
		a.has = true
		return
	}
	a.sum, a.err = storage.DecimalBinary("+", a.sum, candidate)
}
func (a decimalAggregate) average(count int64) (storage.Decimal, error) {
	if a.err != nil {
		return "", a.err
	}
	return storage.DecimalBinary("/", a.sum, storage.Decimal(strconv.FormatInt(count, 10)))
}

func decimalPredicateLiteral(literal parser.Literal, column storage.Column) (storage.Value, error) {
	if column.Type == storage.TypeDecimal && literal.Kind != parser.LiteralNull {
		return storage.NewValue(storage.TypeDecimal, literal.Text)
	}
	return literalToValue(literal, column)
}
func decimalResultDeclaration(column Column, rows [][]any, index int) (string, error) {
	if column.SQLType != "" {
		return column.SQLType, nil
	}
	scale, integerDigits := 0, 0
	for _, row := range rows {
		if index >= len(row) || row[index] == nil {
			continue
		}
		d, err := storage.DecimalFrom(row[index])
		if err != nil {
			return "", err
		}
		scale = max(scale, d.Scale())
		s := strings.TrimPrefix(string(d), "-")
		s, _, _ = strings.Cut(s, ".")
		integerDigits = max(integerDigits, len(strings.TrimLeft(s, "0")))
	}
	if integerDigits+scale > storage.MaxDecimalPrecision {
		return "", storage.ErrDecimalRange
	}
	return fmt.Sprintf("decimal(%d,%d)", max(1, integerDigits+scale), scale), nil
}
