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
		{"compute.networks.cancelRequestRemovePeering", "POST", "projects/p/global/networks/x/cancelRequestRemovePeering"},
		{"compute.networks.getEffectiveFirewalls", "GET", "projects/p/global/networks/x/getEffectiveFirewalls"},
		{"compute.networks.listPeeringRoutes", "GET", "projects/p/global/networks/x/listPeeringRoutes"},
		{"compute.networks.requestRemovePeering", "POST", "projects/p/global/networks/x/requestRemovePeering"},
		{"compute.firewalls.testIamPermissions", "POST", "projects/p/global/firewalls/x/testIamPermissions"},
		{"compute.routes.testIamPermissions", "POST", "projects/p/global/routes/x/testIamPermissions"},
		{"compute.addresses.move", "POST", "projects/p/regions/us-central1/addresses/x/move"},
		{"compute.addresses.testIamPermissions", "POST", "projects/p/regions/us-central1/addresses/x/testIamPermissions"},
		{"compute.globalAddresses.move", "POST", "projects/p/global/addresses/x/move"},
		{"compute.globalAddresses.testIamPermissions", "POST", "projects/p/global/addresses/x/testIamPermissions"},
		{"compute.images.getIamPolicy", "GET", "projects/debian-cloud/global/images/x/getIamPolicy"},
		{"compute.images.patch", "PATCH", "projects/p/global/images/x"},
		{"compute.images.setIamPolicy", "POST", "projects/p/global/images/x/setIamPolicy"},
		{"compute.images.testIamPermissions", "POST", "projects/p/global/images/x/testIamPermissions"},
		{"compute.machineTypes.aggregatedList", "GET", "projects/p/aggregated/machineTypes"},
		{"compute.diskTypes.aggregatedList", "GET", "projects/p/aggregated/diskTypes"},
		{"compute.disks.setIamPolicy", "POST", "projects/p/zones/us-central1-a/disks/x/setIamPolicy"},
		{"compute.disks.startAsyncReplication", "POST", "projects/p/zones/us-central1-a/disks/x/startAsyncReplication"},
		{"compute.disks.stopAsyncReplication", "POST", "projects/p/zones/us-central1-a/disks/x/stopAsyncReplication"},
		{"compute.disks.testIamPermissions", "POST", "projects/p/zones/us-central1-a/disks/x/testIamPermissions"},
		{"compute.disks.bulkInsert", "POST", "projects/p/zones/us-central1-a/disks/bulkInsert"},
		{"compute.disks.getIamPolicy", "GET", "projects/p/zones/us-central1-a/disks/x/getIamPolicy"},
		{"compute.disks.stopGroupAsyncReplication", "POST", "projects/p/zones/us-central1-a/disks/stopGroupAsyncReplication"},
		{"compute.disks.update", "PATCH", "projects/p/zones/us-central1-a/disks/x"},
		{"compute.regionDisks.bulkInsert", "POST", "projects/p/regions/us-central1/disks/bulkInsert"},
		{"compute.regionDisks.getIamPolicy", "GET", "projects/p/regions/us-central1/disks/x/getIamPolicy"},
		{"compute.regionDisks.setIamPolicy", "POST", "projects/p/regions/us-central1/disks/x/setIamPolicy"},
		{"compute.regionDisks.startAsyncReplication", "POST", "projects/p/regions/us-central1/disks/x/startAsyncReplication"},
		{"compute.regionDisks.stopAsyncReplication", "POST", "projects/p/regions/us-central1/disks/x/stopAsyncReplication"},
		{"compute.regionDisks.stopGroupAsyncReplication", "POST", "projects/p/regions/us-central1/disks/stopGroupAsyncReplication"},
		{"compute.regionDisks.testIamPermissions", "POST", "projects/p/regions/us-central1/disks/x/testIamPermissions"},
		{"compute.regionDisks.update", "PATCH", "projects/p/regions/us-central1/disks/x"},
		{"compute.snapshots.getIamPolicy", "GET", "projects/p/global/snapshots/x/getIamPolicy"},
		{"compute.snapshots.setIamPolicy", "POST", "projects/p/global/snapshots/x/setIamPolicy"},
		{"compute.snapshots.testIamPermissions", "POST", "projects/p/global/snapshots/x/testIamPermissions"},
		{"compute.instances.getEffectiveFirewalls", "GET", "projects/p/zones/us-central1-a/instances/x/getEffectiveFirewalls"},
		{"compute.instances.getGuestAttributes", "GET", "projects/p/zones/us-central1-a/instances/x/getGuestAttributes"},
		{"compute.instances.getIamPolicy", "GET", "projects/p/zones/us-central1-a/instances/x/getIamPolicy"},
		{"compute.instances.getScreenshot", "GET", "projects/p/zones/us-central1-a/instances/x/getScreenshot"},
		{"compute.instances.getShieldedInstanceIdentity", "GET", "projects/p/zones/us-central1-a/instances/x/getShieldedInstanceIdentity"},
		{"compute.instances.performMaintenance", "POST", "projects/p/zones/us-central1-a/instances/x/performMaintenance"},
		{"compute.instances.setIamPolicy", "POST", "projects/p/zones/us-central1-a/instances/x/setIamPolicy"},
		{"compute.instances.setMinCpuPlatform", "POST", "projects/p/zones/us-central1-a/instances/x/setMinCpuPlatform"},
		{"compute.instances.setName", "POST", "projects/p/zones/us-central1-a/instances/x/setName"},
		{"compute.instances.setServiceAccount", "POST", "projects/p/zones/us-central1-a/instances/x/setServiceAccount"},
		{"compute.instances.setShieldedInstanceIntegrityPolicy", "PATCH", "projects/p/zones/us-central1-a/instances/x/setShieldedInstanceIntegrityPolicy"},
		{"compute.instances.simulateMaintenanceEvent", "POST", "projects/p/zones/us-central1-a/instances/x/simulateMaintenanceEvent"},
		{"compute.instances.startWithEncryptionKey", "POST", "projects/p/zones/us-central1-a/instances/x/startWithEncryptionKey"},
		{"compute.instances.testIamPermissions", "POST", "projects/p/zones/us-central1-a/instances/x/testIamPermissions"},
		{"compute.instances.updateAccessConfig", "POST", "projects/p/zones/us-central1-a/instances/x/updateAccessConfig"},
		{"compute.instances.updateDisplayDevice", "PATCH", "projects/p/zones/us-central1-a/instances/x/updateDisplayDevice"},
		{"compute.instances.updateNetworkInterface", "PATCH", "projects/p/zones/us-central1-a/instances/x/updateNetworkInterface"},
		{"compute.instances.updateShieldedInstanceConfig", "PATCH", "projects/p/zones/us-central1-a/instances/x/updateShieldedInstanceConfig"},
		{"compute.instances.bulkInsert", "POST", "projects/p/zones/us-central1-a/instances/bulkInsert"},
		{"compute.instances.update", "PUT", "projects/p/zones/us-central1-a/instances/x"},
		{"compute.resourcePolicies.getIamPolicy", "GET", "projects/p/regions/us-central1/resourcePolicies/x/getIamPolicy"},
		{"compute.resourcePolicies.patch", "PATCH", "projects/p/regions/us-central1/resourcePolicies/x"},
		{"compute.resourcePolicies.setIamPolicy", "POST", "projects/p/regions/us-central1/resourcePolicies/x/setIamPolicy"},
		{"compute.resourcePolicies.testIamPermissions", "POST", "projects/p/regions/us-central1/resourcePolicies/x/testIamPermissions"},
		{"compute.instanceTemplates.getIamPolicy", "GET", "projects/p/global/instanceTemplates/x/getIamPolicy"},
		{"compute.instanceTemplates.setIamPolicy", "POST", "projects/p/global/instanceTemplates/x/setIamPolicy"},
		{"compute.instanceTemplates.testIamPermissions", "POST", "projects/p/global/instanceTemplates/x/testIamPermissions"},
		{"compute.machineImages.getIamPolicy", "GET", "projects/p/global/machineImages/x/getIamPolicy"},
		{"compute.machineImages.setIamPolicy", "POST", "projects/p/global/machineImages/x/setIamPolicy"},
		{"compute.machineImages.testIamPermissions", "POST", "projects/p/global/machineImages/x/testIamPermissions"},
		{"compute.instanceGroups.testIamPermissions", "POST", "projects/p/zones/us-central1-a/instanceGroups/x/testIamPermissions"},
		{"compute.instanceGroupManagers.abandonInstances", "POST", "projects/p/zones/us-central1-a/instanceGroupManagers/x/abandonInstances"},
		{"compute.instanceGroupManagers.createInstances", "POST", "projects/p/zones/us-central1-a/instanceGroupManagers/x/createInstances"},
		{"compute.instanceGroupManagers.deletePerInstanceConfigs", "POST", "projects/p/zones/us-central1-a/instanceGroupManagers/x/deletePerInstanceConfigs"},
		{"compute.instanceGroupManagers.listErrors", "GET", "projects/p/zones/us-central1-a/instanceGroupManagers/x/listErrors"},
		{"compute.instanceGroupManagers.listPerInstanceConfigs", "POST", "projects/p/zones/us-central1-a/instanceGroupManagers/x/listPerInstanceConfigs"},
		{"compute.instanceGroupManagers.patchPerInstanceConfigs", "POST", "projects/p/zones/us-central1-a/instanceGroupManagers/x/patchPerInstanceConfigs"},
		{"compute.instanceGroupManagers.setTargetPools", "POST", "projects/p/zones/us-central1-a/instanceGroupManagers/x/setTargetPools"},
		{"compute.instanceGroupManagers.updatePerInstanceConfigs", "POST", "projects/p/zones/us-central1-a/instanceGroupManagers/x/updatePerInstanceConfigs"},
		{"compute.regionInstanceGroupManagers.abandonInstances", "POST", "projects/p/regions/us-central1/instanceGroupManagers/x/abandonInstances"},
		{"compute.regionInstanceGroupManagers.createInstances", "POST", "projects/p/regions/us-central1/instanceGroupManagers/x/createInstances"},
		{"compute.regionInstanceGroupManagers.deletePerInstanceConfigs", "POST", "projects/p/regions/us-central1/instanceGroupManagers/x/deletePerInstanceConfigs"},
		{"compute.regionInstanceGroupManagers.listErrors", "GET", "projects/p/regions/us-central1/instanceGroupManagers/x/listErrors"},
		{"compute.regionInstanceGroupManagers.listPerInstanceConfigs", "POST", "projects/p/regions/us-central1/instanceGroupManagers/x/listPerInstanceConfigs"},
		{"compute.regionInstanceGroupManagers.patchPerInstanceConfigs", "POST", "projects/p/regions/us-central1/instanceGroupManagers/x/patchPerInstanceConfigs"},
		{"compute.regionInstanceGroupManagers.setTargetPools", "POST", "projects/p/regions/us-central1/instanceGroupManagers/x/setTargetPools"},
		{"compute.regionInstanceGroupManagers.updatePerInstanceConfigs", "POST", "projects/p/regions/us-central1/instanceGroupManagers/x/updatePerInstanceConfigs"},
		{"compute.regionInstanceGroups.testIamPermissions", "POST", "projects/p/regions/us-central1/instanceGroups/x/testIamPermissions"},
		{"compute.autoscalers.testIamPermissions", "POST", "projects/p/zones/us-central1-a/autoscalers/x/testIamPermissions"},
		{"compute.regionAutoscalers.testIamPermissions", "POST", "projects/p/regions/us-central1/autoscalers/x/testIamPermissions"},
		{"compute.routers.deleteNamedSet", "POST", "projects/p/regions/us-central1/routers/x/deleteNamedSet"},
		{"compute.routers.deleteRoutePolicy", "POST", "projects/p/regions/us-central1/routers/x/deleteRoutePolicy"},
		{"compute.routers.getNamedSet", "GET", "projects/p/regions/us-central1/routers/x/getNamedSet"},
		{"compute.routers.getRoutePolicy", "GET", "projects/p/regions/us-central1/routers/x/getRoutePolicy"},
		{"compute.routers.listBgpRoutes", "GET", "projects/p/regions/us-central1/routers/x/listBgpRoutes"},
		{"compute.routers.listNamedSets", "GET", "projects/p/regions/us-central1/routers/x/listNamedSets"},
		{"compute.routers.listRoutePolicies", "GET", "projects/p/regions/us-central1/routers/x/listRoutePolicies"},
		{"compute.routers.patchNamedSet", "POST", "projects/p/regions/us-central1/routers/x/patchNamedSet"},
		{"compute.routers.patchRoutePolicy", "POST", "projects/p/regions/us-central1/routers/x/patchRoutePolicy"},
		{"compute.routers.updateNamedSet", "POST", "projects/p/regions/us-central1/routers/x/updateNamedSet"},
		{"compute.routers.updateRoutePolicy", "POST", "projects/p/regions/us-central1/routers/x/updateRoutePolicy"},
		{"compute.projects.moveDisk", "POST", "projects/p/moveDisk"},
		{"compute.globalForwardingRules.patch", "PATCH", "projects/p/global/forwardingRules/x"},
		{"compute.globalForwardingRules.setTarget", "POST", "projects/p/global/forwardingRules/x/setTarget"},
		{"compute.forwardingRules.patch", "PATCH", "projects/p/regions/us-central1/forwardingRules/x"},
		{"compute.forwardingRules.setTarget", "POST", "projects/p/regions/us-central1/forwardingRules/x/setTarget"},
		{"compute.serviceAttachments.getIamPolicy", "GET", "projects/p/regions/us-central1/serviceAttachments/x/getIamPolicy"},
		{"compute.serviceAttachments.setIamPolicy", "POST", "projects/p/regions/us-central1/serviceAttachments/x/setIamPolicy"},
		{"compute.serviceAttachments.testIamPermissions", "POST", "projects/p/regions/us-central1/serviceAttachments/x/testIamPermissions"},
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
