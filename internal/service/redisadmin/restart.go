package redisadmin

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"

	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
)

// A Redis cluster's or Valkey instance's data across a restart of
// CloudBurrow (#1557). In persistent mode each node's server keeps its
// files on its own volume, in a directory of the formation it belongs to,
// /data/g<DataGen>: a create is formation 1, and each time CloudBurrow
// forms the nodes again after a restart they start empty in the next
// directory. With persistence_config RDB or AOF, the previous formation's
// files of each shard's primary are then served by a loopback server in
// that node's container and their keys moved (MIGRATE) to the primaries
// that own their slots now, as a restore from a backup is (backups.go);
// the previous directory is removed from every node once loaded. With
// persistence DISABLED nothing is loaded, as Memorystore documents: nodes
// do not load snapshots made before persistence was disabled, and without
// persistence the data of a node that restarts is lost.

// persistenceMode is r's persistence_config mode: RDB, AOF, or "" for
// DISABLED (the default, for a Redis cluster and a Valkey instance).
func (r *clusterRecord) persistenceMode() string {
	if r.Valkey != nil {
		switch r.Valkey.persistence().GetMode() {
		case memorystorepb.PersistenceConfig_RDB:
			return "RDB"
		case memorystorepb.PersistenceConfig_AOF:
			return "AOF"
		}
		return ""
	}
	p := r.clusterPersistence()
	switch p.GetMode() {
	case clusterpb.ClusterPersistenceConfig_RDB:
		return "RDB"
	case clusterpb.ClusterPersistenceConfig_AOF:
		return "AOF"
	}
	return ""
}

func (r *clusterRecord) clusterPersistence() *clusterpb.ClusterPersistenceConfig {
	p := &clusterpb.ClusterPersistenceConfig{}
	if r.Persistence != "" {
		_ = json.Unmarshal([]byte(r.Persistence), p)
	}
	return p
}

// clusterRDBPeriods are a Redis cluster's RDB snapshot periods, in seconds.
var clusterRDBPeriods = map[clusterpb.ClusterPersistenceConfig_RDBConfig_SnapshotPeriod]int{
	clusterpb.ClusterPersistenceConfig_RDBConfig_ONE_HOUR:          3600,
	clusterpb.ClusterPersistenceConfig_RDBConfig_SIX_HOURS:         6 * 3600,
	clusterpb.ClusterPersistenceConfig_RDBConfig_TWELVE_HOURS:      12 * 3600,
	clusterpb.ClusterPersistenceConfig_RDBConfig_TWENTY_FOUR_HOURS: 24 * 3600,
}

// persistenceSettings are the server settings of r's persistence_config:
// RDB snapshots at the period (save N 1), the append-only file with its
// fsync, or neither.
func (r *clusterRecord) persistenceSettings() [][2]string {
	if r.Valkey != nil {
		return r.Valkey.persistenceSettings()
	}
	p := r.clusterPersistence()
	switch p.GetMode() {
	case clusterpb.ClusterPersistenceConfig_RDB:
		period := clusterRDBPeriods[p.GetRdbConfig().GetRdbSnapshotPeriod()]
		if period == 0 {
			period = 3600
		}
		return [][2]string{{"appendonly", "no"}, {"save", fmt.Sprintf("%d 1", period)}}
	case clusterpb.ClusterPersistenceConfig_AOF:
		fsync := "everysec"
		switch p.GetAofConfig().GetAppendFsync() {
		case clusterpb.ClusterPersistenceConfig_AOFConfig_ALWAYS:
			fsync = "always"
		case clusterpb.ClusterPersistenceConfig_AOFConfig_NO:
			fsync = "no"
		}
		return [][2]string{{"save", ""}, {"appendonly", "yes"}, {"appendfsync", fsync}}
	}
	return [][2]string{{"save", ""}, {"appendonly", "no"}}
}

// dataDir is the directory of formation gen on a node's volume; 0, a
// record kept before formations, has none.
func dataDir(gen int) string {
	if gen <= 0 {
		return ""
	}
	return "/data/g" + strconv.Itoa(gen)
}

// reloadData moves the keys of the previous formation's files, prev, of
// each of r's primaries into the nodes formed now (mu not held). r's
// nodes serve and are formed.
func (c *clusters) reloadData(ctx context.Context, r *clusterRecord, prev string, mode string) error {
	sh, ok := c.rt.(Shell)
	if !ok {
		return nil
	}
	bin := "valkey"
	port := strconv.Itoa(importPort)
	cleanup := context.WithoutCancel(ctx)
	for _, n := range primariesOf(r.Nodes) {
		args := []string{bin + "-server", "--port", port, "--bind", "127.0.0.1", "--protected-mode", "no", "--dir", prev,
			"--save", "", "--daemonize", "yes", "--maxmemory", "0"}
		if mode == "AOF" {
			args = append(args, "--appendonly", "yes", "--appendfsync", "no", "--auto-aof-rewrite-percentage", "0")
		} else {
			args = append(args, "--appendonly", "no")
		}
		if r.TransitCA != "" {
			// MIGRATE to the nodes, which serve TLS only, over TLS.
			d := "/tmp/tls"
			args = append(args, "--tls-cluster", "yes", "--tls-replication", "yes", "--tls-cert-file", d+"/server.crt",
				"--tls-key-file", d+"/server.key", "--tls-ca-cert-file", d+"/ca.crt")
		}
		if out, err := sh.Exec(ctx, n.Server, nil, args...); err != nil {
			return fmt.Errorf("node %s: start the server that serves the previous files: %s", n.Server, lastLine(out, err))
		}
		err := c.migrateAll(ctx, sh, r, n.Server, bin, port)
		_, _ = sh.Exec(cleanup, n.Server, nil, bin+"-cli", "-p", port, "shutdown", "nosave")
		if err != nil {
			return fmt.Errorf("node %s: %w", n.Server, err)
		}
	}
	return nil
}

// dropDir removes a previous formation's directory from nodes.
func (c *clusters) dropDir(ctx context.Context, nodes []*clusterNode, dir string) {
	sh, ok := c.rt.(Shell)
	if !ok || dir == "" {
		return
	}
	for _, n := range nodes {
		if out, err := sh.Exec(ctx, n.Server, nil, "rm", "-rf", dir); err != nil {
			log.Printf("memorystore: remove %s from node %s: %s", dir, n.Server, lastLine(out, err))
		}
	}
}
