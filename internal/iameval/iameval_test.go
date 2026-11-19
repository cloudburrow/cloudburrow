package iameval

import (
	"context"
	"testing"
	"time"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
)

type fakeSource struct {
	ancestry map[string][]string
	policies map[string]*iampolicy.Stored
	custom   map[string][]string
	groups   map[string][]string
}

func (f fakeSource) Ancestry(p string) []string {
	if a, ok := f.ancestry[p]; ok {
		return a
	}
	return []string{"projects/" + p}
}
func (f fakeSource) Policy(r string) *iampolicy.Stored { return f.policies[r] }
func (f fakeSource) CustomRolePermissions(r string) ([]string, bool) {
	p, ok := f.custom[r]
	return p, ok
}
func (f fakeSource) Principals(e string) []string { return append([]string{Member(e)}, f.groups[e]...) }

func pol(bs ...iampolicy.Binding) *iampolicy.Stored { return &iampolicy.Stored{Bindings: bs} }

const sa = "worker@p.iam.gserviceaccount.com"

func check(src fakeSource, own []Attached, perm string, at time.Time) bool {
	e := &Evaluator{Source: src}
	return e.Check(Request{Email: sa, Permission: perm, Resource: "projects/p/secrets/s", Project: "p", Own: own, Time: at}).Granted
}

func TestCheck(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	access := "secretmanager.versions.access"
	accessor := iampolicy.Binding{Role: "roles/secretmanager.secretAccessor", Members: []string{"serviceAccount:" + sa}}
	src := fakeSource{ancestry: map[string][]string{"p": {"projects/p", "folders/1", "organizations/9"}}, policies: map[string]*iampolicy.Stored{}}
	if check(src, nil, access, now) {
		t.Fatal("granted with no policy")
	}
	// The resource's own policy.
	if !check(src, []Attached{{Resource: "//s/projects/p/secrets/s", Policy: pol(accessor)}}, access, now) {
		t.Fatal("own policy did not grant")
	}
	// Inherited from the organization.
	src.policies["organizations/9"] = pol(accessor)
	if !check(src, nil, access, now) {
		t.Fatal("organization policy did not grant")
	}
	delete(src.policies, "organizations/9")
	// A role without the permission.
	src.policies["projects/p"] = pol(iampolicy.Binding{Role: "roles/secretmanager.viewer", Members: []string{"serviceAccount:" + sa}})
	if check(src, nil, access, now) {
		t.Fatal("viewer granted access")
	}
	if !check(src, nil, "secretmanager.secrets.get", now) {
		t.Fatal("viewer did not grant get")
	}
	// A group grant.
	src.groups = map[string][]string{sa: {"group:team@example.com"}}
	src.policies["folders/1"] = pol(iampolicy.Binding{Role: "roles/secretmanager.secretAccessor", Members: []string{"group:team@example.com"}})
	if !check(src, nil, access, now) {
		t.Fatal("group grant did not grant")
	}
	delete(src.policies, "folders/1")
	// A condition on request.time.
	cond := accessor
	cond.Condition = &iampolicy.Condition{Title: "until", Expression: `request.time < timestamp("2026-11-20T00:00:00Z")`}
	src.policies["projects/p"] = pol(cond)
	if !check(src, nil, access, now) {
		t.Fatal("condition true did not grant")
	}
	if check(src, nil, access, now.Add(48*time.Hour)) {
		t.Fatal("condition false granted")
	}
	// Custom role, basic roles.
	src.custom = map[string][]string{"projects/p/roles/r": {access}}
	src.policies["projects/p"] = pol(iampolicy.Binding{Role: "projects/p/roles/r", Members: []string{"allAuthenticatedUsers"}})
	if !check(src, nil, access, now) {
		t.Fatal("custom role to allAuthenticatedUsers did not grant")
	}
	src.policies["projects/p"] = pol(iampolicy.Binding{Role: "roles/viewer", Members: []string{"serviceAccount:" + sa}})
	if check(src, nil, access, now) || !check(src, nil, "secretmanager.secrets.list", now) {
		t.Fatal("roles/viewer is read-only")
	}
}

func TestInterceptorDenies(t *testing.T) {
	on := true
	e := &Enforcer{Eval: &Evaluator{Source: fakeSource{}}, On: func() bool { return on },
		CallerOf: func(b string) Caller {
			switch b {
			case "adc":
				return Caller{Owner: true}
			case "sa":
				return Caller{Email: sa}
			}
			return Caller{Invalid: true}
		}}
	ic := e.Interceptor(SecretManager)
	info := &grpc.UnaryServerInfo{FullMethod: "/google.cloud.secretmanager.v1.SecretManagerService/AccessSecretVersion"}
	req := &secretmanagerpb.AccessSecretVersionRequest{Name: "projects/p/secrets/s/versions/1"}
	ran := false
	h := func(context.Context, any) (any, error) { ran = true; return nil, nil }
	call := func(tok string) error {
		ran = false
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+tok))
		_, err := ic(ctx, req, info, h)
		return err
	}
	err := call("sa")
	st, _ := status.FromError(err)
	if st.Code() != codes.PermissionDenied || ran ||
		st.Message() != "Permission 'secretmanager.versions.access' denied for resource 'projects/p/secrets/s/versions/1' (or it may not exist)." {
		t.Fatalf("denied: %v ran=%v", err, ran)
	}
	var info0 *errdetails.ErrorInfo
	for _, d := range st.Details() {
		if ei, ok := d.(*errdetails.ErrorInfo); ok {
			info0 = ei
		}
	}
	if info0 == nil || info0.GetReason() != "IAM_PERMISSION_DENIED" {
		t.Fatalf("ErrorInfo: %v", st.Details())
	}
	if err := call("adc"); err != nil || !ran {
		t.Fatalf("owner: %v", err)
	}
	if err := call("forged"); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("forged: %v", err)
	}
	on = false
	if err := call("sa"); err != nil || !ran {
		t.Fatalf("off: %v", err)
	}
}

func TestPolicyResources(t *testing.T) {
	got := KMS.PolicyResources("projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1")
	if len(got) != 2 || got[0] != "projects/p/locations/l/keyRings/r/cryptoKeys/k" || got[1] != "projects/p/locations/l/keyRings/r" {
		t.Fatal(got)
	}
	if KMS.Type("projects/p/locations/l/keyRings/r/cryptoKeys/k") != "cloudkms.googleapis.com/CryptoKey" {
		t.Fatal(KMS.Type("projects/p/locations/l/keyRings/r/cryptoKeys/k"))
	}
	if p := KMS.Methods["/google.iam.v1.IAMPolicy/SetIamPolicy"]; p.PermissionOf("projects/p/locations/l/keyRings/r/cryptoKeys/k") != "cloudkms.cryptoKeys.setIamPolicy" {
		t.Fatal("kms iam permission")
	}
}
