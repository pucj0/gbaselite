package executor

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gbaselite/parser"
	"gbaselite/storage"
)

// jsonDocument keeps JSON provenance inside expressions without retaining a parsed tree.
// Storage and the wire protocol continue to use text.
type jsonDocument string

func (d jsonDocument) String() string { return string(d) }

// JSONFunctionError carries the corresponding MySQL error number to the server.
type JSONFunctionError struct {
	Code    uint16
	Message string
}

func (e *JSONFunctionError) Error() string        { return e.Message }
func jsonError(code uint16, message string) error { return &JSONFunctionError{code, message} }

const maxJSONDepth = 100

func jsonFunctionType(name string) (storage.DataType, bool) {
	switch name {
	case "JSON_OBJECT", "JSON_ARRAY", "JSON_EXTRACT", "JSON_SET", "JSON_INSERT", "JSON_REPLACE", "JSON_REMOVE", "JSON_KEYS":
		return storage.TypeText, true
	case "JSON_VALID", "JSON_LENGTH", "JSON_DEPTH", "JSON_CONTAINS_PATH":
		return storage.TypeBigInt, true
	case "JSON_QUOTE", "JSON_UNQUOTE", "JSON_TYPE":
		return storage.TypeVarchar, true
	}
	return "", false
}

func jsonArgumentText(v any) string {
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return fmt.Sprint(v)
}

func validJSONDocument(s string) error {
	if !utf8.ValidString(s) || !json.Valid([]byte(s)) {
		return jsonError(3141, "invalid JSON document")
	}
	depth, quoted, escaped := 0, false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quoted {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				quoted = false
			}
			continue
		}
		switch c {
		case '"':
			quoted = true
		case '{', '[':
			depth++
			if depth > maxJSONDepth {
				return jsonError(3157, "JSON nesting exceeds 100 levels")
			}
		case '}', ']':
			depth--
		}
	}
	return nil
}

func decodeJSON(v any) (any, error) {
	s := jsonArgumentText(v)
	if err := validJSONDocument(s); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(strings.NewReader(s))
	decoder.UseNumber() // Never round 64-bit JSON integers through float64.
	var result any
	if err := decoder.Decode(&result); err != nil {
		return nil, jsonError(3141, "invalid JSON document")
	}
	return result, nil
}

func encodeJSON(v any) (jsonDocument, error) {
	encoded, err := json.Marshal(v)
	if err != nil {
		return "", jsonError(3141, "cannot encode JSON value: "+err.Error())
	}
	result := jsonDocument(encoded)
	if err := validJSONDocument(string(result)); err != nil {
		return "", err
	}
	return result, nil
}

func jsonConstructorValue(v any) (any, error) {
	switch x := v.(type) {
	case collatedText:
		return x.Text, nil
	case jsonDocument:
		if err := validJSONDocument(string(x)); err != nil {
			return nil, err
		}
		return json.RawMessage(x), nil
	case []byte:
		return string(x), nil
	case time.Time:
		return x.Format("2006-01-02 15:04:05.999999"), nil
	case string:
		if !utf8.ValidString(x) {
			return nil, jsonError(3141, "invalid UTF-8 JSON string")
		}
	}
	return v, nil
}

func jsonColumnValue(column storage.Column, value storage.Value) any {
	if !value.Null && strings.EqualFold(strings.TrimSpace(column.SQLType), "JSON") {
		return jsonDocument(value.Text)
	}
	if !value.Null && column.Collation != "" && isTextColumn(column.Type) {
		return collatedText{Text: value.Text, Collation: column.Collation}
	}
	return value.Interface()
}

func evaluateJSONFunction(name string, args []any) (any, error) {
	n := len(args)
	validCount := false
	switch name {
	case "JSON_OBJECT":
		validCount = n%2 == 0
	case "JSON_ARRAY":
		validCount = true
	case "JSON_QUOTE", "JSON_UNQUOTE", "JSON_VALID", "JSON_TYPE", "JSON_DEPTH":
		validCount = n == 1
	case "JSON_LENGTH", "JSON_KEYS":
		validCount = n == 1 || n == 2
	case "JSON_EXTRACT", "JSON_REMOVE":
		validCount = n >= 2
	case "JSON_SET", "JSON_INSERT", "JSON_REPLACE":
		validCount = n >= 3 && n%2 == 1
	case "JSON_CONTAINS_PATH":
		validCount = n >= 3
	}
	if !validCount {
		return nil, jsonError(1582, "incorrect argument count for "+name)
	}
	switch name {
	case "JSON_OBJECT":
		object := make(map[string]any, n/2)
		for i := 0; i < n; i += 2 {
			if args[i] == nil {
				return nil, jsonError(3158, "JSON_OBJECT key cannot be NULL")
			}
			key := jsonArgumentText(args[i])
			if !utf8.ValidString(key) {
				return nil, jsonError(3141, "invalid UTF-8 JSON key")
			}
			value, err := jsonConstructorValue(args[i+1])
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		return encodeJSON(object)
	case "JSON_ARRAY":
		values := make([]any, n)
		for i, v := range args {
			var err error
			values[i], err = jsonConstructorValue(v)
			if err != nil {
				return nil, err
			}
		}
		return encodeJSON(values)
	}
	if args[0] == nil {
		return nil, nil
	}
	switch name {
	case "JSON_VALID":
		if validJSONDocument(jsonArgumentText(args[0])) != nil {
			return int64(0), nil
		}
		return int64(1), nil
	case "JSON_QUOTE":
		value := jsonArgumentText(args[0])
		if !utf8.ValidString(value) {
			return nil, jsonError(3141, "invalid UTF-8 JSON string")
		}
		result, err := encodeJSON(value)
		return string(result), err
	case "JSON_UNQUOTE":
		s := jsonArgumentText(args[0])
		if strings.HasPrefix(s, "\"") {
			var value string
			if err := json.Unmarshal([]byte(s), &value); err != nil || !utf8.ValidString(s) {
				return nil, jsonError(3141, "invalid JSON string in JSON_UNQUOTE")
			}
			return value, nil
		}
		return s, nil
	}
	// NULL paths propagate, but NULL replacement values become JSON null.
	pathStart, pathStep := 1, 1
	if name == "JSON_SET" || name == "JSON_INSERT" || name == "JSON_REPLACE" {
		pathStep = 2
	}
	if name == "JSON_CONTAINS_PATH" {
		pathStart = 2
		if args[1] == nil {
			return nil, nil
		}
	}
	for i := pathStart; i < n; i += pathStep {
		if args[i] == nil {
			return nil, nil
		}
	}
	doc, err := decodeJSON(args[0])
	if err != nil {
		return nil, err
	}
	switch name {
	case "JSON_TYPE":
		switch v := doc.(type) {
		case nil:
			return "NULL", nil
		case bool:
			return "BOOLEAN", nil
		case string:
			return "STRING", nil
		case []any:
			return "ARRAY", nil
		case map[string]any:
			return "OBJECT", nil
		case json.Number:
			if strings.ContainsAny(string(v), ".eE") {
				return "DOUBLE", nil
			}
			if _, err := strconv.ParseInt(string(v), 10, 64); err == nil {
				return "INTEGER", nil
			}
			if _, err := strconv.ParseUint(string(v), 10, 64); err == nil {
				return "UNSIGNED INTEGER", nil
			}
			return "DOUBLE", nil
		}
	case "JSON_DEPTH":
		return int64(jsonValueDepth(doc)), nil
	case "JSON_LENGTH", "JSON_KEYS":
		if n == 2 {
			path, err := parseJSONPath(jsonArgumentText(args[1]), false)
			if err != nil {
				return nil, err
			}
			matches := path.matches(doc)
			if len(matches) == 0 {
				return nil, nil
			}
			doc = matches[0]
		}
		if name == "JSON_KEYS" {
			object, ok := doc.(map[string]any)
			if !ok {
				return nil, nil
			}
			return encodeJSON(jsonObjectKeys(object))
		}
		switch value := doc.(type) {
		case []any:
			return int64(len(value)), nil
		case map[string]any:
			return int64(len(value)), nil
		}
		return int64(1), nil
	case "JSON_EXTRACT":
		var matches []any
		multiple := n > 2
		for _, raw := range args[1:] {
			path, err := parseJSONPath(jsonArgumentText(raw), true)
			if err != nil {
				return nil, err
			}
			multiple = multiple || path.multiple
			matches = append(matches, path.matches(doc)...)
		}
		if len(matches) == 0 {
			return nil, nil
		}
		if multiple {
			return encodeJSON(matches)
		}
		return encodeJSON(matches[0])
	case "JSON_CONTAINS_PATH":
		mode := strings.ToLower(jsonArgumentText(args[1]))
		if mode != "one" && mode != "all" {
			return nil, jsonError(3154, "JSON_CONTAINS_PATH requires 'one' or 'all'")
		}
		found, all := false, true
		for _, raw := range args[2:] {
			path, err := parseJSONPath(jsonArgumentText(raw), true)
			if err != nil {
				return nil, err
			}
			exists := path.exists(doc)
			found, all = found || exists, all && exists
		}
		if mode == "one" && found || mode == "all" && all {
			return int64(1), nil
		}
		return int64(0), nil
	case "JSON_SET", "JSON_INSERT", "JSON_REPLACE", "JSON_REMOVE":
		for i := 1; i < n; i += pathStep {
			path, err := parseJSONPath(jsonArgumentText(args[i]), false)
			if err != nil {
				return nil, err
			}
			if name == "JSON_REMOVE" && len(path.legs) == 0 {
				return nil, jsonError(3153, "JSON_REMOVE cannot remove the root")
			}
			var replacement any
			if name != "JSON_REMOVE" {
				replacement, err = jsonConstructorValue(args[i+1])
				if err != nil {
					return nil, err
				}
				// Mutation traversal needs structured values for subsequent path/value pairs.
				if raw, ok := replacement.(json.RawMessage); ok {
					replacement, err = decodeJSON(string(raw))
					if err != nil {
						return nil, err
					}
				}
			}
			doc = modifyJSONPath(doc, path.legs, name, replacement)
		}
		return encodeJSON(doc)
	}
	return nil, fmt.Errorf("unsupported JSON function %s", name)
}

func jsonValueDepth(v any) int {
	depth := 1
	visit := func(child any) {
		if d := 1 + jsonValueDepth(child); d > depth {
			depth = d
		}
	}
	switch value := v.(type) {
	case []any:
		for _, child := range value {
			visit(child)
		}
	case map[string]any:
		for _, child := range value {
			visit(child)
		}
	}
	return depth
}

func expressionReturnsJSON(expr parser.Expr, table *storage.Table) bool {
	switch v := expr.(type) {
	case parser.FunctionExpr:
		typ, ok := jsonFunctionType(strings.ToUpper(v.Name))
		return ok && typ == storage.TypeText
	case parser.Identifier:
		if table != nil {
			if index, ok := queryColumnIndex(table, v.Name); ok {
				return strings.EqualFold(strings.TrimSpace(table.ColumnsView()[index].SQLType), "JSON")
			}
		}
	}
	return false
}
