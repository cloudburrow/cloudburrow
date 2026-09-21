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
