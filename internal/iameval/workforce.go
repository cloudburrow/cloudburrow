package iameval

import "regexp"

// Workforce Identity Federation's checks (#2056): the IAM API v1's
// locations.workforcePools, their providers, keys and subjects, served
// over JSON on Resource Manager's port. A pool belongs to an organization
// its name does not show, so its methods are checked on the pool with the
// organization's ancestry (the scope cmd/cloudburrow gives
// iam.googleapis.com); list and create are checked on the organization the
// request names (its parent). Permissions are Google's
// (https://docs.cloud.google.com/iam/docs/reference/rest/v1/locations.workforcePools,
// .providers, .providers.keys and .subjects, and the permissions index of
// roles/iam.workforcePoolAdmin, read 2026-11-20).

// WorkforceIdentity is the IAM API's table for Workforce Identity
// Federation; a denial is the IAM API's.
var WorkforceIdentity = &Service{
	Name: "iam.googleapis.com",
	Types: map[string]string{
		"workforcePools": "iam.googleapis.com/WorkforcePool",
		"providers":      "iam.googleapis.com/WorkforcePoolProvider",
		"keys":           "iam.googleapis.com/WorkforcePoolProviderKey",
		"subjects":       "iam.googleapis.com/WorkforcePoolSubject",
	},
	Deny: iamDeny,
}

const (
	wfPool     = `^/v1/(locations/[^/]+/workforcePools/[^/:]+)`
	wfProvider = `^/v1/(locations/[^/]+/workforcePools/[^/:]+/providers/[^/:]+)`
	wfKey      = `^/v1/(locations/[^/]+/workforcePools/[^/:]+/providers/[^/:]+/keys/[^/:]+)`
	wfSubject  = `^/v1/(locations/[^/]+/workforcePools/[^/:]+/subjects/[^/:]+)`
)

// WorkforcePoolsCollection is the pools collection's path: its list's
// resource is the parent query parameter, its create's the body's parent.
var WorkforcePoolsCollection = regexp.MustCompile(`^/v1/locations/[^/]+/workforcePools$`)

// WorkforceIdentityJSON are the served workforce methods below a pool.
var WorkforceIdentityJSON = []JSONMethod{
	{Method: "GET", Pattern: regexp.MustCompile(wfPool + `$`), Permission: "iam.workforcePools.get"},
	{Method: "PATCH", Pattern: regexp.MustCompile(wfPool + `$`), Permission: "iam.workforcePools.update"},
	{Method: "DELETE", Pattern: regexp.MustCompile(wfPool + `$`), Permission: "iam.workforcePools.delete"},
	{Method: "POST", Pattern: regexp.MustCompile(wfPool + `:undelete$`), Permission: "iam.workforcePools.undelete"},
	{Method: "GET", Pattern: regexp.MustCompile(wfPool + `/providers$`), Permission: "iam.workforcePoolProviders.list"},
	{Method: "POST", Pattern: regexp.MustCompile(wfPool + `/providers$`), Permission: "iam.workforcePoolProviders.create"},
	{Method: "GET", Pattern: regexp.MustCompile(wfProvider + `$`), Permission: "iam.workforcePoolProviders.get"},
	{Method: "PATCH", Pattern: regexp.MustCompile(wfProvider + `$`), Permission: "iam.workforcePoolProviders.update"},
	{Method: "DELETE", Pattern: regexp.MustCompile(wfProvider + `$`), Permission: "iam.workforcePoolProviders.delete"},
	{Method: "POST", Pattern: regexp.MustCompile(wfProvider + `:undelete$`), Permission: "iam.workforcePoolProviders.undelete"},
	{Method: "GET", Pattern: regexp.MustCompile(wfProvider + `/keys$`), Permission: "iam.workforcePoolProviderKeys.list"},
	{Method: "POST", Pattern: regexp.MustCompile(wfProvider + `/keys$`), Permission: "iam.workforcePoolProviderKeys.create"},
	{Method: "GET", Pattern: regexp.MustCompile(wfKey + `$`), Permission: "iam.workforcePoolProviderKeys.get"},
	{Method: "DELETE", Pattern: regexp.MustCompile(wfKey + `$`), Permission: "iam.workforcePoolProviderKeys.delete"},
	{Method: "POST", Pattern: regexp.MustCompile(wfKey + `:undelete$`), Permission: "iam.workforcePoolProviderKeys.undelete"},
	{Method: "DELETE", Pattern: regexp.MustCompile(wfSubject + `$`), Permission: "iam.workforcePoolSubjects.delete"},
	{Method: "POST", Pattern: regexp.MustCompile(wfSubject + `:undelete$`), Permission: "iam.workforcePoolSubjects.undelete"},
	{Method: "POST", Pattern: regexp.MustCompile(wfSubject + `:revokeSessions$`), Permission: "iam.workforcePoolSubjects.revokeSessions"},
}
