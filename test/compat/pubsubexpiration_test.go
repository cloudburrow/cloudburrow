//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/api/iterator"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/pubsubfront"
)

// advancePubSubClock moves the Pub/Sub front's clock forward (#873), so a
// ttl of a day, the least Google accepts, passes in a moment. It is the one
// CloudBurrow method on the Pub/Sub port, and not part of the Pub/Sub API.
// Every idle subscription of every test sees the same clock, so the tests
// that use it keep their total advance far below the 31-day default ttl.
func advancePubSubClock(t *testing.T, h *Harness, d time.Duration) {
	t.Helper()
	conn, err := grpc.NewClient(h.Endpoint(EnvPubSub), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var now timestamppb.Timestamp
	if err := conn.Invoke(h.Context(), pubsubfront.ClockMethod, durationpb.New(d), &now); err != nil {
		t.Fatalf("advance the Pub/Sub clock by %s: %v", d, err)
	}
}

// listedSubscriptions is the project's subscriptions. Listing names no
// subscription, so it is not activity on any, where GetSubscription would be.
func listedSubscriptions(t *testing.T, h *Harness, c *pubsub.Client) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	it := c.SubscriptionAdminClient.ListSubscriptions(h.Context(), &pubsubpb.ListSubscriptionsRequest{Project: "projects/" + h.Project()})
	for {
		s, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out
		}
		if err != nil {
			t.Fatalf("ListSubscriptions: %v", err)
		}
		out[s.GetName()] = true
	}
}

func days(n int) *durationpb.Duration { return durationpb.New(time.Duration(n) * 24 * time.Hour) }

// TestPubSubSubscriptionExpiresWhenIdle (#873): a subscription with a
// one-day ttl that nothing touches is deleted once it has been idle for a
// day, and reads NOT_FOUND. A pull restarts the clock, an open streaming
// pull holds it, a policy without a ttl never expires, and a subscription
// created with no policy reads back Google's 31-day default. Time passes
// through the front's clock, never by accepting a ttl Google refuses.
// covers: google.pubsub.v1.Subscriber/CreateSubscription, google.pubsub.v1.Subscriber/ListSubscriptions, google.pubsub.v1.Subscriber/GetSubscription
func TestPubSubSubscriptionExpiresWhenIdle(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	ctx := h.Context()
	topicName := topic(t, h, c, "expiry-topic")
	name := func(id string) string { return fmt.Sprintf("projects/%s/subscriptions/expiry-%s", h.Project(), id) }
	oneDay := &pubsubpb.ExpirationPolicy{Ttl: days(1)}
	for _, s := range []*pubsubpb.Subscription{
		{Name: name("idle"), ExpirationPolicy: oneDay, MessageRetentionDuration: days(1)},
		{Name: name("pulled"), ExpirationPolicy: oneDay, MessageRetentionDuration: days(1)},
		{Name: name("streaming"), ExpirationPolicy: oneDay, MessageRetentionDuration: days(1)},
		{Name: name("never"), ExpirationPolicy: &pubsubpb.ExpirationPolicy{}},
		{Name: name("default")},
	} {
		s.Topic = topicName
		if _, err := c.SubscriptionAdminClient.CreateSubscription(ctx, s); err != nil {
			t.Fatalf("CreateSubscription %s: %v", s.Name, err)
		}
		n := s.Name
		t.Cleanup(func() {
			_ = c.SubscriptionAdminClient.DeleteSubscription(context.Background(), &pubsubpb.DeleteSubscriptionRequest{Subscription: n})
		})
	}
	dflt, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name("default")})
	if err != nil || dflt.GetExpirationPolicy().GetTtl().AsDuration() != 31*24*time.Hour {
		t.Errorf("a subscription created with no policy reads %v, %v; want Google's 31-day default", dflt.GetExpirationPolicy(), err)
	}

	// A streaming pull, open until the test ends.
	rctx, stop := context.WithCancel(ctx)
	received := make(chan struct{}, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = c.Subscriber(name("streaming")).Receive(rctx, func(_ context.Context, m *pubsub.Message) {
			m.Ack()
			select {
			case received <- struct{}{}:
			default:
			}
		})
	}()
	t.Cleanup(func() { stop(); wg.Wait() })
	if _, err := c.Publisher(topicName).Publish(ctx, &pubsub.Message{Data: []byte("x")}).Get(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-received:
	case <-time.After(30 * time.Second):
		t.Fatal("the streaming pull received nothing in 30s")
	}

	advancePubSubClock(t, h, 20*time.Hour)
	if resp, err := c.SubscriptionAdminClient.Pull(ctx, &pubsubpb.PullRequest{Subscription: name("pulled"), MaxMessages: 1}); err != nil ||
		len(resp.GetReceivedMessages()) != 1 {
		t.Fatalf("Pull = %v, %v", resp, err)
	}
	advancePubSubClock(t, h, 5*time.Hour)
	got := listedSubscriptions(t, h, c)
	for id, want := range map[string]bool{"idle": false, "pulled": true, "streaming": true, "never": true, "default": true} {
		if got[name(id)] != want {
			t.Errorf("25h after creation, %s listed = %v, want %v", id, got[name(id)], want)
		}
	}
	if _, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name("idle")}); status.Code(err) != codes.NotFound {
		t.Errorf("the expired subscription reads %v, want NOT_FOUND", err)
	}

	advancePubSubClock(t, h, 20*time.Hour)
	got = listedSubscriptions(t, h, c)
	for id, want := range map[string]bool{"pulled": false, "streaming": true, "never": true, "default": true} {
		if got[name(id)] != want {
			t.Errorf("45h after creation, 25h after the pull, %s listed = %v, want %v", id, got[name(id)], want)
		}
	}
}

// TestPubSubRefusesAnExpirationGoogleRefuses (#873): a ttl under a day, and
// one shorter than the subscription's message retention (seven days when
// none is given), are refused and create nothing; a day with a day's
// retention is created and reads back as given.
// covers: google.pubsub.v1.Subscriber/CreateSubscription
// unverified: google.pubsub.v1.Subscriber/CreateSubscription INVALID_ARGUMENT: an expiration_policy.ttl under 1 day, or under message_retention_duration (Google documents the rule, not the code)
func TestPubSubRefusesAnExpirationGoogleRefuses(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	ctx := h.Context()
	topicName := topic(t, h, c, "expiry-rules-topic")
	for _, tc := range []struct {
		id        string
		ttl       time.Duration
		retention *durationpb.Duration
		ok        bool
	}{
		{"twelve-hours", 12 * time.Hour, durationpb.New(time.Hour), false},
		{"under-default-retention", 24 * time.Hour, nil, false},
		{"under-retention", 48 * time.Hour, days(3), false},
		{"one-day", 24 * time.Hour, days(1), true},
	} {
		n := fmt.Sprintf("projects/%s/subscriptions/expiry-rules-%s", h.Project(), tc.id)
		t.Cleanup(func() {
			_ = c.SubscriptionAdminClient.DeleteSubscription(context.Background(), &pubsubpb.DeleteSubscriptionRequest{Subscription: n})
		})
		got, err := c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: n, Topic: topicName,
			ExpirationPolicy: &pubsubpb.ExpirationPolicy{Ttl: durationpb.New(tc.ttl)}, MessageRetentionDuration: tc.retention})
		if !tc.ok {
			if status.Code(err) != codes.InvalidArgument {
				t.Errorf("%s: CreateSubscription = %v, want INVALID_ARGUMENT", tc.id, err)
			}
			if _, gerr := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: n}); status.Code(gerr) != codes.NotFound {
				t.Errorf("%s: refused, and then reads %v", tc.id, gerr)
			}
			continue
		}
		if err != nil || got.GetExpirationPolicy().GetTtl().AsDuration() != tc.ttl {
			t.Errorf("%s: CreateSubscription = %v, %v", tc.id, got.GetExpirationPolicy(), err)
		}
	}
}

// TestPubSubExactlyOnceDelivery (#873): on a subscription with exactly-once
// delivery, the official client's AckWithResult reports success and the
// acknowledged message is not delivered again after its deadline; a message
// left unacknowledged is redelivered with a new ack ID once its deadline
// passes, and the old ack ID is then refused INVALID_ARGUMENT with the
// PERMANENT_FAILURE_INVALID_ACK_ID status the exactly-once documentation
// names, while the new one is accepted. The emulator implements this itself;
// the front passes it through.
// covers: google.pubsub.v1.Subscriber/StreamingPull, google.pubsub.v1.Subscriber/Acknowledge, google.pubsub.v1.Subscriber/Pull
func TestPubSubExactlyOnceDelivery(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	ctx := h.Context()
	topicName := topic(t, h, c, "eod-topic")
	create := func(id string) string {
		n := fmt.Sprintf("projects/%s/subscriptions/%s", h.Project(), id)
		s, err := c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: n, Topic: topicName,
			AckDeadlineSeconds: 10, EnableExactlyOnceDelivery: true})
		if err != nil || !s.GetEnableExactlyOnceDelivery() {
			t.Fatalf("CreateSubscription %s = %v, %v", n, s, err)
		}
		t.Cleanup(func() {
			_ = c.SubscriptionAdminClient.DeleteSubscription(context.Background(), &pubsubpb.DeleteSubscriptionRequest{Subscription: n})
		})
		return n
	}
	acked, stale := create("eod-acked"), create("eod-stale")
	if _, err := c.Publisher(topicName).Publish(ctx, &pubsub.Message{Data: []byte("once")}).Get(ctx); err != nil {
		t.Fatal(err)
	}

	t.Run("acknowledged", func(t *testing.T) {
		rctx, stop := context.WithTimeout(ctx, 30*time.Second)
		defer stop()
		var result pubsub.AcknowledgeStatus = -1
		var resErr error
		err := c.Subscriber(acked).Receive(rctx, func(_ context.Context, m *pubsub.Message) {
			result, resErr = m.AckWithResult().Get(rctx)
			stop()
		})
		if err != nil {
			t.Fatalf("Receive: %v", err)
		}
		if result != pubsub.AcknowledgeStatusSuccess || resErr != nil {
			t.Fatalf("AckWithResult = %v, %v; want success", result, resErr)
		}
		// Past the 10s deadline: an acknowledged message is not resent.
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			resp, _ := c.SubscriptionAdminClient.Pull(pctx, &pubsubpb.PullRequest{Subscription: acked, MaxMessages: 1, ReturnImmediately: true})
			cancel()
			if n := len(resp.GetReceivedMessages()); n > 0 {
				t.Fatalf("the acknowledged message was delivered again: %v", resp.GetReceivedMessages())
			}
			time.Sleep(time.Second)
		}
	})

	t.Run("stale ack ID", func(t *testing.T) {
		first, err := c.SubscriptionAdminClient.Pull(ctx, &pubsubpb.PullRequest{Subscription: stale, MaxMessages: 1})
		if err != nil || len(first.GetReceivedMessages()) != 1 {
			t.Fatalf("Pull = %v, %v", first, err)
		}
		m1 := first.GetReceivedMessages()[0]
		// Not resent while outstanding; resent, with a new ack ID, once the
		// deadline passes. Polled, since the emulator keeps the time.
		start := time.Now()
		var m2 *pubsubpb.ReceivedMessage
		for m2 == nil && time.Since(start) < 30*time.Second {
			pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			resp, _ := c.SubscriptionAdminClient.Pull(pctx, &pubsubpb.PullRequest{Subscription: stale, MaxMessages: 1, ReturnImmediately: true})
			cancel()
			if len(resp.GetReceivedMessages()) > 0 {
				m2 = resp.GetReceivedMessages()[0]
				if d := time.Since(start); d < 8*time.Second {
					t.Errorf("redelivered after %s, before the 10s deadline", d)
				}
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if m2 == nil {
			t.Fatal("not redelivered within 30s of a 10s deadline")
		}
		if m2.GetMessage().GetMessageId() != m1.GetMessage().GetMessageId() || m2.GetAckId() == m1.GetAckId() {
			t.Fatalf("redelivered %v after %v; want the same message under a new ack ID", m2, m1)
		}
		err = c.SubscriptionAdminClient.Acknowledge(ctx, &pubsubpb.AcknowledgeRequest{Subscription: stale, AckIds: []string{m1.GetAckId()}})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("acknowledging the stale ack ID = %v, want INVALID_ARGUMENT", err)
		}
		var info *errdetails.ErrorInfo
		for _, d := range status.Convert(err).Details() {
			if i, ok := d.(*errdetails.ErrorInfo); ok {
				info = i
			}
		}
		if info == nil || info.GetMetadata()[m1.GetAckId()] != "PERMANENT_FAILURE_INVALID_ACK_ID" {
			t.Errorf("the refusal's ErrorInfo is %v; want %s marked PERMANENT_FAILURE_INVALID_ACK_ID", info, m1.GetAckId())
		}
		if err := c.SubscriptionAdminClient.Acknowledge(ctx, &pubsubpb.AcknowledgeRequest{Subscription: stale, AckIds: []string{m2.GetAckId()}}); err != nil {
			t.Errorf("acknowledging the current ack ID = %v", err)
		}
	})
}
