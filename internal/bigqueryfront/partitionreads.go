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

// Reads of a partitioned table: the _PARTITIONTIME and _PARTITIONDATE
// pseudo-columns (#1277), partition decorators (#1273) and FOR SYSTEM_TIME
// AS OF (#1276).
//
// Measured against the pinned image through the front (2026-10-15): the
// engine (googlesqlite v0.3.1, analyzer.go's applyIngestionPartitionRewrite)
// writes every bare _PARTITIONTIME as CURRENT_TIMESTAMP() and _PARTITIONDATE
// as CURRENT_DATE(), on any table, where BigQuery has them only on an
// ingestion-time partitioned table and fails "Unrecognized name" on any other
// (https://cloud.google.com/bigquery/docs/querying-partitioned-tables). The
// emulator keeps no row's ingestion time, so on an ingestion-time
// partitioned table that holds rows the front cannot answer them: 501. On
// an empty one it lets the query run (it reads no row); on a table that is
// not ingestion-time partitioned it fails the query as BigQuery does.
//
// `p.d.t$20240101` failed 400 "Table not found". BigQuery reads one
// partition so (https://cloud.google.com/bigquery/docs/managing-partitioned-table-data):
// YYYYMMDD, YYYYMMDDHH, YYYYMM or YYYY by the table's time-unit, the start
// of an integer range, __NULL__ and __UNPARTITIONED__. The front writes a
// query's read of a decorated name of a column-partitioned table as a
// subquery of the partition's rows, and deletes a partition
// (tables.delete of the decorated name) with a DELETE of its rows. A
// decorator on a table that is not partitioned is refused 400 (wording
// UNVERIFIED). tabledata.list and tables.get of a partition and a query job
// into one are
// in partitionwrites.go. Not built (501): an ingestion-time partition (no
// row's ingestion time is kept) and a decorated destination of a load or
// copy job.
//
// FOR SYSTEM_TIME AS OF failed 400 "FOR SYSTEM_TIME AS OF is not
// supported", the emulator keeps no history. The front reads the time
// first: a time before the table's time travel window (its creation, or
// the dataset's maxTimeTravelHours, 168 by default, before now) fails as
// BigQuery documents it (https://cloud.google.com/bigquery/docs/time-travel),
// a time at the engine's now reads the table as it is, and any other is
// 501.

// tableRead is one read of a table in a FROM clause of a query.
type tableRead struct {
	stmt                      []token
	at, next                  int // the path's tokens, stmt[at:next]
	dataset, table, decorator string
}

// tableReads returns the reads of the project's tables in sql's FROM
// clauses, and its statements' tokens.
func tableReads(sql, project, defaultDataset string) (reads []tableRead, stmts [][]token, ok bool) {
	toks, ok := lex(sql)
	if !ok {
		return nil, nil, false
	}
	for _, stmt := range splitStatements(toks) {
		body, _, _ := stripControlFlow(stmt)
		stmts = append(stmts, body)
		sources, _ := sourcePositions(body)
		for j := range body {
			if !sources[j] || body[j].kind != tokWord && body[j].kind != tokQuoted ||
				body[j].kind == tokWord && reservedWords[strings.ToUpper(body[j].text)] {
				continue
			}
			parts, next := path(body, j)
			if next <= j || next < len(body) && body[next].punct("(") {
				continue
			}
			if len(parts) == 1 && defaultDataset != "" {
				parts = []string{defaultDataset, parts[0]}
			}
			dataset, table, named := resolve(parts, project)
			if !named {
				continue
			}
			tr := tableRead{stmt: body, at: j, next: next, dataset: dataset, table: table}
			if i := strings.Index(table, "$"); i > 0 {
				tr.table, tr.decorator = table[:i], table[i+1:]
			}
			reads = append(reads, tr)
		}
	}
	return reads, stmts, true
}

// partitionMeta is what a read of a partitioned table needs of its
// tables.get.
type partitionMeta struct {
	Created          string `json:"creationTime"`
	NumRows          string `json:"numRows"`
	TimePartitioning *struct {
		Type  string `json:"type"`
		Field string `json:"field"`
	} `json:"timePartitioning"`
	RangePartitioning *struct {
		Field string `json:"field"`
		Range struct {
			Start    string `json:"start"`
			End      string `json:"end"`
			Interval string `json:"interval"`
		} `json:"range"`
	} `json:"rangePartitioning"`
	Schema tableSchema `json:"schema"`
}

func (m partitionMeta) ingestion() bool {
	return m.TimePartitioning != nil && m.TimePartitioning.Field == ""
}

func (f front) partitionMetaOf(r *http.Request, dataset, table string) (partitionMeta, bool) {
	status, got := f.get(r, tablePath(dataset, table))
	var m partitionMeta
	if status != http.StatusOK || json.Unmarshal(got, &m) != nil {
		return m, false
	}
	return m, true
}

// lineCol is BigQuery's [line:column] of byte offset pos of sql.
func lineCol(sql string, pos int) string {
	line, col := 1, 1
	for i := 0; i < pos && i < len(sql); i++ {
		if sql[i] == '\n' {
			line, col = line+1, 1
		} else {
			col++
		}
	}
	return fmt.Sprintf("[%d:%d]", line, col)
}

// partitionReads answers q, or rewrites it and runs it, when it reads a
// pseudo-column, a partition decorator or FOR SYSTEM_TIME AS OF (above);
// false when it reads none and is left to the rest of runQuery.
func (f front) partitionReads(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool) bool {
	if !strings.Contains(strings.ToUpper(q.Query), "_PARTITION") && !strings.Contains(q.Query, "$") &&
		!strings.Contains(strings.ToUpper(q.Query), "SYSTEM_TIME") {
		return false
	}
	project := projectOf(f.base)
	reads, stmts, ok := tableReads(q.Query, project, defaultDatasetOf(q))
	if !ok {
		return false
	}
	metas := map[string]partitionMeta{}
	meta := func(tr tableRead) (partitionMeta, bool) {
		k := tr.dataset + "\x00" + tr.table
		if m, ok := metas[k]; ok {
			return m, true
		}
		m, ok := f.partitionMetaOf(r, tr.dataset, tr.table)
		if ok {
			metas[k] = m
		}
		return m, ok
	}

	// _PARTITIONTIME and _PARTITIONDATE (#1277).
	for _, stmt := range stmts {
		if len(stmt) == 0 || stmt[0].is("CREATE") || stmt[0].is("ALTER") {
			continue // PARTITION BY _PARTITIONDATE is partitionddl.go's
		}
		var pseudo *token
		for i := range stmt {
			if stmt[i].is("_PARTITIONTIME") || stmt[i].is("_PARTITIONDATE") {
				pseudo = &stmt[i]
				break
			}
		}
		if pseudo == nil {
			continue
		}
		found, ingestion, rows := false, false, false
		for _, tr := range reads {
			if len(tr.stmt) == 0 || &tr.stmt[0] != &stmt[0] {
				continue
			}
			m, ok := meta(tr)
			if !ok {
				continue
			}
			found = true
			if m.ingestion() {
				ingestion = true
				if n, _ := strconv.Atoi(m.NumRows); n > 0 {
					rows = true
				}
			}
		}
		switch {
		case !found:
		case !ingestion:
			writeError(w, http.StatusBadRequest, "invalidQuery", "Unrecognized name: "+strings.ToUpper(pseudo.text)+
				" at "+lineCol(q.Query, pseudo.pos))
			return true
		case rows:
			writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: "+strings.ToUpper(pseudo.text)+
				" of an ingestion-time partitioned table that holds rows. The emulator behind CloudBurrow keeps no row's "+
				"ingestion time, so it cannot answer the pseudo-column. Partition the table by a column instead "+
				"(docs/compatibility.md). Nothing was run.")
			return true
		}
	}

	type edit struct {
		pos, end int
		text     string
	}
	var edits []edit
	for _, tr := range reads {
		ref := fmt.Sprintf("%s:%s.%s", project, tr.dataset, tr.table)
		// FOR SYSTEM_TIME AS OF (#1276).
		k := tr.next
		if k < len(tr.stmt) && tr.stmt[k].is("AS") {
			k += 2
		} else if k < len(tr.stmt) && tr.stmt[k].kind == tokWord && !reservedWords[strings.ToUpper(tr.stmt[k].text)] && !tr.stmt[k].is("FOR") {
			k++
		}
		if k+3 < len(tr.stmt) && tr.stmt[k].is("FOR") && tr.stmt[k+1].is("SYSTEM_TIME") && tr.stmt[k+2].is("AS") && tr.stmt[k+3].is("OF") {
			end := systemTimeEnd(tr.stmt, k+4)
			if end <= k+4 {
				return false // the engine's own error
			}
			expr := q.Query[tr.stmt[k+4].pos:tr.stmt[end-1].end]
			m, ok := meta(tr)
			if !ok {
				return false // not found: the engine's own error
			}
			code, msg := f.systemTime(r, q, tr.dataset, ref, m, expr)
			if code != 0 {
				reason := reasonOf(code, "invalidQuery")
				writeError(w, code, reason, msg)
				return true
			}
			edits = append(edits, edit{tr.stmt[k].pos, tr.stmt[end-1].end, ""})
		}
		// table$partition (#1273).
		if tr.decorator == "" {
			continue
		}
		m, ok := meta(tr)
		if !ok {
			return false
		}
		filter, code, msg := partitionFilter(m, tr.decorator, ref+"$"+tr.decorator)
		if code != 0 {
			reason := reasonOf(code, "invalid")
			writeError(w, code, reason, msg)
			return true
		}
		text := "(SELECT * FROM " + quotePath([]string{project, tr.dataset, tr.table}) + " WHERE " + filter + ")"
		if n := tr.next; n >= len(tr.stmt) || !tr.stmt[n].is("AS") && (tr.stmt[n].kind != tokWord && tr.stmt[n].kind != tokQuoted ||
			tr.stmt[n].kind == tokWord && reservedWords[strings.ToUpper(tr.stmt[n].text)] || tr.stmt[n].is("FOR")) {
			text += " AS " + quotePath([]string{tr.table})
		}
		edits = append(edits, edit{tr.stmt[tr.at].pos, tr.stmt[tr.next-1].end, text})
	}
	if len(edits) == 0 {
		return false
	}
	var b strings.Builder
	last := 0
	for _, e := range sortByPos(edits, func(e edit) int { return e.pos }) {
		b.WriteString(q.Query[last:e.pos])
		b.WriteString(e.text)
		last = e.end
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

// sortByPos orders edits by position (insertion sort: a query has few).
func sortByPos[E any](edits []E, pos func(E) int) []E {
	for i := 1; i < len(edits); i++ {
		for j := i; j > 0 && pos(edits[j]) < pos(edits[j-1]); j-- {
			edits[j], edits[j-1] = edits[j-1], edits[j]
		}
	}
	return edits
}

// systemTimeEnd returns the index of the token after the timestamp
// expression of FOR SYSTEM_TIME AS OF that starts at i.
func systemTimeEnd(stmt []token, i int) int {
	depth := 0
	for ; i < len(stmt); i++ {
		t := stmt[i]
		switch {
		case t.punct("(") || t.punct("["):
			depth++
		case t.punct(")") || t.punct("]"):
			if depth == 0 {
				return i
			}
			depth--
		case depth > 0:
		case t.punct(",") || t.punct(";"):
			return i
		case t.kind == tokWord && (t.is("AS") || t.is("WHERE") || t.is("JOIN") || t.is("INNER") || t.is("LEFT") || t.is("RIGHT") ||
			t.is("FULL") || t.is("CROSS") || t.is("GROUP") || t.is("ORDER") || t.is("LIMIT") || t.is("UNION") ||
			t.is("INTERSECT") || t.is("EXCEPT") || t.is("ON") || t.is("USING") || t.is("QUALIFY") || t.is("HAVING") ||
			t.is("WINDOW") || t.is("TABLESAMPLE") || t.is("WHEN")):
			return i
		}
	}
	return i
}

// systemTime checks a read of the table ref (of dataset, with m) AS OF
// expr: 0 when it reads the table as it is, else the status and message
// it fails with (above).
func (f front) systemTime(r *http.Request, q queryOptions, dataset, ref string, m partitionMeta, expr string) (int, string) {
	notBuilt := "Not implemented here: FOR SYSTEM_TIME AS OF a time before now. The emulator behind CloudBurrow keeps no " +
		"history of a table's rows, so it can read a table only as it is now (docs/compatibility.md). Nothing was run."
	legacy := false
	body, err := json.Marshal(queryOptions{Query: "SELECT UNIX_MICROS(" + expr + "), UNIX_MICROS(CURRENT_TIMESTAMP())",
		UseLegacySQL: &legacy, DefaultDataset: q.DefaultDataset, ParameterMode: q.ParameterMode, QueryParameters: q.QueryParameters})
	if err != nil {
		return http.StatusInternalServerError, err.Error()
	}
	status, got := f.send(r, http.MethodPost, "/queries", body)
	if status != http.StatusOK {
		return http.StatusBadRequest, errorMessage(got, status)
	}
	var res struct {
		Rows []struct {
			F []struct {
				V *string `json:"v"`
			} `json:"f"`
		} `json:"rows"`
	}
	if json.Unmarshal(got, &res) != nil || len(res.Rows) != 1 || len(res.Rows[0].F) != 2 || res.Rows[0].F[1].V == nil {
		return http.StatusInternalServerError, "cloudburrow: could not read the time of FOR SYSTEM_TIME AS OF"
	}
	if res.Rows[0].F[0].V == nil {
		return http.StatusBadRequest, "FOR SYSTEM_TIME AS OF expression must not be NULL" // UNVERIFIED wording
	}
	ts, err1 := strconv.ParseInt(*res.Rows[0].F[0].V, 10, 64)
	now, err2 := strconv.ParseInt(*res.Rows[0].F[1].V, 10, 64)
	if err1 != nil || err2 != nil {
		return http.StatusInternalServerError, "cloudburrow: could not read the time of FOR SYSTEM_TIME AS OF"
	}
	hours := int64(168)
	if st, got := f.get(r, "/datasets/"+url.PathEscape(dataset)); st == http.StatusOK {
		var ds struct {
			Hours string `json:"maxTimeTravelHours"`
		}
		if json.Unmarshal(got, &ds) == nil {
			if h, err := strconv.ParseInt(ds.Hours, 10, 64); err == nil && h > 0 {
				hours = h
			}
		}
	}
	// The engine evaluates each CURRENT_TIMESTAMP() of a query on its own,
	// so the time is now when the expression is CURRENT_TIMESTAMP() itself.
	compact := strings.ToUpper(strings.Join(strings.Fields(expr), ""))
	current := compact == "CURRENT_TIMESTAMP()" || compact == "CURRENT_TIMESTAMP" || ts == now
	tsMs, nowMs := ts/1000, now/1000
	earliest := nowMs - hours*int64(time.Hour/time.Millisecond)
	c, err := strconv.ParseInt(m.Created, 10, 64)
	if err == nil && c < 1e11 {
		c *= 1000 // the emulator's creationTime is in seconds (measured)
	}
	if err == nil && c > earliest {
		earliest = c
	}
	switch {
	case tsMs < earliest:
		// As https://cloud.google.com/bigquery/docs/time-travel documents it.
		return http.StatusBadRequest, fmt.Sprintf("Invalid snapshot time %d for table %s@%d. Cannot read before %d",
			tsMs, ref, tsMs, earliest)
	case current:
		return 0, ""
	case ts > now:
		return http.StatusNotImplemented, "Not implemented here: FOR SYSTEM_TIME AS OF a time in the future. " +
			"CloudBurrow does not know how BigQuery answers it (docs/compatibility.md). Nothing was run."
	}
	return http.StatusNotImplemented, notBuilt
}

// partitionFilter returns the condition the rows of partition dec of a
// table with m hold, or the status and message of why the decorator is
// refused (above). name is the decorated table's name, for the message.
func partitionFilter(m partitionMeta, dec, name string) (string, int, string) {
	typeOf := func(field string) string {
		for _, fl := range m.Schema.Fields {
			if strings.EqualFold(fl.Name, field) {
				return strings.ToUpper(fl.Type)
			}
		}
		return ""
	}
	switch {
	case m.RangePartitioning != nil:
		rp := m.RangePartitioning
		col := quoteName(rp.Field)
		if rp.Range.Start == "" {
			rp.Range.Start = "0" // the emulator drops a start of 0 (partitionddl.go)
		}
		start, e1 := strconv.ParseInt(rp.Range.Start, 10, 64)
		end, e2 := strconv.ParseInt(rp.Range.End, 10, 64)
		step, e3 := strconv.ParseInt(rp.Range.Interval, 10, 64)
		if e1 != nil || e2 != nil || e3 != nil || step <= 0 {
			return "", http.StatusInternalServerError, "cloudburrow: could not read the table's range partitioning"
		}
		switch dec {
		case "__NULL__":
			return col + " IS NULL", 0, ""
		case "__UNPARTITIONED__":
			return fmt.Sprintf("(%s < %d OR %s >= %d)", col, start, col, end), 0, ""
		}
		v, err := strconv.ParseInt(dec, 10, 64)
		if err != nil || v < start || v >= end || (v-start)%step != 0 {
			return "", http.StatusBadRequest, "Invalid partition decorator " + name // UNVERIFIED wording
		}
		hi := v + step
		if hi > end {
			hi = end
		}
		return fmt.Sprintf("(%s >= %d AND %s < %d)", col, v, col, hi), 0, ""
	case m.TimePartitioning == nil:
		return "", http.StatusBadRequest, "Cannot read partition information from a table that is not partitioned: " + name // UNVERIFIED wording
	case m.ingestion():
		return "", http.StatusNotImplemented, "Not implemented here: a partition decorator of an ingestion-time partitioned " +
			"table. The emulator behind CloudBurrow keeps no row's ingestion time, so it cannot tell a partition's rows. " +
			"Partition the table by a column instead (docs/compatibility.md). Nothing was run."
	}
	tp := m.TimePartitioning
	col := quoteName(tp.Field)
	expr := col
	if t := typeOf(tp.Field); t == "DATE" || t == "DATETIME" {
		expr = "TIMESTAMP(" + col + ")"
	}
	switch dec {
	case "__NULL__":
		return col + " IS NULL", 0, ""
	case "__UNPARTITIONED__":
		return "(" + expr + " < TIMESTAMP '1960-01-01' OR " + expr + " >= TIMESTAMP '2160-01-01')", 0, ""
	}
	layout, unit := map[string][2]string{"DAY": {"20060102", "day"}, "HOUR": {"2006010215", "hour"},
		"MONTH": {"200601", "month"}, "YEAR": {"2006", "year"}}[strings.ToUpper(tp.Type)], ""
	if layout[0] == "" {
		layout = [2]string{"20060102", "day"}
	}
	from, err := time.Parse(layout[0], dec)
	if err != nil || len(dec) != len(layout[0]) {
		return "", http.StatusBadRequest, "Invalid partition decorator " + name // UNVERIFIED wording
	}
	var to time.Time
	switch unit = layout[1]; unit {
	case "hour":
		to = from.Add(time.Hour)
	case "month":
		to = from.AddDate(0, 1, 0)
	case "year":
		to = from.AddDate(1, 0, 0)
	default:
		to = from.AddDate(0, 0, 1)
	}
	const ts = "2006-01-02 15:04:05"
	return fmt.Sprintf("(%s >= TIMESTAMP '%s' AND %s < TIMESTAMP '%s')", expr, from.Format(ts), expr, to.Format(ts)), 0, ""
}

// servePartition answers a request for a decorated table, dataset.table
// (#1273, above): tables.delete deletes the partition's rows;
// tabledata.list and tables.get read them (partitionwrites.go); any other
// request of a partition is 501; any request of a table that is
// not partitioned is refused.
func (f front) servePartition(w http.ResponseWriter, r *http.Request, dataset, table, sub string) {
	i := strings.Index(table, "$")
	base, dec := table[:i], table[i+1:]
	project := projectOf(f.base)
	name := fmt.Sprintf("%s:%s.%s", project, dataset, table)
	m, ok := f.partitionMetaOf(r, dataset, base)
	if !ok {
		writeError(w, http.StatusNotFound, "notFound", fmt.Sprintf("Not found: Table %s", name))
		return
	}
	filter, code, msg := partitionFilter(m, dec, name)
	if code != 0 {
		reason := reasonOf(code, "invalid")
		writeError(w, code, reason, msg)
		return
	}
	if r.Method == http.MethodDelete && sub == "" {
		if msg := f.runDML(r, "DELETE FROM "+quotePath([]string{project, dataset, base})+" WHERE "+filter); msg != "" {
			writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not delete the partition: "+msg)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method == http.MethodGet && sub == "data" {
		f.partitionRows(w, r, dataset, base, filter, m) // partitionwrites.go
		return
	}
	if r.Method == http.MethodGet && sub == "" {
		f.partitionTable(w, r, dataset, base, table, filter) // partitionwrites.go
		return
	}
	writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: "+r.Method+" of a partition ("+
		name+"). CloudBurrow reads a partition in a query (SELECT * FROM `"+dataset+"."+table+"`) and with "+
		"tabledata.list and tables.get, and deletes one (tables.delete); nothing else is built (docs/compatibility.md).")
}

// reasonOf is the ErrorProto reason of a refusal of code: bad for a 400.
func reasonOf(code int, bad string) string {
	switch code {
	case http.StatusBadRequest:
		return bad
	case http.StatusNotImplemented:
		return "notImplemented"
	}
	return "internalError"
}
