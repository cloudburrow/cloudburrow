package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// CREATE TABLE ... CLONE, CREATE SNAPSHOT TABLE ... CLONE and DROP SNAPSHOT
// TABLE (#1002). The emulator refuses all three (measured, #937: 400
// "CREATE TABLE CLONE is not supported", and "Statement not supported" for
// the snapshot ones), so the front carries out a query that is one such
// statement itself, the way a copy job with operationType CLONE or SNAPSHOT
// is carried out (copyjob.go):
//
//   - CREATE TABLE [IF NOT EXISTS] dest CLONE src
//     (https://cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language#create_table_clone_statement)
//     makes dest with src's schema and rows and a cloneDefinition (src as
//     baseTableReference, the statement's start as cloneTime), a writable
//     TABLE. Of a table snapshot it restores it, as "Restore a table
//     snapshot" does with this statement: a standard table, no
//     cloneDefinition, as a RESTORE copy job makes.
//   - CREATE SNAPSHOT TABLE [IF NOT EXISTS] snap CLONE src
//     (#create_snapshot_table_statement) makes snap with a
//     snapshotDefinition, read-only (snapshots.go). Of a snapshot, a view or
//     an external table it fails, as the SNAPSHOT copy job does.
//   - DROP SNAPSHOT TABLE [IF EXISTS] snap (#drop_snapshot_table_statement)
//     deletes a snapshot.
//
// Without IF NOT EXISTS a destination that exists fails "Already Exists",
// as the syntax gives; with it nothing is done (ddlOperationPerformed SKIP).
// The job is recorded with a statement that does nothing (failBeforeRun's)
// and the client's text, and jobs.get reports the statementType
// (CREATE_SNAPSHOT_TABLE and DROP_SNAPSHOT_TABLE, as JobStatistics2 names
// them; CREATE_TABLE for a clone, UNVERIFIED), ddlOperationPerformed and
// ddlTargetTable (jobText.ddl).
//
// FOR SYSTEM_TIME AS OF the source is read as a query's is
// (partitionreads.go, #1276): a time before the table's time travel window
// fails 400 as BigQuery documents it, CURRENT_TIMESTAMP() (the engine's
// now) clones the table as it is, and any other time is 501, as the
// emulator keeps no history of a table's rows. OPTIONS(...) after the
// source sets description, friendly_name, labels and expiration_timestamp
// on the new table as CREATE TABLE's OPTIONS does (ddloptions.go); the
// emulator does not delete a table when its expiration passes. Any other
// option is 501.
//
// 501, nothing done: OR REPLACE (not in the
// documented syntax), a table of another project, a dry run, legacy SQL,
// any of these statements inside a multi-statement script (checkDDL), and
// DROP SNAPSHOT TABLE of a table that is not a snapshot (BigQuery's
// documentation does not say what it does).

// cloneStmt is one of the statements above.
type cloneStmt struct {
	kind     string // "clone", "snapshot" or "drop"
	ifClause bool   // IF NOT EXISTS, or (drop) IF EXISTS
	dest     []string
	src      []string
	// asOf is the expression of FOR SYSTEM_TIME AS OF, "" without one;
	// options the statement's OPTIONS(...), nil without them.
	asOf    string
	options *ddlOptions
}

// parseCloneDDL reads a lone CREATE [SNAPSHOT] TABLE ... CLONE or DROP
// SNAPSHOT TABLE statement. ok is false for any other query; unsupported
// names what of one the front does not carry out.
func parseCloneDDL(sql string) (st cloneStmt, unsupported string, ok bool) {
	t, lexed := lex(sql)
	if !lexed || len(t) < 4 {
		return st, "", false
	}
	end := len(t)
	if t[end-1].punct(";") {
		end--
	}
	for _, tok := range t[:end] {
		if tok.punct(";") {
			return st, "", false
		}
	}
	t = t[:end]
	if t[0].is("DROP") {
		if len(t) < 4 || !t[1].is("SNAPSHOT") || !t[2].is("TABLE") {
			return st, "", false
		}
		st.kind = "drop"
		i := 3
		if i+1 < len(t) && t[i].is("IF") && t[i+1].is("EXISTS") {
			st.ifClause, i = true, i+2
		}
		st.dest, i = path(t, i)
		if len(st.dest) == 0 || i != len(t) {
			return st, "", false
		}
		return st, "", true
	}
	if !t[0].is("CREATE") {
		return st, "", false
	}
	i, replace := 1, false
	if i+1 < len(t) && t[i].is("OR") && t[i+1].is("REPLACE") {
		replace, i = true, i+2
	}
	st.kind = "clone"
	if i < len(t) && t[i].is("SNAPSHOT") {
		st.kind, i = "snapshot", i+1
	}
	if i >= len(t) || !t[i].is("TABLE") {
		return st, "", false
	}
	i++
	if i+2 < len(t) && t[i].is("IF") && t[i+1].is("NOT") && t[i+2].is("EXISTS") {
		st.ifClause, i = true, i+3
	}
	st.dest, i = path(t, i)
	if len(st.dest) == 0 || i >= len(t) || !t[i].is("CLONE") {
		return st, "", false
	}
	st.src, i = path(t, i+1)
	if len(st.src) == 0 {
		return st, "", false
	}
	if replace {
		return st, "CREATE OR REPLACE " + strings.ToUpper(cloneStatement(st.kind)) + ", which BigQuery's documented " +
			"syntax does not have", true
	}
	if i+3 < len(t) && t[i].is("FOR") && t[i+1].is("SYSTEM_TIME") && t[i+2].is("AS") && t[i+3].is("OF") {
		end := i + 4
		for depth := 0; end < len(t); end++ {
			switch {
			case t[end].punct("(") || t[end].punct("["):
				depth++
			case t[end].punct(")") || t[end].punct("]"):
				depth--
			}
			if depth == 0 && t[end].is("OPTIONS") && end+1 < len(t) && t[end+1].punct("(") {
				break
			}
		}
		if end == i+4 {
			return st, "", false
		}
		st.asOf = sql[t[i+4].pos:t[end-1].end]
		i = end
	}
	if i+1 < len(t) && t[i].is("OPTIONS") && t[i+1].punct("(") {
		close := skipTo(t, i+2, ")")
		if close != len(t)-1 {
			return st, "", false
		}
		st.options = &ddlOptions{}
		if msg := st.options.readOptions(sql, t[i+2:close]); msg != "" {
			return st, cloneStatement(st.kind) + " with " + msg, true
		}
		i = len(t)
	}
	if i < len(t) {
		return st, "", false
	}
	return st, "", true
}

// cloneStatement names a cloneStmt's kind as its statement.
func cloneStatement(kind string) string {
	switch kind {
	case "snapshot":
		return "CREATE SNAPSHOT TABLE"
	case "drop":
		return "DROP SNAPSHOT TABLE"
	}
	return "CREATE TABLE CLONE"
}

// serveCloneDDL carries out st, the whole of q (above).
func (f front) serveCloneDDL(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool, st cloneStmt) {
	project := projectOf(f.base)
	what := cloneStatement(st.kind)
	failed := func(code int, reason, msg string) {
		f.failBeforeRun(w, r, q, insert, "_cloudburrow", code, rowError{Reason: reason, Message: msg})
	}
	notImplemented := func(msg string) {
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: "+msg+". Nothing was run.")
	}
	ref := func(p []string) (tableRef, bool) {
		ds, table, ok := tableOf(q, p)
		if !ok {
			return tableRef{}, false
		}
		out := tableRef{ProjectID: project, DatasetID: ds, TableID: table}
		if len(p) >= 3 && p[len(p)-3] != project {
			out.ProjectID = p[len(p)-3]
		}
		return out, true
	}
	dest, ok := ref(st.dest)
	if !ok {
		failed(http.StatusBadRequest, "invalidQuery", "Table \""+strings.Join(st.dest, ".")+
			"\" must be qualified with a dataset (e.g. dataset.table).")
		return
	}
	var src tableRef
	if st.kind != "drop" {
		if src, ok = ref(st.src); !ok {
			failed(http.StatusBadRequest, "invalidQuery", "Table \""+strings.Join(st.src, ".")+
				"\" must be qualified with a dataset (e.g. dataset.table).")
			return
		}
		if msg := checkTableID(dest.TableID); msg != "" {
			failed(http.StatusBadRequest, "invalidQuery", msg)
			return
		}
	}
	for _, t := range []tableRef{dest, src} {
		if t.ProjectID != "" && t.ProjectID != project {
			notImplemented(fmt.Sprintf("%s of a table of project %s, not the job's project %s", what, t.ProjectID, project))
			return
		}
	}
	if st.kind != "drop" {
		if why := f.protectedRead(r, project, src.DatasetID, src.TableID, what); why != "" { // #1218
			writeError(w, http.StatusNotImplemented, "notImplemented", why)
			return
		}
	}
	d := f.copyTableMeta(r, dest)
	done := func(op string) {
		ddl := &ownDDL{target: tableRef{ProjectID: project, DatasetID: dest.DatasetID, TableID: dest.TableID}, operation: op}
		switch st.kind {
		case "clone":
			ddl.statementType = "CREATE_TABLE"
		case "snapshot":
			ddl.statementType = "CREATE_SNAPSHOT_TABLE"
		default:
			ddl.statementType = "DROP_SNAPSHOT_TABLE"
		}
		if !setQueryText(r, insert, "DROP TABLE IF EXISTS "+quotePath([]string{"_cloudburrow", scratchTable()})) {
			writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query")
			return
		}
		f.forward(w, r, jobText{query: q.Query, ddl: ddl})
	}

	if st.kind == "drop" {
		switch {
		case !d.exists && st.ifClause:
			done("SKIP")
		case !d.exists:
			failed(http.StatusNotFound, "notFound", "Not found: Table "+tableName(project, dest))
		case d.snapshot == nil:
			notImplemented(fmt.Sprintf("DROP SNAPSHOT TABLE of %s, which is not a table snapshot. BigQuery's "+
				"documentation does not say what it does; DROP TABLE deletes it", tableName(project, dest)))
		default:
			if status, got := f.send(r, http.MethodDelete, tablePath(dest.DatasetID, dest.TableID), nil); status/100 != 2 {
				failed(status, "backendError", errorMessage(got, status))
				return
			}
			done("DROP")
		}
		return
	}

	s := f.copyTableMeta(r, src)
	switch {
	case !s.exists:
		failed(http.StatusNotFound, "notFound", "Not found: Table "+tableName(project, src))
		return
	case isView(s.kind) || strings.EqualFold(s.kind, "EXTERNAL"):
		failed(http.StatusBadRequest, "invalidQuery", fmt.Sprintf("%s is not allowed for %s because it is currently a %s.",
			tableName(project, src), what, strings.ToUpper(s.kind)))
		return
	case st.kind == "snapshot" && s.snapshot != nil:
		failed(http.StatusBadRequest, "invalidQuery", fmt.Sprintf("%s is not allowed for %s because it is a table snapshot.",
			tableName(project, src), what))
		return
	case d.exists && st.ifClause:
		done("SKIP")
		return
	case d.exists:
		failed(http.StatusConflict, "duplicate", "Already Exists: Table "+tableName(project, dest))
		return
	}
	if status, _ := f.get(r, "/datasets/"+url.PathEscape(dest.DatasetID)); status == http.StatusNotFound {
		failed(http.StatusNotFound, "notFound", "Not found: Dataset "+project+":"+dest.DatasetID)
		return
	}
	if st.asOf != "" {
		// Read as a query's FOR SYSTEM_TIME AS OF is (partitionreads.go):
		// before the time travel window fails, now is the table as it is,
		// and any other time is 501.
		var m partitionMeta
		if status, got := f.get(r, tablePath(src.DatasetID, src.TableID)); status == http.StatusOK {
			_ = json.Unmarshal(got, &m)
		}
		switch code, msg := f.systemTime(r, q, src.DatasetID, tableName(project, src), m, st.asOf); {
		case code == http.StatusNotImplemented:
			writeError(w, code, "notImplemented", msg)
			return
		case code != 0:
			failed(code, "invalidQuery", msg)
			return
		}
	}
	var expiration string
	var setExpiration bool
	if st.options != nil {
		var status int
		var body []byte
		if expiration, setExpiration, status, body = f.expirationMillis(r, q, st.options.expiration); status != http.StatusOK {
			failed(http.StatusBadRequest, "invalidQuery", "expiration_timestamp: "+errorMessage(body, status))
			return
		}
	}
	start := time.Now().UTC().Format(time.RFC3339Nano)
	base := map[string]string{"projectId": project, "datasetId": src.DatasetID, "tableId": src.TableID}
	var extra map[string]any
	switch {
	case st.kind == "snapshot":
		extra = map[string]any{"snapshotDefinition": map[string]any{"baseTableReference": base, "snapshotTime": start}}
	case s.snapshot == nil:
		extra = map[string]any{"cloneDefinition": map[string]any{"baseTableReference": base, "cloneTime": start}}
	}
	if msg := f.makeTableWith(r, dest, s.schema, extra); msg != "" {
		failed(http.StatusBadRequest, "invalidQuery", msg)
		return
	}
	if msg := f.insertSelect(r, dest, []copyTable{s}); msg != "" {
		f.send(r, http.MethodDelete, tablePath(dest.DatasetID, dest.TableID), nil)
		failed(http.StatusInternalServerError, "backendError", msg)
		return
	}
	if st.options != nil && st.options.sets() {
		if msg := f.setTableOptions(r, dest.DatasetID, dest.TableID, *st.options, expiration, setExpiration); msg != "" {
			f.send(r, http.MethodDelete, tablePath(dest.DatasetID, dest.TableID), nil)
			failed(http.StatusInternalServerError, "backendError", msg)
			return
		}
	}
	done("CREATE")
}

// ownDDL is what jobs.get reports of a DDL statement the front carried out
// itself (cloneddl.go), in statistics.query.
type ownDDL struct {
	statementType string
	operation     string // ddlOperationPerformed
	target        tableRef
}

// patch puts d in a Job resource.
func (d ownDDL) patch(job map[string]any) {
	st, _ := job["statistics"].(map[string]any)
	if st == nil {
		st = map[string]any{}
		job["statistics"] = st
	}
	q, _ := st["query"].(map[string]any)
	if q == nil {
		q = map[string]any{}
		st["query"] = q
	}
	q["statementType"] = d.statementType
	q["ddlOperationPerformed"] = d.operation
	q["ddlTargetTable"] = map[string]any{"projectId": d.target.ProjectID, "datasetId": d.target.DatasetID,
		"tableId": d.target.TableID}
}
