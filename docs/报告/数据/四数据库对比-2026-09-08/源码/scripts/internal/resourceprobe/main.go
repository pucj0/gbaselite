// resourceprobe drives an explicitly supplied isolated benchmark server.
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	_ "github.com/go-sql-driver/mysql"
	"os"
	"sort"
	"strings"
	"time"
)

type measurement struct {
	Count   int     `json:"count"`
	TotalMS float64 `json:"total_ms"`
	P50MS   float64 `json:"p50_ms"`
	P95MS   float64 `json:"p95_ms"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	address := flag.String("address", "", "isolated server address")
	rowCount := flag.Int("rows", 10000, "seed row count")
	workers := flag.Int("workers", 1, "sustained concurrent clients")
	duration := flag.Duration("duration", 0, "sustained workload duration; zero runs the fixed workload")
	flag.Parse()
	if *rowCount < 100 || *rowCount > 1000000 || *workers < 1 || *workers > 32 || *duration < 0 || *duration > 120*time.Second {
		return fmt.Errorf("invalid workload bounds")
	}
	if *address == "" {
		return fmt.Errorf("-address is required")
	}
	db, err := sql.Open("mysql", "root:resource-probe-only@tcp("+*address+")/?timeout=5s&readTimeout=60s&writeTimeout=60s")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	deadline := time.Now().Add(20 * time.Second)
	for {
		if err = db.Ping(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
	setupStart := time.Now()
	for _, query := range []string{
		"CREATE DATABASE resource_probe", "USE resource_probe",
		"CREATE TABLE parent(id INT PRIMARY KEY, grp INT, payload VARCHAR(80), KEY grp_idx(grp))",
		"CREATE TABLE child(id INT PRIMARY KEY, parent_id INT, FOREIGN KEY(parent_id) REFERENCES parent(id))",
		"BEGIN",
	} {
		if _, err = db.Exec(query); err != nil {
			return err
		}
	}
	for start := 1; start <= *rowCount; start += 100 {
		var q strings.Builder
		q.WriteString("INSERT INTO parent VALUES ")
		for i := start; i < start+100 && i <= *rowCount; i++ {
			if i > start {
				q.WriteByte(',')
			}
			fmt.Fprintf(&q, "(%d,%d,'benchmark-payload')", i, (i*37)%100)
		}
		if _, err = db.Exec(q.String()); err != nil {
			return err
		}
	}
	for i := 1; i <= 100; i++ {
		if _, err = db.Exec(fmt.Sprintf("INSERT INTO child VALUES(%d,%d)", i, 1+(i*97)%*rowCount)); err != nil {
			return err
		}
	}
	if _, err = db.Exec("COMMIT"); err != nil {
		return err
	}
	report := map[string]any{"setup_ms": float64(time.Since(setupStart).Microseconds()) / 1000}
	if *duration > 0 {
		sustained, err := runSustained(db, *rowCount, *workers, *duration)
		if err != nil {
			return err
		}
		report["sustained"] = sustained
		report["verified"] = true
		return json.NewEncoder(os.Stdout).Encode(report)
	}
	measure := func(name string, count int, action func(int) error) error {
		durations := make([]float64, 0, count)
		started := time.Now()
		for i := 0; i < count; i++ {
			at := time.Now()
			if err := action(i); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			durations = append(durations, float64(time.Since(at).Nanoseconds())/1e6)
		}
		total := float64(time.Since(started).Nanoseconds()) / 1e6
		sort.Float64s(durations)
		report[name] = measurement{Count: count, TotalMS: total, P50MS: durations[count/2], P95MS: durations[(count*95-1)/100]}
		return nil
	}
	query := func(q string) error {
		rows, err := db.Query(q)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
		}
		return rows.Err()
	}
	if err = measure("point_select", 1000, func(i int) error {
		return query(fmt.Sprintf("SELECT payload FROM parent WHERE id=%d", 1+(i*97)%*rowCount))
	}); err != nil {
		return err
	}
	if err = measure("range_select", 200, func(i int) error {
		return query(fmt.Sprintf("SELECT id FROM parent WHERE grp=%d ORDER BY grp LIMIT 20", i%100))
	}); err != nil {
		return err
	}
	if err = measure("join_select", 100, func(i int) error {
		return query("SELECT c.id,p.payload FROM child c JOIN parent p ON c.parent_id=p.id LIMIT 20")
	}); err != nil {
		return err
	}
	if err = measure("durable_insert", 100, func(i int) error {
		_, err := db.Exec(fmt.Sprintf("INSERT INTO parent VALUES(%d,%d,'new')", *rowCount+1+i, i%100))
		return err
	}); err != nil {
		return err
	}
	if err = measure("durable_update", 50, func(i int) error {
		_, err := db.Exec(fmt.Sprintf("UPDATE parent SET payload='updated' WHERE id=%d", i+1))
		return err
	}); err != nil {
		return err
	}
	var count int
	if err = db.QueryRow("SELECT COUNT(*) FROM parent").Scan(&count); err != nil {
		return err
	}
	if count != *rowCount+100 {
		return fmt.Errorf("parent count %d, want %d", count, *rowCount+100)
	}
	if err = db.QueryRow("SELECT COUNT(*) FROM parent WHERE payload='updated'").Scan(&count); err != nil || count != 50 {
		return fmt.Errorf("updated count %d, error %v", count, err)
	}
	// Read on-demand runtime counters after the timed workload. Old binaries
	// legitimately return no Gbaselite-specific status rows.
	status, err := db.Query("SHOW STATUS LIKE 'Gbaselite%'")
	if err != nil {
		return err
	}
	counters := make(map[string]string)
	for status.Next() {
		var name, value string
		if err := status.Scan(&name, &value); err != nil {
			status.Close()
			return err
		}
		counters[name] = value
	}
	if err := status.Err(); err != nil {
		status.Close()
		return err
	}
	status.Close()
	report["runtime_after_workload"] = counters
	report["verified"] = true
	return json.NewEncoder(os.Stdout).Encode(report)
}
