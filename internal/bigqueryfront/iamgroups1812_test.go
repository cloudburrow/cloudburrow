package bigqueryfront

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

func TestEntryNames1812(t *testing.T) {
	ps := []string{"group:g@cloudburrow.local", "domain:cloudburrow.local"}
	for _, tc := range []struct {
		user, group, domain, member string
		want                        bool
	}{
		{group: "G@cloudburrow.local", want: true},
		{group: "other@cloudburrow.local"},
		{domain: "cloudburrow.local", want: true},
		{domain: "example.com"},
		{member: "group:g@cloudburrow.local", want: true},
		{member: "domain:cloudburrow.local", want: true},
		{member: "group:h@cloudburrow.local"},
		{user: "u@cloudburrow.local", want: true},
	} {
		if got := entryNames(tc.user, tc.group, tc.domain, tc.member, "u@cloudburrow.local", ps); got != tc.want {
			t.Errorf("entryNames(%+v) = %v", tc, got)
		}
	}
	if entryNames("", "g@cloudburrow.local", "", "", "u@cloudburrow.local", nil) {
		t.Errorf("without principals a group entry names nobody")
	}
}

// TestConnectionMembers1812: a connection check's account is named by the
// Cloud Identity groups it belongs to, nested too, read from Resource
// Manager's Cloud Identity surface (searchTransitiveGroups).
func TestConnectionMembers1812(t *testing.T) {
	reg := resourcemanager.New(store.NewMemory())
	mk := func(email string) *resourcemanager.Group {
		g, err := reg.CreateGroup(resourcemanager.GroupInput{Email: email, Parent: resourcemanager.CustomerName,
			Labels: map[string]string{resourcemanager.LabelDiscussionForum: ""}})
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	outer, inner := mk("outer@"+resourcemanager.OrganizationDomain), mk("inner@"+resourcemanager.OrganizationDomain)
	sa := "cloudburrow-local@p.iam.gserviceaccount.com"
	if _, _, err := reg.CreateMembership(inner.Name(), sa, "", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reg.CreateMembership(outer.Name(), inner.Email, "", nil); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(reg.IdentityHandler())
	defer srv.Close()
	f := front{resourceManager: srv.URL}
	got := f.members(context.Background(), "serviceAccount:"+sa)
	if len(got) != 3 || got[1] != "group:"+inner.Email || got[2] != "group:"+outer.Email {
		t.Fatalf("members = %v", got)
	}
	bindings := []policyBinding{{Role: "roles/bigquery.connectionUser", Members: []string{"group:" + outer.Email}}}
	if !bindingsGrant(bindings, got, permConnectionsUse) {
		t.Errorf("group:%s does not grant %s", outer.Email, sa)
	}
	if bindingsGrant(bindings, []string{"serviceAccount:" + sa}, permConnectionsUse) {
		t.Errorf("without the groups, the binding granted")
	}
	if m := (front{}).members(context.Background(), "serviceAccount:"+sa); len(m) != 1 {
		t.Errorf("without Resource Manager, members = %v", m)
	}
}
