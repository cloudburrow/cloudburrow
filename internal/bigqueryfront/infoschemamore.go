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

// A dataset's INFORMATION_SCHEMA PARTITIONS, ROUTINE_OPTIONS,
// TABLE_SNAPSHOTS, TABLE_CONSTRAINTS, KEY_COLUMN_USAGE,
// CONSTRAINT_COLUMN_USAGE and the search and vector index views (#1328),
// answered as TABLES is (infoschema.go): each reference becomes a subquery
// with BigQuery's documented columns and types
// (https://cloud.google.com/bigquery/docs/information-schema-partitions,
// -routine-options, -snapshots, -table-constraints, -key-column-usage,
// -constraint-column-usage, -indexes, -index-columns, -index-options,
// -vector-indexes, -vector-index-columns, -vector-index-options).
// Measured first: the engine failed each 400 "Table not found"; the front
// refused them 501.
//
// PARTITIONS has one row per partition that holds rows (and one, its
// partition_id NULL, per table that is not partitioned; a snapshot's too,
// UNVERIFIED), counted by a
// query of the table, its partition_id as the partition decorator names it
// (partitionreads.go: YYYYMMDD by the time unit, an integer range's start,
// __NULL__, __UNPARTITIONED__). Left out, since the emulator keeps none of
// them faithfully: total_logical_bytes, total_billable_bytes and
// last_modified_time (it keeps no time per partition). storage_tier is
// ACTIVE for a table made in the last 90 days (no partition of it can have
// gone 90 days unmodified, https://cloud.google.com/bigquery/pricing#storage),
// and NULL otherwise. An ingestion-time partitioned table's partitions are
// its rows' _PARTITIONTIME, which CloudBurrow's engine keeps (#1277).
//
// The constraint views read each table's tableConstraints: the primary
// key, named "<table>.pk$" as BigQuery names it, and its foreign keys.
// ROUTINE_OPTIONS gives a routine's description and library. The search
// and vector index views are always empty: the instance makes no index
// (CREATE SEARCH INDEX and CREATE VECTOR INDEX fail in the engine).
//
// Not built (501): MATERIALIZED_VIEWS, OBJECT_PRIVILEGES, SCHEMATA_OPTIONS,
// SCHEMATA_LINKS.

func init() {
	for k, v := range map[string]func(front, *http.Request, string, *datasetMeta) (string, int, string){
		"PARTITIONS":              front.partitionsRows,
		"ROUTINE_OPTIONS":         routineRowsOf(routineOptionsColumns, routineOptionsRows),
		"TABLE_SNAPSHOTS":         tableRowsOf(snapshotsColumns, snapshotsRows),
		"TABLE_CONSTRAINTS":       tableRowsOf(tableConstraintsColumns, tableConstraintsRows),
		"KEY_COLUMN_USAGE":        tableRowsOf(keyColumnUsageColumns, keyColumnUsageRows),
		"CONSTRAINT_COLUMN_USAGE": tableRowsOf(constraintColumnUsageColumns, constraintColumnUsageRows),
		"SEARCH_INDEXES":          emptyRows(searchIndexesColumns),
		"SEARCH_INDEX_COLUMNS":    emptyRows(indexColumnsColumns),
		"SEARCH_INDEX_OPTIONS":    emptyRows(indexOptionsColumns),
		"VECTOR_INDEXES":          emptyRows(vectorIndexesColumns),
		"VECTOR_INDEX_COLUMNS":    emptyRows(indexColumnsColumns),
		"VECTOR_INDEX_OPTIONS":    emptyRows(indexOptionsColumns),
	} {
		infoSchemaBuilders[k] = v
		delete(unbuiltInfoSchema, k)
	}
}

// emptyRows is a view the instance never has a row of, of a dataset that
// exists.
func emptyRows(columns string) func(front, *http.Request, string, *datasetMeta) (string, int, string) {
	return func(f front, r *http.Request, dataset string, _ *datasetMeta) (string, int, string) {
		if st, _ := f.get(r, "/datasets/"+url.PathEscape(dataset)); st == http.StatusNotFound {
			return "", http.StatusNotFound, fmt.Sprintf("Not found: Dataset %s:%s", projectOf(f.base), dataset)
		}
		return rowsSubquery(columns, nil), 0, ""
	}
}

const partitionsColumns = "STRUCT<table_catalog STRING, table_schema STRING, table_name STRING, partition_id STRING, " +
	"total_rows INT64, storage_tier STRING>"

// partitionIDExpr is the SQL of a row's partition_id in a table with m, or
// "" for a table that is not partitioned.
func partitionIDExpr(m partitionMeta) (string, bool) {
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
			return "", false
		}
		return fmt.Sprintf("CASE WHEN %s IS NULL THEN '__NULL__' WHEN %s < %d OR %s >= %d THEN '__UNPARTITIONED__' "+
			"ELSE CAST(%d + DIV(%s - %d, %d) * %d AS STRING) END", col, col, start, col, end, start, col, start, step, step), true
	case m.TimePartitioning == nil:
		return "", true
	}
	tp := m.TimePartitioning
	format := map[string]string{"HOUR": "%Y%m%d%H", "MONTH": "%Y%m", "YEAR": "%Y"}[strings.ToUpper(tp.Type)]
	if format == "" {
		format = "%Y%m%d"
	}
	if m.ingestion() {
		// The engine keeps each row's _PARTITIONTIME (#1277).
		return "CASE WHEN _PARTITIONTIME IS NULL THEN '__UNPARTITIONED__' ELSE FORMAT_TIMESTAMP('" + format +
			"', _PARTITIONTIME, 'UTC') END", true
	}
	col := quoteName(tp.Field)
	expr := col
	for _, fl := range m.Schema.Fields {
		if strings.EqualFold(fl.Name, tp.Field) && (strings.EqualFold(fl.Type, "DATE") || strings.EqualFold(fl.Type, "DATETIME")) {
			expr = "TIMESTAMP(" + col + ")"
		}
	}
	return fmt.Sprintf("CASE WHEN %s IS NULL THEN '__NULL__' WHEN %s < TIMESTAMP '1960-01-01' OR %s >= TIMESTAMP '2160-01-01' "+
		"THEN '__UNPARTITIONED__' ELSE FORMAT_TIMESTAMP('%s', %s, 'UTC') END", col, expr, expr, format, expr), true
}

// partitionsRows returns the subquery of dataset's tables' partitions.
func (f front) partitionsRows(r *http.Request, dataset string, m *datasetMeta) (string, int, string) {
	tables, st, msg := f.tablesOf(r, dataset, m)
	if st != 0 {
		return "", st, msg
	}
	project := projectOf(f.base)
	var selects []string
	for _, t := range tables {
		if str(t, "type") != "TABLE" && str(t, "type") != "" {
			continue
		}
		raw, err := json.Marshal(t)
		var pm partitionMeta
		if err != nil || json.Unmarshal(raw, &pm) != nil {
			return "", http.StatusInternalServerError, "cloudburrow: could not read a table's partitioning"
		}
		name := str(t, "tableId")
		id, ok := partitionIDExpr(pm)
		if !ok {
			return "", http.StatusInternalServerError, "cloudburrow: could not read the range partitioning of " + name
		}
		tier := "CAST(NULL AS STRING)"
		if ms, err := strconv.ParseInt(fmt.Sprint(t["creationTime"]), 10, 64); err == nil && time.Since(time.UnixMilli(ms)) < 90*24*time.Hour {
			tier = "'ACTIVE'"
		}
		head := sqlString(project) + " AS table_catalog, " + sqlString(dataset) + " AS table_schema, " + sqlString(name) +
			" AS table_name, "
		from := " FROM `" + project + "." + dataset + "." + name + "`"
		if id == "" {
			selects = append(selects, "SELECT "+head+"CAST(NULL AS STRING) AS partition_id, COUNT(*) AS total_rows, "+tier+
				" AS storage_tier"+from)
		} else {
			selects = append(selects, "SELECT "+head+id+" AS partition_id, COUNT(*) AS total_rows, "+tier+" AS storage_tier"+
				from+" GROUP BY 1, 2, 3, 4, 6")
		}
	}
	if len(selects) == 0 {
		return rowsSubquery(partitionsColumns, nil), 0, ""
	}
	return "(SELECT * FROM (" + strings.Join(selects, " UNION ALL ") + "))", 0, ""
}

const routineOptionsColumns = "STRUCT<specific_catalog STRING, specific_schema STRING, specific_name STRING, " +
	"option_name STRING, option_type STRING, option_value STRING>"

func routineOptionsRows(project, dataset string, def map[string]any) [][]string {
	ref, _ := def["routineReference"].(map[string]any)
	h := []string{sqlString(project), sqlString(dataset), sqlString(str(ref, "routineId"))}
	var rows [][]string
	if d := str(def, "description"); d != "" {
		rows = append(rows, append(append([]string{}, h...), sqlString("description"), sqlString("STRING"), sqlString(strconv.Quote(d))))
	}
	if raw, _ := def["importedLibraries"].([]any); len(raw) > 0 {
		var libs []string
		for _, x := range raw {
			libs = append(libs, strconv.Quote(fmt.Sprint(x)))
		}
		rows = append(rows, append(append([]string{}, h...), sqlString("library"), sqlString("ARRAY<STRING>"),
			sqlString("["+strings.Join(libs, ", ")+"]")))
	}
	return rows
}

const snapshotsColumns = "STRUCT<table_catalog STRING, table_schema STRING, table_name STRING, base_table_catalog STRING, " +
	"base_table_schema STRING, base_table_name STRING, snapshot_time TIMESTAMP>"

func snapshotsRows(project, dataset string, t infoTable) [][]string {
	base, _ := t["snapshotDefinition"].(map[string]any)
	if base == nil {
		return nil
	}
	ref, _ := base["baseTableReference"].(map[string]any)
	snap := "NULL"
	if ts, err := time.Parse(time.RFC3339Nano, str(base, "snapshotTime")); err == nil {
		snap = "TIMESTAMP_MICROS(" + strconv.FormatInt(ts.UnixMicro(), 10) + ")"
	}
	return [][]string{{sqlString(project), sqlString(dataset), sqlString(str(t, "tableId")), sqlOrNull(str(ref, "projectId")),
		sqlOrNull(str(ref, "datasetId")), sqlOrNull(str(ref, "tableId")), snap}}
}

// constraintsOf is a table's primary key columns and foreign keys.
type infoForeignKey struct {
	name                     string
	refProject, refDS, refTb string
	columns, refColumns      []string
}

func constraintsOf(t infoTable) (pk []string, fks []infoForeignKey) {
	tc, _ := t["tableConstraints"].(map[string]any)
	if p, _ := tc["primaryKey"].(map[string]any); p != nil {
		cols, _ := p["columns"].([]any)
		for _, c := range cols {
			pk = append(pk, fmt.Sprint(c))
		}
	}
	raw, _ := tc["foreignKeys"].([]any)
	for _, x := range raw {
		m, _ := x.(map[string]any)
		ref, _ := m["referencedTable"].(map[string]any)
		fk := infoForeignKey{name: str(m, "name"), refProject: str(ref, "projectId"), refDS: str(ref, "datasetId"), refTb: str(ref, "tableId")}
		cr, _ := m["columnReferences"].([]any)
		for _, y := range cr {
			c, _ := y.(map[string]any)
			fk.columns = append(fk.columns, str(c, "referencingColumn"))
			fk.refColumns = append(fk.refColumns, str(c, "referencedColumn"))
		}
		fks = append(fks, fk)
	}
	return pk, fks
}

const tableConstraintsColumns = "STRUCT<constraint_catalog STRING, constraint_schema STRING, constraint_name STRING, " +
	"table_catalog STRING, table_schema STRING, table_name STRING, constraint_type STRING, is_deferrable STRING, " +
	"initially_deferred STRING, enforced STRING>"

func tableConstraintsRows(project, dataset string, t infoTable) [][]string {
	pk, fks := constraintsOf(t)
	name := str(t, "tableId")
	row := func(constraint, typ string) []string {
		return []string{sqlString(project), sqlString(dataset), sqlString(constraint), sqlString(project), sqlString(dataset),
			sqlString(name), sqlString(typ), sqlString("NO"), sqlString("NO"), sqlString("NO")}
	}
	var rows [][]string
	if len(pk) > 0 {
		rows = append(rows, row(name+".pk$", "PRIMARY KEY"))
	}
	for _, fk := range fks {
		rows = append(rows, row(fk.name, "FOREIGN KEY"))
	}
	return rows
}

const keyColumnUsageColumns = "STRUCT<constraint_catalog STRING, constraint_schema STRING, constraint_name STRING, " +
	"table_catalog STRING, table_schema STRING, table_name STRING, column_name STRING, ordinal_position INT64, " +
	"position_in_unique_constraint INT64>"

func keyColumnUsageRows(project, dataset string, t infoTable) [][]string {
	pk, fks := constraintsOf(t)
	name := str(t, "tableId")
	row := func(constraint, column string, i int, unique string) []string {
		return []string{sqlString(project), sqlString(dataset), sqlString(constraint), sqlString(project), sqlString(dataset),
			sqlString(name), sqlString(column), strconv.Itoa(i + 1), unique}
	}
	var rows [][]string
	for i, c := range pk {
		rows = append(rows, row(name+".pk$", c, i, "NULL"))
	}
	for _, fk := range fks {
		for i, c := range fk.columns {
			rows = append(rows, row(fk.name, c, i, strconv.Itoa(i+1)))
		}
	}
	return rows
}

const constraintColumnUsageColumns = "STRUCT<table_catalog STRING, table_schema STRING, table_name STRING, column_name STRING, " +
	"constraint_catalog STRING, constraint_schema STRING, constraint_name STRING>"

// constraintColumnUsageRows: a primary key's columns, and the referenced
// (primary key) table's columns of each foreign key.
func constraintColumnUsageRows(project, dataset string, t infoTable) [][]string {
	pk, fks := constraintsOf(t)
	name := str(t, "tableId")
	var rows [][]string
	for _, c := range pk {
		rows = append(rows, []string{sqlString(project), sqlString(dataset), sqlString(name), sqlString(c), sqlString(project),
			sqlString(dataset), sqlString(name + ".pk$")})
	}
	for _, fk := range fks {
		p, d := fk.refProject, fk.refDS
		if p == "" {
			p = project
		}
		if d == "" {
			d = dataset
		}
		for _, c := range fk.refColumns {
			rows = append(rows, []string{sqlString(p), sqlString(d), sqlString(fk.refTb), sqlString(c), sqlString(project),
				sqlString(dataset), sqlString(fk.name)})
		}
	}
	return rows
}

const searchIndexesColumns = "STRUCT<index_catalog STRING, index_schema STRING, table_name STRING, index_name STRING, " +
	"index_status STRING, creation_time TIMESTAMP, last_modification_time TIMESTAMP, last_refresh_time TIMESTAMP, " +
	"disable_time TIMESTAMP, disable_reason STRING, ddl STRING, coverage_percentage INT64, unindexed_row_count INT64, " +
	"total_logical_bytes INT64, total_storage_bytes INT64, analyzer STRING, index_usage_mode STRING>"

const vectorIndexesColumns = "STRUCT<index_catalog STRING, index_schema STRING, table_name STRING, index_name STRING, " +
	"index_status STRING, creation_time TIMESTAMP, last_modification_time TIMESTAMP, last_refresh_time TIMESTAMP, " +
	"disable_time TIMESTAMP, disable_reason STRING, ddl STRING, coverage_percentage INT64, unindexed_row_count INT64, " +
	"total_logical_bytes INT64, total_storage_bytes INT64>"

const indexColumnsColumns = "STRUCT<index_catalog STRING, index_schema STRING, table_name STRING, index_name STRING, " +
	"index_column_name STRING, index_field_path STRING>"

const indexOptionsColumns = "STRUCT<index_catalog STRING, index_schema STRING, table_name STRING, index_name STRING, " +
	"option_name STRING, option_type STRING, option_value STRING>"
