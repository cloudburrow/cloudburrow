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
	// Workforce Identity Federation (#2056, workforce.go): pools, providers,
	// keys, subjects and their operations; the pools' and SCIM tenants' own
	// IAM policies (#2065, workforce_iam.go); SCIM tenants and their tokens
	// (Gemini Enterprise and Looker, #2065, workforce_scim.go).
	"iam.locations.workforcePools.create":                                   true,
	"iam.locations.workforcePools.delete":                                   true,
	"iam.locations.workforcePools.get":                                      true,
	"iam.locations.workforcePools.getIamPolicy":                             true,
	"iam.locations.workforcePools.list":                                     true,
	"iam.locations.workforcePools.operations.get":                           true,
	"iam.locations.workforcePools.patch":                                    true,
	"iam.locations.workforcePools.providers.create":                         true,
	"iam.locations.workforcePools.providers.delete":                         true,
	"iam.locations.workforcePools.providers.get":                            true,
	"iam.locations.workforcePools.providers.keys.create":                    true,
	"iam.locations.workforcePools.providers.keys.delete":                    true,
	"iam.locations.workforcePools.providers.keys.get":                       true,
	"iam.locations.workforcePools.providers.keys.list":                      true,
	"iam.locations.workforcePools.providers.keys.operations.get":            true,
	"iam.locations.workforcePools.providers.keys.undelete":                  true,
	"iam.locations.workforcePools.providers.list":                           true,
	"iam.locations.workforcePools.providers.operations.get":                 true,
	"iam.locations.workforcePools.providers.patch":                          true,
	"iam.locations.workforcePools.providers.scimTenants.create":             true,
	"iam.locations.workforcePools.providers.scimTenants.delete":             true,
	"iam.locations.workforcePools.providers.scimTenants.get":                true,
	"iam.locations.workforcePools.providers.scimTenants.getIamPolicy":       true,
	"iam.locations.workforcePools.providers.scimTenants.list":               true,
	"iam.locations.workforcePools.providers.scimTenants.patch":              true,
	"iam.locations.workforcePools.providers.scimTenants.setIamPolicy":       true,
	"iam.locations.workforcePools.providers.scimTenants.testIamPermissions": true,
	"iam.locations.workforcePools.providers.scimTenants.tokens.create":      true,
	"iam.locations.workforcePools.providers.scimTenants.tokens.delete":      true,
	"iam.locations.workforcePools.providers.scimTenants.tokens.get":         true,
	"iam.locations.workforcePools.providers.scimTenants.tokens.list":        true,
	"iam.locations.workforcePools.providers.scimTenants.tokens.patch":       true,
	"iam.locations.workforcePools.providers.scimTenants.undelete":           true,
	"iam.locations.workforcePools.providers.undelete":                       true,
	"iam.locations.workforcePools.setIamPolicy":                             true,
	"iam.locations.workforcePools.subjects.delete":                          true,
	"iam.locations.workforcePools.subjects.operations.get":                  true,
	"iam.locations.workforcePools.subjects.revokeSessions":                  true,
	"iam.locations.workforcePools.subjects.undelete":                        true,
	"iam.locations.workforcePools.testIamPermissions":                       true,
	"iam.locations.workforcePools.undelete":                                 true,
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
