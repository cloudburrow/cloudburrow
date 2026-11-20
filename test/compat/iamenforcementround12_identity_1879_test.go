//go:build compat

package compat

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	ci "google.golang.org/api/cloudidentity/v1"
	"google.golang.org/api/googleapi"
	iamv1 "google.golang.org/api/iam/v1"
)

// wantAPIDenied fails unless err is a 403 whose message is msg.
func wantAPIDenied(t *testing.T, what string, err error, msg string) {
	t.Helper()
	var g *googleapi.Error
	if !errors.As(err, &g) || g.Code != http.StatusForbidden || g.Message != msg {
		t.Fatalf("%s = %v; want 403 %q", what, err, msg)
	}
}

// ciDenied is Cloud Identity's refusal of resource, as for one that does
// not exist.
func ciDenied(resource string) string {
	return "Error(2028): Permission denied for resource " + resource + " (or it may not exist)."
}

// covers: iam.projects.locations.workloadIdentityPools.create, iam.projects.locations.workloadIdentityPools.get, iam.projects.locations.workloadIdentityPools.patch, iam.projects.locations.workloadIdentityPools.delete, iam.projects.locations.workloadIdentityPools.providers.list
//
// TestIAMEnforcementWorkloadIdentity (#1879), with the official
// google.golang.org/api/iam/v1 client and a minted token: without a role,
// a pool's get (iam.workloadIdentityPools.get) and its providers' list
// (iam.workloadIdentityPoolProviders.list) are refused with the IAM API's
// words; Workload Identity Pool Viewer on the project lets them through,
// not a patch (iam.workloadIdentityPools.update), which Workload Identity
// Pool Admin grants. The instance's own account is never refused.
func TestIAMEnforcementWorkloadIdentity(t *testing.T) {
	h := New(t)
	rm := h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iowif12-")
	p := newIAMPrincipal(t, h, project, "iowif12")
	owner := iamREST(t, h)
	as, err := iamv1.NewService(ctx, restAs(rm, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	pools := "projects/" + project + "/locations/global"
	id := fmt.Sprintf("pool-%d", time.Now().UnixNano()%1e8)
	pool := pools + "/workloadIdentityPools/" + id
	if _, err := owner.Projects.Locations.WorkloadIdentityPools.Create(pools, &iamv1.WorkloadIdentityPool{DisplayName: "enforced"}).
		WorkloadIdentityPoolId(id).Context(ctx).Do(); err != nil {
		t.Fatalf("the instance's own account: pools.create = %v", err)
	}
	t.Cleanup(func() { _, _ = owner.Projects.Locations.WorkloadIdentityPools.Delete(pool).Do() })
	if _, err := owner.Projects.Locations.WorkloadIdentityPools.Get(pool).Context(ctx).Do(); err != nil {
		t.Fatalf("the instance's own account: pools.get = %v", err)
	}
	get := func() error {
		_, err := as.Projects.Locations.WorkloadIdentityPools.Get(pool).Context(ctx).Do()
		return err
	}
	providers := func() error {
		_, err := as.Projects.Locations.WorkloadIdentityPools.Providers.List(pool).Context(ctx).Do()
		return err
	}
	patch := func() error {
		_, err := as.Projects.Locations.WorkloadIdentityPools.Patch(pool, &iamv1.WorkloadIdentityPool{Description: "patched"}).
			UpdateMask("description").Context(ctx).Do()
		return err
	}
	wantAPIDenied(t, "pools.get without a role", get(), iamDenied("iam.workloadIdentityPools.get"))
	wantAPIDenied(t, "providers.list without a role", providers(), iamDenied("iam.workloadIdentityPoolProviders.list"))

	p.grant("roles/iam.workloadIdentityPoolViewer")
	t.Cleanup(func() { p.revoke("roles/iam.workloadIdentityPoolViewer") })
	waitGranted(t, "pools.get as a Workload Identity Pool Viewer", get)
	if err := providers(); err != nil {
		t.Errorf("providers.list as a Workload Identity Pool Viewer: %v", err)
	}
	wantAPIDenied(t, "pools.patch as a viewer", patch(), iamDenied("iam.workloadIdentityPools.update"))

	p.grant("roles/iam.workloadIdentityPoolAdmin")
	t.Cleanup(func() { p.revoke("roles/iam.workloadIdentityPoolAdmin") })
	waitGranted(t, "pools.patch as a Workload Identity Pool Admin", patch)
	if _, err := owner.Projects.Locations.WorkloadIdentityPools.Delete(pool).Context(ctx).Do(); err != nil {
		t.Errorf("the instance's own account: pools.delete = %v", err)
	}
}

// covers: cloudidentity.groups.create, cloudidentity.groups.get, cloudidentity.groups.list, cloudidentity.groups.patch, cloudidentity.groups.delete, cloudidentity.groups.memberships.create, cloudidentity.groups.memberships.list, cloudidentity.groups.memberships.modifyMembershipRoles, cloudidentity.inboundSamlSsoProfiles.list
//
// TestIAMEnforcementCloudIdentity (#1879), with the official
// google.golang.org/api/cloudidentity/v1 client and a minted token: Cloud
// Identity keeps its own model, the group's roles. A caller in no group
// is refused a group's get with Cloud Identity's "Permission denied for
// resource" (as for a missing group), and a list leaves the group out; a
// MEMBER may read the group and its memberships but not patch it or add
// a member; a MANAGER may do both, but not make an owner or delete the
// group; a caller may create a group WITH_INITIAL_OWNER, which it owns
// and may delete. Inbound SSO profiles are the customer's admin's alone.
// The instance's own account (the customer's admin) is never refused.
func TestIAMEnforcementCloudIdentity(t *testing.T) {
	h := New(t)
	rm := h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	owner := cloudIdentity(t, h, rm)
	project := iamProject(t, h, "ioci12-")
	p := newIAMPrincipal(t, h, project, "ioci12")
	as, err := ci.NewService(ctx, restAs(rm, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1e8)
	group, _ := groupFixture(t, owner, "io12-a-"+suffix, "io12-b-"+suffix)
	label := map[string]string{"cloudidentity.googleapis.com/groups.discussion_forum": ""}
	get := func() error { _, err := as.Groups.Get(group.Name).Context(ctx).Do(); return err }
	lists := func() bool {
		l, err := as.Groups.List().Parent(localCustomer).Context(ctx).Do()
		if err != nil {
			t.Fatalf("groups.list: %v", err)
		}
		for _, g := range l.Groups {
			if g.Name == group.Name {
				return true
			}
		}
		return false
	}
	patch := func() error {
		_, err := as.Groups.Patch(group.Name, &ci.Group{Description: "patched"}).UpdateMask("description").Context(ctx).Do()
		return err
	}
	add := func(email string, roles ...string) error {
		m := &ci.Membership{PreferredMemberKey: &ci.EntityKey{Id: email}}
		for _, r := range roles {
			m.Roles = append(m.Roles, &ci.MembershipRole{Name: r})
		}
		_, err := as.Groups.Memberships.Create(group.Name, m).Context(ctx).Do()
		return err
	}

	wantAPIDenied(t, "groups.get in no group", get(), ciDenied(group.Name))
	if lists() {
		t.Error("groups.list in no group lists the group")
	}
	mine := addMember(t, owner, group.Name, p.email)
	if err := get(); err != nil {
		t.Errorf("groups.get as a MEMBER: %v", err)
	}
	if !lists() {
		t.Error("groups.list as a MEMBER leaves the group out")
	}
	if _, err := as.Groups.Memberships.List(group.Name).Context(ctx).Do(); err != nil {
		t.Errorf("memberships.list as a MEMBER: %v", err)
	}
	wantAPIDenied(t, "groups.patch as a MEMBER", patch(), ciDenied(group.Name))
	wantAPIDenied(t, "memberships.create as a MEMBER", add("io12-x-"+suffix+"@"+localDomain, "MEMBER"), ciDenied(group.Name))

	if _, err := owner.Groups.Memberships.ModifyMembershipRoles(mine.Name, &ci.ModifyMembershipRolesRequest{
		AddRoles: []*ci.MembershipRole{{Name: "MANAGER"}}}).Context(ctx).Do(); err != nil {
		t.Fatalf("the instance's own account: modifyMembershipRoles = %v", err)
	}
	if err := patch(); err != nil {
		t.Errorf("groups.patch as a MANAGER: %v", err)
	}
	if err := add("io12-x-"+suffix+"@"+localDomain, "MEMBER"); err != nil {
		t.Errorf("memberships.create as a MANAGER: %v", err)
	}
	wantAPIDenied(t, "memberships.create of an OWNER as a MANAGER", add("io12-y-"+suffix+"@"+localDomain, "MEMBER", "OWNER"), ciDenied(group.Name))
	_, err = as.Groups.Memberships.ModifyMembershipRoles(mine.Name, &ci.ModifyMembershipRolesRequest{
		AddRoles: []*ci.MembershipRole{{Name: "OWNER"}}}).Context(ctx).Do()
	wantAPIDenied(t, "modifyMembershipRoles to OWNER as a MANAGER", err, ciDenied(mine.Name))
	_, err = as.Groups.Delete(group.Name).Context(ctx).Do()
	wantAPIDenied(t, "groups.delete as a MANAGER", err, ciDenied(group.Name))

	// A group the caller creates is its own.
	email := "io12-own-" + suffix + "@" + localDomain
	if _, err := as.Groups.Create(&ci.Group{Parent: localCustomer, GroupKey: &ci.EntityKey{Id: email}, Labels: label}).
		InitialGroupConfig("WITH_INITIAL_OWNER").Context(ctx).Do(); err != nil {
		t.Fatalf("groups.create WITH_INITIAL_OWNER: %v", err)
	}
	look, err := owner.Groups.Lookup().GroupKeyId(email).Context(ctx).Do()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = owner.Groups.Delete(look.Name).Do() })
	if _, err := as.Groups.Delete(look.Name).Context(ctx).Do(); err != nil {
		t.Errorf("groups.delete of its own group: %v", err)
	}

	_, err = as.InboundSamlSsoProfiles.List().Filter(`customer=="` + localCustomer + `"`).Context(ctx).Do()
	wantAPIDenied(t, "inboundSamlSsoProfiles.list as a group manager", err, "The caller does not have permission")
	if _, err := owner.InboundSamlSsoProfiles.List().Filter(`customer=="` + localCustomer + `"`).Context(ctx).Do(); err != nil {
		t.Errorf("the instance's own account: inboundSamlSsoProfiles.list = %v", err)
	}
	if _, err := owner.Groups.Get(group.Name).Context(ctx).Do(); err != nil || !strings.HasPrefix(group.Name, "groups/") {
		t.Errorf("the instance's own account: groups.get = %v", err)
	}
}
