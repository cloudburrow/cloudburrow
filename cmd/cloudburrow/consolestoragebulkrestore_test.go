package main

import (
	"context"
	"testing"
)

// Bulk restore deleted objects (#1279) is offered on a bucket's page and by
// the action route; with a glob it restores only the matching soft-deleted
// objects, answers with its operation and counts, and the bucket's page
// then has an Operations tab listing it.
func TestStorageBulkRestoreThroughTheConsole(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, c := newObjectOpsProvider(t)
	for _, name := range []string{"logs/a.txt", "logs/b.txt", "img/c.png"} {
		putObject(t, c, "other", name, "v", nil)
		if err := c.Bucket("other").Object(name).Delete(ctx); err != nil {
			t.Fatal(err)
		}
	}
	d, err := p.Detail(ctx, "p", []string{"other"})
	if err != nil {
		t.Fatal(err)
	}
	if !hasAction(d.Actions, "bulkrestore") || !hasAction(p.DetailActions(ctx, "p", []string{"other"}), "bulkrestore") {
		t.Fatal("the bucket's page does not offer Bulk restore deleted objects")
	}
	if sectionNamed(d, "operations") != nil {
		t.Error("a plain bucket with no operation shows an Operations tab")
	}
	res, err := p.ActAtResult(ctx, "p", []string{"other"}, "bulkrestore", map[string]string{"matchGlobs": "logs/**"})
	if err != nil {
		t.Fatalf("Bulk restore: %v", err)
	}
	got := map[string]string{}
	for _, r := range res.Items {
		got[r.Name] = r.Fields["Value"]
	}
	if got["State"] != "Done" || got["Restored"] != "2" || got["Type"] != "BULK_RESTORE_OBJECTS" {
		t.Errorf("Bulk restore answers %v", got)
	}
	if v := readObject(t, c, "other", "logs/b.txt"); v != "v" {
		t.Errorf("logs/b.txt after the bulk restore reads %q", v)
	}
	d, err = p.Detail(ctx, "p", []string{"other"})
	if err != nil {
		t.Fatal(err)
	}
	ops := sectionNamed(d, "operations")
	if ops == nil || len(ops.Listing.Items) != 1 || ops.Listing.Items[0].Fields["Type"] != "BULK_RESTORE_OBJECTS" {
		t.Errorf("the Operations tab after the bulk restore is %+v", ops)
	}
	if _, err := p.ActAtResult(ctx, "p", []string{"other"}, "bulkrestore", map[string]string{"softDeletedAfterTime": "soon"}); err == nil {
		t.Error("Bulk restore with a malformed time succeeded")
	}
}
