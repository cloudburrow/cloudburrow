package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Functions a failed script made or dropped (#976).
//
// The emulator's SQL engine adds a function to its in-memory catalog as
// CREATE FUNCTION runs, and takes it out as DROP FUNCTION runs, as it does
// a table (resyncCatalog), and a script that then fails in jobs.query is
// rolled back without putting the catalog back. Measured against the
// pinned image through the official Go client:
//
//   - Rolled back (jobs.query): after `CREATE FUNCTION ds.f(x INT64) AS (x
//     + 1); SELECT * FROM nope.nope`, `SELECT ds.f(1)` read 2, and so for
//     CREATE TABLE FUNCTION; after `DROP FUNCTION ds.g; SELECT * FROM
//     nope.nope`, `SELECT ds.g(1)` failed "Function not found". The front
//     answers such a script 501, saying nothing of it was kept.
//   - Committed (a query job): the function was made, or dropped, as
//     BigQuery keeps what the statements before the failing one did.
//
// So, for a script of several statements sent to jobs.query: before it
// runs, a CREATE [OR REPLACE] FUNCTION's function is looked up
// (functionExists), and after the script fails, each one that did not
// exist is taken out of the catalog again, as a table is (uncatalog: a
// script that drops it IF EXISTS and then fails, which jobs.query rolls
// back); a table function is taken out with DROP TABLE FUNCTION, which the
// engine CloudBurrow builds carries out since #1061 (the pinned v0.8.1
// refused it: 400 "Statement not supported: DropTableFunctionStatement",
// measured). A DROP [TABLE] FUNCTION in such a script is 501 before it
// runs: the emulator has no routines.get, so the front cannot read the
// function to make it again. A TEMP function is gone with its script, and
// a query job is committed as BigQuery keeps it: neither is changed.
//
// DROP SCHEMA is not put back because the emulator never runs it: it
// refused it alone and in a script, through jobs.query and as a query job
// (measured: 400 "currently unsupported DROP SCHEMA statement", and the
// dataset and its tables stayed); since #990 the front carries it out
// through datasets.delete after the query succeeds, and keeps the dataset
// when it fails (dropschema.go).

// funcStmt is a CREATE or DROP of a function in a script.
type funcStmt struct {
	pos         int
	path        []string
	drop, table bool
	temp        bool
	// replace and ifNotExists are whether a CREATE is CREATE OR REPLACE
	// or CREATE ... IF NOT EXISTS (#986).
	replace, ifNotExists bool
	// pathPos and pathEnd are the offsets of the path in the query, and
	// end the statement's end; index is the statement's place in the
	// script, from 0 (#986).
	pathPos, pathEnd, end int
	index                 int
	// oldTable is, for a CREATE OR REPLACE of a function that exists,
	// whether the one it replaces is a table function (#1061).
	oldTable bool
}

// dropFunction is the statement that drops a function, or a table function
// when table: the engine CloudBurrow builds carries out DROP TABLE
// FUNCTION since #1061 (third_party/bigquery-emulator).
func dropFunction(table bool) string {
	if table {
		return "DROP TABLE FUNCTION"
	}
	return "DROP FUNCTION"
}

// functionStatement reads a CREATE [OR REPLACE] [TEMP] [AGGREGATE|TABLE]
// FUNCTION [IF NOT EXISTS] or DROP [TABLE] FUNCTION [IF EXISTS] statement.
func functionStatement(t []token) (funcStmt, bool) {
	var s funcStmt
	if len(t) < 3 {
		return s, false
	}
	i := 1
	switch {
	case t[0].is("CREATE"):
		if i+1 < len(t) && t[i].is("OR") && t[i+1].is("REPLACE") {
			s.replace = true
			i += 2
		}
		if i < len(t) && (t[i].is("TEMP") || t[i].is("TEMPORARY")) {
			s.temp = true
			i++
		}
		if i < len(t) && (t[i].is("AGGREGATE") || t[i].is("TABLE")) {
			s.table = t[i].is("TABLE")
			i++
		}
		if i >= len(t) || !t[i].is("FUNCTION") {
			return s, false
		}
		i++
		if i+2 < len(t) && t[i].is("IF") && t[i+1].is("NOT") && t[i+2].is("EXISTS") {
			s.ifNotExists = true
			i += 3
		}
	case t[0].is("DROP"):
		s.drop = true
		if i < len(t) && t[i].is("TABLE") {
			s.table = true
			i++
		}
		if i >= len(t) || !t[i].is("FUNCTION") {
			return s, false
		}
		i++
		if i+1 < len(t) && t[i].is("IF") && t[i+1].is("EXISTS") {
			i += 2
		}
	default:
		return s, false
	}
	parts, next := path(t, i)
	if len(parts) == 0 {
		return s, false
	}
	s.pos, s.path = t[0].pos, parts
	s.pathPos, s.pathEnd, s.end = t[i].pos, t[next-1].end, t[len(t)-1].end
	return s, true
}

// functionPath is a function's path with its dataset, from the query's
// default dataset when the statement names none.
func functionPath(q queryOptions, p []string) ([]string, bool) {
	ds, name, ok := tableOf(q, p)
	if !ok {
		return nil, false
	}
	if len(p) >= 3 {
		return p[len(p)-3:], true
	}
	return []string{ds, name}, true
}

// functionExists reports whether the engine has a function, by calling it
// with no arguments: a function it does not have fails "Function not
// found" (measured), one it has "No matching signature" or runs. A
// function that cannot be told is taken to exist, so it is left as it is.
func (f front) functionExists(r *http.Request, p []string) bool {
	legacy := false
	body, err := json.Marshal(queryOptions{Query: "SELECT " + functionName(p) + "()", UseLegacySQL: &legacy})
	if err != nil {
		return true
	}
	status, got := f.send(r, http.MethodPost, "/queries", body)
	return status == http.StatusOK || !strings.Contains(string(got), "Function not found")
}

// scriptFunctions checks a script of several statements given to
// jobs.query for the functions it makes or drops (above). It returns the
// functions to take out of the catalog again when the script fails, or
// the 501 to answer.
func (f front) scriptFunctions(r *http.Request, q queryOptions, v ddlVerdict) (made []funcStmt, msg string) {
	for _, s := range v.funcs {
		if s.temp {
			continue
		}
		full, ok := functionPath(q, s.path)
		if !ok {
			continue
		}
		if s.drop {
			what := "FUNCTION"
			if s.table {
				what = "TABLE FUNCTION"
			}
			return nil, fmt.Sprintf("Not implemented here: DROP %s %s inside a script of several statements given to "+
				"jobs.query. If the script fails, the emulator behind CloudBurrow rolls it back but leaves the function "+
				"dropped in its SQL engine (measured: it was then \"not found\"), and it has no routines.get for "+
				"CloudBurrow to read the function to make it again. Nothing was run. Run the script as a query job "+
				"(jobs.insert; Query.Run in the Go client), which keeps what its statements before a failing one did, "+
				"as BigQuery does, or run the DROP %s as a query of its own.", what, strings.Join(s.path, "."), what)
		}
		if !f.functionExists(r, full) {
			s.path = full
			made = append(made, s)
		}
	}
	return made, ""
}

// uncatalogFunctions takes the functions a rolled-back script made out
// of the engine's catalog.
func (f front) uncatalogFunctions(r *http.Request, made []funcStmt) {
	for _, s := range made {
		f.sendDDL(r, dropFunction(s.table)+" IF EXISTS "+functionName(s.path)+"; SELECT * FROM "+quotePath([]string{scratchTable(), "t"}))
	}
}

// functionName writes a function's path with each part quoted: the
// engine does not find a table function by a path quoted whole
// (measured: "Table-valued function not found").
func functionName(p []string) string {
	parts := make([]string, len(p))
	for i, s := range p {
		parts[i] = quoteName(s)
	}
	return strings.Join(parts, ".")
}
