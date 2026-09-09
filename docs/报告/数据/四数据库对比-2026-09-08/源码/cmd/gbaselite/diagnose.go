package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gbaselite/config"
	"gbaselite/executor"
)

var diagnosticRotatedLogName = regexp.MustCompile(`^gbaselite-\d{8}-\d{6}\.\d{9}Z(?:-\d+)?\.log$`)

func runDiagnose(args []string) error {
	flags := flag.NewFlagSet("diagnose", flag.ContinueOnError)
	configPath := flags.String("config", "config.yaml", "configuration file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	return diagnose(*configPath, 2*time.Second, os.Stdout)
}

func diagnose(configPath string, timeout time.Duration, output io.Writer) error {
	absConfig, absErr := filepath.Abs(configPath)
	if absErr != nil {
		return fmt.Errorf("resolve configuration path: %w", absErr)
	}
	configStatus, configOK := diagnosticPath(absConfig, false)
	cfg, err := config.Load(absConfig)
	if err != nil {
		return fmt.Errorf("load configuration %s: %w", absConfig, err)
	}

	dataPath := diagnosticAbsolutePath(cfg.Storage.Path)
	logPath := diagnosticAbsolutePath(cfg.Log.Path)
	dataStatus, dataOK := diagnosticPath(dataPath, true)
	logStatus, logOK := diagnosticPath(logPath, true)
	snapshotPath := filepath.Join(dataPath, "databases", "store.gob")
	userCatalogPath := filepath.Join(dataPath, "users", "users.gob")
	mainLogPath := filepath.Join(logPath, "gbaselite.log")
	snapshotStatus, snapshotOK := diagnosticDurableFile(snapshotPath)
	userCatalogStatus, userCatalogOK := diagnosticDurableFile(userCatalogPath)
	mainLogStatus, _ := diagnosticPath(mainLogPath, false)

	tlsStatus := "disabled"
	tlsOK := true
	if cfg.TLS.Enabled {
		if _, tlsErr := cfg.ServerTLSConfig(); tlsErr != nil {
			tlsStatus = "invalid: " + tlsErr.Error()
			tlsOK = false
		} else if cfg.TLS.RequireSecureTransport {
			tlsStatus = "enabled (TLS 1.2+, secure transport required)"
		} else {
			tlsStatus = "enabled (TLS 1.2+, plaintext also allowed)"
		}
	}

	host := cfg.Server.Host
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	address := net.JoinHostPort(host, strconv.Itoa(cfg.Server.Port))
	listenerStatus := "unreachable"
	listenerOK := false
	connection, dialErr := net.DialTimeout("tcp", address, timeout)
	if dialErr == nil {
		listenerStatus = "reachable"
		listenerOK = true
		_ = connection.Close()
	} else {
		listenerStatus = "unreachable: " + dialErr.Error()
	}

	fmt.Fprintln(output, "GBaseLite diagnostic")
	fmt.Fprintf(output, "Version: %s\n", executor.Version)
	fmt.Fprintf(output, "Config: %s (%s)\n", absConfig, configStatus)
	fmt.Fprintf(output, "Configured address: %s\n", cfg.Address())
	fmt.Fprintf(output, "TCP listener: %s (%s)\n", address, listenerStatus)
	fmt.Fprintf(output, "Data directory: %s (%s)\n", dataPath, dataStatus)
	fmt.Fprintf(output, "Data volume: %s\n", diagnosticVolumeStatus(dataPath))
	fmt.Fprintf(output, "Snapshot: %s (%s)\n", snapshotPath, snapshotStatus)
	fmt.Fprintf(output, "User catalog: %s (%s)\n", userCatalogPath, userCatalogStatus)
	fmt.Fprintf(output, "Log directory: %s (%s)\n", logPath, logStatus)
	fmt.Fprintf(output, "Log volume: %s\n", diagnosticVolumeStatus(logPath))
	fmt.Fprintf(output, "Main log: %s (%s), rotated=%s, max_size=%d MiB, retention=%s\n", mainLogPath, mainLogStatus, diagnosticRotatedLogs(logPath), cfg.Log.MaxSizeMB, diagnosticRetention(cfg.Log.RetentionDays))
	fmt.Fprintf(output, "TLS: %s\n", tlsStatus)
	fmt.Fprintf(output, "Audit: %s\n", diagnosticJournalStatus(cfg.Audit.Enabled, cfg.AuditPath(), cfg.Audit.RetentionDays))
	fmt.Fprintf(output, "Binlog: %s\n", diagnosticJournalStatus(cfg.Binlog.Enabled, cfg.BinlogPath(), cfg.Binlog.RetentionDays))

	if !configOK || !dataOK || !logOK || !snapshotOK || !userCatalogOK || !tlsOK || !listenerOK {
		return fmt.Errorf("one or more diagnostic checks failed")
	}
	return nil
}

func diagnosticAbsolutePath(path string) string {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return absPath
}

func diagnosticPath(path string, directory bool) (string, bool) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "missing", false
		}
		return "unavailable: " + err.Error(), false
	}
	if info.IsDir() != directory {
		if directory {
			return "not a directory", false
		}
		return "not a file", false
	}
	if directory {
		return "available", true
	}
	return fmt.Sprintf("available, %d bytes, modified %s", info.Size(), info.ModTime().Format(time.RFC3339)), true
}

func diagnosticDurableFile(path string) (string, bool) {
	status, ok := diagnosticPath(path, false)
	if ok {
		if _, temporaryErr := os.Stat(path + ".tmp"); temporaryErr == nil {
			return status + "; recovery candidate also exists at " + path + ".tmp", false
		}
		return status, true
	}
	if _, temporaryErr := os.Stat(path + ".tmp"); temporaryErr == nil {
		return "missing; recovery candidate exists at " + path + ".tmp", false
	}
	return status, false
}

func diagnosticJournalStatus(enabled bool, path string, retentionDays int) string {
	absPath := diagnosticAbsolutePath(path)
	status, _ := diagnosticPath(absPath, false)
	if !enabled {
		return strings.Join([]string{"disabled", absPath, status}, ", ")
	}
	return strings.Join([]string{"enabled", absPath, status, "retention=" + diagnosticRetention(retentionDays)}, ", ")
}

func diagnosticVolumeStatus(path string) string {
	space, err := readDiagnosticDiskSpace(path)
	if err != nil {
		return "unavailable: " + err.Error()
	}
	return fmt.Sprintf("total=%d bytes, available=%d bytes", space.TotalBytes, space.AvailableBytes)
}

func diagnosticRotatedLogs(directory string) string {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return "unavailable: " + err.Error()
	}
	count := 0
	var size int64
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !diagnosticRotatedLogName.MatchString(name) {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil || !info.Mode().IsRegular() {
			continue
		}
		count++
		size += info.Size()
	}
	return fmt.Sprintf("%d files, %d bytes", count, size)
}

func diagnosticRetention(retentionDays int) string {
	if retentionDays == 0 {
		return "permanent"
	}
	return fmt.Sprintf("%d days", retentionDays)
}
