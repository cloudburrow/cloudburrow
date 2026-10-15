package bigqueryfront

import (
	"net/http"
	"regexp"
)

// The REST methods the emulator behind CloudBurrow does not carry out, which
// the front answers 501 itself (#1200). Measured against the pinned
// emulator through the official generated client, each answered something
// that was not a refusal:
//
//   - datasets.patch and datasets.update answered 200 with the dataset as
//     sent, and kept nothing: a datasets.get afterwards read the dataset as
//     it was before;
//   - datasets.undelete answered 400 "Table ID is required";
//   - projects.getServiceAccount answered 200 with an empty body, no email;
//   - every rowAccessPolicies method, and tables.getIamPolicy,
//     tables.setIamPolicy and tables.testIamPermissions, answered 500
//     ("unexpected request path", "unsupported bigquery.…"), which the Go
//     client retries;
//   - routines.getIamPolicy, routines.setIamPolicy and
//     routines.testIamPermissions answered 405 "method not allowed".
var (
	serviceAccountRoute = regexp.MustCompile(`^(?:/bigquery/v2)?/projects/[^/]+/serviceAccount$`)
	datasetRoute        = regexp.MustCompile(`^(?:/bigquery/v2)?/projects/[^/]+/datasets/[^/:]+(:undelete)?$`)
	rowPoliciesRoute    = regexp.MustCompile(`^(?:/bigquery/v2)?/projects/[^/]+/datasets/[^/]+/tables/[^/]+/rowAccessPolicies(?:(:batchDelete)|/([^/:]+)(?::(getIamPolicy|testIamPermissions))?)?$`)
	iamRoute            = regexp.MustCompile(`^(?:/bigquery/v2)?/projects/[^/]+/datasets/[^/]+/(tables|routines)/[^/:]+:(getIamPolicy|setIamPolicy|testIamPermissions)$`)
)

// unservedMethod is the discovery ID of the method r calls when it is one
// the front refuses, such as bigquery.tables.getIamPolicy, and "" otherwise.
func unservedMethod(r *http.Request) string {
	p := r.URL.Path
	switch {
	case serviceAccountRoute.MatchString(p) && r.Method == http.MethodGet:
		return "bigquery.projects.getServiceAccount"
	case iamRoute.MatchString(p) && r.Method == http.MethodPost:
		m := iamRoute.FindStringSubmatch(p)
		return "bigquery." + m[1] + "." + m[2]
	}
	if m := datasetRoute.FindStringSubmatch(p); m != nil {
		switch {
		case m[1] != "" && r.Method == http.MethodPost:
			return "bigquery.datasets.undelete"
		case m[1] == "" && r.Method == http.MethodPatch:
			return "bigquery.datasets.patch"
		case m[1] == "" && r.Method == http.MethodPut:
			return "bigquery.datasets.update"
		}
		return ""
	}
	m := rowPoliciesRoute.FindStringSubmatch(p)
	if m == nil {
		return ""
	}
	batch, id, iam := m[1] != "", m[2] != "", m[3]
	switch {
	case batch && r.Method == http.MethodPost:
		return "bigquery.rowAccessPolicies.batchDelete"
	case iam != "" && r.Method == http.MethodPost:
		return "bigquery.rowAccessPolicies." + iam
	case iam != "" || batch:
		return ""
	case !id && r.Method == http.MethodGet:
		return "bigquery.rowAccessPolicies.list"
	case !id && r.Method == http.MethodPost:
		return "bigquery.rowAccessPolicies.insert"
	case id && r.Method == http.MethodGet:
		return "bigquery.rowAccessPolicies.get"
	case id && r.Method == http.MethodPut:
		return "bigquery.rowAccessPolicies.update"
	case id && r.Method == http.MethodDelete:
		return "bigquery.rowAccessPolicies.delete"
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
		"bigquery.datasets.patch":             "the emulator answers it but keeps nothing (measured: a datasets.get afterwards reads the dataset as before). Nothing was changed.",
		"bigquery.datasets.update":            "the emulator answers it but keeps nothing (measured: a datasets.get afterwards reads the dataset as before). Nothing was changed.",
		"bigquery.datasets.undelete":          "the emulator keeps no deleted datasets.",
		"bigquery.projects.getServiceAccount": "the emulator has no service account to name.",
	}[method]
	if why == "" {
		why = "the emulator has no row-level security or IAM policies on tables and routines."
	}
	writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: "+method+". "+why)
	return true
}
