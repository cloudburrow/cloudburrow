package resourcemanager

import (
	"bytes"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	admin "google.golang.org/api/admin/directory/v1"
	ci "google.golang.org/api/cloudidentity/v1"
	"google.golang.org/api/googleapi"
)

// signIn2103 signs a user in on a device, with reported attributes.
func signIn2103(t *testing.T, url string, in map[string]any) *ci.GoogleAppsCloudidentityDevicesV1DeviceUser {
	t.Helper()
	b, _ := json.Marshal(in)
	resp, err := http.Post(url+"/cloudburrow/v1/devices:signIn", "application/json", bytes.NewReader(b))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("sign in: %v %v", resp, err)
	}
	defer resp.Body.Close()
	var du ci.GoogleAppsCloudidentityDevicesV1DeviceUser
	_ = json.NewDecoder(resp.Body).Decode(&du)
	return &du
}

// TestDeviceSearchFields2103: every Mobile device search field, with its
// values, date ranges, multi-word values and errors.
func TestDeviceSearchFields2103(t *testing.T) {
	_, c, a, url := ci2103(t)
	for _, u := range []*admin.User{
		{PrimaryEmail: "joe.sabia@" + OrganizationDomain, Name: &admin.UserName{GivenName: "Joe", FamilyName: "Sabia"}, Password: "password-1"},
		{PrimaryEmail: "joel@" + OrganizationDomain, Name: &admin.UserName{GivenName: "Joel", FamilyName: "Ortiz"}, Password: "password-2"},
	} {
		if _, err := a.Users.Insert(u).Do(); err != nil {
			t.Fatal(err)
		}
	}
	signIn2103(t, url, map[string]any{"userEmail": "joe.sabia@" + OrganizationDomain, "serialNumber": "015d3fb66e241807", "deviceType": "ANDROID",
		"model": "Nexus S", "osVersion": "Android 13", "manufacturer": "LGE", "languageCode": "fr-FR",
		"apps": []string{"com.android.mms", "Calendar"}, "hardware": "bullhead", "hardwareId": "A0000292789YD",
		"attributes": map[string]any{"brand": "google", "bootloaderVersion": "BHZ11e", "buildNumber": "6.7.2_EDEM-18", "networkOperator": "Airtel",
			"compromisedState": "UNCOMPROMISED", "encryptionState": "ENCRYPTED", "kernelVersion": "3.1.10-g1e42d16", "imei": "353020051574987",
			"wifiMacAddresses": []string{"30:85:a9:59:d5:ed"}, "securityPatchTime": "2017-08-20T00:00:00Z",
			"androidSpecificAttributes": map[string]any{"ownershipPrivilege": "PROFILE_OWNER"},
			"endpointVerificationSpecificAttributes": map[string]any{"certificateAttributes": []map[string]any{
				{"fingerprint": "3dputW0W79tfvdGY4HD6fPm6VNzlG+x0TRVFvtQnWik", "thumbprint": "0484c31611dbb49d394d7cf3750ed70edf664fbb"}}}}})
	signIn2103(t, url, map[string]any{"userEmail": "joel@" + OrganizationDomain, "serialNumber": "IOS-1", "deviceType": "IOS",
		"model": "iPhone 15", "osVersion": "iOS 9.3.1", "managementType": "basic"})
	n := func(filter string) int {
		t.Helper()
		l, err := c.Devices.List().Filter(filter).Do()
		if err != nil {
			t.Fatalf("list %q: %v", filter, err)
		}
		return len(l.Devices)
	}
	for f, want := range map[string]int{
		"": 2, "015d3fb": 1, "serial:015d3fb66e241807": 1, "apps:Calendar": 1, "apps:com.android.mms": 1, "apps:Maps": 0,
		"bootloader_version:BHZ11e": 1, "brand:google": 1, "build_number:6.7.2_EDEM-18": 1, "carrier:Airtel": 1,
		"compromised_status:uncompromised": 1, "compromised_status:undetected": 1, "compromised_status:compromised": 0,
		"default_language:French": 1, "default_language:fr": 1, "default_language:English": 1,
		"email:joe@example.com": 1, "email:joel": 1, "encryption_status:encrypted": 1, "encryption_status:not_encrypted": 0,
		"endpoint_verification_cert_fingerprint:3dputW0W79tfvdGY4HD6fPm6VNzlG+x0TRVFvtQnWik": 1,
		"endpoint_verification_cert_thumbprint:0484c31611dbb49d394d7cf3750ed70edf664fbb":     1,
		"hardware:bullhead": 1, "hid:A0000292789YD": 1, "imei:353020051574987": 1, "kernel_version:3.1.10": 1,
		"mac_address:30:85:a9:59:d5:ed": 1, "management_type:advanced": 1, "management_type:basic": 1, "manufacturer:LGE": 1,
		"meid:A09": 0, "model:nexus s": 1, "name:joe": 1, "name:Joe Sabia": 1, "name:Joey": 0, "name:Ortiz": 1,
		"os:Android": 1, "os:ios 9": 1, "os:'ios 9'": 1, "owner:byod": 2, "owner:company": 0, "privilege:profile_owner": 1,
		"privilege:device_owner": 0, "register:2000-01-01..": 2, "register:..2000-01-01": 0,
		"security_patch_level:2017-08-05..2017-09-05": 1, "security_patch_level:2017-08-20": 1, "security_patch_level:2017-09-01..": 0,
		"sync:2000-01-01..2999-01-01T00:00:00": 2, "status:pending": 2, "status:approved": 0, "type:android": 1, "type:ios": 1,
		"type:googlesync": 0, "status:pending type:ios os:ios 9": 1,
	} {
		if got := n(f); got != want {
			t.Errorf("filter %q: %d devices, want %d", f, got, want)
		}
	}
	for _, f := range []string{"color:red", "status:sleeping", "register:yesterday", "model:", "os:'ios"} {
		if _, err := c.Devices.List().Filter(f).Do(); httpCode2103(err) != http.StatusBadRequest {
			t.Errorf("filter %q = %v, want 400", f, err)
		}
	}
	if l, err := c.Devices.DeviceUsers.List("devices/-").Filter("name:Joel Ortiz").Do(); err != nil || len(l.DeviceUsers) != 1 {
		t.Errorf("deviceUsers name: = %+v, %v", l, err)
	}

	// clientStates.list takes the same grammar, over each state's device
	// and user.
	l, _ := c.Devices.DeviceUsers.List("devices/-").Do()
	for _, du := range l.DeviceUsers {
		cs := du.Name + "/clientStates/" + OrganizationCustomerID[1:] + "-scanner"
		if _, err := c.Devices.DeviceUsers.ClientStates.Patch(cs, &ci.GoogleAppsCloudidentityDevicesV1ClientState{ComplianceState: "COMPLIANT"}).Do(); err != nil {
			t.Fatal(err)
		}
	}
	for f, want := range map[string]int{"": 2, "type:ios": 1, "email:joel": 1, "status:approved": 0} {
		l, err := c.Devices.DeviceUsers.ClientStates.List("devices/-/deviceUsers/-").Filter(f).Do()
		if err != nil || len(l.ClientStates) != want {
			t.Errorf("clientStates.list %q = %+v, %v; want %d", f, l, err, want)
		}
	}
	if _, err := c.Devices.DeviceUsers.ClientStates.List("devices/-/deviceUsers/-").Filter("color:red").Do(); httpCode2103(err) != http.StatusBadRequest {
		t.Errorf("clientStates.list with an unknown field = %v", err)
	}
}

// TestAllowlistFilter2103: "only exact matches on the domain".
func TestAllowlistFilter2103(t *testing.T) {
	_, c, _, _ := ci2103(t)
	for _, d := range []string{"partner.com", "other.org"} {
		if _, err := c.AllowlistedDomains.Create(&ci.AllowlistedDomain{Domain: d}).Do(); err != nil {
			t.Fatal(err)
		}
	}
	for f, want := range map[string][]string{"": {"other.org", "partner.com"}, "domain = 'partner.com'": {"partner.com"},
		`domain="other.org"`: {"other.org"}, "domain = 'nope.com'": nil} {
		l, err := c.AllowlistedDomains.List().Filter(f).Do()
		if err != nil {
			t.Fatalf("%q: %v", f, err)
		}
		var got []string
		for _, d := range l.AllowlistedDomains {
			got = append(got, d.Domain)
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%q = %v, want %v", f, got, want)
		}
	}
	for _, f := range []string{"domain = 'a.com' OR domain = 'b.com'", "domain:partner", "name = 'x'"} {
		if _, err := c.AllowlistedDomains.List().Filter(f).Do(); httpCode2103(err) != http.StatusBadRequest {
			t.Errorf("%q = %v, want 400", f, err)
		}
	}
}

// TestUsersQuery2103: the Directory API's user search fields and
// operators.
func TestUsersQuery2103(t *testing.T) {
	_, _, a, _ := ci2103(t)
	d := OrganizationDomain
	for _, u := range []*admin.User{
		{PrimaryEmail: "jane@" + d, Name: &admin.UserName{GivenName: "Jane", FamilyName: "Smith"}, Password: "password-1",
			Organizations: []map[string]any{{"name": "Human Resources", "title": "HR Manager", "department": "People", "costCenter": "cc-1", "description": "staff"}},
			Addresses:     []map[string]any{{"country": "Sweden", "locality": "Stockholm", "streetAddress": "1 Main St", "postalCode": "11122", "region": "AB", "poBox": "PB 9", "extendedAddress": "Floor 3"}},
			Phones:        []map[string]any{{"value": "+46 555 0100", "type": "work"}},
			Ims:           []map[string]any{{"im": "jane.im", "protocol": "jabber", "type": "work"}},
			ExternalIds:   []map[string]any{{"value": "E-100", "type": "organization"}},
			OrgUnitPath:   "/Sales/EMEA",
			CustomSchemas: map[string]googleapi.RawMessage{"EmploymentData": googleapi.RawMessage(`{"projects":[{"value":"GeneGnomes"},{"value":"Other Thing"}],"location":"Atlanta","jobLevel":7,"start":"2020-05-01"}`)}},
		{PrimaryEmail: "admin-team@" + d, Name: &admin.UserName{GivenName: "Janet", FamilyName: "Ann"}, Password: "password-2", Suspended: true,
			Aliases:       []string{"admin-team@" + d},
			Relations:     []map[string]any{{"value": "jane@" + d, "type": "manager"}},
			CustomSchemas: map[string]googleapi.RawMessage{"EmploymentData": googleapi.RawMessage(`{"jobLevel":4}`)}},
		{PrimaryEmail: "bob@" + d, Name: &admin.UserName{GivenName: "Jane Ann", FamilyName: "Jones"}, Password: "password-3",
			Relations: []map[string]any{{"value": "janet@" + d, "type": "manager"}}, OrgUnitPath: "/Sales"},
	} {
		if _, err := a.Users.Insert(u).Do(); err != nil {
			t.Fatal(err)
		}
	}
	// A renamed user keeps its old address as an alias.
	if _, err := a.Users.Patch("admin-team@"+d, &admin.User{PrimaryEmail: "janet@" + d}).Do(); err != nil {
		t.Fatal(err)
	}
	jane, _ := a.Users.Get("jane@" + d).Do()
	janet, _ := a.Users.Get("janet@" + d).Do()
	find := func(q string) []string {
		t.Helper()
		l, err := a.Users.List().Customer("my_customer").Query(q).Do()
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		var out []string
		for _, u := range l.Users {
			out = append(out, u.PrimaryEmail[:len(u.PrimaryEmail)-len(d)-1])
		}
		return out
	}
	for q, want := range map[string][]string{
		"":                             {"bob", "jane", "janet"},
		"jane":                         {"bob", "jane"},
		"name='Jane Smith'":            {"jane"},
		"name:'Jane'":                  {"bob", "jane"},
		"givenName=Jane":               {"jane"},
		"givenName:Jane":               {"bob", "jane"},
		"givenName:Jan*":               {"bob", "jane", "janet"},
		"email:admin*":                 {"janet"},
		"email='admin-team@" + d + "'": {"janet"},
		"familyName=smith":             {"jane"},
		"isAdmin=false":                {"bob", "jane", "janet"},
		"isAdmin=true":                 nil,
		"isSuspended=true":             {"janet"},
		"isDelegatedAdmin=false isArchived=false isEnrolledIn2Sv=false isEnforcedIn2Sv=false isGuest=false": {"bob", "jane", "janet"},
		"im=jane.im":                     {"jane"},
		"externalId=E-100":               {"jane"},
		"manager='jane@" + d + "'":       {"bob", "janet"},
		"directManager='jane@" + d + "'": {"janet"},
		"managerId=" + jane.Id:           {"bob", "janet"},
		"directManagerId=" + janet.Id:    {"bob"},
		"address:Stockholm":              {"jane"},
		"addressCountry='Sweden'":        {"jane"},
		"addressPoBox='PB 9' addressExtended:Floor addressStreet:'Main St' addressLocality=Stockholm addressRegion=AB addressPostalCode=11122": {"jane"},
		"orgName='Human Resources'": {"jane"},
		"orgTitle:Manager":          {"jane"},
		"orgDepartment=People orgDescription:staff orgCostCenter=cc-1": {"jane"},
		"phone='+46 555 0100'":                 {"jane"},
		"orgUnitPath=/Sales":                   {"bob", "jane"},
		"orgUnitPath='/Sales/EMEA'":            {"jane"},
		"orgUnitPath=/":                        {"bob", "jane", "janet"},
		"EmploymentData.projects:'GeneGnomes'": {"jane"},
		"EmploymentData.location='Atlanta'":    {"jane"},
		"EmploymentData.jobLevel>=7":           {"jane"},
		"EmploymentData.jobLevel<7":            {"janet"},
		"EmploymentData.jobLevel:[4,7]":        {"janet"},
		"EmploymentData.jobLevel:[5,8]":        {"jane"},
		"EmploymentData.start>2020-01-01":      {"jane"},
		"orgName=Engineering orgTitle:Manager": nil,
	} {
		got := find(q)
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("query %q = %v, want %v", q, got, want)
		}
	}
	for _, q := range []string{"color=red", "isAdmin:true", "isAdmin=yes", "phone:555", "name:Ja*", "EmploymentData.location:Atl*", "name='open", "address=x"} {
		if _, err := a.Users.List().Customer("my_customer").Query(q).Do(); httpCode2103(err) != http.StatusBadRequest {
			t.Errorf("query %q = %v, want 400", q, err)
		}
	}
	if _, err := a.Users.List().Customer("my_customer").ViewType("domain_public").Query("orgUnitPath=/").Do(); httpCode2103(err) != http.StatusBadRequest {
		t.Errorf("orgUnitPath with domain_public = %v, want 400", err)
	}
}
