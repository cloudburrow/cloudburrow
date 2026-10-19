//go:build browser

package browser

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
	logging "google.golang.org/api/logging/v2"
	"google.golang.org/api/option"
)

// TestLoggingViewPermissionsThroughTheBrowser (#1519), in the storage
// shard, which serves Cloud Logging: on Log storage, _Default's _AllLogs
// row has Permissions; role bindings saved there are the policy Google's
// generated Logging v2 client reads with views.getIamPolicy, and the form
// opens filled with them.
func TestLoggingViewPermissionsThroughTheBrowser(t *testing.T) {
	needService(t, "logging-buckets")
	logAddr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envLogging)), "http://")
	if logAddr == "" {
		t.Skipf("%s is not set: the policy is read back through the Logging API", envLogging)
	}
	svc, err := logging.NewService(context.Background(), option.WithEndpoint("http://"+logAddr+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	p := open(t)
	project := uniqueProject(t)
	bucket := "projects/" + project + "/locations/global/buckets/_Default"
	view := bucket + "/views/_AllLogs"
	openForm := func() {
		p.waitFor(`document.querySelector("#tab-views") !== null`)
		p.run(chromedp.Click(chromedp.CSS(`#tab-views`)))
		p.waitFor(`document.querySelector('button[aria-label="Actions for _AllLogs"]') !== null`)
		p.run(chromedp.Click(chromedp.CSS(`button[aria-label="Actions for _AllLogs"]`)))
		p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Permissions")
		p.waitFor(`document.querySelector(".modal.is-open #f-bindings") !== null`)
	}

	openBucket := func() {
		p.navigate("/logs/storage?project=" + project)
		p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody a")].some((a) => a.textContent === %q)`, bucket))
		p.clickText("#view tbody a", bucket)
	}
	openBucket()
	openForm()
	bindings := "roles/logging.viewAccessor user:a@example.com, group:g@example.com"
	p.eval(fmt.Sprintf(`(() => { const f = document.querySelector(".modal.is-open #f-bindings");
		f.value = %q; f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, bindings), nil)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)

	pol, err := svc.Projects.Locations.Buckets.Views.GetIamPolicy(view, &logging.GetIamPolicyRequest{}).Do()
	if err != nil {
		t.Fatalf("views.getIamPolicy: %v", err)
	}
	if len(pol.Bindings) != 1 || pol.Bindings[0].Role != "roles/logging.viewAccessor" || len(pol.Bindings[0].Members) != 2 {
		t.Errorf("the policy saved in the browser reads %+v", pol.Bindings)
	}
	// Opened again, the bucket's page reads the policy anew.
	openBucket()
	openForm()
	var prefilled string
	p.eval(`document.querySelector(".modal.is-open #f-bindings").value`, &prefilled)
	if prefilled != "roles/logging.viewAccessor group:g@example.com, user:a@example.com" {
		t.Errorf("Permissions opens with %q", prefilled)
	}
}
