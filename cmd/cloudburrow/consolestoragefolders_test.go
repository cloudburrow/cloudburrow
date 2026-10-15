package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"cloud.google.com/go/storage"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// The Create form's Hierarchical namespace holds uniform bucket-level
// access on and versioning off, by fields that exist on the form, and is
// sent as hierarchicalNamespace.enabled (#1203).
func TestStorageCreateFormHierarchicalNamespace(t *testing.T) {
	t.Parallel()
	_, fields := storageProvider{}.CreateForm()
	byName := map[string]console.Field{}
	for _, f := range fields {
		byName[f.Name] = f
	}
	hns, ok := byName["hierarchicalNamespace"]
	if !ok || hns.Type != "checkbox" || hns.Default != "false" {
		t.Fatalf("the Create form's hierarchical namespace field is %+v", hns)
	}
	if strings.Join(hns.Requires, ",") != "uniformAccess" || strings.Join(hns.Excludes, ",") != "versioning" {
		t.Errorf("hierarchical namespace requires %v and excludes %v", hns.Requires, hns.Excludes)
	}
	for _, n := range append(append([]string{}, hns.Requires...), hns.Excludes...) {
		if byName[n].Type != "checkbox" {
			t.Errorf("hierarchical namespace ties %q, which is no checkbox on the form", n)
		}
	}
	raw, _, err := bucketCreateRequest("b", map[string]string{"hierarchicalNamespace": "true", "uniformAccess": "true"})
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		HierarchicalNamespace struct{ Enabled bool } `json:"hierarchicalNamespace"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || !body.HierarchicalNamespace.Enabled {
		t.Errorf("buckets.insert body is %s", raw)
	}
	if raw, _, _ := bucketCreateRequest("b", map[string]string{"hierarchicalNamespace": "false"}); strings.Contains(string(raw), "hierarchicalNamespace") {
		t.Errorf("an unchecked hierarchical namespace is sent: %s", raw)
	}
}

func folderRowNamed(l console.Listing, name string) *console.Resource {
	for i := range l.Items {
		if l.Items[i].Name == name {
			return &l.Items[i]
		}
	}
	return nil
}

func sectionNamed(d console.Detail, id string) *console.Section {
	for i := range d.Sections {
		if d.Sections[i].ID == id {
			return &d.Sections[i]
		}
	}
	return nil
}

// An HNS bucket's page lists its folders from folders.list, offers Create
// folder, and on each folder's row Rename folder, Delete folder while it is
// empty and Delete folder and contents; its Operations tab lists the
// rename and the recursive delete, each opening its own page. A plain
// bucket offers none of it (#1203).
func TestStorageHierarchicalFoldersThroughTheConsole(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, c := newObjectOpsProvider(t)

	// The API refuses versioning on a hierarchical bucket, with its message.
	if _, err := p.Create(ctx, "p", map[string]string{"name": "hns-versioned", "uniformAccess": "true",
		"hierarchicalNamespace": "true", "versioning": "true"}); err == nil || !strings.Contains(err.Error(), "versioning") {
		t.Errorf("an HNS bucket with versioning was created, or refused with %v", err)
	}
	if _, err := p.Create(ctx, "p", map[string]string{"name": "hns", "uniformAccess": "true",
		"hierarchicalNamespace": "true"}); err != nil {
		t.Fatalf("create an HNS bucket: %v", err)
	}

	d, err := p.Detail(ctx, "p", []string{"hns"})
	if err != nil || d.Unavailable != "" {
		t.Fatalf("the HNS bucket's page: %v %s", err, d.Unavailable)
	}
	if !hasAction(d.Actions, "createfolder") || !hasAction(p.DetailActions(ctx, "p", []string{"hns"}), "createfolder") {
		t.Errorf("the HNS bucket's page offers %+v", d.Actions)
	}
	if sectionNamed(d, "operations") == nil {
		t.Error("the HNS bucket's page has no Operations tab")
	}
	for _, f := range d.Edit.Fields {
		if (f.Name == "versioning" || f.Name == "uniformAccess") && !f.Immutable {
			t.Errorf("Edit bucket lets an HNS bucket change %s", f.Name)
		}
	}
	plain, _ := p.Detail(ctx, "p", []string{"ops"})
	if hasAction(plain.Actions, "createfolder") || sectionNamed(plain, "operations") != nil ||
		hasAction(p.DetailActions(ctx, "p", []string{"ops"}), "createfolder") {
		t.Error("a bucket without hierarchical namespace offers folders")
	}

	// Create folder, nested, under the bucket's root.
	if err := p.ActAt(ctx, "p", []string{"hns"}, "createfolder", map[string]string{"name": "logs/2026", "recursive": "true"}); err != nil {
		t.Fatalf("Create folder: %v", err)
	}
	if err := p.ActAt(ctx, "p", []string{"hns"}, "createfolder", map[string]string{"name": "x/y", "recursive": "false"}); err == nil {
		t.Error("a nested folder without its parent was created without recursive")
	}
	objects, err := p.objects(ctx, "hns", "", []string{"hns"})
	if err != nil {
		t.Fatal(err)
	}
	logs := folderRowNamed(objects, "logs/")
	if logs == nil || logs.Fields["Type"] != "Folder" || strings.Join(logs.Target, "|") != "_folder|hns|logs/" {
		t.Fatalf("the folder is listed as %+v", logs)
	}
	if hasAction(logs.Actions, "deletefolder") || !hasAction(logs.Actions, "renamefolder") || !hasAction(logs.Actions, "deletefolderrecursive") {
		t.Errorf("a folder holding a folder offers %+v", logs.Actions)
	}
	sub, _ := p.objects(ctx, "hns", "logs/", []string{"hns", "logs"})
	y := folderRowNamed(sub, "2026/")
	if y == nil || !hasAction(y.Actions, "deletefolder") || !hasAction(p.DetailActions(ctx, "p", y.Target), "deletefolder") {
		t.Fatalf("the empty folder is listed as %+v", y)
	}
	fd, _ := p.Detail(ctx, "p", []string{"hns", "logs"})
	if !hasAction(fd.Actions, "createfolder") || !strings.Contains(propsText(fd.Summary), "Folder (hierarchical namespace)") {
		t.Errorf("a folder's page: actions %+v, summary %+v", fd.Actions, fd.Summary)
	}

	// Delete folder, on the empty one; then an object makes logs/ non-empty.
	if err := p.ActAt(ctx, "p", y.Target, "deletefolder", nil); err != nil {
		t.Fatalf("Delete folder: %v", err)
	}
	putObject(t, c, "hns", "logs/a.txt", "a", nil)
	if hasAction(p.DetailActions(ctx, "p", []string{"_folder", "hns", "logs/"}), "deletefolder") {
		t.Error("Delete folder is offered on a folder holding an object")
	}

	// Rename folder answers with its operation; the object moved with it.
	res, err := p.ActAtResult(ctx, "p", []string{"_folder", "hns", "logs/"}, "renamefolder", map[string]string{"destination": "archive"})
	if err != nil {
		t.Fatalf("Rename folder: %v", err)
	}
	if r := folderRowNamed(*res, "Type"); r == nil || r.Fields["Value"] != "RENAME_FOLDER" {
		t.Errorf("Rename folder answered %+v", res)
	}
	if _, err := c.Bucket("hns").Object("archive/a.txt").Attrs(ctx); err != nil {
		t.Errorf("the renamed folder's object: %v", err)
	}

	// Delete folder and contents.
	if _, err := p.ActAtResult(ctx, "p", []string{"_folder", "hns", "archive/"}, "deletefolderrecursive", nil); err != nil {
		t.Fatalf("Delete folder and contents: %v", err)
	}
	if _, err := c.Bucket("hns").Object("archive/a.txt").Attrs(ctx); !errors.Is(err, storage.ErrObjectNotExist) {
		t.Errorf("after Delete folder and contents the object reads %v", err)
	}
	if p.DetailActions(ctx, "p", []string{"_folder", "hns", "archive/"}) != nil {
		t.Error("a deleted folder still offers actions")
	}

	// The Operations tab lists both; each opens buckets.operations.get.
	d, _ = p.Detail(ctx, "p", []string{"hns"})
	ops := sectionNamed(d, "operations")
	if ops == nil || len(ops.Listing.Items) != 2 {
		t.Fatalf("the Operations tab is %+v", ops)
	}
	types := map[string]bool{}
	for _, op := range ops.Listing.Items {
		types[op.Fields["Type"]] = true
		od, err := p.Detail(ctx, "p", op.Opens)
		if err != nil || od.Unavailable != "" || !strings.Contains(propsText(od.Sections[0].Groups[0].Properties), op.Fields["Type"]) {
			t.Errorf("operation %s's page: %v %s %+v", op.Name, err, od.Unavailable, od.Sections)
		}
		if p.DetailActions(ctx, "p", op.Opens) != nil {
			t.Errorf("operation %s offers actions", op.Name)
		}
	}
	if !types["RENAME_FOLDER"] || !types["DELETE_FOLDER"] {
		t.Errorf("the Operations tab lists %v", types)
	}
}

func propsText(props []console.Property) string {
	var b strings.Builder
	for _, p := range props {
		b.WriteString(p.Label + "=" + p.Value + ";")
	}
	return b.String()
}
