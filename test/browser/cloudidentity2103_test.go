//go:build browser

package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
	admin "google.golang.org/api/admin/directory/v1"
	ci "google.golang.org/api/cloudidentity/v1"
	"google.golang.org/api/option"
)

func directoryClient(t *testing.T) *admin.Service {
	t.Helper()
	ep := endpoint(t, envResourceManager)
	svc, err := admin.NewService(context.Background(), option.WithEndpoint("http://"+strings.TrimPrefix(ep, "http://")+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// TestConsoleCloudIdentity2103 (#2103): Users' Add new user makes a
// Directory user the official client reads; Groups' Create with a dynamic
// membership query makes a dynamic group whose members are that user, and
// editing the user's department on its page takes it out; Devices' Add
// company-owned device imports a device, a signed-in user's row is
// approved and then removed on the device's page; User invitations' Invite user sends an
// invitation; Policies' Create and Allowlisted domains' Add make what the
// official client lists. Then every screen and dialog is audited for WCAG
// (names, contrast in both themes, focus and dialogs).
func TestConsoleCloudIdentity2103(t *testing.T) {
	for _, s := range []string{"iam-users", "iam-groups", "iam-devices", "iam-user-invitations", "iam-identity-policies", "iam-allowlisted-domains"} {
		needService(t, s)
	}
	svc := cloudIdentityClient(t)
	dir := directoryClient(t)
	project := uniqueProject(t)
	sfx := project[len(project)-6:]
	email := "bt-" + sfx + "@cloudburrow.local"
	dept := "dept" + sfx

	p := open(t)
	p.navigate("/iam-admin/users?project=" + project)
	p.clickText("#view button", "Add new user")
	p.waitFor(`document.querySelector("#view #f-email, .modal.is-open #f-email") !== null`)
	sel := func(id string) string {
		return fmt.Sprintf(`document.querySelector(".modal.is-open #f-%s") ? ".modal.is-open #f-%s" : "#view #f-%s"`, id, id, id)
	}
	field := func(id, v string) {
		var s string
		p.eval(sel(id), &s)
		p.setField(s, v)
	}
	field("givenName", "Browser")
	field("familyName", "Tester")
	field("email", email)
	field("password", "password-"+sfx)
	field("department", dept)
	p.eval(`(document.querySelector('.modal.is-open button[type="submit"]') || document.querySelector('#view form button[type="submit"].primary')).click()`, nil)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, email))
	u, err := dir.Users.Get(email).Do()
	if err != nil || u.Name.GivenName != "Browser" {
		t.Fatalf("users.get after Add new user = %+v, %v", u, err)
	}
	t.Cleanup(func() { _ = dir.Users.Delete(email).Do() })

	group := "btdyn-" + sfx + "@cloudburrow.local"
	p.navigate("/iam-admin/groups/create?project=" + project)
	p.waitFor(`document.querySelector("#view #f-dynamicQuery") !== null`)
	p.setField("#view #f-email", group)
	p.setField("#view #f-dynamicQuery", "user.organizations.exists(org, org.department=='"+dept+"')")
	p.eval(`document.querySelector('#view form button[type="submit"].primary').click()`, nil)
	p.waitFor(`!location.pathname.endsWith("/create")`)
	look, err := svc.Groups.Lookup().GroupKeyId(group).Do()
	if err != nil {
		t.Fatalf("groups.lookup of the dynamic group: %v", err)
	}
	t.Cleanup(func() { _, _ = svc.Groups.Delete(look.Name).Do() })
	members := func() []*ci.Membership {
		l, err := svc.Groups.Memberships.List(look.Name).Do()
		if err != nil {
			t.Fatal(err)
		}
		return l.Memberships
	}
	if ms := members(); len(ms) != 1 || ms[0].PreferredMemberKey.Id != email {
		t.Fatalf("the dynamic group's members = %+v", ms)
	}
	p.navigate("/iam-admin/groups/" + group + "?project=" + project)
	p.waitFor(fmt.Sprintf(`document.querySelector("#view").textContent.includes(%q)`, email))
	p.clickText("#view [role=tab]", "Details")
	p.waitFor(`document.querySelector("#view").textContent.includes("Dynamic membership query")`)

	// Edit user: another department, out of the group.
	p.navigate("/iam-admin/users/" + email + "?project=" + project)
	p.clickText("#view .page-actions button", "Edit user")
	p.waitFor(`document.querySelector(".modal.is-open #f-department") !== null`)
	p.setField(".modal.is-open #f-department", "elsewhere")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	if ms := members(); len(ms) != 0 {
		t.Errorf("after Edit user, the dynamic group's members = %+v", ms)
	}

	// Devices.
	serial := "BT-" + sfx
	p.navigate("/iam-admin/devices?project=" + project)
	p.clickText("#view button", "Add")
	p.waitFor(`document.querySelector("#view #f-serialNumber, .modal.is-open #f-serialNumber") !== null`)
	field("serialNumber", serial)
	p.eval(`(document.querySelector('.modal.is-open button[type="submit"]') || document.querySelector('#view form button[type="submit"].primary')).click()`, nil)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, serial))
	l, err := svc.Devices.List().Filter("serial:" + serial).Do()
	if err != nil || len(l.Devices) != 1 || l.Devices[0].OwnerType != "COMPANY" {
		t.Fatalf("devices.list after Add = %+v, %v", l, err)
	}
	dev := l.Devices[0]
	t.Cleanup(func() { _, _ = svc.Devices.Delete(dev.Name).Do() })
	body, _ := json.Marshal(map[string]string{"userEmail": email, "serialNumber": serial, "deviceType": "ANDROID"})
	resp, err := http.Post("http://"+strings.TrimPrefix(endpoint(t, envResourceManager), "http://")+"/cloudburrow/v1/devices:signIn", "application/json", bytes.NewReader(body))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("sign in = %v, %v", resp, err)
	}
	var du ci.GoogleAppsCloudidentityDevicesV1DeviceUser
	_ = json.NewDecoder(resp.Body).Decode(&du)
	resp.Body.Close()
	id := strings.TrimPrefix(dev.Name, "devices/")
	p.navigate("/iam-admin/devices/" + id + "?project=" + project)
	p.rowAction("Users", du.Name[strings.LastIndex(du.Name, "/")+1:], "Approve", nil, "")
	if got, err := svc.Devices.DeviceUsers.Get(du.Name).Do(); err != nil || got.ManagementState != "APPROVED" {
		t.Errorf("after Approve, the device user = %+v, %v", got, err)
	}
	if got, err := svc.Devices.Get(dev.Name).Do(); err != nil || got.ManagementState != "APPROVED" {
		t.Errorf("after Approve, the device = %+v, %v", got, err)
	}
	duID := du.Name[strings.LastIndex(du.Name, "/")+1:]
	p.rowAction("Users", duID, "Remove account", nil, duID)
	if _, err := svc.Devices.DeviceUsers.Get(du.Name).Do(); err == nil {
		t.Error("after Remove account, the device user is still there")
	}

	// User invitations, policies, allowlisted domains.
	inviteEmail := "btinv-" + sfx + "@cloudburrow.local"
	p.navigate("/iam-admin/user-invitations?project=" + project)
	p.clickText("#view button", "Send invitation")
	p.waitFor(`document.querySelector("#view #f-email, .modal.is-open #f-email") !== null`)
	field("email", inviteEmail)
	p.eval(`(document.querySelector('.modal.is-open button[type="submit"]') || document.querySelector('#view form button[type="submit"].primary')).click()`, nil)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, inviteEmail))
	if inv, err := svc.Customers.Userinvitations.Get("customers/C0cloudburrow/userinvitations/" + inviteEmail).Do(); err != nil || inv.State != "INVITED" {
		t.Errorf("the invitation = %+v, %v", inv, err)
	}

	stype := "settings/browser.test_" + sfx
	p.navigate("/iam-admin/identity-policies?project=" + project)
	p.clickText("#view button", "Create")
	p.waitFor(`document.querySelector("#view #f-type, .modal.is-open #f-type") !== null`)
	field("type", stype)
	field("value", `{"enabled":true}`)
	p.eval(`(document.querySelector('.modal.is-open button[type="submit"]') || document.querySelector('#view form button[type="submit"].primary')).click()`, nil)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, stype))
	pl, err := svc.Policies.List().Filter(fmt.Sprintf("setting.type.matches('^%s$')", stype)).Do()
	if err != nil || len(pl.Policies) != 1 {
		t.Fatalf("policies.list after Create = %+v, %v", pl, err)
	}
	t.Cleanup(func() { _, _ = svc.Policies.Delete(pl.Policies[0].Name).Do() })

	domain := "bt-" + sfx + ".example.com"
	p.navigate("/iam-admin/allowlisted-domains?project=" + project)
	p.clickText("#view button", "Add")
	p.waitFor(`document.querySelector("#view #f-domain, .modal.is-open #f-domain") !== null`)
	field("domain", domain)
	p.eval(`(document.querySelector('.modal.is-open button[type="submit"]') || document.querySelector('#view form button[type="submit"].primary')).click()`, nil)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, domain))
	dl, err := svc.AllowlistedDomains.List().Do()
	found := ""
	for _, d := range dl.AllowlistedDomains {
		if d.Domain == domain {
			found = d.Name
		}
	}
	if err != nil || found == "" {
		t.Fatalf("allowlistedDomains.list after Add = %+v, %v", dl, err)
	}
	t.Cleanup(func() { _, _ = svc.AllowlistedDomains.Delete(found).Do() })

	var pages []a11yPage
	for _, s := range []struct{ name, path string }{
		{"Users", "/iam-admin/users"}, {"User", "/iam-admin/users/" + email}, {"Dynamic group", "/iam-admin/groups/" + group},
		{"Create group", "/iam-admin/groups/create"}, {"Devices", "/iam-admin/devices"}, {"Device", "/iam-admin/devices/" + id},
		{"User invitations", "/iam-admin/user-invitations"}, {"Policies", "/iam-admin/identity-policies"},
		{"Policy", "/iam-admin/identity-policies/" + strings.TrimPrefix(pl.Policies[0].Name, "policies/")},
		{"Allowlisted domains", "/iam-admin/allowlisted-domains"},
	} {
		pages = append(pages, screen(s.name, s.path, project))
	}
	auditScreens(t, pages)
}
