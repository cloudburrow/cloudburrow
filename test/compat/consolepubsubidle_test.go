//go:build compat

package compat

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/pubsubfront"
)

// TestConsolePubSubIdleTimeAndPushAttributes (#996).
//
// Create subscription on a topic's page sets push attributes with the push
// endpoint, which the official client reads back; attributes without an
// endpoint are refused and nothing is made. A subscription's page shows its
// expiration period, how long it was idle until the page read it (the
// front's activity record, after its clock is advanced two hours) and when
// expiration deletes it: a ttl after the page's read, by the front's clock,
// since the read is activity, which the front's record then shows.
//
// covers: google.pubsub.v1.Subscriber/CreateSubscription, google.pubsub.v1.Subscriber/GetSubscription
func TestConsolePubSubIdleTimeAndPushAttributes(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	ps := pubsubClient(t, h)
	ctx := h.Context()
	project := h.Project()
	topic := "projects/" + project + "/topics/console-idle"
	if _, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.TopicAdminClient.DeleteTopic(ctx, &pubsubpb.DeleteTopicRequest{Topic: topic}) })
	push := "projects/" + project + "/subscriptions/console-idle-push"
	idle := "projects/" + project + "/subscriptions/console-idle-pull"
	for _, s := range []string{push, idle} {
		t.Cleanup(func() {
			_ = ps.SubscriptionAdminClient.DeleteSubscription(ctx, &pubsubpb.DeleteSubscriptionRequest{Subscription: s})
		})
	}

	if code, body := consoleAct(t, addr, "pubsub", project, []string{topic}, "create-subscription", map[string]string{
		"name": "console-idle-push", "pushEndpoint": "http://127.0.0.1:1/push",
		"pushAttributes": `{"x-goog-version":"v1","team":"data"}`,
	}); code != http.StatusOK {
		t.Fatalf("console create push subscription = %d: %s", code, body)
	}
	s, err := ps.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: push})
	if err != nil {
		t.Fatal(err)
	}
	if a := s.GetPushConfig().GetAttributes(); s.GetPushConfig().GetPushEndpoint() != "http://127.0.0.1:1/push" ||
		len(a) != 2 || a["x-goog-version"] != "v1" || a["team"] != "data" {
		t.Errorf("the console's push subscription reads back %v", s.GetPushConfig())
	}
	if code, body := consoleAct(t, addr, "pubsub", project, []string{topic}, "create-subscription", map[string]string{
		"name": "console-idle-bad", "pushAttributes": `{"x-goog-version":"v1"}`,
	}); code != http.StatusBadRequest || !strings.Contains(consoleError(t, body), "push attributes need a push endpoint") {
		t.Errorf("push attributes without an endpoint = %d %s", code, body)
	}

	if code, body := consoleAct(t, addr, "pubsub", project, []string{topic}, "create-subscription", map[string]string{
		"name": "console-idle-pull", "messageRetention": "1d", "expiration": "2d",
	}); code != http.StatusOK {
		t.Fatalf("console create pull subscription = %d: %s", code, body)
	}
	conn, err := grpc.NewClient(h.Endpoint(EnvPubSub), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	idleSeconds := func() float64 {
		t.Helper()
		var st structpb.Struct
		if err := conn.Invoke(ctx, pubsubfront.ActivityExportMethod, &emptypb.Empty{}, &st); err != nil {
			t.Fatalf("read the front's activity: %v", err)
		}
		v, ok := st.GetFields()[idle]
		if !ok {
			t.Fatalf("the front has no clock for %s", idle)
		}
		return v.GetNumberValue()
	}
	advancePubSubClock(t, h, 2*time.Hour)
	if n := idleSeconds(); n < 7200 {
		t.Fatalf("after a 2h advance the front counts %vs idle", n)
	}

	var page struct {
		Sections []struct {
			ID     string
			Groups []struct {
				Heading    string
				Properties []struct{ Label, Value string }
			}
		}
	}
	consoleJSON(t, addr, http.MethodGet, "/api/detail/pubsub-subscriptions?project="+url.QueryEscape(project)+
		"&name="+url.QueryEscape(idle), "", &page)
	var now timestamppb.Timestamp
	if err := conn.Invoke(ctx, pubsubfront.ClockMethod, durationpb.New(0), &now); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, sec := range page.Sections {
		for _, g := range sec.Groups {
			if sec.ID == "configuration" && g.Heading == "Expiration" {
				for _, p := range g.Properties {
					got[p.Label] = p.Value
				}
			}
		}
	}
	if got["Expiration period"] != "2d" {
		t.Errorf("the page shows the expiration period %q, want 2d", got["Expiration period"])
	}
	if !regexp.MustCompile(`^(\d+d)?\d+h(\d+m)?(\d+s)?, until this page read it$`).MatchString(got["Idle for"]) {
		t.Errorf("the page shows idle for %q, want the 2h or more the front counted", got["Idle for"])
	}
	when := got["Deleted by expiration"]
	at, err := time.Parse(time.RFC3339, strings.Fields(when + " ")[0])
	if err != nil || at.Sub(now.AsTime().Add(48*time.Hour)).Abs() > time.Minute || !strings.Contains(when, "clock restarted") {
		t.Errorf("the page shows deletion by expiration %q (%v); want 2d after the page's read, %s", when, err, now.AsTime())
	}
	if n := idleSeconds(); n > 60 {
		t.Errorf("after the page's read the front counts %vs idle; the read is activity", n)
	}
}
