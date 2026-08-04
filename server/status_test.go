package server

import (
	"database/sql"
	"strconv"
	"strings"
	"testing"
)

func TestShowStatusReportsRuntimeConnectionTLSAndQueryMetrics(t *testing.T) {
	databaseServer, address, stop := startTLSTestServer(t, false)
	defer stop()

	client, err := sql.Open("mysql", tlsTestDSN(t, address, databaseServer.TLSConfig))
	if err != nil {
		t.Fatal(err)
	}
	client.SetMaxOpenConns(1)
	defer client.Close()
	if err := client.Ping(); err != nil {
		t.Fatal(err)
	}

	status := queryStatus(t, client, "SHOW GLOBAL STATUS")
	for _, name := range []string{"Connections", "Threads_connected", "Max_used_connections", "Questions", "Threads_running", "Ssl_accepts", "Ssl_cipher", "Ssl_version", "Gbaselite_storage_state"} {
		if _, ok := status[name]; !ok {
			t.Fatalf("SHOW STATUS is missing %s: %#v", name, status)
		}
	}
	for _, name := range []string{"Connections", "Threads_connected", "Max_used_connections", "Questions", "Threads_running", "Ssl_accepts"} {
		value, parseErr := strconv.ParseUint(status[name], 10, 64)
		if parseErr != nil || value < 1 {
			t.Fatalf("%s = %q, want a positive integer", name, status[name])
		}
	}
	if status["Ssl_cipher"] == "" || !strings.HasPrefix(status["Ssl_version"], "TLS 1.") {
		t.Fatalf("TLS status cipher=%q version=%q", status["Ssl_cipher"], status["Ssl_version"])
	}
	if status["Gbaselite_storage_state"] != "available" {
		t.Fatalf("storage state = %q", status["Gbaselite_storage_state"])
	}

	tlsStatus := queryStatus(t, client, "SHOW SESSION STATUS LIKE 'Ssl_%'")
	if len(tlsStatus) != 3 || tlsStatus["Ssl_accepts"] == "" || tlsStatus["Ssl_cipher"] == "" || tlsStatus["Ssl_version"] == "" {
		t.Fatalf("filtered TLS status = %#v", tlsStatus)
	}
	if _, err := client.Exec("SHOW STATUS WHERE Variable_name = 'Connections'"); err == nil {
		t.Fatal("unsupported SHOW STATUS WHERE filter was accepted")
	}
}

func queryStatus(t *testing.T, client *sql.DB, query string) map[string]string {
	t.Helper()
	rows, err := client.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	result := make(map[string]string)
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			t.Fatal(err)
		}
		result[name] = value
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}
