package bigqueryfront

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// serveQuery checks and runs a query, from jobs.query or (insert) a query
// job's jobs.insert, whose body r still holds (#881, #901, #916, #917,
// #918).
//
// Before it is sent: a name BigQuery refuses in its DDL is 400 and a
// statement the emulator would not run is 501 (checkDDL), and so is a
// column name a CREATE TABLE ... AS SELECT's or a CREATE VIEW's query
// gives (ctasColumns).
//
// Measured against the pinned image, three more things differ from
// BigQuery, and are handled here:
//
//   - CREATE OR REPLACE TABLE, or VIEW, on one that exists fails with 400
//     "duplicate: table ... is already created"; the failed OR REPLACE
//     VIEW also left the view unreadable ("no such column"). On a table
//     or view that does not exist it creates it. So a lone statement that
//     replaces one is carried out by the front (replace); inside a script
//     of several statements, where the front cannot do that between them,
//     it is 501.
//   - A BEGIN ... EXCEPTION block's handler is never run: a failing
//     statement in the block fails the whole script with its error. A
//     script with a handler that fails is therefore 501, naming the
//     emulator's error: BigQuery may have handled it. The emulator runs a
//     script in one transaction, so a failed script left nothing of its
//     earlier statements (measured).
//   - A CREATE TABLE ... AS SELECT (or CREATE VIEW) whose query cannot run
//     alone, because it names a script variable or a table an earlier
//     statement of the script makes, cannot be checked beforehand. The
//     emulator supports no sessions (a query with createSession got no
//     session back, measured), so the script's earlier statements cannot
//     be run first. It is checked after the script ran instead, in the
//     table it made: a name BigQuery refuses deletes the table and fails
//     the query. The script's later statements were run, and are not
//     undone; the error says so. A TEMP table, gone when the script ends,
//     is checked before the script instead (tempColumns, #938).
//   - (#932) CREATE TABLE or VIEW ... IF NOT EXISTS of one that exists
//     fails, where BigQuery does nothing: such a statement is replaced by
//     one that does nothing (skipIfExists).
//   - (#933) The emulator keeps a script's variables for every later query:
//     they are sent under names of their own (renameVariables).
//   - (#935) A failed script is rolled back whole, where BigQuery keeps what
//     its statements did before the failing one: a script with a statement
//     that changes data and another after it is 501 when it fails
//     (ddlVerdict.keepsOnFailure), naming the emulator's error. Running the
//     statements before the failure again is not safe: measured, the
//     rollback left the emulator's catalog out of step with its tables (a
//     rolled-back DROP TABLE left the table listed and not found by a
//     query; a rolled-back CREATE TEMP TABLE left its columns for the next
//     one of that name to be analysed against). A syntax error fails a
//     script before anything runs, in BigQuery too, so it is answered as
//     the emulator answered it.
//
// A query sent with another text than the client's is recorded so, and
// jobs.get shows the client's (jobTexts, #939).
func (f front) serveQuery(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool) {
	if q.UseLegacySQL != nil && *q.UseLegacySQL {
		// Legacy SQL has no DDL or scripting.
		f.next.ServeHTTP(w, r)
		return
	}
	v := checkDDL(q.Query)
	if v.code != 0 {
		writeError(w, v.code, v.reason, v.msg)
		return
	}
	var deferred []deferredCheck
	var unchecked []string // TEMP tables whose columns could not be read (#938)
	for _, c := range v.selects() {
		msg, ran := f.ctasColumns(r, q, c.query)
		if msg != "" {
			writeError(w, http.StatusBadRequest, "invalidQuery", msg)
			return
		}
		if ran {
			continue
		}
		if c.temp {
			code, msg := f.tempColumns(r, q, v, c)
			switch code {
			case 0:
			case http.StatusAccepted:
				unchecked = append(unchecked, msg)
			case http.StatusNotImplemented:
				writeError(w, code, "notImplemented", msg)
				return
			default:
				writeError(w, code, "invalidQuery", msg)
				return
			}
			continue
		}
		if ds, table, ok := tableOf(q, c.path); ok {
			if status, _ := f.get(r, tablePath(ds, table)); status == http.StatusNotFound {
				deferred = append(deferred, deferredCheck{c, ds, table})
			}
		}
	}
	for _, c := range v.creates {
		if !c.replace || c.temp {
			continue
		}
		ds, table, ok := tableOf(q, c.path)
		if !ok {
			continue
		}
		status, got := f.get(r, tablePath(ds, table))
		var meta struct {
			Type string `json:"type"`
		}
		if status != http.StatusOK || json.Unmarshal(got, &meta) != nil || strings.EqualFold(meta.Type, "VIEW") != c.view {
			// Not there, or not a table (a view): the emulator's own
			// answer stands.
			continue
		}
		if v.statements > 1 {
			what := "TABLE"
			if c.view {
				what = "VIEW"
			}
			writeError(w, http.StatusNotImplemented, "notImplemented", fmt.Sprintf("Not implemented here: CREATE OR REPLACE "+
				"%s %s, which exists, inside a script of several statements. BigQuery replaces it, but the emulator behind "+
				"CloudBurrow fails on an existing one (400 \"table is already created\", measured), and CloudBurrow replaces "+
				"it only for a statement run on its own. Nothing was run. Run the CREATE OR REPLACE as a query of its own, or "+
				"DROP the %s first.", what, strings.Join(c.path, "."), strings.ToLower(what)))
			return
		}
		f.replace(w, r, q, c, ds, table, insert)
		return
	}
	text, changed := f.skipIfExists(r, q, v)
	text, names := renameVariables(text)
	var client jobText
	if changed || names != nil {
		if !setQueryText(r, insert, text) {
			writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query")
			return
		}
		client = jobText{query: q.Query, names: names}
	}
	keeps := v.keepsOnFailure()
	if len(deferred) == 0 && len(unchecked) == 0 && !v.handler && !keeps {
		f.forward(w, r, client)
		return
	}

	rec := newRecorder()
	f.forward(rec, r, client)
	var job map[string]any
	if insert && rec.status == http.StatusOK {
		_ = json.Unmarshal(rec.body.Bytes(), &job)
	}
	errMsg, failed := queryFailure(rec, job)
	if v.handler && failed {
		e := rowError{Reason: "notImplemented", Message: "Not implemented here: the script failed (" + errMsg + ") and has a " +
			"BEGIN ... EXCEPTION block. BigQuery runs the handler when a statement in its block fails, but the emulator " +
			"behind CloudBurrow never runs a handler: it fails the whole script with the statement's error (measured), " +
			"so BigQuery may have handled this failure. The emulator ran the script in one transaction, so nothing of it " +
			"was kept."}
		f.fail(w, rec, job, e)
		return
	}
	if failed && keeps && !strings.HasPrefix(errMsg, "failed to parse statements") {
		// #935. A syntax error fails a script before anything runs, in
		// BigQuery as in the emulator.
		why := "BigQuery keeps what the statements before the failing one did"
		if v.transaction {
			why = "BigQuery keeps what the statements before the failing one did, outside a transaction it rolls back"
		}
		f.fail(w, rec, job, rowError{Reason: "notImplemented", Message: "Not implemented here: the script failed (" + errMsg +
			"). " + why + ", but the emulator behind CloudBurrow runs a script in one transaction and rolled all of it back " +
			"(measured), so nothing the script did was kept. Run the statements that must be kept as a query of their " +
			"own, before the rest."})
		return
	}
	if failed {
		rec.copyTo(w)
		return
	}
	if len(unchecked) > 0 {
		f.fail(w, rec, job, rowError{Reason: "notImplemented", Message: unchecked[0]})
		return
	}
	for _, d := range deferred {
		status, got := f.get(r, tablePath(d.dataset, d.table))
		var meta struct {
			Schema tableSchema `json:"schema"`
		}
		if status != http.StatusOK || json.Unmarshal(got, &meta) != nil {
			continue
		}
		msg := checkNames(meta.Schema.Fields, "", anonymousColumn, checkColumnName)
		if msg == "" {
			continue
		}
		f.send(r, http.MethodDelete, tablePath(d.dataset, d.table), nil)
		what := "CREATE TABLE ... AS SELECT"
		if d.view {
			what = "CREATE VIEW"
		}
		msg += fmt.Sprintf(" The name was given by the query of the %s %s, which names a script variable or a table the "+
			"script makes, so it could only be checked after the script ran; %s was then deleted.", what,
			strings.Join(d.path, "."), strings.Join(d.path, "."))
		if d.later > 0 {
			msg += fmt.Sprintf(" The %d statements after it in the script were run and are not undone; BigQuery would "+
				"not have run them.", d.later)
		}
		f.fail(w, rec, job, rowError{Reason: "invalidQuery", Message: msg})
		return
	}
	rec.copyTo(w)
}

// skipIfExists returns q's text with each CREATE TABLE or CREATE VIEW ...
// IF NOT EXISTS whose table or view exists replaced by a statement that
// does nothing, and whether it replaced any (#932).
//
// "If any table exists with the same name, the CREATE statement has no
// effect", and the same for a view
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language).
// The emulator instead fails every form of it on an existing table or
// view, 400 "table is already created" or, for CREATE TABLE ... AS
// SELECT, "SQL logic error: table ... already exists" (measured against
// the pinned image), and in a script that fails the whole script. It
// creates one that does not exist.
//
// A table exists for the statement when it exists before the query runs,
// or when an earlier statement of the script creates it; but not when an
// earlier DROP of it or of its dataset may have removed it first: that
// statement is left to the emulator, which then creates the table if the
// DROP removed it. The replacement is DROP TABLE IF EXISTS of a table
// that is not there, in the same dataset: the emulator runs it as a
// statement that returns no rows and changes nothing (measured), so the
// query is still a DDL statement with no result, and a query job is
// recorded by the emulator as usual, with that text. A TEMP table is not
// looked up: it is gone when its script ends.
func (f front) skipIfExists(r *http.Request, q queryOptions, v ddlVerdict) (string, bool) {
	type edit struct {
		pos, end int
		text     string
	}
	var edits []edit
	made := map[string]bool{}
	for _, c := range v.creates {
		ds, table, ok := tableOf(q, c.path)
		if c.temp || !ok {
			continue
		}
		key := ds + "\x00" + table
		if c.ifNotExists && !droppedBefore(q, v.drops, c.pos, ds, table) {
			exists := made[key]
			if !exists {
				status, _ := f.get(r, tablePath(ds, table))
				exists = status == http.StatusOK
			}
			if exists {
				scratch := append(append([]string{}, c.path[:len(c.path)-1]...), scratchTable())
				edits = append(edits, edit{c.pos, c.pos + len(c.text), "DROP TABLE IF EXISTS " + quotePath(scratch)})
			}
		}
		made[key] = true
	}
	if len(edits) == 0 {
		return q.Query, false
	}
	text := q.Query
	for i := len(edits) - 1; i >= 0; i-- {
		e := edits[i]
		text = text[:e.pos] + e.text + text[e.end:]
	}
	return text, true
}

// droppedBefore reports whether a DROP before offset pos names the table
// or its dataset.
func droppedBefore(q queryOptions, drops []dropStmt, pos int, dataset, table string) bool {
	for _, d := range drops {
		if d.pos >= pos {
			continue
		}
		if d.schema {
			if d.path[len(d.path)-1] == dataset {
				return true
			}
			continue
		}
		if ds, t, ok := tableOf(q, d.path); !ok || ds == dataset && t == table {
			return true
		}
	}
	return false
}

// deferredCheck is a CREATE whose columns are checked after the script
// ran, in the table it made.
type deferredCheck struct {
	createStmt
	dataset, table string
}

// queryFailure reports whether the emulator failed a query: jobs.query's
// error status, or a query job's errorResult, and its message.
func queryFailure(rec *recorder, job map[string]any) (string, bool) {
	if rec.status != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(rec.body.Bytes(), &e)
		if e.Error.Message == "" {
			e.Error.Message = fmt.Sprintf("HTTP %d", rec.status)
		}
		return e.Error.Message, true
	}
	if status, ok := job["status"].(map[string]any); ok {
		if res, ok := status["errorResult"].(map[string]any); ok {
			msg, _ := res["message"].(string)
			return msg, true
		}
	}
	return "", false
}

// fail answers a query as failed with e: a query job (job, from
// jobs.insert) as done with e as its errorResult, which jobs.get and
// jobs.list then report too; jobs.query as an error, 501 for
// notImplemented and 400 for any other reason.
func (f front) fail(w http.ResponseWriter, rec *recorder, job map[string]any, e rowError) {
	if job == nil {
		code := http.StatusBadRequest
		if e.Reason == "notImplemented" {
			code = http.StatusNotImplemented
		}
		writeError(w, code, e.Reason, e.Message)
		return
	}
	project, id := projectOf(f.base), ""
	if ref, ok := job["jobReference"].(map[string]any); ok {
		id, _ = ref["jobId"].(string)
		if p, ok := ref["projectId"].(string); ok && p != "" {
			project = p
		}
	}
	f.failed.add(project, id, e)
	failJob(job, e)
	writeJSON(w, http.StatusOK, job)
}

// replace carries out a lone CREATE OR REPLACE TABLE or VIEW whose table
// or view exists, which the emulator fails (#918). BigQuery replaces it
// only if the new one can be made, so the old one is kept when the
// statement fails:
//
//   - A table is first made under a scratch name in the same dataset, by
//     the same statement: if that fails, its error is the answer and the
//     table is untouched. Then the table is deleted and the statement is
//     sent with its query replaced by SELECT * FROM the scratch table
//     (with a column list and no query, as it was), which reads what the
//     query gave even when the query read the table itself; then the
//     scratch table is deleted. The job the emulator records shows that
//     query; jobs.insert's answer, jobs.get and jobs.list show the
//     client's (jobTexts, #939).
//   - A view's query was run alone to check its columns; if it cannot run
//     alone, that error is the answer. Then the view is deleted and the
//     statement sent as it is.
func (f front) replace(w http.ResponseWriter, r *http.Request, q queryOptions, c createStmt, dataset, table string, insert bool) {
	text := c.text
	if c.view {
		if _, status, body := f.lone(r, q, c.query); status != http.StatusOK {
			writeRaw(w, status, body)
			return
		}
	} else {
		scratch := scratchTable()
		scratchPath := quotePath(append(append([]string{}, c.path[:len(c.path)-1]...), scratch))
		body, err := json.Marshal(queryOptions{Query: text[:c.pathPos] + scratchPath + text[c.pathEnd:],
			UseLegacySQL: q.UseLegacySQL, DefaultDataset: q.DefaultDataset, ParameterMode: q.ParameterMode,
			QueryParameters: q.QueryParameters})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internalError", err.Error())
			return
		}
		status, got := f.send(r, http.MethodPost, "/queries", body)
		if status != http.StatusOK {
			f.send(r, http.MethodDelete, tablePath(dataset, scratch), nil)
			writeRaw(w, status, bytes.ReplaceAll(got, []byte(scratch), []byte(table)))
			return
		}
		defer f.send(r, http.MethodDelete, tablePath(dataset, scratch), nil)
		if c.query == "" {
			f.send(r, http.MethodDelete, tablePath(dataset, scratch), nil)
		} else {
			text = text[:c.queryPos] + "SELECT * FROM " + scratchPath + text[c.queryEnd:]
		}
	}
	if !setQueryText(r, insert, q.Query[:c.pos]+text+q.Query[c.pos+len(c.text):]) {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query")
		return
	}
	f.send(r, http.MethodDelete, tablePath(dataset, table), nil)
	f.forward(w, r, jobText{query: q.Query})
}

// setQueryText replaces the query text in r's body: jobs.query's query, or
// (insert) a query job's configuration.query.query.
func setQueryText(r *http.Request, insert bool, text string) bool {
	b, err := readBody(r)
	if err != nil {
		return false
	}
	var body map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if dec.Decode(&body) != nil {
		return false
	}
	target := body
	if insert {
		conf, _ := body["configuration"].(map[string]any)
		target, _ = conf["query"].(map[string]any)
		if target == nil {
			return false
		}
	}
	target["query"] = text
	out, err := json.Marshal(body)
	if err != nil {
		return false
	}
	setBody(r, out)
	return true
}

// tableOf returns the dataset and table a statement's path names: the
// dataset in the path, else the query's default dataset. ok is false when
// neither gives one.
func tableOf(q queryOptions, path []string) (dataset, table string, ok bool) {
	if len(path) == 0 {
		return "", "", false
	}
	table = path[len(path)-1]
	if len(path) >= 2 {
		return path[len(path)-2], table, true
	}
	var def struct {
		DatasetID string `json:"datasetId"`
	}
	if json.Unmarshal(q.DefaultDataset, &def) != nil || def.DatasetID == "" {
		return "", "", false
	}
	return def.DatasetID, table, true
}

func tablePath(dataset, table string) string {
	return "/datasets/" + url.PathEscape(dataset) + "/tables/" + url.PathEscape(table)
}

// quotePath writes a table path as one quoted identifier.
func quotePath(parts []string) string {
	return "`" + strings.ReplaceAll(strings.Join(parts, "."), "`", "\\`") + "`"
}

// scratchTable names a table the front makes and deletes within one
// request.
func scratchTable() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "_cloudburrow_replace_" + hex.EncodeToString(b)
}

// writeRaw answers with an emulator's status and JSON body as they were.
func writeRaw(w http.ResponseWriter, status int, body []byte) {
	if status == 0 {
		status = http.StatusBadGateway
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// tempColumns checks the columns of a CREATE TEMP TABLE ... AS SELECT
// whose query cannot run alone, because it names a script variable or a
// table an earlier statement makes (#938). It returns the status to
// answer the query with, 400 for a column BigQuery refuses and 501 when
// the columns cannot be read by running the statements before it, or 0;
// or StatusAccepted when the statements before it and the query failed
// to run, with the message to fail the script with should it not fail.
//
// A TEMP table is gone when its script ends, so it cannot be read after
// the script as other tables are (serveQuery), and the emulator has no
// sessions. The query is instead run after the statements before it, as
// the last statement of a script of its own: those statements, then
// `SELECT * FROM (query) LIMIT 0`, whose result's columns are the table's
// (measured: such a script returned its last query's columns, names such
// as "b!" and a STRUCT's field "c?" included, and no rows). This runs the
// statements before it twice, so it is done only when none of them
// changes what is kept after a script (ddlVerdict.writesBefore): a query,
// DECLARE, SET, a TEMP table and DML on one. A block the statement is in
// is closed with END. The variables are renamed as the script's are
// (renameVariables), so none outlives the check.
func (f front) tempColumns(r *http.Request, q queryOptions, v ddlVerdict, c createStmt) (int, string) {
	name := strings.Join(c.path, ".")
	if v.writesBefore(c.pos) {
		return http.StatusNotImplemented, "Not implemented here: CREATE TEMP TABLE " + name + " AS SELECT, whose query " +
			"names a script variable or a table the script makes, after a statement that changes data. BigQuery checks " +
			"the names of the columns it gives, but the emulator behind CloudBurrow does not, and CloudBurrow reads them " +
			"by running the statements before it first, which it does only when none of them changes data (a query, " +
			"DECLARE, SET, and TEMP tables). Nothing was run."
	}
	prefix := q.Query[:c.pos]
	depth := 0
	if toks, ok := lex(prefix); ok {
		for _, stmt := range splitStatements(toks) {
			for i := 0; i < len(stmt); i++ {
				if i+1 < len(stmt) && (stmt[i].kind == tokWord || stmt[i].kind == tokQuoted) && stmt[i+1].punct(":") {
					i++
					continue
				}
				if !stmt[i].is("BEGIN") || i+1 < len(stmt) && (stmt[i+1].is("TRANSACTION") || stmt[i+1].is("TRAN")) {
					break
				}
				depth++
			}
			if len(stmt) > 0 && stmt[0].is("END") && (len(stmt) == 1 || len(stmt) == 2 && stmt[1].kind == tokWord) {
				depth--
			}
		}
	}
	check := prefix + "SELECT * FROM (\n" + c.query + "\n) LIMIT 0"
	for ; depth > 0; depth-- {
		check += ";\nEND"
	}
	check, _ = renameVariables(check)
	legacy := false
	req, err := json.Marshal(queryOptions{Query: check, UseLegacySQL: &legacy, DefaultDataset: q.DefaultDataset,
		ParameterMode: q.ParameterMode, QueryParameters: q.QueryParameters})
	if err != nil {
		return http.StatusInternalServerError, err.Error()
	}
	status, got := f.send(r, http.MethodPost, "/queries", req)
	var res struct {
		Schema tableSchema `json:"schema"`
		Error  struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(got, &res)
	if status != http.StatusOK || len(res.Schema.Fields) == 0 {
		// The script is then run: if it fails, its failure is the
		// answer, as the statements up to this one failed alike; if it
		// does not, the query is failed 501 (StatusAccepted says so).
		why := res.Error.Message
		if why == "" {
			why = fmt.Sprintf("HTTP %d, with no columns", status)
		}
		return http.StatusAccepted, "Not implemented here: CREATE TEMP TABLE " + name + " AS SELECT, whose query names a " +
			"script variable or a table the script makes. BigQuery checks the names of the columns it gives, but the " +
			"emulator behind CloudBurrow does not, and CloudBurrow could not read them by running the statements before " +
			"it and then its query (" + why + "). The script was run, and what it did was kept."
	}
	if msg := checkNames(res.Schema.Fields, "", anonymousColumn, checkColumnName); msg != "" {
		return http.StatusBadRequest, msg + " The name was given by the query of the CREATE TEMP TABLE " + name + " AS SELECT."
	}
	return 0, ""
}
