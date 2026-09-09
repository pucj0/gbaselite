package executor

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

type jsonPathLeg struct {
	key             string
	index           uint64
	array, wildcard bool
}
type jsonPath struct {
	legs     []jsonPathLeg
	multiple bool
}

// Parse a bounded subset of MySQL paths. Unsupported syntax fails explicitly.
func parseJSONPath(raw string, allowWildcard bool) (jsonPath, error) {
	p := jsonPath{}
	fail := func() (jsonPath, error) { return jsonPath{}, jsonError(3143, "invalid or unsupported JSON path: "+raw) }
	if len(raw) > 4096 || !utf8.ValidString(raw) {
		return fail()
	}
	s := strings.TrimSpace(raw)
	if len(s) == 0 || s[0] != '$' {
		return fail()
	}
	s = s[1:]
	for len(strings.TrimSpace(s)) > 0 {
		s = strings.TrimSpace(s)
		leg := jsonPathLeg{}
		switch s[0] {
		case '.':
			s = strings.TrimLeftFunc(s[1:], unicode.IsSpace)
			if s == "" {
				return fail()
			}
			if s[0] == '*' {
				leg.wildcard = true
				s = s[1:]
			} else if s[0] == '"' {
				end, escaped := 1, false
				for ; end < len(s); end++ {
					if escaped {
						escaped = false
						continue
					}
					if s[end] == '\\' {
						escaped = true
						continue
					}
					if s[end] == '"' {
						break
					}
				}
				if end == len(s) || json.Unmarshal([]byte(s[:end+1]), &leg.key) != nil {
					return fail()
				}
				s = s[end+1:]
			} else {
				end := 0
				for end < len(s) {
					r, size := utf8.DecodeRuneInString(s[end:])
					valid := unicode.IsLetter(r) || r == '_' || r == '$' || end > 0 && (unicode.IsDigit(r) || unicode.IsMark(r) || r == '\u200c' || r == '\u200d')
					if !valid {
						break
					}
					end += size
				}
				if end == 0 {
					return fail()
				}
				leg.key, s = s[:end], s[end:]
			}
		case '[':
			end := strings.IndexByte(s, ']')
			if end < 0 {
				return fail()
			}
			token := strings.TrimSpace(s[1:end])
			leg.array = true
			if token == "*" {
				leg.wildcard = true
			} else {
				if token == "" {
					return fail()
				}
				for _, c := range token {
					if c < '0' || c > '9' {
						return fail()
					}
				}
				index, err := strconv.ParseUint(token, 10, 64)
				if err != nil {
					return fail()
				}
				leg.index = index
			}
			s = s[end+1:]
		default:
			return fail()
		}
		if leg.wildcard {
			if !allowWildcard {
				return jsonPath{}, jsonError(3149, "wildcards are not supported by this JSON function")
			}
			p.multiple = true
		}
		p.legs = append(p.legs, leg)
		if len(p.legs) > 100 {
			return fail()
		}
	}
	return p, nil
}

func jsonObjectKeys(object map[string]any) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// The callback returns false to stop searching (used by CONTAINS_PATH).
func visitJSONPath(value any, legs []jsonPathLeg, emit func(any) bool) bool {
	if len(legs) == 0 {
		return emit(value)
	}
	leg := legs[0]
	if leg.array {
		if values, ok := value.([]any); ok {
			if leg.wildcard {
				for _, child := range values {
					if !visitJSONPath(child, legs[1:], emit) {
						return false
					}
				}
			} else if leg.index < uint64(len(values)) {
				return visitJSONPath(values[int(leg.index)], legs[1:], emit)
			}
		} else if !leg.wildcard && leg.index == 0 {
			return visitJSONPath(value, legs[1:], emit)
		}
	} else if object, ok := value.(map[string]any); ok {
		if leg.wildcard {
			for _, key := range jsonObjectKeys(object) {
				if !visitJSONPath(object[key], legs[1:], emit) {
					return false
				}
			}
		} else if child, exists := object[leg.key]; exists {
			return visitJSONPath(child, legs[1:], emit)
		}
	}
	return true
}
func (p jsonPath) matches(value any) []any {
	var result []any
	visitJSONPath(value, p.legs, func(v any) bool { result = append(result, v); return true })
	return result
}
func (p jsonPath) exists(value any) bool {
	found := false
	visitJSONPath(value, p.legs, func(any) bool { found = true; return false })
	return found
}

// Each call owns its parsed document. Mutations never touch stored/shared values.
func modifyJSONPath(value any, legs []jsonPathLeg, mode string, replacement any) any {
	if len(legs) == 0 {
		if mode != "JSON_INSERT" && mode != "JSON_REMOVE" {
			return replacement
		}
		return value
	}
	leg, leaf := legs[0], len(legs) == 1
	if !leg.array {
		object, ok := value.(map[string]any)
		if !ok {
			return value
		}
		child, exists := object[leg.key]
		if leaf {
			if mode == "JSON_REMOVE" {
				delete(object, leg.key)
			} else if exists && mode != "JSON_INSERT" || !exists && mode != "JSON_REPLACE" {
				object[leg.key] = replacement
			}
		} else if exists {
			object[leg.key] = modifyJSONPath(child, legs[1:], mode, replacement)
		}
		return object
	}
	values, array := value.([]any)
	if !array {
		if leg.index == 0 {
			if mode == "JSON_REMOVE" && leaf {
				return value
			}
			return modifyJSONPath(value, legs[1:], mode, replacement)
		}
		if leaf && (mode == "JSON_SET" || mode == "JSON_INSERT") {
			return []any{value, replacement}
		}
		return value
	}
	if leg.index >= uint64(len(values)) {
		// Appending past the end adds one element, never a sparse allocation.
		if leaf && (mode == "JSON_SET" || mode == "JSON_INSERT") {
			return append(values, replacement)
		}
		return value
	}
	index := int(leg.index)
	if leaf && mode == "JSON_REMOVE" {
		copy(values[index:], values[index+1:])
		values[len(values)-1] = nil
		return values[:len(values)-1]
	}
	values[index] = modifyJSONPath(values[index], legs[1:], mode, replacement)
	return values
}
