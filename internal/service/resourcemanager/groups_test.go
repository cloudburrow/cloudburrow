package resourcemanager

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/transport/rest"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

func TestGroupsPrincipals1812(t *testing.T) {
	r := New(store.NewMemory())
	mk := func(email string) *Group {
		g, err := r.CreateGroup(GroupInput{Email: email, Parent: CustomerName, Labels: map[string]string{LabelDiscussionForum: ""}})
		if err != nil {
			t.Fatalf("CreateGroup %s: %v", email, err)
		}
		return g
	}
	outer, inner := mk("outer@"+OrganizationDomain), mk("inner@"+OrganizationDomain)
	sa := "a@p.iam.gserviceaccount.com"
	if _, _, err := r.CreateMembership(inner.Name(), sa, "", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.CreateMembership(outer.Name(), inner.Email, "", nil); err != nil {
		t.Fatal(err)
	}
	// A cycle is followed once.
	if _, _, err := r.CreateMembership(inner.Name(), outer.Email, "", nil); err != nil {
		t.Fatal(err)
	}
	got := r.Principals(sa)
	want := []string{"serviceAccount:" + sa, "group:inner@" + OrganizationDomain, "group:outer@" + OrganizationDomain}
	if !slices.Equal(got, want) {
		t.Errorf("Principals(%s) = %v, want %v", sa, got, want)
	}
	if !r.MemberNames("group:OUTER@"+OrganizationDomain, sa) || r.MemberNames("domain:"+OrganizationDomain, sa) {
		t.Errorf("MemberNames: the outer group names %s, the domain does not (a service account)", sa)
	}
	user := "u@" + OrganizationDomain
	if p := r.Principals(user); !slices.Equal(p, []string{"user:" + user, "domain:" + OrganizationDomain}) {
		t.Errorf("Principals(%s) = %v", user, p)
	}
	if r.MemberNames("domain:example.com", "u@example.com") {
		t.Errorf("domain:example.com is not the instance's domain, and names nobody")
	}
	rels, _ := r.TransitiveGroups(sa)
	for _, x := range rels {
		if x.Group.Email == inner.Email && !(x.Direct && x.Indirect) {
			t.Errorf("inner: the account is a direct member and, through the cycle, an indirect one: %+v", x)
		}
	}
	if _, err := r.CreateGroup(GroupInput{Email: "outer@" + OrganizationDomain, Parent: CustomerName,
		Labels: map[string]string{LabelDiscussionForum: ""}}); err == nil {
		t.Errorf("a taken email was created")
	}
	if err := r.DeleteGroup(inner.Name()); err != nil {
		t.Fatal(err)
	}
	if p := r.Principals(sa); !slices.Equal(p, []string{"serviceAccount:" + sa}) {
		t.Errorf("after the inner group was deleted, Principals = %v", p)
	}
	g, _ := r.GetGroup(outer.Name())
	if len(g.Members) != 0 {
		t.Errorf("the deleted group is still a member of outer: %+v", g.Members)
	}
}

// TestCloudIdentityUnservedAre501 (#1812): every Cloud Identity method the
// table says is not served answers 501 at its path, and every served one
// does not.
func TestCloudIdentityUnservedAre501(t *testing.T) {
	r := New(store.NewMemory())
	rt := rest.NewRouter()
	r.identityRoutes(rt)
	fill := regexp.MustCompile(`\{[^}]+\}`)
	for id, m := range CloudIdentityMethods {
		path := "/" + fill.ReplaceAllString(m.Path, "x")
		req := httptest.NewRequest(m.HTTPMethod, path, strings.NewReader("{}"))
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, req)
		if got := rec.Code == http.StatusNotImplemented; got == m.Served {
			t.Errorf("%s (%s %s) = %d, served %v", id, m.HTTPMethod, path, rec.Code, m.Served)
		}
	}
}

// TestPrincipalsOfAResourceIdentity (#2054): a resource's own principal://
// identity, a Parameter Manager parameter's, is its only policy member.
func TestPrincipalsOfAResourceIdentity(t *testing.T) {
	const p = "principal://parametermanager.googleapis.com/projects/1/uid/locations/global/parameters/abc"
	var r *Registry
	if got := r.Principals(p); len(got) != 1 || got[0] != p {
		t.Fatalf("Principals(%q) = %v", p, got)
	}
}
