//go:build browser

package browser

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	eventarc "cloud.google.com/go/eventarc/apiv1"
	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"
	functions "cloud.google.com/go/functions/apiv2"
	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestCloudRunFunctionsPagesThroughTheBrowser (#1853), in the run shard:
// Cloud Run functions as Google's console has them.
//   - The list is titled Cloud Run functions, with Google's columns, and
//     Create function opens Cloud Run's Write a function, its inline editor.
//   - A function's page has Metrics, Details, Source, Variables, Triggers,
//     Permissions, Logs and Testing, in that order, and Edit function,
//     Copy function and Download source beside the title.
//   - Source shows the archive's files; Edit source opens the inline editor
//     over them, ending in Save and redeploy.
//   - Triggers shows the HTTP trigger; Add trigger makes an Eventarc
//     trigger to the function, which the official Eventarc client reads,
//     and Delete trigger removes it.
//   - A Cloud Firestore function's Edit trigger changes its event trigger
//     to Cloud Audit Logs, which GetFunction reads.
//   - Testing shows the curl command and Test the function's JSON body.
//
// Each function is made with the official v2 client from an uploaded
// archive, its build pointed at a generation of it that does not exist, so
// the build fails at once instead of running Google's builder (whose
// images are linux/amd64 only); its page, source and triggers are all
// there. Save and redeploy and Copy function rebuild, so they are sent
// only where builds are asked for (CLOUDBURROW_TEST_FUNCTIONS), and the
// unit test TestConsoleFunctionsParity sends both against the adapter.
func TestCloudRunFunctionsPagesThroughTheBrowser(t *testing.T) {
	needService(t, "functions")
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envRun)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: the functions are made and read back through the Cloud Functions v2 API", envRun)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	opts := []option.ClientOption{option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
	c, err := functions.NewFunctionClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ea, err := eventarc.NewClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ea.Close() })
	project := uniqueProject(t)
	parent := "projects/" + project + "/locations/us-central1"
	suffix := time.Now().UnixNano() % 1e6

	var src bytes.Buffer
	zw := zip.NewWriter(&src)
	for file, body := range map[string]string{
		"package.json": `{"name":"hello","version":"1.0.0","main":"index.js"}`,
		"index.js":     `exports.hello = (req, res) => res.send("hello from the source tab");`,
	} {
		w, _ := zw.Create(file)
		_, _ = w.Write([]byte(body))
	}
	_ = zw.Close()
	mkFunction := func(id string, trigger *functionspb.EventTrigger) string {
		t.Helper()
		up, err := c.GenerateUploadUrl(ctx, &functionspb.GenerateUploadUrlRequest{Parent: parent})
		if err != nil {
			t.Skipf("GenerateUploadUrl: %v: this instance takes no function source", err)
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPut, up.GetUploadUrl(), bytes.NewReader(src.Bytes()))
		req.Header.Set("Content-Type", "application/zip")
		client := &http.Client{Timeout: time.Minute}
		defer client.CloseIdleConnections()
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("upload the source: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("upload the source = %s", resp.Status)
		}
		ss := up.GetStorageSource()
		ss.Generation = 1 << 50 // no such generation: the build fails at once
		name := parent + "/functions/" + id
		if _, err := c.CreateFunction(ctx, &functionspb.CreateFunctionRequest{Parent: parent, FunctionId: id, Function: &functionspb.Function{
			BuildConfig: &functionspb.BuildConfig{Runtime: "nodejs22", EntryPoint: "hello",
				Source: &functionspb.Source{Source: &functionspb.Source_StorageSource{StorageSource: ss}}},
			ServiceConfig: &functionspb.ServiceConfig{AvailableMemory: "512M", EnvironmentVariables: map[string]string{"MODE": "browser"}},
			EventTrigger:  trigger}}); err != nil {
			t.Fatalf("CreateFunction %s: %v", id, err)
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
		return name
	}
	httpID := fmt.Sprintf("browser-par-%d", suffix)
	httpName := mkFunction(httpID, nil)

	p := open(t)
	// The list: Google's title and columns; Create function is Write a function.
	p.navigate("/functions?project=" + project)
	p.waitFor(`document.querySelector("#view table thead") !== null`)
	var heads string
	p.eval(`[...document.querySelectorAll("#view table thead th")].map((n) => n.textContent.trim()).filter(Boolean).join("|")`, &heads)
	for _, col := range []string{"Environment", "Name", "Region", "Trigger", "Runtime", "Memory allocated", "Executed function", "Last deployed", "Deployed by", "Authentication"} {
		if !strings.Contains("|"+heads+"|", "|"+col+"|") {
			t.Errorf("the functions list's columns %q lack %q", heads, col)
		}
	}
	var title string
	p.eval(`document.querySelector("#view h1").textContent.trim()`, &title)
	if title != "Cloud Run functions" {
		t.Errorf("the list is titled %q; want Cloud Run functions", title)
	}
	p.clickText("#view button", "Create function")
	p.waitFor(`location.pathname === "/run" && document.querySelector(".modal.is-open .sourcefiles-editor") !== null`)
	var label string
	p.eval(`document.querySelector(".modal.is-open h2, .modal.is-open .modal-title").textContent.trim()`, &label)
	if !strings.Contains(label, "Write a function") {
		t.Errorf("Create function opened %q; want Cloud Run's Write a function", label)
	}
	p.clickText(".modal.is-open button", "Cancel")
	p.waitFor(`document.querySelector(".modal.is-open") === null`)

	// The function's page: Google's tabs and actions.
	page := "/functions/" + httpID + "?project=" + project
	p.navigate(page)
	p.waitFor(`document.querySelectorAll("#view .tab-strip .tab").length > 0`)
	var tabs, actions string
	p.eval(`[...document.querySelectorAll("#view .tab-strip .tab")].map((n) => n.textContent.trim()).join("|")`, &tabs)
	if !strings.HasPrefix(tabs, "Metrics|Details|Source|Variables|Triggers|Permissions|Logs|Testing") {
		t.Errorf("the function's tabs = %q; want Google's", tabs)
	}
	p.eval(`[...document.querySelectorAll("#view .page-actions button")].map((n) => n.textContent.trim()).join("|")`, &actions)
	for _, a := range []string{"Edit function", "Copy function", "Download source", "Delete"} {
		if !strings.Contains("|"+actions+"|", "|"+a+"|") {
			t.Errorf("the page's actions %q lack %s", actions, a)
		}
	}

	// Source: the files, and Edit source over them.
	p.clickText("#view .tab-strip .tab", "Source")
	p.waitFor(`document.body.textContent.includes("hello from the source tab")`)
	p.clickText("#view button", "Edit source")
	p.waitFor(`document.querySelector(".modal.is-open .sourcefiles-editor") !== null`)
	var files, submit string
	p.eval(`[...document.querySelectorAll(".modal.is-open .sourcefiles-file")].map((n) => n.textContent).join("|")`, &files)
	p.eval(`document.querySelector(".modal.is-open button[type='submit']").textContent.trim()`, &submit)
	if files != "index.js|package.json" || submit != "Save and redeploy" {
		t.Errorf("Edit source holds %q, ending in %q; want the function's files and Save and redeploy", files, submit)
	}
	if os.Getenv("CLOUDBURROW_TEST_FUNCTIONS") != "" {
		p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
		p.waitFor(`document.querySelector(".modal.is-open") === null`)
		if fn, err := c.GetFunction(ctx, &functionspb.GetFunctionRequest{Name: httpName}); err != nil ||
			fn.GetBuildConfig().GetSource().GetStorageSource().GetGeneration() == 1<<50 {
			t.Errorf("after Save and redeploy the function = %v, %v; want a new source", fn, err)
		}
	} else {
		p.clickText(".modal.is-open button", "Cancel")
		p.waitFor(`document.querySelector(".modal.is-open") === null`)
	}

	// Variables: the runtime environment.
	p.navigate(page + "&tab=variables")
	p.waitFor(`document.body.textContent.includes("MODE") && document.body.textContent.includes("browser")`)

	// Testing: the curl command and the JSON body.
	p.navigate(page + "&tab=testing")
	p.waitFor(`document.body.textContent.includes("Test command") && [...document.querySelectorAll("#view button")].some((b) => b.textContent.trim() === "Test the function")`)

	// Triggers: HTTP, then an Eventarc trigger added and deleted.
	p.navigate(page + "&tab=triggers")
	p.waitFor(`document.querySelector("#view table tbody") !== null && document.querySelector("#view table tbody").textContent.includes("HTTP")`)
	triggerID := fmt.Sprintf("browser-tr-%d", suffix)
	// Add trigger is Google's Eventarc pane (#2049), its destination the
	// function's Cloud Run service; the id's dots are escaped for CSS.
	p.pageAction("Add trigger", map[string]string{"name": triggerID, "triggerType": "Google Sources", "provider": "pubsub.googleapis.com",
		`eventType\\.pubsub\\.googleapis\\.com`: "google.cloud.pubsub.topic.v1.messagePublished"}, "")
	tr, err := ea.GetTrigger(ctx, &eventarcpb.GetTriggerRequest{Name: parent + "/triggers/" + triggerID})
	if err != nil || tr.GetDestination().GetCloudRun().GetService() == "" && tr.GetDestination().GetCloudFunction() != httpName {
		t.Fatalf("GetTrigger after Add trigger = %v, %v; want one to the function", tr, err)
	}
	// The page redraws once Add trigger's pane closes, so its table can be
	// missing for a frame (main run 45530827608).
	p.waitFor(fmt.Sprintf(`document.querySelector("#view table tbody") !== null && document.querySelector("#view table tbody").textContent.includes(%q)`, triggerID))
	p.pageAction("Delete trigger", map[string]string{"name": triggerID}, "")
	it := ea.ListTriggers(ctx, &eventarcpb.ListTriggersRequest{Parent: parent})
	for {
		x, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListTriggers: %v", err)
		}
		if x.GetName() == parent+"/triggers/"+triggerID {
			t.Errorf("%s is still listed after Delete trigger", triggerID)
		}
	}

	// A Cloud Firestore function: Edit trigger changes it to Cloud Audit Logs.
	fsID := fmt.Sprintf("browser-fs-%d", suffix)
	fsName := mkFunction(fsID, &functionspb.EventTrigger{EventType: "google.cloud.firestore.document.v1.written",
		EventFilters: []*functionspb.EventFilter{{Attribute: "database", Value: "(default)"}}})
	p.navigate("/functions/" + fsID + "?project=" + project + "&tab=triggers")
	p.waitFor(`document.body.textContent.includes("google.cloud.firestore.document.v1.written")`)
	p.pageAction("Edit trigger", map[string]string{"trigger": "Cloud Audit Logs", "serviceName": "secretmanager.googleapis.com",
		"methodName": "google.cloud.secretmanager.v1.SecretManagerService.CreateSecret"}, "")
	fn, err := c.GetFunction(ctx, &functionspb.GetFunctionRequest{Name: fsName})
	if err != nil || fn.GetEventTrigger().GetEventType() != "google.cloud.audit.log.v1.written" {
		t.Errorf("GetFunction after Edit trigger = %v, %v; want the audit-log trigger", fn.GetEventTrigger(), err)
	}

	if os.Getenv("CLOUDBURROW_TEST_FUNCTIONS") != "" {
		copyID := httpID + "-copy"
		p.navigate(page)
		p.pageAction("Copy function", map[string]string{"name": copyID}, "")
		t.Cleanup(func() {
			if op, err := c.DeleteFunction(context.Background(), &functionspb.DeleteFunctionRequest{Name: parent + "/functions/" + copyID}); err == nil {
				_ = op.Wait(context.Background())
			}
		})
		if _, err := c.GetFunction(ctx, &functionspb.GetFunctionRequest{Name: parent + "/functions/" + copyID}); err != nil {
			t.Errorf("GetFunction of the copy: %v", err)
		}
	}
}
