package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"cloud.google.com/go/storage"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	gcsbuiltin "github.com/cloudburrow/cloudburrow/internal/service/storage"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestStorageBucketRestorePane (#2047): a bucket soft-deleted twice is
// listed once per generation, and each row's Restore is Google's pane: the
// Specify bucket generation tab offering both generations, the row's
// chosen; Specify objects to restore with Don't restore objects first; and
// Additional options. Restoring the older generation with objects that
// match criteria (Filter by glob pattern) brings that generation back with
// only the matching soft-deleted object live, as the official client reads.
func TestStorageBucketRestorePane(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, c := newObjectOpsProvider(t)
	gone := func(objects ...string) {
		t.Helper()
		if _, err := p.Create(ctx, "p", map[string]string{"name": "twice"}); err != nil {
			t.Fatal(err)
		}
		for _, o := range objects {
			putObject(t, c, "twice", o, "data-"+o, nil)
			if err := c.Bucket("twice").Object(o).Delete(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if err := p.Delete(ctx, "p", "twice"); err != nil {
			t.Fatal(err)
		}
	}
	gone("logs/a.txt", "data/b.bin")
	gone()

	dp := storageDeletedProvider{endpoint: p.endpoint}
	l, err := dp.List(ctx, "p")
	if err != nil || len(l.Items) != 2 {
		t.Fatalf("Deleted buckets lists %+v (%v)", l.Items, err)
	}
	older := l.Items[0]
	if older.Target[0] > l.Items[1].Target[0] {
		older = l.Items[1]
	}
	if len(older.Actions) != 1 {
		t.Fatalf("the row offers %+v", older.Actions)
	}
	pane := older.Actions[0]
	fields := map[string]console.Field{}
	for _, f := range pane.Fields {
		fields[f.Name] = f
	}
	gen := fields["generation"]
	if pane.ID != "restore" || pane.Submit != "Restore" || gen.Tab != "Specify bucket generation" || len(gen.Options) != 2 ||
		gen.Default != older.Target[0] || fields["objects"].Tab != "Specify objects to restore" ||
		fields["objects"].Default != restoreObjectsNone || fields["objects"].OptionLabels[restoreObjectsNone] != "Don't restore objects" ||
		fields["objects"].OptionLabels[restoreObjectsCriteria] != "Restore objects that match criteria" ||
		fields["filterByGlob"].Label != "Filter by glob pattern" || fields["allowOverwrite"].Section != "Additional options" {
		t.Fatalf("Restore is %+v", pane)
	}
	if got := dp.DetailActions(ctx, "p", older.Target); len(got) != 1 || len(got[0].Fields) != len(pane.Fields) || len(got[0].Fields[0].Options) != 2 {
		t.Fatalf("the action route would refuse the pane the row offers: %+v", got)
	}

	// A glob filter with no glob is refused before anything is restored.
	if err := dp.ActAt(ctx, "p", older.Target, "restore", map[string]string{"objects": restoreObjectsCriteria, "filterByGlob": "true"}); err == nil {
		t.Fatal("a glob filter with no pattern was sent")
	}
	if _, err := c.Bucket("twice").Attrs(ctx); err == nil {
		t.Fatal("a refused restore restored the bucket")
	}
	if err := dp.ActAt(ctx, "p", older.Target, "restore", map[string]string{
		"generation": older.Target[0], "objects": restoreObjectsCriteria, "filterByGlob": "true", "matchGlobs": "logs/**",
	}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	// The generation through Google's generated JSON API client, which
	// reads it where the official client does not.
	api, err := jsonAPI(ctx, p.endpoint)
	if err != nil {
		t.Fatal(err)
	}
	b, err := api.Buckets.Get("twice").Context(ctx).Do()
	if err != nil || strconv.FormatInt(b.Generation, 10) != older.Target[0] {
		t.Fatalf("the restored bucket reads %+v (%v); want generation %s", b, err, older.Target[0])
	}
	if got := readObject(t, c, "twice", "logs/a.txt"); got != "data-logs/a.txt" {
		t.Errorf("the matching object reads %q", got)
	}
	if _, err := c.Bucket("twice").Object("data/b.bin").Attrs(ctx); !errors.Is(err, storage.ErrObjectNotExist) {
		t.Errorf("an object the glob does not match was restored: %v", err)
	}
	if err := dp.ActAt(ctx, "p", older.Target, "restore", map[string]string{"objects": "some"}); err == nil {
		t.Error("an unknown choice of objects was accepted")
	}
}

// TestStorageVersionHistoryCheckboxDelete (#2047): Version history's rows
// are checked for its Delete, confirmed by the word delete, and no longer
// carry Delete version in their menus. Delete takes the checked
// generations, as the table names them, and the official client then
// reads only the one left; a name that is not a version of the object is
// refused before anything is deleted.
func TestStorageVersionHistoryCheckboxDelete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, c := newObjectOpsProvider(t)
	versionedBucket(t, c, "hist")
	var gens []int64
	for _, body := range []string{"one", "two", "three"} {
		w := c.Bucket("hist").Object("v.txt").NewWriter(ctx)
		_, _ = w.Write([]byte(body))
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		gens = append(gens, w.Attrs().Generation)
	}
	path := objectPath("hist", "v.txt")
	od, err := p.Detail(ctx, "p", path)
	if err != nil {
		t.Fatal(err)
	}
	hist := sectionNamed(od, "versions")
	if hist == nil || len(hist.Listing.Items) != 3 || len(hist.Listing.SelectActions) != 1 {
		t.Fatalf("Version history is %+v", hist)
	}
	del := hist.Listing.SelectActions[0]
	if del.ID != deleteVersionsID || del.Label != "Delete" || del.ConfirmStyle != console.DeleteConfirmWord ||
		!del.Destructive || del.SelectionField != versionsField {
		t.Errorf("Version history's Delete is %+v", del)
	}
	for _, row := range hist.Listing.Items {
		if !row.Selectable || hasAction(row.Actions, "deleteversion") {
			t.Errorf("a Version history row is %+v; want it checkable, with no Delete version", row)
		}
	}
	if !hasAction(p.DetailActions(ctx, "p", path), deleteVersionsID) {
		t.Fatal("the action route would refuse the Delete the table offers")
	}
	if err := p.ActAt(ctx, "p", path, deleteVersionsID, map[string]string{versionsField: "other.txt#" + strconv.FormatInt(gens[0], 10)}); err == nil {
		t.Error("a version of another object was accepted")
	}
	checked := "v.txt#" + strconv.FormatInt(gens[0], 10) + "\nv.txt#" + strconv.FormatInt(gens[2], 10)
	if err := p.ActAt(ctx, "p", path, deleteVersionsID, map[string]string{versionsField: checked}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	var left []int64
	it := c.Bucket("hist").Objects(ctx, &storage.Query{Versions: true, Prefix: "v.txt"})
	for {
		o, err := it.Next()
		if err != nil {
			break
		}
		left = append(left, o.Generation)
	}
	if len(left) != 1 || left[0] != gens[1] {
		t.Errorf("after Delete the client lists generations %v; want only %d", left, gens[1])
	}
}

// TestStorageIntelligenceConfigureDialog (#2047): Enable Storage
// Intelligence is Google's Configure Storage Intelligence dialog, its
// resource first, Continue, bucket filters by location and by name with Add
// regular expression, ending in Enable this configuration. Enabled for the
// project with an included location and an excluded name regex, the
// configuration read back is STANDARD with that filter; the Overview's
// Status offers Disable, confirmed by typing Disable, which disables it.
func TestStorageIntelligenceConfigureDialog(t *testing.T) {
	ctx := context.Background()
	gcs, err := gcsbuiltin.NewServer(gcsbuiltin.Options{})
	if err != nil {
		t.Fatal(err)
	}
	backend := httptest.NewServer(gcs)
	t.Cleanup(backend.Close)
	p := storageIntelligenceProvider{endpoint: strings.TrimPrefix(backend.URL, "http://"), registry: resourcemanager.New(store.NewMemory())}
	const project = "intel-dialog"
	acts := p.ListActions(ctx, project)
	if len(acts) != 1 {
		t.Fatalf("an inheriting project's page offers %+v; want Enable Storage Intelligence only", acts)
	}
	dlg := acts[0]
	if dlg.Label != "Enable Storage Intelligence" || dlg.Title != "Configure Storage Intelligence" || dlg.Steps != "continue" ||
		dlg.Submit != "Enable this configuration" || dlg.Fields[0].Name != "resource" || dlg.Fields[0].Section != "Resource" {
		t.Fatalf("the dialog is %+v", dlg)
	}
	var addRegex bool
	for _, f := range dlg.Fields {
		addRegex = addRegex || (f.AddLabel == "Add regular expression" && f.Type == console.GroupFieldType)
	}
	if !addRegex {
		t.Error("the dialog has no Add regular expression")
	}
	if err := p.ActAt(ctx, project, nil, intelligenceConfigureID, map[string]string{"resource": "projects/someone-else"}); err == nil {
		t.Error("a resource not on the page was configured")
	}
	if err := p.ActAt(ctx, project, nil, intelligenceConfigureID, map[string]string{
		"resource": "projects/" + project, "bucketFilter": filterExclude}); err == nil {
		t.Error("a name filter with no regular expression was sent")
	}
	if err := p.ActAt(ctx, project, nil, intelligenceConfigureID, map[string]string{
		"resource": "projects/" + project, "locationFilter": filterInclude,
		"locations":    console.FormatGroup([]map[string]string{{"location": "us-central1"}}),
		"bucketFilter": filterExclude,
		"regexes":      console.FormatGroup([]map[string]string{{"regex": "^tmp-"}, {"regex": "-scratch$"}}),
	}); err != nil {
		t.Fatalf("Enable this configuration: %v", err)
	}
	var c intelligenceJSON
	if err := storageCall(ctx, "GET", p.url("projects/"+project), nil, &c); err != nil {
		t.Fatal(err)
	}
	if c.EditionConfig != "STANDARD" || c.Filter == nil || c.Filter.IncludedCloudStorageLocations == nil ||
		strings.Join(c.Filter.IncludedCloudStorageLocations.Locations, ",") != "us-central1" ||
		c.Filter.ExcludedCloudStorageBuckets == nil || strings.Join(c.Filter.ExcludedCloudStorageBuckets.BucketIDRegexes, ",") != "^tmp-,-scratch$" {
		t.Fatalf("the configuration read back is %+v", c)
	}
	l, err := p.List(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	status := l.Summary[0].Properties[1]
	if status.Value != "Enabled" || status.Action != intelligenceDisableID || status.ActionText != "Disable" {
		t.Errorf("the Overview's Status is %+v; want Disable beside Enabled", status)
	}
	acts = p.ListActions(ctx, project)
	if len(acts) != 2 || acts[1].ID != intelligenceDisableID || !acts[1].Linked || acts[1].ConfirmStyle != console.DeleteConfirmDisable {
		t.Fatalf("an enabled project's page offers %+v", acts)
	}
	if err := p.ActAt(ctx, project, nil, intelligenceDisableID, nil); err != nil {
		t.Fatal(err)
	}
	if err := storageCall(ctx, "GET", p.url("projects/"+project), nil, &c); err != nil || c.EditionConfig != "DISABLED" {
		t.Errorf("after Disable the edition is %q (%v)", c.EditionConfig, err)
	}
	for _, a := range intelligenceActions("STANDARD", false) {
		if a.ID == "disable" && a.ConfirmStyle != console.DeleteConfirmDisable {
			t.Errorf("a row's Disable is %+v; want it confirmed by Disable", a)
		}
	}
}
