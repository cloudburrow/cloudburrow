package bigqueryfront

import (
	"encoding/json"
	"net/http"
	"strings"
)

// An emulator 500 to a query (#1109).
//
// The emulator's recovery middleware answers a panic in its engine 500
// internalError, with the panic's text, and the official clients retry a
// 500 until their deadline. Its engine no longer panics on a NULL argument
// (third_party/bigquery-emulator, googlesqlite patch 0011, #1121: a
// function's panic is returned as the query's error), but a query it
// fails so fails the same way each time; so the front answers a 500 to a
// query (jobs.query, or jobs.insert of a query job) 400 invalidQuery with
// the emulator's text, which a client does not retry. Measured through the
// front before that patch with the official Go client: SELECT
// ARRAY_TO_STRING(CAST(NULL AS ARRAY<STRING>), ',') was answered 500
// "runtime error: invalid memory address or nil pointer dereference" and
// retried for 60 s.

// queryFailures notes whether the emulator answered a query sent through
// it (jobs.query or jobs.insert) 500: that is a failure of the query, where
// a 500 to anything else (a datasets.insert the front makes first, say) is
// not.
type queryFailures struct{ failed bool }

// watch returns next, noting in q each 500 it answers to a query.
func (q *queryFailures) watch(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || jobsRoute.FindStringSubmatch(r.URL.EscapedPath()) == nil {
			next.ServeHTTP(w, r)
			return
		}
		rec := newRecorder()
		next.ServeHTTP(rec, r)
		if rec.status == http.StatusInternalServerError {
			q.failed = true
		}
		rec.copyTo(w)
	})
}

// noRetry answers w with rec, a query's answer (jobs.query, or jobs.insert
// of a query job), with a 500 answered 400 invalidQuery (above) when the
// emulator answered a query so (q).
func noRetry(w http.ResponseWriter, rec *recorder, q *queryFailures) {
	if rec.status != http.StatusInternalServerError || !q.failed {
		rec.copyTo(w)
		return
	}
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.body.Bytes(), &e)
	msg := e.Error.Message
	if msg == "" {
		msg = strings.TrimSpace(rec.body.String())
	}
	writeError(w, http.StatusBadRequest, "invalidQuery", "The emulator behind CloudBurrow failed on this query: "+msg+
		" (its answer was 500 internalError, which a client retries; CloudBurrow answers 400, as the query fails the same way "+
		"each time; see docs/compatibility.md).")
}
