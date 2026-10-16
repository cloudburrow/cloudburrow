package bigqueryfront

import (
	"net/http"
	"regexp"
	"strings"
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
//   - every rowAccessPolicies method (the front's since #1218,
//     rowpolicies.go, and the two IAM ones since #1231, iam.go), and
//     tables.getIamPolicy, tables.setIamPolicy and tables.testIamPermissions
//     (the front's since #1231, iam.go), answered 500 ("unexpected request
//     path", "unsupported bigquery.…"), which the Go client retries;
//   - routines.getIamPolicy, routines.setIamPolicy and
//     routines.testIamPermissions answered 405 "method not allowed" (the
//     front's since #1231, iam.go).
var (
	serviceAccountRoute = regexp.MustCompile(`^(?:/bigquery/v2)?/projects/([^/]+)/serviceAccount$`)
	datasetRoute        = regexp.MustCompile(`^(?:/bigquery/v2)?/projects/[^/]+/datasets/[^/:]+(:undelete)?$`)
)

// unservedMethod is the discovery ID of the method r calls when it is one
// the front refuses, such as bigquery.datasets.undelete, and "" otherwise.
func unservedMethod(r *http.Request) string {
	p := r.URL.Path
	if m := datasetRoute.FindStringSubmatch(p); m != nil && m[1] != "" && r.Method == http.MethodPost {
		return "bigquery.datasets.undelete"
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
		"bigquery.datasets.undelete": undropWhy,
	}[method]
	writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: "+method+". "+why)
	return true
}

// undropWhy is why datasets.undelete and UNDROP SCHEMA are 501 (#1460):
// CloudBurrow reads a table's past rows from its change history
// (timetravel.go), but the engine drops a table's change history with the
// table, and a dataset's tables with it.
const undropWhy = "the emulator deletes a dataset's tables, their rows and their change history with it, and keeps no " +
	"deleted dataset to restore (docs/compatibility.md)."

// refuseUndrop answers 501 a query that is UNDROP SCHEMA
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language#undrop_schema_statement),
// which the engine fails as a syntax error, and reports whether it did.
func refuseUndrop(w http.ResponseWriter, q queryOptions) bool {
	if !strings.Contains(strings.ToUpper(q.Query), "UNDROP") {
		return false
	}
	toks, ok := lex(q.Query)
	if !ok {
		return false
	}
	for _, stmt := range splitStatements(toks) {
		if len(stmt) > 1 && stmt[0].is("UNDROP") && stmt[1].is("SCHEMA") {
			writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: UNDROP SCHEMA. "+undropWhy+" Nothing was run.")
			return true
		}
	}
	return false
}
