package server

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type preparedStatement struct {
	query          string
	parameterCount int
	parameterTypes []uint16
}

func countPlaceholders(query string) int {
	count := 0
	walkSQL(query, func(byte) string {
		count++
		return "?"
	})
	return count
}

func bindPreparedQuery(query string, values []any) (string, error) {
	position := 0
	bound := walkSQL(query, func(byte) string {
		if position >= len(values) {
			position++
			return "?"
		}
		value := sqlLiteral(values[position])
		position++
		return value
	})
	if position != len(values) {
		return "", fmt.Errorf("prepared statement expects %d parameters, got %d", position, len(values))
	}
	return bound, nil
}

func walkSQL(query string, replace func(byte) string) string {
	var result strings.Builder
	result.Grow(len(query))
	for position := 0; position < len(query); {
		current := query[position]
		switch current {
		case '\'', '"', '`':
			quote := current
			result.WriteByte(current)
			position++
			for position < len(query) {
				current = query[position]
				result.WriteByte(current)
				position++
				if current == '\\' && position < len(query) {
					result.WriteByte(query[position])
					position++
					continue
				}
				if current == quote {
					if position < len(query) && query[position] == quote {
						result.WriteByte(query[position])
						position++
						continue
					}
					break
				}
			}
		case '#':
			for position < len(query) {
				current = query[position]
				result.WriteByte(current)
				position++
				if current == '\n' {
					break
				}
			}
		case '-':
			if position+1 < len(query) && query[position+1] == '-' {
				for position < len(query) {
					current = query[position]
					result.WriteByte(current)
					position++
					if current == '\n' {
						break
					}
				}
			} else {
				result.WriteByte(current)
				position++
			}
		case '/':
			if position+1 < len(query) && query[position+1] == '*' {
				for position < len(query) {
					current = query[position]
					result.WriteByte(current)
					position++
					if current == '*' && position < len(query) && query[position] == '/' {
						result.WriteByte('/')
						position++
						break
					}
				}
			} else {
				result.WriteByte(current)
				position++
			}
		case '?':
			result.WriteString(replace(current))
			position++
		default:
			result.WriteByte(current)
			position++
		}
	}
	return result.String()
}

func sqlLiteral(value any) string {
	switch typed := value.(type) {
	case nil:
		return "NULL"
	case bool:
		if typed {
			return "TRUE"
		}
		return "FALSE"
	case int:
		return strconv.FormatInt(int64(typed), 10)
	case int8:
		return strconv.FormatInt(int64(typed), 10)
	case int16:
		return strconv.FormatInt(int64(typed), 10)
	case int32:
		return strconv.FormatInt(int64(typed), 10)
	case int64:
		return strconv.FormatInt(typed, 10)
	case uint:
		return strconv.FormatUint(uint64(typed), 10)
	case uint8:
		return strconv.FormatUint(uint64(typed), 10)
	case uint16:
		return strconv.FormatUint(uint64(typed), 10)
	case uint32:
		return strconv.FormatUint(uint64(typed), 10)
	case uint64:
		return strconv.FormatUint(typed, 10)
	case float32:
		return strconv.FormatFloat(float64(typed), 'g', -1, 32)
	case float64:
		return strconv.FormatFloat(typed, 'g', -1, 64)
	case time.Time:
		return quoteSQLString(typed.Format("2006-01-02 15:04:05.999999"))
	case []byte:
		return quoteSQLString(string(typed))
	case string:
		return quoteSQLString(typed)
	default:
		return quoteSQLString(fmt.Sprint(typed))
	}
}

func quoteSQLString(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "'", "''")
	return "'" + value + "'"
}
