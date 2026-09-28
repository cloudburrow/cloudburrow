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
// SCHEMA of one that does not exist, measured the same way, #951, below.)
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

// CREATE SCHEMA of a dataset that does not exist (#951).
//
// The emulator runs it as done and makes no dataset (measured against the
// pinned image through the official Go client, jobs.query and jobs.insert
// alike: datasets.get of it then answered 404, and a CREATE TABLE in it
// failed "dataset ... is not found"), so a script that makes a dataset and
// then tables in it failed later, at the first table. Its datasets.insert
// does make one, and keeps a description, friendlyName, labels and
// location given with it (measured the same way: datasets.get read each
// back); and CREATE SCHEMA of a dataset that exists, run after it, is
// again reported done and changes nothing (measured, #946), and a CREATE
// TABLE in that dataset later in the same script is then run.
//
// So the front carries out such a statement through datasets.insert, when
// the query is sent on to the emulator, and then sends the query on
// unchanged: the emulator runs the CREATE SCHEMA as done, and the rest of
// a script finds the dataset. The dataset is made with the statement's
// OPTIONS that name what the dataset only records: description,
// friendly_name, labels and location; with no location option, in the
// query's location (jobs.query's location, a query job's
// jobReference.location), as BigQuery makes it "in the location you
// specify in the query settings". Any other option, and DEFAULT COLLATE,
// is 501 naming it: each changes how the dataset's tables behave, which
// the emulator does not do. Nothing is run.
//
// Inside a script of several statements the dataset is made before the
// script runs, which BigQuery does when the statement is reached. That is
// the same for the script's other statements when none of them can see it
// before then, so the front makes it only then and answers 501 otherwise:
// when an earlier statement names the dataset, when a DROP SCHEMA of it
// comes earlier (the emulator would drop the dataset and then make none),
// when an earlier RETURN may end the script first, or when the statement
// is in an EXCEPTION handler (run only when another statement fails). A
// statement inside IF, LOOP and the other control-flow blocks is already
// 501 (checkDDL). When the query fails, the datasets the front made are
// deleted again: the emulator runs a script in one transaction, and a
// failed one keeps nothing of its other statements either (measured,
// serveQuery).

// createSchema answers a query whose CREATE SCHEMA the emulator would not
// carry out as BigQuery does (above, and #946), and reports whether it
// did. When it did not, it returns the handler to send the query on
// through: f.next, or one that first makes the datasets the query's
// CREATE SCHEMA statements create.
func (f front) createSchema(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool) (http.Handler, bool) {
	if !strings.Contains(strings.ToUpper(q.Query), "SCHEMA") {
		return f.next, false
	}
	toks, ok := lex(q.Query)
	if !ok {
		return f.next, false
	}
	project := projectOf(f.base)
	made, dropped, noop := map[string]bool{}, map[string]bool{}, map[string]bool{}
	var creates []newDataset
	var before [][]token
	returned := false
	n := 0
	for _, stmt := range splitStatements(toks) {
		if len(stmt) == 0 {
			continue
		}
		n++
		earlier := before
		before = append(before, stmt)
		body, _, handler := stripControlFlow(stmt)
		if len(body) > 0 && body[0].is("RETURN") {
			returned = true
		}
		ds, ifNotExists, drop, ok := schemaStatement(body, project)
		if !ok {
			continue
		}
		if drop {
			// DROP SCHEMA IF EXISTS of a dataset that is not there does
			// nothing; the front then leaves it out when it makes the
			// dataset before the script runs (noopDrop).
			if !made[ds] && !dropped[ds] && len(body) > 3 && body[2].is("IF") && body[3].is("EXISTS") {
				if status, _ := f.get(r, "/datasets/"+url.PathEscape(ds)); status == http.StatusNotFound {
					noop[ds] = true
					before = before[:len(before)-1]
					continue
				}
			}
			delete(made, ds)
			dropped[ds] = true
			continue
		}
		exists := made[ds]
		if !exists && !dropped[ds] {
			status, _ := f.get(r, "/datasets/"+url.PathEscape(ds))
			exists = status == http.StatusOK
		}
		if !exists {
			why, what := "", ", which does not exist,"
			switch {
			case dropped[ds]:
				what = ""
				why = "after a DROP SCHEMA of it that may remove it, in the same script. BigQuery makes the dataset again, but the " +
					"emulator behind CloudBurrow runs CREATE SCHEMA as done without making a dataset (measured), and " +
					"CloudBurrow makes one only before the script runs, when the DROP SCHEMA would remove it"
			case handler:
				why = "in an EXCEPTION handler. BigQuery makes the dataset only if the handler runs, but the emulator " +
					"behind CloudBurrow makes no dataset for CREATE SCHEMA (measured), and CloudBurrow makes one only " +
					"before the script runs, without knowing whether the handler will"
			case returned:
				why = "after a RETURN. BigQuery makes the dataset only if the script gets that far, but the emulator " +
					"behind CloudBurrow makes no dataset for CREATE SCHEMA (measured), and CloudBurrow makes one only " +
					"before the script runs"
			case namesDataset(earlier, ds):
				why = "after other statements of the script that name it. BigQuery makes the dataset only when the " +
					"CREATE SCHEMA runs, but the emulator behind CloudBurrow makes no dataset for CREATE SCHEMA " +
					"(measured), and CloudBurrow makes one before the script runs, which those statements would see"
			}
			if why != "" {
				writeError(w, http.StatusNotImplemented, "notImplemented", fmt.Sprintf("Not implemented here: CREATE "+
					"SCHEMA %s%s %s. Nothing was run. Run the CREATE SCHEMA as a query of its own first.", ds, what, why))
				return nil, true
			}
			d, msg := schemaOptions(body, ds)
			if msg != "" {
				writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: CREATE SCHEMA "+ds+
					" with "+msg+". Nothing was run. Make the dataset with CREATE SCHEMA and set it with "+
					"datasets.update or datasets.patch (Dataset.Update in the Go client) if the emulator applies it.")
				return nil, true
			}
			made[ds] = true
			d.noopDrop = noop[ds]
			creates = append(creates, d)
			continue
		}
		made[ds] = true
		if ifNotExists {
			continue
		}
		if n > 1 || len(body) != len(stmt) {
			writeError(w, http.StatusNotImplemented, "notImplemented", fmt.Sprintf("Not implemented here: CREATE SCHEMA "+
				"%s, which exists, after other statements of a script. BigQuery runs the statements before it and fails "+
				"there (\"Already Exists\"), but the emulator behind CloudBurrow runs CREATE SCHEMA of an existing dataset "+
				"as done (measured), and CloudBurrow cannot stop the script in between. Nothing was run. Use CREATE SCHEMA "+
				"IF NOT EXISTS, or run the CREATE SCHEMA first.", ds))
			return nil, true
		}
		f.existingSchema(w, r, q, insert, ds)
		return nil, true
	}
	if len(creates) == 0 {
		return f.next, false
	}
	location := queryLocation(r, insert)
	for i := range creates {
		if creates[i].Location == "" {
			creates[i].Location = location
		}
		creates[i].DatasetReference.ProjectID = project
	}
	return f.makingDatasets(r, q.Query, insert, creates), false
}

// existingSchema answers a lone CREATE SCHEMA of ds, which exists, as
// BigQuery fails it (#946).
func (f front) existingSchema(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool, ds string) {
	e := rowError{Reason: "duplicate", Message: "Already Exists: Dataset " + projectOf(f.base) + ":" + ds}
	if !insert {
		writeError(w, http.StatusConflict, e.Reason, e.Message)
		return
	}
	scratch := quotePath([]string{ds, scratchTable()})
	if !setQueryText(r, true, "DROP TABLE IF EXISTS "+scratch) {
		writeError(w, http.StatusConflict, e.Reason, e.Message)
		return
	}
	rec := newRecorder()
	f.next.ServeHTTP(rec, r)
	var job map[string]any
	if rec.status != http.StatusOK || json.Unmarshal(rec.body.Bytes(), &job) != nil {
		writeError(w, http.StatusConflict, e.Reason, e.Message)
		return
	}
	if conf, ok := job["configuration"].(map[string]any); ok {
		if qc, ok := conf["query"].(map[string]any); ok {
			qc["query"] = q.Query
		}
	}
	f.fail(w, rec, job, e)
}

// makingDatasets returns f.next, making the datasets in sets first when
// the query req, whose text is query, is sent through it, and deleting
// them again when the query fails. A DROP SCHEMA IF EXISTS of one of
// them before its CREATE SCHEMA, which does nothing in BigQuery, is sent
// as a statement that does nothing, so it does not drop the dataset just
// made (noopDrop). Every other request passes through as it is.
func (f front) makingDatasets(req *http.Request, query string, insert bool, sets []newDataset) http.Handler {
	next := f.next
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r != req {
			next.ServeHTTP(w, r)
			return
		}
		rewritten := false
		if text, ok := currentQuery(r, insert); ok {
			if out, changed := noopDrops(text, projectOf(f.base), sets); changed {
				if !setQueryText(r, insert, out) {
					writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query")
					return
				}
				rewritten = true
			}
		}
		var madeNow []string
		undo := func() {
			for _, id := range madeNow {
				f.send(r, http.MethodDelete, "/datasets/"+url.PathEscape(id), nil)
			}
		}
		for _, d := range sets {
			b, err := json.Marshal(d)
			if err != nil {
				undo()
				writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: "+err.Error())
				return
			}
			status, got := f.send(r, http.MethodPost, "/datasets", b)
			if status != http.StatusOK {
				undo()
				writeRaw(w, status, got)
				return
			}
			madeNow = append(madeNow, d.DatasetReference.DatasetID)
		}
		rec := newRecorder()
		next.ServeHTTP(rec, r)
		var job map[string]any
		if insert && rec.status == http.StatusOK {
			_ = json.Unmarshal(rec.body.Bytes(), &job)
		}
		if _, failed := queryFailure(rec, job); failed {
			undo()
		}
		if rewritten && job != nil {
			if conf, ok := job["configuration"].(map[string]any); ok {
				if qc, ok := conf["query"].(map[string]any); ok {
					qc["query"] = query
					writeJSON(w, rec.status, job)
					return
				}
			}
		}
		rec.copyTo(w)
	})
}

// noopDrops returns text with each DROP SCHEMA IF EXISTS of a dataset of
// sets whose noopDrop is set, before the dataset's first CREATE SCHEMA,
// replaced by DROP TABLE IF EXISTS of a table that is not in it, which
// the emulator runs as a statement that does nothing (measured, #932).
func noopDrops(text, project string, sets []newDataset) (string, bool) {
	pending := map[string]bool{}
	for _, d := range sets {
		if d.noopDrop {
			pending[d.DatasetReference.DatasetID] = true
		}
	}
	if len(pending) == 0 {
		return text, false
	}
	toks, ok := lex(text)
	if !ok {
		return text, false
	}
	type edit struct {
		pos, end int
		with     string
	}
	var edits []edit
	for _, stmt := range splitStatements(toks) {
		body, _, _ := stripControlFlow(stmt)
		ds, _, drop, ok := schemaStatement(body, project)
		if !ok || !pending[ds] {
			continue
		}
		if !drop {
			delete(pending, ds)
			continue
		}
		edits = append(edits, edit{body[0].pos, body[len(body)-1].end, "DROP TABLE IF EXISTS " + quotePath([]string{ds, scratchTable()})})
	}
	for i := len(edits) - 1; i >= 0; i-- {
		e := edits[i]
		text = text[:e.pos] + e.with + text[e.end:]
	}
	return text, len(edits) > 0
}

// currentQuery returns the query text r's body holds now.
func currentQuery(r *http.Request, insert bool) (string, bool) {
	b, err := readBody(r)
	if err != nil {
		return "", false
	}
	var body struct {
		Query         string `json:"query"`
		Configuration struct {
			Query struct {
				Query string `json:"query"`
			} `json:"query"`
		} `json:"configuration"`
	}
	if json.Unmarshal(b, &body) != nil {
		return "", false
	}
	if insert {
		return body.Configuration.Query.Query, true
	}
	return body.Query, true
}

// newDataset is the datasets.insert body of a dataset a CREATE SCHEMA
// makes.
// https://cloud.google.com/bigquery/docs/reference/rest/v2/datasets#Dataset
type newDataset struct {
	DatasetReference struct {
		ProjectID string `json:"projectId"`
		DatasetID string `json:"datasetId"`
	} `json:"datasetReference"`
	Description  string            `json:"description,omitempty"`
	FriendlyName string            `json:"friendlyName,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	Location     string            `json:"location,omitempty"`
	// noopDrop is whether a DROP SCHEMA IF EXISTS of the dataset, which
	// does not exist, comes before its CREATE SCHEMA in the script.
	noopDrop bool
}

// schemaOptions reads what follows the dataset in a CREATE SCHEMA
// statement: OPTIONS(name = value, ...) with the options newDataset holds.
// msg names what the front does not carry out, if any.
// https://cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language#schema_option_list
func schemaOptions(t []token, ds string) (d newDataset, msg string) {
	d.DatasetReference.DatasetID = ds
	i := 2
	if i+2 < len(t) && t[i].is("IF") && t[i+1].is("NOT") && t[i+2].is("EXISTS") {
		i += 3
	}
	_, i = path(t, i)
	if i >= len(t) {
		return d, ""
	}
	if t[i].is("DEFAULT") {
		return d, "DEFAULT COLLATE, which sets how the dataset's tables compare strings"
	}
	if !t[i].is("OPTIONS") || i+1 >= len(t) || !t[i+1].punct("(") {
		return d, fmt.Sprintf("%q after the dataset's name, which CloudBurrow does not read", t[i].text)
	}
	i += 2
	for i < len(t) && !t[i].punct(")") {
		if t[i].kind != tokWord || i+1 >= len(t) || !t[i+1].punct("=") {
			return d, "OPTIONS that CloudBurrow does not read"
		}
		name := strings.ToLower(t[i].text)
		i += 2
		end := skipTo(t, i, ",", ")")
		val := t[i:end]
		isNull := len(val) == 1 && val[0].is("NULL")
		switch name {
		case "description", "friendly_name", "location":
			s, ok := "", isNull
			if !isNull && len(val) == 1 {
				s, ok = stringLiteral(val[0])
			}
			if !ok {
				return d, fmt.Sprintf("the option %s set to other than a string literal, which CloudBurrow does not read", name)
			}
			switch name {
			case "description":
				d.Description = s
			case "friendly_name":
				d.FriendlyName = s
			default:
				d.Location = s
			}
		case "labels":
			if isNull {
				break
			}
			labels, ok := labelsLiteral(val)
			if !ok {
				return d, "the option labels set to other than an array of (key, value) string literals, which " +
					"CloudBurrow does not read"
			}
			d.Labels = labels
		default:
			return d, fmt.Sprintf("the option %s. CloudBurrow makes the dataset with description, friendly_name, labels "+
				"and location, which the emulator behind it records (measured); %s changes how the dataset's tables "+
				"behave, which the emulator does not carry out", name, name)
		}
		i = end
		if i < len(t) && t[i].punct(",") {
			i++
		}
	}
	return d, ""
}

// labelsLiteral reads [("key", "value"), ...].
func labelsLiteral(t []token) (map[string]string, bool) {
	if len(t) < 2 || !t[0].punct("[") || !t[len(t)-1].punct("]") {
		return nil, false
	}
	labels := map[string]string{}
	t = t[1 : len(t)-1]
	for len(t) > 0 {
		if t[0].is("STRUCT") {
			t = t[1:]
		}
		if len(t) < 5 || !t[0].punct("(") || !t[2].punct(",") || !t[4].punct(")") {
			return nil, false
		}
		k, ok1 := stringLiteral(t[1])
		v, ok2 := stringLiteral(t[3])
		if !ok1 || !ok2 {
			return nil, false
		}
		labels[k] = v
		t = t[5:]
		if len(t) > 0 {
			if !t[0].punct(",") {
				return nil, false
			}
			t = t[1:]
		}
	}
	return labels, true
}

// stringLiteral returns the value of a string literal token: quoted with '
// or ", triple-quoted or not, raw (r'...') or not; ok is false for a bytes
// literal, another token, or an escape it does not read.
// https://cloud.google.com/bigquery/docs/reference/standard-sql/lexical#string_and_bytes_literals
func stringLiteral(t token) (string, bool) {
	if t.kind != tokString {
		return "", false
	}
	s, raw := t.text, false
	for len(s) > 0 && s[0] != '\'' && s[0] != '"' {
		switch s[0] {
		case 'r', 'R':
			raw = true
		default:
			return "", false // b'...': bytes
		}
		s = s[1:]
	}
	q := s[:1]
	if strings.HasPrefix(s, q+q+q) && len(s) >= 6 {
		q = q + q + q
	}
	if len(s) < 2*len(q) {
		return "", false
	}
	s = s[len(q) : len(s)-len(q)]
	if raw {
		return s, true
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			continue
		}
		i++
		if i >= len(s) {
			return "", false
		}
		switch c := s[i]; c {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case '\\', '\'', '"', '`', '?':
			b.WriteByte(c)
		default:
			return "", false
		}
	}
	return b.String(), true
}

// namesDataset reports whether any of stmts has ds as a name or a part of
// one.
func namesDataset(stmts [][]token, ds string) bool {
	for _, stmt := range stmts {
		for _, t := range stmt {
			switch t.kind {
			case tokWord:
				if strings.EqualFold(t.text, ds) {
					return true
				}
			case tokQuoted:
				for _, p := range strings.Split(t.text, ".") {
					if strings.EqualFold(p, ds) {
						return true
					}
				}
			}
		}
	}
	return false
}

// queryLocation returns the location a query is run in, which r's body
// gives: jobs.query's location, or with insert, a query job's
// jobReference.location; or "".
func queryLocation(r *http.Request, insert bool) string {
	b, err := readBody(r)
	if err != nil {
		return ""
	}
	var body struct {
		Location     string `json:"location"`
		JobReference struct {
			Location string `json:"location"`
		} `json:"jobReference"`
	}
	if json.Unmarshal(b, &body) != nil {
		return ""
	}
	if insert {
		return body.JobReference.Location
	}
	return body.Location
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
