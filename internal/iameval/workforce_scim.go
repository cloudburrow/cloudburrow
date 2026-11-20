package iameval

import "regexp"

// SCIM tenants and tokens of a workforce pool provider (#2065), checked
// on the tenant (or, for a list or create, its provider) with the pool's
// organization above, as the other workforce methods are.
//
// UNVERIFIED (searched 2026-11-20): neither the scimTenants and
// scimTenants.tokens REST pages (which name OAuth scopes only), nor
// "Configure SCIM in OIDC or SAML IdP", "Configure SCIM in Okta" or the
// IAM roles and permissions index
// (https://docs.cloud.google.com/iam/docs/roles-permissions/iam) name these
// methods' permissions; the index names only the SCIM protocol's
// iam.googleapis.com/workforcePoolProviderScimUsers.* and
// .workforcePoolProviderScimGroups.*. The names below follow the API's
// pattern for the provider's other children (iam.workforcePoolProviderKeys.*).
const (
	wfScimTenant = `^/v1/(locations/[^/]+/workforcePools/[^/:]+/providers/[^/:]+/scimTenants/[^/:]+)`
	wfScimToken  = `^/v1/(locations/[^/]+/workforcePools/[^/:]+/providers/[^/:]+/scimTenants/[^/:]+/tokens/[^/:]+)`
)

// WorkforceScimTestIAM is a SCIM tenant's testIamPermissions path; its
// first group is the tenant.
var WorkforceScimTestIAM = regexp.MustCompile(wfScimTenant + `:testIamPermissions$`)

// WorkforceScimJSON are the served SCIM tenant and token methods.
var WorkforceScimJSON = []JSONMethod{
	{Method: "GET", Pattern: regexp.MustCompile(wfProvider + `/scimTenants$`), Permission: "iam.workforcePoolProviderScimTenants.list"},
	{Method: "POST", Pattern: regexp.MustCompile(wfProvider + `/scimTenants$`), Permission: "iam.workforcePoolProviderScimTenants.create"},
	{Method: "GET", Pattern: regexp.MustCompile(wfScimTenant + `$`), Permission: "iam.workforcePoolProviderScimTenants.get"},
	{Method: "PATCH", Pattern: regexp.MustCompile(wfScimTenant + `$`), Permission: "iam.workforcePoolProviderScimTenants.update"},
	{Method: "DELETE", Pattern: regexp.MustCompile(wfScimTenant + `$`), Permission: "iam.workforcePoolProviderScimTenants.delete"},
	{Method: "POST", Pattern: regexp.MustCompile(wfScimTenant + `:undelete$`), Permission: "iam.workforcePoolProviderScimTenants.undelete"},
	{Method: "POST", Pattern: regexp.MustCompile(wfScimTenant + `:getIamPolicy$`), Permission: "iam.workforcePoolProviderScimTenants.getIamPolicy"},
	{Method: "POST", Pattern: regexp.MustCompile(wfScimTenant + `:setIamPolicy$`), Permission: "iam.workforcePoolProviderScimTenants.setIamPolicy"},
	{Method: "GET", Pattern: regexp.MustCompile(wfScimTenant + `/tokens$`), Permission: "iam.workforcePoolProviderScimTokens.list"},
	{Method: "POST", Pattern: regexp.MustCompile(wfScimTenant + `/tokens$`), Permission: "iam.workforcePoolProviderScimTokens.create"},
	{Method: "GET", Pattern: regexp.MustCompile(wfScimToken + `$`), Permission: "iam.workforcePoolProviderScimTokens.get"},
	{Method: "PATCH", Pattern: regexp.MustCompile(wfScimToken + `$`), Permission: "iam.workforcePoolProviderScimTokens.update"},
	{Method: "DELETE", Pattern: regexp.MustCompile(wfScimToken + `$`), Permission: "iam.workforcePoolProviderScimTokens.delete"},
}
