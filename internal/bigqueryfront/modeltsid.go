package bigqueryfront

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// A time series model's series IDs: time_series_id_col names one column
// or, as BigQuery documents ("A STRING or ARRAY<STRING> value … Each ID
// identifies a unique time series",
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-time-series,
// read 2026-11-19), several, each STRING or INT64. A series is each
// distinct combination of their values; a row with a NULL in any of them
// is left out, as one with a NULL ID is. Every ML function's output
// starts with the ID columns, in the option's order.
//
// A series' key (arimaSeries.ID) is its one ID's value, or, with several
// columns, the JSON array of their values (tsOptions.IDCols and
// arimaTrained.IDTypes are set only then, so a model of one column is
// stored as before).

// idCols are ts's ID columns, or nil.
func (ts tsOptions) idCols() []string {
	if len(ts.IDCols) > 0 {
		return ts.IDCols
	}
	if ts.IDCol != "" {
		return []string{ts.IDCol}
	}
	return nil
}

// idTypes are a's ID columns' types, or nil.
func (a *arimaTrained) idTypes() []string {
	if len(a.IDTypes) > 0 {
		return a.IDTypes
	}
	if a.IDType != "" {
		return []string{a.IDType}
	}
	return nil
}

// seriesKey is the key of a series of ID values vals.
func seriesKey(vals []string) string {
	if len(vals) == 1 {
		return vals[0]
	}
	b, _ := json.Marshal(vals)
	return string(b)
}

// idValues are the ID values of series key key, of n columns.
func idValues(key string, n int) []string {
	if n == 1 {
		return []string{key}
	}
	var out []string
	_ = json.Unmarshal([]byte(key), &out)
	return out
}

// tsIDDecl is the ID columns' STRUCT fields, each followed by ", ".
func tsIDDecl(ts tsOptions, a *arimaTrained) string {
	var b strings.Builder
	for i, typ := range a.idTypes() {
		b.WriteString(sqlIdent(ts.idCols()[i]) + " " + typ + ", ")
	}
	return b.String()
}

// tsIDSQL is series s's ID values as SQL literals, each followed by ", ".
func tsIDSQL(a *arimaTrained, s arimaSeries) string {
	types := a.idTypes()
	if len(types) == 0 || s.ID == nil {
		return ""
	}
	var b strings.Builder
	for i, v := range idValues(*s.ID, len(types)) {
		b.WriteString(sqlLabel(v, types[i]) + ", ")
	}
	return b.String()
}

// findTSIDs are the indexes of ts's ID columns in cols; missing is the
// first not there, or "".
func findTSIDs(cols []mlColumn, ts tsOptions) (idx []int, missing string) {
	for _, name := range ts.idCols() {
		found := -1
		for i, c := range cols {
			if strings.EqualFold(c.Name, name) {
				found = i
				break
			}
		}
		if found < 0 {
			return nil, name
		}
		idx = append(idx, found)
	}
	return idx, ""
}

// rowSeriesKey is the series key of row by its ID columns idx; ok is
// false when one is NULL. With no ID columns it is "".
func rowSeriesKey(row []*string, idx []int) (key string, ok bool) {
	if len(idx) == 0 {
		return "", true
	}
	vals := make([]string, len(idx))
	for j, i := range idx {
		if row[i] == nil {
			return "", false
		}
		vals[j] = *row[i]
	}
	return seriesKey(vals), true
}

// sortSeriesKeys sorts keys by their ID values, column by column, each as
// its type sorts (sortLabels).
func sortSeriesKeys(keys []string, types []string) {
	if len(types) == 1 {
		sortLabels(keys, types[0])
		return
	}
	less := func(a, b string, typ string) bool {
		if typ == "INT64" {
			x, ex := strconv.ParseInt(a, 10, 64)
			y, ey := strconv.ParseInt(b, 10, 64)
			if ex == nil && ey == nil {
				return x < y
			}
		}
		return a < b
	}
	sort.SliceStable(keys, func(i, j int) bool {
		a, b := idValues(keys[i], len(types)), idValues(keys[j], len(types))
		for k, typ := range types {
			if a[k] != b[k] {
				return less(a[k], b[k], typ)
			}
		}
		return false
	})
}
