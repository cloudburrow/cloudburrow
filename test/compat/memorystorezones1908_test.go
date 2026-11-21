//go:build compat

package compat

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	redispb "cloud.google.com/go/redis/apiv1/redispb"
	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// nodeZones are the availability_zone HELLO returns on every shard's
// nodes of a cluster client, primaries and replicas.
func nodeZones(t *testing.T, ctx context.Context, rc *redis.ClusterClient, nodes int) []string {
	t.Helper()
	// A replica is in CLUSTER SLOTS once it has synced with its primary.
	deadline := time.Now().Add(2 * time.Minute)
	for ; ; time.Sleep(time.Second) {
		listed := 0
		if slots, err := rc.ClusterSlots(ctx).Result(); err == nil {
			for _, sl := range slots {
				listed += len(sl.Nodes)
			}
		}
		if listed >= nodes {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("CLUSTER SLOTS lists %d nodes, want %d", listed, nodes)
		}
	}
	// ForEachMaster and ForEachSlave call each node in its own goroutine,
	// so the zones are gathered under a lock; and the client's view of the
	// nodes is reloaded in the background (ReloadState is lazy), so it may
	// still miss a replica: ask again until every node has answered.
	for {
		rc.ReloadState(ctx)
		var mu sync.Mutex
		var zones []string
		each := func(ctx context.Context, c *redis.Client) error {
			v, err := c.Do(ctx, "HELLO", "3").Result()
			if err != nil {
				return err
			}
			s := fmt.Sprint(v)
			z := ""
			if i := strings.Index(s, "availability_zone:"); i >= 0 {
				z = strings.Fields(s[i+len("availability_zone:"):])[0]
			}
			mu.Lock()
			zones = append(zones, z)
			mu.Unlock()
			return nil
		}
		if err := rc.ForEachMaster(ctx, each); err != nil {
			t.Fatal(err)
		}
		if err := rc.ForEachSlave(ctx, each); err != nil {
			t.Fatal(err)
		}
		if len(zones) >= nodes || time.Now().After(deadline) {
			return zones
		}
		time.Sleep(time.Second)
	}
}

// covers: google.cloud.redis.cluster.v1.CloudRedisCluster/CreateCluster, google.cloud.redis.cluster.v1.CloudRedisCluster/UpdateCluster, google.cloud.memorystore.v1.Memorystore/CreateInstance, google.cloud.memorystore.v1.Memorystore/GetInstance, google.cloud.memorystore.v1.Memorystore/UpdateInstance
//
// TestMemorystoreZoneDistribution (#1908), with cloud.google.com/go/redis/
// cluster/apiv1, cloud.google.com/go/memorystore/apiv1 and go-redis:
// SINGLE_ZONE without a zone, or with a zone of another region, is
// INVALID_ARGUMENT on both APIs, as is a Memorystore for Redis
// instance's location_id outside its region. A SINGLE_ZONE Redis cluster in
// us-central1-f reads it back, and each of its nodes reports that zone to
// clients (HELLO availability_zone); changing its zone distribution is
// INVALID_ARGUMENT. A MULTI_ZONE Valkey instance of one shard and two
// replicas has its three nodes in three zones of the region, which
// active_zones lists; its zone_distribution_config is immutable.
func TestMemorystoreZoneDistribution(t *testing.T) {
	h := New(t)
	cc, project := redisClusterAdminClient(t, h)
	vc, _ := valkeyAdminClient(t, h)
	ctx := memorystoreWait(t)
	parent := "projects/" + project + "/locations/us-central1"
	network := "projects/" + project + "/global/networks/default"
	clusterShape := func(z *clusterpb.ZoneDistributionConfig) *clusterpb.Cluster {
		return &clusterpb.Cluster{ShardCount: proto.Int32(1), ReplicaCount: proto.Int32(1), NodeType: clusterpb.NodeType_REDIS_SHARED_CORE_NANO,
			AuthorizationMode: clusterpb.AuthorizationMode_AUTH_MODE_DISABLED, PscConfigs: []*clusterpb.PscConfig{{Network: network}},
			ZoneDistributionConfig: z}
	}
	valkeyShape := func(z *memorystorepb.ZoneDistributionConfig) *memorystorepb.Instance {
		return &memorystorepb.Instance{ShardCount: 1, ReplicaCount: proto.Int32(2), NodeType: memorystorepb.Instance_SHARED_CORE_NANO,
			Endpoints: []*memorystorepb.Instance_InstanceEndpoint{{Connections: []*memorystorepb.Instance_ConnectionDetail{{
				Connection: &memorystorepb.Instance_ConnectionDetail_PscAutoConnection{PscAutoConnection: &memorystorepb.PscAutoConnection{
					ProjectId: project, Network: network}}}}}},
			ZoneDistributionConfig: z}
	}
	for _, z := range []string{"", "europe-west1-b"} {
		_, err := cc.CreateCluster(ctx, &clusterpb.CreateClusterRequest{Parent: parent, ClusterId: "zones-refused",
			Cluster: clusterShape(&clusterpb.ZoneDistributionConfig{Mode: clusterpb.ZoneDistributionConfig_SINGLE_ZONE, Zone: z})})
		wantRedisCode(t, "CreateCluster SINGLE_ZONE in "+z, err, codes.InvalidArgument)
		_, err = vc.CreateInstance(ctx, &memorystorepb.CreateInstanceRequest{Parent: parent, InstanceId: "zones-refused",
			Instance: valkeyShape(&memorystorepb.ZoneDistributionConfig{Mode: memorystorepb.ZoneDistributionConfig_SINGLE_ZONE, Zone: z})})
		wantRedisCode(t, "Valkey CreateInstance SINGLE_ZONE in "+z, err, codes.InvalidArgument)
	}
	// A Memorystore for Redis instance's location_id is a zone of its region.
	rcl, _ := memorystoreAdminClient(t, h)
	_, err := rcl.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: parent, InstanceId: "zones-refused",
		Instance: &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1, LocationId: "europe-west1-b"}})
	wantRedisCode(t, "CreateInstance with a location_id of another region", err, codes.InvalidArgument)
	suffix := time.Now().UnixNano() % 1e7

	cname := parent + "/clusters/" + fmt.Sprintf("zones-%d", suffix)
	t.Cleanup(func() {
		if op, err := cc.DeleteCluster(context.Background(), &clusterpb.DeleteClusterRequest{Name: cname}); err == nil {
			_ = waitLRODone(context.Background(), op)
		}
	})
	cop, err := cc.CreateCluster(ctx, &clusterpb.CreateClusterRequest{Parent: parent, ClusterId: fmt.Sprintf("zones-%d", suffix),
		Cluster: clusterShape(&clusterpb.ZoneDistributionConfig{Mode: clusterpb.ZoneDistributionConfig_SINGLE_ZONE, Zone: "us-central1-f"})})
	if err != nil {
		t.Fatal(err)
	}
	cl, err := waitLRO(ctx, cop)
	if err != nil {
		t.Fatal(err)
	}
	if z := cl.GetZoneDistributionConfig(); z.GetMode() != clusterpb.ZoneDistributionConfig_SINGLE_ZONE || z.GetZone() != "us-central1-f" {
		t.Errorf("zone_distribution_config = %v", z)
	}
	ep := cl.GetDiscoveryEndpoints()[0]
	rc := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{fmt.Sprintf("%s:%d", ep.GetAddress(), ep.GetPort())}})
	defer rc.Close()
	if zones := nodeZones(t, ctx, rc, 2); len(zones) != 2 || zones[0] != "us-central1-f" || zones[1] != "us-central1-f" {
		t.Errorf("SINGLE_ZONE nodes report zones %v, want us-central1-f twice", zones)
	}
	_, err = cc.UpdateCluster(ctx, &clusterpb.UpdateClusterRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"zone_distribution_config"}},
		Cluster: &clusterpb.Cluster{Name: cname, ZoneDistributionConfig: &clusterpb.ZoneDistributionConfig{Mode: clusterpb.ZoneDistributionConfig_MULTI_ZONE}}})
	wantRedisCode(t, "UpdateCluster of zone_distribution_config", err, codes.InvalidArgument)

	vid := fmt.Sprintf("zones-vk-%d", suffix)
	vname := parent + "/instances/" + vid
	t.Cleanup(func() {
		if op, err := vc.DeleteInstance(context.Background(), &memorystorepb.DeleteInstanceRequest{Name: vname}); err == nil {
			_ = waitLRODone(context.Background(), op)
		}
	})
	vop, err := vc.CreateInstance(ctx, &memorystorepb.CreateInstanceRequest{Parent: parent, InstanceId: vid,
		Instance: valkeyShape(&memorystorepb.ZoneDistributionConfig{Mode: memorystorepb.ZoneDistributionConfig_MULTI_ZONE})})
	if err != nil {
		t.Fatal(err)
	}
	inst, err := waitLRO(ctx, vop)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(inst.GetZoneDistributionConfig().GetActiveZones(), ","); got != "us-central1-a,us-central1-b,us-central1-c" {
		t.Errorf("MULTI_ZONE active_zones = %q", got)
	}
	vrc := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{valkeyEndpoint(t, inst, memorystorepb.ConnectionType_CONNECTION_TYPE_DISCOVERY)}})
	defer vrc.Close()
	seen := map[string]bool{}
	for _, z := range nodeZones(t, ctx, vrc, 3) {
		seen[z] = true
	}
	if len(seen) != 3 || seen[""] {
		t.Errorf("MULTI_ZONE nodes report zones %v, want three zones", seen)
	}
	_, err = vc.UpdateInstance(ctx, &memorystorepb.UpdateInstanceRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"zone_distribution_config"}},
		Instance: &memorystorepb.Instance{Name: vname, ZoneDistributionConfig: &memorystorepb.ZoneDistributionConfig{Mode: memorystorepb.ZoneDistributionConfig_SINGLE_ZONE, Zone: "us-central1-a"}}})
	wantRedisCode(t, "Valkey UpdateInstance of zone_distribution_config", err, codes.InvalidArgument)
}
