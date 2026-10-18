package firestoreadmin

import (
	"net/http/httptest"
	"testing"
)

// TestMayWrite: every Firestore and Admin call but the reads is counted
// as one that may write (#1615), over gRPC and REST.
func TestMayWrite(t *testing.T) {
	grpc := func(path string) bool {
		r := httptest.NewRequest("POST", path, nil)
		r.Header.Set("Content-Type", "application/grpc")
		return mayWrite(r)
	}
	rest := func(method, path string) bool { return mayWrite(httptest.NewRequest(method, path, nil)) }
	for path, want := range map[string]bool{
		"/google.firestore.v1.Firestore/Commit":                     true,
		"/google.firestore.v1.Firestore/BatchWrite":                 true,
		"/google.firestore.v1.Firestore/Write":                      true,
		"/google.firestore.v1.Firestore/ExecutePipeline":            true,
		"/google.firestore.v1.Firestore/RunQuery":                   false,
		"/google.firestore.v1.Firestore/BatchGetDocuments":          false,
		"/google.firestore.v1.Firestore/Listen":                     false,
		"/google.firestore.admin.v1.FirestoreAdmin/RestoreDatabase": true,
		"/google.firestore.admin.v1.FirestoreAdmin/GetDatabase":     false,
		"/google.firestore.admin.v1.FirestoreAdmin/ListBackups":     false,
		"/google.longrunning.Operations/GetOperation":               false,
	} {
		if got := grpc(path); got != want {
			t.Errorf("gRPC %s: mayWrite = %v, want %v", path, got, want)
		}
	}
	for _, c := range []struct {
		method, path string
		want         bool
	}{
		{"POST", "/v1/projects/p/databases/(default)/documents/c", true},
		{"PATCH", "/v1/projects/p/databases/(default)/documents/c/d", true},
		{"DELETE", "/v1/projects/p/databases/(default)/documents/c/d", true},
		{"POST", "/v1/projects/p/databases/(default)/documents:commit", true},
		{"POST", "/v1/projects/p/databases/(default)/documents:runQuery", false},
		{"POST", "/v1/projects/p/databases/(default)/documents:batchGet", false},
		{"GET", "/v1/projects/p/databases/(default)/documents/c/d", false},
		{"POST", "/emulator/v1/projects/p:export", false},
	} {
		if got := rest(c.method, c.path); got != c.want {
			t.Errorf("%s %s: mayWrite = %v, want %v", c.method, c.path, got, c.want)
		}
	}
}
