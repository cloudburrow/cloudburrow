package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// The engine's query errors with BigQuery's reasons and messages (#1582).
//
// The engine fails a query it cannot parse or analyze with reason
// jobInternalError and its own text, the GoogleSQL analyzer's message
// after its own prefix (measured through the front, 2026-10-17):
//
//	failed to analyze: Table not found: ds.nope [at 1:15]
//	failed to analyze: Unrecognized name: nope [at 1:8]
//	failed to analyze: INVALID_ARGUMENT: Could not cast literal "x" to type INT64 [at 1:8]
//	failed to parse statements: failed to parse statement: Syntax error: Unexpected identifier "blah" [at 1:1]
//	dataset nods is not found (CREATE TABLE nods.x)
//
// (the syntax error's position is googlesqlite patch 0071's). BigQuery
// answers them so:
//
//   - a table that does not exist: 404 notFound, "Not found: Table
//     [project_id]:[dataset].[table_name] was not found in location
//     [region]" ("Troubleshoot query errors", Table not found,
//     https://cloud.google.com/bigquery/docs/troubleshoot-queries), and
//     of a dataset that does not exist "Not found: Dataset
//     [project_id]:[dataset_id] was not found in location [region]" (its
//     "Location not found"; the official ODBC driver's
//     data_translation_test.cc expects "Not found: Dataset
//     bigquery-devtools-drivers:ODBC_TEST_DATASET was not found in
//     location us-east4"). The official Python client's system tests
//     expect NotFound (404) for SELECT * FROM not_a_real_dataset.doesnt_exist
//     through jobs.query and through a job's result
//     (tests/system/test_query.py test_query_error_w_api_method_query
//     and test_query_error_w_api_method_default), and for a table not made yet (test_job_retry.py). The
//     error table gives notFound "a resource (a dataset, a table, or a
//     job) that doesn't exist"
//     (https://cloud.google.com/bigquery/docs/error-messages).
//   - any other error of the parser or the analyzer: 400 invalidQuery,
//     "you attempt to run an invalid query" (the error table); the
//     official Go client's TestIntegration_QueryErrors expects
//     invalidQuery for the syntax error of "blah blah broken". BigQuery
//     ends the message with the position as " at [line:column]": the
//     official .NET client's DryRunInvalidQuery sample gives "Unrecognized
//     name: playr; Did you mean player? at [1:8]". So the engine's
//     "[at 1:8]" is written "at [1:8]", and its "INVALID_ARGUMENT: " is
//     dropped. Which text BigQuery gives each of the analyzer's other
//     errors ("Syntax error: ...", "Function not found: ...", "No matching
//     signature ...") is UNVERIFIED: the engine's text is kept, as
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/error-messages
//     BigQuery's analyzer is GoogleSQL too.
//
// The engine's errors of a query it runs (#1585) are 400 jobInternalError
// too. BigQuery gives them reason invalidQuery, the error table's for a
// query that cannot run; their messages are GoogleSQL's:
//
//   - a division by zero: googlesqlite patch 0072 writes GoogleSQL's
//     message, "division by zero: 1 / 0" (BigQuery's / is FLOAT64's; DIV
//     and MOD "division by zero: DIV(1, 0)", UNVERIFIED), in place of
//     the engine's "zero divided error ( 1.000000 / 0 )". The operators
//     reference says only "Divide by zero operations return an error."
//   - ERROR(message): its message, as the debugging functions reference
//     shows ("Found unexpected value: baz"); patch 0072 starts it with
//     "ERROR function: " so that it is told from the engine's own errors,
//     and the front takes that off.
//   - a named query parameter the request does not give: "Query
//     parameter 'p' not found at [1:8]", the position the @'s, where the
//     engine fails "not enough query arguments" (UNVERIFIED: no BigQuery
//     document or client test gives the message; it is the form the
//     analyzer's other errors take). A positional parameter's is the
//     engine's text with reason invalidQuery.
//
// The analyzer names a table by its storage name in some messages (a dry
// run of INSERT INTO ds.t (nosuch): "Column nosuch is not present in
// table p_ds_t"); storageNames writes a table of the job's project ds.t,
// as dmlNames does for DML. jobErrors (errorcodes.go) maps the errors of every
// answer of jobs.query, jobs.insert, jobs.get, jobs.list and
// jobs.getQueryResults, so the front's own code, which reads the engine's
// text, is unchanged.

// engineContext is what mapping a job's error needs: the engine at the
// job's project, its location, and its default dataset.
type engineContext struct {
	r        *http.Request
	engine   http.Handler
	prefix   string // "/bigquery/v2" or ""
	project  string
	location string
	// defaultProject and defaultDataset are the query's default dataset.
	defaultProject, defaultDataset string
	settings                       *projectOptions
	datasets                       map[string]bool
	// query and params are the query's text and the names of its named
	// parameters, lower-cased, where the request or the job gives them.
	query  string
	params map[string]bool
}

// errorFunctionPrefix starts the message of ERROR()'s error (googlesqlite
// patch 0072).
const errorFunctionPrefix = "ERROR function: "

// enginePosition is the position the engine ends an error with.
var enginePosition = regexp.MustCompile(` \[at ([0-9]+):([0-9]+)\]$`)

// engineDatasetMissing is the engine's error of a CREATE TABLE in a
// dataset that does not exist.
var engineDatasetMissing = regexp.MustCompile(`^dataset ([A-Za-z0-9_]+) is not found$`)

// engineText is the parser's or the analyzer's message in msg, the
// engine's error, without the engine's prefixes.
func engineText(msg string) (string, bool) {
	const parse, analyze = "failed to parse statement: ", "failed to analyze: "
	if i := strings.LastIndex(msg, parse); i >= 0 {
		return msg[i+len(parse):], true
	}
	if i := strings.Index(msg, analyze); i >= 0 {
		return strings.TrimPrefix(msg[i+len(analyze):], "INVALID_ARGUMENT: "), true
	}
	return "", false
}

// mapError returns e, an error of a query, with BigQuery's reason and
// message when it is one of the engine's (above), and whether it was.
func (c *engineContext) mapError(e rowError) (rowError, bool) {
	if e.Reason != "jobInternalError" && e.Reason != "invalidQuery" {
		return e, false
	}
	if m := engineDatasetMissing.FindStringSubmatch(e.Message); m != nil && e.Reason == "jobInternalError" {
		e.Reason, e.Message = "notFound", "Not found: Dataset "+c.project+":"+m[1]+" was not found in location "+c.jobLocation()
		return e, true
	}
	if e.Reason == "jobInternalError" {
		if msg, ok := c.runtimeError(e.Message); ok {
			e.Reason, e.Message = "invalidQuery", msg
			return e, true
		}
	}
	text, ok := engineText(e.Message)
	if !ok {
		return e, false
	}
	at := ""
	if m := enginePosition.FindStringSubmatchIndex(text); m != nil {
		at = " at [" + text[m[2]:m[3]] + ":" + text[m[4]:m[5]] + "]"
		text = text[:m[0]]
	}
	if name, ok := strings.CutPrefix(text, "Table not found: "); ok {
		name, _, _ = strings.Cut(name, "; Did you mean ")
		if msg, ok := c.tableNotFound(name); ok {
			e.Reason, e.Message = "notFound", msg
			return e, true
		}
	}
	e.Reason, e.Message = "invalidQuery", c.storageNames(text)+at
	return e, true
}

// runtimeError is BigQuery's message of msg, the engine's error of a query
// it ran, when it is one of those above.
func (c *engineContext) runtimeError(msg string) (string, bool) {
	if plain := errorFunctionMessage(msg); plain != msg {
		return plain, true
	}
	if i := strings.Index(msg, "division by zero: "); i >= 0 {
		return msg[i:], true
	}
	if strings.HasSuffix(msg, "not enough query arguments") {
		if name, at, ok := c.missingParameter(); ok {
			return "Query parameter '" + name + "' not found" + at, true
		}
		return msg, true
	}
	return "", false
}

// missingParameter is the first named parameter of the query that the
// request does not give, and its position as " at [line:column]".
func (c *engineContext) missingParameter() (name, at string, ok bool) {
	toks, lexed := lex(c.query)
	if !lexed {
		return "", "", false
	}
	for i := 0; i+1 < len(toks); i++ {
		t, next := toks[i], toks[i+1]
		if !t.punct("@") || next.kind != tokWord || next.pos != t.end {
			continue
		}
		if i > 0 && toks[i-1].punct("@") && toks[i-1].end == t.pos {
			continue // @@system_variable
		}
		if c.params[strings.ToLower(next.text)] {
			continue
		}
		line, col := lineColumn(c.query, t.pos)
		return next.text, " at [" + strconv.Itoa(line) + ":" + strconv.Itoa(col) + "]", true
	}
	return "", "", false
}

// storageNames writes each storage name of a table of the job's project
// in text, <project>_<dataset>_<table>, as dataset.table: the dataset is
// the longest prefix of the rest, at an underscore, that the engine has.
func (c *engineContext) storageNames(text string) string {
	if c.project == "" || !strings.Contains(text, c.project+"_") {
		return text
	}
	word := func(b byte) bool {
		return b == '_' || b == '-' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
	}
	var out strings.Builder
	rest := text
	for {
		i := strings.Index(rest, c.project+"_")
		if i < 0 {
			out.WriteString(rest)
			return out.String()
		}
		if i > 0 && word(rest[i-1]) {
			out.WriteString(rest[:i+1])
			rest = rest[i+1:]
			continue
		}
		start := i + len(c.project) + 1
		end := start
		for end < len(rest) && (word(rest[end]) && rest[end] != '-') {
			end++
		}
		name := rest[start:end]
		replaced := ""
		for k := strings.LastIndex(name, "_"); k > 0; k = strings.LastIndex(name[:k], "_") {
			if k+1 < len(name) && c.datasetExists(c.project, name[:k]) {
				replaced = name[:k] + "." + name[k+1:]
				break
			}
		}
		out.WriteString(rest[:i])
		if replaced == "" {
			out.WriteString(rest[i:end])
		} else {
			out.WriteString(replaced)
		}
		rest = rest[end:]
	}
}

// tableNotFound is BigQuery's message of name, a table the engine did not
// find: the table's, or its dataset's when that does not exist either.
func (c *engineContext) tableNotFound(name string) (string, bool) {
	path := namePath(name)
	project, dataset, table := c.project, "", ""
	switch len(path) {
	case 3:
		project, dataset, table = path[0], path[1], path[2]
	case 2:
		dataset, table = path[0], path[1]
	case 1:
		if c.defaultDataset == "" {
			return "", false
		}
		project, dataset, table = c.defaultProject, c.defaultDataset, path[0]
		if project == "" {
			project = c.project
		}
	default:
		return "", false
	}
	if !c.datasetExists(project, dataset) {
		return "Not found: Dataset " + project + ":" + dataset + " was not found in location " + c.jobLocation(), true
	}
	return "Not found: Table " + project + ":" + dataset + "." + table + " was not found in location " + c.jobLocation(), true
}

// namePath splits a table's name as the analyzer writes it (ds.t,
// `project.ds.t`, `project`.ds.t) into its parts, or nil.
func namePath(name string) []string {
	parts := strings.Split(strings.ReplaceAll(name, "`", ""), ".")
	for _, p := range parts {
		if p == "" || strings.ContainsAny(p, " \n") {
			return nil
		}
	}
	return parts
}

// datasetExists reports whether the engine has project's dataset; one it
// cannot read is taken to exist.
func (c *engineContext) datasetExists(project, dataset string) bool {
	key := project + ":" + dataset
	if have, ok := c.datasets[key]; ok {
		return have
	}
	f := front{next: c.engine, base: c.prefix + "/projects/" + url.PathEscape(project)}
	st, _ := f.get(c.r, "/datasets/"+url.PathEscape(dataset))
	if c.datasets == nil {
		c.datasets = map[string]bool{}
	}
	c.datasets[key] = st != http.StatusNotFound
	return c.datasets[key]
}

// jobLocation is the job's location, else the project's default.
func (c *engineContext) jobLocation() string {
	if c.location != "" {
		return c.location
	}
	if c.settings != nil {
		return c.settings.defaultLocation(c.project)
	}
	return defaultLocation
}

// fromRequest reads the location and default dataset of a jobs.query
// (query) or jobs.insert request body b.
func (c *engineContext) fromRequest(b []byte, query bool) {
	var body map[string]any
	if json.Unmarshal(b, &body) != nil {
		return
	}
	if query {
		c.location, _ = body["location"].(string)
		c.fromQueryConf(body)
		return
	}
	c.fromJob(body)
}

// fromJob reads the location and default dataset of job, a job resource
// (or a jobs.insert body), where it gives them.
func (c *engineContext) fromJob(job map[string]any) {
	ref, _ := job["jobReference"].(map[string]any)
	if loc, _ := ref["location"].(string); loc != "" {
		c.location = loc
	}
	conf, _ := job["configuration"].(map[string]any)
	q, _ := conf["query"].(map[string]any)
	c.fromQueryConf(q)
}

func (c *engineContext) fromQueryConf(q map[string]any) {
	if query, _ := q["query"].(string); query != "" {
		c.query = query
		c.params = map[string]bool{}
		params, _ := q["queryParameters"].([]any)
		for _, v := range params {
			p, _ := v.(map[string]any)
			if name, _ := p["name"].(string); name != "" {
				c.params[strings.ToLower(name)] = true
			}
		}
	}
	dd, _ := q["defaultDataset"].(map[string]any)
	if ds, _ := dd["datasetId"].(string); ds != "" {
		c.defaultDataset = ds
		c.defaultProject, _ = dd["projectId"].(string)
	}
}

// mapErrorAnswer maps the errors of rec, an error answer, and reports
// whether any changed; the status is left for recodeError. An error
// answered with a job, DONE with its errorResult (a script's statement
// that failed through jobs.query, measured: 400 with the engine's job,
// which the Go client cannot read as an error), is answered as its
// errorResult.
func (c *engineContext) mapErrorAnswer(rec *recorder) bool {
	var body map[string]any
	dec := json.NewDecoder(bytes.NewReader(rec.body.Bytes()))
	dec.UseNumber()
	if dec.Decode(&body) != nil {
		return false
	}
	if status, _ := body["status"].(map[string]any); body["error"] == nil && status != nil {
		er, _ := status["errorResult"].(map[string]any)
		if er == nil {
			return false
		}
		jc := *c
		jc.fromJob(body)
		jc.mapErrorList([]any{er})
		c.datasets = jc.datasets
		errs := []any{er}
		if more, _ := status["errors"].([]any); len(more) > 1 {
			jc.mapErrorList(more)
			errs = more
		}
		code := rec.status
		body = map[string]any{"error": map[string]any{"code": code, "message": er["message"], "errors": errs, "status": grpcStatus[code]}}
		return rewrite(rec, body)
	}
	e, _ := body["error"].(map[string]any)
	errs, _ := e["errors"].([]any)
	if !c.mapErrorList(errs) {
		return false
	}
	if first, _ := errs[0].(map[string]any); first != nil {
		e["message"] = first["message"]
	}
	return rewrite(rec, body)
}

// mapJobs maps the errors of every job resource of rec, an answer of
// jobs.insert, jobs.get or jobs.list, and reports whether any changed.
func (c *engineContext) mapJobs(rec *recorder) bool {
	var body map[string]any
	dec := json.NewDecoder(bytes.NewReader(rec.body.Bytes()))
	dec.UseNumber()
	if dec.Decode(&body) != nil || body == nil {
		return false
	}
	changed := false
	mapJob := func(v any) {
		job, _ := v.(map[string]any)
		status, _ := job["status"].(map[string]any)
		if status == nil {
			return
		}
		jc := *c
		jc.fromJob(job)
		if er, _ := status["errorResult"].(map[string]any); er != nil && jc.mapErrorList([]any{er}) {
			changed = true
		}
		if errs, _ := status["errors"].([]any); jc.mapErrorList(errs) {
			changed = true
		}
		c.datasets = jc.datasets
	}
	mapJob(body)
	if jobs, _ := body["jobs"].([]any); jobs != nil {
		for _, j := range jobs {
			mapJob(j)
		}
	}
	return changed && rewrite(rec, body)
}

// mapErrorList maps each ErrorProto of errs in place.
func (c *engineContext) mapErrorList(errs []any) bool {
	changed := false
	for _, v := range errs {
		ep, _ := v.(map[string]any)
		if ep == nil {
			continue
		}
		reason, _ := ep["reason"].(string)
		msg, _ := ep["message"].(string)
		if mapped, ok := c.mapError(rowError{Reason: reason, Message: msg}); ok {
			ep["reason"], ep["message"] = mapped.Reason, mapped.Message
			changed = true
		}
	}
	return changed
}

// rewrite puts body in rec.
func rewrite(rec *recorder, body any) bool {
	b, err := json.Marshal(body)
	if err != nil {
		return false
	}
	rec.body.Reset()
	rec.body.Write(b)
	rec.header.Del("Content-Length")
	return true
}

// needsJob reports whether rec, an error answer of jobs.getQueryResults,
// is an engine error whose message names the job's location or default
// dataset, or a parameter of its query, which only the job gives.
func needsJob(rec *recorder) bool {
	var e struct {
		Error struct {
			Errors []rowError `json:"errors"`
		} `json:"error"`
	}
	if json.Unmarshal(rec.body.Bytes(), &e) != nil || len(e.Error.Errors) == 0 {
		return false
	}
	first := e.Error.Errors[0]
	return first.Reason == "jobInternalError" &&
		(strings.Contains(first.Message, "Table not found: ") || engineDatasetMissing.MatchString(first.Message) ||
			strings.HasSuffix(first.Message, "not enough query arguments"))
}

// fromJobOf reads the location and default dataset of the job of m, a
// jobs.getQueryResults route's match, from its jobs.get through next.
func (c *engineContext) fromJobOf(next http.Handler, m []string) {
	u := *c.r.URL
	u.Path, u.RawPath = "", m[1]+"/projects/"+m[2]+"/jobs/"+m[4]
	if p, err := url.PathUnescape(u.RawPath); err == nil {
		u.Path = p
	}
	q := url.Values{}
	if loc := c.r.URL.Query().Get("location"); loc != "" {
		q.Set("location", loc)
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(c.r.Context(), http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return
	}
	req.Header = c.r.Header.Clone()
	req.Host, req.RemoteAddr = c.r.Host, c.r.RemoteAddr
	rec := newRecorder()
	next.ServeHTTP(rec, req)
	var job map[string]any
	if rec.status == http.StatusOK && json.Unmarshal(rec.body.Bytes(), &job) == nil {
		c.fromJob(job)
	}
}
