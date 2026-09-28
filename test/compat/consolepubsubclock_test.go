//go:build compat

package compat

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/pubsubfront"
)

// TestConsolePubSubAdvanceClock (#1040).
//
// A subscription's page offers Advance clock (CloudBurrow extension), whose
// confirmation says the clock is the whole instance's and only moves
// forward. Through the console API, advancing by an hour calls the Pub/Sub
// front's EmulatorClock/Advance: a subscription with a one-day expiration
// period that had been idle for 23h30m (set through the front's activity
// records) expires, and the answer names it and the front's new clock; one
// idle as long with a two-day period, and the page's own subscription, are
// kept. Read back with the official Go client: the expired subscription is
// NOT_FOUND and the other two are there, and the front's clock reads at
// least what the answer said. An advance of 0s is refused and moves nothing.
//
// covers: google.pubsub.v1.Subscriber/ListSubscriptions, google.pubsub.v1.Subscriber/GetSubscription
func TestConsolePubSubAdvanceClock(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	ps := pubsubClient(t, h)
	ctx := h.Context()
	project := h.Project()
	topic := "projects/" + project + "/topics/console-clock"
	if _, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.TopicAdminClient.DeleteTopic(ctx, &pubsubpb.DeleteTopicRequest{Topic: topic}) })
	sub := func(id string, ttl time.Duration) string {
		name := "projects/" + project + "/subscriptions/" + id
		s := &pubsubpb.Subscription{Name: name, Topic: topic, MessageRetentionDuration: durationpb.New(time.Hour)}
		if ttl > 0 {
			s.ExpirationPolicy = &pubsubpb.ExpirationPolicy{Ttl: durationpb.New(ttl)}
		}
		if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, s); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = ps.SubscriptionAdminClient.DeleteSubscription(ctx, &pubsubpb.DeleteSubscriptionRequest{Subscription: name})
		})
		return name
	}
	page := sub("console-clock-page", 0)
	due := sub("console-clock-due", 24*time.Hour)
	long := sub("console-clock-long", 48*time.Hour)

	conn, err := grpc.NewClient(h.Endpoint(EnvPubSub), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	idle := 23*time.Hour + 30*time.Minute
	if err := conn.Invoke(ctx, pubsubfront.ActivityImportMethod, &structpb.Struct{Fields: map[string]*structpb.Value{
		due: structpb.NewNumberValue(idle.Seconds()), long: structpb.NewNumberValue(idle.Seconds()),
	}}, &emptypb.Empty{}); err != nil {
		t.Fatalf("set the subscriptions' idle time: %v", err)
	}

	var detail struct {
		Actions []struct {
			ID, Label, Confirm string
		}
	}
	consoleJSON(t, addr, http.MethodGet, "/api/detail/pubsub-subscriptions?project="+url.QueryEscape(project)+"&name="+url.QueryEscape(page), "", &detail)
	offered := false
	for _, a := range detail.Actions {
		if a.ID == "advance-clock" {
			offered = a.Label == "Advance clock (CloudBurrow extension)" &&
				strings.Contains(a.Confirm, "whole instance") && strings.Contains(a.Confirm, "never be moved back")
		}
	}
	if !offered {
		t.Fatalf("the subscription's page offers %+v, want Advance clock labelled and confirmed", detail.Actions)
	}

	code, out := consoleAct(t, addr, "pubsub-subscriptions", project, []string{page}, "advance-clock", map[string]string{"duration": "0s"})
	if code != http.StatusBadRequest || !strings.Contains(consoleError(t, out), "only moves forward") {
		t.Errorf("an advance of 0s = %d %s", code, out)
	}

	code, out = consoleAct(t, addr, "pubsub-subscriptions", project, []string{page}, "advance-clock", map[string]string{"duration": "1h"})
	if code != http.StatusOK {
		t.Fatalf("Advance clock = %d: %s", code, out)
	}
	var res struct {
		Result struct {
			Note  string
			Items []struct {
				Name   string
				Fields map[string]string
			}
		}
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	expired := map[string]bool{}
	for _, it := range res.Result.Items {
		expired[it.Name] = true
	}
	if !expired[due] || expired[long] || expired[page] {
		t.Errorf("the advance reports %v expired, want %s and neither %s nor %s", expired, due, long, page)
	}
	note := res.Result.Note
	if !strings.Contains(note, "advanced by 1h") || !strings.HasPrefix(note, "The Pub/Sub front's clock is now ") {
		t.Fatalf("the note is %q", note)
	}
	said, err := time.Parse(time.RFC3339, strings.TrimSuffix(strings.Fields(strings.TrimPrefix(note, "The Pub/Sub front's clock is now "))[0], ":"))
	if err != nil {
		t.Fatalf("the note's clock: %v (%q)", err, note)
	}
	var now timestamppb.Timestamp
	if err := conn.Invoke(ctx, pubsubfront.ClockMethod, durationpb.New(0), &now); err != nil {
		t.Fatal(err)
	}
	if now.AsTime().Before(said.Add(-time.Second)) || !now.AsTime().After(time.Now().Add(59*time.Minute)) {
		t.Errorf("the front's clock reads %s after the answer said %s", now.AsTime(), said)
	}

	if _, err := ps.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: due}); status.Code(err) != codes.NotFound {
		t.Errorf("the expired subscription reads %v, want NOT_FOUND", err)
	}
	for _, name := range []string{long, page} {
		if !listedSubscriptions(t, h, ps)[name] {
			t.Errorf("%s is gone after the advance", name)
		}
	}
}
