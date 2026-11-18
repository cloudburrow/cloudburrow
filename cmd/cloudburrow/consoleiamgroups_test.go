package main

import (
	"context"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// IAM & Admin > Groups (#1812): Create group, the list, the group's page
// with its members, Add member (a nested group too), Remove member and
// Delete, over the registry Cloud Identity's API serves, with its refusals.
func TestConsoleIAMGroups(t *testing.T) {
	reg := resourcemanager.New(store.NewMemory())
	p := iamGroupsProvider{registry: reg}
	ctx := context.Background()
	d := "@" + resourcemanager.OrganizationDomain
	for _, e := range []string{"outer" + d, "inner" + d} {
		if row, err := p.Create(ctx, "", map[string]string{"email": e, "displayName": "G", "security": "Yes"}); err != nil || row != e {
			t.Fatalf("Create %s = %q, %v", e, row, err)
		}
	}
	if _, err := p.Create(ctx, "", map[string]string{"email": "x@example.com"}); err == nil {
		t.Error("created a group outside the customer's domain")
	}
	l, _ := p.List(ctx, "")
	if len(l.Items) != 2 || l.Items[0].Name != "inner"+d || l.Items[0].Fields["Type"] != "Security" {
		t.Fatalf("List = %+v", l.Items)
	}
	sa := "a@p.iam.gserviceaccount.com"
	if err := p.Edit(ctx, "", []string{"inner" + d}, map[string]string{"add": sa, "role": "MEMBER"}); err != nil {
		t.Fatal(err)
	}
	if err := p.Edit(ctx, "", []string{"outer" + d}, map[string]string{"add": "inner" + d, "role": "MANAGER"}); err != nil {
		t.Fatal(err)
	}
	if !reg.MemberNames("group:outer"+d, sa) {
		t.Errorf("%s is not in outer through inner", sa)
	}
	det, _ := p.Detail(ctx, "", []string{"outer" + d})
	if det.Unavailable != "" || det.Edit == nil || len(det.Sections[0].Listing.Items) != 1 ||
		det.Sections[0].Listing.Items[0].Fields["Type"] != "GROUP" || det.Sections[0].Listing.Items[0].Fields["Roles"] != "MANAGER, MEMBER" {
		t.Fatalf("Detail = %+v", det.Sections[0].Listing.Items)
	}
	if in, _ := p.Detail(ctx, "", []string{"inner" + d}); len(in.Sections[1].Listing.Items) != 1 || in.Sections[1].Listing.Items[0].Name != "outer"+d {
		t.Errorf("inner's Member of = %+v", in.Sections[1].Listing.Items)
	}
	if err := p.Edit(ctx, "", []string{"outer" + d}, map[string]string{"displayName": "Renamed", "description": "d"}); err != nil {
		t.Fatal(err)
	}
	if g, _ := reg.GetGroup(mustLookup(t, reg, "outer"+d)); g.DisplayName != "Renamed" {
		t.Errorf("Edit group: display name %q", g.DisplayName)
	}
	if err := p.Edit(ctx, "", []string{"outer" + d}, map[string]string{"remove": "inner" + d}); err != nil {
		t.Fatal(err)
	}
	if reg.MemberNames("group:outer"+d, sa) {
		t.Errorf("%s still in outer", sa)
	}
	if err := p.Delete(ctx, "", "inner"+d); err != nil {
		t.Fatal(err)
	}
	if l, _ := p.List(ctx, ""); len(l.Items) != 1 {
		t.Errorf("after Delete, List = %+v", l.Items)
	}
}

func mustLookup(t *testing.T, reg *resourcemanager.Registry, email string) string {
	t.Helper()
	n, err := reg.LookupGroup(email, "")
	if err != nil {
		t.Fatal(err)
	}
	return n
}
