package bigqueryfront

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestBytesParameters (#1078): a BYTES parameter is sent as a STRING of
// its standard base64, each reference to it (not one in a string, a
// comment or a system variable) as FROM_BASE64 of it; an ARRAY<BYTES> as
// ARRAY<STRING>, referred to as the array of its elements' bytes; a query
// with positional parameters with named ones. A STRUCT parameter, and an
// ARRAY of them, is sent as the STRUCT built from its leaves (#1082); a
// RANGE inside one is 501, and a value that is not base64 400.
func TestBytesParameters(t *testing.T) {
	for _, c := range []struct {
		name, query, mode, params string
		code                      int
		wantQuery, wantParams     string
	}{
		{"named", "SELECT @p, '@p', @P -- @p\n, @@time_zone, @q", "NAMED",
			`[{"name":"p","parameterType":{"type":"BYTES"},"parameterValue":{"value":"_wBh"}},` +
				`{"name":"q","parameterType":{"type":"INT64"},"parameterValue":{"value":"1"}}]`, 0,
			"SELECT FROM_BASE64(@p), '@p', FROM_BASE64(@P) -- @p\n, @@time_zone, @q",
			`[{"name":"p","parameterType":{"type":"STRING"},"parameterValue":{"value":"/wBh"}},` +
				`{"name":"q","parameterType":{"type":"INT64"},"parameterValue":{"value":"1"}}]`},
		{"a NULL value", "SELECT @p IS NULL", "",
			`[{"name":"p","parameterType":{"type":"BYTES"},"parameterValue":{}}]`, 0,
			"SELECT FROM_BASE64(@p) IS NULL", `[{"name":"p","parameterType":{"type":"STRING"},"parameterValue":{}}]`},
		{"ARRAY<BYTES>", "SELECT @a", "NAMED",
			`[{"name":"a","parameterType":{"type":"ARRAY","arrayType":{"type":"BYTES"}},` +
				`"parameterValue":{"arrayValues":[{"value":"/wBh"},{"value":"YWI"}]}}]`, 0,
			"SELECT IF(@a IS NULL, NULL, ARRAY(SELECT FROM_BASE64(_cloudburrow_e) FROM UNNEST(@a) AS _cloudburrow_e " +
				"WITH OFFSET AS _cloudburrow_o ORDER BY _cloudburrow_o))",
			`[{"name":"a","parameterType":{"arrayType":{"type":"STRING"},"type":"ARRAY"},` +
				`"parameterValue":{"arrayValues":[{"value":"/wBh"},{"value":"YWI="}]}}]`},
		{"positional", "SELECT ?, '?', ?", "POSITIONAL",
			`[{"parameterType":{"type":"INT64"},"parameterValue":{"value":"1"}},` +
				`{"parameterType":{"type":"BYTES"},"parameterValue":{"value":"YQ=="}}]`, 0,
			"SELECT @cloudburrow_p1, '?', FROM_BASE64(@cloudburrow_p2)",
			`[{"name":"cloudburrow_p1","parameterType":{"type":"INT64"},"parameterValue":{"value":"1"}},` +
				`{"name":"cloudburrow_p2","parameterType":{"type":"STRING"},"parameterValue":{"value":"YQ=="}}]`},
		// #1082: a STRUCT is rebuilt from its leaves, each a parameter
		// of its own.
		{"a STRUCT with BYTES", "SELECT @s.x, @s", "NAMED",
			`[{"name":"s","parameterType":{"type":"STRUCT","structTypes":[{"name":"x","type":{"type":"BYTES"}},` +
				`{"name":"n","type":{"type":"INTEGER"}}]},"parameterValue":{"structValues":{"x":{"value":"_wBh"},"n":{"value":"2"}}}}]`, 0,
			"SELECT (STRUCT<`x` BYTES, `n` INT64>(FROM_BASE64(@cloudburrow_s1_1), @cloudburrow_s1_2)).x, " +
				"(STRUCT<`x` BYTES, `n` INT64>(FROM_BASE64(@cloudburrow_s1_1), @cloudburrow_s1_2))",
			`[{"name":"cloudburrow_s1_1","parameterType":{"type":"STRING"},"parameterValue":{"value":"/wBh"}},` +
				`{"name":"cloudburrow_s1_2","parameterType":{"type":"INT64"},"parameterValue":{"value":"2"}}]`},
		{"an ARRAY of STRUCTs, nested, NULLs and other types", "SELECT @a", "NAMED",
			`[{"name":"a","parameterType":{"type":"ARRAY","arrayType":{"type":"STRUCT","structTypes":[` +
				`{"name":"d","type":{"type":"DATE"}},{"name":"i","type":{"type":"STRUCT","structTypes":[{"name":"g","type":{"type":"GEOGRAPHY"}}]}},` +
				`{"name":"e","type":{"type":"ARRAY","arrayType":{"type":"NUMERIC"}}}]}},` +
				`"parameterValue":{"arrayValues":[{"structValues":{"d":{"value":"2020-01-02"},"i":{},"e":{"arrayValues":[{"value":"1.5"}]}}},` +
				`{"structValues":{"d":{},"i":{"structValues":{"g":{"value":"POINT(1 2)"}}},"e":{}}}]}}]`, 0,
			"SELECT (ARRAY<STRUCT<`d` DATE, `i` STRUCT<`g` GEOGRAPHY>, `e` ARRAY<NUMERIC>>>[" +
				"STRUCT<`d` DATE, `i` STRUCT<`g` GEOGRAPHY>, `e` ARRAY<NUMERIC>>(CAST(@cloudburrow_s1_1 AS DATE), CAST(NULL AS STRUCT<`g` GEOGRAPHY>), " +
				"ARRAY<NUMERIC>[CAST(@cloudburrow_s1_2 AS NUMERIC)]), " +
				"STRUCT<`d` DATE, `i` STRUCT<`g` GEOGRAPHY>, `e` ARRAY<NUMERIC>>(CAST(NULL AS DATE), STRUCT<`g` GEOGRAPHY>(ST_GEOGFROMTEXT(@cloudburrow_s1_3)), " +
				"ARRAY<NUMERIC>[])])",
			`[{"name":"cloudburrow_s1_1","parameterType":{"type":"STRING"},"parameterValue":{"value":"2020-01-02"}},` +
				`{"name":"cloudburrow_s1_2","parameterType":{"type":"STRING"},"parameterValue":{"value":"1.5"}},` +
				`{"name":"cloudburrow_s1_3","parameterType":{"type":"STRING"},"parameterValue":{"value":"POINT(1 2)"}}]`},
		{"a positional NULL STRUCT", "SELECT ? IS NULL", "POSITIONAL",
			`[{"parameterType":{"type":"STRUCT","structTypes":[{"name":"x","type":{"type":"STRING"}}]},"parameterValue":{}}]`, 0,
			"SELECT (CAST(NULL AS STRUCT<`x` STRING>)) IS NULL", `[]`},
		{"a RANGE in a STRUCT", "SELECT @s", "NAMED",
			`[{"name":"s","parameterType":{"type":"STRUCT","structTypes":[{"name":"r","type":{"type":"RANGE","rangeElementType":{"type":"DATE"}}}]},` +
				`"parameterValue":{"structValues":{}}}]`, http.StatusNotImplemented, "", ""},
		{"not base64 in a STRUCT", "SELECT @s", "NAMED",
			`[{"name":"s","parameterType":{"type":"STRUCT","structTypes":[{"name":"x","type":{"type":"BYTES"}}]},` +
				`"parameterValue":{"structValues":{"x":{"value":"not base64!"}}}}]`, http.StatusBadRequest, "", ""},
		{"not base64", "SELECT @p", "NAMED",
			`[{"name":"p","parameterType":{"type":"BYTES"},"parameterValue":{"value":"not base64!"}}]`, http.StatusBadRequest, "", ""},
		// #1108: a top-level NUMERIC, BIGNUMERIC, DATE, DATETIME, TIME,
		// INTERVAL, GEOGRAPHY or JSON, and an ARRAY of one, is sent as the
		// STRING of its value in its conversion; TIMESTAMP is left as it is.
		{"scalars sent as STRING", "SELECT @n, @b, @d, @dt, @t, @i, @g, @j, @ts", "NAMED",
			`[{"name":"n","parameterType":{"type":"NUMERIC"},"parameterValue":{"value":"123.45"}},` +
				`{"name":"b","parameterType":{"type":"BIGDECIMAL"},"parameterValue":{"value":"1.5"}},` +
				`{"name":"d","parameterType":{"type":"DATE"},"parameterValue":{"value":"2020-01-02"}},` +
				`{"name":"dt","parameterType":{"type":"DATETIME"},"parameterValue":{}},` +
				`{"name":"t","parameterType":{"type":"TIME"},"parameterValue":{"value":"03:04:05.000006"}},` +
				`{"name":"i","parameterType":{"type":"INTERVAL"},"parameterValue":{"value":"1-2 3 4:5:6"}},` +
				`{"name":"g","parameterType":{"type":"GEOGRAPHY"},"parameterValue":{"value":"POINT(1 2)"}},` +
				`{"name":"j","parameterType":{"type":"JSON"},"parameterValue":{"value":"{\"a\":1}"}},` +
				`{"name":"ts","parameterType":{"type":"TIMESTAMP"},"parameterValue":{"value":"2020-01-02 03:04:05+00:00"}}]`, 0,
			"SELECT CAST(@n AS NUMERIC), CAST(@b AS BIGNUMERIC), CAST(@d AS DATE), CAST(@dt AS DATETIME), CAST(@t AS TIME), " +
				"CAST(@i AS INTERVAL), ST_GEOGFROMTEXT(@g), PARSE_JSON(@j), @ts",
			`[{"name":"n","parameterType":{"type":"STRING"},"parameterValue":{"value":"123.45"}},` +
				`{"name":"b","parameterType":{"type":"STRING"},"parameterValue":{"value":"1.5"}},` +
				`{"name":"d","parameterType":{"type":"STRING"},"parameterValue":{"value":"2020-01-02"}},` +
				`{"name":"dt","parameterType":{"type":"STRING"},"parameterValue":{}},` +
				`{"name":"t","parameterType":{"type":"STRING"},"parameterValue":{"value":"03:04:05.000006"}},` +
				`{"name":"i","parameterType":{"type":"STRING"},"parameterValue":{"value":"1-2 3 4:5:6"}},` +
				`{"name":"g","parameterType":{"type":"STRING"},"parameterValue":{"value":"POINT(1 2)"}},` +
				`{"name":"j","parameterType":{"type":"STRING"},"parameterValue":{"value":"{\"a\":1}"}},` +
				`{"name":"ts","parameterType":{"type":"TIMESTAMP"},"parameterValue":{"value":"2020-01-02 03:04:05+00:00"}}]`},
		{"an ARRAY<DATE>, positional", "SELECT ?", "POSITIONAL",
			`[{"parameterType":{"type":"ARRAY","arrayType":{"type":"DATE"}},"parameterValue":{"arrayValues":[{"value":"2020-01-02"},{"value":"2020-01-03"}]}}]`, 0,
			"SELECT IF(@cloudburrow_p1 IS NULL, NULL, ARRAY(SELECT CAST(_cloudburrow_e AS DATE) FROM UNNEST(@cloudburrow_p1) " +
				"AS _cloudburrow_e WITH OFFSET AS _cloudburrow_o ORDER BY _cloudburrow_o))",
			`[{"name":"cloudburrow_p1","parameterType":{"arrayType":{"type":"STRING"},"type":"ARRAY"},` +
				`"parameterValue":{"arrayValues":[{"value":"2020-01-02"},{"value":"2020-01-03"}]}}]`},
		// A NULL element, which the emulator reads as '' in an
		// ARRAY<STRING>: built as a STRUCT's ARRAY is.
		{"an ARRAY<DATE> with a NULL", "SELECT @a", "NAMED",
			`[{"name":"a","parameterType":{"type":"ARRAY","arrayType":{"type":"DATE"}},"parameterValue":{"arrayValues":[{"value":"2020-01-02"},{"value":null}]}}]`, 0,
			"SELECT (ARRAY<DATE>[CAST(@cloudburrow_s1_1 AS DATE), CAST(NULL AS DATE)])",
			`[{"name":"cloudburrow_s1_1","parameterType":{"type":"STRING"},"parameterValue":{"value":"2020-01-02"}}]`},
		{"an ARRAY<JSON> is not base64", "SELECT @a", "NAMED",
			`[{"name":"a","parameterType":{"type":"ARRAY","arrayType":{"type":"JSON"}},"parameterValue":{"arrayValues":[{"value":"[1]"}]}}]`, 0,
			"SELECT IF(@a IS NULL, NULL, ARRAY(SELECT PARSE_JSON(_cloudburrow_e) FROM UNNEST(@a) " +
				"AS _cloudburrow_e WITH OFFSET AS _cloudburrow_o ORDER BY _cloudburrow_o))",
			`[{"name":"a","parameterType":{"arrayType":{"type":"STRING"},"type":"ARRAY"},"parameterValue":{"arrayValues":[{"value":"[1]"}]}}]`},
		{"no BYTES", "SELECT @p", "NAMED",
			`[{"name":"p","parameterType":{"type":"STRING"},"parameterValue":{"value":"x"}}]`, 0, "SELECT @p", ""},
	} {
		q := queryOptions{Query: c.query, ParameterMode: c.mode, QueryParameters: json.RawMessage(c.params)}
		out, changed, code, msg := bytesParameters(q)
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

// TestBytesParameterJobShowsTheClients (#1078): a query job with a BYTES
// parameter is sent with the parameter as STRING and FROM_BASE64 in the
// text, and jobs.insert's answer and jobs.get show the client's text and
// parameters.
func TestBytesParameterJobShowsTheClients(t *testing.T) {
	emu := &valuesEmulator{}
	h := Wrap(emu)
	const params = `[{"name":"p","parameterType":{"type":"BYTES"},"parameterValue":{"value":"/wBh"}}]`
	job := `{"jobReference":{"projectId":"p","jobId":"j1"},"configuration":{"query":{"query":"SELECT @p",` +
		`"useLegacySql":false,"parameterMode":"NAMED","queryParameters":` + params + `}}}`
	code, got := do(t, h, "POST", base+"/jobs", job)
	if code != 200 || len(emu.jobs) != 1 {
		t.Fatalf("jobs.insert: %d %v, sent %v", code, got, emu.jobs)
	}
	sent := emu.jobs[0]["configuration"].(map[string]any)["query"].(map[string]any)
	if sent["query"] != "SELECT FROM_BASE64(@p)" || !strings.Contains(mustJSON(sent["queryParameters"]), `"type":"STRING"`) {
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
