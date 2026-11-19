package bigqueryfront

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestRangeParameters (#1111): a parameter whose type has a RANGE in it,
// at the top level, in a STRUCT or in an ARRAY, is sent built from its
// leaves, each a parameter of its own type, its references replaced by it;
// a query with positional parameters with named ones. Every other
// parameter, a BYTES, a DATE, an ARRAY or a STRUCT of them, a NULL, is
// sent as the client sent it: the engine types each as declared (#1816).
func TestRangeParameters(t *testing.T) {
	for _, c := range []struct {
		name, query, mode, params string
		code                      int
		wantQuery, wantParams     string
	}{
		{"a RANGE in a STRUCT", "SELECT @s", "NAMED",
			`[{"name":"s","parameterType":{"type":"STRUCT","structTypes":[{"name":"r","type":{"type":"RANGE","rangeElementType":{"type":"DATE"}}}]},` +
				`"parameterValue":{"structValues":{}}}]`, 0, "SELECT (STRUCT<`r` RANGE<DATE>>(CAST(NULL AS RANGE<DATE>)))", `[]`},
		{"a top-level RANGE, in a string and a comment", "SELECT @r, '@r' -- @r\n", "NAMED",
			`[{"name":"r","parameterType":{"type":"RANGE","rangeElementType":{"type":"DATE"}},` +
				`"parameterValue":{"rangeValue":{"start":{"value":"2020-01-01"},"end":{"value":"2020-02-01"}}}},` +
				`{"name":"q","parameterType":{"type":"BYTES"},"parameterValue":{"value":"_wBh"}}]`, 0,
			"SELECT (RANGE(@cloudburrow_s1_1, @cloudburrow_s1_2)), '@r' -- @r\n",
			`[{"name":"q","parameterType":{"type":"BYTES"},"parameterValue":{"value":"_wBh"}},` +
				`{"name":"cloudburrow_s1_1","parameterType":{"type":"DATE"},"parameterValue":{"value":"2020-01-01"}},` +
				`{"name":"cloudburrow_s1_2","parameterType":{"type":"DATE"},"parameterValue":{"value":"2020-02-01"}}]`},
		{"a positional RANGE<TIMESTAMP> with an UNBOUNDED end", "SELECT ?, ?", "",
			`[{"parameterType":{"type":"RANGE","rangeElementType":{"type":"TIMESTAMP"}},` +
				`"parameterValue":{"rangeValue":{"start":{"value":"2020-01-01 00:00:00+00:00"}}}},` +
				`{"parameterType":{"type":"INT64"},"parameterValue":{"value":"1"}}]`, 0,
			"SELECT (RANGE(@cloudburrow_s1_1, CAST(NULL AS TIMESTAMP))), @cloudburrow_p2",
			`[{"name":"cloudburrow_p2","parameterType":{"type":"INT64"},"parameterValue":{"value":"1"}},` +
				`{"name":"cloudburrow_s1_1","parameterType":{"type":"TIMESTAMP"},"parameterValue":{"value":"2020-01-01 00:00:00+00:00"}}]`},
		{"a RANGE with both bounds UNBOUNDED, and a NULL one", "SELECT @r, @n", "NAMED",
			`[{"name":"r","parameterType":{"type":"RANGE","rangeElementType":{"type":"DATETIME"}},"parameterValue":{"rangeValue":{"start":{}}}},` +
				`{"name":"n","parameterType":{"type":"RANGE","rangeElementType":{"type":"DATE"}},"parameterValue":{}}]`, 0,
			"SELECT (RANGE<DATETIME> '[UNBOUNDED, UNBOUNDED)'), (CAST(NULL AS RANGE<DATE>))", `[]`},
		{"an ARRAY of RANGE", "SELECT @r", "NAMED",
			`[{"name":"r","parameterType":{"type":"ARRAY","arrayType":{"type":"RANGE","rangeElementType":{"type":"DATE"}}},` +
				`"parameterValue":{"arrayValues":[]}}]`, 0, "SELECT (ARRAY<RANGE<DATE>>[])", `[]`},
		{"a STRUCT with a RANGE and other leaves", "SELECT @s", "NAMED",
			`[{"name":"s","parameterType":{"type":"STRUCT","structTypes":[{"name":"b","type":{"type":"BYTES"}},` +
				`{"name":"n","type":{"type":"DECIMAL"}},{"name":"r","type":{"type":"RANGE","rangeElementType":{"type":"DATE"}}}]},` +
				`"parameterValue":{"structValues":{"b":{"value":"/wBh"},"n":{},"r":{"rangeValue":{"start":{"value":"2020-01-01"}}}}}}]`, 0,
			"SELECT (STRUCT<`b` BYTES, `n` NUMERIC, `r` RANGE<DATE>>(@cloudburrow_s1_1, CAST(NULL AS NUMERIC), " +
				"RANGE(@cloudburrow_s1_2, CAST(NULL AS DATE))))",
			`[{"name":"cloudburrow_s1_1","parameterType":{"type":"BYTES"},"parameterValue":{"value":"/wBh"}},` +
				`{"name":"cloudburrow_s1_2","parameterType":{"type":"DATE"},"parameterValue":{"value":"2020-01-01"}}]`},
		{"a RANGE of INT64", "SELECT @r", "NAMED",
			`[{"name":"r","parameterType":{"type":"RANGE","rangeElementType":{"type":"INT64"}},` +
				`"parameterValue":{"rangeValue":{"start":{"value":"1"}}}}]`, http.StatusNotImplemented, "", ""},
		// #1816: the engine types these as declared; they are sent as
		// they are.
		{"BYTES", "SELECT @p", "NAMED",
			`[{"name":"p","parameterType":{"type":"BYTES"},"parameterValue":{"value":"_wBh"}}]`, 0, "SELECT @p", ""},
		{"scalars", "SELECT ?, ?, ?", "POSITIONAL",
			`[{"parameterType":{"type":"NUMERIC"},"parameterValue":{"value":"1.5"}},` +
				`{"parameterType":{"type":"DATE"},"parameterValue":{}},{"parameterType":{"type":"JSON"},"parameterValue":{"value":"[1]"}}]`, 0,
			"SELECT ?, ?, ?", ""},
		{"an ARRAY<DATE> and a STRUCT", "SELECT @a, @s.x", "NAMED",
			`[{"name":"a","parameterType":{"type":"ARRAY","arrayType":{"type":"DATE"}},"parameterValue":{"arrayValues":[{"value":"2020-01-02"}]}},` +
				`{"name":"s","parameterType":{"type":"STRUCT","structTypes":[{"name":"x","type":{"type":"BYTES"}}]},` +
				`"parameterValue":{"structValues":{"x":{"value":"not base64!"}}}}]`, 0, "SELECT @a, @s.x", ""},
	} {
		q := queryOptions{Query: c.query, ParameterMode: c.mode, QueryParameters: json.RawMessage(c.params)}
		out, changed, code, msg := rangeParameters(q)
		if code != c.code {
			t.Errorf("%s: code %d %q, want %d", c.name, code, msg, c.code)
			continue
		}
		if code != 0 {
			continue
		}
		if out.Query != c.wantQuery {
			t.Errorf("%s: query\n %q\nwant %q", c.name, out.Query, c.wantQuery)
		}
		if c.wantParams == "" {
			if changed {
				t.Errorf("%s: changed", c.name)
			}
			continue
		}
		var got, want any
		_ = json.Unmarshal(out.QueryParameters, &got)
		_ = json.Unmarshal([]byte(c.wantParams), &want)
		if g, _ := json.Marshal(got); string(g) != mustJSON(want) {
			t.Errorf("%s: parameters\n %s\nwant %s", c.name, g, mustJSON(want))
		}
		if strings.EqualFold(c.mode, "POSITIONAL") && out.ParameterMode != "NAMED" {
			t.Errorf("%s: parameterMode %q", c.name, out.ParameterMode)
		}
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// TestRangeParameterJobShowsTheClients (#1111): a query job with a RANGE
// parameter is sent with the parameter built in the text, and jobs.insert's
// answer and jobs.get show the client's text and parameters.
func TestRangeParameterJobShowsTheClients(t *testing.T) {
	emu := &valuesEmulator{}
	h := Wrap(emu)
	const params = `[{"name":"p","parameterType":{"type":"RANGE","rangeElementType":{"type":"DATE"}},` +
		`"parameterValue":{"rangeValue":{"start":{"value":"2020-01-01"}}}}]`
	job := `{"jobReference":{"projectId":"p","jobId":"j1"},"configuration":{"query":{"query":"SELECT @p",` +
		`"useLegacySql":false,"parameterMode":"NAMED","queryParameters":` + params + `}}}`
	code, got := do(t, h, "POST", base+"/jobs", job)
	if code != 200 || len(emu.jobs) != 1 {
		t.Fatalf("jobs.insert: %d %v, sent %v", code, got, emu.jobs)
	}
	sent := emu.jobs[0]["configuration"].(map[string]any)["query"].(map[string]any)
	if sent["query"] != "SELECT (RANGE(@cloudburrow_s1_1, CAST(NULL AS DATE)))" ||
		!strings.Contains(mustJSON(sent["queryParameters"]), `"name":"cloudburrow_s1_1"`) {
		t.Errorf("sent %v", sent)
	}
	for what, j := range map[string]map[string]any{"jobs.insert": got, "jobs.get": jobGot(t, h)} {
		q, _ := j["configuration"].(map[string]any)["query"].(map[string]any)
		var want any
		_ = json.Unmarshal([]byte(params), &want)
		if q["query"] != "SELECT @p" || mustJSON(q["queryParameters"]) != mustJSON(want) {
			t.Errorf("%s shows %v", what, q)
		}
	}
}
