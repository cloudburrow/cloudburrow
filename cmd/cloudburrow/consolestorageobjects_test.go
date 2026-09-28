package main

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"cloud.google.com/go/storage"

	"github.com/cloudburrow/cloudburrow/internal/console"
	gcsbuiltin "github.com/cloudburrow/cloudburrow/internal/service/storage"
)

// newObjectOpsProvider is the storage provider over an in-process builtin
// storage server, with a bucket holding what is written through the official
// client it returns.
func newObjectOpsProvider(t *testing.T) (storageProvider, *storage.Client) {
	t.Helper()
	srv, err := gcsbuiltin.NewServer(gcsbuiltin.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(srv)
	t.Cleanup(h.Close)
	p := storageProvider{endpoint: strings.TrimPrefix(h.URL, "http://")}
	ctx := context.Background()
	for _, b := range []string{"ops", "other"} {
		if _, err := p.Create(ctx, "p", map[string]string{"name": b}); err != nil {
			t.Fatal(err)
		}
	}
	c, err := p.storageClient(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return p, c
}

func putObject(t *testing.T, c *storage.Client, bucket, name, data string, meta map[string]string) {
	t.Helper()
	w := c.Bucket(bucket).Object(name).NewWriter(context.Background())
	w.ContentType = "text/plain"
	w.Metadata = meta
	if _, err := io.WriteString(w, data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func readObject(t *testing.T, c *storage.Client, bucket, name string) string {
	t.Helper()
	r, err := c.Bucket(bucket).Object(name).NewReader(context.Background())
	if err != nil {
		t.Fatalf("read %s/%s: %v", bucket, name, err)
	}
	defer r.Close()
	b, _ := io.ReadAll(r)
	return string(b)
}

// formValues is what the browser submits for an action: every field's default.
func formValues(a console.Action) map[string]string {
	out := map[string]string{}
	for _, f := range a.Fields {
		out[f.Name] = f.Default
		if f.Type == "checkbox" && f.Default == "" {
			out[f.Name] = "false"
		}
	}
	return out
}

func actionByID(t *testing.T, actions []console.Action, id string) console.Action {
	t.Helper()
	for _, a := range actions {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("no %s action among %v", id, actions)
	return console.Action{}
}

// An object row opens its own page, addressed apart from the folders, whose
// crumbs are its bucket and folders and whose metadata is the object's; the
// row and the page offer the same actions, prefilled from the object (#790).
func TestStorageObjectPageAndActions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, c := newObjectOpsProvider(t)
	putObject(t, c, "ops", "dir/a.txt", "alpha", map[string]string{"team": "blue"})

	d, err := p.Detail(ctx, "p", []string{"ops", "dir"})
	if err != nil {
		t.Fatal(err)
	}
	// A folder page's one action of its own is Create managed folder (#828).
	if len(d.Actions) != 1 || d.Actions[0].ID != "createmanagedfolder" {
		t.Errorf("a folder page draws actions of its own: %v", d.Actions)
	}
	objs := d.Sections[0].Listing
	if len(objs.SelectActions) != 1 || objs.SelectActions[0].ID != "compose" || objs.SelectActions[0].SelectionField != "sources" {
		t.Errorf("the objects listing offers %v on a selection, want Compose into sources", objs.SelectActions)
	}
	row := objs.Items[0]
	if want := []string{objectPage, "ops", "dir/a.txt"}; strings.Join(row.Opens, "|") != strings.Join(want, "|") {
		t.Errorf("the object row opens %v, want %v", row.Opens, want)
	}
	edit := actionByID(t, row.Actions, "editmetadata")
	if v := formValues(edit); v["contentType"] != "text/plain" || v["metadata"] != `{"team":"blue"}` {
		t.Errorf("the row's Edit metadata is prefilled with %v", v)
	}

	page, err := p.Detail(ctx, "p", row.Opens)
	if err != nil || page.Unavailable != "" {
		t.Fatalf("object page: %v %q", err, page.Unavailable)
	}
	var labels []string
	for _, cr := range page.Trail {
		labels = append(labels, cr.Label)
	}
	if strings.Join(labels, "/") != "ops/dir/a.txt" || strings.Join(page.Trail[1].Path, "/") != "ops/dir" {
		t.Errorf("trail = %+v, want ops / dir / a.txt with dir opening ops/dir", page.Trail)
	}
	got := map[string]string{}
	for _, g := range page.Sections[0].Groups {
		for _, pr := range g.Properties {
			got[g.Heading+"/"+pr.Label] = pr.Value
		}
	}
	for k, v := range map[string]string{
		"Object/Content-Type": "text/plain", "Version/Metageneration": "1",
		"Custom metadata/team": "blue", "Protection/Temporary hold": "Off", "Object/Storage class": "STANDARD",
	} {
		if got[k] != v {
			t.Errorf("%s = %q, want %q (page: %v)", k, got[k], v, got)
		}
	}
	if got["Hashes/CRC32C"] == "" || got["Hashes/MD5"] == "" {
		t.Errorf("the page shows no hashes: %v", got)
	}
	var ids []string
	for _, a := range p.DetailActions(ctx, "p", row.Opens) {
		ids = append(ids, a.ID)
	}
	if strings.Join(ids, ",") != "copy,move,editmetadata,storageclass" {
		t.Errorf("object page actions = %v", ids)
	}
}

// Copy, Move, Edit metadata, Edit storage class and Compose each do what the
// official client then reads, and refuse to replace an existing destination
// with the API's precondition failure unless the form says to (#790).
func TestStorageObjectActionsThroughTheAPI(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, c := newObjectOpsProvider(t)
	putObject(t, c, "ops", "a.txt", "alpha", map[string]string{"k": "v"})
	putObject(t, c, "ops", "b.txt", "beta", nil)
	path := objectPath("ops", "a.txt")
	acts := p.DetailActions(ctx, "p", path)

	// Copy onto itself, the form's default: refused, nothing changed.
	copyVals := formValues(actionByID(t, acts, "copy"))
	err := p.ActAt(ctx, "p", path, "copy", copyVals)
	if err == nil || !strings.Contains(err.Error(), "412") {
		t.Errorf("a copy onto an existing name = %v, want the API's 412", err)
	}
	copyVals["bucket"], copyVals["destination"] = "other", "copied.txt"
	if err := p.ActAt(ctx, "p", path, "copy", copyVals); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if got := readObject(t, c, "other", "copied.txt"); got != "alpha" {
		t.Errorf("the copy reads %q", got)
	}
	if a, _ := c.Bucket("other").Object("copied.txt").Attrs(ctx); a == nil || a.Metadata["k"] != "v" || a.ContentType != "text/plain" {
		t.Errorf("the copy's metadata = %+v", a)
	}

	// Move onto b.txt: refused; replace: done, the source gone.
	moveVals := map[string]string{"destination": "b.txt", "replace": "false"}
	if err := p.ActAt(ctx, "p", objectPath("other", "copied.txt"), "move", map[string]string{"destination": "moved.txt", "replace": "false"}); err != nil {
		t.Fatalf("move: %v", err)
	}
	if _, err := c.Bucket("other").Object("copied.txt").Attrs(ctx); !errors.Is(err, storage.ErrObjectNotExist) {
		t.Errorf("the moved source is still there (%v)", err)
	}
	if got := readObject(t, c, "other", "moved.txt"); got != "alpha" {
		t.Errorf("the moved object reads %q", got)
	}
	if err := p.ActAt(ctx, "p", path, "move", moveVals); err == nil || !strings.Contains(err.Error(), "412") {
		t.Errorf("a move onto an existing name = %v, want the API's 412", err)
	}
	if got := readObject(t, c, "ops", "b.txt"); got != "beta" {
		t.Errorf("a refused move changed b.txt to %q", got)
	}

	// Edit metadata: replaces the custom keys and the three headers.
	before, _ := c.Bucket("ops").Object("a.txt").Attrs(ctx)
	meta := map[string]string{"contentType": "application/json", "cacheControl": "no-cache",
		"contentDisposition": "attachment", "metadata": `{"n":"1"}`}
	if err := p.ActAt(ctx, "p", path, "editmetadata", meta); err != nil {
		t.Fatalf("edit metadata: %v", err)
	}
	after, _ := c.Bucket("ops").Object("a.txt").Attrs(ctx)
	if after.ContentType != "application/json" || after.CacheControl != "no-cache" || after.ContentDisposition != "attachment" ||
		len(after.Metadata) != 1 || after.Metadata["n"] != "1" {
		t.Errorf("after the edit Attrs reads %+v", after)
	}
	if after.Generation != before.Generation || after.Metageneration != before.Metageneration+1 {
		t.Errorf("an edit moved generation %d->%d, metageneration %d->%d; want the same generation, one more metageneration",
			before.Generation, after.Generation, before.Metageneration, after.Metageneration)
	}

	// Edit storage class: a new generation with the same bytes.
	if err := p.ActAt(ctx, "p", path, "storageclass", map[string]string{"storageClass": "NEARLINE"}); err != nil {
		t.Fatalf("storage class: %v", err)
	}
	rewritten, _ := c.Bucket("ops").Object("a.txt").Attrs(ctx)
	if rewritten.StorageClass != "NEARLINE" || rewritten.Generation == after.Generation || rewritten.Metadata["n"] != "1" {
		t.Errorf("after the rewrite Attrs reads class %s generation %d metadata %v", rewritten.StorageClass, rewritten.Generation, rewritten.Metadata)
	}
	if err := p.ActAt(ctx, "p", path, "storageclass", map[string]string{"storageClass": "REGIONAL"}); err == nil {
		t.Error("a legacy storage class was accepted")
	}

	// Compose b.txt and b.txt, in the order given, on the bucket's page.
	compose := map[string]string{"sources": "b.txt\nb.txt\n", "destination": "joined.txt", "contentType": "text/plain", "replace": "false"}
	if err := p.ActAt(ctx, "p", []string{"ops"}, "compose", compose); err != nil {
		t.Fatalf("compose: %v", err)
	}
	if got := readObject(t, c, "ops", "joined.txt"); got != "betabeta" {
		t.Errorf("the composed object reads %q", got)
	}
	if err := p.ActAt(ctx, "p", []string{"ops"}, "compose", compose); err == nil || !strings.Contains(err.Error(), "412") {
		t.Errorf("a compose onto an existing name = %v, want the API's 412", err)
	}
	compose["replace"] = "true"
	compose["sources"] = "b.txt"
	if err := p.ActAt(ctx, "p", []string{"ops"}, "compose", compose); err != nil {
		t.Fatalf("compose with replace: %v", err)
	}
	if got := readObject(t, c, "ops", "joined.txt"); got != "beta" {
		t.Errorf("the replaced composed object reads %q", got)
	}
}

// Every Replace checkbox asks for the destination's name back (#790).
func TestStorageObjectActionFieldsCarryTheReplaceConfirmation(t *testing.T) {
	t.Parallel()
	for _, a := range append(objectActions("b", objectMeta{Name: "o"}), composeAction("")) {
		for _, f := range a.Fields {
			if f.Confirm != "" && (f.Type != "checkbox" || f.ConfirmWith != "destination") {
				t.Errorf("%s/%s confirms but is a %s naming %q", a.ID, f.Name, f.Type, f.ConfirmWith)
			}
		}
	}
}
