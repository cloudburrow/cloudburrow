package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/faultgate"
)

// TestFaultRulesReachTheCluster (#2113): a rule for a service served in
// the cluster is accepted once the service is interposed remotely, and a
// front's gate, asking the host through faultgate, fails the matching
// gRPC call with the rule's status, once for a count-1 rule, and passes a
// call another project's or method's; the storage Faulter gets the HTTP
// status.
func TestFaultRulesReachTheCluster(t *testing.T) {
	f := NewFaults(NewRecorder(100, time.Now))
	add := func(body string) int {
		rec := httptest.NewRecorder()
		f.handleAdd(rec, httptest.NewRequest(http.MethodPost, "/admin/faults", strings.NewReader(body)))
		return rec.Code
	}
	if code := add(`{"service":"pubsub","method":"Publish","code":"PERMISSION_DENIED","count":1}`); code != http.StatusBadRequest {
		t.Fatalf("a pubsub rule before InterposeRemote = %d, want 400", code)
	}
	f.InterposeRemote("pubsub", "storage")
	if code := add(`{"service":"pubsub","method":"Publish","project":"p","code":"PERMISSION_DENIED","count":1}`); code != http.StatusCreated {
		t.Fatalf("a pubsub rule = %d, want 201", code)
	}
	if code := add(`{"service":"storage","method":"storage.buckets.get","httpStatus":503,"count":1}`); code != http.StatusCreated {
		t.Fatalf("a storage rule = %d, want 201", code)
	}
	host := httptest.NewServer(faultgate.Handler(f))
	defer host.Close()
	faultgate.Use(&faultgate.Remote{Base: host.URL})
	defer faultgate.Use(nil)

	served := 0
	gate := faultgate.Gate("pubsub", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { served++ }))
	call := func(method, resource string) string {
		req := httptest.NewRequest(http.MethodPost, "/google.pubsub.v1.Publisher/"+method, nil)
		req.Header.Set("Content-Type", "application/grpc")
		req.Header.Set("X-Goog-Request-Params", "topic="+resource)
		rec := httptest.NewRecorder()
		gate.ServeHTTP(rec, req)
		return rec.Header().Get("Grpc-Status")
	}
	if got := call("Publish", "projects/other/topics/t"); got != "" {
		t.Errorf("another project's Publish = status %q, want it served", got)
	}
	if got := call("GetTopic", "projects/p/topics/t"); got != "" {
		t.Errorf("GetTopic = status %q, want it served", got)
	}
	if got := call("Publish", "projects/p/topics/t"); got != "7" {
		t.Errorf("Publish = status %q, want 7 (PERMISSION_DENIED)", got)
	}
	if got := call("Publish", "projects/p/topics/t"); got != "" {
		t.Errorf("Publish after the count-1 rule's fault = status %q, want it served", got)
	}
	if served != 3 {
		t.Errorf("the front served %d calls, want 3", served)
	}
	st, _, ok := faultgate.Faulter{Service: "storage"}.Fault("storage.buckets.get", "b/x")
	if !ok || st != 503 {
		t.Errorf("storage Fault = %d, %v; want 503", st, ok)
	}
	if _, _, ok := (faultgate.Faulter{Service: "storage"}).Fault("storage.buckets.get", "b/x"); ok {
		t.Error("the storage rule faulted twice")
	}
	if evs := f.rec.Events("pubsub", 10); len(evs) == 0 {
		t.Error("no pubsub fault event was recorded")
	}
}
