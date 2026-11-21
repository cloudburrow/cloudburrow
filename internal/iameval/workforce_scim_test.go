package iameval

import (
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/iamroles"
)

// The SCIM endpoint's permissions (#2068) are Google's, and the SCIM Data
// Syncer and Workforce Pool Admin roles grant each.
func TestScimProtocolPermissions(t *testing.T) {
	for _, role := range []string{"roles/iam.scimSyncer", "roles/iam.workforcePoolAdmin"} {
		r := iamroles.Predefined(role)
		if r == nil {
			t.Fatalf("%s is not in the role catalog", role)
		}
		for method, p := range ScimProtocolPermissions {
			// The catalog writes the new-style iam.googleapis.com/ form.
			if !iamroles.Grants(r.Permissions, p) && !iamroles.Grants(r.Permissions, "iam.googleapis.com/"+strings.TrimPrefix(p, "iam.")) {
				t.Errorf("%s (%s) is not granted by %s", p, method, role)
			}
		}
	}
}
