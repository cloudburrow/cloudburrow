//go:build browser

package browser

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	functions "cloud.google.com/go/functions/apiv2"
	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	"cloud.google.com/go/iam/apiv1/iampb"
	monitoring "cloud.google.com/go/monitoring/apiv3/v2"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	runclient "cloud.google.com/go/run/apiv2"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestCloudRunFunctionsRound4ThroughTheBrowser (#2049), in the run shard,
// on any node architecture (nothing here is built):
//   - Write a function has Add trigger, which reveals Google's Eventarc
//     pane (its event providers read from the served Eventarc API), and
//     the Cloud Run settings on Container(s), Volumes and Networking;
//   - Create makes the service with Google's placeholder revision through
//     the Cloud Run API, with Allow public access and the second
//     generation execution environment, and lands on its Source tab, which
//     offers Save and redeploy over the runtime's sample;
//   - the placeholder answers at the service's URL, and those requests
//     are Cloud Monitoring's run.googleapis.com/request_count, which the
//     official Monitoring client reads and the Metrics tab charts;
//   - the Services list's Deployed by is the service's last_modifier;
//   - a function's Source tab has Base image and Edit runtime and security
//     updates, whose On deploy is stored as on_deploy_update_policy;
//   - the Testing tab's Run in Cloud Shell runs the test command in the
//     console's terminal.
//
// Save and redeploy builds with Google's builder, so it is submitted only
// where builds run (amd64 nodes, CLOUDBURROW_TEST_FUNCTIONS). Then the
// screens pass the console's WCAG 2.1 AA audit.
func TestCloudRunFunctionsRound4ThroughTheBrowser(t *testing.T) {
	needService(t, "functions")
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envRun)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: the service is read back through the Cloud Run API", envRun)
	}
	monAddr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envMonitoring)), "http://")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	opts := []option.ClientOption{option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
	rc, err := runclient.NewServicesClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rc.Close() })
	fc, err := functions.NewFunctionClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fc.Close() })
	project := uniqueProject(t)
	parent := "projects/" + project + "/locations/us-central1"
	id := fmt.Sprintf("browser-r4-%d", time.Now().UnixNano()%1e6)
	svcName := parent + "/services/" + id
	t.Cleanup(func() {
		if op, err := fc.DeleteFunction(context.Background(), &functionspb.DeleteFunctionRequest{Name: parent + "/functions/" + id}); err == nil {
			_ = op.Wait(context.Background())
		}
		if op, err := rc.DeleteService(context.Background(), &runpb.DeleteServiceRequest{Name: svcName}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})

	p := open(t)
	write := "/run?project=" + project + "&action=write-function"
	p.navigate(write)
	p.waitFor(`document.querySelector(".modal.is-open .sourcefiles-editor") !== null`)
	var labels string
	visibleLabels := `[...document.querySelectorAll(".modal.is-open form label, .modal.is-open form legend")].map((n) => n.textContent.trim()).join("|")`
	p.eval(visibleLabels, &labels)
	for _, l := range []string{"Add trigger", "Execution environment", "Cloud SQL connections", "In-memory volumes",
		"Cloud Storage volumes", "NFS volumes", "Use HTTP/2 end-to-end", "VPC connector", "Session affinity"} {
		if !strings.Contains(labels, l) {
			t.Errorf("Write a function lacks %s: %q", l, labels)
		}
	}
	if strings.Contains(labels, "Environment|") && strings.Contains(labels, "2nd gen") {
		t.Errorf("Write a function still asks for an Environment: %q", labels)
	}
	// Add trigger reveals Google's Eventarc pane.
	p.eval(`(() => { const c = document.querySelector(".modal.is-open #f-addTrigger"); c.checked = true;
		c.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, nil)
	p.waitFor(`(() => { const f = document.querySelector(".modal.is-open [id='f-trigger.triggerType']");
		return f !== null && f.closest("[hidden]") === null; })()`)
	var pane, providers string
	p.eval(`[...document.querySelectorAll(".modal.is-open form label")].filter((n) => n.closest("[hidden]") === null &&
		(n.htmlFor || "").startsWith("f-trigger.")).map((n) => n.textContent.trim()).join("|")`, &pane)
	for _, l := range []string{"Trigger name", "Trigger type", "Event provider", "Event type", "Region", "Service account", "Service URL path"} {
		if !strings.Contains(pane, l) {
			t.Errorf("the Eventarc pane lacks %s: %q", l, pane)
		}
	}
	p.eval(`[...document.querySelectorAll(".modal.is-open [id='f-trigger.provider'] option")].map((o) => o.value).filter(Boolean).join(",")`, &providers)
	if providers == "" {
		t.Errorf("the Eventarc pane's Event provider offers nothing: ListProviders was not read")
	}
	p.eval(`(() => { const c = document.querySelector(".modal.is-open #f-addTrigger"); c.checked = false;
		c.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, nil)

	// Create: the placeholder revision, publicly invocable, second
	// generation execution environment.
	p.eval(fmt.Sprintf(`(() => { const f = document.querySelector(".modal.is-open #f-name"); f.value = %q;
		f.dispatchEvent(new Event("input", { bubbles: true }));
		const r = [...document.querySelectorAll(".modal.is-open input[type=radio]")].find((r) => r.value === "Allow public access");
		r.checked = true; r.dispatchEvent(new Event("change", { bubbles: true }));
		const e = document.querySelector(".modal.is-open #f-executionEnvironment"); e.value = "Second generation";
		e.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, id), nil)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitForWithin(fmt.Sprintf(`location.pathname === "/run/%s" && new URLSearchParams(location.search).get("tab") === "source" ||
		[...document.querySelectorAll(".modal.is-open .form-error")].some((e) => !e.hidden && e.textContent)`, id), 5*time.Minute)
	var where string
	p.eval(`location.pathname + location.search + " " + [...document.querySelectorAll(".modal.is-open .form-error")].map((e) => e.textContent).join(" ")`, &where)
	if !strings.HasPrefix(where, "/run/"+id) || !strings.Contains(where, "tab=source") {
		t.Fatalf("Write a function's Create went to %q; want the service's Source tab", where)
	}
	p.waitFor(`document.body.textContent.includes("placeholder revision") && document.body.textContent.includes("index.js")`)
	var buttons string
	p.eval(`[...document.querySelectorAll("#view button")].map((b) => b.textContent.trim()).join("|")`, &buttons)
	if !strings.Contains("|"+buttons+"|", "|Edit source|") {
		t.Errorf("the placeholder's Source tab lacks Edit source: %q", buttons)
	}

	svc, err := rc.GetService(ctx, &runpb.GetServiceRequest{Name: svcName})
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	c := svc.GetTemplate().GetContainers()
	bc := svc.GetBuildConfig()
	if len(c) != 1 || c[0].GetImage() != "us-docker.pkg.dev/cloudrun/container/placeholder" ||
		svc.GetTemplate().GetExecutionEnvironment() != runpb.ExecutionEnvironment_EXECUTION_ENVIRONMENT_GEN2 ||
		bc.GetFunctionTarget() != "helloHttp" || !strings.HasSuffix(bc.GetBaseImage(), "/runtimes/nodejs22") ||
		!strings.HasPrefix(bc.GetSourceLocation(), "gs://") || !bc.GetEnableAutomaticUpdates() ||
		svc.GetLastModifier() == "" || svc.GetCreator() == "" {
		t.Errorf("the placeholder's service = %v", svc)
	}
	pol, err := rc.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: svcName})
	public := false
	for _, b := range pol.GetBindings() {
		public = public || b.GetRole() == "roles/run.invoker" && slices.Contains(b.GetMembers(), "allUsers")
	}
	if err != nil || !public {
		t.Errorf("the service's policy after Allow public access = %v, %v", pol, err)
	}

	// The placeholder answers, through the gateway, at the service's URL.
	gw := gatewayForward(t)
	sent := 0
	for i := 0; i < 5; i++ {
		body, code := getThroughGateway(ctx, t, gw, svc.GetUri())
		if code != http.StatusOK || !strings.Contains(body, "placeholder") {
			t.Fatalf("GET %s = %d %q; want the placeholder's page", svc.GetUri(), code, body)
		}
		sent++
	}

	// Deployed by: the service's last_modifier.
	p.navigate("/run?project=" + project)
	p.waitFor(fmt.Sprintf(`document.querySelector("#view table tbody") !== null && document.querySelector("#view table tbody").textContent.includes(%q)`, id))
	var by string
	p.eval(fmt.Sprintf(`(() => { const r = [...document.querySelectorAll("#view table tbody tr")].find((r) => r.textContent.includes(%q));
		const i = [...document.querySelectorAll("#view table thead th")].findIndex((h) => h.textContent.trim() === "Deployed by");
		return r && i >= 0 ? r.children[i].textContent.trim() : ""; })()`, id), &by)
	if by != svc.GetLastModifier() {
		t.Errorf("Deployed by = %q; want the service's last_modifier %q", by, svc.GetLastModifier())
	}

	// Metrics: Cloud Monitoring's request count, read through the official
	// client, and charted on the Metrics tab.
	metrics := "/run/" + id + "?project=" + project + "&tab=metrics"
	if monAddr == "" {
		t.Logf("%s is not set: the Metrics tab is checked without reading Cloud Monitoring back", envMonitoring)
	} else {
		mc, err := monitoring.NewMetricClient(ctx, option.WithEndpoint(monAddr), option.WithoutAuthentication(),
			option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = mc.Close() })
		var total int64
		for deadline := time.Now().Add(3 * time.Minute); total < int64(sent) && time.Now().Before(deadline); time.Sleep(5 * time.Second) {
			total = 0
			it := mc.ListTimeSeries(ctx, &monitoringpb.ListTimeSeriesRequest{Name: "projects/" + project,
				Filter:   fmt.Sprintf(`metric.type = "run.googleapis.com/request_count" AND resource.labels.service_name = %q`, id),
				Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(time.Now().Add(-time.Hour)), EndTime: timestamppb.Now()}})
			for {
				ts, err := it.Next()
				if err != nil {
					if err != iterator.Done {
						t.Logf("ListTimeSeries: %v", err)
					}
					break
				}
				for _, pt := range ts.GetPoints() {
					total += pt.GetValue().GetInt64Value()
				}
			}
		}
		if total < int64(sent) {
			t.Errorf("run.googleapis.com/request_count for %s = %d; want at least the %d requests sent", id, total, sent)
		}
	}
	p.navigate(metrics)
	p.waitForWithin(`document.body.textContent.includes("Request count") && document.body.textContent.includes("Container instance count")`, 2*time.Minute)

	// A function's Source tab: Base image, Edit runtime and security
	// updates. A function whose build fails at once (no such source
	// generation) has the tab without Google's builder.
	fnID := id + "-fn"
	up, err := fc.GenerateUploadUrl(ctx, &functionspb.GenerateUploadUrlRequest{Parent: parent})
	if err != nil {
		t.Fatalf("GenerateUploadUrl: %v", err)
	}
	var src bytes.Buffer
	zw := zip.NewWriter(&src)
	w, _ := zw.Create("index.js")
	_, _ = w.Write([]byte(`exports.hello = (req, res) => res.send("round four");`))
	_ = zw.Close()
	put, _ := http.NewRequestWithContext(ctx, http.MethodPut, up.GetUploadUrl(), bytes.NewReader(src.Bytes()))
	put.Header.Set("Content-Type", "application/zip")
	if resp, err := http.DefaultClient.Do(put); err != nil {
		t.Fatalf("upload the source: %v", err)
	} else {
		resp.Body.Close()
	}
	ss := up.GetStorageSource()
	ss.Generation = 1 << 50
	fnName := parent + "/functions/" + fnID
	if _, err := fc.CreateFunction(ctx, &functionspb.CreateFunctionRequest{Parent: parent, FunctionId: fnID, Function: &functionspb.Function{
		BuildConfig: &functionspb.BuildConfig{Runtime: "nodejs22", EntryPoint: "hello",
			Source: &functionspb.Source{Source: &functionspb.Source_StorageSource{StorageSource: ss}}}}}); err != nil {
		t.Fatalf("CreateFunction: %v", err)
	}
	t.Cleanup(func() {
		if op, err := fc.DeleteFunction(context.Background(), &functionspb.DeleteFunctionRequest{Name: fnName}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	settle := func() *functionspb.Function {
		for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(time.Second) {
			fn, err := fc.GetFunction(ctx, &functionspb.GetFunctionRequest{Name: fnName})
			if err == nil && fn.GetState() != functionspb.Function_DEPLOYING {
				return fn
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s is still deploying: %v, %v", fnID, fn, err)
			}
		}
	}
	if fn := settle(); fn.GetBuildConfig().GetAutomaticUpdatePolicy() == nil {
		t.Errorf("a new function's runtime update policy = %v; want automatic, the default", fn.GetBuildConfig())
	}
	source := "/functions/" + fnID + "?project=" + project + "&tab=source"
	p.navigate(source)
	p.waitFor(`document.body.textContent.includes("Base image: us-central1-docker.pkg.dev/serverless-runtimes/")`)
	p.clickText("#view button", "Edit runtime and security updates")
	p.waitFor(`document.querySelector(".modal.is-open #f-runtime") !== null`)
	p.eval(`(() => { const r = [...document.querySelectorAll(".modal.is-open input[type=radio]")].find((r) => r.value === "On deploy");
		r.checked = true; r.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, nil)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	if fn := settle(); fn.GetBuildConfig().GetOnDeployUpdatePolicy() == nil {
		t.Errorf("after On deploy the policy = %v; want on_deploy_update_policy", fn.GetBuildConfig())
	}

	// Testing: Run in Cloud Shell runs the command in the terminal.
	testTab := "/functions/" + fnID + "?project=" + project + "&tab=testing"
	p.navigate(testTab)
	p.waitFor(`document.body.textContent.includes("Test command (curl)")`)
	p.clickText("#view button", "Run in Cloud Shell")
	p.waitFor(`!document.querySelector("#terminal-drawer").hidden`)
	p.awaitTerminal()
	p.waitForWithin(`document.querySelector("#terminal-screen .xterm-rows").innerText.includes("--connect-to ::kourier")`, 3*time.Minute)

	builds, why := amd64FunctionBuilds(ctx, t)
	if !builds {
		t.Logf("the placeholder's Save and redeploy is not submitted: %s", why)
	} else {
		// Save and redeploy builds the sample and deploys it onto the
		// service, which keeps what Write a function set on it.
		p.navigate("/run/" + id + "?project=" + project + "&tab=source")
		p.waitFor(`document.body.textContent.includes("placeholder revision")`)
		p.clickText("#view button", "Edit source")
		p.waitFor(`document.querySelector(".modal.is-open .sourcefiles-editor") !== null`)
		p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
		p.waitFor(`document.querySelector(".modal.is-open") === null`)
		for deadline := time.Now().Add(15 * time.Minute); ; time.Sleep(5 * time.Second) {
			fn, err := fc.GetFunction(ctx, &functionspb.GetFunctionRequest{Name: parent + "/functions/" + id})
			if err == nil && fn.GetState() == functionspb.Function_ACTIVE {
				break
			}
			if err == nil && fn.GetState() == functionspb.Function_FAILED || time.Now().After(deadline) {
				t.Fatalf("the placeholder's function did not deploy: %v, %v", fn, err)
			}
		}
		deployed, err := rc.GetService(ctx, &runpb.GetServiceRequest{Name: svcName})
		if err != nil || deployed.GetTemplate().GetContainers()[0].GetImage() == "us-docker.pkg.dev/cloudrun/container/placeholder" ||
			deployed.GetTemplate().GetExecutionEnvironment() != runpb.ExecutionEnvironment_EXECUTION_ENVIRONMENT_GEN2 {
			t.Errorf("after Save and redeploy the service = %v, %v", deployed, err)
		}
		if body, code := getThroughGateway(ctx, t, gw, deployed.GetUri()); code != http.StatusOK || !strings.Contains(body, "Hello") {
			t.Errorf("the deployed function answered %d %q", code, body)
		}
	}

	click := func(sel, label string) string {
		return fmt.Sprintf(`[...document.querySelectorAll(%q)].find((b) => b.textContent.trim() === %q).click()`, sel, label)
	}
	auditScreens(t, []a11yPage{
		{"Write a function with Add trigger", write, `document.querySelector(".modal.is-open .sourcefiles-editor") !== null`,
			`(() => { const c = document.querySelector(".modal.is-open #f-addTrigger"); c.checked = true;
			c.dispatchEvent(new Event("change", { bubbles: true })); })()`},
		{"placeholder Source tab", "/run/" + id + "?project=" + project + "&tab=source", `document.body.textContent.includes("placeholder revision")`, ""},
		{"service Metrics tab", metrics, `document.body.textContent.includes("Request count")`, ""},
		{"Edit runtime and security updates", source, `document.body.textContent.includes("Base image")`,
			click("#view button", "Edit runtime and security updates")},
	})
}

// gatewayForward port-forwards the cluster's ingress gateway, which serves
// every service's URL, to a loopback port, and returns its address.
func gatewayForward(t *testing.T) string {
	t.Helper()
	kubeconfig := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_KUBECONFIG"))
	if kubeconfig == "" {
		t.Skip("CLOUDBURROW_TEST_KUBECONFIG is not set; the service's URL is reached through a port-forward of the gateway")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig, "-n", "kourier-system", "port-forward", "svc/kourier",
		fmt.Sprintf("%d:80", port))
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("port-forward the gateway: %v", err)
	}
	t.Cleanup(func() { cancel(); _ = cmd.Wait() })
	a := fmt.Sprintf("127.0.0.1:%d", port)
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		if c, err := net.DialTimeout("tcp", a, time.Second); err == nil {
			_ = c.Close()
			return a
		}
		if time.Now().After(deadline) {
			t.Fatalf("the gateway's port-forward did not open")
		}
	}
}

// getThroughGateway GETs a service URL through the gateway at gw, the
// URL's host as the request's.
func getThroughGateway(ctx context.Context, t *testing.T, gw, raw string) (string, int) {
	t.Helper()
	d := &net.Dialer{Timeout: 10 * time.Second}
	hc := &http.Client{Timeout: time.Minute, Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return d.DialContext(ctx, network, gw)
	}}}
	defer hc.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		t.Fatalf("service URL %q: %v", raw, err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", raw, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	return string(b), resp.StatusCode
}
