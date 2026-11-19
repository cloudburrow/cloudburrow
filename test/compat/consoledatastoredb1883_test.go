//go:build compat

package compat

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"cloud.google.com/go/datastore"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
)

// TestConsoleDatastoreWritesTheChosenDatabase (#1883): the Datastore
// page's database picker lists the project's Datastore mode databases, and
// with ?database= its Create entity addresses that database, which the
// official client opened on it reads back; (default) is untouched. The
// kind's entity list and the entity's own page in that database show the
// entity, and (default)'s kind list does not (#1885).
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

	detail := func(database string, path ...string) string {
		t.Helper()
		v := url.Values{"project": {project}, "name": path}
		if database != "" {
			v.Set("database", database)
		}
		code, out := consoleDo(t, addr, http.MethodGet, "/api/detail/datastore?"+v.Encode(), "")
		if code != http.StatusOK {
			t.Fatalf("the Datastore page %v in %q = %d: %s", path, database, code, out)
		}
		return out
	}
	var kindPage struct {
		Sections []struct {
			Listing struct {
				Items []struct {
					Name  string
					Opens []string
				}
			}
		}
	}
	if err := json.Unmarshal([]byte(detail(id, "Picked")), &kindPage); err != nil {
		t.Fatal(err)
	}
	var opens []string
	for _, s := range kindPage.Sections {
		for _, it := range s.Listing.Items {
			if it.Name == "name=p1" {
				opens = it.Opens
			}
		}
	}
	if opens == nil {
		t.Fatalf("the kind's entity list in %s does not list name=p1: %+v", id, kindPage)
	}
	var entityPage struct {
		Title, Unavailable string
		Summary            []struct{ Label, Value string }
	}
	if err := json.Unmarshal([]byte(detail(id, opens...)), &entityPage); err != nil {
		t.Fatal(err)
	}
	if entityPage.Unavailable != "" || !strings.Contains(fmt.Sprint(entityPage), "p1") {
		t.Errorf("the entity's page in %s: %+v", id, entityPage)
	}
	if out := detail("", "Picked"); strings.Contains(out, `"name=p1"`) {
		t.Errorf("(default)'s kind list shows %s's entity: %s", id, out)
	}
}
