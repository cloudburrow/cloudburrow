//go:build compat

package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fv1 "cloud.google.com/go/functions/apiv1/functionspb"
	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// consoleCreateFunction posts Create function through the console's action
// upload route, as the form does: the action's request, then the zip.
func consoleCreateFunction(ctx context.Context, t *testing.T, addr, project string, values map[string]string, data []byte) (int, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	w, _ := mw.CreateFormField("request")
	_ = json.NewEncoder(w).Encode(map[string]any{"Path": []string{}, "Action": "create-function", "Values": values})
	part, _ := mw.CreateFormFile("file", "function.zip")
	_, _ = part.Write(data)
	_ = mw.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+addr+"/api/actions/functions/upload?project="+url.QueryEscape(project), &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// consoleFunctionPage is a function's page: its summary and actions.
func consoleFunctionPage(t *testing.T, addr, project, id string) (map[string]string, []string) {
	t.Helper()
	q := url.Values{"project": {project}, "name": {id}}
	code, body := consoleDo(t, addr, http.MethodGet, "/api/detail/functions?"+q.Encode(), "")
	if code != http.StatusOK {
		t.Fatalf("console detail %s = %d: %s", id, code, body)
	}
	var page struct {
		Unavailable string
		Summary     []struct{ Label, Value string }
		Actions     []struct{ ID string }
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("decode detail: %v: %s", err, body)
	}
	if page.Unavailable != "" {
		t.Fatalf("console detail %s is unavailable: %s", id, page.Unavailable)
	}
	props := map[string]string{}
	for _, p := range page.Summary {
		props[p.Label] = p.Value
	}
	var acts []string
	for _, a := range page.Actions {
		acts = append(acts, a.ID)
	}
	return props, acts
}

// covers: google.cloud.functions.v1.CloudFunctionsService/GenerateUploadUrl, google.cloud.functions.v1.CloudFunctionsService/CreateFunction, google.cloud.functions.v1.CloudFunctionsService/GetFunction, google.cloud.functions.v1.CloudFunctionsService/CallFunction, google.cloud.functions.v2.FunctionService/DeleteFunction
//
// TestConsoleFunctionsV1 (#1428): the console's Create function with
// Environment 1st gen, checked through the official Cloud Functions v1
// client. Ungated: a source that is not a zip, with an HTTP trigger, a
// Pub/Sub topic and a Cloud Storage event, each fails with this instance's
// build error and is a 1st-gen function the v1 client reads OFFLINE with
// the trigger the form chose and its memory; the page shows the v1
// resource, offers Test function on the HTTP one only, and Test function
// is refused while it is not ACTIVE; Delete removes each. Gated as the
// Functions tests are: testdata/function's Hello created as 1st gen HTTP
// becomes ACTIVE, and Test function answers its greeting through call.
func TestConsoleFunctionsV1(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	storageAddr := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	pubsubAddr := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_PUBSUB", EnvPubSub)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	t.Cleanup(cancel)
	project := h.Project()
	parent := runParent(h)
	c := functionsV1Client(t, h)

	t.Setenv("PUBSUB_EMULATOR_HOST", pubsubAddr)
	pc, err := pubsub.NewClient(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	tn := "projects/" + project + "/topics/console-v1-events"
	if _, err := pc.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: tn}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	t.Cleanup(func() {
		_ = pc.TopicAdminClient.DeleteTopic(context.Background(), &pubsubpb.DeleteTopicRequest{Topic: tn})
	})
	sc, err := storage.NewClient(ctx, option.WithoutAuthentication(), option.WithEndpoint(storageAddr+"/storage/v1/"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	bh := bucket(t, h, sc)

	del := func(id string) (int, string) {
		return consoleDo(t, addr, http.MethodDelete, "/api/resources/functions?project="+url.QueryEscape(project)+"&name="+id, "")
	}
	for _, cs := range []struct {
		id     string
		values map[string]string
		check  func(*fv1.CloudFunction) bool
	}{
		{"console-v1-http", map[string]string{"trigger": "HTTP"}, func(fn *fv1.CloudFunction) bool {
			return strings.HasSuffix(fn.GetHttpsTrigger().GetUrl(), "/console-v1-http")
		}},
		{"console-v1-topic", map[string]string{"trigger": "Cloud Pub/Sub", "topic": "console-v1-events"}, func(fn *fv1.CloudFunction) bool {
			et := fn.GetEventTrigger()
			return et.GetEventType() == "google.pubsub.topic.publish" && et.GetResource() == tn
		}},
		{"console-v1-bucket", map[string]string{"trigger": "Cloud Storage", "bucket": bh.BucketName(), "storageEvent": "google.storage.object.finalize"},
			func(fn *fv1.CloudFunction) bool {
				et := fn.GetEventTrigger()
				return et.GetEventType() == "google.storage.object.finalize" && et.GetResource() == "projects/_/buckets/"+bh.BucketName()
			}},
	} {
		name := parent + "/functions/" + cs.id
		t.Cleanup(func() { _, _ = del(cs.id) })
		values := map[string]string{"environment": "1st gen", "name": cs.id, "region": "us-central1", "runtime": "go123",
			"entryPoint": "Hello", "memory": "512M"}
		for k, v := range cs.values {
			values[k] = v
		}
		code, out := consoleCreateFunction(ctx, t, addr, project, values, []byte("not a zip"))
		if code == http.StatusOK || !strings.Contains(out, "Build failed") {
			t.Errorf("console Create function %s of a source that is not a zip = %d %s; want this instance's build error", cs.id, code, out)
		}
		fn, err := c.GetFunction(ctx, &fv1.GetFunctionRequest{Name: name})
		if err != nil {
			t.Fatalf("v1 GetFunction %s: %v", cs.id, err)
		}
		if fn.GetStatus() != fv1.CloudFunctionStatus_OFFLINE || fn.GetRuntime() != "go123" || fn.GetEntryPoint() != "Hello" ||
			fn.GetAvailableMemoryMb() != 512 || fn.GetSourceUploadUrl() == "" || !cs.check(fn) {
			t.Errorf("v1 GetFunction %s = %v; want it OFFLINE with the form's trigger, runtime and memory", cs.id, fn)
		}
		props, acts := consoleFunctionPage(t, addr, project, cs.id)
		if props["Environment"] != "1st gen" || props["Status"] != "OFFLINE" || props["Memory allocated"] != "512 MB" ||
			props["Source upload URL"] != fn.GetSourceUploadUrl() {
			t.Errorf("the console's page of %s = %v; want the v1 resource", cs.id, props)
		}
		offered := strings.Join(acts, ",") == "test-function"
		if cs.id == "console-v1-http" {
			if !offered || props["URL"] != fn.GetHttpsTrigger().GetUrl() {
				t.Errorf("the HTTP function's page = %v with actions %v; want its URL and Test function", props, acts)
			}
			code, out := consoleAct(t, addr, "functions", project, []string{cs.id}, "test-function", map[string]string{"data": "{}"})
			if code == http.StatusOK || !strings.Contains(out, "not ACTIVE") {
				t.Errorf("Test function of an OFFLINE function = %d %s; want call's refusal", code, out)
			}
		} else if offered || props["Trigger resource"] != fn.GetEventTrigger().GetResource() {
			t.Errorf("the event function's page = %v with actions %v; want its trigger and no Test function", props, acts)
		}
	}
	code, out := consoleDo(t, addr, http.MethodGet, "/api/resources/functions?project="+url.QueryEscape(project), "")
	var list struct {
		Items []struct {
			Name   string
			Fields map[string]string
		}
	}
	_ = json.Unmarshal([]byte(out), &list)
	listed := map[string]string{}
	for _, it := range list.Items {
		if it.Fields["Environment"] == "1st gen" {
			listed[it.Name] = it.Fields["Trigger"]
		}
	}
	if code != http.StatusOK || len(listed) != 3 || listed["console-v1-http"] != "HTTP" ||
		listed["console-v1-topic"] != "google.pubsub.topic.publish" || listed["console-v1-bucket"] != "google.storage.object.finalize" {
		t.Errorf("the console's list = %d %v; want the three 1st-gen functions with their triggers", code, listed)
	}
	for _, id := range []string{"console-v1-http", "console-v1-topic", "console-v1-bucket"} {
		if code, out := del(id); code/100 != 2 {
			t.Errorf("console Delete %s = %d %s", id, code, out)
		}
		if _, err := c.GetFunction(ctx, &fv1.GetFunctionRequest{Name: parent + "/functions/" + id}); status.Code(err) != codes.NotFound {
			t.Errorf("v1 GetFunction after the console's Delete = %v; want NOT_FOUND", err)
		}
	}

	requireAmd64Functions(ctx, t)
	id := "console-v1-built"
	t.Cleanup(func() { _, _ = del(id) })
	code, out = consoleCreateFunction(ctx, t, addr, project, map[string]string{"environment": "1st gen", "name": id, "region": "us-central1",
		"trigger": "HTTP", "runtime": "go123", "entryPoint": "Hello", "memory": "256M"},
		zipFunction(t, filepath.Join(moduleRoot(t), "testdata", "function")))
	if code != http.StatusOK {
		t.Fatalf("console Create function %s = %d %s", id, code, out)
	}
	fn, err := c.GetFunction(ctx, &fv1.GetFunctionRequest{Name: parent + "/functions/" + id})
	if err != nil || fn.GetStatus() != fv1.CloudFunctionStatus_ACTIVE || fn.GetHttpsTrigger().GetUrl() == "" {
		t.Fatalf("v1 GetFunction after the console's create = %v, %v; want it ACTIVE with its url", fn, err)
	}
	code, out = consoleAct(t, addr, "functions", project, []string{id}, "test-function", map[string]string{"data": `{"name":"console"}`})
	if code != http.StatusOK || !strings.Contains(out, "hello console") {
		t.Errorf("Test function = %d %s; want the greeting", code, out)
	}
	if code, out := del(id); code/100 != 2 {
		t.Errorf("console Delete %s = %d %s", id, code, out)
	}
}
