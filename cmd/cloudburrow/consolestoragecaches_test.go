package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	gcsbuiltin "github.com/cloudburrow/cloudburrow/internal/service/storage"
)

// Anywhere Caches, relocation and cancel (#1444) on a bucket's page: Create
// Anywhere Cache answers with its operation and the Anywhere caches tab
// lists the cache, saying no cache exists locally; Relocate bucket starts a
// relocation the Operations tab lists, and Cancel operation cancels it.
func TestStorageAnywhereCachesThroughTheConsole(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, _ := newObjectOpsProvider(t)
	for _, id := range []string{"createanywherecache", "changeanywherecache", "relocatebucket", "finalizerelocation", "canceloperation"} {
		if !hasAction(p.DetailActions(ctx, "p", []string{"other"}), id) {
			t.Errorf("the bucket's page does not offer %s", id)
		}
	}
	res, err := p.ActAtResult(ctx, "p", []string{"other"}, "createanywherecache", map[string]string{"zone": "us-central1-a", "ttl": "86400s", "admissionPolicy": "admit-on-first-miss"})
	if err != nil || res == nil {
		t.Fatalf("Create Anywhere Cache: %v", err)
	}
	d, err := p.Detail(ctx, "p", []string{"other"})
	if err != nil {
		t.Fatal(err)
	}
	caches := sectionNamed(d, "anywherecaches")
	if caches == nil || len(caches.Listing.Items) != 1 || caches.Listing.Items[0].Name != "us-central1-a" || !strings.Contains(caches.Listing.Note, "no cache tier") {
		t.Fatalf("the Anywhere caches tab is %+v", caches)
	}
	if _, err := p.ActAtResult(ctx, "p", []string{"other"}, "createanywherecache", map[string]string{"zone": "us-central1-a"}); err == nil {
		t.Error("a second cache in the zone was created")
	}
	if _, err := p.ActAtResult(ctx, "p", []string{"other"}, "relocatebucket", map[string]string{"location": "US-EAST1"}); err != nil {
		t.Fatalf("Relocate bucket: %v", err)
	}
	d, _ = p.Detail(ctx, "p", []string{"other"})
	ops := sectionNamed(d, "operations")
	var reloc string
	for _, op := range ops.Listing.Items {
		if op.Fields["Type"] == "relocate-bucket" {
			reloc = op.Name
		}
	}
	if reloc == "" {
		t.Fatalf("the Operations tab lists no relocation: %+v", ops)
	}
	if _, err := p.ActAtResult(ctx, "p", []string{"other"}, "canceloperation", map[string]string{"operation": reloc}); err != nil {
		t.Errorf("Cancel operation: %v", err)
	}
	if _, err := p.ActAtResult(ctx, "p", []string{"other"}, "canceloperation", map[string]string{"operation": reloc}); err == nil {
		t.Error("a cancelled relocation was cancelled again")
	}
}

// TestStorageAnywhereCachePauseThroughTheConsole: Change Anywhere Cache's
// pause goes through Storage Control's PauseAnywhereCache over gRPC on the
// storage port (#1791: the JSON API's anywhereCaches.pause is no longer in
// the discovery document), so pausing a paused cache is gRPC's
// FAILED_PRECONDITION; resume runs it again.
func TestStorageAnywhereCachePauseThroughTheConsole(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var mu sync.Mutex
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	srv, err := gcsbuiltin.NewServer(gcsbuiltin.Options{Now: func() time.Time { mu.Lock(); defer mu.Unlock(); return now }})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewUnstartedServer(srv)
	h.Config.Protocols = new(http.Protocols)
	h.Config.Protocols.SetHTTP1(true)
	h.Config.Protocols.SetUnencryptedHTTP2(true)
	h.Start()
	t.Cleanup(h.Close)
	p := storageProvider{endpoint: strings.TrimPrefix(h.URL, "http://")}
	if _, err := p.Create(ctx, "p", map[string]string{"name": "pausecache"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ActAtResult(ctx, "p", []string{"pausecache"}, "createanywherecache", map[string]string{"zone": "us-central1-a"}); err != nil {
		t.Fatalf("Create Anywhere Cache: %v", err)
	}
	mu.Lock()
	now = now.Add(time.Minute)
	mu.Unlock()
	state := func() string {
		t.Helper()
		d, err := p.Detail(ctx, "p", []string{"pausecache"})
		if err != nil {
			t.Fatal(err)
		}
		c := sectionNamed(d, "anywherecaches")
		if c == nil || len(c.Listing.Items) != 1 {
			t.Fatalf("the Anywhere caches tab is %+v", c)
		}
		return c.Listing.Items[0].Fields["State"]
	}
	if st := state(); st != "running" {
		t.Fatalf("created cache is %q, want running", st)
	}
	change := func(c string) error {
		_, err := p.ActAtResult(ctx, "p", []string{"pausecache"}, "changeanywherecache", map[string]string{"zone": "us-central1-a", "change": c})
		return err
	}
	if err := change("pause"); err != nil {
		t.Fatalf("Change Anywhere Cache pause: %v", err)
	}
	if st := state(); st != "paused" {
		t.Errorf("after pause the cache is %q, want paused", st)
	}
	if err := change("pause"); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("pause a paused cache = %v; want gRPC FAILED_PRECONDITION", err)
	}
	if err := change("resume"); err != nil {
		t.Fatalf("Change Anywhere Cache resume: %v", err)
	}
	if st := state(); st != "running" {
		t.Errorf("after resume the cache is %q, want running", st)
	}
}
