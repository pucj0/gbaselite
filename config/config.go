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
	Server struct {
		Host            string
		Port            int
		MaxConnections  int
		WriteBufferSize int
		SlowQuery       time.Duration
		TimeZone        string
	}
	Storage  struct{ Path string }
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
	cfg.Server.WriteBufferSize = 8 << 10
	cfg.Server.SlowQuery = 100 * time.Millisecond
	cfg.Server.TimeZone = "SYSTEM"
	cfg.Storage.Path = "./data"
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
			case "server.write_buffer_kb":
				var kilobytes int
				kilobytes, err = strconv.Atoi(value)
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
