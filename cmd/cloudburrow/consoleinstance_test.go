package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// queueBook is a component for the Instance page's tests: queue names held
// in memory, reset by project, seeded, and saved and loaded through the
// state archive, as Cloud Tasks is.
type queueBook struct {
	mu     sync.Mutex
	queues map[string]bool
}

func (b *queueBook) Name() string { return "tasks" }
func (b *queueBook) Reset(context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.queues = map[string]bool{}
	return nil
}
func (b *queueBook) ResetProject(_ context.Context, project string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for q := range b.queues {
		if strings.HasPrefix(q, "projects/"+project+"/") {
			delete(b.queues, q)
		}
	}
	return nil
}
func (b *queueBook) Seed(_ context.Context, spec json.RawMessage) error {
	var s struct {
		IfNotExists bool     `json:"ifNotExists"`
		Queues      []string `json:"queues"`
	}
	if err := json.Unmarshal(spec, &s); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, q := range s.Queues {
		if b.queues[q] {
			if s.IfNotExists {
				continue
			}
			return status.Errorf(codes.AlreadyExists, "queue %s already exists; set ifNotExists to skip it", q)
		}
		b.queues[q] = true
	}
	return nil
}
func (b *queueBook) Secret() bool { return true }
func (b *queueBook) Export(_ context.Context, w admin.EntryWriter) error {
	data, _ := json.Marshal(b.names())
	return w.Add("queues.json", int64(len(data)), bytes.NewReader(data))
}
func (b *queueBook) Import(_ context.Context, r admin.EntryReader) error {
	f, err := r.Open("queues.json")
	if err != nil {
		return err
	}
	defer f.Close()
	var names []string
	if err := json.NewDecoder(f).Decode(&names); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.queues = map[string]bool{}
	for _, q := range names {
		b.queues[q] = true
	}
	return nil
}
func (b *queueBook) names() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := []string{}
	for q := range b.queues {
		out = append(out, q)
	}
	sort.Strings(out)
	return out
}

// unscopedResetter cannot be confined to a project.
type unscopedResetter struct{}

func (unscopedResetter) Name() string                { return "logging" }
func (unscopedResetter) Reset(context.Context) error { return nil }

const instanceTestToken = "console-instance-test-token"

// instanceConsole is a console attached, as up attaches it (#801), to an
// admin API that requires token, with one queue in each of two projects.
func instanceConsole(t *testing.T, token string) (*admin.API, *queueBook, *httptest.Server) {
	t.Helper()
	api := admin.NewAPI(admin.NewRecorder(100, nil))
	api.RequireToken(token)
	book := &queueBook{queues: map[string]bool{
		"projects/alpha/locations/us-central1/queues/a": true,
		"projects/beta/locations/us-central1/queues/b":  true,
	}}
	api.RegisterResetter(book, unscopedResetter{})
	api.RegisterSeeder(book)
	api.RegisterSnapshotter(book)
	api.RegisterNotCaptured("kms", "key material is not exported")
	api.SetStateProducer("test", "demo")
	src := newConsoleInstance(config.Config{Name: "demo"}, api)
	src.dir = t.TempDir()
	c := console.New("127.0.0.1:0", nil)
	c.SetInstance(src, "/state/demo/admin-token")
	srv := httptest.NewServer(c.Handler())
	t.Cleanup(srv.Close)
	return api, book, srv
}

// instanceRequest sends one request to the console's Instance endpoints,
// with token as the page sends it when set, then headers.
func instanceRequest(t *testing.T, srv *httptest.Server, method, path, contentType string, body []byte, token string, headers map[string]string) (int, http.Header, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b
}

// savedArchive is an archive saved through the console with the token.
func savedArchive(t *testing.T, srv *httptest.Server) []byte {
	t.Helper()
	code, h, b := instanceRequest(t, srv, http.MethodPost, "/api/instance/save", "", nil, instanceTestToken, nil)
	if code != http.StatusOK || h.Get("Content-Type") != "application/gzip" ||
		!strings.Contains(h.Get("Content-Disposition"), `filename="cloudburrow-state-demo-`) {
		t.Fatalf("POST /api/instance/save = %d %v %.200s", code, h, b)
	}
	return b
}

// operationCount is how many entries the console's operations ledger holds.
func operationCount(t *testing.T, srv *httptest.Server) (int, string) {
	t.Helper()
	_, _, b := instanceRequest(t, srv, http.MethodGet, "/api/operations", "", nil, "", nil)
	var ops struct{ Operations []console.Operation }
	if err := json.Unmarshal(b, &ops); err != nil {
		t.Fatal(err)
	}
	return len(ops.Operations), string(b)
}

// instanceActions are every Instance endpoint, each with a body that would
// change something if it were accepted.
func instanceActions(t *testing.T, archive []byte) []struct {
	method, path, contentType string
	body                      []byte
} {
	return []struct {
		method, path, contentType string
		body                      []byte
	}{
		{http.MethodGet, "/api/instance", "", nil},
		{http.MethodPost, "/api/instance/save", "", nil},
		{http.MethodPost, "/api/instance/load", "application/gzip", archive},
		{http.MethodPost, "/api/instance/reset", "", nil},
		{http.MethodPost, "/api/instance/reset?service=tasks&project=alpha", "", nil},
		{http.MethodPost, "/api/instance/seed", "application/json",
			[]byte(`{"components":{"tasks":{"queues":["projects/gamma/locations/us-central1/queues/g"]}}}`)},
	}
}

// TestConsoleInstanceRefusesARequestWithoutTheAdminToken (#801, #553): the
// Instance page's endpoints add no token of their own, so without the
// instance's admin token each is refused with the admin API's 401, naming the
// token file and never carrying the token, and changes nothing and records
// nothing in the operations ledger: also a request shaped like a workload's
// (Host host.docker.internal, no Origin, no fetch metadata), one carrying
// same-origin headers, and one with a wrong token.
func TestConsoleInstanceRefusesARequestWithoutTheAdminToken(t *testing.T) {
	_, book, srv := instanceConsole(t, instanceTestToken)
	archive := savedArchive(t, srv)
	if _, _, b := instanceRequest(t, srv, http.MethodPost, "/api/instance/reset", "", nil, instanceTestToken, nil); !strings.Contains(string(b), `"reset":["tasks","logging"]`) {
		t.Fatalf("a reset with the token = %s", b)
	}
	before, _ := operationCount(t, srv)
	for _, c := range []struct {
		name    string
		token   string
		headers map[string]string
	}{
		{"no token", "", nil},
		{"a workload on Docker Desktop", "", map[string]string{"Host": "host.docker.internal:9090"}},
		{"same-origin headers", "", map[string]string{"Host": "127.0.0.1:9090", "Origin": "http://127.0.0.1:9090", "Sec-Fetch-Site": "same-origin"}},
		{"a wrong token", "not-the-token", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, r := range instanceActions(t, archive) {
				code, _, b := instanceRequest(t, srv, r.method, r.path, r.contentType, r.body, c.token, c.headers)
				body := string(b)
				if code != http.StatusUnauthorized || !strings.Contains(body, "admin token") ||
					!strings.Contains(body, `"token_file":"/state/demo/admin-token"`) {
					t.Errorf("%s %s = %d %s; want the admin API's 401, naming the token file", r.method, r.path, code, body)
				}
				if strings.Contains(body, instanceTestToken) {
					t.Errorf("%s %s: the refusal carries the token", r.method, r.path)
				}
			}
			if q := book.names(); len(q) != 0 {
				t.Errorf("refused requests left the queues %q; the reset with the token had emptied them", q)
			}
			if n, ops := operationCount(t, srv); n != before {
				t.Errorf("refused requests changed the ledger from %d entries: %s", before, ops)
			}
		})
	}
}

// TestConsoleInstanceRefusesACrossOriginRequest: the console's same-origin
// and Host guards apply to the Instance endpoints as to the rest of /api, so
// a page on another site cannot reset, load or seed even with the token.
func TestConsoleInstanceRefusesACrossOriginRequest(t *testing.T) {
	_, book, srv := instanceConsole(t, instanceTestToken)
	archive := savedArchive(t, srv)
	want := book.names()
	for _, c := range []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"cross-site fetch metadata", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"another origin", map[string]string{"Origin": "http://attacker.example"}, http.StatusForbidden},
		{"a rebound host", map[string]string{"Host": "attacker.example:9090", "Origin": "http://attacker.example:9090",
			"Sec-Fetch-Site": "same-origin"}, http.StatusMisdirectedRequest},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, r := range instanceActions(t, archive) {
				if code, _, b := instanceRequest(t, srv, r.method, r.path, r.contentType, r.body, instanceTestToken, c.headers); code != c.want {
					t.Errorf("%s %s = %d %.200s, want %d", r.method, r.path, code, b, c.want)
				}
			}
		})
	}
	if got := book.names(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("cross-origin requests changed the queues from %q to %q", want, got)
	}
}

// TestConsoleInstanceActsThroughTheAdminAPI (#801): with the token, the page
// reads what reset, seed and the archive accept from GET /admin/instance; a
// seed sent twice with If not exists creates each queue once, and without it
// the second is refused with the admin API's conflict; Save downloads the
// archive `state save` writes; a project-scoped reset leaves the other project
// alone, one naming a component that cannot be scoped is refused with the
// admin API's message and resets nothing, and reseed with no startup seed is
// refused; Load restores what was saved; and each action is in the operations
// ledger with its outcome, never its payload.
func TestConsoleInstanceActsThroughTheAdminAPI(t *testing.T) {
	api, book, srv := instanceConsole(t, instanceTestToken)
	do := func(method, path, contentType string, body []byte) (int, string) {
		t.Helper()
		code, _, b := instanceRequest(t, srv, method, path, contentType, body, instanceTestToken, nil)
		return code, string(b)
	}

	code, body := do(http.MethodGet, "/api/instance", "", nil)
	var info admin.InstanceInfo
	if err := json.Unmarshal([]byte(body), &info); err != nil || code != http.StatusOK {
		t.Fatalf("GET /api/instance = %d %s", code, body)
	}
	if len(info.Reset) != 2 || info.Reset[0] != (admin.ResetTarget{Name: "tasks", ByProject: true}) || info.Reset[1].ByProject ||
		strings.Join(info.Seed, ",") != "tasks" || len(info.Reseed) != 0 || len(info.State.Services) != 2 || !info.State.ContainsSecretValues {
		t.Errorf("GET /api/instance = %+v", info)
	}

	const secretQueue = "projects/gamma/locations/us-central1/queues/seeded-secret-name"
	seed := []byte(`{"components":{"tasks":{"queues":["` + secretQueue + `"]}}}`)
	for i := 0; i < 2; i++ {
		if code, body := do(http.MethodPost, "/api/instance/seed?ifNotExists=true", "application/json", seed); code != http.StatusOK ||
			!strings.Contains(body, `"seeded":["tasks"]`) || !strings.Contains(body, `"operation":"op-`) {
			t.Fatalf("seed %d with If not exists = %d %s", i+1, code, body)
		}
	}
	if got := book.names(); len(got) != 3 {
		t.Errorf("after two seeds with If not exists the queues are %q; want the seeded one once beside the two", got)
	}
	if code, body := do(http.MethodPost, "/api/instance/seed", "application/json", seed); code != http.StatusConflict ||
		!strings.Contains(body, "already exists; set ifNotExists to skip it") {
		t.Errorf("a second seed without If not exists = %d %s; want the admin API's 409", code, body)
	}

	archive := savedArchive(t, srv)
	checkSameArchive(t, api, archive)

	if code, body := do(http.MethodPost, "/api/instance/reset?service=tasks&service=logging&project=alpha", "", nil); code != http.StatusBadRequest ||
		!strings.Contains(body, "cannot be reset by project") || !strings.Contains(body, `"cannot_scope_by_project":["logging"]`) {
		t.Errorf("a project reset naming logging = %d %s; want the admin API's refusal", code, body)
	}
	if code, body := do(http.MethodPost, "/api/instance/reset?reseed=true", "", nil); code != http.StatusBadRequest ||
		!strings.Contains(body, "up was not given a seed file") {
		t.Errorf("reseed with no startup seed = %d %s", code, body)
	}
	if got := book.names(); len(got) != 3 {
		t.Fatalf("refused resets changed the queues to %q", got)
	}
	if code, body := do(http.MethodPost, "/api/instance/reset?service=tasks&project=alpha", "", nil); code != http.StatusOK ||
		!strings.Contains(body, `"reset":["tasks"]`) {
		t.Fatalf("reset tasks in project alpha = %d %s", code, body)
	}
	if got := strings.Join(book.names(), ","); got != "projects/beta/locations/us-central1/queues/b,"+secretQueue {
		t.Errorf("after a reset of project alpha the queues are %s; want the other projects' left alone", got)
	}
	if code, body := do(http.MethodPost, "/api/instance/reset", "", nil); code != http.StatusOK || len(book.names()) != 0 {
		t.Fatalf("reset every service = %d %s, left %q", code, body, book.names())
	}

	if code, body := do(http.MethodPost, "/api/instance/load", "application/gzip", []byte("not an archive")); code != http.StatusBadRequest ||
		!strings.Contains(body, "not a state archive") || !strings.Contains(body, "nothing was loaded") {
		t.Errorf("load a file that is not an archive = %d %s; want the admin API's refusal", code, body)
	}
	if code, body := do(http.MethodPost, "/api/instance/load", "application/gzip", archive); code != http.StatusOK ||
		!strings.Contains(body, `"loaded":["tasks"]`) || !strings.Contains(body, `"name":"kms"`) {
		t.Fatalf("load the saved archive = %d %s", code, body)
	}
	if got := book.names(); len(got) != 3 || got[2] != secretQueue {
		t.Errorf("after the load the queues are %q; want the three saved", got)
	}

	_, ops := operationCount(t, srv)
	var ledger struct{ Operations []console.Operation }
	_ = json.Unmarshal([]byte(ops), &ledger)
	kinds := map[string][]console.OperationState{}
	for _, o := range ledger.Operations {
		kinds[o.Kind] = append(kinds[o.Kind], o.State)
	}
	for kind, want := range map[string]int{"Seed": 3, "Save state": 1, "Reset": 4, "Load state": 2} {
		if len(kinds[kind]) != want {
			t.Errorf("the ledger holds %d %q operations, want %d: %s", len(kinds[kind]), kind, want, ops)
		}
	}
	if !strings.Contains(ops, `"state":"FAILED"`) || !strings.Contains(ops, "already exists") || !strings.Contains(ops, `"project":"alpha"`) {
		t.Errorf("the ledger does not carry the outcomes: %s", ops)
	}
	// The outcome only: a success names the components, a failure carries
	// the admin API's message; the document and the archive are not kept.
	for _, o := range ledger.Operations {
		if o.State == console.OperationSucceeded && strings.Contains(o.Resource+o.Error, "seeded-secret-name") {
			t.Errorf("a successful %s names what the seed created: %+v", o.Kind, o)
		}
	}
	if strings.Contains(ops, "components") || strings.Contains(ops, "queues.json") {
		t.Errorf("the ledger carries a seed document or an archive's content: %s", ops)
	}
}

// checkSameArchive: the console's Save gives the archive `state save`
// writes from the same admin API: the same entries with the same bytes, and a
// manifest that differs only in when it was made.
func checkSameArchive(t *testing.T, api *admin.API, fromConsole []byte) {
	t.Helper()
	mux := http.NewServeMux()
	api.Routes(mux)
	ctl := httptest.NewServer(mux)
	defer ctl.Close()
	dir := t.TempDir()
	cfg := config.Config{Name: "demo", StateDir: dir}
	if err := os.MkdirAll(cfg.InstanceDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(adminTokenPath(cfg), []byte(instanceTestToken), 0o600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "saved.tar.gz")
	var out bytes.Buffer
	if err := saveState(cfg, ctl.URL+"/admin/state/export", file, &out); err != nil {
		t.Fatalf("state save: %v", err)
	}
	fromCLI, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	a, b := archiveEntries(t, fromCLI), archiveEntries(t, fromConsole)
	if len(a) != len(b) || len(a) < 2 {
		t.Fatalf("state save wrote %d entries, the console gave %d", len(a), len(b))
	}
	for i := range a {
		if a[i].name != b[i].name {
			t.Errorf("entry %d: state save %s, the console %s", i, a[i].name, b[i].name)
			continue
		}
		if a[i].name == "manifest.json" {
			var ma, mb admin.Manifest
			_ = json.Unmarshal(a[i].data, &ma)
			_ = json.Unmarshal(b[i].data, &mb)
			ma.Created = mb.Created
			ja, _ := json.Marshal(ma)
			jb, _ := json.Marshal(mb)
			if !bytes.Equal(ja, jb) {
				t.Errorf("manifests differ: state save %s, the console %s", ja, jb)
			}
			continue
		}
		if !bytes.Equal(a[i].data, b[i].data) {
			t.Errorf("%s differs: state save %q, the console %q", a[i].name, a[i].data, b[i].data)
		}
	}
}

type archiveEntry struct {
	name string
	data []byte
}

func archiveEntries(t *testing.T, archive []byte) []archiveEntry {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("not gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	var out []archiveEntry
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("unreadable archive: %v", err)
		}
		b, _ := io.ReadAll(tr)
		out = append(out, archiveEntry{hdr.Name, b})
	}
}

// TestConsoleWithoutAdminOffersNoInstancePage: a console not attached to
// an instance's admin API says so.
func TestConsoleWithoutAdminOffersNoInstancePage(t *testing.T) {
	srv := httptest.NewServer(console.New("127.0.0.1:0", nil).Handler())
	defer srv.Close()
	if code, _, b := instanceRequest(t, srv, http.MethodGet, "/api/instance", "", nil, "", nil); code != http.StatusServiceUnavailable ||
		!strings.Contains(string(b), "not available") {
		t.Errorf("GET /api/instance on a console with no admin API = %d %s", code, b)
	}
}
