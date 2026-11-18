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
	btadmin "cloud.google.com/go/bigtable/admin/apiv2"
	"cloud.google.com/go/iam/apiv1/iampb"
	"github.com/chromedp/chromedp"
	logging "google.golang.org/api/logging/v2"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// The IAM Conditions of #1748 on the Permissions tabs that read version 1
// before: a Bigtable table's page and instance's, and a log view's page.
// Each tab grants a role under a condition and edits it, and each change
// is read back at version 3 with Google's own client.

// grantAndEditCondition is Grant access with a condition on the open
// page's Permissions tab, then Edit condition on the principal's row; read
// returns the stored condition as "title|description|expression".
func grantAndEditCondition(t *testing.T, p *tab, role, postPath string, read func() string) {
	t.Helper()
	ada := "user:ada@example.com"
	first := `request.time < timestamp("2099-01-01T00:00:00Z")`
	second := `request.time.getHours("Europe/Berlin") >= 9`
	p.waitFor(`[...document.querySelectorAll("#view .tab-strip .tab")].some((t) => t.textContent === "Permissions")`)
	p.clickText("#view .tab-strip .tab", "Permissions")
	p.waitFor(`document.querySelector("#grant-access") !== null`)
	p.clickText("#view #grant-access", "Grant access")
	p.waitFor(`document.querySelector(".modal.is-open #f-conditionExpression") !== null`)
	p.eval(fmt.Sprintf(`(() => { const set = (s, v) => { const e = document.querySelector(".modal.is-open " + s);
		e.value = v; e.dispatchEvent(new Event("input", { bubbles: true })); };
		set("#f-members", %q); set("#f-role", %q); set("#f-conditionTitle", "Until 2099");
		set("#f-conditionDescription", "expires"); set("#f-conditionExpression", %q); return true; })()`, ada, role, first), nil)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	p.waitFor(`(document.querySelector("#permissions-table .permissions-condition") || {}).textContent?.includes("Until 2099")`)
	if got, want := read(), "Until 2099|expires|"+first; got != want {
		t.Errorf("after Grant access with a condition the API reads %q; want %q", got, want)
	}

	p.run(chromedp.Click(chromedp.CSS(fmt.Sprintf(`button[aria-label=%q]`, "Edit the condition of "+ada+"'s "+role))))
	p.waitFor(`document.querySelector(".modal.is-open #f-conditionExpression") !== null`)
	p.eval(fmt.Sprintf(`(() => { const set = (s, v) => { const e = document.querySelector(".modal.is-open " + s);
		e.value = v; e.dispatchEvent(new Event("input", { bubbles: true })); };
		set("#f-conditionTitle", "Office hours"); set("#f-conditionExpression", %q); return true; })()`, second), nil)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	p.waitFor(`(document.querySelector("#permissions-table .permissions-condition") || {}).textContent?.includes("Office hours")`)
	if got, want := read(), "Office hours|expires|"+second; got != want {
		t.Errorf("after Edit condition the API reads %q; want %q", got, want)
	}
	if sent := p.posts(postPath); len(sent) != 2 {
		t.Errorf("the grant and the edit sent %d requests to %s, want 2: %v", len(sent), postPath, sent)
	}
}

// firstCondition is a version 3 policy's first condition, as
// grantAndEditCondition compares it.
func firstCondition(pol *iampb.Policy) string {
	for _, b := range pol.GetBindings() {
		if c := b.GetCondition(); c != nil {
			return c.GetTitle() + "|" + c.GetDescription() + "|" + c.GetExpression()
		}
	}
	return ""
}

// bigtableGAPICOptions dial the Bigtable front, as the generated admin
// clients the console calls.
func bigtableGAPICOptions(t *testing.T) []option.ClientOption {
	t.Helper()
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envBigtable)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: every change is read back through the admin API itself", envBigtable)
	}
	return []option.ClientOption{option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
}

// TestBigtableTableIAMConditions (#1748): a table's Permissions tab grants
// and edits a conditional binding, which BigtableTableAdmin.GetIamPolicy
// reads back at version 3.
func TestBigtableTableIAMConditions(t *testing.T) {
	needService(t, "bigtable")
	opts := bigtableGAPICOptions(t)
	project := uniqueProject(t)
	_, _, table := bigtableClients(t, project)
	ctx := context.Background()
	c, err := btadmin.NewBigtableTableAdminClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	name := "projects/" + project + "/instances/" + bigtableInstanceID + "/tables/" + table
	p := open(t)
	p.navigate("/bigtable/" + table + "?project=" + project)
	grantAndEditCondition(t, p, "roles/bigtable.reader", "/api/permissions/bigtable", func() string {
		pol, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name, Options: &iampb.GetPolicyOptions{RequestedPolicyVersion: 3}})
		if err != nil {
			t.Fatalf("GetIamPolicy of %s: %v", name, err)
		}
		return firstCondition(pol)
	})
}

// TestBigtableInstanceIAMConditions (#1748): an instance's Permissions tab
// grants and edits a conditional binding, which
// BigtableInstanceAdmin.GetIamPolicy reads back at version 3.
func TestBigtableInstanceIAMConditions(t *testing.T) {
	needService(t, "bigtable")
	opts := bigtableGAPICOptions(t)
	project := uniqueProject(t)
	ctx := context.Background()
	t.Setenv("BIGTABLE_EMULATOR_HOST", strings.TrimPrefix(strings.TrimSpace(os.Getenv(envBigtable)), "http://"))
	iac, err := bigtable.NewInstanceAdminClient(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = iac.Close() })
	id := fmt.Sprintf("br-cond-%d", time.Now().UnixNano()%1e8)
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: id, DisplayName: "Conditions",
		ClusterId: id + "-c1", Zone: "us-central1-b", NumNodes: 1}); err != nil {
		t.Fatalf("CreateInstance with the official client: %v", err)
	}
	t.Cleanup(func() { _ = iac.DeleteInstance(context.Background(), id) })
	c, err := btadmin.NewBigtableInstanceAdminClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	name := "projects/" + project + "/instances/" + id
	p := open(t)
	p.navigate("/bigtable-instances/" + id + "?project=" + project)
	grantAndEditCondition(t, p, "roles/bigtable.user", "/api/permissions/bigtable-instances", func() string {
		pol, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name, Options: &iampb.GetPolicyOptions{RequestedPolicyVersion: 3}})
		if err != nil {
			t.Fatalf("GetIamPolicy of %s: %v", name, err)
		}
		return firstCondition(pol)
	})
}

// TestLoggingViewIAMConditions (#1748): _AllLogs' page, which its row
// opens, has a Permissions tab that grants and edits a conditional binding,
// which Google's generated Logging v2 client reads back with
// views.getIamPolicy at version 3; the row's text form then keeps the
// conditional binding when it saves the
// others.
func TestLoggingViewIAMConditions(t *testing.T) {
	needService(t, "logging-buckets")
	logAddr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envLogging)), "http://")
	if logAddr == "" {
		t.Skipf("%s is not set: the policy is read back through the Logging API", envLogging)
	}
	svc, err := logging.NewService(context.Background(), option.WithEndpoint("http://"+logAddr+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	project := uniqueProject(t)
	bucket := "projects/" + project + "/locations/global/buckets/_Default"
	view := bucket + "/views/_AllLogs"
	read := func() *logging.Policy {
		pol, err := svc.Projects.Locations.Buckets.Views.GetIamPolicy(view, &logging.GetIamPolicyRequest{
			Options: &logging.GetPolicyOptions{RequestedPolicyVersion: 3}}).Do()
		if err != nil {
			t.Fatalf("views.getIamPolicy: %v", err)
		}
		return pol
	}
	p := open(t)
	openBucket := func() {
		p.navigate("/logs/storage?project=" + project)
		p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody a")].some((a) => a.textContent === %q)`, bucket))
		p.clickText("#view tbody a", bucket)
		p.waitFor(`document.querySelector("#tab-views") !== null`)
		p.run(chromedp.Click(chromedp.CSS(`#tab-views`)))
		p.waitFor(`document.querySelector('button[aria-label="Actions for _AllLogs"]') !== null`)
	}
	openBucket()
	p.waitFor(`[...document.querySelectorAll("#view tbody a")].some((a) => a.textContent === "_AllLogs")`)
	p.clickText("#view tbody a", "_AllLogs")
	grantAndEditCondition(t, p, "roles/logging.viewAccessor", "/api/permissions/logging-buckets", func() string {
		for _, b := range read().Bindings {
			if c := b.Condition; c != nil {
				return c.Title + "|" + c.Description + "|" + c.Expression
			}
		}
		return ""
	})

	// The row's text form edits the other bindings and keeps this one.
	openBucket()
	p.run(chromedp.Click(chromedp.CSS(`button[aria-label="Actions for _AllLogs"]`)))
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Permissions")
	p.waitFor(`document.querySelector(".modal.is-open #f-bindings") !== null`)
	var prefilled string
	p.eval(`document.querySelector(".modal.is-open #f-bindings").value`, &prefilled)
	if prefilled != "" {
		t.Errorf("the text form opens with the conditional binding: %q", prefilled)
	}
	p.eval(`(() => { const f = document.querySelector(".modal.is-open #f-bindings");
		f.value = "roles/logging.viewAccessor group:g@example.com"; f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, nil)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	pol := read()
	if pol.Version != 3 || len(pol.Bindings) != 2 {
		t.Fatalf("after the text form the view's policy reads version %d, %d bindings; want 3, 2", pol.Version, len(pol.Bindings))
	}
	kept := false
	for _, b := range pol.Bindings {
		kept = kept || (b.Condition != nil && b.Condition.Title == "Office hours" && strings.Join(b.Members, ",") == "user:ada@example.com")
	}
	if !kept {
		t.Errorf("the text form dropped the conditional binding: %+v", pol.Bindings)
	}
}
