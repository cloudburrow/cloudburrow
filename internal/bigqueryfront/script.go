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
//     undone; the error says so. A TEMP table is not checked: it is gone
//     when the script ends.
//   - (#932) CREATE TABLE or VIEW ... IF NOT EXISTS of one that exists
//     fails, where BigQuery does nothing: such a statement is replaced by
//     one that does nothing (skipIfExists).
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
	for _, c := range v.selects() {
		msg, ran := f.ctasColumns(r, q, c.query)
		if msg != "" {
			writeError(w, http.StatusBadRequest, "invalidQuery", msg)
			return
		}
		if ran || c.temp {
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
	if text, changed := f.skipIfExists(r, q, v); changed {
		if !setQueryText(r, insert, text) {
			writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query")
			return
		}
	}
	if len(deferred) == 0 && !v.handler {
		f.next.ServeHTTP(w, r)
		return
	}

	rec := newRecorder()
	f.next.ServeHTTP(rec, r)
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
	if failed {
		rec.copyTo(w)
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
//     query, not the client's.
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
	f.next.ServeHTTP(w, r)
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
