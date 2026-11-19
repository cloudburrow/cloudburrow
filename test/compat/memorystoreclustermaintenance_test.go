//go:build compat

package compat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"github.com/redis/go-redis/v9"
	"google.golang.org/genproto/googleapis/type/dayofweek"
	"google.golang.org/genproto/googleapis/type/timeofday"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// scheduleClusterMaintenance schedules a maintenance event on a Redis
// cluster or (api "memorystore") a Valkey instance through CloudBurrow's
// control endpoint, not a Memorystore API.
func scheduleClusterMaintenance(t *testing.T, h *Harness, name, api string) {
	t.Helper()
	url := "http://" + h.Endpoint(EnvMemorystoreAdmin) + "/cloudburrow/v1/" + name + ":scheduleMaintenance"
	res, err := http.Post(url, "application/json", bytes.NewBufferString(`{"api":"`+api+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	if res.StatusCode != http.StatusOK || out["maintenanceSchedule"] == nil {
		t.Fatalf("scheduleMaintenance: %d %v", res.StatusCode, out)
	}
}

// clusterNodeIDs are CLUSTER NODES' node IDs.
func clusterNodeIDs(h *Harness, rc *redis.ClusterClient) map[string]bool {
	rc.ReloadState(h.Context())
	nodes, err := rc.ClusterNodes(h.Context()).Result()
	out := map[string]bool{}
	if err != nil {
		return out
	}
	for _, l := range strings.Split(strings.TrimSpace(nodes), "\n") {
		if f := strings.Fields(l); len(f) > 0 {
			out[f[0]] = true
		}
	}
	return out
}

// covers: google.cloud.redis.cluster.v1.CloudRedisCluster/RescheduleClusterMaintenance, google.cloud.redis.cluster.v1.CloudRedisCluster/GetCluster
//
// TestRedisClusterRescheduleMaintenance (#1524), in the memorystore shard,
// with cloud.google.com/go/redis/cluster/apiv1 and gcloud: a one-shard
// cluster with a replica and a Sunday 03:00 UTC window. With nothing
// scheduled RescheduleClusterMaintenance is FAILED_PRECONDITION; once an
// event is scheduled (CloudBurrow's control endpoint), maintenance_schedule
// is the first window a week away, an hour long. SPECIFIC_TIME more than
// 14 days after the original time is INVALID_ARGUMENT; `gcloud redis
// clusters reschedule-maintenance --reschedule-type=specific-time` moves
// it ten days on, and the client again a day on. IMMEDIATE then applies
// it: every node is replaced (CLUSTER NODES names none of the old IDs),
// the keys are kept, and the cluster is ACTIVE with no
// maintenance_schedule.
func TestRedisClusterRescheduleMaintenance(t *testing.T) {
	h := New(t)
	c, project := redisClusterAdminClient(t, h)
	g := newGcloudSession(t, h)
	parent := "projects/" + project + "/locations/us-central1"
	id := fmt.Sprintf("maint-cl-%d", time.Now().UnixNano()%1e8)
	name := parent + "/clusters/" + id
	op, err := c.CreateCluster(h.Context(), &clusterpb.CreateClusterRequest{Parent: parent, ClusterId: id, Cluster: &clusterpb.Cluster{
		ShardCount: proto.Int32(1), ReplicaCount: proto.Int32(1), NodeType: clusterpb.NodeType_REDIS_SHARED_CORE_NANO,
		AuthorizationMode: clusterpb.AuthorizationMode_AUTH_MODE_DISABLED,
		PscConfigs:        []*clusterpb.PscConfig{{Network: "projects/" + project + "/global/networks/default"}},
		MaintenancePolicy: &clusterpb.ClusterMaintenancePolicy{WeeklyMaintenanceWindow: []*clusterpb.ClusterWeeklyMaintenanceWindow{
			{Day: dayofweek.DayOfWeek_SUNDAY, StartTime: &timeofday.TimeOfDay{Hours: 3}}}},
	}})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteCluster(h.Context(), &clusterpb.DeleteClusterRequest{Name: name}); err == nil {
			_ = waitLRODone(h.Context(), op)
		}
	})
	cl, err := waitLRO(memorystoreWait(t), op)
	if err != nil {
		t.Fatalf("CreateCluster operation: %v", err)
	}
	resched := func(typ clusterpb.RescheduleClusterMaintenanceRequest_RescheduleType, at time.Time) error {
		req := &clusterpb.RescheduleClusterMaintenanceRequest{Name: name, RescheduleType: typ}
		if !at.IsZero() {
			req.ScheduleTime = timestamppb.New(at)
		}
		op, err := c.RescheduleClusterMaintenance(h.Context(), req)
		if err != nil {
			return err
		}
		_, err = op.Wait(h.Context())
		return err
	}
	wantRedisCode(t, "RescheduleClusterMaintenance with nothing scheduled", resched(clusterpb.RescheduleClusterMaintenanceRequest_IMMEDIATE, time.Time{}), codes.FailedPrecondition)
	scheduleClusterMaintenance(t, h, name, "")
	got, err := c.GetCluster(h.Context(), &clusterpb.GetClusterRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	ms := got.GetMaintenanceSchedule()
	orig := ms.GetStartTime().AsTime()
	if orig.Weekday() != time.Sunday || orig.Hour() != 3 || time.Until(orig) < 7*24*time.Hour || !ms.GetEndTime().AsTime().Equal(orig.Add(time.Hour)) {
		t.Fatalf("maintenance_schedule = %v; want the first Sunday 03:00 UTC a week away, an hour long", ms)
	}
	wantRedisCode(t, "SPECIFIC_TIME 15 days on", resched(clusterpb.RescheduleClusterMaintenanceRequest_SPECIFIC_TIME, orig.Add(15*24*time.Hour)), codes.InvalidArgument)
	at := orig.Add(10 * 24 * time.Hour)
	g.must("redis", "clusters", "reschedule-maintenance", id, "--project="+project, "--region=us-central1",
		"--reschedule-type=specific-time", "--schedule-time="+at.Format(time.RFC3339), "--quiet")
	desc := strings.TrimSpace(g.must("redis", "clusters", "describe", id, "--project="+project, "--region=us-central1",
		"--format=value(maintenanceSchedule.startTime)"))
	if gt, err := time.Parse(time.RFC3339Nano, desc); err != nil || !gt.Equal(at) {
		t.Errorf("after gcloud reschedule-maintenance: startTime %q, want %s", desc, at.Format(time.RFC3339))
	}
	if err := resched(clusterpb.RescheduleClusterMaintenanceRequest_SPECIFIC_TIME, orig.Add(11*24*time.Hour)); err != nil {
		t.Fatalf("a second SPECIFIC_TIME: %v", err)
	}

	ep := cl.GetDiscoveryEndpoints()[0]
	rc := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{net.JoinHostPort(ep.GetAddress(), strconv.Itoa(int(ep.GetPort())))}})
	defer rc.Close()
	clusterKeys(t, h, rc, "m", 20, true)
	before := clusterNodeIDs(h, rc)
	if len(before) != 2 {
		t.Fatalf("CLUSTER NODES before maintenance: %v", before)
	}
	if err := resched(clusterpb.RescheduleClusterMaintenanceRequest_IMMEDIATE, time.Time{}); err != nil {
		t.Fatalf("IMMEDIATE: %v", err)
	}
	var after *clusterpb.Cluster
	replaced := false
	for end := time.Now().Add(8 * time.Minute); time.Now().Before(end) && !replaced; time.Sleep(2 * time.Second) {
		if after, err = c.GetCluster(h.Context(), &clusterpb.GetClusterRequest{Name: name}); err != nil {
			t.Fatal(err)
		}
		ep := after.GetDiscoveryEndpoints()
		if len(ep) == 0 {
			continue
		}
		rc2 := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{net.JoinHostPort(ep[0].GetAddress(), strconv.Itoa(int(ep[0].GetPort())))}})
		now := clusterNodeIDs(h, rc2)
		_ = rc2.Close()
		replaced = len(now) == 2
		for id := range now {
			replaced = replaced && !before[id]
		}
	}
	if !replaced || after.GetState() != clusterpb.Cluster_ACTIVE || after.GetMaintenanceSchedule() != nil {
		t.Fatalf("after IMMEDIATE: nodes replaced %v; cluster %v", replaced, after)
	}
	ep = after.GetDiscoveryEndpoints()[0]
	rc3 := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{net.JoinHostPort(ep.GetAddress(), strconv.Itoa(int(ep.GetPort())))}})
	defer rc3.Close()
	eventuallyTrue(t, "the keys are kept through the maintenance", func() bool {
		for i := 0; i < 20; i++ {
			if v, err := rc3.Get(h.Context(), fmt.Sprintf("m-%d", i)).Result(); err != nil || v != strconv.Itoa(i) {
				return false
			}
		}
		return true
	})
}

// covers: google.cloud.memorystore.v1.Memorystore/RescheduleMaintenance, google.cloud.memorystore.v1.Memorystore/GetInstance
//
// TestValkeyRescheduleMaintenance (#1524), in the memorystore shard, with
// cloud.google.com/go/memorystore/apiv1 and gcloud: a CLUSTER_DISABLED
// instance with a replica. With nothing scheduled RescheduleMaintenance is
// FAILED_PRECONDITION; once an event is scheduled, maintenance_schedule is
// a week on, an hour long; SPECIFIC_TIME before the original time is
// INVALID_ARGUMENT; `gcloud memorystore instances reschedule-maintenance
// --reschedule-type=specific-time` moves it. `--reschedule-type=immediate`
// then applies it: both nodes are replaced and the primary handed over
// (the primary endpoint's run_id changes), the primary endpoint keeps the
// key and takes writes, and the instance is ACTIVE with no
// maintenance_schedule.
func TestValkeyRescheduleMaintenance(t *testing.T) {
	h := New(t)
	c, project := valkeyAdminClient(t, h)
	g := newGcloudSession(t, h)
	parent := "projects/" + project + "/locations/us-central1"
	id := fmt.Sprintf("maint-vk-%d", time.Now().UnixNano()%1e8)
	name := parent + "/instances/" + id
	op, err := c.CreateInstance(h.Context(), &memorystorepb.CreateInstanceRequest{Parent: parent, InstanceId: id, Instance: &memorystorepb.Instance{
		Mode: memorystorepb.Instance_CLUSTER_DISABLED, ReplicaCount: proto.Int32(1), NodeType: memorystorepb.Instance_SHARED_CORE_NANO,
		PscAutoConnections: []*memorystorepb.PscAutoConnection{{ProjectId: project, Network: "projects/" + project + "/global/networks/default"}},
	}})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(h.Context(), &memorystorepb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = waitLRODone(h.Context(), op)
		}
	})
	inst, err := waitLRO(memorystoreWait(t), op)
	if err != nil {
		t.Fatalf("CreateInstance operation: %v", err)
	}
	resched := func(typ memorystorepb.RescheduleMaintenanceRequest_RescheduleType, at time.Time) error {
		req := &memorystorepb.RescheduleMaintenanceRequest{Name: name, RescheduleType: typ}
		if !at.IsZero() {
			req.ScheduleTime = timestamppb.New(at)
		}
		op, err := c.RescheduleMaintenance(h.Context(), req)
		if err != nil {
			return err
		}
		_, err = op.Wait(h.Context())
		return err
	}
	wantRedisCode(t, "RescheduleMaintenance with nothing scheduled", resched(memorystorepb.RescheduleMaintenanceRequest_IMMEDIATE, time.Time{}), codes.FailedPrecondition)
	scheduleClusterMaintenance(t, h, name, "memorystore")
	got, err := c.GetInstance(h.Context(), &memorystorepb.GetInstanceRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	ms := got.GetMaintenanceSchedule()
	orig := ms.GetStartTime().AsTime()
	if time.Until(orig) < 7*24*time.Hour-time.Minute || !ms.GetEndTime().AsTime().Equal(orig.Add(time.Hour)) {
		t.Fatalf("maintenance_schedule = %v; want a week on, an hour long", ms)
	}
	wantRedisCode(t, "SPECIFIC_TIME before the original time", resched(memorystorepb.RescheduleMaintenanceRequest_SPECIFIC_TIME, orig.Add(-time.Hour)), codes.InvalidArgument)
	at := orig.Add(13 * 24 * time.Hour)
	g.must("memorystore", "instances", "reschedule-maintenance", id, "--project="+project, "--location=us-central1",
		"--reschedule-type=specific-time", "--schedule-time="+at.Format(time.RFC3339), "--quiet")
	got, _ = c.GetInstance(h.Context(), &memorystorepb.GetInstanceRequest{Name: name})
	if !got.GetMaintenanceSchedule().GetStartTime().AsTime().Equal(at) {
		t.Errorf("after gcloud reschedule-maintenance: maintenance_schedule %v, want %s", got.GetMaintenanceSchedule(), at)
	}

	primary := redis.NewClient(&redis.Options{Addr: valkeyEndpoint(t, inst, memorystorepb.ConnectionType_CONNECTION_TYPE_PRIMARY)})
	defer primary.Close()
	if err := primary.Set(h.Context(), "kept", "yes", 0).Err(); err != nil {
		t.Fatal(err)
	}
	runID := func() string {
		info, err := primary.Info(h.Context(), "server").Result()
		if err != nil {
			return ""
		}
		return clusterInfoLine(info, "run_id")
	}
	oldRun := runID()
	g.must("memorystore", "instances", "reschedule-maintenance", id, "--project="+project, "--location=us-central1",
		"--reschedule-type=immediate", "--quiet")
	eventuallyWithin(t, 8*time.Minute, "the maintenance hands the primary over to a new node", func() bool {
		r := runID()
		return r != "" && r != oldRun
	})
	eventuallyWithin(t, 8*time.Minute, "the maintenance ends", func() bool {
		got, err := c.GetInstance(h.Context(), &memorystorepb.GetInstanceRequest{Name: name})
		return err == nil && got.GetState() == memorystorepb.Instance_ACTIVE && got.GetMaintenanceSchedule() == nil
	})
	eventuallyTrue(t, "the primary endpoint keeps the key and takes writes", func() bool {
		v, err := primary.Get(h.Context(), "kept").Result()
		return err == nil && v == "yes" && primary.Set(h.Context(), "after", "1", 0).Err() == nil
	})
}

// clusterInfoLine is field's value in an INFO reply.
func clusterInfoLine(info, field string) string {
	for _, l := range strings.Split(info, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), field+":"); ok {
			return v
		}
	}
	return ""
}

// eventuallyWithin polls cond every second for up to d.
func eventuallyWithin(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(time.Second) {
		if cond() {
			return
		}
	}
	t.Fatalf("%s: not within %s", what, d)
}
