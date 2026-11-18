package resourcemanager

import (
	"context"
	"testing"
	"time"

	"cloud.google.com/go/iam/admin/apiv1/adminpb"
)

// The OnMemberRewrite hooks (#1751) see a deleted account's rewrites: to
// the deleted identifier on delete, back on undelete, and to "" (dropped)
// when the account is purged 30 days after deletion.
func TestMemberRewriteHooks(t *testing.T) {
	reg, _, c := startIAMAdmin(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	reg.now = func() time.Time { return now }
	if _, err := reg.Create(Project{ProjectID: "rw-project"}); err != nil {
		t.Fatal(err)
	}
	type rw struct{ from, to string }
	var got []rw
	reg.OnMemberRewrite(func(from, to string) error { got = append(got, rw{from, to}); return nil })
	sa, err := c.CreateServiceAccount(ctx, &adminpb.CreateServiceAccountRequest{Name: "projects/rw-project", AccountId: "victim"})
	if err != nil {
		t.Fatal(err)
	}
	live := "serviceAccount:" + sa.GetEmail()
	deleted := "deleted:serviceAccount:" + sa.GetEmail() + "?uid=" + sa.GetUniqueId()
	if err := c.DeleteServiceAccount(ctx, &adminpb.DeleteServiceAccountRequest{Name: sa.GetName()}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UndeleteServiceAccount(ctx, &adminpb.UndeleteServiceAccountRequest{Name: "projects/-/serviceAccounts/" + sa.GetUniqueId()}); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteServiceAccount(ctx, &adminpb.DeleteServiceAccountRequest{Name: sa.GetName()}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(31 * 24 * time.Hour)
	if _, err := c.ListServiceAccounts(ctx, &adminpb.ListServiceAccountsRequest{Name: "projects/rw-project"}).Next(); err == nil {
		t.Fatal("a purged account is still listed")
	}
	want := []rw{{live, deleted}, {deleted, live}, {live, deleted}, {deleted, ""}}
	if len(got) != len(want) {
		t.Fatalf("rewrites %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("rewrite %d = %v, want %v", i, got[i], want[i])
		}
	}
}
