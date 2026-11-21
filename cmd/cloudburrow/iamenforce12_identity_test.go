package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/iameval"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestIdentityEnforcer (#1879): Cloud Identity's own model, the group's
// roles, decides each call; the admin (no token) is never refused.
func TestIdentityEnforcer(t *testing.T) {
	reg := resourcemanager.New(store.NewMemory())
	g, err := reg.CreateGroup(resourcemanager.GroupInput{Parent: resourcemanager.CustomerName, Email: "g@cloudburrow.local",
		Labels: map[string]string{resourcemanager.LabelDiscussionForum: ""}})
	if err != nil {
		t.Fatal(err)
	}
	for email, roles := range map[string][]string{"m@x.iam.gserviceaccount.com": {"MEMBER"},
		"mgr@x.iam.gserviceaccount.com": {"MEMBER", "MANAGER"}, "own@x.iam.gserviceaccount.com": {"MEMBER", "OWNER"}} {
		if _, _, err := reg.CreateMembership(g.Name(), email, "", roles); err != nil {
			t.Fatal(err)
		}
	}
	ownerMembership, err := reg.LookupMembership(g.Name(), "own@x.iam.gserviceaccount.com", "")
	if err != nil {
		t.Fatal(err)
	}
	e := &iameval.Enforcer{On: func() bool { return true }, CallerOf: func(b string) iameval.Caller {
		if b == "bad" {
			return iameval.Caller{Invalid: true}
		}
		return iameval.Caller{Email: b}
	}}
	h := identityEnforcer(e, func() *resourcemanager.Registry { return reg }, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	call := func(who, method, path, body string) int {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if who != "" {
			r.Header.Set("Authorization", "Bearer "+who)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	const (
		none, member, manager, owner = "n@x.iam.gserviceaccount.com", "m@x.iam.gserviceaccount.com",
			"mgr@x.iam.gserviceaccount.com", "own@x.iam.gserviceaccount.com"
	)
	gp := "/v1/" + g.Name()
	for _, c := range []struct {
		who, method, path, body string
		want                    int
	}{
		{"", "DELETE", gp, "", 200},
		{"bad", "GET", gp, "", 401},
		{none, "GET", gp, "", 403},
		{none, "POST", "/v1/groups", "{}", 200},
		{none, "GET", "/v1/inboundSamlSsoProfiles", "", 403},
		{member, "GET", gp, "", 200},
		{member, "GET", gp + "/memberships", "", 200},
		{member, "PATCH", gp, "{}", 403},
		{member, "POST", gp + "/memberships", `{"roles":[{"name":"MEMBER"}]}`, 403},
		{manager, "PATCH", gp, "{}", 200},
		{manager, "PATCH", gp + "/securitySettings", "{}", 200},
		{manager, "POST", gp + "/memberships", `{"roles":[{"name":"MEMBER"}]}`, 200},
		{manager, "POST", gp + "/memberships", `{"roles":[{"name":"MEMBER"},{"name":"OWNER"}]}`, 403},
		{manager, "DELETE", "/v1/" + ownerMembership, "", 403},
		{manager, "DELETE", gp, "", 403},
		{owner, "DELETE", "/v1/" + ownerMembership, "", 200},
		{owner, "POST", gp + "/memberships", `{"roles":[{"name":"OWNER"}]}`, 200},
		{owner, "DELETE", gp, "", 200},
	} {
		if got := call(c.who, c.method, c.path, c.body); got != c.want {
			t.Errorf("%s %s %s = %d; want %d", c.who, c.method, c.path, got, c.want)
		}
	}
}
