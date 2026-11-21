package iameval

import "testing"

// TestRound12TagResourceCheck (#1879): a tag binding on another service's
// resource needs that service's SERVICE.COLLECTION.VERB on the resource,
// in that service's table; Resource Manager's own resources and
// permissions Google's index lacks need none.
func TestRound12TagResourceCheck(t *testing.T) {
	for _, tc := range []struct{ full, verb, permission, resource, host string }{
		{"//storage.googleapis.com/projects/_/buckets/b", "createTagBinding", "storage.buckets.createTagBinding", "projects/_/buckets/b", "storage.googleapis.com"},
		{"//compute.googleapis.com/projects/p/zones/z/instances/i", "deleteTagBinding", "compute.instances.deleteTagBinding", "projects/p/zones/z/instances/i", "compute.googleapis.com"},
		{"//secretmanager.googleapis.com/projects/p/secrets/s", "listTagBindings", "secretmanager.secrets.listTagBindings", "projects/p/secrets/s", "secretmanager.googleapis.com"},
		{"//sqladmin.googleapis.com/projects/p/instances/i", "createTagBinding", "cloudsql.instances.createTagBinding", "projects/p/instances/i", "sqladmin.googleapis.com"},
	} {
		c, ok := TagResourceCheck(tc.full, tc.verb)
		if !ok || c.Permission != tc.permission || c.Resource != tc.resource || c.Service == nil || c.Service.Name != tc.host {
			t.Errorf("TagResourceCheck(%s) = %+v, %v", tc.full, c, ok)
		}
	}
	if c, ok := TagResourceCheck("//secretmanager.googleapis.com/projects/p/secrets/s", "createTagBinding"); !ok || c.Service != SecretManager {
		t.Errorf("a secret's binding is not checked in Secret Manager's table: %+v", c)
	}
	for _, full := range []string{"//cloudresourcemanager.googleapis.com/projects/1", "//example.googleapis.com/projects/p/widgets/w", "projects/p"} {
		if c, ok := TagResourceCheck(full, "createTagBinding"); ok {
			t.Errorf("TagResourceCheck(%s) = %+v", full, c)
		}
	}
	if r := tagBindingResource("tagBindings/%2F%2Fstorage.googleapis.com%2Fprojects%2F_%2Fbuckets%2Fb/tagValues/1"); r != "//storage.googleapis.com/projects/_/buckets/b" {
		t.Errorf("tagBindingResource = %q", r)
	}
}

// TestRound12V2RolePermissions: a predefined role listing a permission by
// its v2 name alone grants the v1 permission (Workload Identity Pool
// Viewer's iam.googleapis.com/workloadIdentityPools.get).
func TestRound12V2RolePermissions(t *testing.T) {
	if !predefinedGrants("roles/iam.workloadIdentityPoolViewer", "iam.workloadIdentityPools.get") {
		t.Error("Workload Identity Pool Viewer does not grant iam.workloadIdentityPools.get")
	}
	if predefinedGrants("roles/iam.workloadIdentityPoolViewer", "iam.workloadIdentityPools.update") {
		t.Error("Workload Identity Pool Viewer grants iam.workloadIdentityPools.update")
	}
}
