//go:build browser

package browser

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	functions "cloud.google.com/go/functions/apiv2"
	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	"cloud.google.com/go/iam/apiv1/iampb"
	runclient "cloud.google.com/go/run/apiv2"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestCloudRunFunctionsRound3ThroughTheBrowser (#1853), in the run shard:
//   - the functions list leads with Environment, then Name, as Google's;
//   - Write a function asks for Service name, Region, Runtime, Trigger and
//     Authentication (Allow public access or Require authentication), then
//     Billing, Service scaling, Ingress and the container's settings, in
//     Google's order;
//   - the Source tab has Show Payload (the example payload: an HTTP
//     function's JSON body) and Test beside Edit source, and the inline
//     editor has Rename file;
//   - where builds are asked for (CLOUDBURROW_TEST_FUNCTIONS) and can run
//     (amd64 nodes, the Google builder's), Write a
//     function's Create with Allow public access lands on the function's
//     Source tab, and the function's Cloud Run service grants allUsers
//     roles/run.invoker, which the official Run client reads.
//
// Then the screens pass the console's WCAG 2.1 AA audit.
func TestCloudRunFunctionsRound3ThroughTheBrowser(t *testing.T) {
	needService(t, "functions")
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envRun)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: the functions are made and read back through the Cloud Functions v2 API", envRun)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	opts := []option.ClientOption{option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
	c, err := functions.NewFunctionClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	project := uniqueProject(t)
	parent := "projects/" + project + "/locations/us-central1"
	suffix := time.Now().UnixNano() % 1e6

	// A function whose build fails at once (its source generation does not
	// exist), so its page is there without running Google's builder.
	var src bytes.Buffer
	zw := zip.NewWriter(&src)
	for file, body := range map[string]string{
		"package.json": `{"name":"hello","version":"1.0.0","main":"index.js"}`,
		"index.js":     `exports.hello = (req, res) => res.send("round three");`,
	} {
		w, _ := zw.Create(file)
		_, _ = w.Write([]byte(body))
	}
	_ = zw.Close()
	up, err := c.GenerateUploadUrl(ctx, &functionspb.GenerateUploadUrlRequest{Parent: parent})
	if err != nil {
		t.Skipf("GenerateUploadUrl: %v: this instance takes no function source", err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, up.GetUploadUrl(), bytes.NewReader(src.Bytes()))
	req.Header.Set("Content-Type", "application/zip")
	hc := &http.Client{Timeout: time.Minute}
	defer hc.CloseIdleConnections()
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("upload the source: %v", err)
	}
	resp.Body.Close()
	ss := up.GetStorageSource()
	ss.Generation = 1 << 50
	id := fmt.Sprintf("browser-r3-%d", suffix)
	name := parent + "/functions/" + id
	if _, err := c.CreateFunction(ctx, &functionspb.CreateFunctionRequest{Parent: parent, FunctionId: id, Function: &functionspb.Function{
		BuildConfig: &functionspb.BuildConfig{Runtime: "nodejs22", EntryPoint: "hello",
			Source: &functionspb.Source{Source: &functionspb.Source_StorageSource{StorageSource: ss}}}}}); err != nil {
		t.Fatalf("CreateFunction: %v", err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteFunction(context.Background(), &functionspb.DeleteFunctionRequest{Name: name}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(time.Second) {
		fn, err := c.GetFunction(ctx, &functionspb.GetFunctionRequest{Name: name})
		if err == nil && fn.GetState() != functionspb.Function_DEPLOYING {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s is still deploying: %v, %v", id, fn, err)
		}
	}

	p := open(t)
	// The list: Environment, then Name.
	list := "/functions?project=" + project
	p.navigate(list)
	p.waitFor(fmt.Sprintf(`document.querySelector("#view table tbody") !== null && document.querySelector("#view table tbody").textContent.includes(%q)`, id))
	var heads string
	p.eval(`[...document.querySelectorAll("#view table thead th")].map((n) => n.textContent.trim()).filter(Boolean).join("|")`, &heads)
	cols := strings.Split(heads, "|")
	if e, n := slices.Index(cols, "Environment"), slices.Index(cols, "Name"); e < 0 || n != e+1 {
		t.Errorf("the functions list's columns = %q; want Environment, then Name", heads)
	}
	var cell string
	p.eval(fmt.Sprintf(`(() => { const r = [...document.querySelectorAll("#view table tbody tr")].find((r) => r.textContent.includes(%q));
		const i = [...document.querySelectorAll("#view table thead th")].findIndex((h) => h.textContent.trim() === "Name");
		return r && i >= 0 ? r.children[i].textContent.trim() : ""; })()`, id), &cell)
	if cell != id {
		t.Errorf("the Name column's cell = %q; want %q", cell, id)
	}

	// Write a function: Google's fields, in Google's order.
	p.clickText("#view button", "Create function")
	p.waitFor(`location.pathname === "/run" && document.querySelector(".modal.is-open .sourcefiles-editor") !== null`)
	var labels string
	p.eval(`[...document.querySelectorAll(".modal.is-open form label, .modal.is-open form legend")].map((n) => n.textContent.trim()).join("|")`, &labels)
	at := func(l string) int { return strings.Index("|"+labels+"|", "|"+l) }
	order := []string{"Service name", "Region", "Runtime", "Trigger type", "Authentication", "Billing", "Minimum number of instances",
		"Maximum number of instances", "Ingress"}
	for i, l := range order {
		if at(l) < 0 {
			t.Errorf("Write a function lacks %s: %q", l, labels)
		} else if i > 0 && at(order[i-1]) > at(l) {
			t.Errorf("Write a function puts %s before %s: %q", order[i-1], l, labels)
		}
	}
	for _, l := range []string{"Allow public access", "Require authentication", "Memory", "CPU", "Request timeout",
		"Maximum concurrent requests per instance", "Environment variables", "Service account"} {
		if !strings.Contains(labels, l) {
			t.Errorf("Authentication lacks %s: %q", l, labels)
		}
	}
	builds, why := amd64FunctionBuilds(ctx, t)
	if !builds {
		t.Logf("Write a function's Create is not submitted: %s", why)
	}
	if builds {
		writeID := fmt.Sprintf("browser-wf-%d", suffix)
		p.eval(fmt.Sprintf(`(() => { const f = document.querySelector(".modal.is-open #f-name"); f.value = %q;
			f.dispatchEvent(new Event("input", { bubbles: true }));
			const r = [...document.querySelectorAll(".modal.is-open input[type=radio]")].find((r) => r.value === "Allow public access");
			r.checked = true; r.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, writeID), nil)
		p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
		wfName := parent + "/functions/" + writeID
		t.Cleanup(func() {
			if op, err := c.DeleteFunction(context.Background(), &functionspb.DeleteFunctionRequest{Name: wfName}); err == nil {
				_ = op.Wait(context.Background())
			}
		})
		p.waitForWithin(fmt.Sprintf(`location.pathname === "/functions/%s" && new URLSearchParams(location.search).get("tab") === "source" ||
			[...document.querySelectorAll(".modal.is-open .form-error")].some((e) => !e.hidden && e.textContent)`, writeID), 10*time.Minute)
		var where string
		p.eval(`location.pathname + location.search + " " + [...document.querySelectorAll(".modal.is-open .form-error")].map((e) => e.textContent).join(" ")`, &where)
		if !strings.HasPrefix(where, "/functions/"+writeID) || !strings.Contains(where, "tab=source") {
			t.Fatalf("Write a function's Create went to %q; want the function's Source tab", where)
		}
		p.waitFor(`document.querySelector("#view .tab-strip .tab.is-selected, #view .tab-strip .tab[aria-selected=true]") !== null`)
		var sel string
		p.eval(`document.querySelector("#view .tab-strip .tab[aria-selected=true]").textContent.trim()`, &sel)
		if sel != "Source" {
			t.Errorf("the page opened on %q; want Source", sel)
		}
		fn, err := c.GetFunction(ctx, &functionspb.GetFunctionRequest{Name: wfName})
		if err != nil {
			t.Fatalf("GetFunction %s: %v", writeID, err)
		}
		rc, err := runclient.NewServicesClient(ctx, opts...)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = rc.Close() })
		pol, err := rc.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: fn.GetServiceConfig().GetService()})
		public := false
		for _, b := range pol.GetBindings() {
			public = public || b.GetRole() == "roles/run.invoker" && slices.Contains(b.GetMembers(), "allUsers")
		}
		if err != nil || !public {
			t.Errorf("the service's policy after Allow public access = %v, %v; want allUsers as roles/run.invoker", pol, err)
		}
	} else {
		p.clickText(".modal.is-open button", "Cancel")
		p.waitFor(`document.querySelector(".modal.is-open") === null`)
	}

	// The Source tab: Show Payload, Test and Edit source with Rename file.
	source := "/functions/" + id + "?project=" + project + "&tab=source"
	p.navigate(source)
	p.waitFor(`document.body.textContent.includes("round three")`)
	var buttons string
	p.eval(`[...document.querySelectorAll("#view button")].map((b) => b.textContent.trim()).join("|")`, &buttons)
	for _, b := range []string{"Edit source", "Show Payload", "Test"} {
		if !strings.Contains("|"+buttons+"|", "|"+b+"|") {
			t.Errorf("the Source tab's buttons %q lack %s", buttons, b)
		}
	}
	p.clickText("#view button", "Show Payload")
	p.waitFor(`document.querySelector(".modal.is-open #action-result-text") !== null`)
	var payload string
	p.eval(`document.querySelector(".modal.is-open #action-result-text").textContent`, &payload)
	if payload != `{"name":"Hello World"}` {
		t.Errorf("Show Payload = %q; want the HTTP function's JSON body", payload)
	}
	p.clickText(".modal.is-open .modal-actions button", "Close")
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	p.clickText("#view button", "Test")
	p.waitFor(`document.querySelector(".modal.is-open #f-data") !== null`)
	var body string
	p.eval(`document.querySelector(".modal.is-open #f-data").value`, &body)
	if body != `{"name":"Hello World"}` {
		t.Errorf("Test's triggering event = %q", body)
	}
	p.clickText(".modal.is-open button", "Cancel")
	p.waitFor(`document.querySelector(".modal.is-open") === null`)

	p.clickText("#view button", "Edit source")
	p.waitFor(`document.querySelector(".modal.is-open .sourcefiles-rename") !== null`)
	p.eval(`(() => { [...document.querySelectorAll(".modal.is-open .sourcefiles-file")].find((b) => b.textContent === "index.js").click();
		const n = document.querySelector(".modal.is-open .sourcefiles-new"); n.value = "main.js";
		document.querySelector(".modal.is-open .sourcefiles-rename").click(); return true; })()`, nil)
	var files, value string
	p.eval(`[...document.querySelectorAll(".modal.is-open .sourcefiles-file")].map((n) => n.textContent).join("|")`, &files)
	p.eval(`document.querySelector(".modal.is-open .sourcefiles-editor").value`, &value)
	if files != "main.js|package.json" || !strings.Contains(value, "round three") {
		t.Errorf("after Rename file the files are %q, the open one %q", files, value)
	}
	p.clickText(".modal.is-open button", "Cancel")
	p.waitFor(`document.querySelector(".modal.is-open") === null`)

	click := func(sel, label string) string {
		return fmt.Sprintf(`[...document.querySelectorAll(%q)].find((b) => b.textContent.trim() === %q).click()`, sel, label)
	}
	auditScreens(t, []a11yPage{
		{"Cloud Run functions list", list, settled + ` && document.querySelector("#view table tbody") !== null`, ""},
		{"Write a function", "/run?project=" + project + "&action=write-function", `document.querySelector(".modal.is-open .sourcefiles-editor") !== null`, ""},
		{"function Source tab", source, `document.body.textContent.includes("round three")`, ""},
		{"Show Payload", source, `document.body.textContent.includes("round three")`, click("#view button", "Show Payload")},
		{"Edit source with Rename file", source, `document.body.textContent.includes("round three")`, click("#view button", "Edit source")},
	})
}
