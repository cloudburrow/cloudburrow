package run

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
)

// restServer is the Cloud Run JSON API over the fake kubectl, with what
// cmd/cloudburrow registers: Services, Revisions, Jobs, Executions and
// Operations.
func restServer(t *testing.T) *httptest.Server {
	t.Helper()
	s := NewServer(&Knative{Kubeconfig: "k", Namespace: "default", Runner: newFakeKube()}, "inst", 5*time.Second)
	ops := NewOperationsServer(s)
	srv := httptest.NewServer(NewRESTHandler(func(g grpc.ServiceRegistrar) {
		s.Register(g)
		s.Revisions().Register(g)
		s.Jobs().Register(g)
		s.Executions().Register(g)
		ops.Register(g)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func call(t *testing.T, srv *httptest.Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("%s %s: the response is not JSON: %v", method, path, err)
	}
	return resp.StatusCode, out
}

// Google's bindings are the routes: every Run v2 service's, and the four
// Operations bindings of run_v2.yaml.
func TestRESTRoutesAreGooglesBindings(t *testing.T) {
	t.Parallel()
	have := map[string]bool{}
	for _, r := range Routes() {
		have[r.Method+" "+r.Pattern+" "+r.RPC] = true
	}
	for _, want := range []string{
		"POST /v2/{parent=projects/*/locations/*}/services Services/CreateService",
		"GET /v2/{name=projects/*/locations/*/services/*} Services/GetService",
		"PATCH /v2/{service.name=projects/*/locations/*/services/*} Services/UpdateService",
		"DELETE /v2/{name=projects/*/locations/*/services/*} Services/DeleteService",
		"GET /v2/{parent=projects/*/locations/*/services/*}/revisions Revisions/ListRevisions",
		"POST /v2/{parent=projects/*/locations/*}/jobs Jobs/CreateJob",
		"POST /v2/{name=projects/*/locations/*/jobs/*}:run Jobs/RunJob",
		"GET /v2/{parent=projects/*/locations/*/jobs/*}/executions Executions/ListExecutions",
		"GET /v2/{name=projects/*/locations/*/operations/*} Operations/GetOperation",
		"DELETE /v2/{name=projects/*/locations/*/operations/*} Operations/DeleteOperation",
		"GET /v2/{name=projects/*/locations/*}/operations Operations/ListOperations",
		"POST /v2/{name=projects/*/locations/*/operations/*}:wait Operations/WaitOperation",
	} {
		if !have[want] {
			t.Errorf("no route %s", want)
		}
	}
}

// A job goes through its whole lifecycle over JSON, as Terraform's
// google_cloud_run_v2_job drives it: create (an operation, polled by name),
// read, a PATCH replacing it, delete, and a 404 after.
func TestRESTJobLifecycle(t *testing.T) {
	t.Parallel()
	srv := restServer(t)
	base := "/v2/" + jobParent
	body := `{"labels":{"env":"local"},"template":{"template":{"containers":[{"image":"docker.io/library/busybox:1.36","env":[{"name":"MODE","value":"a"}]}]}}}`

	// validateOnly is a request field, so a query parameter.
	code, op := call(t, srv, http.MethodPost, base+"/jobs?jobId=j&validateOnly=true", body)
	if code != http.StatusOK || op["done"] != true {
		t.Fatalf("validate-only create = %d %v", code, op)
	}
	if code, got := call(t, srv, http.MethodGet, base+"/jobs/j", ""); code != http.StatusNotFound {
		t.Fatalf("after a validate-only create, GET = %d %v; want 404", code, got)
	}

	code, op = call(t, srv, http.MethodPost, base+"/jobs?jobId=j", body)
	name, _ := op["name"].(string)
	if code != http.StatusOK || !strings.HasPrefix(name, jobParent+"/operations/") {
		t.Fatalf("create = %d %v", code, op)
	}
	code, op = call(t, srv, http.MethodGet, "/v2/"+name, "")
	if code != http.StatusOK || op["done"] != true {
		t.Fatalf("GET %s = %d %v", name, code, op)
	}
	if resp, _ := op["response"].(map[string]any); resp["@type"] != "type.googleapis.com/google.cloud.run.v2.Job" {
		t.Errorf("the operation's response = %v", op["response"])
	}

	code, job := call(t, srv, http.MethodGet, base+"/jobs/j", "")
	if code != http.StatusOK || job["name"] != jobParent+"/jobs/j" || job["labels"].(map[string]any)["env"] != "local" {
		t.Fatalf("GET job = %d %v", code, job)
	}

	upd := strings.Replace(strings.Replace(body, `"a"`, `"b"`, 1), `"local"`, `"changed"`, 1)
	if code, op := call(t, srv, http.MethodPatch, base+"/jobs/j", upd); code != http.StatusOK {
		t.Fatalf("PATCH = %d %v", code, op)
	}
	_, job = call(t, srv, http.MethodGet, base+"/jobs/j", "")
	env := job["template"].(map[string]any)["template"].(map[string]any)["containers"].([]any)[0].(map[string]any)["env"].([]any)[0]
	if env.(map[string]any)["value"] != "b" || job["labels"].(map[string]any)["env"] != "changed" {
		t.Errorf("after PATCH, env %v labels %v", env, job["labels"])
	}

	if code, op := call(t, srv, http.MethodDelete, base+"/jobs/j", ""); code != http.StatusOK {
		t.Fatalf("DELETE = %d %v", code, op)
	}
	code, got := call(t, srv, http.MethodGet, base+"/jobs/j", "")
	if e, _ := got["error"].(map[string]any); code != http.StatusNotFound || e["status"] != "NOT_FOUND" {
		t.Errorf("GET after DELETE = %d %v; want 404 NOT_FOUND", code, got)
	}
}

// What is bound but not served answers 501; what Google does not bind, 404;
// an unknown query parameter stays INVALID_ARGUMENT.
func TestRESTUnservedPaths(t *testing.T) {
	t.Parallel()
	srv := restServer(t)
	for _, c := range []struct {
		method, path string
		code         int
	}{
		{http.MethodGet, "/v2/" + jobParent + "/workerPools", http.StatusNotImplemented},
		{http.MethodGet, "/v2/" + jobParent + "/jobs/j/executions/e/tasks", http.StatusNotImplemented},
		{http.MethodGet, "/v2/" + jobParent + "/operations", http.StatusNotImplemented},
		{http.MethodPost, "/v2/" + jobParent + "/operations/op-1:wait", http.StatusNotImplemented},
		{http.MethodGet, "/v2/" + jobParent, http.StatusNotFound},
		{http.MethodGet, "/v1/" + jobParent + "/services", http.StatusNotFound},
		{http.MethodGet, "/v2/" + jobParent + "/services?bogus=1", http.StatusBadRequest},
	} {
		if code, got := call(t, srv, c.method, c.path, ""); code != c.code {
			t.Errorf("%s %s = %d %v; want %d", c.method, c.path, code, got, c.code)
		}
	}
}
