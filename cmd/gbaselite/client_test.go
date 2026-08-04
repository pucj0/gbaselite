package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestParseClientOptionsMySQLStyle(t *testing.T) {
	options, err := parseClientOptions([]string{"-hdb.internal", "-P3308", "-u", "admin", "-psecret", "-D", "app", "-e", "SELECT 1"})
	if err != nil {
		t.Fatal(err)
	}
	if options.Host != "db.internal" || options.Port != 3308 || options.User != "admin" || options.Password != "secret" || options.Database != "app" || options.Execute != "SELECT 1" || options.PromptPassword {
		t.Fatalf("unexpected options: %#v", options)
	}
}

func TestParseClientOptionsPasswordPromptDefaults(t *testing.T) {
	options, err := parseClientOptions([]string{"-u", "root", "-p"})
	if err != nil {
		t.Fatal(err)
	}
	if options.Host != "127.0.0.1" || options.Port != 3307 || options.User != "root" || !options.PromptPassword {
		t.Fatalf("unexpected defaults: %#v", options)
	}
	if !isClientInvocation([]string{"-u", "root", "-p"}) || isClientInvocation([]string{"--config", "config.yaml"}) {
		t.Fatal("client invocation detection is incorrect")
	}
}

func TestParseClientOptionsRejectsInvalidPort(t *testing.T) {
	if _, err := parseClientOptions([]string{"-P", "70000"}); err == nil {
		t.Fatal("expected invalid port error")
	}
}

func TestWriteClientTableAlignsChineseText(t *testing.T) {
	var output bytes.Buffer
	writeClientTable(&output, []string{"编号", "名称"}, [][]string{{"1", "中文正常"}, {"20", "广州源码"}})
	expected := strings.Join([]string{
		"+------+----------+",
		"| 编号 | 名称     |",
		"+------+----------+",
		"| 1    | 中文正常 |",
		"| 20   | 广州源码 |",
		"+------+----------+",
		"",
	}, "\n")
	if output.String() != expected {
		t.Fatalf("unexpected table:\n%s", output.String())
	}
}

func TestClientFormattingAndDatabaseSelection(t *testing.T) {
	if got := formatClientValue([]byte("first\tline\nsecond")); got != `first\tline\nsecond` {
		t.Fatalf("formatted value = %q", got)
	}
	if got := formatClientDuration(1250 * time.Millisecond); got != "1.250 sec" {
		t.Fatalf("duration = %q", got)
	}
	if got := formatClientDuration(250 * time.Microsecond); got != "0.250 ms" {
		t.Fatalf("sub-second duration = %q", got)
	}
	if got := formatClientDuration(0); got != "<0.001 ms" {
		t.Fatalf("minimum duration = %q", got)
	}
	if got := clientRowCountText(1); got != "1 row in set" {
		t.Fatalf("single row label = %q", got)
	}
	if got := clientRowCountText(2); got != "2 rows in set" {
		t.Fatalf("multiple row label = %q", got)
	}
	if database, ok := clientSelectedDatabase(" USE `test``db`; "); !ok || database != "test`db" {
		t.Fatalf("selected database = %q, %v", database, ok)
	}
	if got := formatClientConsoleError(&staticClientError{"Error 1064 (HY000): failed (0.001 sec)"}); got != "ERROR 1064 (HY000): failed (0.001 sec)" {
		t.Fatalf("console error = %q", got)
	}
}

type staticClientError struct{ message string }

func (e *staticClientError) Error() string { return e.message }
