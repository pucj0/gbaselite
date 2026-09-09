package config

import (
	"path/filepath"
	"testing"
)

func TestServerTimeZoneConfiguration(t *testing.T) {
	if defaults := Default(); defaults.Server.TimeZone != "SYSTEM" {
		t.Fatalf("default time zone = %q", defaults.Server.TimeZone)
	}
	t.Setenv("DB_TIME_ZONE", "+08:00")
	cfg, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.TimeZone != "+08:00" {
		t.Fatalf("environment time zone = %q", cfg.Server.TimeZone)
	}
	t.Setenv("DB_TIME_ZONE", "+25:00")
	if _, err := Load(filepath.Join(t.TempDir(), "missing-invalid.yaml")); err == nil {
		t.Fatal("invalid environment time zone was accepted")
	}
}
