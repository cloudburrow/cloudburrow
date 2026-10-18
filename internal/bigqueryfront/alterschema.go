package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ALTER SCHEMA … SET OPTIONS (#1522).
//
// BigQuery: "Sets or updates an option on a dataset"
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language#alter_schema_set_options_statement),
// the options of schema_set_options_list; "IF EXISTS: If no dataset exists
// with that name, the statement has no effect"; labels set "replaces the
// existing labels on the dataset", and an option set to NULL clears it.
// Measured first, through the official Go client: the emulator refused
// every ALTER SCHEMA, 400 "Statement not supported: AlterSchemaStatement".
//
// So the front carries out a lone ALTER SCHEMA [IF EXISTS] dataset SET
// OPTIONS (...) as the datasets.patch of what it sets (datasetupdate.go),
// which, for max_time_travel_hours, lets go of the change history outside
// the window it had, or the shorter it gets, first (#1503,
// changehistoryretention.go), and then sends the query on as a statement
// that does nothing, so the job is the emulator's as any other. One in a
// multi-statement query is carried out so in its turn, the script run a
// statement at a time (#1548, runScript), and so is one in a script with
// a TEMP table or function, in the script's session (#1562,
// tempsession.go); inside a transaction it fails the script as BigQuery's
// transactions do not take it (transactions.go). 501 only where the TEMP
// script cannot be read so. Other
// forms of ALTER SCHEMA (SET DEFAULT COLLATE, ADD or DROP REPLICA) are left
// to the emulator.
func (f front) alterSchema(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool) bool {
	upper := strings.ToUpper(q.Query)
	if !strings.Contains(upper, "ALTER") || !strings.Contains(upper, "SCHEMA") {
		return false
	}
	toks, ok := lex(q.Query)
	if !ok {
		return false
	}
	var alter []token
	n, alters := 0, 0
	for _, stmt := range splitStatements(toks) {
		if len(stmt) == 0 {
			continue
		}
		n++
		body, _, _ := stripControlFlow(stmt)
		if len(body) > 1 && body[0].is("ALTER") && body[1].is("SCHEMA") {
			i := 2
			if i+1 < len(body) && body[i].is("IF") && body[i+1].is("EXISTS") {
				i += 2
			}
			_, i = path(body, i)
			if i+1 < len(body) && body[i].is("SET") && body[i+1].is("OPTIONS") {
				alters++
				alter = body
			}
		}
	}
	if alters == 0 {
		return false
	}
	if n > 1 {
		if !txnRequest(r) && !hasTemp(q.Query) {
			// #1548: carried out a statement at a time (runScript), each
			// ALTER SCHEMA alone, in its turn, as below.
			f.runScript(w, r, q, insert)
			return true
		}
		if !txnRequest(r) && f.runTempScript(w, r, q, insert) {
			// #1562: a script with a TEMP table or function, a statement
			// at a time too, its TEMP tables and functions hidden ones of
			// the script's session (tempsession.go).
			return true
		}
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: ALTER SCHEMA … SET OPTIONS in a "+
			"multi-statement query with a TEMP table or function, or in a transaction, which CloudBurrow cannot run a "+
			"statement at a time (docs/compatibility.md). Nothing was run.")
		return true
	}
	project := projectOf(f.base)
	i := 2
	ifExists := false
	if alter[i].is("IF") && alter[i+1].is("EXISTS") {
		i, ifExists = i+2, true
	}
	p, next := path(alter, i)
	if len(p) == 0 || len(p) > 2 || len(p) == 2 && p[0] != project {
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: ALTER SCHEMA of "+strings.Join(p, ".")+
			": CloudBurrow alters a dataset of the instance's project, named alone or with the project. Nothing was run.")
		return true
	}
	ds := p[len(p)-1]
	// schemaOptions reads CREATE SCHEMA path OPTIONS(...): the same
	// tokens without IF EXISTS and SET.
	synth := append(append(append([]token{}, alter[0:2]...), alter[i:next]...), alter[next+1:]...)
	d, msg, bad := schemaOptions(synth, ds)
	if bad != "" {
		writeError(w, http.StatusBadRequest, "invalidQuery", bad)
		return true
	}
	if msg != "" {
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: ALTER SCHEMA "+ds+" with "+msg+
			". Nothing was run. Change the dataset with datasets.patch (Dataset.Update in the Go client).")
		return true
	}
	status, got := f.get(r, "/datasets/"+url.PathEscape(ds))
	if status == http.StatusNotFound {
		if ifExists {
			return f.alterSchemaDone(w, r, q, insert)
		}
		f.failBeforeRun(w, r, q, insert, "_cloudburrow", http.StatusNotFound,
			rowError{Reason: "notFound", Message: "Not found: Dataset " + project + ":" + ds})
		return true
	}
	var current struct {
		Labels map[string]string `json:"labels"`
	}
	_ = json.Unmarshal(got, &current)
	patch, ok := alterPatch(alter[next+3:], d, current.Labels)
	if !ok {
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: ALTER SCHEMA "+ds+
			" with OPTIONS that CloudBurrow does not read. Nothing was run.")
		return true
	}
	body, err := json.Marshal(patch)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internalError", err.Error())
		return true
	}
	pr := r.Clone(r.Context())
	pr.Method = http.MethodPatch
	pr.Header.Del("If-Match")
	pr.Header.Del("Content-Encoding")
	pr.Body = io.NopCloser(bytes.NewReader(body))
	pr.ContentLength = int64(len(body))
	rec := newRecorder()
	f.updateDataset(rec, pr, ds)
	if rec.status != 0 && rec.status != http.StatusOK {
		reason := "invalidQuery"
		if rec.status != http.StatusBadRequest {
			reason = "backendError"
		}
		f.failBeforeRun(w, r, q, insert, "_cloudburrow", rec.status,
			rowError{Reason: reason, Message: errorMessage(rec.body.Bytes(), rec.status)})
		return true
	}
	return f.alterSchemaDone(w, r, q, insert)
}

// alterSchemaDone sends q on as a statement that does nothing, the
// ALTER SCHEMA carried out (above).
func (f front) alterSchemaDone(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool) bool {
	if !setQueryText(r, insert, "DROP TABLE IF EXISTS "+quotePath([]string{"_cloudburrow", scratchTable()})) {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query")
		return true
	}
	f.forward(w, r, jobText{query: q.Query})
	return true
}

// alterPatch is the datasets.patch body of ALTER SCHEMA's OPTIONS, t from
// OPTIONS' "(": each option given, as d holds it, or null when set to NULL;
// labels replace the dataset's (labels, its current ones, each left out
// removed).
func alterPatch(t []token, d newDataset, labels map[string]string) (map[string]any, bool) {
	var set map[string]json.RawMessage
	b, _ := json.Marshal(d)
	if json.Unmarshal(b, &set) != nil {
		return nil, false
	}
	field := map[string]string{"description": "description", "friendly_name": "friendlyName", "labels": "labels", "location": "location"}
	for k, v := range schemaOptionField {
		field[k], _, _ = strings.Cut(v, ".")
	}
	patch := map[string]any{}
	for i := 0; i < len(t) && !t[i].punct(")"); {
		if t[i].kind != tokWord || i+1 >= len(t) || !t[i+1].punct("=") {
			return nil, false
		}
		name := strings.ToLower(t[i].text)
		end := skipTo(t, i+2, ",", ")")
		val := t[i+2 : end]
		key, ok := field[name]
		if !ok {
			return nil, false
		}
		if v, given := set[key]; given && !(len(val) == 1 && val[0].is("NULL")) {
			patch[key] = v
		} else {
			patch[key] = nil
		}
		if key == "labels" {
			merged := map[string]any{}
			for k := range labels {
				merged[k] = nil
			}
			for k, v := range d.Labels {
				merged[k] = v
			}
			patch[key] = merged
		}
		i = end
		if i < len(t) && t[i].punct(",") {
			i++
		}
	}
	return patch, true
}

// runTempScript carries out q, a script with a TEMP table or function, a
// statement at a time in a session of its own (tempsession.go, #1562), and
// reports whether it did.
func (f front) runTempScript(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool) bool {
	if sessionOf(r) != nil {
		return false
	}
	s, ok := tempSessionScript(q.Query)
	if !ok {
		return false
	}
	f.runTempSession(w, r, q, insert, s)
	return true
}
