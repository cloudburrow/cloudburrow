package iameval

import "regexp"

// SCIM tenants and tokens of a workforce pool provider (#2065), checked
// on the tenant (or, for a list or create, its provider) with the pool's
// organization above, as the other workforce methods are.
//
// UNVERIFIED (re-read 2026-11-20 for #2068, first searched 2026-11-20):
// neither the scimTenants and scimTenants.tokens REST pages (which name
// OAuth scopes only), nor "Configure SCIM in OIDC or SAML IdP", "Configure
// SCIM in Okta", "SCIM provisioning for Workforce Identity Federation", the
// SCIM audit logging page, nor the IAM roles and permissions index
// (https://docs.cloud.google.com/iam/docs/roles-permissions/iam, including
// the full permission lists of roles/iam.workforcePoolAdmin and
// roles/iam.workforcePoolViewer) name these methods' permissions, and the
// index lists no iam.workforcePoolProviderScimTenants.* or
// .workforcePoolProviderScimTokens.* permission at all. The names below
// follow the API's pattern for the provider's other children
// (iam.workforcePoolProviderKeys.*).
//
// Verified 2026-11-20: the SCIM protocol's own methods (#2068,
// ScimProtocolPermissions) are named by "SCIM audit logging"
// (https://docs.cloud.google.com/iam/docs/audit-logging/audit-logging-iamscim)
// and the roles index: iam.workforcePoolProviderScimUsers.{create,delete,
// get,list,patch,put} and .workforcePoolProviderScimGroups.{create,delete,
// get,list,patch,put}, granted by roles/iam.scimSyncer (SCIM Data Syncer),
// roles/iam.workforcePoolAdmin and roles/iam.admin. "Configure SCIM in
// Okta" grants roles/iam.scimSyncer on the organization to the tenant's
// service agent; the endpoint authenticates the tenant's SCIM tokens and
// acts as that service agent.
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

// ScimProtocolPermissions are the SCIM endpoint's methods (Google's
// google.cloud.iamscim.v1alpha1 RPCs) and their permissions, keyed by
// "METHOD Resource" with Resource Users or Groups and an ID present for
// one resource. Schemas and ServiceProviderConfig need none.
var ScimProtocolPermissions = map[string]string{
	"POST Users":   "iam.workforcePoolProviderScimUsers.create",
	"GET Users":    "iam.workforcePoolProviderScimUsers.list",
	"GET User":     "iam.workforcePoolProviderScimUsers.get",
	"PUT User":     "iam.workforcePoolProviderScimUsers.put",
	"PATCH User":   "iam.workforcePoolProviderScimUsers.patch",
	"DELETE User":  "iam.workforcePoolProviderScimUsers.delete",
	"POST Groups":  "iam.workforcePoolProviderScimGroups.create",
	"GET Groups":   "iam.workforcePoolProviderScimGroups.list",
	"GET Group":    "iam.workforcePoolProviderScimGroups.get",
	"PATCH Group":  "iam.workforcePoolProviderScimGroups.patch",
	"DELETE Group": "iam.workforcePoolProviderScimGroups.delete",
}
