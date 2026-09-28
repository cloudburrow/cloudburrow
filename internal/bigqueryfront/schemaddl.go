package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// CREATE SCHEMA of a dataset that exists (#946).
//
// BigQuery fails it: "CREATE SCHEMA ... Creates a new dataset"; with IF
// NOT EXISTS, "If any dataset exists with the same name, the CREATE
// statement has no effect"
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language#create_schema_statement),
// and without it the statement fails "Already Exists: Dataset
// project:name", as datasets.insert of one does (409 duplicate, which the
// front answers since #861). The emulator runs it as done (measured
// against the pinned image through the official Go client, jobs.query and
// jobs.insert alike): no error, and the dataset kept as it was, its
// tables too, even with OPTIONS(description=...). With IF NOT EXISTS it
// does nothing, as BigQuery does. (It also makes no dataset for CREATE
// SCHEMA of one that does not exist, measured the same way; that is not
// handled here, #951.)
//
// So a query whose first statement is CREATE SCHEMA, without IF NOT
// EXISTS, of a dataset of the instance's project that exists fails as
// BigQuery fails it, before anything is run: jobs.query with 409
// duplicate; a query job with that errorResult, which jobs.get and
// jobs.list then report (the emulator records the job with a statement
// that does nothing in its place). A later statement of a script that
// does so, for a dataset that exists before the script or that an earlier
// statement creates (and no earlier DROP SCHEMA may have removed), is 501:
// BigQuery runs the statements before it, and fails there; the emulator
// cannot be stopped in between. Nothing is run.

// createSchemaExists answers a query that creates a dataset that exists,
// as above, and reports whether it did.
func (f front) createSchemaExists(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool) bool {
	if !strings.Contains(strings.ToUpper(q.Query), "SCHEMA") {
		return false
	}
	toks, ok := lex(q.Query)
	if !ok {
		return false
	}
	project := projectOf(f.base)
	made, dropped := map[string]bool{}, map[string]bool{}
	n := 0
	for _, stmt := range splitStatements(toks) {
		if len(stmt) == 0 {
			continue
		}
		n++
		body, _, _ := stripControlFlow(stmt)
		ds, ifNotExists, drop, ok := schemaStatement(body, project)
		if !ok {
			continue
		}
		if drop {
			delete(made, ds)
			dropped[ds] = true
			continue
		}
		exists := made[ds]
		if !exists && !dropped[ds] {
			status, _ := f.get(r, "/datasets/"+url.PathEscape(ds))
			exists = status == http.StatusOK
		}
		made[ds] = true
		if !exists || ifNotExists {
			continue
		}
		if n > 1 || len(body) != len(stmt) {
			writeError(w, http.StatusNotImplemented, "notImplemented", fmt.Sprintf("Not implemented here: CREATE SCHEMA "+
				"%s, which exists, after other statements of a script. BigQuery runs the statements before it and fails "+
				"there (\"Already Exists\"), but the emulator behind CloudBurrow runs CREATE SCHEMA of an existing dataset "+
				"as done (measured), and CloudBurrow cannot stop the script in between. Nothing was run. Use CREATE SCHEMA "+
				"IF NOT EXISTS, or run the CREATE SCHEMA first.", ds))
			return true
		}
		e := rowError{Reason: "duplicate", Message: "Already Exists: Dataset " + project + ":" + ds}
		if !insert {
			writeError(w, http.StatusConflict, e.Reason, e.Message)
			return true
		}
		scratch := quotePath([]string{ds, scratchTable()})
		if !setQueryText(r, true, "DROP TABLE IF EXISTS "+scratch) {
			writeError(w, http.StatusConflict, e.Reason, e.Message)
			return true
		}
		rec := newRecorder()
		f.next.ServeHTTP(rec, r)
		var job map[string]any
		if rec.status != http.StatusOK || json.Unmarshal(rec.body.Bytes(), &job) != nil {
			writeError(w, http.StatusConflict, e.Reason, e.Message)
			return true
		}
		if conf, ok := job["configuration"].(map[string]any); ok {
			if qc, ok := conf["query"].(map[string]any); ok {
				qc["query"] = q.Query
			}
		}
		f.fail(w, rec, job, e)
		return true
	}
	return false
}

// schemaStatement reads a CREATE SCHEMA or DROP SCHEMA statement of a
// dataset in project: the dataset, and whether it is IF NOT EXISTS or a
// DROP. ok is false for any other statement.
func schemaStatement(t []token, project string) (ds string, ifNotExists, drop, ok bool) {
	if len(t) < 3 || !t[1].is("SCHEMA") {
		return "", false, false, false
	}
	i := 2
	switch {
	case t[0].is("CREATE"):
		if i+2 < len(t) && t[i].is("IF") && t[i+1].is("NOT") && t[i+2].is("EXISTS") {
			ifNotExists, i = true, i+3
		}
	case t[0].is("DROP"):
		drop = true
		if i+1 < len(t) && t[i].is("IF") && t[i+1].is("EXISTS") {
			i += 2
		}
	default:
		return "", false, false, false
	}
	parts, _ := path(t, i)
	switch {
	case len(parts) == 1:
	case len(parts) == 2 && parts[0] == project:
	default:
		return "", false, false, false
	}
	return parts[len(parts)-1], ifNotExists, drop, true
}
