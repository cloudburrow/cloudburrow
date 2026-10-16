package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Materialized views (#1390).
//
// Measured against the pinned image through the front (2026-10-16): CREATE
// MATERIALIZED VIEW and DROP MATERIALIZED VIEW failed 400 "Statement not
// supported" (the front refused them 501), and tables.insert with a
// materializedView made an ordinary view, its query run at every read. A
// materialized view in BigQuery holds its query's results as of its last
// refresh (https://cloud.google.com/bigquery/docs/materialized-views-intro).
//
// The front makes one as a backing table and a view:
//   - the backing table, mvBacking, is in resultsDataset, the hidden
//     dataset of the query results (results.go), made with CREATE TABLE
//     ... AS the materialized view's query, its results at the refresh;
//   - the materialized view is the engine's view of SELECT * FROM the
//     backing table, whose table resource the front then gives the type
//     MATERIALIZED_VIEW and a materializedView definition (query,
//     enableRefresh, refreshIntervalMs, lastRefreshTime, maxStaleness,
//     allowNonIncrementalDefinition), with the statement's OPTIONS
//     (ddloptions.go) and PARTITION BY and CLUSTER BY (partitionddl.go).
//
// A query of it reads the backing table. BigQuery's query of a
// materialized view returns current data, its cached results combined
// with the base tables' changes, unless max_staleness lets it return the
// last refresh's (#1415). So at each query of one, and each tables.get of
// it (syncMV), the front compares the backing table with its query's
// results now (mvCurrent): when a base table changed since the last
// refresh, it refreshes it if enableRefresh and refreshIntervalMs (30
// minutes by default) has passed since the last refresh, by the front's
// clock, which the admin's clock:advance moves (streamingbuffer.go);
// otherwise a query brings the backing table up to date without a
// refresh, lastRefreshTime kept and the table marked changed (MVChanged)
// so the next due refresh happens, unless the last refresh is within
// maxStaleness, when it reads the last refresh's results. A refresh
// deletes the backing table's rows and inserts its query's again, and sets
// lastRefreshTime: CALL BQ.REFRESH_MATERIALIZED_VIEW, and the automatic
// one above. BigQuery's automatic refresh is best-effort within five
// minutes of a change; the front's happens at the first read or get once
// due. A backing table brought up to date while there was no max_staleness
// stays so should an ALTER then set one.
//
// In a multi-statement script with no TEMP table or function (#1415), a
// CREATE, ALTER or DROP MATERIALIZED VIEW, and CALL
// BQ.REFRESH_MATERIALIZED_VIEW, are run in their turn as lone ones, each a
// child job (scriptflow.go); with a TEMP table or function, the script
// is run as the text it amounts to, each TEMP one kept for the script in
// the hidden dataset (#1434, tempsession.go); in a dry run of such a
// script they are 501 (checkDDL).
//
// ALTER MATERIALIZED VIEW SET OPTIONS sets enable_refresh,
// refresh_interval_minutes, max_staleness and the table options; DROP
// MATERIALIZED VIEW deletes both. tables.insert with a materializedView
// makes one as CREATE does. INFORMATION_SCHEMA.MATERIALIZED_VIEWS lists
// them (infoschemaschemata.go), TABLE_OPTIONS gives their options and
// TABLES their DDL.
//
// The documented limits of a materialized view's query
// (https://cloud.google.com/bigquery/docs/materialized-views-create#query_limitations)
// are checked before anything is made: a non-deterministic function
// always; analytic functions, ORDER BY, LIMIT, ARRAY_AGG, STRING_AGG,
// COUNT(DISTINCT ...) and RIGHT or FULL OUTER JOIN unless
// allow_non_incremental_definition is true (with which BigQuery requires
// max_staleness, and refuses enable_refresh false... it is read as given).
// The wording of each 400 is CloudBurrow's, UNVERIFIED.
//
// Not built: smart tuning (BigQuery's rewriting of a query of a base
// table to read a materialized view instead), an optimization whose
// results are the same. tables.delete of a materialized view leaves its backing
// table in the hidden dataset.

// mvOptions are a materialized view's own options.
type mvOptions struct {
	query          string
	enableRefresh  *bool
	refreshMs      *int64
	staleness      *moreOption
	nonIncremental *bool
	lastRefresh    int64
	// fresh marks a materialized view made now, its definition whole.
	fresh bool
}

// mvDefaultRefreshMs is refresh_interval_minutes' default, 30 minutes.
const mvDefaultRefreshMs = 30 * 60 * 1000

func (m *mvOptions) read(name string, val []token) string {
	isNull := len(val) == 1 && val[0].is("NULL")
	switch name {
	case "enable_refresh", "allow_non_incremental_definition":
		var b *bool
		switch {
		case isNull:
		case len(val) == 1 && (val[0].is("TRUE") || val[0].is("FALSE")):
			v := val[0].is("TRUE")
			b = &v
		default:
			return "the option " + name + " must be a BOOL literal"
		}
		if name == "enable_refresh" {
			m.enableRefresh = b
		} else {
			m.nonIncremental = b
		}
	case "refresh_interval_minutes":
		if isNull {
			v := int64(mvDefaultRefreshMs)
			m.refreshMs = &v
			break
		}
		if len(val) != 1 || val[0].kind != tokWord {
			return "the option refresh_interval_minutes must be a number literal"
		}
		mins, err := strconv.ParseFloat(val[0].text, 64)
		if err != nil || mins < 1 || mins > 10080 {
			return "Invalid value for refresh_interval_minutes: it must be between 1 and 10080 minutes (7 days)"
		}
		v := int64(math.Round(mins * 60000))
		m.refreshMs = &v
	}
	return ""
}

// put sets m in meta, a table resource.
func (m *mvOptions) put(meta map[string]json.RawMessage) {
	def := map[string]any{}
	_ = json.Unmarshal(meta["materializedView"], &def)
	if m.fresh {
		def = map[string]any{"query": m.query, "enableRefresh": true, "refreshIntervalMs": strconv.Itoa(mvDefaultRefreshMs)}
		delete(meta, "view")
		b, _ := json.Marshal("MATERIALIZED_VIEW")
		meta["type"] = b
	}
	if m.lastRefresh != 0 {
		def["lastRefreshTime"] = strconv.FormatInt(m.lastRefresh, 10)
	}
	if m.enableRefresh != nil {
		if *m.enableRefresh {
			def["enableRefresh"] = true
		} else {
			delete(def, "enableRefresh")
		}
	}
	if m.refreshMs != nil {
		def["refreshIntervalMs"] = strconv.FormatInt(*m.refreshMs, 10)
	}
	if m.nonIncremental != nil {
		if *m.nonIncremental {
			def["allowNonIncrementalDefinition"] = true
		} else {
			delete(def, "allowNonIncrementalDefinition")
		}
	}
	if m.staleness != nil { // the table's maxStaleness, as BigQuery has it now
		delete(def, "maxStaleness")
		if s, ok := m.staleness.value.(string); ok {
			b, _ := json.Marshal(s)
			meta["maxStaleness"] = b
		} else {
			delete(meta, "maxStaleness")
		}
	}
	b, _ := json.Marshal(def)
	meta["materializedView"] = b
}

// mvBacking is the name of the backing table of dataset.table (above).
func mvBacking(dataset, table string) string {
	return scratchPrefix + "mv_" + dataset + "__" + table
}

// mvNonDeterministic are the functions a materialized view's query cannot
// call.
var mvNonDeterministic = map[string]bool{"CURRENT_TIMESTAMP": true, "CURRENT_DATE": true, "CURRENT_TIME": true,
	"CURRENT_DATETIME": true, "RAND": true, "GENERATE_UUID": true, "SESSION_USER": true, "NOW": true}

// mvQueryLimit returns why BigQuery refuses query as a materialized view's
// (above), or "".
func mvQueryLimit(query string, nonIncremental bool) string {
	t, ok := lex(query)
	if !ok {
		return ""
	}
	reads := false
	for i, tok := range t {
		if tok.kind != tokWord {
			continue
		}
		up := strings.ToUpper(tok.text)
		if mvNonDeterministic[up] {
			return fmt.Sprintf("Materialized view query contains unsupported feature: the non-deterministic function %s.", up)
		}
		if up == "FROM" || up == "JOIN" {
			reads = true
		}
		if nonIncremental {
			continue
		}
		next := func(word string) bool { return i+1 < len(t) && t[i+1].is(word) }
		switch {
		case up == "OVER" && i > 0 && t[i-1].punct(")"):
			return "Materialized view query contains unsupported feature: analytic functions. " +
				"Set allow_non_incremental_definition to true to use them."
		case up == "ORDER" && next("BY"), up == "LIMIT":
			return fmt.Sprintf("Materialized view query contains unsupported feature: %s. "+
				"Set allow_non_incremental_definition to true to use it.", map[bool]string{true: "ORDER BY", false: "LIMIT"}[up == "ORDER"])
		case (up == "ARRAY_AGG" || up == "STRING_AGG") && next("("):
			return "Materialized view query contains unsupported aggregate function " + up + ". " +
				"Set allow_non_incremental_definition to true to use it."
		case up == "COUNT" && i+2 < len(t) && t[i+1].punct("(") && t[i+2].is("DISTINCT"):
			return "Materialized view query contains unsupported feature: COUNT(DISTINCT). Use APPROX_COUNT_DISTINCT, " +
				"or set allow_non_incremental_definition to true."
		case (up == "RIGHT" || up == "FULL") && (next("JOIN") || next("OUTER")):
			return "Materialized view query contains unsupported feature: " + up + " OUTER JOIN. " +
				"Set allow_non_incremental_definition to true to use it."
		}
	}
	if !reads {
		return "Materialized view query must reference at least one base table."
	}
	return ""
}

// mvStatement is a lone CREATE, ALTER or DROP MATERIALIZED VIEW, or a
// CALL BQ.REFRESH_MATERIALIZED_VIEW.
type mvStatement struct {
	kind             string // create, alter, drop, refresh
	path             []string
	replace, ifCheck bool
	d                ddlOptions
	msg              string // a 400's, or 501's with notImpl
	notImpl          bool
}

// parseMVStatement reads sql as one statement of the kinds above.
func parseMVStatement(sql string) (st mvStatement, ok bool) {
	upper := strings.ToUpper(sql)
	if !strings.Contains(upper, "MATERIALIZED") {
		return st, false
	}
	t, lexed := lex(sql)
	if !lexed || len(t) < 3 {
		return st, false
	}
	end := len(t)
	if t[end-1].punct(";") {
		end--
	}
	for _, tok := range t[:end] {
		if tok.punct(";") {
			return st, false
		}
	}
	t = t[:end]
	i := 1
	switch {
	case t[0].is("CALL"):
		parts, next := path(t, 1)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "BQ") || !strings.EqualFold(parts[1], "REFRESH_MATERIALIZED_VIEW") {
			return st, false
		}
		st.kind = "refresh"
		if next+2 < len(t) && t[next].punct("(") && t[len(t)-1].punct(")") && next+2 == len(t)-1 {
			if s, ok := stringLiteral(t[next+1]); ok {
				st.path = strings.Split(s, ".")
				return st, true
			}
		}
		st.msg = "BQ.REFRESH_MATERIALIZED_VIEW takes the materialized view's name as one STRING literal"
		return st, true
	case t[0].is("CREATE"):
		if i+1 < end && t[i].is("OR") && t[i+1].is("REPLACE") {
			st.replace = true
			i += 2
		}
		if i+1 >= end || !t[i].is("MATERIALIZED") || !t[i+1].is("VIEW") {
			return st, false
		}
		st.kind = "create"
		// Read as CREATE TABLE ... AS: its PARTITION BY, CLUSTER BY,
		// OPTIONS and query (ddloptions.go).
		text := sql[:t[0].end] + " TABLE " + sql[t[i+1].end:]
		st.d, st.msg, _ = parseDDLOptionsMV(text)
		if st.d.query == "" { // after OPTIONS, which parseDDLOptions stops at
			if tt, ok := lex(text); ok {
				for j := 0; j < len(tt); j++ {
					if tt[j].punct("(") || tt[j].punct("[") {
						j = skipTo(tt, j+1, ")", "]")
						continue
					}
					if tt[j].is("AS") {
						st.d.query = strings.TrimSuffix(strings.TrimSpace(text[tt[j].end:]), ";")
						break
					}
				}
			}
		}
		st.ifCheck = st.d.ifNotExists
		st.path = st.d.path
		if st.d.query == "" && st.msg == "" {
			st.msg = "Syntax error: CREATE MATERIALIZED VIEW without AS and its query"
		}
		st.notImpl = st.d.unsupported != ""
		if st.msg == "" {
			st.msg = st.d.invalid
		}
		return st, len(st.path) > 0
	case t[0].is("ALTER"), t[0].is("DROP"):
		if i+1 >= end || !t[i].is("MATERIALIZED") || !t[i+1].is("VIEW") {
			return st, false
		}
		i += 2
		if i+1 < end && t[i].is("IF") && t[i+1].is("EXISTS") {
			st.ifCheck = true
			i += 2
		}
		var next int
		st.path, next = path(t, i)
		if len(st.path) == 0 {
			return st, false
		}
		if t[0].is("DROP") {
			st.kind = "drop"
			if next != end {
				st.msg = "Syntax error: unexpected text after DROP MATERIALIZED VIEW's name"
			}
			return st, true
		}
		st.kind = "alter"
		if next+2 >= end || !t[next].is("SET") || !t[next+1].is("OPTIONS") || !t[next+2].punct("(") ||
			skipTo(t, next+3, ")") != end-1 {
			st.msg, st.notImpl = "ALTER MATERIALIZED VIEW other than SET OPTIONS", true
			return st, true
		}
		st.d.alter, st.d.mv = true, &mvOptions{}
		st.msg = st.d.readOptions(sql, t[next+3:end-1])
		st.notImpl = st.d.unsupported != ""
		if st.msg == "" {
			st.msg = st.d.invalid
		}
		return st, true
	}
	return st, false
}

// isMVBody reports whether body, a statement of the script sql, is a
// CREATE, ALTER or DROP MATERIALIZED VIEW, which a script's run carries
// out in its turn through runQuery (#1415).
func isMVBody(sql string, body []token) bool {
	if len(body) < 3 || !body[0].is("CREATE") && !body[0].is("ALTER") && !body[0].is("DROP") {
		return false
	}
	st, ok := parseMVStatement(sql[body[0].pos:body[len(body)-1].end])
	return ok && st.kind != "refresh"
}

// parseDDLOptionsMV is parseDDLOptions of text, a CREATE TABLE written for
// a CREATE MATERIALIZED VIEW, its options read as a materialized view's.
func parseDDLOptionsMV(text string) (ddlOptions, string, bool) {
	return parseDDLOptionsOf(text, true)
}

// serveMV carries out st (above), a lone statement of q.
func (f front) serveMV(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool, st mvStatement) {
	project := projectOf(f.base)
	failed := func(code int, reason, msg string) {
		f.failBeforeRun(w, r, q, insert, "_cloudburrow", code, rowError{Reason: reason, Message: msg})
	}
	if st.msg != "" {
		if st.notImpl {
			writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: "+st.msg+
				". CloudBurrow sets a materialized view's description, friendly_name, labels, expiration_timestamp, "+
				"kms_key_name (stored), enable_refresh, refresh_interval_minutes, max_staleness and "+
				"allow_non_incremental_definition. Nothing was run.")
			return
		}
		failed(http.StatusBadRequest, "invalidQuery", st.msg)
		return
	}
	ds, table, ok := tableOf(q, st.path)
	if !ok || len(st.path) > 3 || len(st.path) == 3 && st.path[0] != project {
		failed(http.StatusBadRequest, "invalidQuery", "Table \""+strings.Join(st.path, ".")+
			"\" must be qualified with a dataset (e.g. dataset.table).")
		return
	}
	name := project + ":" + ds + "." + table
	done := func(statementType, op string) {
		ddl := &ownDDL{target: tableRef{ProjectID: project, DatasetID: ds, TableID: table}, operation: op,
			statementType: statementType}
		if !setQueryText(r, insert, "DROP TABLE IF EXISTS "+quotePath([]string{"_cloudburrow", scratchTable()})) {
			writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query")
			return
		}
		f.forward(w, r, jobText{query: q.Query, ddl: ddl})
	}
	status, got := f.get(r, tablePath(ds, table))
	var meta struct {
		Type string `json:"type"`
	}
	exists := status == http.StatusOK && json.Unmarshal(got, &meta) == nil
	isMV := exists && meta.Type == "MATERIALIZED_VIEW"
	if st.kind != "create" && !isMV {
		switch {
		case !exists && st.ifCheck:
			done(map[string]string{"drop": "DROP_MATERIALIZED_VIEW", "alter": "ALTER_MATERIALIZED_VIEW"}[st.kind], "SKIP")
		case !exists:
			failed(http.StatusNotFound, "notFound", "Not found: Table "+name)
		default:
			failed(http.StatusBadRequest, "invalidQuery", name+" is not a materialized view; it is a "+meta.Type+".")
		}
		return
	}
	switch st.kind {
	case "drop":
		var code int
		var msg string
		f.keepingDrop(r, ds, table, func() { code, msg = f.dropMV(r, ds, table) }) // #1483, dropped.go
		if code != 0 {
			failed(code, "backendError", msg)
			return
		}
		done("DROP_MATERIALIZED_VIEW", "DROP")
	case "refresh":
		if code, msg := f.refreshMV(r, ds, table); code != 0 {
			failed(code, "invalidQuery", msg)
			return
		}
		done("CALL", "")
	case "alter":
		if st.d.mv.refreshMs == nil && st.d.mv.enableRefresh == nil && st.d.mv.staleness == nil && st.d.mv.nonIncremental == nil &&
			!st.d.sets() {
			done("ALTER_MATERIALIZED_VIEW", "")
			return
		}
		if code, msg := f.mvOptions(r, q, ds, table, &st.d); code != 0 {
			failed(code, "invalidQuery", msg)
			return
		}
		done("ALTER_MATERIALIZED_VIEW", "")
	case "create":
		if msg := unqualifiedViewTable(st.d.query); msg != "" { // #1035, viewnames.go
			failed(http.StatusBadRequest, "invalid", msg)
			return
		}
		if exists && st.ifCheck {
			done("CREATE_MATERIALIZED_VIEW", "SKIP")
			return
		}
		if exists && !st.replace {
			failed(http.StatusConflict, "duplicate", "Already Exists: Table "+name)
			return
		}
		if exists {
			if !isMV {
				failed(http.StatusBadRequest, "invalidQuery", name+" is not a materialized view; it is a "+meta.Type+
					". CREATE OR REPLACE MATERIALIZED VIEW replaces only a materialized view.")
				return
			}
			if code, msg := f.dropMV(r, ds, table); code != 0 {
				failed(code, "backendError", msg)
				return
			}
		}
		if code, msg := f.createMV(r, q, ds, table, &st.d); code != 0 {
			reason := "invalidQuery"
			if code == http.StatusNotFound {
				reason = "notFound"
			}
			failed(code, reason, msg)
			return
		}
		op := "CREATE"
		if exists {
			op = "REPLACE"
		}
		done("CREATE_MATERIALIZED_VIEW", op)
	}
}

// createMV makes the materialized view dataset.table of d's query and
// options (above).
func (f front) createMV(r *http.Request, q queryOptions, dataset, table string, d *ddlOptions) (int, string) {
	if d.mv == nil {
		d.mv = &mvOptions{}
	}
	d.mv.query, d.mv.fresh = strings.TrimSpace(d.query), true
	if msg := checkTableID(table); msg != "" {
		return http.StatusBadRequest, msg
	}
	if msg := mvQueryLimit(d.mv.query, d.mv.nonIncremental != nil && *d.mv.nonIncremental); msg != "" {
		return http.StatusBadRequest, msg
	}
	if st, _ := f.get(r, "/datasets/"+url.PathEscape(dataset)); st == http.StatusNotFound {
		return http.StatusNotFound, fmt.Sprintf("Not found: Dataset %s:%s", projectOf(f.base), dataset)
	}
	expiration, set, status, body := f.expirationMillis(r, q, d.expiration)
	if status != http.StatusOK {
		return http.StatusBadRequest, "expiration_timestamp: " + errorMessage(body, status)
	}
	spec, code, msg := f.partitioning(r, q, *d) // partitionddl.go
	if code != 0 {
		return code, msg
	}
	d.spec = spec
	if code, msg := f.evalMore(r, q, d); code != 0 {
		return code, msg
	}
	backing := quotePath([]string{resultsDataset, mvBacking(dataset, table)})
	f.send(r, http.MethodDelete, tablePath(resultsDataset, mvBacking(dataset, table)), nil)
	if code, msg := f.mvQuery(r, dataset, "CREATE TABLE "+backing+" AS "+d.mv.query); code != 0 {
		return code, msg
	}
	view, _ := json.Marshal(map[string]any{
		"tableReference": map[string]string{"projectId": projectOf(f.base), "datasetId": dataset, "tableId": table},
		"view":           map[string]any{"query": "SELECT * FROM `" + projectOf(f.base) + "`." + backing, "useLegacySql": false},
	})
	if st, got := f.send(r, http.MethodPost, "/datasets/"+url.PathEscape(dataset)+"/tables", view); st != http.StatusOK {
		return st, errorMessage(got, st)
	}
	d.mv.lastRefresh = f.buffers.clock().UnixMilli()
	if msg := f.setTableOptions(r, dataset, table, *d, expiration, set); msg != "" {
		return http.StatusInternalServerError, "cloudburrow: the materialized view was made, but not its definition: " + msg
	}
	f.tables.watchTable(streamKey(projectOf(f.base), dataset, table))
	return 0, ""
}

// mvOptions sets d's options (ALTER MATERIALIZED VIEW SET OPTIONS).
func (f front) mvOptions(r *http.Request, q queryOptions, dataset, table string, d *ddlOptions) (int, string) {
	expiration, set, status, body := f.expirationMillis(r, q, d.expiration)
	if status != http.StatusOK {
		return http.StatusBadRequest, "expiration_timestamp: " + errorMessage(body, status)
	}
	if code, msg := f.evalMore(r, q, d); code != 0 {
		return code, msg
	}
	if msg := f.setTableOptions(r, dataset, table, *d, expiration, set); msg != "" {
		return http.StatusInternalServerError, msg
	}
	return 0, ""
}

// evalMore evaluates the options of d the engine must (max_staleness).
func (f front) evalMore(r *http.Request, q queryOptions, d *ddlOptions) (int, string) {
	opts := []*moreOption{}
	for i := range d.more {
		opts = append(opts, &d.more[i])
	}
	if d.mv != nil && d.mv.staleness != nil {
		opts = append(opts, d.mv.staleness)
	}
	for _, o := range opts {
		if o.eval == "" {
			continue
		}
		v, msg := f.evalString(r, q, o.eval)
		if msg != "" {
			return http.StatusBadRequest, o.name + ": " + msg
		}
		o.value, o.eval = nil, ""
		if v != nil {
			o.value = *v
		}
	}
	return 0, ""
}

// mvQuery runs sql, a statement of the front's, with dataset as its
// default dataset.
func (f front) mvQuery(r *http.Request, dataset, sql string) (int, string) {
	legacy := false
	def, _ := json.Marshal(map[string]string{"projectId": projectOf(f.base), "datasetId": dataset})
	body, err := json.Marshal(queryOptions{Query: sql, UseLegacySQL: &legacy, DefaultDataset: def})
	if err != nil {
		return http.StatusInternalServerError, err.Error()
	}
	st, got := f.send(r, http.MethodPost, "/queries", body)
	if st != http.StatusOK {
		return http.StatusBadRequest, errorMessage(got, st)
	}
	return 0, ""
}

// dropMV deletes the materialized view dataset.table and its backing table.
func (f front) dropMV(r *http.Request, dataset, table string) (int, string) {
	if st, got := f.send(r, http.MethodDelete, tablePath(dataset, table), nil); st/100 != 2 && st != http.StatusNotFound {
		return st, errorMessage(got, st)
	}
	f.send(r, http.MethodDelete, tablePath(resultsDataset, mvBacking(dataset, table)), nil)
	return 0, ""
}

// refreshMV runs dataset.table's query into its backing table again.
func (f front) refreshMV(r *http.Request, dataset, table string) (int, string) {
	st, got := f.get(r, tablePath(dataset, table))
	var meta map[string]json.RawMessage
	if st != http.StatusOK || json.Unmarshal(got, &meta) != nil {
		return st, errorMessage(got, st)
	}
	var def struct {
		Query string `json:"query"`
	}
	if json.Unmarshal(meta["materializedView"], &def) != nil || def.Query == "" {
		return http.StatusBadRequest, "Not a materialized view: " + dataset + "." + table
	}
	backing := quotePath([]string{resultsDataset, mvBacking(dataset, table)})
	if code, msg := f.mvQuery(r, dataset, "DELETE FROM "+backing+" WHERE TRUE"); code != 0 {
		return code, msg
	}
	if code, msg := f.mvQuery(r, dataset, "INSERT INTO "+backing+" "+def.Query); code != 0 {
		return code, msg
	}
	m := mvOptions{lastRefresh: f.buffers.clock().UnixMilli()}
	m.put(meta)
	body, _ := json.Marshal(meta)
	if st, got := f.send(r, http.MethodPut, tablePath(dataset, table), body); st != http.StatusOK {
		return http.StatusInternalServerError, errorMessage(got, st)
	}
	var created string
	_ = json.Unmarshal(meta["creationTime"], &created)
	f.tables.setMVChanged(streamKey(projectOf(f.base), dataset, table), created, false)
	return 0, ""
}

// syncMV brings dataset.table, a materialized view whose resource is
// meta, to what BigQuery would give (#1415, above): when its base tables
// changed since its last refresh, it is refreshed if enableRefresh and
// its refresh interval has passed; otherwise, for a read, its backing
// table is brought up to date, as BigQuery's read of the base tables
// would, unless its last refresh is within maxStaleness.
func (f front) syncMV(r *http.Request, dataset, table string, meta map[string]any, read bool) {
	def, _ := meta["materializedView"].(map[string]any)
	if def == nil || str(def, "query") == "" {
		return
	}
	key := streamKey(projectOf(f.base), dataset, table)
	created := str(meta, "creationTime")
	same, ok := f.mvCurrent(r, dataset, table, str(def, "query"))
	if !ok {
		return
	}
	pending := f.tables.get(key, created).MVChanged
	if same && !pending {
		return
	}
	now := f.buffers.clock().UnixMilli()
	last, _ := strconv.ParseInt(str(def, "lastRefreshTime"), 10, 64)
	every, err := strconv.ParseInt(str(def, "refreshIntervalMs"), 10, 64)
	if err != nil || every <= 0 {
		every = mvDefaultRefreshMs
	}
	if def["enableRefresh"] == true && now-last >= every {
		f.refreshMV(r, dataset, table)
		return
	}
	if !read {
		return
	}
	if ms, ok := intervalMillis(str(meta, "maxStaleness")); ok && now-last <= ms {
		return // within max_staleness: the last refresh's results
	}
	if !same {
		backing := quotePath([]string{resultsDataset, mvBacking(dataset, table)})
		if code, _ := f.mvQuery(r, dataset, "DELETE FROM "+backing+" WHERE TRUE"); code != 0 {
			return
		}
		if code, _ := f.mvQuery(r, dataset, "INSERT INTO "+backing+" "+str(def, "query")); code != 0 {
			return
		}
	}
	f.tables.setMVChanged(key, created, true)
}

// mvCurrent reports whether dataset.table's backing table holds query's
// results now: the same rows, by their count and two sums of their
// fingerprints (ok false when that could not be found).
func (f front) mvCurrent(r *http.Request, dataset, table, query string) (same, ok bool) {
	print := func(from string) string {
		return "TO_JSON_STRING((SELECT AS STRUCT COUNT(*) AS n, BIT_XOR(fp) AS x, SUM(MOD(fp, 1000000007)) AS s FROM " +
			"(SELECT FARM_FINGERPRINT(TO_JSON_STRING(t)) AS fp FROM (" + from + ") AS t)))"
	}
	backing := "SELECT * FROM `" + projectOf(f.base) + "`." + quotePath([]string{resultsDataset, mvBacking(dataset, table)})
	def, _ := json.Marshal(map[string]string{"projectId": projectOf(f.base), "datasetId": dataset})
	v, msg := f.evalString(r, queryOptions{DefaultDataset: def}, "CAST("+print(query)+" = "+print(backing)+" AS STRING)")
	if msg != "" || v == nil {
		return false, false
	}
	return *v == "true", true
}

// intervalMillis is s, an INTERVAL in BigQuery's canonical form "Y-M D
// H:M:S[.F]" (a month taken as 30 days), in milliseconds.
func intervalMillis(s string) (int64, bool) {
	f := strings.Fields(s)
	if len(f) != 3 {
		return 0, false
	}
	ym := strings.SplitN(f[0], "-", 2)
	if strings.HasPrefix(f[0], "-") {
		return 0, false
	}
	hms := strings.Split(f[2], ":")
	if len(ym) != 2 || len(hms) != 3 {
		return 0, false
	}
	y, e1 := strconv.ParseInt(ym[0], 10, 64)
	mo, e2 := strconv.ParseInt(ym[1], 10, 64)
	d, e3 := strconv.ParseInt(f[1], 10, 64)
	h, e4 := strconv.ParseInt(hms[0], 10, 64)
	mi, e5 := strconv.ParseInt(hms[1], 10, 64)
	sec, e6 := strconv.ParseFloat(hms[2], 64)
	for _, e := range []error{e1, e2, e3, e4, e5, e6} {
		if e != nil {
			return 0, false
		}
	}
	days := (y*12+mo)*30 + d
	return ((days*24+h)*60+mi)*60000 + int64(math.Round(sec*1000)), true
}

// syncGot is syncMV of dataset.table, when it is a materialized view, for
// a tables.get of it: its lastRefreshTime as of an automatic refresh due.
func (f front) syncGot(r *http.Request, dataset, table string) {
	st, got := f.get(r, tablePath(dataset, table))
	var meta map[string]any
	if st != http.StatusOK || json.Unmarshal(got, &meta) != nil || str(meta, "type") != "MATERIALIZED_VIEW" {
		return
	}
	f.syncMV(r, dataset, table, meta, false)
}

// insertMV serves tables.insert of body, a table with a materializedView.
func (f front) insertMV(w http.ResponseWriter, r *http.Request, dataset string, raw []byte) {
	var body struct {
		TableReference struct {
			TableID string `json:"tableId"`
		} `json:"tableReference"`
		Description      *string           `json:"description"`
		FriendlyName     *string           `json:"friendlyName"`
		Labels           map[string]string `json:"labels"`
		MaterializedView struct {
			Query                         string `json:"query"`
			EnableRefresh                 *bool  `json:"enableRefresh"`
			RefreshIntervalMs             string `json:"refreshIntervalMs"`
			MaxStaleness                  string `json:"maxStaleness"`
			AllowNonIncrementalDefinition *bool  `json:"allowNonIncrementalDefinition"`
		} `json:"materializedView"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	table := body.TableReference.TableID
	if st, _ := f.get(r, tablePath(dataset, table)); st == http.StatusOK {
		writeError(w, http.StatusConflict, "duplicate", "Already Exists: Table "+projectOf(f.base)+":"+dataset+"."+table)
		return
	}
	mv := body.MaterializedView
	d := ddlOptions{query: mv.Query, description: body.Description, friendlyName: body.FriendlyName,
		mv: &mvOptions{enableRefresh: mv.EnableRefresh, nonIncremental: mv.AllowNonIncrementalDefinition}}
	if body.Labels != nil {
		d.labels, d.hasLabels = body.Labels, true
	}
	if mv.RefreshIntervalMs != "" {
		ms, err := strconv.ParseInt(mv.RefreshIntervalMs, 10, 64)
		if err != nil || ms < 60000 || ms > 7*86400000 {
			writeError(w, http.StatusBadRequest, "invalid", "Invalid refreshIntervalMs: it must be between 1 minute and 7 days")
			return
		}
		d.mv.refreshMs = &ms
	}
	if mv.MaxStaleness != "" {
		d.mv.staleness = &moreOption{name: "max_staleness", value: mv.MaxStaleness}
	}
	if code, msg := f.createMV(r, queryOptions{}, dataset, table, &d); code != 0 {
		reason := "invalid"
		if code == http.StatusNotFound {
			reason = "notFound"
		}
		writeError(w, code, reason, msg)
		return
	}
	st, got := f.get(r, tablePath(dataset, table))
	writeRaw(w, st, got)
}

// deleteWatched serves tables.delete of a table the front watches: a
// materialized view's backing table goes with it.
func (f front) deleteWatched(w http.ResponseWriter, r *http.Request, dataset, table string) {
	st, got := f.get(r, tablePath(dataset, table))
	var meta struct {
		Type string `json:"type"`
	}
	if st != http.StatusOK || json.Unmarshal(got, &meta) != nil || meta.Type != "MATERIALIZED_VIEW" {
		f.deleteTableKeeping(w, r, dataset, table) // #1483: a watched table is kept too (dropped.go)
		return
	}
	rec := newRecorder()
	f.keepingDrop(r, dataset, table, func() { f.next.ServeHTTP(rec, r) }) // #1483, dropped.go
	if rec.status/100 == 2 || rec.status == 0 {
		f.send(r, http.MethodDelete, tablePath(resultsDataset, mvBacking(dataset, table)), nil)
	}
	rec.copyTo(w)
}
