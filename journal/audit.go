package journal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
)

type AuditEvent struct {
	Timestamp    time.Time `json:"timestamp"`
	ConnectionID uint32    `json:"connection_id"`
	Username     string    `json:"username,omitempty"`
	RemoteIP     string    `json:"remote_ip,omitempty"`
	RemotePort   string    `json:"remote_port,omitempty"`
	Database     string    `json:"database,omitempty"`
	Operation    string    `json:"operation"`
	Result       string    `json:"result"`
	AffectedRows uint64    `json:"affected_rows"`
	DurationMS   float64   `json:"duration_ms"`
	SQL          string    `json:"sql,omitempty"`
	ErrorCode    uint16    `json:"error_code,omitempty"`
}

type AuditLog struct {
	log *retainedJSONL
	mu  sync.Mutex
}

func OpenAudit(path string, retentionDays int) (*AuditLog, error) {
	retained, err := openRetainedJSONL(path, retentionDays)
	if err != nil {
		return nil, err
	}
	return &AuditLog{log: retained}, nil
}

func (l *AuditLog) Append(event AuditEvent) error {
	if l == nil {
		return nil
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = l.log.currentTime()
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.log.prepareAppend(l.log.currentTime()); err != nil {
		return err
	}
	return l.log.write(payload)
}

func (l *AuditLog) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.log.close()
}

func RedactSQL(query string) string {
	const maxRunes = 2048
	runes := []rune(strings.TrimSpace(query))
	var output strings.Builder
	lastSpace := false
	for index := 0; index < len(runes); {
		character := runes[index]
		switch {
		case character == '`':
			index = copyQuotedIdentifier(&output, runes, index)
			lastSpace = false
		case character == '\'' || character == '"':
			index = skipQuotedLiteral(runes, index, character)
			output.WriteByte('?')
			lastSpace = false
		case character == '#':
			index = skipLineComment(runes, index)
		case character == '-' && index+1 < len(runes) && runes[index+1] == '-':
			index = skipLineComment(runes, index+2)
		case character == '/' && index+1 < len(runes) && runes[index+1] == '*':
			index = skipBlockComment(runes, index+2)
		case unicode.IsDigit(character) && numberBoundary(runes, index):
			index = skipNumber(runes, index)
			output.WriteByte('?')
			lastSpace = false
		case unicode.IsSpace(character):
			if !lastSpace && output.Len() > 0 {
				output.WriteByte(' ')
				lastSpace = true
			}
			index++
		default:
			output.WriteRune(character)
			lastSpace = false
			index++
		}
		if utf8RuneCount(output.String()) >= maxRunes {
			break
		}
	}
	redacted := strings.TrimSpace(output.String())
	redactedRunes := []rune(redacted)
	if len(redactedRunes) > maxRunes {
		redacted = string(redactedRunes[:maxRunes]) + "..."
	}
	return redacted
}

func Operation(query string) string {
	fields := strings.Fields(strings.ToUpper(RedactSQL(query)))
	if len(fields) == 0 {
		return "EMPTY"
	}
	first := strings.Trim(fields[0], "();")
	if len(fields) > 1 {
		switch first {
		case "ALTER", "CREATE", "DROP", "RENAME", "SHOW":
			return first + " " + strings.Trim(fields[1], "();")
		case "START":
			return "START TRANSACTION"
		}
	}
	return first
}

func openAppendFile(path string) (*os.File, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("log path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
}

func copyQuotedIdentifier(output *strings.Builder, runes []rune, index int) int {
	output.WriteRune('`')
	index++
	for index < len(runes) {
		output.WriteRune(runes[index])
		if runes[index] == '`' {
			if index+1 < len(runes) && runes[index+1] == '`' {
				output.WriteRune(runes[index+1])
				index += 2
				continue
			}
			return index + 1
		}
		index++
	}
	return index
}

func skipQuotedLiteral(runes []rune, index int, quote rune) int {
	index++
	escaped := false
	for index < len(runes) {
		character := runes[index]
		if escaped {
			escaped = false
			index++
			continue
		}
		if character == '\\' {
			escaped = true
			index++
			continue
		}
		if character == quote {
			if index+1 < len(runes) && runes[index+1] == quote {
				index += 2
				continue
			}
			return index + 1
		}
		index++
	}
	return index
}

func skipLineComment(runes []rune, index int) int {
	for index < len(runes) && runes[index] != '\n' {
		index++
	}
	return index
}

func skipBlockComment(runes []rune, index int) int {
	for index+1 < len(runes) {
		if runes[index] == '*' && runes[index+1] == '/' {
			return index + 2
		}
		index++
	}
	return len(runes)
}

func numberBoundary(runes []rune, index int) bool {
	if index == 0 {
		return true
	}
	previous := runes[index-1]
	return !(unicode.IsLetter(previous) || unicode.IsDigit(previous) || previous == '_' || previous == '$')
}

func skipNumber(runes []rune, index int) int {
	for index < len(runes) {
		character := runes[index]
		if unicode.IsDigit(character) || character == '.' || character == 'x' || character == 'X' || character == 'e' || character == 'E' || character == '+' || character == '-' || (character >= 'a' && character <= 'f') || (character >= 'A' && character <= 'F') {
			index++
			continue
		}
		break
	}
	return index
}

func utf8RuneCount(value string) int { return len([]rune(value)) }
