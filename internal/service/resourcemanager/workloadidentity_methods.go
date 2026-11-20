package resourcemanager

// WorkloadIdentityMethods is every method of the IAM v1 discovery document
// under projects.locations.workloadIdentityPools (google.golang.org/api/iam/v1),
// and the Security Token Service's token method, true for the ones served
// (#1941): pools, providers, keys and their operations here, sts.token by
// internal/metadata. Managed workload identity (namespaces and attestation
// rules) and the pools' own IAM policies answer 501;
// listAttestationRules answers an empty list, as a federation pool has none.
var WorkloadIdentityMethods = map[string]bool{
	"iam.projects.locations.workloadIdentityPools.addAttestationRule":                                          false,
	"iam.projects.locations.workloadIdentityPools.create":                                                      true,
	"iam.projects.locations.workloadIdentityPools.delete":                                                      true,
	"iam.projects.locations.workloadIdentityPools.get":                                                         true,
	"iam.projects.locations.workloadIdentityPools.getIamPolicy":                                                false,
	"iam.projects.locations.workloadIdentityPools.list":                                                        true,
	"iam.projects.locations.workloadIdentityPools.listAttestationRules":                                        true,
	"iam.projects.locations.workloadIdentityPools.namespaces.create":                                           false,
	"iam.projects.locations.workloadIdentityPools.namespaces.delete":                                           false,
	"iam.projects.locations.workloadIdentityPools.namespaces.get":                                              false,
	"iam.projects.locations.workloadIdentityPools.namespaces.list":                                             false,
	"iam.projects.locations.workloadIdentityPools.namespaces.managedIdentities.addAttestationRule":             false,
	"iam.projects.locations.workloadIdentityPools.namespaces.managedIdentities.create":                         false,
	"iam.projects.locations.workloadIdentityPools.namespaces.managedIdentities.delete":                         false,
	"iam.projects.locations.workloadIdentityPools.namespaces.managedIdentities.get":                            false,
	"iam.projects.locations.workloadIdentityPools.namespaces.managedIdentities.list":                           false,
	"iam.projects.locations.workloadIdentityPools.namespaces.managedIdentities.listAttestationRules":           false,
	"iam.projects.locations.workloadIdentityPools.namespaces.managedIdentities.operations.get":                 false,
	"iam.projects.locations.workloadIdentityPools.namespaces.managedIdentities.patch":                          false,
	"iam.projects.locations.workloadIdentityPools.namespaces.managedIdentities.removeAttestationRule":          false,
	"iam.projects.locations.workloadIdentityPools.namespaces.managedIdentities.setAttestationRules":            false,
	"iam.projects.locations.workloadIdentityPools.namespaces.managedIdentities.undelete":                       false,
	"iam.projects.locations.workloadIdentityPools.namespaces.managedIdentities.workloadSources.operations.get": false,
	"iam.projects.locations.workloadIdentityPools.namespaces.operations.get":                                   false,
	"iam.projects.locations.workloadIdentityPools.namespaces.patch":                                            false,
	"iam.projects.locations.workloadIdentityPools.namespaces.undelete":                                         false,
	"iam.projects.locations.workloadIdentityPools.operations.get":                                              true,
	"iam.projects.locations.workloadIdentityPools.patch":                                                       true,
	"iam.projects.locations.workloadIdentityPools.providers.create":                                            true,
	"iam.projects.locations.workloadIdentityPools.providers.delete":                                            true,
	"iam.projects.locations.workloadIdentityPools.providers.get":                                               true,
	"iam.projects.locations.workloadIdentityPools.providers.keys.create":                                       true,
	"iam.projects.locations.workloadIdentityPools.providers.keys.delete":                                       true,
	"iam.projects.locations.workloadIdentityPools.providers.keys.get":                                          true,
	"iam.projects.locations.workloadIdentityPools.providers.keys.list":                                         true,
	"iam.projects.locations.workloadIdentityPools.providers.keys.operations.get":                               true,
	"iam.projects.locations.workloadIdentityPools.providers.keys.undelete":                                     true,
	"iam.projects.locations.workloadIdentityPools.providers.list":                                              true,
	"iam.projects.locations.workloadIdentityPools.providers.operations.get":                                    true,
	"iam.projects.locations.workloadIdentityPools.providers.patch":                                             true,
	"iam.projects.locations.workloadIdentityPools.providers.undelete":                                          true,
	"iam.projects.locations.workloadIdentityPools.removeAttestationRule":                                       false,
	"iam.projects.locations.workloadIdentityPools.setAttestationRules":                                         false,
	"iam.projects.locations.workloadIdentityPools.setIamPolicy":                                                false,
	"iam.projects.locations.workloadIdentityPools.testIamPermissions":                                          false,
	"iam.projects.locations.workloadIdentityPools.undelete":                                                    true,
	"sts.token": true,
}

// WorkloadIdentityMethodStatuses is WorkloadIdentityMethods, for the coverage
// tool.
func WorkloadIdentityMethodStatuses() map[string]bool {
	out := make(map[string]bool, len(WorkloadIdentityMethods))
	for k, v := range WorkloadIdentityMethods {
		out[k] = v
	}
	return out
}
