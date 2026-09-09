package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

const MaxDecimalPrecision = 65
const MaxDecimalScale = 30

var ErrDecimalRange = errors.New("DECIMAL value out of range")

// Decimal is an immutable fixed-point value. Its canonical text preserves scale;
// temporary big integers are used only while calculating, never retained in rows.
type Decimal string

func (d Decimal) String() string { return string(d) }
func (d Decimal) MarshalJSON() ([]byte, error) {
	if _, err := ParseDecimal(string(d)); err != nil {
		return nil, err
	}
	return []byte(d), nil
}
func (d Decimal) Scale() int {
	if i := strings.IndexByte(string(d), '.'); i >= 0 {
		return len(d) - i - 1
	}
	return 0
}
func (d Decimal) IsZero() bool {
	for _, c := range d {
		if c >= '1' && c <= '9' {
			return false
		}
	}
	return true
}
func (d Decimal) Float64() (float64, error) { return strconv.ParseFloat(string(d), 64) }
func (d Decimal) Int64() (int64, error) {
	s := string(d)
	if dot := strings.IndexByte(s, '.'); dot >= 0 {
		if strings.Trim(s[dot+1:], "0") != "" {
			return 0, ErrDecimalRange
		}
		s = s[:dot]
	}
	return strconv.ParseInt(s, 10, 64)
}

// ParseDecimal rejects unbounded text/exponents before allocating big integers.
func ParseDecimal(raw string) (Decimal, error) {
	s := strings.TrimSpace(raw)
	if len(s) == 0 || len(s) > 256 {
		return "", fmt.Errorf("%w: invalid decimal", ErrDecimalRange)
	}
	negative := false
	if s[0] == '+' || s[0] == '-' {
		negative = s[0] == '-'
		s = s[1:]
	}
	if s == "" {
		return "", fmt.Errorf("%w: invalid decimal", ErrDecimalRange)
	}
	exponent := 0
	if e := strings.IndexAny(s, "eE"); e >= 0 {
		var err error
		exponent, err = strconv.Atoi(s[e+1:])
		if err != nil || exponent < -MaxDecimalScale || exponent > MaxDecimalPrecision {
			return "", ErrDecimalRange
		}
		s = s[:e]
	}
	dot := -1
	digits := 0
	for i, c := range s {
		if c == '.' && dot < 0 {
			dot = i
			continue
		}
		if c < '0' || c > '9' {
			return "", fmt.Errorf("%w: invalid decimal", ErrDecimalRange)
		}
		digits++
	}
	if digits == 0 {
		return "", fmt.Errorf("%w: invalid decimal", ErrDecimalRange)
	}
	scale := 0
	if dot >= 0 {
		scale = len(s) - dot - 1
		s = s[:dot] + s[dot+1:]
	}
	scale -= exponent
	if scale < 0 {
		s += strings.Repeat("0", -scale)
		scale = 0
	}
	if scale > MaxDecimalScale {
		return "", ErrDecimalRange
	}
	s = strings.TrimLeft(s, "0")
	if s == "" {
		s = "0"
		negative = false
	}
	if len(s) > MaxDecimalPrecision || max(len(s), scale) > MaxDecimalPrecision {
		return "", ErrDecimalRange
	}
	if scale > 0 {
		if len(s) <= scale {
			s = strings.Repeat("0", scale+1-len(s)) + s
		}
		s = s[:len(s)-scale] + "." + s[len(s)-scale:]
	}
	if negative {
		s = "-" + s
	}
	return Decimal(s), nil
}

func DecimalFrom(raw any) (Decimal, error) {
	switch v := raw.(type) {
	case Decimal:
		return ParseDecimal(string(v))
	case string:
		return ParseDecimal(v)
	case []byte:
		return ParseDecimal(string(v))
	case json.Number:
		return ParseDecimal(string(v))
	case int:
		return Decimal(strconv.FormatInt(int64(v), 10)), nil
	case int8:
		return Decimal(strconv.FormatInt(int64(v), 10)), nil
	case int16:
		return Decimal(strconv.FormatInt(int64(v), 10)), nil
	case int32:
		return Decimal(strconv.FormatInt(int64(v), 10)), nil
	case int64:
		return Decimal(strconv.FormatInt(v, 10)), nil
	case uint:
		return Decimal(strconv.FormatUint(uint64(v), 10)), nil
	case uint8:
		return Decimal(strconv.FormatUint(uint64(v), 10)), nil
	case uint16:
		return Decimal(strconv.FormatUint(uint64(v), 10)), nil
	case uint32:
		return Decimal(strconv.FormatUint(uint64(v), 10)), nil
	case uint64:
		return Decimal(strconv.FormatUint(v, 10)), nil
	case bool:
		if v {
			return "1", nil
		}
		return "0", nil
	case float32:
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return "", ErrDecimalRange
		}
		return ParseDecimal(strconv.FormatFloat(float64(v), 'f', -1, 32))
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return "", ErrDecimalRange
		}
		return ParseDecimal(strconv.FormatFloat(v, 'f', -1, 64))
	default:
		return "", typeError(TypeDecimal, raw)
	}
}
func decimalCoefficient(d Decimal) (*big.Int, int) {
	s := string(d)
	scale := d.Scale()
	s = strings.Replace(s, ".", "", 1)
	n := new(big.Int)
	n.SetString(s, 10)
	return n, scale
}
func decimalPower(n int) *big.Int { return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil) }
func decimalFromCoefficient(n *big.Int, scale int) (Decimal, error) {
	s := n.Text(10)
	negative := strings.HasPrefix(s, "-")
	if negative {
		s = s[1:]
	}
	if len(s) > MaxDecimalPrecision || scale > MaxDecimalScale {
		return "", ErrDecimalRange
	}
	if scale > 0 {
		if len(s) <= scale {
			s = strings.Repeat("0", scale+1-len(s)) + s
		}
		s = s[:len(s)-scale] + "." + s[len(s)-scale:]
	}
	if negative {
		s = "-" + s
	}
	return Decimal(s), nil
}
func roundCoefficient(n *big.Int, places int, truncate bool) *big.Int {
	if places <= 0 {
		return n.Mul(n, decimalPower(-places))
	}
	divisor := decimalPower(places)
	q, r := new(big.Int), new(big.Int)
	q.QuoRem(n, divisor, r)
	if !truncate && new(big.Int).Lsh(new(big.Int).Abs(r), 1).Cmp(divisor) >= 0 {
		if n.Sign() < 0 {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}
	return q
}
func (d Decimal) Round(scale int, truncate bool) (Decimal, error) {
	if scale < -MaxDecimalPrecision || scale > MaxDecimalScale {
		return "", ErrDecimalRange
	}
	n, oldScale := decimalCoefficient(d)
	n = roundCoefficient(n, oldScale-scale, truncate)
	if scale < 0 {
		n.Mul(n, decimalPower(-scale))
		scale = 0
	}
	return decimalFromCoefficient(n, scale)
}
func (d Decimal) Quantize(precision, scale int) (Decimal, error) {
	if precision < 1 || precision > MaxDecimalPrecision || scale < 0 || scale > MaxDecimalScale || scale > precision {
		return "", ErrDecimalRange
	}
	rounded, err := d.Round(scale, false)
	if err != nil {
		return "", err
	}
	integer := strings.TrimPrefix(string(rounded), "-")
	if dot := strings.IndexByte(integer, '.'); dot >= 0 {
		integer = integer[:dot]
	}
	if len(strings.TrimLeft(integer, "0")) > precision-scale {
		return "", ErrDecimalRange
	}
	return rounded, nil
}
func DecimalBinary(operator string, left, right Decimal) (Decimal, error) {
	a, as := decimalCoefficient(left)
	b, bs := decimalCoefficient(right)
	scale := max(as, bs)
	switch operator {
	case "+", "-", "%":
		a.Mul(a, decimalPower(scale-as))
		b.Mul(b, decimalPower(scale-bs))
		switch operator {
		case "+":
			a.Add(a, b)
		case "-":
			a.Sub(a, b)
		case "%":
			if b.Sign() == 0 {
				return "", errors.New("division by zero")
			}
			a.Rem(a, b)
		}
	case "*":
		a.Mul(a, b)
		scale = as + bs
		if scale > MaxDecimalScale {
			a = roundCoefficient(a, scale-MaxDecimalScale, false)
			scale = MaxDecimalScale
		}
	case "/":
		if b.Sign() == 0 {
			return "", errors.New("division by zero")
		}
		scale = min(MaxDecimalScale, as+4)
		a.Mul(a, decimalPower(scale+bs-as))
		quotient, remainder := new(big.Int), new(big.Int)
		quotient.QuoRem(a, b, remainder)
		if new(big.Int).Lsh(new(big.Int).Abs(remainder), 1).Cmp(new(big.Int).Abs(b)) >= 0 {
			if a.Sign()*b.Sign() < 0 {
				quotient.Sub(quotient, big.NewInt(1))
			} else {
				quotient.Add(quotient, big.NewInt(1))
			}
		}
		a = quotient
	default:
		return "", fmt.Errorf("unsupported decimal operator %s", operator)
	}
	return decimalFromCoefficient(a, scale)
}

// CompareDecimal compares normalized decimal text without allocating big.Ints.
func CompareDecimal(left, right Decimal) int {
	ls, rs := string(left), string(right)
	ln, rn := strings.HasPrefix(ls, "-"), strings.HasPrefix(rs, "-")
	if left.IsZero() {
		ln = false
	}
	if right.IsZero() {
		rn = false
	}
	if ln != rn {
		if ln {
			return -1
		}
		return 1
	}
	ls = strings.TrimPrefix(ls, "-")
	rs = strings.TrimPrefix(rs, "-")
	li, lf, _ := strings.Cut(ls, ".")
	ri, rf, _ := strings.Cut(rs, ".")
	li = strings.TrimLeft(li, "0")
	ri = strings.TrimLeft(ri, "0")
	result := 0
	if len(li) < len(ri) {
		result = -1
	} else if len(li) > len(ri) {
		result = 1
	} else {
		result = strings.Compare(li, ri)
	}
	if result == 0 {
		for i := 0; i < max(len(lf), len(rf)); i++ {
			l, r := byte('0'), byte('0')
			if i < len(lf) {
				l = lf[i]
			}
			if i < len(rf) {
				r = rf[i]
			}
			if l < r {
				result = -1
				break
			}
			if l > r {
				result = 1
				break
			}
		}
	}
	if ln {
		return -result
	}
	return result
}

// DecimalKey removes insignificant fractional zeros for unique/index equality.
func DecimalKey(d Decimal) string {
	s := string(d)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimSuffix(s, ".")
	}
	if s == "-0" {
		s = "0"
	}
	return s
}

func DecimalColumnSpec(column Column) (precision, scale int, unsigned bool, err error) {
	s := strings.ToLower(strings.TrimSpace(column.SQLType))
	precision = 10
	if s == "" {
		return precision, 0, false, nil
	}
	end := strings.IndexAny(s, "( ")
	if end < 0 {
		end = len(s)
	}
	switch s[:end] {
	case "decimal", "numeric", "dec", "fixed":
	default:
		return 0, 0, false, fmt.Errorf("invalid DECIMAL declaration %q", s)
	}
	rest := strings.TrimSpace(s[end:])
	if strings.HasPrefix(rest, "(") {
		close := strings.IndexByte(rest, ')')
		if close < 0 {
			return 0, 0, false, ErrDecimalRange
		}
		parts := strings.Split(rest[1:close], ",")
		if len(parts) < 1 || len(parts) > 2 {
			return 0, 0, false, ErrDecimalRange
		}
		precision, err = strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil {
			return 0, 0, false, ErrDecimalRange
		}
		if len(parts) == 2 {
			scale, err = strconv.Atoi(strings.TrimSpace(parts[1]))
			if err != nil {
				return 0, 0, false, ErrDecimalRange
			}
		}
		rest = strings.TrimSpace(rest[close+1:])
	}
	for _, word := range strings.Fields(rest) {
		switch word {
		case "unsigned":
			unsigned = true
		case "zerofill":
			unsigned = true
		default:
			return 0, 0, false, fmt.Errorf("invalid DECIMAL modifier %q", word)
		}
	}
	if precision < 1 || precision > MaxDecimalPrecision || scale < 0 || scale > MaxDecimalScale || scale > precision {
		return 0, 0, false, ErrDecimalRange
	}
	return
}
func DecimalColumnValue(column Column, raw any) (Value, error) {
	p, s, unsigned, err := DecimalColumnSpec(column)
	if err != nil {
		return Value{}, err
	}
	if raw == nil {
		return NullValue(TypeDecimal), nil
	}
	d, err := DecimalFrom(raw)
	if err != nil {
		return Value{}, err
	}
	d, err = d.Quantize(p, s)
	if err != nil {
		return Value{}, fmt.Errorf("%w for column %q", err, column.Name)
	}
	if unsigned && strings.HasPrefix(string(d), "-") {
		return Value{}, fmt.Errorf("%w for unsigned column %q", ErrDecimalRange, column.Name)
	}
	return Value{Type: TypeDecimal, Text: string(d)}, nil
}

// NormalizeDecimalSnapshot upgrades legacy float-backed DECIMAL columns only in
// a copied snapshot. Historical floating-point precision cannot be recovered.
func NormalizeDecimalSnapshot(source TableSnapshot) (TableSnapshot, error) {
	var legacy []int
	for i, c := range source.Columns {
		if c.Type == TypeDouble && isDecimalDeclaration(c.SQLType) {
			legacy = append(legacy, i)
		}
	}
	if len(legacy) == 0 {
		return source, nil
	}
	source.Columns = append([]Column(nil), source.Columns...)
	for _, i := range legacy {
		c := &source.Columns[i]
		c.Type = TypeDecimal
		if c.HasDefault {
			v, err := DecimalColumnValue(*c, c.Default.Interface())
			if err != nil {
				return TableSnapshot{}, err
			}
			c.Default = v
		}
	}
	source.Rows = cloneRows(source.Rows)
	for _, row := range source.Rows {
		for _, i := range legacy {
			if i >= len(row) {
				return TableSnapshot{}, ErrColumnCount
			}
			v, err := DecimalColumnValue(source.Columns[i], row[i].Interface())
			if err != nil {
				return TableSnapshot{}, err
			}
			row[i] = v
		}
	}
	return source, nil
}
func isDecimalDeclaration(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.IndexAny(s, "( "); i >= 0 {
		s = s[:i]
	}
	return s == "decimal" || s == "numeric" || s == "dec" || s == "fixed"
}
