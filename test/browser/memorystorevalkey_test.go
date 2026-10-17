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
	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

// TestMemorystoreValkeySecondaryThroughThePage (#1557), in the emulators
// shard. A CLUSTER_DISABLED primary is made with the official client; the
// Memorystore for Valkey page's Create instance, with Secondary of naming
// it and Engine version VALKEY_9_0, makes a secondary GetInstance reads
// with that role and version. The secondary's page shows its role, its
// primary, the replication membership and the engine version; Detach,
// confirmed by the instance's name, leaves the replication, as GetInstance
// then reads.
func TestMemorystoreValkeySecondaryThroughThePage(t *testing.T) {
	needService(t, "memorystore-valkey")
	addr := strings.TrimSpace(os.Getenv(envMemorystoreAdmin))
	project := strings.TrimSpace(os.Getenv(envMemorystoreProject))
	if addr == "" || project == "" {
		t.Skipf("%s and %s are not set: every change is read back through the admin API itself", envMemorystoreAdmin, envMemorystoreProject)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	c, err := memorystore.NewClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	sfx := time.Now().UnixNano() % 1e7
	parent := "projects/" + project + "/locations/us-central1"
	pid, sid := fmt.Sprintf("br-vkp-%d", sfx), fmt.Sprintf("br-vks-%d", sfx)
	network := "projects/" + project + "/global/networks/default"
	for _, id := range []string{sid, pid} {
		name := parent + "/instances/" + id
		t.Cleanup(func() {
			if op, err := c.DeleteInstance(context.Background(), &memorystorepb.DeleteInstanceRequest{Name: name}); err == nil {
				_ = op.Wait(context.Background())
			}
		})
	}
	op, err := c.CreateInstance(ctx, &memorystorepb.CreateInstanceRequest{Parent: parent, InstanceId: pid, Instance: &memorystorepb.Instance{
		Mode: memorystorepb.Instance_CLUSTER_DISABLED, ReplicaCount: proto.Int32(0), NodeType: memorystorepb.Instance_SHARED_CORE_NANO,
		PscAutoConnections: []*memorystorepb.PscAutoConnection{{ProjectId: project, Network: network}}}})
	if err != nil {
		t.Fatal(err)
	}
	primary, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateInstance of the primary: %v", err)
	}

	p := open(t)
	q := "?project=" + project
	p.navigate("/memorystore-valkey" + q)
	p.waitFor(`[...document.querySelectorAll("#view button")].some((b) => b.textContent === "Create instance")`)
	p.clickText("#view button", "Create instance")
	p.run(chromedp.WaitVisible(`form.create-page #f-id`, chromedp.ByQuery))
	for f, v := range map[string]string{"id": sid, "mode": "CLUSTER_DISABLED", "shardCount": "1", "replicaCount": "0",
		"nodeType": "SHARED_CORE_NANO", "network": network, "engineVersion": "VALKEY_9_0", "primaryInstance": primary.GetName()} {
		p.setField("form.create-page #f-"+f, v)
	}
	p.run(chromedp.Click(`form.create-page button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(fmt.Sprintf(`document.querySelector("#view").textContent.includes(%q) && document.querySelector("form.create-page") === null`, sid))
	sname := parent + "/instances/" + sid
	sec, err := c.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: sname})
	if err != nil || sec.GetEngineVersion() != "VALKEY_9_0" ||
		sec.GetCrossInstanceReplicationConfig().GetInstanceRole() != memorystorepb.CrossInstanceReplicationConfig_SECONDARY ||
		sec.GetCrossInstanceReplicationConfig().GetPrimaryInstance().GetInstance() != primary.GetName() {
		t.Fatalf("after Create instance, GetInstance = %v, %v", sec, err)
	}

	p.navigate("/memorystore-valkey" + q)
	p.clickText("#view tbody a", sid)
	p.waitFor(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent === "Detach")`)
	var page string
	p.eval(`document.querySelector("#view").textContent`, &page)
	for _, want := range []string{"Cross-instance replication", "SECONDARY", primary.GetName(), "Replication membership", "VALKEY_9_0", "Customer-managed key"} {
		if !strings.Contains(page, want) {
			t.Errorf("the secondary's page does not show %q", want)
		}
	}
	p.clickText("#view .page-actions button", "Detach")
	p.waitFor(`document.activeElement === document.querySelector(".modal #confirm-input")`)
	var title string
	p.eval(`document.querySelector(".modal #confirm-title").textContent`, &title)
	word := strings.TrimSuffix(strings.TrimPrefix(title, "Detach "), "?")
	p.confirmByName(word)
	p.waitFor(`document.querySelector(".modal") === null`)
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(500 * time.Millisecond) {
		got, err := c.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: sname})
		if err == nil && got.GetCrossInstanceReplicationConfig() == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after Detach, GetInstance = %v, %v; want no cross_instance_replication_config", got.GetCrossInstanceReplicationConfig(), err)
		}
	}
}
