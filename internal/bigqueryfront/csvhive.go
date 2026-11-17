package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Hive-partitioned CSV loads (#1547).
//
// "Loading externally partitioned data"
// (https://cloud.google.com/bigquery/docs/hive-partitioned-loads-gcs, read
// 2026-11-17) lists CSV among the formats ("Currently supported formats
// are: JSON, CSV, ORC, Avro and Parquet") and shows a CSV load (bq load
// --source_format=CSV --autodetect --hive_partitioning_mode=AUTO). The
// keys are read and checked as for any hive-partitioned load
// (loadVariants.go); csvLoad then reads each file's records by the load's
// CSV options as it reads any (csvdialect.go), and each record gets its
// file's keys' values in the columns the keys are: "Hive partition keys
// appear as normal columns".
//
// The columns are the load's schema, which gets each key it lacks at its
// end (loadvariants.go), or the table it loads into, which must have a
// column for each key. A file's values go, in order, to the columns that
// are not keys, a key's column being wherever the schema puts it
// (UNVERIFIED: the page does not say where a key's column is; a schema
// naming it is read by name, as for a JSON load). allowJaggedRows and
// ignoreUnknownValues count a record's width without the keys.
//
// Still 501, as the page documents none of them for CSV: a load with
// autodetect and no columns given (BigQuery's detection of the file's
// columns with the keys is not described, and the emulator detects the
// columns from the first file's header, without the keys);
// sourceColumnMatch NAME (the page does not say whether a header names the
// keys); and a timeZone or a date or time format (the page does not say
// whether they apply to a key's value).

// csvHive is a hive-partitioned CSV load's keys, as csvLoad reads them.
type csvHive struct {
	files *hiveFiles
	// at is, for each column, the index of the key it is, or -1 for a
	// column of the file's.
	at []int
}

// csvHiveOf returns the keys of the hive-partitioned CSV load h is, whose
// values go to cols; or why the load is refused (a reason beginning "!"
// is 501). It returns nil, "" when h is nil.
func csvHiveOf(h *hiveFiles, cols []field, o csvOptions) (*csvHive, string) {
	if h == nil {
		return nil, ""
	}
	switch {
	case len(cols) == 0:
		return nil, "!a hive-partitioned CSV load with autodetect and no schema, into a new table: BigQuery's " +
			"loading page (hive-partitioned-loads-gcs) does not say how the file's columns are detected with the " +
			"partition keys. Give the load a schema"
	case strings.EqualFold(o.SourceColumnMatch, "NAME"):
		return nil, "!a hive-partitioned CSV load with sourceColumnMatch NAME: BigQuery does not document whether the " +
			"header names the partition keys"
	case o.TimeZone != "" || o.DateFormat != "" || o.DatetimeFormat != "" || o.TimeFormat != "" || o.TimestampFormat != "":
		return nil, "!a hive-partitioned CSV load with a timeZone or a date or time format: BigQuery does not document " +
			"whether they apply to the partition keys' values"
	}
	c := &csvHive{files: h, at: make([]int, len(cols))}
	for i := range c.at {
		c.at[i] = -1
	}
	for k, key := range h.keys {
		found := false
		for i, col := range cols {
			if strings.EqualFold(col.Name, key.name) {
				c.at[i], found = k, true
				break
			}
		}
		if !found {
			return nil, fmt.Sprintf("!a hive-partitioned CSV load into a table with no column for partition key %s: "+
				"BigQuery does not document adding the key's column to a table that exists", key.name)
		}
	}
	return c, ""
}

// keys is how many columns are keys.
func (c *csvHive) keys() int { return len(c.files.keys) }

// values returns the keys' values in the file at uri, as CSV values.
func (c *csvHive) values(uri string) []string {
	out := make([]string, len(c.files.keys))
	for i, k := range c.files.keys {
		var v string
		if vals := c.files.vals[uri]; i < len(vals) {
			v = vals[i]
		}
		b, _ := hiveValue(v, k.typ)
		var s string
		if json.Unmarshal(b, &s) == nil {
			out[i] = s
		} else {
			out[i] = string(b)
		}
	}
	return out
}

// merge returns the record of a file (rec, its values quoted and given)
// with vals, its file's keys' values, in their columns. A key's value is
// not given, so that no null marker is read in it.
func (c *csvHive) merge(rec []string, quoted, given []bool, vals []string) ([]string, []bool, []bool) {
	out, q, g := make([]string, len(c.at)), make([]bool, len(c.at)), make([]bool, len(c.at))
	j := 0
	for i, k := range c.at {
		if k >= 0 {
			if k < len(vals) {
				out[i] = vals[k]
			}
			continue
		}
		if j < len(rec) {
			out[i], q[i], g[i] = rec[j], quoted[j], given[j]
		}
		j++
	}
	return out, q, g
}
