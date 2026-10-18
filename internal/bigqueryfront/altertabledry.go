package bigqueryfront

import (
	"net/http"
)

// ALTER TABLE in a dry run (#1416).
//
// "If the job is a dry run, then the query isn't run and BigQuery
// returns statistics about the job"
// (https://cloud.google.com/bigquery/docs/reference/rest/v2/Job#JobConfiguration);
// a dry run validates the statement and changes nothing. The emulator's
// own dry run of an ALTER TABLE answers it as a SELECT and checks nothing
// (measured, #901), and checkDDL refused it 501 before this.
//
// The front now checks a lone ALTER TABLE as its run would (alterTable,
// in a scriptRun with dry set), every check it makes before changing the
// table: the statement read (400, or 501 for what the front does not
// carry out), the table (404 notFound, nothing with IF EXISTS), each
// action against the table's columns, keys, partitioning and clustering
// (400: a column missing or taken, a coercion SET DATA TYPE does not
// make, a rounding_mode refused, a REFERENCES of a missing table 404, and
// the rest of altertable.go's refusals), and stops before the table is
// changed. The answer is the emulator's dry run of a statement that does
// nothing, as BigQuery's for the statement: statistics.query.statementType
// ALTER_TABLE, totalBytesProcessed 0 and no schema or rows, as the
// statement gives no result (dryDDLAnswer); jobs.get shows the client's
// text. BigQuery's dry run of an ALTER TABLE was not measured: the
// statement type is the reference's (JobStatistics2.statementType
// "ALTER_TABLE"), the rest UNVERIFIED. The schema checks the tables.patch
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/reference/rest/v2/Job
// path makes when it is sent (updateTable) are not made in a dry run. A
// script's dry run that stops at an ALTER TABLE checks it so too
// (scriptdrystop.go, #1456); one reaching it in a block CloudBurrow does
// not read for the stop stays 501 (checkDDL).

// loneAlterTable returns the tokens of sql when it is one ALTER TABLE
// statement, with no other statement or control flow.
func loneAlterTable(sql string) ([]token, bool) {
	toks, ok := lex(sql)
	if !ok {
		return nil, false
	}
	var found []token
	for _, stmt := range splitStatements(toks) {
		if len(stmt) == 0 {
			continue
		}
		if found != nil || !isAlterTable(stmt) {
			return nil, false
		}
		found = stmt
	}
	return found, found != nil
}

// dryRunAlter answers the dry run of stmt, q's lone ALTER TABLE (above).
func (f front) dryRunAlter(w http.ResponseWriter, r *http.Request, q queryOptions, stmt []token, insert bool) {
	s := &scriptRun{f: f, r: r, q: q, sql: q.Query, scopes: []map[string]scriptVar{{}}, start: scriptNow(), plain: true, dry: true}
	flow := s.alterTable(stmt)
	if s.notImpl != "" {
		writeError(w, http.StatusNotImplemented, "notImplemented", s.notImpl)
		return
	}
	if flow != nil && flow.failed {
		if len(flow.body) == 0 {
			writeError(w, flow.status, "invalidQuery", flow.msg)
			return
		}
		writeRaw(w, flow.status, flow.body)
		return
	}
	if !setQueryText(r, insert, "SELECT 1") {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query")
		return
	}
	rec := newRecorder()
	f.next.ServeHTTP(rec, r)
	if rec.status == http.StatusOK {
		b := dryDDLAnswer(rec.body.Bytes(), "ALTER_TABLE")
		rec.body.Reset()
		rec.body.Write(b)
	}
	f.clientText(w, rec, jobText{query: q.Query})
}
