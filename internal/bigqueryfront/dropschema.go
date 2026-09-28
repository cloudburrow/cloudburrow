package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// DROP SCHEMA, carried out through datasets.delete (#990).
//
// BigQuery: "DROP SCHEMA [IF EXISTS] name [CASCADE | RESTRICT]" deletes a
// dataset; "CASCADE: Deletes the dataset and all resources within the
// dataset, such as tables, views, and functions"; "RESTRICT: Deletes the
// dataset only if it's empty. Otherwise, returns an error", which is the
// default; with IF EXISTS, a dataset that does not exist is no error
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language#drop_schema_statement).
//
// The emulator refuses every DROP SCHEMA, IF EXISTS of a dataset that
// does not exist too, alone and in a script, through jobs.query and as a
// query job (measured against the pinned image through the official Go
// client: 400 "currently unsupported DROP SCHEMA statement", the dataset
// and its tables kept). Its datasets.delete does delete a dataset
// (measured the same way): without deleteContents it refuses one with a
// table, 400 resourceInUse "Dataset project:ds is still in use"; with it,
// the tables go too, and a dataset made again under the name has none.
// It does not count a dataset's functions, and leaves them callable in
// its engine after the dataset is gone (measured: `ds.f(1)` still read 2),
// until a DROP FUNCTION takes them out.
//
// So the front carries out DROP SCHEMA itself: the statement is sent to the
// emulator as one that does nothing (noopDrops), and after the query
// succeeds the dataset is deleted through datasets.delete, with
// deleteContents for CASCADE, and each function the front knows it to hold
// (knownFunctions: made by a CREATE FUNCTION statement or routines.insert
// the front saw, or by a job from before it started, #1001) is taken out
// with DROP FUNCTION. Before the query is
// sent, the statement is checked as BigQuery would run it:
//
//   - a dataset that does not exist: nothing with IF EXISTS; else it fails,
//     404 notFound "Not found: Dataset project:ds";
//   - RESTRICT (or neither): a dataset with a table, or a function the
//     front knows, fails 400 resourceInUse "Dataset project:ds is still in
//     use", the datasets.delete error;
//   - CASCADE of a dataset with a table function is 501: the engine cannot
//     drop one (#976).
//
// When it fails and it is the query's first statement, the query fails as
// BigQuery fails it, before anything runs (failBeforeRun). In a script,
// the dataset is dropped after the script runs, which BigQuery does when
// the statement is reached: that is the same for the other statements only
// when none of them can see the dataset, so a DROP SCHEMA is 501, and
// nothing is run, when another statement names the dataset (or the query's
// default dataset is it), when it is in an EXCEPTION handler or after a
// RETURN, or when it fails the checks above after other statements (which
// BigQuery would run first). If the script fails, the dataset is kept: a
// script given to jobs.query is rolled back whole by the emulator (and
// answered 501, #935); a failed query job is answered 501 naming the DROP
// SCHEMA.

// dropSchema is a DROP SCHEMA the front carries out after its query.
type dropSchema struct {
	ds      string
	cascade bool
	// funcs are the dataset's functions that exist, to drop after it.
	funcs [][]string
}

// dropContext is where a DROP SCHEMA statement is in its query.
type dropContext struct {
	stmts [][]token // the query's statements
	index int       // this statement's in stmts
	n     int       // its place among the non-empty ones, from 1
	body  []token   // the statement without its control-flow text
	// handler and returned: in an EXCEPTION handler, after a RETURN.
	handler, returned bool
	ds                string
}

// planDropSchema checks a DROP SCHEMA statement (above) and returns what
// to carry out after the query, or answers w and reports true.
func (f front) planDropSchema(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool, c dropContext) (dropSchema, bool) {
	d := dropSchema{ds: c.ds}
	project := projectOf(f.base)
	notImplemented := func(why string) (dropSchema, bool) {
		writeError(w, http.StatusNotImplemented, "notImplemented", fmt.Sprintf("Not implemented here: DROP SCHEMA %s %s. "+
			"Nothing was run. The emulator behind CloudBurrow does not run DROP SCHEMA (measured: 400 \"currently "+
			"unsupported DROP SCHEMA statement\"), so CloudBurrow deletes the dataset itself after the query runs. Run the "+
			"DROP SCHEMA as a query of its own, or delete the dataset with datasets.delete (Dataset.Delete or "+
			"DeleteWithContents in the Go client).", c.ds, why))
		return d, true
	}
	// DROP SCHEMA [IF EXISTS] path [CASCADE | RESTRICT]
	i := 2
	if i+1 < len(c.body) && c.body[i].is("IF") && c.body[i+1].is("EXISTS") {
		i += 2
	}
	_, i = path(c.body, i)
	switch rest := c.body[i:]; {
	case len(rest) == 0 || len(rest) == 1 && rest[0].is("RESTRICT"):
	case len(rest) == 1 && rest[0].is("CASCADE"):
		d.cascade = true
	default:
		return notImplemented(fmt.Sprintf("with %q after the dataset's name, which CloudBurrow does not read", rest[0].text))
	}
	var others [][]token
	for j, s := range c.stmts {
		if j != c.index && len(s) > 0 {
			others = append(others, s)
		}
	}
	if len(others) > 0 {
		var def struct {
			DatasetID string `json:"datasetId"`
		}
		_ = json.Unmarshal(q.DefaultDataset, &def)
		switch {
		case c.handler:
			return notImplemented("in an EXCEPTION handler, which BigQuery runs only when another statement fails")
		case c.returned:
			return notImplemented("after a RETURN, which may end the script before it")
		case namesDataset(others, c.ds) || def.DatasetID == c.ds:
			return notImplemented("in a script whose other statements name the dataset (or use it as the default " +
				"dataset), which would see it before or after BigQuery drops it")
		}
	}
	first := c.n == 1
	failFirst := func(code int, e rowError, why string) (dropSchema, bool) {
		if !first {
			return notImplemented(why)
		}
		f.failBeforeRun(w, r, q, insert, c.ds, code, e)
		return d, true
	}
	status, _ := f.get(r, "/datasets/"+url.PathEscape(c.ds))
	if status == http.StatusNotFound {
		return failFirst(http.StatusNotFound, rowError{Reason: "notFound", Message: "Not found: Dataset " + project + ":" + c.ds},
			"after other statements of the script, of a dataset that does not exist. BigQuery runs the statements before "+
				"it and fails there")
	}
	for _, p := range f.functionsIn(r, project, c.ds) {
		exists, kind := f.functionKind(r, p)
		if !exists {
			continue
		}
		if kind == "table" && d.cascade {
			return notImplemented(fmt.Sprintf("CASCADE of a dataset with the table function %s. BigQuery drops it with "+
				"the dataset, but the emulator's engine cannot drop a table function (\"Statement not supported: "+
				"DropTableFunctionStatement\", measured)", strings.Join(p, ".")))
		}
		d.funcs = append(d.funcs, p)
	}
	inUse := len(d.funcs) > 0
	if !d.cascade && !inUse {
		status, got := f.get(r, "/datasets/"+url.PathEscape(c.ds)+"/tables")
		var list struct {
			Tables []json.RawMessage `json:"tables"`
		}
		inUse = status == http.StatusOK && json.Unmarshal(got, &list) == nil && len(list.Tables) > 0
	}
	if !d.cascade && inUse {
		return failFirst(http.StatusBadRequest, rowError{Reason: "resourceInUse", Message: "Dataset " + project + ":" + c.ds +
			" is still in use"}, "after other statements of the script, of a dataset that is not empty, without CASCADE. "+
			"BigQuery runs the statements before it and fails there")
	}
	return d, false
}

// dropDataset deletes a dataset DROP SCHEMA drops, through
// datasets.delete, and then the functions it held, and returns
// datasets.delete's status and body.
func (f front) dropDataset(r *http.Request, d dropSchema) (int, []byte) {
	p := "/datasets/" + url.PathEscape(d.ds)
	if d.cascade {
		p += "?deleteContents=true"
	}
	status, got := f.send(r, http.MethodDelete, p, nil)
	if status != http.StatusOK && status != http.StatusNoContent {
		return status, got
	}
	for _, fn := range d.funcs {
		f.sendDDL(r, "DROP FUNCTION IF EXISTS "+functionName(fn))
	}
	f.functions.forget(projectOf(f.base), d.ds)
	return status, got
}

// errorMessage returns the message of an error body the emulator wrote,
// or its status.
func errorMessage(body []byte, status int) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	return fmt.Sprintf("HTTP %d", status)
}
