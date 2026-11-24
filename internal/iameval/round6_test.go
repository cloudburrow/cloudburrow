package iameval

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	_ "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	_ "cloud.google.com/go/cloudbuild/apiv2/cloudbuildpb"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/cloudburrow/cloudburrow/internal/iamcheck"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/iamroles"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// hierarchySource is fakeSource with folders' and billing accounts'
// ancestry.
type hierarchySource struct{ fakeSource }

func (h hierarchySource) HierarchyAncestry(root string) []string {
	switch root {
	case "folders/2":
		return []string{"folders/2", "folders/1", "organizations/9"}
	case "organizations/9":
		return []string{"organizations/9"}
	case "billingAccounts/AAAAAA-BBBBBB-CCCCCC":
		return []string{root, "organizations/9"}
	}
	return nil
}

// TestLoggingHierarchy (#1879): a Logging call on a folder's, the
// organization's or a billing account's resource is checked against that
// root's policy and its ancestors', not only the instance's own account.
func TestLoggingHierarchy(t *testing.T) {
	viewer := iampolicy.Binding{Role: "roles/logging.viewer", Members: []string{"serviceAccount:" + sa}}
	src := hierarchySource{fakeSource{policies: map[string]*iampolicy.Stored{}}}
	e := &Enforcer{Eval: &Evaluator{Source: src}, On: func() bool { return true }, CallerOf: func(string) Caller { return Caller{Email: sa} }}
	ctx := incomingBearer("x")
	call := func(full string, req any) error {
		_, err := e.Interceptor(Logging)(ctx, req, &grpc.UnaryServerInfo{FullMethod: full}, func(context.Context, any) (any, error) { return "ok", nil })
		return err
	}
	const list = "/google.logging.v2.ConfigServiceV2/ListSinks"
	for _, parent := range []string{"folders/2", "organizations/9", "billingAccounts/AAAAAA-BBBBBB-CCCCCC"} {
		if err := call(list, &loggingpb.ListSinksRequest{Parent: parent}); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("ListSinks on %s without a role = %v", parent, err)
		}
	}
	// Logs Viewer on a folder reaches its sinks, not the organization's.
	src.policies["folders/1"] = pol(viewer)
	if err := call(list, &loggingpb.ListSinksRequest{Parent: "folders/2"}); err != nil {
		t.Fatalf("ListSinks on a folder with Logs Viewer on its parent = %v", err)
	}
	if err := call(list, &loggingpb.ListSinksRequest{Parent: "organizations/9"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("ListSinks on the organization with Logs Viewer on a folder = %v", err)
	}
	// On the organization it reaches its billing accounts' too.
	src.policies["organizations/9"] = pol(viewer)
	for _, parent := range []string{"organizations/9", "billingAccounts/AAAAAA-BBBBBB-CCCCCC"} {
		if err := call(list, &loggingpb.ListSinksRequest{Parent: parent}); err != nil {
			t.Fatalf("ListSinks on %s with Logs Viewer on the organization = %v", parent, err)
		}
	}
	// A write needs its own permission still.
	if err := call("/google.logging.v2.ConfigServiceV2/CreateSink", &loggingpb.CreateSinkRequest{Parent: "folders/2"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("CreateSink with Logs Viewer = %v", err)
	}
	if got := HierarchyRoot("projects/p/sinks/s"); got != "" {
		t.Errorf("HierarchyRoot of a project's sink = %q", got)
	}
}

// TestSinkCustomWriterActAs (#1879): sinks.create over JSON with
// customWriterIdentity needs iam.serviceAccounts.actAs on that account.
func TestSinkCustomWriterActAs(t *testing.T) {
	const allowed = "ok@p.iam.gserviceaccount.com"
	writer := iampolicy.Binding{Role: "roles/logging.configWriter", Members: []string{"serviceAccount:" + sa}}
	src := actAsSource{fakeSource: fakeSource{policies: map[string]*iampolicy.Stored{"projects/p": pol(writer)}}, user: sa, account: allowed}
	e := &Enforcer{Eval: &Evaluator{Source: src}, On: func() bool { return true },
		CallerOf: func(string) Caller { return Caller{Email: sa} },
		Policies: map[string]PolicyGetter{IAM.Name: func(_ context.Context, r string) *iampolicy.Stored { return src.Policy(r) }}}
	h := e.HTTP(Logging, loggingMatcher(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	post := func(account string) int {
		r := httptest.NewRequest(http.MethodPost, "/v2/projects/p/sinks?customWriterIdentity=serviceAccount:"+account,
			strings.NewReader(`{"name":"s","destination":"logging.googleapis.com/projects/q/locations/global/buckets/b"}`))
		r.Header.Set("Authorization", "Bearer x")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if got := post("other@p.iam.gserviceaccount.com"); got != http.StatusForbidden {
		t.Fatalf("sinks.create with a writer the caller may not act as = %d", got)
	}
	if got := post(allowed); got != http.StatusOK {
		t.Fatalf("sinks.create with a writer the caller may act as = %d", got)
	}
}

// TestRoundSixTables (#1879): every row of the sixth round's tables names
// a method its gRPC service has, request fields it has, and permissions
// Google's index names; RunJob's overrides permission too.
func TestRoundSixTables(t *testing.T) {
	names := iamroles.Names()
	for _, svc := range []*Service{Run, Functions, ArtifactRegistry, CloudBuild} {
		for full, m := range svc.Methods {
			service, method, _ := strings.Cut(strings.TrimPrefix(full, "/"), "/")
			d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(service))
			if err != nil {
				t.Errorf("%s: no service %s", full, service)
				continue
			}
			md := d.(protoreflect.ServiceDescriptor).Methods().ByName(protoreflect.Name(method))
			if md == nil {
				t.Errorf("%s: no such method", full)
				continue
			}
			for _, p := range append(strings.Split(m.Field, "|"), m.ActAs...) {
				if !hasPath(md.Input(), strings.Split(p, ".")) {
					t.Errorf("%s: request has no string field %s", full, p)
				}
			}
			perms := []string{m.Permission, m.IfSetPermission}
			if m.PermissionFor != nil {
				perms = append(perms, m.PermissionFor("projects/p/locations/l/functions/f"))
			}
			for _, p := range perms {
				if p == "" {
					continue
				}
				if _, ok := slices.BinarySearch(names, p); !ok {
					t.Errorf("%s: %s is not in Google's permission index", full, p)
				}
			}
			if m.Permission == "" && m.PermissionFor == nil {
				t.Errorf("%s checks no permission of its own", full)
			}
		}
	}
	// Cloud Build's project_id is the project's resource.
	if got := fieldValues((&cloudbuildpb.GetBuildRequest{ProjectId: "p", Id: "b"}).ProtoReflect(), "name|project_id"); got[0] != "projects/p" {
		t.Errorf("GetBuild's resource = %v", got)
	}
	// TestIamPermissions is answered by its own API's table alone.
	if Run.answersTestIAM("/google.devtools.artifactregistry.v1.ArtifactRegistry/TestIamPermissions") ||
		!ArtifactRegistry.answersTestIAM("/google.devtools.artifactregistry.v1.ArtifactRegistry/TestIamPermissions") ||
		!Functions.answersTestIAM("/google.iam.v1.IAMPolicy/TestIamPermissions") || Run.answersTestIAM("/google.iam.v1.IAMPolicy/TestIamPermissions") {
		t.Error("answersTestIAM answers another API's TestIamPermissions")
	}
}

// TestRunJobOverrides (#1879): RunJob with overrides also needs
// run.jobs.runWithOverrides, which Cloud Run Invoker lacks.
func TestRunJobOverrides(t *testing.T) {
	invoker := iampolicy.Binding{Role: "roles/run.invoker", Members: []string{"serviceAccount:" + sa}}
	src := fakeSource{policies: map[string]*iampolicy.Stored{"projects/p": pol(invoker)}}
	e := &Enforcer{Eval: &Evaluator{Source: src}, On: func() bool { return true }, CallerOf: func(string) Caller { return Caller{Email: sa} }}
	run := func(req *runpb.RunJobRequest) error {
		_, err := e.Interceptor(Run)(incomingBearer("x"), req, &grpc.UnaryServerInfo{FullMethod: "/google.cloud.run.v2.Jobs/RunJob"},
			func(context.Context, any) (any, error) { return "ok", nil })
		return err
	}
	const job = "projects/p/locations/l/jobs/j"
	if err := run(&runpb.RunJobRequest{Name: job}); err != nil {
		t.Fatalf("RunJob as an invoker = %v", err)
	}
	err := run(&runpb.RunJobRequest{Name: job, Overrides: &runpb.RunJobRequest_Overrides{TaskCount: 2}})
	if st, _ := status.FromError(err); st.Code() != codes.PermissionDenied || !strings.Contains(st.Message(), RunJobOverrides) {
		t.Fatalf("RunJob with overrides as an invoker = %v", err)
	}
}

// TestRemoteActAs (#1879): an in-cluster server's actAs check (a Pub/Sub
// push subscription's OIDC token account) is decided on the account by its
// own policy, which the host keeps.
func TestRemoteActAs(t *testing.T) {
	const allowed = "ok@p.iam.gserviceaccount.com"
	src := actAsSource{user: sa, account: allowed}
	e := &Enforcer{Eval: &Evaluator{Source: src}, On: func() bool { return true },
		CallerOf: func(string) Caller { return Caller{Email: sa} },
		Policies: map[string]PolicyGetter{IAM.Name: func(_ context.Context, r string) *iampolicy.Stored { return src.Policy(r) }}}
	if d := e.CheckRemote(iamcheck.Request{Bearer: "x", Project: "p", ActAs: allowed}); !d.Holds(ActAs) {
		t.Fatalf("actAs on the account the caller may act as = %+v", d)
	}
	if d := e.CheckRemote(iamcheck.Request{Bearer: "x", Project: "p", ActAs: "other@p.iam.gserviceaccount.com"}); d.Holds(ActAs) {
		t.Fatalf("actAs on another account = %+v", d)
	}
}

func loggingMatcher() *rest.Matcher {
	return rest.NewMatcher([]string{"google.logging.v2.LoggingServiceV2", "google.logging.v2.ConfigServiceV2", "google.logging.v2.MetricsServiceV2"}, nil)
}
