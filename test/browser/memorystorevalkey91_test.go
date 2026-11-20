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
)

// TestMemorystoreValkeyChooseVersion91 (#2019), in the memorystore shard.
// Create a Valkey instance's Choose version offers Valkey 7.2, 8.0, 9.0
// and 9.1, every one choosable, with Valkey 9.1 chosen, as Google's page
// ("The default version is version 9.1"). Choosing Valkey 8.0 and then
// Valkey 9.1 again moves the choice. The instance created with it is
// VALKEY_9_1, as GetInstance reads; its page shows Valkey 9.1, and Edit's
// Engine version holds Valkey 9.1.
func TestMemorystoreValkeyChooseVersion91(t *testing.T) {
	needService(t, "memorystore-valkey")
	addr := strings.TrimSpace(os.Getenv(envMemorystoreAdmin))
	project := strings.TrimSpace(os.Getenv(envMemorystoreProject))
	if addr == "" || project == "" {
		t.Skipf("%s and %s are not set: the instance is read back through the admin API itself", envMemorystoreAdmin, envMemorystoreProject)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	c, err := memorystore.NewClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	id := fmt.Sprintf("br-v91-%d", time.Now().UnixNano()%1e7)
	name := "projects/" + project + "/locations/us-central1/instances/" + id
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(context.Background(), &memorystorepb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = op.Wait(context.Background())
		}
	})

	p := open(t)
	q := "?project=" + project
	p.navigate("/memorystore-valkey" + q)
	p.waitFor(`[...document.querySelectorAll("#view button")].some((b) => b.textContent === "Create instance")`)
	p.clickText("#view button", "Create instance")
	p.run(chromedp.WaitVisible(chromedp.CSS(`form.create-page #f-id`)))
	versions := func() string {
		var s string
		p.eval(`[...document.querySelectorAll("form.create-page [name='f-engineVersion-choice']")].map((r) =>
			r.value + (r.checked ? "*" : "") + (r.disabled ? ":off" : "")).join(",") + "=" + document.querySelector("form.create-page #f-engineVersion").value`, &s)
		return s
	}
	if got := versions(); got != "VALKEY_7_2,VALKEY_8_0,VALKEY_9_0,VALKEY_9_1*=VALKEY_9_1" {
		t.Errorf("Choose version = %q; want 7.2, 8.0, 9.0 and 9.1, all available, 9.1 chosen", got)
	}
	var labels string
	p.eval(`[...document.querySelectorAll("form.create-page [name='f-engineVersion-choice']")].map((r) => document.querySelector("form.create-page label[for='" + r.id + "']").textContent.trim()).join(",")`, &labels)
	if labels != "Valkey 7.2,Valkey 8.0,Valkey 9.0,Valkey 9.1" {
		t.Errorf("Choose version reads %q; want Google's names", labels)
	}
	p.clickText("form.create-page label", "Valkey 8.0")
	if got := versions(); got != "VALKEY_7_2,VALKEY_8_0*,VALKEY_9_0,VALKEY_9_1=VALKEY_8_0" {
		t.Errorf("after choosing Valkey 8.0, Choose version = %q", got)
	}
	p.clickText("form.create-page label", "Valkey 9.1")
	if got := versions(); got != "VALKEY_7_2,VALKEY_8_0,VALKEY_9_0,VALKEY_9_1*=VALKEY_9_1" {
		t.Errorf("after choosing Valkey 9.1, Choose version = %q", got)
	}
	for f, v := range map[string]string{"id": id, "mode": "CLUSTER_DISABLED", "shardCount": "1", "replicaCount": "0",
		"nodeType": "SHARED_CORE_NANO", "network": "projects/" + project + "/global/networks/default"} {
		p.setField("form.create-page #f-"+f, v)
	}
	p.run(chromedp.Click(chromedp.CSS(`form.create-page button[type="submit"]`)))
	p.waitFor(fmt.Sprintf(`document.querySelector("#view").textContent.includes(%q) && document.querySelector("form.create-page") === null`, id))
	inst, err := c.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: name})
	if err != nil || inst.GetEngineVersion() != "VALKEY_9_1" {
		t.Fatalf("after Create instance, GetInstance = %q, %v; want VALKEY_9_1", inst.GetEngineVersion(), err)
	}

	p.navigate("/memorystore-valkey" + q)
	p.clickText("#view tbody a", id)
	p.waitFor(`[...document.querySelectorAll("#view .page-actions a, #view .page-actions button")].some((b) => b.textContent.trim() === "Edit")`)
	if page := memorystoreTabsText(p); !strings.Contains(page, "Valkey 9.1") {
		t.Errorf("the instance's page does not show Valkey 9.1")
	}
	p.clickText("#view .page-actions a, #view .page-actions button", "Edit")
	p.waitFor(`document.querySelector("#f-engineVersion") !== null`)
	var edit string
	p.eval(`document.querySelector("#f-engineVersion").value`, &edit)
	if edit != "VALKEY_9_1" {
		t.Errorf("Edit's Engine version holds %q, want VALKEY_9_1", edit)
	}
}
