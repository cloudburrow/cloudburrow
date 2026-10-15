package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// PARTITION BY and CLUSTER BY in CREATE TABLE (#1272).
//
// Measured against the pinned image through the front (2026-10-15):
// CREATE TABLE p.t (a INT64, d DATE, s STRING) PARTITION BY d CLUSTER BY s,
// a RANGE_BUCKET partitioning and a CREATE TABLE ... AS SELECT with them
// all made the table, and tables.get read no timePartitioning,
// rangePartitioning or clustering back: the emulator makes a table's
// metadata from its name and columns only (as its OPTIONS, ddloptions.go).
// It also made a table PARTITION BY a STRING column, which BigQuery refuses.
// Its tables.update keeps all three (measured), so a lone CREATE [OR
// REPLACE] TABLE with either clause is carried out as one with OPTIONS is:
// run, then given the partitioning and clustering BigQuery would read from
// it (https://cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language#partition_expression,
// #clustering_column_list).
//
// The columns' types come from the statement's column list, or, for AS
// SELECT, from the query run with LIMIT 0 before the statement; a clause
// that names a column of the wrong type, or is of a form BigQuery does not
// take, is refused 400 before anything is run. The messages are
// UNVERIFIED: Google documents the forms, not the wording.
//
// Not carried out: what partitioning does to queries (partition pruning,
// require_partition_filter, partition_expiration_days: 501 as options),
// and _PARTITIONTIME of an ingestion-time partitioned table (#1277).

// partitionSpec is the metadata a statement's PARTITION BY and CLUSTER BY
// give its table: tables.get's timePartitioning, rangePartitioning and
// clustering, nil for those it does not set.
type partitionSpec struct {
	time, rangeP, clustering map[string]any
}

const partitionForms = "PARTITION BY expression must be _PARTITIONDATE, DATE(_PARTITIONTIME), DATE(<timestamp_column>), " +
	"DATE(<datetime_column>), DATETIME_TRUNC(<datetime_column>, DAY/HOUR/MONTH/YEAR), a DATE column, " +
	"TIMESTAMP_TRUNC(<timestamp_column>, DAY/HOUR/MONTH/YEAR), TIMESTAMP_TRUNC(_PARTITIONTIME, DAY/HOUR/MONTH/YEAR), " +
	"DATE_TRUNC(<date_column>, MONTH/YEAR), or RANGE_BUCKET(<int64_column>, GENERATE_ARRAY(<int64_value>, <int64_value>[, <int64_value>]))"

// clusterTypes are the column types BigQuery clusters on.
var clusterTypes = map[string]bool{"BIGNUMERIC": true, "BOOL": true, "DATE": true, "DATETIME": true, "GEOGRAPHY": true,
	"INT64": true, "NUMERIC": true, "RANGE": true, "STRING": true, "TIMESTAMP": true}

// partitioning reads d's PARTITION BY and CLUSTER BY against its columns'
// types; status is 0, or the code of msg, why it is refused.
func (f front) partitioning(r *http.Request, q queryOptions, d ddlOptions) (spec partitionSpec, status int, msg string) {
	if d.partition == nil && d.cluster == nil {
		return spec, 0, ""
	}
	types, status, msg := f.columnTypes(r, q, d)
	if status != 0 {
		return spec, status, msg
	}
	if d.partition != nil {
		if spec.time, spec.rangeP, msg = partitionOf(d.partition, types); msg != "" {
			return spec, http.StatusBadRequest, msg
		}
	}
	if d.cluster != nil {
		if spec.clustering, msg = clusteringOf(d.cluster, types); msg != "" {
			return spec, http.StatusBadRequest, msg
		}
	}
	return spec, 0, ""
}

// clusteringOf reads a CLUSTER BY list: the table's clustering, or why
// BigQuery refuses it.
func clusteringOf(c []token, types map[string]string) (map[string]any, string) {
	var fields []string
	for _, item := range splitTop(c) {
		if len(item) != 1 || (item[0].kind != tokWord && item[0].kind != tokQuoted) {
			return nil, "CLUSTER BY takes a list of column names"
		}
		name := item[0].text
		typ, ok := types[strings.ToLower(name)]
		switch {
		case !ok:
			return nil, "Unrecognized name: " + name
		case !clusterTypes[typ]:
			return nil, fmt.Sprintf("Clustering on a column of type %s is not supported: %s", typ, name)
		}
		fields = append(fields, name)
	}
	if len(fields) > 4 {
		return nil, fmt.Sprintf("Too many clustering columns: %d, only up to 4 are allowed", len(fields))
	}
	return map[string]any{"fields": fields}, ""
}

// partitionOf reads a PARTITION BY expression: the table's timePartitioning
// or rangePartitioning, or why BigQuery refuses it.
func partitionOf(p []token, types map[string]string) (timeP, rangeP map[string]any, msg string) {
	column := func(tk token, want ...string) (string, string) {
		if tk.kind != tokWord && tk.kind != tokQuoted {
			return "", partitionForms
		}
		typ, ok := types[strings.ToLower(tk.text)]
		if !ok {
			return "", "Unrecognized name: " + tk.text
		}
		for _, w := range want {
			if typ == w {
				return tk.text, ""
			}
		}
		return "", partitionForms
	}
	day := map[string]any{"type": "DAY"}
	if len(p) == 1 {
		if p[0].is("_PARTITIONDATE") {
			return day, nil, ""
		}
		name, msg := column(p[0], "DATE")
		if msg != "" {
			return nil, nil, msg
		}
		return map[string]any{"type": "DAY", "field": name}, nil, ""
	}
	if len(p) < 4 || p[0].kind != tokWord || !p[1].punct("(") || !p[len(p)-1].punct(")") {
		return nil, nil, partitionForms
	}
	args := splitTop(p[2 : len(p)-1])
	fn := strings.ToUpper(p[0].text)
	unit := func(allowed ...string) (string, bool) {
		if len(args) != 2 || len(args[1]) != 1 {
			return "", false
		}
		for _, a := range allowed {
			if args[1][0].is(a) {
				return a, true
			}
		}
		return "", false
	}
	switch fn {
	case "DATE":
		if len(args) != 1 || len(args[0]) != 1 {
			return nil, nil, partitionForms
		}
		if args[0][0].is("_PARTITIONTIME") {
			return day, nil, ""
		}
		name, msg := column(args[0][0], "TIMESTAMP", "DATETIME")
		if msg != "" {
			return nil, nil, msg
		}
		return map[string]any{"type": "DAY", "field": name}, nil, ""
	case "TIMESTAMP_TRUNC", "DATETIME_TRUNC", "DATE_TRUNC":
		units, typ := []string{"DAY", "HOUR", "MONTH", "YEAR"}, map[string]string{"TIMESTAMP_TRUNC": "TIMESTAMP",
			"DATETIME_TRUNC": "DATETIME", "DATE_TRUNC": "DATE"}[fn]
		if fn == "DATE_TRUNC" {
			units = []string{"MONTH", "YEAR"}
		}
		u, ok := unit(units...)
		if !ok || len(args[0]) != 1 {
			return nil, nil, partitionForms
		}
		if fn == "TIMESTAMP_TRUNC" && args[0][0].is("_PARTITIONTIME") {
			return map[string]any{"type": u}, nil, ""
		}
		name, msg := column(args[0][0], typ)
		if msg != "" {
			return nil, nil, msg
		}
		return map[string]any{"type": u, "field": name}, nil, ""
	case "RANGE_BUCKET":
		if len(args) != 2 || len(args[0]) != 1 {
			return nil, nil, partitionForms
		}
		name, msg := column(args[0][0], "INT64")
		if msg != "" {
			return nil, nil, msg
		}
		g := args[1]
		if len(g) < 4 || !g[0].is("GENERATE_ARRAY") || !g[1].punct("(") || !g[len(g)-1].punct(")") {
			return nil, nil, partitionForms
		}
		var bounds []int64
		for _, a := range splitTop(g[2 : len(g)-1]) {
			v, ok := intLiteral(a)
			if !ok {
				return nil, nil, partitionForms
			}
			bounds = append(bounds, v)
		}
		if len(bounds) == 2 {
			bounds = append(bounds, 1)
		}
		if len(bounds) != 3 {
			return nil, nil, partitionForms
		}
		if bounds[2] <= 0 || bounds[1] <= bounds[0] {
			return nil, nil, "RANGE_BUCKET partitioning needs an end above its start and a positive interval"
		}
		return nil, map[string]any{"field": name, "range": map[string]any{"start": strconv.FormatInt(bounds[0], 10),
			"end": strconv.FormatInt(bounds[1], 10), "interval": strconv.FormatInt(bounds[2], 10)}}, ""
	}
	return nil, nil, partitionForms
}

// intLiteral reads an INT64 literal, with a sign.
func intLiteral(t []token) (int64, bool) {
	neg := false
	if len(t) == 2 && (t[0].punct("-") || t[0].punct("+")) {
		neg, t = t[0].punct("-"), t[1:]
	}
	if len(t) != 1 || t[0].kind != tokWord {
		return 0, false
	}
	v, err := strconv.ParseInt(t[0].text, 10, 64)
	if err != nil {
		return 0, false
	}
	if neg {
		v = -v
	}
	return v, true
}

// typeNames are GoogleSQL's type aliases, by the type tables.get names.
var typeNames = map[string]string{"INT": "INT64", "INTEGER": "INT64", "BIGINT": "INT64", "SMALLINT": "INT64",
	"TINYINT": "INT64", "BYTEINT": "INT64", "DECIMAL": "NUMERIC", "BIGDECIMAL": "BIGNUMERIC", "BOOLEAN": "BOOL",
	"FLOAT": "FLOAT64", "RECORD": "STRUCT"}

// columnTypes reads the top-level columns of the statement's table and
// their types (upper case, INT64 for INTEGER), by lower-case name: from its
// column list, or from its query run with LIMIT 0.
func (f front) columnTypes(r *http.Request, q queryOptions, d ddlOptions) (map[string]string, int, string) {
	types := map[string]string{}
	if d.columns != nil {
		for _, col := range splitTop(d.columns) {
			if len(col) < 2 || (col[0].kind != tokWord && col[0].kind != tokQuoted) || col[1].kind != tokWord ||
				col[0].is("PRIMARY") || col[0].is("FOREIGN") || col[0].is("CONSTRAINT") {
				continue
			}
			typ := strings.ToUpper(col[1].text)
			if n, ok := typeNames[typ]; ok {
				typ = n
			}
			if len(col) > 2 && col[2].punct("<") {
				typ = strings.ToUpper(col[1].text) // ARRAY<...> or STRUCT<...>
			}
			types[strings.ToLower(col[0].text)] = typ
		}
		return types, 0, ""
	}
	if strings.TrimSpace(d.query) == "" {
		return types, 0, ""
	}
	legacy := false
	req, err := json.Marshal(queryOptions{Query: "SELECT * FROM (" + d.query + "\n) LIMIT 0", UseLegacySQL: &legacy,
		DefaultDataset: q.DefaultDataset, ParameterMode: q.ParameterMode, QueryParameters: q.QueryParameters})
	if err != nil {
		return nil, http.StatusInternalServerError, err.Error()
	}
	status, got := f.send(r, http.MethodPost, "/queries", req)
	if status != http.StatusOK {
		return nil, status, errorMessage(got, status)
	}
	var res struct {
		Schema struct {
			Fields []struct {
				Name, Type, Mode string
			} `json:"fields"`
		} `json:"schema"`
	}
	if json.Unmarshal(got, &res) != nil {
		return nil, http.StatusInternalServerError, "cloudburrow: could not read the query's columns"
	}
	for _, fl := range res.Schema.Fields {
		typ := strings.ToUpper(fl.Type)
		if n, ok := typeNames[typ]; ok {
			typ = n
		}
		if strings.EqualFold(fl.Mode, "REPEATED") {
			typ = "ARRAY"
		}
		types[strings.ToLower(fl.Name)] = typ
	}
	return types, 0, ""
}
