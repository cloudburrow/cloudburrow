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

// Filter operations (#1280) is offered on a bucket's page and answers with
// the operations buckets.operations.list keeps for the filter; a malformed
// filter is the API's refusal.
func TestStorageFilterOperationsThroughTheConsole(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, c := newObjectOpsProvider(t)
	putObject(t, c, "other", "a.txt", "v", nil)
	if err := c.Bucket("other").Object("a.txt").Delete(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := p.ActAtResult(ctx, "p", []string{"other"}, "bulkrestore", nil); err != nil {
			t.Fatal(err)
		}
	}
	d, err := p.Detail(ctx, "p", []string{"other"})
	if err != nil {
		t.Fatal(err)
	}
	if !hasAction(d.Actions, "filteroperations") || !hasAction(p.DetailActions(ctx, "p", []string{"other"}), "filteroperations") {
		t.Fatal("the bucket's page does not offer Filter operations")
	}
	for filter, want := range map[string]int{"done = true": 2, `metadata.commonMetadata.type = "RENAME_FOLDER"`: 0} {
		res, err := p.ActAtResult(ctx, "p", []string{"other"}, "filteroperations", map[string]string{"filter": filter})
		if err != nil || len(res.Items) != want {
			t.Errorf("Filter operations %q = %+v, %v; want %d", filter, res, err, want)
		}
	}
	if _, err := p.ActAtResult(ctx, "p", []string{"other"}, "filteroperations", map[string]string{"filter": "kind = x"}); err == nil {
		t.Error("Filter operations with a field that is no Operation field succeeded")
	}
}
