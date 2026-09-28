package bigqueryfront

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// nestedTestSchema is (ts TIMESTAMP, s STRING, rec STRUCT<ts TIMESTAMP,
// s STRING, rr ARRAY<STRUCT<ts TIMESTAMP>>>, rts ARRAY<TIMESTAMP>).
const nestedTestSchema = `{"fields":[{"name":"ts","type":"TIMESTAMP"},{"name":"s","type":"STRING"},` +
	`{"name":"rec","type":"RECORD","fields":[{"name":"ts","type":"TIMESTAMP"},{"name":"s","type":"STRING"},` +
	`{"name":"rr","type":"RECORD","mode":"REPEATED","fields":[{"name":"ts","type":"TIMESTAMP"}]}]},` +
	`{"name":"rts","type":"TIMESTAMP","mode":"REPEATED"}]}`

// nestedTestRow is a row as the pinned emulator writes it (measured, #1101):
// the top-level TIMESTAMP converted, the nested ones the engine's text; a
// STRING that reads like one is a STRING.
const nestedTestRow = `{"f":[{"v":"1704164645123456"},{"v":"2020-01-01 00:00:00+00"},` +
	`{"v":{"f":[{"v":"2020-01-01 00:00:00+00"},{"v":"1999-01-01 00:00:00+00"},{"v":[{"v":{"f":[{"v":"1969-12-31 23:59:59.500000+00"}]}}]}]}},` +
	`{"v":[{"v":"2021-01-01 00:00:01.500000+00"},{"v":null}]}]}`

// TestNestedTimestampsInRESTRows (#1101): each nested TIMESTAMP is written
// in the form the request asks for, and nothing else is changed.
func TestNestedTimestampsInRESTRows(t *testing.T) {
	for _, tc := range []struct {
		int64Timestamp bool
		want           string
	}{
		{true, `{"f":[{"v":"1704164645123456"},{"v":"2020-01-01 00:00:00+00"},` +
			`{"v":{"f":[{"v":"1577836800000000"},{"v":"1999-01-01 00:00:00+00"},{"v":[{"v":{"f":[{"v":"-500000"}]}}]}]}},` +
			`{"v":[{"v":"1609459201500000"},{"v":null}]}]}`},
		{false, `{"f":[{"v":"1704164645123456"},{"v":"2020-01-01 00:00:00+00"},` +
			`{"v":{"f":[{"v":"1577836800.000000"},{"v":"1999-01-01 00:00:00+00"},{"v":[{"v":{"f":[{"v":"-0.500000"}]}}]}]}},` +
			`{"v":[{"v":"1609459201.500000"},{"v":null}]}]}`},
	} {
		got := infinityTableRows([]byte(`{"schema":`+nestedTestSchema+`}`), []json.RawMessage{json.RawMessage(nestedTestRow)}, tc.int64Timestamp)
		if string(got[0]) != tc.want {
			t.Errorf("useInt64Timestamp %v:\n%s\nwant\n%s", tc.int64Timestamp, got[0], tc.want)
		}
	}

	// jobs.query and jobs.getQueryResults answers, by the request's
	// formatOptions: the body's for jobs.query, the query string's for
	// getQueryResults.
	answer := `{"schema":` + nestedTestSchema + `,"rows":[` + nestedTestRow + `],"jobComplete":true}`
	for _, tc := range []struct {
		name string
		req  *http.Request
		want string
	}{
		{"jobs.query", httptest.NewRequest(http.MethodPost, "/bigquery/v2/projects/p/queries",
			strings.NewReader(`{"query":"SELECT 1","formatOptions":{"useInt64Timestamp":true}}`)), `"1577836800000000"`},
		{"jobs.query, seconds", httptest.NewRequest(http.MethodPost, "/bigquery/v2/projects/p/queries",
			strings.NewReader(`{"query":"SELECT 1"}`)), `"1577836800.000000"`},
		{"getQueryResults", httptest.NewRequest(http.MethodGet, "/bigquery/v2/projects/p/queries/j?formatOptions.useInt64Timestamp=true", nil),
			`"1577836800000000"`},
	} {
		var body string
		w := httptest.NewRecorder()
		withInfinities(w, tc.req, func(w http.ResponseWriter) {
			if tc.req.Body != nil {
				b, _ := io.ReadAll(tc.req.Body)
				body = string(b)
			}
			_, _ = io.WriteString(w, answer)
		})
		if !strings.Contains(w.Body.String(), tc.want) || strings.Contains(w.Body.String(), `"2020-01-01 00:00:00+00"}]}}`) {
			t.Errorf("%s: %s", tc.name, w.Body.String())
		}
		if tc.req.Method == http.MethodPost && !strings.Contains(body, `"query":"SELECT 1"`) {
			t.Errorf("%s: the handler read %q, not the request's body", tc.name, body)
		}
	}
}
