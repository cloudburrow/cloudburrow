//go:build compat

package compat

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	redispb "cloud.google.com/go/redis/apiv1/redispb"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// role is INFO replication's role of the server rc reaches.
func role(ctx context.Context, t *testing.T, rc *redis.Client) string {
	t.Helper()
	info, err := rc.Info(ctx, "replication").Result()
	if err != nil {
		t.Fatalf("INFO replication: %v", err)
	}
	for _, l := range strings.Split(info, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "role:"); ok {
			return v
		}
	}
	return ""
}

// eventually polls f for up to a minute.
func eventuallyTrue(t *testing.T, what string, f func() bool) {
	t.Helper()
	for end := time.Now().Add(time.Minute); ; {
		if f() {
			return
		}
		if time.Now().After(end) {
			t.Fatalf("%s: not within a minute", what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// covers: google.cloud.redis.v1.CloudRedis/CreateInstance, google.cloud.redis.v1.CloudRedis/FailoverInstance, google.cloud.redis.v1.CloudRedis/UpdateInstance
//
// TestMemorystoreStandardTierFailover (#1385), with cloud.google.com/go/
// redis/apiv1: a STANDARD_HA instance with read replicas
// (READ_REPLICAS_ENABLED, replica_count 2) is three Valkey servers, nodes
// node-0 to node-2, its primary in location_id and current_location_id.
// A key SET at host:port is read from a replica at read_endpoint, which
// refuses writes (READONLY). FailoverInstance (LIMITED_DATA_LOSS)
// promotes a replica: host:port is the same, and a client there reaches
// the new primary (role:master) with the key, and current_location_id
// moves. UpdateInstance of replica_count to 3 adds a node, which has the
// data.
func TestMemorystoreStandardTierFailover(t *testing.T) {
	h := New(t)
	c, project := memorystoreAdminClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	parent := "projects/" + project + "/locations/us-central1"
	id := fmt.Sprintf("ha-%d", time.Now().UnixNano()%1e8)
	name := parent + "/instances/" + id
	op, err := c.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: parent, InstanceId: id,
		Instance: &redispb.Instance{Tier: redispb.Instance_STANDARD_HA, MemorySizeGb: 5, AuthEnabled: true,
			LocationId: "us-central1-a", AlternativeLocationId: "us-central1-c",
			ReadReplicasMode: redispb.Instance_READ_REPLICAS_ENABLED, ReplicaCount: 2}})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(context.Background(), &redispb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	inst, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateInstance operation: %v", err)
	}
	if inst.GetTier() != redispb.Instance_STANDARD_HA || inst.GetReplicaCount() != 2 || len(inst.GetNodes()) != 3 ||
		inst.GetCurrentLocationId() != "us-central1-a" || inst.GetAlternativeLocationId() != "us-central1-c" ||
		inst.GetReadReplicasMode() != redispb.Instance_READ_REPLICAS_ENABLED || inst.GetReadEndpointPort() == 0 {
		t.Fatalf("the created instance reads back as %v", inst)
	}
	auth, err := c.GetInstanceAuthString(ctx, &redispb.GetInstanceAuthStringRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	pw := auth.GetAuthString()
	primary := redisAt(inst, pw)
	defer primary.Close()
	if r := role(ctx, t, primary); r != "master" {
		t.Fatalf("host:port reaches a %s", r)
	}
	if err := primary.Set(ctx, "k", "v", 0).Err(); err != nil {
		t.Fatalf("SET on the primary: %v", err)
	}
	reader := redis.NewClient(&redis.Options{Addr: net.JoinHostPort(inst.GetReadEndpoint(), strconv.Itoa(int(inst.GetReadEndpointPort()))), Password: pw})
	defer reader.Close()
	eventuallyTrue(t, "the replica has the key", func() bool { v, err := reader.Get(ctx, "k").Result(); return err == nil && v == "v" })
	if r := role(ctx, t, reader); r != "slave" {
		t.Errorf("the read endpoint reaches a %s, want a replica", r)
	}
	if err := reader.Set(ctx, "w", "1", 0).Err(); err == nil || !strings.Contains(err.Error(), "READONLY") {
		t.Errorf("SET at the read endpoint = %v, want READONLY", err)
	}

	fop, err := c.FailoverInstance(ctx, &redispb.FailoverInstanceRequest{Name: name,
		DataProtectionMode: redispb.FailoverInstanceRequest_LIMITED_DATA_LOSS})
	if err != nil {
		t.Fatalf("FailoverInstance: %v", err)
	}
	after, err := fop.Wait(ctx)
	if err != nil {
		t.Fatalf("FailoverInstance operation: %v", err)
	}
	if after.GetHost() != inst.GetHost() || after.GetPort() != inst.GetPort() {
		t.Errorf("the primary endpoint moved: %s:%d, was %s:%d", after.GetHost(), after.GetPort(), inst.GetHost(), inst.GetPort())
	}
	if after.GetCurrentLocationId() == "us-central1-a" || after.GetState() != redispb.Instance_READY {
		t.Errorf("after failover: current_location_id %s, state %s", after.GetCurrentLocationId(), after.GetState())
	}
	next := redisAt(after, pw)
	defer next.Close()
	if v, err := next.Get(ctx, "k").Result(); err != nil || v != "v" {
		t.Errorf("GET k on the new primary = %q, %v", v, err)
	}
	if r := role(ctx, t, next); r != "master" {
		t.Errorf("after failover host:port reaches a %s", r)
	}
	if err := next.Set(ctx, "k2", "v2", 0).Err(); err != nil {
		t.Errorf("SET on the new primary: %v", err)
	}
	eventuallyTrue(t, "a replica of the new primary has its write", func() bool {
		v, err := reader.Get(ctx, "k2").Result()
		return err == nil && v == "v2"
	})

	uop, err := c.UpdateInstance(ctx, &redispb.UpdateInstanceRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"replica_count"}},
		Instance: &redispb.Instance{Name: name, ReplicaCount: 3}})
	if err != nil {
		t.Fatalf("UpdateInstance replica_count: %v", err)
	}
	scaled, err := uop.Wait(ctx)
	if err != nil {
		t.Fatalf("UpdateInstance operation: %v", err)
	}
	if scaled.GetReplicaCount() != 3 || len(scaled.GetNodes()) != 4 {
		t.Errorf("after scaling: replica_count %d, nodes %v", scaled.GetReplicaCount(), scaled.GetNodes())
	}
	for i := 0; i < 6; i++ {
		rc := redis.NewClient(&redis.Options{Addr: net.JoinHostPort(scaled.GetReadEndpoint(), strconv.Itoa(int(scaled.GetReadEndpointPort()))), Password: pw})
		if v, err := rc.Get(ctx, "k2").Result(); err != nil || v != "v2" {
			t.Errorf("GET k2 at the read endpoint after scaling = %q, %v", v, err)
		}
		rc.Close()
	}
}

// covers: google.cloud.redis.v1.CloudRedis/CreateInstance, google.cloud.redis.v1.CloudRedis/FailoverInstance
//
// TestGcloudRedisFailover (#1385): the real gcloud creates a Standard tier
// instance (`--tier=standard`) and fails it over (`gcloud redis instances
// failover --data-protection-mode=force-data-loss`); the primary moves
// to the other zone and keeps the data at the same address.
func TestGcloudRedisFailover(t *testing.T) {
	h := New(t)
	c, _ := memorystoreAdminClient(t, h)
	g := newGcloudSession(t, h)
	ctx := h.Context()
	id := fmt.Sprintf("gcloud-ha-%d", time.Now().UnixNano()%1e8)
	name := "projects/" + g.project + "/locations/us-central1/instances/" + id
	p, region := "--project="+g.project, "--region=us-central1"
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(context.Background(), &redispb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	g.must("redis", "instances", "create", id, p, region, "--tier=standard", "--size=1", "--zone=us-central1-a")
	inst, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
	if err != nil || inst.GetTier() != redispb.Instance_STANDARD_HA || inst.GetReplicaCount() != 1 || inst.GetCurrentLocationId() != "us-central1-a" {
		t.Fatalf("after gcloud create, GetInstance = %v, %v", inst, err)
	}
	rc := redisAt(inst, "")
	defer rc.Close()
	if err := rc.Set(ctx, "k", "v", 0).Err(); err != nil {
		t.Fatal(err)
	}
	eventuallyTrue(t, "the replica has the key", func() bool {
		// WAIT 1: the write reached the replica.
		n, err := rc.Do(ctx, "WAIT", 1, 1000).Int()
		return err == nil && n >= 1
	})
	g.must("redis", "instances", "failover", id, p, region, "--data-protection-mode=force-data-loss", "--quiet")
	after, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
	if err != nil || after.GetCurrentLocationId() != inst.GetAlternativeLocationId() || after.GetPort() != inst.GetPort() {
		t.Fatalf("after gcloud failover, GetInstance = %v, %v", after, err)
	}
	next := redisAt(after, "")
	defer next.Close()
	if v, err := next.Get(ctx, "k").Result(); err != nil || v != "v" {
		t.Errorf("GET k after failover = %q, %v", v, err)
	}
	if r := role(ctx, t, next); r != "master" {
		t.Errorf("after failover the endpoint reaches a %s", r)
	}
}
