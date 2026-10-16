package bigqueryfront

import (
	"net/http"
	"strings"
)

// The engine's own pseudo-columns in a client's query (#1458).
//
// The engine gives a table the pseudo-columns _PARTITION_BUFFERED and
// _PARTITION_STREAMED (streamingbuffer.go), which the front's own queries
// read. BigQuery has neither, and reserves every column name that starts
// with _PARTITION ("Column names cannot use any of the following
// prefixes: ... _PARTITION", https://cloud.google.com/bigquery/docs/schemas#column_names),
// so no table of a client has a column of either name, and a query of the
// client that names one fails as a query naming any column that does not
// exist fails: 400 invalidQuery "Unrecognized name: <name> at [line:col]"
// (the wording measured on the engine for a missing column, catalogsync.go;
// BigQuery's own text UNVERIFIED). Measured first: such a query read the
// engine's marks. The check is made where the client's text enters the
// front, jobs.query and jobs.insert, before any rewrite of the front's
// that names them.

// enginePseudoColumns are the engine's pseudo-columns (above), upper case.
var enginePseudoColumns = map[string]bool{bufferedRows: true, streamedAt: true}

// namedPseudoColumn returns the first of the engine's pseudo-columns sql
// names as an identifier, as it is written, and its byte offset, or ""
// when it names none. A string literal or comment naming one is not a
// reference.
func namedPseudoColumn(sql string) (string, int) {
	toks, ok := lex(sql)
	if !ok {
		return "", 0
	}
	for _, t := range toks {
		if t.kind != tokWord && t.kind != tokQuoted {
			continue
		}
		// A quoted path (`t._PARTITION_STREAMED`) is one token.
		parts := strings.Split(t.text, ".")
		for _, p := range parts {
			if enginePseudoColumns[strings.ToUpper(p)] {
				return p, t.pos
			}
		}
	}
	return "", 0
}

// refusePseudoColumns fails q, as BigQuery fails a query naming a column
// that does not exist, when the client's GoogleSQL text names one of the
// engine's pseudo-columns (above); it reports whether it did.
func (f front) refusePseudoColumns(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool) bool {
	if q.UseLegacySQL != nil && *q.UseLegacySQL {
		return false
	}
	name, pos := namedPseudoColumn(q.Query)
	if name == "" {
		return false
	}
	f.failBeforeRun(w, r, q, insert, "_cloudburrow", http.StatusBadRequest, rowError{Reason: "invalidQuery",
		Message: "Unrecognized name: " + name + " at " + lineCol(q.Query, pos)})
	return true
}
