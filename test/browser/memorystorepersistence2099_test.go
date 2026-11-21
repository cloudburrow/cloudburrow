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
	"github.com/chromedp/chromedp"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

// memorystoreDialog opens the page action label, chooses each radio's
// option by index and sets each other field, submits it and waits for it
// to close, failing on a refusal.
func memorystoreDialog(p *tab, label string, radios map[string]int, values map[string]string) {
	p.t.Helper()
	p.waitFor(`document.querySelector(".modal") === null`)
	where := p.findAction(label)
	if where == "" {
		p.t.Fatalf("the page offers no %s", label)
	}
	p.clickText(where, label)
	p.waitFor(`document.querySelector(".modal.is-open form") !== null`)
	for name, i := range radios {
		p.run(chromedp.Click(chromedp.CSS(fmt.Sprintf(".modal.is-open #f-%s-%d", name, i))))
	}
	for id, v := range values {
		p.eval(fmt.Sprintf(`(() => { const f = document.querySelector(".modal.is-open #f-%s");
			f.value = %q; f.dispatchEvent(new Event("input", { bubbles: true })); f.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, id, v), nil)
	}
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitForWithin(`document.querySelector(".modal.is-open") === null ||
		[...document.querySelectorAll(".modal.is-open .form-error")].some((e) => !e.hidden && e.textContent)`, 3*time.Minute)
	var refusal string
	p.eval(`[...document.querySelectorAll(".modal.is-open .form-error")].filter((e) => !e.hidden).map((e) => e.textContent).join(" ")`, &refusal)
	if refusal != "" {
		p.t.Fatalf("%s was refused: %s", label, refusal)
	}
}

// TestMemorystorePersistenceAndMaintenanceWindowThroughThePage (#2099), in
// the memorystore shard. Since #1911 a cluster or instance without
// persistence keeps no data across a restart of its nodes, and since #1909
// its maintenance window is where a release schedules maintenance, so both
// are set on the page, as Google's console does. On a Valkey instance's
// page, Edit persistence ("In the Configurations section, click the pencil
// icon next to Persistence", https://docs.cloud.google.com/memorystore/docs/valkey/manage-persistence)
// sets Memory Snapshot every hour, then Append Only File (AOF), then No
// persistence, each read back through GetInstance; on a Redis cluster's
// page Redis Database (RDB) every 6 hours, then No persistence. Edit
// maintenance window ("Select Custom. From the Day menu ... From the Time
// menu ... Click Update cluster",
// https://docs.cloud.google.com/memorystore/docs/cluster/find-and-set-maintenance-windows)
// sets a Tuesday 05:00 window and Default removes it. Both dialogs pass the
// WCAG 2.1 AA audit.
func TestMemorystorePersistenceAndMaintenanceWindowThroughThePage(t *testing.T) {
	needService(t, "memorystore")
	addr := strings.TrimSpace(os.Getenv(envMemorystoreAdmin))
	project := strings.TrimSpace(os.Getenv(envMemorystoreProject))
	if addr == "" || project == "" {
		t.Skipf("%s and %s are not set: the settings are read back through the admin API itself", envMemorystoreAdmin, envMemorystoreProject)
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
	cid, vid := fmt.Sprintf("browser-pc-%d", n), fmt.Sprintf("browser-pv-%d", n)
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
	valkey := func(what string, ok func(*memorystorepb.Instance) bool) {
		t.Helper()
		var i *memorystorepb.Instance
		for end := time.Now().Add(3 * time.Minute); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
			if i, err = vc.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: vname}); err == nil && ok(i) {
				return
			}
		}
		t.Fatalf("Valkey instance %s: %v, %v", what, i.GetPersistenceConfig(), err)
	}
	redisCluster := func(what string, ok func(*clusterpb.Cluster) bool) {
		t.Helper()
		var c *clusterpb.Cluster
		for end := time.Now().Add(3 * time.Minute); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
			if c, err = cc.GetCluster(ctx, &clusterpb.GetClusterRequest{Name: cname}); err == nil && ok(c) {
				return
			}
		}
		t.Fatalf("cluster %s: %v %v, %v", what, c.GetPersistenceConfig(), c.GetMaintenancePolicy(), err)
	}
	p := open(t)
	vpage := "/memorystore-valkey/" + vid + "?project=" + project
	cpage := "/memorystore-clusters/" + cid + "?project=" + project

	// Persistence's options: DISABLED, AOF, RDB.
	p.navigate(vpage)
	memorystoreDialog(p, "Edit persistence", map[string]int{"persistence": 2}, map[string]string{"snapshotInterval": "ONE_HOUR"})
	valkey("with Memory Snapshot every hour", func(i *memorystorepb.Instance) bool {
		pc := i.GetPersistenceConfig()
		return pc.GetMode() == memorystorepb.PersistenceConfig_RDB && pc.GetRdbConfig().GetRdbSnapshotPeriod() == memorystorepb.PersistenceConfig_RDBConfig_ONE_HOUR
	})
	// The Configuration tab says it, and holds Edit persistence, as
	// Google's Configurations section has the pencil next to Persistence.
	p.navigate(vpage + "&tab=configuration")
	p.waitFor(`document.querySelector("#view").textContent.includes("Memory Snapshot") &&
		[...document.querySelectorAll("#view .section-actions button")].some((b) => b.textContent.trim() === "Edit persistence") &&
		![...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent.trim() === "Edit persistence")`)
	memorystoreDialog(p, "Edit persistence", map[string]int{"persistence": 1}, map[string]string{"appendFsync": "ALWAYS"})
	valkey("with AOF, fsync always", func(i *memorystorepb.Instance) bool {
		pc := i.GetPersistenceConfig()
		return pc.GetMode() == memorystorepb.PersistenceConfig_AOF && pc.GetAofConfig().GetAppendFsync() == memorystorepb.PersistenceConfig_AOFConfig_ALWAYS
	})
	p.navigate(vpage)
	memorystoreDialog(p, "Edit persistence", map[string]int{"persistence": 0}, nil)
	valkey("without persistence", func(i *memorystorepb.Instance) bool {
		return i.GetPersistenceConfig().GetMode() == memorystorepb.PersistenceConfig_DISABLED
	})
	p.navigate(vpage + "&tab=configuration")
	p.waitFor(`document.querySelector("#view").textContent.includes("No persistence")`)

	p.navigate(cpage)
	memorystoreDialog(p, "Edit persistence", map[string]int{"persistence": 2}, map[string]string{"snapshotInterval": "SIX_HOURS"})
	redisCluster("with RDB every 6 hours", func(c *clusterpb.Cluster) bool {
		pc := c.GetPersistenceConfig()
		return pc.GetMode() == clusterpb.ClusterPersistenceConfig_RDB &&
			pc.GetRdbConfig().GetRdbSnapshotPeriod() == clusterpb.ClusterPersistenceConfig_RDBConfig_SIX_HOURS
	})
	p.navigate(cpage)
	memorystoreDialog(p, "Edit persistence", map[string]int{"persistence": 0}, nil)
	redisCluster("without persistence", func(c *clusterpb.Cluster) bool {
		return c.GetPersistenceConfig().GetMode() == clusterpb.ClusterPersistenceConfig_DISABLED
	})

	// Edit maintenance window: Default (0) or Custom (1).
	p.navigate(cpage)
	memorystoreDialog(p, "Edit maintenance window", map[string]int{"maintenance": 1}, map[string]string{"maintenanceDay": "TUESDAY", "maintenanceHour": "5"})
	redisCluster("with a Tuesday 05:00 window", func(c *clusterpb.Cluster) bool {
		ws := c.GetMaintenancePolicy().GetWeeklyMaintenanceWindow()
		return len(ws) == 1 && ws[0].GetDay().String() == "TUESDAY" && ws[0].GetStartTime().GetHours() == 5
	})
	p.navigate(cpage + "&tab=maintenance")
	p.waitFor(`document.querySelector("#view").textContent.includes("TUESDAY, 05:00") &&
		[...document.querySelectorAll("#view .section-actions button")].some((b) => b.textContent.trim() === "Edit maintenance window")`)

	open := func(label string) string {
		return `(() => { for (const t of document.querySelectorAll("#view [role=tab]")) { t.click();
			const b = [...document.querySelectorAll("#view .page-actions button, #view .section-actions button")].find((b) => b.textContent.trim() === "` + label + `" && b.getClientRects().length);
			if (b) { b.click(); return; } } })()`
	}
	auditScreens(t, []a11yPage{
		{"Edit persistence dialog", vpage, `document.querySelector("#view [role=tab]") !== null`, open("Edit persistence")},
		{"Edit maintenance window dialog", cpage, `document.querySelector("#view [role=tab]") !== null`, open("Edit maintenance window")},
	})

	p.navigate(cpage)
	memorystoreDialog(p, "Edit maintenance window", map[string]int{"maintenance": 0}, nil)
	redisCluster("with the window removed", func(c *clusterpb.Cluster) bool {
		return len(c.GetMaintenancePolicy().GetWeeklyMaintenanceWindow()) == 0
	})
}
