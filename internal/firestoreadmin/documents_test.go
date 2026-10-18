package firestoreadmin

import (
	"net/http"
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

// TestAStreamsWritesAreMarkedAsTheyGo (#1615): a write answered in the
// middle of a long gRPC Write stream makes the documents dirty while the
// stream is still open, so the next export, not the stream's end, keeps
// it; a REST call is counted when it starts and ends only.
func TestAStreamsWritesAreMarkedAsTheyGo(t *testing.T) {
	d := &Documents{restored: make(chan struct{})}
	close(d.restored)
	answered := make(chan struct{})
	release := make(chan struct{})
	h := d.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("write response"))
		w.(http.Flusher).Flush()
		close(answered)
		<-release // the stream stays open
	}))
	r := httptest.NewRequest("POST", "/google.firestore.v1.Firestore/Write", nil)
	r.Header.Set("Content-Type", "application/grpc")
	go h.ServeHTTP(httptest.NewRecorder(), r)
	<-answered
	// The stream's start was counted and exported: only the answered
	// write makes the documents dirty again.
	d.exported.Store(1)
	if got := d.writes.Load(); got != 2 {
		t.Errorf("after a stream's first answered write, writes = %d, want 2 (its start and its write)", got)
	}
	if !d.dirty() {
		t.Error("a write answered mid-stream does not make the documents dirty")
	}
	close(release)

	d2 := &Documents{restored: make(chan struct{})}
	close(d2.restored)
	d2.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("{}"))
		if _, ok := w.(*markingWriter); ok {
			t.Error("a REST call's response writer is wrapped")
		}
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("PATCH", "/v1/projects/p/databases/(default)/documents/c/d", nil))
	if got := d2.writes.Load(); got != 2 {
		t.Errorf("a REST write: writes = %d, want 2 (its start and end)", got)
	}
}
