//go:build compat

package compat

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"cloud.google.com/go/memorystore/apiv1/memorystorepb"
	"cloud.google.com/go/redis/apiv1/redispb"
	"cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
)

// The memorystore and sql shards' names for the Compute Engine API
// (#1936), so its own suite runs only in served.
const (
	envMemorystoreCompute = "CLOUDBURROW_TEST_MEMORYSTORE_COMPUTE"
	envSQLCompute         = "CLOUDBURROW_TEST_SQL_COMPUTE"
)

// sqlComputeEndpoint is Compute Engine's address for a test that needs it
// beside Cloud SQL (#2141): the sql shards' SQL_COMPUTE, else COMPUTE
// (skipping without either), so the test runs where both are served.
func sqlComputeEndpoint(h *Harness) string {
	if v := os.Getenv(envSQLCompute); v != "" {
		return v
	}
	return h.Endpoint(EnvCompute)
}

// sqlComputeOpts are computeOpts at sqlComputeEndpoint.
func sqlComputeOpts(t *testing.T, h *Harness) []option.ClientOption {
	t.Helper()
	addr := sqlComputeEndpoint(h)
	if addr == "" {
		t.Fatal("no Compute Engine endpoint")
	}
	return []option.ClientOption{option.WithEndpoint("http://" + addr), option.WithoutAuthentication()}
}

// computeNetwork makes a custom mode network with a subnet and a
// VPC_PEERING allocated range through the official client, and deletes
// them when the test ends. ensure tolerates what a rerun left.
func computeNetwork(t *testing.T, addr, project, network, subnetRange, allocated string) {
	t.Helper()
	ctx := context.Background()
	opts := []option.ClientOption{option.WithEndpoint("http://" + addr), option.WithoutAuthentication()}
	nets, err := computeapi.NewNetworksRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	subs, _ := computeapi.NewSubnetworksRESTClient(ctx, opts...)
	gaddrs, _ := computeapi.NewGlobalAddressesRESTClient(ctx, opts...)
	wait := func(op *computeapi.Operation, err error) {
		t.Helper()
		if err != nil && computeCode(err) != 409 {
			t.Fatal(err)
		}
		if err == nil {
			if err := op.Wait(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	wait(nets.Insert(ctx, &computepb.InsertNetworkRequest{Project: project, NetworkResource: &computepb.Network{
		Name: proto.String(network), AutoCreateSubnetworks: proto.Bool(false)}}))
	self := "projects/" + project + "/global/networks/" + network
	if subnetRange != "" {
		wait(subs.Insert(ctx, &computepb.InsertSubnetworkRequest{Project: project, Region: "us-central1", SubnetworkResource: &computepb.Subnetwork{
			Name: proto.String(network + "-s"), Network: proto.String(self), IpCidrRange: proto.String(subnetRange)}}))
	}
	if allocated != "" {
		wait(gaddrs.Insert(ctx, &computepb.InsertGlobalAddressRequest{Project: project, AddressResource: &computepb.Address{
			Name: proto.String(allocated), AddressType: proto.String("INTERNAL"), Purpose: proto.String("VPC_PEERING"),
			PrefixLength: proto.Int32(24), Network: proto.String(self)}}))
	}
	t.Cleanup(func() {
		if allocated != "" {
			_, _ = gaddrs.Delete(ctx, &computepb.DeleteGlobalAddressRequest{Project: project, Address: allocated})
		}
		if subnetRange != "" {
			_, _ = subs.Delete(ctx, &computepb.DeleteSubnetworkRequest{Project: project, Region: "us-central1", Subnetwork: network + "-s"})
		}
		_, _ = nets.Delete(ctx, &computepb.DeleteNetworkRequest{Project: project, Network: network})
	})
}

// memorystoreComputeAddr is the Compute Engine API beside Memorystore, or
// "" when the instance does not serve it (Memorystore then checks no
// network, as before #1936).
func memorystoreComputeAddr(h *Harness) string {
	if os.Getenv(envMemorystoreCompute) == "" && os.Getenv(EnvCompute) == "" {
		return ""
	}
	return runShardEndpoint(h, envMemorystoreCompute, EnvCompute)
}

// TestMemorystoreNetworksCheckedAgainstComputeEngine (#1936), with the
// official redis/apiv1, redis/cluster/apiv1 and memorystore/apiv1 clients
// beside Compute Engine: an authorized_network that does not exist, a
// reserved_ip_range overlapping a subnet of the network, and a
// PRIVATE_SERVICE_ACCESS range name that is not an allocated range are
// INVALID_ARGUMENT, as are a Redis Cluster PSC network and a Memorystore
// for Valkey PSC auto connection's network that do not exist.
func TestMemorystoreNetworksCheckedAgainstComputeEngine(t *testing.T) {
	h := New(t)
	addr := runShardEndpoint(h, envMemorystoreCompute, EnvCompute) // skips without Compute
	c, project := memorystoreAdminClient(t, h)
	cc, _ := redisClusterAdminClient(t, h)
	vc, _ := valkeyAdminClient(t, h)
	ctx := memorystoreWait(t)
	network := fmt.Sprintf("ms-vpc-%d", time.Now().UnixNano()%1e6)
	computeNetwork(t, addr, project, network, "10.91.0.0/24", network+"-psa")
	full := "projects/" + project + "/global/networks/" + network
	missing := "projects/" + project + "/global/networks/no-such-" + network
	parent := "projects/" + project + "/locations/us-central1"
	for what, in := range map[string]*redispb.Instance{
		"a network that does not exist": {AuthorizedNetwork: missing},
		"a range on a subnet":           {AuthorizedNetwork: full, ReservedIpRange: "10.91.0.8/29"},
		"an unknown allocated range": {AuthorizedNetwork: full, ConnectMode: redispb.Instance_PRIVATE_SERVICE_ACCESS,
			ReservedIpRange: "not-allocated"},
	} {
		in.Tier, in.MemorySizeGb = redispb.Instance_BASIC, 1
		_, err := c.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: parent, InstanceId: "vpc-refused", Instance: in})
		wantRedisCode(t, "CreateInstance with "+what, err, codes.InvalidArgument)
		if err != nil && what == "a range on a subnet" && !strings.Contains(err.Error(), "non-overlapping") {
			t.Errorf("CreateInstance with %s: %v", what, err)
		}
	}
	_, err := cc.CreateCluster(ctx, &clusterpb.CreateClusterRequest{Parent: parent, ClusterId: "vpc-refused", Cluster: &clusterpb.Cluster{
		ShardCount: proto.Int32(3), ReplicaCount: proto.Int32(0), NodeType: clusterpb.NodeType_REDIS_SHARED_CORE_NANO,
		AuthorizationMode: clusterpb.AuthorizationMode_AUTH_MODE_DISABLED, PscConfigs: []*clusterpb.PscConfig{{Network: missing}}}})
	wantRedisCode(t, "CreateCluster on a network that does not exist", err, codes.InvalidArgument)
	_, err = vc.CreateInstance(ctx, &memorystorepb.CreateInstanceRequest{Parent: parent, InstanceId: "vpc-refused", Instance: &memorystorepb.Instance{
		Mode: memorystorepb.Instance_CLUSTER_DISABLED, ReplicaCount: proto.Int32(0), NodeType: memorystorepb.Instance_SHARED_CORE_NANO,
		PscAutoConnections: []*memorystorepb.PscAutoConnection{{ProjectId: project, Network: missing}}}})
	wantRedisCode(t, "Memorystore CreateInstance on a network that does not exist", err, codes.InvalidArgument)
}

// TestCloudSQLPrivateNetworkCheckedAgainstComputeEngine (#1936), with the
// official sqladmin/v1 client beside Compute Engine: instances.insert with
// a settings.ipConfiguration.privateNetwork that does not exist, or an
// allocatedIpRange that is not one of the network's allocated ranges, is
// refused with HTTP 400 before anything is made.
func TestCloudSQLPrivateNetworkCheckedAgainstComputeEngine(t *testing.T) {
	h := New(t)
	addr := runShardEndpoint(h, envSQLCompute, EnvCompute) // skips without Compute
	svc := sqlAdminService(t, h)
	project := h.Project()
	network := fmt.Sprintf("sql-vpc-%d", time.Now().UnixNano()%1e6)
	computeNetwork(t, addr, project, network, "", network+"-psa")
	for what, ip := range map[string]*sqladmin.IpConfiguration{
		"a network that does not exist": {PrivateNetwork: "projects/" + project + "/global/networks/no-such-" + network},
		"an unknown allocated range":    {PrivateNetwork: "projects/" + project + "/global/networks/" + network, AllocatedIpRange: "not-allocated"},
	} {
		_, err := svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: "vpc-refused", DatabaseVersion: "POSTGRES_17",
			Region: "us-central1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", IpConfiguration: ip}}).Do()
		wantHTTP(t, "instances.insert with "+what, err, 400)
	}
}
