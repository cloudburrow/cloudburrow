package bigqueryfront

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeDocumentAI answers GET processors and :process as the local Document
// AI API does, and records what :process was sent.
func fakeDocumentAI(t *testing.T, sent *[]map[string]any) *httptest.Server {
	t.Helper()
	processors := map[string]string{
		"/v1/projects/42/locations/us/processors/aaaa": `{"name":"projects/42/locations/us/processors/aaaa","type":"LAYOUT_PARSER_PROCESSOR","state":"ENABLED",` +
			`"defaultProcessorVersion":"projects/42/locations/us/processors/aaaa/processorVersions/v1"}`,
		"/v1/projects/p/locations/us/processors/bbbb":  `{"name":"projects/p/locations/us/processors/bbbb","type":"OCR_PROCESSOR","state":"ENABLED"}`,
		"/v1/projects/42/locations/us/processors/cccc": `{"name":"projects/42/locations/us/processors/cccc","type":"LAYOUT_PARSER_PROCESSOR","state":"DISABLED"}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := processors[strings.TrimSuffix(r.URL.Path, ":process")]; ok && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, ":process") {
			var m map[string]any
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &m)
			*sent = append(*sent, m)
			_, _ = io.WriteString(w, `{"document":{"mimeType":"application/pdf","chunkedDocument":{"chunks":[{"chunkId":"c1","content":"hi"}]}}}`)
			return
		}
		if p, ok := processors[r.URL.Path]; ok && r.Method == http.MethodGet {
			_, _ = io.WriteString(w, p)
			return
		}
		w.WriteHeader(404)
		_, _ = io.WriteString(w, `{"error":{"code":404,"message":"Processor not found","status":"NOT_FOUND"}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDocumentProcessorRules(t *testing.T) {
	var sent []map[string]any
	f := front{vertexai: fakeDocumentAI(t, &sent).URL}
	ctx := context.Background()
	for _, c := range []struct {
		v, loc, want string
		code         int
	}{
		{"aaaa", "US", "projects/42/locations/us/processors/aaaa", 0},
		{"projects/42/locations/us/processors/aaaa/processorVersions/v1", "us", "projects/42/locations/us/processors/aaaa/processorVersions/v1", 0},
		{"projects/42/locations/us/processors/aaaa/processorVersions/v2", "us", "", 400}, // no such version
		{"bbbb", "US", "", 400}, // the OCR processor is not supported
		{"cccc", "US", "", 400}, // not enabled
		{"dddd", "US", "", 400}, // does not exist
		{"projects/42/locations/eu/processors/aaaa", "US", "", 400}, // another region
		{"aaaa", "us-central1", "", 400},                            // US and EU alone
		{"projects/42/processors/aaaa", "US", "", 400},              // malformed
	} {
		got, code, msg := f.documentProcessor(ctx, c.v, "p", 42, c.loc)
		if got != c.want || code != c.code {
			t.Errorf("%s in %s: %q %d %s; want %q %d", c.v, c.loc, got, code, msg, c.want, c.code)
		}
	}

	res, status := f.processDocument(ctx, "projects/42/locations/us/processors/aaaa", "application/pdf", []byte("%PDF-1.4"),
		json.RawMessage(`{"layout_config":{"chunking_config":{"chunk_size":250}}}`))
	if status != "" || !strings.Contains(string(res), `"chunkId":"c1"`) || len(sent) != 1 {
		t.Fatalf("processDocument: %s %q", res, status)
	}
	raw, _ := sent[0]["rawDocument"].(map[string]any)
	if raw["mimeType"] != "application/pdf" || raw["content"] != "JVBERi0xLjQ=" || sent[0]["processOptions"] == nil {
		t.Errorf("sent %v", sent[0])
	}
	if _, status := f.processDocument(ctx, "projects/42/locations/us/processors/zzzz", "image/png", []byte("x"), nil); !strings.Contains(status, "NOT_FOUND") {
		t.Errorf("unknown processor's status: %q", status)
	}
	if _, status := f.processDocument(ctx, "projects/42/locations/us/processors/aaaa", "", []byte("x"), nil); !strings.Contains(status, "content_type") {
		t.Errorf("no content type: %q", status)
	}
}

func TestObjectTableOfQuery(t *testing.T) {
	for q, want := range map[string]string{
		"SELECT uri, content_type FROM ds.docs WHERE size < 1000":        "ds.docs",
		"SELECT * FROM `p.ds.docs` LIMIT 3":                              "p.ds.docs",
		"SELECT uri, content_type FROM ds.docs d":                        "",
		"SELECT uri FROM ds.a JOIN ds.b USING (uri)":                     "",
		"SELECT uri FROM ds.a, ds.b":                                     "",
		"SELECT uri FROM (SELECT * FROM ds.a)":                           "",
		"SELECT uri FROM ds.a UNION ALL SELECT uri FROM ds.b":            "",
		"WITH x AS (SELECT * FROM ds.a) SELECT uri, content_type FROM x": "",
	} {
		got, ok := objectTableOfQuery(q)
		if strings.Join(got, ".") != want || ok != (want != "") {
			t.Errorf("%s: %v %v, want %q", q, got, ok, want)
		}
	}
}
