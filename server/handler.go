package server

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gbaselite/catalog"
	"gbaselite/executor"
	"gbaselite/parser"
	"gbaselite/storage"
)

var tableSchemaPattern = regexp.MustCompile(`(?i)table_schema\s*(?:=\s*|like\s+(?:binary\s+)?)'([^']+)'`)
var tableNamePattern = regexp.MustCompile(`(?i)table_name\s*(?:=\s*|like\s+(?:binary\s+)?)'([^']+)'`)
var columnNamePattern = regexp.MustCompile(`(?i)column_name\s*(?:=\s*|like\s+(?:binary\s+)?)'([^']+)'`)
var tableTypePattern = regexp.MustCompile(`(?i)table_type\s*=\s*'([^']+)'`)
var viewFilePattern = regexp.MustCompile(`(?i)concat\s*\(\s*@@datadir\s*,\s*'([^']+)'\s*,\s*'/'\s*,\s*'([^']+)'\s*,\s*'\.frm'\s*\)`)

func ExecuteCompatible(engine *executor.Engine, session *executor.Session, query string) (*executor.Result, error) {
	session.InitializeSettings()
	if err := engine.AvailabilityError(); err != nil {
		return nil, err
	}
	expanded, err := parser.ExpandMySQLExecutableComments(query)
	if err != nil {
		return nil, err
	}
	query = trimLeadingCompatibilityComments(expanded)
	trimmed := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(query), ";"))
	upper := strings.ToUpper(trimmed)
	metadataUpper := strings.ReplaceAll(upper, "`", "")
	switch {
	case trimmed == "":
		return &executor.Result{}, nil
	case strings.HasPrefix(upper, "SET ") && !strings.HasPrefix(upper, "SET PASSWORD"):
		return executeSessionSet(session, trimmed)
	case upper == "FLUSH PRIVILEGES":
		return &executor.Result{Message: "privileges are already persistent"}, nil
	case strings.HasPrefix(upper, "FLUSH TABLES"):
		return &executor.Result{Message: "table flush compatibility accepted"}, nil
	case strings.HasPrefix(upper, "LOCK TABLES ") || upper == "UNLOCK TABLES":
		return &executor.Result{Message: "table lock compatibility accepted"}, nil
	case strings.HasPrefix(upper, "SAVEPOINT ") || strings.HasPrefix(upper, "RELEASE SAVEPOINT ") || strings.HasPrefix(upper, "ROLLBACK TO "):
		return &executor.Result{Message: "savepoint compatibility accepted"}, nil
	case strings.HasPrefix(upper, "KILL "):
		return &executor.Result{Message: "connection management accepted"}, nil
	case strings.HasPrefix(upper, "ALTER TABLE ") && (strings.Contains(upper, " DISABLE KEYS") || strings.Contains(upper, " ENABLE KEYS")):
		return &executor.Result{Message: "index maintenance accepted"}, nil
	case upper == "SHOW WARNINGS" || upper == "SHOW ERRORS":
		return &executor.Result{Columns: []executor.Column{{Name: "Level", Type: storage.TypeVarchar}, {Name: "Code", Type: storage.TypeInt}, {Name: "Message", Type: storage.TypeText}}}, nil
	case strings.HasPrefix(upper, "SHOW PROCEDURE STATUS") || strings.HasPrefix(upper, "SHOW FUNCTION STATUS"):
		return routineStatus(), nil
	case strings.HasPrefix(upper, "SHOW TRIGGERS"):
		return triggerStatus(), nil
	case strings.HasPrefix(upper, "SHOW EVENTS"):
		return eventStatus(), nil
	case strings.HasPrefix(upper, "SHOW FULL TABLES"):
		return showTables(engine, session, trimmed, true)
	case strings.HasPrefix(upper, "SHOW TABLES"):
		return showTables(engine, session, trimmed, false)
	case strings.HasPrefix(upper, "SHOW FULL COLUMNS"):
		return engine.Execute(session, trimmed)
	case strings.HasPrefix(upper, "SHOW COLUMNS") || strings.HasPrefix(upper, "SHOW FIELDS"):
		return engine.Execute(session, trimmed)
	case strings.HasPrefix(upper, "SHOW INDEX ") || strings.HasPrefix(upper, "SHOW INDEXES ") || strings.HasPrefix(upper, "SHOW KEYS "):
		return engine.Execute(session, trimmed)
	case strings.HasPrefix(upper, "SHOW VARIABLES") || strings.HasPrefix(upper, "SHOW SESSION VARIABLES") || strings.HasPrefix(upper, "SHOW GLOBAL VARIABLES"):
		return variableRows(session, trimmed)
	case strings.HasPrefix(upper, "SHOW DATABASES") || strings.HasPrefix(upper, "SHOW SCHEMAS"):
		return databaseRows(engine, session), nil
	case strings.HasPrefix(upper, "SHOW CHARACTER SET") || strings.HasPrefix(upper, "SHOW CHARSET"):
		return characterSetRows(trimmed)
	case strings.HasPrefix(upper, "SHOW COLLATION"):
		return collationRows(trimmed)
	case strings.HasPrefix(upper, "SHOW ENGINES"):
		return &executor.Result{Columns: []executor.Column{{Name: "Engine", Type: storage.TypeVarchar}, {Name: "Support", Type: storage.TypeVarchar}, {Name: "Comment", Type: storage.TypeText}}, Rows: [][]any{{"GBaseLite", "DEFAULT", "GBaseLite storage engine"}}}, nil
	case strings.HasPrefix(upper, "SHOW PROCESSLIST"):
		return &executor.Result{Columns: []executor.Column{{Name: "Id", Type: storage.TypeBigInt}, {Name: "User", Type: storage.TypeVarchar}, {Name: "Host", Type: storage.TypeVarchar}, {Name: "db", Type: storage.TypeVarchar}, {Name: "Command", Type: storage.TypeVarchar}}, Rows: [][]any{}}, nil
	case isShowStatusQuery(upper):
		return statusRows(session, trimmed, runtimeStatus{})
	case strings.HasPrefix(upper, "SHOW TABLE STATUS"):
		return tableStatus(engine, session, trimmed)
	case metadataFrom(metadataUpper, "SCHEMATA"):
		return schemaInformation(engine, session), nil
	case metadataFrom(metadataUpper, "FILES"):
		return emptyMetadata([]string{"TABLESPACE_NAME"}), nil
	case metadataFrom(metadataUpper, "TABLESPACES"):
		return emptyMetadata([]string{"TABLESPACE_NAME", "ENGINE", "TABLESPACE_TYPE", "LOGFILE_GROUP_NAME"}), nil
	case metadataFrom(metadataUpper, "TABLES"):
		return tableInformation(engine, session, trimmed)
	case metadataFrom(metadataUpper, "COLUMNS"):
		return columnInformation(engine, session, trimmed)
	case metadataFrom(metadataUpper, "PARTITIONS"):
		return emptyMetadata([]string{
			"TABLE_NAME", "PARTITION_NAME", "SUBPARTITION_NAME", "PARTITION_METHOD",
			"SUBPARTITION_METHOD", "PARTITION_EXPRESSION", "SUBPARTITION_EXPRESSION",
			"PARTITION_DESCRIPTION", "PARTITION_COMMENT", "NODEGROUP", "TABLESPACE_NAME",
		}), nil
	case metadataFrom(metadataUpper, "STATISTICS"):
		return indexInformation(engine, session, trimmed)
	case metadataFrom(metadataUpper, "KEY_COLUMN_USAGE"):
		return keyColumnUsageInformation(engine, session, trimmed)
	case metadataFrom(metadataUpper, "TABLE_CONSTRAINTS"):
		return tableConstraintInformation(engine, session, trimmed)
	case metadataFrom(metadataUpper, "REFERENTIAL_CONSTRAINTS"):
		return referentialConstraintInformation(engine, session, trimmed)
	case metadataFrom(metadataUpper, "CHECK_CONSTRAINTS"):
		return checkConstraintInformation(engine, session, trimmed)
	case metadataFrom(metadataUpper, "ROUTINES"):
		return emptyMetadata([]string{"ROUTINE_SCHEMA", "ROUTINE_NAME", "PARAMETER"}), nil
	case metadataFrom(metadataUpper, "VIEWS"):
		return viewInformation(engine, session, trimmed)
	case metadataFrom(metadataUpper, "USER_PRIVILEGES"):
		return privilegeInformation(engine, session, trimmed, "USER"), nil
	case metadataFrom(metadataUpper, "SCHEMA_PRIVILEGES"):
		return privilegeInformation(engine, session, trimmed, "SCHEMA"), nil
	case metadataFrom(metadataUpper, "TABLE_PRIVILEGES"):
		return privilegeInformation(engine, session, trimmed, "TABLE"), nil
	case metadataFrom(metadataUpper, "PARAMETERS"):
		return emptyMetadata([]string{"SPECIFIC_SCHEMA", "SPECIFIC_NAME", "PARAMETER_NAME"}), nil
	case metadataFrom(metadataUpper, "TRIGGERS"):
		return emptyMetadata([]string{"TRIGGER_SCHEMA", "TRIGGER_NAME", "EVENT_OBJECT_TABLE"}), nil
	case metadataFrom(metadataUpper, "EVENTS"):
		return emptyMetadata([]string{"EVENT_SCHEMA", "EVENT_NAME"}), nil
	case viewFilePattern.MatchString(trimmed):
		return navicatViewFile(engine, session, trimmed)
	case metadataFromMySQLUser(metadataUpper):
		return mysqlUserInformation(engine, session, trimmed)
	case strings.HasPrefix(upper, "SELECT @@"):
		return compatibilityVariables(session, trimmed), nil
	}
	return engine.Execute(session, trimmed)
}

func trimLeadingCompatibilityComments(query string) string {
	query = strings.TrimPrefix(query, "\ufeff")
	for {
		query = strings.TrimSpace(query)
		switch {
		case strings.HasPrefix(query, "/*"):
			end := strings.Index(query[2:], "*/")
			if end < 0 {
				return query
			}
			query = query[end+4:]
		case strings.HasPrefix(query, "#"):
			newline := strings.IndexByte(query, '\n')
			if newline < 0 {
				return ""
			}
			query = query[newline+1:]
		case strings.HasPrefix(query, "--") && (len(query) == 2 || query[2] <= ' '):
			newline := strings.IndexByte(query, '\n')
			if newline < 0 {
				return ""
			}
			query = query[newline+1:]
		default:
			return query
		}
	}
}

func routineStatus() *executor.Result {
	return emptyMetadata([]string{
		"Db", "Name", "Type", "Definer", "Modified", "Created", "Security_type",
		"Comment", "character_set_client", "collation_connection", "Database Collation",
	})
}

func triggerStatus() *executor.Result {
	return emptyMetadata([]string{
		"Trigger", "Event", "Table", "Statement", "Timing", "Created", "sql_mode",
		"Definer", "character_set_client", "collation_connection", "Database Collation",
	})
}

func eventStatus() *executor.Result {
	return emptyMetadata([]string{
		"Db", "Name", "Definer", "Time zone", "Type", "Execute at", "Interval value",
		"Interval field", "Starts", "Ends", "Status", "Originator", "character_set_client",
		"collation_connection", "Database Collation",
	})
}

func navicatViewFile(engine *executor.Engine, session *executor.Session, query string) (*executor.Result, error) {
	match := viewFilePattern.FindStringSubmatch(query)
	if len(match) != 3 {
		return nil, fmt.Errorf("invalid view source query")
	}
	database, err := engine.Store.Database(match[1])
	if err != nil {
		return nil, err
	}
	if !engine.Users.HasObjectAccess(session.Username, session.Host, database.Name(), match[2]) {
		return nil, fmt.Errorf("access denied for user %q@%q to %s.%s", session.Username, session.Host, database.Name(), match[2])
	}
	view, err := database.View(match[2])
	if err != nil {
		return nil, err
	}
	source := "TYPE=VIEW\n" +
		"query=" + view.Definition + "\n" +
		"updatable=0\nalgorithm=0\n" +
		"definer_user=root\ndefiner_host=%\nsuid=2\nwith_check_option=0\n" +
		"source=" + view.Definition + "\n" +
		"client_cs_name=utf8mb4\nconnection_cl_name=utf8mb4_general_ci\n" +
		"view_body_utf8=" + view.Definition + "\n"
	return &executor.Result{Columns: []executor.Column{{Name: "source", Type: storage.TypeText}}, Rows: [][]any{{source}}}, nil
}

func metadataFrom(query, table string) bool {
	const marker = "FROM INFORMATION_SCHEMA."
	start := strings.Index(query, marker)
	if start < 0 {
		return false
	}
	start += len(marker)
	if len(query)-start < len(table) || query[start:start+len(table)] != table {
		return false
	}
	end := start + len(table)
	if end == len(query) {
		return true
	}
	next := query[end]
	return (next < 'A' || next > 'Z') && (next < '0' || next > '9') && next != '_'
}

func metadataFromMySQLUser(query string) bool {
	marker := "FROM MYSQL.USER"
	start := strings.Index(query, marker)
	if start < 0 {
		return false
	}
	end := start + len(marker)
	return end == len(query) || ((query[end] < 'A' || query[end] > 'Z') && (query[end] < '0' || query[end] > '9') && query[end] != '_')
}

func emptyMetadata(names []string) *executor.Result {
	result := &executor.Result{}
	for _, name := range names {
		result.Columns = append(result.Columns, executor.Column{Name: name, Type: storage.TypeVarchar})
	}
	return result
}

func databaseRows(engine *executor.Engine, session *executor.Session) *executor.Result {
	result := &executor.Result{Columns: []executor.Column{{Name: "Database", Type: storage.TypeVarchar}}}
	for _, name := range engine.Store.ListDatabases() {
		if engine.Users.HasDatabaseAccess(session.Username, session.Host, name) {
			result.Rows = append(result.Rows, []any{name})
		}
	}
	return result
}

func showTables(engine *executor.Engine, session *executor.Session, query string, full bool) (*executor.Result, error) {
	prefix := "SHOW TABLES"
	if full {
		prefix = "SHOW FULL TABLES"
	}
	remainder := strings.TrimSpace(query[len(prefix):])
	databaseName := session.CurrentDatabase
	if rest, ok := consumeShowKeyword(remainder, "FROM"); ok {
		name, tail, err := consumeShowIdentifier(rest)
		if err != nil {
			return nil, err
		}
		databaseName, remainder = name, strings.TrimSpace(tail)
	} else if rest, ok := consumeShowKeyword(remainder, "IN"); ok {
		name, tail, err := consumeShowIdentifier(rest)
		if err != nil {
			return nil, err
		}
		databaseName, remainder = name, strings.TrimSpace(tail)
	}
	database, err := engine.Store.Database(databaseName)
	if err != nil {
		return nil, err
	}

	likePattern := ""
	tableType := ""
	tableTypeNegated := false
	if rest, ok := consumeShowKeyword(remainder, "LIKE"); ok {
		likePattern, remainder, err = consumeShowString(rest)
		if err != nil {
			return nil, err
		}
		remainder = strings.TrimSpace(remainder)
	} else if rest, ok := consumeShowKeyword(remainder, "WHERE"); ok {
		tableType, tableTypeNegated, err = parseShowTableTypeCondition(rest)
		if err != nil {
			return nil, err
		}
		remainder = ""
	}
	if remainder != "" {
		return nil, fmt.Errorf("unsupported SHOW TABLES clause: %s", remainder)
	}

	columns := []executor.Column{{Name: "Tables_in_" + database.Name(), Type: storage.TypeVarchar}}
	if full {
		columns = append(columns, executor.Column{Name: "Table_type", Type: storage.TypeVarchar})
	}
	result := &executor.Result{Columns: columns}
	includeType := func(candidate string) bool {
		if tableType == "" {
			return true
		}
		matches := strings.EqualFold(candidate, tableType)
		if tableTypeNegated {
			return !matches
		}
		return matches
	}
	appendRelation := func(name, relationType string) {
		if !engine.Users.HasObjectAccess(session.Username, session.Host, database.Name(), name) {
			return
		}
		if likePattern != "" && !showLikeMatch(name, likePattern) {
			return
		}
		if !includeType(relationType) {
			return
		}
		row := []any{name}
		if full {
			row = append(row, relationType)
		}
		result.Rows = append(result.Rows, row)
	}
	for _, name := range database.ListRelations() {
		relationType := "BASE TABLE"
		if _, viewErr := database.View(name); viewErr == nil {
			relationType = "VIEW"
		}
		appendRelation(name, relationType)
	}
	return result, nil
}

func consumeShowKeyword(input, keyword string) (string, bool) {
	input = strings.TrimSpace(input)
	if len(input) < len(keyword) || !strings.EqualFold(input[:len(keyword)], keyword) {
		return input, false
	}
	if len(input) > len(keyword) && !strings.ContainsRune(" \t\r\n", rune(input[len(keyword)])) {
		return input, false
	}
	return strings.TrimSpace(input[len(keyword):]), true
}

func showColumns(engine *executor.Engine, session *executor.Session, query string, full bool) (*executor.Result, error) {
	prefix := "SHOW COLUMNS"
	if full {
		prefix = "SHOW FULL COLUMNS"
	} else if strings.HasPrefix(strings.ToUpper(query), "SHOW FIELDS") {
		prefix = "SHOW FIELDS"
	}
	remainder := strings.TrimSpace(query[len(prefix):])
	if rest, ok := consumeShowKeyword(remainder, "FROM"); ok {
		remainder = rest
	} else if rest, ok := consumeShowKeyword(remainder, "IN"); ok {
		remainder = rest
	} else {
		return nil, fmt.Errorf("SHOW COLUMNS requires FROM or IN")
	}
	tableName, remainder, err := consumeShowIdentifier(remainder)
	if err != nil {
		return nil, err
	}
	databaseName := ""
	trimmedRemainder := strings.TrimSpace(remainder)
	if strings.HasPrefix(trimmedRemainder, ".") {
		databaseName = tableName
		tableName, remainder, err = consumeShowIdentifier(trimmedRemainder[1:])
	} else if dot := strings.IndexByte(tableName, '.'); dot >= 0 {
		databaseName, tableName = tableName[:dot], tableName[dot+1:]
	}
	if err != nil {
		return nil, err
	}
	if rest, ok := consumeShowKeyword(remainder, "FROM"); ok {
		if databaseName != "" {
			return nil, fmt.Errorf("SHOW COLUMNS specifies a database twice")
		}
		databaseName, remainder, err = consumeShowIdentifier(rest)
	} else if rest, ok := consumeShowKeyword(remainder, "IN"); ok {
		if databaseName != "" {
			return nil, fmt.Errorf("SHOW COLUMNS specifies a database twice")
		}
		databaseName, remainder, err = consumeShowIdentifier(rest)
	}
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(remainder) != "" {
		return nil, fmt.Errorf("unsupported SHOW COLUMNS clause: %s", remainder)
	}
	qualified := tableName
	if databaseName != "" {
		qualified = databaseName + "." + tableName
	}
	result, err := engine.Execute(session, "SHOW COLUMNS FROM `"+strings.ReplaceAll(qualified, "`", "``")+"`")
	if err != nil || !full {
		return result, err
	}
	resolvedDatabase := databaseName
	if resolvedDatabase == "" {
		resolvedDatabase = session.CurrentDatabase
	}
	database, databaseErr := engine.Store.Database(resolvedDatabase)
	if databaseErr != nil {
		return nil, databaseErr
	}
	table, tableErr := database.Table(tableName)
	if tableErr != nil {
		return result, nil
	}
	definitions := table.Columns()
	fullResult := &executor.Result{Columns: []executor.Column{
		{Name: "Field", Type: storage.TypeVarchar}, {Name: "Type", Type: storage.TypeVarchar},
		{Name: "Collation", Type: storage.TypeVarchar}, {Name: "Null", Type: storage.TypeVarchar},
		{Name: "Key", Type: storage.TypeVarchar}, {Name: "Default", Type: storage.TypeVarchar},
		{Name: "Extra", Type: storage.TypeVarchar}, {Name: "Privileges", Type: storage.TypeVarchar},
		{Name: "Comment", Type: storage.TypeText},
	}}
	for index, row := range result.Rows {
		var collation any
		comment := ""
		if index < len(definitions) {
			definition := definitions[index]
			if definition.Type == storage.TypeVarchar || definition.Type == storage.TypeText {
				collation = "utf8mb4_general_ci"
			}
			comment = definition.Comment
		}
		fullResult.Rows = append(fullResult.Rows, []any{row[0], row[1], collation, row[2], row[3], row[4], row[5], "select,insert,update,references", comment})
	}
	return fullResult, nil
}

func consumeShowIdentifier(input string) (string, string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", "", fmt.Errorf("SHOW TABLES requires a database name")
	}
	if input[0] != '`' {
		end := strings.IndexAny(input, " \t\r\n")
		if end < 0 {
			return input, "", nil
		}
		return input[:end], input[end:], nil
	}
	var name strings.Builder
	for index := 1; index < len(input); index++ {
		if input[index] != '`' {
			name.WriteByte(input[index])
			continue
		}
		if index+1 < len(input) && input[index+1] == '`' {
			name.WriteByte('`')
			index++
			continue
		}
		return name.String(), input[index+1:], nil
	}
	return "", "", fmt.Errorf("unterminated database identifier in SHOW TABLES")
}

func consumeShowString(input string) (string, string, error) {
	input = strings.TrimSpace(input)
	if input == "" || (input[0] != '\'' && input[0] != '"') {
		return "", "", fmt.Errorf("SHOW TABLES requires a quoted string")
	}
	quote := input[0]
	var value strings.Builder
	for index := 1; index < len(input); index++ {
		switch input[index] {
		case quote:
			if index+1 < len(input) && input[index+1] == quote {
				value.WriteByte(quote)
				index++
				continue
			}
			return value.String(), input[index+1:], nil
		case '\\':
			if index+1 < len(input) {
				index++
				value.WriteByte(input[index])
				continue
			}
			value.WriteByte('\\')
		default:
			value.WriteByte(input[index])
		}
	}
	return "", "", fmt.Errorf("unterminated string in SHOW TABLES")
}

func parseShowTableTypeCondition(condition string) (string, bool, error) {
	condition = strings.TrimSpace(condition)
	if len(condition) >= 2 && condition[0] == '(' && condition[len(condition)-1] == ')' {
		condition = strings.TrimSpace(condition[1 : len(condition)-1])
	}
	if strings.HasPrefix(condition, "`") {
		end := strings.Index(condition[1:], "`")
		if end < 0 {
			return "", false, fmt.Errorf("unterminated column name in SHOW TABLES WHERE")
		}
		column := condition[1 : end+1]
		if !strings.EqualFold(column, "Table_type") {
			return "", false, fmt.Errorf("unsupported SHOW TABLES WHERE column: %s", column)
		}
		condition = strings.TrimSpace(condition[end+2:])
	} else {
		end := strings.IndexAny(condition, " \t\r\n=<>!")
		if end < 0 || !strings.EqualFold(condition[:end], "Table_type") {
			return "", false, fmt.Errorf("unsupported SHOW TABLES WHERE condition: %s", condition)
		}
		condition = strings.TrimSpace(condition[end:])
	}
	operator := ""
	for _, candidate := range []string{"!=", "<>", "="} {
		if strings.HasPrefix(condition, candidate) {
			operator = candidate
			condition = strings.TrimSpace(condition[len(candidate):])
			break
		}
	}
	if operator == "" {
		return "", false, fmt.Errorf("unsupported SHOW TABLES WHERE operator")
	}
	value, remainder, err := consumeShowString(condition)
	if err != nil {
		return "", false, err
	}
	if strings.TrimSpace(remainder) != "" {
		return "", false, fmt.Errorf("unsupported SHOW TABLES WHERE condition: %s", condition)
	}
	return strings.ToUpper(value), operator != "=", nil
}

func showLikeMatch(value, pattern string) bool {
	valueRunes := []rune(strings.ToLower(value))
	patternRunes := []rune(strings.ToLower(pattern))
	previous := make([]bool, len(valueRunes)+1)
	previous[0] = true
	for _, token := range patternRunes {
		current := make([]bool, len(valueRunes)+1)
		switch token {
		case '%':
			current[0] = previous[0]
			for index := 1; index <= len(valueRunes); index++ {
				current[index] = previous[index] || current[index-1]
			}
		case '_':
			for index := 1; index <= len(valueRunes); index++ {
				current[index] = previous[index-1]
			}
		default:
			for index := 1; index <= len(valueRunes); index++ {
				current[index] = previous[index-1] && valueRunes[index-1] == token
			}
		}
		previous = current
	}
	return previous[len(valueRunes)]
}

type runtimeStatus struct {
	Uptime             uint64
	Connections        uint64
	ActiveConnections  int64
	MaxUsedConnections int64
	Questions          uint64
	ActiveQueries      int64
	AbortedConnections uint64
	TLSConnections     uint64
	StorageState       string
}

func isShowStatusQuery(upper string) bool {
	return strings.HasPrefix(upper, "SHOW STATUS") ||
		strings.HasPrefix(upper, "SHOW SESSION STATUS") ||
		strings.HasPrefix(upper, "SHOW GLOBAL STATUS")
}

func statusRows(session *executor.Session, query string, status runtimeStatus) (*executor.Result, error) {
	likePattern, hasLikePattern, err := showStatusLikePattern(query)
	if err != nil {
		return nil, err
	}
	storageState := status.StorageState
	if storageState == "" {
		storageState = "available"
	}
	rows := [][]any{
		{"Aborted_connects", strconv.FormatUint(status.AbortedConnections, 10)},
		{"Connections", strconv.FormatUint(status.Connections, 10)},
		{"Gbaselite_storage_state", storageState},
		{"Max_used_connections", strconv.FormatInt(status.MaxUsedConnections, 10)},
		{"Questions", strconv.FormatUint(status.Questions, 10)},
		{"Ssl_accepts", strconv.FormatUint(status.TLSConnections, 10)},
		{"Ssl_cipher", session.TLSCipher},
		{"Ssl_version", session.TLSVersion},
		{"Threads_connected", strconv.FormatInt(status.ActiveConnections, 10)},
		{"Threads_running", strconv.FormatInt(status.ActiveQueries, 10)},
		{"Uptime", strconv.FormatUint(status.Uptime, 10)},
	}
	result := &executor.Result{Columns: []executor.Column{{Name: "Variable_name", Type: storage.TypeVarchar}, {Name: "Value", Type: storage.TypeVarchar}}}
	for _, row := range rows {
		if !hasLikePattern || showLikeMatch(row[0].(string), likePattern) {
			result.Rows = append(result.Rows, row)
		}
	}
	return result, nil
}

func showStatusLikePattern(query string) (string, bool, error) {
	remainder := strings.TrimSpace(query)
	for _, prefix := range []string{"SHOW SESSION STATUS", "SHOW GLOBAL STATUS", "SHOW STATUS"} {
		if len(remainder) >= len(prefix) && strings.EqualFold(remainder[:len(prefix)], prefix) {
			remainder = strings.TrimSpace(remainder[len(prefix):])
			break
		}
	}
	if remainder == "" {
		return "", false, nil
	}
	remainder, ok := consumeShowKeyword(remainder, "LIKE")
	if !ok {
		return "", false, fmt.Errorf("unsupported SHOW STATUS filter: %s", remainder)
	}
	pattern, remainder, err := consumeShowString(remainder)
	if err != nil {
		return "", false, err
	}
	if strings.TrimSpace(remainder) != "" {
		return "", false, fmt.Errorf("unsupported SHOW STATUS filter: %s", remainder)
	}
	return pattern, true, nil
}
func schemaInformation(engine *executor.Engine, session *executor.Session) *executor.Result {
	result := &executor.Result{Columns: []executor.Column{{Name: "SCHEMA_NAME", Type: storage.TypeVarchar}}}
	for _, name := range engine.Store.ListDatabases() {
		if engine.Users.HasDatabaseAccess(session.Username, session.Host, name) {
			result.Rows = append(result.Rows, []any{name})
		}
	}
	return result
}
func tableInformation(engine *executor.Engine, session *executor.Session, query string) (*executor.Result, error) {
	databaseName := session.CurrentDatabase
	if match := tableSchemaPattern.FindStringSubmatch(query); len(match) > 1 {
		databaseName = match[1]
	}
	database, err := engine.Store.Database(databaseName)
	if err != nil {
		return nil, err
	}
	result := &executor.Result{Columns: informationSchemaTableColumns()}
	tableName := ""
	if match := tableNamePattern.FindStringSubmatch(query); len(match) > 1 {
		tableName = match[1]
	}
	tableType := ""
	if match := tableTypePattern.FindStringSubmatch(query); len(match) > 1 {
		tableType = strings.ToUpper(match[1])
	}
	if tableType == "" || tableType == "BASE TABLE" {
		for _, name := range database.ListTables() {
			if !engine.Users.HasObjectAccess(session.Username, session.Host, database.Name(), name) {
				continue
			}
			if tableName != "" && !strings.EqualFold(tableName, name) {
				continue
			}
			table, _ := database.Table(name)
			result.Rows = append(result.Rows, informationSchemaTableRow(database.Name(), table))
		}
	}
	if tableType == "" || tableType == "VIEW" {
		for _, name := range database.ListViews() {
			if !engine.Users.HasObjectAccess(session.Username, session.Host, database.Name(), name) {
				continue
			}
			if tableName != "" && !strings.EqualFold(tableName, name) {
				continue
			}
			result.Rows = append(result.Rows, []any{"def", database.Name(), name, "VIEW", nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, "utf8mb4_general_ci", nil, nil, "", "VIEW"})
		}
	}
	return projectMetadataColumns(query, result), nil
}
func columnInformation(engine *executor.Engine, session *executor.Session, query string) (*executor.Result, error) {
	databaseName := session.CurrentDatabase
	if match := tableSchemaPattern.FindStringSubmatch(query); len(match) > 1 {
		databaseName = match[1]
	}
	tableName := ""
	if match := tableNamePattern.FindStringSubmatch(query); len(match) > 1 {
		tableName = match[1]
	}
	columnName := ""
	if match := columnNamePattern.FindStringSubmatch(query); len(match) > 1 {
		columnName = match[1]
	}
	database, err := engine.Store.Database(databaseName)
	if err != nil {
		return nil, err
	}
	if tableName != "" && !engine.Users.HasObjectAccess(session.Username, session.Host, database.Name(), tableName) {
		return nil, fmt.Errorf("access denied for user %q@%q to %s.%s", session.Username, session.Host, database.Name(), tableName)
	}
	result := &executor.Result{Columns: informationSchemaColumnColumns()}
	appendTable := func(table *storage.Table) {
		for position, column := range table.Columns() {
			if columnName != "" && !showLikeMatch(column.Name, columnName) {
				continue
			}
			result.Rows = append(result.Rows, informationSchemaColumnRow(database.Name(), table.Name(), position, column, table.ColumnKey(column.Name)))
		}
	}
	if tableName != "" {
		if table, tableErr := database.Table(tableName); tableErr == nil {
			appendTable(table)
			return projectMetadataColumns(query, result), nil
		}
		columns, viewErr := viewColumns(engine, session, database.Name(), tableName)
		if viewErr != nil {
			return nil, viewErr
		}
		for position, column := range columns {
			if columnName != "" && !showLikeMatch(column.Name, columnName) {
				continue
			}
			definition := storage.Column{Name: column.Name, Type: column.Type}
			result.Rows = append(result.Rows, informationSchemaColumnRow(database.Name(), tableName, position, definition, ""))
		}
		return projectMetadataColumns(query, result), nil
	}
	for _, name := range database.ListTables() {
		if engine.Users.HasObjectAccess(session.Username, session.Host, database.Name(), name) {
			table, _ := database.Table(name)
			appendTable(table)
		}
	}
	for _, name := range database.ListViews() {
		if !engine.Users.HasObjectAccess(session.Username, session.Host, database.Name(), name) {
			continue
		}
		columns, columnErr := viewColumns(engine, session, database.Name(), name)
		if columnErr != nil {
			// A legacy view may have duplicate or otherwise non-materializable
			// output columns. Keep its failure from hiding every base table.
			continue
		}
		for position, column := range columns {
			if columnName != "" && !showLikeMatch(column.Name, columnName) {
				continue
			}
			definition := storage.Column{Name: column.Name, Type: column.Type}
			result.Rows = append(result.Rows, informationSchemaColumnRow(database.Name(), name, position, definition, ""))
		}
	}
	return projectMetadataColumns(query, result), nil
}

func informationSchemaColumnColumns() []executor.Column {
	names := []string{"TABLE_CATALOG", "TABLE_SCHEMA", "TABLE_NAME", "COLUMN_NAME", "ORDINAL_POSITION", "COLUMN_DEFAULT", "IS_NULLABLE", "DATA_TYPE", "CHARACTER_MAXIMUM_LENGTH", "CHARACTER_OCTET_LENGTH", "NUMERIC_PRECISION", "NUMERIC_SCALE", "DATETIME_PRECISION", "CHARACTER_SET_NAME", "COLLATION_NAME", "COLUMN_TYPE", "COLUMN_KEY", "EXTRA", "PRIVILEGES", "COLUMN_COMMENT", "GENERATION_EXPRESSION", "SRS_ID"}
	columns := make([]executor.Column, len(names))
	for index, name := range names {
		columns[index] = executor.Column{Name: name, Type: storage.TypeVarchar}
	}
	columns[4].Type = storage.TypeBigInt
	return columns
}

func informationSchemaTableColumns() []executor.Column {
	names := []string{"TABLE_CATALOG", "TABLE_SCHEMA", "TABLE_NAME", "TABLE_TYPE", "ENGINE", "VERSION", "ROW_FORMAT", "TABLE_ROWS", "AVG_ROW_LENGTH", "DATA_LENGTH", "MAX_DATA_LENGTH", "INDEX_LENGTH", "DATA_FREE", "AUTO_INCREMENT", "CREATE_TIME", "UPDATE_TIME", "CHECK_TIME", "TABLE_COLLATION", "CHECKSUM", "CREATE_OPTIONS", "TABLE_COMMENT"}
	columns := make([]executor.Column, len(names))
	for index, name := range names {
		columns[index] = executor.Column{Name: name, Type: storage.TypeVarchar}
	}
	for _, index := range []int{5, 7, 8, 9, 10, 11, 12, 13, 18} {
		columns[index].Type = storage.TypeBigInt
	}
	for _, index := range []int{14, 15, 16} {
		columns[index].Type = storage.TypeDateTime
	}
	return columns
}

func informationSchemaTableRow(schema string, table *storage.Table) []any {
	comment, createdAt, updatedAt := table.Metadata()
	createdAt = createdAt.Local().Truncate(time.Second)
	updatedAt = updatedAt.Local().Truncate(time.Second)
	rows := int64(table.RowCount())
	dataLength := table.DataLength()
	averageLength := int64(0)
	if rows > 0 {
		averageLength = dataLength / rows
	}
	return []any{"def", schema, table.Name(), "BASE TABLE", "GBaseLite", int64(10), "Dynamic", rows, averageLength, dataLength, int64(0), int64(0), int64(0), nil, createdAt, updatedAt, nil, "utf8mb4_general_ci", nil, "", comment}
}

func informationSchemaColumnRow(schema, table string, position int, column storage.Column, key string) []any {
	nullable := "NO"
	if storage.ColumnNullable(column) {
		nullable = "YES"
	}
	var defaultValue any
	if column.HasDefault {
		if column.DefaultExpression != "" {
			defaultValue = column.DefaultExpression
		} else {
			defaultValue = column.Default.Interface()
		}
	}
	extra := ""
	if column.AutoIncrement {
		extra = "auto_increment"
	}
	if column.OnUpdate != "" {
		if extra != "" {
			extra += " "
		}
		extra += "on update " + column.OnUpdate
	}
	var characterLength any
	characterSet, collation := any(nil), any(nil)
	if column.Type == storage.TypeVarchar || column.Type == storage.TypeText {
		if column.Length > 0 {
			characterLength = int64(column.Length)
		}
		characterSet, collation = "utf8mb4", "utf8mb4_general_ci"
	}
	return []any{"def", schema, table, column.Name, int64(position + 1), defaultValue, nullable, storage.ColumnDataType(column), characterLength, characterLength, nil, nil, nil, characterSet, collation, navicatColumnType(column), key, extra, "select,insert,update,references", column.Comment, "", nil}
}

func indexInformation(engine *executor.Engine, session *executor.Session, query string) (*executor.Result, error) {
	databaseName := session.CurrentDatabase
	if match := tableSchemaPattern.FindStringSubmatch(query); len(match) > 1 {
		databaseName = match[1]
	}
	database, err := engine.Store.Database(databaseName)
	if err != nil {
		return nil, err
	}
	tableFilter := ""
	if match := tableNamePattern.FindStringSubmatch(query); len(match) > 1 {
		tableFilter = match[1]
	}
	result := emptyMetadata([]string{"TABLE_CATALOG", "TABLE_SCHEMA", "TABLE_NAME", "NON_UNIQUE", "INDEX_SCHEMA", "INDEX_NAME", "SEQ_IN_INDEX", "COLUMN_NAME", "COLLATION", "CARDINALITY", "SUB_PART", "PACKED", "NULLABLE", "INDEX_TYPE", "COMMENT", "INDEX_COMMENT", "IS_VISIBLE", "EXPRESSION"})
	for _, tableName := range database.ListTables() {
		if tableFilter != "" && !strings.EqualFold(tableFilter, tableName) {
			continue
		}
		if !engine.Users.HasObjectAccess(session.Username, session.Host, database.Name(), tableName) {
			continue
		}
		table, _ := database.Table(tableName)
		columns := table.Columns()
		for _, definition := range table.Indexes() {
			nonUnique := int64(1)
			if definition.Unique || definition.Primary || strings.EqualFold(definition.Name, "PRIMARY") {
				nonUnique = 0
			}
			indexName := definition.Name
			if definition.Primary || strings.EqualFold(definition.Name, "PRIMARY") {
				indexName = "PRIMARY"
			}
			for position, columnName := range definition.Columns {
				nullable := "YES"
				if columnPosition, ok := table.ColumnIndex(columnName); ok && !storage.ColumnNullable(columns[columnPosition]) {
					nullable = ""
				}
				result.Rows = append(result.Rows, []any{"def", database.Name(), table.Name(), nonUnique, database.Name(), indexName, int64(position + 1), columnName, "A", int64(table.RowCount()), nil, nil, nullable, "BTREE", "", "", "YES", nil})
			}
		}
	}
	return projectMetadataColumns(query, result), nil
}

func tableConstraintInformation(engine *executor.Engine, session *executor.Session, query string) (*executor.Result, error) {
	databaseName := session.CurrentDatabase
	if match := tableSchemaPattern.FindStringSubmatch(query); len(match) > 1 {
		databaseName = match[1]
	}
	database, err := engine.Store.Database(databaseName)
	if err != nil {
		return nil, err
	}
	tableFilter := ""
	if match := tableNamePattern.FindStringSubmatch(query); len(match) > 1 {
		tableFilter = match[1]
	}
	result := emptyMetadata([]string{"CONSTRAINT_CATALOG", "CONSTRAINT_SCHEMA", "CONSTRAINT_NAME", "TABLE_SCHEMA", "TABLE_NAME", "CONSTRAINT_TYPE", "ENFORCED"})
	for _, tableName := range database.ListTables() {
		if tableFilter != "" && !strings.EqualFold(tableFilter, tableName) {
			continue
		}
		if !engine.Users.HasObjectAccess(session.Username, session.Host, database.Name(), tableName) {
			continue
		}
		table, _ := database.Table(tableName)
		for _, definition := range table.Indexes() {
			constraintType := ""
			constraintName := definition.Name
			switch {
			case definition.Primary || strings.EqualFold(definition.Name, "PRIMARY"):
				constraintName, constraintType = "PRIMARY", "PRIMARY KEY"
			case definition.Unique:
				constraintType = "UNIQUE"
			}
			if constraintType != "" {
				result.Rows = append(result.Rows, []any{"def", database.Name(), constraintName, database.Name(), table.Name(), constraintType, "YES"})
			}
		}
		for _, definition := range table.ForeignKeys() {
			result.Rows = append(result.Rows, []any{"def", database.Name(), definition.Name, database.Name(), table.Name(), "FOREIGN KEY", "YES"})
		}
		for _, definition := range table.CheckConstraints() {
			result.Rows = append(result.Rows, []any{"def", database.Name(), definition.Name, database.Name(), table.Name(), "CHECK", "YES"})
		}
	}
	return projectMetadataColumns(query, result), nil
}

func referentialConstraintInformation(engine *executor.Engine, session *executor.Session, query string) (*executor.Result, error) {
	databaseName := session.CurrentDatabase
	if match := tableSchemaPattern.FindStringSubmatch(query); len(match) > 1 {
		databaseName = match[1]
	}
	database, err := engine.Store.Database(databaseName)
	if err != nil {
		return nil, err
	}
	result := emptyMetadata([]string{"CONSTRAINT_SCHEMA", "CONSTRAINT_NAME", "UNIQUE_CONSTRAINT_SCHEMA", "UNIQUE_CONSTRAINT_NAME", "MATCH_OPTION", "UPDATE_RULE", "DELETE_RULE", "TABLE_NAME", "REFERENCED_TABLE_NAME"})
	for _, tableName := range database.ListTables() {
		if !engine.Users.HasObjectAccess(session.Username, session.Host, database.Name(), tableName) {
			continue
		}
		table, _ := database.Table(tableName)
		for _, definition := range table.ForeignKeys() {
			referencedTable := definition.RefTable
			if dot := strings.LastIndex(referencedTable, "."); dot >= 0 {
				referencedTable = referencedTable[dot+1:]
			}
			updateRule, deleteRule := definition.OnUpdate, definition.OnDelete
			if updateRule == "" {
				updateRule = "RESTRICT"
			}
			if deleteRule == "" {
				deleteRule = "RESTRICT"
			}
			result.Rows = append(result.Rows, []any{database.Name(), definition.Name, database.Name(), "PRIMARY", "NONE", updateRule, deleteRule, table.Name(), referencedTable})
		}
	}
	return projectMetadataColumns(query, result), nil
}

func checkConstraintInformation(engine *executor.Engine, session *executor.Session, query string) (*executor.Result, error) {
	databaseName := session.CurrentDatabase
	if match := tableSchemaPattern.FindStringSubmatch(query); len(match) > 1 {
		databaseName = match[1]
	}
	database, err := engine.Store.Database(databaseName)
	if err != nil {
		return nil, err
	}
	result := emptyMetadata([]string{"CONSTRAINT_SCHEMA", "CONSTRAINT_NAME", "CHECK_CLAUSE"})
	for _, tableName := range database.ListTables() {
		if !engine.Users.HasObjectAccess(session.Username, session.Host, database.Name(), tableName) {
			continue
		}
		table, _ := database.Table(tableName)
		for _, definition := range table.CheckConstraints() {
			result.Rows = append(result.Rows, []any{database.Name(), definition.Name, definition.Expression})
		}
	}
	return projectMetadataColumns(query, result), nil
}

func keyColumnUsageInformation(engine *executor.Engine, session *executor.Session, query string) (*executor.Result, error) {
	databaseName := session.CurrentDatabase
	if match := tableSchemaPattern.FindStringSubmatch(query); len(match) > 1 {
		databaseName = match[1]
	}
	database, err := engine.Store.Database(databaseName)
	if err != nil {
		return nil, err
	}
	tableFilter := ""
	if match := tableNamePattern.FindStringSubmatch(query); len(match) > 1 {
		tableFilter = match[1]
	}
	result := emptyMetadata([]string{"CONSTRAINT_CATALOG", "CONSTRAINT_SCHEMA", "CONSTRAINT_NAME", "TABLE_CATALOG", "TABLE_SCHEMA", "TABLE_NAME", "COLUMN_NAME", "ORDINAL_POSITION", "POSITION_IN_UNIQUE_CONSTRAINT", "REFERENCED_TABLE_SCHEMA", "REFERENCED_TABLE_NAME", "REFERENCED_COLUMN_NAME"})
	for _, tableName := range database.ListTables() {
		if tableFilter != "" && !strings.EqualFold(tableFilter, tableName) {
			continue
		}
		if !engine.Users.HasObjectAccess(session.Username, session.Host, database.Name(), tableName) {
			continue
		}
		table, _ := database.Table(tableName)
		for _, definition := range table.Indexes() {
			if !definition.Unique && !definition.Primary && !strings.EqualFold(definition.Name, "PRIMARY") {
				continue
			}
			constraintName := definition.Name
			if definition.Primary || strings.EqualFold(definition.Name, "PRIMARY") {
				constraintName = "PRIMARY"
			}
			for position, columnName := range definition.Columns {
				result.Rows = append(result.Rows, []any{"def", database.Name(), constraintName, "def", database.Name(), table.Name(), columnName, int64(position + 1), nil, nil, nil, nil})
			}
		}
		for _, definition := range table.ForeignKeys() {
			referencedTable := definition.RefTable
			if dot := strings.LastIndex(referencedTable, "."); dot >= 0 {
				referencedTable = referencedTable[dot+1:]
			}
			for position, columnName := range definition.Columns {
				result.Rows = append(result.Rows, []any{"def", database.Name(), definition.Name, "def", database.Name(), table.Name(), columnName, int64(position + 1), int64(position + 1), database.Name(), referencedTable, definition.RefColumns[position]})
			}
		}
	}
	return projectMetadataColumns(query, result), nil
}

func viewColumns(engine *executor.Engine, session *executor.Session, databaseName, viewName string) ([]executor.Column, error) {
	query := fmt.Sprintf("SELECT * FROM `%s`.`%s` LIMIT 0", strings.ReplaceAll(databaseName, "`", "``"), strings.ReplaceAll(viewName, "`", "``"))
	result, err := engine.Execute(session, query)
	if err != nil {
		return nil, err
	}
	return result.Columns, nil
}

func viewInformation(engine *executor.Engine, session *executor.Session, query string) (*executor.Result, error) {
	databaseName := session.CurrentDatabase
	if match := tableSchemaPattern.FindStringSubmatch(query); len(match) > 1 {
		databaseName = match[1]
	}
	database, err := engine.Store.Database(databaseName)
	if err != nil {
		return nil, err
	}
	result := &executor.Result{Columns: []executor.Column{
		{Name: "TABLE_CATALOG", Type: storage.TypeVarchar},
		{Name: "TABLE_SCHEMA", Type: storage.TypeVarchar},
		{Name: "TABLE_NAME", Type: storage.TypeVarchar},
		{Name: "VIEW_DEFINITION", Type: storage.TypeText},
		{Name: "CHECK_OPTION", Type: storage.TypeVarchar},
		{Name: "IS_UPDATABLE", Type: storage.TypeVarchar},
		{Name: "DEFINER", Type: storage.TypeVarchar},
		{Name: "SECURITY_TYPE", Type: storage.TypeVarchar},
		{Name: "CHARACTER_SET_CLIENT", Type: storage.TypeVarchar},
		{Name: "COLLATION_CONNECTION", Type: storage.TypeVarchar},
	}}
	for _, name := range database.ListViews() {
		if !engine.Users.HasObjectAccess(session.Username, session.Host, database.Name(), name) {
			continue
		}
		view, _ := database.View(name)
		result.Rows = append(result.Rows, []any{"def", database.Name(), view.Name, view.Definition, "NONE", "NO", "root@%", "DEFINER", executor.DefaultCharacterSet, executor.DefaultCollation})
	}
	return projectMetadataColumns(query, result), nil
}

func mysqlUserInformation(engine *executor.Engine, session *executor.Session, query string) (*executor.Result, error) {
	if !canInspectAccounts(engine, session) {
		return nil, fmt.Errorf("access denied for user %q@%q to mysql.user", session.Username, session.Host)
	}
	result := &executor.Result{Columns: []executor.Column{
		{Name: "Host", Type: storage.TypeVarchar}, {Name: "User", Type: storage.TypeVarchar},
		{Name: "Select_priv", Type: storage.TypeVarchar}, {Name: "Insert_priv", Type: storage.TypeVarchar},
		{Name: "Update_priv", Type: storage.TypeVarchar}, {Name: "Delete_priv", Type: storage.TypeVarchar},
		{Name: "Create_priv", Type: storage.TypeVarchar}, {Name: "Drop_priv", Type: storage.TypeVarchar},
		{Name: "Grant_priv", Type: storage.TypeVarchar}, {Name: "Super_priv", Type: storage.TypeVarchar},
		{Name: "plugin", Type: storage.TypeVarchar}, {Name: "authentication_string", Type: storage.TypeText},
		{Name: "account_locked", Type: storage.TypeVarchar},
	}}
	yesNo := func(account catalog.Account, privilege string) string {
		if engine.Users.Allowed(account.Username, account.Host, privilege, "*", "*") {
			return "Y"
		}
		return "N"
	}
	for _, account := range engine.Users.Accounts() {
		grantPrivilege := "N"
		if engine.Users.CanGrant(account.Username, account.Host, "SELECT", "*", "*") {
			grantPrivilege = "Y"
		}
		result.Rows = append(result.Rows, []any{
			account.Host, account.Username,
			yesNo(account, "SELECT"), yesNo(account, "INSERT"), yesNo(account, "UPDATE"), yesNo(account, "DELETE"),
			yesNo(account, "CREATE"), yesNo(account, "DROP"), grantPrivilege, yesNo(account, "PROCESS"),
			"mysql_native_password", "", "N",
		})
	}
	return projectMetadataColumns(query, result), nil
}

func privilegeInformation(engine *executor.Engine, session *executor.Session, query, level string) *executor.Result {
	result := &executor.Result{Columns: []executor.Column{
		{Name: "GRANTEE", Type: storage.TypeVarchar}, {Name: "TABLE_CATALOG", Type: storage.TypeVarchar},
		{Name: "TABLE_SCHEMA", Type: storage.TypeVarchar}, {Name: "TABLE_NAME", Type: storage.TypeVarchar},
		{Name: "PRIVILEGE_TYPE", Type: storage.TypeVarchar}, {Name: "IS_GRANTABLE", Type: storage.TypeVarchar},
	}}
	accounts := engine.Users.Accounts()
	if !canInspectAccounts(engine, session) {
		accounts = []catalog.Account{{Username: session.Username, Host: session.Host}}
	}
	for _, account := range accounts {
		grants, exists := engine.Users.GrantsFor(account.Username, account.Host)
		if !exists {
			continue
		}
		for _, grant := range grants {
			matchesLevel := (level == "USER" && grant.Database == "*" && grant.Table == "*") ||
				(level == "SCHEMA" && grant.Database != "*" && grant.Table == "*") ||
				(level == "TABLE" && grant.Database != "*" && grant.Table != "*")
			if !matchesLevel {
				continue
			}
			for _, privilege := range expandMetadataPrivileges(grant.Privileges) {
				grantable := "NO"
				if grant.GrantOption {
					grantable = "YES"
				}
				result.Rows = append(result.Rows, []any{
					"'" + account.Username + "'@'" + account.Host + "'", "def", grant.Database, grant.Table, privilege, grantable,
				})
			}
		}
	}
	return projectMetadataColumns(query, result)
}

func expandMetadataPrivileges(privileges []string) []string {
	for _, privilege := range privileges {
		if privilege == "ALL" {
			return []string{"SELECT", "INSERT", "UPDATE", "DELETE", "CREATE", "DROP", "ALTER", "INDEX", "CREATE VIEW", "SHOW VIEW", "CREATE USER"}
		}
	}
	return privileges
}

func canInspectAccounts(engine *executor.Engine, session *executor.Session) bool {
	return session.Username == "" || engine.Users.Allowed(session.Username, session.Host, "CREATE USER", "*", "*") || engine.Users.Allowed(session.Username, session.Host, "SELECT", "mysql", "user")
}

func projectMetadataColumns(query string, result *executor.Result) *executor.Result {
	if statement, err := parser.Parse(query); err == nil {
		if selectStatement, ok := statement.(parser.Select); ok && len(selectStatement.Items) == 1 {
			item := selectStatement.Items[0]
			if expression, parseErr := parser.ParseExpression(item.Expression); parseErr == nil {
				if function, isFunction := expression.(parser.FunctionExpr); isFunction && strings.EqualFold(function.Name, "COUNT") && function.Star {
					name := item.Alias
					if name == "" {
						name = item.Expression
					}
					return &executor.Result{
						Columns: []executor.Column{{Name: name, Type: storage.TypeBigInt}},
						Rows:    [][]any{{int64(len(result.Rows))}},
					}
				}
			}
		}
	}
	upper := strings.ToUpper(query)
	selectPosition := strings.Index(upper, "SELECT ")
	fromPosition := strings.Index(upper, " FROM ")
	if selectPosition < 0 || fromPosition <= selectPosition || strings.Contains(upper[selectPosition:fromPosition], "*") {
		return result
	}
	expressions := strings.Split(query[selectPosition+len("SELECT "):fromPosition], ",")
	positions := make([]int, 0, len(expressions))
	columns := make([]executor.Column, 0, len(expressions))
	for _, expression := range expressions {
		name := strings.TrimSpace(expression)
		name = strings.TrimSpace(strings.TrimPrefix(strings.ToUpper(name), "DISTINCT "))
		if open := strings.IndexByte(name, '('); open >= 0 && strings.HasSuffix(name, ")") {
			name = strings.TrimSpace(name[open+1 : len(name)-1])
		}
		if as := strings.Index(name, " AS "); as >= 0 {
			name = strings.TrimSpace(name[:as])
		}
		name = strings.Trim(name, "`")
		if dot := strings.LastIndexByte(name, '.'); dot >= 0 {
			name = strings.Trim(name[dot+1:], "`")
		}
		position := -1
		for index, column := range result.Columns {
			if strings.EqualFold(column.Name, name) {
				position = index
				columns = append(columns, column)
				break
			}
		}
		if position < 0 {
			return result
		}
		positions = append(positions, position)
	}
	projected := &executor.Result{Columns: columns, Rows: make([][]any, len(result.Rows))}
	for rowIndex, row := range result.Rows {
		projected.Rows[rowIndex] = make([]any, len(positions))
		for columnIndex, position := range positions {
			projected.Rows[rowIndex][columnIndex] = row[position]
		}
	}
	return projected
}

func navicatColumnType(column storage.Column) string {
	return storage.ColumnSQLType(column)
}
func tableStatus(engine *executor.Engine, session *executor.Session, query string) (*executor.Result, error) {
	databaseName := session.CurrentDatabase
	remainder := strings.TrimSpace(query[len("SHOW TABLE STATUS"):])
	if rest, ok := consumeShowKeyword(remainder, "FROM"); ok {
		name, remaining, parseErr := consumeShowIdentifier(rest)
		if parseErr != nil {
			return nil, parseErr
		}
		databaseName = name
		remainder = strings.TrimSpace(remaining)
	} else if rest, ok := consumeShowKeyword(remainder, "IN"); ok {
		name, remaining, parseErr := consumeShowIdentifier(rest)
		if parseErr != nil {
			return nil, parseErr
		}
		databaseName = name
		remainder = strings.TrimSpace(remaining)
	}
	likePattern := ""
	hasLikePattern := false
	if rest, ok := consumeShowKeyword(remainder, "LIKE"); ok {
		var parseErr error
		likePattern, remainder, parseErr = consumeShowString(rest)
		if parseErr != nil {
			return nil, parseErr
		}
		hasLikePattern = true
	}
	if strings.TrimSpace(remainder) != "" {
		return nil, fmt.Errorf("unsupported SHOW TABLE STATUS clause: %s", remainder)
	}
	database, err := engine.Store.Database(databaseName)
	if err != nil {
		return nil, err
	}
	result := &executor.Result{Columns: []executor.Column{
		{Name: "Name", Type: storage.TypeVarchar}, {Name: "Engine", Type: storage.TypeVarchar},
		{Name: "Version", Type: storage.TypeBigInt}, {Name: "Row_format", Type: storage.TypeVarchar},
		{Name: "Rows", Type: storage.TypeBigInt}, {Name: "Avg_row_length", Type: storage.TypeBigInt},
		{Name: "Data_length", Type: storage.TypeBigInt}, {Name: "Max_data_length", Type: storage.TypeBigInt},
		{Name: "Index_length", Type: storage.TypeBigInt}, {Name: "Data_free", Type: storage.TypeBigInt},
		{Name: "Auto_increment", Type: storage.TypeBigInt}, {Name: "Create_time", Type: storage.TypeDateTime},
		{Name: "Update_time", Type: storage.TypeDateTime}, {Name: "Check_time", Type: storage.TypeDateTime},
		{Name: "Collation", Type: storage.TypeVarchar}, {Name: "Checksum", Type: storage.TypeBigInt},
		{Name: "Create_options", Type: storage.TypeVarchar}, {Name: "Comment", Type: storage.TypeText},
	}}
	for _, name := range database.ListRelations() {
		if hasLikePattern && !showLikeMatch(name, likePattern) {
			continue
		}
		if !engine.Users.HasObjectAccess(session.Username, session.Host, database.Name(), name) {
			continue
		}
		if _, viewErr := database.View(name); viewErr == nil {
			result.Rows = append(result.Rows, []any{
				name, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, "VIEW",
			})
			continue
		}
		table, _ := database.Table(name)
		comment, createdAt, updatedAt := table.Metadata()
		createdAt = createdAt.Local().Truncate(time.Second)
		updatedAt = updatedAt.Local().Truncate(time.Second)
		rows := int64(table.RowCount())
		dataLength := table.DataLength()
		averageLength := int64(0)
		if rows > 0 {
			averageLength = dataLength / rows
		}
		result.Rows = append(result.Rows, []any{name, "GBaseLite", int64(10), "Dynamic", rows, averageLength, dataLength, int64(0), int64(0), int64(0), nil, createdAt, updatedAt, nil, "utf8mb4_general_ci", nil, "", comment})
	}
	return result, nil
}
