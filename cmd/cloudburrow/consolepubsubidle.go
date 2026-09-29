package main

// A subscription's idle time and when expiration deletes it (#996).
//
// The emulator stores an expiration policy and never acts on it; CloudBurrow's
// front (internal/pubsubfront, #873) keeps each subscription's last activity
// and deletes one idle for its ttl. It exposes those clocks through two
// CloudBurrow methods on the Pub/Sub port, which `cloudburrow state save`
// reads too: SubscriptionActivity/Export, each subscription's seconds idle,
// and EmulatorClock/Advance, which with a zero duration reads the front's
// clock (the wall clock plus whatever a test advanced it by). The front does
// not count a GetSubscription as activity (#1039): Google names "open
// connections, active pulls, or successful pushes" as subscriber activity, so
// the page's own read leaves the clock alone, and the deletion it names is a
// ttl after the last activity, if nothing names the subscription until then.
// The front sweeps every 30 seconds, so the deletion follows within one
// sweep of that time.

import (
	"context"
	"fmt"
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

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/pubsubfront"
)

// subscriptionActivity is what the front reported of one subscription,
// read before the page's GetSubscription.
type subscriptionActivity struct {
	// err is why the clocks could not be read: a Pub/Sub endpoint without
	// the front answers UNIMPLEMENTED.
	err error
	// known is whether the front had a clock for the subscription, and
	// idle how long it had been idle.
	known bool
	idle  time.Duration
	// now is the front's clock, and skew how far it is ahead of this
	// machine's.
	now  time.Time
	skew time.Duration
}

// readSubscriptionActivity reads the front's clock and one subscription's
// idle time.
func readSubscriptionActivity(ctx context.Context, endpoint, name string) subscriptionActivity {
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return subscriptionActivity{err: err}
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var now timestamppb.Timestamp
	before := time.Now()
	if err := conn.Invoke(ctx, pubsubfront.ClockMethod, durationpb.New(0), &now); err != nil {
		return subscriptionActivity{err: err}
	}
	var idle structpb.Struct
	if err := conn.Invoke(ctx, pubsubfront.ActivityExportMethod, &emptypb.Empty{}, &idle); err != nil {
		return subscriptionActivity{err: err}
	}
	a := subscriptionActivity{now: now.AsTime(), skew: now.AsTime().Sub(before)}
	if v, ok := idle.GetFields()[name]; ok {
		a.known = true
		a.idle = time.Duration(v.GetNumberValue() * float64(time.Second)).Round(time.Second)
	}
	return a
}

// subscriptionExpiryGroup is the page's Expiration group: the ttl, how long
// the subscription was idle, and when expiration deletes it.
func subscriptionExpiryGroup(s *pubsubpb.Subscription, a subscriptionActivity) console.PropertyGroup {
	g := console.PropertyGroup{Heading: "Expiration"}
	add := func(label, value string) {
		g.Properties = append(g.Properties, console.Property{Label: label, Value: value})
	}
	ttl, expires := pubsubfront.DefaultTTL, true
	if p := s.GetExpirationPolicy(); p != nil {
		ttl = p.GetTtl().AsDuration()
		expires = ttl > 0
	}
	if expires {
		add("Expiration period", formatPubSubDuration(durationpb.New(ttl)))
	} else {
		add("Expiration period", "Never expires")
	}
	if a.err != nil {
		reason := "the Pub/Sub front did not answer: " + a.err.Error()
		if status.Code(a.err) == codes.Unimplemented {
			reason = "this Pub/Sub endpoint has no CloudBurrow front, which keeps the expiration clocks"
		}
		add("Idle for", "Not known: "+reason)
		return g
	}
	if a.known {
		add("Idle for", formatIdle(a.idle))
	} else {
		add("Idle for", "Not recorded: the front had seen no call naming it")
	}
	if !expires {
		add("Deleted by expiration", "Never: its expiration policy has no period")
		return g
	}
	left := ttl
	if a.known {
		left = max(ttl-a.idle, 0)
	}
	at := a.now.Add(left).UTC()
	when := at.Format(time.RFC3339) + " (in " + formatPubSubDuration(durationpb.New(left)) + "), unless something " +
		"names it before then. Opening this page is not activity, so it did not restart the clock."
	if s.GetPushConfig().GetPushEndpoint() != "" {
		when += " A successful push is activity too."
	}
	if a.skew > time.Minute {
		when += fmt.Sprintf(" By the Pub/Sub front's clock, which was advanced and is %s ahead of this machine's.",
			formatIdle(a.skew.Round(time.Second)))
	}
	add("Deleted by expiration", when)
	return g
}

// formatIdle is a duration in days, hours, minutes and seconds, largest
// first, leaving out the zero ones: 1d2h, 45s, 0s.
func formatIdle(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	out := ""
	for _, u := range []struct {
		unit time.Duration
		name string
	}{{24 * time.Hour, "d"}, {time.Hour, "h"}, {time.Minute, "m"}, {time.Second, "s"}} {
		if n := d / u.unit; n > 0 {
			out += fmt.Sprintf("%d%s", n, u.name)
			d -= n * u.unit
		}
	}
	if out == "" {
		return "0s"
	}
	return out
}
