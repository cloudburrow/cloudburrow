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
	s, err := New(Options{VMs: fakeVMs{}})
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
		{"compute.subnetworks.listUsable", "GET", "projects/p/aggregated/subnetworks/listUsable"},
		{"compute.firewalls.testIamPermissions", "POST", "projects/p/global/firewalls/x/testIamPermissions"},
		{"compute.routes.testIamPermissions", "POST", "projects/p/global/routes/x/testIamPermissions"},
		{"compute.addresses.move", "POST", "projects/p/regions/us-central1/addresses/x/move"},
		{"compute.addresses.testIamPermissions", "POST", "projects/p/regions/us-central1/addresses/x/testIamPermissions"},
		{"compute.forwardingRules.patch", "PATCH", "projects/p/regions/us-central1/forwardingRules/x"},
		{"compute.forwardingRules.setTarget", "POST", "projects/p/regions/us-central1/forwardingRules/x/setTarget"},
		{"compute.globalAddresses.move", "POST", "projects/p/global/addresses/x/move"},
		{"compute.globalAddresses.testIamPermissions", "POST", "projects/p/global/addresses/x/testIamPermissions"},
		{"compute.images.delete", "DELETE", "projects/debian-cloud/global/images/x"},
		{"compute.images.deprecate", "POST", "projects/debian-cloud/global/images/x/deprecate"},
		{"compute.images.getIamPolicy", "GET", "projects/debian-cloud/global/images/x/getIamPolicy"},
		{"compute.images.insert", "POST", "projects/p/global/images"},
		{"compute.images.patch", "PATCH", "projects/p/global/images/x"},
		{"compute.images.setIamPolicy", "POST", "projects/p/global/images/x/setIamPolicy"},
		{"compute.images.setLabels", "POST", "projects/p/global/images/x/setLabels"},
		{"compute.images.testIamPermissions", "POST", "projects/p/global/images/x/testIamPermissions"},
		{"compute.machineTypes.aggregatedList", "GET", "projects/p/aggregated/machineTypes"},
		{"compute.disks.update", "PATCH", "projects/p/zones/us-central1-a/disks/x"},
		{"compute.regionDisks.update", "PATCH", "projects/p/regions/us-central1/disks/x"},
		{"compute.regionDisks.updateKmsKey", "POST", "projects/p/regions/us-central1/disks/x/updateKmsKey"},
		{"compute.resourcePolicies.patch", "PATCH", "projects/p/regions/us-central1/resourcePolicies/x"},
		{"compute.resourcePolicies.getIamPolicy", "GET", "projects/p/regions/us-central1/resourcePolicies/x/getIamPolicy"},
		{"compute.resourcePolicies.setIamPolicy", "POST", "projects/p/regions/us-central1/resourcePolicies/x/setIamPolicy"},
		{"compute.resourcePolicies.testIamPermissions", "POST", "projects/p/regions/us-central1/resourcePolicies/x/testIamPermissions"},
		{"compute.instances.addAccessConfig", "POST", "projects/p/zones/us-central1-a/instances/x/addAccessConfig"},
		{"compute.instances.addResourcePolicies", "POST", "projects/p/zones/us-central1-a/instances/x/addResourcePolicies"},
		{"compute.instances.deleteAccessConfig", "POST", "projects/p/zones/us-central1-a/instances/x/deleteAccessConfig"},
		{"compute.instances.getEffectiveFirewalls", "GET", "projects/p/zones/us-central1-a/instances/x/getEffectiveFirewalls"},
		{"compute.instances.getGuestAttributes", "GET", "projects/p/zones/us-central1-a/instances/x/getGuestAttributes"},
		{"compute.instances.getScreenshot", "GET", "projects/p/zones/us-central1-a/instances/x/getScreenshot"},
		{"compute.instances.getShieldedInstanceIdentity", "GET", "projects/p/zones/us-central1-a/instances/x/getShieldedInstanceIdentity"},
		{"compute.instances.performMaintenance", "POST", "projects/p/zones/us-central1-a/instances/x/performMaintenance"},
		{"compute.instances.removeResourcePolicies", "POST", "projects/p/zones/us-central1-a/instances/x/removeResourcePolicies"},
		{"compute.instances.setMinCpuPlatform", "POST", "projects/p/zones/us-central1-a/instances/x/setMinCpuPlatform"},
		{"compute.instances.setName", "POST", "projects/p/zones/us-central1-a/instances/x/setName"},
		{"compute.instances.setScheduling", "POST", "projects/p/zones/us-central1-a/instances/x/setScheduling"},
		{"compute.instances.setServiceAccount", "POST", "projects/p/zones/us-central1-a/instances/x/setServiceAccount"},
		{"compute.instances.setShieldedInstanceIntegrityPolicy", "PATCH", "projects/p/zones/us-central1-a/instances/x/setShieldedInstanceIntegrityPolicy"},
		{"compute.instances.simulateMaintenanceEvent", "POST", "projects/p/zones/us-central1-a/instances/x/simulateMaintenanceEvent"},
		{"compute.instances.startWithEncryptionKey", "POST", "projects/p/zones/us-central1-a/instances/x/startWithEncryptionKey"},
		{"compute.instances.updateAccessConfig", "POST", "projects/p/zones/us-central1-a/instances/x/updateAccessConfig"},
		{"compute.instances.updateDisplayDevice", "PATCH", "projects/p/zones/us-central1-a/instances/x/updateDisplayDevice"},
		{"compute.instances.updateNetworkInterface", "PATCH", "projects/p/zones/us-central1-a/instances/x/updateNetworkInterface"},
		{"compute.instances.updateShieldedInstanceConfig", "PATCH", "projects/p/zones/us-central1-a/instances/x/updateShieldedInstanceConfig"},
		{"compute.instances.bulkInsert", "POST", "projects/p/zones/us-central1-a/instances/bulkInsert"},
		{"compute.instances.update", "PUT", "projects/p/zones/us-central1-a/instances/x"},
		{"compute.projects.disableXpnHost", "POST", "projects/p/disableXpnHost"},
		{"compute.projects.enableXpnHost", "POST", "projects/p/enableXpnHost"},
		{"compute.projects.getXpnHost", "GET", "projects/p/getXpnHost"},
		{"compute.projects.getXpnResources", "GET", "projects/p/getXpnResources"},
		{"compute.projects.listXpnHosts", "POST", "projects/p/listXpnHosts"},
		{"compute.projects.moveDisk", "POST", "projects/p/moveDisk"},
		{"compute.projects.moveInstance", "POST", "projects/p/moveInstance"},
		{"compute.projects.setDefaultNetworkTier", "POST", "projects/p/setDefaultNetworkTier"},
		{"compute.projects.setUsageExportBucket", "POST", "projects/p/setUsageExportBucket"},
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
