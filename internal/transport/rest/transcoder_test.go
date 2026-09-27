package rest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/scheduler/apiv1/schedulerpb"
	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	grpctransport "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
)

// The Transcoder is tested against Cloud Scheduler and long-running
// Operations, neither of which CloudBurrow serves over JSON yet, to show it
// holds nothing of Cloud KMS.

// fakeScheduler records the last request each method was given. RunJob and
// the rest are left to the Unimplemented embedding.
type fakeScheduler struct {
	schedulerpb.UnimplementedCloudSchedulerServer
	mu   sync.Mutex
	last proto.Message
}

func (f *fakeScheduler) got(m proto.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last = proto.Clone(m)
}

func (f *fakeScheduler) request() proto.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last
}

func (f *fakeScheduler) ListJobs(_ context.Context, r *schedulerpb.ListJobsRequest) (*schedulerpb.ListJobsResponse, error) {
	f.got(r)
	return &schedulerpb.ListJobsResponse{Jobs: []*schedulerpb.Job{{Name: r.GetParent() + "/jobs/j", State: schedulerpb.Job_ENABLED}}}, nil
}

func (f *fakeScheduler) GetJob(_ context.Context, r *schedulerpb.GetJobRequest) (*schedulerpb.Job, error) {
	f.got(r)
	if strings.HasSuffix(r.GetName(), "/absent") {
		return nil, apierror.NotFound("job %s not found", r.GetName())
	}
	return &schedulerpb.Job{Name: r.GetName(), State: schedulerpb.Job_PAUSED}, nil
}

func (f *fakeScheduler) CreateJob(_ context.Context, r *schedulerpb.CreateJobRequest) (*schedulerpb.Job, error) {
	f.got(r)
	return r.GetJob(), nil
}

func (f *fakeScheduler) UpdateJob(_ context.Context, r *schedulerpb.UpdateJobRequest) (*schedulerpb.Job, error) {
	f.got(r)
	return r.GetJob(), nil
}

func (f *fakeScheduler) PauseJob(_ context.Context, r *schedulerpb.PauseJobRequest) (*schedulerpb.Job, error) {
	f.got(r)
	return &schedulerpb.Job{Name: r.GetName(), State: schedulerpb.Job_PAUSED}, nil
}

type fakeOperations struct {
	longrunningpb.UnimplementedOperationsServer
	sched *fakeScheduler // shares its recorder
}

func (f *fakeOperations) GetOperation(_ context.Context, r *longrunningpb.GetOperationRequest) (*longrunningpb.Operation, error) {
	f.sched.got(r)
	return &longrunningpb.Operation{Name: r.GetName(), Done: true}, nil
}

func (f *fakeOperations) CancelOperation(_ context.Context, r *longrunningpb.CancelOperationRequest) (*emptypb.Empty, error) {
	f.sched.got(r)
	return &emptypb.Empty{}, nil
}

func (f *fakeOperations) ListOperations(_ context.Context, r *longrunningpb.ListOperationsRequest) (*longrunningpb.ListOperationsResponse, error) {
	f.sched.got(r)
	return &longrunningpb.ListOperationsResponse{}, nil
}

// setup serves Scheduler's and Operations' bindings, with Scheduler's
// Locations mixin and one to a method that exists nowhere.
func setup(t *testing.T, register bool, tweak func(*Transcoder)) (*httptest.Server, *fakeScheduler) {
	t.Helper()
	tr := &Transcoder{
		Packages: []string{"google.cloud.scheduler.v1", "google.longrunning"},
		Mixins: []*annotations.HttpRule{
			{Selector: "google.cloud.location.Locations.ListLocations", Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*}/locations"}},
			{Selector: "google.longrunning.Operations.GetOperation", Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*/locations/*/operations/*}"}},
			{Selector: "google.cloud.scheduler.v1.CloudScheduler.Nonexistent", Pattern: &annotations.HttpRule_Delete{Delete: "/v1/{name=projects/*/locations/*}"}},
		},
	}
	if tweak != nil {
		tweak(tr)
	}
	f := &fakeScheduler{}
	if register {
		schedulerpb.RegisterCloudSchedulerServer(tr, f)
		longrunningpb.RegisterOperationsServer(tr, &fakeOperations{sched: f})
	}
	srv := httptest.NewServer(tr)
	t.Cleanup(srv.Close)
	return srv, f
}

func call(t *testing.T, method, url, body string) (int, map[string]any, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s %s: not JSON: %s", method, url, raw)
	}
	return resp.StatusCode, m, string(raw)
}

// wantError checks an AIP-193 envelope: code, status and a message naming
// what went wrong.
func wantError(t *testing.T, what string, code int, m map[string]any, raw string, wantCode int, wantStatus, names string) {
	t.Helper()
	e, _ := m["error"].(map[string]any)
	msg, _ := e["message"].(string)
	if code != wantCode || e["code"] != float64(wantCode) || e["status"] != wantStatus || !strings.Contains(msg, names) {
		t.Errorf("%s = %d %s; want %d %s naming %q", what, code, raw, wantCode, wantStatus, names)
	}
}

// Routes lists every google.api.http binding of the packages, then the
// mixins in order.
func TestRoutesListEveryBinding(t *testing.T) {
	tr := &Transcoder{Packages: []string{"google.cloud.scheduler.v1"}}
	got := map[string]Route{}
	for _, r := range tr.Routes() {
		got[r.RPC] = r
	}
	var want []string
	protoregistry.GlobalFiles.RangeFilesByPackage("google.cloud.scheduler.v1", func(f protoreflect.FileDescriptor) bool {
		for i := 0; i < f.Services().Len(); i++ {
			sd := f.Services().Get(i)
			for j := 0; j < sd.Methods().Len(); j++ {
				if proto.HasExtension(sd.Methods().Get(j).Options(), annotations.E_Http) {
					want = append(want, string(sd.Name())+"/"+string(sd.Methods().Get(j).Name()))
				}
			}
		}
		return true
	})
	sort.Strings(want)
	if len(want) < 8 || len(got) != len(want) {
		t.Fatalf("routes %v; want one per annotated method %v", tr.Routes(), want)
	}
	for _, rpc := range want {
		if _, ok := got[rpc]; !ok {
			t.Errorf("no route for %s", rpc)
		}
	}
	for rpc, w := range map[string]Route{
		"CloudScheduler/CreateJob": {Method: "POST", Pattern: "/v1/{parent=projects/*/locations/*}/jobs", Body: "job"},
		"CloudScheduler/UpdateJob": {Method: "PATCH", Pattern: "/v1/{job.name=projects/*/locations/*/jobs/*}", Body: "job"},
		"CloudScheduler/PauseJob":  {Method: "POST", Pattern: "/v1/{name=projects/*/locations/*/jobs/*}:pause", Body: "*"},
	} {
		g := got[rpc]
		if g.Method != w.Method || g.Pattern != w.Pattern || g.Body != w.Body || g.Service != "google.cloud.scheduler.v1.CloudScheduler" {
			t.Errorf("%s = %+v; want %+v", rpc, g, w)
		}
	}

	mixed := &Transcoder{Packages: []string{"google.cloud.scheduler.v1"}, Mixins: []*annotations.HttpRule{
		{Selector: "google.cloud.location.Locations.GetLocation", Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*/locations/*}"}},
	}}
	rs := mixed.Routes()
	if last := rs[len(rs)-1]; len(rs) != len(want)+1 || last.RPC != "Locations/GetLocation" || last.Service != "google.cloud.location.Locations" {
		t.Errorf("the mixin is not last: %v", rs)
	}
}

// Status by path: a bound path reaches the registered server; a binding
// whose service is not registered, whose method the server leaves
// unimplemented, or whose method exists nowhere answers 501 naming it; a
// path nothing binds answers 404.
func TestStatusByPath(t *testing.T) {
	for _, register := range []bool{true, false} {
		srv, _ := setup(t, register, nil)
		const job = "/v1/projects/p/locations/l/jobs/j"
		get := 501
		if register {
			get = 200
		}
		for _, c := range []struct {
			method, path string
			code         int
			names        string
		}{
			{"GET", job, get, "CloudScheduler/GetJob"},
			{"POST", job + ":run", 501, "RunJob"},
			{"DELETE", job, 501, "DeleteJob"},
			{"GET", "/v1/projects/p/locations", 501, "Locations/ListLocations"},
			{"DELETE", "/v1/projects/p/locations/l", 501, "CloudScheduler/Nonexistent"},
			{"POST", job + ":nope", 404, ":nope"},
			{"PUT", job, 404, "PUT"},
			{"GET", job + "/extra", 404, "/extra"}, // * is one segment
			{"GET", "/v2/projects/p/locations/l/jobs/j", 404, "/v2/"},
		} {
			code, m, raw := call(t, c.method, srv.URL+c.path, "")
			switch c.code {
			case 200:
				if code != 200 || m["name"] != strings.TrimPrefix(c.path, "/v1/") {
					t.Errorf("%s %s = %d %s", c.method, c.path, code, raw)
				}
			case 501:
				wantError(t, c.method+" "+c.path, code, m, raw, 501, "UNIMPLEMENTED", c.names)
			case 404:
				wantError(t, c.method+" "+c.path, code, m, raw, 404, "NOT_FOUND", c.names)
			}
		}
	}
}

// Path variables: `*` is one segment and `**` any number; a variable may
// name a nested field; the value is unescaped; the verb is not part of it.
func TestPathVariables(t *testing.T) {
	srv, f := setup(t, true, nil)
	for _, c := range []struct{ method, path, want string }{
		{"GET", "/v1/operations/a", "operations/a"},
		{"GET", "/v1/operations/a/b/c", "operations/a/b/c"},
		{"POST", "/v1/operations/a/b:cancel", "operations/a/b"},
		{"GET", "/v1/projects/p/locations/l/operations/o", "projects/p/locations/l/operations/o"}, // a mixin
		{"GET", "/v1/projects/p/locations/l/jobs/j%20k", "projects/p/locations/l/jobs/j k"},
	} {
		code, _, raw := call(t, c.method, srv.URL+c.path, "")
		got, _ := f.request().ProtoReflect().Get(f.request().ProtoReflect().Descriptor().Fields().ByName("name")).Interface().(string)
		if code != 200 || got != c.want {
			t.Errorf("%s %s = %d %s, name %q; want %q", c.method, c.path, code, raw, got, c.want)
		}
	}
	// The server's own error comes back in the envelope, with its code.
	code, m, raw := call(t, "GET", srv.URL+"/v1/projects/p/locations/l/jobs/absent", "")
	wantError(t, "an absent job", code, m, raw, 404, "NOT_FOUND", "absent")
	// {name=operations} binds exactly that literal.
	if code, _, raw := call(t, "GET", srv.URL+"/v1/operations?filter=done", ""); code != 200 ||
		f.request().(*longrunningpb.ListOperationsRequest).GetName() != "operations" ||
		f.request().(*longrunningpb.ListOperationsRequest).GetFilter() != "done" {
		t.Errorf("ListOperations = %d %s %v", code, raw, f.request())
	}
}

// Query parameters bind the fields the path and body leave, in either
// spelling, with the system parameters and the refusals of query.go.
func TestQueryBinding(t *testing.T) {
	srv, f := setup(t, true, nil)
	jobs := srv.URL + "/v1/projects/p/locations/l/jobs"
	for _, q := range []string{"pageSize=5&pageToken=t", "page_size=5&page_token=t", "pageSize=5&page_token=t&alt=json&prettyPrint=false&$.xgafv=2"} {
		code, _, raw := call(t, "GET", jobs+"?"+q, "")
		r, _ := f.request().(*schedulerpb.ListJobsRequest)
		if code != 200 || r.GetPageSize() != 5 || r.GetPageToken() != "t" || r.GetParent() != "projects/p/locations/l" {
			t.Errorf("ListJobs?%s = %d %s, request %v", q, code, raw, r)
		}
	}
	// Enums by name, or by number when the GAPIC client asks, raw or escaped.
	if _, m, raw := call(t, "GET", jobs+"/j", ""); m["state"] != "PAUSED" {
		t.Errorf("state = %s; want by name", raw)
	}
	for _, alt := range []string{"$alt=json;enum-encoding=int", "%24alt=json%3Benum-encoding%3Dint"} {
		if _, m, raw := call(t, "GET", jobs+"/j?"+alt, ""); m["state"] != float64(schedulerpb.Job_PAUSED) {
			t.Errorf("state with %s = %s; want a number", alt, raw)
		}
	}
	// A FieldMask parameter is lowerCamelCase paths, handed on in proto form.
	code, _, raw := call(t, "PATCH", jobs+"/j?updateMask=retryConfig.retryCount,description", `{"description":"d"}`)
	u, _ := f.request().(*schedulerpb.UpdateJobRequest)
	if code != 200 || strings.Join(u.GetUpdateMask().GetPaths(), ",") != "retry_config.retry_count,description" ||
		u.GetJob().GetName() != "projects/p/locations/l/jobs/j" || u.GetJob().GetDescription() != "d" {
		t.Errorf("UpdateJob = %d %s, request %v", code, raw, u)
	}

	for _, c := range []struct {
		method, url string
		code        int
		status      string
		names       string
	}{
		{"GET", jobs + "?bogus=1", 400, "INVALID_ARGUMENT", "bogus"},
		{"GET", jobs + "/j?pageSize=1", 400, "INVALID_ARGUMENT", "pageSize"},     // GetJob has only name
		{"GET", jobs + "/j?name=other", 400, "INVALID_ARGUMENT", "name"},         // the path binds it
		{"PATCH", jobs + "/j?job.description=x", 400, "INVALID_ARGUMENT", "job"}, // the body holds it
		{"POST", jobs + "/j:pause?name=x", 400, "INVALID_ARGUMENT", "name"},      // body "*" leaves nothing
		{"GET", jobs + "?pageSize=two", 400, "INVALID_ARGUMENT", "pageSize"},
		{"GET", jobs + "?pageSize=1&pageSize=2", 400, "INVALID_ARGUMENT", "repeated"},
		{"GET", jobs + "?pageSize=1&page_size=2", 400, "INVALID_ARGUMENT", "repeated"},
		{"GET", jobs + "?pageSize=two&zzz=1", 400, "INVALID_ARGUMENT", "zzz"}, // names are checked first
		{"PATCH", jobs + "/j?updateMask=retry_config", 400, "INVALID_ARGUMENT", "updateMask"},
		{"GET", jobs + "?fields=jobs", 501, "UNIMPLEMENTED", "fields"},
		{"GET", jobs + "?$fields=jobs", 501, "UNIMPLEMENTED", "$fields"},
		{"GET", jobs + "?alt=proto", 501, "UNIMPLEMENTED", "alt"},
	} {
		code, m, raw := call(t, c.method, c.url, "")
		wantError(t, c.method+" "+c.url, code, m, raw, c.code, c.status, c.names)
	}
}

// Body "*" is the whole request; body "<field>" is that field, the rest of
// the request coming from the path and query. An empty body is an empty
// message. A malformed body is refused without quoting it.
func TestBodyMapping(t *testing.T) {
	srv, f := setup(t, true, nil)
	jobs := srv.URL + "/v1/projects/p/locations/l/jobs"

	code, m, raw := call(t, "POST", jobs, `{"name":"projects/p/locations/l/jobs/n","schedule":"* * * * *","httpTarget":{"uri":"http://x"}}`)
	c, _ := f.request().(*schedulerpb.CreateJobRequest)
	if code != 200 || c.GetParent() != "projects/p/locations/l" || c.GetJob().GetSchedule() != "* * * * *" ||
		c.GetJob().GetHttpTarget().GetUri() != "http://x" || m["schedule"] != "* * * * *" {
		t.Errorf("CreateJob = %d %s, request %v", code, raw, c)
	}
	if code, _, raw := call(t, "POST", jobs, ""); code != 200 || f.request().(*schedulerpb.CreateJobRequest).Job == nil {
		t.Errorf("CreateJob with no body = %d %s; want an empty job, not none", code, raw)
	}

	// Body "*", naming another resource: the path wins unless the method is
	// one of RefuseBodyConflicts.
	if code, _, raw := call(t, "POST", jobs+"/j:pause", `{"name":"projects/p/locations/l/jobs/other"}`); code != 200 ||
		f.request().(*schedulerpb.PauseJobRequest).GetName() != "projects/p/locations/l/jobs/j" {
		t.Errorf("PauseJob = %d %s, request %v", code, raw, f.request())
	}
	if code, _, raw := call(t, "POST", srv.URL+"/v1/operations/a/b:cancel", `{"name":"operations/z"}`); code != 200 ||
		f.request().(*longrunningpb.CancelOperationRequest).GetName() != "operations/a/b" {
		t.Errorf("CancelOperation = %d %s, request %v", code, raw, f.request())
	}
	strict, sf := setup(t, true, func(tr *Transcoder) {
		tr.RefuseBodyConflicts = []string{"google.cloud.scheduler.v1.CloudScheduler.PauseJob", "google.cloud.scheduler.v1.CloudScheduler.UpdateJob"}
		tr.MaxBodyBytes = 64
	})
	sjobs := strict.URL + "/v1/projects/p/locations/l/jobs"
	code, m, raw = call(t, "POST", sjobs+"/j:pause", `{"name":"projects/p/locations/l/jobs/other"}`)
	wantError(t, "a refused conflict", code, m, raw, 400, "INVALID_ARGUMENT", "differs")
	code, m, raw = call(t, "PATCH", sjobs+"/j", `{"name":"projects/p/locations/l/jobs/other"}`)
	wantError(t, "a refused nested conflict", code, m, raw, 400, "INVALID_ARGUMENT", "differs")
	if code, _, raw := call(t, "POST", sjobs+"/j:pause", `{"name":"projects/p/locations/l/jobs/j"}`); code != 200 ||
		sf.request().(*schedulerpb.PauseJobRequest).GetName() != "projects/p/locations/l/jobs/j" {
		t.Errorf("an agreeing name = %d %s", code, raw)
	}
	code, m, raw = call(t, "POST", sjobs, `{"description":"`+strings.Repeat("x", 64)+`"}`)
	wantError(t, "an oversized body", code, m, raw, 400, "INVALID_ARGUMENT", "64 bytes")

	for what, body := range map[string]string{
		"a syntax error":   `{"description": "SECRETMARKER`,
		"an unknown field": `{"SECRETMARKER":1}`,
		"a wrong type":     `{"description": ["SECRETMARKER"]}`,
	} {
		code, m, raw := call(t, "POST", jobs, body)
		wantError(t, what, code, m, raw, 400, "INVALID_ARGUMENT", "Job")
		if strings.Contains(raw, "SECRETMARKER") {
			t.Errorf("%s quoted the body: %s", what, raw)
		}
	}
}

// One port serves gRPC and JSON (h2c), both reaching the same server.
func TestSameListenerAsGRPC(t *testing.T) {
	f := &fakeScheduler{}
	tr := &Transcoder{Packages: []string{"google.cloud.scheduler.v1"}}
	schedulerpb.RegisterCloudSchedulerServer(tr, f)
	s := grpctransport.New("127.0.0.1:0")
	s.ServeHTTP(tr)
	if err := s.Register(func(g *grpc.Server) { schedulerpb.RegisterCloudSchedulerServer(g, f) }); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Stop(context.Background()) })

	conn, err := grpc.NewClient(s.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	j, err := schedulerpb.NewCloudSchedulerClient(conn).GetJob(context.Background(), &schedulerpb.GetJobRequest{Name: "projects/p/locations/l/jobs/g"})
	if err != nil || j.GetName() != "projects/p/locations/l/jobs/g" {
		t.Fatalf("gRPC GetJob = %v, %v", j, err)
	}
	code, m, raw := call(t, "GET", "http://"+s.Addr()+"/v1/projects/p/locations/l/jobs/h", "")
	if code != 200 || m["name"] != "projects/p/locations/l/jobs/h" {
		t.Errorf("JSON GetJob = %d %s", code, raw)
	}
}

// A mixin selector must name a method.
func TestMixinSelectorMustNameAMethod(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("a selector with no method did not panic")
		}
	}()
	(&Transcoder{Mixins: []*annotations.HttpRule{{Selector: "nomethod", Pattern: &annotations.HttpRule_Get{Get: "/v1/x"}}}}).Routes()
}
