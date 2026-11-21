package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	"google.golang.org/grpc"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// memObjects is an in-memory Cloud Storage for a function's archives.
type memObjects struct {
	mu   sync.Mutex
	objs map[string][]byte
}

func (m *memObjects) Fetch(_ context.Context, bucket, object string, _ int64) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[bucket+"/"+object]
	if !ok {
		return nil, apierror.NotFound("no object %s/%s", bucket, object)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (m *memObjects) Put(_ context.Context, _, bucket, object string, data []byte, _ bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[bucket+"/"+object] = data
	return nil
}

// noTransport takes every trigger's transport and publishes nothing.
type noTransport struct{}

func (noTransport) CreateTopic(context.Context, string, map[string]string) error { return nil }
func (noTransport) DeleteTopic(context.Context, string) error                    { return nil }
func (noTransport) CreatePushSubscription(context.Context, runadapter.PushSubscription) error {
	return nil
}
func (noTransport) DeleteSubscription(context.Context, string) error { return nil }
func (noTransport) CreateNotification(context.Context, string, string, string) (string, error) {
	return "1", nil
}
func (noTransport) DeleteNotification(context.Context, string, string) error { return nil }
func (noTransport) Publish(context.Context, string, []byte, map[string]string) error {
	return nil
}

func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for n, body := range files {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func fnSection(d console.Detail, id string) (console.Section, bool) {
	for _, s := range d.Sections {
		if s.ID == id {
			return s, true
		}
	}
	return console.Section{}, false
}

// TestConsoleFunctionsParity (#1853): the functions list has Google's
// columns and Create function opens Cloud Run's Write a function; a
// function's page has Google's tabs in Google's order; the Source tab
// reads the archive's files and Save and redeploy sends a new archive
// with UpdateFunction; Copy function creates the copy from the same
// files; a 2nd-gen function takes a Cloud Firestore or Cloud Audit Logs
// trigger, and Edit trigger changes it; Testing shows the command.
func TestConsoleFunctionsParity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	objs := &memObjects{objs: map[string][]byte{}}
	s := runadapter.NewServer(&runadapter.Knative{}, "t", time.Minute)
	var fs *runadapter.FunctionsServer
	files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fs.SourceHandler(http.NotFoundHandler()).ServeHTTP(w, r)
	}))
	defer files.Close()
	fs = s.Functions(s.Builds(objs, failingBuilder{}), func() string { return files.URL }).
		WithEvents(runadapter.Events{Transport: noTransport{}, PushBase: func() string { return files.URL }})
	srv := grpc.NewServer()
	fs.Register(srv)
	runadapter.NewOperationsServer(s).Register(srv)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	p := functionsProvider{defaultProject: "console-parity-fn", runAddr: func() string { return ln.Addr().String() }, storage: true}

	src := map[string]string{"index.js": "exports.hello = (req, res) => res.send('v1');\n", "package.json": "{}\n"}
	create := func(id, env string, extra map[string]string) {
		t.Helper()
		v := map[string]string{"name": id, "environment": env, "region": "us-central1", "runtime": "nodejs22", "entryPoint": "hello", "memory": "512M"}
		for k, x := range extra {
			v[k] = x
		}
		_, err := p.ActAtFile(ctx, "", nil, functionsCreateAction, v, console.UploadedFile{Name: "src.zip", Reader: bytes.NewReader(zipOf(t, src))})
		if err == nil || !strings.Contains(err.Error(), "no runtime detected") {
			t.Fatalf("create %s = %v; want the build's error", id, err)
		}
		t.Cleanup(func() { _ = p.Delete(context.Background(), "", id) })
	}
	create("par-gen2", functionGen2, nil)
	create("par-gen1", functionGen1, nil)

	l, err := p.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(l.Columns, ",") != "Environment,Region,Trigger,Runtime,Memory allocated,Executed function,Last deployed,Deployed by,Authentication,URL" ||
		l.NameColumn != "Name" || l.NameAfter != "Environment" {
		t.Errorf("the list's columns = %v (%s)", l.Columns, l.NameColumn)
	}
	for _, it := range l.Items {
		if it.Fields["Memory allocated"] != "512 MiB" || it.Fields["Executed function"] != "hello" || it.Fields["Trigger"] != "HTTP" ||
			it.Fields["Deployed by"] == "" || strings.Contains(it.Fields["Deployed by"], "API") {
			t.Errorf("%s's row = %v", it.Name, it.Fields)
		}
	}

	for _, id := range []string{"par-gen2", "par-gen1"} {
		d, err := p.Detail(ctx, "", []string{id})
		if err != nil || d.Unavailable != "" {
			t.Fatalf("Detail(%s) = %+v, %v", id, d, err)
		}
		var ids []string
		for _, sec := range d.Sections {
			ids = append(ids, sec.ID)
		}
		if strings.Join(ids, ",") != "metrics,details,source,variables,triggers,permissions,logs,testing" {
			t.Errorf("%s's tabs = %v; want Google's", id, ids)
		}
		srcTab, _ := fnSection(d, "source")
		acts := srcTab.Actions
		if id == "par-gen2" {
			// Base image's Edit runtime and security updates (#2049), a
			// 2nd gen function's.
			if len(acts) != 4 || acts[1].Label != "Edit runtime and security updates" ||
				!strings.Contains(srcTab.Note, "Base image: us-central1-docker.pkg.dev/serverless-runtimes/") {
				t.Errorf("%s's Base image = %q, %v", id, srcTab.Note, actionIDs(acts))
			} else {
				acts = append(acts[:1:1], acts[2:]...)
			}
		}
		if len(srcTab.Tabs) != 2 || srcTab.Tabs[0].Label != "index.js" || !strings.Contains(srcTab.Tabs[0].Text, "'v1'") ||
			len(acts) != 3 || acts[0].Submit != "Save and redeploy" ||
			acts[1].Label != "Show Payload" || acts[2].Label != "Test" {
			t.Errorf("%s's Source tab = %+v", id, srcTab)
		}
		trig, _ := fnSection(d, "triggers")
		if trig.Listing.Total < 1 || trig.Listing.Items[0].Name != "HTTP" {
			t.Errorf("%s's Triggers tab = %+v", id, trig.Listing)
		}
		testing, _ := fnSection(d, "testing")
		want := "gcloud functions call par-gen1"
		if id == "par-gen2" {
			want = "no URL yet"
		}
		if !strings.Contains(testing.Text, want) || len(testing.Actions) != 2 || testing.Actions[0].ID != functionsTestAction ||
			testing.Actions[1].Label != "Run in Cloud Shell" || testing.Actions[1].Terminal == "" {
			t.Errorf("%s's Testing tab = %+v; want %q", id, testing, want)
		}
	}

	// Show Payload and the Source tab's Test are offered and answer.
	pay, err := p.ActAtResult(ctx, "", []string{"par-gen2"}, functionsShowPayloadAction, nil)
	if err != nil || pay == nil || pay.Text != functionTestBody {
		t.Errorf("Show Payload on an HTTP function = %+v, %v", pay, err)
	}
	if _, err := p.ActAtResult(ctx, "", []string{"par-gen2"}, functionsSourceTestAction, map[string]string{"data": "{}"}); err == nil ||
		!strings.Contains(err.Error(), "no URL") {
		t.Errorf("Test on the Source tab = %v; want the Testing tab's answer for an undeployed function", err)
	}
	offered := map[string]bool{}
	for _, a := range p.DetailActions(ctx, "", []string{"par-gen2"}) {
		offered[a.ID] = true
	}
	if !offered[functionsShowPayloadAction] || !offered[functionsSourceTestAction] {
		t.Errorf("DetailActions = %v; want Show Payload and Test offered", offered)
	}

	// Allow public access on a 1st gen function: allUsers is granted
	// roles/cloudfunctions.invoker through the v1 API, and the list says so.
	if err := p.allowPublicInvocation(ctx, "projects/console-parity-fn/locations/us-central1/functions/par-gen1", true); err != nil {
		t.Fatalf("Allow public access: %v", err)
	}
	l, _ = p.List(ctx, "")
	for _, it := range l.Items {
		if it.Name == "par-gen1" && it.Fields["Authentication"] != runAllowPublic {
			t.Errorf("par-gen1's Authentication = %q; want %q", it.Fields["Authentication"], runAllowPublic)
		}
	}

	// Save and redeploy: the edited files are the function's new source.
	edited, _ := json.Marshal(map[string]string{"index.js": "exports.hello = (req, res) => res.send('v2');\n", "package.json": "{}\n"})
	before, _ := p.find("", "par-gen2")
	if _, err := p.ActAtResult(ctx, "", []string{"par-gen2"}, functionsEditSourceAction,
		map[string]string{"runtime": "nodejs22", "entryPoint": "hello2", runSourceFilesField: string(edited)}); err != nil {
		t.Fatalf("Save and redeploy: %v", err)
	}
	after, _ := p.find("", "par-gen2")
	if after.GetBuildConfig().GetSource().GetStorageSource().GetObject() == before.GetBuildConfig().GetSource().GetStorageSource().GetObject() ||
		after.GetBuildConfig().GetEntryPoint() != "hello2" {
		t.Errorf("after Save and redeploy the build config = %v", after.GetBuildConfig())
	}
	data, err := p.functionArchive(ctx, after)
	if err != nil {
		t.Fatal(err)
	}
	if got, _, _ := archiveFiles(data); !strings.Contains(got["index.js"], "'v2'") {
		t.Errorf("the redeployed source = %v", got)
	}

	// Copy function: the copy has the function's files.
	waitIdle := func(id string) {
		t.Helper()
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if fn, err := p.find("", id); err == nil && fn.GetState() != functionspb.Function_DEPLOYING {
				return
			}
		}
		t.Fatalf("%s is still deploying", id)
	}
	waitIdle("par-gen1")
	if _, err := p.ActAtResult(ctx, "", []string{"par-gen1"}, functionsCopyAction, map[string]string{"name": "par-gen1-copy", "region": "us-central1"}); err != nil {
		t.Fatalf("Copy function: %v", err)
	}
	t.Cleanup(func() { _ = p.Delete(context.Background(), "", "par-gen1-copy") })
	cp, err := p.find("", "par-gen1-copy")
	if err != nil || cp.GetEnvironment() != functionspb.Environment_GEN_1 || cp.GetBuildConfig().GetEntryPoint() != "hello" {
		t.Fatalf("the copy = %v, %v", cp, err)
	}

	// Write a function's settings reach the function's service config.
	create("par-set", functionGen2, map[string]string{"minInstances": "1", "maxInstances": "3", "timeout": "120", "concurrency": "8",
		"cpu": "2", "env": `{"MODE":"set"}`, "serviceAccount": "fn@console-parity-fn.iam.gserviceaccount.com", "billing": runBillingRequest, "ingress": runIngressAll})
	if fn, _ := p.find("", "par-set"); fn.GetServiceConfig().GetMinInstanceCount() != 1 || fn.GetServiceConfig().GetMaxInstanceCount() != 3 ||
		fn.GetServiceConfig().GetTimeoutSeconds() != 120 || fn.GetServiceConfig().GetMaxInstanceRequestConcurrency() != 8 ||
		fn.GetServiceConfig().GetAvailableCpu() != "2" || fn.GetServiceConfig().GetEnvironmentVariables()["MODE"] != "set" ||
		fn.GetServiceConfig().GetServiceAccountEmail() != "fn@console-parity-fn.iam.gserviceaccount.com" {
		t.Errorf("the settings reached the service config as %v", fn.GetServiceConfig())
	}

	// A Cloud Firestore trigger, then Edit trigger to Cloud Audit Logs.
	create("par-fs", functionGen2, map[string]string{"trigger": functionTriggerFirestore, "firestoreEvent": "created", "document": "users/{u}"})
	fn, _ := p.find("", "par-fs")
	if et := fn.GetEventTrigger(); et.GetEventType() != "google.cloud.firestore.document.v1.created" || len(et.GetEventFilters()) != 2 ||
		et.GetEventFilters()[1].GetOperator() != functionPathPatternOp {
		t.Errorf("the Firestore trigger = %v", et)
	}
	waitIdle("par-fs")
	if _, err := p.ActAtResult(ctx, "", []string{"par-fs"}, functionsEditTriggerAction, map[string]string{"trigger": functionTriggerAudit,
		"serviceName": "secretmanager.googleapis.com", "methodName": "google.cloud.secretmanager.v1.SecretManagerService.CreateSecret"}); err != nil {
		t.Fatalf("Edit trigger: %v", err)
	}
	fn, _ = p.find("", "par-fs")
	if et := fn.GetEventTrigger(); et.GetEventType() != functionAuditEvent || len(et.GetEventFilters()) != 2 {
		t.Errorf("after Edit trigger the trigger = %v", et)
	}
	if err := func() error {
		_, err := p.ActAtFile(ctx, "", nil, functionsCreateAction, map[string]string{"name": "par-bad", "environment": functionGen1,
			"trigger": functionTriggerAudit, "serviceName": "s", "methodName": "m", "runtime": "nodejs22", "entryPoint": "h"},
			console.UploadedFile{Name: "src.zip", Reader: bytes.NewReader(zipOf(t, src))})
		return err
	}(); err == nil || !strings.Contains(err.Error(), "choose 2nd gen") {
		t.Errorf("a 1st gen audit-log function = %v; want it refused", err)
	}
	if v := functionTriggerValues(fn.GetEventTrigger()); v["trigger"] != functionTriggerAudit || v["serviceName"] != "secretmanager.googleapis.com" {
		t.Errorf("Edit trigger's prefill = %v", v)
	}
}
