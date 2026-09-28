//go:build compat

package compat

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
)

// TestConsoleStorageBucketSettings (#789): the bucket settings the console
// offers are performed through its own routes, as its forms and buttons
// submit them, and read back through the official storage client.
//
//   - Edit bucket (buckets.patch): labels, the default storage class,
//     versioning, a retention period, a lifecycle rule and a CORS
//     configuration are what BucketHandle.Attrs reads; a label removed in the
//     form is gone; a soft delete retention the API refuses is refused with
//     its message.
//   - Lock retention policy (buckets.lockRetentionPolicy): Attrs reads the
//     policy IsLocked; the page no longer offers the lock, and the route
//     refuses a second one; removing the locked policy is refused with the
//     API's message.
//   - A soft-deleted object is listed on its bucket's page and its Restore
//     (objects.restore) makes it readable again through the client.
//   - A deleted bucket is listed on the Deleted buckets page and its Restore
//     (buckets.restore) makes it readable again through the client.
//
// Managed folders are listed from managedFolders.list and none can be made on
// this instance (managedFolders.insert answers 501), so the browser shows
// none and offers no control to make one.
func TestConsoleStorageBucketSettings(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	sc := storageClient(t, h)
	ctx := h.Context()
	project := h.Project()
	bh := bucket(t, h, sc)
	b := bh.BucketName()

	type field struct {
		Name, Type, Default string
		Immutable           bool
	}
	type action struct{ ID, Confirm string }
	type row struct {
		Name    string
		Fields  map[string]string
		Target  []string
		Actions []action
	}
	type page struct {
		Unavailable string
		Actions     []action
		Edit        *struct{ Fields []field }
		Sections    []struct {
			ID      string
			Listing struct {
				Items []row
				Note  string
			}
		}
	}
	detail := func(path ...string) page {
		t.Helper()
		v := url.Values{"project": {project}}
		for _, s := range path {
			v.Add("name", s)
		}
		code, body := consoleDo(t, addr, http.MethodGet, "/api/detail/storage?"+v.Encode(), "")
		var d page
		if err := json.Unmarshal([]byte(body), &d); code != http.StatusOK || err != nil || d.Unavailable != "" {
			t.Fatalf("console detail %v = %d (%v): %s", path, code, err, body)
		}
		return d
	}
	// form is Edit bucket's defaults, what the browser submits untouched.
	form := func(d page) map[string]string {
		t.Helper()
		if d.Edit == nil {
			t.Fatal("the bucket page offers no Edit bucket")
		}
		out := map[string]string{}
		for _, f := range d.Edit.Fields {
			if f.Immutable {
				continue
			}
			out[f.Name] = f.Default
		}
		return out
	}
	edit := func(values map[string]string) (int, string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"Path": []string{b}, "Values": values})
		return consoleDo(t, addr, http.MethodPatch, "/api/resources/storage?project="+project, string(body))
	}
	act := func(service string, path []string, id string) (int, string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"Path": path, "Action": id})
		return consoleDo(t, addr, http.MethodPost, "/api/actions/"+service+"?project="+project, string(body))
	}
	offers := func(actions []action, id string) bool {
		for _, a := range actions {
			if a.ID == id {
				return true
			}
		}
		return false
	}

	// Edit bucket.
	values := form(detail(b))
	if values["storageClass"] != "STANDARD" || values["softDeleteSeconds"] != "604800" {
		t.Errorf("Edit bucket is prefilled with %v", values)
	}
	values["labels"] = `{"team":"blue","env":"dev"}`
	values["storageClass"] = "NEARLINE"
	values["versioning"] = "true"
	values["retentionPeriod"] = "60"
	values["lifecycle"] = `[{"action":{"type":"Delete"},"condition":{"age":30}}]`
	values["cors"] = `[{"origin":["http://localhost:8080"],"method":["GET"],"maxAgeSeconds":3600}]`
	if code, body := edit(values); code != http.StatusOK {
		t.Fatalf("console Edit bucket = %d: %s", code, body)
	}
	a, err := bh.Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if a.Labels["team"] != "blue" || a.Labels["env"] != "dev" || a.StorageClass != "NEARLINE" || !a.VersioningEnabled {
		t.Errorf("after the console's edit Attrs reads labels %v, class %s, versioning %v", a.Labels, a.StorageClass, a.VersioningEnabled)
	}
	if len(a.Lifecycle.Rules) != 1 || a.Lifecycle.Rules[0].Action.Type != storage.DeleteAction ||
		a.Lifecycle.Rules[0].Condition.AgeInDays != 30 {
		t.Errorf("after the console's edit the lifecycle rules are %+v", a.Lifecycle.Rules)
	}
	if a.RetentionPolicy == nil || a.RetentionPolicy.RetentionPeriod != time.Minute || a.RetentionPolicy.IsLocked {
		t.Errorf("after the console's edit the retention policy is %+v", a.RetentionPolicy)
	}
	if len(a.CORS) != 1 || len(a.CORS[0].Origins) != 1 || a.CORS[0].Origins[0] != "http://localhost:8080" ||
		a.CORS[0].MaxAge != time.Hour {
		t.Errorf("after the console's edit the CORS configuration is %+v", a.CORS)
	}

	values = form(detail(b))
	values["labels"] = `{"team":"blue"}`
	if code, body := edit(values); code != http.StatusOK {
		t.Fatalf("console Edit bucket = %d: %s", code, body)
	}
	if a, _ = bh.Attrs(ctx); len(a.Labels) != 1 || a.Labels["team"] != "blue" {
		t.Errorf("a label removed in the form left %v", a.Labels)
	}
	bad := form(detail(b))
	bad["softDeleteSeconds"] = "5"
	if code, body := edit(bad); code != http.StatusBadRequest ||
		!strings.Contains(body, "between 604800 (7 days) and 7776000 (90 days)") {
		t.Errorf("a soft delete retention of 5 s = %d %s; want the API's refusal", code, body)
	}

	// Lock retention policy.
	d := detail(b)
	if !offers(d.Actions, "lockretention") {
		t.Fatalf("a bucket with an unlocked policy offers %+v", d.Actions)
	}
	for _, x := range d.Actions {
		if x.ID == "lockretention" && !strings.Contains(x.Confirm, "never be removed") {
			t.Errorf("Lock retention policy's confirmation says %q", x.Confirm)
		}
	}
	if code, body := act("storage", []string{b}, "lockretention"); code != http.StatusOK {
		t.Fatalf("console Lock retention policy = %d: %s", code, body)
	}
	if a, _ = bh.Attrs(ctx); a.RetentionPolicy == nil || !a.RetentionPolicy.IsLocked ||
		a.RetentionPolicy.RetentionPeriod != time.Minute {
		t.Errorf("after the console's lock the policy is %+v", a.RetentionPolicy)
	}
	if d := detail(b); offers(d.Actions, "lockretention") {
		t.Error("a locked policy is still offered for locking")
	}
	if code, body := act("storage", []string{b}, "lockretention"); code != http.StatusBadRequest ||
		!strings.Contains(body, "not available") {
		t.Errorf("a second lock = %d %s; want it refused as not offered", code, body)
	}
	values = form(detail(b))
	values["retentionPeriod"] = ""
	if code, body := edit(values); code != http.StatusBadRequest || !strings.Contains(body, "cannot be removed") {
		t.Errorf("removing the locked policy = %d %s; want the API's refusal", code, body)
	}

	// A soft-deleted object, restored from its bucket's page.
	soft := sc.Bucket(project + "-soft")
	if err := soft.Create(ctx, project, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { emptyAndDelete(h.Context(), soft) })
	putObject(t, ctx, soft.Object("logs/a.txt"), "alpha")
	if err := soft.Object("logs/a.txt").Delete(ctx); err != nil {
		t.Fatal(err)
	}
	var deleted *row
	for _, s := range detail(soft.BucketName()).Sections {
		if s.ID == "deleted" && len(s.Listing.Items) == 1 {
			deleted = &s.Listing.Items[0]
		}
	}
	if deleted == nil || deleted.Name != "logs/a.txt" || !offers(deleted.Actions, "restore") || len(deleted.Target) == 0 {
		t.Fatalf("the bucket page's deleted objects hold %+v", deleted)
	}
	if code, body := act("storage", deleted.Target, "restore"); code != http.StatusOK {
		t.Fatalf("console Restore object = %d: %s", code, body)
	}
	r, err := soft.Object("logs/a.txt").NewReader(ctx)
	if err != nil {
		t.Fatalf("the restored object cannot be read: %v", err)
	}
	data, _ := io.ReadAll(r)
	_ = r.Close()
	if string(data) != "alpha" {
		t.Errorf("the restored object reads %q", data)
	}

	// A deleted bucket, restored from the Deleted buckets page.
	gone := sc.Bucket(project + "-gone")
	if err := gone.Create(ctx, project, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { emptyAndDelete(h.Context(), gone) })
	if err := gone.Delete(ctx); err != nil {
		t.Fatal(err)
	}
	code, body := consoleDo(t, addr, http.MethodGet, "/api/resources/storage-deleted?project="+project, "")
	var listing struct{ Items []row }
	if err := json.Unmarshal([]byte(body), &listing); code != http.StatusOK || err != nil {
		t.Fatalf("Deleted buckets = %d (%v): %s", code, err, body)
	}
	var brow *row
	for i := range listing.Items {
		if listing.Items[i].Name == gone.BucketName() {
			brow = &listing.Items[i]
		}
	}
	if brow == nil || brow.Fields["Generation"] == "" || brow.Fields["Hard delete"] == "" || !offers(brow.Actions, "restore") {
		t.Fatalf("Deleted buckets lists %+v", listing.Items)
	}
	if code, body := act("storage-deleted", brow.Target, "restore"); code != http.StatusOK {
		t.Fatalf("console Restore bucket = %d: %s", code, body)
	}
	if _, err := gone.Attrs(ctx); err != nil {
		t.Errorf("the restored bucket cannot be read: %v", err)
	}

	// Managed folders: listed, and none exist to list.
	for _, s := range detail(b).Sections {
		if s.ID == "objects" && s.Listing.Note != "" {
			t.Errorf("the objects listing carries %q", s.Listing.Note)
		}
	}
}
