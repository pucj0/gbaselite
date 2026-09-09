package config

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"gbaselite/internal/mysqlcompat"
)

type Config struct {
	Replication struct {
		Enabled, Bootstrap                                 bool
		ID, Bind, Advertise, Peers, TLSCert, TLSKey, TLSCA string
	}
	Server struct {
		Host                  string
		Port                  int
		MaxConnections        int
		MaxPreparedStatements int
		MaxPreparedMemoryKB   int
		WriteBufferSize       int
		SlowQuery             time.Duration
		TimeZone              string
	}
	Resources struct {
		MemoryLimitMB          int
		WorkingSetLimitMB      int
		MaxProcs               int
		QueryTimeout           time.Duration
		SortMemoryMB           int
		QueryResultMemoryMB    int
		QueryTempMB            int
		TransactionWriteMB     int
		QueryTempPath          string
		OptimisticTransactions bool
	}
	Storage struct {
		LocalWAL          bool
		Path              string
		Mode              string
		PageCacheMB       int
		ColdRead          bool
		ColdMaterializeMB int
	}
	Auth     struct{ Username, Password string }
	Security struct {
		LoginFailureLimit  int
		LoginFailureWindow time.Duration
		LoginFailureBlock  time.Duration
	}
	TLS struct {
		Enabled                bool
		CertFile               string
		KeyFile                string
		RequireSecureTransport bool
	}
	Log struct {
		Path          string
		MaxSizeMB     int
		RetentionDays int
	}
	Audit struct {
		Enabled       bool
		Path          string
		RetentionDays int
	}
	Binlog struct {
		Enabled       bool
		Path          string
		RetentionDays int
	}
}

func Default() Config {
	var cfg Config
	cfg.Server.Host = "127.0.0.1"
	cfg.Server.Port = 3307
	cfg.Server.MaxConnections = 512
	cfg.Server.MaxPreparedStatements = 128
	cfg.Server.MaxPreparedMemoryKB = 1024
	cfg.Server.WriteBufferSize = 8 << 10
	cfg.Server.SlowQuery = 100 * time.Millisecond
	cfg.Server.TimeZone = "SYSTEM"
	cfg.Storage.Path = "./data"
	cfg.Storage.Mode = "snapshot"
	cfg.Storage.PageCacheMB = 16
	cfg.Storage.ColdMaterializeMB = 64
	cfg.Auth.Username = "root"
	cfg.Auth.Password = "change-this-password"
	cfg.Security.LoginFailureLimit = 5
	cfg.Security.LoginFailureWindow = time.Minute
	cfg.Security.LoginFailureBlock = 30 * time.Second
	cfg.Log.Path = "./logs"
	cfg.Log.MaxSizeMB = 20
	cfg.Log.RetentionDays = 7
	cfg.Audit.RetentionDays = 7
	cfg.Binlog.RetentionDays = 7
	return cfg
}

func Load(path string) (Config, error) {
	cfg := Default()
	file, err := os.Open(path)
	if err != nil && !os.IsNotExist(err) {
		return cfg, err
	}
	if err == nil {
		defer file.Close()
		section := ""
		scanner := bufio.NewScanner(file)
		line := 0
		for scanner.Scan() {
			line++
			raw := strings.TrimSpace(stripYAMLComment(scanner.Text()))
			if raw == "" {
				continue
			}
			if strings.HasSuffix(raw, ":") {
				section = strings.TrimSuffix(raw, ":")
				continue
			}
			parts := strings.SplitN(raw, ":", 2)
			if len(parts) != 2 {
				return cfg, fmt.Errorf("invalid config line %d", line)
			}
			key := strings.TrimSpace(parts[0])
			value, valueErr := parseYAMLScalar(parts[1])
			if valueErr != nil {
				return cfg, fmt.Errorf("invalid config line %d: %w", line, valueErr)
			}
			switch section + "." + key {
			case "server.host":
				cfg.Server.Host = value
			case "server.port":
				cfg.Server.Port, err = strconv.Atoi(value)
			case "server.max_connections":
				cfg.Server.MaxConnections, err = strconv.Atoi(value)
			case "resources.working_set_limit_mb":
				cfg.Resources.WorkingSetLimitMB, err = parseNonNegativeInt(value, "working_set_limit_mb")
			case "resources.memory_limit_mb":
				cfg.Resources.MemoryLimitMB, err = parseNonNegativeInt(value, "memory_limit_mb")
			case "resources.max_procs":
				cfg.Resources.MaxProcs, err = parseNonNegativeInt(value, "max_procs")
			case "resources.query_timeout_ms":
				var milliseconds int
				milliseconds, err = parseNonNegativeInt(value, "query_timeout_ms")
				if err == nil && milliseconds > 86400000 {
					err = fmt.Errorf("query_timeout_ms must be 0..86400000")
				}
				cfg.Resources.QueryTimeout = time.Duration(milliseconds) * time.Millisecond
			case "resources.sort_memory_mb":
				cfg.Resources.SortMemoryMB, err = parseNonNegativeInt(value, "sort_memory_mb")
			case "resources.query_result_memory_mb":
				cfg.Resources.QueryResultMemoryMB, err = parseNonNegativeInt(value, "query_result_memory_mb")
			case "resources.transaction_write_mb":
				cfg.Resources.TransactionWriteMB, err = parseNonNegativeInt(value, "transaction_write_mb")
			case "resources.query_temp_mb":
				cfg.Resources.QueryTempMB, err = parseNonNegativeInt(value, "query_temp_mb")
			case "resources.query_temp_path":
				cfg.Resources.QueryTempPath = value
			case "resources.optimistic_transactions":
				cfg.Resources.OptimisticTransactions, err = strconv.ParseBool(value)
			case "replication.enabled":
				cfg.Replication.Enabled, err = strconv.ParseBool(value)
			case "replication.bootstrap":
				cfg.Replication.Bootstrap, err = strconv.ParseBool(value)
			case "replication.node_id":
				cfg.Replication.ID = value
			case "replication.bind":
				cfg.Replication.Bind = value
			case "replication.advertise":
				cfg.Replication.Advertise = value
			case "replication.peers":
				cfg.Replication.Peers = value
			case "replication.tls_cert":
				cfg.Replication.TLSCert = value
			case "replication.tls_key":
				cfg.Replication.TLSKey = value
			case "replication.tls_ca":
				cfg.Replication.TLSCA = value
			case "storage.local_wal":
				cfg.Storage.LocalWAL, err = strconv.ParseBool(value)
			case "storage.mode":
				cfg.Storage.Mode = strings.ToLower(value)
			case "storage.cold_reads":
				cfg.Storage.ColdRead, err = strconv.ParseBool(value)
			case "storage.cold_materialize_mb":
				cfg.Storage.ColdMaterializeMB, err = parseBoundedPositiveInt(value, "cold_materialize_mb", 1048576)
			case "storage.page_cache_mb":
				cfg.Storage.PageCacheMB, err = parseNonNegativeInt(value, "page_cache_mb")
			case "server.max_prepared_statements":
				cfg.Server.MaxPreparedStatements, err = parseBoundedPositiveInt(value, "max_prepared_statements", 65535)
			case "server.max_prepared_memory_kb":
				cfg.Server.MaxPreparedMemoryKB, err = parseBoundedPositiveInt(value, "max_prepared_memory_kb", 1048576)
			case "server.write_buffer_kb":
				var kilobytes int
				kilobytes, err = parseNonNegativeInt(value, "write_buffer_kb")
				if err == nil && kilobytes > 1024 {
					err = fmt.Errorf("write_buffer_kb must be 0..1024")
				}
				cfg.Server.WriteBufferSize = kilobytes << 10
			case "server.slow_query_ms":
				var milliseconds int
				milliseconds, err = strconv.Atoi(value)
				cfg.Server.SlowQuery = time.Duration(milliseconds) * time.Millisecond
			case "server.time_zone":
				cfg.Server.TimeZone = value
			case "storage.path":
				cfg.Storage.Path = value
			case "auth.username":
				cfg.Auth.Username = value
			case "auth.password":
				cfg.Auth.Password = value
			case "security.login_failure_limit":
				cfg.Security.LoginFailureLimit, err = parseNonNegativeInt(value, "login_failure_limit")
			case "security.login_failure_window_seconds":
				cfg.Security.LoginFailureWindow, err = parseNonNegativeSeconds(value, "login_failure_window_seconds")
			case "security.login_failure_block_seconds":
				cfg.Security.LoginFailureBlock, err = parseNonNegativeSeconds(value, "login_failure_block_seconds")
			case "tls.enabled":
				cfg.TLS.Enabled, err = strconv.ParseBool(value)
			case "tls.cert_file":
				cfg.TLS.CertFile = value
			case "tls.key_file":
				cfg.TLS.KeyFile = value
			case "tls.require_secure_transport":
				cfg.TLS.RequireSecureTransport, err = strconv.ParseBool(value)
			case "log.path":
				cfg.Log.Path = value
			case "log.max_size_mb":
				cfg.Log.MaxSizeMB, err = parseBoundedPositiveInt(value, "max_size_mb", 1024)
			case "log.retention_days":
				cfg.Log.RetentionDays, err = parseRetentionDays(value)
			case "audit.enabled":
				cfg.Audit.Enabled, err = strconv.ParseBool(value)
			case "audit.path":
				cfg.Audit.Path = value
			case "audit.retention_days":
				cfg.Audit.RetentionDays, err = parseRetentionDays(value)
			case "binlog.enabled":
				cfg.Binlog.Enabled, err = strconv.ParseBool(value)
			case "binlog.path":
				cfg.Binlog.Path = value
			case "binlog.retention_days":
				cfg.Binlog.RetentionDays, err = parseRetentionDays(value)
			}
			if err != nil {
				return cfg, fmt.Errorf("invalid config line %d: %w", line, err)
			}
		}
		if err := scanner.Err(); err != nil {
			return cfg, err
		}
	}
	if value := os.Getenv("DB_USER"); value != "" {
		cfg.Auth.Username = value
	}
	if value := os.Getenv("DB_PASSWORD"); value != "" {
		cfg.Auth.Password = value
	}
	if value := os.Getenv("DB_HOST"); value != "" {
		cfg.Server.Host = value
	}
	if value := os.Getenv("DB_PORT"); value != "" {
		port, parseErr := strconv.Atoi(value)
		if parseErr != nil {
			return cfg, parseErr
		}
		cfg.Server.Port = port
	}
	if value := os.Getenv("DB_MAX_CONNECTIONS"); value != "" {
		cfg.Server.MaxConnections, err = strconv.Atoi(value)
		if err != nil {
			return cfg, err
		}
	}
	for _, setting := range []struct {
		name   string
		target *int
	}{
		{"DB_MEMORY_LIMIT_MB", &cfg.Resources.MemoryLimitMB},
		{"DB_MAX_PROCS", &cfg.Resources.MaxProcs},
		{"DB_SORT_MEMORY_MB", &cfg.Resources.SortMemoryMB},
		{"DB_QUERY_RESULT_MEMORY_MB", &cfg.Resources.QueryResultMemoryMB},
		{"DB_QUERY_TEMP_MB", &cfg.Resources.QueryTempMB},
		{"DB_TRANSACTION_WRITE_MB", &cfg.Resources.TransactionWriteMB},
		{"DB_PAGE_CACHE_MB", &cfg.Storage.PageCacheMB},
		{"DB_COLD_MATERIALIZE_MB", &cfg.Storage.ColdMaterializeMB},
		{"DB_MAX_PREPARED_STATEMENTS", &cfg.Server.MaxPreparedStatements},
		{"DB_MAX_PREPARED_MEMORY_KB", &cfg.Server.MaxPreparedMemoryKB},
	} {
		if value := os.Getenv(setting.name); value != "" {
			*setting.target, err = parseNonNegativeInt(value, setting.name)
			if err != nil {
				return cfg, err
			}
		}
	}
	if value := os.Getenv("DB_SLOW_QUERY_MS"); value != "" {
		milliseconds, parseErr := strconv.Atoi(value)
		if parseErr != nil {
			return cfg, parseErr
		}
		cfg.Server.SlowQuery = time.Duration(milliseconds) * time.Millisecond
	}
	if value := os.Getenv("DB_TIME_ZONE"); value != "" {
		cfg.Server.TimeZone = value
	}
	if value := os.Getenv("DB_DATA_PATH"); value != "" {
		cfg.Storage.Path = value
	}
	if value := os.Getenv("DB_LOGIN_FAILURE_LIMIT"); value != "" {
		cfg.Security.LoginFailureLimit, err = parseNonNegativeInt(value, "DB_LOGIN_FAILURE_LIMIT")
		if err != nil {
			return cfg, err
		}
	}
	if value := os.Getenv("DB_LOGIN_FAILURE_WINDOW_SECONDS"); value != "" {
		cfg.Security.LoginFailureWindow, err = parseNonNegativeSeconds(value, "DB_LOGIN_FAILURE_WINDOW_SECONDS")
		if err != nil {
			return cfg, err
		}
	}
	if value := os.Getenv("DB_LOGIN_FAILURE_BLOCK_SECONDS"); value != "" {
		cfg.Security.LoginFailureBlock, err = parseNonNegativeSeconds(value, "DB_LOGIN_FAILURE_BLOCK_SECONDS")
		if err != nil {
			return cfg, err
		}
	}
	if value := os.Getenv("DB_TLS_ENABLED"); value != "" {
		cfg.TLS.Enabled, err = strconv.ParseBool(value)
		if err != nil {
			return cfg, err
		}
	}
	if value := os.Getenv("DB_TLS_CERT_FILE"); value != "" {
		cfg.TLS.CertFile = value
	}
	if value := os.Getenv("DB_TLS_KEY_FILE"); value != "" {
		cfg.TLS.KeyFile = value
	}
	if value := os.Getenv("DB_REQUIRE_SECURE_TRANSPORT"); value != "" {
		cfg.TLS.RequireSecureTransport, err = strconv.ParseBool(value)
		if err != nil {
			return cfg, err
		}
	}
	if value := os.Getenv("DB_LOG_MAX_SIZE_MB"); value != "" {
		cfg.Log.MaxSizeMB, err = parseBoundedPositiveInt(value, "DB_LOG_MAX_SIZE_MB", 1024)
		if err != nil {
			return cfg, err
		}
	}
	if value := os.Getenv("DB_LOG_RETENTION_DAYS"); value != "" {
		cfg.Log.RetentionDays, err = parseRetentionDays(value)
		if err != nil {
			return cfg, err
		}
	}
	if value := os.Getenv("DB_AUDIT_ENABLED"); value != "" {
		cfg.Audit.Enabled, err = strconv.ParseBool(value)
		if err != nil {
			return cfg, err
		}
	}
	if value := os.Getenv("DB_AUDIT_PATH"); value != "" {
		cfg.Audit.Path = value
	}
	if value := os.Getenv("DB_AUDIT_RETENTION_DAYS"); value != "" {
		cfg.Audit.RetentionDays, err = parseRetentionDays(value)
		if err != nil {
			return cfg, err
		}
	}
	if value := os.Getenv("DB_BINLOG_ENABLED"); value != "" {
		cfg.Binlog.Enabled, err = strconv.ParseBool(value)
		if err != nil {
			return cfg, err
		}
	}
	if value := os.Getenv("DB_BINLOG_PATH"); value != "" {
		cfg.Binlog.Path = value
	}
	if value := os.Getenv("DB_BINLOG_RETENTION_DAYS"); value != "" {
		cfg.Binlog.RetentionDays, err = parseRetentionDays(value)
		if err != nil {
			return cfg, err
		}
	}
	if value := os.Getenv("DB_QUERY_TIMEOUT_MS"); value != "" {
		milliseconds, parseErr := parseNonNegativeInt(value, "DB_QUERY_TIMEOUT_MS")
		if parseErr != nil {
			return cfg, parseErr
		}
		if milliseconds > 86400000 {
			return cfg, fmt.Errorf("DB_QUERY_TIMEOUT_MS must be 0..86400000")
		}
		cfg.Resources.QueryTimeout = time.Duration(milliseconds) * time.Millisecond
	}
	if value := os.Getenv("DB_QUERY_TEMP_PATH"); value != "" {
		cfg.Resources.QueryTempPath = value
	}
	if value := os.Getenv("DB_STORAGE_MODE"); value != "" {
		cfg.Storage.Mode = strings.ToLower(value)
	}
	if value := os.Getenv("DB_OPTIMISTIC_TRANSACTIONS"); value != "" {
		cfg.Resources.OptimisticTransactions, err = strconv.ParseBool(value)
		if err != nil {
			return cfg, err
		}
	}
	if value := os.Getenv("DB_COLD_READS"); value != "" {
		cfg.Storage.ColdRead, err = strconv.ParseBool(value)
		if err != nil {
			return cfg, err
		}
	}
	if cfg.Replication.Enabled && cfg.Storage.Mode != "mvcc" {
		return cfg, fmt.Errorf("replication requires storage.mode=mvcc")
	}
	if cfg.Storage.Mode == "mvcc" && cfg.Binlog.Enabled {
		return cfg, fmt.Errorf("MVCC uses its own durable log; legacy binlog is not supported")
	}
	if cfg.Storage.ColdRead && cfg.Storage.Mode != "paged" {
		return cfg, fmt.Errorf("storage.cold_reads requires storage.mode=paged")
	}
	if cfg.Storage.ColdMaterializeMB < 1 || cfg.Storage.ColdMaterializeMB > 1048576 {
		return cfg, fmt.Errorf("storage.cold_materialize_mb must be 1..1048576")
	}
	if cfg.Storage.Mode != "snapshot" && cfg.Storage.Mode != "paged" && cfg.Storage.Mode != "mvcc" {
		return cfg, fmt.Errorf("storage.mode must be snapshot, paged or mvcc")
	}
	if cfg.Resources.TransactionWriteMB > 1048576 {
		return cfg, fmt.Errorf("transaction_write_mb must be 0..1048576 MiB")
	}
	if cfg.Storage.PageCacheMB > 1048576 || cfg.Resources.SortMemoryMB > 1048576 || cfg.Resources.QueryResultMemoryMB > 1048576 || cfg.Resources.QueryTempMB > 1048576 {
		return cfg, fmt.Errorf("page cache and query memory/disk settings must be 0..1048576 MB")
	}
	if cfg.Resources.WorkingSetLimitMB != 0 && (runtime.GOOS != "windows" || cfg.Resources.WorkingSetLimitMB < 16 || cfg.Resources.WorkingSetLimitMB > 1048576) {
		return cfg, fmt.Errorf("working_set_limit_mb requires Windows and 16..1048576 MiB")
	}
	if cfg.Resources.MemoryLimitMB > 1048576 || cfg.Resources.MaxProcs > 1024 {
		return cfg, fmt.Errorf("resources.memory_limit_mb must be 0..1048576 and max_procs 0..1024")
	}
	if cfg.Server.MaxPreparedStatements < 1 || cfg.Server.MaxPreparedStatements > 65535 || cfg.Server.MaxPreparedMemoryKB < 1 || cfg.Server.MaxPreparedMemoryKB > 1048576 {
		return cfg, fmt.Errorf("max_prepared_statements must be 1..65535 and max_prepared_memory_kb 1..1048576")
	}
	if cfg.Server.MaxConnections < 0 || cfg.Server.WriteBufferSize < 0 || cfg.Server.WriteBufferSize > 1<<20 {
		return cfg, fmt.Errorf("max_connections must be nonnegative; write_buffer_kb must be 0..1024")
	}
	if runtime.GOOS == "windows" {
		if cfg.Storage.Path == "/app/data" {
			cfg.Storage.Path = "./data"
		}
		if cfg.Log.Path == "/app/logs" {
			cfg.Log.Path = "./logs"
		}
		if cfg.Audit.Path == "/app/logs/audit.jsonl" {
			cfg.Audit.Path = "./logs/audit.jsonl"
		}
		if cfg.Binlog.Path == "/app/data/binlog.jsonl" {
			cfg.Binlog.Path = "./data/binlog.jsonl"
		}
	}
	if _, _, err := mysqlcompat.ParseTimeZone(cfg.Server.TimeZone); err != nil {
		return cfg, fmt.Errorf("invalid server.time_zone: %w", err)
	}
	if cfg.TLS.RequireSecureTransport && !cfg.TLS.Enabled {
		return cfg, fmt.Errorf("tls.require_secure_transport requires tls.enabled")
	}
	if cfg.TLS.Enabled && (strings.TrimSpace(cfg.TLS.CertFile) == "" || strings.TrimSpace(cfg.TLS.KeyFile) == "") {
		return cfg, fmt.Errorf("tls.enabled requires tls.cert_file and tls.key_file")
	}
	return cfg, nil
}

func (c Config) Address() string { return fmt.Sprintf("%s:%d", c.Server.Host, c.Server.Port) }

func (c Config) AuditPath() string {
	if c.Audit.Path != "" {
		return c.Audit.Path
	}
	return filepath.Join(c.Log.Path, "audit.jsonl")
}

func (c Config) BinlogPath() string {
	if c.Binlog.Path != "" {
		return c.Binlog.Path
	}
	return filepath.Join(c.Storage.Path, "binlog.jsonl")
}

func (c Config) ServerTLSConfig() (*tls.Config, error) {
	if !c.TLS.Enabled {
		return nil, nil
	}
	certificate, err := tls.LoadX509KeyPair(c.TLS.CertFile, c.TLS.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load TLS certificate or key: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

func parseRetentionDays(value string) (int, error) {
	days, err := strconv.Atoi(value)
	if err != nil {
		return 0, err
	}
	if days < 0 || days > 365 {
		return 0, fmt.Errorf("retention_days must be 0 or between 1 and 365")
	}
	return days, nil
}

func parseNonNegativeInt(value, name string) (int, error) {
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, err
	}
	if parsed < 0 {
		return 0, fmt.Errorf("%s must be zero or greater", name)
	}
	return parsed, nil
}

func parseBoundedPositiveInt(value, name string, maximum int) (int, error) {
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, err
	}
	if parsed < 1 || parsed > maximum {
		return 0, fmt.Errorf("%s must be between 1 and %d", name, maximum)
	}
	return parsed, nil
}

func parseNonNegativeSeconds(value, name string) (time.Duration, error) {
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, err
	}
	const maxDurationSeconds = int64(1<<63-1) / int64(time.Second)
	if seconds < 0 || seconds > maxDurationSeconds {
		return 0, fmt.Errorf("%s must be zero or a valid duration in seconds", name)
	}
	return time.Duration(seconds) * time.Second, nil
}

func stripYAMLComment(line string) string {
	var quote rune
	escaped := false
	for index, character := range line {
		if escaped {
			escaped = false
			continue
		}
		if quote == '"' && character == '\\' {
			escaped = true
			continue
		}
		if character == '\'' || character == '"' {
			if quote == 0 {
				quote = character
			} else if quote == character {
				quote = 0
			}
			continue
		}
		if character == '#' && quote == 0 {
			return line[:index]
		}
	}
	return line
}

func parseYAMLScalar(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if len(value) < 2 {
		return value, nil
	}
	if value[0] == '\'' && value[len(value)-1] == '\'' {
		return strings.ReplaceAll(value[1:len(value)-1], "''", "'"), nil
	}
	if value[0] == '"' && value[len(value)-1] == '"' {
		return strconv.Unquote(value)
	}
	return value, nil
}
