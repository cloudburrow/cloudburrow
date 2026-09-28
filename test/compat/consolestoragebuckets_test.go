//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	storagev1 "google.golang.org/api/storage/v1"
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
// Managed folders, made and deleted in the browser, are
// TestConsoleStorageManagedFolders (#828).
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

	// Managed folders: listed without a note, which says they were read.
	for _, s := range detail(b).Sections {
		if s.ID == "objects" && s.Listing.Note != "" {
			t.Errorf("the objects listing carries %q", s.Listing.Note)
		}
	}
}

// TestConsoleStorageManagedFolders (#828): a managed folder created through
// the JSON API client appears in the console's bucket browser, typed Managed
// folder, with Delete managed folder on its row; Create managed folder,
// submitted as the page's form submits it, makes one under the page's prefix
// that managedFolders.get reads; and Delete managed folder removes it through
// the API, leaving the object under it. In a bucket without uniform
// bucket-level access the create is refused with the API's message.
func TestConsoleStorageManagedFolders(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	sc := storageClient(t, h)
	s := storageJSON(t, h)
	ctx := h.Context()
	project := h.Project()
	bh := managedFolderBucket(t, h, sc, s)
	b := bh.BucketName()

	type action struct{ ID string }
	type row struct {
		Name    string
		Fields  map[string]string
		Target  []string
		Actions []action
	}
	type page struct {
		Unavailable string
		Actions     []action
		Sections    []struct {
			ID      string
			Listing struct{ Items []row }
		}
	}
	detail := func(path ...string) page {
		t.Helper()
		v := url.Values{"project": {project}}
		for _, p := range path {
			v.Add("name", p)
		}
		code, body := consoleDo(t, addr, http.MethodGet, "/api/detail/storage?"+v.Encode(), "")
		var d page
		if err := json.Unmarshal([]byte(body), &d); code != http.StatusOK || err != nil || d.Unavailable != "" {
			t.Fatalf("console detail %v = %d (%v): %s", path, code, err, body)
		}
		return d
	}
	rowNamed := func(d page, name string) *row {
		for _, sec := range d.Sections {
			if sec.ID != "objects" {
				continue
			}
			for i := range sec.Listing.Items {
				if sec.Listing.Items[i].Name == name {
					return &sec.Listing.Items[i]
				}
			}
		}
		return nil
	}
	offers := func(actions []action, id string) bool {
		for _, a := range actions {
			if a.ID == id {
				return true
			}
		}
		return false
	}
	act := func(path []string, id string, values map[string]string) (int, string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"Path": path, "Action": id, "Values": values})
		return consoleDo(t, addr, http.MethodPost, "/api/actions/storage?project="+project, string(body))
	}

	// Made through the API, shown in the browser.
	if _, err := s.ManagedFolders.Insert(b, &storagev1.ManagedFolder{Name: "api-made/"}).Context(ctx).Do(); err != nil {
		t.Fatalf("managedFolders.insert: %v", err)
	}
	r := rowNamed(detail(b), "api-made/")
	if r == nil || r.Fields["Type"] != "Managed folder" || !offers(r.Actions, "deletemanagedfolder") {
		t.Fatalf("the managed folder made through the API is shown as %+v", r)
	}

	// Create managed folder on a folder's page.
	putObject(t, ctx, bh.Object("logs/a.txt"), "a")
	if d := detail(b, "logs"); !offers(d.Actions, "createmanagedfolder") {
		t.Fatalf("a folder page offers %+v", d.Actions)
	}
	if code, body := act([]string{b, "logs"}, "createmanagedfolder", map[string]string{"name": "reports"}); code != http.StatusOK {
		t.Fatalf("console Create managed folder = %d: %s", code, body)
	}
	if f, err := s.ManagedFolders.Get(b, "logs/reports/").Context(ctx).Do(); err != nil || f.Name != "logs/reports/" {
		t.Fatalf("managedFolders.get after the console's create = %+v, %v", f, err)
	}
	putObject(t, ctx, bh.Object("logs/reports/r.txt"), "r")
	r = rowNamed(detail(b, "logs"), "reports/")
	if r == nil || r.Fields["Type"] != "Managed folder" || len(r.Target) == 0 {
		t.Fatalf("the console-made managed folder is shown as %+v", r)
	}

	// Delete managed folder from its row, at its target.
	if code, body := act(r.Target, "deletemanagedfolder", nil); code != http.StatusOK {
		t.Fatalf("console Delete managed folder = %d: %s", code, body)
	}
	if _, err := s.ManagedFolders.Get(b, "logs/reports/").Context(ctx).Do(); httpCode(err) != http.StatusNotFound {
		t.Errorf("managedFolders.get after the console's delete = %v; want 404", err)
	}
	if _, err := bh.Object("logs/reports/r.txt").Attrs(ctx); err != nil {
		t.Errorf("the object under the deleted managed folder: %v", err)
	}
	if r := rowNamed(detail(b, "logs"), "reports/"); r == nil || r.Fields["Type"] != "Folder" {
		t.Errorf("after the delete the folder is shown as %+v", r)
	}
	if code, body := act(r.Target, "deletemanagedfolder", nil); code != http.StatusBadRequest || !strings.Contains(body, "not available") {
		t.Errorf("a second delete = %d %s; want it refused as not offered", code, body)
	}

	// A bucket without uniform bucket-level access: the API's refusal.
	plain := bucket(t, h, sc)
	code, body := act([]string{plain.BucketName()}, "createmanagedfolder", map[string]string{"name": "x"})
	if code == http.StatusOK || !strings.Contains(body, "uniform bucket-level access") {
		t.Errorf("Create managed folder in a bucket without uniform access = %d %s; want the API's refusal", code, body)
	}
	_ = s.ManagedFolders.Delete(b, "api-made/").Context(context.Background()).Do()
}
