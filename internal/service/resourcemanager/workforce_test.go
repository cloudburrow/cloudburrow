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
	n := 0
	for id, served := range WorkloadIdentityMethods {
		tail, ok := strings.CutPrefix(id, "iam.locations.workforcePools.")
		if !ok || served {
			continue
		}
		n++
		method, path := "POST", base+"/staff-pool:"+tail
		if strings.HasPrefix(tail, "providers.scimTenants") {
			method, path = "GET", base+"/staff-pool/providers/p-one/scimTenants"
		}
		if code := wfCall(t, r, method, path, map[string]any{}); code != 501 {
			t.Errorf("%s (%s %s) = %d; want 501", id, method, path, code)
		}
	}
	if n == 0 {
		t.Fatal("no unserved workforce methods")
	}
}
