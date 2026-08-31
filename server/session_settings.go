package server

import (
	"fmt"
	"strings"
	"time"

	"gbaselite/executor"
	"gbaselite/internal/mysqlcompat"
	"gbaselite/storage"
)

func executeSessionSet(session *executor.Session, query string) (*executor.Result, error) {
	session.InitializeSettings()
	body := strings.TrimSpace(query[len("SET "):])
	upper := strings.ToUpper(body)
	if strings.HasPrefix(upper, "NAMES ") {
		fields := strings.Fields(body[len("NAMES "):])
		if len(fields) != 1 && !(len(fields) == 3 && strings.EqualFold(fields[1], "COLLATE")) {
			return nil, fmt.Errorf("invalid SET NAMES syntax")
		}
		charset := settingValue(fields[0])
		collation := ""
		if strings.EqualFold(charset, "DEFAULT") {
			charset = executor.DefaultCharacterSet
		}
		if len(fields) == 3 {
			collation = settingValue(fields[2])
		}
		if err := session.SetNames(charset, collation); err != nil {
			return nil, err
		}
		return &executor.Result{Message: "session character set changed"}, nil
	}
	if strings.HasPrefix(upper, "CHARACTER SET ") {
		charset := settingValue(strings.TrimSpace(body[len("CHARACTER SET "):]))
		if strings.EqualFold(charset, "DEFAULT") {
			charset = executor.DefaultCharacterSet
		}
		if err := session.SetNames(charset, ""); err != nil {
			return nil, err
		}
		return &executor.Result{Message: "session character set changed"}, nil
	}

	updated := *session
	changed := false
	for _, assignment := range splitSetAssignments(body) {
		left, right, ok := strings.Cut(assignment, "=")
		if !ok {
			continue
		}
		variable, global := normalizeSetVariable(left)
		value := settingValue(right)
		if strings.EqualFold(value, "DEFAULT") {
			switch variable {
			case "time_zone":
				value = executor.DefaultTimeZone
			case "collation_connection":
				value = executor.DefaultCollation
			default:
				value = executor.DefaultCharacterSet
			}
		}
		switch variable {
		case "time_zone":
			if global {
				return nil, fmt.Errorf("SET GLOBAL time_zone is not supported; configure each session with SET time_zone")
			}
			if err := updated.SetTimeZone(value); err != nil {
				return nil, err
			}
			changed = true
		case "character_set_client", "character_set_connection", "character_set_results", "collation_connection":
			if global {
				return nil, fmt.Errorf("SET GLOBAL %s is not supported", variable)
			}
			if variable == "character_set_results" && strings.EqualFold(value, "NULL") {
				updated.CharacterSetResults = "NULL"
				changed = true
				continue
			}
			if err := updated.SetCharacterSet(variable, value); err != nil {
				return nil, err
			}
			changed = true
		}
	}
	if changed {
		*session = updated
	}
	return &executor.Result{Message: "setting accepted"}, nil
}

func splitSetAssignments(value string) []string {
	var result []string
	start := 0
	var quote byte
	for index := 0; index < len(value); index++ {
		character := value[index]
		if quote != 0 {
			if character == '\\' {
				index++
				continue
			}
			if character == quote {
				if index+1 < len(value) && value[index+1] == quote {
					index++
					continue
				}
				quote = 0
			}
			continue
		}
		if character == '\'' || character == '"' {
			quote = character
			continue
		}
		if character == ',' {
			result = append(result, strings.TrimSpace(value[start:index]))
			start = index + 1
		}
	}
	return append(result, strings.TrimSpace(value[start:]))
}

func normalizeSetVariable(value string) (string, bool) {
	value = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(value), "`", ""))
	value = strings.TrimSpace(strings.TrimPrefix(value, "@@"))
	global := strings.HasPrefix(value, "global.") || strings.HasPrefix(value, "global ")
	for _, prefix := range []string{"session.", "local.", "global.", "session ", "local ", "global "} {
		value = strings.TrimSpace(strings.TrimPrefix(value, prefix))
	}
	return value, global
}

func settingValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && ((value[0] == '\'' && value[len(value)-1] == '\'') || (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '`' && value[len(value)-1] == '`')) {
		quote := value[:1]
		value = value[1 : len(value)-1]
		value = strings.ReplaceAll(value, quote+quote, quote)
	}
	return value
}

func characterSetRows(query string) (*executor.Result, error) {
	pattern, hasPattern, err := showSettingsLikePattern(query, "SHOW CHARACTER SET", "SHOW CHARSET")
	if err != nil {
		return nil, err
	}
	result := &executor.Result{Columns: []executor.Column{{Name: "Charset", Type: storage.TypeVarchar}, {Name: "Description", Type: storage.TypeText}, {Name: "Default collation", Type: storage.TypeVarchar}, {Name: "Maxlen", Type: storage.TypeInt}}}
	for _, charset := range mysqlcompat.CharacterSets() {
		if !hasPattern || showLikeMatch(charset.Name, pattern) {
			result.Rows = append(result.Rows, []any{charset.Name, charset.Description, charset.DefaultCollation, charset.MaxLength})
		}
	}
	return result, nil
}

func collationRows(query string) (*executor.Result, error) {
	pattern, hasPattern, err := showSettingsLikePattern(query, "SHOW COLLATION")
	if err != nil {
		return nil, err
	}
	result := &executor.Result{Columns: []executor.Column{{Name: "Collation", Type: storage.TypeVarchar}, {Name: "Charset", Type: storage.TypeVarchar}, {Name: "Id", Type: storage.TypeInt}, {Name: "Default", Type: storage.TypeVarchar}}}
	for _, collation := range mysqlcompat.Collations() {
		if !hasPattern || showLikeMatch(collation.Name, pattern) {
			isDefault := ""
			if collation.IsDefault {
				isDefault = "Yes"
			}
			result.Rows = append(result.Rows, []any{collation.Name, collation.Charset, collation.ID, isDefault})
		}
	}
	return result, nil
}

func showSettingsLikePattern(query string, prefixes ...string) (string, bool, error) {
	remainder := strings.TrimSpace(query)
	for _, prefix := range prefixes {
		if len(remainder) >= len(prefix) && strings.EqualFold(remainder[:len(prefix)], prefix) {
			remainder = strings.TrimSpace(remainder[len(prefix):])
			break
		}
	}
	if remainder == "" {
		return "", false, nil
	}
	rest, ok := consumeShowKeyword(remainder, "LIKE")
	if !ok {
		return "", false, fmt.Errorf("unsupported SHOW setting clause: %s", remainder)
	}
	pattern, tail, err := consumeShowString(rest)
	if err != nil {
		return "", false, err
	}
	if strings.TrimSpace(tail) != "" {
		return "", false, fmt.Errorf("unsupported SHOW setting clause: %s", tail)
	}
	return pattern, true, nil
}

func variableRows(session *executor.Session, query string) (*executor.Result, error) {
	pattern, hasPattern, err := showSettingsLikePattern(query, "SHOW SESSION VARIABLES", "SHOW GLOBAL VARIABLES", "SHOW VARIABLES")
	if err != nil {
		return nil, err
	}
	rows := sessionVariableValues(session, strings.HasPrefix(strings.ToUpper(strings.TrimSpace(query)), "SHOW GLOBAL VARIABLES"))
	result := &executor.Result{Columns: []executor.Column{{Name: "Variable_name", Type: storage.TypeVarchar}, {Name: "Value", Type: storage.TypeVarchar}}}
	for _, row := range rows {
		if !hasPattern || showLikeMatch(row[0].(string), pattern) {
			result.Rows = append(result.Rows, row)
		}
	}
	return result, nil
}

func sessionVariableValues(session *executor.Session, global bool) [][]any {
	session.InitializeSettings()
	timeZone := session.TimeZone
	if global {
		timeZone = session.ServerTimeZone
	}
	return [][]any{
		{"autocommit", "ON"},
		{"character_set_client", session.CharacterSetClient},
		{"character_set_connection", session.CharacterSetConnection},
		{"character_set_database", executor.DefaultCharacterSet},
		{"character_set_results", session.CharacterSetResults},
		{"character_set_server", executor.DefaultCharacterSet},
		{"collation_connection", session.CollationConnection},
		{"collation_database", executor.DefaultCollation},
		{"collation_server", executor.DefaultCollation},
		{"lower_case_table_names", "1"},
		{"system_time_zone", time.Local.String()},
		{"time_zone", timeZone},
		{"version", "5.7.44-GBaseLite"},
		{"version_comment", "GBaseLite"},
	}
}

func compatibilityVariables(session *executor.Session, query string) *executor.Result {
	expressions := splitSetAssignments(strings.TrimSpace(query[len("SELECT "):]))
	values := make(map[string]any)
	for _, row := range sessionVariableValues(session, false) {
		values[strings.ToLower(row[0].(string))] = row[1]
	}
	result := &executor.Result{Rows: [][]any{make([]any, len(expressions))}}
	globalValues := make(map[string]any)
	for _, row := range sessionVariableValues(session, true) {
		globalValues[strings.ToLower(row[0].(string))] = row[1]
	}
	for index, expression := range expressions {
		label := strings.TrimSpace(expression)
		variable := label
		if position := strings.Index(strings.ToUpper(variable), " AS "); position >= 0 {
			variable = strings.TrimSpace(variable[:position])
		}
		variable = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(variable, "@@")))
		global := strings.HasPrefix(variable, "global.")
		for _, prefix := range []string{"session.", "local.", "global."} {
			variable = strings.TrimPrefix(variable, prefix)
		}
		result.Columns = append(result.Columns, executor.Column{Name: label, Type: storage.TypeVarchar})
		if global {
			result.Rows[0][index] = globalValues[variable]
		} else {
			result.Rows[0][index] = values[variable]
		}
	}
	return result
}
