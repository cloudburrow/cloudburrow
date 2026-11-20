//go:build compat

package compat

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	redispb "cloud.google.com/go/redis/apiv1/redispb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// covers: google.cloud.redis.v1.CloudRedis/CreateInstance, google.cloud.redis.v1.CloudRedis/GetInstance, google.cloud.redis.v1.CloudRedis/UpdateInstance
//
// TestMemorystoreInstanceNetwork (#1906), with cloud.google.com/go/redis/
// apiv1 and go-redis: an instance created with no network fields reads
// back on the project's default network, DIRECT_PEERING, with a /29
// reserved_ip_range chosen; one created with PRIVATE_SERVICE_ACCESS on a
// network of its own and an allocated range's name reads them back, and
// serves (every local client reaches it, whatever the network). A range
// smaller than /29, a public range, a network that is not a full name and
// a range name with DIRECT_PEERING are INVALID_ARGUMENT, as is an update
// of connect_mode, authorized_network or reserved_ip_range (Google: "you
// cannot switch the connection mode of an existing instance").
func TestMemorystoreInstanceNetwork(t *testing.T) {
	h := New(t)
	c, project := memorystoreAdminClient(t, h)
	ctx := memorystoreWait(t)
	parent := "projects/" + project + "/locations/us-central1"
	for _, in := range []*redispb.Instance{
		{ReservedIpRange: "10.0.0.0/30"},
		{ReservedIpRange: "8.8.8.0/29"},
		{ReservedIpRange: "google-managed-services-default"},
		{AuthorizedNetwork: "networks/default"},
	} {
		in.Tier, in.MemorySizeGb = redispb.Instance_BASIC, 1
		_, err := c.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: parent, InstanceId: "net-refused", Instance: in})
		wantRedisCode(t, fmt.Sprintf("CreateInstance(%v)", in), err, codes.InvalidArgument)
	}
	create := func(id string, in *redispb.Instance) *redispb.Instance {
		t.Helper()
		name := parent + "/instances/" + id
		t.Cleanup(func() {
			if op, err := c.DeleteInstance(context.Background(), &redispb.DeleteInstanceRequest{Name: name}); err == nil {
				_ = waitLRODone(context.Background(), op)
			}
		})
		op, err := c.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: parent, InstanceId: id, Instance: in})
		if err != nil {
			t.Fatalf("CreateInstance: %v", err)
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
	suffix := time.Now().UnixNano() % 1e7
	def := create(fmt.Sprintf("net-def-%d", suffix), &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1})
	if def.GetAuthorizedNetwork() != "projects/"+project+"/global/networks/default" || def.GetConnectMode() != redispb.Instance_DIRECT_PEERING ||
		!strings.HasPrefix(def.GetReservedIpRange(), "10.") || !strings.HasSuffix(def.GetReservedIpRange(), "/29") {
		t.Errorf("defaults: authorized_network %q, connect_mode %v, reserved_ip_range %q", def.GetAuthorizedNetwork(), def.GetConnectMode(), def.GetReservedIpRange())
	}
	network := "projects/" + project + "/global/networks/app-vpc"
	psa := create(fmt.Sprintf("net-psa-%d", suffix), &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1,
		AuthorizedNetwork: network, ConnectMode: redispb.Instance_PRIVATE_SERVICE_ACCESS, ReservedIpRange: "my-allocated-range"})
	if psa.GetAuthorizedNetwork() != network || psa.GetConnectMode() != redispb.Instance_PRIVATE_SERVICE_ACCESS || psa.GetReservedIpRange() != "my-allocated-range" {
		t.Errorf("PRIVATE_SERVICE_ACCESS: authorized_network %q, connect_mode %v, reserved_ip_range %q", psa.GetAuthorizedNetwork(), psa.GetConnectMode(), psa.GetReservedIpRange())
	}
	rc := redisAt(psa, "")
	defer rc.Close()
	if err := rc.Ping(ctx).Err(); err != nil {
		t.Errorf("PING the PRIVATE_SERVICE_ACCESS instance: %v", err)
	}
	for _, path := range []string{"connect_mode", "authorized_network", "reserved_ip_range"} {
		_, err := c.UpdateInstance(ctx, &redispb.UpdateInstanceRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{path}},
			Instance: &redispb.Instance{Name: psa.GetName(), ConnectMode: redispb.Instance_DIRECT_PEERING, AuthorizedNetwork: "projects/" + project + "/global/networks/default", ReservedIpRange: "10.1.0.0/29"}})
		wantRedisCode(t, "UpdateInstance of "+path, err, codes.InvalidArgument)
	}
}
