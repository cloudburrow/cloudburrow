package main

// Cloud Identity's directory screens (#2103), as Google's Admin console
// lays them out (https://knowledge.workspace.google.com/admin): Directory >
// Users (a user's name, email, organizational unit and status, Add new
// user, a user's page with its attributes, Edit, Delete); Devices > Mobile
// & endpoints > Devices (the device list with type, serial number,
// ownership, status, users and last sync, Add company-owned device, a
// device's page with its users and their Approve, Block, Wipe account and
// Cancel wipe, the device's Wipe device and Cancel wipe, and the client
// states); Users > Invite users (user invitations: Send invitation,
// Resend, Cancel); Account > Policies (the Policy API's policies: Create,
// Edit value, Delete); and Allowlisted domains. Each is over the registry
// the official clients reach, so what is made here is what the API serves.

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"

	ci "google.golang.org/api/cloudidentity/v1"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

const directoryNote = "The customer's users (" + resourcemanager.CustomerName + ", domain " + resourcemanager.OrganizationDomain +
	"), which the Admin SDK Directory API serves at admin/directory/v1/users and dynamic groups' queries read."

// ---- Users

type ciUsersProvider struct{ registry *resourcemanager.Registry }

func (ciUsersProvider) ID() string    { return "iam-users" }
func (ciUsersProvider) Title() string { return "Users" }

func userField(u *resourcemanager.DirectoryUser, path ...string) string {
	var v any = u.Object
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return ""
		}
		v = m[p]
	}
	s, _ := v.(string)
	return s
}

// firstOf is the k field of the first element of u's list attribute.
func firstOf(u *resourcemanager.DirectoryUser, list, k string) string {
	l, _ := u.Object[list].([]any)
	for _, e := range l {
		if m, ok := e.(map[string]any); ok {
			if s, _ := m[k].(string); s != "" {
				return s
			}
		}
	}
	return ""
}

func userStatus(u *resourcemanager.DirectoryUser) string {
	if b, _ := u.Object["suspended"].(bool); b {
		return "SUSPENDED"
	}
	return "ACTIVE"
}

func (p ciUsersProvider) List(_ context.Context, _ string) (console.Listing, error) {
	l := console.Listing{NameColumn: "Email", Columns: []string{"Name", "Department", "Title", "Org unit path"}, Noun: "users", Note: directoryNote}
	us, err := p.registry.Users()
	if err != nil {
		l.Unavailable = consoleErr(err)
		return l, nil
	}
	for _, u := range us {
		l.Items = append(l.Items, console.Resource{Name: u.Email(), Status: userStatus(u), Fields: map[string]string{
			"Name": orDash(userField(u, "name", "fullName")), "Department": orDash(firstOf(u, "organizations", "department")),
			"Title": orDash(firstOf(u, "organizations", "title")), "Org unit path": u.OrgUnitPath()}})
	}
	l.Total = len(l.Items)
	return l, nil
}

func (ciUsersProvider) CreateTitle() string { return "Add new user" }

func userFields(u *resourcemanager.DirectoryUser) []console.Field {
	def := func(f func(*resourcemanager.DirectoryUser) string) string {
		if u == nil {
			return ""
		}
		return f(u)
	}
	return []console.Field{
		{Name: "givenName", Label: "First name", Type: "text", Required: u == nil, Default: def(func(u *resourcemanager.DirectoryUser) string { return userField(u, "name", "givenName") }), Section: "User information"},
		{Name: "familyName", Label: "Last name", Type: "text", Required: u == nil, Default: def(func(u *resourcemanager.DirectoryUser) string { return userField(u, "name", "familyName") }), Section: "User information"},
		{Name: "department", Label: "Department", Type: "text", Default: def(func(u *resourcemanager.DirectoryUser) string { return firstOf(u, "organizations", "department") }), Section: "Employee information"},
		{Name: "title", Label: "Job title", Type: "text", Default: def(func(u *resourcemanager.DirectoryUser) string { return firstOf(u, "organizations", "title") }), Section: "Employee information"},
		{Name: "costCenter", Label: "Cost center", Type: "text", Default: def(func(u *resourcemanager.DirectoryUser) string { return firstOf(u, "organizations", "costCenter") }), Section: "Employee information"},
		{Name: "locality", Label: "City (work address)", Type: "text", Default: def(func(u *resourcemanager.DirectoryUser) string { return firstOf(u, "addresses", "locality") }), Section: "Contact information"},
		{Name: "orgUnitPath", Label: "Organizational unit", Type: "text", Default: def(func(u *resourcemanager.DirectoryUser) string { return u.OrgUnitPath() }), Section: "User information",
			Help: "The user's organizational unit path, / for the top one."},
	}
}

func (ciUsersProvider) CreateForm() (string, []console.Field) {
	f := userFields(nil)
	f = append(f[:2], append([]console.Field{
		{Name: "email", Label: "Primary email", Type: "text", Required: true, Section: "User information",
			Help: "NAME@" + resourcemanager.OrganizationDomain + ": the customer's users are in its domain."},
		{Name: "password", Label: "Password", Type: "password", Required: true, Section: "User information", Help: "8 to 100 characters."},
	}, f[2:]...)...)
	return "Add new user", f
}

func userObject(v map[string]string) map[string]any {
	obj := map[string]any{"name": map[string]any{"givenName": strings.TrimSpace(v["givenName"]), "familyName": strings.TrimSpace(v["familyName"])}}
	org := map[string]any{"primary": true, "type": "work"}
	for _, k := range []string{"department", "title", "costCenter"} {
		if s := strings.TrimSpace(v[k]); s != "" {
			org[k] = s
		}
	}
	obj["organizations"] = []any{org}
	if s := strings.TrimSpace(v["locality"]); s != "" {
		obj["addresses"] = []any{map[string]any{"type": "work", "locality": s}}
	} else {
		obj["addresses"] = []any{}
	}
	if s := strings.TrimSpace(v["orgUnitPath"]); s != "" {
		obj["orgUnitPath"] = s
	}
	return obj
}

func (p ciUsersProvider) Create(_ context.Context, _ string, v map[string]string) (string, error) {
	obj := userObject(v)
	obj["primaryEmail"] = strings.TrimSpace(v["email"])
	obj["password"] = v["password"]
	u, err := p.registry.InsertUser(obj)
	if err != nil {
		return "", err
	}
	return u.Email(), nil
}

func (p ciUsersProvider) Detail(_ context.Context, _ string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	u, err := p.registry.User(path[0])
	if err != nil {
		return console.Detail{Unavailable: "cannot read the user: " + consoleErr(err)}, nil
	}
	props := []console.Property{
		{Label: "Primary email", Value: u.Email()},
		{Label: "Name", Value: orDash(userField(u, "name", "fullName"))},
		{Label: "Status", Value: userStatus(u)},
		{Label: "User ID", Value: u.ID()},
		{Label: "Organizational unit", Value: u.OrgUnitPath() + " (" + resourcemanager.OrgUnitID(u.OrgUnitPath()) + ")"},
		{Label: "Department", Value: orDash(firstOf(u, "organizations", "department"))},
		{Label: "Job title", Value: orDash(firstOf(u, "organizations", "title"))},
		{Label: "City (work address)", Value: orDash(firstOf(u, "addresses", "locality"))},
		{Label: "Created", Value: userField(u, "creationTime")},
	}
	groups := console.Listing{NameColumn: "Group", Columns: []string{"Relation"}, Noun: "groups"}
	if rels, err := p.registry.TransitiveGroups(u.Email()); err == nil {
		for _, x := range rels {
			groups.Items = append(groups.Items, console.Resource{Name: x.Group.Email, Fields: map[string]string{"Relation": relationLabel(x.Direct, x.Indirect)}})
		}
	}
	groups.Total = len(groups.Items)
	raw, _ := json.MarshalIndent(u.Object, "", "  ")
	return console.Detail{Summary: props[:3], Sections: []console.Section{
		{ID: "details", Label: "User details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "User information", Properties: props}}},
		{ID: "groups", Label: "Groups", Listing: groups},
		{ID: "json", Label: "Equivalent REST", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "admin#directory#user",
			Properties: []console.Property{{Label: "JSON", Value: string(raw)}}}}},
	}, Edit: &console.EditForm{Label: "Edit user", Fields: userFields(u)}}, nil
}

func (p ciUsersProvider) Edit(_ context.Context, _ string, path []string, v map[string]string) error {
	if len(path) != 1 {
		return errors.New("only a user can be edited")
	}
	_, err := p.registry.UpdateUser(path[0], userObject(v))
	return err
}

func (p ciUsersProvider) Delete(_ context.Context, _ string, row string) error {
	return p.registry.DeleteUser(row)
}

var _ console.Creator = ciUsersProvider{}
var _ console.Deleter = ciUsersProvider{}
var _ console.Driller = ciUsersProvider{}
var _ console.Editor = ciUsersProvider{}

// ---- Devices

type ciDevicesProvider struct{ registry *resourcemanager.Registry }

func (ciDevicesProvider) ID() string    { return "iam-devices" }
func (ciDevicesProvider) Title() string { return "Devices" }

const devicesNote = "The customer's devices, as Cloud Identity's Devices API serves them. No device checks in on its own here: " +
	"a user signs in on a device with POST /cloudburrow/v1/devices:signIn, and a device syncs (carrying out a pending wipe) with " +
	"POST /cloudburrow/v1/devices/ID:sync, both at the Resource Manager endpoint."

var deviceStatusLabels = map[string]string{"APPROVED": "Approved", "BLOCKED": "Blocked", "PENDING": "Pending", "PENDING_APPROVAL": "Pending approval",
	"UNPROVISIONED": "Unprovisioned", "WIPING": "Wiping", "WIPED": "Wiped", "UNENROLLED": "Unenrolled"}

func deviceID(d *resourcemanager.DeviceRecord) string { return d.ID() }

func (p ciDevicesProvider) List(_ context.Context, _ string) (console.Listing, error) {
	l := console.Listing{NameColumn: "Device ID", Columns: []string{"Type", "Serial number", "Model", "Ownership", "Users", "Last sync"}, Noun: "devices", Note: devicesNote}
	ds, err := p.registry.Devices()
	if err != nil {
		l.Unavailable = consoleErr(err)
		return l, nil
	}
	for _, d := range ds {
		var users []string
		for _, u := range d.Users {
			users = append(users, u.User.UserEmail)
		}
		dev := d.Device
		l.Items = append(l.Items, console.Resource{Name: deviceID(d), Status: dev.ManagementState, Fields: map[string]string{
			"Type": dev.DeviceType, "Serial number": orDash(dev.SerialNumber), "Model": orDash(dev.Model), "Ownership": dev.OwnerType,
			"Users": orDash(strings.Join(users, ", ")), "Last sync": orDash(dev.LastSyncTime)}})
	}
	l.Total = len(l.Items)
	return l, nil
}

func (ciDevicesProvider) CreateTitle() string { return "Add company-owned device" }

func (ciDevicesProvider) CreateForm() (string, []console.Field) {
	return "Add", []console.Field{
		{Name: "serialNumber", Label: "Serial number", Type: "text", Required: true},
		{Name: "deviceType", Label: "Device type", Type: "select", Default: "ANDROID",
			Options: []string{"ANDROID", "IOS", "WINDOWS", "MAC_OS", "LINUX", "CHROME_OS", "GOOGLE_SYNC"}},
		{Name: "assetTag", Label: "Asset tag", Type: "text", Help: "Optional."},
	}
}

func (p ciDevicesProvider) Create(_ context.Context, _ string, v map[string]string) (string, error) {
	d, err := p.registry.CreateDevice(&ci.GoogleAppsCloudidentityDevicesV1Device{SerialNumber: strings.TrimSpace(v["serialNumber"]),
		DeviceType: v["deviceType"], AssetTag: strings.TrimSpace(v["assetTag"])})
	if err != nil {
		return "", err
	}
	return deviceID(d), nil
}

func deviceActions(d *resourcemanager.DeviceRecord) []console.Action {
	st := d.Device.ManagementState
	wipe := console.Action{ID: "wipe", Label: "Wipe device", Destructive: true, Confirm: "every account and its data on device " + deviceID(d)}
	cancel := console.Action{ID: "cancel-wipe", Label: "Cancel wipe"}
	switch st {
	case "WIPING":
		wipe.Unavailable = "The device is already being wiped."
	case "WIPED":
		wipe.Unavailable = "The device has been wiped."
		cancel.Unavailable = "The wipe command has already been issued to the device."
	}
	if st != "WIPING" && st != "WIPED" {
		cancel.Unavailable = "The device is not in a pending wipe state."
	}
	return []console.Action{wipe, cancel, {ID: "delete", Label: "Delete device", Destructive: true, Leaves: true, Confirm: "device " + deviceID(d) + " and its users"}}
}

func deviceUserActions(u *resourcemanager.DeviceUser) []console.Action {
	st := u.User.ManagementState
	out := []console.Action{{ID: "approve", Label: "Approve"}, {ID: "block", Label: "Block"},
		{ID: "wipe-account", Label: "Wipe account", Destructive: true, Confirm: "the work account " + u.User.UserEmail + " on the device"},
		{ID: "cancel-wipe-account", Label: "Cancel wipe"},
		{ID: "delete-account", Label: "Remove account", Destructive: true, Confirm: "the account " + u.User.UserEmail + " from the device"}}
	switch st {
	case "APPROVED":
		out[0].Unavailable = "The user is approved."
	case "BLOCKED":
		out[1].Unavailable = "The user is blocked."
	case "WIPING", "WIPED":
		out[0].Unavailable, out[1].Unavailable, out[2].Unavailable = "The account is "+strings.ToLower(st)+".", "The account is "+strings.ToLower(st)+".", "The account is "+strings.ToLower(st)+"."
	}
	if st != "WIPING" {
		out[3].Unavailable = "The account has no unfinished wipe."
	}
	return out
}

func (p ciDevicesProvider) Detail(_ context.Context, _ string, path []string) (console.Detail, error) {
	if len(path) > 2 {
		return console.DeeperThan(2, path), nil
	}
	d, err := p.registry.Device("devices/" + path[0])
	if err != nil {
		return console.Detail{Unavailable: "cannot read the device: " + consoleErr(err)}, nil
	}
	if len(path) == 2 {
		_, u, err := p.registry.DeviceUserByName(d.Device.Name + "/deviceUsers/" + path[1])
		if err != nil {
			return console.Detail{Unavailable: "cannot read the device user: " + consoleErr(err)}, nil
		}
		props := []console.Property{{Label: "User", Value: u.User.UserEmail}, {Label: "Status", Value: deviceStatusLabels[u.User.ManagementState]},
			{Label: "Resource name", Value: u.User.Name}, {Label: "First sync", Value: orDash(u.User.FirstSyncTime)}, {Label: "Last sync", Value: orDash(u.User.LastSyncTime)},
			{Label: "Compromised", Value: u.User.CompromisedState}, {Label: "Password", Value: u.User.PasswordState}}
		cs := console.Listing{NameColumn: "Client state", Columns: []string{"Compliance", "Health score", "Managed", "Updated"}, Noun: "client states"}
		for _, s := range u.ClientStates {
			id := s.Name[strings.LastIndex(s.Name, "/")+1:]
			cs.Items = append(cs.Items, console.Resource{Name: id, Fields: map[string]string{"Compliance": orDash(s.ComplianceState),
				"Health score": orDash(s.HealthScore), "Managed": orDash(s.Managed), "Updated": s.LastUpdateTime}})
		}
		cs.Total = len(cs.Items)
		return console.Detail{Summary: props[:2], Sections: []console.Section{
			{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Device user", Properties: props}}},
			{ID: "client-states", Label: "Client states", Listing: cs},
		}}, nil
	}
	dev := d.Device
	props := []console.Property{
		{Label: "Device ID", Value: deviceID(d)}, {Label: "Status", Value: deviceStatusLabels[dev.ManagementState]},
		{Label: "Type", Value: dev.DeviceType}, {Label: "Ownership", Value: dev.OwnerType}, {Label: "Serial number", Value: orDash(dev.SerialNumber)},
		{Label: "Asset tag", Value: orDash(dev.AssetTag)}, {Label: "Model", Value: orDash(dev.Model)}, {Label: "OS version", Value: orDash(dev.OsVersion)},
		{Label: "Imported", Value: orDash(dev.CreateTime)}, {Label: "Last sync", Value: orDash(dev.LastSyncTime)},
	}
	users := console.Listing{NameColumn: "Device user", Columns: []string{"User", "Status", "Last sync"}, Noun: "device users", RowsOpenable: true}
	for _, u := range d.Users {
		users.Items = append(users.Items, console.Resource{Name: u.User.Name[strings.LastIndex(u.User.Name, "/")+1:], Status: u.User.ManagementState,
			Fields:  map[string]string{"User": u.User.UserEmail, "Status": deviceStatusLabels[u.User.ManagementState], "Last sync": orDash(u.User.LastSyncTime)},
			Actions: deviceUserActions(u)})
	}
	users.Total = len(users.Items)
	return console.Detail{Summary: props[:4], Sections: []console.Section{
		{ID: "details", Label: "Device details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Device", Properties: props}}},
		{ID: "users", Label: "Users", Listing: users},
	}}, nil
}

func (p ciDevicesProvider) DetailActions(_ context.Context, _ string, path []string) []console.Action {
	d, err := p.registry.Device("devices/" + path[0])
	if err != nil {
		return nil
	}
	if len(path) == 1 {
		return deviceActions(d)
	}
	if len(path) == 2 {
		if _, u, err := p.registry.DeviceUserByName(d.Device.Name + "/deviceUsers/" + path[1]); err == nil {
			return deviceUserActions(u)
		}
	}
	return nil
}

func (p ciDevicesProvider) ActAt(_ context.Context, _ string, path []string, action string, _ map[string]string) error {
	name := "devices/" + path[0]
	if len(path) == 2 && action == "delete-account" {
		return p.registry.DeleteDeviceUser(name + "/deviceUsers/" + path[1])
	}
	if len(path) == 2 {
		verb := map[string]string{"approve": "approve", "block": "block", "wipe-account": "wipe", "cancel-wipe-account": "cancelWipe"}[action]
		if verb == "" {
			return errors.New("unknown action " + action)
		}
		_, _, err := p.registry.ActOnDeviceUser(name+"/deviceUsers/"+path[1], verb)
		return err
	}
	switch action {
	case "wipe", "cancel-wipe":
		_, err := p.registry.WipeDevice(name, action == "cancel-wipe")
		return err
	case "delete":
		return p.registry.DeleteDevice(name)
	}
	return errors.New("unknown action " + action)
}

func (p ciDevicesProvider) Delete(_ context.Context, _ string, row string) error {
	return p.registry.DeleteDevice("devices/" + row)
}

var _ console.Creator = ciDevicesProvider{}
var _ console.Driller = ciDevicesProvider{}
var _ console.PathActor = ciDevicesProvider{}

// ---- User invitations

type ciInvitationsProvider struct{ registry *resourcemanager.Registry }

func (ciInvitationsProvider) ID() string    { return "iam-user-invitations" }
func (ciInvitationsProvider) Title() string { return "User invitations" }

func (p ciInvitationsProvider) List(_ context.Context, _ string) (console.Listing, error) {
	l := console.Listing{NameColumn: "Email", Columns: []string{"State", "Emails sent", "Updated"}, Noun: "user invitations",
		Note: "Invitations to unmanaged accounts of " + resourcemanager.OrganizationDomain + " to join the customer. No mail leaves the instance; " +
			"the invited user answers with POST /cloudburrow/v1/customers/C0cloudburrow/userinvitations/EMAIL:accept (or :decline), which makes the account a managed user."}
	invs, err := p.registry.Invitations()
	if err != nil {
		l.Unavailable = consoleErr(err)
		return l, nil
	}
	for _, inv := range invs {
		email := inv.Name[strings.LastIndex(inv.Name, "/")+1:]
		r := console.Resource{Name: email, Status: inv.State, Fields: map[string]string{"State": inv.State,
			"Emails sent": strconv.FormatInt(inv.MailsSentCount, 10), "Updated": inv.UpdateTime}}
		resend := console.Action{ID: "send", Label: "Resend invitation"}
		cancel := console.Action{ID: "cancel", Label: "Cancel invitation"}
		if inv.State == resourcemanager.InvitationAccepted {
			resend.Unavailable = "The user has accepted the invitation."
		}
		if inv.State != resourcemanager.InvitationInvited {
			cancel.Unavailable = "Only a sent invitation can be cancelled."
		}
		r.Actions = []console.Action{resend, cancel}
		l.Items = append(l.Items, r)
	}
	l.Total = len(l.Items)
	return l, nil
}

func (ciInvitationsProvider) CreateTitle() string { return "Invite user" }

func (ciInvitationsProvider) CreateForm() (string, []console.Field) {
	return "Send invitation", []console.Field{{Name: "email", Label: "Email", Type: "text", Required: true,
		Help: "An unmanaged account of " + resourcemanager.OrganizationDomain + " (not one of the customer's users)."}}
}

func (p ciInvitationsProvider) Create(_ context.Context, _ string, v map[string]string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(v["email"]))
	if _, err := p.registry.InvitationAction(resourcemanager.CustomerName+"/userinvitations/"+email, "send", resourcemanager.AcceptInput{}); err != nil {
		return "", err
	}
	return email, nil
}

func (p ciInvitationsProvider) Actions(r console.Resource) []console.Action { return r.Actions }

func (p ciInvitationsProvider) Act(_ context.Context, _ string, name, action string) error {
	if action != "send" && action != "cancel" {
		return errors.New("unknown action " + action)
	}
	_, err := p.registry.InvitationAction(resourcemanager.CustomerName+"/userinvitations/"+name, action, resourcemanager.AcceptInput{})
	return err
}

var _ console.Creator = ciInvitationsProvider{}
var _ console.Actor = ciInvitationsProvider{}

// ---- Policies

type ciPoliciesProvider struct{ registry *resourcemanager.Registry }

func (ciPoliciesProvider) ID() string    { return "iam-identity-policies" }
func (ciPoliciesProvider) Title() string { return "Policies" }

func (p ciPoliciesProvider) List(_ context.Context, _ string) (console.Listing, error) {
	l := console.Listing{NameColumn: "Policy", Columns: []string{"Setting", "Org unit", "Group", "Sort order", "Type"}, Noun: "policies",
		Note: "The Cloud Identity Policy API's policies of " + resourcemanager.CustomerName + ": stored settings, which no Workspace service here reads."}
	ps, err := p.registry.Policies()
	if err != nil {
		l.Unavailable = consoleErr(err)
		return l, nil
	}
	for _, x := range ps {
		l.Items = append(l.Items, console.Resource{Name: strings.TrimPrefix(x.Name, "policies/"), Fields: map[string]string{
			"Setting": x.Setting.Type, "Org unit": x.PolicyQuery.OrgUnit, "Group": orDash(x.PolicyQuery.Group),
			"Sort order": strconv.FormatFloat(x.PolicyQuery.SortOrder, 'f', -1, 64), "Type": x.Type}})
	}
	l.Total = len(l.Items)
	return l, nil
}

func (ciPoliciesProvider) CreateTitle() string { return "Create policy" }

func (ciPoliciesProvider) CreateForm() (string, []console.Field) {
	return "Create", []console.Field{
		{Name: "type", Label: "Setting type", Type: "text", Required: true, Help: "settings/SERVICE.SETTING, such as settings/security.password."},
		{Name: "value", Label: "Setting value (JSON)", Type: "textarea", Required: true, Default: "{}"},
		{Name: "orgUnit", Label: "Organizational unit", Type: "text", Default: "orgUnits/" + resourcemanager.RootOrgUnitID, Required: true},
		{Name: "group", Label: "Group", Type: "text", Help: "Optional: groups/ID, to apply the policy to a group's members."},
	}
}

func (p ciPoliciesProvider) Create(_ context.Context, _ string, v map[string]string) (string, error) {
	x, err := p.registry.CreatePolicy(&ci.Policy{PolicyQuery: &ci.PolicyQuery{OrgUnit: strings.TrimSpace(v["orgUnit"]), Group: strings.TrimSpace(v["group"])},
		Setting: &ci.Setting{Type: strings.TrimSpace(v["type"]), Value: []byte(v["value"])}})
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(x.Name, "policies/"), nil
}

func (p ciPoliciesProvider) Detail(_ context.Context, _ string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	x, err := p.registry.GetPolicy("policies/" + path[0])
	if err != nil {
		return console.Detail{Unavailable: "cannot read the policy: " + consoleErr(err)}, nil
	}
	props := []console.Property{{Label: "Resource name", Value: x.Name}, {Label: "Setting type", Value: x.Setting.Type},
		{Label: "Setting value", Value: string(x.Setting.Value)}, {Label: "Org unit", Value: x.PolicyQuery.OrgUnit},
		{Label: "Group", Value: orDash(x.PolicyQuery.Group)}, {Label: "Query", Value: orDash(x.PolicyQuery.Query)},
		{Label: "Sort order", Value: strconv.FormatFloat(x.PolicyQuery.SortOrder, 'f', -1, 64)}, {Label: "Type", Value: x.Type}}
	return console.Detail{Summary: props[:2], Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties,
		Groups: []console.PropertyGroup{{Heading: "Policy", Properties: props}}}},
		Edit: &console.EditForm{Label: "Edit value", Fields: []console.Field{{Name: "value", Label: "Setting value (JSON)", Type: "textarea", Required: true,
			Default: string(x.Setting.Value)}}, Note: "The customer, the query and the setting type are immutable."}}, nil
}

func (p ciPoliciesProvider) Edit(_ context.Context, _ string, path []string, v map[string]string) error {
	if len(path) != 1 {
		return errors.New("only a policy can be edited")
	}
	_, err := p.registry.PatchPolicy("policies/"+path[0], &ci.Policy{Setting: &ci.Setting{Value: []byte(v["value"])}})
	return err
}

func (p ciPoliciesProvider) Delete(_ context.Context, _ string, row string) error {
	return p.registry.DeletePolicy("policies/" + row)
}

var _ console.Creator = ciPoliciesProvider{}
var _ console.Editor = ciPoliciesProvider{}
var _ console.Deleter = ciPoliciesProvider{}

// ---- Allowlisted domains

type ciAllowlistProvider struct{ registry *resourcemanager.Registry }

func (ciAllowlistProvider) ID() string    { return "iam-allowlisted-domains" }
func (ciAllowlistProvider) Title() string { return "Allowlisted domains" }

func (p ciAllowlistProvider) List(_ context.Context, _ string) (console.Listing, error) {
	l := console.Listing{NameColumn: "Domain", Columns: []string{"Resource name"}, Noun: "allowlisted domains",
		Note: "Domains on the customer's allowlist, as Cloud Identity's allowlistedDomains serve them: stored configuration."}
	ds, err := p.registry.AllowlistedDomains()
	if err != nil {
		l.Unavailable = consoleErr(err)
		return l, nil
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i].Domain < ds[j].Domain })
	for _, d := range ds {
		l.Items = append(l.Items, console.Resource{Name: d.Domain, Fields: map[string]string{"Resource name": d.Name}})
	}
	l.Total = len(l.Items)
	return l, nil
}

func (ciAllowlistProvider) CreateTitle() string { return "Add domain" }

func (ciAllowlistProvider) CreateForm() (string, []console.Field) {
	return "Add", []console.Field{{Name: "domain", Label: "Domain", Type: "text", Required: true, Help: "Such as partner.example.com."}}
}

func (p ciAllowlistProvider) Create(_ context.Context, _ string, v map[string]string) (string, error) {
	d, err := p.registry.CreateAllowlistedDomain(v["domain"])
	if err != nil {
		return "", err
	}
	return d.Domain, nil
}

func (p ciAllowlistProvider) Delete(_ context.Context, _ string, row string) error {
	ds, err := p.registry.AllowlistedDomains()
	if err != nil {
		return err
	}
	for _, d := range ds {
		if d.Domain == row {
			return p.registry.DeleteAllowlistedDomain(d.Name)
		}
	}
	return errors.New("no such allowlisted domain: " + row)
}

var _ console.Creator = ciAllowlistProvider{}
var _ console.Deleter = ciAllowlistProvider{}
