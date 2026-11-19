package callfront

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/frontstate"
)

// The Datastore front asks the Firestore front's Admin API whether the
// Datastore API serves a database, and refuses as it says (#1880).
func TestDatastoreGateAsksTheAdminAPI(t *testing.T) {
	var mu sync.Mutex
	asked := map[string]int{}
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != frontstate.FirestoreAccessPath || r.URL.Query().Get("api") != "datastore" {
			http.NotFound(w, r)
			return
		}
		name := r.URL.Query().Get("name")
		mu.Lock()
		asked[name]++
		mu.Unlock()
		ans := map[string]any{"code": 0}
		if strings.HasSuffix(name, "/native-db") {
			ans = map[string]any{"code": int(codes.FailedPrecondition), "message": "The Cloud Datastore API is not available for Firestore in Native mode database " + name + "."}
		}
		_ = json.NewEncoder(w).Encode(ans)
	}))
	defer admin.Close()
	g := newDatastoreGate(strings.TrimPrefix(admin.URL, "http://"))
	now := time.Unix(0, 0)
	g.now = func() time.Time { return now }

	grpcCall := func(params string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/google.datastore.v1.Datastore/Lookup", nil)
		r.Header.Set("Content-Type", "application/grpc")
		r.Header.Set("X-Goog-Request-Params", params)
		return r
	}
	if g.Claims(grpcCall("project_id=p&database_id=ds-db")) {
		t.Error("a Datastore mode database's call was claimed")
	}
	refused := grpcCall("project_id=p&database_id=native-db")
	if !g.Claims(refused) {
		t.Fatal("a Native mode database's call was not claimed")
	}
	w := httptest.NewRecorder()
	g.ServeHTTP(w, refused)
	if w.Header().Get("Grpc-Status") != "9" || !strings.Contains(w.Header().Get("Grpc-Message"), "Native mode database projects/p/databases/native-db") {
		t.Errorf("refusal: %v", w.Header())
	}
	// Answers are kept for gateTTL.
	g.Claims(grpcCall("project_id=p&database_id=native-db"))
	mu.Lock()
	if n := asked["projects/p/databases/native-db"]; n != 1 {
		t.Errorf("asked %d times within gateTTL, want 1", n)
	}
	mu.Unlock()
	// No database_id is (default).
	g.Claims(grpcCall("project_id=p"))
	mu.Lock()
	if asked["projects/p/databases/(default)"] != 1 {
		t.Errorf("asked = %v, want (default) asked", asked)
	}
	mu.Unlock()

	// A REST call names its database in its body, which is put back.
	body := `{"databaseId":"native-db","keys":[]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/projects/p:lookup", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if !g.Claims(r) {
		t.Fatal("a REST call on a Native mode database was not claimed")
	}
	w = httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "FAILED_PRECONDITION") {
		t.Errorf("REST refusal = %d %s", w.Code, w.Body)
	}
	r = httptest.NewRequest(http.MethodPost, "/v1/projects/p:lookup", strings.NewReader(`{"databaseId":"ds-db"}`))
	r.Header.Set("Content-Type", "application/json")
	if g.Claims(r) {
		t.Error("a REST call on a Datastore mode database was claimed")
	}
	if b, _ := io.ReadAll(r.Body); string(b) != `{"databaseId":"ds-db"}` {
		t.Errorf("the body passed on = %q", b)
	}

	// With the Admin API gone, the last answer stands and a database it
	// never answered for is served.
	admin.Close()
	now = now.Add(2 * gateTTL)
	if !g.Claims(grpcCall("project_id=p&database_id=native-db")) {
		t.Error("with the Admin API gone, the last refusal no longer stands")
	}
	if g.Claims(grpcCall("project_id=p&database_id=never-asked")) {
		t.Error("with the Admin API gone, a database never answered for is refused")
	}
}
