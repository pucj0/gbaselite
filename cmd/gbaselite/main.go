package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gbaselite/config"
	"gbaselite/executor"
	"gbaselite/internal/rotatinglog"
	"gbaselite/journal"
	gbmysql "gbaselite/mysql"
	"gbaselite/server"
)

func main() {
	configureConsoleEncoding()
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
func run(args []string) error {
	if isClientInvocation(args) {
		return runClient(args)
	}
	command := "start"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command = args[0]
		args = args[1:]
	}
	switch command {
	case "server":
		return runServer(args)
	case "service":
		return runWindowsService(args)
	case "start":
		return runStart(args)
	case "stop":
		return runStop(args)
	case "restart":
		if err := runStop(args); err != nil {
			return err
		}
		return runStart(args)
	case "shell":
		return runShell(args)
	case "client", "connect":
		return runClient(args)
	case "import":
		if len(args) == 0 || args[0] != "mysql" {
			return fmt.Errorf("usage: gbaselite import mysql [options]")
		}
		return runImport(args[1:])
	case "export":
		if len(args) == 0 || args[0] != "mysql" {
			return fmt.Errorf("usage: gbaselite export mysql [options]")
		}
		return runExport(args[1:])
	case "backup":
		return runBackup(args)
	case "restore":
		return runRestore(args)
	case "replay-binlog":
		return runReplayBinlog(args)
	case "healthcheck":
		return runHealthcheck(args)
	case "diagnose":
		return runDiagnose(args)
	case "inspect-snapshot":
		return runInspectSnapshot(args)
	case "inspect-instance":
		return runInspectInstance(args)
	case "version":
		fmt.Println("GBaseLite", executor.Version)
		return nil
	case "help", "--help", "-h":
		printHelp()
		return nil
	default:
		return fmt.Errorf("unknown command %q", command)
	}
}

func runServer(args []string) error {
	return runServerControlled(args, nil, nil)
}

func runServerControlled(args []string, externalStop <-chan struct{}, ready chan<- struct{}) error {
	flags := flag.NewFlagSet("server", flag.ContinueOnError)
	configPath := flags.String("config", "config.yaml", "configuration file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	logger, closeLog, err := newLogger(cfg.Log.Path, cfg.Log.MaxSizeMB, cfg.Log.RetentionDays)
	if err != nil {
		return err
	}
	defer closeLog()
	startupFailure := func(err error) error {
		logger.Printf("startup failed: %v", err)
		return err
	}
	tlsConfig, err := cfg.ServerTLSConfig()
	if err != nil {
		return startupFailure(err)
	}
	listener, err := net.Listen("tcp", cfg.Address())
	if err != nil {
		return startupFailure(fmt.Errorf("listen on %s: %w", cfg.Address(), err))
	}
	defer listener.Close()
	engine, err := executor.Open(cfg.Storage.Path, cfg.Auth.Username, cfg.Auth.Password)
	if err != nil {
		return startupFailure(err)
	}
	var auditLog *journal.AuditLog
	if cfg.Audit.Enabled {
		auditLog, err = journal.OpenAudit(cfg.AuditPath(), cfg.Audit.RetentionDays)
		if err != nil {
			return startupFailure(fmt.Errorf("open audit log: %w", err))
		}
		defer auditLog.Close()
	}
	var binlog *journal.Binlog
	if cfg.Binlog.Enabled {
		binlog, err = journal.OpenBinlog(cfg.BinlogPath(), cfg.Binlog.RetentionDays)
		if err != nil {
			return startupFailure(fmt.Errorf("open binlog: %w", err))
		}
		defer binlog.Close()
		engine.SetBinlog(binlog)
	}
	pidPath := filepath.Join(cfg.Storage.Path, "gbaselite.pid")
	if err := claimPIDFile(pidPath); err != nil {
		return startupFailure(err)
	}
	defer os.Remove(pidPath)
	mysqlServer := &server.MySQLServer{
		Address:                cfg.Address(),
		Engine:                 engine,
		Logger:                 logger,
		MaxConnections:         cfg.Server.MaxConnections,
		WriteBufferSize:        cfg.Server.WriteBufferSize,
		SlowQuery:              cfg.Server.SlowQuery,
		DefaultTimeZone:        cfg.Server.TimeZone,
		Audit:                  auditLog,
		AuthFailureLimit:       cfg.Security.LoginFailureLimit,
		AuthFailureWindow:      cfg.Security.LoginFailureWindow,
		AuthFailureBlock:       cfg.Security.LoginFailureBlock,
		TLSConfig:              tlsConfig,
		RequireSecureTransport: cfg.TLS.RequireSecureTransport,
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	go func() {
		select {
		case <-signals:
		case <-externalStop:
		}
		logger.Print("shutdown signal received")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = mysqlServer.Shutdown(ctx)
	}()
	logger.Printf("GBaseLite %s starting data=%s", executor.Version, cfg.Storage.Path)
	if ready != nil {
		close(ready)
	}
	return mysqlServer.Serve(listener)
}

func runStart(args []string) error {
	flags := flag.NewFlagSet("start", flag.ContinueOnError)
	configPath := flags.String("config", "config.yaml", "configuration file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	absConfig, err := filepath.Abs(*configPath)
	if err != nil {
		return err
	}
	cfg, err := config.Load(absConfig)
	if err != nil {
		return err
	}
	pidPath := filepath.Join(cfg.Storage.Path, "gbaselite.pid")
	if pid, ok := readRunningPID(pidPath); ok {
		fmt.Printf("GBaseLite is already running (PID %d); restarting\n", pid)
		if err := runStop([]string{"--config", absConfig}); err != nil {
			return err
		}
	}
	host := cfg.Server.Host
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	address := net.JoinHostPort(host, strconv.Itoa(cfg.Server.Port))
	portDeadline := time.Now().Add(3 * time.Second)
	for {
		connection, dialErr := net.DialTimeout("tcp", address, 200*time.Millisecond)
		if dialErr != nil {
			break
		}
		_ = connection.Close()
		if time.Now().After(portDeadline) {
			return fmt.Errorf("cannot start GBaseLite: %s is already in use", address)
		}
		time.Sleep(100 * time.Millisecond)
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	command := exec.Command(executable, "server", "--config", absConfig)
	configureDetached(command)
	logPath := filepath.Join(cfg.Log.Path, "gbaselite.log")
	logOffset := fileSize(logPath)
	if err := command.Start(); err != nil {
		return err
	}
	pid := command.Process.Pid
	_ = command.Process.Release()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processIsAlive(pid) {
			return backgroundStartupError(pid, "exited during startup", logPath, logOffset)
		}
		connection, dialErr := net.DialTimeout("tcp", address, 200*time.Millisecond)
		pidInFile, pidMatches := readRunningPID(pidPath)
		if dialErr == nil && pidMatches && pidInFile == pid {
			_ = connection.Close()
			fmt.Printf("GBaseLite started (PID %d, %s)\n", pid, address)
			return nil
		}
		if dialErr == nil {
			_ = connection.Close()
		}
		time.Sleep(100 * time.Millisecond)
	}
	return backgroundStartupError(pid, "did not become ready", logPath, logOffset)
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func backgroundStartupError(pid int, summary, logPath string, offset int64) error {
	if detail := lastLogEntrySince(logPath, offset); detail != "" {
		return fmt.Errorf("GBaseLite process %d %s: %s", pid, summary, detail)
	}
	return fmt.Errorf("GBaseLite process %d %s; check %s", pid, summary, logPath)
}

func lastLogEntrySince(path string, offset int64) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() <= offset {
		return ""
	}
	if offset < 0 || offset > info.Size() {
		offset = 0
	}
	const maximumRead = int64(64 << 10)
	if info.Size()-offset > maximumRead {
		offset = info.Size() - maximumRead
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return ""
	}
	contents, err := io.ReadAll(file)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(contents)), "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		if line := strings.TrimSpace(lines[index]); line != "" {
			return line
		}
	}
	return ""
}

func runStop(args []string) error {
	flags := flag.NewFlagSet("stop", flag.ContinueOnError)
	configPath := flags.String("config", "config.yaml", "configuration file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	pidPath := filepath.Join(cfg.Storage.Path, "gbaselite.pid")
	pid, ok := readRunningPID(pidPath)
	if !ok {
		_ = os.Remove(pidPath)
		fmt.Println("GBaseLite is not running")
		return nil
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := terminateProcess(process); err != nil && processIsAlive(pid) {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for processIsAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if processIsAlive(pid) {
		return fmt.Errorf("timed out stopping GBaseLite PID %d", pid)
	}
	_ = os.Remove(pidPath)
	fmt.Printf("GBaseLite stopped (PID %d)\n", pid)
	return nil
}

func claimPIDFile(path string) error {
	currentPID := os.Getpid()
	if pid, ok := readRunningPID(path); ok && pid != currentPID {
		return fmt.Errorf("GBaseLite is already running (PID %d)", pid)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.Itoa(currentPID)), 0o600)
}

func readRunningPID(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, processIsAlive(pid)
}
func runShell(args []string) error {
	flags := flag.NewFlagSet("shell", flag.ContinueOnError)
	configPath := flags.String("config", "config.yaml", "configuration file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	engine, err := executor.Open(cfg.Storage.Path, cfg.Auth.Username, cfg.Auth.Password)
	if err != nil {
		return err
	}
	defer engine.Close()
	session := &executor.Session{}
	scanner := bufio.NewScanner(os.Stdin)
	var statement strings.Builder
	for {
		fmt.Print("gbase> ")
		if !scanner.Scan() {
			break
		}
		statement.WriteString(scanner.Text())
		statement.WriteByte('\n')
		if !strings.Contains(scanner.Text(), ";") {
			continue
		}
		result, err := engine.Execute(session, statement.String())
		statement.Reset()
		if err != nil {
			fmt.Println("ERROR:", err)
			continue
		}
		printResult(result)
	}
	return scanner.Err()
}
func runImport(args []string) error {
	flags := flag.NewFlagSet("import mysql", flag.ContinueOnError)
	configPath := flags.String("config", "config.yaml", "GBaseLite configuration")
	host := flags.String("host", "127.0.0.1", "MySQL host")
	port := flags.Int("port", 3306, "MySQL port")
	user := flags.String("user", "root", "MySQL user")
	password := flags.String("password", "", "MySQL password")
	database := flags.String("database", "", "database to import")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *database == "" {
		return fmt.Errorf("--database is required")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	engine, err := executor.Open(cfg.Storage.Path, cfg.Auth.Username, cfg.Auth.Password)
	if err != nil {
		return err
	}
	count, err := gbmysql.Import(engine.Store, gbmysql.ImportOptions{Host: *host, Port: *port, User: *user, Password: *password, Database: *database})
	if err == nil {
		err = engine.Close()
	}
	if err == nil {
		fmt.Printf("imported %d rows into %s\n", count, *database)
	}
	return err
}
func runExport(args []string) error {
	flags := flag.NewFlagSet("export mysql", flag.ContinueOnError)
	configPath := flags.String("config", "config.yaml", "GBaseLite configuration")
	database := flags.String("database", "", "database to export")
	output := flags.String("output", "backup.sql", "output SQL file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *database == "" {
		return fmt.Errorf("--database is required")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	engine, err := executor.Open(cfg.Storage.Path, cfg.Auth.Username, cfg.Auth.Password)
	if err != nil {
		return err
	}
	if err = gbmysql.Export(engine.Store, *database, *output); err == nil {
		fmt.Println("exported", *output)
	}
	return err
}

func runBackup(args []string) error {
	flags := flag.NewFlagSet("backup", flag.ContinueOnError)
	configPath := flags.String("config", "config.yaml", "GBaseLite configuration")
	database := flags.String("database", "", "database to back up")
	allDatabases := flags.Bool("all-databases", false, "back up every database")
	output := flags.String("output", "backup.sql", "output SQL file")
	schemaOnly := flags.Bool("no-data", false, "write schema without table rows")
	dataOnly := flags.Bool("no-create-info", false, "write table rows without schema")
	addDropDatabase := flags.Bool("add-drop-database", false, "write DROP DATABASE before CREATE DATABASE")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *allDatabases == (*database != "") {
		return fmt.Errorf("specify exactly one of --database or --all-databases")
	}
	if *schemaOnly && *dataOnly {
		return fmt.Errorf("--no-data and --no-create-info cannot be combined")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	engine, err := executor.Open(cfg.Storage.Path, cfg.Auth.Username, cfg.Auth.Password)
	if err != nil {
		return err
	}
	options := executor.BackupOptions{SchemaOnly: *schemaOnly, DataOnly: *dataOnly, AddDropDatabase: *addDropDatabase}
	if !*allDatabases {
		options.Databases = []string{*database}
	}
	if err := executor.BackupSQL(engine.Store, *output, options); err != nil {
		return err
	}
	fmt.Println("backup written to", *output)
	return nil
}

func runRestore(args []string) error {
	flags := flag.NewFlagSet("restore", flag.ContinueOnError)
	configPath := flags.String("config", "config.yaml", "GBaseLite configuration")
	input := flags.String("input", "", "SQL backup file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *input == "" {
		return fmt.Errorf("--input is required")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	pidPath := filepath.Join(cfg.Storage.Path, "gbaselite.pid")
	if pid, running := readRunningPID(pidPath); running {
		return fmt.Errorf("stop GBaseLite before restore (currently PID %d)", pid)
	}
	engine, err := executor.Open(cfg.Storage.Path, cfg.Auth.Username, cfg.Auth.Password)
	if err != nil {
		return err
	}
	executed, err := gbmysql.RestoreSQL(engine, *input)
	if err != nil {
		return err
	}
	if err := engine.Close(); err != nil {
		return err
	}
	fmt.Printf("restored %d statements from %s\n", executed, *input)
	return nil
}

func runReplayBinlog(args []string) error {
	flags := flag.NewFlagSet("replay-binlog", flag.ContinueOnError)
	configPath := flags.String("config", "config.yaml", "GBaseLite configuration")
	input := flags.String("input", "", "binlog JSONL file; defaults to the configured binlog path")
	afterSequence := flags.Uint64("after-sequence", 0, "replay records after this sequence")
	untilValue := flags.String("until", "", "replay records at or before this RFC3339 timestamp")
	checkOnly := flags.Bool("check-only", false, "validate records without opening or modifying the data directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	var until time.Time
	var err error
	if *untilValue != "" {
		until, err = time.Parse(time.RFC3339Nano, *untilValue)
		if err != nil {
			return fmt.Errorf("invalid --until timestamp: %w", err)
		}
	}
	var cfg config.Config
	if !*checkOnly || *input == "" {
		cfg, err = config.Load(*configPath)
		if err != nil {
			return err
		}
	}
	if *input == "" {
		*input = cfg.BinlogPath()
	}
	options := journal.ReplayOptions{AfterSequence: *afterSequence, Until: until}
	if *checkOnly {
		validated, last, err := journal.ReadBinlog(*input, options, func(journal.BinlogRecord) error { return nil })
		if err != nil {
			return fmt.Errorf("validate binlog: %w", err)
		}
		if validated == 0 {
			fmt.Println("binlog valid: 0 transactions selected")
		} else {
			fmt.Printf("binlog valid: %d transactions selected through sequence %d\n", validated, last)
		}
		return nil
	}
	pidPath := filepath.Join(cfg.Storage.Path, "gbaselite.pid")
	if pid, running := readRunningPID(pidPath); running {
		return fmt.Errorf("stop GBaseLite before replaying binlog (currently PID %d)", pid)
	}
	engine, err := executor.Open(cfg.Storage.Path, cfg.Auth.Username, cfg.Auth.Password)
	if err != nil {
		return err
	}
	sessions := make(map[string]*executor.Session)
	applied, last, replayErr := journal.ReadBinlog(*input, options, func(record journal.BinlogRecord) error {
		sessionKey := record.SessionID
		if sessionKey == "" {
			sessionKey = record.Transaction
		}
		session := sessions[sessionKey]
		if session == nil {
			session = &executor.Session{}
			sessions[sessionKey] = session
		}
		session.ReplayTimestamp = record.Timestamp
		if _, err := engine.Execute(session, "BEGIN"); err != nil {
			return err
		}
		for _, statement := range record.Statements {
			session.CurrentDatabase = statement.Database
			if _, err := engine.Execute(session, statement.SQL); err != nil {
				_, _ = engine.Execute(session, "ROLLBACK")
				return err
			}
		}
		if _, err := engine.Execute(session, "COMMIT"); err != nil {
			return err
		}
		return nil
	})
	if replayErr != nil {
		_ = engine.Close()
		return replayErr
	}
	if err := engine.Close(); err != nil {
		return err
	}
	fmt.Printf("replayed %d transactions through binlog sequence %d\n", applied, last)
	return nil
}
func runHealthcheck(args []string) error {
	flags := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	host := flags.String("host", "127.0.0.1", "server host")
	port := flags.Int("port", 3307, "server port")
	if err := flags.Parse(args); err != nil {
		return err
	}
	connection, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", *host, *port), 2*time.Second)
	if err != nil {
		return err
	}
	return connection.Close()
}
func newLogger(directory string, maxSizeMB, retentionDays int) (*log.Logger, func(), error) {
	file, err := rotatinglog.Open(directory, "gbaselite.log", int64(maxSizeMB)<<20, retentionDays)
	if err != nil {
		return nil, nil, err
	}
	logger := log.New(io.MultiWriter(os.Stdout, file), "", log.LstdFlags|log.Lmicroseconds)
	return logger, func() { _ = file.Close() }, nil
}
func printResult(result *executor.Result) {
	if len(result.Columns) == 0 {
		fmt.Printf("Query OK, %d rows affected %s\n", result.AffectedRows, result.Message)
		return
	}
	for _, column := range result.Columns {
		fmt.Printf("%s\t", column.Name)
	}
	fmt.Println()
	for _, row := range result.Rows {
		for _, value := range row {
			fmt.Printf("%v\t", value)
		}
		fmt.Println()
	}
	fmt.Printf("%d rows\n", len(result.Rows))
}
func printHelp() {
	fmt.Print(`GBaseLite commands:
  gbaselite -u USER -p [-h HOST] [-P PORT] [-D DATABASE]
  gbaselite client -u USER -p [-h HOST] [-P PORT] [-D DATABASE]
  gbaselite server [--config config.yaml]
  gbaselite service [--config config.yaml]  (Windows Service Control Manager only)
  gbaselite start [--config config.yaml]
  gbaselite stop [--config config.yaml]
  gbaselite restart [--config config.yaml]
  gbaselite shell [--config config.yaml]
  gbaselite import mysql --host HOST --port 3306 --user USER --password PASS --database DB
  gbaselite export mysql --database DB [--output backup.sql]
  gbaselite backup (--database DB | --all-databases) [--output backup.sql] [--no-data | --no-create-info] [--add-drop-database]
  gbaselite restore --input backup.sql [--config config.yaml]
	  gbaselite replay-binlog [--input binlog.jsonl] [--after-sequence N] [--until RFC3339] [--check-only] [--config config.yaml]
  gbaselite healthcheck [--host 127.0.0.1 --port 3307]
  gbaselite diagnose [--config config.yaml]
	  gbaselite inspect-snapshot --file store.gob [--compare store.gob.tmp]
	  gbaselite inspect-instance --directory copied-data-directory
  gbaselite version
`)
}
