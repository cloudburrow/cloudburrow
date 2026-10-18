package bigqueryfront

import "net/http"

// ALTER TABLE in legacy SQL (#1456).
//
// Legacy SQL has no DDL: GoogleSQL "lets you use features not supported
// by legacy SQL, such as DML and DDL statements"
// (https://docs.cloud.google.com/bigquery/docs/legacy-sql-feature-availability,
// docs re-read 2026-10-17), and the DDL reference is GoogleSQL's. So a
// legacy SQL query with an ALTER TABLE, which checkDDL refused 501 before
// this, fails as an invalid query, 400 invalidQuery, before anything runs.
// BigQuery's legacy SQL parser message for it is not documented: the text
// below is CloudBurrow's own, UNVERIFIED (docs re-read 2026-10-17,
// https://docs.cloud.google.com/bigquery/docs/reference/legacy-sql).

// legacyAlterTableMessage is the 400's message.
const legacyAlterTableMessage = "ALTER TABLE is a GoogleSQL statement: legacy SQL has no DDL statements. " +
	"Run it with useLegacySql set to false."

// legacyAlterTable reports whether sql has an ALTER TABLE statement.
func legacyAlterTable(sql string) bool {
	toks, ok := lex(sql)
	if !ok {
		return false
	}
	for _, stmt := range splitStatements(toks) {
		if isAlterTable(stmt) {
			return true
		}
	}
	return false
}

// refuseLegacyAlterTable answers q, a legacy SQL query, 400 when it has an
// ALTER TABLE, and reports whether it did.
func (f front) refuseLegacyAlterTable(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool) bool {
	if !legacyAlterTable(q.Query) {
		return false
	}
	f.failBeforeRun(w, r, q, insert, "_cloudburrow", http.StatusBadRequest,
		rowError{Reason: "invalidQuery", Message: legacyAlterTableMessage})
	return true
}
