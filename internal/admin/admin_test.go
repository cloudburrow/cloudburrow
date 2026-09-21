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
