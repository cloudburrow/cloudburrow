//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	redispb "cloud.google.com/go/redis/apiv1/redispb"
	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"google.golang.org/genproto/googleapis/type/dayofweek"
	"google.golang.org/genproto/googleapis/type/timeofday"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
)

// memorystoreControl POSTs body to CloudBurrow's Memorystore control
// path (not a Memorystore API) and decodes the answer.
func memorystoreControl(t *testing.T, h *Harness, path, body string) map[string]any {
	t.Helper()
	res, err := http.Post("http://"+h.Endpoint(EnvMemorystoreAdmin)+"/cloudburrow/v1/"+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST %s: %d %v", path, res.StatusCode, out)
	}
	return out
}

// covers: google.cloud.redis.v1.CloudRedis/CreateInstance, google.cloud.redis.v1.CloudRedis/GetInstance, google.cloud.redis.cluster.v1.CloudRedisCluster/CreateCluster, google.cloud.memorystore.v1.Memorystore/CreateInstance
//
// TestMemorystoreMaintenanceWindowSchedules (#1909), with cloud.google.com/
// go/redis/apiv1, redis/cluster/apiv1 and memorystore/apiv1: a Redis
// cluster's maintenance policy with a window and no day, and a Valkey
// instance's with two windows, are INVALID_ARGUMENT. A Basic tier Redis
// instance with a Sunday 03:00 window gets, from a release
// (:releaseUpdate, CloudBurrow's stand-in for Google releasing a
// version), maintenance at the first such window at least a week away,
// an hour long, reschedulable, its deadline a week after; advancing the
// Memorystore clock (clock:advance) to that start applies it: the
// instance's maintenance_schedule is gone and it serves again.
func TestMemorystoreMaintenanceWindowSchedules(t *testing.T) {
	h := New(t)
	rc, project := memorystoreAdminClient(t, h)
	cc, _ := redisClusterAdminClient(t, h)
	vc, _ := valkeyAdminClient(t, h)
	ctx := memorystoreWait(t)
	parent := "projects/" + project + "/locations/us-central1"
	network := "projects/" + project + "/global/networks/default"

	_, err := cc.CreateCluster(ctx, &clusterpb.CreateClusterRequest{Parent: parent, ClusterId: "window-refused", Cluster: &clusterpb.Cluster{
		ShardCount: proto.Int32(1), NodeType: clusterpb.NodeType_REDIS_SHARED_CORE_NANO, PscConfigs: []*clusterpb.PscConfig{{Network: network}},
		MaintenancePolicy: &clusterpb.ClusterMaintenancePolicy{WeeklyMaintenanceWindow: []*clusterpb.ClusterWeeklyMaintenanceWindow{
			{StartTime: &timeofday.TimeOfDay{Hours: 3}}}}}})
	wantRedisCode(t, "CreateCluster with a window without a day", err, codes.InvalidArgument)
	_, err = vc.CreateInstance(ctx, &memorystorepb.CreateInstanceRequest{Parent: parent, InstanceId: "window-refused", Instance: &memorystorepb.Instance{
		ShardCount: 1, NodeType: memorystorepb.Instance_SHARED_CORE_NANO,
		Endpoints: []*memorystorepb.Instance_InstanceEndpoint{{Connections: []*memorystorepb.Instance_ConnectionDetail{{
			Connection: &memorystorepb.Instance_ConnectionDetail_PscAutoConnection{PscAutoConnection: &memorystorepb.PscAutoConnection{ProjectId: project, Network: network}}}}}},
		MaintenancePolicy: &memorystorepb.MaintenancePolicy{WeeklyMaintenanceWindow: []*memorystorepb.WeeklyMaintenanceWindow{
			{Day: dayofweek.DayOfWeek_MONDAY, StartTime: &timeofday.TimeOfDay{Hours: 1}}, {Day: dayofweek.DayOfWeek_TUESDAY, StartTime: &timeofday.TimeOfDay{Hours: 1}}}}}})
	wantRedisCode(t, "Valkey CreateInstance with two windows", err, codes.InvalidArgument)

	id := fmt.Sprintf("window-%d", time.Now().UnixNano()%1e7)
	name := parent + "/instances/" + id
	t.Cleanup(func() {
		if op, err := rc.DeleteInstance(context.Background(), &redispb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	op, err := rc.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: parent, InstanceId: id, Instance: &redispb.Instance{
		Tier: redispb.Instance_BASIC, MemorySizeGb: 1,
		MaintenancePolicy: &redispb.MaintenancePolicy{WeeklyMaintenanceWindow: []*redispb.WeeklyMaintenanceWindow{
			{Day: dayofweek.DayOfWeek_SUNDAY, StartTime: &timeofday.TimeOfDay{Hours: 3}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	out := memorystoreControl(t, h, "projects/"+project+"/locations/us-central1:releaseUpdate", "{}")
	if !strings.Contains(fmt.Sprint(out["scheduled"]), name) {
		t.Fatalf("release scheduled %v, not %s", out["scheduled"], name)
	}
	inst, err := rc.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	ms := inst.GetMaintenanceSchedule()
	start := ms.GetStartTime().AsTime().UTC()
	clock := memorystoreControl(t, h, "clock:advance", `{"duration":"0s"}`)
	now, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(clock["now"]))
	if start.Weekday() != time.Sunday || start.Hour() != 3 || start.Minute() != 0 || start.Before(now.Add(7*24*time.Hour)) || start.After(now.Add(14*24*time.Hour)) {
		t.Errorf("maintenance starts %s (now %s); want the first Sunday 03:00 UTC at least a week away", start, now)
	}
	if !ms.GetEndTime().AsTime().Equal(start.Add(time.Hour)) || !ms.GetCanReschedule() || !ms.GetScheduleDeadlineTime().AsTime().Equal(start.Add(7*24*time.Hour)) {
		t.Errorf("maintenance_schedule = %v", ms)
	}

	memorystoreControl(t, h, "clock:advance", fmt.Sprintf(`{"duration":"%ds"}`, int64(start.Sub(now)/time.Second)+1))
	deadline := time.Now().Add(3 * time.Minute)
	for {
		inst, err = rc.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
		if err == nil && inst.GetState() == redispb.Instance_READY && inst.GetMaintenanceSchedule() == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("at the window: %v, %v", inst, err)
		}
		time.Sleep(time.Second)
	}
	c := redisAt(inst, "")
	defer c.Close()
	var perr error
	for end := time.Now().Add(time.Minute); time.Now().Before(end); time.Sleep(500 * time.Millisecond) {
		if perr = c.Ping(ctx).Err(); perr == nil {
			break
		}
	}
	if perr != nil {
		t.Errorf("PING after maintenance: %v", perr)
	}
}
