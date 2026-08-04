package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadQuotedPasswordAndInlineComment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := "auth:\n  username: 'root user' # account\n  password: 'p#ss:''word' # secret\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.Username != "root user" {
		t.Fatalf("username = %q", cfg.Auth.Username)
	}
	if cfg.Auth.Password != "p#ss:'word" {
		t.Fatalf("password = %q", cfg.Auth.Password)
	}
}

func TestLoadDoubleQuotedEscapes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := "storage:\n  path: \"C:\\\\ProgramData\\\\GBaseLite\\\\data\"\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Storage.Path != `C:\ProgramData\GBaseLite\data` {
		t.Fatalf("storage path = %q", cfg.Storage.Path)
	}
}

func TestLoadConcurrencySettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := "server:\n  max_connections: 96\n  write_buffer_kb: 4\n  slow_query_ms: 75\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.MaxConnections != 96 || cfg.Server.WriteBufferSize != 4<<10 || cfg.Server.SlowQuery != 75*time.Millisecond {
		t.Fatalf("server tuning = %#v", cfg.Server)
	}
}

func TestSecurityDefaultsAndSettings(t *testing.T) {
	defaults := Default()
	if defaults.Server.Host != "127.0.0.1" {
		t.Fatalf("default host = %q", defaults.Server.Host)
	}
	if defaults.Security.LoginFailureLimit != 5 || defaults.Security.LoginFailureWindow != time.Minute || defaults.Security.LoginFailureBlock != 30*time.Second {
		t.Fatalf("security defaults = %#v", defaults.Security)
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := "security:\n  login_failure_limit: 3\n  login_failure_window_seconds: 45\n  login_failure_block_seconds: 20\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Security.LoginFailureLimit != 3 || cfg.Security.LoginFailureWindow != 45*time.Second || cfg.Security.LoginFailureBlock != 20*time.Second {
		t.Fatalf("security config = %#v", cfg.Security)
	}
}

func TestSecurityEnvironmentAndValidation(t *testing.T) {
	t.Setenv("DB_LOGIN_FAILURE_LIMIT", "2")
	t.Setenv("DB_LOGIN_FAILURE_WINDOW_SECONDS", "12")
	t.Setenv("DB_LOGIN_FAILURE_BLOCK_SECONDS", "7")
	cfg, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Security.LoginFailureLimit != 2 || cfg.Security.LoginFailureWindow != 12*time.Second || cfg.Security.LoginFailureBlock != 7*time.Second {
		t.Fatalf("security environment = %#v", cfg.Security)
	}

	for _, setting := range []string{
		"  login_failure_limit: -1\n",
		"  login_failure_window_seconds: -1\n",
		"  login_failure_block_seconds: -1\n",
	} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte("security:\n"+setting), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("invalid security setting was accepted: %s", setting)
		}
	}
}

func TestLoadAuditAndBinlogSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := "storage:\n  path: data-dir\nlog:\n  path: log-dir\naudit:\n  enabled: true\n  retention_days: 14\nbinlog:\n  enabled: true\n  path: changes.jsonl\n  retention_days: 30\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Audit.Enabled || cfg.AuditPath() != filepath.Join("log-dir", "audit.jsonl") || cfg.Audit.RetentionDays != 14 {
		t.Fatalf("audit config = %#v path=%q", cfg.Audit, cfg.AuditPath())
	}
	if !cfg.Binlog.Enabled || cfg.BinlogPath() != "changes.jsonl" || cfg.Binlog.RetentionDays != 30 {
		t.Fatalf("binlog config = %#v path=%q", cfg.Binlog, cfg.BinlogPath())
	}
}

func TestMainLogDefaultsSettingsAndEnvironment(t *testing.T) {
	defaults := Default()
	if defaults.Log.MaxSizeMB != 20 || defaults.Log.RetentionDays != 7 {
		t.Fatalf("main log defaults = %#v", defaults.Log)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("log:\n  path: main-logs\n  max_size_mb: 64\n  retention_days: 30\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Log.Path != "main-logs" || cfg.Log.MaxSizeMB != 64 || cfg.Log.RetentionDays != 30 {
		t.Fatalf("main log config = %#v", cfg.Log)
	}

	t.Setenv("DB_LOG_MAX_SIZE_MB", "12")
	t.Setenv("DB_LOG_RETENTION_DAYS", "0")
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Log.MaxSizeMB != 12 || cfg.Log.RetentionDays != 0 {
		t.Fatalf("main log environment = %#v", cfg.Log)
	}
}

func TestMainLogSettingsRejectInvalidRanges(t *testing.T) {
	for _, contents := range []string{
		"log:\n  max_size_mb: 0\n",
		"log:\n  max_size_mb: 1025\n",
		"log:\n  retention_days: -1\n",
		"log:\n  retention_days: 366\n",
	} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("invalid main log config was accepted: %s", contents)
		}
	}
}

func TestRetentionDaysRange(t *testing.T) {
	for _, value := range []string{"0", "1", "365"} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte("audit:\n  retention_days: "+value+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err != nil {
			t.Fatalf("retention_days %s: %v", value, err)
		}
	}
	for _, value := range []string{"-1", "366"} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte("binlog:\n  retention_days: "+value+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("retention_days %s was accepted", value)
		}
	}
}

func TestTLSSettingsAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := "tls:\n  enabled: true\n  cert_file: server.crt\n  key_file: server.key\n  require_secure_transport: true\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.TLS.Enabled || cfg.TLS.CertFile != "server.crt" || cfg.TLS.KeyFile != "server.key" || !cfg.TLS.RequireSecureTransport {
		t.Fatalf("TLS config = %#v", cfg.TLS)
	}

	for _, invalid := range []string{
		"tls:\n  require_secure_transport: true\n",
		"tls:\n  enabled: true\n  cert_file: server.crt\n",
		"tls:\n  enabled: true\n  key_file: server.key\n",
	} {
		invalidPath := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(invalidPath, []byte(invalid), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(invalidPath); err == nil {
			t.Fatalf("invalid TLS configuration was accepted: %s", invalid)
		}
	}
}

func TestTLSEnvironmentOverrides(t *testing.T) {
	t.Setenv("DB_TLS_ENABLED", "true")
	t.Setenv("DB_TLS_CERT_FILE", "environment.crt")
	t.Setenv("DB_TLS_KEY_FILE", "environment.key")
	t.Setenv("DB_REQUIRE_SECURE_TRANSPORT", "true")
	cfg, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.TLS.Enabled || cfg.TLS.CertFile != "environment.crt" || cfg.TLS.KeyFile != "environment.key" || !cfg.TLS.RequireSecureTransport {
		t.Fatalf("TLS environment config = %#v", cfg.TLS)
	}
}

func TestServerTLSConfigReportsUnreadableCertificate(t *testing.T) {
	cfg := Default()
	cfg.TLS.Enabled = true
	cfg.TLS.CertFile = filepath.Join(t.TempDir(), "missing.crt")
	cfg.TLS.KeyFile = filepath.Join(t.TempDir(), "missing.key")
	if _, err := cfg.ServerTLSConfig(); err == nil {
		t.Fatal("missing TLS certificate and key were accepted")
	}
}
