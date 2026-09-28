package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/console"
	gcsbuiltin "github.com/cloudburrow/cloudburrow/internal/service/storage"
)

// storageSettingsConsole is the console's Settings screen over an in-process
// builtin storage server whose calls feed the Request Log, and the official
// client against the same server.
func storageSettingsConsole(t *testing.T) (*httptest.Server, *storage.Client) {
	t.Helper()
	rec := admin.NewRecorder(1000, time.Now)
	gcs, err := gcsbuiltin.NewServer(gcsbuiltin.Options{Observe: storageEvents(rec, nil)})
	if err != nil {
		t.Fatal(err)
	}
	backend := httptest.NewServer(gcs)
	t.Cleanup(backend.Close)
	endpoint := strings.TrimPrefix(backend.URL, "http://")
	srv := console.New("127.0.0.1:0", nil, storageSettingsProvider{endpoint: endpoint})
	srv.SetRequests(&consoleRequests{rec: rec, unmeasured: func() []string { return nil }})
	h := httptest.NewServer(srv.Handler())
	t.Cleanup(h.Close)
	c, err := storageProvider{endpoint: endpoint}.storageClient(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return h, c
}

func consoleCall(t *testing.T, srv *httptest.Server, method, path, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// hmacRow is one row of the Settings screen, with the actions its menu offers.
type hmacRow struct {
	Name, Status string
	Fields       map[string]string
	Actions      []console.Action
}

func actionIDs(actions []console.Action) string {
	var ids []string
	for _, a := range actions {
		ids = append(ids, a.ID)
	}
	return strings.Join(ids, ",")
}

// TestStorageSettingsHMACKeysThroughTheConsole (#792): the Settings screen
// names the project's service account from serviceAccount.get. Create key
// answers with the key's 40-character secret, and the key is listed by the
// official client's ListHMACKeys as ACTIVE. Its row offers Deactivate and no
// Delete; Deactivate makes the client read INACTIVE, and the row then offers
// Activate and Delete; a delete through the route while the key is ACTIVE
// is refused before the API is asked. Delete removes it from the client's
// list. The secret is in the create response and in nothing read after it:
// the listing, the operations ledger, the logs, the Request Log and every
// action's response.
func TestStorageSettingsHMACKeysThroughTheConsole(t *testing.T) {
	t.Parallel()
	srv, c := storageSettingsConsole(t)
	ctx := context.Background()
	const project = "hmac-p"
	sa := "app@" + project + ".iam.gserviceaccount.com"
	var later []string // every response read after the create

	list := func() (console.Listing, []hmacRow) {
		t.Helper()
		code, body := consoleCall(t, srv, http.MethodGet, "/api/resources/storage-settings?project="+project, "")
		later = append(later, body)
		var l console.Listing
		var rows struct{ Items []hmacRow }
		if err := json.Unmarshal([]byte(body), &l); code != http.StatusOK || err != nil || l.Unavailable != "" {
			t.Fatalf("list = %d (%v): %s", code, err, body)
		}
		_ = json.Unmarshal([]byte(body), &rows)
		return l, rows.Items
	}
	act := func(name, action string) (int, string) {
		t.Helper()
		b, _ := json.Marshal(map[string]string{"Name": name, "Action": action})
		code, body := consoleCall(t, srv, http.MethodPost, "/api/actions/storage-settings?project="+project, string(b))
		later = append(later, body)
		return code, body
	}
	sdkState := func(id string) string {
		t.Helper()
		k, err := c.HMACKeyHandle(project, id).Get(ctx)
		if err != nil {
			t.Fatalf("the official client's Get(%s): %v", id, err)
		}
		if k.Secret != "" {
			t.Errorf("Get returns a secret after the create")
		}
		return string(k.State)
	}

	l, rows := list()
	if len(rows) != 0 {
		t.Fatalf("a new project lists keys: %v", rows)
	}
	want, err := c.ServiceAccount(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Summary) != 1 || l.Summary[0].Properties[1].Value != want {
		t.Errorf("the screen's summary is %+v; want the service account %s", l.Summary, want)
	}

	code, body := consoleCall(t, srv, http.MethodPost, "/api/resources/storage-settings?project="+project,
		`{"serviceAccountEmail":"`+sa+`"}`)
	var created struct {
		Name    string
		OneTime *console.OneTime
	}
	if err := json.Unmarshal([]byte(body), &created); code != http.StatusOK || err != nil || created.OneTime == nil {
		t.Fatalf("Create key = %d (%v): %s", code, err, body)
	}
	secret, id := created.OneTime.Value, created.Name
	if len(secret) != 40 || len(id) != 61 || created.OneTime.Label != "Secret" ||
		!strings.Contains(created.OneTime.Note, "won't see this secret again") {
		t.Errorf("Create key answered access ID %q and one-time %+v", id, created.OneTime)
	}

	it := c.ListHMACKeys(ctx, project)
	found := false
	for {
		k, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		found = found || (k.AccessID == id && k.ServiceAccountEmail == sa && k.State == storage.Active)
	}
	if !found {
		t.Fatalf("ListHMACKeys does not list the console's key %s as ACTIVE", id)
	}

	_, rows = list()
	if len(rows) != 1 || rows[0].Name != id || rows[0].Status != "ACTIVE" || rows[0].Fields["Service account"] != sa {
		t.Fatalf("the screen lists %+v", rows)
	}
	if got := actionIDs(rows[0].Actions); got != "deactivate" {
		t.Errorf("an ACTIVE key's row offers %q; want Deactivate and no Delete", got)
	}
	if code, body := act(id, "delete"); code != http.StatusBadRequest || !strings.Contains(body, "not available on a key that is ACTIVE") {
		t.Errorf("delete of an ACTIVE key through the route = %d %s; want it refused", code, body)
	}
	if st := sdkState(id); st != "ACTIVE" {
		t.Fatalf("after the refused delete the client reads %s", st)
	}

	if code, body := act(id, "deactivate"); code != http.StatusOK {
		t.Fatalf("Deactivate = %d %s", code, body)
	}
	if st := sdkState(id); st != "INACTIVE" {
		t.Errorf("after Deactivate the client reads %s", st)
	}
	_, rows = list()
	if len(rows) != 1 || rows[0].Status != "INACTIVE" || actionIDs(rows[0].Actions) != "activate,delete" {
		t.Errorf("an INACTIVE key's row = %+v; want Activate and Delete", rows)
	}
	if code, body := act(id, "activate"); code != http.StatusOK || sdkState(id) != "ACTIVE" {
		t.Errorf("Activate = %d %s", code, body)
	}
	if code, body := act(id, "deactivate"); code != http.StatusOK {
		t.Fatalf("Deactivate = %d %s", code, body)
	}
	if code, body := act(id, "delete"); code != http.StatusOK {
		t.Fatalf("Delete = %d %s", code, body)
	}
	if st := sdkState(id); st != "DELETED" {
		t.Errorf("after Delete the client reads %s", st)
	}
	if _, rows = list(); len(rows) != 0 {
		t.Errorf("a deleted key is still listed: %+v", rows)
	}

	for _, path := range []string{"/api/operations?project=" + project, "/api/logs", "/api/requests"} {
		code, body := consoleCall(t, srv, http.MethodGet, path, "")
		if code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, code)
		}
		if !strings.Contains(body, id) && path != "/api/requests" {
			t.Errorf("%s does not name the key created: %s", path, body)
		}
		later = append(later, body)
	}
	if code, body := consoleCall(t, srv, http.MethodGet, "/api/requests", ""); code != http.StatusOK ||
		!strings.Contains(body, "storage.projects.hmacKeys.create") {
		t.Errorf("the Request Log does not show the create: %s", body)
	}
	for _, body := range later {
		if strings.Contains(body, secret) {
			t.Errorf("a response after the create holds the secret: %s", body)
		}
	}
}

// Create key refuses what the API refuses, with the API's message, and asks
// for a project first.
func TestStorageSettingsCreateRefusals(t *testing.T) {
	t.Parallel()
	srv, _ := storageSettingsConsole(t)
	code, body := consoleCall(t, srv, http.MethodPost, "/api/resources/storage-settings?project=p",
		`{"serviceAccountEmail":"not-an-email"}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "is not an email address") {
		t.Errorf("a create for a non-email = %d %s; want the API's refusal", code, body)
	}
	if code, body := consoleCall(t, srv, http.MethodPost, "/api/resources/storage-settings",
		`{"serviceAccountEmail":"a@p.iam.gserviceaccount.com"}`); code != http.StatusBadRequest || !strings.Contains(body, "choose a project") {
		t.Errorf("a create with no project = %d %s", code, body)
	}
	var l console.Listing
	code, body = consoleCall(t, srv, http.MethodGet, "/api/resources/storage-settings", "")
	if err := json.Unmarshal([]byte(body), &l); code != http.StatusOK || err != nil || l.Prompt == "" {
		t.Errorf("the screen with no project = %d %s; want a prompt", code, body)
	}
}
