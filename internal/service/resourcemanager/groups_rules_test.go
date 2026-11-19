package resourcemanager

import (
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

func ruleCode(err error) codes.Code {
	var e *apierror.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return codes.Unknown
}

func forum(extra ...string) map[string]string {
	l := map[string]string{LabelDiscussionForum: ""}
	for _, k := range extra {
		l[k] = ""
	}
	return l
}

// TestGroupEmailRules: a group's email follows a username's rules
// (Directory API: "Group email addresses are subject to the same character
// usage rules as usernames"; name guidelines, support.google.com/a/answer/9193374).
func TestGroupEmailRules(t *testing.T) {
	r := New(store.NewMemory())
	d := "@" + OrganizationDomain
	for _, tc := range []struct {
		local string
		ok    bool
	}{
		{strings.Repeat("a", 64), true},
		{strings.Repeat("a", 65), false},
		{"o'brien-team_1.x", true},
		{".lead", false},
		{"trail.", false},
		{"two..dots", false},
		{"a+b", false},
		{"a=b", false},
		{"a&b", false},
		{"a!b", false},
	} {
		_, err := r.CreateGroup(GroupInput{Email: tc.local + d, Parent: CustomerName, Labels: forum()})
		if tc.ok && err != nil {
			t.Errorf("%s: %v, want created", tc.local, err)
		}
		if !tc.ok && ruleCode(err) != codes.InvalidArgument {
			t.Errorf("%s: %v, want INVALID_ARGUMENT", tc.local, err)
		}
	}
}

// TestGroupDisplayNameLimit: "Can be up to 73 characters", counted in
// characters, on create and on patch; the description's 4,096 likewise.
func TestGroupDisplayNameLimit(t *testing.T) {
	r := New(store.NewMemory())
	d := "@" + OrganizationDomain
	g, err := r.CreateGroup(GroupInput{Email: "n" + d, Parent: CustomerName, Labels: forum(), DisplayName: strings.Repeat("é", 73)})
	if err != nil {
		t.Fatalf("73 characters: %v", err)
	}
	if _, err := r.CreateGroup(GroupInput{Email: "m" + d, Parent: CustomerName, Labels: forum(), DisplayName: strings.Repeat("a", 74)}); ruleCode(err) != codes.InvalidArgument {
		t.Errorf("74 characters on create: %v, want INVALID_ARGUMENT", err)
	}
	if _, err := r.UpdateGroup(g.Name(), GroupInput{DisplayName: strings.Repeat("a", 74)}, []string{"display_name"}); ruleCode(err) != codes.InvalidArgument {
		t.Errorf("74 characters on patch: %v, want INVALID_ARGUMENT", err)
	}
	if _, err := r.UpdateGroup(g.Name(), GroupInput{Description: strings.Repeat("é", 4096)}, []string{"description"}); err != nil {
		t.Errorf("4,096 characters of description: %v", err)
	}
	if _, err := r.UpdateGroup(g.Name(), GroupInput{Description: strings.Repeat("a", 4097)}, []string{"description"}); ruleCode(err) != codes.InvalidArgument {
		t.Errorf("4,097 characters of description: %v, want INVALID_ARGUMENT", err)
	}
}

// TestSecurityGroupMembers: a security group holds users, service accounts
// and security groups only, and the label cannot be applied to a group
// that holds another kind.
func TestSecurityGroupMembers(t *testing.T) {
	r := New(store.NewMemory())
	d := "@" + OrganizationDomain
	mk := func(local string, labels map[string]string) *Group {
		g, err := r.CreateGroup(GroupInput{Email: local + d, Parent: CustomerName, Labels: labels})
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	sec, secInner, plain := mk("sec", forum(LabelSecurity)), mk("sec-inner", forum(LabelSecurity)), mk("plain", forum())
	for _, ok := range []string{"u" + d, "x@example.com", "sa@p.iam.gserviceaccount.com", secInner.Email} {
		if _, _, err := r.CreateMembership(sec.Name(), ok, "", nil); err != nil {
			t.Errorf("security group member %s: %v", ok, err)
		}
	}
	if _, _, err := r.CreateMembership(sec.Name(), plain.Email, "", nil); ruleCode(err) != codes.FailedPrecondition {
		t.Errorf("a Google Group in a security group: %v, want FAILED_PRECONDITION", err)
	}
	// plain may hold plain groups, but then cannot become a security group.
	other := mk("other", forum())
	if _, _, err := r.CreateMembership(plain.Name(), other.Email, "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.UpdateGroup(plain.Name(), GroupInput{Labels: forum(LabelSecurity)}, []string{"labels"}); ruleCode(err) != codes.FailedPrecondition {
		t.Errorf("security label on a group holding a Google Group: %v, want FAILED_PRECONDITION", err)
	}
	if _, err := r.UpdateGroup(other.Name(), GroupInput{Labels: forum(LabelSecurity)}, []string{"labels"}); err != nil {
		t.Errorf("security label on a group of no groups: %v", err)
	}
	if _, err := r.UpdateGroup(sec.Name(), GroupInput{Labels: forum()}, []string{"labels"}); ruleCode(err) != codes.InvalidArgument {
		t.Errorf("removing the security label: %v, want INVALID_ARGUMENT", err)
	}
}

// TestMembershipExpiryRules: what Google documents, and no more. Expiry is
// "only supported for MEMBER MembershipRoles"; it applies to "users,
// groups, and service accounts with the MEMBER role" (Managing membership
// expirations); nothing forbids an expiring MEMBER from also holding OWNER
// or MANAGER, and no maximum is documented.
func TestMembershipExpiryRules(t *testing.T) {
	r := New(store.NewMemory())
	d := "@" + OrganizationDomain
	g, _ := r.CreateGroup(GroupInput{Email: "g" + d, Parent: CustomerName, Labels: forum()})
	inner, _ := r.CreateGroup(GroupInput{Email: "inner" + d, Parent: CustomerName, Labels: forum()})
	far := time.Now().Add(50 * 365 * 24 * time.Hour)
	if _, m, err := r.CreateMembershipExpiring(g.Name(), inner.Email, "", nil, &far); err != nil || m.Type != "GROUP" {
		t.Errorf("an expiring group member: %v %+v", err, m)
	}
	_, m, err := r.CreateMembershipExpiring(g.Name(), "o"+d, "", []string{"OWNER"}, &far)
	if err != nil {
		t.Fatalf("an expiring MEMBER that is also OWNER: %v", err)
	}
	name := MembershipName(g, m)
	if _, _, err := r.ModifyMembershipRoles(name, []RoleChange{{Name: "MANAGER"}}, nil, nil, false); err != nil {
		t.Errorf("adding MANAGER to an expiring member: %v", err)
	}
	soon := time.Now().Add(time.Hour)
	if _, _, err := r.ModifyMembershipRoles(name, nil, nil, []RoleChange{{Name: "MEMBER", ExpireTime: &soon}}, true); err != nil {
		t.Errorf("updating an OWNER's MEMBER expiry: %v", err)
	}
	if _, _, err := r.ModifyMembershipRoles(name, []RoleChange{{Name: "OWNER", ExpireTime: &soon}}, []string{"OWNER"}, nil, false); ruleCode(err) != codes.InvalidArgument {
		t.Errorf("an expiry on OWNER: %v, want INVALID_ARGUMENT", err)
	}
	if _, _, err := r.ModifyMembershipRoles(name, nil, nil, []RoleChange{{Name: "OWNER", ExpireTime: &soon}}, true); ruleCode(err) != codes.InvalidArgument {
		t.Errorf("updateRolesParams on OWNER: %v, want INVALID_ARGUMENT", err)
	}
}
