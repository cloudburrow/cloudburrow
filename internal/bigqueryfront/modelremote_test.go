package bigqueryfront

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseRemoteModel(t *testing.T) {
	for sql, want := range map[string]int{
		"CREATE MODEL ds.m REMOTE WITH CONNECTION `p.us.c` OPTIONS(ENDPOINT='gemini-2.0-flash')":              0,
		"CREATE OR REPLACE MODEL ds.m REMOTE WITH CONNECTION `us.c` OPTIONS(endpoint = 'text-embedding-005')": 0,
		"CREATE MODEL ds.m REMOTE WITH CONNECTION DEFAULT OPTIONS(ENDPOINT='gemini-2.0-flash')":               0,
		"CREATE MODEL ds.m REMOTE WITH CONNECTION `p.us.c` OPTIONS(REMOTE_SERVICE_TYPE='CLOUD_AI_VISION_V1')": 0,
		// #1773: the services with no local model, and their options.
		"CREATE MODEL ds.m REMOTE WITH CONNECTION `p.us.c` OPTIONS(REMOTE_SERVICE_TYPE='CLOUD_AI_TRANSLATE_V3')":                        501,
		"CREATE MODEL ds.m REMOTE WITH CONNECTION `p.us.c` OPTIONS(REMOTE_SERVICE_TYPE='CLOUD_AI_NATURAL_LANGUAGE_V1')":                 501,
		"CREATE MODEL ds.m REMOTE WITH CONNECTION `p.us.c` OPTIONS(REMOTE_SERVICE_TYPE='CLOUD_AI_SPEECH_TO_TEXT_V2')":                   501,
		"CREATE MODEL ds.m REMOTE WITH CONNECTION `p.us.c` OPTIONS(REMOTE_SERVICE_TYPE='CLOUD_AI_DOCUMENT_V1', DOCUMENT_PROCESSOR='x')": 501,
		"CREATE MODEL ds.m REMOTE WITH CONNECTION `p.us.c` OPTIONS(REMOTE_SERVICE_TYPE='CLOUD_AI_NOPE')":                                400,
		"CREATE MODEL ds.m REMOTE WITH CONNECTION `p.us.c` OPTIONS(REMOTE_SERVICE_TYPE='CLOUD_AI_VISION_V1', ENDPOINT='x')":             400,
		"CREATE MODEL ds.m REMOTE WITH CONNECTION `p.us.c` OPTIONS(ENDPOINT='x', bogus=1)":                                              400,
		"CREATE MODEL ds.m REMOTE WITH CONNECTION `p.us.c` OPTIONS(ENDPOINT='x') AS SELECT 1":                                           400,
		"CREATE MODEL ds.m REMOTE WITH CONNECTION `p.us.c`":                                                                             400,
		"CREATE MODEL ds.m REMOTE CONNECTION `p.us.c` OPTIONS(ENDPOINT='x')":                                                            400,
		"CREATE MODEL ds.m REMOTE WITH CONNECTION `a.b.c.d` OPTIONS(ENDPOINT='x')":                                                      400,
	} {
		toks, _ := lex(sql)
		d, is, code, msg := parseModelDDL(toks, sql)
		if !is || code != want {
			t.Errorf("%s: %v %d %s; want %d", sql, is, code, msg, want)
		}
		if code == 0 && (d.remote == nil || d.remote.endpoint == "" && d.remote.serviceType == "" || len(d.remote.connection) != 3 && !d.remote.isDefault) {
			t.Errorf("%s: %+v", sql, d.remote)
		}
	}
}

func TestEndpointModel(t *testing.T) {
	for in, want := range map[string]string{
		"gemini-2.0-flash": "gemini-2.0-flash",
		"https://us-central1-aiplatform.googleapis.com/v1/projects/p/locations/us-central1/publishers/google/models/gemini-2.0-flash": "gemini-2.0-flash",
		"projects/p/locations/l/publishers/google/models/text-embedding-005:predict":                                                  "text-embedding-005",
	} {
		if got := endpointModel(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
	if !isEmbeddingEndpoint("text-embedding-005") || !isEmbeddingEndpoint("embeddinggemma-300m-onnx-community") || isEmbeddingEndpoint("gemma-4-e2b-it-community") {
		t.Error("isEmbeddingEndpoint")
	}
}

// fakeVertex answers generateContent and predict as the local Vertex AI
// endpoint does, and records the paths asked.
func fakeVertex(t *testing.T, paths *[]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*paths = append(*paths, r.URL.Path)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch {
		case strings.Contains(r.URL.Path, "/models/missing:"):
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":{"code":404,"message":"model \"missing\" is not available here","status":"NOT_FOUND"}}`))
		case strings.HasSuffix(r.URL.Path, ":generateContent"):
			_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"Pa"},{"text":"ris"}]},"finishReason":"STOP"}],"modelVersion":"m"}`))
		case strings.HasSuffix(r.URL.Path, ":predict"):
			_, _ = w.Write([]byte(`{"predictions":[{"embeddings":{"values":[0.5,-0.25],"statistics":{"token_count":3,"truncated":false}}}]}`))
		}
	}))
}

func TestRemoteRowsOnTheLocalVertexEndpoint(t *testing.T) {
	var paths []string
	srv := fakeVertex(t, &paths)
	defer srv.Close()
	f := front{vertexai: srv.URL}
	ctx := context.Background()
	gen := &remoteModel{Model: "gemma-4-e2b-it-community", Location: "US"}
	got := f.generateRow(ctx, "p", gen, "capital of France?", true, nil)
	if got[0] != sqlString("Paris") || got[3] != sqlString("") {
		t.Errorf("flattened: %v", got)
	}
	got = f.generateRow(ctx, "p", gen, "x", false, nil)
	if !strings.HasPrefix(got[0], "PARSE_JSON(") || !strings.Contains(got[0], "candidates") || got[1] != sqlString("") {
		t.Errorf("unflattened: %v", got)
	}
	got = f.generateRow(ctx, "p", &remoteModel{Model: "missing", Location: "US"}, "x", true, nil)
	if got[0] != "CAST(NULL AS STRING)" || !strings.Contains(got[3], "NOT_FOUND") {
		t.Errorf("refused: %v", got)
	}
	emb := &remoteModel{Model: "embeddinggemma-300m-onnx-community", Location: "EU", Embedding: true}
	got = f.embedRow(ctx, "p", emb, "hello", true, "RETRIEVAL_QUERY", 2)
	if got[0] != "ARRAY<FLOAT64>[0.5, (-0.25)]" || !strings.Contains(got[1], "token_count") || got[2] != sqlString("") {
		t.Errorf("embedding: %v", got)
	}
	want := []string{"/v1beta1/projects/p/locations/us-central1/publishers/google/models/gemma-4-e2b-it-community:generateContent",
		"/v1beta1/projects/p/locations/us-central1/publishers/google/models/gemma-4-e2b-it-community:generateContent",
		"/v1beta1/projects/p/locations/us-central1/publishers/google/models/missing:generateContent",
		"/v1/projects/p/locations/europe-west4/publishers/google/models/embeddinggemma-300m-onnx-community:predict"}
	if strings.Join(paths, "\n") != strings.Join(want, "\n") {
		t.Errorf("paths:\n%s", strings.Join(paths, "\n"))
	}
}

func TestRemoteOptionsRefusedBeforeAnyCall(t *testing.T) {
	f := front{vertexai: "http://127.0.0.1:1"}
	gen := bqModel{Remote: &remoteModel{Model: "g", Endpoint: "g"}}
	emb := bqModel{Remote: &remoteModel{Model: "e", Endpoint: "e", Embedding: true}}
	for _, tc := range []struct {
		md   bqModel
		c    mlCall
		want int
	}{
		{gen, mlCall{fn: "GENERATE_TEXT", input: "SELECT 'a' AS prompt", struc: map[string]string{"safety_settings": "[]"}}, 501},
		{gen, mlCall{fn: "GENERATE_TEXT", input: "SELECT 'a' AS prompt", struc: map[string]string{"temperature": "1.5"}}, 400},
		{gen, mlCall{fn: "GENERATE_TEXT", input: "SELECT 'a' AS prompt", struc: map[string]string{"max_output_tokens": "0"}}, 400},
		{gen, mlCall{fn: "GENERATE_TEXT", input: "SELECT 'a' AS prompt", struc: map[string]string{"top_p": "-0.1"}}, 400},
		{gen, mlCall{fn: "GENERATE_TEXT", input: "SELECT 'a' AS prompt", struc: map[string]string{"top_k": "41"}}, 400},
		{bqModel{Remote: &remoteModel{Model: "g", Endpoint: "gemini-2.5-flash"}}, mlCall{fn: "GENERATE_TEXT", input: "SELECT 'a' AS prompt", struc: map[string]string{"top_k": "4"}}, 400},
		{gen, mlCall{fn: "GENERATE_TEXT", input: "SELECT 'a' AS prompt", struc: map[string]string{"stop_sequences": "'x'"}}, 400},
		{gen, mlCall{fn: "GENERATE_TEXT", input: "SELECT 'a' AS prompt", struc: map[string]string{"ground_with_google_search": "TRUE"}}, 501},
		{gen, mlCall{fn: "GENERATE_TEXT", input: "SELECT 'a' AS prompt", struc: map[string]string{"bogus": "1"}}, 400},
		{gen, mlCall{fn: "GENERATE_TEXT", input: "SELECT 'a' AS prompt", struc: map[string]string{"flatten_json_output": "1"}}, 400},
		{gen, mlCall{fn: "GENERATE_EMBEDDING", input: "SELECT 'a' AS content"}, 400},
		{emb, mlCall{fn: "GENERATE_TEXT", input: "SELECT 'a' AS prompt"}, 400},
		{gen, mlCall{fn: "PREDICT", input: "SELECT 1"}, 400},
		{gen, mlCall{fn: "GENERATE_TEXT"}, 400},
		{emb, mlCall{fn: "GENERATE_EMBEDDING", input: "SELECT 'a' AS content", struc: map[string]string{"output_dimensionality": "0"}}, 400},
	} {
		if _, code, msg := f.remoteSubquery(nil, queryOptions{}, tc.md, tc.c); code != tc.want {
			t.Errorf("%+v: %d %s; want %d", tc.c, code, msg, tc.want)
		}
	}
	if _, code, _ := (front{}).remoteSubquery(nil, queryOptions{}, gen, mlCall{fn: "GENERATE_TEXT", input: "SELECT 'a' AS prompt"}); code != 501 {
		t.Errorf("no endpoint: %d", code)
	}
}

func TestAIFunctionsAreNamed(t *testing.T) {
	for sql, want := range map[string]string{
		"SELECT AI.GENERATE('hi', connection_id => 'us.c', endpoint => 'gemini-2.5-flash').result": "AI.GENERATE",
		"SELECT * FROM AI.GENERATE_TABLE(MODEL m, (SELECT 'x' AS prompt))":                         "AI.GENERATE_TABLE",
		"select ai.embed(content) from t":                                                          "AI.EMBED",
		"SELECT ai.x FROM t AS ai":                                                                 "",
		"SELECT p.ai.f(1)":                                                                         "",
		"SELECT 'AI.GENERATE(' AS s":                                                               "",
	} {
		if got := aiFunction(sql); got != want {
			t.Errorf("%s: %q, want %q", sql, got, want)
		}
	}
}

func TestParseStringArray(t *testing.T) {
	for in, want := range map[string]string{
		"['text_detection']":                           "text_detection",
		"['TEXT_DETECTION','DOCUMENT_TEXT_DETECTION']": "TEXT_DETECTION|DOCUMENT_TEXT_DETECTION",
		"ARRAY<STRING>['a']":                           "a",
		"[]":                                           "",
		"['a''b']":                                     "",
		"[1]":                                          "",
		"'a'":                                          "",
	} {
		got, ok := parseStringArray(in)
		if strings.Join(got, "|") != want || ok != (want != "") {
			t.Errorf("%s: %v %v; want %q", in, got, ok, want)
		}
	}
}

func TestServiceTypeRefusalsNameTheirFunction(t *testing.T) {
	for typ, fn := range serviceTypeFunctions {
		code, msg := checkServiceType(strings.ToLower(typ))
		if typ == "CLOUD_AI_VISION_V1" {
			if code != 0 {
				t.Errorf("%s: %d %s", typ, code, msg)
			}
			continue
		}
		if code != 501 || !strings.Contains(msg, fn) {
			t.Errorf("%s: %d %s", typ, code, msg)
		}
	}
}

func TestGenerationOptionsBecomeGenerationConfig(t *testing.T) {
	cfg := defaultGenerationConfig()
	for k, v := range map[string]string{"max_output_tokens": "8", "temperature": "0.7", "top_p": "0.5", "top_k": "3", "stop_sequences": "['END', '']"} {
		if code, msg, ok := generationOption("ML.GENERATE_TEXT", "gemma", k, v, cfg); !ok || code != 0 {
			t.Fatalf("%s: %d %s %v", k, code, msg, ok)
		}
	}
	b, _ := json.Marshal(generateBody("hi", cfg))
	want := `{"contents":[{"parts":[{"text":"hi"}],"role":"user"}],"generationConfig":{"maxOutputTokens":8,"stopSequences":["END"],"temperature":0.7,"topK":3,"topP":0.5}}`
	if string(b) != want {
		t.Errorf("body %s\nwant %s", b, want)
	}
	if _, _, ok := generationOption("ML.GENERATE_TEXT", "gemma", "flatten_json_output", "TRUE", cfg); ok {
		t.Error("flatten_json_output taken as a generation option")
	}
	b, _ = json.Marshal(generateBody("hi", defaultGenerationConfig()))
	if !strings.Contains(string(b), `"generationConfig":{"maxOutputTokens":1024,"temperature":0,"topP":0.95}`) {
		t.Errorf("defaults %s", b)
	}
}
