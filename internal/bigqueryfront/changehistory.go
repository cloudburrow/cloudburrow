package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Change history: the APPENDS and CHANGES table functions (#1418).
//
// Measured first through the front: APPENDS(TABLE t, NULL, NULL) failed
// 400 in the engine (no such function), and the engine kept no history
// of a table's rows. BigQuery documents
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/time-series-functions,
// https://cloud.google.com/bigquery/docs/change-history, read 2026-10-16):
//
//   - APPENDS(TABLE table, start_timestamp DEFAULT NULL, end_timestamp
//     DEFAULT NULL), on any regular table: the rows appended (CREATE
//     TABLE, INSERT, MERGE's inserts, loads, streaming), as they were
//     inserted, with _CHANGE_TYPE 'INSERT' and _CHANGE_TIMESTAMP, deleted
//     and updated rows still there;
//   - CHANGES(TABLE table, start_timestamp DEFAULT NULL, end_timestamp),
//     on a table with enable_change_history = TRUE: INSERT, DELETE, and
//     an update as a DELETE (_CHANGE_IS_FOR_UPDATE TRUE, changeForUpdateColumn
//     in the change table) of the old row
//     and an UPDATE of the new; the range at most one day;
//   - start NULL is the table's creation, a start before it the creation;
//     a start before the time travel window fails, and so does a NULL
//     start of a table made before the window; end is exclusive, NULL is
//     the query's start, and a future end fails;
//   - only regular tables: no views, materialized views, snapshots,
//     external tables or wildcards.
//
// The engine records every change (googlesqlite patch 0036): each table
// made by CREATE TABLE has a change table, `project.dataset.table$changes`,
// of every row inserted, deleted and updated, filled by SQLite triggers,
// so DML, MERGE, TRUNCATE TABLE, tabledata.insertAll, loads, the Storage
// Write API, WRITE_TRUNCATE and query destinations are all recorded, each
// with the time of its transaction, kept seven days. The front checks
// the call as BigQuery does and writes it as a subquery of the change
// table, the window's rows, and runs the query so; the job keeps the
// statement the client sent. Its costs: every table keeps a history
// (CHANGES needs none of a table without the option, but APPENDS and time
// travel read any table's, and the option can be set later), and a DELETE
// or TRUNCATE copies the deleted rows; what is older than the dataset's
// time travel window is let go of (#1503, changehistoryretention.go).
// Since #1597 (googlesqlite patch 0074) a row inserted or updated is not
// copied into the change table while it is unchanged in the table: its
// record is pending, its values the row's, until the row changes again
// (the change's trigger writes the old values in) or a query names the
// change table, which every read here does (the engine reads the pending
// values from the table first). Measured (the engine alone, 50,000 rows
// inserted, 10,000 updated and 10,000 deleted): the same insert time (the
// analysis dominates), an UPDATE and a DELETE a fifth to two fifths
// slower than with whole copies, and a database of 39.7 MB rather than
// 51.9 (16.3 with no history). Recording only once a table has the
// option, or is read with APPENDS, would leave APPENDS and time travel
// without the rows before.
//
// #1466 also refuses either function inside a multi-statement
// transaction ("You can't call the APPENDS [CHANGES] function within a
// multi-statement transaction") and CHANGES of a table with change data
// capture, and keeps the history across the table's rebuilds
// (changehistorycarry.go), whose CARRY and END rows the reads leave out.
//
// The ten-minute lag some older documentation gave CHANGES's end is not
// in the documentation read (2026-10-16), which takes NULL as the
// query's start, so the front has none. The error messages are
// UNVERIFIED: no BigQuery to read them from.
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/change-history

// changeFunc is one call of APPENDS or CHANGES in a query: its tokens
// stmt[at:end], the table, and the start and end expressions ("" when
// left out).
type changeFunc struct {
	name           string // APPENDS or CHANGES
	pos, end       int    // byte offsets of the call in the query
	dataset, table string
	start, stop    string
}

// changeFuncs returns the calls of APPENDS and CHANGES in sql, and false
// when one cannot be read.
func changeFuncs(sql, project, defaultDataset string) ([]changeFunc, bool) {
	up := strings.ToUpper(sql)
	if !strings.Contains(up, "APPENDS") && !strings.Contains(up, "CHANGES") {
		return nil, true
	}
	toks, ok := lex(sql)
	if !ok {
		return nil, true
	}
	var calls []changeFunc
	for i := 0; i+3 < len(toks); i++ {
		t := toks[i]
		if t.kind != tokWord || !t.is("APPENDS") && !t.is("CHANGES") || !toks[i+1].punct("(") || !toks[i+2].is("TABLE") {
			continue
		}
		parts, next := path(toks, i+3)
		if next <= i+3 {
			return nil, false
		}
		if len(parts) == 1 && defaultDataset != "" {
			parts = []string{defaultDataset, parts[0]}
		}
		dataset, table, named := resolve(parts, project)
		if !named {
			return nil, false
		}
		c := changeFunc{name: strings.ToUpper(t.text), pos: t.pos, dataset: dataset, table: table}
		var args []string
		j := next
		for j < len(toks) && toks[j].punct(",") {
			k, depth := j+1, 0
			for ; k < len(toks); k++ {
				if toks[k].punct("(") || toks[k].punct("[") {
					depth++
				} else if toks[k].punct(")") || toks[k].punct("]") {
					if depth == 0 {
						break
					}
					depth--
				} else if toks[k].punct(",") && depth == 0 {
					break
				}
			}
			if k >= len(toks) || k == j+1 {
				return nil, false
			}
			args = append(args, sql[toks[j+1].pos:toks[k-1].end])
			j = k
		}
		if j >= len(toks) || !toks[j].punct(")") || len(args) > 2 {
			return nil, false
		}
		if len(args) > 0 {
			c.start = args[0]
		}
		if len(args) > 1 {
			c.stop = args[1]
		}
		c.end = toks[j].end
		calls = append(calls, c)
		i = j
	}
	return calls, true
}

// changeReads answers q, or rewrites it and runs it, when it calls APPENDS
// or CHANGES (above); false when it calls neither.
func (f front) changeReads(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool) bool {
	project := projectOf(f.base)
	calls, ok := changeFuncs(q.Query, project, defaultDatasetOf(q))
	if !ok || len(calls) == 0 {
		return false
	}
	fail := func(dataset string, code int, reason, msg string) bool {
		f.failBeforeRun(w, r, q, insert, dataset, code, rowError{Reason: reason, Message: msg})
		return true
	}
	var b strings.Builder
	last := 0
	for _, c := range calls {
		ref := fmt.Sprintf("%s:%s.%s", project, c.dataset, c.table)
		st, got := f.get(r, tablePath(c.dataset, c.table))
		var meta map[string]any
		if st != http.StatusOK || json.Unmarshal(got, &meta) != nil {
			return fail(c.dataset, http.StatusNotFound, "notFound", "Not found: Table "+ref)
		}
		typ := str(meta, "type")
		if meta["snapshotDefinition"] != nil {
			typ = "SNAPSHOT" // the emulator's type of one is TABLE (snapshots.go, #1466)
		}
		if typ != "" && typ != "TABLE" {
			return fail(c.dataset, http.StatusBadRequest, "invalidQuery", fmt.Sprintf(
				"%s can only be used with a regular table; %s is a %s", c.name, ref, strings.ReplaceAll(typ, "_", " ")))
		}
		created, _ := strconv.ParseInt(str(meta, "creationTime"), 10, 64) // milliseconds
		if inTransaction(q.Query, c.pos) {
			// "You can't call the APPENDS [CHANGES] function within a
			// multi-statement transaction" (#1466).
			return fail(c.dataset, http.StatusBadRequest, "invalidQuery", fmt.Sprintf(
				"%s cannot be called within a multi-statement transaction", c.name))
		}
		if c.name == "CHANGES" && changeDataCapture(meta) {
			// "You can't use the CHANGES function with a table that has
			// change data capture enabled" (#1466).
			return fail(c.dataset, http.StatusBadRequest, "invalidQuery", fmt.Sprintf(
				"CHANGES cannot be used with the table %s, which has change data capture enabled (a primary key and max_staleness)", ref))
		}
		if c.name == "CHANGES" {
			s := f.tables.get(streamKey(project, c.dataset, c.table), str(meta, "creationTime"))
			if s.ChangeHistory == nil || *s.ChangeHistory != "true" {
				return fail(c.dataset, http.StatusBadRequest, "invalidQuery", fmt.Sprintf(
					"CHANGES requires the table %s to have the option enable_change_history set to TRUE", ref))
			}
		}
		start, stop, code, msg := f.changeWindow(r, q, c, ref, created)
		if code != 0 {
			return fail(c.dataset, code, reasonOf(code, "invalidQuery"), msg)
		}
		if c.name == "CHANGES" {
			// #1503, changehistorytxn.go.
			if at, ok := committedIn(f.tables.get(streamKey(project, c.dataset, c.table), str(meta, "creationTime")), start, stop); ok {
				return fail(c.dataset, http.StatusBadRequest, "invalidQuery", committedRefused(ref, at))
			}
		}
		// The change table's flag of an update's DELETE is
		// changeForUpdateColumn (googlesqlite patch 0065), which CHANGES
		// names _CHANGE_IS_FOR_UPDATE; APPENDS has none. A table of a
		// column of its own named _CHANGE_IS_FOR_UPDATE keeps it.
		cols := "* EXCEPT (" + changeForUpdateColumn + "), " + changeForUpdateColumn + " AS _CHANGE_IS_FOR_UPDATE"
		// The engine's START row marks when the change table was made
		// (googlesqlite patch 0041, timetravel.go).
		filter := fmt.Sprintf("_CHANGE_TIMESTAMP >= TIMESTAMP_MICROS(%d) AND _CHANGE_TIMESTAMP < TIMESTAMP_MICROS(%d) AND _CHANGE_TYPE NOT IN ('START', '%s', '%s')", start, stop, changeCarry, changeEnd)
		if c.name == "APPENDS" {
			cols = "* EXCEPT (" + changeForUpdateColumn + ")"
			filter += " AND _CHANGE_TYPE = 'INSERT'"
		}
		b.WriteString(q.Query[last:c.pos])
		fmt.Fprintf(&b, "(SELECT %s FROM %s WHERE %s)", cols, quotePath([]string{project, c.dataset, c.table + changeTableSuffix}), filter)
		last = c.end
	}
	b.WriteString(q.Query[last:])
	sql := b.String()
	if !setQueryText(r, insert, sql) {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query")
		return true
	}
	client := jobText{query: q.Query}
	q.Query = sql
	rec := newRecorder()
	f.runQuery(rec, r, q, insert)
	f.clientText(w, rec, client)
	return true
}

// changeTableSuffix ends the name of a table's change table in the engine
// (googlesqlite patch 0036).
const changeTableSuffix = "$changes"

// changeForUpdateColumn is the change table's column that marks the
// DELETE of an update's old row, CHANGES' _CHANGE_IS_FOR_UPDATE
// (googlesqlite patch 0065, #1548): a name under the reserved prefix
// _CHANGE_TYPE, so that no table's own column has it.
const changeForUpdateColumn = "_CHANGE_TYPE_FOR_UPDATE"

// changeWindow evaluates the window of c, a call on the table ref made at
// created (milliseconds), as microseconds since the epoch: [start, stop).
// code is not 0 when the window is refused (above).
func (f front) changeWindow(r *http.Request, q queryOptions, c changeFunc, ref string, created int64) (start, stop int64, code int, msg string) {
	expr := func(e string) string {
		if strings.TrimSpace(e) == "" {
			return "CAST(NULL AS TIMESTAMP)"
		}
		return "CAST((" + e + ") AS TIMESTAMP)"
	}
	legacy := false
	body, err := json.Marshal(queryOptions{Query: "SELECT UNIX_MICROS(" + expr(c.start) + "), UNIX_MICROS(" + expr(c.stop) +
		"), UNIX_MICROS(CURRENT_TIMESTAMP())", UseLegacySQL: &legacy, DefaultDataset: q.DefaultDataset,
		ParameterMode: q.ParameterMode, QueryParameters: q.QueryParameters})
	if err != nil {
		return 0, 0, http.StatusInternalServerError, err.Error()
	}
	status, got := f.send(r, http.MethodPost, "/queries", body)
	if status != http.StatusOK {
		return 0, 0, http.StatusBadRequest, errorMessage(got, status)
	}
	var res struct {
		Rows []struct {
			F []struct {
				V *string `json:"v"`
			} `json:"f"`
		} `json:"rows"`
	}
	if json.Unmarshal(got, &res) != nil || len(res.Rows) != 1 || len(res.Rows[0].F) != 3 || res.Rows[0].F[2].V == nil {
		return 0, 0, http.StatusInternalServerError, "cloudburrow: could not read the times of " + c.name
	}
	at := func(v *string) (int64, bool) {
		if v == nil {
			return 0, false
		}
		n, err := strconv.ParseInt(*v, 10, 64)
		return n, err == nil
	}
	now, _ := at(res.Rows[0].F[2].V)
	hours := int64(168)
	if st, got := f.get(r, "/datasets/"+url.PathEscape(c.dataset)); st == http.StatusOK {
		var ds struct {
			Hours string `json:"maxTimeTravelHours"`
		}
		if json.Unmarshal(got, &ds) == nil {
			if h, err := strconv.ParseInt(ds.Hours, 10, 64); err == nil && h > 0 {
				hours = h
			}
		}
	}
	earliest := now - hours*int64(time.Hour/time.Microsecond)
	_ = f.pruneHistory(r, c.dataset, c.table, earliest) // #1503, changehistoryretention.go: read within the window either way
	createdMicros := created * 1000
	// The history starts at the table's creation by the engine's clock,
	// which follows the front's (the emulator keeps creationTime by the
	// system's), and a table made again by CREATE OR REPLACE TABLE keeps
	// the start of the one it replaced (#1466): NULL reads from there.
	if start, ok := f.historyStart(r, c.dataset, c.table); ok {
		createdMicros = start
	}
	start, hasStart := at(res.Rows[0].F[0].V)
	stop, hasStop := at(res.Rows[0].F[1].V)
	switch {
	case hasStart && start < earliest:
		return 0, 0, http.StatusBadRequest, fmt.Sprintf("Invalid start_timestamp of %s: %s is before the time travel window of %s, which starts at %s",
			c.name, micros(start), ref, micros(earliest))
	case !hasStart && createdMicros < earliest:
		return 0, 0, http.StatusBadRequest, fmt.Sprintf("Invalid start_timestamp of %s: NULL reads %s from its creation at %s, "+
			"before its time travel window, which starts at %s; give a start_timestamp within the window", c.name, ref, micros(createdMicros), micros(earliest))
	case hasStop && stop > now:
		return 0, 0, http.StatusBadRequest, fmt.Sprintf("Invalid end_timestamp of %s: %s is in the future", c.name, micros(stop))
	}
	// "The maximum allowed time range between the start_timestamp and
	// end_timestamp arguments you specify is one day" (#1466): the
	// arguments as given, and the range they read from the table's
	// creation (below).
	if c.name == "CHANGES" && hasStart && hasStop && stop-start > day {
		return 0, 0, http.StatusBadRequest, fmt.Sprintf("Invalid time range of CHANGES on %s: from %s to %s is more than one day, "+
			"the longest range allowed", ref, micros(start), micros(stop))
	}
	if !hasStart || start < createdMicros {
		start = createdMicros
	}
	if !hasStop {
		stop = now
	}
	if c.name == "CHANGES" && stop-start > day {
		return 0, 0, http.StatusBadRequest, fmt.Sprintf("Invalid time range of CHANGES on %s: from %s to %s is more than one day, "+
			"the longest range allowed", ref, micros(start), micros(stop))
	}
	return start, stop, 0, ""
}

// day is one day in microseconds.
const day = int64(24 * time.Hour / time.Microsecond)

// micros is t, microseconds since the epoch, as a UTC timestamp.
func micros(t int64) string {
	return time.UnixMicro(t).UTC().Format("2006-01-02 15:04:05.999999 UTC")
}

// inTransaction reports whether the byte offset pos of sql is inside a
// multi-statement transaction: after a BEGIN TRANSACTION and before the
// COMMIT or ROLLBACK that ends it.
func inTransaction(sql string, pos int) bool {
	toks, ok := lex(sql)
	if !ok {
		return false
	}
	in := false
	for i, t := range toks {
		if t.pos >= pos {
			break
		}
		start := i == 0 || toks[i-1].punct(";")
		switch {
		case start && isBeginTransaction(toks[i:]):
			in = true
		case start && (t.is("COMMIT") || t.is("ROLLBACK")):
			in = false
		}
	}
	return in
}

// changeDataCapture reports whether the table of the tables.get answer
// meta has change data capture enabled: BigQuery's CDC needs a primary
// key and max_staleness (https://cloud.google.com/bigquery/docs/change-data-capture).
// CloudBurrow serves no _CHANGE_TYPE upserts, so a table so configured is
// taken for one.
func changeDataCapture(meta map[string]any) bool {
	tc, _ := meta["tableConstraints"].(map[string]any)
	pk, _ := tc["primaryKey"].(map[string]any)
	cols, _ := pk["columns"].([]any)
	return len(cols) > 0 && str(meta, "maxStaleness") != ""
}
