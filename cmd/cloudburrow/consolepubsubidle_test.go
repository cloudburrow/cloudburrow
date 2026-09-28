package main

import (
	"context"
	"strings"
	"testing"
	"time"

	pubsub "cloud.google.com/go/pubsub/v2"
	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// expiryGroup is a subscription page's Expiration group, by label.
func expiryGroup(t *testing.T, d console.Detail) map[string]string {
	t.Helper()
	for _, sec := range d.Sections {
		for _, g := range sec.Groups {
			if sec.ID == "configuration" && g.Heading == "Expiration" {
				out := map[string]string{}
				for _, p := range g.Properties {
					out[p.Label] = p.Value
				}
				return out
			}
		}
	}
	t.Fatalf("the page has no Expiration group: %+v", d.Sections)
	return nil
}

// TestPubSubSubscriptionPageShowsIdleTimeAndExpiry (#996), through the real
// front before an in-memory Pub/Sub: a subscription's page shows its ttl,
// how long it was idle until the page read it (the front's clock, advanced,
// included), and when expiration deletes it, a ttl after the page's read,
// which the front counts as activity; a policy with no period never
// deletes it; without the front the idle time is not known and says why.
func TestPubSubSubscriptionPageShowsIdleTimeAndExpiry(t *testing.T) {
	front, addr := pubsubBehindFront(t)
	ctx := context.Background()
	const project = "idle-proj"
	c, err := pubsub.NewClient(ctx, project, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	topic := "projects/" + project + "/topics/t"
	if _, err := c.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatal(err)
	}
	sub := "projects/" + project + "/subscriptions/two-days"
	kept := "projects/" + project + "/subscriptions/kept"
	for name, p := range map[string]*pubsubpb.ExpirationPolicy{
		sub: {Ttl: durationpb.New(48 * time.Hour)}, kept: {},
	} {
		if _, err := c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{
			Name: name, Topic: topic, ExpirationPolicy: p, MessageRetentionDuration: durationpb.New(24 * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	front.SetIdle(map[string]time.Duration{sub: 5 * time.Hour})
	now := front.Advance(ctx, 3*time.Hour)

	p := pubsubSubscriptionsProvider{endpoint: addr}
	d, err := p.Detail(ctx, project, []string{sub})
	if err != nil {
		t.Fatal(err)
	}
	g := expiryGroup(t, d)
	if g["Expiration period"] != "2d" || g["Idle for"] != "8h, until this page read it" {
		t.Errorf("the Expiration group is %v; want 2d, idle 8h", g)
	}
	when := g["Deleted by expiration"]
	at, err := time.Parse(time.RFC3339, strings.Fields(when)[0])
	if err != nil || at.Sub(now.Add(48*time.Hour)).Abs() > 5*time.Second || !strings.Contains(when, "(in 2d)") ||
		!strings.Contains(when, "clock restarted") || !strings.Contains(when, "3h ahead of this machine's") {
		t.Errorf("Deleted by expiration is %q (%v); want 2d after %s, the restart and the advanced clock", when, err, now)
	}
	if idle := front.Idle()[sub]; idle > time.Minute {
		t.Errorf("after the page's read the front counts %v idle; the read is activity", idle)
	}

	d, err = p.Detail(ctx, project, []string{kept})
	if err != nil {
		t.Fatal(err)
	}
	if g := expiryGroup(t, d); g["Expiration period"] != "Never expires" || !strings.HasPrefix(g["Deleted by expiration"], "Never") {
		t.Errorf("a policy with no period shows %v", g)
	}

	// Without the front: the in-memory Pub/Sub alone.
	bare, c2 := pubsubEditFixture(t, project)
	if _, err := c2.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatal(err)
	}
	if _, err := c2.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sub, Topic: topic}); err != nil {
		t.Fatal(err)
	}
	d, err = pubsubSubscriptionsProvider{endpoint: bare}.Detail(ctx, project, []string{sub})
	if err != nil {
		t.Fatal(err)
	}
	if g := expiryGroup(t, d); g["Expiration period"] != "31d" || !strings.Contains(g["Idle for"], "no CloudBurrow front") ||
		g["Deleted by expiration"] != "" {
		t.Errorf("without the front the group is %v; want Google's default and the idle time unknown", g)
	}
}

func TestFormatIdle(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0: "0s", -time.Second: "0s", 45 * time.Second: "45s", 90 * time.Minute: "1h30m",
		26*time.Hour + 5*time.Second: "1d2h5s", 31 * 24 * time.Hour: "31d",
	} {
		if got := formatIdle(d); got != want {
			t.Errorf("formatIdle(%v) = %q, want %q", d, got, want)
		}
	}
}
