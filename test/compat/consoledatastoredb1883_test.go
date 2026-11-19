//go:build compat

package compat

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/datastore"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
)

// TestConsoleDatastoreWritesTheChosenDatabase (#1883): the Datastore
// page's database picker lists the project's Datastore mode databases, and
// with ?database= its Create entity addresses that database, which the official client opened on it reads
// back; (default) is untouched.
func TestConsoleDatastoreWritesTheChosenDatabase(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	dsAddr := h.Endpoint(EnvDatastore)
	ctx := h.Context()
	project := h.Project()
	id := uniqueDB("cdb")
	if _, err := createDB(t, h, id, &adminpb.Database{LocationId: "nam5", Type: adminpb.Database_DATASTORE_MODE}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATASTORE_EMULATOR_HOST", dsAddr)
	dc, err := datastore.NewClientWithDatabase(ctx, project, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dc.Close() })
	def, err := datastore.NewClient(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = def.Close() })
	q := "?project=" + project + "&database=" + id

	code, out := consoleDo(t, addr, http.MethodGet, "/api/resources/datastore"+q, "")
	if code != http.StatusOK || !strings.Contains(out, `"`+id+`"`) {
		t.Fatalf("the Datastore page with %s = %d: %s", id, code, out)
	}
	body, _ := json.Marshal(map[string]string{"kind": "Picked", "key": "p1"})
	if code, out := consoleDo(t, addr, http.MethodPost, "/api/resources/datastore"+q, string(body)); code != http.StatusOK {
		t.Fatalf("Create entity in %s = %d: %s", id, code, out)
	}
	key := datastore.NameKey("Picked", "p1", nil)
	var props datastore.PropertyList
	if err := dc.Get(ctx, key, &props); err != nil {
		t.Fatalf("the entity created in %s: %v", id, err)
	}
	if err := def.Get(ctx, key, &props); !errors.Is(err, datastore.ErrNoSuchEntity) {
		t.Errorf("(default) holds the entity created in %s: %v", id, err)
	}
}
