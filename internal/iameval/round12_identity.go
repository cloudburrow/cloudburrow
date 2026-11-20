package iameval

import "regexp"

// The twelfth round's Workload Identity Federation checks (#1879): the IAM
// API v1's workloadIdentityPools, their providers and the providers' keys,
// served over JSON alone on Resource Manager's port.

// WorkloadIdentity is the IAM API's table for Workload Identity
// Federation: the pools keep no policy of their own here (their
// getIamPolicy and setIamPolicy are not served), so a pool's project's
// ancestry applies. A denial is the IAM API's.
var WorkloadIdentity = &Service{
	Name: "iam.googleapis.com",
	Types: map[string]string{
		"workloadIdentityPools": "iam.googleapis.com/WorkloadIdentityPool",
		"providers":             "iam.googleapis.com/WorkloadIdentityPoolProvider",
		"keys":                  "iam.googleapis.com/WorkloadIdentityPoolProviderKey",
	},
	Deny: iamDeny,
}

const (
	wifLocation = `^/v1/(projects/[^/]+/locations/[^/]+)`
	wifPool     = `^/v1/(projects/[^/]+/locations/[^/]+/workloadIdentityPools/[^/:]+)`
	wifProvider = `^/v1/(projects/[^/]+/locations/[^/]+/workloadIdentityPools/[^/:]+/providers/[^/:]+)`
	wifKey      = `^/v1/(projects/[^/]+/locations/[^/]+/workloadIdentityPools/[^/:]+/providers/[^/:]+/keys/[^/:]+)`
)

// WorkloadIdentityJSON are the served Workload Identity Federation
// methods, each with the permission its REST reference names
// (https://docs.cloud.google.com/iam/docs/reference/rest/v1/projects.locations.workloadIdentityPools
// and .providers, read 2026-11-20: iam.workloadIdentityPools.* and
// iam.workloadIdentityPoolProviders.*, a list or create on its parent, the
// others on the resource; patch is .update). The keys' pages name no
// permission; each is checked with the permission of its name in Google's
// permission index, iam.workloadIdentityPoolProviderKeys.* (Workload
// Identity Pool Admin's), on the same resources. listAttestationRules'
// page names "CALLBACK", no permission; the index's
// iam.workloadIdentityPools.getAttestationRules is checked on the pool.
// The operations' pages name none, nor does the Security Token Service's
// token exchange, which the pool's provider authorizes: those are not
// checked.
var WorkloadIdentityJSON = []JSONMethod{
	{Method: "GET", Pattern: regexp.MustCompile(wifLocation + `/workloadIdentityPools$`), Permission: "iam.workloadIdentityPools.list"},
	{Method: "POST", Pattern: regexp.MustCompile(wifLocation + `/workloadIdentityPools$`), Permission: "iam.workloadIdentityPools.create"},
	{Method: "GET", Pattern: regexp.MustCompile(wifPool + `$`), Permission: "iam.workloadIdentityPools.get"},
	{Method: "GET", Pattern: regexp.MustCompile(wifPool + `:listAttestationRules$`), Permission: "iam.workloadIdentityPools.getAttestationRules"},
	{Method: "PATCH", Pattern: regexp.MustCompile(wifPool + `$`), Permission: "iam.workloadIdentityPools.update"},
	{Method: "DELETE", Pattern: regexp.MustCompile(wifPool + `$`), Permission: "iam.workloadIdentityPools.delete"},
	{Method: "POST", Pattern: regexp.MustCompile(wifPool + `:undelete$`), Permission: "iam.workloadIdentityPools.undelete"},
	{Method: "GET", Pattern: regexp.MustCompile(wifPool + `/providers$`), Permission: "iam.workloadIdentityPoolProviders.list"},
	{Method: "POST", Pattern: regexp.MustCompile(wifPool + `/providers$`), Permission: "iam.workloadIdentityPoolProviders.create"},
	{Method: "GET", Pattern: regexp.MustCompile(wifProvider + `$`), Permission: "iam.workloadIdentityPoolProviders.get"},
	{Method: "PATCH", Pattern: regexp.MustCompile(wifProvider + `$`), Permission: "iam.workloadIdentityPoolProviders.update"},
	{Method: "DELETE", Pattern: regexp.MustCompile(wifProvider + `$`), Permission: "iam.workloadIdentityPoolProviders.delete"},
	{Method: "POST", Pattern: regexp.MustCompile(wifProvider + `:undelete$`), Permission: "iam.workloadIdentityPoolProviders.undelete"},
	{Method: "GET", Pattern: regexp.MustCompile(wifProvider + `/keys$`), Permission: "iam.workloadIdentityPoolProviderKeys.list"},
	{Method: "POST", Pattern: regexp.MustCompile(wifProvider + `/keys$`), Permission: "iam.workloadIdentityPoolProviderKeys.create"},
	{Method: "GET", Pattern: regexp.MustCompile(wifKey + `$`), Permission: "iam.workloadIdentityPoolProviderKeys.get"},
	{Method: "DELETE", Pattern: regexp.MustCompile(wifKey + `$`), Permission: "iam.workloadIdentityPoolProviderKeys.delete"},
	{Method: "POST", Pattern: regexp.MustCompile(wifKey + `:undelete$`), Permission: "iam.workloadIdentityPoolProviderKeys.undelete"},
}
