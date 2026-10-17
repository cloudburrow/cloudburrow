package bigqueryfront

import (
	"encoding/json"
	"net/http"
	"strings"
)

// A dry run of a multi-statement script (#1459).
//
// BigQuery's dry run of a query "returns statistics" when the query is
// valid, and fails the request (400 invalidQuery) when it is not
// (https://cloud.google.com/bigquery/docs/running-queries#dry-run); a
// script's job is of statementType SCRIPT
// (https://cloud.google.com/bigquery/docs/reference/rest/v2/Job#JobStatistics2).
// The emulator's dry run of a script, with a TEMP table or function or
// not, checks each statement, but answered a job of statementType SELECT,
// and a statement it could not analyze as a job done with an errorResult
// (measured through the front: "failed to analyze: Unrecognized name:
// nope [at 1:47]" for SELECT nope FROM a TEMP table the script made). So
// the front answers such a failure as BigQuery does, 400 invalidQuery with
// the emulator's message, and a valid script's job with statementType
// SCRIPT. Nothing is run or made: a TEMP table of the script is checked as
// the emulator's dry run checks it, in one request.

// dryRunScript serves q, a dry run of a script of several statements.
func (f front) dryRunScript(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool) {
	rec := newRecorder()
	f.serveQuery(rec, r, q, insert)
	var resp map[string]any
	if json.Unmarshal(rec.body.Bytes(), &resp) != nil {
		rec.copyTo(w)
		return
	}
	if msg, failed := queryFailure(rec, resp); failed {
		if rec.status != http.StatusOK && rec.status != 0 {
			rec.copyTo(w)
			return
		}
		writeError(w, http.StatusBadRequest, "invalidQuery", strings.TrimPrefix(msg, "failed to analyze: "))
		return
	}
	if _, ok := resp["configuration"]; !ok {
		rec.copyTo(w)
		return
	}
	stats, _ := resp["statistics"].(map[string]any)
	if stats == nil {
		stats = map[string]any{}
		resp["statistics"] = stats
	}
	qs, _ := stats["query"].(map[string]any)
	if qs == nil {
		qs = map[string]any{}
		stats["query"] = qs
	}
	qs["statementType"] = "SCRIPT"
	writeRecorded(w, rec, resp)
}
