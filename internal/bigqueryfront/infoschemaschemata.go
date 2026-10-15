package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// INFORMATION_SCHEMA SCHEMATA_OPTIONS and MATERIALIZED_VIEWS (#1351),
// answered as the other dataset views are (infoschema.go), dataset- and
// region-qualified (regionViews), with BigQuery's documented columns
// (https://cloud.google.com/bigquery/docs/information-schema-datasets-schemata-options,
// -materialized-views). Measured first: the engine failed each 400 "Table
// not found"; the front refused them 501.
//
// SCHEMATA_OPTIONS has a row per option the dataset's datasets.get
// resource holds: description, friendly_name, labels,
// default_table_expiration_days, default_partition_expiration_days,
// max_time_travel_hours and default_kms_key_name, each value written as
// SQL, as TABLE_OPTIONS writes a table's. MATERIALIZED_VIEWS is always
// empty: the engine makes no materialized view.
//
// Not built (501): OBJECT_PRIVILEGES, SCHEMATA_LINKS.

const schemataOptionsColumns = "STRUCT<catalog_name STRING, schema_name STRING, option_name STRING, " +
	"option_type STRING, option_value STRING>"

const materializedViewsColumns = "STRUCT<table_catalog STRING, table_schema STRING, table_name STRING, " +
	"last_refresh_time TIMESTAMP, refresh_watermark TIMESTAMP, " +
	"last_refresh_status STRUCT<reason STRING, location STRING, debug_info STRING, message STRING>>"

func init() {
	infoSchemaBuilders["SCHEMATA_OPTIONS"] = front.schemataOptionsRows
	infoSchemaBuilders["MATERIALIZED_VIEWS"] = emptyRows(materializedViewsColumns)
	for view, columns := range map[string]string{
		"SCHEMATA_OPTIONS":   schemataOptionsColumns,
		"MATERIALIZED_VIEWS": materializedViewsColumns,
	} {
		delete(unbuiltInfoSchema, view)
		regionViews[view] = true
		regionColumns[view] = columns
	}
}

// schemataOptionsRows returns the subquery of dataset's options.
func (f front) schemataOptionsRows(r *http.Request, dataset string, _ *datasetMeta) (string, int, string) {
	project := projectOf(f.base)
	st, got := f.get(r, "/datasets/"+url.PathEscape(dataset))
	if st == http.StatusNotFound {
		return "", http.StatusNotFound, fmt.Sprintf("Not found: Dataset %s:%s", project, dataset)
	}
	var ds map[string]any
	if st != http.StatusOK || json.Unmarshal(got, &ds) != nil {
		return "", http.StatusInternalServerError, "cloudburrow: could not read the dataset: " + errorMessage(got, st)
	}
	var rows [][]string
	for _, o := range datasetOptions(ds) {
		rows = append(rows, []string{sqlString(project), sqlString(dataset), sqlString(o[0]), sqlString(o[1]), sqlString(o[2])})
	}
	return rowsSubquery(schemataOptionsColumns, rows), 0, ""
}

// datasetOptions are a dataset's options as SCHEMATA_OPTIONS gives them:
// name, type and value, by name.
func datasetOptions(ds map[string]any) [][3]string {
	var out [][3]string
	add := func(name, typ, value string) { out = append(out, [3]string{name, typ, value}) }
	days := func(field string) (string, bool) {
		ms, err := strconv.ParseInt(str(ds, field), 10, 64)
		if err != nil {
			return "", false
		}
		return strconv.FormatFloat(float64(ms)/86400000, 'g', -1, 64), true
	}
	if e, _ := ds["defaultEncryptionConfiguration"].(map[string]any); str(e, "kmsKeyName") != "" {
		add("default_kms_key_name", "STRING", strconv.Quote(str(e, "kmsKeyName")))
	}
	if d, ok := days("defaultPartitionExpirationMs"); ok {
		add("default_partition_expiration_days", "FLOAT64", d)
	}
	if d, ok := days("defaultTableExpirationMs"); ok {
		add("default_table_expiration_days", "FLOAT64", d)
	}
	if d := str(ds, "description"); d != "" {
		add("description", "STRING", strconv.Quote(d))
	}
	if n := str(ds, "friendlyName"); n != "" {
		add("friendly_name", "STRING", strconv.Quote(n))
	}
	if labels, _ := ds["labels"].(map[string]any); len(labels) > 0 {
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
	if h, err := strconv.ParseInt(str(ds, "maxTimeTravelHours"), 10, 64); err == nil {
		add("max_time_travel_hours", "INT64", strconv.FormatInt(h, 10))
	}
	return out
}
