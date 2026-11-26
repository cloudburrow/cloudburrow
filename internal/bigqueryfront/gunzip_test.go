package bigqueryfront

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A gzip-compressed body (google-api-client's default for the Java
// clients' JSON bodies, #2215) reaches the front's checks uncompressed and
// without its Content-Encoding; an uncompressed one is unchanged; a body
// that is not gzip is 400.
func TestGunzipRequests(t *testing.T) {
	var got []byte
	var enc string
	h := gunzipRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		enc = r.Header.Get("Content-Encoding")
	}))
	body := `{"configuration":{"extract":{"destinationFormat":"AVRO"}}}`
	var z bytes.Buffer
	zw := gzip.NewWriter(&z)
	_, _ = zw.Write([]byte(body))
	_ = zw.Close()
	r := httptest.NewRequest(http.MethodPost, "/bigquery/v2/projects/p/jobs", &z)
	r.Header.Set("Content-Encoding", "gzip")
	h.ServeHTTP(httptest.NewRecorder(), r)
	if string(got) != body || enc != "" {
		t.Errorf("gzip body = %q, Content-Encoding %q; want %q and none", got, enc, body)
	}
	r = httptest.NewRequest(http.MethodPost, "/bigquery/v2/projects/p/jobs", bytes.NewReader([]byte(body)))
	h.ServeHTTP(httptest.NewRecorder(), r)
	if string(got) != body {
		t.Errorf("plain body = %q; want %q", got, body)
	}
	r = httptest.NewRequest(http.MethodPost, "/bigquery/v2/projects/p/jobs", bytes.NewReader([]byte(body)))
	r.Header.Set("Content-Encoding", "gzip")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("a body that is not gzip: %d; want 400", w.Code)
	}
}

// An extract to Apache Beam's DIR//*.avro (a directory ending in "/", then
// "/*.avro") is written to DIR/000000000000.avro, where Beam reads it
// (#2215); the bucket's name is left alone.
func TestExtractFilesCollapseSlashes(t *testing.T) {
	files := extractFiles(writtenExtract{uris: []string{"gs://b/tmp/BigQueryExtractTemp/u//*.avro"}}, [][]any{{"ada"}})
	if len(files) != 1 || files[0].name != "gs://b/tmp/BigQueryExtractTemp/u/000000000000.avro" {
		t.Errorf("files = %+v; want gs://b/tmp/BigQueryExtractTemp/u/000000000000.avro", files)
	}
	if got := collapseObjectSlashes("gs://b/a///c"); got != "gs://b/a/c" {
		t.Errorf("collapseObjectSlashes = %s", got)
	}
}
