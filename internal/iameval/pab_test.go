package iameval

import (
	"testing"
	"time"

	iamv3 "cloud.google.com/go/iam/apiv3/iampb"
	"google.golang.org/genproto/googleapis/type/expr"

	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
)

type pabSource struct {
	fakeSource
	bound map[string][]Bound
}

func (s pabSource) BoundaryBindings(set string) []Bound { return s.bound[set] }
func (s pabSource) BoundaryOrganization() string        { return "organizations/9" }

func pabPolicy(version string, resources ...string) *iamv3.PrincipalAccessBoundaryPolicy {
	return &iamv3.PrincipalAccessBoundaryPolicy{Name: "organizations/9/locations/global/principalAccessBoundaryPolicies/p",
		Details: &iamv3.PrincipalAccessBoundaryPolicyDetails{EnforcementVersion: version,
			Rules: []*iamv3.PrincipalAccessBoundaryPolicyRule{{Resources: resources, Effect: iamv3.PrincipalAccessBoundaryPolicyRule_ALLOW}}}}
}

func TestPABBlockable(t *testing.T) {
	for _, c := range []struct {
		perm, version string
		want          bool
	}{
		{"storage.objects.get", "1", true},
		{"storage.objects.list", "1", false}, // version 1 names objects.get, not list
		{"storage.objects.list", "2", true},  // version 2's objects.*
		{"storage.objects.list", "latest", true},
		{"storage.objects.list", "", true},
		{"secretmanager.versions.access", "3", false},
		{"secretmanager.versions.access", "4", true},
		{"pubsub.subscriptions.consume", "1", false}, // excepted in version 1
		{"pubsub.subscriptions.consume", "3", true},  // version 3's pubsub *.* has no exception
		{"pubsub.topics.getIamPolicy", "1", false},
		{"resourcemanager.projects.createPolicyBinding", "4", false},
		{"resourcemanager.projects.get", "2", true},
		{"cloudkms.locations.list", "4", false},
		{"storage.objects.get", "5", false},
	} {
		if got := PABBlockable(c.perm, c.version); got != c.want {
			t.Errorf("PABBlockable(%s, %q) = %v, want %v", c.perm, c.version, got, c.want)
		}
	}
	if v, ok := EnforcementVersion("latest"); !ok || v != DefaultEnforcementVersion {
		t.Errorf("latest = %d, %v", v, ok)
	}
	for _, bad := range []string{"0", "5", "01", "v1"} {
		if _, ok := EnforcementVersion(bad); ok {
			t.Errorf("EnforcementVersion(%q) is valid", bad)
		}
	}
}

func TestPABBoundary(t *testing.T) {
	saEmail := "app@p1.iam.gserviceaccount.com"
	get := "storage.objects.get"
	viewer := iampolicy.Binding{Role: "roles/storage.objectViewer", Members: []string{"serviceAccount:" + saEmail}}
	anc := map[string][]string{"p1": {"projects/p1", "folders/1", "organizations/9"}, "p2": {"projects/p2", "organizations/9"}}
	base := fakeSource{ancestry: anc, policies: map[string]*iampolicy.Stored{"projects/p1": pol(viewer), "projects/p2": pol(viewer)}}
	req := func(project string) Request {
		return Request{Email: saEmail, Permission: get, Resource: "projects/_/buckets/b/objects/o", Service: "storage.googleapis.com", Project: project, Time: time.Now()}
	}
	binding := &iamv3.PolicyBinding{Name: "organizations/9/locations/global/policyBindings/b"}
	src := pabSource{fakeSource: base, bound: map[string][]Bound{
		"//cloudresourcemanager.googleapis.com/organizations/9": {{Binding: binding, Policy: pabPolicy("1", "//cloudresourcemanager.googleapis.com/projects/p1")}}}}
	e := &Evaluator{Source: src}
	if d := e.Check(req("p1")); !d.Granted || d.Blocked {
		t.Errorf("p1 is in the boundary: %+v", d)
	}
	d := e.Check(req("p2"))
	if d.Granted || !d.Blocked || !d.Allowed || len(d.Boundary) != 1 || d.Boundary[0].State != PABNotAllowed {
		t.Errorf("p2 is outside the boundary: %+v", d)
	}
	// A folder rule covers its descendants.
	src.bound["//cloudresourcemanager.googleapis.com/organizations/9"][0].Policy = pabPolicy("1", "//cloudresourcemanager.googleapis.com/folders/1")
	if !e.Check(req("p1")).Granted || e.Check(req("p2")).Granted {
		t.Error("folders/1 holds p1 only")
	}
	// A permission the version cannot block is not blocked.
	r := req("p2")
	r.Permission = "storage.objects.list"
	src.policies["projects/p2"] = pol(iampolicy.Binding{Role: "roles/storage.objectViewer", Members: []string{"serviceAccount:" + saEmail}})
	if d := e.Check(r); !d.Granted || d.Boundary[0].State != PABNotEnforced {
		t.Errorf("version 1 cannot block objects.list: %+v", d)
	}
	src.bound["//cloudresourcemanager.googleapis.com/organizations/9"][0].Policy = pabPolicy("latest", "//cloudresourcemanager.googleapis.com/folders/1")
	if e.Check(r).Granted {
		t.Error("latest blocks objects.list")
	}
	// A condition that is false does not enforce the policy.
	binding.Condition = &expr.Expr{Expression: "principal.type != 'iam.googleapis.com/ServiceAccount'"}
	if !e.Check(r).Granted {
		t.Error("the condition exempts service accounts")
	}
	binding.Condition = &expr.Expr{Expression: "principal.subject == '" + saEmail + "'"}
	if e.Check(r).Granted {
		t.Error("the condition names the account")
	}
	// The owner's path: Denies reports a block.
	if !e.Denies(r) {
		t.Error("Denies misses the block")
	}
	// Additive: a second policy that makes p2 eligible lets it through.
	src.bound["//cloudresourcemanager.googleapis.com/projects/p1"] = []Bound{{Binding: &iamv3.PolicyBinding{Name: "projects/p1/locations/global/policyBindings/c"},
		Policy: pabPolicy("4", "//cloudresourcemanager.googleapis.com/projects/p2")}}
	if !e.Check(r).Granted {
		t.Error("the project's binding makes p2 eligible")
	}
	// A user outside any domain is in no principal set.
	if sets, _, _ := e.PrincipalSets("someone@gmail.com"); len(sets) != 0 {
		t.Errorf("sets = %v", sets)
	}
}

func TestPrincipalSets(t *testing.T) {
	DomainCustomers["example.com"] = "C0abc"
	defer delete(DomainCustomers, "example.com")
	e := &Evaluator{Source: pabSource{fakeSource: fakeSource{ancestry: map[string][]string{"123": {"projects/p", "organizations/9"}}}}}
	for email, want := range map[string][]string{
		"alice@example.com": {"//iam.googleapis.com/locations/global/workspace/C0abc", "//cloudresourcemanager.googleapis.com/organizations/9"},
		"principal://iam.googleapis.com/locations/global/workforcePools/wf/subject/bob": {
			"//iam.googleapis.com/locations/global/workforcePools/wf", "//cloudresourcemanager.googleapis.com/organizations/9"},
		"principal://iam.googleapis.com/projects/123/locations/global/workloadIdentityPools/wl/subject/s": {
			"//iam.googleapis.com/projects/123/locations/global/workloadIdentityPools/wl",
			"//cloudresourcemanager.googleapis.com/projects/p", "//cloudresourcemanager.googleapis.com/organizations/9"},
	} {
		got, _, _ := e.PrincipalSets(email)
		if len(got) != len(want) {
			t.Fatalf("%s: %v", email, got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s: %v, want %v", email, got, want)
			}
		}
	}
}
