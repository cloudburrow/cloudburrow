package spannerfront

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"
)

// TestCMEKRESTDataGate (#1972): a customer-managed database's REST data
// calls are claimed, and refused with FAILED_PRECONDITION, only while its
// key version is disabled; DeleteSession, other databases' calls and the
// admin routes are not claimed by the gate.
func TestCMEKRESTDataGate(t *testing.T) {
	ctx := context.Background()
	s, k, key := newKeyedFront(t)
	db := cmekProject + "/instances/i/databases/d"
	calls := []struct{ method, path string }{
		{"POST", "/v1/" + db + "/sessions"},
		{"POST", "/v1/" + db + "/sessions:batchCreate"},
		{"GET", "/v1/" + db + "/sessions/s1"},
		{"GET", "/v1/" + db + "/sessions"},
		{"POST", "/v1/" + db + "/sessions/s1:executeSql"},
		{"POST", "/v1/" + db + "/sessions/s1:executeStreamingSql"},
		{"POST", "/v1/" + db + "/sessions/s1:commit"},
		{"POST", "/v1/" + db + "/sessions/s1:read"},
		{"POST", "/v1/" + db + "/sessions/s1:batchWrite"},
	}
	claimed := func(method, path string) bool {
		return s.Claims(httptest.NewRequest(method, path, nil))
	}
	for _, c := range calls {
		if claimed(c.method, c.path) {
			t.Fatalf("%s %s claimed with no customer-managed database", c.method, c.path)
		}
	}
	rec, err := s.databaseKeyRecord(ctx, "CreateDatabase", db, []string{key})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.dbKeys[db] = rec
	s.mu.Unlock()
	for _, c := range calls {
		if claimed(c.method, c.path) {
			t.Fatalf("%s %s claimed while the key is enabled", c.method, c.path)
		}
	}
	setSpannerKeyVersion(t, k, key, kmspb.CryptoKeyVersion_DISABLED)
	for _, c := range calls {
		r := httptest.NewRequest(c.method, c.path, nil)
		if !s.Claims(r) {
			t.Fatalf("%s %s not claimed with the key disabled", c.method, c.path)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		var body struct {
			Error struct {
				Code   int
				Status string
			}
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != http.StatusBadRequest ||
			body.Error.Status != "FAILED_PRECONDITION" {
			t.Fatalf("%s %s = %d %s, want 400 FAILED_PRECONDITION", c.method, c.path, w.Code, w.Body)
		}
	}
	if claimed("DELETE", "/v1/"+db+"/sessions/s1") {
		t.Fatal("DeleteSession claimed: closing a session reads nothing")
	}
	if claimed("POST", "/v1/"+cmekProject+"/instances/i/databases/other/sessions") {
		t.Fatal("another database's CreateSession claimed")
	}
	setSpannerKeyVersion(t, k, key, kmspb.CryptoKeyVersion_ENABLED)
	if claimed("POST", "/v1/"+db+"/sessions/s1:executeSql") {
		t.Fatal("executeSql claimed after the key was re-enabled")
	}
}
