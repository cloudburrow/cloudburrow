package resourcemanager

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/store"
	"github.com/cloudburrow/cloudburrow/internal/wif"
)

func wfCall(t *testing.T, r *Registry, method, path string, body any) int {
	t.Helper()
	b, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	r.ServeWorkforceIdentity(rec, httptest.NewRequest(method, path, bytes.NewReader(b)))
	return rec.Code
}

// Workforce pools (#2056): a subject made by an exchange, deleted (its
// exchanges refused, its principal inactive) and undeleted; revoked
// sessions; and every unserved method answering 501.
func TestWorkforceSubjectsAndUnserved(t *testing.T) {
	r := New(store.NewMemory())
	base := "/v1/locations/global/workforcePools"
	if code := wfCall(t, r, "POST", base+"?workforcePoolId=staff-pool", map[string]any{"parent": OrganizationName}); code != 200 {
		t.Fatalf("create pool = %d", code)
	}
	principal := wif.WorkforcePrincipal("staff-pool", "alice")
	if err := r.RecordFederatedPrincipal(principal, nil); err != nil {
		t.Fatal(err)
	}
	if !r.FederatedPrincipalActive(principal) {
		t.Fatal("a new subject is not active")
	}
	issued := time.Now()
	if code := wfCall(t, r, "POST", base+"/staff-pool/subjects/alice:revokeSessions", map[string]any{}); code != 200 {
		t.Fatalf("revokeSessions = %d", code)
	}
	if r.FederatedSessionLive(principal, issued) || !r.FederatedSessionLive(principal, time.Now().Add(time.Second)) {
		t.Fatal("revokeSessions did not end the earlier session only")
	}
	if code := wfCall(t, r, "DELETE", base+"/staff-pool/subjects/alice", nil); code != 200 {
		t.Fatalf("delete subject = %d", code)
	}
	if r.FederatedPrincipalActive(principal) || r.RecordFederatedPrincipal(principal, nil) != wif.ErrSubjectDeleted {
		t.Fatal("a deleted subject is still active or exchanges")
	}
	if code := wfCall(t, r, "POST", base+"/staff-pool/subjects/alice:undelete", map[string]any{}); code != 200 || !r.FederatedPrincipalActive(principal) {
		t.Fatalf("undelete subject = %d", code)
	}
	if code := wfCall(t, r, "DELETE", base+"/staff-pool/subjects/nobody", nil); code != 404 {
		t.Fatalf("delete of an unknown subject = %d; want 404", code)
	}
	for id, served := range WorkloadIdentityMethods {
		if strings.HasPrefix(id, "iam.locations.workforcePools.") && !served {
			t.Errorf("%s is not served", id)
		}
	}
}

// A workforce pool's own IAM policy (#2065): set, read back with its etag,
// testIamPermissions keeping Workforce Identity Federation's permissions,
// an unknown pool's testIamPermissions empty (not NOT_FOUND) and its
// getIamPolicy 404, a deleted pool's setIamPolicy FAILED_PRECONDITION.
func TestWorkforcePoolIAMPolicy(t *testing.T) {
	r := New(store.NewMemory())
	base := "/v1/locations/global/workforcePools"
	if code := wfCall(t, r, "POST", base+"?workforcePoolId=iam-pool", map[string]any{"parent": OrganizationName}); code != 200 {
		t.Fatalf("create pool = %d", code)
	}
	call := func(method, path string, body any) (int, map[string]any) {
		t.Helper()
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeWorkforceIdentity(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	member := "principal://iam.googleapis.com/locations/global/workforcePools/iam-pool/subject/alice"
	code, out := call("POST", base+"/iam-pool:setIamPolicy", map[string]any{"policy": map[string]any{
		"bindings": []any{map[string]any{"role": "roles/iam.workforcePoolViewer", "members": []string{member}}}}})
	if code != 200 || out["etag"] == nil {
		t.Fatalf("setIamPolicy = %d %v", code, out)
	}
	code, out = call("POST", base+"/iam-pool:getIamPolicy", map[string]any{})
	if b, _ := json.Marshal(out); code != 200 || !strings.Contains(string(b), member) {
		t.Fatalf("getIamPolicy = %d %s", code, b)
	}
	code, out = call("POST", base+"/iam-pool:testIamPermissions", map[string]any{"permissions": []string{"iam.workforcePools.get", "storage.buckets.get"}})
	if b, _ := json.Marshal(out["permissions"]); code != 200 || string(b) != `["iam.workforcePools.get"]` {
		t.Fatalf("testIamPermissions = %d %s", code, b)
	}
	code, out = call("POST", base+"/no-such-pool:testIamPermissions", map[string]any{"permissions": []string{"iam.workforcePools.get"}})
	if code != 200 || out["permissions"] != nil {
		t.Fatalf("testIamPermissions of an unknown pool = %d %v", code, out)
	}
	if code, _ := call("POST", base+"/no-such-pool:getIamPolicy", map[string]any{}); code != 404 {
		t.Fatalf("getIamPolicy of an unknown pool = %d", code)
	}
	if code := wfCall(t, r, "DELETE", base+"/iam-pool", nil); code != 200 {
		t.Fatalf("delete pool = %d", code)
	}
	if code, _ := call("POST", base+"/iam-pool:setIamPolicy", map[string]any{"policy": map[string]any{}}); code != 400 {
		t.Fatalf("setIamPolicy of a deleted pool = %d; want 400", code)
	}
}
