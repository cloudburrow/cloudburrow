package iameval

import (
	"context"
	"slices"
	"strings"
	"testing"

	_ "cloud.google.com/go/eventarc/publishing/apiv1/publishingpb"
	_ "cloud.google.com/go/iam/admin/apiv1/adminpb"
	_ "cloud.google.com/go/iam/apiv2/iampb"
	_ "cloud.google.com/go/orgpolicy/apiv2/orgpolicypb"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/iamroles"
)

// TestRoundElevenTables (#1879): every row of the Resource Manager, IAM,
// deny policy, Organization Policy and Eventarc tables names a method its
// gRPC service has, request fields it has, and permissions Google's index
// names; every method those services serve has a row, but the ones whose
// pages name no permission.
func TestRoundElevenTables(t *testing.T) {
	names := iamroles.Names()
	unchecked := map[string]bool{
		"/google.iam.admin.v1.IAM/QueryGrantableRoles": true, "/google.iam.admin.v1.IAM/QueryTestablePermissions": true,
		"/google.iam.admin.v1.IAM/QueryAuditableServices": true, "/google.iam.admin.v1.IAM/LintPolicy": true,
		"/google.iam.admin.v1.IAM/TestIamPermissions": true,
	}
	for name, svc := range Services {
		if svc != ResourceManager && svc != IAM && svc != DenyPolicies && svc != OrgPolicy && svc != Eventarc {
			continue
		}
		d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(name))
		if err != nil {
			t.Errorf("no service %s", name)
			continue
		}
		ms := d.(protoreflect.ServiceDescriptor).Methods()
		for i := 0; i < ms.Len(); i++ {
			full := "/" + name + "/" + string(ms.Get(i).Name())
			if _, ok := svc.Methods[full]; !ok && !unchecked[full] && !strings.HasSuffix(full, "/TestIamPermissions") {
				t.Errorf("%s has no row", full)
			}
		}
	}
	for _, svc := range []*Service{ResourceManager, IAM, DenyPolicies, OrgPolicy, Eventarc} {
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
			var paths []string
			if m.Field != "" {
				paths = append(paths, m.Field)
			}
			paths = append(paths, m.ActAs...)
			for _, a := range m.Also {
				paths = append(paths, a.Field)
				if _, ok := slices.BinarySearch(names, a.Permission); !ok {
					t.Errorf("%s: %s is not in Google's permission index", full, a.Permission)
				}
			}
			for _, p := range paths {
				if !hasPath(md.Input(), strings.Split(p, ".")) {
					t.Errorf("%s: request has no string field %s", full, p)
				}
			}
			if m.Filter != "" {
				fd := md.Output().Fields().ByName(protoreflect.Name(m.Filter))
				if fd == nil || !fd.IsList() {
					t.Errorf("%s: response has no list %s", full, m.Filter)
				}
			}
			if m.Permission == "" && m.PermissionFor == nil && m.Extra == nil && len(m.Also) == 0 {
				t.Errorf("%s checks nothing", full)
			}
			if m.Permission != "" {
				if _, ok := slices.BinarySearch(names, m.Permission); !ok {
					t.Errorf("%s: %s is not in Google's permission index", full, m.Permission)
				}
			}
		}
	}
	for _, p := range []string{"resourcemanager.tagValueBindings.delete", "resourcemanager.hierarchyNodes.deleteTagBinding",
		"resourcemanager.hierarchyNodes.listTagBindings", "resourcemanager.hierarchyNodes.listEffectiveTags",
		"resourcemanager.hierarchyNodes.createTagBinding", "resourcemanager.folders.move", "iam.roles.get"} {
		if _, ok := slices.BinarySearch(names, p); !ok {
			t.Errorf("%s is not in Google's permission index", p)
		}
	}
}

// TestRoundElevenNames: a tag binding's name and a deny policy's
// attachment point are read as Google writes them.
func TestRoundElevenNames(t *testing.T) {
	got := tagBindingChecks("tagBindings/%2F%2Fcloudresourcemanager.googleapis.com%2Fprojects%2F123/tagValues/456", "v", "n")
	want := []Check{{"v", "tagValues/456"}, {"n", "projects/123"}}
	if !slices.Equal(got, want) {
		t.Errorf("tagBindingChecks = %v, want %v", got, want)
	}
	got = tagBindingChecks("tagBindings/%2F%2Fstorage.googleapis.com%2Fprojects%2F_%2Fbuckets%2Fb/tagValues/456", "v", "n")
	if !slices.Equal(got, []Check{{"v", "tagValues/456"}}) {
		t.Errorf("a bucket's binding = %v", got)
	}
	if a := DenyAttachment("policies/cloudresourcemanager.googleapis.com%2Fprojects%2Fp/denypolicies/d"); a != "projects/p" {
		t.Errorf("DenyAttachment = %q", a)
	}
	if a := DenyAttachment("policies/cloudresourcemanager.googleapis.com%2Forganizations%2F1/denypolicies"); a != "organizations/1" {
		t.Errorf("DenyAttachment = %q", a)
	}
	if customRole("iam.roles.get")("roles/viewer") != "" || customRole("iam.roles.get")("projects/p/roles/r") != "iam.roles.get" {
		t.Error("customRole")
	}
}

// TestRoundElevenEnforcer: a search keeps only what the caller may get;
// a Scope's parent's ancestry applies to a tag value; the owner passes
// any check no deny policy refuses, its project policy's bindings or not.
func TestRoundElevenEnforcer(t *testing.T) {
	const user = "u@p.iam.gserviceaccount.com"
	src := fakeSource{policies: map[string]*iampolicy.Stored{
		"projects/a":      pol(iampolicy.Binding{Role: "roles/browser", Members: []string{"serviceAccount:" + user}}),
		"organizations/1": pol(iampolicy.Binding{Role: "roles/resourcemanager.tagViewer", Members: []string{"serviceAccount:" + user}}),
	}}
	e := &Enforcer{Eval: &Evaluator{Source: hierSource{src}}, On: func() bool { return true },
		CallerOf: func(b string) Caller {
			if b == "owner" {
				return Caller{Email: "owner@p.iam.gserviceaccount.com", Owner: true}
			}
			return Caller{Email: user}
		},
		Scopes: map[string]func(string) Scope{ResourceManager.Name: func(r string) Scope {
			if r == "tagValues/9" {
				return Scope{Own: []string{"tagValues/9", "tagKeys/8"}, Parent: "organizations/1"}
			}
			return Scope{}
		}},
	}
	icpt := e.Interceptor(ResourceManager)
	ctx := incomingBearer("user")
	search := &grpc.UnaryServerInfo{FullMethod: "/google.cloud.resourcemanager.v3.Projects/SearchProjects"}
	resp, err := icpt(ctx, &rmpb.SearchProjectsRequest{}, search, func(context.Context, any) (any, error) {
		return &rmpb.SearchProjectsResponse{Projects: []*rmpb.Project{{Name: "projects/a"}, {Name: "projects/b"}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if ps := resp.(*rmpb.SearchProjectsResponse).GetProjects(); len(ps) != 1 || ps[0].GetName() != "projects/a" {
		t.Errorf("search kept %v", ps)
	}
	ok := func(context.Context, any) (any, error) { return &rmpb.TagValue{}, nil }
	get := &grpc.UnaryServerInfo{FullMethod: "/google.cloud.resourcemanager.v3.TagValues/GetTagValue"}
	if _, err := icpt(ctx, &rmpb.GetTagValueRequest{Name: "tagValues/9"}, get, ok); err != nil {
		t.Errorf("tag viewer on the organization: %v", err)
	}
	set := &grpc.UnaryServerInfo{FullMethod: "/google.cloud.resourcemanager.v3.Projects/SetIamPolicy"}
	if _, err := icpt(ctx, &rmpb.GetProjectRequest{}, set, ok); err == nil {
		t.Error("a request without its field passed")
	}
	getP := &grpc.UnaryServerInfo{FullMethod: "/google.cloud.resourcemanager.v3.Projects/GetProject"}
	if _, err := icpt(ctx, &rmpb.GetProjectRequest{Name: "projects/b"}, getP, ok); status.Code(err) != codes.PermissionDenied ||
		!strings.Contains(status.Convert(err).Message(), "//cloudresourcemanager.googleapis.com/projects/b") {
		t.Errorf("GetProject b: %v", err)
	}
	if _, err := icpt(incomingBearer("owner"), &rmpb.GetProjectRequest{Name: "projects/b"}, getP, ok); err != nil {
		t.Errorf("owner: %v", err)
	}
	create := &grpc.UnaryServerInfo{FullMethod: "/google.cloud.resourcemanager.v3.Projects/CreateProject"}
	if _, err := icpt(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: "n"}}, create, ok); err != nil {
		t.Errorf("a project under no parent: %v", err)
	}
	if _, err := icpt(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{Parent: "organizations/1"}}, create, ok); status.Code(err) != codes.PermissionDenied {
		t.Errorf("a project under the organization: %v", err)
	}
}

// hierSource is a fakeSource that knows the organization's ancestry.
type hierSource struct{ fakeSource }

func (h hierSource) HierarchyAncestry(root string) []string {
	if root == "organizations/1" {
		return []string{root}
	}
	return nil
}
