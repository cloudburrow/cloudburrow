package storage

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http/httptest"
	"testing"
)

// TestGunzipJSONBody: a JSON API body sent gzipped (Google's HTTP client
// for Java's objects.rewrite) is decoded; an upload's is the object's.
func TestGunzipJSONBody(t *testing.T) {
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write([]byte("{}"))
	_ = w.Close()
	r := httptest.NewRequest("POST", "/storage/v1/b/b/o/a/rewriteTo/b/b/o/c", bytes.NewReader(gz.Bytes()))
	r.Header.Set("Content-Encoding", "gzip")
	gunzipJSONBody(r)
	if b, _ := io.ReadAll(r.Body); string(b) != "{}" || r.Header.Get("Content-Encoding") != "" {
		t.Errorf("JSON body %q, Content-Encoding %q", b, r.Header.Get("Content-Encoding"))
	}
	u := httptest.NewRequest("POST", "/upload/storage/v1/b/b/o?uploadType=media&name=x", bytes.NewReader(gz.Bytes()))
	u.Header.Set("Content-Encoding", "gzip")
	gunzipJSONBody(u)
	if b, _ := io.ReadAll(u.Body); !bytes.Equal(b, gz.Bytes()) || u.Header.Get("Content-Encoding") != "gzip" {
		t.Error("an upload's gzip body was decoded")
	}
}
