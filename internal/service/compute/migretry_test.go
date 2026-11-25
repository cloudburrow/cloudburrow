package compute

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"
)

// flakyStartVMs fails the first fail starts, as a guest whose sshd never
// listened did (main run 45555303799: one of a group's two VMs).
type flakyStartVMs struct {
	fakeVMs
	mu     *sync.Mutex
	fail   *int
	starts *int
}

func (f flakyStartVMs) Start(context.Context, VMSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	*f.starts++
	if *f.fail > 0 {
		*f.fail--
		return errors.New("forward the SSH port: did not serve")
	}
	return nil
}

// TestManagedGroupRetriesAFailedCreation: a managed instance group whose
// VM failed to be created (its insert undone) makes it again, after a
// back-off that grows with each failure in a row, until it has its target
// size, as Google's groups keep trying ("creating" in currentActions); a
// group resized down in between, or deleted, makes none.
func TestManagedGroupRetriesAFailedCreation(t *testing.T) {
	now := time.Date(2026, 11, 25, 8, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	fail, starts := 2, 0
	s, err := New(Options{VMs: flakyStartVMs{fakeVMs: fakeVMs{free: 1 << 40}, mu: &mu, fail: &fail, starts: &starts},
		Now: func() time.Time { mu.Lock(); defer mu.Unlock(); return now }})
	if err != nil {
		t.Fatal(err)
	}
	pass := func(d time.Duration) {
		t.Helper()
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
		s.rollOnce()
		waitIdle(t, s)
	}
	call := func(method, path string, body obj) {
		t.Helper()
		if _, err := s.Call(method, "p", path, nil, body); err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		waitIdle(t, s)
	}
	live := func() int {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.liveManagedLocked("projects/p/zones/us-central1-a/instanceGroupManagers/web"))
	}
	call("POST", "global/instanceTemplates", obj{"name": "web", "properties": obj{"machineType": "e2-small",
		"disks": []any{map[string]any{"boot": true, "autoDelete": true, "initializeParams": map[string]any{"sourceImage": "projects/debian-cloud/global/images/family/debian-12"}}}}})
	call("POST", "zones/us-central1-a/instanceGroupManagers", obj{"name": "web", "baseInstanceName": "web",
		"instanceTemplate": "global/instanceTemplates/web", "targetSize": 2.0})
	if n := live(); n != 0 {
		t.Fatalf("both creations failed, yet %d VMs", n)
	}
	// Not before the back-off.
	pass(time.Second)
	if n := live(); n != 0 || starts != 2 {
		t.Fatalf("retried before the back-off: %d VMs, %d starts", n, starts)
	}
	pass(migRetryDelay * 2)
	if n := live(); n != 2 {
		t.Fatalf("after the back-off the group has %d VMs, want 2 (%d starts)", n, starts)
	}
	g, err := s.Call(http.MethodGet, "p", "zones/us-central1-a/instanceGroupManagers/web", nil, nil)
	if err != nil || g["targetSize"] != 2.0 {
		t.Fatalf("group: %v %v", g, err)
	}
	pass(migRetryMax)
	if n := live(); n != 2 || starts != 4 {
		t.Fatalf("a whole group made more VMs: %d VMs, %d starts", n, starts)
	}

	// A failure followed by a resize back down makes nothing more; a
	// deleted group neither, and forgets its retry.
	mu.Lock()
	fail = 1
	mu.Unlock()
	resize := func(n string) {
		t.Helper()
		if _, err := s.Call(http.MethodPost, "p", "zones/us-central1-a/instanceGroupManagers/web/resize", map[string][]string{"size": {n}}, nil); err != nil {
			t.Fatal(err)
		}
		waitIdle(t, s)
	}
	resize("3")
	if n := live(); n != 2 {
		t.Fatalf("the third VM failed, yet %d VMs", n)
	}
	resize("2")
	before := starts
	pass(migRetryMax)
	if n := live(); n != 2 || starts != before {
		t.Fatalf("a group resized back down made VMs: %d VMs, %d starts, was %d", n, starts, before)
	}
	mu.Lock()
	fail = 1
	mu.Unlock()
	resize("3")
	call("DELETE", "zones/us-central1-a/instanceGroupManagers/web", nil)
	before = starts
	pass(migRetryMax)
	if starts != before {
		t.Fatalf("a deleted group made VMs: %d starts, was %d", starts, before)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.migRetry) != 0 {
		t.Fatalf("retries left for a deleted group: %v", s.migRetry)
	}
}
