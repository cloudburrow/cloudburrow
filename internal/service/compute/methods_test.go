package compute

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestEveryUnservedMethodIsNotImplemented: each method of a served resource
// that the table marks unimplemented answers 501 over HTTP, at its
// discovery document's path, as tools/coverage reports it.
func TestEveryUnservedMethodIsNotImplemented(t *testing.T) {
	s, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	unserved := []struct{ id, method, path string }{
		{"compute.networks.addPeering", "POST", "projects/p/global/networks/x/addPeering"},
		{"compute.networks.cancelRequestRemovePeering", "POST", "projects/p/global/networks/x/cancelRequestRemovePeering"},
		{"compute.networks.getEffectiveFirewalls", "GET", "projects/p/global/networks/x/getEffectiveFirewalls"},
		{"compute.networks.listPeeringRoutes", "GET", "projects/p/global/networks/x/listPeeringRoutes"},
		{"compute.networks.removePeering", "POST", "projects/p/global/networks/x/removePeering"},
		{"compute.networks.requestRemovePeering", "POST", "projects/p/global/networks/x/requestRemovePeering"},
		{"compute.networks.updatePeering", "PATCH", "projects/p/global/networks/x/updatePeering"},
		{"compute.subnetworks.getIamPolicy", "GET", "projects/p/regions/us-central1/subnetworks/x/getIamPolicy"},
		{"compute.subnetworks.listUsable", "GET", "projects/p/aggregated/subnetworks/listUsable"},
		{"compute.subnetworks.setIamPolicy", "POST", "projects/p/regions/us-central1/subnetworks/x/setIamPolicy"},
		{"compute.subnetworks.testIamPermissions", "POST", "projects/p/regions/us-central1/subnetworks/x/testIamPermissions"},
		{"compute.firewalls.testIamPermissions", "POST", "projects/p/global/firewalls/x/testIamPermissions"},
		{"compute.routes.testIamPermissions", "POST", "projects/p/global/routes/x/testIamPermissions"},
		{"compute.addresses.move", "POST", "projects/p/regions/us-central1/addresses/x/move"},
		{"compute.addresses.testIamPermissions", "POST", "projects/p/regions/us-central1/addresses/x/testIamPermissions"},
		{"compute.globalAddresses.move", "POST", "projects/p/global/addresses/x/move"},
		{"compute.globalAddresses.testIamPermissions", "POST", "projects/p/global/addresses/x/testIamPermissions"},
	}
	listed := map[string]bool{}
	for _, u := range unserved {
		listed[u.id] = true
		req := httptest.NewRequest(u.method, Prefix+u.path, strings.NewReader("{}"))
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s (%s %s): %d %s", u.id, u.method, u.path, rec.Code, rec.Body)
		}
	}
	for id, built := range MethodStatuses() {
		if !built && !listed[id] {
			t.Errorf("%s is unimplemented but not checked here", id)
		}
	}
}
