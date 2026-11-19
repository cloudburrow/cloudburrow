package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// pageAndTabActions are every action a detail page draws: beside its title
// and at the top of each tab (#1843).
func pageAndTabActions(d console.Detail) []console.Action {
	out := append([]console.Action{}, d.Actions...)
	for _, s := range d.Sections {
		out = append(out, s.Actions...)
	}
	return out
}

// tabActions are the actions drawn at the top of one tab.
func tabActions(d console.Detail, id string) []console.Action {
	for _, s := range d.Sections {
		if s.ID == id {
			return s.Actions
		}
	}
	return nil
}

// Create a bucket is Google's five steps, in its order, ending in Create, and
// its defaults make the bucket Google's defaults make: US multi-region,
// Standard, uniform access, public access prevention enforced, soft delete
// for 7 days (#1843). The list shows it with Google's columns, and its page
// has Google's tabs in Google's order.
func TestStorageConsoleMatchesGoogleBucketPages(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, c := newObjectOpsProvider(t)

	label, fields := p.CreateForm()
	if label != "Create" || p.CreateTitle() != "Create a bucket" || !p.CreateOnPage() {
		t.Errorf("the form is %q titled %q", label, p.CreateTitle())
	}
	var steps []string
	for _, f := range fields {
		if f.Section != "" && (len(steps) == 0 || steps[len(steps)-1] != f.Section) {
			steps = append(steps, f.Section)
		}
	}
	want := []string{"Choose where to store your data", "Choose how to store your data",
		"Choose how to control access to objects", "Choose how to protect object data"}
	if strings.Join(steps, "|") != strings.Join(want, "|") || fields[0].Section != "" {
		t.Errorf("the steps after Get started are %q; want %q", steps, want)
	}

	values := map[string]string{}
	for _, f := range fields {
		values[f.Name] = f.Default
	}
	values["name"] = "form-defaults"
	if _, err := p.Create(ctx, "p", values); err != nil {
		t.Fatalf("create with the form's defaults: %v", err)
	}
	a, err := c.Bucket("form-defaults").Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if a.Location != "US" || a.StorageClass != "STANDARD" || !a.UniformBucketLevelAccess.Enabled ||
		a.PublicAccessPrevention != storage.PublicAccessPreventionEnforced ||
		a.SoftDeletePolicy == nil || a.SoftDeletePolicy.RetentionDuration != 7*24*time.Hour {
		t.Errorf("the defaults made location %s, class %s, uniform %v, PAP %v, soft delete %+v",
			a.Location, a.StorageClass, a.UniformBucketLevelAccess.Enabled, a.PublicAccessPrevention, a.SoftDeletePolicy)
	}

	values["name"] = "custom-choices"
	values["locationType"], values["location"], values["turboReplication"] = "Dual-region", "nam4", "true"
	values["softDeleteDays"], values["uniformAccess"], values["publicAccessPrevention"] = "14", "false", "false"
	if _, err := p.Create(ctx, "p", values); err != nil {
		t.Fatalf("create with choices: %v", err)
	}
	a, err = c.Bucket("custom-choices").Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if a.Location != "NAM4" || a.RPO != storage.RPOAsyncTurbo || a.UniformBucketLevelAccess.Enabled ||
		a.SoftDeletePolicy.RetentionDuration != 14*24*time.Hour {
		t.Errorf("the choices made location %s, rpo %v, uniform %v, soft delete %+v",
			a.Location, a.RPO, a.UniformBucketLevelAccess.Enabled, a.SoftDeletePolicy)
	}
	values["name"], values["softDelete"] = "no-soft-delete", "false"
	if _, err := p.Create(ctx, "p", values); err != nil {
		t.Fatal(err)
	}
	if a, err := c.Bucket("no-soft-delete").Attrs(ctx); err != nil || (a.SoftDeletePolicy != nil && a.SoftDeletePolicy.RetentionDuration != 0) {
		t.Errorf("a cleared soft delete policy made %+v (%v)", a, err)
	}

	list, err := p.List(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(list.Columns, "|") != strings.Join(bucketListColumns, "|") {
		t.Errorf("the list's columns are %q", list.Columns)
	}
	var row *console.Resource
	for i := range list.Items {
		if list.Items[i].Name == "custom-choices" {
			row = &list.Items[i]
		}
	}
	if row == nil || row.Fields["Location type"] != "Dual-region" || row.Fields["Access control"] != "Fine-grained" ||
		row.Fields["Default storage class"] != "Standard" || row.Fields["Protection"] != "Soft Delete" ||
		row.Fields["Lifecycle"] != "None" || !hasAction(row.Actions, "bulkrestore") {
		t.Fatalf("the row is %+v", row)
	}

	d, err := p.Detail(ctx, "p", []string{"form-defaults"})
	if err != nil || d.Unavailable != "" {
		t.Fatalf("bucket page: %v %s", err, d.Unavailable)
	}
	var tabs []string
	for _, s := range d.Sections {
		tabs = append(tabs, s.ID)
	}
	if got := strings.Join(tabs, ","); !strings.HasPrefix(got, "objects,configuration,permissions,protection,lifecycle,") {
		t.Errorf("the tabs are %s", got)
	}
}

// Add a rule appends one lifecycle rule through buckets.patch, its row
// shows its action and conditions as Google words them, and Delete rule
// removes that rule alone (#1843).
func TestStorageLifecycleRulesThroughTheTab(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, c := newObjectOpsProvider(t)
	if err := c.Bucket("rules").Create(ctx, "p", nil); err != nil {
		t.Fatal(err)
	}
	d, _ := p.Detail(ctx, "p", []string{"rules"})
	add := actionByID(t, tabActions(d, "lifecycle"), "addlifecyclerule")
	if !hasAction(p.DetailActions(ctx, "p", []string{"rules"}), "addlifecyclerule") {
		t.Fatal("the bucket's page does not offer Add a rule")
	}
	if err := p.ActAt(ctx, "p", []string{"rules"}, "addlifecyclerule", map[string]string{
		"metageneration": add.Fields[0].Default, "action": "Delete object"}); err == nil {
		t.Error("a rule with no condition was accepted")
	}
	for _, v := range []map[string]string{
		{"action": "Set storage class to Coldline", "age": "30", "matchesStorageClass": "standard"},
		{"action": "Delete object", "numNewerVersions": "3", "isLive": "Noncurrent"},
	} {
		d, _ = p.Detail(ctx, "p", []string{"rules"})
		v["metageneration"] = actionByID(t, tabActions(d, "lifecycle"), "addlifecyclerule").Fields[0].Default
		if err := p.ActAt(ctx, "p", []string{"rules"}, "addlifecyclerule", v); err != nil {
			t.Fatalf("add %v: %v", v, err)
		}
	}
	a, _ := c.Bucket("rules").Attrs(ctx)
	if len(a.Lifecycle.Rules) != 2 || a.Lifecycle.Rules[0].Action.StorageClass != "COLDLINE" ||
		a.Lifecycle.Rules[0].Condition.AgeInDays != 30 || a.Lifecycle.Rules[1].Condition.NumNewerVersions != 3 {
		t.Fatalf("the rules read %+v", a.Lifecycle.Rules)
	}
	d, _ = p.Detail(ctx, "p", []string{"rules"})
	var lc console.Section
	for _, s := range d.Sections {
		if s.ID == "lifecycle" {
			lc = s
		}
	}
	if len(lc.Listing.Items) != 2 || lc.Listing.Items[0].Fields["Action"] != "Set to Coldline" ||
		!strings.Contains(lc.Listing.Items[0].Fields["Object condition"], "Age: 30 days") ||
		!strings.Contains(lc.Listing.Items[1].Fields["Object condition"], "Live state: Noncurrent") {
		t.Fatalf("the Lifecycle tab lists %+v", lc.Listing.Items)
	}
	first := lc.Listing.Items[0]
	del := actionByID(t, p.DetailActions(ctx, "p", first.Target), "deletelifecyclerule")
	if err := p.ActAt(ctx, "p", first.Target, "deletelifecyclerule", map[string]string{"metageneration": del.Fields[0].Default}); err != nil {
		t.Fatal(err)
	}
	a, _ = c.Bucket("rules").Attrs(ctx)
	if len(a.Lifecycle.Rules) != 1 || a.Lifecycle.Rules[0].Condition.NumNewerVersions != 3 {
		t.Errorf("after Delete rule the rules read %+v", a.Lifecycle.Rules)
	}
	// A stale page is refused rather than overwriting a newer change.
	if err := p.ActAt(ctx, "p", first.Target, "deletelifecyclerule", map[string]string{"metageneration": del.Fields[0].Default}); err == nil {
		t.Error("a Delete rule from a stale page was accepted")
	}
}

// Create folder on a bucket without hierarchical namespace makes the empty
// "name/" object, which the browser lists as a folder (#1843).
func TestStorageCreateFolderOnAFlatBucket(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, c := newObjectOpsProvider(t)
	if err := c.Bucket("flat").Create(ctx, "p", nil); err != nil {
		t.Fatal(err)
	}
	if !hasAction(p.DetailActions(ctx, "p", []string{"flat"}), "createsimplefolder") {
		t.Fatal("a flat bucket does not offer Create folder")
	}
	if err := p.ActAt(ctx, "p", []string{"flat"}, "createsimplefolder", map[string]string{"name": "reports"}); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, "p", []string{"flat", "reports"}, "createsimplefolder", map[string]string{"name": "2026/"}); err != nil {
		t.Fatal(err)
	}
	if a, err := c.Bucket("flat").Object("reports/2026/").Attrs(ctx); err != nil || a.Size != 0 {
		t.Fatalf("the nested folder object: %+v %v", a, err)
	}
	d, _ := p.Detail(ctx, "p", []string{"flat"})
	if len(d.Sections[0].Listing.Items) != 1 || d.Sections[0].Listing.Items[0].Name != "reports/" {
		t.Errorf("the bucket lists %+v", d.Sections[0].Listing.Items)
	}
	if err := p.ActAt(ctx, "p", []string{"flat"}, "createsimplefolder", map[string]string{"name": "reports"}); err == nil {
		t.Error("an existing folder was made again")
	}
}
