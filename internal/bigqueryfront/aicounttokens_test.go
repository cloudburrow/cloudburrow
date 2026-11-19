package bigqueryfront

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCountTokensRow (#1962): AI.COUNT_TOKENS sends the input as a user
// turn to the local endpoint's :countTokens under /v1 and answers its
// totalTokens and the response; a failed call is NULL in both.
func TestCountTokensRow(t *testing.T) {
	var paths, bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		paths, bodies = append(paths, r.URL.Path), append(bodies, string(b))
		if strings.Contains(r.URL.Path, "/missing:") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":404,"message":"Publisher Model missing was not found.","status":"NOT_FOUND"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"totalTokens":11,"promptTokensDetails":[{"modality":"TEXT","tokenCount":11}]}`)
	}))
	defer srv.Close()
	f := front{vertexai: srv.URL}
	ctx := context.Background()
	a := aiResolved{rm: &remoteModel{Model: "gemma-4-e2b-it-community", Location: "US"}}
	vals, status, ok, code, _ := f.countTokensRow(ctx, "p", a, "Token count isn't always equal to word count.")
	if !ok || code != 0 || status != "" || vals[0] != "11" || !strings.Contains(vals[1], `"totalTokens":11`) || !strings.HasPrefix(vals[1], "PARSE_JSON(") {
		t.Errorf("counted: %v %q %v %d", vals, status, ok, code)
	}
	a.rm.Model = "missing"
	vals, status, ok, _, _ = f.countTokensRow(ctx, "p", a, "x")
	if !ok || vals[0] != "CAST(NULL AS INT64)" || vals[1] != "CAST(NULL AS JSON)" || !strings.Contains(status, "NOT_FOUND") {
		t.Errorf("refused: %v %q", vals, status)
	}
	if want := "/v1/projects/p/locations/us-central1/publishers/google/models/gemma-4-e2b-it-community:countTokens"; paths[0] != want {
		t.Errorf("path %s, want %s", paths[0], want)
	}
	if !strings.Contains(bodies[0], `"role":"user"`) || !strings.Contains(bodies[0], "word count") || strings.Contains(bodies[0], "generationConfig") {
		t.Errorf("body %s", bodies[0])
	}
}

// TestFindCountTokens: the input is positional or named, endpoint named,
// and nothing else is taken.
func TestFindCountTokens(t *testing.T) {
	for sql, want := range map[string]int{
		"SELECT AI.COUNT_TOKENS('hi').result":                                       0,
		"SELECT AI.COUNT_TOKENS(input => s, endpoint => 'gemini-2.5-flash') FROM t": 0,
		"SELECT AI.COUNT_TOKENS(s, connection_id => 'us.c') FROM t":                 400,
		"SELECT AI.COUNT_TOKENS(s, 'gemini-2.5-flash') FROM t":                      400,
		"SELECT AI.COUNT_TOKENS(endpoint => 'gemini-2.5-flash')":                    400,
	} {
		toks, _ := lex(sql)
		calls, code, msg := findAICalls(sql, toks, tokenDepths(toks))
		if code != want || code == 0 && (len(calls) != 1 || calls[0].fn != "COUNT_TOKENS") {
			t.Errorf("%s: %d %s, %v; want %d", sql, code, msg, calls, want)
		}
	}
	if fields, scalar := (aiCall{fn: "COUNT_TOKENS"}).output(aiResolved{}); scalar || strings.Join(fields, ",") != "result INT64,full_response JSON" {
		t.Errorf("output %v %v", fields, scalar)
	}
}
