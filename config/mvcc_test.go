package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMVCCReplicationConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	text := "storage:\n  mode: mvcc\nreplication:\n  enabled: true\n  node_id: n1\n  bind: 127.0.0.1:7301\n  bootstrap: true\n  peers: n1=127.0.0.1:7301,n2=127.0.0.1:7302,n3=127.0.0.1:7303\n"
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil || !c.Replication.Enabled || !c.Replication.Bootstrap || c.Replication.ID != "n1" {
		t.Fatalf("config %+v %v", c.Replication, err)
	}
}

func TestLocalWALConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	for _, v := range []string{"true", "false", "invalid"} {
		if err := os.WriteFile(path, []byte("storage:\n  mode: mvcc\n  local_wal: "+v+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		c, err := Load(path)
		if v == "invalid" {
			if err == nil {
				t.Fatal("invalid boolean accepted")
			}
			continue
		}
		if err != nil || c.Storage.LocalWAL != (v == "true") {
			t.Fatal(c.Storage, err)
		}
	}
}
