package resourcemanager

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	admin "google.golang.org/api/admin/directory/v1"
	ci "google.golang.org/api/cloudidentity/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

func ci2103(t *testing.T) (*Registry, *ci.Service, *admin.Service, string) {
	t.Helper()
	r := New(store.NewMemory())
	srv := httptest.NewServer(r.IdentityHandler())
	t.Cleanup(srv.Close)
	ctx := context.Background()
	c, err := ci.NewService(ctx, option.WithEndpoint(srv.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	a, err := admin.NewService(ctx, option.WithEndpoint(srv.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	return r, c, a, srv.URL
}

func httpCode2103(err error) int {
	if e, ok := err.(*googleapi.Error); ok {
		return e.Code
	}
	return 0
}

// TestDynamicGroups2103: a dynamic group's members are its query's users,
// following their attributes; the query rules and limits are applied.
func TestDynamicGroups2103(t *testing.T) {
	r, c, a, _ := ci2103(t)
	for _, u := range []*admin.User{
		{PrimaryEmail: "ann@" + OrganizationDomain, Name: &admin.UserName{GivenName: "Ann", FamilyName: "Lee"}, Password: "password-1",
			Organizations: []map[string]any{{"department": "engineering", "type": "work", "primary": true}}},
		{PrimaryEmail: "bob@" + OrganizationDomain, Name: &admin.UserName{GivenName: "Bob", FamilyName: "Ray"}, Password: "password-2",
			Addresses: []map[string]any{{"locality": "Sunnyvale"}}},
	} {
		if _, err := a.Users.Insert(u).Do(); err != nil {
			t.Fatal(err)
		}
	}
	op, err := c.Groups.Create(&ci.Group{Parent: CustomerName, GroupKey: &ci.EntityKey{Id: "eng@" + OrganizationDomain},
		Labels: map[string]string{labelDynamic: ""}, DynamicGroupMetadata: &ci.DynamicGroupMetadata{Queries: []*ci.DynamicGroupQuery{
			{ResourceType: "USER", Query: "user.organizations.exists(org, org.department=='engineering' && org.type == 1)"}}}}).InitialGroupConfig("EMPTY").Do()
	if err != nil {
		t.Fatal(err)
	}
	var g ci.Group
	_ = json.Unmarshal(op.Response, &g)
	members := func() []string {
		t.Helper()
		l, err := c.Groups.Memberships.List(g.Name).Do()
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, m := range l.Memberships {
			out = append(out, m.PreferredMemberKey.Id)
		}
		return out
	}
	if got := members(); !slices.Equal(got, []string{"ann@" + OrganizationDomain}) {
		t.Fatalf("members = %v", got)
	}
	if !slices.Contains(r.Principals("ann@"+OrganizationDomain), "group:eng@"+OrganizationDomain) {
		t.Errorf("IAM does not see ann in the dynamic group: %v", r.Principals("ann@"+OrganizationDomain))
	}
	// Bob joins engineering: he is a member at once.
	if _, err := a.Users.Patch("bob@"+OrganizationDomain, &admin.User{Organizations: []map[string]any{{"department": "engineering", "type": "work"}}}).Do(); err != nil {
		t.Fatal(err)
	}
	if got := members(); len(got) != 2 {
		t.Fatalf("after bob's change, members = %v", got)
	}
	gg, err := c.Groups.Get(g.Name).Do()
	if err != nil || gg.DynamicGroupMetadata == nil || gg.DynamicGroupMetadata.Status.Status != "UP_TO_DATE" {
		t.Fatalf("get = %+v, %v", gg, err)
	}
	// Memberships are the query's.
	if _, err := c.Groups.Memberships.Create(g.Name, &ci.Membership{PreferredMemberKey: &ci.EntityKey{Id: "x@" + OrganizationDomain}}).Do(); code(err) != http.StatusBadRequest {
		t.Errorf("manual membership of a dynamic group: %v", err)
	}
	// A dynamic group is not a member of a Google Group.
	op2, err := c.Groups.Create(&ci.Group{Parent: CustomerName, GroupKey: &ci.EntityKey{Id: "all@" + OrganizationDomain},
		Labels: map[string]string{LabelDiscussionForum: ""}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	var all ci.Group
	_ = json.Unmarshal(op2.Response, &all)
	if _, err := c.Groups.Memberships.Create(all.Name, &ci.Membership{PreferredMemberKey: &ci.EntityKey{Id: "eng@" + OrganizationDomain}}).Do(); code(err) != http.StatusBadRequest {
		t.Errorf("a dynamic group as a member: %v", err)
	}
	// Patching the query.
	if _, err := c.Groups.Patch(g.Name, &ci.Group{DynamicGroupMetadata: &ci.DynamicGroupMetadata{Queries: []*ci.DynamicGroupQuery{
		{ResourceType: "USER", Query: "user.addresses.exists(ad, ad.locality=='Sunnyvale')"}}}}).UpdateMask("dynamic_group_metadata").Do(); err != nil {
		t.Fatal(err)
	}
	if got := members(); !slices.Equal(got, []string{"bob@" + OrganizationDomain}) {
		t.Fatalf("after the patch, members = %v", got)
	}
	for _, q := range []string{
		"!user.organizations.exists(org, org.title=='a' && org.department=='b')",
		"user.organizations.exists(org, !(org.title=='a'))",
		"user.organizations.exists(",
	} {
		_, err := c.Groups.Create(&ci.Group{Parent: CustomerName, GroupKey: &ci.EntityKey{Id: "bad@" + OrganizationDomain},
			Labels: map[string]string{labelDynamic: ""}, DynamicGroupMetadata: &ci.DynamicGroupMetadata{Queries: []*ci.DynamicGroupQuery{{ResourceType: "USER", Query: q}}}}).Do()
		if code(err) != http.StatusBadRequest {
			t.Errorf("query %q: %v", q, err)
		}
	}
	// Other documented forms.
	for q, want := range map[string]bool{
		"user.name.value.equalsIgnoreCase('ann LEE')":                                   true,
		"user.org_unit_id==orgUnitId('" + RootOrgUnitID + "')":                          true,
		"!(user.org_unit_id==orgUnitId('" + RootOrgUnitID + "'))":                       false,
		"user.organizations.exists(org, org.title = \"Marketing\")":                     false,
		"user.org_units.exists(ou, ou.org_unit_id==orgUnitId('" + RootOrgUnitID + "'))": true,
		"user.suspended == false":                                                       true,
	} {
		dq, err := CompileDynamicQuery(q)
		if err != nil {
			t.Errorf("%q: %v", q, err)
			continue
		}
		u, _ := r.User("ann@" + OrganizationDomain)
		if got := dq.Matches(queryUser(u, func(string) string { return "" })); got != want {
			t.Errorf("%q matches ann = %v, want %v", q, got, want)
		}
	}
	// A user deleted leaves the group.
	if err := a.Users.Delete("bob@" + OrganizationDomain).Do(); err != nil {
		t.Fatal(err)
	}
	if got := members(); len(got) != 0 {
		t.Errorf("after delete, members = %v", got)
	}
}

// TestIdentityMappedGroups2103: groups of a workforce pool's identity
// source.
func TestIdentityMappedGroups2103(t *testing.T) {
	r, c, _, _ := ci2103(t)
	src := "identitysources/corp-pool"
	_, err := c.Groups.Create(&ci.Group{Parent: src, GroupKey: &ci.EntityKey{Id: "eng", Namespace: src}, Labels: map[string]string{labelExternal: ""}}).Do()
	if code(err) != http.StatusForbidden {
		t.Fatalf("no such source: %v", err)
	}
	if _, err := r.CreateWorkforcePool("corp-pool", WorkforcePool{Parent: OrganizationName}); err != nil {
		t.Fatal(err)
	}
	op, err := c.Groups.Create(&ci.Group{Parent: src, GroupKey: &ci.EntityKey{Id: "eng", Namespace: src}, Labels: map[string]string{labelExternal: ""}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	var g ci.Group
	_ = json.Unmarshal(op.Response, &g)
	if g.Parent != src || g.GroupKey.Namespace != src {
		t.Errorf("group = %+v", g)
	}
	if _, err := c.Groups.Memberships.Create(g.Name, &ci.Membership{PreferredMemberKey: &ci.EntityKey{Id: "alice", Namespace: src}}).Do(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Groups.Memberships.Create(g.Name, &ci.Membership{PreferredMemberKey: &ci.EntityKey{Id: "bob@x.com"}}).Do(); code(err) != http.StatusBadRequest {
		t.Errorf("a Google member of a mapped group: %v", err)
	}
	l, err := c.Groups.List().Parent(src).Do()
	if err != nil || len(l.Groups) != 1 {
		t.Fatalf("list = %+v, %v", l, err)
	}
	lk, err := c.Groups.Lookup().GroupKeyId("eng").GroupKeyNamespace(src).Do()
	if err != nil || lk.Name != g.Name {
		t.Fatalf("lookup = %+v, %v", lk, err)
	}
	ml, err := c.Groups.Memberships.Lookup(g.Name).MemberKeyId("alice").MemberKeyNamespace(src).Do()
	if err != nil || ml.Name == "" {
		t.Fatalf("membership lookup = %+v, %v", ml, err)
	}
	if l, _ := c.Groups.List().Parent(CustomerName).Do(); len(l.Groups) != 0 {
		t.Errorf("a mapped group is listed under the customer: %+v", l.Groups)
	}
}

// TestDevices2103: a device's life: import, sign-in, approve, block,
// wipe, cancel, client states, lookup.
func TestDevices2103(t *testing.T) {
	r, c, a, url := ci2103(t)
	r.SetCallerResolver(func(b string) (string, bool) { return "ann@" + OrganizationDomain, b == "ann-token" })
	if _, err := a.Users.Insert(&admin.User{PrimaryEmail: "ann@" + OrganizationDomain, Name: &admin.UserName{GivenName: "A", FamilyName: "L"}, Password: "password-1"}).Do(); err != nil {
		t.Fatal(err)
	}
	op, err := c.Devices.Create(&ci.GoogleAppsCloudidentityDevicesV1Device{SerialNumber: "SN1", DeviceType: "ANDROID", AssetTag: "tag1"}).Do()
	if err != nil {
		t.Fatal(err)
	}
	var d ci.GoogleAppsCloudidentityDevicesV1Device
	_ = json.Unmarshal(op.Response, &d)
	if d.ManagementState != "UNPROVISIONED" || d.OwnerType != "COMPANY" {
		t.Fatalf("device = %+v", d)
	}
	if _, err := c.Devices.Create(&ci.GoogleAppsCloudidentityDevicesV1Device{SerialNumber: "SN1", DeviceType: "ANDROID"}).Do(); code(err) != http.StatusConflict {
		t.Errorf("duplicate: %v", err)
	}
	body, _ := json.Marshal(SignInInput{UserEmail: "ann@" + OrganizationDomain, SerialNumber: "SN1", DeviceType: "ANDROID", DeviceID: "aid-1"})
	resp, err := http.Post(url+"/cloudburrow/v1/devices:signIn", "application/json", bytes.NewReader(body))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("sign in: %v %v", resp, err)
	}
	var du ci.GoogleAppsCloudidentityDevicesV1DeviceUser
	_ = json.NewDecoder(resp.Body).Decode(&du)
	resp.Body.Close()
	if du.ManagementState != "PENDING_APPROVAL" || !strings.HasPrefix(du.Name, d.Name+"/deviceUsers/") {
		t.Fatalf("device user = %+v", du)
	}
	if got, _ := c.Devices.Get(d.Name).Do(); got.ManagementState != "PENDING" {
		t.Errorf("device after sign-in: %s", got.ManagementState)
	}
	state := func() string {
		t.Helper()
		u, err := c.Devices.DeviceUsers.Get(du.Name).Do()
		if err != nil {
			t.Fatal(err)
		}
		return u.ManagementState
	}
	if _, err := c.Devices.DeviceUsers.Approve(du.Name, &ci.GoogleAppsCloudidentityDevicesV1ApproveDeviceUserRequest{}).Do(); err != nil || state() != "APPROVED" {
		t.Fatalf("approve: %v %s", err, state())
	}
	if _, err := c.Devices.DeviceUsers.Block(du.Name, &ci.GoogleAppsCloudidentityDevicesV1BlockDeviceUserRequest{}).Do(); err != nil || state() != "BLOCKED" {
		t.Fatalf("block: %v", err)
	}
	if _, err := c.Devices.DeviceUsers.Wipe(du.Name, &ci.GoogleAppsCloudidentityDevicesV1WipeDeviceUserRequest{}).Do(); err != nil || state() != "WIPING" {
		t.Fatalf("wipe: %v", err)
	}
	if _, err := c.Devices.DeviceUsers.CancelWipe(du.Name, &ci.GoogleAppsCloudidentityDevicesV1CancelWipeDeviceUserRequest{}).Do(); err != nil || state() != "BLOCKED" {
		t.Fatalf("cancel wipe: %v %s", err, state())
	}
	if _, err := c.Devices.DeviceUsers.CancelWipe(du.Name, &ci.GoogleAppsCloudidentityDevicesV1CancelWipeDeviceUserRequest{}).Do(); code(err) != http.StatusBadRequest {
		t.Errorf("cancel without a wipe: %v", err)
	}
	// Client state.
	csName := du.Name + "/clientStates/" + strings.TrimPrefix(OrganizationCustomerID, "C") + "-scanner"
	op, err = c.Devices.DeviceUsers.ClientStates.Patch(csName, &ci.GoogleAppsCloudidentityDevicesV1ClientState{ComplianceState: "COMPLIANT",
		HealthScore: "GOOD", KeyValuePairs: map[string]ci.GoogleAppsCloudidentityDevicesV1CustomAttributeValue{"k": {StringValue: "v"}}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	cs, err := c.Devices.DeviceUsers.ClientStates.Get(csName).Do()
	if err != nil || cs.ComplianceState != "COMPLIANT" || cs.OwnerType != "OWNER_TYPE_CUSTOMER" || cs.Etag == "" {
		t.Fatalf("client state = %+v, %v", cs, err)
	}
	if _, err := c.Devices.DeviceUsers.ClientStates.Patch(du.Name+"/clientStates/partner-x", &ci.GoogleAppsCloudidentityDevicesV1ClientState{}).Do(); code(err) != http.StatusBadRequest {
		t.Errorf("a foreign partner's ID: %v", err)
	}
	if _, err := c.Devices.DeviceUsers.ClientStates.Patch(csName, &ci.GoogleAppsCloudidentityDevicesV1ClientState{Etag: `"stale"`, HealthScore: "POOR"}).UpdateMask("healthScore").Do(); code(err) != http.StatusConflict {
		t.Errorf("stale etag: %v", err)
	}
	if l, err := c.Devices.DeviceUsers.ClientStates.List("devices/-/deviceUsers/-").Do(); err != nil || len(l.ClientStates) != 1 {
		t.Errorf("client states = %+v, %v", l, err)
	}
	// Lists and filters.
	if l, err := c.Devices.List().Filter("serial:SN status:blocked").Do(); err != nil || len(l.Devices) != 1 {
		t.Errorf("filtered list = %+v, %v", l, err)
	}
	if l, err := c.Devices.List().Filter("type:ios").Do(); err != nil || len(l.Devices) != 0 {
		t.Errorf("type:ios = %+v, %v", l, err)
	}
	if l, err := c.Devices.DeviceUsers.List("devices/-").Filter("email:ann").Do(); err != nil || len(l.DeviceUsers) != 1 {
		t.Errorf("device users = %+v, %v", l, err)
	}
	// Lookup is the caller's own.
	req, _ := http.NewRequest("GET", url+"/v1/devices/-/deviceUsers:lookup?userId=me&androidId=aid-1", nil)
	req.Header.Set("Authorization", "Bearer ann-token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("lookup: %v %v", resp, err)
	}
	var lr ci.GoogleAppsCloudidentityDevicesV1LookupSelfDeviceUsersResponse
	_ = json.NewDecoder(resp.Body).Decode(&lr)
	resp.Body.Close()
	if len(lr.Names) != 1 || lr.Names[0] != du.Name {
		t.Errorf("lookup = %+v", lr)
	}
	// Device wipe: pending until the device syncs.
	if _, err := c.Devices.Wipe(d.Name, &ci.GoogleAppsCloudidentityDevicesV1WipeDeviceRequest{}).Do(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Devices.CancelWipe(d.Name, &ci.GoogleAppsCloudidentityDevicesV1CancelWipeDeviceRequest{}).Do(); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Devices.Get(d.Name).Do(); got.ManagementState != "BLOCKED" {
		t.Errorf("after cancel wipe: %s", got.ManagementState)
	}
	if _, err := c.Devices.Wipe(d.Name, &ci.GoogleAppsCloudidentityDevicesV1WipeDeviceRequest{}).Do(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SyncDevice(d.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Devices.CancelWipe(d.Name, &ci.GoogleAppsCloudidentityDevicesV1CancelWipeDeviceRequest{}).Do(); code(err) != http.StatusBadRequest {
		t.Errorf("cancel after the wipe: %v", err)
	}
	if got, _ := c.Devices.Get(d.Name).Do(); got.ManagementState != "WIPED" {
		t.Errorf("after sync: %s", got.ManagementState)
	}
	if _, err := c.Devices.Delete(d.Name).Do(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Devices.Get(d.Name).Do(); code(err) != http.StatusNotFound {
		t.Errorf("deleted device: %v", err)
	}
}

// TestInvitationsPoliciesAllowlist2103.
func TestInvitationsPoliciesAllowlist2103(t *testing.T) {
	_, c, a, url := ci2103(t)
	name := CustomerName + "/userinvitations/carol@" + OrganizationDomain
	if v, err := c.Customers.Userinvitations.IsInvitableUser(name).Do(); err != nil || !v.IsInvitableUser {
		t.Fatalf("invitable = %+v, %v", v, err)
	}
	if v, _ := c.Customers.Userinvitations.IsInvitableUser(CustomerName + "/userinvitations/x@gmail.com").Do(); v.IsInvitableUser {
		t.Errorf("another domain is invitable")
	}
	if _, err := c.Customers.Userinvitations.Send(name, &ci.SendUserInvitationRequest{}).Do(); err != nil {
		t.Fatal(err)
	}
	inv, err := c.Customers.Userinvitations.Get(name).Do()
	if err != nil || inv.State != "INVITED" || inv.MailsSentCount != 1 {
		t.Fatalf("invitation = %+v, %v", inv, err)
	}
	if l, err := c.Customers.Userinvitations.List(CustomerName).Filter("state=='invited'").Do(); err != nil || len(l.UserInvitations) != 1 {
		t.Errorf("list = %+v, %v", l, err)
	}
	if _, err := c.Customers.Userinvitations.Cancel(name, &ci.CancelUserInvitationRequest{}).Do(); err != nil {
		t.Fatal(err)
	}
	if inv, _ := c.Customers.Userinvitations.Get(name).Do(); inv.State != "NOT_YET_SENT" {
		t.Errorf("after cancel: %s", inv.State)
	}
	_, _ = c.Customers.Userinvitations.Send(name, &ci.SendUserInvitationRequest{}).Do()
	resp, err := http.Post(url+"/cloudburrow/v1/"+name+":accept", "application/json", strings.NewReader(`{"givenName":"Carol","familyName":"Ng"}`))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("accept: %v %v", resp, err)
	}
	resp.Body.Close()
	if u, err := a.Users.Get("carol@" + OrganizationDomain).Do(); err != nil || u.Name.GivenName != "Carol" {
		t.Errorf("accepted user = %+v, %v", u, err)
	}
	if v, _ := c.Customers.Userinvitations.IsInvitableUser(name).Do(); v.IsInvitableUser {
		t.Errorf("a managed user is invitable")
	}

	// Policies.
	op, err := c.Policies.Create(&ci.Policy{Customer: CustomerName, PolicyQuery: &ci.PolicyQuery{OrgUnit: "orgUnits/" + RootOrgUnitID},
		Setting: &ci.Setting{Type: "settings/security.password", Value: []byte(`{"minimumLength":12}`)}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	var p ci.Policy
	_ = json.Unmarshal(op.Response, &p)
	if p.Type != "ADMIN" || p.PolicyQuery.SortOrder != 1 {
		t.Errorf("policy = %+v", p)
	}
	if _, err := c.Policies.Create(&ci.Policy{PolicyQuery: &ci.PolicyQuery{OrgUnit: "orgUnits/nope"},
		Setting: &ci.Setting{Type: "settings/security.password", Value: []byte(`{}`)}}).Do(); code(err) != http.StatusBadRequest {
		t.Errorf("unknown org unit: %v", err)
	}
	if l, err := c.Policies.List().Filter(`setting.type.matches('^settings/security\\..*$')`).Do(); err != nil || len(l.Policies) != 1 {
		t.Errorf("filtered = %+v, %v", l, err)
	}
	if l, err := c.Policies.List().Filter(`setting.type.matches('^settings/gmail\\..*$')`).Do(); err != nil || len(l.Policies) != 0 {
		t.Errorf("gmail filtered = %+v, %v", l, err)
	}
	if _, err := c.Policies.Patch(p.Name, &ci.Policy{Setting: &ci.Setting{Value: []byte(`{"minimumLength":14}`)}}).Do(); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Policies.Get(p.Name).Do(); !strings.Contains(string(got.Setting.Value), "14") {
		t.Errorf("patched = %s", got.Setting.Value)
	}
	if _, err := c.Policies.Patch(p.Name, &ci.Policy{Setting: &ci.Setting{Type: "settings/x.y", Value: []byte(`{}`)}}).Do(); code(err) != http.StatusBadRequest {
		t.Errorf("type change: %v", err)
	}
	if _, err := c.Policies.Delete(p.Name).Do(); err != nil {
		t.Fatal(err)
	}

	// Allowlisted domains.
	op, err = c.AllowlistedDomains.Create(&ci.AllowlistedDomain{Domain: "partner.example.com"}).Do()
	if err != nil {
		t.Fatal(err)
	}
	var d ci.AllowlistedDomain
	_ = json.Unmarshal(op.Response, &d)
	if _, err := c.AllowlistedDomains.Create(&ci.AllowlistedDomain{Domain: "partner.example.com"}).Do(); code(err) != http.StatusConflict {
		t.Errorf("duplicate: %v", err)
	}
	if l, err := c.AllowlistedDomains.List().Do(); err != nil || len(l.AllowlistedDomains) != 1 {
		t.Errorf("list = %+v, %v", l, err)
	}
	if _, err := c.AllowlistedDomains.Delete(d.Name).Do(); err != nil {
		t.Fatal(err)
	}
}
