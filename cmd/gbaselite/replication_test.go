package main

import (
	"gbaselite/config"
	"testing"
)

func TestMVCCServingAndOfflineGuards(t *testing.T) {
	c := config.Default()
	c.Storage.Mode = "mvcc"
	c.Resources.TransactionWriteMB = 512
	c.Replication.Enabled = true
	c.Replication.ID = "n1"
	c.Replication.Bind = "127.0.0.1:7301"
	c.Replication.Peers = "n1=127.0.0.1:7301,n2=127.0.0.1:7302,n3=127.0.0.1:7303"
	o, err := engineOptions(c)
	if o.TransactionWriteBytes != 512<<20 {
		t.Fatal("transaction budget not forwarded")
	}
	if err != nil || o.Replication == nil || len(o.Replication.Peers) != 3 {
		t.Fatalf("options %+v %v", o, err)
	}
	if err = run([]string{"shell"}); err == nil {
		t.Fatal("unsafe offline opening allowed")
	}
}
