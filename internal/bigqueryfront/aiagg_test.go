package bigqueryfront

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// TestFindAIAgg (#1777): AI.AGG's arguments, DISTINCT, and the calls
// refused before anything runs.
func TestFindAIAgg(t *testing.T) {
	sql := "SELECT g, AI.AGG(DISTINCT review, 'Summarize', endpoint => 'm', output_schema => 'a STRING') AS s FROM t GROUP BY g"
	toks, _ := lex(sql)
	depth := tokenDepths(toks)
	calls, code, msg := findAIAggCalls(sql, toks, depth)
	if code != 0 || len(calls) != 1 {
		t.Fatalf("%d %s %v", code, msg, calls)
	}
	c := calls[0]
	if !c.distinct || c.args["input"] != "review" || c.args["instruction"] != "'Summarize'" || c.args["endpoint"] != "'m'" ||
		c.args["output_schema"] != "'a STRING'" || !toks[c.block].is("SELECT") {
		t.Errorf("%+v", c)
	}
	if got := c.contentExpr(); got != "TO_JSON_STRING(ARRAY_AGG(DISTINCT TO_JSON_STRING(review) ORDER BY TO_JSON_STRING(review)))" {
		t.Errorf("content %s", got)
	}
	for in, want := range map[string]int{
		"SELECT AI.AGG(s, 'x') FROM t":                                        0,
		"SELECT AI.AGG(input => s, instruction => 'x') FROM t":                0,
		"SELECT AI.AGG(s, 'x', max_error_ratio => 0.5) FROM t":                0,
		"SELECT AI.AGG(s) FROM t":                                             400,
		"SELECT AI.AGG(s, 'x', 'y') FROM t":                                   400,
		"SELECT AI.AGG(s, 'x', model_params => JSON '{}') FROM t":             400,
		"SELECT AI.AGG(s, 'x', endpoint => 'a', endpoint => 'b')":             400,
		"SELECT AI.AGG(DISTINCT, 'x') FROM t":                                 400,
		"SELECT AI.AGG(s, 'x') OVER (PARTITION BY g) FROM t":                  501,
		"SELECT x.ai.agg(s, 'x') FROM t":                                      0,
		"SELECT AI.AGG(s, 'x') FROM t GROUP BY g HAVING AI.AGG(s, 'y') = 'z'": 0,
	} {
		toks, _ := lex(in)
		if _, code, msg := findAIAggCalls(in, toks, tokenDepths(toks)); code != want {
			t.Errorf("%s: %d %s, want %d", in, code, msg, want)
		}
	}
}

// TestAIAggProbe: the probe keeps the select list (GROUP BY ordinals and
// aliases), FROM, WHERE, GROUP BY and WINDOW, drops HAVING, QUALIFY, ORDER
// BY and LIMIT, and makes every AI.AGG a typed NULL.
func TestAIAggProbe(t *testing.T) {
	sql := "WITH t AS (SELECT 1 AS g, 'a' AS s) SELECT g AS k, AI.AGG(s, 'x').n AS n, AI.AGG(s, 'y') AS y FROM t WHERE s IS NOT NULL GROUP BY 1 " +
		"HAVING AI.AGG(s, 'z') != '' ORDER BY k LIMIT 3"
	toks, _ := lex(sql)
	depth := tokenDepths(toks)
	calls, code, msg := findAIAggCalls(sql, toks, depth)
	if code != 0 || len(calls) != 3 {
		t.Fatalf("%d %s %v", code, msg, calls)
	}
	types := []string{"STRUCT<`n` INT64>", "STRING", "STRING"}
	probe, code, msg := aggProbe(sql, toks, depth, calls, types, 1)
	want := "WITH t AS (SELECT 1 AS g, 'a' AS s) SELECT DISTINCT _cbai_c, FARM_FINGERPRINT(_cbai_c) AS _cbai_fp FROM (SELECT g AS k, " +
		"CAST(NULL AS STRUCT<`n` INT64>).n AS n, CAST(NULL AS STRING) AS y, " +
		"TO_JSON_STRING(ARRAY_AGG(TO_JSON_STRING(s) ORDER BY TO_JSON_STRING(s))) AS _cbai_c FROM t WHERE s IS NOT NULL GROUP BY 1)"
	if code != 0 || probe != want {
		t.Errorf("%d %s\nprobe %s\nwant  %s", code, msg, probe, want)
	}
	// No FROM: one group.
	sql = "SELECT AI.AGG('a', 'x')"
	toks, _ = lex(sql)
	depth = tokenDepths(toks)
	calls, _, _ = findAIAggCalls(sql, toks, depth)
	if probe, _, _ := aggProbe(sql, toks, depth, calls, []string{"STRING"}, 0); !strings.HasSuffix(probe, "AS _cbai_c)") {
		t.Errorf("no FROM: %s", probe)
	}
	for _, in := range []string{"SELECT g FROM t WHERE AI.AGG(s, 'x') = 'a'", "SELECT 1 FROM t GROUP BY AI.AGG(s, 'x')"} {
		toks, _ := lex(in)
		depth := tokenDepths(toks)
		calls, _, _ := findAIAggCalls(in, toks, depth)
		if _, code, msg := aggProbe(in, toks, depth, calls, []string{"STRING"}, 0); code != 400 || !strings.Contains(msg, "not allowed") {
			t.Errorf("%s: %d %s", in, code, msg)
		}
	}
}

// fakeAggVertex answers each generateContent with the number of <row> or
// <part> elements in its prompt, or fails a prompt holding "boom".
func fakeAggVertex(t *testing.T, prompts *[]string, cfgs *[]map[string]any) *httptest.Server {
	var mu sync.Mutex
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Contents []struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"contents"`
			GenerationConfig map[string]any `json:"generationConfig"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		p := body.Contents[0].Parts[0].Text
		mu.Lock()
		*prompts = append(*prompts, p)
		*cfgs = append(*cfgs, body.GenerationConfig)
		mu.Unlock()
		if strings.Contains(p, "boom") {
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"error":{"code":503,"message":"overloaded","status":"UNAVAILABLE"}}`))
			return
		}
		ans := "rows=" + strconv.Itoa(strings.Count(p, "\n<row>")) + " parts=" + strconv.Itoa(strings.Count(p, "\n<part>"))
		if _, ok := body.GenerationConfig["responseSchema"]; ok {
			ans = `{"n": ` + strconv.Itoa(strings.Count(p, "\n<part>")+strings.Count(p, "\n<row>")) + `}`
		}
		out, _ := json.Marshal(map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"role": "model",
			"parts": []any{map[string]string{"text": ans}}}, "finishReason": "STOP"}}})
		_, _ = w.Write(out)
	}))
}

// TestAIAggGroupBatches: a group that fits is one call; a larger one is
// batched, each batch answered, and the answers combined; a failed batch
// drops its rows (partial results) and counts them; output_schema
// constrains the last call only.
func TestAIAggGroupBatches(t *testing.T) {
	var prompts []string
	var cfgs []map[string]any
	srv := fakeAggVertex(t, &prompts, &cfgs)
	defer srv.Close()
	f := front{vertexai: srv.URL}
	r := httptest.NewRequest("POST", "/", nil)
	a := aiResolved{rm: &remoteModel{Model: "gemma-4-e2b-it-community", Location: "US"}, maxErrRatio: 1}

	lit, ok, failed, _ := f.aggGroup(r, "p", a, "Summarize", []string{"one", "two", "three"})
	if !ok || failed != 0 || lit != sqlString("rows=3 parts=0") || len(prompts) != 1 || !strings.HasPrefix(prompts[0], "Summarize\n\n") {
		t.Errorf("one call: %s %v %d %q", lit, ok, failed, prompts)
	}
	if cfgs[0]["maxOutputTokens"] != float64(aiAggOutputTokens) {
		t.Errorf("generationConfig %v", cfgs[0])
	}

	defer func(n int) { aiAggBatchBytes = n }(aiAggBatchBytes)
	aiAggBatchBytes = len("Summarize") + 2*len("<row>xxxx</row>\n") // two rows a batch
	prompts, cfgs = nil, nil
	rows := []string{"r001", "r002", "r003", "r004", "r005"}
	lit, ok, failed, _ = f.aggGroup(r, "p", a, "Summarize", rows)
	// 5 rows: 3 batches (2, 2, 1), then their 3 answers combined: the
	// answers are longer than the budget, so in pairs (2, 1), then 1.
	if !ok || failed != 0 || len(prompts) < 4 || !strings.Contains(prompts[len(prompts)-1], "<part>") {
		t.Fatalf("batched: %s %v %d\n%s", lit, ok, failed, strings.Join(prompts, "\n---\n"))
	}
	for i, p := range prompts[:3] {
		if strings.Contains(p, "<part>") || cfgs[i]["maxOutputTokens"] != float64(aiAggPartialTokens) {
			t.Errorf("batch %d: %q %v", i, p, cfgs[i])
		}
	}

	prompts, cfgs = nil, nil
	lit, ok, failed, status := f.aggGroup(r, "p", a, "Summarize", []string{"r001", "boom", "r003", "r004"})
	if !ok || failed != 2 || !strings.Contains(status, "UNAVAILABLE") {
		t.Errorf("a failed batch: %s %v %d %s", lit, ok, failed, status)
	}
	if _, ok, failed, _ := f.aggGroup(r, "p", a, "Summarize", []string{"boom", "boom"}); ok || failed != 2 {
		t.Errorf("every call failed: %v %d", ok, failed)
	}

	aiAggBatchBytes = 1 << 20
	fields, _ := parseOutputSchema("n INT64")
	a.schema = fields
	prompts, cfgs = nil, nil
	lit, ok, failed, _ = f.aggGroup(r, "p", a, "Count", []string{"a", "b"})
	if !ok || failed != 0 || lit != "STRUCT<`n` INT64>(2)" || cfgs[0]["responseSchema"] == nil || !strings.Contains(prompts[0], "JSON object") {
		t.Errorf("output_schema: %s %v %d %v", lit, ok, failed, cfgs)
	}
}
