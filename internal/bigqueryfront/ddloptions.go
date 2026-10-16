package bigqueryfront

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
)

// A table's or a view's OPTIONS in DDL (#1251).
//
// Measured against the pinned image through the front (2026-10-15): CREATE
// TABLE, CREATE TABLE ... AS SELECT and CREATE VIEW (with or without a
// column list) with OPTIONS(description=..., friendly_name=..., labels=[...],
// expiration_timestamp=...) made the table or view, and tables.get read
// none of the four back; ALTER VIEW ... SET OPTIONS failed 400 "Statement
// not supported: AlterViewStatement". The emulator makes a table's metadata from the engine's
// table spec, its name and columns (and a view's query), never its options
// (server/handler.go addTableMetadata). BigQuery keeps them
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language#table_option_list,
// #view_option_list).
//
// A query that is one CREATE [OR REPLACE] TABLE or VIEW statement, of any
// form the front carries out (AS SELECT, a column list, LIKE, COPY), or
// one ALTER VIEW ... SET OPTIONS, with any of the four, is
// run as it would be, and then the table's metadata is read, given the
// options as BigQuery sets them and written back with the emulator's
// tables.update, which keeps all four (measured). description,
// friendly_name and labels are read from their literals (as CREATE
// SCHEMA's are, schemaddl.go), expiration_timestamp is evaluated by the
// engine (UNIX_MILLIS of it), so CURRENT_TIMESTAMP() + an interval works.
// IF NOT EXISTS of a table that exists sets nothing, as in BigQuery.
// ALTER sets only the options it names: NULL clears one, labels replaces
// the table's labels. Any other option is 501, nothing run: the emulator
// does not carry out what they do (partitions, encryption, staleness).
// The emulator does not delete a table when its expiration passes.
//
// ALTER TABLE is not read here: altertable.go carries it out (#1227),
// SET OPTIONS among the rest, alone and in a script.
//
// In a multi-statement script with no TEMP table or function, each such
// statement is set so too (#1390, hasDDLOptions). Not carried out: options
// in a script with a TEMP table or function, which the emulator still
// drops, and a column's OPTIONS in a column definition, but for a NUMERIC
// or BIGNUMERIC column's rounding_mode (#1401, decimalddl.go).

// ddlOptions is a lone DDL statement's table OPTIONS.
type ddlOptions struct {
	alter, view, ifNotExists, ifExists bool
	path                               []string
	description, friendlyName          *string
	clearDescription, clearFriendly    bool
	labels                             map[string]string
	hasLabels                          bool
	expiration                         string // the expression's text; "NULL" clears
	// partitionMs is partition_expiration_days in milliseconds, "NULL"
	// to clear it; kmsKey is kms_key_name (#1390).
	partitionMs     string
	kmsKey          *string
	clearKMS, isKMS bool
	// more are the options of tableoptions.go (#1390), and mv the
	// options of a materialized view (materializedviews.go).
	more []moreOption
	mv   *mvOptions
	// connection is the statement's WITH CONNECTION, a table for Apache
	// Iceberg's (#1427, iceberg.go), or "".
	connection string
	// invalid is why BigQuery refuses an option's value (400), or "".
	invalid string
	// unsupported names the first option the front does not set.
	unsupported string
	// The statement's PARTITION BY and CLUSTER BY (#1272,
	// partitionddl.go), its column list's tokens (after the "(") and its
	// query's text, for their columns' types.
	partition, cluster []token
	columns            []token
	query              string
	spec               partitionSpec
	// decimals are the column list's parameterized NUMERIC and
	// BIGNUMERIC columns and rounding modes (#1401, decimalddl.go).
	decimals []decimalColumn
	// sent is the statement as the engine is sent it, a STRUCT field's
	// OPTIONS (rounding_mode) cut, or "" for the statement as it is
	// (#1409, decimalddl.go).
	sent string
}

// optionsKey marks a request whose options the front is already setting,
// so that runQuery runs its statement as it would without them.
type optionsKey struct{}

// parseDDLOptions reads sql as one CREATE TABLE, CREATE VIEW or ALTER VIEW
// SET OPTIONS statement with table OPTIONS. ok is false for any other
// query (ALTER TABLE among them: altertable.go), and for one with no
// OPTIONS.
func parseDDLOptions(sql string) (d ddlOptions, msg string, ok bool) {
	return parseDDLOptionsOf(sql, false)
}

// parseDDLOptionsOf is parseDDLOptions; with mv, of a materialized view's
// CREATE written as a CREATE TABLE (materializedviews.go).
func parseDDLOptionsOf(sql string, mv bool) (d ddlOptions, msg string, ok bool) {
	if mv {
		d.mv = &mvOptions{}
	}
	t, lexed := lex(sql)
	if !lexed || len(t) < 4 {
		return d, "", false
	}
	end := len(t)
	if t[end-1].punct(";") {
		end--
	}
	for _, tok := range t[:end] {
		if tok.punct(";") {
			return d, "", false
		}
	}
	t = t[:end]
	i := 1
	switch {
	case t[0].is("CREATE"):
		if i+1 < end && t[i].is("OR") && t[i+1].is("REPLACE") {
			i += 2
		}
		if i < end && (t[i].is("TEMP") || t[i].is("TEMPORARY")) {
			return d, "", false // a TEMP table has no tables.get
		}
		if i >= end || !(t[i].is("TABLE") || t[i].is("VIEW")) {
			return d, "", false
		}
		d.view = t[i].is("VIEW")
		i++
		if i < end && t[i].is("FUNCTION") {
			return d, "", false // CREATE TABLE FUNCTION: functionddl.go
		}
		if i+2 < end && t[i].is("IF") && t[i+1].is("NOT") && t[i+2].is("EXISTS") {
			d.ifNotExists = true
			i += 3
		}
	case t[0].is("ALTER"):
		if !t[i].is("VIEW") { // ALTER TABLE: altertable.go (#1227)
			return d, "", false
		}
		d.alter, d.view = true, true
		i++
		if i+1 < end && t[i].is("IF") && t[i+1].is("EXISTS") {
			d.ifExists = true
			i += 2
		}
	default:
		return d, "", false
	}
	d.path, i = path(t, i)
	if len(d.path) == 0 {
		return d, "", false
	}
	if !d.alter && !d.view && i < end && t[i].punct("(") {
		d.columns = t[i+1 : skipTo(t, i+1, ")")]
		var invalid string
		if d.decimals, invalid = readDecimalColumns(d.columns); invalid != "" { // #1401
			d.invalid = invalid
		}
		d.sent = withoutFieldRounding(sql, d.columns) // #1409
	}
	// The OPTIONS at the statement's own level, before its query, and the
	// PARTITION BY and CLUSTER BY before them.
	opt, part, clus := -1, -1, -1
	for j := i; j < end; j++ {
		if t[j].punct("(") || t[j].punct("[") {
			j = skipTo(t, j+1, ")", "]")
			continue
		}
		if !d.alter && t[j].is("AS") {
			d.query = sql[t[j].end:t[end-1].end]
			break
		}
		if !d.alter && !d.view && j+1 < end && t[j+1].is("BY") && (t[j].is("PARTITION") || t[j].is("CLUSTER")) {
			if t[j].is("PARTITION") {
				part = j + 2
			} else {
				clus = j + 2
			}
			j++
			continue
		}
		if !d.alter && !d.view && j+2 < end && t[j].is("WITH") && t[j+1].is("CONNECTION") { // #1427, iceberg.go
			k := j + 3
			if t[j+2].is("DEFAULT") {
				d.connection = "DEFAULT"
			} else {
				var p []string
				p, k = path(t, j+2)
				d.connection = strings.Join(p, ".")
			}
			if d.connection == "" || k > end {
				d.invalid = "WITH CONNECTION takes a connection's name or DEFAULT"
				break
			}
			cut := sql[t[j].pos:t[k-1].end]
			if d.sent == "" {
				d.sent = sql
			}
			d.sent = strings.Replace(d.sent, cut, "", 1)
			j = k - 1
			continue
		}
		if t[j].is("OPTIONS") && j+1 < end && t[j+1].punct("(") {
			opt = j
			break
		}
	}
	// A clause ends at the next one at its level.
	clause := func(from int) []token {
		k := from
		for ; k < end; k++ {
			if t[k].punct("(") || t[k].punct("[") {
				k = skipTo(t, k+1, ")", "]")
				continue
			}
			if t[k].is("CLUSTER") || t[k].is("OPTIONS") || t[k].is("AS") || t[k].is("WITH") {
				break
			}
		}
		return t[from:min(k, end)]
	}
	if part >= 0 {
		d.partition = clause(part)
	}
	if clus >= 0 {
		d.cluster = clause(clus)
	}
	if opt < 0 && (part >= 0 || clus >= 0 || len(d.decimals) > 0 || d.invalid != "") {
		return d, "", true
	}
	if d.alter {
		// ALTER ... SET OPTIONS(...) and nothing else.
		if opt != i+1 || !t[i].is("SET") {
			return d, "", false
		}
		if close := skipTo(t, opt+2, ")"); close != end-1 {
			return d, "", false
		}
	}
	if opt < 0 {
		return d, "", false
	}
	msg = d.readOptions(sql, t[opt+2:skipTo(t, opt+2, ")")])
	if msg == "" && d.invalid == "" {
		d.invalid = d.checkIceberg() // #1427, iceberg.go
	}
	return d, msg, true
}

// readOptions reads name = value, ... into d, and returns why the front
// cannot set them, or "".
func (d *ddlOptions) readOptions(sql string, t []token) string {
	for i := 0; i < len(t); {
		if t[i].kind != tokWord || i+1 >= len(t) || !t[i+1].punct("=") {
			return "OPTIONS that CloudBurrow does not read"
		}
		name := strings.ToLower(t[i].text)
		i += 2
		end := skipTo(t, i, ",")
		if end > len(t) {
			end = len(t)
		}
		val := t[i:end]
		if len(val) == 0 {
			return "OPTIONS that CloudBurrow does not read"
		}
		isNull := len(val) == 1 && val[0].is("NULL")
		switch name {
		case "description", "friendly_name":
			var s string
			ok := isNull
			if !isNull && len(val) == 1 {
				s, ok = stringLiteral(val[0])
			}
			if !ok {
				return fmt.Sprintf("the option %s set to other than a string literal, which CloudBurrow does not read", name)
			}
			switch {
			case name == "description" && isNull:
				d.description, d.clearDescription = nil, true
			case name == "description":
				d.description, d.clearDescription = &s, false
			case isNull:
				d.friendlyName, d.clearFriendly = nil, true
			default:
				d.friendlyName, d.clearFriendly = &s, false
			}
		case "labels":
			d.hasLabels = true
			if isNull {
				d.labels = nil
				break
			}
			labels, ok := labelsLiteral(val)
			if !ok {
				return "the option labels set to other than an array of (key, value) string literals, which " +
					"CloudBurrow does not read"
			}
			d.labels = labels
		case "expiration_timestamp":
			d.expiration = sql[val[0].pos:val[len(val)-1].end]
		case "partition_expiration_days", "kms_key_name":
			if d.view {
				if d.unsupported == "" {
					d.unsupported = name
				}
				break
			}
			if name == "kms_key_name" {
				s, ok := "", isNull
				if !isNull && len(val) == 1 {
					s, ok = stringLiteral(val[0])
				}
				if !ok {
					return "the option kms_key_name set to other than a string literal, which CloudBurrow does not read"
				}
				d.isKMS, d.clearKMS = true, isNull
				if !isNull {
					d.kmsKey = &s
				}
				break
			}
			ms, ok := partitionExpirationMs(val)
			if !ok {
				return "the option partition_expiration_days set to other than a number literal, which CloudBurrow does not read"
			}
			d.partitionMs = ms
		case "enable_refresh", "refresh_interval_minutes", "allow_non_incremental_definition":
			if d.mv == nil {
				if d.unsupported == "" {
					d.unsupported = name
				}
				break
			}
			if msg := d.mv.read(name, val); msg != "" && d.invalid == "" {
				d.invalid = msg
			}
		default:
			switch {
			case d.mv != nil && name == "max_staleness":
				o, msg := readMoreOption(name, val, sql)
				if msg != "" && d.invalid == "" {
					d.invalid = msg
				}
				d.mv.staleness = &o
			case moreOptionNames[name] && !d.view:
				o, msg := readMoreOption(name, val, sql)
				if msg != "" && d.invalid == "" {
					d.invalid = msg
				}
				d.more = append(d.more, o)
			case d.unsupported == "":
				d.unsupported = name
			}
		}
		i = end
		if i < len(t) && t[i].punct(",") {
			i++
		}
	}
	if d.unsupported != "" {
		kind := "table"
		if d.view {
			kind = "view"
		}
		also := ""
		if !d.view {
			also = ", partition_expiration_days, kms_key_name (stored), max_staleness, default_rounding_mode, tags, " +
				"require_partition_filter, enable_change_history, storage_uri, file_format, table_format and privacy_policy"
		}
		return fmt.Sprintf("the %s option %s. CloudBurrow sets a %s's description, friendly_name, labels, "+
			"expiration_timestamp%s from OPTIONS; the emulator behind it does not carry out what %s does", kind,
			d.unsupported, kind, also, d.unsupported)
	}
	return ""
}

// sets reports whether d sets anything.
func (d ddlOptions) sets() bool {
	return d.description != nil || d.clearDescription || d.friendlyName != nil || d.clearFriendly ||
		d.hasLabels || d.expiration != "" || d.partition != nil || d.cluster != nil || d.partitionMs != "" || d.isKMS ||
		len(d.more) > 0 || len(d.decimals) > 0
}

// partitionExpirationMs reads partition_expiration_days, a FLOAT64
// literal (or NULL), as timePartitioning.expirationMs (#1390).
func partitionExpirationMs(val []token) (string, bool) {
	if len(val) == 1 && val[0].is("NULL") {
		return "NULL", true
	}
	if len(val) != 1 || val[0].kind != tokWord {
		return "", false
	}
	days, err := strconv.ParseFloat(val[0].text, 64)
	if err != nil || days < 0 {
		return "", false
	}
	return strconv.FormatInt(int64(math.Round(days*86400000)), 10), true
}

// partitionExpirationRefused is the 400 of partition_expiration_days on a
// table that is not time-partitioned (wording CloudBurrow's, UNVERIFIED).
const partitionExpirationRefused = "partition_expiration_days is only valid for a table partitioned by a date or time column " +
	"or by ingestion time."

// ddlWithOptions carries out d, the lone statement of q (above).
func (f front) ddlWithOptions(w http.ResponseWriter, r *http.Request, q queryOptions, d ddlOptions, insert bool) {
	r = r.WithContext(context.WithValue(r.Context(), optionsKey{}, true))
	ds, table, ok := tableOf(q, d.path)
	if !ok {
		f.runQuery(w, r, q, insert)
		return
	}
	if d.iceberg() { // #1427, iceberg.go
		if msg := f.icebergColumnsOf(r, q, d); msg != "" {
			writeError(w, http.StatusNotImplemented, "notImplemented", msg)
			return
		}
	}
	expiration, set, status, body := f.expirationMillis(r, q, d.expiration)
	if status != http.StatusOK {
		writeError(w, http.StatusBadRequest, "invalidQuery", "expiration_timestamp: "+errorMessage(body, status))
		return
	}
	spec, status, msg := f.partitioning(r, q, d) // #1272, partitionddl.go
	if status != 0 {
		reason := "invalidQuery"
		if status != http.StatusBadRequest {
			reason = "internalError"
		}
		writeError(w, status, reason, msg)
		return
	}
	d.spec = spec
	if d.partitionMs != "" && d.partitionMs != "NULL" && spec.time == nil {
		writeError(w, http.StatusBadRequest, "invalidQuery", partitionExpirationRefused)
		return
	}
	existed := false
	if d.ifNotExists || d.alter {
		st, _ := f.get(r, tablePath(ds, table))
		existed = st == http.StatusOK
	}
	if d.alter && !existed {
		if d.ifExists {
			f.forward(w, r, jobText{}) // the emulator runs it as done
			return
		}
		writeError(w, http.StatusNotFound, "notFound", fmt.Sprintf("Not found: Table %s:%s.%s", projectOf(f.base), ds, table))
		return
	}
	rec := newRecorder()
	client := jobText{}
	if d.alter {
		client.query = q.Query
	}
	if d.alter {
		// The emulator refuses ALTER VIEW (measured: 400 "Statement not
		// supported: AlterViewStatement") and answers ALTER TABLE ... SET
		// OPTIONS as done, changing nothing: an ALTER TABLE of the view's
		// path is sent straight to it, and its job is the answer, with the
		// client's text.
		sent := "ALTER TABLE " + quotePath([]string{ds, table}) + " SET OPTIONS(description = NULL)"
		if !setQueryText(r, insert, sent) {
			writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query")
			return
		}
		f.next.ServeHTTP(rec, r)
	} else {
		if d.sent != "" { // #1409, decimalddl.go
			if !setQueryText(r, insert, d.sent) {
				writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query")
				return
			}
			client.query, q.Query = q.Query, d.sent
		}
		f.runQuery(rec, r, q, insert)
	}
	var job map[string]any
	if insert && rec.status == http.StatusOK {
		_ = json.Unmarshal(rec.body.Bytes(), &job)
	}
	if _, failed := queryFailure(rec, job); failed || (d.ifNotExists && existed) {
		f.clientText(w, rec, client)
		return
	}
	for i, o := range d.more { // #1390, tableoptions.go
		if o.eval == "" {
			continue
		}
		v, msg := f.evalString(r, q, o.eval)
		if msg != "" {
			writeError(w, http.StatusBadRequest, "invalidQuery", o.name+": "+msg)
			return
		}
		d.more[i].value, d.more[i].eval = nil, ""
		if v != nil {
			d.more[i].value = *v
		}
	}
	if msg := f.setTableOptions(r, ds, table, d, expiration, set); msg != "" {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: the statement ran, but its "+
			"OPTIONS could not be set: "+msg)
		return
	}
	f.clientText(w, rec, client)
}

// expirationMillis evaluates expr, an expiration_timestamp, as the
// statement's query would (its default dataset and parameters): its
// UNIX_MILLIS, as a string, and whether it sets one. NULL clears it.
func (f front) expirationMillis(r *http.Request, q queryOptions, expr string) (millis string, set bool, status int, body []byte) {
	if expr == "" {
		return "", false, http.StatusOK, nil
	}
	if strings.EqualFold(expr, "NULL") {
		return "", true, http.StatusOK, nil
	}
	legacy := false
	req, err := json.Marshal(queryOptions{Query: "SELECT UNIX_MILLIS(" + expr + ")", UseLegacySQL: &legacy,
		DefaultDataset: q.DefaultDataset, ParameterMode: q.ParameterMode, QueryParameters: q.QueryParameters})
	if err != nil {
		return "", false, http.StatusInternalServerError, nil
	}
	status, got := f.send(r, http.MethodPost, "/queries", req)
	if status != http.StatusOK {
		return "", false, status, got
	}
	var res struct {
		Rows []struct {
			F []struct {
				V *string `json:"v"`
			} `json:"f"`
		} `json:"rows"`
	}
	if json.Unmarshal(got, &res) != nil || len(res.Rows) != 1 || len(res.Rows[0].F) != 1 {
		return "", false, http.StatusInternalServerError, got
	}
	if v := res.Rows[0].F[0].V; v != nil {
		return *v, true, http.StatusOK, nil
	}
	return "", true, http.StatusOK, nil
}

// setTableOptions gives dataset.table d's options, with the emulator's
// tables.update of the table as it is, and returns why it could not, or "".
func (f front) setTableOptions(r *http.Request, dataset, table string, d ddlOptions, expiration string, setExpiration bool) string {
	status, got := f.get(r, tablePath(dataset, table))
	var meta map[string]json.RawMessage
	if status != http.StatusOK || json.Unmarshal(got, &meta) != nil {
		return errorMessage(got, status)
	}
	put := func(key string, v any) {
		b, _ := json.Marshal(v)
		meta[key] = b
	}
	switch {
	case d.description != nil:
		put("description", *d.description)
	case d.clearDescription:
		delete(meta, "description")
	}
	switch {
	case d.friendlyName != nil:
		put("friendlyName", *d.friendlyName)
	case d.clearFriendly:
		delete(meta, "friendlyName")
	}
	if d.hasLabels {
		if len(d.labels) == 0 {
			delete(meta, "labels")
		} else {
			put("labels", d.labels)
		}
	}
	if setExpiration {
		if expiration == "" {
			delete(meta, "expirationTime")
		} else {
			put("expirationTime", expiration)
		}
	}
	for key, v := range map[string]map[string]any{"timePartitioning": d.spec.time,
		"rangePartitioning": d.spec.rangeP, "clustering": d.spec.clustering} {
		if v != nil {
			put(key, v)
		}
	}
	if d.partitionMs != "" { // #1390
		var tp map[string]any
		_ = json.Unmarshal(meta["timePartitioning"], &tp)
		if tp != nil {
			if d.partitionMs == "NULL" {
				delete(tp, "expirationMs")
			} else {
				tp["expirationMs"] = d.partitionMs
			}
			put("timePartitioning", tp)
		}
	}
	switch {
	case d.kmsKey != nil:
		put("encryptionConfiguration", map[string]string{"kmsKeyName": *d.kmsKey})
	case d.clearKMS:
		delete(meta, "encryptionConfiguration")
	}
	if d.connection != "" { // #1427, iceberg.go
		moreOption{key: "biglakeConfiguration", value: map[string]any{"connectionId": d.connection}}.put(meta)
	}
	for _, o := range d.more { // #1390, tableoptions.go
		o.put(meta)
		if o.name == "require_partition_filter" && o.value == true {
			f.tables.watchTable(streamKey(projectOf(f.base), dataset, table))
		}
	}
	if d.mv != nil {
		d.mv.put(meta)
	}
	putDecimalColumns(meta, d.decimals) // #1401, decimalddl.go
	body, err := json.Marshal(meta)
	if err != nil {
		return err.Error()
	}
	if status, got := f.send(r, http.MethodPut, tablePath(dataset, table), body); status != http.StatusOK {
		return errorMessage(got, status)
	}
	var created struct {
		CreationTime string `json:"creationTime"`
	}
	_ = json.Unmarshal(meta["creationTime"], &created.CreationTime)
	for _, o := range d.more {
		if o.setting {
			f.tables.set(streamKey(projectOf(f.base), dataset, table), created.CreationTime, o)
		}
	}
	if uri, ok, _ := icebergConfig(meta["biglakeConfiguration"]); ok && f.icebergs != nil { // #1427
		f.icebergs.add(f, dataset, table, uri, created.CreationTime)
	}
	return ""
}

// hasDDLOptions reports whether body, a statement of the script sql, is a
// CREATE TABLE, CREATE VIEW or ALTER VIEW with table OPTIONS (#1390): the
// emulator drops them from a script it runs, so the script is carried out
// by the front a statement at a time (hasControlFlow), each statement's
// OPTIONS set as a lone statement's are (ddlWithOptions), and one the
// front does not set is 501 there.
func hasDDLOptions(sql string, body []token) bool {
	if len(body) < 4 || !body[0].is("CREATE") && !body[0].is("ALTER") {
		return false
	}
	d, msg, ok := parseDDLOptions(sql[body[0].pos:body[len(body)-1].end])
	// PARTITION BY and CLUSTER BY alone are left as they were.
	return ok && (msg != "" || d.description != nil || d.clearDescription || d.friendlyName != nil || d.clearFriendly || d.hasLabels ||
		d.expiration != "" || d.partitionMs != "" || d.isKMS || len(d.more) > 0 || len(d.decimals) > 0 || d.invalid != "")
}
