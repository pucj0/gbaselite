package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"gbaselite/executor"
	driver "github.com/go-sql-driver/mysql"
)

type clientOptions struct {
	Host           string
	Port           int
	User           string
	Password       string
	Database       string
	Execute        string
	PromptPassword bool
}

func isClientInvocation(args []string) bool {
	if len(args) == 0 {
		return false
	}
	argument := args[0]
	for _, prefix := range []string{"-u", "--user", "-p", "--password", "-h", "--host", "-P", "--port", "-D", "--database", "-e", "--execute", "--protocol"} {
		if argument == prefix || strings.HasPrefix(argument, prefix+"=") || (len(prefix) == 2 && strings.HasPrefix(argument, prefix) && len(argument) > 2) {
			return true
		}
	}
	return false
}

func parseClientOptions(args []string) (clientOptions, error) {
	options := clientOptions{Host: "127.0.0.1", Port: 3307, User: "root"}
	nextValue := func(index *int, option string) (string, error) {
		if *index+1 >= len(args) {
			return "", fmt.Errorf("%s requires a value", option)
		}
		*index++
		return args[*index], nil
	}
	for index := 0; index < len(args); index++ {
		argument := args[index]
		switch {
		case argument == "-u" || argument == "--user":
			value, err := nextValue(&index, argument)
			if err != nil {
				return options, err
			}
			options.User = value
		case strings.HasPrefix(argument, "--user="):
			options.User = strings.TrimPrefix(argument, "--user=")
		case strings.HasPrefix(argument, "-u") && len(argument) > 2:
			options.User = argument[2:]
		case argument == "-p" || argument == "--password":
			options.Password = ""
			options.PromptPassword = true
		case strings.HasPrefix(argument, "--password="):
			options.Password = strings.TrimPrefix(argument, "--password=")
			options.PromptPassword = false
		case strings.HasPrefix(argument, "-p") && len(argument) > 2:
			options.Password = argument[2:]
			options.PromptPassword = false
		case argument == "-h" || argument == "--host":
			value, err := nextValue(&index, argument)
			if err != nil {
				return options, err
			}
			options.Host = value
		case strings.HasPrefix(argument, "--host="):
			options.Host = strings.TrimPrefix(argument, "--host=")
		case strings.HasPrefix(argument, "-h") && len(argument) > 2:
			options.Host = argument[2:]
		case argument == "-P" || argument == "--port":
			value, err := nextValue(&index, argument)
			if err != nil {
				return options, err
			}
			port, err := parseClientPort(value)
			if err != nil {
				return options, err
			}
			options.Port = port
		case strings.HasPrefix(argument, "--port="):
			port, err := parseClientPort(strings.TrimPrefix(argument, "--port="))
			if err != nil {
				return options, err
			}
			options.Port = port
		case strings.HasPrefix(argument, "-P") && len(argument) > 2:
			port, err := parseClientPort(argument[2:])
			if err != nil {
				return options, err
			}
			options.Port = port
		case argument == "-D" || argument == "--database":
			value, err := nextValue(&index, argument)
			if err != nil {
				return options, err
			}
			options.Database = value
		case strings.HasPrefix(argument, "--database="):
			options.Database = strings.TrimPrefix(argument, "--database=")
		case strings.HasPrefix(argument, "-D") && len(argument) > 2:
			options.Database = argument[2:]
		case argument == "-e" || argument == "--execute":
			value, err := nextValue(&index, argument)
			if err != nil {
				return options, err
			}
			options.Execute = value
		case strings.HasPrefix(argument, "--execute="):
			options.Execute = strings.TrimPrefix(argument, "--execute=")
		case strings.HasPrefix(argument, "-e") && len(argument) > 2:
			options.Execute = argument[2:]
		case argument == "--protocol=tcp":
		case strings.HasPrefix(argument, "-"):
			return options, fmt.Errorf("unknown client option %q", argument)
		case options.Database == "":
			options.Database = argument
		default:
			return options, fmt.Errorf("unexpected argument %q", argument)
		}
	}
	if options.Host == "" || options.User == "" {
		return options, errors.New("host and user must not be empty")
	}
	return options, nil
}

func parseClientPort(value string) (int, error) {
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("invalid port %q", value)
	}
	return port, nil
}

func runClient(args []string) error {
	options, err := parseClientOptions(args)
	if err != nil {
		return err
	}
	if options.PromptPassword {
		fmt.Fprint(os.Stderr, "Enter password: ")
		options.Password, err = readPassword(os.Stdin)
		if err != nil {
			return fmt.Errorf("read password: %w", err)
		}
	}
	configuration := driver.NewConfig()
	configuration.User = options.User
	configuration.Passwd = options.Password
	configuration.Net = "tcp"
	configuration.Addr = net.JoinHostPort(options.Host, strconv.Itoa(options.Port))
	configuration.DBName = options.Database
	configuration.Params = map[string]string{"charset": "utf8mb4"}
	connector, err := driver.NewConnector(configuration)
	if err != nil {
		return err
	}
	database := sql.OpenDB(connector)
	database.SetMaxOpenConns(1)
	defer database.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := database.PingContext(ctx); err != nil {
		return fmt.Errorf("connect to %s: %w", configuration.Addr, err)
	}
	if options.Execute != "" {
		return executeClientStatement(database, options.Execute, os.Stdout)
	}
	fmt.Fprintf(os.Stdout, "GBaseLite %s\n", executor.Version)
	fmt.Fprintf(os.Stdout, "  Connection  %s@%s\n", options.User, configuration.Addr)
	currentDatabase := options.Database
	if currentDatabase == "" {
		currentDatabase = "(none)"
	}
	fmt.Fprintf(os.Stdout, "  Database    %s\n\n", currentDatabase)
	return runClientConsole(database, os.Stdin, os.Stdout, options.Database)
}

func runClientConsole(database *sql.DB, input io.Reader, output io.Writer, currentDatabase string) error {
	scanner := bufio.NewScanner(input)
	var statement strings.Builder
	for {
		if statement.Len() == 0 {
			promptDatabase := currentDatabase
			if promptDatabase == "" {
				promptDatabase = "none"
			}
			fmt.Fprintf(output, "gbaselite [%s]> ", promptDatabase)
		} else {
			fmt.Fprint(output, "               -> ")
		}
		if !scanner.Scan() {
			return scanner.Err()
		}
		line := scanner.Text()
		if statement.Len() == 0 {
			line = strings.TrimPrefix(line, "\ufeff")
		}
		trimmed := strings.TrimSpace(line)
		if statement.Len() == 0 && (strings.EqualFold(trimmed, "exit") || strings.EqualFold(trimmed, "quit") || trimmed == "\\q") {
			return nil
		}
		statement.WriteString(line)
		statement.WriteByte('\n')
		if !strings.Contains(line, ";") {
			continue
		}
		query := strings.TrimSpace(statement.String())
		statement.Reset()
		if err := executeClientStatement(database, query, output); err != nil {
			fmt.Fprintln(output, formatClientConsoleError(err))
			fmt.Fprintln(output)
			continue
		}
		if selected, ok := clientSelectedDatabase(query); ok {
			currentDatabase = selected
		}
	}
}

func executeClientStatement(database *sql.DB, statement string, output io.Writer) error {
	started := time.Now()
	if clientStatementReturnsRows(statement) {
		rows, err := database.Query(statement)
		if err != nil {
			return clientTimedError(err, started)
		}
		defer rows.Close()
		count, err := printClientRows(rows, output)
		if err != nil {
			return clientTimedError(err, started)
		}
		fmt.Fprintf(output, "%s (%s)\n\n", clientRowCountText(count), formatClientDuration(time.Since(started)))
		return nil
	}
	result, err := database.Exec(statement)
	if err != nil {
		return clientTimedError(err, started)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		affected = 0
	}
	if _, ok := clientSelectedDatabase(statement); ok {
		fmt.Fprintf(output, "Database changed (%s)\n\n", formatClientDuration(time.Since(started)))
		return nil
	}
	fmt.Fprintf(output, "Query OK, %d rows affected (%s)\n\n", affected, formatClientDuration(time.Since(started)))
	return nil
}

func clientStatementReturnsRows(statement string) bool {
	fields := strings.Fields(strings.TrimSpace(statement))
	if len(fields) == 0 {
		return false
	}
	switch strings.ToUpper(fields[0]) {
	case "SELECT", "SHOW", "DESCRIBE", "DESC", "EXPLAIN", "WITH":
		return true
	default:
		return false
	}
}

func printClientRows(rows *sql.Rows, output io.Writer) (int, error) {
	columns, err := rows.Columns()
	if err != nil {
		return 0, err
	}
	values := make([]any, len(columns))
	destinations := make([]any, len(columns))
	for index := range values {
		destinations[index] = &values[index]
	}
	count := 0
	tableRows := make([][]string, 0)
	for rows.Next() {
		if err := rows.Scan(destinations...); err != nil {
			return 0, err
		}
		formatted := make([]string, len(values))
		for index, value := range values {
			formatted[index] = formatClientValue(value)
		}
		tableRows = append(tableRows, formatted)
		count++
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	writeClientTable(output, columns, tableRows)
	return count, nil
}

func formatClientValue(value any) string {
	var text string
	switch typed := value.(type) {
	case nil:
		return "NULL"
	case []byte:
		text = string(typed)
	default:
		text = fmt.Sprint(typed)
	}
	return strings.NewReplacer("\r", `\r`, "\n", `\n`, "\t", `\t`).Replace(text)
}

func writeClientTable(output io.Writer, columns []string, rows [][]string) {
	widths := make([]int, len(columns))
	for index, column := range columns {
		widths[index] = clientDisplayWidth(column)
	}
	for _, row := range rows {
		for index := range widths {
			if index < len(row) && clientDisplayWidth(row[index]) > widths[index] {
				widths[index] = clientDisplayWidth(row[index])
			}
		}
	}
	border := clientTableBorder(widths)
	fmt.Fprintln(output, border)
	writeClientTableRow(output, columns, widths)
	fmt.Fprintln(output, border)
	for _, row := range rows {
		writeClientTableRow(output, row, widths)
	}
	fmt.Fprintln(output, border)
}

func clientTableBorder(widths []int) string {
	var border strings.Builder
	border.WriteByte('+')
	for _, width := range widths {
		border.WriteString(strings.Repeat("-", width+2))
		border.WriteByte('+')
	}
	return border.String()
}

func writeClientTableRow(output io.Writer, values []string, widths []int) {
	fmt.Fprint(output, "|")
	for index, width := range widths {
		value := ""
		if index < len(values) {
			value = values[index]
		}
		fmt.Fprintf(output, " %s%s |", value, strings.Repeat(" ", width-clientDisplayWidth(value)))
	}
	fmt.Fprintln(output)
}

func clientDisplayWidth(value string) int {
	width := 0
	for _, character := range value {
		switch {
		case unicode.IsControl(character), unicode.Is(unicode.Mn, character), unicode.Is(unicode.Me, character):
		case unicode.Is(unicode.Han, character), unicode.Is(unicode.Hangul, character),
			unicode.Is(unicode.Hiragana, character), unicode.Is(unicode.Katakana, character),
			character >= 0xFF01 && character <= 0xFF60,
			character >= 0xFFE0 && character <= 0xFFE6:
			width += 2
		default:
			width++
		}
	}
	return width
}

func formatClientDuration(duration time.Duration) string {
	if duration < time.Microsecond {
		return "<0.001 ms"
	}
	if duration < time.Second {
		return fmt.Sprintf("%.3f ms", float64(duration)/float64(time.Millisecond))
	}
	return fmt.Sprintf("%.3f sec", duration.Seconds())
}

func clientRowCountText(count int) string {
	if count == 1 {
		return "1 row in set"
	}
	return fmt.Sprintf("%d rows in set", count)
}

func clientTimedError(err error, started time.Time) error {
	return fmt.Errorf("%w (%s)", err, formatClientDuration(time.Since(started)))
}

func formatClientConsoleError(err error) string {
	message := err.Error()
	if strings.HasPrefix(message, "Error ") {
		return "ERROR " + strings.TrimPrefix(message, "Error ")
	}
	return "ERROR: " + message
}

func clientSelectedDatabase(statement string) (string, bool) {
	trimmed := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(statement), ";"))
	fields := strings.Fields(trimmed)
	if len(fields) != 2 || !strings.EqualFold(fields[0], "USE") {
		return "", false
	}
	name := fields[1]
	if len(name) >= 2 && name[0] == '`' && name[len(name)-1] == '`' {
		name = strings.ReplaceAll(name[1:len(name)-1], "``", "`")
	}
	return name, name != ""
}

func readLineFromFile(file *os.File) (string, error) {
	var value strings.Builder
	buffer := make([]byte, 1)
	for {
		count, err := file.Read(buffer)
		if count > 0 {
			if buffer[0] == '\n' {
				return strings.TrimSuffix(value.String(), "\r"), nil
			}
			value.WriteByte(buffer[0])
		}
		if err != nil {
			if errors.Is(err, io.EOF) && value.Len() > 0 {
				return strings.TrimSuffix(value.String(), "\r"), nil
			}
			return "", err
		}
	}
}
