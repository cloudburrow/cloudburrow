//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/storage"
)

// TestConsoleStorageHMACKeys (#792): the console's Cloud Storage Settings
// screen, through its own routes as the page calls them, read back through
// the official storage client. The screen names the service account
// ServiceAccount returns. A key made with Create key is listed by
// ListHMACKeys, ACTIVE, for the service account the form named, and its
// secret, 40 characters, is in the create response only: the client's Get
// has none, and neither the screen's listing, the operations ledger, the
// logs, the Request Log nor any action's response holds it. The ACTIVE key's
// row offers no Delete, and a delete sent anyway is refused and changes
// nothing; Deactivate makes the client read INACTIVE, the row then offers
// Delete, and Delete makes the client read DELETED.
func TestConsoleStorageHMACKeys(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	sc := storageClient(t, h)
	ctx := h.Context()
	project := h.Project()
	sa := "console-hmac@" + project + ".iam.gserviceaccount.com"
	t.Cleanup(func() { deleteHMACKeys(context.Background(), sc, project, sa) })
	var later []string

	type row struct {
		Name, Status string
		Fields       map[string]string
		Actions      []struct{ ID string }
	}
	list := func() (summary string, rows []row) {
		t.Helper()
		code, body := consoleDo(t, addr, http.MethodGet, "/api/resources/storage-settings?project="+project, "")
		later = append(later, body)
		var l struct {
			Unavailable string
			Summary     []struct {
				Properties []struct{ Label, Value string }
			}
			Items []row
		}
		if err := json.Unmarshal([]byte(body), &l); code != http.StatusOK || err != nil || l.Unavailable != "" {
			t.Fatalf("the Settings listing = %d (%v): %s", code, err, body)
		}
		for _, g := range l.Summary {
			for _, p := range g.Properties {
				summary += p.Label + "=" + p.Value + ";"
			}
		}
		for _, r := range l.Items {
			if r.Fields["Service account"] == sa {
				rows = append(rows, r)
			}
		}
		return summary, rows
	}
	offers := func(r row, id string) bool {
		for _, a := range r.Actions {
			if a.ID == id {
				return true
			}
		}
		return false
	}
	act := func(name, action string) (int, string) {
		t.Helper()
		b, _ := json.Marshal(map[string]string{"Name": name, "Action": action})
		code, body := consoleDo(t, addr, http.MethodPost, "/api/actions/storage-settings?project="+project, string(b))
		later = append(later, body)
		return code, body
	}
	state := func(id string) storage.HMACState {
		t.Helper()
		k, err := sc.HMACKeyHandle(project, id).Get(ctx)
		if err != nil {
			t.Fatalf("Get %s: %v", id, err)
		}
		if k.Secret != "" {
			t.Errorf("the client's Get returns a secret after the create")
		}
		return k.State
	}

	email, err := sc.ServiceAccount(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	if summary, _ := list(); !strings.Contains(summary, "="+email+";") {
		t.Errorf("the Settings screen's summary is %q; want the service account %s", summary, email)
	}

	code, body := consoleDo(t, addr, http.MethodPost, "/api/resources/storage-settings?project="+project,
		`{"serviceAccountEmail":"`+sa+`"}`)
	var created struct {
		Name    string
		OneTime struct{ Label, Value, Note string }
	}
	if err := json.Unmarshal([]byte(body), &created); code != http.StatusOK || err != nil {
		t.Fatalf("Create key = %d (%v): %s", code, err, body)
	}
	id, secret := created.Name, created.OneTime.Value
	if len(secret) != 40 || created.OneTime.Label != "Secret" || created.OneTime.Note == "" {
		t.Fatalf("Create key answered %q with a %d-character secret: %s", id, len(secret), created.OneTime.Note)
	}

	it := sc.ListHMACKeys(ctx, project, storage.ForHMACKeyServiceAccountEmail(sa))
	k, err := it.Next()
	if err != nil || k.AccessID != id || k.State != storage.Active || k.Secret != "" {
		t.Fatalf("ListHMACKeys = %+v, %v; want the console's key %s, ACTIVE, with no secret", k, err, id)
	}

	_, rows := list()
	if len(rows) != 1 || rows[0].Name != id || rows[0].Status != "ACTIVE" {
		t.Fatalf("the screen lists %+v", rows)
	}
	if offers(rows[0], "delete") || !offers(rows[0], "deactivate") {
		t.Errorf("an ACTIVE key's row offers %+v; want Deactivate and no Delete", rows[0].Actions)
	}
	if code, body := act(id, "delete"); code != http.StatusBadRequest {
		t.Errorf("a delete of the ACTIVE key = %d %s; want it refused", code, body)
	}
	if st := state(id); st != storage.Active {
		t.Fatalf("after the refused delete the client reads %s", st)
	}

	if code, body := act(id, "deactivate"); code != http.StatusOK {
		t.Fatalf("Deactivate = %d %s", code, body)
	}
	if st := state(id); st != storage.Inactive {
		t.Errorf("after Deactivate from the console the client reads %s; want INACTIVE", st)
	}
	if _, rows = list(); len(rows) != 1 || rows[0].Status != "INACTIVE" || !offers(rows[0], "delete") {
		t.Errorf("the INACTIVE key's row = %+v; want Delete offered", rows)
	}
	if code, body := act(id, "delete"); code != http.StatusOK {
		t.Fatalf("Delete = %d %s", code, body)
	}
	if st := state(id); st != storage.Deleted {
		t.Errorf("after Delete from the console the client reads %s; want DELETED", st)
	}

	for _, path := range []string{"/api/operations?project=" + project, "/api/logs", "/api/requests"} {
		code, body := consoleDo(t, addr, http.MethodGet, path, "")
		if code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, code)
		}
		later = append(later, body)
	}
	for _, body := range later {
		if strings.Contains(body, secret) {
			t.Errorf("a console response after the create holds the key's secret: %.300s", body)
		}
	}
}
