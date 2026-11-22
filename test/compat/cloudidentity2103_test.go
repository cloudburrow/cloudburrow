//go:build compat

package compat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	admin "google.golang.org/api/admin/directory/v1"
	ci "google.golang.org/api/cloudidentity/v1"
	iamv1 "google.golang.org/api/iam/v1"
	"google.golang.org/api/option"
	pt "google.golang.org/api/policytroubleshooter/v3"
)

const dynamicLabel = "cloudidentity.googleapis.com/groups.dynamic"

// directoryUsers is the official Admin SDK Directory client
// (google.golang.org/api/admin/directory/v1) at the Resource Manager
// endpoint, which serves the customer's users (#2103).
func directoryUsers(t *testing.T, h *Harness, rm string) *admin.Service {
	t.Helper()
	if rm == "" {
		t.Skip("no Resource Manager endpoint")
	}
	svc, err := admin.NewService(h.Context(), option.WithEndpoint("http://"+strings.TrimPrefix(rm, "http://")+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// newDirectoryUser inserts a user, deleted at the end.
func newDirectoryUser(t *testing.T, a *admin.Service, u *admin.User) *admin.User {
	t.Helper()
	u.Password = "Passw0rd-" + fmt.Sprint(time.Now().UnixNano()%1e6)
	out, err := a.Users.Insert(u).Do()
	if err != nil {
		t.Fatalf("users.insert %s: %v", u.PrimaryEmail, err)
	}
	t.Cleanup(func() { _ = a.Users.Delete(out.Id).Do() })
	return out
}

func groupMembers(t *testing.T, svc *ci.Service, group string) []string {
	t.Helper()
	l, err := svc.Groups.Memberships.List(group).Do()
	if err != nil {
		t.Fatalf("memberships.list %s: %v", group, err)
	}
	var out []string
	for _, m := range l.Memberships {
		out = append(out, m.PreferredMemberKey.Id)
	}
	slices.Sort(out)
	return out
}

// covers: cloudidentity.groups.create, cloudidentity.groups.get, cloudidentity.groups.patch, cloudidentity.groups.memberships.list, cloudidentity.groups.memberships.create
//
// TestCloudIdentityDynamicGroups2103 (#2103), with the official
// cloudidentity/v1, admin/directory/v1 and policytroubleshooter/v3
// clients: a dynamic group (the groups.dynamic label and one USER query)
// has the users its query names as MEMBERs, status UP_TO_DATE; a user
// whose attributes change joins or leaves at once; a project role granted
// to the group is its members' (Policy Troubleshooter); its memberships
// cannot be added by hand, it cannot be a member of a Google Group, and
// queries Google does not support are 400; patching the query changes
// the members.
func TestCloudIdentityDynamicGroups2103(t *testing.T) {
	h := New(t)
	rm := h.Endpoint(EnvResourceManager)
	svc := cloudIdentity(t, h, rm)
	a := directoryUsers(t, h, rm)
	sfx := fmt.Sprintf("%d", time.Now().UnixNano()%1e9)
	ann := newDirectoryUser(t, a, &admin.User{PrimaryEmail: "ann-" + sfx + "@" + localDomain, Name: &admin.UserName{GivenName: "Ann", FamilyName: "Lee"},
		Organizations: []map[string]any{{"department": "eng-" + sfx, "type": "work", "primary": true}}})
	bob := newDirectoryUser(t, a, &admin.User{PrimaryEmail: "bob-" + sfx + "@" + localDomain, Name: &admin.UserName{GivenName: "Bob", FamilyName: "Ray"},
		Addresses: []map[string]any{{"locality": "Sunnyvale-" + sfx}}})
	if u, err := a.Users.Get(ann.PrimaryEmail).Do(); err != nil || u.Id != ann.Id || u.Name.FullName != "Ann Lee" || u.OrgUnitPath != "/" {
		t.Fatalf("users.get = %+v, %v", u, err)
	}

	email := "dyn-" + sfx + "@" + localDomain
	op, err := svc.Groups.Create(&ci.Group{Parent: localCustomer, GroupKey: &ci.EntityKey{Id: email}, Labels: map[string]string{dynamicLabel: ""},
		DynamicGroupMetadata: &ci.DynamicGroupMetadata{Queries: []*ci.DynamicGroupQuery{{ResourceType: "USER",
			Query: "user.organizations.exists(org, org.department=='eng-" + sfx + "')"}}}}).InitialGroupConfig("EMPTY").Do()
	if err != nil || !op.Done {
		t.Fatalf("groups.create dynamic = %+v, %v", op, err)
	}
	var g ci.Group
	_ = json.Unmarshal(op.Response, &g)
	t.Cleanup(func() { _, _ = svc.Groups.Delete(g.Name).Do() })
	if got := groupMembers(t, svc, g.Name); !slices.Equal(got, []string{ann.PrimaryEmail}) {
		t.Fatalf("dynamic members = %v, want ann", got)
	}
	gg, err := svc.Groups.Get(g.Name).Do()
	if err != nil || gg.DynamicGroupMetadata == nil || gg.DynamicGroupMetadata.Status == nil || gg.DynamicGroupMetadata.Status.Status != "UP_TO_DATE" ||
		gg.DynamicGroupMetadata.Queries[0].ResourceType != "USER" {
		t.Fatalf("groups.get = %+v, %v", gg, err)
	}

	// IAM: a role of the group is its members'.
	project := iamProject(t, h, "dyn-")
	change := projectRoles(t, h, project, "group:"+email)
	change("roles/browser", true)
	t.Cleanup(func() { change("roles/browser", false) })
	pts, err := pt.NewService(h.Context(), option.WithEndpoint("http://"+strings.TrimPrefix(rm, "http://")+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	access := func(who string) string {
		t.Helper()
		resp, err := pts.Iam.Troubleshoot(&pt.GoogleCloudPolicytroubleshooterIamV3TroubleshootIamPolicyRequest{
			AccessTuple: &pt.GoogleCloudPolicytroubleshooterIamV3AccessTuple{Principal: who,
				FullResourceName: "//cloudresourcemanager.googleapis.com/projects/" + project, Permission: "resourcemanager.projects.get"}}).Do()
		if err != nil {
			t.Fatalf("iam:troubleshoot: %v", err)
		}
		return resp.OverallAccessState
	}
	if got := access(ann.PrimaryEmail); got != "CAN_ACCESS" {
		t.Errorf("ann, a member of the dynamic group: %s", got)
	}
	if got := access(bob.PrimaryEmail); got == "CAN_ACCESS" {
		t.Errorf("bob, not a member, can access")
	}
	// Bob joins the department: a member at once, with the role.
	if _, err := a.Users.Patch(bob.Id, &admin.User{Organizations: []map[string]any{{"department": "eng-" + sfx}}}).Do(); err != nil {
		t.Fatalf("users.patch: %v", err)
	}
	if got := groupMembers(t, svc, g.Name); len(got) != 2 {
		t.Errorf("after bob's change, members = %v", got)
	}
	if got := access(bob.PrimaryEmail); got != "CAN_ACCESS" {
		t.Errorf("bob after joining: %s", got)
	}
	// Ann leaves.
	if _, err := a.Users.Update(ann.Id, &admin.User{Organizations: []map[string]any{{"department": "sales"}}}).Do(); err != nil {
		t.Fatalf("users.update: %v", err)
	}
	if got := groupMembers(t, svc, g.Name); !slices.Equal(got, []string{bob.PrimaryEmail}) {
		t.Errorf("after ann left, members = %v", got)
	}
	if got := access(ann.PrimaryEmail); got == "CAN_ACCESS" {
		t.Errorf("ann after leaving can still access")
	}

	if _, err := svc.Groups.Memberships.Create(g.Name, &ci.Membership{PreferredMemberKey: &ci.EntityKey{Id: "x-" + sfx + "@" + localDomain}}).Do(); apiCode(err) != http.StatusBadRequest {
		t.Errorf("a membership added by hand = %v, want 400", err)
	}
	forum, _ := groupFixture(t, svc, "dynhost-"+sfx, "dynhost2-"+sfx)
	if _, err := svc.Groups.Memberships.Create(forum.Name, &ci.Membership{PreferredMemberKey: &ci.EntityKey{Id: email}}).Do(); apiCode(err) != http.StatusBadRequest {
		t.Errorf("a dynamic group as a member of a Google Group = %v, want 400", err)
	}
	for _, q := range []string{
		"user.organizations.exists(org, !(org.title=='a'))",
		"!user.organizations.exists(org, org.title=='a' && org.department=='b')",
		"user.organizations.exists(org,",
	} {
		_, err := svc.Groups.Create(&ci.Group{Parent: localCustomer, GroupKey: &ci.EntityKey{Id: "bad-" + sfx + "@" + localDomain}, Labels: map[string]string{dynamicLabel: ""},
			DynamicGroupMetadata: &ci.DynamicGroupMetadata{Queries: []*ci.DynamicGroupQuery{{ResourceType: "USER", Query: q}}}}).Do()
		if apiCode(err) != http.StatusBadRequest {
			t.Errorf("query %q = %v, want 400", q, err)
		}
	}
	if _, err := svc.Groups.Patch(g.Name, &ci.Group{DynamicGroupMetadata: &ci.DynamicGroupMetadata{Queries: []*ci.DynamicGroupQuery{{ResourceType: "USER",
		Query: "user.addresses.exists(ad, ad.locality=='Sunnyvale-" + sfx + "')"}}}}).UpdateMask("dynamic_group_metadata").Do(); err != nil {
		t.Fatalf("groups.patch dynamic_group_metadata: %v", err)
	}
	if got := groupMembers(t, svc, g.Name); !slices.Equal(got, []string{bob.PrimaryEmail}) {
		t.Errorf("after the patch, members = %v", got)
	}
}

// covers: cloudidentity.groups.create, cloudidentity.groups.list, cloudidentity.groups.lookup, cloudidentity.groups.memberships.create, cloudidentity.groups.memberships.lookup
//
// TestCloudIdentityMappedGroups2103 (#2103): an identity-mapped group of
// a workforce pool's identity source (identitysources/POOL, label
// system/groups/external), with a member of that source; a source that
// does not exist is 403, a Google member 400; listed under its source,
// not the customer.
func TestCloudIdentityMappedGroups2103(t *testing.T) {
	h := New(t)
	rm := h.Endpoint(EnvResourceManager)
	svc := cloudIdentity(t, h, rm)
	pools := iamREST(t, h).Locations.WorkforcePools
	id := wfPoolID(h, "cim-")
	src := "identitysources/" + id
	mapped := func() (*ci.Operation, error) {
		return svc.Groups.Create(&ci.Group{Parent: src, GroupKey: &ci.EntityKey{Id: "engineering", Namespace: src},
			DisplayName: "Engineering (IdP)", Labels: map[string]string{"system/groups/external": ""}}).Do()
	}
	if _, err := mapped(); apiCode(err) != http.StatusForbidden {
		t.Fatalf("a group of a missing source = %v, want 403", err)
	}
	if _, err := pools.Create("locations/global", &iamv1.WorkforcePool{Parent: localOrgRoles}).WorkforcePoolId(id).Do(); err != nil {
		t.Fatalf("workforcePools.create: %v", err)
	}
	t.Cleanup(func() { _, _ = pools.Delete("locations/global/workforcePools/" + id).Do() })
	op, err := mapped()
	if err != nil {
		t.Fatalf("groups.create mapped: %v", err)
	}
	var g ci.Group
	_ = json.Unmarshal(op.Response, &g)
	t.Cleanup(func() { _, _ = svc.Groups.Delete(g.Name).Do() })
	if g.Parent != src || g.GroupKey.Namespace != src {
		t.Errorf("mapped group = %+v", g)
	}
	if _, err := svc.Groups.Memberships.Create(g.Name, &ci.Membership{PreferredMemberKey: &ci.EntityKey{Id: "alice", Namespace: src}}).Do(); err != nil {
		t.Fatalf("memberships.create mapped: %v", err)
	}
	if _, err := svc.Groups.Memberships.Create(g.Name, &ci.Membership{PreferredMemberKey: &ci.EntityKey{Id: "bob@" + localDomain}}).Do(); apiCode(err) != http.StatusBadRequest {
		t.Errorf("a Google member of a mapped group = %v, want 400", err)
	}
	if l, err := svc.Groups.List().Parent(src).Do(); err != nil || len(l.Groups) != 1 || l.Groups[0].Name != g.Name {
		t.Errorf("groups.list of the source = %+v, %v", l, err)
	}
	if lk, err := svc.Groups.Lookup().GroupKeyId("engineering").GroupKeyNamespace(src).Do(); err != nil || lk.Name != g.Name {
		t.Errorf("groups.lookup = %+v, %v", lk, err)
	}
	if ml, err := svc.Groups.Memberships.Lookup(g.Name).MemberKeyId("alice").MemberKeyNamespace(src).Do(); err != nil || ml.Name == "" {
		t.Errorf("memberships.lookup = %+v, %v", ml, err)
	}
	if l, err := svc.Groups.List().Parent(localCustomer).Do(); err == nil {
		for _, x := range l.Groups {
			if x.Name == g.Name {
				t.Errorf("the mapped group is listed under the customer")
			}
		}
	}
}

// covers: cloudidentity.devices.create, cloudidentity.devices.get, cloudidentity.devices.list, cloudidentity.devices.wipe, cloudidentity.devices.cancelWipe, cloudidentity.devices.delete
// covers: cloudidentity.devices.deviceUsers.get, cloudidentity.devices.deviceUsers.list, cloudidentity.devices.deviceUsers.approve, cloudidentity.devices.deviceUsers.block, cloudidentity.devices.deviceUsers.wipe, cloudidentity.devices.deviceUsers.cancelWipe, cloudidentity.devices.deviceUsers.delete, cloudidentity.devices.deviceUsers.lookup
// covers: cloudidentity.devices.deviceUsers.clientStates.patch, cloudidentity.devices.deviceUsers.clientStates.get, cloudidentity.devices.deviceUsers.clientStates.list
//
// TestCloudIdentityDevices2103 (#2103): a company-owned device imported
// (UNPROVISIONED), a managed user signing in on it (the device's side,
// /cloudburrow/v1/devices:signIn: PENDING_APPROVAL, the device PENDING),
// approved, blocked, wiped and the wipe cancelled; a client state of the
// customer's ID patched, read and listed, a stale etag 409; the lists'
// filters and views; lookup without end-user credentials 403; a device
// wipe cancelled, then carried out at its next sync and no longer
// cancellable; the device deleted.
func TestCloudIdentityDevices2103(t *testing.T) {
	h := New(t)
	rm := h.Endpoint(EnvResourceManager)
	svc := cloudIdentity(t, h, rm)
	a := directoryUsers(t, h, rm)
	base := "http://" + strings.TrimPrefix(rm, "http://")
	sfx := fmt.Sprintf("%d", time.Now().UnixNano()%1e9)
	ann := newDirectoryUser(t, a, &admin.User{PrimaryEmail: "dev-" + sfx + "@" + localDomain, Name: &admin.UserName{GivenName: "Dev", FamilyName: "User"}})
	serial := "SN-" + sfx
	op, err := svc.Devices.Create(&ci.GoogleAppsCloudidentityDevicesV1Device{SerialNumber: serial, DeviceType: "ANDROID", AssetTag: "tag-" + sfx}).Customer("customers/my_customer").Do()
	if err != nil || !op.Done {
		t.Fatalf("devices.create = %+v, %v", op, err)
	}
	var d ci.GoogleAppsCloudidentityDevicesV1Device
	_ = json.Unmarshal(op.Response, &d)
	t.Cleanup(func() { _, _ = svc.Devices.Delete(d.Name).Do() })
	if d.OwnerType != "COMPANY" || d.ManagementState != "UNPROVISIONED" || !strings.HasPrefix(d.Name, "devices/") {
		t.Fatalf("device = %+v", d)
	}
	if _, err := svc.Devices.Create(&ci.GoogleAppsCloudidentityDevicesV1Device{SerialNumber: serial, DeviceType: "ANDROID"}).Do(); apiCode(err) != http.StatusConflict {
		t.Errorf("a second device of the serial = %v, want 409", err)
	}
	body, _ := json.Marshal(map[string]string{"userEmail": ann.PrimaryEmail, "serialNumber": serial, "deviceType": "ANDROID", "deviceId": "aid-" + sfx, "model": "Pixel 9"})
	resp, err := http.Post(base+"/cloudburrow/v1/devices:signIn", "application/json", bytes.NewReader(body))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("sign in = %v, %v", resp, err)
	}
	var du ci.GoogleAppsCloudidentityDevicesV1DeviceUser
	_ = json.NewDecoder(resp.Body).Decode(&du)
	resp.Body.Close()
	if du.ManagementState != "PENDING_APPROVAL" || du.UserEmail != ann.PrimaryEmail {
		t.Fatalf("device user = %+v", du)
	}
	if got, err := svc.Devices.Get(d.Name).Do(); err != nil || got.ManagementState != "PENDING" || got.Model != "Pixel 9" {
		t.Errorf("devices.get after sign-in = %+v, %v", got, err)
	}
	state := func() string {
		t.Helper()
		u, err := svc.Devices.DeviceUsers.Get(du.Name).Do()
		if err != nil {
			t.Fatalf("deviceUsers.get: %v", err)
		}
		return u.ManagementState
	}
	for _, s := range []struct {
		what string
		do   func() (*ci.Operation, error)
		want string
	}{
		{"approve", func() (*ci.Operation, error) {
			return svc.Devices.DeviceUsers.Approve(du.Name, &ci.GoogleAppsCloudidentityDevicesV1ApproveDeviceUserRequest{Customer: "customers/my_customer"}).Do()
		}, "APPROVED"},
		{"block", func() (*ci.Operation, error) {
			return svc.Devices.DeviceUsers.Block(du.Name, &ci.GoogleAppsCloudidentityDevicesV1BlockDeviceUserRequest{}).Do()
		}, "BLOCKED"},
		{"approve again", func() (*ci.Operation, error) {
			return svc.Devices.DeviceUsers.Approve(du.Name, &ci.GoogleAppsCloudidentityDevicesV1ApproveDeviceUserRequest{}).Do()
		}, "APPROVED"},
		{"wipe", func() (*ci.Operation, error) {
			return svc.Devices.DeviceUsers.Wipe(du.Name, &ci.GoogleAppsCloudidentityDevicesV1WipeDeviceUserRequest{}).Do()
		}, "WIPING"},
		{"cancelWipe", func() (*ci.Operation, error) {
			return svc.Devices.DeviceUsers.CancelWipe(du.Name, &ci.GoogleAppsCloudidentityDevicesV1CancelWipeDeviceUserRequest{}).Do()
		}, "APPROVED"},
	} {
		op, err := s.do()
		if err != nil || !op.Done {
			t.Fatalf("%s = %+v, %v", s.what, op, err)
		}
		if got := state(); got != s.want {
			t.Errorf("after %s, managementState = %s, want %s", s.what, got, s.want)
		}
	}
	if _, err := svc.Devices.DeviceUsers.CancelWipe(du.Name, &ci.GoogleAppsCloudidentityDevicesV1CancelWipeDeviceUserRequest{}).Do(); apiCode(err) != http.StatusBadRequest {
		t.Errorf("cancelWipe with no wipe = %v, want 400", err)
	}

	cs := du.Name + "/clientStates/0cloudburrow-scanner"
	if _, err := svc.Devices.DeviceUsers.ClientStates.Patch(cs, &ci.GoogleAppsCloudidentityDevicesV1ClientState{ComplianceState: "COMPLIANT", HealthScore: "GOOD",
		CustomId: "asset-1", KeyValuePairs: map[string]ci.GoogleAppsCloudidentityDevicesV1CustomAttributeValue{"disk": {StringValue: "encrypted"}}}).Do(); err != nil {
		t.Fatalf("clientStates.patch: %v", err)
	}
	got, err := svc.Devices.DeviceUsers.ClientStates.Get(cs).Do()
	if err != nil || got.ComplianceState != "COMPLIANT" || got.OwnerType != "OWNER_TYPE_CUSTOMER" || got.Etag == "" || got.KeyValuePairs["disk"].StringValue != "encrypted" {
		t.Fatalf("clientStates.get = %+v, %v", got, err)
	}
	if _, err := svc.Devices.DeviceUsers.ClientStates.Patch(cs, &ci.GoogleAppsCloudidentityDevicesV1ClientState{HealthScore: "POOR", Etag: got.Etag}).UpdateMask("healthScore").Do(); err != nil {
		t.Errorf("clientStates.patch with its etag: %v", err)
	}
	if _, err := svc.Devices.DeviceUsers.ClientStates.Patch(cs, &ci.GoogleAppsCloudidentityDevicesV1ClientState{HealthScore: "GOOD", Etag: got.Etag}).UpdateMask("healthScore").Do(); apiCode(err) != http.StatusConflict {
		t.Errorf("clientStates.patch with a stale etag = %v, want 409", err)
	}
	if _, err := svc.Devices.DeviceUsers.ClientStates.Patch(du.Name+"/clientStates/C99-other", &ci.GoogleAppsCloudidentityDevicesV1ClientState{}).Do(); apiCode(err) != http.StatusBadRequest {
		t.Errorf("another customer's client state ID = %v, want 400", err)
	}
	if l, err := svc.Devices.DeviceUsers.ClientStates.List(du.Name).Do(); err != nil || len(l.ClientStates) != 1 {
		t.Errorf("clientStates.list = %+v, %v", l, err)
	}

	if l, err := svc.Devices.List().Filter("serial:" + serial + " status:approved type:android").Do(); err != nil || len(l.Devices) != 1 {
		t.Errorf("devices.list filtered = %+v, %v", l, err)
	}
	if l, err := svc.Devices.List().Filter("serial:" + serial + " owner:byod").Do(); err != nil || len(l.Devices) != 0 {
		t.Errorf("devices.list owner:byod = %+v, %v", l, err)
	}
	if l, err := svc.Devices.List().View("USER_ASSIGNED_DEVICES").Filter(serial).Do(); err != nil || len(l.Devices) != 1 || l.Devices[0].AssetTag != "" {
		t.Errorf("USER_ASSIGNED_DEVICES = %+v, %v", l, err)
	}
	if l, err := svc.Devices.DeviceUsers.List("devices/-").Filter("email:" + ann.PrimaryEmail).Do(); err != nil || len(l.DeviceUsers) != 1 {
		t.Errorf("deviceUsers.list = %+v, %v", l, err)
	}
	if _, err := svc.Devices.DeviceUsers.Lookup("devices/-/deviceUsers").UserId("me").AndroidId("aid-" + sfx).Do(); apiCode(err) != http.StatusForbidden {
		t.Errorf("lookup with the admin's credentials = %v, want 403", err)
	}

	if _, err := svc.Devices.Wipe(d.Name, &ci.GoogleAppsCloudidentityDevicesV1WipeDeviceRequest{RemoveResetLock: true}).Do(); err != nil {
		t.Fatalf("devices.wipe: %v", err)
	}
	if g, _ := svc.Devices.Get(d.Name).Do(); g == nil || g.ManagementState != "WIPING" {
		t.Errorf("after wipe = %+v", g)
	}
	if _, err := svc.Devices.CancelWipe(d.Name, &ci.GoogleAppsCloudidentityDevicesV1CancelWipeDeviceRequest{}).Do(); err != nil {
		t.Fatalf("devices.cancelWipe: %v", err)
	}
	if g, _ := svc.Devices.Get(d.Name).Do(); g == nil || g.ManagementState != "APPROVED" {
		t.Errorf("after cancelWipe = %+v", g)
	}
	if _, err := svc.Devices.Wipe(d.Name, &ci.GoogleAppsCloudidentityDevicesV1WipeDeviceRequest{}).Do(); err != nil {
		t.Fatalf("devices.wipe: %v", err)
	}
	resp, err = http.Post(base+"/cloudburrow/v1/"+d.Name+":sync", "application/json", nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("sync = %v, %v", resp, err)
	}
	resp.Body.Close()
	if _, err := svc.Devices.CancelWipe(d.Name, &ci.GoogleAppsCloudidentityDevicesV1CancelWipeDeviceRequest{}).Do(); apiCode(err) != http.StatusBadRequest {
		t.Errorf("cancelWipe after the device was wiped = %v, want 400", err)
	}
	if got := state(); got != "WIPED" {
		t.Errorf("device user after the device's wipe = %s", got)
	}
	if _, err := svc.Devices.DeviceUsers.Delete(du.Name).Do(); err != nil {
		t.Errorf("deviceUsers.delete: %v", err)
	}
	if _, err := svc.Devices.Delete(d.Name).Do(); err != nil {
		t.Fatalf("devices.delete: %v", err)
	}
	if _, err := svc.Devices.Get(d.Name).Do(); apiCode(err) != http.StatusNotFound {
		t.Errorf("devices.get of a deleted device = %v, want 404", err)
	}
}

// covers: cloudidentity.customers.userinvitations.isInvitableUser, cloudidentity.customers.userinvitations.send, cloudidentity.customers.userinvitations.get, cloudidentity.customers.userinvitations.list, cloudidentity.customers.userinvitations.cancel
// covers: cloudidentity.policies.create, cloudidentity.policies.get, cloudidentity.policies.list, cloudidentity.policies.patch, cloudidentity.policies.delete
// covers: cloudidentity.allowlistedDomains.create, cloudidentity.allowlistedDomains.get, cloudidentity.allowlistedDomains.list, cloudidentity.allowlistedDomains.delete
//
// TestCloudIdentityInvitationsPolicies2103 (#2103): an unmanaged address
// of the customer's domain is invitable, another domain's not; send makes
// it INVITED (mailsSentCount 1), list filters by state, cancel takes it
// back to NOT_YET_SENT; accepted (the user's side), the account is a
// managed user and no longer invitable. A policy of the root
// organizational unit is created (ADMIN, sortOrder 1), filtered by its
// setting type, its value patched, its type immutable, deleted. An
// allowlisted domain is created, a second of it 409, listed, read and
// deleted.
func TestCloudIdentityInvitationsPolicies2103(t *testing.T) {
	h := New(t)
	rm := h.Endpoint(EnvResourceManager)
	svc := cloudIdentity(t, h, rm)
	a := directoryUsers(t, h, rm)
	base := "http://" + strings.TrimPrefix(rm, "http://")
	sfx := fmt.Sprintf("%d", time.Now().UnixNano()%1e9)
	email := "inv-" + sfx + "@" + localDomain
	name := localCustomer + "/userinvitations/" + email
	inv := svc.Customers.Userinvitations
	if v, err := inv.IsInvitableUser(name).Do(); err != nil || !v.IsInvitableUser {
		t.Fatalf("isInvitableUser = %+v, %v", v, err)
	}
	if v, err := inv.IsInvitableUser(localCustomer + "/userinvitations/x-" + sfx + "@example.com").Do(); err != nil || v.IsInvitableUser {
		t.Errorf("another domain's isInvitableUser = %+v, %v", v, err)
	}
	if _, err := inv.Get(name).Do(); apiCode(err) != http.StatusNotFound {
		t.Errorf("get before send = %v, want 404", err)
	}
	if op, err := inv.Send(name, &ci.SendUserInvitationRequest{}).Do(); err != nil || !op.Done {
		t.Fatalf("send = %+v, %v", op, err)
	}
	if got, err := inv.Get(name).Do(); err != nil || got.State != "INVITED" || got.MailsSentCount != 1 {
		t.Errorf("get after send = %+v, %v", got, err)
	}
	listed := func(filter string) bool {
		t.Helper()
		l, err := inv.List(localCustomer).Filter(filter).OrderBy("email desc").Do()
		if err != nil {
			t.Fatalf("list %q: %v", filter, err)
		}
		for _, x := range l.UserInvitations {
			if x.Name == name {
				return true
			}
		}
		return false
	}
	if !listed("state=='invited'") || listed("state=='accepted'||state=='declined'") || !listed("state!='accepted'") {
		t.Errorf("list's state filters")
	}
	if _, err := inv.Cancel(name, &ci.CancelUserInvitationRequest{}).Do(); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got, _ := inv.Get(name).Do(); got == nil || got.State != "NOT_YET_SENT" {
		t.Errorf("after cancel = %+v", got)
	}
	if _, err := inv.Send(name, &ci.SendUserInvitationRequest{}).Do(); err != nil {
		t.Fatalf("send again: %v", err)
	}
	resp, err := http.Post(base+"/cloudburrow/v1/"+name+":accept", "application/json", strings.NewReader(`{"givenName":"Inv","familyName":"Tee"}`))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("accept = %v, %v", resp, err)
	}
	resp.Body.Close()
	t.Cleanup(func() { _ = a.Users.Delete(email).Do() })
	if u, err := a.Users.Get(email).Do(); err != nil || u.Name.GivenName != "Inv" {
		t.Errorf("the accepted account = %+v, %v", u, err)
	}
	if got, _ := inv.Get(name).Do(); got == nil || got.State != "ACCEPTED" || got.MailsSentCount != 2 {
		t.Errorf("after accept = %+v", got)
	}
	if v, _ := inv.IsInvitableUser(name).Do(); v == nil || v.IsInvitableUser {
		t.Errorf("a managed user is invitable")
	}

	stype := "settings/security.password_" + sfx
	stype = strings.ReplaceAll(stype, "-", "_")
	op, err := svc.Policies.Create(&ci.Policy{Customer: localCustomer, PolicyQuery: &ci.PolicyQuery{OrgUnit: "orgUnits/03cloudburrowroot"},
		Setting: &ci.Setting{Type: stype, Value: []byte(`{"minimumLength":12}`)}}).Do()
	if err != nil || !op.Done {
		t.Fatalf("policies.create = %+v, %v", op, err)
	}
	var p ci.Policy
	_ = json.Unmarshal(op.Response, &p)
	t.Cleanup(func() { _, _ = svc.Policies.Delete(p.Name).Do() })
	if p.Type != "ADMIN" || p.PolicyQuery.SortOrder != 1 || p.Customer != localCustomer {
		t.Errorf("policy = %+v", p)
	}
	if _, err := svc.Policies.Create(&ci.Policy{PolicyQuery: &ci.PolicyQuery{OrgUnit: "orgUnits/nosuch"},
		Setting: &ci.Setting{Type: stype, Value: []byte(`{}`)}}).Do(); apiCode(err) != http.StatusBadRequest {
		t.Errorf("an unknown org unit = %v, want 400", err)
	}
	found := func(filter string) bool {
		t.Helper()
		l, err := svc.Policies.List().Filter(filter).Do()
		if err != nil {
			t.Fatalf("policies.list %q: %v", filter, err)
		}
		for _, x := range l.Policies {
			if x.Name == p.Name {
				return true
			}
		}
		return false
	}
	if !found(`setting.type.matches('^settings/security\\..*$')`) || found(`setting.type.matches('^settings/gmail\\..*$')`) ||
		!found(`customer == "`+localCustomer+`"`) {
		t.Errorf("policies.list filters")
	}
	if _, err := svc.Policies.Patch(p.Name, &ci.Policy{Setting: &ci.Setting{Value: []byte(`{"minimumLength":14}`)}}).Do(); err != nil {
		t.Fatalf("policies.patch: %v", err)
	}
	if got, err := svc.Policies.Get(p.Name).Do(); err != nil || !strings.Contains(string(got.Setting.Value), "14") {
		t.Errorf("policies.get after patch = %+v, %v", got, err)
	}
	if _, err := svc.Policies.Patch(p.Name, &ci.Policy{Setting: &ci.Setting{Type: "settings/other.x", Value: []byte(`{}`)}}).Do(); apiCode(err) != http.StatusBadRequest {
		t.Errorf("a changed setting.type = %v, want 400", err)
	}
	if _, err := svc.Policies.Delete(p.Name).Do(); err != nil {
		t.Fatalf("policies.delete: %v", err)
	}
	if _, err := svc.Policies.Get(p.Name).Do(); apiCode(err) != http.StatusNotFound {
		t.Errorf("a deleted policy = %v, want 404", err)
	}

	dom := "partner-" + sfx + ".example.com"
	op, err = svc.AllowlistedDomains.Create(&ci.AllowlistedDomain{Domain: dom}).Do()
	if err != nil || !op.Done {
		t.Fatalf("allowlistedDomains.create = %+v, %v", op, err)
	}
	var d ci.AllowlistedDomain
	_ = json.Unmarshal(op.Response, &d)
	if _, err := svc.AllowlistedDomains.Create(&ci.AllowlistedDomain{Domain: dom}).Do(); apiCode(err) != http.StatusConflict {
		t.Errorf("a second allowlisting = %v, want 409", err)
	}
	if got, err := svc.AllowlistedDomains.Get(d.Name).Do(); err != nil || got.Domain != dom {
		t.Errorf("allowlistedDomains.get = %+v, %v", got, err)
	}
	if l, err := svc.AllowlistedDomains.List().Do(); err != nil || !slices.ContainsFunc(l.AllowlistedDomains, func(x *ci.AllowlistedDomain) bool { return x.Name == d.Name }) {
		t.Errorf("allowlistedDomains.list = %+v, %v", l, err)
	}
	if _, err := svc.AllowlistedDomains.Delete(d.Name).Do(); err != nil {
		t.Fatalf("allowlistedDomains.delete: %v", err)
	}
}

// TestGcloudIdentityDynamicGroups2103 (#2103): gcloud identity groups
// create --group-type=dynamic --dynamic-user-query makes a dynamic group
// whose memberships list is its query's users, and groups update
// --dynamic-user-query changes them.
func TestGcloudIdentityDynamicGroups2103(t *testing.T) {
	h := New(t)
	rm := h.Endpoint(EnvResourceManager)
	a := directoryUsers(t, h, rm)
	g := newGcloudSession(t, h)
	customer := strings.TrimPrefix(localCustomer, "customers/")
	sfx := fmt.Sprintf("%d", time.Now().UnixNano()%1e9)
	u := newDirectoryUser(t, a, &admin.User{PrimaryEmail: "gcd-" + sfx + "@" + localDomain, Name: &admin.UserName{GivenName: "G", FamilyName: "D"},
		Organizations: []map[string]any{{"title": "SWE" + sfx}}})
	email := "gcdyn-" + sfx + "@" + localDomain
	g.must("identity", "groups", "create", email, "--customer", customer, "--group-type", "dynamic",
		"--dynamic-user-query", "user.organizations.exists(org,org.title=='SWE"+sfx+"')", "--display-name", "dynamic by gcloud")
	t.Cleanup(func() { _, _ = g.run(nil, "identity", "groups", "delete", email, "--quiet") })
	if out := g.must("identity", "groups", "describe", email, "--format=value(dynamicGroupMetadata.status.status,labels)"); !strings.Contains(out, "UP_TO_DATE") ||
		!strings.Contains(out, "groups.dynamic") {
		t.Errorf("describe = %q", out)
	}
	members := func() string {
		return g.must("identity", "groups", "memberships", "list", "--group-email", email, "--format=value(preferredMemberKey.id)")
	}
	if out := members(); !strings.Contains(out, u.PrimaryEmail) {
		t.Errorf("memberships list = %q; want %s", out, u.PrimaryEmail)
	}
	g.must("identity", "groups", "update", email, "--dynamic-user-query", "user.organizations.exists(org,org.title=='nobody"+sfx+"')")
	if out := members(); strings.Contains(out, u.PrimaryEmail) {
		t.Errorf("memberships list after update = %q", out)
	}
}
