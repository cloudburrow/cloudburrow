package main

import (
	"strings"
	"testing"

	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
)

// TestConsoleRedisClusterReplication (#1577): the Create cluster page's
// Secondary of field asks for a secondary of the cluster named; a page
// shows the role, primary, secondaries and membership; a secondary offers
// Switchover and Detach, a primary Detach secondaries, others neither.
func TestConsoleRedisClusterReplication(t *testing.T) {
	_, fields := redisClusterProvider{}.CreateForm()
	found := false
	for _, f := range fields {
		found = found || f.Name == "primaryCluster"
	}
	if !found {
		t.Error("Create cluster has no Secondary of field")
	}
	const p, s = "projects/p/locations/us-central1/clusters/a", "projects/p/locations/us-central1/clusters/b"
	in := &clusterpb.Cluster{}
	redisClusterSecondaryOf(in, map[string]string{"primaryCluster": " " + p + " "})
	if cfg := in.GetCrossClusterReplicationConfig(); cfg.GetClusterRole() != clusterpb.CrossClusterReplicationConfig_SECONDARY || cfg.GetPrimaryCluster().GetCluster() != p {
		t.Errorf("Secondary of = %v", cfg)
	}
	redisClusterSecondaryOf(in, map[string]string{})

	sec := &clusterpb.Cluster{Name: s, CrossClusterReplicationConfig: &clusterpb.CrossClusterReplicationConfig{
		ClusterRole:    clusterpb.CrossClusterReplicationConfig_SECONDARY,
		PrimaryCluster: &clusterpb.CrossClusterReplicationConfig_RemoteCluster{Cluster: p},
		Membership: &clusterpb.CrossClusterReplicationConfig_Membership{PrimaryCluster: &clusterpb.CrossClusterReplicationConfig_RemoteCluster{Cluster: p},
			SecondaryClusters: []*clusterpb.CrossClusterReplicationConfig_RemoteCluster{{Cluster: s}}}}}
	var text []string
	for _, pr := range redisClusterReplicationProps(sec) {
		text = append(text, pr.Label+"="+pr.Value)
	}
	if got := strings.Join(text, "; "); !strings.Contains(got, "Cross-region replication=SECONDARY") || !strings.Contains(got, "Primary cluster="+p) ||
		!strings.Contains(got, "Replication membership="+p+" (primary), "+s) {
		t.Errorf("secondary's properties = %s", got)
	}
	ids := func(c *clusterpb.Cluster) string {
		var out []string
		for _, a := range redisClusterReplicationActions(c) {
			out = append(out, a.ID)
		}
		return strings.Join(out, ",")
	}
	if got := ids(sec); got != "switchover,detach-replication" {
		t.Errorf("secondary's actions = %s", got)
	}
	prim := &clusterpb.Cluster{Name: p, CrossClusterReplicationConfig: &clusterpb.CrossClusterReplicationConfig{ClusterRole: clusterpb.CrossClusterReplicationConfig_PRIMARY}}
	if got := ids(prim); got != "detach-secondaries" {
		t.Errorf("primary's actions = %s", got)
	}
	if got := ids(&clusterpb.Cluster{Name: p}); got != "" {
		t.Errorf("an unreplicated cluster's actions = %s", got)
	}
	if props := redisClusterReplicationProps(&clusterpb.Cluster{}); len(props) != 1 || props[0].Value != "NONE" {
		t.Errorf("an unreplicated cluster's properties = %v", props)
	}
}
