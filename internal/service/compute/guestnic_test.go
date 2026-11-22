package compute

import (
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// The guest's metadata answers the keys Google's Ops Agent reads (#2139),
// as Google's metadata server does: instance/tags as a JSON list, the
// network interfaces' directories, ip, mac, network
// (projects/NUMBER/networks/NAME) and access configs with external-ip
// (the natIP), and a directory named without its slash as a 301 to it.
func TestGuestMetadataNetworkInterfacesAndTags(t *testing.T) {
	s, err := New(Options{VMs: fakeVMs{}})
	if err != nil {
		t.Fatal(err)
	}
	z := "zones/us-central1-a/"
	mk := func(name string, extra obj) {
		t.Helper()
		b := obj{"name": name, "machineType": z + "machineTypes/e2-small",
			"disks":             []any{obj{"boot": true, "initializeParams": obj{"sourceImage": "projects/debian-cloud/global/images/family/debian-12"}}},
			"networkInterfaces": []any{obj{"network": "global/networks/default"}}}
		for k, v := range extra {
			b[k] = v
		}
		if _, err := vmCall(t, s, "POST", z+"instances", b); err != nil {
			t.Fatal(err)
		}
	}
	mk("web", obj{"tags": obj{"items": []any{"http-server", "web"}},
		"networkInterfaces": []any{obj{"network": "global/networks/default", "accessConfigs": []any{obj{"name": "External NAT", "type": "ONE_TO_ONE_NAT"}}}}})
	mk("internal", nil)
	inst, _ := s.Call("GET", "p", z+"instances/web", nil, nil)
	nic := inst["networkInterfaces"].([]any)[0].(map[string]any)
	natIP := str(nic["accessConfigs"].([]any)[0].(map[string]any), "natIP")
	if natIP == "" {
		t.Fatalf("no natIP: %v", nic)
	}
	token := func(name string) string {
		for k, o := range s.res {
			if strings.HasSuffix(k, "/instances/"+name) {
				return str(o, "_guestToken")
			}
		}
		return ""
	}
	get := func(name, path string) (int, string, string) {
		req := httptest.NewRequest("GET", GuestPrefix+token(name)+"/computeMetadata/v1/"+path, nil)
		req.Header.Set("Metadata-Flavor", "Google")
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String(), rec.Header().Get("Location")
	}
	number := strconv.FormatInt(resource.ProjectNumber("p"), 10)
	for _, c := range []struct{ vm, path, want string }{
		{"web", "instance/tags", `["http-server","web"]`},
		{"internal", "instance/tags", `[]`},
		{"web", "instance/network-interfaces/", "0/\n"},
		{"web", "instance/network-interfaces/0/", "access-configs/\nip\nmac\nnetwork\n"},
		{"web", "instance/network-interfaces/0/ip", str(nic, "networkIP")},
		{"web", "instance/network-interfaces/0/mac", macOf(str(nic, "networkIP"))},
		{"web", "instance/network-interfaces/0/network", "projects/" + number + "/networks/default"},
		{"web", "instance/network-interfaces/0/access-configs/", "0/\n"},
		{"web", "instance/network-interfaces/0/access-configs/0/external-ip", natIP},
		{"web", "instance/network-interfaces/0/access-configs/0/type", "ONE_TO_ONE_NAT"},
		{"internal", "instance/network-interfaces/0/access-configs/", "0/\n"},
		{"internal", "instance/network-interfaces/0/access-configs/0/external-ip", ""},
		{"internal", "instance/network-interfaces/0/access-configs/0/type", "ONE_TO_ONE_NAT"},
	} {
		if code, body, _ := get(c.vm, c.path); code != 200 || body != c.want {
			t.Errorf("%s %s: %d %q, want %q", c.vm, c.path, code, body, c.want)
		}
	}
	for _, path := range []string{"instance/network-interfaces/0/access-configs/1/external-ip", "instance/network-interfaces/1/ip", "instance/network-interfaces/x/ip"} {
		if code, _, _ := get("internal", path); code != 404 {
			t.Errorf("internal %s: %d, want 404", path, code)
		}
	}
	for _, path := range []string{"instance/attributes", "instance/network-interfaces", "instance/network-interfaces/0", "instance/network-interfaces/0/access-configs/0"} {
		code, _, loc := get("web", path+"?alt=text")
		if want := GuestPrefix + token("web") + "/computeMetadata/v1/" + path + "/?alt=text"; code != 301 || loc != want {
			t.Errorf("%s: %d %q, want 301 to %q", path, code, loc, want)
		}
	}
}
