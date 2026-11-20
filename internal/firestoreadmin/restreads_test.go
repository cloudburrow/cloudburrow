package firestoreadmin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/firestoreadmin/fstest"
)

// TestRESTReadsAtAReadTimeOrInATransaction (#1942): the REST gets and lists
// the emulator's gateway cannot answer (an RFC 3339 readTime, a
// transaction) are claimed and served through gRPC; any other call is the
// emulator's.
func TestRESTReadsAtAReadTimeOrInATransaction(t *testing.T) {
	docs, addr, _, _ := fstest.Serve(t)
	db := "projects/p/databases/(default)"
	docs.Seed(db)
	s := New(addr).RESTReads()
	base := "/v1/" + db + "/documents"
	for _, c := range []struct {
		method, url string
		claimed     bool
	}{
		{http.MethodGet, base + "/cities?readTime=2026-11-20T08:00:00.123456Z", true},
		{http.MethodGet, base + "/cities/sf?transaction=EQIAAAAAAAAA", true},
		{http.MethodGet, base + "/cities/sf", false},
		{http.MethodGet, base + "/cities/sf?readTime.seconds=1", false},
		{http.MethodPost, base + ":runQuery?readTime=2026-11-20T08:00:00Z", false},
		{http.MethodGet, base + "?readTime=2026-11-20T08:00:00Z", false},
	} {
		if got := s.Claims(httptest.NewRequest(c.method, c.url, nil)); got != c.claimed {
			t.Errorf("%s %s claimed = %v, want %v", c.method, c.url, got, c.claimed)
		}
	}

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, base+"/cities?readTime=2026-11-20T08:00:00.123456Z&pageSize=10", nil))
	var list struct {
		Documents []struct{ Name string } `json:"documents"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); w.Code != http.StatusOK || err != nil || len(list.Documents) == 0 ||
		!strings.HasPrefix(list.Documents[0].Name, db+"/documents/cities/") {
		t.Errorf("list at a read time = %d %s", w.Code, w.Body)
	}
	for _, q := range []string{"readTime=yesterday", "transaction=%%%", "readTime=2026-11-20T08:00:00Z&transaction=EQIAAAAAAAAA", "readTime=2026-11-20T08:00:00Z&pageSize=many"} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, base+"/cities?"+strings.ReplaceAll(q, "%%%", "%21%21"), nil))
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "INVALID_ARGUMENT") {
			t.Errorf("list with %s = %d %s, want 400 INVALID_ARGUMENT", q, w.Code, w.Body)
		}
	}
}
