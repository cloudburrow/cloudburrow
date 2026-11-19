//go:build browser

package browser

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	"github.com/chromedp/chromedp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestBigtableInstanceAdminThroughTheBrowser (#1358). On the Bigtable
// instances screen of a new project, Create instance (Google's steps)
// with a name, an ID and a first cluster of two nodes makes the
// instance and cluster the official InstanceAdminClient reads. On the
// instance's page, Edit cluster from the cluster's row menu on the Clusters
// page sets three nodes, as GetCluster reads; Delete instance, confirmed by
// its ID, removes it, and InstanceInfo is NOT_FOUND.
func TestBigtableInstanceAdminThroughTheBrowser(t *testing.T) {
	needService(t, "bigtable-instances")
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envBigtable)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: every change is read back through the instance admin API itself", envBigtable)
	}
	t.Setenv("BIGTABLE_EMULATOR_HOST", addr)
	ctx := context.Background()
	project := uniqueProject(t)
	iac, err := bigtable.NewInstanceAdminClient(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = iac.Close() })
	id := fmt.Sprintf("br-inst-%d", time.Now().UnixNano()%1e8)
	cluster := id + "-c1"
	t.Cleanup(func() { _ = iac.DeleteInstance(context.Background(), id) })

	p := open(t)
	p.navigate("/bigtable-instances?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view .state button.primary")].some((b) => b.textContent === "Create instance")`)
	p.clickText(`#view .state button.primary`, "Create instance")
	p.waitFor(`document.querySelector("form.create-page #f-displayName") !== null`)
	// Google's steps (#1858), each with Continue.
	var steps []string
	p.eval(`[...document.querySelectorAll(".create-step-title")].map((s) => s.textContent)`, &steps)
	want := []string{"Name your instance", "Select your edition", "Select your storage type", "Configure your first cluster"}
	if len(steps) < len(want) {
		t.Fatalf("the Create instance page's steps are %v; want %v first", steps, want)
	}
	for i, w := range want {
		if !strings.Contains(steps[i], w) {
			t.Errorf("step %d is %q; want %q", i+1, steps[i], w)
		}
	}
	setField(p, "form.create-page", "f-displayName", "Browser made")
	setField(p, "form.create-page", "f-instance", id)
	setField(p, "form.create-page", "f-cluster", cluster)
	setField(p, "form.create-page", "f-nodes", "2")
	p.run(chromedp.Click(chromedp.CSS(`form.create-page button[type="submit"].primary`)))
	p.waitFor(fmt.Sprintf(`document.querySelector("#view").textContent.includes(%q)`, "Browser made"))
	info, err := iac.InstanceInfo(ctx, id)
	if err != nil || info.DisplayName != "Browser made" {
		t.Fatalf("after Create instance, InstanceInfo = %+v, %v", info, err)
	}
	if cl, err := iac.GetCluster(ctx, id, cluster); err != nil || cl.ServeNodes != 2 || cl.Zone != "us-central1-b" {
		t.Fatalf("after Create instance, GetCluster = %+v, %v", cl, err)
	}

	p.navigate("/bigtable-instances/" + id + "?project=" + project)
	p.waitFor(`document.querySelector("#tab-clusters") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#tab-clusters`)))
	p.cellMenu(cluster, "Edit cluster")
	p.waitFor(`document.querySelector(".modal #f-nodes") !== null`)
	p.setField(".modal #f-nodes", "3")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	if cl, err := iac.GetCluster(ctx, id, cluster); err != nil || cl.ServeNodes != 3 {
		t.Errorf("after Edit cluster, GetCluster = %+v, %v", cl, err)
	}

	p.navigate("/bigtable-instances/" + id + "?project=" + project)
	p.clickText("#view .page-actions button", "Delete instance")
	p.confirmByName(id)
	p.waitFor(`document.querySelector(".modal") === null`)
	if _, err := iac.InstanceInfo(ctx, id); status.Code(err) != codes.NotFound {
		t.Errorf("after Delete instance, InstanceInfo: %v, want NotFound", err)
	}
}
