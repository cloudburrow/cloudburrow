package main

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// Cloud Storage console parity, round 3 (#1843): the bucket page's tabs in
// Google's order with Observability and Inventory reports; soft-deleted
// objects in the Objects tab's Show list rather than a tab; an object's
// Version history with Restore confirmed by Confirm, and its soft-deleted
// generations in that tab's Show list; the Object versioning dialog's
// recommended lifecycle rules; and Google's words on the restore job pane,
// Create a key for a service account and the Buckets list's View options.
func TestStorageConsoleParityRound3(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, c := newObjectOpsProvider(t)

	d, err := p.Detail(ctx, "p", []string{"ops"})
	if err != nil {
		t.Fatal(err)
	}
	var tabs []string
	for _, s := range d.Sections {
		if s.ShownIn == "" {
			tabs = append(tabs, s.ID)
		}
	}
	if got := strings.Join(tabs, ","); !strings.HasPrefix(got, "objects,configuration,permissions,protection,lifecycle,observability,inventory,") {
		t.Errorf("the bucket's tabs are %s", got)
	}
	if del := sectionNamed(d, "deleted"); del == nil || del.ShownIn != "objects" || del.ShowAs != "Soft-deleted objects only" {
		t.Errorf("the soft-deleted objects are %+v; want them in the Objects tab's Show list", del)
	}
	if obs := sectionNamed(d, "observability"); obs == nil || !strings.HasPrefix(obs.Unavailable, "Not available locally") {
		t.Errorf("the Observability tab is %+v", obs)
	}
	if inv := sectionNamed(d, "inventory"); inv == nil || inv.Label != "Inventory reports" || !strings.Contains(inv.Unavailable, "not served") {
		t.Errorf("without Storage Insights the Inventory reports tab is %+v", inv)
	}

	// The Object versioning dialog: the limits are the recommended rules',
	// read only while that box is checked.
	meta := func() string {
		return actionByID(t, p.DetailActions(ctx, "p", []string{"ops"}), "editversioning").Fields[0].Default
	}
	v := actionByID(t, p.DetailActions(ctx, "p", []string{"ops"}), "editversioning")
	if v.Title != "Object versioning" || v.Submit != "Confirm" || v.Fields[2].Label != "Add recommended lifecycle rules to manage version costs" {
		t.Errorf("Edit object versioning is %+v", v)
	}
	if err := p.ActAt(ctx, "p", []string{"ops"}, "editversioning", map[string]string{"metageneration": meta(),
		"versioning": "Enabled", "recommended": "true"}); err == nil {
		t.Error("the recommended rules were accepted with no limit")
	}
	if err := p.ActAt(ctx, "p", []string{"ops"}, "editversioning", map[string]string{"metageneration": meta(),
		"versioning": "Enabled", "recommended": "false", "maxVersions": "3"}); err != nil {
		t.Fatal(err)
	}
	if a, _ := c.Bucket("ops").Attrs(ctx); !a.VersioningEnabled || len(a.Lifecycle.Rules) != 0 {
		t.Fatalf("unchecked, the recommended rules were added: versioning %v, rules %+v", a.VersioningEnabled, a.Lifecycle.Rules)
	}

	// Two generations of one object: Version history lists both, the
	// noncurrent one with Restore, confirmed by Confirm.
	for _, body := range []string{"one", "two"} {
		w := c.Bucket("ops").Object("v.txt").NewWriter(ctx)
		_, _ = w.Write([]byte(body))
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	od, err := p.Detail(ctx, "p", objectPath("ops", "v.txt"))
	if err != nil {
		t.Fatal(err)
	}
	hist := sectionNamed(od, "versions")
	if hist == nil || hist.Label != "Version history" || hist.Show != "Live and noncurrent objects" || len(hist.Listing.Items) != 2 {
		t.Fatalf("the object's Version history is %+v", hist)
	}
	var restore *console.Action
	var target []string
	for _, row := range hist.Listing.Items {
		for i := range row.Actions {
			if row.Actions[i].ID == "restoreversion" {
				restore, target = &row.Actions[i], row.Target
			}
		}
	}
	if restore == nil || restore.Label != "Restore" || restore.Submit != "Confirm" ||
		restore.ConfirmTitle != "Restore object version" || restore.ConfirmStyle != console.DeleteConfirmClick {
		t.Fatalf("the noncurrent version's Restore is %+v", restore)
	}
	if err := p.ActAt(ctx, "p", target, "restoreversion", nil); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	r, err := c.Bucket("ops").Object("v.txt").NewReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 8)
	n, _ := r.Read(buf)
	_ = r.Close()
	if string(buf[:n]) != "one" {
		t.Errorf("after Restore the live version reads %q", buf[:n])
	}
	if soft := sectionNamed(od, "versionsdeleted"); soft == nil || soft.ShownIn != "versions" || soft.ShowAs != "Soft-deleted objects only" {
		t.Errorf("the soft-deleted versions are %+v; want them in Version history's Show list", soft)
	}

	// Google's words.
	br := bulkRestoreAction()
	if br.Submit != "Create" || br.Fields[0].Label != "Filter by glob pattern" ||
		br.Fields[0].Section != "Choose which soft-deleted objects to restore" || br.Fields[3].Section != "Restore options" {
		t.Errorf("the restore job pane is %+v", br)
	}
	if label, _ := (storageSettingsProvider{}).CreateForm(); label != "Create a key for a service account" ||
		(storageSettingsProvider{}).CreateSubmit() != "Create key" {
		t.Errorf("HMAC create is %q", label)
	}
	if s := bucketViewScope(bucketViewLive); !s.ViewOptions || s.Options[1].Label != "Soft deleted buckets only" {
		t.Errorf("the Buckets list's View options is %+v", s)
	}
}
