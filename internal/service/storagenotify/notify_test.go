package storagenotify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/identity-wael/cloudburrow/internal/apierror"
	"github.com/identity-wael/cloudburrow/internal/store"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	db := store.NewMemory()
	t.Cleanup(func() { _ = db.Close() })
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	return NewStoreWithClock(db, func() time.Time { now = now.Add(time.Second); return now })
}

func wantCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got no error", want)
	}
	if got := apierror.From(err).Code; got != want {
		t.Errorf("code = %s, want %s (%v)", got, want, err)
	}
}

const topicA = "projects/demo/topics/a"
const topicB = "projects/demo/topics/b"

// --- configuration store ----------------------------------------------

func TestCreateAssignsSequentialIDs(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	first, err := s.Create("b1", Config{Topic: topicA})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if first.ID != "1" {
		t.Errorf("first ID = %q, want 1", first.ID)
	}
	if first.PayloadFormat != PayloadJSONAPIV1 {
		t.Errorf("payload format = %q, want the JSON_API_V1 default", first.PayloadFormat)
	}

	second, err := s.Create("b1", Config{Topic: topicB})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if second.ID == first.ID {
		t.Errorf("two configurations share ID %q", second.ID)
	}

	// IDs are per bucket, as Cloud Storage assigns them.
	other, err := s.Create("b2", Config{Topic: topicA})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if other.ID != "1" {
		t.Errorf("a new bucket started at %q, want 1", other.ID)
	}
}

// Two identical registrations would deliver every event twice, which looks
// like the backend emitting duplicates rather than the caller registering
// twice.
func TestDuplicateConfigurationIsRefused(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	if _, err := s.Create("b1", Config{Topic: topicA, ObjectNamePrefix: "in/"}); err != nil {
		t.Fatal(err)
	}
	_, err := s.Create("b1", Config{Topic: topicA, ObjectNamePrefix: "in/"})
	wantCode(t, err, codes.AlreadyExists)

	// A different filter is a different configuration.
	if _, err := s.Create("b1", Config{Topic: topicA, ObjectNamePrefix: "out/"}); err != nil {
		t.Errorf("a configuration with a different prefix was refused: %v", err)
	}
}

// A configuration with an unusable topic would be accepted and then drop
// every event it matched, with nothing to point at.
func TestInvalidTopicIsRefusedAtCreation(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	for _, topic := range []string{"", "a", "projects/demo/subscriptions/x",
		"projects//topics/a", "projects/demo/topics/"} {
		_, err := s.Create("b1", Config{Topic: topic})
		wantCode(t, err, codes.InvalidArgument)
	}
}

func TestInvalidPayloadFormatAndEventTypeAreRefused(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	_, err := s.Create("b1", Config{Topic: topicA, PayloadFormat: "XML"})
	wantCode(t, err, codes.InvalidArgument)

	_, err = s.Create("b1", Config{Topic: topicA, EventTypes: []EventType{"OBJECT_EXPLODE"}})
	wantCode(t, err, codes.InvalidArgument)
}

func TestGetListAndDelete(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	created, err := s.Create("b1", Config{Topic: topicA})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("b1", created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Topic != topicA {
		t.Errorf("topic = %q", got.Topic)
	}

	if _, err := s.Get("b1", "999"); err == nil {
		t.Error("an unknown configuration was found")
	} else {
		wantCode(t, err, codes.NotFound)
	}

	list, err := s.List("b1")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("list = %d", len(list))
	}

	if err := s.Delete("b1", created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	wantCode(t, s.Delete("b1", created.ID), codes.NotFound)
}

// "10" must come after "9", which lexical ordering gets wrong.
func TestListIsInNumericOrder(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for i := 0; i < 11; i++ {
		if _, err := s.Create("b1", Config{Topic: fmt.Sprintf("projects/demo/topics/t%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.List("b1")
	if err != nil {
		t.Fatal(err)
	}
	if list[len(list)-1].ID != "11" {
		t.Errorf("last ID = %q, want 11; the listing is ordered lexically", list[len(list)-1].ID)
	}
}

// --- matching ----------------------------------------------------------

// An empty event_types list means every type, which is what the API
// documents. Treating it as "none" would make a configuration created with
// defaults silently deliver nothing.
func TestEmptyEventTypesMatchesEverything(t *testing.T) {
	t.Parallel()
	c := Config{Topic: topicA}
	for _, e := range KnownEventTypes() {
		if !c.Matches(e, "any/object.txt") {
			t.Errorf("an unfiltered configuration did not match %s", e)
		}
	}
}

func TestEventTypeAndPrefixFilters(t *testing.T) {
	t.Parallel()
	c := Config{
		Topic:            topicA,
		EventTypes:       []EventType{EventFinalize},
		ObjectNamePrefix: "uploads/",
	}

	if !c.Matches(EventFinalize, "uploads/a.txt") {
		t.Error("a matching event was filtered out")
	}
	if c.Matches(EventDelete, "uploads/a.txt") {
		t.Error("a configuration filtered to FINALIZE matched a DELETE")
	}
	if c.Matches(EventFinalize, "other/a.txt") {
		t.Error("the prefix filter was ignored")
	}
}

func TestMatchingReturnsEveryApplicableConfiguration(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if _, err := s.Create("b1", Config{Topic: topicA}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("b1", Config{Topic: topicB, EventTypes: []EventType{EventFinalize}}); err != nil {
		t.Fatal(err)
	}

	matches, err := s.Matching("b1", EventFinalize, "x.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 {
		t.Errorf("FINALIZE matched %d configurations, want 2", len(matches))
	}

	matches, err = s.Matching("b1", EventDelete, "x.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Errorf("DELETE matched %d configurations, want 1", len(matches))
	}
}

// --- router ------------------------------------------------------------

type fakeSource struct {
	messages []Message
}

func (f *fakeSource) Receive(ctx context.Context, fn func(Message)) error {
	for _, m := range f.messages {
		fn(m)
	}
	<-ctx.Done()
	return ctx.Err()
}

type fakePublisher struct {
	mu   sync.Mutex
	sent []published
	err  error
}

type published struct {
	topic string
	data  []byte
	attrs map[string]string
}

func (f *fakePublisher) Publish(_ context.Context, topic string, data []byte, attrs map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, published{topic: topic, data: data, attrs: attrs})
	return nil
}

func (f *fakePublisher) all() []published {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]published(nil), f.sent...)
}

func event(bucket, object string, e EventType) Message {
	return Message{
		Attributes: map[string]string{
			"bucketId": bucket, "objectId": object,
			"eventType": string(e), "payloadFormat": "JSON_API_V1",
			"objectGeneration": "1", "eventTime": "2026-09-21T12:00:00Z",
		},
		Data: []byte(`{"kind":"storage#object","name":"` + object + `"}`),
	}
}

func runRouter(t *testing.T, s *Store, msgs []Message, pub *fakePublisher) *Router {
	t.Helper()
	r := NewRouter(s, &fakeSource{messages: msgs}, pub, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := r.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		_ = r.Stop(stopCtx)
	})
	return r
}

// Without routing, a caller's notificationConfig would be recorded and never
// deliver anything: the backend publishes to one fixed topic.
func TestRouterDeliversToTheConfiguredTopic(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if _, err := s.Create("b1", Config{Topic: topicA}); err != nil {
		t.Fatal(err)
	}

	pub := &fakePublisher{}
	r := runRouter(t, s, []Message{event("b1", "a.txt", EventFinalize)}, pub)
	if !r.WaitRouted(1, 3*time.Second) {
		t.Fatal("nothing was routed")
	}

	sent := pub.all()
	if len(sent) != 1 {
		t.Fatalf("published %d messages", len(sent))
	}
	if sent[0].topic != topicA {
		t.Errorf("topic = %q", sent[0].topic)
	}
	if sent[0].attrs["eventType"] != string(EventFinalize) {
		t.Errorf("attributes were not carried through: %v", sent[0].attrs)
	}
	if !strings.Contains(string(sent[0].data), "a.txt") {
		t.Errorf("payload = %s", sent[0].data)
	}
	if !strings.Contains(sent[0].attrs["notificationConfig"], "/notificationConfigs/1") {
		t.Errorf("notificationConfig attribute = %q", sent[0].attrs["notificationConfig"])
	}
}

func TestRouterFansOutToEveryMatchingConfiguration(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, topic := range []string{topicA, topicB} {
		if _, err := s.Create("b1", Config{Topic: topic}); err != nil {
			t.Fatal(err)
		}
	}

	pub := &fakePublisher{}
	r := runRouter(t, s, []Message{event("b1", "a.txt", EventFinalize)}, pub)
	if !r.WaitRouted(2, 3*time.Second) {
		t.Fatalf("routed %d of 2", len(pub.all()))
	}

	topics := map[string]bool{}
	for _, p := range pub.all() {
		topics[p.topic] = true
	}
	if !topics[topicA] || !topics[topicB] {
		t.Errorf("not every configured topic received the event: %v", topics)
	}
}

func TestRouterRespectsFilters(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if _, err := s.Create("b1", Config{
		Topic: topicA, EventTypes: []EventType{EventDelete}, ObjectNamePrefix: "tmp/",
	}); err != nil {
		t.Fatal(err)
	}

	pub := &fakePublisher{}
	r := runRouter(t, s, []Message{
		event("b1", "tmp/a.txt", EventFinalize), // wrong type
		event("b1", "keep/a.txt", EventDelete),  // wrong prefix
		event("b2", "tmp/a.txt", EventDelete),   // wrong bucket
		event("b1", "tmp/a.txt", EventDelete),   // matches
	}, pub)
	if !r.WaitRouted(1, 3*time.Second) {
		t.Fatal("the matching event was not routed")
	}
	// Give any mistaken deliveries a chance to arrive before asserting.
	time.Sleep(100 * time.Millisecond)

	sent := pub.all()
	if len(sent) != 1 {
		t.Fatalf("published %d messages, want exactly the matching one: %+v", len(sent), sent)
	}
	if sent[0].attrs["objectId"] != "tmp/a.txt" {
		t.Errorf("the wrong event was delivered: %v", sent[0].attrs)
	}
}

// A configuration that set eventType would make the message lie about what
// happened.
func TestCustomAttributesCannotOverwriteStandardOnes(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if _, err := s.Create("b1", Config{
		Topic: topicA,
		CustomAttributes: map[string]string{
			"team":      "platform",
			"eventType": "OBJECT_DELETE",
			"bucketId":  "somewhere-else",
		},
	}); err != nil {
		t.Fatal(err)
	}

	pub := &fakePublisher{}
	r := runRouter(t, s, []Message{event("b1", "a.txt", EventFinalize)}, pub)
	if !r.WaitRouted(1, 3*time.Second) {
		t.Fatal("nothing was routed")
	}

	attrs := pub.all()[0].attrs
	if attrs["eventType"] != string(EventFinalize) {
		t.Errorf("a custom attribute overwrote eventType: %q", attrs["eventType"])
	}
	if attrs["bucketId"] != "b1" {
		t.Errorf("a custom attribute overwrote bucketId: %q", attrs["bucketId"])
	}
	if attrs["team"] != "platform" {
		t.Errorf("the custom attribute was not applied: %v", attrs)
	}
}

// NONE means attributes only. Sending the body anyway would make the setting
// meaningless.
func TestPayloadFormatNoneSendsNoBody(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if _, err := s.Create("b1", Config{Topic: topicA, PayloadFormat: PayloadNone}); err != nil {
		t.Fatal(err)
	}

	pub := &fakePublisher{}
	r := runRouter(t, s, []Message{event("b1", "a.txt", EventFinalize)}, pub)
	if !r.WaitRouted(1, 3*time.Second) {
		t.Fatal("nothing was routed")
	}

	sent := pub.all()[0]
	if len(sent.data) != 0 {
		t.Errorf("payload_format NONE still sent a body: %s", sent.data)
	}
	if sent.attrs["payloadFormat"] != string(PayloadNone) {
		t.Errorf("payloadFormat attribute = %q", sent.attrs["payloadFormat"])
	}
}

// A bucket with no configuration is the normal case, and must not be counted
// as a failure — an operator reading the statistics would go looking for a
// broken delivery that never happened.
func TestUnconfiguredBucketIsDroppedNotFailed(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	pub := &fakePublisher{}
	r := runRouter(t, s, []Message{event("no-config", "a.txt", EventFinalize)}, pub)

	deadline := time.Now().Add(2 * time.Second)
	for {
		_, dropped, failures := r.Stats()
		if dropped == 1 {
			if failures != 0 {
				t.Errorf("failures = %d, want 0 for a bucket with no configuration", failures)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the event was neither routed nor dropped: %d dropped", dropped)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestPublishFailureIsCountedNotSwallowed(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if _, err := s.Create("b1", Config{Topic: topicA}); err != nil {
		t.Fatal(err)
	}

	var reported []error
	var mu sync.Mutex
	pub := &fakePublisher{err: errors.New("topic does not exist")}
	r := NewRouter(s, &fakeSource{messages: []Message{event("b1", "a.txt", EventFinalize)}}, pub,
		func(err error) { mu.Lock(); reported = append(reported, err); mu.Unlock() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Stop(context.Background()) }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, _, failures := r.Stats(); failures > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a publish failure was not counted")
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reported) == 0 {
		t.Error("a publish failure was not reported")
	} else if !strings.Contains(reported[0].Error(), "topic does not exist") {
		t.Errorf("the cause was lost: %v", reported[0])
	}
}

func TestMessageWithNoBucketIsReported(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	var got []error
	var mu sync.Mutex
	r := NewRouter(s, &fakeSource{messages: []Message{{Attributes: map[string]string{}}}},
		&fakePublisher{}, func(err error) { mu.Lock(); got = append(got, err); mu.Unlock() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Stop(context.Background()) }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("a message with no bucketId was silently ignored")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRouterNeedsASourceAndAPublisher(t *testing.T) {
	t.Parallel()
	if err := NewRouter(newTestStore(t), nil, &fakePublisher{}, nil).Start(context.Background()); err == nil {
		t.Error("a router with no source started")
	}
	if err := NewRouter(newTestStore(t), &fakeSource{}, nil, nil).Start(context.Background()); err == nil {
		t.Error("a router with no publisher started")
	}
}

// --- REST --------------------------------------------------------------

func newHandler(t *testing.T) (*httptest.Server, *Store, *httptest.Server) {
	t.Helper()
	// A stand-in backend, so the test can prove that unmatched requests are
	// forwarded rather than swallowed.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("backend:" + r.URL.Path))
	}))
	t.Cleanup(backend.Close)

	u, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	s := newTestStore(t)
	front := httptest.NewServer(NewHandler(s, u))
	t.Cleanup(front.Close)
	return front, s, backend
}

func req(t *testing.T, srv *httptest.Server, method, path, body string) (int, string) {
	t.Helper()
	var rdr *strings.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	var r *http.Request
	var err error
	if rdr != nil {
		r, err = http.NewRequest(method, srv.URL+path, rdr)
	} else {
		r, err = http.NewRequest(method, srv.URL+path, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// Publishing notificationConfigs on a second port would mean a client could
// reach the rest of the API and not this, which is a shape no Google endpoint
// has.
func TestUnmatchedRequestsReachTheBackend(t *testing.T) {
	t.Parallel()
	front, _, _ := newHandler(t)

	code, body := req(t, front, http.MethodGet, "/storage/v1/b/mybucket/o/thing.txt", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d: %s", code, body)
	}
	if !strings.HasPrefix(body, "backend:") {
		t.Errorf("the request did not reach the backend: %q", body)
	}
}

func TestNotificationConfigsCRUDOverREST(t *testing.T) {
	t.Parallel()
	front, _, _ := newHandler(t)

	code, body := req(t, front, http.MethodPost, "/storage/v1/b/mybucket/notificationConfigs",
		`{"topic":"projects/demo/topics/a","payload_format":"JSON_API_V1","event_types":["OBJECT_FINALIZE"]}`)
	if code != http.StatusOK {
		t.Fatalf("create = %d: %s", code, body)
	}
	var created jsonConfig
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("decode: %v\n%s", err, body)
	}
	if created.Kind != "storage#notification" {
		t.Errorf("kind = %q", created.Kind)
	}
	if created.ID == "" || created.SelfLink == "" {
		t.Errorf("incomplete resource: %+v", created)
	}

	code, body = req(t, front, http.MethodGet,
		"/storage/v1/b/mybucket/notificationConfigs/"+created.ID, "")
	if code != http.StatusOK {
		t.Fatalf("get = %d: %s", code, body)
	}

	code, body = req(t, front, http.MethodGet, "/storage/v1/b/mybucket/notificationConfigs", "")
	if code != http.StatusOK {
		t.Fatalf("list = %d: %s", code, body)
	}
	if !strings.Contains(body, "storage#notifications") {
		t.Errorf("listing kind is wrong: %s", body)
	}

	// Cloud Storage returns 204 with no body for a delete.
	code, body = req(t, front, http.MethodDelete,
		"/storage/v1/b/mybucket/notificationConfigs/"+created.ID, "")
	if code != http.StatusNoContent {
		t.Errorf("delete = %d, want 204: %s", code, body)
	}
	if code, _ := req(t, front, http.MethodGet,
		"/storage/v1/b/mybucket/notificationConfigs/"+created.ID, ""); code != http.StatusNotFound {
		t.Errorf("the configuration survived deletion: %d", code)
	}
}

func TestRESTErrorsUseGoogleStatusCodes(t *testing.T) {
	t.Parallel()
	front, _, _ := newHandler(t)

	for _, tc := range []struct {
		name, method, path, body string
		want                     int
	}{
		{"missing", http.MethodGet, "/storage/v1/b/b1/notificationConfigs/99", "", http.StatusNotFound},
		{"bad topic", http.MethodPost, "/storage/v1/b/b1/notificationConfigs", `{"topic":"nope"}`, http.StatusBadRequest},
		{"malformed", http.MethodPost, "/storage/v1/b/b1/notificationConfigs", `{`, http.StatusBadRequest},
		{"unknown field", http.MethodPost, "/storage/v1/b/b1/notificationConfigs",
			`{"topic":"projects/d/topics/a","nosuch":1}`, http.StatusBadRequest},
		{"wrong method", http.MethodPut, "/storage/v1/b/b1/notificationConfigs/1", "", http.StatusBadRequest},
	} {
		code, body := req(t, front, tc.method, tc.path, tc.body)
		if code != tc.want {
			t.Errorf("%s: %d, want %d: %s", tc.name, code, tc.want, body)
		}
	}
}

func TestNotificationPathMatching(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		path        string
		bucket, id  string
		shouldMatch bool
	}{
		{"/storage/v1/b/b1/notificationConfigs", "b1", "", true},
		{"/storage/v1/b/b1/notificationConfigs/7", "b1", "7", true},
		{"/storage/v1/b/b1/notificationconfigs/7", "b1", "7", true},
		{"/storage/v1/b/b1/o/thing.txt", "", "", false},
		{"/storage/v1/b/b1/notificationConfigs/7/extra", "", "", false},
		{"/b1/object.txt", "", "", false},
	} {
		bucket, id, ok := notificationConfigsPath(tc.path)
		if ok != tc.shouldMatch {
			t.Errorf("%s matched = %v, want %v", tc.path, ok, tc.shouldMatch)
			continue
		}
		if ok && (bucket != tc.bucket || id != tc.id) {
			t.Errorf("%s -> bucket %q id %q, want %q/%q", tc.path, bucket, id, tc.bucket, tc.id)
		}
	}
}

func TestResetRemovesEveryConfiguration(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if _, err := s.Create("b1", Config{Topic: topicA}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("b2", Config{Topic: topicB}); err != nil {
		t.Fatal(err)
	}
	if err := s.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	for _, b := range []string{"b1", "b2"} {
		list, err := s.List(b)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 0 {
			t.Errorf("%s still has %d configurations", b, len(list))
		}
	}
}

func TestTopicHelpers(t *testing.T) {
	t.Parallel()
	full := InternalTopicName("proj", "events")
	if full != "projects/proj/topics/events" {
		t.Errorf("InternalTopicName = %q", full)
	}
	if TopicID(full) != "events" {
		t.Errorf("TopicID = %q", TopicID(full))
	}
	if TopicProject(full) != "proj" {
		t.Errorf("TopicProject = %q", TopicProject(full))
	}
	if TopicProject("nonsense") != "" {
		t.Error("TopicProject invented a project")
	}
}

// The official Go client sends the service-qualified form. Accepting only the
// bare one rejected every notification a real SDK created.
func TestTopicAcceptsBothSpellingsAndStoresOne(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	qualified, err := s.Create("b1", Config{Topic: ServicePrefix + topicA})
	if err != nil {
		t.Fatalf("the service-qualified form was rejected: %v", err)
	}
	if qualified.Topic != topicA {
		t.Errorf("stored topic = %q, want the bare form %q", qualified.Topic, topicA)
	}

	// The two spellings are the same configuration, so the second is a
	// duplicate — otherwise a client could register the same delivery twice
	// by changing only how it spelled the topic.
	_, err = s.Create("b1", Config{Topic: topicA})
	wantCode(t, err, codes.AlreadyExists)

	if got := QualifiedTopic(topicA); got != ServicePrefix+topicA {
		t.Errorf("QualifiedTopic = %q", got)
	}
}

// A client parses the topic back out of the response, so it must come back in
// the form it sent.
func TestRESTReportsTheQualifiedTopic(t *testing.T) {
	t.Parallel()
	front, _, _ := newHandler(t)

	code, body := req(t, front, http.MethodPost, "/storage/v1/b/b1/notificationConfigs",
		`{"topic":"`+ServicePrefix+topicA+`","payload_format":"JSON_API_V1"}`)
	if code != http.StatusOK {
		t.Fatalf("create = %d: %s", code, body)
	}
	var created jsonConfig
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatal(err)
	}
	if created.Topic != ServicePrefix+topicA {
		t.Errorf("topic = %q, want the service-qualified form", created.Topic)
	}
}
