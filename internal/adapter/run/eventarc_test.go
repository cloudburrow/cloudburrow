package run

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

func TestPubSubPushBecomesMessagePublishedCloudEvent(t *testing.T) {
	spec := &functionspb.EventTrigger{EventType: pubsubPublished, PubsubTopic: "projects/p/topics/t"}
	body := `{"message":{"data":"aGk=","attributes":{"k":"v"},"messageId":"42","publishTime":"2026-10-16T00:00:00Z"},"subscription":"projects/p/subscriptions/s"}`
	ev, err := ToCloudEvent(spec, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"ce-id": "42", "ce-source": "//pubsub.googleapis.com/projects/p/topics/t",
		"ce-type": pubsubPublished, "ce-specversion": "1.0", "ce-time": "2026-10-16T00:00:00Z", "Content-Type": "application/json"} {
		if got := ev.Headers.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	var data struct {
		Message struct {
			Data, MessageID, MessageIDSnake string
			Attributes                      map[string]string
		}
		Subscription string
	}
	var raw map[string]map[string]any
	_ = json.Unmarshal(ev.Data, &raw)
	if err := json.Unmarshal(ev.Data, &data); err != nil || data.Subscription != "projects/p/subscriptions/s" ||
		data.Message.Data != "aGk=" || data.Message.Attributes["k"] != "v" || raw["message"]["message_id"] != "42" {
		t.Errorf("data = %s", ev.Data)
	}
}

func TestStorageNotificationBecomesObjectCloudEvent(t *testing.T) {
	spec := &functionspb.EventTrigger{EventType: storageEventPrefix + "finalized",
		EventFilters: []*functionspb.EventFilter{{Attribute: "bucket", Value: "b"}}}
	obj := `{"bucket":"b","name":"dir/o.txt","size":"3"}`
	body := `{"message":{"data":"` + base64.StdEncoding.EncodeToString([]byte(obj)) + `","attributes":{"eventType":"OBJECT_FINALIZE","bucketId":"b","objectId":"dir/o.txt","eventTime":"2026-10-16T01:00:00Z"},"messageId":"7"},"subscription":"projects/p/subscriptions/s"}`
	ev, err := ToCloudEvent(spec, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"ce-subject": "objects/dir/o.txt", "ce-source": "//storage.googleapis.com/projects/_/buckets/b",
		"ce-type": storageEventPrefix + "finalized", "ce-time": "2026-10-16T01:00:00Z", "ce-bucket": "b", "ce-id": "7"} {
		if got := ev.Headers.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if string(ev.Data) != obj {
		t.Errorf("data = %s, want the object", ev.Data)
	}
	if _, err := ToCloudEvent(spec, []byte("not json")); err == nil {
		t.Error("a body that is not a push converted")
	}
}

func TestEventTriggersAreChecked(t *testing.T) {
	off := &FunctionsServer{}
	if err := off.checkEventTrigger(&functionspb.EventTrigger{EventType: pubsubPublished, PubsubTopic: "projects/p/topics/t"}); apierror.From(err).Code != codes.Unimplemented ||
		!strings.Contains(err.Error(), "pubsub") {
		t.Errorf("without Pub/Sub = %v; want UNIMPLEMENTED naming the services", err)
	}
	on := (&FunctionsServer{}).WithEvents(Events{})
	for _, c := range []struct {
		trig *functionspb.EventTrigger
		code codes.Code
	}{
		{&functionspb.EventTrigger{EventType: pubsubPublished, PubsubTopic: "projects/p/topics/t"}, codes.OK},
		{&functionspb.EventTrigger{EventType: pubsubPublished, PubsubTopic: "t"}, codes.InvalidArgument},
		{&functionspb.EventTrigger{EventType: storageEventPrefix + "deleted", EventFilters: []*functionspb.EventFilter{{Attribute: "bucket", Value: "b"}}}, codes.OK},
		{&functionspb.EventTrigger{EventType: storageEventPrefix + "finalized"}, codes.InvalidArgument},
		{&functionspb.EventTrigger{EventType: storageEventPrefix + "finalized", EventFilters: []*functionspb.EventFilter{{Attribute: "bucket", Value: "b"}, {Attribute: "name", Value: "x"}}}, codes.Unimplemented},
		{&functionspb.EventTrigger{EventType: "google.cloud.audit.log.v1.written"}, codes.Unimplemented},
		{&functionspb.EventTrigger{EventType: pubsubPublished, PubsubTopic: "projects/p/topics/t", Channel: "c"}, codes.Unimplemented},
	} {
		err := on.checkEventTrigger(c.trig)
		if got := codes.OK; err != nil {
			got = apierror.From(err).Code
			if got != c.code {
				t.Errorf("%v = %v; want %v", c.trig, err, c.code)
			}
		} else if c.code != codes.OK {
			t.Errorf("%v accepted; want %v", c.trig, c.code)
		}
	}
}

type fakeTransport struct{ calls []string }

func (f *fakeTransport) CreateTopic(_ context.Context, topic string, _ map[string]string) error {
	f.calls = append(f.calls, "topic "+topic)
	return nil
}
func (f *fakeTransport) DeleteTopic(_ context.Context, topic string) error {
	f.calls = append(f.calls, "-topic "+topic)
	return nil
}
func (f *fakeTransport) CreatePushSubscription(_ context.Context, s PushSubscription) error {
	f.calls = append(f.calls, "sub "+s.Name+" "+s.Topic+" "+s.Endpoint+" "+s.Labels["goog-managed-by"])
	return nil
}
func (f *fakeTransport) DeleteSubscription(_ context.Context, name string) error {
	f.calls = append(f.calls, "-sub "+name)
	return nil
}
func (f *fakeTransport) CreateNotification(_ context.Context, bucket, topic, eventType string) (string, error) {
	f.calls = append(f.calls, "notification "+bucket+" "+eventType)
	return "3", nil
}
func (f *fakeTransport) DeleteNotification(_ context.Context, bucket, id string) error {
	f.calls = append(f.calls, "-notification "+bucket+" "+id)
	return nil
}

type fakeTokens struct{ account, audience string }

func (f *fakeTokens) ServiceAccountIDToken(account, audience string) (string, error) {
	f.account, f.audience = account, audience
	return "tok", nil
}

// A trigger is made with Eventarc's names, a push reaches the function as a
// CloudEvent with the account's token, a failure is retried only under
// RETRY, and the function's delete tears the transport down.
func TestStorageTriggerDeliversAndTearsDown(t *testing.T) {
	var got *http.Request
	var gotBody string
	status := http.StatusOK
	fnSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(status)
	}))
	defer fnSrv.Close()
	tr, tokens := &fakeTransport{}, &fakeTokens{}
	f := (&FunctionsServer{}).WithEvents(Events{Transport: tr, Tokens: tokens, PushBase: func() string { return "http://host:1" },
		Ingress: strings.TrimPrefix(fnSrv.URL, "http://")})
	name := "projects/p/locations/us-central1/functions/fn"
	fn := &functionspb.Function{Name: name, ServiceConfig: &functionspb.ServiceConfig{Uri: "http://fn.ns.cloudburrow.localhost",
		ServiceAccountEmail: "sa@p.iam.gserviceaccount.com"},
		EventTrigger: &functionspb.EventTrigger{EventType: storageEventPrefix + "finalized",
			EventFilters: []*functionspb.EventFilter{{Attribute: "bucket", Value: "b"}}}}
	defaultEventTrigger(fn)
	functions.mu.Lock()
	functions.fns[name] = &storedFunction{fn: fn}
	functions.mu.Unlock()
	t.Cleanup(func() { functions.mu.Lock(); delete(functions.fns, name); functions.mu.Unlock() })
	if err := f.syncTrigger(context.Background(), name); err != nil {
		t.Fatal(err)
	}
	trigger := fn.GetEventTrigger().GetTrigger()
	if !strings.HasPrefix(trigger, "projects/p/locations/us-central1/triggers/fn-") || len(tr.calls) != 3 ||
		!strings.HasPrefix(tr.calls[0], "topic projects/p/topics/eventarc-us-central1-fn-") ||
		tr.calls[1] != "notification b OBJECT_FINALIZE" ||
		!strings.HasPrefix(tr.calls[2], "sub projects/p/subscriptions/eventarc-us-central1-fn-") ||
		!strings.HasSuffix(tr.calls[2], " http://host:1/cloudburrow/eventarc/push/"+trigger+" eventarc") {
		t.Fatalf("trigger %q made %q", trigger, tr.calls)
	}
	if fn.GetEventTrigger().GetRetryPolicy() != functionspb.EventTrigger_RETRY_POLICY_DO_NOT_RETRY ||
		fn.GetEventTrigger().GetServiceAccountEmail() != "sa@p.iam.gserviceaccount.com" || fn.GetEventTrigger().GetTriggerRegion() != "us-central1" {
		t.Errorf("defaults = %v", fn.GetEventTrigger())
	}

	obj := `{"bucket":"b","name":"o"}`
	push := func() int {
		body := `{"message":{"data":"` + base64.StdEncoding.EncodeToString([]byte(obj)) + `","attributes":{"bucketId":"b","objectId":"o"},"messageId":"1"},"subscription":"s"}`
		rec := httptest.NewRecorder()
		f.push(rec, httptest.NewRequest(http.MethodPost, eventarcPushPrefix+trigger, strings.NewReader(body)))
		return rec.Code
	}
	if code := push(); code != http.StatusNoContent || got == nil || got.Host != "fn.ns.cloudburrow.localhost" ||
		got.Header.Get("ce-subject") != "objects/o" || got.Header.Get("Authorization") != "Bearer tok" || gotBody != obj {
		t.Fatalf("push = %d; the function got %v %q", code, got, gotBody)
	}
	if tokens.account != "sa@p.iam.gserviceaccount.com" || tokens.audience != "http://fn.ns.cloudburrow.localhost" {
		t.Errorf("token for %q, audience %q", tokens.account, tokens.audience)
	}
	status = http.StatusInternalServerError
	if code := push(); code != http.StatusNoContent {
		t.Errorf("a failed delivery under DO_NOT_RETRY = %d; want it acknowledged", code)
	}
	fn.EventTrigger.RetryPolicy = functionspb.EventTrigger_RETRY_POLICY_RETRY
	functions.fns[name].trigger.spec.RetryPolicy = functionspb.EventTrigger_RETRY_POLICY_RETRY
	if code := push(); code != http.StatusInternalServerError {
		t.Errorf("a failed delivery under RETRY = %d; want the function's 500", code)
	}

	tr.calls = nil
	f.teardown(context.Background(), functions.fns[name].trigger)
	if len(tr.calls) != 3 || !strings.HasPrefix(tr.calls[0], "-sub ") || tr.calls[1] != "-notification b 3" || !strings.HasPrefix(tr.calls[2], "-topic ") {
		t.Errorf("teardown = %q", tr.calls)
	}
}
