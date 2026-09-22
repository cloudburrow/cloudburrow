package console

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fakeProvider struct {
	id, title string
	listing   Listing
	err       error
	// seen records the project each call was given, so scoping can be
	// asserted rather than assumed.
	seen *[]string
}

func (f fakeProvider) ID() string    { return f.id }
func (f fakeProvider) Title() string { return f.title }
func (f fakeProvider) List(_ context.Context, project string) (Listing, error) {
	if f.seen != nil {
		*f.seen = append(*f.seen, project)
	}
	return f.listing, f.err
}

func serve(t *testing.T, providers ...Provider) *httptest.Server {
	t.Helper()
	s := New("127.0.0.1:0", func(context.Context) Status {
		return Status{Instance: "test", Ready: true, State: "ready", Cluster: "cloudburrow-test"}
	}, providers...)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, srv *httptest.Server, path string, headers map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// --- assets ------------------------------------------------------------

// The assets ship in the binary. A console that fetched anything from a CDN
// would stop working offline, which is the one environment it exists for.
func TestAssetsAreEmbeddedAndSelfContained(t *testing.T) {
	t.Parallel()
	srv := serve(t)

	for _, path := range []string{"/", "/console.css", "/console.js", "/icon.svg"} {
		code, body := get(t, srv, path, nil)
		if code != http.StatusOK {
			t.Errorf("GET %s = %d", path, code)
			continue
		}
		if body == "" {
			t.Errorf("GET %s returned nothing", path)
		}
	}

	_, html := get(t, srv, "/", nil)
	_, css := get(t, srv, "/console.css", nil)
	_, js := get(t, srv, "/console.js", nil)

	for name, body := range map[string]string{"index.html": html, "console.css": css, "console.js": js} {
		for _, remote := range []string{
			"https://fonts.googleapis.com", "https://fonts.gstatic.com",
			"cdn.jsdelivr.net", "unpkg.com", "cdnjs.cloudflare.com",
			"@import url(http", "https://ajax.googleapis.com",
		} {
			if strings.Contains(body, remote) {
				t.Errorf("%s references %s; the console must work with no network", name, remote)
			}
		}
	}
}

func TestShellCarriesASelfOnlyContentSecurityPolicy(t *testing.T) {
	t.Parallel()
	srv := serve(t)

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	csp := resp.Header.Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("the shell carries no Content-Security-Policy")
	}
	for _, want := range []string{"default-src 'self'", "connect-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("policy is missing %q: %s", want, csp)
		}
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("X-Content-Type-Options is not set")
	}
}

// A deep link opened directly must render, not 404: a route that only works
// after clicking through is not a deep link.
func TestDeepLinksServeTheShell(t *testing.T) {
	t.Parallel()
	srv := serve(t)

	for _, path := range []string{"/storage/browser", "/run", "/pubsub/topics", "/nope"} {
		code, body := get(t, srv, path, nil)
		if code != http.StatusOK {
			t.Errorf("GET %s = %d, want the shell", path, code)
		}
		if !strings.Contains(body, "CloudBurrow") {
			t.Errorf("GET %s did not return the shell", path)
		}
	}
}

// The indicator is what lets someone with both consoles open tell them apart
// without reading the data.
func TestShellShowsCloudBurrowBrandingAndTheLocalIndicator(t *testing.T) {
	t.Parallel()
	srv := serve(t)
	_, html := get(t, srv, "/", nil)

	if !strings.Contains(html, "CloudBurrow") {
		t.Error("the shell does not carry CloudBurrow branding")
	}
	if !strings.Contains(html, "LOCAL") {
		t.Error("the shell has no LOCAL indicator")
	}
	// Google branding must not appear anywhere.
	for _, forbidden := range []string{"Google Cloud", "googleusercontent", "google-logo"} {
		if strings.Contains(html, forbidden) {
			t.Errorf("the shell carries Google branding: %q", forbidden)
		}
	}
}

// --- cross-site protection ---------------------------------------------

// The console binds loopback, but loopback is reachable from any page the
// developer's browser has open. Without this, a visited website could drive
// the API.
func TestCrossSiteRequestsToTheAPIAreRefused(t *testing.T) {
	t.Parallel()
	srv := serve(t)

	for _, site := range []string{"cross-site", "same-site"} {
		code, _ := get(t, srv, "/api/status", map[string]string{"Sec-Fetch-Site": site})
		if code != http.StatusForbidden {
			t.Errorf("Sec-Fetch-Site: %s = %d, want 403", site, code)
		}
	}

	code, _ := get(t, srv, "/api/status", map[string]string{"Sec-Fetch-Site": "same-origin"})
	if code != http.StatusOK {
		t.Errorf("a same-origin request was refused: %d", code)
	}
}

func TestCrossOriginRequestsAreRefusedEvenWithoutFetchMetadata(t *testing.T) {
	t.Parallel()
	srv := serve(t)

	code, _ := get(t, srv, "/api/status", map[string]string{"Origin": "http://evil.example"})
	if code != http.StatusForbidden {
		t.Errorf("a cross-origin request = %d, want 403", code)
	}
}

// The protection must not apply to the assets, or the page could not load.
func TestAssetsAreNotSubjectToTheAPIGuard(t *testing.T) {
	t.Parallel()
	srv := serve(t)

	code, _ := get(t, srv, "/console.css", map[string]string{"Sec-Fetch-Site": "cross-site"})
	if code != http.StatusOK {
		t.Errorf("the stylesheet was refused: %d", code)
	}
}

// --- API ---------------------------------------------------------------

func TestStatusReportsLiveInstanceState(t *testing.T) {
	t.Parallel()
	srv := serve(t)

	code, body := get(t, srv, "/api/status", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	var st Status
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatalf("decode: %v\n%s", err, body)
	}
	if st.Instance != "test" || !st.Ready || st.Cluster != "cloudburrow-test" {
		t.Errorf("status = %+v", st)
	}
}

// A cached resource list shows a developer a bucket they deleted and lets
// them conclude the delete did not work.
func TestAPIResponsesAreNotCached(t *testing.T) {
	t.Parallel()
	srv := serve(t)

	resp, err := http.Get(srv.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

func TestServicesListsOnlyRegisteredProviders(t *testing.T) {
	t.Parallel()
	srv := serve(t,
		fakeProvider{id: "storage", title: "Buckets"},
		fakeProvider{id: "run", title: "Services"},
	)

	code, body := get(t, srv, "/api/services", nil)
	if code != http.StatusOK {
		t.Fatalf("services = %d", code)
	}
	for _, want := range []string{`"storage"`, `"Buckets"`, `"run"`, `"Services"`} {
		if !strings.Contains(body, want) {
			t.Errorf("services response is missing %s: %s", want, body)
		}
	}
	// A service with no provider must not be advertised: the navigation
	// would link to a screen that cannot load.
	if strings.Contains(body, "pubsub") {
		t.Errorf("an unregistered service was advertised: %s", body)
	}
}

func TestResourcesReturnsAProvidersListing(t *testing.T) {
	t.Parallel()
	srv := serve(t, fakeProvider{
		id: "storage", title: "Buckets",
		listing: Listing{
			Columns: []string{"Location"},
			Items: []Resource{
				{Name: "bucket-a", Fields: map[string]string{"Location": "US-CENTRAL1"}},
			},
			Total: 1,
		},
	})

	code, body := get(t, srv, "/api/resources/storage", nil)
	if code != http.StatusOK {
		t.Fatalf("resources = %d: %s", code, body)
	}
	var l Listing
	if err := json.Unmarshal([]byte(body), &l); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(l.Items) != 1 || l.Items[0].Name != "bucket-a" {
		t.Errorf("listing = %+v", l)
	}
	if l.Items[0].Fields["Location"] != "US-CENTRAL1" {
		t.Errorf("fields were dropped: %+v", l.Items[0])
	}
}

// An empty table says "you have none", which sends a developer to debug their
// own code. A failure must arrive as a failure.
func TestProviderFailureIsReportedAsUnavailableNotAsEmpty(t *testing.T) {
	t.Parallel()
	srv := serve(t, fakeProvider{
		id: "storage", title: "Buckets",
		err: errors.New("connection refused"),
	})

	code, body := get(t, srv, "/api/resources/storage", nil)
	if code != http.StatusOK {
		t.Fatalf("resources = %d", code)
	}
	var l Listing
	if err := json.Unmarshal([]byte(body), &l); err != nil {
		t.Fatal(err)
	}
	if l.Unavailable == "" {
		t.Fatal("a provider failure was reported as an empty listing")
	}
	if !strings.Contains(l.Unavailable, "connection refused") {
		t.Errorf("the cause was lost: %q", l.Unavailable)
	}
}

// An empty collection must be an empty array, not null: a client that
// iterates would otherwise have to special-case it.
func TestEmptyListingIsAnArrayNotNull(t *testing.T) {
	t.Parallel()
	srv := serve(t, fakeProvider{id: "storage", title: "Buckets"})

	_, body := get(t, srv, "/api/resources/storage", nil)
	if strings.Contains(body, `"items":null`) {
		t.Errorf("empty items rendered as null: %s", body)
	}
	if !strings.Contains(body, `"items":[]`) {
		t.Errorf("items = %s", body)
	}
}

func TestUnknownServiceIsNotFound(t *testing.T) {
	t.Parallel()
	srv := serve(t)

	code, body := get(t, srv, "/api/resources/nosuch", nil)
	if code != http.StatusNotFound {
		t.Errorf("unknown service = %d, want 404: %s", code, body)
	}
}

// A link carries its scope, so the project in the query must reach the
// provider — otherwise a shared link shows the wrong project's data.
func TestProjectScopeReachesTheProvider(t *testing.T) {
	t.Parallel()
	var seen []string
	srv := serve(t, fakeProvider{id: "storage", title: "Buckets", seen: &seen})

	get(t, srv, "/api/resources/storage?project=demo", nil)
	get(t, srv, "/api/resources/storage", nil)

	if len(seen) != 2 {
		t.Fatalf("provider called %d times", len(seen))
	}
	if seen[0] != "demo" {
		t.Errorf("project = %q, want demo", seen[0])
	}
	if seen[1] != "" {
		t.Errorf("an unscoped request passed %q", seen[1])
	}
}

// --- lifecycle ---------------------------------------------------------

func TestServerStartsServesAndStops(t *testing.T) {
	t.Parallel()
	s := New("127.0.0.1:0", nil)

	if s.URL() != "" {
		t.Error("URL reported an address before Start")
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	url := s.URL()
	if url == "" {
		t.Fatal("no URL after Start")
	}

	resp, err := http.Get(url + "/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("shell = %d", resp.StatusCode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// Stop is idempotent: the coordinator may stop a component twice during
	// a failed startup.
	if err := s.Stop(ctx); err != nil {
		t.Errorf("second Stop: %v", err)
	}
	if _, err := http.Get(url + "/"); err == nil {
		t.Error("the console still served after Stop")
	}
}

func TestStartReportsABindFailure(t *testing.T) {
	t.Parallel()
	first := New("127.0.0.1:0", nil)
	if err := first.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Stop(context.Background()) }()

	clash := New(first.Addr(), nil)
	if err := clash.Start(context.Background()); err == nil {
		_ = clash.Stop(context.Background())
		t.Fatal("binding an address already in use reported success")
	}
}

// A status source that was never set must not panic the page.
func TestMissingStatusSourceIsSafe(t *testing.T) {
	t.Parallel()
	s := New("127.0.0.1:0", nil)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	code, _ := get(t, srv, "/api/status", nil)
	if code != http.StatusOK {
		t.Errorf("status with no source = %d", code)
	}
}

// A screen that silently ignores a filter it offers is lying about what the
// rows are, so a caveat must survive to the client.
func TestListingNoteIsCarriedThrough(t *testing.T) {
	t.Parallel()
	srv := serve(t, fakeProvider{
		id: "storage", title: "Buckets",
		listing: Listing{Note: "the backend does not scope by project", Items: []Resource{{Name: "b"}}},
	})

	_, body := get(t, srv, "/api/resources/storage", nil)
	if !strings.Contains(body, "does not scope by project") {
		t.Errorf("the caveat was dropped: %s", body)
	}
}

// --- mutations ---------------------------------------------------------

type mutableProvider struct {
	fakeProvider
	created   map[string]string
	deleted   []string
	acted     []string
	createErr error
	actErr    error
}

func (m *mutableProvider) CreateForm() (string, []Field) {
	return "Create thing", []Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: "^[a-z]+$"},
	}
}

func (m *mutableProvider) Create(_ context.Context, project string, values map[string]string) (string, error) {
	if m.createErr != nil {
		return "", m.createErr
	}
	if m.created == nil {
		m.created = map[string]string{}
	}
	m.created[values["name"]] = project
	return "projects/" + project + "/things/" + values["name"], nil
}

func (m *mutableProvider) Delete(_ context.Context, _ string, name string) error {
	m.deleted = append(m.deleted, name)
	return nil
}

func (m *mutableProvider) Actions(Resource) []Action {
	return []Action{{ID: "pause", Label: "Pause"}, {ID: "purge", Label: "Purge", Destructive: true}}
}

func (m *mutableProvider) Act(_ context.Context, _, name, action string) error {
	if m.actErr != nil {
		return m.actErr
	}
	m.acted = append(m.acted, name+":"+action)
	return nil
}

func post(t *testing.T, srv *httptest.Server, path, body string) (int, string) {
	t.Helper()
	return sendBody(t, srv, http.MethodPost, path, body)
}

func sendBody(t *testing.T, srv *httptest.Server, method, path, body string) (int, string) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// A control only appears when the backend says the service can perform it,
// so an unsupported operation is absent rather than disabled-and-mysterious.
func TestCapabilitiesAreAdvertisedPerService(t *testing.T) {
	t.Parallel()
	srv := serve(t,
		&mutableProvider{fakeProvider: fakeProvider{id: "things", title: "Things"}},
		fakeProvider{id: "readonly", title: "Read only"},
	)

	_, body := get(t, srv, "/api/services", nil)
	var got struct {
		Services []struct {
			ID     string `json:"id"`
			Create *struct {
				Label  string  `json:"label"`
				Fields []Field `json:"fields"`
			} `json:"create"`
			Delete bool `json:"delete"`
		} `json:"services"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, body)
	}

	byID := map[string]int{}
	for i, s := range got.Services {
		byID[s.ID] = i
	}
	mutable := got.Services[byID["things"]]
	if mutable.Create == nil || mutable.Create.Label != "Create thing" {
		t.Errorf("create capability missing: %+v", mutable)
	}
	if len(mutable.Create.Fields) != 1 || mutable.Create.Fields[0].Pattern == "" {
		t.Errorf("the form carries no constraint: %+v", mutable.Create)
	}
	if !mutable.Delete {
		t.Error("delete capability missing")
	}

	readonly := got.Services[byID["readonly"]]
	if readonly.Create != nil || readonly.Delete {
		t.Errorf("a read-only service advertised mutations: %+v", readonly)
	}
}

func TestCreateGoesThroughTheProvider(t *testing.T) {
	t.Parallel()
	p := &mutableProvider{fakeProvider: fakeProvider{id: "things", title: "Things"}}
	srv := serve(t, p)

	code, body := post(t, srv, "/api/resources/things?project=demo", `{"name":"alpha"}`)
	if code != http.StatusOK {
		t.Fatalf("create = %d: %s", code, body)
	}
	if p.created["alpha"] != "demo" {
		t.Errorf("the provider did not receive the create: %+v", p.created)
	}
	if !strings.Contains(body, "projects/demo/things/alpha") {
		t.Errorf("the created name was not returned: %s", body)
	}
}

// A generic "could not create" hides the constraint the caller violated.
func TestCreateFailureReturnsTheServiceMessage(t *testing.T) {
	t.Parallel()
	p := &mutableProvider{
		fakeProvider: fakeProvider{id: "things", title: "Things"},
		createErr:    errors.New("bucket names must be at least 3 characters"),
	}
	srv := serve(t, p)

	code, body := post(t, srv, "/api/resources/things", `{"name":"a"}`)
	if code != http.StatusBadRequest {
		t.Errorf("create failure = %d, want 400", code)
	}
	if !strings.Contains(body, "at least 3 characters") {
		t.Errorf("the service's message was lost: %s", body)
	}
}

func TestCreateOnAReadOnlyServiceIsNotImplemented(t *testing.T) {
	t.Parallel()
	srv := serve(t, fakeProvider{id: "readonly", title: "Read only"})

	code, body := post(t, srv, "/api/resources/readonly", `{"name":"x"}`)
	if code != http.StatusNotImplemented {
		t.Errorf("create = %d, want 501: %s", code, body)
	}
}

func TestDeleteRequiresANameAndReachesTheProvider(t *testing.T) {
	t.Parallel()
	p := &mutableProvider{fakeProvider: fakeProvider{id: "things", title: "Things"}}
	srv := serve(t, p)

	// Without a name there is nothing to delete, and guessing would be
	// catastrophic.
	code, body := sendBody(t, srv, http.MethodDelete, "/api/resources/things", "")
	if code != http.StatusBadRequest {
		t.Errorf("delete with no name = %d, want 400: %s", code, body)
	}
	if len(p.deleted) != 0 {
		t.Fatalf("a delete with no name reached the provider: %v", p.deleted)
	}

	code, body = sendBody(t, srv, http.MethodDelete, "/api/resources/things?name=alpha", "")
	if code != http.StatusOK {
		t.Fatalf("delete = %d: %s", code, body)
	}
	if len(p.deleted) != 1 || p.deleted[0] != "alpha" {
		t.Errorf("the provider received %v", p.deleted)
	}
}

// An action only appears when the provider offers it for that resource.
func TestActionsAreAttachedToEachResource(t *testing.T) {
	t.Parallel()
	p := &mutableProvider{fakeProvider: fakeProvider{
		id: "things", title: "Things",
		listing: Listing{Items: []Resource{{Name: "q1", Status: "RUNNING"}}},
	}}
	srv := serve(t, p)

	_, body := get(t, srv, "/api/resources/things", nil)
	var l Listing
	if err := json.Unmarshal([]byte(body), &l); err != nil {
		t.Fatal(err)
	}
	if len(l.Items) != 1 || len(l.Items[0].Actions) != 2 {
		t.Fatalf("actions were not attached: %+v", l.Items)
	}
	// A destructive action must be marked, so the client can name what it
	// is about to affect rather than asking "are you sure" with no subject.
	var destructive bool
	for _, a := range l.Items[0].Actions {
		if a.ID == "purge" {
			destructive = a.Destructive
		}
	}
	if !destructive {
		t.Error("purge is not marked destructive")
	}
}

func TestActionReachesTheProvider(t *testing.T) {
	t.Parallel()
	p := &mutableProvider{fakeProvider: fakeProvider{id: "things", title: "Things"}}
	srv := serve(t, p)

	code, body := post(t, srv, "/api/actions/things", `{"Name":"q1","Action":"pause"}`)
	if code != http.StatusOK {
		t.Fatalf("action = %d: %s", code, body)
	}
	if len(p.acted) != 1 || p.acted[0] != "q1:pause" {
		t.Errorf("the provider received %v", p.acted)
	}

	code, _ = post(t, srv, "/api/actions/things", `{"Name":"q1"}`)
	if code != http.StatusBadRequest {
		t.Errorf("an action with no action id = %d, want 400", code)
	}
}

func TestActionOnAServiceWithNoneIsNotImplemented(t *testing.T) {
	t.Parallel()
	srv := serve(t, fakeProvider{id: "plain", title: "Plain"})

	code, _ := post(t, srv, "/api/actions/plain", `{"Name":"x","Action":"pause"}`)
	if code != http.StatusNotImplemented {
		t.Errorf("action = %d, want 501", code)
	}
}

// Mutations must be subject to the same cross-site guard as reads, or a
// visited page could create and delete resources.
func TestMutationsAreAlsoProtectedFromCrossSiteRequests(t *testing.T) {
	t.Parallel()
	p := &mutableProvider{fakeProvider: fakeProvider{id: "things", title: "Things"}}
	srv := serve(t, p)

	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/api/resources/things?name=alpha", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a cross-site delete = %d, want 403", resp.StatusCode)
	}
	if len(p.deleted) != 0 {
		t.Fatalf("a cross-site delete reached the provider: %v", p.deleted)
	}
}

// A gRPC error stringifies with the transport in front of the message a
// developer needs to read.
func TestGRPCErrorsAreRenderedWithoutTheTransportEnvelope(t *testing.T) {
	t.Parallel()
	p := &mutableProvider{
		fakeProvider: fakeProvider{id: "things", title: "Things"},
		createErr:    status.Error(codes.AlreadyExists, "Topic already exists"),
	}
	srv := serve(t, p)

	_, body := post(t, srv, "/api/resources/things", `{"name":"dup"}`)
	if strings.Contains(body, "rpc error") || strings.Contains(body, "desc =") {
		t.Errorf("the transport envelope reached the screen: %s", body)
	}
	// The code still matters: it says whether this is the caller's mistake.
	if !strings.Contains(body, "AlreadyExists") {
		t.Errorf("the status code was dropped: %s", body)
	}
	if !strings.Contains(body, "Topic already exists") {
		t.Errorf("the message was dropped: %s", body)
	}
}

// A plain error must survive unchanged.
func TestPlainErrorsAreNotRewritten(t *testing.T) {
	t.Parallel()
	p := &mutableProvider{
		fakeProvider: fakeProvider{id: "things", title: "Things"},
		createErr:    errors.New("choose a project first"),
	}
	srv := serve(t, p)

	_, body := post(t, srv, "/api/resources/things", `{"name":"x"}`)
	if !strings.Contains(body, "choose a project first") {
		t.Errorf("the message was altered: %s", body)
	}
}

// A client that iterates before checking `unavailable` must not fall over on
// top of the failure it was about to report.
func TestAnUnavailableListingStillCarriesEmptyCollections(t *testing.T) {
	t.Parallel()
	srv := serve(t, fakeProvider{id: "s", title: "S", err: errors.New("backend is down")})

	_, body := get(t, srv, "/api/resources/s", nil)
	if strings.Contains(body, `"items":null`) || strings.Contains(body, `"columns":null`) {
		t.Errorf("an unavailable listing returned null collections: %s", body)
	}
}

// --- operations and deadlines -----------------------------------------

// An action is a mutation, and must leave the same trail one does.
//
// Cloud Tasks' purge destroys a queue's contents and produced no record
// anywhere: not in /api/operations, not in Activity, not in the logs. The
// screen listing operations is headed "Operations this console performed",
// which was untrue.
func TestAnActionIsRecordedLikeACreate(t *testing.T) {
	t.Parallel()
	p := &mutableProvider{fakeProvider: fakeProvider{id: "things", title: "Things"}}
	srv := serve(t, p)

	code, body := post(t, srv, "/api/actions/things?project=demo",
		`{"Name":"q1","Action":"purge"}`)
	if code != http.StatusOK {
		t.Fatalf("action = %d: %s", code, body)
	}
	var applied struct{ Applied, Operation string }
	if err := json.Unmarshal([]byte(body), &applied); err != nil {
		t.Fatalf("decode action response: %v", err)
	}
	if applied.Operation == "" {
		t.Fatal("the action response carries no operation id, so the client " +
			"cannot reconcile its own record with the server's")
	}

	_, ops := get(t, srv, "/api/operations?project=demo", nil)
	if !strings.Contains(ops, applied.Operation) {
		t.Errorf("the operation is missing from /api/operations: %s", ops)
	}
	if !strings.Contains(ops, "purge") {
		t.Errorf("the operation does not name the action: %s", ops)
	}

	_, logs := get(t, srv, "/api/logs?operation="+applied.Operation, nil)
	if !strings.Contains(logs, "purge applied to q1") {
		t.Errorf("no log entry carries the operation id: %s", logs)
	}
}

// A failed action records the failure, not silence.
func TestAFailedActionRecordsItsCause(t *testing.T) {
	t.Parallel()
	p := &mutableProvider{
		fakeProvider: fakeProvider{id: "things", title: "Things"},
		actErr:       errors.New("the queue is already paused"),
	}
	srv := serve(t, p)

	code, _ := post(t, srv, "/api/actions/things?project=demo",
		`{"Name":"q1","Action":"pause"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("a failing action returned %d, want 400", code)
	}

	_, ops := get(t, srv, "/api/operations?project=demo", nil)
	if !strings.Contains(ops, "the queue is already paused") {
		t.Errorf("the operation does not carry the cause: %s", ops)
	}
	if !strings.Contains(ops, "FAILED") {
		t.Errorf("the operation is not marked failed: %s", ops)
	}
}

// A create operation names what it created, not the product it used.
//
// The operation is opened before the name exists — the backend may derive one
// the form never carried — so both the Activity screen and the notifications
// panel used to say "create Pub/Sub".
func TestACreateOperationNamesWhatItCreated(t *testing.T) {
	t.Parallel()
	p := &mutableProvider{fakeProvider: fakeProvider{id: "things", title: "Things"}}
	srv := serve(t, p)

	code, body := post(t, srv, "/api/resources/things?project=demo", `{"name":"alpha"}`)
	if code != http.StatusOK {
		t.Fatalf("create = %d: %s", code, body)
	}

	_, ops := get(t, srv, "/api/operations?project=demo", nil)
	if !strings.Contains(ops, "projects/demo/things/alpha") {
		t.Errorf("the operation does not name the resource it created: %s", ops)
	}
}

// A failure is a record like a success, and carries the same id.
//
// Without it the browser cannot tell the server's copy of a failed operation
// from its own optimistic entry, and the notifications panel showed the same
// failure twice.
func TestAFailedMutationReturnsItsOperationID(t *testing.T) {
	t.Parallel()
	p := &mutableProvider{
		fakeProvider: fakeProvider{id: "things", title: "Things"},
		createErr:    errors.New("already exists"),
		actErr:       errors.New("already paused"),
	}
	srv := serve(t, p)

	for _, c := range []struct {
		name, method, path, body string
	}{
		{"create", http.MethodPost, "/api/resources/things?project=demo", `{"name":"alpha"}`},
		{"action", http.MethodPost, "/api/actions/things?project=demo", `{"Name":"a","Action":"pause"}`},
	} {
		_, body := sendBody(t, srv, c.method, c.path, c.body)
		var out struct{ Error, Operation string }
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("%s: decode: %v", c.name, err)
		}
		if out.Operation == "" {
			t.Errorf("a failed %s returns no operation id: %s", c.name, body)
		}
		if out.Error == "" {
			t.Errorf("a failed %s returns no cause: %s", c.name, body)
		}
	}
}

// blockingDriller records the context its Detail call was given.
type blockingDriller struct {
	fakeProvider
	deadline  chan bool
	detailErr error
}

func (b *blockingDriller) Detail(ctx context.Context, _ string, _ []string) (Detail, error) {
	_, ok := ctx.Deadline()
	select {
	case b.deadline <- ok:
	default:
	}
	if b.detailErr != nil {
		return Detail{}, b.detailErr
	}
	return Detail{Sections: []Section{{ID: "things", Label: "Things"}}}, nil
}

// Every read the console serves is bounded.
//
// handleResources already was; handleDetail, handleStatus and handleMetrics
// passed the request's own context straight through, so a wedged provider left
// a skeleton shimmering with no elapsed time, no cancel and no eventual error.
//
// The contract is asserted rather than the duration: a test that actually
// waited out the budget would take twenty seconds to prove one line. The
// second half — that a provider which does hit the deadline produces a screen
// that says so — is asserted directly below it.
func TestEveryReadCarriesADeadline(t *testing.T) {
	t.Parallel()
	d := &blockingDriller{
		fakeProvider: fakeProvider{id: "things", title: "Things"},
		deadline:     make(chan bool, 1),
	}
	srv := serve(t, d)

	if code, body := get(t, srv, "/api/detail/things?name=one", nil); code != http.StatusOK {
		t.Fatalf("detail = %d: %s", code, body)
	}
	select {
	case ok := <-d.deadline:
		if !ok {
			t.Error("Detail was given a context with no deadline, so a hung " +
				"provider hangs the screen")
		}
	default:
		t.Fatal("Detail was never called")
	}
}

// A read that runs out of time says so in words a user can act on.
func TestADeadlineReadsAsTheInstanceNotAnswering(t *testing.T) {
	t.Parallel()
	d := &blockingDriller{
		fakeProvider: fakeProvider{id: "things", title: "Things"},
		deadline:     make(chan bool, 1),
		detailErr:    context.DeadlineExceeded,
	}
	srv := serve(t, d)

	_, body := get(t, srv, "/api/detail/things?name=one", nil)
	if strings.Contains(body, "context deadline exceeded") {
		t.Errorf("the screen would show Go's own wording: %s", body)
	}
	if !strings.Contains(body, "did not answer in time") {
		t.Errorf("the failure does not say what happened: %s", body)
	}
}

// A log entry that belongs to no project must not vanish when a project is
// selected.
//
// Every pod log line is unattributed — the Pub/Sub emulator serves every
// project from one container, so there is nothing to attribute it to — and
// the console always sends the toolbar's project. The Logs Explorer therefore
// showed nothing at all on an instance holding hundreds of lines, which reads
// as "my application is silent".
func TestUnattributedLogEntriesSurviveAProjectScope(t *testing.T) {
	t.Parallel()
	s := New("127.0.0.1:0", nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	s.Logs().Log(Entry{Source: "pods/one", Message: "a pod line with no project"})
	s.Logs().Log(Entry{Source: "storage", Project: "demo", Message: "a console mutation"})
	s.Logs().Log(Entry{Source: "storage", Project: "other", Message: "another project"})

	// All sources: everything, whatever it can be attributed to.
	_, all := get(t, srv, "/api/logs", nil)
	for _, want := range []string{"a pod line with no project", "a console mutation", "another project"} {
		if !strings.Contains(all, want) {
			t.Errorf("the unscoped view dropped %q: %s", want, all)
		}
	}

	// Scoped: one project's own entries, and not another's.
	_, scoped := get(t, srv, "/api/logs?project=demo", nil)
	if !strings.Contains(scoped, "a console mutation") {
		t.Errorf("the scoped view lost the entry it is scoped to: %s", scoped)
	}
	if strings.Contains(scoped, "another project") {
		t.Errorf("the scoped view leaked another project's entry: %s", scoped)
	}

	// And the counts that let the screen explain an empty scoped view.
	var census struct{ Held, Unattributed int }
	if err := json.Unmarshal([]byte(scoped), &census); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if census.Held != 3 {
		t.Errorf("held = %d, want 3", census.Held)
	}
	if census.Unattributed != 1 {
		t.Errorf("unattributed = %d, want 1; without it the empty state cannot "+
			"say why it is empty", census.Unattributed)
	}
}

// The history is the console's, and it is honest about how short it is.
func TestSeriesKeepsABoundedHistory(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	series := NewSeries(3, func() time.Time { return at })

	for i := 0; i < 5; i++ {
		series.Add(Metrics{Nodes: []NodeMetrics{{
			Name: "n", CPUUsedCores: float64(i),
			At: at.Add(time.Duration(i) * time.Second).Format(time.RFC3339),
		}}})
	}

	samples, started, interval := series.Window()
	if len(samples) != 3 {
		t.Fatalf("kept %d samples, want the last 3", len(samples))
	}
	// Oldest first, and the two earliest dropped.
	if samples[0].Nodes[0].CPUUsedCores != 2 || samples[2].Nodes[0].CPUUsedCores != 4 {
		t.Errorf("the ring dropped the wrong end: %v", samples)
	}
	// The kubelet's timestamp, not the host clock at decode.
	if !samples[2].At.Equal(at.Add(4 * time.Second)) {
		t.Errorf("sample time = %v, want the kubelet's own", samples[2].At)
	}
	if started != at {
		t.Errorf("started = %v; a client cannot tell a young instance from a quiet one", started)
	}
	if interval != SampleInterval {
		t.Errorf("interval = %v", interval)
	}
}

// A failed read is kept as a gap rather than skipped.
//
// Skipping it would join the readings either side with a straight line
// through a period nobody measured.
func TestSeriesRecordsAGapAsAGap(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	series := NewSeries(10, func() time.Time { return at })

	series.Add(Metrics{Nodes: []NodeMetrics{{Name: "n", CPUUsedCores: 1}}})
	series.Add(Metrics{Unavailable: "the kubelet did not answer"})
	series.Add(Metrics{Nodes: []NodeMetrics{{Name: "n", CPUUsedCores: 3}}})

	samples, _, _ := series.Window()
	if len(samples) != 3 {
		t.Fatalf("kept %d samples; the failure was dropped", len(samples))
	}
	if samples[1].Unavailable == "" {
		t.Error("the failed read is indistinguishable from a real reading")
	}
	if len(samples[1].Nodes) != 0 {
		t.Error("a failed read carries node numbers it did not take")
	}
}

// A server with no series says so rather than returning an empty array that
// reads as an idle cluster.
func TestSeriesEndpointDistinguishesAbsentFromEmpty(t *testing.T) {
	t.Parallel()
	srv := serve(t)
	_, body := get(t, srv, "/api/metrics/series", nil)
	if !strings.Contains(body, "not retaining metric history") {
		t.Errorf("an instance keeping no history returns a bare empty list: %s", body)
	}
}

// The sampler reads on its own clock, not on a request.
func TestSamplerRecordsWithoutAnyRequest(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	s := New("127.0.0.1:0", nil)
	s.SetSeries(NewSeries(10, func() time.Time { return at }))

	reads := 0
	s.SetMetrics(func(context.Context) Metrics {
		reads++
		return Metrics{Nodes: []NodeMetrics{{Name: "n", CPUUsedCores: 0.5}}}
	})

	sampler := NewSampler(s, time.Hour) // long, so only the initial read fires
	if err := sampler.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sampler.Stop(context.Background()) })

	if reads != 1 {
		t.Fatalf("the sampler took %d readings at start, want 1 — a dashboard "+
			"opened straight after startup would show an empty chart", reads)
	}
	samples, _, _ := s.series.Window()
	if len(samples) != 1 {
		t.Errorf("the reading was not retained: %v", samples)
	}
}

// The kubelet refreshes about every ten seconds while the sampler reads every
// five, so two consecutive reads routinely carry the same kubelet timestamp
// and the same counter. Storing both makes the window cover less wall-clock
// time than its length implies.
func TestSeriesDropsARepeatedKubeletReading(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	series := NewSeries(10, func() time.Time { return at })

	sample := func(sec int, cpu float64) Metrics {
		return Metrics{Nodes: []NodeMetrics{{
			Name: "n", CPUUsedCores: cpu,
			At: at.Add(time.Duration(sec) * time.Second).Format(time.RFC3339),
		}}}
	}
	series.Add(sample(0, 1))
	series.Add(sample(0, 1)) // the kubelet had not refreshed
	series.Add(sample(10, 2))

	samples, _, _ := series.Window()
	if len(samples) != 2 {
		t.Fatalf("kept %d samples, want 2: the repeat was stored", len(samples))
	}

	// A failed read is new information even when the numbers are not, so it
	// is kept regardless.
	series.Add(Metrics{Unavailable: "the kubelet did not answer"})
	series.Add(Metrics{Unavailable: "the kubelet did not answer"})
	samples, _, _ = series.Window()
	if len(samples) != 4 {
		t.Errorf("kept %d samples; a gap must never be collapsed away", len(samples))
	}
}

// A section can hold something other than rows.
//
// Everything a resource page needs to say that is not tabular — its
// configuration, its YAML, a chart — had nowhere to go, so "open this
// resource" meant "here is one more list".
func TestSectionCarriesMoreThanATable(t *testing.T) {
	t.Parallel()
	p := &kindedDriller{fakeProvider: fakeProvider{id: "things", title: "Things"}}
	srv := serve(t, p)

	_, body := get(t, srv, "/api/detail/things?name=one", nil)
	var d Detail
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(d.Sections) != 4 {
		t.Fatalf("got %d sections: %s", len(d.Sections), body)
	}

	byID := map[string]Section{}
	for _, s := range d.Sections {
		byID[s.ID] = s
	}

	// A listing section keeps its old shape, and its kind is empty so every
	// provider written before this keeps working unchanged.
	if got := byID["rows"]; got.Kind != KindListing || len(got.Listing.Items) != 1 {
		t.Errorf("the listing section changed shape: %+v", got)
	}
	// Properties arrive grouped, because a resource's configuration divides.
	props := byID["config"]
	if props.Kind != KindProperties || len(props.Groups) != 1 ||
		props.Groups[0].Heading != "Networking" {
		t.Errorf("properties section: %+v", props)
	}
	// Text keeps its newlines: a YAML document that lost them is not one.
	if txt := byID["yaml"]; txt.Kind != KindText || !strings.Contains(txt.Text, "\n") {
		t.Errorf("text section lost its newlines: %+v", txt)
	}
	// A chart point with no reading survives as null rather than zero, so the
	// client can draw a gap instead of a line through a period nobody
	// measured.
	chart := byID["metrics"]
	if chart.Kind != KindChart || len(chart.Series) != 1 {
		t.Fatalf("chart section: %+v", chart)
	}
	if !strings.Contains(body, `"value":null`) {
		t.Error("a missing reading was serialised as a number; the chart would " +
			"draw a line through a period nobody measured")
	}
}

type kindedDriller struct{ fakeProvider }

func (kindedDriller) Detail(context.Context, string, []string) (Detail, error) {
	half := 0.5
	return Detail{Sections: []Section{
		{ID: "rows", Label: "Rows", Listing: Listing{Items: []Resource{{Name: "r"}}}},
		{ID: "config", Label: "Configuration", Kind: KindProperties,
			Groups: []PropertyGroup{{Heading: "Networking",
				Properties: []Property{{Label: "Port", Value: "8080"}}}}},
		{ID: "yaml", Label: "YAML", Kind: KindText, Text: "a: 1\nb: 2\n"},
		{ID: "metrics", Label: "Metrics", Kind: KindChart,
			Series: []ChartSeries{{Label: "CPU", Unit: "cores", Points: []ChartPoint{
				{At: "2026-09-22T12:00:00Z", Value: &half},
				{At: "2026-09-22T12:00:05Z", Value: nil},
			}}}},
	}}, nil
}

// A resource contains resources, and each level has its own address.
//
// Driller took a single leaf name, so the console could express exactly two
// levels — and the third, the one a developer opens a database console to
// reach, had nowhere to live.
func TestDetailAddressesAResourcePath(t *testing.T) {
	t.Parallel()
	p := &pathDriller{fakeProvider: fakeProvider{id: "things", title: "Things"}}
	srv := serve(t, p)

	// Each segment travels as its own parameter, so one containing a slash —
	// an object key routinely does — survives without a second escaping
	// convention on top of the URL's own.
	_, body := get(t, srv, "/api/detail/things?name=db&name=a%2Fb%2Fc", nil)
	if !strings.Contains(body, "db → a/b/c") {
		t.Errorf("the path did not arrive intact: %s", body)
	}
	if got := p.seen; len(got) != 2 || got[0] != "db" || got[1] != "a/b/c" {
		t.Errorf("provider saw %q", got)
	}

	// One segment still works, which is every provider written before this.
	_, body = get(t, srv, "/api/detail/things?name=db", nil)
	if !strings.Contains(body, "db") || strings.Contains(body, "→") {
		t.Errorf("a one-level path changed shape: %s", body)
	}

	// An empty path is a bad request, not a guess.
	if code, _ := get(t, srv, "/api/detail/things", nil); code != http.StatusBadRequest {
		t.Errorf("an addressless detail returned %d", code)
	}
}

// A provider that cannot go deeper says so rather than silently showing the
// level above, which would be the console answering a question it was not
// asked.
func TestDeeperThanNamesWhereItStopped(t *testing.T) {
	t.Parallel()
	d := DeeperThan(1, []string{"db", "table", "column"})
	if d.Unavailable == "" {
		t.Fatal("no explanation")
	}
	if !strings.Contains(d.Unavailable, "db") {
		t.Errorf("the refusal does not say where it stopped: %q", d.Unavailable)
	}
	if strings.Contains(d.Unavailable, "column") {
		t.Errorf("the refusal quotes a level it never reached: %q", d.Unavailable)
	}
}

type pathDriller struct {
	fakeProvider
	seen []string
}

func (p *pathDriller) Detail(_ context.Context, _ string, path []string) (Detail, error) {
	p.seen = path
	return Detail{Sections: []Section{{
		ID: "s", Label: strings.Join(path, " → "),
		Listing: Listing{Items: []Resource{}},
	}}}, nil
}

// A provider that cannot be queried says so, rather than the console
// offering an editor over nothing.
func TestQueryIsOfferedOnlyWhereItCanBeAnswered(t *testing.T) {
	t.Parallel()
	srv := serve(t,
		&queryable{fakeProvider: fakeProvider{id: "db", title: "DB"}},
		&fakeProvider{id: "plain", title: "Plain"},
	)

	_, services := get(t, srv, "/api/services", nil)
	if !strings.Contains(services, `"hint"`) {
		t.Error("a queryable provider advertises no query capability")
	}

	code, body := post(t, srv, "/api/query/plain", `{"Path":["x"],"Statement":"SELECT 1"}`)
	if code != http.StatusNotImplemented {
		t.Errorf("querying a provider that cannot = %d, want 501: %s", code, body)
	}
	if !strings.Contains(body, "Plain cannot be queried") {
		t.Errorf("the refusal does not name the service: %s", body)
	}
}

// A query is something the user did, so Activity shows it — and the
// statement itself is never written to the log, because it is the user's
// text and may carry a literal they would not choose to keep.
func TestAQueryIsRecordedWithoutItsStatement(t *testing.T) {
	t.Parallel()
	srv := serve(t, &queryable{fakeProvider: fakeProvider{id: "db", title: "DB"}})

	code, body := post(t, srv, "/api/query/db?project=demo",
		`{"Path":["main"],"Statement":"SELECT secret_column FROM vault"}`)
	if code != http.StatusOK {
		t.Fatalf("query = %d: %s", code, body)
	}
	var out struct{ Operation string }
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Operation == "" {
		t.Fatal("a query produced no operation, so Activity cannot show it")
	}

	_, ops := get(t, srv, "/api/operations?project=demo", nil)
	if !strings.Contains(ops, "query") || !strings.Contains(ops, "main") {
		t.Errorf("the query is missing from the ledger: %s", ops)
	}
	_, logs := get(t, srv, "/api/logs?operation="+out.Operation, nil)
	if !strings.Contains(logs, "query returned 1 rows") {
		t.Errorf("no log entry for the query: %s", logs)
	}
	if strings.Contains(logs, "secret_column") || strings.Contains(ops, "secret_column") {
		t.Error("the statement was written to the record; it is the user's text " +
			"and may carry a literal they would not choose to keep")
	}
}

// An empty statement or an unaddressed query is a bad request, not a guess.
func TestQueryRefusesWhatItCannotRun(t *testing.T) {
	t.Parallel()
	srv := serve(t, &queryable{fakeProvider: fakeProvider{id: "db", title: "DB"}})

	for _, c := range []struct{ name, body string }{
		{"no statement", `{"Path":["main"],"Statement":"  "}`},
		{"no resource", `{"Path":[],"Statement":"SELECT 1"}`},
		{"unknown field", `{"Path":["main"],"Statement":"SELECT 1","Mode":"write"}`},
	} {
		if code, body := post(t, srv, "/api/query/db", c.body); code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400: %s", c.name, code, body)
		}
	}
}

type queryable struct{ fakeProvider }

func (queryable) QueryHint() string { return "Read-only." }
func (queryable) Query(_ context.Context, _ string, path []string, _ string) (Listing, error) {
	return Listing{
		NameColumn: "id", Columns: []string{"name"},
		Items: []Resource{{Name: "1", Fields: map[string]string{"name": path[0]}}},
	}, nil
}

// A listing can mix rows that open with rows that do not.
//
// A bucket's Objects section is folders and objects together: one kind leads
// somewhere and the other is a leaf. A single per-listing flag cannot say
// that, and marking every row openable would offer links that lead nowhere.
func TestARowCanDeclareWhereItOpens(t *testing.T) {
	t.Parallel()
	p := &mixedDriller{fakeProvider: fakeProvider{id: "bucket", title: "Bucket"}}
	srv := serve(t, p)

	_, body := get(t, srv, "/api/detail/bucket?name=b", nil)
	var d Detail
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatal(err)
	}
	items := d.Sections[0].Listing.Items
	if len(items) != 2 {
		t.Fatalf("got %d rows", len(items))
	}
	if len(items[0].Opens) == 0 {
		t.Error("the folder row declares no path, so it cannot be opened")
	}
	if len(items[1].Opens) != 0 {
		t.Error("the object row declares a path, so the console would offer a " +
			"link into a resource that has no level below it")
	}
}

type mixedDriller struct{ fakeProvider }

func (mixedDriller) Detail(_ context.Context, _ string, path []string) (Detail, error) {
	return Detail{Sections: []Section{{ID: "objects", Label: "Objects", Listing: Listing{
		Items: []Resource{
			{Name: "reports/", Opens: append(append([]string{}, path...), "reports")},
			{Name: "readme.txt"},
		},
	}}}}, nil
}
