package main

import (
	"context"
	"strings"
	"testing"

	"cloud.google.com/go/storage"
)

// The bucket tabs' own actions (#1843): Switch to on Permissions, Edit
// object versioning on Protection with its limits as lifecycle rules, and
// Edit Autoclass on Configuration; Create a bucket's radio choices; and an
// object page's addresses.
func TestStorageBucketTabActions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, c := newObjectOpsProvider(t)
	if _, err := p.Create(ctx, "p", map[string]string{"name": "tabs", "uniformAccess": accessFineGrained,
		"storageClassChoice": classChoiceAutoclass}); err != nil {
		t.Fatal(err)
	}
	// Autoclass read as the JSON API returns it: the stored setting.
	b, err := p.readBucket(ctx, "tabs")
	if err != nil || b.IAMConfiguration.UniformBucketLevelAccess.Enabled || !b.autoclassOn() {
		t.Fatalf("Create made %+v, %v", b, err)
	}
	act := func(id string, values map[string]string) {
		t.Helper()
		d, _ := p.Detail(ctx, "p", []string{"tabs"})
		var tab string
		for _, s := range d.Sections {
			for _, x := range s.Actions {
				if x.ID == id {
					tab = s.ID
				}
			}
		}
		if tab == "" {
			t.Fatalf("no tab offers %s", id)
		}
		values["metageneration"] = actionByID(t, p.DetailActions(ctx, "p", []string{"tabs"}), id).Fields[0].Default
		if err := p.ActAt(ctx, "p", []string{"tabs"}, id, values); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	d, _ := p.Detail(ctx, "p", []string{"tabs"})
	if perm := sectionNamed(d, "permissions"); perm == nil || !strings.HasPrefix(perm.Note, "Access control: Fine-grained") {
		t.Errorf("the Permissions tab reads %+v", perm)
	}
	act("switchaccess", map[string]string{"uniformAccess": accessUniform})
	act("editversioning", map[string]string{"versioning": "Enabled", "maxVersions": "3", "expireDays": "7"})
	act("editautoclass", map[string]string{"autoclass": "Disabled"})
	a, _ := c.Bucket("tabs").Attrs(ctx)
	if b, _ = p.readBucket(ctx, "tabs"); b.autoclassOn() {
		t.Error("Edit Autoclass did not disable it")
	}
	if !a.UniformBucketLevelAccess.Enabled || !a.VersioningEnabled ||
		len(a.Lifecycle.Rules) != 2 || a.Lifecycle.Rules[0].Condition.NumNewerVersions != 3 ||
		a.Lifecycle.Rules[1].Condition.DaysSinceNoncurrentTime != 7 ||
		a.Lifecycle.Rules[0].Condition.Liveness != storage.Archived {
		t.Errorf("after the tab actions the bucket reads uniform %v, versioning %v, autoclass %+v, rules %+v",
			a.UniformBucketLevelAccess.Enabled, a.VersioningEnabled, a.Autoclass, a.Lifecycle.Rules)
	}

	// An object's page: Google's addresses, and Download and Delete.
	w := c.Bucket("tabs").Object("dir/a.txt").NewWriter(ctx)
	_, _ = w.Write([]byte("x"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	od, err := p.Detail(ctx, "p", objectPath("tabs", "dir/a.txt"))
	if err != nil || len(od.Sections) == 0 {
		t.Fatalf("object page: %v", err)
	}
	if strings.Join(od.Object, "/") != "tabs/dir/a.txt" {
		t.Errorf("the object page's object is %v", od.Object)
	}
	var copies []string
	for _, g := range od.Sections[0].Groups {
		for _, pr := range g.Properties {
			if pr.Copy != "" {
				copies = append(copies, pr.Copy+"="+pr.Value)
			}
		}
	}
	if len(copies) != 3 || !strings.HasPrefix(copies[0], "Copy URL=http://") ||
		!strings.Contains(copies[1], "/api/objects/storage/download?") || copies[2] != "Copy gsutil URI=gs://tabs/dir/a.txt" {
		t.Errorf("the object page's addresses are %q", copies)
	}
}
