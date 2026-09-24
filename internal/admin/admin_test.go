package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

type fakeResetter struct {
	name    string
	err     error
	calls   *[]string
	ordered *[]string
}

func (f fakeResetter) Name() string { return f.name }
func (f fakeResetter) Reset(context.Context) error {
	if f.ordered != nil {
		*f.ordered = append(*f.ordered, f.name)
	}
	return f.err
}

type fakeSeeder struct {
	name string
	err  error
	got  *json.RawMessage
}

func (f fakeSeeder) Name() string { return f.name }
func (f fakeSeeder) Seed(_ context.Context, spec json.RawMessage) error {
	if f.got != nil {
		*f.got = spec
	}
	return f.err
}

func serve(a *API) *httptest.Server {
	mux := http.NewServeMux()
	a.Routes(mux)
	return httptest.NewServer(mux)
}

func post(t *testing.T, url, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		t.Fatalf("read body: %v", readErr)
	}
	return resp.StatusCode, string(respBody)
}

func TestResetRunsEveryComponentInOrder(t *testing.T) {
	t.Parallel()
	var order []string
	a := NewAPI(NewRecorder(10, nil))
	a.RegisterResetter(
		fakeResetter{name: "first", ordered: &order},
		fakeResetter{name: "second", ordered: &order},
	)
	srv := serve(a)
	defer srv.Close()

	status, body := post(t, srv.URL+"/admin/reset", "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if strings.Join(order, ",") != "first,second" {
		t.Errorf("reset order = %v, want registration order", order)
	}
}

// A partial reset that claimed success would leave a developer debugging state
// they believe was cleared.
func TestResetReportsPerComponentFailure(t *testing.T) {
	t.Parallel()
	var order []string
	a := NewAPI(NewRecorder(10, nil))
	a.RegisterResetter(
		fakeResetter{name: "ok", ordered: &order},
		fakeResetter{name: "broken", err: errors.New("disk on fire"), ordered: &order},
		fakeResetter{name: "also-ok", ordered: &order},
	)
	srv := serve(a)
	defer srv.Close()

	status, body := post(t, srv.URL+"/admin/reset", "")
	if status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 when a component failed", status)
	}
	if !strings.Contains(body, "disk on fire") {
		t.Errorf("failure cause not reported: %s", body)
	}
	// Every component is still attempted, so one failure does not hide the rest.
	if strings.Join(order, ",") != "ok,broken,also-ok" {
		t.Errorf("a failure stopped the remaining resets: %v", order)
	}
	if !strings.Contains(body, "also-ok") {
		t.Errorf("successful components not reported: %s", body)
	}
}

// An unknown component must be rejected before anything is seeded, or the
// environment is left half-populated.
func TestSeedValidatesAllNamesFirst(t *testing.T) {
	t.Parallel()
	var got json.RawMessage
	a := NewAPI(NewRecorder(10, nil))
	a.RegisterSeeder(fakeSeeder{name: "storage", got: &got})
	srv := serve(a)
	defer srv.Close()

	status, body := post(t, srv.URL+"/admin/seed",
		`{"components":{"storage":{"buckets":["a"]},"nosuch":{}}}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
	if !strings.Contains(body, "nosuch") {
		t.Errorf("error should name the unknown component: %s", body)
	}
	if got != nil {
		t.Error("a component was seeded despite another name being invalid")
	}
}

func TestSeedPassesTheDocumentThrough(t *testing.T) {
	t.Parallel()
	var got json.RawMessage
	a := NewAPI(NewRecorder(10, nil))
	a.RegisterSeeder(fakeSeeder{name: "pubsub", got: &got})
	srv := serve(a)
	defer srv.Close()

	status, body := post(t, srv.URL+"/admin/seed", `{"components":{"pubsub":{"topics":["orders"]}}}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if !strings.Contains(string(got), "orders") {
		t.Errorf("seed document did not reach the component: %s", got)
	}
}

func TestSeedRejectsMalformedAndEmpty(t *testing.T) {
	t.Parallel()
	a := NewAPI(NewRecorder(10, nil))
	a.RegisterSeeder(fakeSeeder{name: "x"})
	srv := serve(a)
	defer srv.Close()

	for _, body := range []string{`not json`, `{}`, `{"components":{}}`} {
		if status, _ := post(t, srv.URL+"/admin/seed", body); status != http.StatusBadRequest {
			t.Errorf("POST %q = %d, want 400", body, status)
		}
	}
}

func TestEventsAreNewestFirstAndFilterable(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rec := NewRecorder(100, func() time.Time { now = now.Add(time.Second); return now })
	rec.Record("pubsub", "publish", "topics/a", map[string]string{"id": "1"})
	rec.Record("storage", "upload", "objects/x", nil)
	rec.Record("pubsub", "push", "services/worker", nil)

	a := NewAPI(rec)
	srv := serve(a)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/admin/events")
	if err != nil {
		t.Fatal(err)
	}
	var all struct {
		Events []Event `json:"events"`
		Count  int     `json:"count"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&all)
	resp.Body.Close()
	if all.Count != 3 {
		t.Fatalf("count = %d, want 3", all.Count)
	}
	if all.Events[0].Kind != "push" {
		t.Errorf("newest first expected; got %q", all.Events[0].Kind)
	}

	resp, err = http.Get(srv.URL + "/admin/events?service=pubsub")
	if err != nil {
		t.Fatal(err)
	}
	var filtered struct {
		Events []Event `json:"events"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&filtered)
	resp.Body.Close()
	if len(filtered.Events) != 2 {
		t.Errorf("filtered = %d events, want 2", len(filtered.Events))
	}
	for _, e := range filtered.Events {
		if e.Service != "pubsub" {
			t.Errorf("filter leaked %s", e.Service)
		}
	}
}

// An unbounded event log in a long-running dev environment is a memory leak.
func TestRecorderIsBounded(t *testing.T) {
	t.Parallel()
	rec := NewRecorder(5, nil)
	for i := 0; i < 50; i++ {
		rec.Record("s", "k", fmt.Sprint(i), nil)
	}
	if got := rec.Len(); got != 5 {
		t.Errorf("Len() = %d, want the 5-event limit", got)
	}
	events := rec.Events("", 0)
	// The newest must be kept, not the oldest.
	if events[0].Target != "49" {
		t.Errorf("newest event = %q, want 49", events[0].Target)
	}
}

func TestEventsLimitMustBeAnInteger(t *testing.T) {
	t.Parallel()
	a := NewAPI(NewRecorder(10, nil))
	srv := serve(a)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/admin/events?limit=lots")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// A nil recorder must be safe: components record events unconditionally.
func TestNilRecorderIsSafe(t *testing.T) {
	t.Parallel()
	var rec *Recorder
	rec.Record("s", "k", "t", nil)
}

// projectResetter is a fakeResetter that can also reset one project.
type projectResetter struct {
	fakeResetter
}

func (p projectResetter) ResetProject(_ context.Context, project string) error {
	*p.calls = append(*p.calls, p.name+"@"+project)
	return p.err
}

// TestResetCanBeNarrowedToNamedServices.
//
// A reset used to be all or nothing, so clearing Pub/Sub between two test cases
// also emptied the buckets the next case depended on (#274).
func TestResetCanBeNarrowedToNamedServices(t *testing.T) {
	var calls []string
	a := NewAPI(NewRecorder(10, nil))
	a.RegisterResetter(fakeResetter{name: "storage", calls: &calls, ordered: &calls},
		fakeResetter{name: "pubsub", calls: &calls, ordered: &calls}, fakeResetter{name: "tasks", calls: &calls, ordered: &calls})
	srv := serve(a)
	defer srv.Close()

	// Named in the opposite order, and one twice: the order that matters is
	// registration order (storage before pubsub, so notification configs go
	// before their topics), and a service is reset once however often named.
	code, body := post(t, srv.URL+"/admin/reset?service=tasks&service=pubsub&service=tasks", "")
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	if strings.Join(calls, ",") != "pubsub,tasks" {
		t.Fatalf("reset %v, want only pubsub and tasks in registration order", calls)
	}
	// Comma-separated works too.
	calls = nil
	post(t, srv.URL+"/admin/reset?service=storage,tasks", "")
	if strings.Join(calls, ",") != "storage,tasks" {
		t.Fatalf("comma form reset %v", calls)
	}
}

// TestAnUnknownServiceResetsNothing.
//
// Half-applying a malformed reset is worse than refusing it: a typo in one name
// must not leave the other named services already wiped.
func TestAnUnknownServiceResetsNothing(t *testing.T) {
	var calls []string
	a := NewAPI(NewRecorder(10, nil))
	a.RegisterResetter(fakeResetter{name: "storage", calls: &calls, ordered: &calls})
	srv := serve(a)
	defer srv.Close()

	code, body := post(t, srv.URL+"/admin/reset?service=storage&service=pubsbu", "")
	if code != 400 {
		t.Fatalf("status %d, want 400: %s", code, body)
	}
	if len(calls) != 0 {
		t.Fatalf("a request naming an unknown service still reset %v", calls)
	}
	if !strings.Contains(body, "pubsbu") || !strings.Contains(body, "storage") {
		t.Errorf("the refusal names neither the unknown service nor the known ones: %s", body)
	}
}

// TestAProjectResetIsRefusedWhereItCannotBeHonoured.
//
// fake-gcs-server lists every bucket whatever project is asked for, so a
// project-scoped Storage reset would delete another project's buckets or
// nothing. It is refused, before anything else is touched.
func TestAProjectResetIsRefusedWhereItCannotBeHonoured(t *testing.T) {
	var calls []string
	a := NewAPI(NewRecorder(10, nil))
	a.RegisterResetter(projectResetter{fakeResetter{name: "pubsub", calls: &calls, ordered: &calls}},
		fakeResetter{name: "storage", calls: &calls, ordered: &calls})
	srv := serve(a)
	defer srv.Close()

	code, body := post(t, srv.URL+"/admin/reset?project=p1", "")
	if code != 400 || !strings.Contains(body, "storage") {
		t.Fatalf("status %d, want 400 naming storage: %s", code, body)
	}
	if len(calls) != 0 {
		t.Fatalf("a refused project reset still reset %v", calls)
	}

	// Leaving storage out makes the same request valid, and it goes through
	// ResetProject rather than a full Reset.
	code, body = post(t, srv.URL+"/admin/reset?project=p1&service=pubsub", "")
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	if strings.Join(calls, ",") != "pubsub@p1" {
		t.Fatalf("calls = %v, want pubsub@p1", calls)
	}
}

// validatingSeeder is a fakeSeeder that validates its document first.
type validatingSeeder struct {
	fakeSeeder
	invalid error
	order   *[]string
}

func (v validatingSeeder) Validate(json.RawMessage) error { return v.invalid }
func (v validatingSeeder) Seed(ctx context.Context, spec json.RawMessage) error {
	if v.order != nil {
		*v.order = append(*v.order, v.name)
	}
	return v.fakeSeeder.Seed(ctx, spec)
}

// TestOneInvalidDocumentSeedsNothing.
//
// Validating names alone let a bad field in one component fail after another
// component was already created (#275). Every document is validated first.
func TestOneInvalidDocumentSeedsNothing(t *testing.T) {
	t.Parallel()
	var order []string
	a := NewAPI(NewRecorder(10, nil))
	a.RegisterSeeder(validatingSeeder{fakeSeeder: fakeSeeder{name: "aaa"}, order: &order})
	a.RegisterSeeder(validatingSeeder{fakeSeeder: fakeSeeder{name: "zzz"}, order: &order,
		invalid: errors.New(`buckets[0].name "Bad" is not a valid bucket name`)})
	srv := serve(a)
	defer srv.Close()

	status, body := post(t, srv.URL+"/admin/seed", `{"components":{"aaa":{},"zzz":{}}}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", status, body)
	}
	if !strings.Contains(body, "buckets[0].name") || !strings.Contains(body, `"component":"zzz"`) {
		t.Errorf("the refusal does not name the component and field: %s", body)
	}
	if len(order) != 0 {
		t.Errorf("seeded %v although another component's document was invalid", order)
	}
}

func TestSeedingRunsInNameOrder(t *testing.T) {
	t.Parallel()
	var order []string
	a := NewAPI(NewRecorder(10, nil))
	for _, n := range []string{"tasks", "storage", "pubsub", "secretmanager"} {
		a.RegisterSeeder(validatingSeeder{fakeSeeder: fakeSeeder{name: n}, order: &order})
	}
	srv := serve(a)
	defer srv.Close()
	for i := 0; i < 5; i++ {
		order = nil
		post(t, srv.URL+"/admin/seed", `{"components":{"tasks":{},"storage":{},"pubsub":{},"secretmanager":{}}}`)
		if got := strings.Join(order, ","); got != "pubsub,secretmanager,storage,tasks" {
			t.Fatalf("seed order %s, want a fixed name order", got)
		}
	}
}

// A resource that already exists is the caller's conflict. Reporting it as a
// server fault would hide the difference a re-running script needs.
func TestAnExistingResourceIsAConflict(t *testing.T) {
	t.Parallel()
	a := NewAPI(NewRecorder(10, nil))
	a.RegisterSeeder(fakeSeeder{name: "storage", err: fmt.Errorf("seeding: %w", apierror.AlreadyExists("bucket b already exists"))})
	a.RegisterSeeder(fakeSeeder{name: "broken", err: errors.New("disk on fire")})
	srv := serve(a)
	defer srv.Close()

	if status, body := post(t, srv.URL+"/admin/seed", `{"components":{"storage":{}}}`); status != http.StatusConflict {
		t.Errorf("an existing resource returned %d, want 409: %s", status, body)
	}
	if status, _ := post(t, srv.URL+"/admin/seed", `{"components":{"broken":{}}}`); status != http.StatusInternalServerError {
		t.Errorf("an unclassified failure returned %d, want 500", status)
	}
}
