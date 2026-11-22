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
	"google.golang.org/api/googleapi"
)

// covers: cloudidentity.devices.list, cloudidentity.devices.deviceUsers.list, cloudidentity.devices.deviceUsers.clientStates.list, cloudidentity.allowlistedDomains.list
//
// TestCloudIdentitySearch2103 (#2103, round 2): devices.list's Mobile
// device search fields (every field, its values, date ranges, multi-word
// values, unknown fields 400), deviceUsers.list's and clientStates.list's
// filters in the same grammar, and allowlistedDomains.list's exact domain
// filter, with the official cloudidentity/v1 client.
func TestCloudIdentitySearch2103(t *testing.T) {
	h := New(t)
	rm := h.Endpoint(EnvResourceManager)
	svc := cloudIdentity(t, h, rm)
	a := directoryUsers(t, h, rm)
	base := "http://" + strings.TrimPrefix(rm, "http://")
	sfx := fmt.Sprintf("%d", time.Now().UnixNano()%1e9)
	joe := newDirectoryUser(t, a, &admin.User{PrimaryEmail: "joe.s" + sfx + "@" + localDomain, Name: &admin.UserName{GivenName: "Joe" + sfx, FamilyName: "Sabia"}})
	joel := newDirectoryUser(t, a, &admin.User{PrimaryEmail: "joel" + sfx + "@" + localDomain, Name: &admin.UserName{GivenName: "Joel" + sfx, FamilyName: "Ortiz"}})
	signIn := func(in map[string]any) *ci.GoogleAppsCloudidentityDevicesV1DeviceUser {
		t.Helper()
		b, _ := json.Marshal(in)
		resp, err := http.Post(base+"/cloudburrow/v1/devices:signIn", "application/json", bytes.NewReader(b))
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("sign in = %v, %v", resp, err)
		}
		defer resp.Body.Close()
		var du ci.GoogleAppsCloudidentityDevicesV1DeviceUser
		_ = json.NewDecoder(resp.Body).Decode(&du)
		dev := du.Name[:strings.Index(du.Name, "/deviceUsers/")]
		t.Cleanup(func() { _, _ = svc.Devices.Delete(dev).Do() })
		return &du
	}
	serialA, serialB := "SA"+sfx, "SB"+sfx
	ua := signIn(map[string]any{"userEmail": joe.PrimaryEmail, "serialNumber": serialA, "deviceType": "ANDROID", "model": "Nexus S",
		"osVersion": "Android 13", "manufacturer": "LGE", "languageCode": "fr-FR", "apps": []string{"com.android.mms", "Calendar"},
		"hardware": "bullhead", "hardwareId": "HID" + sfx, "attributes": map[string]any{"brand": "google", "bootloaderVersion": "BHZ11e",
			"buildNumber": "6.7.2_EDEM-18", "networkOperator": "Airtel", "compromisedState": "UNCOMPROMISED", "encryptionState": "ENCRYPTED",
			"kernelVersion": "3.1.10-g1e42d16", "imei": "35" + sfx, "meid": "A0" + sfx, "wifiMacAddresses": []string{"30:85:a9:59:d5:ed"},
			"securityPatchTime": "2017-08-20T00:00:00Z", "androidSpecificAttributes": map[string]any{"ownershipPrivilege": "PROFILE_OWNER"},
			"endpointVerificationSpecificAttributes": map[string]any{"certificateAttributes": []map[string]any{
				{"fingerprint": "fp" + sfx, "thumbprint": "tp" + sfx}}}}})
	signIn(map[string]any{"userEmail": joel.PrimaryEmail, "serialNumber": serialB, "deviceType": "IOS", "model": "iPhone 15",
		"osVersion": "iOS 9.3.1", "managementType": "basic"})
	if _, err := svc.Devices.DeviceUsers.Approve(ua.Name, &ci.GoogleAppsCloudidentityDevicesV1ApproveDeviceUserRequest{}).Do(); err != nil {
		t.Fatalf("approve: %v", err)
	}

	// Each filter is narrowed to this test's two devices by its serial
	// prefix, when it does not name one.
	mine := func(l []*ci.GoogleAppsCloudidentityDevicesV1Device) []string {
		var out []string
		for _, d := range l {
			if d.SerialNumber == serialA || d.SerialNumber == serialB {
				out = append(out, d.SerialNumber[:2])
			}
		}
		slices.Sort(out)
		return out
	}
	A, B, AB := []string{"SA"}, []string{"SB"}, []string{"SA", "SB"}
	for f, want := range map[string][]string{
		serialA: A, "serial:" + serialB: B, "apps:Calendar": A, "apps:com.android.mms": A, "bootloader_version:BHZ11e": A,
		"brand:google": A, "build_number:6.7.2_EDEM-18": A, "carrier:Airtel": A, "compromised_status:uncompromised": A,
		"compromised_status:undetected": B, "compromised_status:compromised": nil, "default_language:French": A,
		"default_language:English": B, "email:joe.s" + sfx: A, "email:" + joel.PrimaryEmail: B, "encryption_status:encrypted": A,
		"endpoint_verification_cert_fingerprint:fp" + sfx: A, "endpoint_verification_cert_thumbprint:tp" + sfx: A,
		"hardware:bullhead": A, "hid:HID" + sfx: A, "id:" + strings.TrimPrefix(ua.Name[:strings.Index(ua.Name, "/deviceUsers/")], "devices/"): A,
		"imei:35" + sfx: A, "kernel_version:3.1.10": A, "mac_address:30:85:a9:59:d5:ed": A, "management_type:advanced": A,
		"management_type:basic": B, "manufacturer:LGE": A, "meid:A0" + sfx: A, "model:nexus s": A, "name:Joe" + sfx: A,
		"name:Joe" + sfx + " Sabia": A, "name:Joel" + sfx + " Ortiz": B, "os:Android": A, "os:ios 9": B, "os:'ios 9'": B,
		"owner:byod": AB, "owner:company": nil, "privilege:profile_owner": A, "privilege:device_owner": nil,
		"register:2000-01-01..": AB, "register:..2000-01-01": nil, "security_patch_level:2017-08-05..2017-09-05": A,
		"security_patch_level:2017-08-20": A, "sync:2000-01-01..2999-01-01T00:00:00": AB, "status:approved": A,
		"status:pending": B, "type:android": A, "type:ios": B, "type:googlesync": nil, "status:pending type:ios os:ios 9": B,
	} {
		l, err := svc.Devices.List().Filter(f).PageSize(100).Do()
		if err != nil {
			t.Errorf("devices.list %q: %v", f, err)
			continue
		}
		all := l.Devices
		for l.NextPageToken != "" {
			if l, err = svc.Devices.List().Filter(f).PageSize(100).PageToken(l.NextPageToken).Do(); err != nil {
				t.Fatalf("devices.list %q next page: %v", f, err)
			}
			all = append(all, l.Devices...)
		}
		if got := mine(all); !slices.Equal(got, want) {
			t.Errorf("devices.list %q = %v, want %v", f, got, want)
		}
	}
	for _, f := range []string{"color:red", "status:sleeping", "register:yesterday", "model:", "os:'ios"} {
		if _, err := svc.Devices.List().Filter(f).Do(); apiCode(err) != http.StatusBadRequest {
			t.Errorf("devices.list %q = %v, want 400", f, err)
		}
	}
	if l, err := svc.Devices.DeviceUsers.List("devices/-").Filter("name:Joel" + sfx + " Ortiz").Do(); err != nil || len(l.DeviceUsers) != 1 {
		t.Errorf("deviceUsers.list name: = %+v, %v", l, err)
	}

	// clientStates.list's filter: devices.list's grammar (UNVERIFIED (docs
	// re-read 2026-11-21:
	// <https://docs.cloud.google.com/identity/docs/reference/rest/v1/devices.deviceUsers.clientStates/list>):
	// Google does not document it), over each state's device and user.
	l, err := svc.Devices.DeviceUsers.List("devices/-").Filter("email:joe.s" + sfx).Do()
	if err != nil || len(l.DeviceUsers) != 1 {
		t.Fatalf("deviceUsers.list = %+v, %v", l, err)
	}
	cs := l.DeviceUsers[0].Name + "/clientStates/" + "0cloudburrow-search" + sfx
	if _, err := svc.Devices.DeviceUsers.ClientStates.Patch(cs, &ci.GoogleAppsCloudidentityDevicesV1ClientState{ComplianceState: "COMPLIANT"}).Do(); err != nil {
		t.Fatalf("clientStates.patch: %v", err)
	}
	for f, want := range map[string]int{"serial:" + serialA: 1, "serial:" + serialA + " type:ios": 0, "email:joe.s" + sfx + " status:approved": 1} {
		l, err := svc.Devices.DeviceUsers.ClientStates.List("devices/-/deviceUsers/-").Filter(f).Do()
		if err != nil || len(l.ClientStates) != want {
			t.Errorf("clientStates.list %q = %+v, %v; want %d", f, l, err, want)
		}
	}
	if _, err := svc.Devices.DeviceUsers.ClientStates.List("devices/-/deviceUsers/-").Filter("color:red").Do(); apiCode(err) != http.StatusBadRequest {
		t.Errorf("clientStates.list unknown field = %v, want 400", err)
	}

	// allowlistedDomains.list: "only exact matches on the domain are
	// supported, such as \"domain = 'google.com'\", with no composite
	// conditions".
	dom := "allow" + sfx + ".example.com"
	op, err := svc.AllowlistedDomains.Create(&ci.AllowlistedDomain{Domain: dom}).Do()
	if err != nil {
		t.Fatalf("allowlistedDomains.create: %v", err)
	}
	var ad ci.AllowlistedDomain
	_ = json.Unmarshal(op.Response, &ad)
	t.Cleanup(func() { _, _ = svc.AllowlistedDomains.Delete(ad.Name).Do() })
	if l, err := svc.AllowlistedDomains.List().Filter("domain = '" + dom + "'").Do(); err != nil || len(l.AllowlistedDomains) != 1 || l.AllowlistedDomains[0].Domain != dom {
		t.Errorf("allowlistedDomains.list exact = %+v, %v", l, err)
	}
	if l, err := svc.AllowlistedDomains.List().Filter("domain = 'no" + sfx + ".example.com'").Do(); err != nil || len(l.AllowlistedDomains) != 0 {
		t.Errorf("allowlistedDomains.list other = %+v, %v", l, err)
	}
	if _, err := svc.AllowlistedDomains.List().Filter("domain = 'a.com' OR domain = 'b.com'").Do(); apiCode(err) != http.StatusBadRequest {
		t.Errorf("a composite filter = %v, want 400", err)
	}
}

// TestDirectoryUsersQuery2103 (#2103, round 2): users.list's query, every
// field of Google's "Search for users" guide with its operators, with the
// official admin/directory/v1 client.
func TestDirectoryUsersQuery2103(t *testing.T) {
	h := New(t)
	rm := h.Endpoint(EnvResourceManager)
	a := directoryUsers(t, h, rm)
	sfx := fmt.Sprintf("%d", time.Now().UnixNano()%1e9)
	d := localDomain
	jane := newDirectoryUser(t, a, &admin.User{PrimaryEmail: "jane" + sfx + "@" + d, Name: &admin.UserName{GivenName: "Jane", FamilyName: "Smith" + sfx},
		Organizations: []map[string]any{{"name": "Human Resources", "title": "HR Manager", "department": "People" + sfx, "costCenter": "cc-1", "description": "staff"}},
		Addresses:     []map[string]any{{"country": "Sweden", "locality": "Stockholm", "streetAddress": "1 Main St", "postalCode": "11122", "region": "AB", "poBox": "PB 9", "extendedAddress": "Floor 3"}},
		Phones:        []map[string]any{{"value": "+46 555 " + sfx, "type": "work"}},
		Ims:           []map[string]any{{"im": "jane" + sfx, "protocol": "jabber", "type": "work"}},
		ExternalIds:   []map[string]any{{"value": "E-" + sfx, "type": "organization"}},
		OrgUnitPath:   "/S" + sfx + "/EMEA",
		CustomSchemas: map[string]googleapi.RawMessage{"Emp" + sfx: googleapi.RawMessage(`{"projects":[{"value":"GeneGnomes"}],"location":"Atlanta","jobLevel":7,"start":"2020-05-01"}`)}})
	janet := newDirectoryUser(t, a, &admin.User{PrimaryEmail: "team" + sfx + "@" + d, Name: &admin.UserName{GivenName: "Janet", FamilyName: "Smith" + sfx}, Suspended: true,
		Relations:     []map[string]any{{"value": jane.PrimaryEmail, "type": "manager"}},
		CustomSchemas: map[string]googleapi.RawMessage{"Emp" + sfx: googleapi.RawMessage(`{"jobLevel":4}`)}})
	// A renamed user keeps its old address as an alias.
	renamed := "janet" + sfx + "@" + d
	if _, err := a.Users.Patch(janet.Id, &admin.User{PrimaryEmail: renamed}).Do(); err != nil {
		t.Fatalf("rename: %v", err)
	}
	bob := newDirectoryUser(t, a, &admin.User{PrimaryEmail: "bob" + sfx + "@" + d, Name: &admin.UserName{GivenName: "Jane Ann", FamilyName: "Smith" + sfx},
		Relations: []map[string]any{{"value": renamed, "type": "manager"}}, OrgUnitPath: "/S" + sfx})
	who := map[string]string{jane.Id: "jane", janet.Id: "janet", bob.Id: "bob"}
	find := func(q string) []string {
		t.Helper()
		var out []string
		call := a.Users.List().Customer("my_customer").Query(q).MaxResults(500)
		for {
			l, err := call.Do()
			if err != nil {
				t.Fatalf("users.list %q: %v", q, err)
			}
			for _, u := range l.Users {
				if n, ok := who[u.Id]; ok {
					out = append(out, n)
				}
			}
			if l.NextPageToken == "" {
				break
			}
			call.PageToken(l.NextPageToken)
		}
		slices.Sort(out)
		return out
	}
	fam := "familyName=Smith" + sfx + " "
	all := []string{"bob", "jane", "janet"}
	for q, want := range map[string][]string{
		fam:                                 {"bob", "jane", "janet"},
		fam + "jane":                        {"bob", "jane"},
		"name='Jane Smith" + sfx + "'":      {"jane"},
		fam + "name:'Jane'":                 {"bob", "jane"},
		fam + "givenName=Jane":              {"jane"},
		fam + "givenName:Jane":              {"bob", "jane"},
		fam + "givenName:Jan*":              all,
		"email:team" + sfx + "*":            {"janet"},
		"email='team" + sfx + "@" + d + "'": {"janet"},
		"email:jane" + sfx:                  {"jane"},
		fam + "isAdmin=false":               all,
		fam + "isAdmin=true":                nil,
		fam + "isSuspended=true":            {"janet"},
		fam + "isDelegatedAdmin=false isArchived=false isEnrolledIn2Sv=false isEnforcedIn2Sv=false isGuest=false": all,
		"im=jane" + sfx:                             {"jane"},
		"externalId=E-" + sfx:                       {"jane"},
		"manager='" + jane.PrimaryEmail + "'":       {"bob", "janet"},
		"directManager='" + jane.PrimaryEmail + "'": {"janet"},
		"managerId=" + jane.Id:                      {"bob", "janet"},
		"directManagerId=" + janet.Id:               {"bob"},
		fam + "address:Stockholm":                   {"jane"},
		fam + "addressCountry='Sweden'":             {"jane"},
		fam + "addressPoBox='PB 9' addressExtended:Floor addressStreet:'Main St' addressLocality=Stockholm addressRegion=AB addressPostalCode=11122": {"jane"},
		fam + "orgName='Human Resources'": {"jane"},
		fam + "orgTitle:Manager":          {"jane"},
		"orgDepartment=People" + sfx + " orgDescription:staff orgCostCenter=cc-1": {"jane"},
		"phone='+46 555 " + sfx + "'":                {"jane"},
		"orgUnitPath=/S" + sfx:                       {"bob", "jane"},
		"orgUnitPath='/S" + sfx + "/EMEA'":           {"jane"},
		fam + "orgUnitPath=/":                        all,
		"Emp" + sfx + ".projects:'GeneGnomes'":       {"jane"},
		"Emp" + sfx + ".location='Atlanta'":          {"jane"},
		"Emp" + sfx + ".jobLevel>=7":                 {"jane"},
		"Emp" + sfx + ".jobLevel<7":                  {"janet"},
		"Emp" + sfx + ".jobLevel:[4,7]":              {"janet"},
		"Emp" + sfx + ".jobLevel:[5,8]":              {"jane"},
		"Emp" + sfx + ".start>2020-01-01":            {"jane"},
		fam + "orgName=Engineering orgTitle:Manager": nil,
	} {
		if got := find(q); !slices.Equal(got, want) {
			t.Errorf("users.list query %q = %v, want %v", q, got, want)
		}
	}
	for _, q := range []string{"color=red", "isAdmin:true", "isAdmin=yes", "phone:555", "name:Ja*", "Emp" + sfx + ".location:Atl*", "name='open", "address=x"} {
		if _, err := a.Users.List().Customer("my_customer").Query(q).Do(); apiCode(err) != http.StatusBadRequest {
			t.Errorf("users.list query %q = %v, want 400", q, err)
		}
	}
	if _, err := a.Users.List().Customer("my_customer").ViewType("domain_public").Query("orgUnitPath=/").Do(); apiCode(err) != http.StatusBadRequest {
		t.Errorf("orgUnitPath with domain_public = %v, want 400", err)
	}
}
