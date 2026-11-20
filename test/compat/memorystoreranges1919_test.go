//go:build compat

package compat

import (
	"context"
	"fmt"
	"testing"
	"time"

	redispb "cloud.google.com/go/redis/apiv1/redispb"
	"google.golang.org/grpc/codes"
)

// covers: google.cloud.redis.v1.CloudRedis/CreateInstance, google.cloud.redis.v1.CloudRedis/GetInstance
//
// TestMemorystoreReservedIPRanges (#1919), with cloud.google.com/go/redis/
// apiv1 and go-redis: a reserved_ip_range in a range the networking page's
// "Restricted ranges" (the VPC's prohibited ranges) lists, or reaching out
// of its private range, is INVALID_ARGUMENT; one overlapping another
// instance's range on the same authorized network is INVALID_ARGUMENT
// ("Range must be unique and non-overlapping with existing subnets in an
// authorized network"), the same range on another network is not, and an
// instance with no range gets one no other instance there uses.
func TestMemorystoreReservedIPRanges(t *testing.T) {
	h := New(t)
	c, project := memorystoreAdminClient(t, h)
	ctx := memorystoreWait(t)
	parent := "projects/" + project + "/locations/us-central1"
	suffix := time.Now().UnixNano() % 1e7
	network := fmt.Sprintf("projects/%s/global/networks/rng-%d", project, suffix)
	// Beside Compute Engine (#1936) both authorized networks must exist, as
	// on Google.
	if addr := memorystoreComputeAddr(h); addr != "" {
		computeNetwork(t, addr, project, fmt.Sprintf("rng-%d", suffix), "", "")
		computeNetwork(t, addr, project, fmt.Sprintf("rng-%d-2", suffix), "", "")
	}
	for _, rg := range []string{"169.254.0.0/29", "127.0.0.0/29", "199.36.153.0/29", "224.0.0.0/29", "0.0.0.0/29", "10.0.0.0/7"} {
		_, err := c.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: parent, InstanceId: "rng-refused",
			Instance: &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1, AuthorizedNetwork: network, ReservedIpRange: rg}})
		wantRedisCode(t, "CreateInstance with reserved_ip_range "+rg, err, codes.InvalidArgument)
	}
	create := func(id string, in *redispb.Instance) *redispb.Instance {
		t.Helper()
		name := parent + "/instances/" + id
		t.Cleanup(func() {
			if op, err := c.DeleteInstance(context.Background(), &redispb.DeleteInstanceRequest{Name: name}); err == nil {
				_ = waitLRODone(context.Background(), op)
			}
		})
		in.Tier, in.MemorySizeGb = redispb.Instance_BASIC, 1
		op, err := c.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: parent, InstanceId: id, Instance: in})
		if err != nil {
			t.Fatalf("CreateInstance %s: %v", id, err)
		}
		if _, err := waitLRO(ctx, op); err != nil {
			t.Fatal(err)
		}
		got, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	a := create(fmt.Sprintf("rng-a-%d", suffix), &redispb.Instance{AuthorizedNetwork: network, ReservedIpRange: "10.20.0.0/28"})
	if a.GetReservedIpRange() != "10.20.0.0/28" {
		t.Errorf("reserved_ip_range %q", a.GetReservedIpRange())
	}
	rc := redisAt(a, "")
	defer rc.Close()
	if err := rc.Ping(ctx).Err(); err != nil {
		t.Errorf("PING: %v", err)
	}
	for _, rg := range []string{"10.20.0.8/29", "10.20.0.0/16"} {
		_, err := c.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: parent, InstanceId: "rng-overlap",
			Instance: &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1, AuthorizedNetwork: network, ReservedIpRange: rg}})
		wantRedisCode(t, "CreateInstance overlapping "+a.GetName()+" with "+rg, err, codes.InvalidArgument)
	}
	other := create(fmt.Sprintf("rng-b-%d", suffix), &redispb.Instance{AuthorizedNetwork: network + "-2", ReservedIpRange: "10.20.0.8/29"})
	if other.GetReservedIpRange() != "10.20.0.8/29" {
		t.Errorf("the same range on another network: %q", other.GetReservedIpRange())
	}
}
