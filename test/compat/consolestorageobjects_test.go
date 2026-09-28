//go:build compat

package compat

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"cloud.google.com/go/storage"
)

// TestConsoleStorageObjectOperations (#790): each object action the console
// offers — Copy, Move or rename, Edit metadata, Edit storage class, Compose on
// selected objects — is performed through the console's own routes, exactly
// as its forms submit them, and read back through the official storage
// client: content, metadata, and the generation (a new one for a copy, a
// move, a rewrite and a compose) and metageneration (one more for a metadata
// edit, on the same generation). Copy, Move and Compose onto an existing name
// are refused with the API's precondition failure and change nothing, and
// with Replace checked they overwrite.
func TestConsoleStorageObjectOperations(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	sc := storageClient(t, h)
	ctx := h.Context()
	project := h.Project()
	src := bucket(t, h, sc)
	// A second bucket, for a copy between buckets; bucket() makes one per
	// project.
	dst := sc.Bucket(h.Project() + "-dest")
	if err := dst.Create(ctx, project, nil); err != nil {
		t.Fatalf("create bucket %s: %v", dst.BucketName(), err)
	}
	t.Cleanup(func() { emptyAndDelete(h.Context(), dst) })
	b, other := src.BucketName(), dst.BucketName()

	put := func(bh *storage.BucketHandle, name, data string, meta map[string]string) *storage.ObjectAttrs {
		t.Helper()
		w := bh.Object(name).NewWriter(ctx)
		w.ContentType, w.Metadata = "text/plain", meta
		if _, err := io.WriteString(w, data); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		return w.Attrs()
	}
	read := func(bh *storage.BucketHandle, name string) (string, *storage.ObjectAttrs) {
		t.Helper()
		a, err := bh.Object(name).Attrs(ctx)
		if err != nil {
			t.Fatalf("Attrs %s/%s: %v", bh.BucketName(), name, err)
		}
		r, err := bh.Object(name).NewReader(ctx)
		if err != nil {
			t.Fatalf("NewReader %s/%s: %v", bh.BucketName(), name, err)
		}
		defer r.Close()
		data, _ := io.ReadAll(r)
		return string(data), a
	}

	// detail is the console's page for a path, as the browser reads it.
	type action struct {
		ID, SelectionField string
		Fields             []struct{ Name, Type, Default, Confirm string }
	}
	detail := func(path ...string) (actions []action, selects []action) {
		t.Helper()
		v := url.Values{"project": {project}}
		for _, s := range path {
			v.Add("name", s)
		}
		code, body := consoleDo(t, addr, http.MethodGet, "/api/detail/storage?"+v.Encode(), "")
		var d struct {
			Unavailable string
			Actions     []action
			Sections    []struct {
				Listing struct{ SelectActions []action }
			}
		}
		if err := json.Unmarshal([]byte(body), &d); code != http.StatusOK || err != nil || d.Unavailable != "" {
			t.Fatalf("console detail %v = %d (%v): %s", path, code, err, body)
		}
		for _, s := range d.Sections {
			selects = append(selects, s.Listing.SelectActions...)
		}
		return d.Actions, selects
	}
	// form is an action's defaults, what the browser submits untouched.
	form := func(actions []action, id string) map[string]string {
		t.Helper()
		for _, a := range actions {
			if a.ID != id {
				continue
			}
			out := map[string]string{}
			for _, f := range a.Fields {
				out[f.Name] = f.Default
				if f.Type == "checkbox" {
					out[f.Name] = "false"
					if f.Confirm == "" {
						t.Errorf("%s's %s checkbox asks for no confirmation", id, f.Name)
					}
				}
			}
			return out
		}
		t.Fatalf("the console does not offer %s: %v", id, actions)
		return nil
	}
	act := func(path []string, id string, values map[string]string) (int, string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"Path": path, "Action": id, "Values": values})
		return consoleDo(t, addr, http.MethodPost, "/api/actions/storage?project="+project, string(body))
	}
	objPage := func(bucket, name string) []string { return []string{"_details", bucket, name} }
	refused412 := func(what string, code int, body string) {
		t.Helper()
		var e struct{ Error string }
		_ = json.Unmarshal([]byte(body), &e)
		if code != http.StatusBadRequest || !strings.Contains(e.Error, "412") ||
			!strings.Contains(e.Error, "pre-conditions you specified did not hold") {
			t.Errorf("%s = %d %s; want the API's precondition failure", what, code, body)
		}
	}

	orig := put(src, "docs/a.txt", "alpha", map[string]string{"owner": "console"})
	put(src, "docs/b.txt", "beta", nil)

	// Copy, into another bucket.
	actions, _ := detail(objPage(b, "docs/a.txt")...)
	values := form(actions, "copy")
	if values["bucket"] != b || values["destination"] != "docs/a.txt" {
		t.Errorf("Copy is prefilled with %v", values)
	}
	code, body := act(objPage(b, "docs/a.txt"), "copy", values)
	refused412("Copy onto itself", code, body)
	values["bucket"], values["destination"] = other, "copied/a.txt"
	if code, body := act(objPage(b, "docs/a.txt"), "copy", values); code != http.StatusOK {
		t.Fatalf("console Copy = %d: %s", code, body)
	}
	data, copied := read(dst, "copied/a.txt")
	if data != "alpha" || copied.ContentType != "text/plain" || copied.Metadata["owner"] != "console" ||
		copied.Generation == orig.Generation || copied.Metageneration != 1 {
		t.Errorf("the copy reads %q, %+v", data, copied)
	}

	// Move or rename: onto an existing name refused; elsewhere, the source goes.
	actions, _ = detail(objPage(other, "copied/a.txt")...)
	values = form(actions, "move")
	put(dst, "taken.txt", "taken", nil)
	values["destination"] = "taken.txt"
	code, body = act(objPage(other, "copied/a.txt"), "move", values)
	refused412("Move onto an existing name", code, body)
	if data, _ := read(dst, "taken.txt"); data != "taken" {
		t.Errorf("a refused move changed taken.txt to %q", data)
	}
	values["destination"] = "renamed.txt"
	if code, body := act(objPage(other, "copied/a.txt"), "move", values); code != http.StatusOK {
		t.Fatalf("console Move = %d: %s", code, body)
	}
	if _, err := dst.Object("copied/a.txt").Attrs(ctx); !errors.Is(err, storage.ErrObjectNotExist) {
		t.Errorf("the move's source is still there (%v)", err)
	}
	data, moved := read(dst, "renamed.txt")
	if data != "alpha" || moved.Metadata["owner"] != "console" {
		t.Errorf("the moved object reads %q, %+v", data, moved)
	}
	// With Replace, as the browser sends it after the name is typed back.
	values["destination"], values["replace"] = "taken.txt", "true"
	if code, body := act(objPage(other, "renamed.txt"), "move", values); code != http.StatusOK {
		t.Fatalf("console Move with Replace = %d: %s", code, body)
	}
	if data, _ := read(dst, "taken.txt"); data != "alpha" {
		t.Errorf("Move with Replace left taken.txt reading %q", data)
	}

	// Edit metadata: what ObjectHandle.Attrs returns.
	actions, _ = detail(objPage(b, "docs/a.txt")...)
	values = form(actions, "editmetadata")
	if values["contentType"] != "text/plain" || values["metadata"] != `{"owner":"console"}` {
		t.Errorf("Edit metadata is prefilled with %v", values)
	}
	values["contentType"], values["cacheControl"] = "application/json", "public, max-age=60"
	values["contentDisposition"], values["metadata"] = "attachment; filename=a.json", `{"stage":"edited"}`
	if code, body := act(objPage(b, "docs/a.txt"), "editmetadata", values); code != http.StatusOK {
		t.Fatalf("console Edit metadata = %d: %s", code, body)
	}
	_, edited := read(src, "docs/a.txt")
	if edited.ContentType != "application/json" || edited.CacheControl != "public, max-age=60" ||
		edited.ContentDisposition != "attachment; filename=a.json" ||
		len(edited.Metadata) != 1 || edited.Metadata["stage"] != "edited" {
		t.Errorf("after the console's edit Attrs reads %+v", edited)
	}
	if edited.Generation != orig.Generation || edited.Metageneration != orig.Metageneration+1 {
		t.Errorf("the edit moved generation %d->%d and metageneration %d->%d; want the same generation and one more",
			orig.Generation, edited.Generation, orig.Metageneration, edited.Metageneration)
	}

	// Edit storage class: a rewrite, a new generation of the same bytes.
	values = form(actions, "storageclass")
	values["storageClass"] = "COLDLINE"
	if code, body := act(objPage(b, "docs/a.txt"), "storageclass", values); code != http.StatusOK {
		t.Fatalf("console Edit storage class = %d: %s", code, body)
	}
	data, rewritten := read(src, "docs/a.txt")
	if data != "alpha" || rewritten.StorageClass != "COLDLINE" || rewritten.Generation == edited.Generation ||
		rewritten.Metadata["stage"] != "edited" {
		t.Errorf("after the rewrite: %q, class %s, generation %d (was %d), metadata %v",
			data, rewritten.StorageClass, rewritten.Generation, edited.Generation, rewritten.Metadata)
	}

	// Compose, on the objects selected in the docs folder.
	_, selects := detail(b, "docs")
	values = form(selects, "compose")
	values["sources"] = "docs/b.txt\ndocs/b.txt"
	values["destination"] = "docs/joined.txt"
	if code, body := act([]string{b, "docs"}, "compose", values); code != http.StatusOK {
		t.Fatalf("console Compose = %d: %s", code, body)
	}
	data, joined := read(src, "docs/joined.txt")
	if data != "betabeta" || joined.ComponentCount != 2 {
		t.Errorf("the composed object reads %q with %d components", data, joined.ComponentCount)
	}
	code, body = act([]string{b, "docs"}, "compose", values)
	refused412("Compose onto an existing name", code, body)
	values["sources"], values["replace"] = "docs/b.txt", "true"
	if code, body := act([]string{b, "docs"}, "compose", values); code != http.StatusOK {
		t.Fatalf("console Compose with Replace = %d: %s", code, body)
	}
	if data, again := read(src, "docs/joined.txt"); data != "beta" || again.Generation == joined.Generation {
		t.Errorf("Compose with Replace reads %q at generation %d (was %d)", data, again.Generation, joined.Generation)
	}

	// An action the page does not offer is refused by the route.
	if code, body := act([]string{b, "docs"}, "copy", map[string]string{}); code != http.StatusBadRequest ||
		!strings.Contains(body, "not available") {
		t.Errorf("Copy on a folder = %d %s; want it refused as not offered", code, body)
	}
}
