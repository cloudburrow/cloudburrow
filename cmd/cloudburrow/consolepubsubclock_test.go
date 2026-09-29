package main

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	pubsub "cloud.google.com/go/pubsub/v2"
	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/durationpb"
)

func TestParseAdvance(t *testing.T) {
	for raw, want := range map[string]time.Duration{
		"1d": 24 * time.Hour, " 31d ": 31 * 24 * time.Hour, "36h": 36 * time.Hour, "1d12h30m5s": 36*time.Hour + 30*time.Minute + 5*time.Second,
		"90m": 90 * time.Minute, "5s": 5 * time.Second,
	} {
		if got, err := parseAdvance(raw); err != nil || got != want {
			t.Errorf("parseAdvance(%q) = %v, %v; want %v", raw, got, err, want)
		}
	}
	re := regexp.MustCompile(advancePattern)
	for _, raw := range []string{"", "0s", "0d0h", "-1d", "1.5d", "1h1d", "1 day", "99999999999999d"} {
		if _, err := parseAdvance(raw); err == nil {
			t.Errorf("parseAdvance(%q) was accepted", raw)
		}
	}
	for _, raw := range []string{"1d", "1d12h30m5s", "0s"} {
		if !re.MatchString(raw) {
			t.Errorf("the form's pattern refuses %q", raw)
		}
	}
	for _, raw := range []string{"1h1d", "-1d", "1.5d"} {
		if re.MatchString(raw) {
			t.Errorf("the form's pattern takes %q", raw)
		}
	}
}

// TestPubSubAdvanceClockReportsWhatExpired (#1040), through the real front
// before an in-memory Pub/Sub: Advance clock on a subscription's page is
// offered with its CloudBurrow-extension label and a confirmation that says
// the clock is the whole instance's and moves only forward. Advancing by a
// day expires a subscription of another project that was idle for its
// one-day period and not one with a two-day period, and the answer names
// the one that expired and the front's new clock; the page's own
// subscription, with a two-day period and idle 23h (the page's read is not
// activity, #1039), is kept. Without the front it is refused, saying why.
func TestPubSubAdvanceClockReportsWhatExpired(t *testing.T) {
	front, addr := pubsubBehindFront(t)
	ctx := context.Background()
	client := func(project string) *pubsub.Client {
		c, err := pubsub.NewClient(ctx, project, option.WithEndpoint(addr), option.WithoutAuthentication(),
			option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	mk := func(project, id string, ttl time.Duration) string {
		c := client(project)
		topic := "projects/" + project + "/topics/t"
		_, _ = c.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic})
		name := "projects/" + project + "/subscriptions/" + id
		if _, err := c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: name, Topic: topic,
			ExpirationPolicy: &pubsubpb.ExpirationPolicy{Ttl: durationpb.New(ttl)}, MessageRetentionDuration: durationpb.New(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		return name
	}
	page := mk("clock-a", "page", 48*time.Hour)
	other := mk("clock-b", "short", 24*time.Hour)
	long := mk("clock-b", "long", 48*time.Hour)
	front.SetIdle(map[string]time.Duration{page: 23 * time.Hour, other: 23 * time.Hour, long: 23 * time.Hour})

	p := pubsubSubscriptionsProvider{endpoint: addr}
	var offered bool
	for _, a := range p.DetailActions(ctx, "clock-a", []string{page}) {
		if a.ID == actAdvanceClock {
			offered = a.Label == "Advance clock (CloudBurrow extension)" &&
				strings.Contains(a.Confirm, "whole instance") && strings.Contains(a.Confirm, "never be moved back")
		}
	}
	if !offered {
		t.Fatal("the subscription's page does not offer Advance clock, labelled and confirmed")
	}
	before := time.Now()
	res, err := p.ActAtResult(ctx, "clock-a", []string{page}, actAdvanceClock, map[string]string{"duration": "1d"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 1 || res.Items[0].Name != other || res.Items[0].Fields["Project"] != "clock-b" {
		t.Errorf("the advance reports %+v expired, want only %s", res.Items, other)
	}
	if !strings.Contains(res.Note, "advanced by 1d") || !strings.Contains(res.Note, "1 subscription was idle") ||
		strings.Contains(res.Note, "its page is gone") {
		t.Errorf("the note is %q", res.Note)
	}
	clock := strings.TrimSuffix(strings.Fields(strings.TrimPrefix(res.Note, "The Pub/Sub front's clock is now "))[0], ":")
	if at, err := time.Parse(time.RFC3339, clock); err != nil || at.Before(before.Add(24*time.Hour-time.Second)) {
		t.Errorf("the note names the clock %q (%v), want a day ahead", clock, err)
	}
	subs := func(project string) map[string]bool {
		out := map[string]bool{}
		it := client(project).SubscriptionAdminClient.ListSubscriptions(ctx, &pubsubpb.ListSubscriptionsRequest{Project: "projects/" + project})
		for s, err := it.Next(); err == nil; s, err = it.Next() {
			out[s.GetName()] = true
		}
		return out
	}
	if b := subs("clock-b"); b[other] || !b[long] {
		t.Errorf("after the advance clock-b holds %v; want %s gone and %s kept", b, other, long)
	}
	if !subs("clock-a")[page] {
		t.Errorf("the page's own subscription, read by the page, expired")
	}

	if _, err := p.ActAtResult(ctx, "clock-a", []string{page}, actAdvanceClock, map[string]string{"duration": "0s"}); err == nil {
		t.Error("an advance of 0s was accepted")
	}
	bare, _ := pubsubEditFixture(t, "clock-a")
	if _, err := (pubsubSubscriptionsProvider{endpoint: bare}).ActAtResult(ctx, "clock-a", []string{page}, actAdvanceClock,
		map[string]string{"duration": "1d"}); err == nil || !strings.Contains(err.Error(), "no CloudBurrow front") {
		t.Errorf("an advance without the front = %v", err)
	}
}
