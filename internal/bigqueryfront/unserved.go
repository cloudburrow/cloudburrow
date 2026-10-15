package bigqueryfront

import (
	"net/http"
	"regexp"
)

// The REST methods the emulator behind CloudBurrow does not carry out, which
// the front answers 501 itself (#1200). datasets.patch, datasets.update
// (which the emulator answered without saving) and projects.getServiceAccount
// (which it answered empty) are carried out by the front since #1205
// (datasetupdate.go). Measured against the pinned
// emulator through the official generated client, each answered something
// that was not a refusal:
//
//   - datasets.undelete answered 400 "Table ID is required": the emulator
//     deletes a dataset's tables and their rows with it and keeps nothing
//     to restore within the time travel window;
//   - every rowAccessPolicies method (all but the two IAM ones are the
//     front's since #1218, rowpolicies.go), and tables.getIamPolicy,
//     tables.setIamPolicy and tables.testIamPermissions, answered 500
//     ("unexpected request path", "unsupported bigquery.…"), which the Go
//     client retries;
//   - routines.getIamPolicy, routines.setIamPolicy and
//     routines.testIamPermissions answered 405 "method not allowed".
var (
	serviceAccountRoute = regexp.MustCompile(`^(?:/bigquery/v2)?/projects/([^/]+)/serviceAccount$`)
	datasetRoute        = regexp.MustCompile(`^(?:/bigquery/v2)?/projects/[^/]+/datasets/[^/:]+(:undelete)?$`)
	rowPoliciesRoute    = regexp.MustCompile(`^(?:/bigquery/v2)?/projects/[^/]+/datasets/[^/]+/tables/[^/]+/rowAccessPolicies(?:(:batchDelete)|/([^/:]+)(?::(getIamPolicy|testIamPermissions))?)?$`)
	iamRoute            = regexp.MustCompile(`^(?:/bigquery/v2)?/projects/[^/]+/datasets/[^/]+/(tables|routines)/[^/:]+:(getIamPolicy|setIamPolicy|testIamPermissions)$`)
)

// unservedMethod is the discovery ID of the method r calls when it is one
// the front refuses, such as bigquery.tables.getIamPolicy, and "" otherwise.
func unservedMethod(r *http.Request) string {
	p := r.URL.Path
	switch {
	case iamRoute.MatchString(p) && r.Method == http.MethodPost:
		m := iamRoute.FindStringSubmatch(p)
		return "bigquery." + m[1] + "." + m[2]
	}
	if m := datasetRoute.FindStringSubmatch(p); m != nil {
		if m[1] != "" && r.Method == http.MethodPost {
			return "bigquery.datasets.undelete"
		}
		return ""
	}
	// The other rowAccessPolicies methods are the front's since #1218
	// (rowpolicies.go).
	if m := rowPoliciesRoute.FindStringSubmatch(p); m != nil && m[3] != "" && r.Method == http.MethodPost {
		return "bigquery.rowAccessPolicies." + m[3]
	}
	return ""
}

// serveUnserved answers r 501 when it calls a method unservedMethod names,
// and reports whether it did.
func serveUnserved(w http.ResponseWriter, r *http.Request) bool {
	method := unservedMethod(r)
	if method == "" {
		return false
	}
	why := map[string]string{
		"bigquery.datasets.undelete": "the emulator deletes a dataset's tables and rows with it and keeps no deleted datasets to restore.",
	}[method]
	if why == "" {
		why = "CloudBurrow stores no IAM policies on BigQuery tables, routines or row access policies: ADR-0006 " +
			"(docs/adr/0006-iam-policy-surface.md) stores policies, never enforced, on Secret Manager, Cloud Tasks, " +
			"Cloud KMS and Cloud Storage buckets only, and every other service keeps refusing them (#1218)."
	}
	writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: "+method+". "+why)
	return true
}
