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
	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"github.com/chromedp/chromedp"
)

// TestBigtableConsoleParity1858 (#1858). An instance's page has Google's
// left-hand menu. On its Tables page, Create table with change stream and
// deletion protection makes the table the admin client reads; on its
// Application profiles page, Create application profile with Low routing
// priority makes the profile the instance admin client reads. Bigtable
// Studio's Query builder reads rows by row-key prefix, a row opens its Row
// details, and the Query editor runs GoogleSQL for Bigtable.
func TestBigtableConsoleParity1858(t *testing.T) {
	needService(t, "bigtable-instances")
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envBigtable)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: every change is read back through the admin API itself", envBigtable)
	}
	t.Setenv("BIGTABLE_EMULATOR_HOST", addr)
	ctx := context.Background()
	project := uniqueProject(t)
	iac, err := bigtable.NewInstanceAdminClient(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = iac.Close() })
	id := fmt.Sprintf("br-par-%d", time.Now().UnixNano()%1e8)
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: id, DisplayName: "Parity",
		ClusterId: id + "-c1", Zone: "us-central1-b", NumNodes: 1, StorageType: bigtable.SSD}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = iac.DeleteInstance(context.Background(), id) })
	admin, err := bigtable.NewAdminClient(ctx, project, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	p := open(t)
	q := "?project=" + project
	p.navigate("/bigtable-instances/" + id + q)
	p.waitFor(`document.querySelector("#tab-studio") !== null`)
	var menu []string
	p.eval(`[...document.querySelectorAll("#view [role=tab]")].map((b) => b.textContent.trim())`, &menu)
	got := strings.Join(menu, ",")
	for _, w := range []string{"Overview", "Tables", "Bigtable Studio", "Clusters", "Application profiles", "Backups",
		"Authorized views", "Logical views", "Materialized views", "Monitoring", "Key Visualizer", "System insights"} {
		if !strings.Contains(got, w) {
			t.Errorf("the instance's menu %q has no %s", got, w)
		}
	}

	// Create table.
	p.run(chromedp.Click(chromedp.CSS(`#tab-tables`)))
	p.clickText("#view #section-actions-tables button", "Create table")
	p.waitFor(`document.querySelector(".modal.is-open #f-table") !== null`)
	setField(p, ".modal.is-open", "f-table", "orders")
	setField(p, ".modal.is-open", "f-changeStream", "true")
	setField(p, ".modal.is-open", "f-retention", "3")
	setField(p, ".modal.is-open", "f-protected", "true")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	info, err := admin.TableInfo(ctx, "orders")
	if err != nil || len(info.FamilyInfos) != 1 || info.FamilyInfos[0].Name != "cf1" || info.DeletionProtection != bigtable.Protected {
		t.Fatalf("after Create table, TableInfo = %+v, %v", info, err)
	}
	if d, _ := info.ChangeStreamRetention.(time.Duration); d != 72*time.Hour {
		t.Errorf("after Create table, the change stream retention is %v, want 72h", info.ChangeStreamRetention)
	}
	t.Cleanup(func() {
		_ = admin.UpdateTableWithDeletionProtection(context.Background(), "orders", bigtable.Unprotected)
	})

	// Create application profile.
	p.navigate("/bigtable-instances/" + id + q)
	p.waitFor(`document.querySelector("#tab-appprofiles") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#tab-appprofiles`)))
	p.clickText("#view #section-actions-appprofiles button", "Create application profile")
	p.waitFor(`document.querySelector(".modal.is-open #f-profile") !== null`)
	setField(p, ".modal.is-open", "f-profile", "reports")
	setField(p, ".modal.is-open", "f-description", "Reports")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open #f-priority-2`)))
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	ap, err := iac.GetAppProfile(ctx, id, "reports")
	if err != nil || ap.GetDescription() != "Reports" || ap.GetStandardIsolation().GetPriority() != btapb.AppProfile_PRIORITY_LOW {
		t.Errorf("after Create application profile, GetAppProfile = %v, %v", ap, err)
	}

	// Bigtable Studio.
	client, err := bigtable.NewClient(ctx, project, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	for _, k := range []string{"a1", "a2", "b1"} {
		bigtableWrite(t, client, "orders", k, map[string][]byte{"cf1:v": []byte("val-" + k)})
	}
	p.navigate("/bigtable-instances/" + id + q)
	p.waitFor(`document.querySelector("#tab-studio") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#tab-studio`)))
	p.waitFor(`[...document.querySelectorAll("#bigtable-builder-table option")].some((o) => o.value === "orders")`)
	p.eval(`(() => { const m = document.querySelector("#bigtable-builder-keymode"); m.value = "prefix";
		m.dispatchEvent(new Event("change")); return true; })()`, nil)
	setField(p, "#view", "bigtable-builder-prefix", "a")
	p.run(chromedp.Click(chromedp.CSS(`#bigtable-builder-run`)))
	p.waitFor(`document.querySelectorAll("#bigtable-results-table tbody tr").length === 2`)
	p.run(chromedp.Click(chromedp.CSS(`#bigtable-results-table tbody tr`)))
	p.waitFor(`document.querySelector("#bigtable-row-key")?.textContent === "a1"`)

	p.run(chromedp.Click(chromedp.CSS(`#bigtable-mode-editor`)))
	p.waitFor(`document.querySelector("#bigtable-editor") !== null`)
	setField(p, "#view", "bigtable-editor", "SELECT _key, CAST(cf1['v'] AS STRING) AS v FROM orders WHERE _key = 'b1'")
	p.run(chromedp.Click(chromedp.CSS(`#bigtable-editor-run`)))
	p.waitFor(`[...document.querySelectorAll("#bigtable-results-table td")].some((c) => c.textContent === "val-b1")`)
}
