package iameval

import (
	"context"
	"slices"
	"strings"
	"testing"

	_ "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	_ "cloud.google.com/go/eventarc/apiv1/eventarcpb"
	_ "cloud.google.com/go/functions/apiv1/functionspb"
	_ "cloud.google.com/go/functions/apiv2/functionspb"
	_ "cloud.google.com/go/logging/apiv2/loggingpb"
	_ "cloud.google.com/go/longrunning/autogen/longrunningpb"
	_ "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	_ "cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	_ "cloud.google.com/go/monitoring/dashboard/apiv1/dashboardpb"
	_ "cloud.google.com/go/redis/apiv1/redispb"
	_ "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"cloud.google.com/go/run/apiv2/runpb"
	"cloud.google.com/go/scheduler/apiv1/schedulerpb"
	_ "cloud.google.com/go/workflows/apiv1/workflowspb"
	_ "cloud.google.com/go/workflows/executions/apiv1/executionspb"
	_ "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/iamroles"
)

// TestRoundFiveTables (#1879): every row of the Logging, Monitoring,
// Memorystore, Cloud Run and Cloud Functions tables, and every actAs
// row, names a method its gRPC service has, request fields it has, and
// permissions Google's index names, so a typo cannot deny what no role
// can grant or check a field that is never set.
func TestRoundFiveTables(t *testing.T) {
	names := iamroles.Names()
	for _, svc := range []*Service{Logging, Monitoring, Redis, RedisCluster, Valkey, Run, Functions, Eventarc, Scheduler, Workflows, Tasks} {
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
			paths := append(strings.Split(m.Field, "|"), m.ActAs...)
			for _, p := range paths {
				if !hasPath(md.Input(), strings.Split(p, ".")) {
					t.Errorf("%s: request has no string field %s", full, p)
				}
			}
			if m.Permission == "" && m.PermissionFor == nil && len(m.ActAs) == 0 {
				t.Errorf("%s checks nothing", full)
			}
			if m.Permission != "" {
				if _, ok := slices.BinarySearch(names, m.Permission); !ok {
					t.Errorf("%s: %s is not in Google's permission index", full, m.Permission)
				}
			}
		}
	}
	if _, ok := slices.BinarySearch(names, ActAs); !ok {
		t.Errorf("%s is not in Google's permission index", ActAs)
	}
}

func hasPath(md protoreflect.MessageDescriptor, parts []string) bool {
	fd := md.Fields().ByName(protoreflect.Name(parts[0]))
	if fd == nil {
		return false
	}
	if len(parts) == 1 {
		return fd.Kind() == protoreflect.StringKind
	}
	return fd.Kind() == protoreflect.MessageKind && hasPath(fd.Message(), parts[1:])
}

// actAsSource grants iam.serviceAccounts.actAs to user on one account, by
// the account's own policy.
type actAsSource struct {
	fakeSource
	user, account string
}

func (s actAsSource) Policy(resource string) *iampolicy.Stored {
	if resource == "projects/p/serviceAccounts/"+s.account {
		return &iampolicy.Stored{Bindings: []iampolicy.Binding{{Role: "roles/iam.serviceAccountUser", Members: []string{"serviceAccount:" + s.user}}}}
	}
	return nil
}

// TestActAs (#1879): a Cloud Run service whose template names a service
// account the caller may not act as is refused with Cloud Run's words,
// one it may act as (by the account's own policy, read through the IAM
// policy reader) is served, and none is served; a Scheduler job's OIDC
// token account is checked with Scheduler's.
func TestActAs(t *testing.T) {
	const allowed = "ok@p.iam.gserviceaccount.com"
	src := actAsSource{user: sa, account: allowed}
	e := &Enforcer{Eval: &Evaluator{Source: src}, On: func() bool { return true },
		CallerOf: func(string) Caller { return Caller{Email: sa} },
		Policies: map[string]PolicyGetter{IAM.Name: func(_ context.Context, r string) *iampolicy.Stored { return src.Policy(r) }}}
	ctx := incomingBearer("x")
	run := func(i grpc.UnaryServerInterceptor, full string, req any) error {
		_, err := i(ctx, req, &grpc.UnaryServerInfo{FullMethod: full}, func(context.Context, any) (any, error) { return "ok", nil })
		return err
	}
	svc := func(account string) *runpb.CreateServiceRequest {
		return &runpb.CreateServiceRequest{Parent: "projects/p/locations/us-central1",
			Service: &runpb.Service{Template: &runpb.RevisionTemplate{ServiceAccount: account}}}
	}
	const create = "/google.cloud.run.v2.Services/CreateService"
	err := run(e.Interceptor(Run), create, svc("other@p.iam.gserviceaccount.com"))
	if st, _ := status.FromError(err); st.Code() != codes.PermissionDenied ||
		st.Message() != "Permission 'iam.serviceaccounts.actAs' denied on service account other@p.iam.gserviceaccount.com" {
		t.Fatalf("CreateService without actAs = %v", err)
	}
	if err := run(e.Interceptor(Run), create, svc(allowed)); err != nil {
		t.Fatalf("CreateService with actAs on the account = %v", err)
	}
	if err := run(e.Interceptor(Run), create, svc("")); err != nil {
		t.Fatalf("CreateService with no account = %v", err)
	}
	job := &schedulerpb.CreateJobRequest{Parent: "projects/p/locations/us-central1", Job: &schedulerpb.Job{
		Target: &schedulerpb.Job_HttpTarget{HttpTarget: &schedulerpb.HttpTarget{AuthorizationHeader: &schedulerpb.HttpTarget_OidcToken{
			OidcToken: &schedulerpb.OidcToken{ServiceAccountEmail: "other@p.iam.gserviceaccount.com"}}}}}}
	// The job's own permission first: the caller is granted it nowhere.
	err = run(e.Interceptor(Scheduler), "/google.cloud.scheduler.v1.CloudScheduler/CreateJob", job)
	if st, _ := status.FromError(err); st.Code() != codes.PermissionDenied || !strings.Contains(st.Message(), "cloudscheduler.jobs.create") {
		t.Fatalf("CreateJob = %v", err)
	}
	if got := ServiceAccountResource("q", "123-compute@developer.gserviceaccount.com"); got != "projects/q/serviceAccounts/123-compute@developer.gserviceaccount.com" {
		t.Errorf("default compute account's resource = %s", got)
	}
	if got := ServiceAccountResource("q", "a@p.iam.gserviceaccount.com"); got != "projects/p/serviceAccounts/a@p.iam.gserviceaccount.com" {
		t.Errorf("an account's resource = %s", got)
	}
	if got := jsonActAs([]byte(`{"template":{"serviceAccount":"a@p.iam.gserviceaccount.com"}}`), "service",
		[]string{"service.template.service_account"}); len(got) != 1 || got[0] != "a@p.iam.gserviceaccount.com" {
		t.Errorf("jsonActAs = %v", got)
	}
	if got := jsonResources(nil, []byte(`{"resourceNames":["projects/a","projects/b"]}`), "*", "resource_names"); len(got) != 2 || got[1] != "projects/b" {
		t.Errorf("jsonResources of a repeated field = %v", got)
	}
}

func incomingBearer(token string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+token))
}

// TestCloudSQLJSON (#1879): every Cloud SQL Admin permission is in
// Google's index, and requests match their method on the instance or
// project, under both prefixes.
func TestCloudSQLJSON(t *testing.T) {
	names := iamroles.Names()
	for _, m := range CloudSQLJSON {
		if _, ok := slices.BinarySearch(names, m.Permission); !ok {
			t.Errorf("%s is not in Google's permission index", m.Permission)
		}
	}
	for _, c := range []struct{ method, path, perm, resource string }{
		{"GET", "/v1/projects/p/instances", "cloudsql.instances.list", "projects/p"},
		{"POST", "/sql/v1beta4/projects/p/instances", "cloudsql.instances.create", "projects/p"},
		{"GET", "/v1/projects/p/instances/i", "cloudsql.instances.get", "projects/p/instances/i"},
		{"POST", "/v1/projects/p/instances/i/restart", "cloudsql.instances.restart", "projects/p/instances/i"},
		{"GET", "/v1/projects/p/instances/i/databases/d", "cloudsql.databases.get", "projects/p/instances/i"},
		{"DELETE", "/v1/projects/p/instances/i/users", "cloudsql.users.delete", "projects/p/instances/i"},
		{"POST", "/v1/projects/p/instances/i:generateEphemeralCert", "cloudsql.instances.connect", "projects/p/instances/i"},
		{"GET", "/v1/projects/p/operations/o", "cloudsql.instances.get", "projects/p"},
	} {
		perm, res, ok := MatchJSON(CloudSQLJSON, c.method, c.path)
		if !ok || perm != c.perm || res != c.resource {
			t.Errorf("%s %s = %s %s %v; want %s %s", c.method, c.path, perm, res, ok, c.perm, c.resource)
		}
	}
	if _, _, ok := MatchJSON(CloudSQLJSON, "GET", "/v1/flags"); ok {
		t.Error("flags.list matched")
	}
}
