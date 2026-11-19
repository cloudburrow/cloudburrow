package compute

// Method is one method of the Compute Engine v1 discovery document's
// resources this package serves, by discovery ID, and whether it is
// implemented: the coverage page's table (tools/coverage). The rest of
// those resources' methods (peering, IAM, move, listUsable,
// getEffectiveFirewalls) answer 501, as every other Compute resource does.
type Method struct {
	ID          string
	Implemented bool
}

// Methods are the served resources' methods, from
// google.golang.org/api/compute/v1's compute-api.json.
var Methods = []Method{
	{"compute.networks.addPeering", false},
	{"compute.networks.cancelRequestRemovePeering", false},
	{"compute.networks.delete", true},
	{"compute.networks.get", true},
	{"compute.networks.getEffectiveFirewalls", false},
	{"compute.networks.insert", true},
	{"compute.networks.list", true},
	{"compute.networks.listPeeringRoutes", false},
	{"compute.networks.patch", true},
	{"compute.networks.removePeering", false},
	{"compute.networks.requestRemovePeering", false},
	{"compute.networks.switchToCustomMode", true},
	{"compute.networks.updatePeering", false},
	{"compute.subnetworks.aggregatedList", true},
	{"compute.subnetworks.delete", true},
	{"compute.subnetworks.expandIpCidrRange", true},
	{"compute.subnetworks.get", true},
	{"compute.subnetworks.getIamPolicy", false},
	{"compute.subnetworks.insert", true},
	{"compute.subnetworks.list", true},
	{"compute.subnetworks.listUsable", false},
	{"compute.subnetworks.patch", true},
	{"compute.subnetworks.setIamPolicy", false},
	{"compute.subnetworks.setPrivateIpGoogleAccess", true},
	{"compute.subnetworks.testIamPermissions", false},
	{"compute.firewalls.delete", true},
	{"compute.firewalls.get", true},
	{"compute.firewalls.insert", true},
	{"compute.firewalls.list", true},
	{"compute.firewalls.patch", true},
	{"compute.firewalls.testIamPermissions", false},
	{"compute.firewalls.update", true},
	{"compute.routes.delete", true},
	{"compute.routes.get", true},
	{"compute.routes.insert", true},
	{"compute.routes.list", true},
	{"compute.routes.testIamPermissions", false},
	{"compute.addresses.aggregatedList", true},
	{"compute.addresses.delete", true},
	{"compute.addresses.get", true},
	{"compute.addresses.insert", true},
	{"compute.addresses.list", true},
	{"compute.addresses.move", false},
	{"compute.addresses.setLabels", true},
	{"compute.addresses.testIamPermissions", false},
	{"compute.globalAddresses.delete", true},
	{"compute.globalAddresses.get", true},
	{"compute.globalAddresses.insert", true},
	{"compute.globalAddresses.list", true},
	{"compute.globalAddresses.move", false},
	{"compute.globalAddresses.setLabels", true},
	{"compute.globalAddresses.testIamPermissions", false},
	{"compute.globalOperations.aggregatedList", true},
	{"compute.globalOperations.delete", true},
	{"compute.globalOperations.get", true},
	{"compute.globalOperations.list", true},
	{"compute.globalOperations.wait", true},
	{"compute.regionOperations.delete", true},
	{"compute.regionOperations.get", true},
	{"compute.regionOperations.list", true},
	{"compute.regionOperations.wait", true},
	{"compute.zoneOperations.delete", true},
	{"compute.zoneOperations.get", true},
	{"compute.zoneOperations.list", true},
	{"compute.zoneOperations.wait", true},
	{"compute.regions.get", true},
	{"compute.regions.list", true},
	{"compute.zones.get", true},
	{"compute.zones.list", true},
}

// MethodStatuses is Methods by ID.
func MethodStatuses() map[string]bool {
	out := make(map[string]bool, len(Methods))
	for _, m := range Methods {
		out[m.ID] = m.Implemented
	}
	return out
}
