package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	runv1 "google.golang.org/api/run/v1"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// fakeRunV1InstanceIAM serves v1's projects.locations.instances IAM methods
// for one instance, refusing a stale etag as the adapter does.
type fakeRunV1InstanceIAM struct {
	mu     sync.Mutex
	policy runv1.Policy
	gen    int
	paths  []string
}

func (f *fakeRunV1InstanceIAM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paths = append(f.paths, r.Method+" "+r.URL.Path)
	const res = "/v1/projects/demo/locations/us-central1/instances/web"
	if f.policy.Etag == "" {
		f.policy.Etag = base64.StdEncoding.EncodeToString([]byte("0"))
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == res+":getIamPolicy":
		_ = json.NewEncoder(w).Encode(&f.policy)
	case r.Method == http.MethodPost && r.URL.Path == res+":setIamPolicy":
		var req runv1.SetIamPolicyRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Policy.Etag != f.policy.Etag {
			http.Error(w, `{"error":{"code":409,"message":"the policy's etag does not match the current policy","status":"ABORTED"}}`, http.StatusConflict)
			return
		}
		f.gen++
		f.policy = runv1.Policy{Version: 1, Bindings: req.Policy.Bindings, Etag: base64.StdEncoding.EncodeToString([]byte(strconv.Itoa(f.gen)))}
		_ = json.NewEncoder(w).Encode(&f.policy)
	case r.Method == http.MethodPost && r.URL.Path == res+":testIamPermissions":
		var req runv1.TestIamPermissionsRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		_ = json.NewEncoder(w).Encode(&runv1.TestIamPermissionsResponse{Permissions: req.Permissions})
	case strings.HasPrefix(r.URL.Path, "/v1/projects/demo/locations/us-central1/instances/"):
		http.Error(w, `{"error":{"code":404,"message":"instance not found","status":"NOT_FOUND"}}`, http.StatusNotFound)
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotImplemented)
	}
}

// TestConsoleRunInstancePermissions (#1556): an instance's page has a
// Permissions tab, and only the instance's page; the tab reads, grants and
// tests through v1's projects.locations.instances getIamPolicy,
// setIamPolicy and testIamPermissions, since v2's Instances has no IAM
// methods; a change made with a stale etag is refused with the service's
// message; a missing instance's policy is the service's 404.
func TestConsoleRunInstancePermissions(t *testing.T) {
	fake := &fakeRunV1InstanceIAM{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	p := runInstancesProvider{runAddr: func() string { return host }, defaultProject: "demo"}
	ctx := context.Background()

	if p.PolicyOn(ctx, []string{"web"}) == nil {
		t.Fatal("an instance's page has no Permissions tab")
	}
	if p.PolicyOn(ctx, []string{"web", "more"}) != nil {
		t.Error("a path below an instance has a Permissions tab")
	}
	pol, err := p.GetPolicy(ctx, "", []string{"web"})
	if err != nil || len(pol.Bindings) != 0 {
		t.Fatalf("GetPolicy = %+v, %v; want the empty policy", pol, err)
	}
	stale := pol
	pol.Bindings = []console.Binding{{Role: "roles/run.invoker", Members: []string{"user:ada@example.com"}}}
	if err := p.SetPolicy(ctx, "", []string{"web"}, pol); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}
	got, err := p.GetPolicy(ctx, "demo", []string{"web"})
	if err != nil || len(got.Bindings) != 1 || got.Bindings[0].Role != "roles/run.invoker" || got.Bindings[0].Members[0] != "user:ada@example.com" {
		t.Fatalf("GetPolicy after the grant = %+v, %v", got, err)
	}
	if got.Etag == stale.Etag {
		t.Errorf("the etag did not change with the grant: %q", got.Etag)
	}
	stale.Bindings = nil
	if err := p.SetPolicy(ctx, "", []string{"web"}, stale); err == nil || !strings.Contains(err.Error(), "etag does not match") {
		t.Errorf("SetPolicy with a stale etag = %v; want the service's refusal", err)
	}
	held, err := p.TestPermissions(ctx, "", []string{"web"}, []string{"run.instances.get", "run.instances.delete"})
	if err != nil || len(held) != 2 {
		t.Errorf("TestPermissions = %v, %v", held, err)
	}
	if _, err := p.GetPolicy(ctx, "", []string{"nope"}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("GetPolicy of a missing instance = %v; want the service's 404", err)
	}
	for _, path := range fake.paths {
		if !strings.HasPrefix(path, "GET /v1/projects/demo/locations/us-central1/instances/") &&
			!strings.HasPrefix(path, "POST /v1/projects/demo/locations/us-central1/instances/") {
			t.Errorf("the tab sent %s; want only v1 instance IAM calls", path)
		}
	}
}
