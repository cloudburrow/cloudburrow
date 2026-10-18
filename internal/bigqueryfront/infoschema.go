package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A dataset's INFORMATION_SCHEMA TABLES, COLUMNS, COLUMN_FIELD_PATHS,
// TABLE_OPTIONS, ROUTINES and PARAMETERS (#1286), answered as VIEWS is
// (infoschemaviews.go, #1274): each reference becomes a subquery of rows
// with BigQuery's columns and types, read from the dataset's tables.list
// and each table's tables.get, and from the routines the front knows (the
// routines.list it answers, routinedefs.go).
//
// Measured against the pinned image through the front (2026-10-15): the
// engine served TABLES, COLUMNS and TABLE_OPTIONS (and SCHEMATA) with a
// few of BigQuery's columns only (TABLES 7 of 23, creation_time a STRING;
// COLUMNS 10 of 28), TABLE_OPTIONS empty for a table with a description
// and labels, and ROUTINES, PARAMETERS, PARTITIONS and COLUMN_FIELD_PATHS
// 400 "Table not found". In a script the engine's own TABLES, COLUMNS and
// TABLE_OPTIONS are kept, since a statement of the script may change what
// they read.
//
// The columns are BigQuery's documented ones
// (https://cloud.google.com/bigquery/docs/information-schema-tables,
// -columns, -column-field-paths, -table-options, -routines, -parameters),
// with COLUMNS' async_generation_status always NULL (asyncGenerationNone,
// #1458). Not built (501): PARTITIONS and the other dataset views
// below; the region-qualified forms.

// unbuiltInfoSchema are the dataset-scoped views BigQuery documents that
// the front does not answer: a query of one is 501.
var unbuiltInfoSchema = map[string]bool{
	"PARTITIONS": true, "ROUTINE_OPTIONS": true, "MATERIALIZED_VIEWS": true, "TABLE_SNAPSHOTS": true,
	"TABLE_CONSTRAINTS": true, "KEY_COLUMN_USAGE": true, "CONSTRAINT_COLUMN_USAGE": true,
	"SEARCH_INDEXES": true, "SEARCH_INDEX_COLUMNS": true, "SEARCH_INDEX_OPTIONS": true,
	"VECTOR_INDEXES": true, "VECTOR_INDEX_COLUMNS": true, "VECTOR_INDEX_OPTIONS": true,
}

// regionOnlyInfoSchema are the views BigQuery documents region-qualified
// only ("Queries against this view must include a region qualifier",
// https://cloud.google.com/bigquery/docs/information-schema-object-privileges,
// -datasets-schemata-links, and since #1595 -datasets-schemata-options,
// read 2026-10-17, whose scopes are the project's and a region's only): a
// dataset-qualified reference to one is refused 400 invalidQuery (#1458;
// the wording is BigQuery's documented rule, the error's own text
// UNVERIFIED (docs re-read 2026-10-17: <https://docs.cloud.google.com/bigquery/docs/information-schema-object-privileges>)).
// Their region forms are built in infoschemaprivileges.go and
// infoschemaschemata.go.
var regionOnlyInfoSchema = map[string]bool{"OBJECT_PRIVILEGES": true, "SCHEMATA_LINKS": true, "SCHEMATA_OPTIONS": true}

// engineInfoSchema are the views the engine serves itself.
var engineInfoSchema = map[string]bool{"TABLES": true, "COLUMNS": true, "TABLE_OPTIONS": true}

// infoSchemaBuilders build each view's subquery for a dataset.
var infoSchemaBuilders map[string]func(front, *http.Request, string, *datasetMeta) (string, int, string)

func init() {
	infoSchemaBuilders = map[string]func(front, *http.Request, string, *datasetMeta) (string, int, string){
		"VIEWS":              front.viewsRows,
		"TABLES":             tableRowsOf(tablesColumns, tablesRows),
		"COLUMNS":            tableRowsOf(columnsColumns, columnsRows),
		"COLUMN_FIELD_PATHS": tableRowsOf(fieldPathsColumns, fieldPathsRows),
		"TABLE_OPTIONS":      tableRowsOf(tableOptionsColumns, tableOptionsRows),
		"ROUTINES":           routineRowsOf(routinesColumns, routinesRows),
		"PARAMETERS":         routineRowsOf(parametersColumns, parametersRows),
	}
}

// datasetMeta keeps the tables.get resources of each dataset one query
// reads, so two views of one dataset read them once. With only, it reads
// and keeps only the tables only keeps (pushedNameFilter, #1510); with
// types, only the tables tables.list gives one of those types (or none).
type datasetMeta struct {
	tables map[string][]map[string]any
	only   *nameFilter
	types  map[string]bool
}

// viewTableTypes are, by view, the only types of table that have rows
// in it (tables.list's type): a view of them reads no other table's
// tables.get (#1510). A materialized view may be listed as a VIEW.
var viewTableTypes = map[string]map[string]bool{"MATERIALIZED_VIEWS": {"MATERIALIZED_VIEW": true, "VIEW": true}}

// metaFor is the datasetMeta a reference to view reads, filtered by only:
// shared when nothing narrows it, else its own.
func metaFor(shared *datasetMeta, view string, only *nameFilter) *datasetMeta {
	if only == nil && viewTableTypes[view] == nil {
		return shared
	}
	return &datasetMeta{only: only, types: viewTableTypes[view]}
}

// keeps reports whether m reads table.
func (m *datasetMeta) keeps(table string) bool {
	return m == nil || m.only.table(table)
}

// infoTable is one table's tables.get resource.
type infoTable = map[string]any

// tablesOf returns dataset's tables, sorted by name, or the status and
// message of why it cannot.
func (f front) tablesOf(r *http.Request, dataset string, m *datasetMeta) ([]infoTable, int, string) {
	if got, ok := m.tables[dataset]; ok {
		return got, 0, ""
	}
	project := projectOf(f.base)
	if st, _ := f.get(r, "/datasets/"+url.PathEscape(dataset)); st == http.StatusNotFound {
		return nil, http.StatusNotFound, fmt.Sprintf("Not found: Dataset %s:%s", project, dataset)
	}
	var out []infoTable
	token := ""
	for {
		p := "/datasets/" + url.PathEscape(dataset) + "/tables?maxResults=1000"
		if token != "" {
			p += "&pageToken=" + url.QueryEscape(token)
		}
		st, got := f.get(r, p)
		var list struct {
			Tables []struct {
				TableReference struct {
					TableID string `json:"tableId"`
				} `json:"tableReference"`
				Type string `json:"type"`
			} `json:"tables"`
			NextPageToken string `json:"nextPageToken"`
		}
		if st != http.StatusOK || json.Unmarshal(got, &list) != nil {
			return nil, http.StatusInternalServerError, "cloudburrow: could not list the dataset's tables: " + errorMessage(got, st)
		}
		for _, tb := range list.Tables {
			if dataset == resultsDataset && tb.TableReference.TableID == jobsTableName {
				continue // the front's table of JOBS rows (jobstable.go, #1378)
			}
			if !m.keeps(tb.TableReference.TableID) || m.types != nil && tb.Type != "" && !m.types[strings.ToUpper(tb.Type)] {
				continue // no row of it can pass the query's WHERE, or the view has none of its type
			}
			st, got := f.get(r, tablePath(dataset, tb.TableReference.TableID))
			if st == http.StatusNotFound {
				continue // deleted meanwhile
			}
			var md infoTable
			if st != http.StatusOK || json.Unmarshal(got, &md) != nil {
				return nil, http.StatusInternalServerError, "cloudburrow: could not read a table: " + errorMessage(got, st)
			}
			md["tableId"] = tb.TableReference.TableID
			f.tables.annotate(project, dataset, md)                              // #1390, tableoptions.go
			if v, _ := md["view"].(map[string]any); v != nil && f.views != nil { // the client's text (views.go, #1014)
				if t, ok := f.views.get(viewKey(project, dataset, tb.TableReference.TableID)); ok && t.engine == v["query"] {
					v["query"] = t.client
				}
			}
			out = append(out, md)
		}
		if list.NextPageToken == "" || list.NextPageToken == token {
			break
		}
		token = list.NextPageToken
	}
	sort.Slice(out, func(i, j int) bool { return str(out[i], "tableId") < str(out[j], "tableId") })
	if m.tables == nil {
		m.tables = map[string][]map[string]any{}
	}
	m.tables[dataset] = out
	return out, 0, ""
}

// rowsSubquery is the subquery of rows (each a list of SQL values) of the
// row type columns.
//
// The engine makes an array literal with a SQLite function of one
// argument per element, and SQLite caps a function's arguments: an
// instance's INFORMATION_SCHEMA.JOBS of 1,630 jobs failed "too many
// arguments on function googlesqlite_make_array" (measured, #1201). So
// the rows are arrays of at most rowsPerArray, joined by UNION ALL.
func rowsSubquery(columns string, rows [][]string) string {
	lits := make([]string, len(rows))
	for i, r := range rows {
		lits[i] = "(" + strings.Join(r, ", ") + ")"
	}
	return literalRows(columns, lits)
}

// literalRows is rowsSubquery of rows already written as STRUCT literals.
func literalRows(columns string, lits []string) string {
	var parts []string
	for len(lits) > rowsPerArray {
		parts = append(parts, "SELECT * FROM UNNEST(ARRAY<"+columns+">["+strings.Join(lits[:rowsPerArray], ", ")+"])")
		lits = lits[rowsPerArray:]
	}
	parts = append(parts, "SELECT * FROM UNNEST(ARRAY<"+columns+">["+strings.Join(lits, ", ")+"])")
	// SQLite also caps the terms of one compound SELECT (500): past
	// termsPerUnion arrays, each run of them is a subquery of its own, so
	// no limit of SQLite depends on the number of rows (#1374).
	for len(parts) > termsPerUnion {
		var next []string
		for len(parts) > 0 {
			n := min(termsPerUnion, len(parts))
			if n == 1 {
				next = append(next, parts[0])
			} else {
				next = append(next, "SELECT * FROM ("+strings.Join(parts[:n], " UNION ALL ")+")")
			}
			parts = parts[n:]
		}
		parts = next
	}
	return "(" + strings.Join(parts, " UNION ALL ") + ")"
}

// rowsPerArray is the most elements rowsSubquery puts in one array
// literal, below SQLite's default cap of a function's arguments (127 in
// releases before 3.48, 1,000 since). termsPerUnion is the most terms it
// puts in one UNION ALL, below SQLite's default cap of a compound
// SELECT's terms (SQLITE_MAX_COMPOUND_SELECT, 500).
const (
	rowsPerArray  = 100
	termsPerUnion = 100
)

func tableRowsOf(columns string, rows func(project, dataset string, t infoTable) [][]string) func(front, *http.Request, string, *datasetMeta) (string, int, string) {
	return func(f front, r *http.Request, dataset string, m *datasetMeta) (string, int, string) {
		tables, st, msg := f.tablesOf(r, dataset, m)
		if st != 0 {
			return "", st, msg
		}
		var all [][]string
		for _, t := range tables {
			all = append(all, rows(projectOf(f.base), dataset, t)...)
		}
		return rowsSubquery(columns, all), 0, ""
	}
}

func routineRowsOf(columns string, rows func(project, dataset string, def map[string]any) [][]string) func(front, *http.Request, string, *datasetMeta) (string, int, string) {
	return func(f front, r *http.Request, dataset string, _ *datasetMeta) (string, int, string) {
		project := projectOf(f.base)
		if st, _ := f.get(r, "/datasets/"+url.PathEscape(dataset)); st == http.StatusNotFound {
			return "", http.StatusNotFound, fmt.Sprintf("Not found: Dataset %s:%s", project, dataset)
		}
		var all [][]string
		for _, def := range f.liveRoutines(r, project, dataset) {
			all = append(all, rows(project, dataset, def)...)
		}
		return rowsSubquery(columns, all), 0, ""
	}
}

// str reads a string field of a resource.
func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

// sqlOrNull is s as a SQL string, or NULL when empty.
func sqlOrNull(s string) string {
	if s == "" {
		return "NULL"
	}
	return sqlString(s)
}

// millisTimestamp is a resource's milliseconds since the epoch (a string
// or number) as a SQL TIMESTAMP, or NULL. The engine gives a table's
// creationTime and lastModifiedTime in milliseconds, as BigQuery does
// (bigquery-emulator patch 0013, #1310).
func millisTimestamp(v any) string {
	var ms int64
	switch x := v.(type) {
	case string:
		n, err := strconv.ParseInt(x, 10, 64)
		if err != nil {
			return "NULL"
		}
		ms = n
	case float64:
		ms = int64(x)
	default:
		return "NULL"
	}
	return "TIMESTAMP_MILLIS(" + strconv.FormatInt(ms, 10) + ")"
}

// tableTypes are TABLES' table_type, by tables.get's type.
var tableTypes = map[string]string{"TABLE": "BASE TABLE", "VIEW": "VIEW", "MATERIALIZED_VIEW": "MATERIALIZED VIEW",
	"EXTERNAL": "EXTERNAL", "SNAPSHOT": "SNAPSHOT"}

const tablesColumns = "STRUCT<table_catalog STRING, table_schema STRING, table_name STRING, table_type STRING, " +
	"managed_table_type STRING, is_insertable_into STRING, is_fine_grained_mutations_enabled STRING, is_typed STRING, " +
	"is_change_history_enabled STRING, creation_time TIMESTAMP, base_table_catalog STRING, base_table_schema STRING, " +
	"base_table_name STRING, snapshot_time_ms TIMESTAMP, replica_source_catalog STRING, replica_source_schema STRING, " +
	"replica_source_name STRING, replication_status STRING, replication_error STRING, ddl STRING, " +
	"default_collation_name STRING, sync_status JSON, upsert_stream_apply_watermark TIMESTAMP>"

func tablesRows(project, dataset string, t infoTable) [][]string {
	typ := tableTypes[str(t, "type")]
	if typ == "" {
		typ = "BASE TABLE"
	}
	base, _ := t["snapshotDefinition"].(map[string]any)
	if base == nil {
		if c, _ := t["cloneDefinition"].(map[string]any); c != nil {
			base, typ = c, "CLONE"
		}
	}
	insertable := "NO"
	if typ == "BASE TABLE" || typ == "CLONE" {
		insertable = "YES"
	}
	managed := "NULL"
	if typ == "BASE TABLE" || typ == "CLONE" {
		managed = sqlString("NATIVE")
	}
	bc, bs, bn, snap := "NULL", "NULL", "NULL", "NULL"
	if base != nil {
		ref, _ := base["baseTableReference"].(map[string]any)
		bc, bs, bn = sqlOrNull(str(ref, "projectId")), sqlOrNull(str(ref, "datasetId")), sqlOrNull(str(ref, "tableId"))
		if ts, err := time.Parse(time.RFC3339Nano, str(base, "snapshotTime")); err == nil {
			snap = "TIMESTAMP_MILLIS(" + strconv.FormatInt(ts.UnixMilli(), 10) + ")"
		}
	}
	return [][]string{{sqlString(project), sqlString(dataset), sqlString(str(t, "tableId")), sqlString(typ), managed,
		sqlString(insertable), sqlString("NO"), sqlString("NO"), sqlString("NO"), millisTimestamp(t["creationTime"]),
		bc, bs, bn, snap, "NULL", "NULL", "NULL", "NULL", "NULL", sqlString(tableDDL(project, dataset, t)),
		sqlOrNull(str(t, "defaultCollation")), "NULL", "NULL"}}
}

// fieldSQLType is the GoogleSQL type of a TableFieldSchema, ARRAY<> for a
// REPEATED one and the parameters of a parameterized type.
func fieldSQLType(fl map[string]any) string {
	typ := strings.ToUpper(str(fl, "type"))
	if n, ok := typeNames[typ]; ok {
		typ = n
	}
	switch typ {
	case "STRUCT":
		var parts []string
		for _, sub := range fieldsOf(fl) {
			parts = append(parts, str(sub, "name")+" "+fieldSQLType(sub))
		}
		typ = "STRUCT<" + strings.Join(parts, ", ") + ">"
	case "RANGE":
		e, _ := fl["rangeElementType"].(map[string]any)
		typ = "RANGE<" + strings.ToUpper(str(e, "type")) + ">"
	case "STRING", "BYTES":
		if n := str(fl, "maxLength"); n != "" {
			typ += "(" + n + ")"
		}
	case "NUMERIC", "BIGNUMERIC":
		if p := str(fl, "precision"); p != "" {
			if s := str(fl, "scale"); s != "" {
				typ += "(" + p + ", " + s + ")"
			} else {
				typ += "(" + p + ")"
			}
		}
	}
	if strings.EqualFold(str(fl, "mode"), "REPEATED") {
		typ = "ARRAY<" + typ + ">"
	}
	return typ
}

// fieldsOf returns a schema's or a RECORD field's fields.
func fieldsOf(m map[string]any) []map[string]any {
	raw, _ := m["fields"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, x := range raw {
		if fl, ok := x.(map[string]any); ok {
			out = append(out, fl)
		}
	}
	return out
}

// policyTags is a field's policy tags as a SQL ARRAY<STRING>.
func policyTags(fl map[string]any) string {
	pt, _ := fl["policyTags"].(map[string]any)
	names, _ := pt["names"].([]any)
	var lits []string
	for _, n := range names {
		if s, ok := n.(string); ok {
			lits = append(lits, sqlString(s))
		}
	}
	return "ARRAY<STRING>[" + strings.Join(lits, ", ") + "]"
}

const columnsColumns = "STRUCT<table_catalog STRING, table_schema STRING, table_name STRING, column_name STRING, " +
	"ordinal_position INT64, is_nullable STRING, data_type STRING, is_generated STRING, generation_expression STRING, " +
	"is_stored STRING, async_generation_status STRUCT<blocking_error STRUCT<message STRING>>, is_hidden STRING, " +
	"is_updatable STRING, is_system_defined STRING, is_partitioning_column STRING, " +
	"clustering_ordinal_position INT64, collation_name STRING, column_default STRING, rounding_mode STRING, " +
	"data_policies ARRAY<STRUCT<name STRING>>, policy_tags ARRAY<STRING>, is_identity STRING, identity_generation STRING, " +
	"identity_start INT64, identity_increment INT64, identity_maximum INT64, identity_minimum INT64, identity_cycle STRING>"

// asyncGenerationNone is COLUMNS' async_generation_status of a column
// that is not an automatically generated embedding column, which BigQuery
// documents as NULL (https://cloud.google.com/bigquery/docs/information-schema-columns).
// The emulator makes no such column (is_generated is always NEVER), so
// every row has it. Of the STRUCT's fields BigQuery names one,
// async_generation_status.blocking_error.message, which the type keeps
// (#1458).
const asyncGenerationNone = "CAST(NULL AS STRUCT<blocking_error STRUCT<message STRING>>)"

func yesNo(b bool) string {
	if b {
		return sqlString("YES")
	}
	return sqlString("NO")
}

func columnsRows(project, dataset string, t infoTable) [][]string {
	schema, _ := t["schema"].(map[string]any)
	tp, _ := t["timePartitioning"].(map[string]any)
	rp, _ := t["rangePartitioning"].(map[string]any)
	partCol := str(tp, "field")
	if rp != nil {
		partCol = str(rp, "field")
	}
	cluster := map[string]int{}
	if c, _ := t["clustering"].(map[string]any); c != nil {
		fs, _ := c["fields"].([]any)
		for i, x := range fs {
			if s, ok := x.(string); ok {
				cluster[strings.ToLower(s)] = i + 1
			}
		}
	}
	head := []string{sqlString(project), sqlString(dataset), sqlString(str(t, "tableId"))}
	var rows [][]string
	for i, fl := range fieldsOf(schema) {
		name := str(fl, "name")
		clus := "NULL"
		if n, ok := cluster[strings.ToLower(name)]; ok {
			clus = strconv.Itoa(n)
		}
		rows = append(rows, append(append([]string{}, head...), sqlString(name), strconv.Itoa(i+1),
			yesNo(!strings.EqualFold(str(fl, "mode"), "REQUIRED")), sqlString(fieldSQLType(fl)), sqlString("NEVER"), "NULL",
			"NULL", asyncGenerationNone, sqlString("NO"), "NULL", sqlString("NO"), yesNo(partCol != "" && strings.EqualFold(partCol, name)), clus,
			sqlOrNull(str(fl, "collation")), sqlOrNull(str(fl, "defaultValueExpression")), sqlOrNull(str(fl, "roundingMode")),
			"ARRAY<STRUCT<name STRING>>[]", policyTags(fl), sqlString("NO"), "NULL", "NULL", "NULL", "NULL", "NULL", "NULL"))
	}
	// An ingestion-time partitioned table's pseudo-columns. Their rows
	// lacked async_generation_status when #1458 added it to the type, so
	// COLUMNS of any dataset holding such a table failed 400 "Array element
	// type STRUCT<STRING, ...> does not coerce" (#1490); TestColumnsRowsArity
	// checks every row against the type.
	if tp != nil && partCol == "" {
		pseudo := [][2]string{{"_PARTITIONTIME", "TIMESTAMP"}}
		if t := strings.ToUpper(str(tp, "type")); t == "" || t == "DAY" {
			pseudo = append(pseudo, [2]string{"_PARTITIONDATE", "DATE"})
		}
		for _, p := range pseudo {
			rows = append(rows, append(append([]string{}, head...), sqlString(p[0]), "NULL", sqlString("YES"), sqlString(p[1]),
				sqlString("NEVER"), "NULL", "NULL", asyncGenerationNone, sqlString("YES"), "NULL", sqlString("YES"), yesNo(p[0] == "_PARTITIONTIME"), "NULL",
				"NULL", "NULL", "NULL", "ARRAY<STRUCT<name STRING>>[]", "ARRAY<STRING>[]", sqlString("NO"), "NULL", "NULL", "NULL",
				"NULL", "NULL", "NULL"))
		}
	}
	return rows
}

const fieldPathsColumns = "STRUCT<table_catalog STRING, table_schema STRING, table_name STRING, column_name STRING, " +
	"field_path STRING, data_type STRING, description STRING, collation_name STRING, rounding_mode STRING, " +
	"data_policies ARRAY<STRUCT<name STRING>>, policy_tags ARRAY<STRING>>"

func fieldPathsRows(project, dataset string, t infoTable) [][]string {
	schema, _ := t["schema"].(map[string]any)
	head := []string{sqlString(project), sqlString(dataset), sqlString(str(t, "tableId"))}
	var rows [][]string
	var walk func(column, prefix string, fl map[string]any)
	walk = func(column, prefix string, fl map[string]any) {
		p := prefix + str(fl, "name")
		rows = append(rows, append(append([]string{}, head...), sqlString(column), sqlString(p), sqlString(fieldSQLType(fl)),
			sqlOrNull(str(fl, "description")), sqlOrNull(str(fl, "collation")), sqlOrNull(str(fl, "roundingMode")),
			"ARRAY<STRUCT<name STRING>>[]", policyTags(fl)))
		for _, sub := range fieldsOf(fl) {
			walk(column, p+".", sub)
		}
	}
	for _, fl := range fieldsOf(schema) {
		walk(str(fl, "name"), "", fl)
	}
	return rows
}

const tableOptionsColumns = "STRUCT<table_catalog STRING, table_schema STRING, table_name STRING, option_name STRING, " +
	"option_type STRING, option_value STRING>"

// tableOptions are a table's options as TABLE_OPTIONS and its DDL give
// them: name, type and value, by name.
func tableOptions(t infoTable) [][3]string {
	var out [][3]string
	add := func(name, typ, value string) { out = append(out, [3]string{name, typ, value}) }
	if d := str(t, "description"); d != "" {
		add("description", "STRING", strconv.Quote(d))
	}
	if ts, err := strconv.ParseInt(str(t, "expirationTime"), 10, 64); err == nil {
		add("expiration_timestamp", "TIMESTAMP", `TIMESTAMP "`+time.UnixMilli(ts).UTC().Format("2006-01-02T15:04:05.000Z")+`"`)
	}
	if n := str(t, "friendlyName"); n != "" {
		add("friendly_name", "STRING", strconv.Quote(n))
	}
	if e, _ := t["encryptionConfiguration"].(map[string]any); str(e, "kmsKeyName") != "" {
		add("kms_key_name", "STRING", strconv.Quote(str(e, "kmsKeyName")))
	}
	if labels, _ := t["labels"].(map[string]any); len(labels) > 0 {
		keys := make([]string, 0, len(labels))
		for k := range labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			v, _ := labels[k].(string)
			parts[i] = "STRUCT(" + strconv.Quote(k) + ", " + strconv.Quote(v) + ")"
		}
		add("labels", "ARRAY<STRUCT<STRING, STRING>>", "["+strings.Join(parts, ", ")+"]")
	}
	tp, _ := t["timePartitioning"].(map[string]any)
	if ms, err := strconv.ParseInt(str(tp, "expirationMs"), 10, 64); err == nil {
		add("partition_expiration_days", "FLOAT64", strconv.FormatFloat(float64(ms)/86400000, 'g', -1, 64))
	}
	if b, _ := t["requirePartitionFilter"].(bool); b || tp["requirePartitionFilter"] == true {
		add("require_partition_filter", "BOOL", "true")
	}
	moreTableOptions(t, add) // #1390, tableoptions.go
	sort.SliceStable(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

func tableOptionsRows(project, dataset string, t infoTable) [][]string {
	var rows [][]string
	for _, o := range tableOptions(t) {
		rows = append(rows, []string{sqlString(project), sqlString(dataset), sqlString(str(t, "tableId")),
			sqlString(o[0]), sqlString(o[1]), sqlString(o[2])})
	}
	return rows
}

// tableDDL is the statement that would make t again, as TABLES' ddl.
func tableDDL(project, dataset string, t infoTable) string {
	name := "`" + project + "." + dataset + "." + str(t, "tableId") + "`"
	var opts []string
	for _, o := range tableOptions(t) {
		opts = append(opts, "  "+o[0]+"="+o[2])
	}
	options := ""
	if len(opts) > 0 {
		options = "\nOPTIONS(\n" + strings.Join(opts, ",\n") + "\n)"
	}
	if v, _ := t["view"].(map[string]any); v != nil {
		return "CREATE VIEW " + name + options + "\nAS " + str(v, "query") + ";"
	}
	if v, _ := t["materializedView"].(map[string]any); v != nil { // #1390
		return "CREATE MATERIALIZED VIEW " + name + options + "\nAS " + str(v, "query") + ";"
	}
	schema, _ := t["schema"].(map[string]any)
	var cols []string
	for _, fl := range fieldsOf(schema) {
		c := "  " + str(fl, "name") + " " + fieldSQLType(fl)
		if strings.EqualFold(str(fl, "mode"), "REQUIRED") {
			c += " NOT NULL"
		}
		if d := str(fl, "defaultValueExpression"); d != "" {
			c += " DEFAULT " + d
		}
		if d := str(fl, "description"); d != "" {
			c += " OPTIONS(description=" + strconv.Quote(d) + ")"
		}
		cols = append(cols, c)
	}
	ddl := "CREATE TABLE " + name + "\n(\n" + strings.Join(cols, ",\n") + "\n)"
	if tp, _ := t["timePartitioning"].(map[string]any); tp != nil {
		unit := strings.ToUpper(str(tp, "type"))
		if unit == "" {
			unit = "DAY"
		}
		field := str(tp, "field")
		switch {
		case field == "" && unit == "DAY":
			ddl += "\nPARTITION BY _PARTITIONDATE"
		case field == "":
			ddl += "\nPARTITION BY TIMESTAMP_TRUNC(_PARTITIONTIME, " + unit + ")"
		default:
			typ := ""
			for _, fl := range fieldsOf(schema) {
				if strings.EqualFold(str(fl, "name"), field) {
					typ = fieldSQLType(fl)
				}
			}
			switch {
			case typ == "DATE" && unit == "DAY":
				ddl += "\nPARTITION BY " + field
			case typ == "DATE":
				ddl += "\nPARTITION BY DATE_TRUNC(" + field + ", " + unit + ")"
			case unit == "DAY":
				ddl += "\nPARTITION BY DATE(" + field + ")"
			default:
				ddl += "\nPARTITION BY " + typ + "_TRUNC(" + field + ", " + unit + ")"
			}
		}
	}
	if rp, _ := t["rangePartitioning"].(map[string]any); rp != nil {
		rg, _ := rp["range"].(map[string]any)
		ddl += "\nPARTITION BY RANGE_BUCKET(" + str(rp, "field") + ", GENERATE_ARRAY(" + fmt.Sprint(rg["start"]) + ", " +
			fmt.Sprint(rg["end"]) + ", " + fmt.Sprint(rg["interval"]) + "))"
	}
	if c, _ := t["clustering"].(map[string]any); c != nil {
		fs, _ := c["fields"].([]any)
		var names []string
		for _, x := range fs {
			names = append(names, fmt.Sprint(x))
		}
		if len(names) > 0 {
			ddl += "\nCLUSTER BY " + strings.Join(names, ", ")
		}
	}
	return ddl + options + ";"
}

// standardType is a StandardSqlDataType's GoogleSQL name.
func standardType(t map[string]any) string {
	if t == nil {
		return ""
	}
	kind := strings.ToUpper(str(t, "typeKind"))
	switch kind {
	case "ARRAY":
		e, _ := t["arrayElementType"].(map[string]any)
		return "ARRAY<" + standardType(e) + ">"
	case "STRUCT":
		st, _ := t["structType"].(map[string]any)
		raw, _ := st["fields"].([]any)
		var parts []string
		for _, x := range raw {
			fl, _ := x.(map[string]any)
			ft, _ := fl["type"].(map[string]any)
			parts = append(parts, strings.TrimSpace(str(fl, "name")+" "+standardType(ft)))
		}
		return "STRUCT<" + strings.Join(parts, ", ") + ">"
	case "RANGE":
		e, _ := t["rangeElementType"].(map[string]any)
		return "RANGE<" + standardType(e) + ">"
	}
	return kind
}

// routineTypes are ROUTINES' routine_type, by the Routine's routineType.
var routineTypes = map[string]string{"SCALAR_FUNCTION": "FUNCTION", "AGGREGATE_FUNCTION": "AGGREGATE FUNCTION",
	"PROCEDURE": "PROCEDURE", "TABLE_VALUED_FUNCTION": "TABLE FUNCTION"}

// returnType is a routine's return type: its returnType, or TABLE<...>
// for a table function, or "" for a procedure.
func returnType(def map[string]any) string {
	if rt, _ := def["returnType"].(map[string]any); rt != nil {
		return standardType(rt)
	}
	if rt, _ := def["returnTableType"].(map[string]any); rt != nil {
		raw, _ := rt["columns"].([]any)
		var parts []string
		for _, x := range raw {
			c, _ := x.(map[string]any)
			ct, _ := c["type"].(map[string]any)
			parts = append(parts, str(c, "name")+" "+standardType(ct))
		}
		return "TABLE<" + strings.Join(parts, ", ") + ">"
	}
	return ""
}

// argumentType is an argument's type, ANY TYPE for a templated one.
func argumentType(a map[string]any) string {
	if strings.EqualFold(str(a, "argumentKind"), "ANY_TYPE") {
		return "ANY TYPE"
	}
	dt, _ := a["dataType"].(map[string]any)
	return standardType(dt)
}

func arguments(def map[string]any) []map[string]any {
	raw, _ := def["arguments"].([]any)
	var out []map[string]any
	for _, x := range raw {
		if a, ok := x.(map[string]any); ok {
			out = append(out, a)
		}
	}
	return out
}

const routinesColumns = "STRUCT<specific_catalog STRING, specific_schema STRING, specific_name STRING, routine_catalog STRING, " +
	"routine_schema STRING, routine_name STRING, routine_type STRING, data_type STRING, routine_body STRING, " +
	"routine_definition STRING, external_language STRING, is_deterministic STRING, security_type STRING, " +
	"created TIMESTAMP, last_altered TIMESTAMP, ddl STRING, connection STRING>"

func routinesRows(project, dataset string, def map[string]any) [][]string {
	ref, _ := def["routineReference"].(map[string]any)
	name := str(ref, "routineId")
	kind := str(def, "routineType")
	typ := routineTypes[kind]
	if typ == "" {
		typ = "FUNCTION"
	}
	body, lang := "SQL", "NULL"
	if strings.EqualFold(str(def, "language"), "JAVASCRIPT") {
		body, lang = "EXTERNAL", sqlString("JAVASCRIPT")
	}
	det := "NULL"
	switch str(def, "determinismLevel") {
	case "DETERMINISTIC":
		det = sqlString("YES")
	case "NOT_DETERMINISTIC":
		det = sqlString("NO")
	}
	h := []string{sqlString(project), sqlString(dataset), sqlString(name)}
	return [][]string{append(append(h, h...), sqlString(typ), sqlOrNull(returnType(def)), sqlString(body),
		sqlString(str(def, "definitionBody")), lang, det, "NULL", millisTimestamp(def["creationTime"]),
		millisTimestamp(def["lastModifiedTime"]), sqlString(routineDDL(project, dataset, def)), "NULL")}
}

// routineDDL is the statement that would make the routine again, as
// ROUTINES' ddl.
func routineDDL(project, dataset string, def map[string]any) string {
	ref, _ := def["routineReference"].(map[string]any)
	name := "`" + project + "." + dataset + "." + str(ref, "routineId") + "`"
	var args []string
	for _, a := range arguments(def) {
		s := str(a, "name") + " " + argumentType(a)
		if m := strings.ToUpper(str(a, "mode")); m != "" && str(def, "routineType") == "PROCEDURE" {
			s = m + " " + s
		}
		args = append(args, s)
	}
	head := "(" + strings.Join(args, ", ") + ")"
	body := str(def, "definitionBody")
	switch str(def, "routineType") {
	case "PROCEDURE":
		return "CREATE PROCEDURE " + name + head + "\n" + body + ";"
	case "TABLE_VALUED_FUNCTION":
		return "CREATE TABLE FUNCTION " + name + head + " AS (\n" + body + "\n);"
	}
	ret := ""
	if rt := returnType(def); rt != "" {
		ret = " RETURNS " + rt
	}
	if strings.EqualFold(str(def, "language"), "JAVASCRIPT") {
		return "CREATE FUNCTION " + name + head + ret + " LANGUAGE js AS " + strconv.Quote(body) + ";"
	}
	return "CREATE FUNCTION " + name + head + ret + " AS (\n" + body + "\n);"
}

const parametersColumns = "STRUCT<specific_catalog STRING, specific_schema STRING, specific_name STRING, ordinal_position INT64, " +
	"parameter_mode STRING, is_result STRING, parameter_name STRING, data_type STRING, parameter_default STRING, " +
	"is_aggregate STRING>"

func parametersRows(project, dataset string, def map[string]any) [][]string {
	ref, _ := def["routineReference"].(map[string]any)
	h := []string{sqlString(project), sqlString(dataset), sqlString(str(ref, "routineId"))}
	var rows [][]string
	if rt := returnType(def); rt != "" {
		rows = append(rows, append(append([]string{}, h...), "0", "NULL", sqlString("YES"), "NULL", sqlString(rt), "NULL", "NULL"))
	}
	procedure := strings.EqualFold(str(def, "routineType"), "PROCEDURE")
	for i, a := range arguments(def) {
		// A procedure's argument is IN, OUT or INOUT; a function's is
		// NULL, as the PARAMETERS page's example row of a function
		// argument is (information-schema-parameters, read 2026-10-17;
		// #1595).
		mode := "NULL"
		if procedure {
			m := strings.ToUpper(str(a, "mode"))
			if m == "" {
				m = "IN"
			}
			mode = sqlString(m)
		}
		rows = append(rows, append(append([]string{}, h...), strconv.Itoa(i+1), mode, sqlString("NO"),
			sqlOrNull(str(a, "name")), sqlString(argumentType(a)), "NULL", "NULL"))
	}
	return rows
}
