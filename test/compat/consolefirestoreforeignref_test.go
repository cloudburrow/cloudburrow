//go:build compat

package compat

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"cloud.google.com/go/firestore"
)

// TestConsoleFirestoreEditFieldRefusesAForeignReference (#1050).
//
// A top-level field holding a reference to a document in another project,
// written with the official client, is offered no Edit field on its page
// (#995), and the Edit field route refuses it too when the PATCH is posted
// by hand, with the note the page shows: the form writes a reference into
// this project's default database, which would change the document it
// names. The client reads the field back unchanged. A reference into this
// project's default database is still edited through the same route.
//
// covers: google.firestore.v1.Firestore/Commit
func TestConsoleFirestoreEditFieldRefusesAForeignReference(t *testing.T) {
	h := New(t)
	t.Setenv("FIRESTORE_EMULATOR_HOST", h.Endpoint(EnvFirestore))
	addr := consoleAddr(t, h)
	ctx := h.Context()
	project := h.Project()
	c, err := firestore.NewClient(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	other, err := firestore.NewClient(ctx, project+"-other")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()

	const coll = "console-foreign-refs"
	doc := c.Collection(coll).Doc("holder")
	t.Cleanup(func() { _, _ = doc.Delete(ctx) })
	foreign := other.Doc("users/alice")
	if _, err := doc.Set(ctx, map[string]any{"elsewhere": foreign, "local": c.Doc("users/bob")}); err != nil {
		t.Fatalf("write the references: %v", err)
	}

	q := url.Values{"project": {project}, "name": {coll, "holder", "elsewhere"}}
	var page struct {
		Unavailable string
		Edit        json.RawMessage
	}
	consoleJSON(t, addr, http.MethodGet, "/api/detail/firestore?"+q.Encode(), "", &page)
	if page.Unavailable != "" || len(page.Edit) > 0 && string(page.Edit) != "null" {
		t.Errorf("the foreign reference's page is unavailable (%q) or offers Edit field %s", page.Unavailable, page.Edit)
	}

	code, out := consoleEdit(t, addr, "firestore", project, []string{coll, "holder", "elsewhere"},
		map[string]string{"type": "reference", "value": "users/alice"})
	if msg := consoleError(t, out); code != http.StatusBadRequest || !strings.Contains(msg, foreign.Path) ||
		!strings.Contains(msg, "another project or database") {
		t.Errorf("Edit field on a foreign reference posted by hand = %d %q, want the page's note", code, msg)
	}
	snap, err := doc.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ref, ok := snap.Data()["elsewhere"].(*firestore.DocumentRef); !ok || ref.Path != foreign.Path {
		t.Errorf("after the refused edit the field reads %#v, want %s unchanged", snap.Data()["elsewhere"], foreign.Path)
	}

	if code, out := consoleEdit(t, addr, "firestore", project, []string{coll, "holder", "local"},
		map[string]string{"type": "reference", "value": "users/carol"}); code != http.StatusOK {
		t.Fatalf("Edit field on a local reference = %d: %s", code, out)
	}
	snap, err = doc.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ref, ok := snap.Data()["local"].(*firestore.DocumentRef); !ok || ref.Path != c.Doc("users/carol").Path {
		t.Errorf("the local reference reads %#v after its edit, want users/carol", snap.Data()["local"])
	}
}
