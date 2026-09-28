package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// A TIMESTAMP inside a RECORD or a REPEATED column in a REST read (#1101).
//
// BigQuery's REST API gives every TIMESTAMP value of a row, at any depth,
// in one form: microseconds since the epoch as a decimal string when the
// request's formatOptions.useInt64Timestamp is true ("Output timestamp as
// usec int64", https://cloud.google.com/bigquery/docs/reference/rest/v2/DataFormatOptions),
// otherwise seconds since the epoch as a floating-point string. The
// emulator converts a cell only when its column is a TIMESTAMP
// (internal/types/types.go, Format, read in its source), so a TIMESTAMP in
// a RECORD or a REPEATED column comes back as its engine's text. Measured
// on the pinned image, for (ts TIMESTAMP, rec STRUCT<ts TIMESTAMP, ...>,
// rts ARRAY<TIMESTAMP>) with useInt64Timestamp: ts "1704164645123456", but
// rec.ts "2020-01-01 00:00:00+00" and rts [{"v":"2021-01-01
// 00:00:01.500000+00"}], and the Go client's Table.Read failed
// "strconv.ParseInt: parsing \"2020-01-01 00:00:00+00\"". The other types
// were measured the same at depth as at the top level (DATETIME
// "2024-01-02T03:04:05.123456", DATE, TIME, NUMERIC, BIGNUMERIC, BYTES in
// base64, GEOGRAPHY, FLOAT64, BOOL, INT64, JSON), so only a TIMESTAMP is
// changed: in the rows of tabledata.list, jobs.query and
// jobs.getQueryResults, by the schema, each TIMESTAMP value that is the
// engine's text is written in the request's form, as the top-level ones
// are (the emulator's seconds are "%d.%06d").

// timestampRowsText is the text a nested TIMESTAMP value is rewritten
// from: the engine's, whose offset is always "+00".
var timestampRowsText = []byte(`+00"`)

// nestedTimestampCells writes each TIMESTAMP value in row, a TableRow of
// fields or a RECORD value ({"f": [{"v": ...}, ...]}), that is the
// engine's text in the form int64Timestamp asks for (above), and reports
// whether it changed one. A top-level TIMESTAMP the emulator wrote in that
// form already, so it is never the engine's text.
func nestedTimestampCells(fields []field, row any, int64Timestamp bool) bool {
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
				if em, ok := e.(map[string]any); ok && nestedTimestampValue(elem, em, int64Timestamp) {
					changed = true
				}
			}
			continue
		}
		if nestedTimestampValue(fl, cell, int64Timestamp) {
			changed = true
		}
	}
	return changed
}

func nestedTimestampValue(fl field, cell map[string]any, int64Timestamp bool) bool {
	switch legacyType(fl.Type) {
	case "TIMESTAMP":
		s, ok := cell["v"].(string)
		if !ok || !strings.HasSuffix(s, "+00") {
			return false
		}
		if t := restTimestamp(s, int64Timestamp); t != "" {
			cell["v"] = t
			return true
		}
	case "RECORD":
		return nestedTimestampCells(fl.Fields, cell["v"], int64Timestamp)
	}
	return false
}

// restTimestamp writes s, the engine's text of a TIMESTAMP, in the REST
// form (above), or "" when it does not read.
func restTimestamp(s string, int64Timestamp bool) string {
	t, err := parseTimestamp(s) // storagerows.go
	if err != nil {
		return ""
	}
	us := t.UnixMicro()
	if int64Timestamp {
		return strconv.FormatInt(us, 10)
	}
	sign := ""
	if us < 0 {
		sign, us = "-", -us
	}
	return fmt.Sprintf("%s%d.%06d", sign, us/1e6, us%1e6)
}

// requestInt64Timestamp reads formatOptions.useInt64Timestamp of a REST
// read: the query parameter (tabledata.list, jobs.getQueryResults), or the
// body of a jobs.query, which is put back for the handler.
func requestInt64Timestamp(r *http.Request) bool {
	if v := r.URL.Query().Get("formatOptions.useInt64Timestamp"); v != "" {
		b, _ := strconv.ParseBool(v)
		return b
	}
	if r.Method != http.MethodPost || r.Body == nil {
		return false
	}
	body, err := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil {
		return false
	}
	var q struct {
		FormatOptions struct {
			UseInt64Timestamp bool `json:"useInt64Timestamp"`
		} `json:"formatOptions"`
	}
	_ = json.Unmarshal(body, &q)
	return q.FormatOptions.UseInt64Timestamp
}
