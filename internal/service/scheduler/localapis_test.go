package scheduler

import "testing"

// TestLocalTarget: a googleapis.com target goes to the instance's own
// endpoint of its API, an unserved one is refused, anything else is left.
func TestLocalTarget(t *testing.T) {
	r := (&Runner{}).WithLocalAPIs(func(api string) string {
		if api == "workflowexecutions" {
			return "http://127.0.0.1:9999"
		}
		return ""
	})
	got, err := r.localTarget("https://workflowexecutions.googleapis.com/v1/projects/p/locations/us-central1/workflows/w/executions")
	if err != nil || got != "http://127.0.0.1:9999/v1/projects/p/locations/us-central1/workflows/w/executions" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := r.localTarget("https://translate.googleapis.com/v3/x"); err == nil {
		t.Fatal("an unserved googleapis.com API was not refused")
	}
	if _, err := (&Runner{}).localTarget("https://storage.googleapis.com/b"); err == nil {
		t.Fatal("a runner without local APIs sent a googleapis.com target on")
	}
	if got, err := r.localTarget("http://localhost:8080/hook"); err != nil || got != "http://localhost:8080/hook" {
		t.Fatalf("a local target became %q, %v", got, err)
	}
}
