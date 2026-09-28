package pubsubfront

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/structpb"
)

// Exactly-once delivery is refused on a push or export subscription however
// the pair would come about: created together, a push endpoint given to an
// exactly-once subscription, or exactly-once turned on for a push one. Each
// refusal leaves the subscription as it was.
func TestExactlyOnceIsRefusedWithPush(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	topic := fx.topic(t, "t")
	push := &pubsubpb.PushConfig{PushEndpoint: "http://127.0.0.1:1/push"}
	for _, s := range []*pubsubpb.Subscription{
		{Name: subName("eod-push"), Topic: topic, EnableExactlyOnceDelivery: true, PushConfig: push},
		{Name: subName("eod-bq"), Topic: topic, EnableExactlyOnceDelivery: true,
			BigqueryConfig: &pubsubpb.BigQueryConfig{Table: "p.d.t"}},
	} {
		_, err := fx.create(t, s)
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "pull subscriptions") {
			t.Errorf("create %s = %v, want INVALID_ARGUMENT", s.Name, err)
		}
		if fx.listed(t)[s.Name] {
			t.Errorf("%s: refused, and created anyway", s.Name)
		}
	}
	if _, err := fx.create(t, &pubsubpb.Subscription{Name: subName("eod"), Topic: topic, EnableExactlyOnceDelivery: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.create(t, &pubsubpb.Subscription{Name: subName("push"), Topic: topic, PushConfig: push}); err != nil {
		t.Fatal(err)
	}
	update := func(s *pubsubpb.Subscription, paths ...string) error {
		_, err := fx.client.SubscriptionAdminClient.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{
			Subscription: s, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}})
		return err
	}
	if err := update(&pubsubpb.Subscription{Name: subName("eod"), PushConfig: push}, "push_config"); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a push endpoint for an exactly-once subscription = %v, want INVALID_ARGUMENT", err)
	}
	if err := update(&pubsubpb.Subscription{Name: subName("push"), EnableExactlyOnceDelivery: true},
		"enable_exactly_once_delivery"); status.Code(err) != codes.InvalidArgument {
		t.Errorf("exactly-once for a push subscription = %v, want INVALID_ARGUMENT", err)
	}
	if err := fx.client.SubscriptionAdminClient.ModifyPushConfig(ctx, &pubsubpb.ModifyPushConfigRequest{
		Subscription: subName("eod"), PushConfig: push}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("ModifyPushConfig on an exactly-once subscription = %v, want INVALID_ARGUMENT", err)
	}
	for id, eod := range map[string]bool{"eod": true, "push": false} {
		s, err := fx.upstream.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: subName(id)})
		if err != nil || s.GetEnableExactlyOnceDelivery() != eod || (s.GetPushConfig().GetPushEndpoint() != "") == eod {
			t.Errorf("%s after the refusals: %v, %v", id, s, err)
		}
	}
	// Both at once, the other way round, is a pull subscription with it.
	if err := update(&pubsubpb.Subscription{Name: subName("push"), EnableExactlyOnceDelivery: true, PushConfig: &pubsubpb.PushConfig{}},
		"enable_exactly_once_delivery", "push_config"); err != nil {
		t.Errorf("switching to pull with exactly-once = %v", err)
	}
}

// pushTarget is a push endpoint that answers code and records what it got.
type pushTarget struct {
	srv  *httptest.Server
	mu   sync.Mutex
	code int
	got  []string
	hdr  http.Header
}

func newPushTarget(t *testing.T, code int) *pushTarget {
	p := &pushTarget{code: code}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		defer p.mu.Unlock()
		p.got = append(p.got, r.URL.RequestURI()+" "+string(b))
		p.hdr = r.Header.Clone()
		w.Header().Set("X-Target", "yes")
		w.WriteHeader(p.code)
		_, _ = io.WriteString(w, "answer")
	}))
	t.Cleanup(p.srv.Close)
	return p
}

// deliver makes a push the way the emulator does: a POST to the endpoint it
// holds, which is the relay's.
func (fx *fixture) deliver(t *testing.T, sub string) *http.Response {
	t.Helper()
	s, err := fx.upstream.SubscriptionAdminClient.GetSubscription(context.Background(), &pubsubpb.GetSubscriptionRequest{Subscription: sub})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(s.GetPushConfig().GetPushEndpoint(), "application/json", strings.NewReader(`{"message":{"data":"eA=="}}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp
}

// With the relay, the emulator holds the relay's endpoint and every client
// reads the real one; a push reaches the real endpoint with its body, query
// and headers, the endpoint's answer comes back, and a successful push is
// activity that keeps the subscription from expiring while a failed one is
// not.
func TestPushRelayObservesPushes(t *testing.T) {
	fx := newFixtureWith(t, true)
	ctx := context.Background()
	topic := fx.topic(t, "t")
	ok, failing := newPushTarget(t, http.StatusNoContent), newPushTarget(t, http.StatusServiceUnavailable)
	oneDay := &pubsubpb.ExpirationPolicy{Ttl: day(1)}
	endpoint := ok.srv.URL + "/push?token=abc"
	for id, ep := range map[string]string{"ok": endpoint, "failing": failing.srv.URL + "/push", "idle": ok.srv.URL + "/idle"} {
		got, err := fx.create(t, &pubsubpb.Subscription{Name: subName(id), Topic: topic, ExpirationPolicy: oneDay,
			MessageRetentionDuration: day(1), PushConfig: &pubsubpb.PushConfig{PushEndpoint: ep}})
		if err != nil || got.GetPushConfig().GetPushEndpoint() != ep {
			t.Fatalf("create %s = %v, %v; want the real endpoint read back", id, got.GetPushConfig(), err)
		}
	}
	held, err := fx.upstream.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: subName("ok")})
	if err != nil || !strings.HasPrefix(held.GetPushConfig().GetPushEndpoint(), fx.front.relaying()) {
		t.Fatalf("the emulator holds %v, %v; want the relay's endpoint", held.GetPushConfig(), err)
	}
	read, err := fx.client.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: subName("ok")})
	if err != nil || read.GetPushConfig().GetPushEndpoint() != endpoint {
		t.Errorf("GetSubscription reads %v, %v", read.GetPushConfig(), err)
	}
	it := fx.client.SubscriptionAdminClient.ListSubscriptions(ctx, &pubsubpb.ListSubscriptionsRequest{Project: "projects/" + project})
	for {
		s, err := it.Next()
		if err != nil {
			break
		}
		if strings.Contains(s.GetPushConfig().GetPushEndpoint(), relayPath) {
			t.Errorf("ListSubscriptions shows the relay: %v", s.GetPushConfig())
		}
	}
	// An update keeps the real endpoint on the way out and the relay's in.
	upd, err := fx.client.SubscriptionAdminClient.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{
		Subscription: &pubsubpb.Subscription{Name: subName("ok"), PushConfig: &pubsubpb.PushConfig{PushEndpoint: endpoint}},
		UpdateMask:   &fieldmaskpb.FieldMask{Paths: []string{"push_config"}}})
	if err != nil || upd.GetPushConfig().GetPushEndpoint() != endpoint {
		t.Errorf("UpdateSubscription = %v, %v", upd.GetPushConfig(), err)
	}

	resp := fx.deliver(t, subName("ok"))
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("X-Target") != "yes" {
		t.Errorf("the relay answered %d %v, want the endpoint's 204", resp.StatusCode, resp.Header)
	}
	ok.mu.Lock()
	if len(ok.got) != 1 || ok.got[0] != `/push?token=abc {"message":{"data":"eA=="}}` || ok.hdr.Get("Content-Type") != "application/json" {
		t.Errorf("the endpoint got %q with %v", ok.got, ok.hdr)
	}
	ok.mu.Unlock()
	if resp := fx.deliver(t, subName("failing")); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("a failing push answered %d, want the endpoint's 503", resp.StatusCode)
	}

	fx.advance(t, 20*time.Hour)
	fx.deliver(t, subName("ok"))
	fx.deliver(t, subName("failing"))
	fx.advance(t, 5*time.Hour)
	got := fx.listed(t)
	for id, want := range map[string]bool{"ok": true, "failing": false, "idle": false} {
		if got[subName(id)] != want {
			t.Errorf("25h on, %s listed = %v, want %v", id, got[subName(id)], want)
		}
	}
	fx.advance(t, 20*time.Hour)
	if fx.listed(t)[subName("ok")] {
		t.Error("25h after its last successful push, the subscription is still there")
	}
}

// The relay refuses an address it did not make, and reports an endpoint it
// cannot reach as a failed push.
func TestPushRelayRefusesOtherAddresses(t *testing.T) {
	fx := newFixtureWith(t, true)
	base := fx.front.relaying()
	for _, u := range []string{base + "nonsense", base + "!!/!!"} {
		resp, err := http.Post(u, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("POST %s = %d, want 404", u, resp.StatusCode)
		}
	}
	resp, err := http.Post(fx.front.relayEndpoint(subName("s"), "http://127.0.0.1:1/x"), "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("an unreachable endpoint = %d, want 502", resp.StatusCode)
	}
}

// The activity methods read each subscription's idle time and restore it, so
// a restored clock expires the subscription when the saved one would have.
func TestActivityExportAndImport(t *testing.T) {
	fx := newFixture(t)
	topic := fx.topic(t, "t")
	for _, id := range []string{"a", "b"} {
		if _, err := fx.create(t, &pubsubpb.Subscription{Name: subName(id), Topic: topic,
			ExpirationPolicy: &pubsubpb.ExpirationPolicy{Ttl: day(2)}, MessageRetentionDuration: day(1)}); err != nil {
			t.Fatal(err)
		}
	}
	fx.advance(t, 30*time.Hour)
	var st structpb.Struct
	if err := fx.conn.Invoke(context.Background(), ActivityExportMethod, &emptypb.Empty{}, &st); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if v := st.GetFields()[subName(id)].GetNumberValue(); v < 30*3600 || v > 30*3600+60 {
			t.Errorf("%s idle %vs, want 30h", id, v)
		}
	}
	// b is set back to idle for 40h: 8h more and it has been idle 48h.
	in := &structpb.Struct{Fields: map[string]*structpb.Value{subName("b"): structpb.NewNumberValue(40 * 3600)}}
	if err := fx.conn.Invoke(context.Background(), ActivityImportMethod, in, &emptypb.Empty{}); err != nil {
		t.Fatal(err)
	}
	fx.advance(t, 9*time.Hour)
	got := fx.listed(t)
	if !got[subName("a")] || got[subName("b")] {
		t.Errorf("listed %v: want a (39h idle) kept and b (49h idle) expired", got)
	}
	bad := &structpb.Struct{Fields: map[string]*structpb.Value{subName("a"): structpb.NewNumberValue(-1)}}
	if err := fx.conn.Invoke(context.Background(), ActivityImportMethod, bad, &emptypb.Empty{}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a negative idle time = %v, want INVALID_ARGUMENT", err)
	}
}

// ProjectsMethod lists the projects calls have named, from any service, and
// nothing a message's data holds.
func TestProjectsNamedAreListed(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	topic := fx.topic(t, "t")
	if _, err := fx.client.Publisher(topic).Publish(ctx, &pubsub.Message{Data: []byte("projects/in-the-data/topics/x")}).Get(ctx); err != nil {
		t.Fatal(err)
	}
	it := fx.client.TopicAdminClient.ListTopics(ctx, &pubsubpb.ListTopicsRequest{Project: "projects/listed-only"})
	_, _ = it.Next()
	var l structpb.ListValue
	if err := fx.conn.Invoke(ctx, ProjectsMethod, &emptypb.Empty{}, &l); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, v := range l.GetValues() {
		got = append(got, v.GetStringValue())
	}
	if strings.Join(got, ",") != "front-test,listed-only" {
		t.Errorf("projects = %v, want front-test and listed-only", got)
	}
	for p, ok := range map[string]bool{"abcdef": true, "a-1-b-2": true, "abc": false, "Abcdef": false, "abcdef-": false, "1bcdef": false} {
		if validProject(p) != ok {
			t.Errorf("validProject(%q) = %v", p, !ok)
		}
	}
}
