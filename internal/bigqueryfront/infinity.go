package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
)

// ±Infinity in a FLOAT64 value (#1077).
//
// Writing. The emulator's engine keeps a STRUCT or ARRAY value as JSON
// (googlesqlite, internal/value/encoder.go, valueLayoutFromValue through
// json.Marshal), and JSON has no infinity, so a load that hands it a
// RECORD holding ±Infinity fails. Measured through the front with the
// official Go client, on the pinned image: a NEWLINE_DELIMITED_JSON load
// of {"r":{"g":"-Infinity"}} (or "Infinity") failed 400 "json: unsupported
// value: -Inf". The same value streamed (tabledata.insertAll) was stored
// and read back -Inf: the emulator gives a streamed RECORD to its engine
// as a list of one-field objects in the schema's order (types.go,
// normalizeData), whose values the engine keeps as the text they came as,
// and a load as the object it read. Measured: the load of
// {"r":[{"g":"Infinity"}]}, that list, was stored and read back +Inf. So
// a JSON load's RECORD value that holds ±Infinity, at any depth, is sent
// to the emulator in that form (infinityRecords); every other value is
// sent as it was. A REPEATED FLOAT64 column's ±Infinity and a top-level
// one were stored already (TestBigQueryNaNWritesAreNotImplemented).
//
// Reading. BigQuery's REST API gives a FLOAT64 value in a row as a string:
// "Infinity", "-Infinity" or "NaN" for the values JSON has no number for
// (https://cloud.google.com/bigquery/docs/reference/rest/v2/tabledata/list
// reads rows as the TableRow of the query and tabledata APIs, and the
// client libraries parse those names). The emulator writes Go's "+Inf"
// and "-Inf" (measured: tabledata.list gave {"v":"+Inf"} at the top
// level, in a RECORD and in a REPEATED column), which the Go client reads
// (strconv) but which are not BigQuery's text. So the front writes them as
// BigQuery does in the rows of tabledata.list, jobs.query and
// jobs.getQueryResults (infinityRows), by the schema: only a FLOAT64
// value is changed, never a STRING that reads "+Inf".

// infinityRecords changes each RECORD value of obj, a row or RECORD value
// of fields read from JSON, that holds ±Infinity at any depth to the form
// the emulator stores it in (above), and reports whether it changed one.
func infinityRecords(fields []field, obj map[string]any) bool {
	changed := false
	for k, v := range obj {
		f, ok := fieldNamed(fields, k)
		if !ok || v == nil || !isRecord(f.Type) || !holdsInfinity(f, v) {
			continue
		}
		obj[k] = listForm(f, v)
		changed = true
	}
	return changed
}

// holdsInfinity reports whether v, a value of f read from JSON, holds a
// FLOAT64 ±Infinity at any depth.
func holdsInfinity(f field, v any) bool {
	if strings.EqualFold(f.Mode, "REPEATED") {
		arr, _ := v.([]any)
		elem := f
		elem.Mode = ""
		for _, e := range arr {
			if holdsInfinity(elem, e) {
				return true
			}
		}
		return false
	}
	switch strings.ToUpper(f.Type) {
	case "RECORD", "STRUCT":
		m, _ := v.(map[string]any)
		for k, e := range m {
			if sub, ok := fieldNamed(f.Fields, k); ok && e != nil && holdsInfinity(sub, e) {
				return true
			}
		}
	case "FLOAT", "FLOAT64":
		s, ok := v.(string)
		return ok && isInfText(s)
	}
	return false
}

// isInfText reports whether s is ±Infinity as the engine reads a FLOAT64
// from text (strconv.ParseFloat).
func isInfText(s string) bool {
	x, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return err == nil && math.IsInf(x, 0)
}

// listForm writes v, a value of the RECORD field f read from JSON, as the
// emulator writes a streamed one (types.go, normalizeData): a REPEATED
// RECORD as a list of its elements so written; a RECORD as a list of one
// object for each of its fields, in the schema's order, with the field's
// value (null when v has none), a RECORD field's so written. A value of
// another shape is left as it was, for the emulator to answer.
func listForm(f field, v any) any {
	if strings.EqualFold(f.Mode, "REPEATED") {
		arr, ok := v.([]any)
		if !ok {
			return v
		}
		elem := f
		elem.Mode = ""
		out := make([]any, len(arr))
		for i, e := range arr {
			out[i] = listForm(elem, e)
		}
		return out
	}
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	out := make([]any, 0, len(f.Fields))
	for _, sub := range f.Fields {
		var val any
		for k, e := range m {
			if strings.EqualFold(k, sub.Name) {
				val = e
				break
			}
		}
		if val != nil && isRecord(sub.Type) {
			val = listForm(sub, val)
		}
		out = append(out, map[string]any{sub.Name: val})
	}
	return out
}

// infinityText is BigQuery's text of a FLOAT64 ±Infinity for the
// emulator's, or "".
func infinityText(s string) string {
	switch s {
	case "+Inf", "Inf":
		return "Infinity"
	case "-Inf":
		return "-Infinity"
	}
	return ""
}

// infinityRows writes each FLOAT64 ±Infinity in rows, TableRows of the
// columns fields, as BigQuery does (above), and reports whether it changed
// one. A row whose cells do not line up with fields is left as it was.
func infinityRows(fields []field, rows []any) bool {
	changed := false
	for _, row := range rows {
		if infinityCells(fields, row) {
			changed = true
		}
	}
	return changed
}

// infinityCells is infinityRows for one row, or one RECORD value:
// {"f": [{"v": ...}, ...]}.
func infinityCells(fields []field, row any) bool {
	m, _ := row.(map[string]any)
	cells, _ := m["f"].([]any)
	if len(cells) != len(fields) {
		return false
	}
	changed := false
	for i, fl := range fields {
		cell, _ := cells[i].(map[string]any)
		if cell == nil {
			continue
		}
		if strings.EqualFold(fl.Mode, "REPEATED") {
			elems, _ := cell["v"].([]any)
			elem := fl
			elem.Mode = ""
			for _, e := range elems {
				if em, ok := e.(map[string]any); ok && infinityValue(elem, em) {
					changed = true
				}
			}
			continue
		}
		if infinityValue(fl, cell) {
			changed = true
		}
	}
	return changed
}

// infinityValue changes one cell, {"v": ...}, of the (not REPEATED) field
// fl.
func infinityValue(fl field, cell map[string]any) bool {
	switch strings.ToUpper(fl.Type) {
	case "FLOAT", "FLOAT64":
		if s, ok := cell["v"].(string); ok {
			if t := infinityText(s); t != "" {
				cell["v"] = t
				return true
			}
		}
	case "RECORD", "STRUCT":
		return infinityCells(fl.Fields, cell["v"])
	}
	return false
}

// infinityAnswer answers w with rec, a jobs.query or jobs.getQueryResults
// answer (a QueryResponse or GetQueryResultsResponse, with its schema and
// rows), with its ±Infinity written as BigQuery writes it (above).
func infinityAnswer(w http.ResponseWriter, rec *recorder) {
	if rec.status != http.StatusOK && rec.status != 0 || !bytes.Contains(rec.body.Bytes(), []byte(`Inf"`)) {
		rec.copyTo(w)
		return
	}
	var resp map[string]any
	dec := json.NewDecoder(bytes.NewReader(rec.body.Bytes()))
	dec.UseNumber()
	if dec.Decode(&resp) != nil {
		rec.copyTo(w)
		return
	}
	var schema struct {
		Schema tableSchema `json:"schema"`
	}
	b, _ := json.Marshal(map[string]any{"schema": resp["schema"]})
	rows, _ := resp["rows"].([]any)
	if json.Unmarshal(b, &schema) != nil || !infinityRows(schema.Schema.Fields, rows) {
		rec.copyTo(w)
		return
	}
	out, err := json.Marshal(resp)
	if err != nil {
		rec.copyTo(w)
		return
	}
	rec.body.Reset()
	rec.body.Write(out)
	rec.copyTo(w)
}

// withInfinities serves r through serve, answering with infinityAnswer.
func withInfinities(w http.ResponseWriter, serve func(http.ResponseWriter)) {
	rec := newRecorder()
	serve(rec)
	infinityAnswer(w, rec)
}

// infinityTableRows is infinityRows for tabledata.list's rows, of the
// table whose tables.get resource is table.
func infinityTableRows(table []byte, rows []json.RawMessage) []json.RawMessage {
	var meta struct {
		Schema tableSchema `json:"schema"`
	}
	if json.Unmarshal(table, &meta) != nil {
		return rows
	}
	for i, raw := range rows {
		if !bytes.Contains(raw, []byte(`Inf"`)) {
			continue
		}
		var row any
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if dec.Decode(&row) != nil || !infinityCells(meta.Schema.Fields, row) {
			continue
		}
		if b, err := json.Marshal(row); err == nil {
			rows[i] = b
		}
	}
	return rows
}
