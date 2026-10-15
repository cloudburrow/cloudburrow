package bigqueryfront

import (
	"sort"
	"strings"
)

// A NEWLINE_DELIMITED_JSON load's field outside its columns (#1339).
//
// BigQuery's ignoreUnknownValues: "Indicates if BigQuery should allow
// extra values that are not represented in the table schema. If true, the
// extra values are ignored. If false, records with extra columns are
// treated as bad records, and if there are too many bad records, an
// invalid error is returned in the job result. The default value is
// false. ... JSON: Named values that don't match any column names";
// maxBadRecords: "The maximum number of bad records that BigQuery can
// ignore when running the job".
// https://cloud.google.com/bigquery/docs/reference/rest/v2/Job#JobConfigurationLoad
//
// The emulator drops such a field and loads the record (measured: a record
// {"i":13,"zz":1} loaded into a table without zz, with no error). So the
// front reads every JSON load whose columns it knows (the table's, or the
// load's schema, or the one it detected, jsondetect.go) and, without
// ignoreUnknownValues, holds each record to them, at any depth of a
// RECORD and in each element of a REPEATED one. Names match without
// regard to case, as BigQuery's columns and the emulator's matching do. A
// record with a field outside the columns is bad: up to maxBadRecords are
// left out, each listed in the job's status.errors, and one more fails
// the load, 400 invalid. A load with autodetect is held to the schema
// detected from its first 500 records, which is the load's schema: a field
// first seen after them is outside it, so its record is bad too. A field
// outside the columns whose value is null is 501: the reference does not
// say whether BigQuery counts it.
//
// A CSV record with more values than columns follows the same rule; the
// front reads it so already (csvdialect.go, dialectRecords), and the
// emulator fails a wide record it is passed (measured).

// unknownField returns the first field of obj, a record of fields, that
// fields lacks, by its path from prefix, and whether its value is null;
// or "" when it has none.
func unknownField(fields []field, obj map[string]any, prefix string) (string, bool) {
	byName := make(map[string]field, len(fields))
	for _, f := range fields {
		byName[strings.ToLower(f.Name)] = f
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys) // the same field named every time
	for _, k := range keys {
		f, ok := byName[strings.ToLower(k)]
		if !ok {
			return prefix + k, obj[k] == nil
		}
		typ := strings.ToUpper(f.Type)
		if typ != "RECORD" && typ != "STRUCT" {
			continue
		}
		var elems []any
		switch v := obj[k].(type) {
		case map[string]any:
			elems = []any{v}
		case []any:
			elems = v
		}
		for _, e := range elems {
			if sub, ok := e.(map[string]any); ok {
				if name, isNull := unknownField(f.Fields, sub, prefix+k+"."); name != "" {
					return name, isNull
				}
			}
		}
	}
	return "", false
}

// knownFields returns a's fields and those of b that a lacks, a RECORD in
// both with the fields of each.
func knownFields(a, b []field) []field {
	out := append([]field(nil), a...)
	at := make(map[string]int, len(a))
	for i, f := range a {
		at[strings.ToLower(f.Name)] = i
	}
	for _, f := range b {
		i, ok := at[strings.ToLower(f.Name)]
		if !ok {
			out = append(out, f)
			continue
		}
		if len(f.Fields) > 0 {
			out[i].Fields = knownFields(out[i].Fields, f.Fields)
		}
	}
	return out
}
