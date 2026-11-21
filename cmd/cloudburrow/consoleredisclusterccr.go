package main

import (
	"context"
	"strings"

	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/redisadmin"
)

// Cross-region replication on the Redis Cluster pages (#1577), as Google's
// console and gcloud redis clusters create --cross-cluster-replication-role
// secondary --primary-cluster, switchover, detach and detach-secondaries
// offer it: a secondary is created from the Create cluster page's
// "Secondary of" field, a cluster's page shows its role, primary,
// secondaries and membership, and its actions are a secondary's Switchover
// and Detach, and a primary's Detach secondaries.

// redisClusterSecondaryField is the Create cluster page's field.
var redisClusterSecondaryField = console.Field{Name: "primaryCluster", Label: "Secondary of (cross-region replication)", Type: "text", Section: msSectionAdvanced,
	Help: "Optional. projects/{project}/locations/{location}/clusters/{cluster}: the cluster replicates that primary and is read-only; it takes the primary's shard count, node type, security, in-transit encryption, Redis configs and persistence."}

// redisClusterSecondaryOf sets in's cross_cluster_replication_config from
// the Create cluster page's values.
func redisClusterSecondaryOf(in *clusterpb.Cluster, values map[string]string) {
	if p := strings.TrimSpace(values["primaryCluster"]); p != "" {
		in.CrossClusterReplicationConfig = &clusterpb.CrossClusterReplicationConfig{ClusterRole: clusterpb.CrossClusterReplicationConfig_SECONDARY,
			PrimaryCluster: &clusterpb.CrossClusterReplicationConfig_RemoteCluster{Cluster: p}}
	}
}

// redisClusterReplicationProps are a cluster's role, primary, secondaries
// and membership.
func redisClusterReplicationProps(c *clusterpb.Cluster) []console.Property {
	cfg := c.GetCrossClusterReplicationConfig()
	role := "NONE"
	if cfg != nil {
		role = cfg.GetClusterRole().String()
	}
	props := []console.Property{{Label: "Cross-region replication", Value: role}}
	if cfg == nil {
		return props
	}
	var secs, members []string
	for _, s := range cfg.GetSecondaryClusters() {
		secs = append(secs, s.GetCluster())
	}
	if m := cfg.GetMembership(); m != nil {
		members = append(members, m.GetPrimaryCluster().GetCluster()+" (primary)")
		for _, s := range m.GetSecondaryClusters() {
			members = append(members, s.GetCluster())
		}
	}
	return append(props,
		console.Property{Label: "Primary cluster", Value: orDash(cfg.GetPrimaryCluster().GetCluster())},
		console.Property{Label: "Secondary clusters", Value: orDash(strings.Join(secs, ", "))},
		console.Property{Label: "Replication membership", Value: orDash(strings.Join(members, ", "))})
}

// redisClusterReplicationActions are a secondary's Switchover and Detach,
// and a primary's Detach secondaries.
func redisClusterReplicationActions(c *clusterpb.Cluster) []console.Action {
	switch c.GetCrossClusterReplicationConfig().GetClusterRole() {
	case clusterpb.CrossClusterReplicationConfig_SECONDARY:
		return []console.Action{
			{ID: "switchover", Label: "Switchover", Confirm: "This cluster becomes the primary, writable; its primary and the other secondaries replicate it."},
			{ID: "detach-replication", Label: "Detach", Destructive: true, Confirm: "This cluster stops replicating its primary and becomes writable."}}
	case clusterpb.CrossClusterReplicationConfig_PRIMARY:
		return []console.Action{{ID: "detach-secondaries", Label: "Detach secondaries", Destructive: true, Fields: []console.Field{
			{Name: "clusters", Label: "Secondary clusters to detach", Type: "text", Required: true,
				Help: "Comma-separated projects/{project}/locations/{location}/clusters/{cluster}: each stops replicating and becomes writable."}}}}
	}
	return nil
}

// redisClusterReplicationAct runs one of redisClusterReplicationActions
// as gcloud sends it (UpdateCluster of cross_cluster_replication_config),
// waited for; ok is false for an action of another kind.
func redisClusterReplicationAct(ctx context.Context, api *redisadmin.ClusterServer, srv *redisadmin.Server, c *clusterpb.Cluster, action string, values map[string]string) (ok bool, err error) {
	cfg := c.GetCrossClusterReplicationConfig()
	var want *clusterpb.CrossClusterReplicationConfig
	switch action {
	case "detach-replication":
		want = &clusterpb.CrossClusterReplicationConfig{ClusterRole: clusterpb.CrossClusterReplicationConfig_NONE}
	case "switchover":
		secs := []*clusterpb.CrossClusterReplicationConfig_RemoteCluster{{Cluster: cfg.GetPrimaryCluster().GetCluster()}}
		for _, s := range cfg.GetMembership().GetSecondaryClusters() {
			if s.GetCluster() != c.GetName() {
				secs = append(secs, &clusterpb.CrossClusterReplicationConfig_RemoteCluster{Cluster: s.GetCluster()})
			}
		}
		want = &clusterpb.CrossClusterReplicationConfig{ClusterRole: clusterpb.CrossClusterReplicationConfig_PRIMARY, SecondaryClusters: secs}
	case "detach-secondaries":
		gone := map[string]bool{}
		for _, s := range strings.Split(values["clusters"], ",") {
			gone[strings.TrimSpace(s)] = true
		}
		var keep []*clusterpb.CrossClusterReplicationConfig_RemoteCluster
		for _, s := range cfg.GetSecondaryClusters() {
			if !gone[s.GetCluster()] {
				keep = append(keep, &clusterpb.CrossClusterReplicationConfig_RemoteCluster{Cluster: s.GetCluster()})
			}
		}
		want = &clusterpb.CrossClusterReplicationConfig{ClusterRole: clusterpb.CrossClusterReplicationConfig_PRIMARY, SecondaryClusters: keep}
		if len(keep) == 0 {
			want = &clusterpb.CrossClusterReplicationConfig{ClusterRole: clusterpb.CrossClusterReplicationConfig_NONE}
		}
	default:
		return false, nil
	}
	op, err := api.UpdateCluster(ctx, &clusterpb.UpdateClusterRequest{
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"cross_cluster_replication_config"}},
		Cluster:    &clusterpb.Cluster{Name: c.GetName(), CrossClusterReplicationConfig: want}})
	if err != nil {
		return true, err
	}
	return true, waitRedisOp(ctx, srv, op.GetName())
}
