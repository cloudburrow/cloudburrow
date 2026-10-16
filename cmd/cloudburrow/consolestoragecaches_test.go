package main

import (
	"context"
	"strings"
	"testing"
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
