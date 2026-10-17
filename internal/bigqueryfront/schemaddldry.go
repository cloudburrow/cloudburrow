package bigqueryfront

import (
	"net/http"
	"strings"
)

// A dry run of UNDROP SCHEMA and of ALTER SCHEMA … SET OPTIONS, alone or
// in a script (#1562).
//
// BigQuery's dry run "returns statistics" of a valid query without running
// it (https://cloud.google.com/bigquery/docs/running-queries#dry-run), and
// a job's statementType is UNDROP_SCHEMA for an UNDROP SCHEMA statement,
// ALTER_SCHEMA for an ALTER SCHEMA statement and SCRIPT for a script
// (https://cloud.google.com/bigquery/docs/reference/rest/v2/Job#JobStatistics2).
// The emulator knows neither statement (measured: 400 "Statement not
// supported: AlterSchemaStatement", and a syntax error at UNDROP), and the
// front carries both out itself when the query runs (dropped.go,
// alterschema.go). So a dry run of one alone is checked as the front reads
// it (the dataset's path; ALTER SCHEMA's OPTIONS, as alterSchema reads
// them) and answered as a dry run of a statement that does nothing, of its
// statementType; in a script, each is replaced by a statement that does
// nothing (SELECT 1) in the text the emulator's dry run checks
// (dryRunScript), the job SCRIPT, the client's text kept in the job.
// Nothing is run or changed. UNVERIFIED: whether BigQuery's dry run also
// looks the dataset up (an UNDROP SCHEMA of a dataset not dropped, an
// ALTER SCHEMA of none); CloudBurrow's does not.

// schemaDDLKind is "UNDROP_SCHEMA" or "ALTER_SCHEMA" for a statement body
// the front carries out, with a message for one it reads as invalid (bad,
// 400) or would refuse when run (notBuilt, 501, as alterSchema does), or
// "" for any other statement.
func (f front) schemaDDLKind(body []token) (kind, bad, notBuilt string) {
	switch {
	case len(body) > 2 && body[0].is("UNDROP") && body[1].is("SCHEMA"):
		i := 2
		if i+2 < len(body) && body[i].is("IF") && body[i+1].is("NOT") && body[i+2].is("EXISTS") {
			i += 3
		}
		p, next := path(body, i)
		if len(p) == 0 || len(p) > 2 || next != len(body) {
			return "UNDROP_SCHEMA", "Syntax error: UNDROP SCHEMA takes a dataset name", ""
		}
		return "UNDROP_SCHEMA", "", ""
	case len(body) > 2 && body[0].is("ALTER") && body[1].is("SCHEMA"):
		i := 2
		if i+1 < len(body) && body[i].is("IF") && body[i+1].is("EXISTS") {
			i += 2
		}
		p, next := path(body, i)
		if len(p) == 0 || next+1 >= len(body) || !body[next].is("SET") || !body[next+1].is("OPTIONS") {
			return "", "", "" // another form of ALTER SCHEMA: the emulator's
		}
		synth := append(append(append([]token{}, body[0:2]...), body[i:next]...), body[next+1:]...)
		_, msg, bad := schemaOptions(synth, p[len(p)-1])
		if bad != "" {
			return "ALTER_SCHEMA", bad, ""
		}
		if msg != "" {
			return "ALTER_SCHEMA", "", "Not implemented here: ALTER SCHEMA with " + msg + ". Nothing was run."
		}
		return "ALTER_SCHEMA", "", ""
	}
	return "", "", ""
}

// dryRunSchemaDDL serves a dry run of q when it has an UNDROP SCHEMA or an
// ALTER SCHEMA … SET OPTIONS (above), and reports whether it did.
func (f front) dryRunSchemaDDL(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool) bool {
	upper := strings.ToUpper(q.Query)
	if !strings.Contains(upper, "UNDROP") && !(strings.Contains(upper, "ALTER") && strings.Contains(upper, "SCHEMA")) {
		return false
	}
	toks, ok := lex(q.Query)
	if !ok {
		return false
	}
	type cut struct{ pos, end int }
	var cuts []cut
	n, lone := 0, ""
	for _, stmt := range splitStatements(toks) {
		if len(stmt) == 0 {
			continue
		}
		n++
		body, _, _ := stripControlFlow(stmt)
		for len(body) > 0 && body[len(body)-1].punct(";") {
			body = body[:len(body)-1]
		}
		kind, bad, notBuilt := f.schemaDDLKind(body)
		if bad != "" {
			writeError(w, http.StatusBadRequest, "invalidQuery", bad)
			return true
		}
		if notBuilt != "" {
			writeError(w, http.StatusNotImplemented, "notImplemented", notBuilt)
			return true
		}
		if kind != "" {
			cuts = append(cuts, cut{body[0].pos, body[len(body)-1].end})
			lone = kind
		}
	}
	if len(cuts) == 0 {
		return false
	}
	if n == 1 {
		if !setQueryText(r, insert, "SELECT 1") {
			writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query")
			return true
		}
		rec := newRecorder()
		f.next.ServeHTTP(rec, r)
		if rec.status == http.StatusOK || rec.status == 0 {
			b := dryDDLAnswer(rec.body.Bytes(), lone)
			rec.body.Reset()
			rec.body.Write(b)
		}
		f.clientText(w, rec, jobText{query: q.Query})
		return true
	}
	var b strings.Builder
	last := 0
	for _, c := range cuts {
		b.WriteString(q.Query[last:c.pos])
		b.WriteString("SELECT 1")
		last = c.end
	}
	b.WriteString(q.Query[last:])
	text := b.String()
	if !setQueryText(r, insert, text) {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query")
		return true
	}
	client := q.Query
	q.Query = text
	rec := newRecorder()
	f.dryRunScript(rec, r, q, insert)
	f.clientText(w, rec, jobText{query: client})
	return true
}
