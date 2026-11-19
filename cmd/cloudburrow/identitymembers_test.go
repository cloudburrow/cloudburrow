package main

import (
	"slices"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestMemberIndex1812: the host's checks (BigQuery's payload, Firestore
// with MongoDB compatibility's, Memorystore's and IAM Credentials')
// expand a group: member to its accounts, nested groups too, and
// domain: to the domain's accounts, never a service account.
func TestMemberIndex1812(t *testing.T) {
	reg := resourcemanager.New(store.NewMemory())
	d := "@" + resourcemanager.OrganizationDomain
	var names []string
	for _, e := range []string{"outer" + d, "inner" + d} {
		g, err := reg.CreateGroup(resourcemanager.GroupInput{Email: e, Parent: resourcemanager.CustomerName,
			Labels: map[string]string{resourcemanager.LabelDiscussionForum: ""}})
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, g.Name())
	}
	sa, user := "a@p.iam.gserviceaccount.com", "u"+d
	for _, m := range [][2]string{{names[1], sa}, {names[0], "inner" + d}, {names[0], user}} {
		if _, _, err := reg.CreateMembership(m[0], m[1], "", nil); err != nil {
			t.Fatal(err)
		}
	}
	ix := newMemberIndex(reg, map[string]bool{sa: true, user: true, "x@example.com": true})
	for member, want := range map[string][]string{
		"group:OUTER" + d: {sa, user},
		"group:inner" + d: {sa},
		"domain:" + resourcemanager.OrganizationDomain: {user},
		"serviceAccount:" + sa:                         {sa},
		"domain:example.com":                           nil,
	} {
		if got := ix.accounts(member); !slices.Equal(got, want) {
			t.Errorf("accounts(%s) = %v, want %v", member, got, want)
		}
	}
	if !(serviceAccountDirectory{reg: reg}).heldBy("group:outer"+d, "serviceAccount:"+sa) {
		t.Errorf("IAM Credentials: group:outer does not hold for %s", sa)
	}
}
