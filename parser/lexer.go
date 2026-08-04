package parser

import (
	"fmt"
	"strings"
	"unicode"
)

type TokenKind uint8

const (
	TokenEOF TokenKind = iota
	TokenIdentifier
	TokenString
	TokenNumber
	TokenComma
	TokenLParen
	TokenRParen
	TokenSemicolon
	TokenStar
	TokenOperator
	TokenDot
	TokenAt
)

type Token struct {
	Kind     TokenKind
	Text     string
	Position int
}

func Lex(sql string) ([]Token, error) {
	runes := []rune(sql)
	tokens := make([]Token, 0, len(runes)/2)
	for i := 0; i < len(runes); {
		r := runes[i]
		if unicode.IsSpace(r) {
			i++
			continue
		}
		if r == '#' {
			for i < len(runes) && runes[i] != '\n' {
				i++
			}
			continue
		}
		if r == '-' && i+1 < len(runes) && runes[i+1] == '-' {
			i += 2
			for i < len(runes) && runes[i] != '\n' {
				i++
			}
			continue
		}
		if r == '/' && i+1 < len(runes) && runes[i+1] == '*' {
			start := i
			i += 2
			for i+1 < len(runes) && !(runes[i] == '*' && runes[i+1] == '/') {
				i++
			}
			if i+1 >= len(runes) {
				return nil, fmt.Errorf("unterminated comment at %d", start)
			}
			i += 2
			continue
		}
		start := i
		switch r {
		case ',':
			tokens = append(tokens, Token{TokenComma, ",", start})
			i++
		case '(':
			tokens = append(tokens, Token{TokenLParen, "(", start})
			i++
		case ')':
			tokens = append(tokens, Token{TokenRParen, ")", start})
			i++
		case ';':
			tokens = append(tokens, Token{TokenSemicolon, ";", start})
			i++
		case '*':
			tokens = append(tokens, Token{TokenStar, "*", start})
			i++
		case '.':
			tokens = append(tokens, Token{TokenDot, ".", start})
			i++
		case '@':
			tokens = append(tokens, Token{TokenAt, "@", start})
			i++
		case '=', '!', '>', '<', '+', '/', '%':
			if r == '<' && i+2 < len(runes) && runes[i+1] == '=' && runes[i+2] == '>' {
				tokens = append(tokens, Token{TokenOperator, "<=>", start})
				i += 3
				continue
			}
			i++
			if i < len(runes) && (runes[i] == '=' || (r == '<' && runes[i] == '>')) {
				i++
			}
			tokens = append(tokens, Token{TokenOperator, string(runes[start:i]), start})
		case '-':
			if i+1 < len(runes) && unicode.IsDigit(runes[i+1]) && minusStartsNumber(tokens) {
				i++
				for i < len(runes) && (unicode.IsDigit(runes[i]) || runes[i] == '.') {
					i++
				}
				tokens = append(tokens, Token{TokenNumber, string(runes[start:i]), start})
			} else {
				tokens = append(tokens, Token{TokenOperator, "-", start})
				i++
			}
		case '\'', '"':
			quote := r
			i++
			var value strings.Builder
			closed := false
			for i < len(runes) {
				if runes[i] == quote {
					if i+1 < len(runes) && runes[i+1] == quote {
						value.WriteRune(quote)
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				if runes[i] == '\\' && i+1 < len(runes) {
					i++
					value.WriteRune(runes[i])
					i++
					continue
				}
				value.WriteRune(runes[i])
				i++
			}
			if !closed {
				return nil, fmt.Errorf("unterminated string at %d", start)
			}
			tokens = append(tokens, Token{TokenString, value.String(), start})
		case '`':
			i++
			var value strings.Builder
			closed := false
			for i < len(runes) {
				if runes[i] == '`' {
					i++
					closed = true
					break
				}
				value.WriteRune(runes[i])
				i++
			}
			if !closed {
				return nil, fmt.Errorf("unterminated identifier at %d", start)
			}
			tokens = append(tokens, Token{TokenIdentifier, value.String(), start})
		default:
			if unicode.IsDigit(r) || (r == '-' && i+1 < len(runes) && unicode.IsDigit(runes[i+1])) {
				i++
				for i < len(runes) && unicode.IsDigit(runes[i]) {
					i++
				}
				if unicode.IsDigit(r) && i < len(runes) && (unicode.IsLetter(runes[i]) || runes[i] == '_' || runes[i] == '$') {
					for i < len(runes) && (unicode.IsLetter(runes[i]) || unicode.IsDigit(runes[i]) || runes[i] == '_' || runes[i] == '$') {
						i++
					}
					tokens = append(tokens, Token{TokenIdentifier, string(runes[start:i]), start})
					continue
				}
				for i < len(runes) && (unicode.IsDigit(runes[i]) || runes[i] == '.') {
					i++
				}
				tokens = append(tokens, Token{TokenNumber, string(runes[start:i]), start})
				continue
			}
			if unicode.IsLetter(r) || r == '_' || r == '$' {
				i++
				for i < len(runes) && (unicode.IsLetter(runes[i]) || unicode.IsDigit(runes[i]) || runes[i] == '_' || runes[i] == '$') {
					i++
				}
				tokens = append(tokens, Token{TokenIdentifier, string(runes[start:i]), start})
				continue
			}
			return nil, fmt.Errorf("unexpected character %q at %d", r, start)
		}
	}
	tokens = append(tokens, Token{Kind: TokenEOF, Position: len(runes)})
	return tokens, nil
}

func minusStartsNumber(tokens []Token) bool {
	if len(tokens) == 0 {
		return true
	}
	previous := tokens[len(tokens)-1]
	if previous.Kind == TokenOperator || previous.Kind == TokenLParen || previous.Kind == TokenComma {
		return true
	}
	if previous.Kind != TokenIdentifier {
		return false
	}
	switch strings.ToUpper(previous.Text) {
	case "SELECT", "VALUES", "SET", "WHERE", "AND", "OR", "NOT", "IN", "BETWEEN", "WHEN", "THEN", "ELSE", "BY", "HAVING", "LIMIT", "OFFSET":
		return true
	default:
		return false
	}
}
