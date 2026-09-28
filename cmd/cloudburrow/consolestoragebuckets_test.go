package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// bucketEditValues is what the browser submits for an edit form untouched: every
// field's default except the immutable ones, which it does not send.
func bucketEditValues(f *console.EditForm) map[string]string {
	out := map[string]string{}
	for _, field := range f.Fields {
		if field.Immutable {
			continue
		}
		out[field.Name] = field.Default
		if field.Type == "checkbox" && field.Default == "" {
			out[field.Name] = "false"
		}
	}
	return out
}

func hasAction(actions []console.Action, id string) bool {
	for _, a := range actions {
		if a.ID == id {
			return true
		}
	}
	return false
}

// Edit bucket is prefilled from the bucket, and what it saves through
// buckets.patch is what the official client's Attrs reads; a label removed
// in the form is removed; a value the API refuses is refused with its own
// message. Lock retention policy is offered only while a policy is there and
// unlocked, locks it, and a locked policy cannot then be removed (#789).
func TestStorageBucketSettingsThroughTheAPI(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, c := newObjectOpsProvider(t)

	d, err := p.Detail(ctx, "p", []string{"ops"})
	if err != nil || d.Edit == nil {
		t.Fatalf("the bucket page offers no edit (%v): %+v", err, d)
	}
	if hasAction(d.Actions, "lockretention") || hasAction(p.DetailActions(ctx, "p", []string{"ops"}), "lockretention") {
		t.Error("Lock retention policy is offered on a bucket with no retention policy")
	}
	values := bucketEditValues(d.Edit)
	if values["storageClass"] != "STANDARD" || values["softDeleteSeconds"] != "604800" ||
		values["versioning"] != "false" || values["retentionPeriod"] != "" || values["labels"] != "" {
		t.Errorf("Edit bucket is prefilled with %v", values)
	}
	for _, f := range d.Edit.Fields {
		if f.Name == "location" && (!f.Immutable || f.Default != "US") {
			t.Errorf("the location field is %+v; want US, immutable", f)
		}
	}

	values["labels"] = `{"team":"blue","env":"dev"}`
	values["storageClass"] = "NEARLINE"
	values["versioning"] = "true"
	values["retentionPeriod"] = "60"
	values["lifecycle"] = `[{"action":{"type":"Delete"},"condition":{"age":30}}]`
	values["cors"] = `[{"origin":["http://localhost:8080"],"method":["GET"],"maxAgeSeconds":3600}]`
	if err := p.Edit(ctx, "p", []string{"ops"}, values); err != nil {
		t.Fatalf("Edit bucket: %v", err)
	}
	a, err := c.Bucket("ops").Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if a.Labels["team"] != "blue" || a.Labels["env"] != "dev" || a.StorageClass != "NEARLINE" || !a.VersioningEnabled ||
		a.RetentionPolicy == nil || a.RetentionPolicy.RetentionPeriod != time.Minute ||
		len(a.Lifecycle.Rules) != 1 || a.Lifecycle.Rules[0].Action.Type != storage.DeleteAction ||
		a.Lifecycle.Rules[0].Condition.AgeInDays != 30 ||
		len(a.CORS) != 1 || a.CORS[0].Origins[0] != "http://localhost:8080" || a.CORS[0].MaxAge != time.Hour {
		t.Errorf("after the edit Attrs reads %+v", a)
	}

	// Prefilled from what was saved; a label taken out is removed.
	d, _ = p.Detail(ctx, "p", []string{"ops"})
	values = bucketEditValues(d.Edit)
	if !strings.Contains(values["lifecycle"], `"age": 30`) || values["retentionPeriod"] != "60" {
		t.Errorf("Edit bucket is prefilled with %v after the save", values)
	}
	values["labels"] = `{"team":"blue"}`
	values["cors"] = ""
	if err := p.Edit(ctx, "p", []string{"ops"}, values); err != nil {
		t.Fatalf("Edit bucket: %v", err)
	}
	if a, _ = c.Bucket("ops").Attrs(ctx); len(a.Labels) != 1 || a.Labels["team"] != "blue" || len(a.CORS) != 0 ||
		len(a.Lifecycle.Rules) != 1 {
		t.Errorf("removing a label and the CORS left labels %v, CORS %v, rules %v", a.Labels, a.CORS, a.Lifecycle.Rules)
	}

	// Refusals are the API's.
	for _, tc := range []struct{ field, value, want string }{
		{"softDeleteSeconds", "5", "between 604800 (7 days) and 7776000 (90 days)"},
		{"lifecycle", `[{"action":{"type":"Delete"},"condition":{"ageInDays":30}}]`, "ageInDays"},
		{"lifecycle", `{"rule":[]}`, "not a JSON array"},
		{"storageClass", "GLACIER", "storageClass"},
	} {
		bad := bucketEditValues(d.Edit)
		bad[tc.field] = tc.value
		if err := p.Edit(ctx, "p", []string{"ops"}, bad); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s=%s was refused with %v; want it to name %q", tc.field, tc.value, err, tc.want)
		}
	}

	// Lock retention policy.
	d, _ = p.Detail(ctx, "p", []string{"ops"})
	if !hasAction(d.Actions, "lockretention") {
		t.Fatalf("a bucket with an unlocked policy offers %v", d.Actions)
	}
	for _, act := range d.Actions {
		if act.ID == "lockretention" && (!act.Destructive || !strings.Contains(act.Confirm, "never be removed")) {
			t.Errorf("Lock retention policy is %+v; want it destructive and saying it is permanent", act)
		}
	}
	if !hasAction(p.DetailActions(ctx, "p", []string{"ops"}), "lockretention") {
		t.Error("the action route would refuse the lock the page offers")
	}
	if err := p.ActAt(ctx, "p", []string{"ops"}, "lockretention", nil); err != nil {
		t.Fatalf("Lock retention policy: %v", err)
	}
	if a, _ = c.Bucket("ops").Attrs(ctx); a.RetentionPolicy == nil || !a.RetentionPolicy.IsLocked {
		t.Errorf("after the lock Attrs reads the policy %+v", a.RetentionPolicy)
	}
	if hasAction(p.DetailActions(ctx, "p", []string{"ops"}), "lockretention") {
		t.Error("a locked policy is still offered for locking")
	}
	d, _ = p.Detail(ctx, "p", []string{"ops"})
	values = bucketEditValues(d.Edit)
	values["retentionPeriod"] = ""
	if err := p.Edit(ctx, "p", []string{"ops"}, values); err == nil || !strings.Contains(err.Error(), "cannot be removed") {
		t.Errorf("removing a locked policy was answered %v; want the API's refusal", err)
	}
}

// A soft-deleted object is listed on its bucket's page with its generation
// and Restore, and restoring it makes it readable through the client; a
// soft-deleted bucket is listed on the Deleted buckets page and restored
// there (#789).
func TestStorageSoftDeletedRestoreThroughTheAPI(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, c := newObjectOpsProvider(t)
	putObject(t, c, "other", "logs/a.txt", "alpha", nil)
	if err := c.Bucket("other").Object("logs/a.txt").Delete(ctx); err != nil {
		t.Fatal(err)
	}

	d, err := p.Detail(ctx, "p", []string{"other"})
	if err != nil {
		t.Fatal(err)
	}
	var deleted *console.Listing
	for i := range d.Sections {
		if d.Sections[i].ID == "deleted" {
			deleted = &d.Sections[i].Listing
		}
	}
	if deleted == nil || len(deleted.Items) != 1 || deleted.Items[0].Name != "logs/a.txt" {
		t.Fatalf("the bucket's Deleted objects are %+v", deleted)
	}
	row := deleted.Items[0]
	if len(row.Target) != 4 || row.Target[0] != softObjectPage || row.Fields["Generation"] != row.Target[2] ||
		!hasAction(row.Actions, "restore") {
		t.Errorf("the deleted object's row is %+v", row)
	}
	if !hasAction(p.DetailActions(ctx, "p", row.Target), "restore") {
		t.Fatal("the action route would refuse the Restore the row offers")
	}
	if err := p.ActAt(ctx, "p", row.Target, "restore", nil); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := readObject(t, c, "other", "logs/a.txt"); got != "alpha" {
		t.Errorf("the restored object reads %q", got)
	}
	if hasAction(p.DetailActions(ctx, "p", row.Target), "restore") {
		t.Error("a restored object is still offered for restoring")
	}
	if err := p.ActAt(ctx, "p", row.Target, "restore", nil); err == nil {
		t.Error("restoring the same generation twice succeeded")
	}

	// Deleted buckets.
	if _, err := p.Create(ctx, "p", map[string]string{"name": "gone"}); err != nil {
		t.Fatal(err)
	}
	if err := p.Delete(ctx, "p", "gone"); err != nil {
		t.Fatal(err)
	}
	dp := storageDeletedProvider{endpoint: p.endpoint}
	l, err := dp.List(ctx, "p")
	if err != nil || len(l.Items) != 1 || l.Items[0].Name != "gone" {
		t.Fatalf("Deleted buckets lists %+v (%v)", l.Items, err)
	}
	brow := l.Items[0]
	if len(brow.Target) != 2 || brow.Target[1] != "gone" || brow.Target[0] != brow.Fields["Generation"] ||
		brow.Fields["Hard delete"] == "" || !hasAction(brow.Actions, "restore") {
		t.Errorf("the deleted bucket's row is %+v", brow)
	}
	if !hasAction(dp.DetailActions(ctx, "p", brow.Target), "restore") {
		t.Fatal("the action route would refuse the Restore the row offers")
	}
	if err := dp.ActAt(ctx, "p", brow.Target, "restore", nil); err != nil {
		t.Fatalf("Restore bucket: %v", err)
	}
	if _, err := c.Bucket("gone").Attrs(ctx); err != nil {
		t.Errorf("the restored bucket does not read: %v", err)
	}
	if l, _ := dp.List(ctx, "p"); len(l.Items) != 0 {
		t.Errorf("Deleted buckets still lists %+v after the restore", l.Items)
	}
	if l, _ := dp.List(ctx, ""); l.Prompt == "" {
		t.Error("Deleted buckets with no project asks for none")
	}
	if dp.DetailActions(ctx, "p", []string{"x", "gone"}) != nil {
		t.Error("a malformed generation is offered Restore")
	}
}

// Managed folders from managedFolders.list are folders marked managed in
// the browser, whether or not an object is under them, driven here by a
// server that answers as the API documents (#789); the builtin server's are
// TestStorageBrowserCreatesAndDeletesManagedFolders (#828).
func TestStorageBrowserMarksManagedFolders(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/storage/v1/b/b/o":
			_, _ = w.Write([]byte(`{"prefixes":["logs/app/","logs/reports/"]}`))
		case "/storage/v1/b/b/managedFolders":
			if r.URL.Query().Get("prefix") != "logs/" {
				http.Error(w, `{"error":{"message":"wrong prefix"}}`, http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"kind":"storage#managedFolders","items":[` +
				`{"name":"logs/reports/"},{"name":"logs/empty/"},{"name":"logs/reports/deeper/"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p := storageProvider{endpoint: strings.TrimPrefix(srv.URL, "http://")}
	l, err := p.objects(context.Background(), "b", "logs/", []string{"b", "logs"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, it := range l.Items {
		got[it.Name] = it.Fields["Type"]
	}
	want := map[string]string{"app/": "Folder", "reports/": "Managed folder", "empty/": "Managed folder"}
	if len(got) != len(want) {
		t.Errorf("the listing is %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s is %q, want %q (listing %v)", k, got[k], v, got)
		}
	}
	if l.Note != "" {
		t.Errorf("a listing whose managed folders were read carries the note %q", l.Note)
	}
}

// Against the builtin server managedFolders.list answers, empty, and the
// browser says nothing about it. A folder page offers no settings, only
// Create managed folder (#828).
func TestStorageBrowserListsManagedFoldersFromTheServer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, c := newObjectOpsProvider(t)
	putObject(t, c, "ops", "logs/a.txt", "a", nil)
	d, err := p.Detail(ctx, "p", []string{"ops", "logs"})
	if err != nil || d.Unavailable != "" {
		t.Fatalf("folder page: %v %+v", err, d)
	}
	if n := d.Sections[0].Listing.Note; n != "" {
		t.Errorf("the folder listing carries %q", n)
	}
	if d.Edit != nil || len(d.Actions) != 1 || d.Actions[0].ID != "createmanagedfolder" {
		t.Errorf("a folder offers %+v and %v; a folder has no settings", d.Edit, d.Actions)
	}
	if err := p.Edit(ctx, "p", []string{"ops", "logs"}, nil); err == nil {
		t.Error("a folder was accepted for editing")
	}
}

// Create managed folder on a folder's page makes one under its prefix, which
// the listing then types Managed folder with Delete managed folder on its
// row; the delete removes it and keeps the objects under it (#828). A bucket
// without uniform bucket-level access is refused with the API's message, and
// Edit bucket's checkbox turns the setting on.
func TestStorageBrowserCreatesAndDeletesManagedFolders(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, c := newObjectOpsProvider(t)
	putObject(t, c, "ops", "logs/a.txt", "a", nil)

	err := p.ActAt(ctx, "p", []string{"ops"}, "createmanagedfolder", map[string]string{"name": "logs/reports"})
	if err == nil || !strings.Contains(err.Error(), "uniform bucket-level access") {
		t.Fatalf("a managed folder in a bucket without uniform access = %v; want the API's refusal", err)
	}
	d, err := p.Detail(ctx, "p", []string{"ops"})
	if err != nil || d.Edit == nil {
		t.Fatalf("bucket page: %v %+v", err, d)
	}
	values := map[string]string{}
	for _, f := range d.Edit.Fields {
		if !f.Immutable {
			values[f.Name] = f.Default
		}
	}
	if values["uniformAccess"] != "false" {
		t.Fatalf("Edit bucket's uniform access is prefilled %q", values["uniformAccess"])
	}
	values["uniformAccess"] = "true"
	if err := p.Edit(ctx, "p", []string{"ops"}, values); err != nil {
		t.Fatal(err)
	}
	if a, err := c.Bucket("ops").Attrs(ctx); err != nil || !a.UniformBucketLevelAccess.Enabled {
		t.Fatalf("after Edit bucket uniform access is %+v, %v", a.UniformBucketLevelAccess, err)
	}

	offered := func(actions []console.Action, id string) bool {
		for _, a := range actions {
			if a.ID == id {
				return true
			}
		}
		return false
	}
	if !offered(p.DetailActions(ctx, "p", []string{"ops", "logs"}), "createmanagedfolder") {
		t.Fatal("a folder page does not offer Create managed folder")
	}
	if err := p.ActAt(ctx, "p", []string{"ops", "logs"}, "createmanagedfolder", map[string]string{"name": "reports/"}); err != nil {
		t.Fatalf("Create managed folder: %v", err)
	}
	s, err := jsonAPI(ctx, p.endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if f, err := s.ManagedFolders.Get("ops", "logs/reports/").Context(ctx).Do(); err != nil || f.Name != "logs/reports/" {
		t.Fatalf("managedFolders.get after the console's create = %+v, %v", f, err)
	}
	putObject(t, c, "ops", "logs/reports/r.txt", "r", nil)

	d, err = p.Detail(ctx, "p", []string{"ops", "logs"})
	if err != nil {
		t.Fatal(err)
	}
	var row *console.Resource
	for i, it := range d.Sections[0].Listing.Items {
		if it.Name == "reports/" {
			row = &d.Sections[0].Listing.Items[i]
		}
	}
	if row == nil || row.Fields["Type"] != "Managed folder" || !offered(row.Actions, "deletemanagedfolder") ||
		strings.Join(row.Target, "|") != "_managedfolder|ops|logs/reports/" {
		t.Fatalf("the managed folder's row is %+v", row)
	}
	if !offered(p.DetailActions(ctx, "p", row.Target), "deletemanagedfolder") {
		t.Fatal("the managed folder's target is not offered Delete managed folder")
	}
	if err := p.ActAt(ctx, "p", row.Target, "deletemanagedfolder", nil); err != nil {
		t.Fatalf("Delete managed folder: %v", err)
	}
	if _, err := s.ManagedFolders.Get("ops", "logs/reports/").Context(ctx).Do(); err == nil {
		t.Error("the managed folder is still there after the console's delete")
	}
	if _, err := c.Bucket("ops").Object("logs/reports/r.txt").Attrs(ctx); err != nil {
		t.Errorf("the object under the deleted managed folder: %v", err)
	}
	if p.DetailActions(ctx, "p", row.Target) != nil {
		t.Error("a deleted managed folder is still offered Delete")
	}
}
