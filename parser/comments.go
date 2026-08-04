package parser

import (
	"fmt"
	"strings"
	"unicode"
)

// ExpandMySQLExecutableComments unwraps MySQL's /*!version SQL */ form while
// leaving ordinary comments intact for the lexer to ignore.
func ExpandMySQLExecutableComments(sql string) (string, error) {
	runes := []rune(sql)
	var expanded strings.Builder
	expanded.Grow(len(sql))
	var quote rune
	escaped := false

	for index := 0; index < len(runes); {
		character := runes[index]
		if quote != 0 {
			expanded.WriteRune(character)
			index++
			if escaped {
				escaped = false
				continue
			}
			if character == '\\' && quote != '`' {
				escaped = true
				continue
			}
			if character == quote {
				if index < len(runes) && runes[index] == quote {
					expanded.WriteRune(runes[index])
					index++
					continue
				}
				quote = 0
			}
			continue
		}

		if character == '\'' || character == '"' || character == '`' {
			quote = character
			expanded.WriteRune(character)
			index++
			continue
		}
		if character == '#' || (character == '-' && index+1 < len(runes) && runes[index+1] == '-' &&
			(index+2 == len(runes) || unicode.IsSpace(runes[index+2]))) {
			for index < len(runes) {
				expanded.WriteRune(runes[index])
				if runes[index] == '\n' {
					index++
					break
				}
				index++
			}
			continue
		}
		if character != '/' || index+1 >= len(runes) || runes[index+1] != '*' {
			expanded.WriteRune(character)
			index++
			continue
		}

		start := index
		end := index + 2
		for end+1 < len(runes) && !(runes[end] == '*' && runes[end+1] == '/') {
			end++
		}
		if end+1 >= len(runes) {
			return "", fmt.Errorf("unterminated comment at %d", start)
		}
		if index+2 >= len(runes) || runes[index+2] != '!' {
			expanded.WriteString(string(runes[index : end+2]))
			index = end + 2
			continue
		}

		contentStart := index + 3
		for contentStart < end && unicode.IsDigit(runes[contentStart]) {
			contentStart++
		}
		for contentStart < end && unicode.IsSpace(runes[contentStart]) {
			contentStart++
		}
		expanded.WriteRune(' ')
		expanded.WriteString(string(runes[contentStart:end]))
		expanded.WriteRune(' ')
		index = end + 2
	}
	return expanded.String(), nil
}
