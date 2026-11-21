package iameval

import (
	"slices"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/iamroles"
)

// TestRound12IdentityTables (#1879): every Workload Identity Federation
// permission is in Google's index, and each served path is bound to its
// method's permission and resource.
func TestRound12IdentityTables(t *testing.T) {
	names := iamroles.Names()
	for _, m := range WorkloadIdentityJSON {
		if _, ok := slices.BinarySearch(names, m.Permission); !ok {
			t.Errorf("%s is not in Google's permission index", m.Permission)
		}
	}
	const loc = "projects/p/locations/global"
	const pool = loc + "/workloadIdentityPools/pool"
	const prov = pool + "/providers/prov"
	const key = prov + "/keys/k"
	for _, c := range []struct{ method, path, perm, res string }{
		{"GET", "/v1/" + loc + "/workloadIdentityPools", "iam.workloadIdentityPools.list", loc},
		{"POST", "/v1/" + loc + "/workloadIdentityPools", "iam.workloadIdentityPools.create", loc},
		{"GET", "/v1/" + pool, "iam.workloadIdentityPools.get", pool},
		{"GET", "/v1/" + pool + ":listAttestationRules", "iam.workloadIdentityPools.getAttestationRules", pool},
		{"PATCH", "/v1/" + pool, "iam.workloadIdentityPools.update", pool},
		{"DELETE", "/v1/" + pool, "iam.workloadIdentityPools.delete", pool},
		{"POST", "/v1/" + pool + ":undelete", "iam.workloadIdentityPools.undelete", pool},
		{"GET", "/v1/" + pool + "/providers", "iam.workloadIdentityPoolProviders.list", pool},
		{"POST", "/v1/" + pool + "/providers", "iam.workloadIdentityPoolProviders.create", pool},
		{"GET", "/v1/" + prov, "iam.workloadIdentityPoolProviders.get", prov},
		{"PATCH", "/v1/" + prov, "iam.workloadIdentityPoolProviders.update", prov},
		{"DELETE", "/v1/" + prov, "iam.workloadIdentityPoolProviders.delete", prov},
		{"POST", "/v1/" + prov + ":undelete", "iam.workloadIdentityPoolProviders.undelete", prov},
		{"GET", "/v1/" + prov + "/keys", "iam.workloadIdentityPoolProviderKeys.list", prov},
		{"POST", "/v1/" + prov + "/keys", "iam.workloadIdentityPoolProviderKeys.create", prov},
		{"GET", "/v1/" + key, "iam.workloadIdentityPoolProviderKeys.get", key},
		{"DELETE", "/v1/" + key, "iam.workloadIdentityPoolProviderKeys.delete", key},
		{"POST", "/v1/" + key + ":undelete", "iam.workloadIdentityPoolProviderKeys.undelete", key},
	} {
		perm, res, ok := MatchJSON(WorkloadIdentityJSON, c.method, c.path)
		if !ok || perm != c.perm || res != c.res {
			t.Errorf("%s %s = %q on %q (%v); want %q on %q", c.method, c.path, perm, res, ok, c.perm, c.res)
		}
	}
	// Operations name no permission, and other APIs' paths are not bound.
	for _, p := range []string{"/v1/" + pool + "/operations/op", "/v1/" + prov + "/operations/op",
		"/v1/projects/p/serviceAccounts", "/v1/projects/p"} {
		if perm, _, ok := MatchJSON(WorkloadIdentityJSON, "GET", p); ok {
			t.Errorf("GET %s is bound to %s", p, perm)
		}
	}
}
