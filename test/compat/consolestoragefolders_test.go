//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"cloud.google.com/go/storage/control/apiv2/controlpb"
	"google.golang.org/grpc/codes"
)

// TestConsoleStorageHierarchicalFolders (#1203): the console's
// hierarchical-namespace folders, performed through its own routes as its
// forms and buttons submit them, and read back through the official
// clients.
//
//   - Create, with Hierarchical namespace checked, makes a bucket the
//     official storage client reads with HierarchicalNamespace enabled and
//     uniform bucket-level access; with versioning as well, the API refuses.
//   - Create folder makes one the Storage Control client's GetFolder reads;
//     the bucket's page lists it typed Folder, with Rename folder, Delete
//     folder (empty only) and Delete folder and contents on its row.
//   - Rename folder answers with its operation, and the object in the
//     folder is read at its new name; Delete folder and contents leaves
//     GetFolder NotFound and the object gone; Delete folder deletes an
//     empty one.
//   - The Operations tab lists the rename and the recursive delete, and an
//     operation's page reads what buckets.operations.get returns.
func TestConsoleStorageHierarchicalFolders(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	sc := storageClient(t, h)
	s := storageJSON(t, h)
	cc := storageControl(t, h)
	ctx := h.Context()
	project := h.Project()
	b := project + "-hns-console"

	create := func(values map[string]string) (int, string) {
		t.Helper()
		values["name"] = b
		body, _ := json.Marshal(values)
		return consoleDo(t, addr, http.MethodPost, "/api/resources/storage?project="+project, string(body))
	}
	if code, body := create(map[string]string{"uniformAccess": "true", "hierarchicalNamespace": "true", "versioning": "true"}); code == http.StatusOK ||
		!strings.Contains(body, "versioning") {
		t.Errorf("console Create with hierarchical namespace and versioning = %d %s; want the API's refusal", code, body)
	}
	if code, body := create(map[string]string{"uniformAccess": "true", "hierarchicalNamespace": "true", "versioning": "false"}); code != http.StatusOK {
		t.Fatalf("console Create with hierarchical namespace = %d: %s", code, body)
	}
	bh := sc.Bucket(b)
	t.Cleanup(func() {
		ctx := context.Background()
		if fs, _ := s.Folders.List(b).Delimiter("/").Context(ctx).Do(); fs != nil {
			for _, f := range fs.Items {
				_, _ = s.Folders.DeleteRecursive(b, f.Name).Context(ctx).Do()
			}
		}
		emptyAndDelete(ctx, bh)
	})
	a, err := bh.Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if a.HierarchicalNamespace == nil || !a.HierarchicalNamespace.Enabled || !a.UniformBucketLevelAccess.Enabled || a.VersioningEnabled {
		t.Errorf("the console's HNS bucket reads hns %+v, uniform %v, versioning %v",
			a.HierarchicalNamespace, a.UniformBucketLevelAccess.Enabled, a.VersioningEnabled)
	}

	type action struct{ ID string }
	type row struct {
		Name    string
		Fields  map[string]string
		Target  []string
		Opens   []string
		Actions []action
	}
	type page struct {
		Unavailable string
		Actions     []action
		Sections    []struct {
			ID      string
			Listing struct{ Items []row }
			Groups  []struct {
				Properties []struct{ Label, Value string }
			}
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
	rows := func(d page, id string) []row {
		for _, sec := range d.Sections {
			if sec.ID == id {
				return sec.Listing.Items
			}
		}
		return nil
	}
	rowNamed := func(d page, name string) *row {
		for _, r := range rows(d, "objects") {
			if r.Name == name {
				return &r
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
	type result struct {
		Error  string
		Result *struct{ Items []row }
	}
	act := func(path []string, id string, values map[string]string) (int, result) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"Path": path, "Action": id, "Values": values})
		code, resp := consoleDo(t, addr, http.MethodPost, "/api/actions/storage?project="+project, string(body))
		var r result
		_ = json.Unmarshal([]byte(resp), &r)
		if r.Error == "" && code != http.StatusOK {
			r.Error = resp
		}
		return code, r
	}
	folderName := func(name string) string { return "projects/_/buckets/" + b + "/folders/" + name }

	// Create folder.
	if d := detail(b); !offers(d.Actions, "createfolder") {
		t.Fatalf("the HNS bucket's page offers %+v", d.Actions)
	}
	if code, r := act([]string{b}, "createfolder", map[string]string{"name": "logs", "recursive": "true"}); code != http.StatusOK {
		t.Fatalf("console Create folder = %d: %s", code, r.Error)
	}
	if code, r := act([]string{b}, "createfolder", map[string]string{"name": "empty", "recursive": "false"}); code != http.StatusOK {
		t.Fatalf("console Create folder = %d: %s", code, r.Error)
	}
	if f, err := cc.GetFolder(ctx, &controlpb.GetFolderRequest{Name: folderName("logs/")}); err != nil || f.GetName() != folderName("logs/") {
		t.Fatalf("GetFolder after the console's create = %v, %v", f, err)
	}
	logs := rowNamed(detail(b), "logs/")
	if logs == nil || logs.Fields["Type"] != "Folder" || !offers(logs.Actions, "renamefolder") ||
		!offers(logs.Actions, "deletefolder") || !offers(logs.Actions, "deletefolderrecursive") {
		t.Fatalf("the console-made folder is listed as %+v", logs)
	}

	// An object in it: Delete folder is no longer offered, nor performed.
	putObject(t, ctx, bh.Object("logs/a.txt"), "a")
	if logs = rowNamed(detail(b), "logs/"); logs == nil || offers(logs.Actions, "deletefolder") {
		t.Fatalf("a folder holding an object is listed as %+v", logs)
	}
	if code, r := act(logs.Target, "deletefolder", nil); code != http.StatusBadRequest || !strings.Contains(r.Error, "not available") {
		t.Errorf("Delete folder on a folder holding an object = %d %s; want it refused as not offered", code, r.Error)
	}

	// Rename folder, an operation.
	code, r := act(logs.Target, "renamefolder", map[string]string{"destination": "archive/"})
	if code != http.StatusOK || r.Result == nil {
		t.Fatalf("console Rename folder = %d: %s %+v", code, r.Error, r.Result)
	}
	answered := map[string]string{}
	for _, it := range r.Result.Items {
		answered[it.Name] = it.Fields["Value"]
	}
	if answered["Type"] != "RENAME_FOLDER" || answered["State"] != "Done" || !strings.Contains(answered["Operation"], "/operations/") {
		t.Errorf("Rename folder answered %v", answered)
	}
	if _, err := bh.Object("archive/a.txt").Attrs(ctx); err != nil {
		t.Errorf("the renamed folder's object: %v", err)
	}
	if _, err := cc.GetFolder(ctx, &controlpb.GetFolderRequest{Name: folderName("logs/")}); grpcCode(err) != codes.NotFound {
		t.Errorf("GetFolder of the renamed folder's old name = %v; want NotFound", err)
	}

	// Delete folder and contents.
	archive := rowNamed(detail(b), "archive/")
	if archive == nil {
		t.Fatal("the renamed folder is not listed")
	}
	if code, r := act(archive.Target, "deletefolderrecursive", nil); code != http.StatusOK {
		t.Fatalf("console Delete folder and contents = %d: %s", code, r.Error)
	}
	if _, err := cc.GetFolder(ctx, &controlpb.GetFolderRequest{Name: folderName("archive/")}); grpcCode(err) != codes.NotFound {
		t.Errorf("GetFolder after Delete folder and contents = %v; want NotFound", err)
	}
	if _, err := bh.Object("archive/a.txt").Attrs(ctx); err == nil {
		t.Error("the object survived Delete folder and contents")
	}

	// Delete folder, on the empty one.
	empty := rowNamed(detail(b), "empty/")
	if empty == nil || !offers(empty.Actions, "deletefolder") {
		t.Fatalf("the empty folder is listed as %+v", empty)
	}
	if code, r := act(empty.Target, "deletefolder", nil); code != http.StatusOK {
		t.Fatalf("console Delete folder = %d: %s", code, r.Error)
	}
	if _, err := cc.GetFolder(ctx, &controlpb.GetFolderRequest{Name: folderName("empty/")}); grpcCode(err) != codes.NotFound {
		t.Errorf("GetFolder after Delete folder = %v; want NotFound", err)
	}

	// The Operations tab, and an operation's page, against the API's own.
	ops := rows(detail(b), "operations")
	if len(ops) != 2 {
		t.Fatalf("the Operations tab lists %+v", ops)
	}
	listed, err := s.Operations.List(b).Context(ctx).Do()
	if err != nil || len(listed.Operations) != 2 {
		t.Fatalf("buckets.operations.list = %+v, %v", listed, err)
	}
	for _, op := range ops {
		got, err := s.Operations.Get(b, op.Name).Context(ctx).Do()
		if err != nil || !got.Done {
			t.Errorf("buckets.operations.get %s = %+v, %v", op.Name, got, err)
		}
		d := detail(op.Opens...)
		props := map[string]string{}
		for _, sec := range d.Sections {
			for _, g := range sec.Groups {
				for _, p := range g.Properties {
					props[p.Label] = p.Value
				}
			}
		}
		if got != nil && (props["Name"] != got.Name || props["Type"] != op.Fields["Type"] || props["State"] != "Done") {
			t.Errorf("operation %s's page reads %v; the API names it %s", op.Name, props, got.Name)
		}
	}
}
