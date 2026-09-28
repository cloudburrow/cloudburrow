package frontready

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Path is 200 while every backend port accepts connections and 503 once
// one does not; any other path goes to the next handler.
func TestWrapAnswersPathByTheBackendsPorts(t *testing.T) {
	a, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h := Wrap(next, a.Addr().String(), b.Addr().String())
	get := func(path string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code
	}
	if got := get(Path); got != http.StatusOK {
		t.Errorf("GET %s with both ports open = %d, want 200", Path, got)
	}
	if got := get("/bigquery/v2/projects/p/datasets"); got != http.StatusTeapot {
		t.Errorf("another path = %d, want the next handler's 418", got)
	}
	_ = b.Close()
	if got := get(Path); got != http.StatusServiceUnavailable {
		t.Errorf("GET %s with a port closed = %d, want 503", Path, got)
	}
}
