package mysql

import (
	"fmt"
	"os"
	"strings"

	"gbaselite/executor"
	"gbaselite/parser"
)

func RestoreSQL(engine *executor.Engine, path string) (int, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	statements, err := SplitSQLStatements(string(content))
	if err != nil {
		return 0, err
	}
	session := &executor.Session{}
	if _, err := engine.Execute(session, "BEGIN"); err != nil {
		return 0, err
	}
	executed := 0
	for index, statement := range statements {
		if skipRestoreStatement(statement) {
			continue
		}
		if _, err := engine.Execute(session, statement); err != nil {
			_, _ = engine.Execute(session, "ROLLBACK")
			return executed, fmt.Errorf("restore statement %d failed: %w", index+1, err)
		}
		executed++
	}
	if _, err := engine.Execute(session, "COMMIT"); err != nil {
		return executed, err
	}
	return executed, nil
}

func skipRestoreStatement(statement string) bool {
	normalized := strings.ToUpper(strings.TrimSpace(statement))
	return normalized == "" || strings.HasPrefix(normalized, "SET ") ||
		strings.HasPrefix(normalized, "LOCK TABLES ") || normalized == "UNLOCK TABLES" ||
		strings.HasPrefix(normalized, "ALTER TABLE ") && (strings.Contains(normalized, " DISABLE KEYS") || strings.Contains(normalized, " ENABLE KEYS")) ||
		normalized == "START TRANSACTION" || normalized == "BEGIN" || normalized == "COMMIT" || normalized == "ROLLBACK"
}

func SplitSQLStatements(script string) ([]string, error) {
	expanded, err := parser.ExpandMySQLExecutableComments(script)
	if err != nil {
		return nil, err
	}
	script = expanded
	var statements []string
	var current strings.Builder
	var quote rune
	escaped := false
	lineComment := false
	blockComment := false
	runes := []rune(script)
	for index := 0; index < len(runes); index++ {
		character := runes[index]
		if lineComment {
			if character == '\n' {
				lineComment = false
				current.WriteRune(' ')
			}
			continue
		}
		if blockComment {
			if character == '*' && index+1 < len(runes) && runes[index+1] == '/' {
				blockComment = false
				index++
				current.WriteRune(' ')
			}
			continue
		}
		if quote != 0 {
			current.WriteRune(character)
			if escaped {
				escaped = false
				continue
			}
			if character == '\\' && quote != '`' {
				escaped = true
				continue
			}
			if character == quote {
				if index+1 < len(runes) && runes[index+1] == quote {
					current.WriteRune(runes[index+1])
					index++
					continue
				}
				quote = 0
			}
			continue
		}
		switch {
		case character == '\'' || character == '"' || character == '`':
			quote = character
			current.WriteRune(character)
		case character == '#':
			lineComment = true
		case character == '-' && index+1 < len(runes) && runes[index+1] == '-' && (index+2 == len(runes) || runes[index+2] == ' ' || runes[index+2] == '\t' || runes[index+2] == '\r' || runes[index+2] == '\n'):
			lineComment = true
			index++
		case character == '/' && index+1 < len(runes) && runes[index+1] == '*':
			blockComment = true
			index++
		case character == ';':
			statement := strings.TrimSpace(current.String())
			if statement != "" {
				statements = append(statements, statement)
			}
			current.Reset()
		default:
			current.WriteRune(character)
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quoted value in SQL backup")
	}
	if blockComment {
		return nil, fmt.Errorf("unterminated block comment in SQL backup")
	}
	if statement := strings.TrimSpace(current.String()); statement != "" {
		statements = append(statements, statement)
	}
	return statements, nil
}
