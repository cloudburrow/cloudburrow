package bigqueryfront

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUnservedMethods(t *testing.T) {
	const ds = "/bigquery/v2/projects/p/datasets/d"
	const tb = ds + "/tables/t"
	for _, c := range []struct{ method, path, want string }{
		{"POST", ds + ":undelete", "bigquery.datasets.undelete"},
		{"GET", tb + "/rowAccessPolicies", ""},                       // rowpolicies.go, #1218
		{"POST", tb + "/rowAccessPolicies", ""},                      // rowpolicies.go, #1218
		{"POST", tb + "/rowAccessPolicies:batchDelete", ""},          // rowpolicies.go, #1218
		{"GET", tb + "/rowAccessPolicies/x", ""},                     // rowpolicies.go, #1218
		{"PUT", tb + "/rowAccessPolicies/x", ""},                     // rowpolicies.go, #1218
		{"DELETE", tb + "/rowAccessPolicies/x", ""},                  // rowpolicies.go, #1218
		{"POST", tb + "/rowAccessPolicies/x:getIamPolicy", ""},       // iam.go, #1231
		{"POST", tb + "/rowAccessPolicies/x:testIamPermissions", ""}, // iam.go, #1231
		{"POST", tb + ":getIamPolicy", ""},                           // iam.go, #1231
		{"POST", tb + ":setIamPolicy", ""},                           // iam.go, #1231
		{"POST", tb + ":testIamPermissions", ""},                     // iam.go, #1231
		{"POST", ds + "/routines/f:getIamPolicy", ""},                // iam.go, #1231
		{"POST", ds + "/routines/f:setIamPolicy", ""},                // iam.go, #1231
		{"POST", ds + "/routines/f:testIamPermissions", ""},          // iam.go, #1231
		// What the emulator serves passes.
		{"GET", ds, ""},
		{"PATCH", ds, ""}, // datasetupdate.go, #1205
		{"PUT", ds, ""},
		{"GET", "/bigquery/v2/projects/p/serviceAccount", ""},
		{"DELETE", ds, ""},
		{"GET", tb, ""},
		{"PATCH", tb, ""},
		{"POST", "/bigquery/v2/projects/p/datasets", ""},
		{"GET", ds + "/routines/f", ""},
		{"GET", "/bigquery/v2/projects", ""},
	} {
		r := httptest.NewRequest(c.method, c.path, nil)
		if got := unservedMethod(r); got != c.want {
			t.Errorf("%s %s = %q, want %q", c.method, c.path, got, c.want)
		}
		w := httptest.NewRecorder()
		if served := serveUnserved(w, r); served != (c.want != "") || served && w.Code != http.StatusNotImplemented {
			t.Errorf("%s %s: served %v, code %d", c.method, c.path, served, w.Code)
		}
	}
}
