package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	"google.golang.org/grpc"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

type failingBuilder struct{}

func (failingBuilder) Build(context.Context, runadapter.BuildSpec) error {
	return errors.New("no runtime detected")
}

// TestConsoleFunctionsPage (#1377): the Functions page lists a function the
// API created, with its state, region and runtime; its page shows the
// runtime, entry point, source and why it failed; Create function is
// offered only with Cloud Storage; Delete goes through DeleteFunction.
func TestConsoleFunctionsPage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := runadapter.NewServer(&runadapter.Knative{}, "t", time.Minute)
	fs := s.Functions(s.Builds(stubSource{}, failingBuilder{}), func() string { return "http://127.0.0.1:1" })
	srv := grpc.NewServer()
	fs.Register(srv)
	runadapter.NewOperationsServer(s).Register(srv)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Stop()

	parent := "projects/console-fn/locations/us-central1"
	if _, err := fs.CreateFunction(ctx, &functionspb.CreateFunctionRequest{Parent: parent, FunctionId: "page-fn", Function: &functionspb.Function{
		BuildConfig: &functionspb.BuildConfig{Runtime: "nodejs22", EntryPoint: "hello",
			Source: &functionspb.Source{Source: &functionspb.Source_StorageSource{StorageSource: &functionspb.StorageSource{Bucket: "b", Object: "o.zip"}}}}}}); err != nil {
		t.Fatal(err)
	}
	p := functionsProvider{defaultProject: "console-fn", runAddr: func() string { return ln.Addr().String() }, storage: true}
	deadline := time.Now().Add(20 * time.Second)
	for {
		l, _ := p.List(ctx, "")
		if len(l.Items) == 1 && l.Items[0].Status == "error" {
			if it := l.Items[0]; it.Name != "page-fn" || it.Fields["Region"] != "us-central1" || it.Fields["Runtime"] != "nodejs22" {
				t.Errorf("the listed function = %+v", it)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the function never failed its build: %+v", l.Items)
		}
		time.Sleep(50 * time.Millisecond)
	}
	d, err := p.Detail(ctx, "", []string{"page-fn"})
	if err != nil {
		t.Fatal(err)
	}
	props := map[string]string{}
	for _, pr := range d.Summary {
		props[pr.Label] = pr.Value
	}
	if props["State"] != "FAILED" || props["Entry point"] != "hello" || props["Source"] != "gs://b/o.zip" ||
		!strings.Contains(props["Status detail"], "no runtime detected") || props["Memory"] != "256M" {
		t.Errorf("the function's page = %v", props)
	}
	if a := p.ListActions(ctx, ""); len(a) != 1 || a[0].ID != functionsCreateAction {
		t.Errorf("with Cloud Storage, ListActions = %+v; want Create function", a)
	}
	if a := (functionsProvider{}).ListActions(ctx, ""); len(a) != 0 {
		t.Errorf("without Cloud Storage, ListActions = %+v; want none", a)
	}
	if err := p.Delete(ctx, "", "page-fn"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if l, _ := p.List(ctx, ""); len(l.Items) != 0 {
		t.Errorf("after Delete, the list = %+v", l.Items)
	}
}

// TestConsoleFunctionsCreateGenerations (#1428): Create function's
// Environment choice. 1st gen goes through the v1 API with an HTTP, a
// Pub/Sub topic or a Cloud Storage trigger, and the function's page shows
// the v1 resource (its status, trigger, source upload URL and memory in
// MB); Test function is offered on a 1st-gen HTTP function only, and
// calls the v1 API, which refuses a function that is not ACTIVE. 2nd gen
// goes through the v2 API, its event trigger the CloudEvent one. Each
// build fails here, so each create reports the build's error.
func TestConsoleFunctionsCreateGenerations(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	uploads := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "want PUT", http.StatusMethodNotAllowed)
		}
	}))
	defer uploads.Close()
	s := runadapter.NewServer(&runadapter.Knative{}, "t", time.Minute)
	// Event triggers are accepted; each build fails before one is wired.
	fs := s.Functions(s.Builds(stubSource{}, failingBuilder{}), func() string { return uploads.URL }).WithEvents(runadapter.Events{})
	srv := grpc.NewServer()
	fs.Register(srv)
	runadapter.NewOperationsServer(s).Register(srv)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Stop()
	p := functionsProvider{defaultProject: "console-gen", runAddr: func() string { return ln.Addr().String() }, storage: true}

	a := p.createAction()
	fields := map[string]console.Field{}
	for _, f := range a.Fields {
		fields[f.Name] = f
	}
	if f := fields["environment"]; f.Default != "2nd gen" || strings.Join(f.Options, ",") != "2nd gen,1st gen" {
		t.Errorf("the Environment field = %+v", f)
	}
	if f := fields["trigger"]; strings.Join(f.Options, ",") != "HTTP,Cloud Pub/Sub,Cloud Storage" {
		t.Errorf("the Trigger type field = %+v", f)
	}

	create := func(values map[string]string) error {
		base := map[string]string{"region": "us-central1", "runtime": "go123", "entryPoint": "Hello", "memory": "512M"}
		for k, v := range values {
			base[k] = v
		}
		_, err := p.ActAtFile(ctx, "", nil, functionsCreateAction, base, console.UploadedFile{Name: "src.zip", Reader: strings.NewReader("not a zip")})
		return err
	}
	props := func(id string) map[string]string {
		t.Helper()
		d, err := p.Detail(ctx, "", []string{id})
		if err != nil || d.Unavailable != "" {
			t.Fatalf("Detail(%s) = %+v, %v", id, d, err)
		}
		out := map[string]string{}
		for _, pr := range d.Summary {
			out[pr.Label] = pr.Value
		}
		return out
	}
	for _, cs := range []struct {
		id      string
		values  map[string]string
		trigger string
		want    map[string]string
	}{
		{"gen1-http", map[string]string{"environment": "1st gen", "trigger": "HTTP"}, "HTTP",
			map[string]string{"Trigger": "HTTP", "URL": "https://us-central1-console-gen.cloudfunctions.net/gen1-http"}},
		{"gen1-topic", map[string]string{"environment": "1st gen", "trigger": "Cloud Pub/Sub", "topic": "orders"}, "google.pubsub.topic.publish",
			map[string]string{"Trigger": "google.pubsub.topic.publish", "Trigger resource": "projects/console-gen/topics/orders", "Retry on failure": "No"}},
		{"gen1-bucket", map[string]string{"environment": "1st gen", "trigger": "Cloud Storage", "bucket": "gs://uploads", "storageEvent": "google.storage.object.delete"},
			"google.storage.object.delete",
			map[string]string{"Trigger": "google.storage.object.delete", "Trigger resource": "projects/_/buckets/uploads"}},
	} {
		values := map[string]string{"name": cs.id}
		for k, v := range cs.values {
			values[k] = v
		}
		err := create(values)
		if err == nil || !strings.Contains(err.Error(), "no runtime detected") {
			t.Errorf("Create function %s = %v; want the build's error", cs.id, err)
		}
		l, _ := p.List(ctx, "")
		found := false
		for _, it := range l.Items {
			if it.Name == cs.id {
				found = true
				if it.Fields["Environment"] != "1st gen" || it.Fields["Trigger"] != cs.trigger {
					t.Errorf("the listed %s = %+v", cs.id, it)
				}
			}
		}
		if !found {
			t.Fatalf("%s is not listed: %+v", cs.id, l.Items)
		}
		got := props(cs.id)
		if got["Environment"] != "1st gen" || got["Status"] != "OFFLINE" || got["Memory allocated"] != "512 MB" ||
			!strings.HasPrefix(got["Source upload URL"], uploads.URL) || got["Runtime"] != "go123" {
			t.Errorf("%s's page = %v; want the v1 resource", cs.id, got)
		}
		for k, v := range cs.want {
			if got[k] != v {
				t.Errorf("%s's page %s = %q; want %q", cs.id, k, got[k], v)
			}
		}
		acts := p.DetailActions(ctx, "", []string{cs.id})
		if cs.id == "gen1-http" {
			if len(acts) != 1 || acts[0].ID != functionsTestAction {
				t.Errorf("a 1st-gen HTTP function's actions = %+v; want Test function", acts)
			}
			_, err := p.ActAtResult(ctx, "", []string{cs.id}, functionsTestAction, map[string]string{"data": `{"name":"x"}`})
			if err == nil || !strings.Contains(err.Error(), "not ACTIVE") {
				t.Errorf("Test function of an OFFLINE function = %v; want the v1 API's refusal", err)
			}
		} else if len(acts) != 0 {
			t.Errorf("an event function's actions = %+v; want none", acts)
		}
	}

	// 2nd gen: the v2 API, with the CloudEvent trigger.
	err = create(map[string]string{"name": "gen2-bucket", "trigger": "Cloud Storage", "bucket": "uploads"})
	if err == nil || !strings.Contains(err.Error(), "no runtime detected") {
		t.Errorf("Create function gen2-bucket = %v; want the build's error", err)
	}
	fn, err := p.find("", "gen2-bucket")
	if err != nil || fn.GetEnvironment() == functionspb.Environment_GEN_1 ||
		fn.GetEventTrigger().GetEventType() != "google.cloud.storage.object.v1.finalized" ||
		len(fn.GetEventTrigger().GetEventFilters()) != 1 || fn.GetEventTrigger().GetEventFilters()[0].GetValue() != "uploads" {
		t.Errorf("the 2nd-gen function = %v, %v", fn, err)
	}
	if acts := p.DetailActions(ctx, "", []string{"gen2-bucket"}); len(acts) != 0 {
		t.Errorf("a 2nd-gen function's actions = %+v; want none", acts)
	}

	for _, bad := range []map[string]string{
		{"name": "bad-topic", "environment": "1st gen", "trigger": "Cloud Pub/Sub"},
		{"name": "bad-bucket", "environment": "1st gen", "trigger": "Cloud Storage"},
		{"name": "bad-env", "environment": "3rd gen"},
		{"name": "bad-event", "environment": "1st gen", "trigger": "Cloud Storage", "bucket": "b", "storageEvent": "object.change"},
	} {
		if err := create(bad); err == nil || strings.Contains(err.Error(), "no runtime detected") {
			t.Errorf("Create function %v = %v; want it refused before any create", bad, err)
		}
		if _, err := p.find("", bad["name"]); err == nil {
			t.Errorf("%s was created", bad["name"])
		}
	}
	for _, id := range []string{"gen1-http", "gen1-topic", "gen1-bucket", "gen2-bucket"} {
		if err := p.Delete(ctx, "", id); err != nil {
			t.Errorf("Delete %s: %v", id, err)
		}
	}
}
