package iameval

import (
	"testing"
	"time"

	iamv2 "cloud.google.com/go/iam/apiv2/iampb"
	"google.golang.org/genproto/googleapis/type/expr"

	"github.com/cloudburrow/cloudburrow/internal/iamcond"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
)

type denySource struct {
	fakeSource
	deny map[string][]*iamv2.Policy
	tags []iamcond.Tag
}

func (d denySource) DenyPolicies(r string) []*iamv2.Policy { return d.deny[r] }
func (d denySource) Tags(string) ([]iamcond.Tag, bool)     { return d.tags, d.tags != nil }

func denyRule(principals, perms []string, mod func(*iamv2.DenyRule)) *iamv2.Policy {
	r := &iamv2.DenyRule{DeniedPrincipals: principals, DeniedPermissions: perms}
	if mod != nil {
		mod(r)
	}
	return &iamv2.Policy{Name: "policies/x/denypolicies/d", Rules: []*iamv2.PolicyRule{{Kind: &iamv2.PolicyRule_DenyRule{DenyRule: r}}}}
}

func TestV2Permission(t *testing.T) {
	for v1, v2 := range map[string]string{
		"secretmanager.versions.access":   "secretmanager.googleapis.com/versions.access",
		"resourcemanager.projects.delete": "cloudresourcemanager.googleapis.com/projects.delete",
		"bigquery.connections.create":     "bigqueryconnection.googleapis.com/connections.create",
	} {
		if got := V2Permission(v1); got != v2 {
			t.Errorf("V2Permission(%s) = %s, want %s", v1, got, v2)
		}
	}
	if !Deniable("secretmanager.versions.access") {
		t.Error("secretmanager.versions.access is deniable per Google's list")
	}
	if _, ok := DeniablePermission("secretmanager.googleapis.com/versions.*"); !ok {
		t.Error("the versions.* group is supported")
	}
	if _, ok := DeniablePermission("secretmanager.googleapis.com/nothing.here"); ok {
		t.Error("an unknown permission is not deniable")
	}
}

func TestDenyBeforeAllow(t *testing.T) {
	access := "secretmanager.versions.access"
	owner := iampolicy.Binding{Role: "roles/owner", Members: []string{"serviceAccount:" + sa}}
	src := denySource{fakeSource: fakeSource{ancestry: map[string][]string{"p": {"projects/p", "folders/1", "organizations/9"}},
		policies: map[string]*iampolicy.Stored{"projects/p": pol(owner)}, groups: map[string][]string{sa: {"group:eng@example.com"}}},
		deny: map[string][]*iamv2.Policy{}}
	e := &Evaluator{Source: src}
	req := Request{Email: sa, Permission: access, Resource: "projects/p/secrets/s", Service: "secretmanager.googleapis.com", Project: "p", Time: time.Now()}
	if !e.Check(req).Granted {
		t.Fatal("owner not granted")
	}
	saID := "principal://iam.googleapis.com/projects/-/serviceAccounts/" + sa
	cases := []struct {
		name   string
		at     string
		policy *iamv2.Policy
		denied bool
	}{
		{"service account, on the organization (inherited)", "organizations/9", denyRule([]string{saID}, []string{"secretmanager.googleapis.com/versions.access"}, nil), true},
		{"group, by a resource group", "folders/1", denyRule([]string{"principalSet://goog/group/eng@example.com"}, []string{"secretmanager.googleapis.com/versions.*"}, nil), true},
		{"everyone, by a service group", "projects/p", denyRule([]string{"principalSet://goog/public:all"}, []string{"secretmanager.googleapis.com/*.*"}, nil), true},
		{"exception principal", "projects/p", denyRule([]string{"principalSet://goog/public:all"}, []string{"secretmanager.googleapis.com/versions.access"},
			func(r *iamv2.DenyRule) { r.ExceptionPrincipals = []string{saID} }), false},
		{"exception permission", "projects/p", denyRule([]string{saID}, []string{"secretmanager.googleapis.com/versions.*"},
			func(r *iamv2.DenyRule) {
				r.ExceptionPermissions = []string{"secretmanager.googleapis.com/versions.access"}
			}), false},
		{"another permission", "projects/p", denyRule([]string{saID}, []string{"secretmanager.googleapis.com/versions.destroy"}, nil), false},
		{"another user", "projects/p", denyRule([]string{"principal://goog/subject/" + sa}, []string{"secretmanager.googleapis.com/versions.access"}, nil), false},
		{"condition false", "projects/p", denyRule([]string{saID}, []string{"secretmanager.googleapis.com/versions.access"},
			func(r *iamv2.DenyRule) {
				r.DenialCondition = &expr.Expr{Expression: `resource.matchTag("9/env", "prod")`}
			}), false},
		{"condition unknown denies", "projects/p", denyRule([]string{saID}, []string{"secretmanager.googleapis.com/versions.access"},
			func(r *iamv2.DenyRule) {
				r.DenialCondition = &expr.Expr{Expression: `resource.hasTagKey("9/missing") && request.time > 1`}
			}), true},
	}
	for _, c := range cases {
		src.deny = map[string][]*iamv2.Policy{c.at: {c.policy}}
		src.tags = []iamcond.Tag{{NamespacedKey: "9/env", ShortValue: "dev"}}
		e := &Evaluator{Source: src}
		d := e.Check(req)
		if d.Denied != c.denied || d.Granted == c.denied || !d.Allowed {
			t.Errorf("%s: denied %v granted %v allowed %v, want denied %v", c.name, d.Denied, d.Granted, d.Allowed, c.denied)
		}
		if e.Denies(req) != c.denied {
			t.Errorf("%s: Denies = %v", c.name, !c.denied)
		}
	}
	// With the prod tag, the condition holds.
	src.deny = map[string][]*iamv2.Policy{"projects/p": {denyRule([]string{saID}, []string{"secretmanager.googleapis.com/versions.access"},
		func(r *iamv2.DenyRule) {
			r.DenialCondition = &expr.Expr{Expression: `resource.matchTag("9/env", "prod")`}
		})}}
	src.tags = []iamcond.Tag{{NamespacedKey: "9/env", ShortValue: "prod"}}
	if d := (&Evaluator{Source: src}).Check(req); !d.Denied {
		t.Error("a true condition did not deny")
	}
}
