//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	ci "google.golang.org/api/cloudidentity/v1"
)

// covers: cloudidentity.groups.create, cloudidentity.groups.patch, cloudidentity.groups.memberships.create, cloudidentity.groups.memberships.modifyMembershipRoles
//
// TestCloudIdentityMembershipRules, with the official
// google.golang.org/api/cloudidentity/v1 client, holds the instance to
// Google's documented membership and group rules, and to no rule Google
// does not state:
//   - a group email follows a username's rules (Directory API groups.email;
//     name guidelines): 64 characters before the @, no leading, trailing
//     or doubled period, no + = & < > , !; a display name is at most 73
//     characters (400 otherwise);
//   - a security group holds users, service accounts and security groups,
//     and the security label cannot be applied to a group that holds a
//     Google Group (400 FAILED_PRECONDITION);
//   - an expiry is for the MEMBER role only (400 on OWNER, on create, in
//     addRoles and in updateRolesParams), but a group member may expire,
//     and an expiring MEMBER may also be OWNER or MANAGER: Google documents
//     neither restriction (docs/gaps.md).
func TestCloudIdentityMembershipRules(t *testing.T) {
	h := New(t)
	svc := cloudIdentity(t, h, h.Endpoint(EnvResourceManager))
	ctx := h.Context()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1e9)
	forum := "cloudidentity.googleapis.com/groups.discussion_forum"
	security := "cloudidentity.googleapis.com/groups.security"

	create := func(local, display string, labels ...string) (*ci.Operation, error) {
		l := map[string]string{forum: ""}
		for _, k := range labels {
			l[k] = ""
		}
		return svc.Groups.Create(&ci.Group{Parent: localCustomer, GroupKey: &ci.EntityKey{Id: local + "@" + localDomain},
			DisplayName: display, Labels: l}).InitialGroupConfig("EMPTY").Context(ctx).Do()
	}
	for _, local := range []string{strings.Repeat("a", 65), ".r" + suffix, "r" + suffix + ".", "r..s" + suffix, "r+s" + suffix, "r=s" + suffix, "r!s" + suffix} {
		if _, err := create(local, ""); apiCode(err) != http.StatusBadRequest {
			t.Errorf("groups.create %q = %v, want 400", local, err)
		}
	}
	if _, err := create("r-long-"+suffix, strings.Repeat("a", 74)); apiCode(err) != http.StatusBadRequest {
		t.Errorf("groups.create with a 74-character display name = %v, want 400", err)
	}

	// plain holds a Google Group; sec and secInner are security groups.
	plain, inner := groupFixture(t, svc, "r-plain-"+suffix, "r-inner-"+suffix)
	if _, err := svc.Groups.Patch(plain.Name, &ci.Group{DisplayName: strings.Repeat("é", 73)}).UpdateMask("display_name").Context(ctx).Do(); err != nil {
		t.Errorf("groups.patch with a 73-character display name: %v", err)
	}
	if _, err := svc.Groups.Patch(plain.Name, &ci.Group{DisplayName: strings.Repeat("a", 74)}).UpdateMask("display_name").Context(ctx).Do(); apiCode(err) != http.StatusBadRequest {
		t.Errorf("groups.patch with a 74-character display name = %v, want 400", err)
	}
	addMember(t, svc, plain.Name, inner.GroupKey.Id)
	if _, err := svc.Groups.Patch(plain.Name, &ci.Group{Labels: map[string]string{forum: "", security: ""}}).UpdateMask("labels").Context(ctx).Do(); apiCode(err) != http.StatusBadRequest {
		t.Errorf("the security label on a group holding a Google Group = %v, want 400", err)
	}
	var secs []*ci.Group
	for _, id := range []string{"r-sec-" + suffix, "r-secin-" + suffix} {
		if _, err := create(id, "", security); err != nil {
			t.Fatalf("groups.create %s: %v", id, err)
		}
		l, err := svc.Groups.Lookup().GroupKeyId(id + "@" + localDomain).Context(ctx).Do()
		if err != nil {
			t.Fatal(err)
		}
		g := &ci.Group{Name: l.Name, GroupKey: &ci.EntityKey{Id: id + "@" + localDomain}}
		t.Cleanup(func() { _, _ = svc.Groups.Delete(g.Name).Do() })
		secs = append(secs, g)
	}
	sec, secInner := secs[0], secs[1]
	for _, email := range []string{"ada-" + suffix + "@example.com", "sa-" + suffix + "@p.iam.gserviceaccount.com", secInner.GroupKey.Id} {
		addMember(t, svc, sec.Name, email)
	}
	if _, err := svc.Groups.Memberships.Create(sec.Name, &ci.Membership{PreferredMemberKey: &ci.EntityKey{Id: inner.GroupKey.Id}}).Context(ctx).Do(); apiCode(err) != http.StatusBadRequest {
		t.Errorf("a Google Group in a security group = %v, want 400", err)
	}

	// Expiry: MEMBER only; a group may expire; an expiring MEMBER may be OWNER.
	expire := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Second).Format(time.RFC3339)
	member := func(email string, roles ...*ci.MembershipRole) (*ci.Operation, error) {
		return svc.Groups.Memberships.Create(plain.Name, &ci.Membership{PreferredMemberKey: &ci.EntityKey{Id: email}, Roles: roles}).Context(ctx).Do()
	}
	if _, err := member("o1-"+suffix+"@example.com", &ci.MembershipRole{Name: "OWNER", ExpiryDetail: &ci.ExpiryDetail{ExpireTime: expire}},
		&ci.MembershipRole{Name: "MEMBER"}); apiCode(err) != http.StatusBadRequest {
		t.Errorf("an expiry on OWNER = %v, want 400", err)
	}
	if _, err := member(secInner.GroupKey.Id, &ci.MembershipRole{Name: "MEMBER", ExpiryDetail: &ci.ExpiryDetail{ExpireTime: expire}}); err != nil {
		t.Errorf("an expiring group member: %v", err)
	}
	owner := "o2-" + suffix + "@example.com"
	if _, err := member(owner, &ci.MembershipRole{Name: "OWNER"}, &ci.MembershipRole{Name: "MEMBER", ExpiryDetail: &ci.ExpiryDetail{ExpireTime: expire}}); err != nil {
		t.Fatalf("an OWNER whose MEMBER role expires: %v", err)
	}
	l, err := svc.Groups.Memberships.Lookup(plain.Name).MemberKeyId(owner).Context(ctx).Do()
	if err != nil {
		t.Fatal(err)
	}
	mod := func(req *ci.ModifyMembershipRolesRequest) (*ci.ModifyMembershipRolesResponse, error) {
		return svc.Groups.Memberships.ModifyMembershipRoles(l.Name, req).Context(ctx).Do()
	}
	if r, err := mod(&ci.ModifyMembershipRolesRequest{AddRoles: []*ci.MembershipRole{{Name: "MANAGER"}}}); err != nil || len(r.Membership.Roles) != 3 {
		t.Errorf("adding MANAGER to an expiring OWNER = %+v, %v", r, err)
	}
	if _, err := mod(&ci.ModifyMembershipRolesRequest{AddRoles: []*ci.MembershipRole{{Name: "OWNER", ExpiryDetail: &ci.ExpiryDetail{ExpireTime: expire}}},
		RemoveRoles: []string{"OWNER"}}); apiCode(err) != http.StatusBadRequest {
		t.Errorf("addRoles OWNER with an expiry = %v, want 400", err)
	}
	if _, err := mod(&ci.ModifyMembershipRolesRequest{UpdateRolesParams: []*ci.UpdateMembershipRolesParams{{FieldMask: "expiry_detail.expire_time",
		MembershipRole: &ci.MembershipRole{Name: "OWNER", ExpiryDetail: &ci.ExpiryDetail{ExpireTime: expire}}}}}); apiCode(err) != http.StatusBadRequest {
		t.Errorf("updateRolesParams on OWNER = %v, want 400", err)
	}
	later := time.Now().Add(30 * 24 * time.Hour).UTC().Truncate(time.Second).Format(time.RFC3339)
	r, err := mod(&ci.ModifyMembershipRolesRequest{UpdateRolesParams: []*ci.UpdateMembershipRolesParams{{FieldMask: "expiry_detail.expire_time",
		MembershipRole: &ci.MembershipRole{Name: "MEMBER", ExpiryDetail: &ci.ExpiryDetail{ExpireTime: later}}}}})
	if err != nil {
		t.Fatalf("updating an OWNER's MEMBER expiry: %v", err)
	}
	for _, x := range r.Membership.Roles {
		if (x.Name == "MEMBER") != (x.ExpiryDetail != nil) {
			t.Errorf("role %s expiry %+v: only MEMBER carries one", x.Name, x.ExpiryDetail)
		}
	}
}
