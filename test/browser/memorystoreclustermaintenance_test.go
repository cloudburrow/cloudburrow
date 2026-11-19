//go:build browser

package browser

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	memorystore "cloud.google.com/go/memorystore/apiv1"
	"cloud.google.com/go/memorystore/apiv1/memorystorepb"
	cluster "cloud.google.com/go/redis/cluster/apiv1"
	"cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

// TestMemorystoreClusterAndValkeyMaintenanceThroughThePage (#1524), in the
// memorystore shard. On a Redis cluster's page: Set maintenance window
// sets a Sunday 03:00 UTC maintenance_policy; Schedule maintenance
// (CloudBurrow) schedules the first window a week away, which GetCluster's
// maintenance_schedule reads and the summary shows; Reschedule
// maintenance with SPECIFIC_TIME moves it ten days; Cancel maintenance,
// confirmed, removes it. On a Valkey instance's page: Schedule
// maintenance, then Reschedule maintenance IMMEDIATE runs it: the instance
// is ACTIVE again with no maintenance_schedule.
func TestMemorystoreClusterAndValkeyMaintenanceThroughThePage(t *testing.T) {
	needService(t, "memorystore")
	addr := strings.TrimSpace(os.Getenv(envMemorystoreAdmin))
	project := strings.TrimSpace(os.Getenv(envMemorystoreProject))
	if addr == "" || project == "" {
		t.Skipf("%s and %s are not set: the maintenance is read back through the admin API itself", envMemorystoreAdmin, envMemorystoreProject)
	}
	ctx := context.Background()
	opts := []option.ClientOption{option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
	cc, err := cluster.NewCloudRedisClusterClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	vc, err := memorystore.NewClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vc.Close() })
	parent := "projects/" + project + "/locations/us-central1"
	n := time.Now().UnixNano() % 1e8
	cid, vid := fmt.Sprintf("browser-cl-%d", n), fmt.Sprintf("browser-vk-%d", n)
	cname, vname := parent+"/clusters/"+cid, parent+"/instances/"+vid
	t.Cleanup(func() {
		if op, err := cc.DeleteCluster(context.Background(), &clusterpb.DeleteClusterRequest{Name: cname}); err == nil {
			_ = op.Wait(context.Background())
		}
		if op, err := vc.DeleteInstance(context.Background(), &memorystorepb.DeleteInstanceRequest{Name: vname}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	network := "projects/" + project + "/global/networks/default"
	cop, err := cc.CreateCluster(ctx, &clusterpb.CreateClusterRequest{Parent: parent, ClusterId: cid, Cluster: &clusterpb.Cluster{
		ShardCount: proto.Int32(1), ReplicaCount: proto.Int32(0), NodeType: clusterpb.NodeType_REDIS_SHARED_CORE_NANO,
		AuthorizationMode: clusterpb.AuthorizationMode_AUTH_MODE_DISABLED, PscConfigs: []*clusterpb.PscConfig{{Network: network}}}})
	if err != nil {
		t.Fatal(err)
	}
	vop, err := vc.CreateInstance(ctx, &memorystorepb.CreateInstanceRequest{Parent: parent, InstanceId: vid, Instance: &memorystorepb.Instance{
		Mode: memorystorepb.Instance_CLUSTER_DISABLED, ReplicaCount: proto.Int32(0), NodeType: memorystorepb.Instance_SHARED_CORE_NANO,
		PscAutoConnections: []*memorystorepb.PscAutoConnection{{ProjectId: project, Network: network}}}})
	if err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if _, err := cop.Wait(wctx); err != nil {
		t.Fatal(err)
	}
	if _, err := vop.Wait(wctx); err != nil {
		t.Fatal(err)
	}
	getCluster := func(what string, ok func(*clusterpb.Cluster) bool) *clusterpb.Cluster {
		t.Helper()
		var c *clusterpb.Cluster
		for end := time.Now().Add(3 * time.Minute); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
			if c, err = cc.GetCluster(ctx, &clusterpb.GetClusterRequest{Name: cname}); err == nil && ok(c) {
				return c
			}
		}
		t.Fatalf("cluster %s: %v, %v", what, c, err)
		return nil
	}

	p := open(t)
	page := "/memorystore-clusters/" + cid + "?project=" + project
	p.navigate(page)
	p.pageAction("Set maintenance window", map[string]string{"maintenanceDay": "SUNDAY", "maintenanceHour": "3"}, "")
	getCluster("with a Sunday 03:00 window", func(c *clusterpb.Cluster) bool {
		ws := c.GetMaintenancePolicy().GetWeeklyMaintenanceWindow()
		return len(ws) == 1 && ws[0].GetDay().String() == "SUNDAY" && ws[0].GetStartTime().GetHours() == 3
	})
	p.navigate(page)
	p.pageAction("Schedule maintenance (CloudBurrow)", map[string]string{"startTime": ""}, "")
	c := getCluster("with the event scheduled", func(c *clusterpb.Cluster) bool { return c.GetMaintenanceSchedule() != nil })
	orig := c.GetMaintenanceSchedule().GetStartTime().AsTime()
	if orig.Weekday() != time.Sunday || orig.Hour() != 3 {
		t.Errorf("scheduled for %s; want a Sunday 03:00 UTC", orig)
	}
	// The summary is on the Maintenance tab (#1862).
	p.navigate(page + "&tab=maintenance")
	p.waitFor(`document.querySelector("#view").textContent.includes("Scheduled maintenance") &&
		document.querySelector("#view").textContent.includes("SUNDAY, 03:00")`)
	at := orig.Add(10 * 24 * time.Hour)
	p.pageAction("Reschedule maintenance", map[string]string{"rescheduleType": "SPECIFIC_TIME", "scheduleTime": at.Format(time.RFC3339)}, "")
	getCluster("with the event ten days on", func(c *clusterpb.Cluster) bool {
		return c.GetMaintenanceSchedule().GetStartTime().AsTime().Equal(at)
	})
	p.navigate(page)
	p.clickAction("Cancel maintenance (CloudBurrow)", cid)
	getCluster("with the event cancelled", func(c *clusterpb.Cluster) bool { return c.GetMaintenanceSchedule() == nil })

	vpage := "/memorystore-valkey/" + vid + "?project=" + project
	p.navigate(vpage)
	p.pageAction("Schedule maintenance (CloudBurrow)", map[string]string{"startTime": ""}, "")
	for end := time.Now().Add(time.Minute); ; time.Sleep(200 * time.Millisecond) {
		if i, err := vc.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: vname}); err == nil && i.GetMaintenanceSchedule() != nil {
			break
		}
		if time.Now().After(end) {
			t.Fatal("the Valkey instance has no maintenance_schedule after Schedule maintenance")
		}
	}
	p.navigate(vpage)
	p.pageAction("Reschedule maintenance", map[string]string{"rescheduleType": "IMMEDIATE"}, "")
	for end := time.Now().Add(5 * time.Minute); ; time.Sleep(time.Second) {
		i, err := vc.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: vname})
		if err == nil && i.GetState() == memorystorepb.Instance_ACTIVE && i.GetMaintenanceSchedule() == nil {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("after Reschedule maintenance IMMEDIATE: %v, %v", i, err)
		}
	}
}
