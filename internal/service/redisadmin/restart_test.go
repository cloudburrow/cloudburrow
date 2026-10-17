package redisadmin

import (
	"encoding/json"
	"testing"

	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
)

// TestNodeDataAcrossFormations (#1557): a node's spec puts its data in the
// formation's directory with the persistence_config's settings, for a
// Redis cluster and a Valkey instance, and only RDB and AOF are reloaded.
func TestNodeDataAcrossFormations(t *testing.T) {
	pc, _ := json.Marshal(&clusterpb.ClusterPersistenceConfig{Mode: clusterpb.ClusterPersistenceConfig_AOF,
		AofConfig: &clusterpb.ClusterPersistenceConfig_AOFConfig{AppendFsync: clusterpb.ClusterPersistenceConfig_AOFConfig_ALWAYS}})
	r := &clusterRecord{Name: "projects/p/locations/l/clusters/a", ShardCount: 1, NodeType: "REDIS_SHARED_CORE_NANO",
		Persistence: string(pc), DataGen: 2}
	s := r.spec("n0")
	if s.Dir != "/data/g2" || s.Configs["appendonly"] != "yes" || s.Configs["appendfsync"] != "always" || s.Configs["save"] != "" {
		t.Errorf("Redis cluster AOF spec = %q %v", s.Dir, s.Configs)
	}
	if m := r.persistenceMode(); m != "AOF" {
		t.Errorf("persistenceMode = %q, want AOF", m)
	}
	r.Persistence = ""
	if m := r.persistenceMode(); m != "" {
		t.Errorf("persistenceMode without persistence_config = %q, want DISABLED", m)
	}
	if s := r.spec("n0"); s.Configs["appendonly"] != "no" || s.Configs["save"] != "" {
		t.Errorf("Redis cluster without persistence: %v", s.Configs)
	}
	v := &clusterRecord{Name: "projects/p/locations/l/instances/v", ShardCount: 1, Valkey: &valkeyFields{Mode: "CLUSTER_DISABLED",
		NodeType: "SHARED_CORE_NANO", Persistence: protoJSON(&memorystorepb.PersistenceConfig{Mode: memorystorepb.PersistenceConfig_RDB,
			RdbConfig: &memorystorepb.PersistenceConfig_RDBConfig{RdbSnapshotPeriod: memorystorepb.PersistenceConfig_RDBConfig_SIX_HOURS}})}, DataGen: 1}
	if s := v.spec("n0"); s.Dir != "/data/g1" || s.Configs["save"] != "21600 1" || s.Configs["appendonly"] != "no" {
		t.Errorf("Valkey RDB spec = %q %v", s.Dir, s.Configs)
	}
	if m := v.persistenceMode(); m != "RDB" {
		t.Errorf("Valkey persistenceMode = %q, want RDB", m)
	}
	if d := dataDir(0); d != "" {
		t.Errorf("dataDir(0) = %q, want none", d)
	}
}
