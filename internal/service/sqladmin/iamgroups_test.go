package sqladmin

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// TestIAMGroupUserNames (#1524): a group's role on PostgreSQL is its
// email, at most 63 characters; on MySQL 'NAME'@'DOMAIN', NAME at most 32.
func TestIAMGroupUserNames(t *testing.T) {
	pg := &namedFake{fake: newFake(), name: "pg", version: "POSTGRES_17"}
	my := &namedFake{fake: newFake(), name: "my", version: "MYSQL_8_0"}
	for _, c := range []struct {
		e          Engine
		email      string
		name, host string
		ok         bool
	}{
		{pg, "Eng@Example.com", "eng@example.com", "", true},
		{my, "eng@example.com", "eng", "example.com", true},
		{pg, strings.Repeat("g", 52) + "@example.com", "", "", false},
		{my, strings.Repeat("g", 33) + "@example.com", "", "", false},
		{pg, "sa@p.iam.gserviceaccount.com", "", "", false},
		{pg, "not a group", "", "", false},
	} {
		u, err := iamGroupUser(c.e, c.email)
		if (err == nil) != c.ok || u.Name != c.name || u.Host != c.host {
			t.Errorf("iamGroupUser(%s, %s) = %+v, %v; want %s@%s ok=%v", c.e.DatabaseVersion(), c.email, u, err, c.name, c.host, c.ok)
		}
		if c.ok && (u.Type != TypeIAMGroup || u.IAMEmail != strings.ToLower(c.email)) {
			t.Errorf("iamGroupUser(%s) = %+v", c.email, u)
		}
	}
}

// TestIAMLoginUserOfGroupMembers (#1524): a sign-in as a name no account
// has is a group member's first only when the instance has a group; an
// existing member's account and an individual IAM user are themselves;
// a built-in user's name is never IAM.
func TestIAMLoginUserOfGroupMembers(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	pg := &namedFake{fake: f, name: "pg", version: "POSTGRES_17"}
	if _, ok := iamLoginUser(ctx, pg, "new@example.com"); ok {
		t.Error("no group: a sign-in as an unknown name is IAM")
	}
	f.users["eng@example.com"] = User{Name: "eng@example.com", Type: TypeIAMGroup, IAMEmail: "eng@example.com"}
	f.users["old@example.com"] = User{Name: "old@example.com", Type: TypeIAMGroupUser, IAMEmail: "old@example.com"}
	f.users["ind@example.com"] = User{Name: "ind@example.com", Type: TypeIAMUser, IAMEmail: "ind@example.com"}
	for name, want := range map[string]string{
		"new@example.com": TypeIAMGroupUser, "old@example.com": TypeIAMGroupUser, "ind@example.com": TypeIAMUser,
		"sa@p.iam": TypeIAMGroupUser,
	} {
		u, ok := iamLoginUser(ctx, pg, name)
		if !ok || u.Type != want || u.Name != name {
			t.Errorf("iamLoginUser(%s) = %+v, %v; want %s", name, u, ok, want)
		}
	}
	for _, name := range []string{"cloudburrow", "plain", "eng@example.com"} {
		if u, ok := iamLoginUser(ctx, pg, name); ok {
			t.Errorf("iamLoginUser(%s) = %+v; want not IAM", name, u)
		}
	}
	// On MySQL a member's name has no @.
	my := &namedFake{fake: f, name: "my", version: "MYSQL_8_0"}
	if u, ok := iamLoginUser(ctx, my, "alice"); !ok || u.Type != TypeIAMGroupUser {
		t.Errorf("MySQL iamLoginUser(alice) = %+v, %v", u, ok)
	}
}

// TestGroupMemberConflict (#1524): an individual IAM user identical to a
// group member's account is refused.
func TestGroupMemberConflict(t *testing.T) {
	f := newFake()
	f.users["m@example.com"] = User{Name: "m@example.com", Type: TypeIAMGroupUser}
	pg := &namedFake{fake: f, name: "pg", version: "POSTGRES_17"}
	if err := groupMemberConflict(context.Background(), pg, "m@example.com"); err == nil {
		t.Error("an individual user identical to a group member's account is taken")
	}
	if err := groupMemberConflict(context.Background(), pg, "x@example.com"); err != nil {
		t.Error(err)
	}
}

// TestIAMGroupRefusedOnMySQL57 (#1524): IAM group authentication is MySQL
// 8.0 and later's.
func TestIAMGroupRefusedOnMySQL57(t *testing.T) {
	srv := NewServer("", newFake())
	for _, v := range []string{"MYSQL_5_7", "MYSQL_5_6"} {
		e := &namedFake{fake: newFake(), name: "old", version: v}
		_, err := srv.insertIAMGroup(context.Background(), "/", "p", e, "g@example.com", "", "")
		if ae, ok := err.(*apiError); !ok || ae.HTTPStatus() != http.StatusBadRequest {
			t.Errorf("CLOUD_IAM_GROUP on %s: %v; want 400", v, err)
		}
	}
}
