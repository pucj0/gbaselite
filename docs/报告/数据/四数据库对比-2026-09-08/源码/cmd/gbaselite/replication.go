package main

import (
	"fmt"
	"gbaselite/config"
	"gbaselite/executor"
	"gbaselite/replication"
	"strings"
)

func engineOptions(cfg config.Config, serving bool) (executor.OpenOptions, error) {
	o := executor.OpenOptions{LocalWAL: cfg.Storage.LocalWAL, TransactionWriteBytes: int64(cfg.Resources.TransactionWriteMB) << 20, StorageMode: cfg.Storage.Mode, PageCacheBytes: int64(cfg.Storage.PageCacheMB) << 20, ColdRead: cfg.Storage.ColdRead, ColdMaterializeBytes: int64(cfg.Storage.ColdMaterializeMB) << 20}
	if !serving && cfg.Storage.Mode == "mvcc" {
		return o, fmt.Errorf("MVCC requires the server SQL interface; legacy offline shell/import/export/backup/restore/replay are disabled")
	}
	if cfg.Replication.Enabled {
		c := cfg.Replication
		o.Replication = &replication.Options{ID: c.ID, Bind: c.Bind, Advertise: c.Advertise, Bootstrap: c.Bootstrap, TLSCert: c.TLSCert, TLSKey: c.TLSKey, TLSCA: c.TLSCA}
		for _, entry := range strings.Split(c.Peers, ",") {
			pair := strings.SplitN(strings.TrimSpace(entry), "=", 2)
			if len(pair) != 2 {
				return o, fmt.Errorf("replication.peers must contain node_id=host:port entries")
			}
			o.Replication.Peers = append(o.Replication.Peers, replication.Peer{ID: strings.TrimSpace(pair[0]), Address: strings.TrimSpace(pair[1])})
		}
	}
	return o, nil
}
