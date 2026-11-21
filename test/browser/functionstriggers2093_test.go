//go:build browser

package browser

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/datastore"
	eventarc "cloud.google.com/go/eventarc/apiv1"
	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"
	"cloud.google.com/go/firestore"
	functions "cloud.google.com/go/functions/apiv2"
	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	run "cloud.google.com/go/run/apiv2"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	secretmanagerpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// The run shard's Firestore and Datastore emulators, which the triggers'
// events are caused on.
const (
	envRunFirestore = "CLOUDBURROW_TEST_RUN_FIRESTORE"
	envRunDatastore = "CLOUDBURROW_TEST_RUN_DATASTORE"
	// browserEventProbe is testdata/eventprobe's image, built for this test.
	browserEventProbe = "dev.local/cloudburrow-eventprobe:browser"
)

// TestCloudRunFunctionsEventarcTriggersDeliverThroughTheBrowser (#2093), in
// the run shard: a function's Add trigger offers every event provider the
// instance's Eventarc serves, in Google's pane (Trigger type, Event
// provider, Event type, the type's filters, Region, Service account), and
// each kind of trigger made there delivers. A Cloud Run service running
// testdata/eventprobe, which records each CloudEvent it is sent, is the
// destination; through its Triggers tab's Add trigger (the tab a Cloud Run
// function has) the test makes
//   - a Cloud Audit Logs trigger on Secret Manager's AddSecretVersion, its
//     Resource a path pattern of the secret's name;
//   - a Cloud Firestore document.v1.created trigger on (default), its
//     Document a path pattern;
//   - a Datastore mode entity.v1.created trigger, its Entity a pattern;
//   - Eventarc's own trigger.v1.created, filtered on a trigger's ID;
//   - Cloud Run functions' function.v2.created, filtered on a function's ID;
//
// then causes each event with the official clients (AddSecretVersion, a
// document, an entity, CreateTrigger, CreateFunction) and sees the probe
// invoked with that event's CloudEvent at the trigger's Service URL path.
// The function's own Triggers tab's Add trigger is the same pane. The
// pane is audited for WCAG 2.1 AA with an audit-log event type chosen.
func TestCloudRunFunctionsEventarcTriggersDeliverThroughTheBrowser(t *testing.T) {
	needService(t, "run")
	needService(t, "functions")
	runAddr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envRun)), "http://")
	smAddr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envSecrets)), "http://")
	fsAddr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envRunFirestore)), "http://")
	dsAddr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envRunDatastore)), "http://")
	for name, v := range map[string]string{envRun: runAddr, envSecrets: smAddr, envRunFirestore: fsAddr, envRunDatastore: dsAddr} {
		if v == "" {
			t.Skipf("%s is not set: each trigger's event is caused through that service's own API", name)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	opts := []option.ClientOption{option.WithEndpoint(runAddr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
	rc, err := run.NewServicesClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rc.Close() })
	fc, err := functions.NewFunctionClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fc.Close() })
	ec, err := eventarc.NewClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ec.Close() })
	sm, err := secretmanager.NewClient(ctx, option.WithEndpoint(smAddr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	// The emulator clients take their emulator's address, set here and
	// checked above, so they never dial Google.
	t.Setenv("FIRESTORE_EMULATOR_HOST", fsAddr)
	t.Setenv("DATASTORE_EMULATOR_HOST", dsAddr)

	loadBrowserEventProbe(ctx, t)
	project := uniqueProject(t)
	parent := "projects/" + project + "/locations/us-central1"
	n := fmt.Sprintf("%06d", time.Now().UnixNano()%1e6)
	svcID := "ea-fn-probe-" + n
	op, err := rc.CreateService(ctx, &runpb.CreateServiceRequest{Parent: parent, ServiceId: svcID, Service: &runpb.Service{
		Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{{Image: browserEventProbe}},
			Scaling: &runpb.RevisionScaling{MinInstanceCount: 1, MaxInstanceCount: 1}}}})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	t.Cleanup(func() {
		dctx, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		_, _ = rc.DeleteService(dctx, &runpb.DeleteServiceRequest{Name: parent + "/services/" + svcID})
	})
	svc, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("the probe service never became ready: %v", err)
	}
	gw := gatewayForward(t)

	p := open(t)
	tab := "/run/" + svcID + "?project=" + project + "&tab=triggers"
	p.navigate(tab)
	p.waitFor(`document.querySelector("#view .section-actions") !== null`)

	// The pane: every served provider, none of Firebase's.
	p.clickText("#view .section-actions button", "Add trigger")
	p.waitFor(`document.querySelector(".modal.is-open #f-provider") !== null`)
	var pane struct {
		Providers, Labels []string
		Submit            string
	}
	p.eval(`(() => { const m = document.querySelector(".modal.is-open"); return {
		Providers: [...m.querySelectorAll("#f-provider option")].map((o) => o.value),
		Labels: [...m.querySelectorAll("label")].map((n) => n.textContent.trim()),
		Submit: m.querySelector('button[type="submit"]').textContent.trim() }; })()`, &pane)
	for _, want := range []string{"pubsub.googleapis.com", "storage.googleapis.com", "firestore.googleapis.com", "secretmanager.googleapis.com",
		"cloudfunctions.googleapis.com", "eventarc.googleapis.com", "redis.googleapis.com"} {
		if !slices.Contains(pane.Providers, want) {
			t.Errorf("Event provider lacks %s: %v", want, pane.Providers)
		}
	}
	for _, pr := range pane.Providers {
		if strings.Contains(pr, "firebase") {
			t.Errorf("Event provider offers %s, which is not served", pr)
		}
	}
	labels := "|" + strings.Join(pane.Labels, "|") + "|"
	for _, want := range []string{"Trigger name", "Trigger type", "Event provider", "Event type", "Region", "Service account", "Service URL path"} {
		if !strings.Contains(labels, "|"+want) {
			t.Errorf("the pane has no %q: %s", want, labels)
		}
	}
	if pane.Submit != "Save trigger" {
		t.Errorf("the pane ends in %q", pane.Submit)
	}
	p.clickText(".modal.is-open button", "Cancel")
	p.waitFor(`document.querySelector(".modal.is-open") === null`)

	esc := func(s string) string { return strings.ReplaceAll(s, ".", `\\.`) }
	add := func(name, provider, eventType, path string, filters map[string]string) {
		t.Helper()
		v := map[string]string{"name": name, "triggerType": "Google Sources", "provider": provider,
			esc("eventType." + provider): eventType, "path": path, "region": "us-central1"}
		for k, x := range filters {
			v[esc(k)] = x
		}
		p.navigate(tab)
		p.waitFor(`document.querySelector("#view .section-actions") !== null`)
		p.pageAction("Add trigger", v, "")
		tr, err := ec.GetTrigger(ctx, &eventarcpb.GetTriggerRequest{Name: parent + "/triggers/" + name})
		if err != nil || tr.GetDestination().GetCloudRun().GetService() != svcID {
			t.Fatalf("GetTrigger %s after Add trigger = %v, %v", name, tr, err)
		}
		t.Cleanup(func() {
			if op, err := ec.DeleteTrigger(context.Background(), &eventarcpb.DeleteTriggerRequest{Name: tr.GetName(), AllowMissing: true}); err == nil {
				_, _ = op.Wait(context.Background())
			}
		})
	}
	secret := "ea-secret-" + n
	collection, kind := "orders"+n, "Task"+n
	targetTrigger, fnID := "ea-target-"+n, "ea-fn-"+n
	add("audit-"+n, "secretmanager.googleapis.com", "google.cloud.audit.log.v1.written", "/audit", map[string]string{
		"filter.secretmanager.googleapis.com.methodName":   "google.cloud.secretmanager.v1.SecretManagerService.AddSecretVersion",
		"resource.secretmanager.googleapis.com":            "Path pattern",
		"filter.secretmanager.googleapis.com.resourceName": "projects/*/secrets/" + secret})
	add("firestore-"+n, "firestore.googleapis.com", "google.cloud.firestore.document.v1.created", "/firestore", map[string]string{
		"filter.firestore.googleapis.com.database": "(default)", "filter.firestore.googleapis.com.document": collection + "/{doc}"})
	add("datastore-"+n, "firestore.googleapis.com", "google.cloud.datastore.entity.v1.created", "/datastore", map[string]string{
		"filter.firestore.googleapis.com.database": "(default)", "filter.firestore.googleapis.com.entity": kind + "/{id}"})
	add("eventarc-"+n, "eventarc.googleapis.com", "google.cloud.eventarc.trigger.v1.created", "/eventarc", map[string]string{
		"filter.eventarc.googleapis.com.trigger": targetTrigger})
	add("functions-"+n, "cloudfunctions.googleapis.com", "google.cloud.functions.function.v2.created", "/functions", map[string]string{
		"filter.cloudfunctions.googleapis.com.function": fnID})

	// The tab lists them with their providers.
	p.navigate(tab)
	p.waitFor(`document.querySelector("#view").textContent.includes("functions-` + n + `")`)
	var rows string
	p.eval(`[...document.querySelectorAll("#view tr")].map((r) => r.textContent).join("\n")`, &rows)
	for _, want := range []string{"Secret Manager (via Cloud Audit Logs)", "Cloud Firestore", "Eventarc", "Cloud Functions",
		"google.cloud.datastore.entity.v1.created", "resourceName: projects/*/secrets/" + secret} {
		if !strings.Contains(rows, want) {
			t.Errorf("the Triggers tab lacks %q:\n%s", want, rows)
		}
	}

	// The watchers take their first snapshot before the events.
	time.Sleep(8 * time.Second)

	// Cloud Audit Logs: AddSecretVersion, whose resource is the secret.
	if _, err := sm.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: "projects/" + project, SecretId: secret,
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_Automatic_{
			Automatic: &secretmanagerpb.Replication_Automatic{}}}}}); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	if _, err := sm.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: "projects/" + project + "/secrets/" + secret,
		Payload: &secretmanagerpb.SecretPayload{Data: []byte("v1")}}); err != nil {
		t.Fatalf("AddSecretVersion: %v", err)
	}
	t.Cleanup(func() {
		_ = sm.DeleteSecret(context.Background(), &secretmanagerpb.DeleteSecretRequest{Name: "projects/" + project + "/secrets/" + secret})
	})
	// Eventarc: the trigger the direct event names.
	top, err := ec.CreateTrigger(ctx, &eventarcpb.CreateTriggerRequest{Parent: parent, TriggerId: targetTrigger, Trigger: &eventarcpb.Trigger{
		EventFilters: []*eventarcpb.EventFilter{{Attribute: "type", Value: "google.cloud.pubsub.topic.v1.messagePublished"}},
		Destination: &eventarcpb.Destination{Descriptor_: &eventarcpb.Destination_CloudRun{CloudRun: &eventarcpb.CloudRun{
			Service: svcID, Region: "us-central1", Path: "/unused"}}}}})
	if err != nil {
		t.Fatalf("CreateTrigger %s: %v", targetTrigger, err)
	}
	if _, err := top.Wait(ctx); err != nil {
		t.Fatalf("CreateTrigger %s: %v", targetTrigger, err)
	}
	t.Cleanup(func() {
		if op, err := ec.DeleteTrigger(context.Background(), &eventarcpb.DeleteTriggerRequest{Name: parent + "/triggers/" + targetTrigger, AllowMissing: true}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})
	// Cloud Run functions: CreateFunction, whose build fails at once.
	fnName := createFailingFunction(ctx, t, fc, parent, fnID)

	// Firestore and Datastore mode: a new document and entity each round,
	// until the probe has one (a write before the watch's first snapshot
	// is not a change).
	fsc, err := firestore.NewClient(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fsc.Close() })
	dsc, err := datastore.NewClient(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dsc.Close() })

	type probed struct {
		Headers map[string]string `json:"headers"`
		Path    string            `json:"path"`
	}
	want := map[string]func(probed) bool{
		"/audit": func(e probed) bool {
			return e.Headers["ce-type"] == "google.cloud.audit.log.v1.written" && e.Headers["ce-methodname"] ==
				"google.cloud.secretmanager.v1.SecretManagerService.AddSecretVersion" && strings.HasSuffix(e.Headers["ce-resourcename"], "/secrets/"+secret)
		},
		"/firestore": func(e probed) bool {
			return e.Headers["ce-type"] == "google.cloud.firestore.document.v1.created" && strings.HasPrefix(e.Headers["ce-document"], collection+"/")
		},
		"/datastore": func(e probed) bool {
			return e.Headers["ce-type"] == "google.cloud.datastore.entity.v1.created" && strings.HasPrefix(e.Headers["ce-entity"], kind+"/")
		},
		"/eventarc": func(e probed) bool {
			return e.Headers["ce-type"] == "google.cloud.eventarc.trigger.v1.created" && e.Headers["ce-trigger"] == targetTrigger
		},
		"/functions": func(e probed) bool {
			return e.Headers["ce-type"] == "google.cloud.functions.function.v2.created" && e.Headers["ce-function"] == fnID
		},
	}
	got := map[string]bool{}
	var last []probed
	for i, deadline := 0, time.Now().Add(2*time.Minute); len(got) < len(want); i++ {
		if !got["/firestore"] {
			if _, err := fsc.Collection(collection).Doc(fmt.Sprintf("d%d", i)).Create(ctx, map[string]any{"n": i}); err != nil {
				t.Fatalf("create a document: %v", err)
			}
		}
		if !got["/datastore"] {
			if _, err := dsc.Put(ctx, datastore.NameKey(kind, fmt.Sprintf("e%d", i), nil), &struct{ N int }{i}); err != nil {
				t.Fatalf("put an entity: %v", err)
			}
		}
		time.Sleep(3 * time.Second)
		body, code := getThroughGateway(ctx, t, gw, svc.GetUri()+"/events")
		if code == http.StatusOK {
			last = nil
			_ = json.Unmarshal([]byte(body), &last)
			for _, e := range last {
				if ok := want[e.Path]; ok != nil && ok(e) {
					got[e.Path] = true
				}
			}
		}
		if time.Now().After(deadline) {
			break
		}
	}
	for path := range want {
		if !got[path] {
			var paths []string
			for _, e := range last {
				paths = append(paths, e.Path+" "+e.Headers["ce-type"])
			}
			t.Errorf("the probe was not invoked at %s with its trigger's event; it got %v", path, paths)
		}
	}

	// The function's own Triggers tab: the same pane, with the same
	// providers, in a tab of its own (each tab has three minutes).
	p = open(t)
	p.navigate("/functions/" + fnID + "?project=" + project + "&tab=triggers")
	p.waitFor(`document.querySelector("#view table tbody") !== null`)
	p.clickText("#view button", "Add trigger")
	p.waitFor(`document.querySelector(".modal.is-open #f-provider") !== null`)
	var fnProviders []string
	p.eval(`[...document.querySelectorAll(".modal.is-open #f-provider option")].map((o) => o.value)`, &fnProviders)
	if !slices.Equal(fnProviders, pane.Providers) {
		t.Errorf("the function's Add trigger offers %v; the service's %v", fnProviders, pane.Providers)
	}
	p.clickText(".modal.is-open button", "Cancel")
	_ = fnName

	auditScreens(t, []a11yPage{
		// A name ending in " dialog" has the audit wait for the pane; its
		// fields are set as soon as it opens.
		{"Add trigger with Cloud Audit Logs dialog", tab, `document.querySelector("#view .section-actions") !== null`,
			`[...document.querySelectorAll("#view .section-actions button")].find((b) => b.textContent.trim() === "Add trigger").click();
			const fill = () => { const m = document.querySelector(".modal.is-open"); if (!m || !m.querySelector("#f-provider")) { setTimeout(fill, 20); return; }
			const set = (sel, v) => { const f = m.querySelector(sel); f.value = v; f.dispatchEvent(new Event("change", { bubbles: true })); };
			set("#f-provider", "secretmanager.googleapis.com");
			set("#f-eventType\\.secretmanager\\.googleapis\\.com", "google.cloud.audit.log.v1.written");
			set("#f-resource\\.secretmanager\\.googleapis\\.com", "Path pattern"); };
			fill()`},
		{"Triggers tab with Eventarc triggers", tab, `document.querySelector("#view").textContent.includes("functions-` + n + `")`, ""},
	})
}

// createFailingFunction makes a 2nd gen function with the official client
// from an uploaded archive, its build pointed at a generation that does
// not exist so that it fails at once, without Google's builder.
func createFailingFunction(ctx context.Context, t *testing.T, c *functions.FunctionClient, parent, id string) string {
	t.Helper()
	var src bytes.Buffer
	zw := zip.NewWriter(&src)
	w, _ := zw.Create("index.js")
	_, _ = w.Write([]byte(`exports.hello = (req, res) => res.send("hello");`))
	_ = zw.Close()
	up, err := c.GenerateUploadUrl(ctx, &functionspb.GenerateUploadUrlRequest{Parent: parent})
	if err != nil {
		t.Fatalf("GenerateUploadUrl: %v", err)
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
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload the source = %s", resp.Status)
	}
	ss := up.GetStorageSource()
	ss.Generation = 1 << 50
	name := parent + "/functions/" + id
	if _, err := c.CreateFunction(ctx, &functionspb.CreateFunctionRequest{Parent: parent, FunctionId: id, Function: &functionspb.Function{
		BuildConfig: &functionspb.BuildConfig{Runtime: "nodejs22", EntryPoint: "hello",
			Source: &functionspb.Source{Source: &functionspb.Source_StorageSource{StorageSource: ss}}}}}); err != nil {
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
			return name
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s is still deploying: %v, %v", id, fn, err)
		}
	}
}

// loadBrowserEventProbe compiles testdata/eventprobe for the cluster's
// nodes, builds its image and loads it into the instance's kind cluster,
// as the compat suite's loadEventProbe does.
func loadBrowserEventProbe(ctx context.Context, t *testing.T) {
	t.Helper()
	cluster := strings.TrimSpace(os.Getenv(envCluster))
	if cluster == "" {
		t.Skipf("%s is not set: the probe image is loaded into the instance's kind cluster", envCluster)
	}
	for _, bin := range []string{"docker", "kind", "go"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is required to build the probe", bin)
		}
	}
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..")
	dir := filepath.Join(root, "testdata", "eventprobe")
	build := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", filepath.Join(dir, "eventprobe"), "./testdata/eventprobe")
	build.Dir = root
	build.Env = append(os.Environ(), "GOOS=linux", "CGO_ENABLED=0")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("compile the event probe: %v\n%s", err, b)
	}
	t.Cleanup(func() { _ = os.Remove(filepath.Join(dir, "eventprobe")) })
	if b, err := exec.CommandContext(ctx, "docker", "build", "-t", browserEventProbe, dir).CombinedOutput(); err != nil {
		t.Fatalf("build the event probe image: %v\n%s", err, b)
	}
	if b, err := exec.CommandContext(ctx, "kind", "load", "docker-image", browserEventProbe, "--name", cluster).CombinedOutput(); err != nil {
		t.Fatalf("load the event probe: %v\n%s", err, b)
	}
}
