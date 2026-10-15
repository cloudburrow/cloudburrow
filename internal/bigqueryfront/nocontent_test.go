package bigqueryfront

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNoContentWriterAnswersAnEmptyDelete204(t *testing.T) {
	for _, c := range []struct {
		name   string
		length string
		code   int
		body   string
		want   int
	}{
		{"empty 200", "0", http.StatusOK, "", http.StatusNoContent},
		{"200 with a body", "2", http.StatusOK, "{}", http.StatusOK},
		{"404", "0", http.StatusNotFound, "", http.StatusNotFound},
		{"no length", "", http.StatusOK, "", http.StatusOK},
	} {
		rec := httptest.NewRecorder()
		w := &noContentWriter{ResponseWriter: rec}
		if c.length != "" {
			w.Header().Set("Content-Length", c.length)
		}
		w.WriteHeader(c.code)
		if c.body != "" {
			_, _ = w.Write([]byte(c.body))
		}
		if rec.Code != c.want {
			t.Errorf("%s: status %d, want %d", c.name, rec.Code, c.want)
		}
		if c.want == http.StatusNoContent && rec.Header().Get("Content-Length") != "" {
			t.Errorf("%s: a 204 kept Content-Length %q", c.name, rec.Header().Get("Content-Length"))
		}
	}
}
