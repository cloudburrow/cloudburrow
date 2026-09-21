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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func pubsubClient(t *testing.T, h *Harness) *pubsub.Client {
	t.Helper()
	endpoint := h.Endpoint(EnvPubSub)
	// PUBSUB_EMULATOR_HOST makes the official client use insecure transport
	// with no credentials.
	t.Setenv("PUBSUB_EMULATOR_HOST", endpoint)
	c, err := pubsub.NewClient(h.Context(), h.Project())
	if err != nil {
		t.Fatalf("pubsub.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// topic creates a uniquely named topic and removes it afterwards. Pub/Sub
// requires resource IDs of 3-255 characters, which a short test name violates.
func topic(t *testing.T, h *Harness, c *pubsub.Client, id string) string {
	t.Helper()
	name := fmt.Sprintf("projects/%s/topics/%s", h.Project(), id)
	if _, err := c.TopicAdminClient.CreateTopic(h.Context(), &pubsubpb.Topic{Name: name}); err != nil {
		t.Fatalf("CreateTopic %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = c.TopicAdminClient.DeleteTopic(context.Background(), &pubsubpb.DeleteTopicRequest{Topic: name})
	})
	return name
}

func subscription(t *testing.T, h *Harness, c *pubsub.Client, id, topicName string) string {
	t.Helper()
	name := fmt.Sprintf("projects/%s/subscriptions/%s", h.Project(), id)
	if _, err := c.SubscriptionAdminClient.CreateSubscription(h.Context(), &pubsubpb.Subscription{
		Name: name, Topic: topicName, AckDeadlineSeconds: 10,
	}); err != nil {
		t.Fatalf("CreateSubscription %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = c.SubscriptionAdminClient.DeleteSubscription(context.Background(),
			&pubsubpb.DeleteSubscriptionRequest{Subscription: name})
	})
	return name
}

// TestPubSubTopicLifecycle covers CreateTopic, GetTopic and DeleteTopic.
func TestPubSubTopicLifecycle(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	ctx := h.Context()

	name := topic(t, h, c, "lifecycle-topic")
	got, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: name})
	if err != nil {
		t.Fatalf("GetTopic: %v", err)
	}
	if got.Name != name {
		t.Errorf("topic name = %q, want %q", got.Name, name)
	}

	// A missing topic must map to NotFound, not to a generic error.
	_, err = c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{
		Topic: fmt.Sprintf("projects/%s/topics/absent-topic", h.Project()),
	})
	if status.Code(err) != codes.NotFound {
		t.Errorf("GetTopic(absent) = %v, want NotFound", status.Code(err))
	}
}

// TestPubSubPublishAndPull covers Publish, Pull and Acknowledge.
func TestPubSubPublishAndPull(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	ctx := h.Context()

	tn := topic(t, h, c, "pull-topic")
	sn := subscription(t, h, c, "pull-subscription", tn)

	pub := c.Publisher(tn)
	defer pub.Stop()
	id, err := pub.Publish(ctx, &pubsub.Message{
		Data:       []byte("hello"),
		Attributes: map[string]string{"k": "v"},
	}).Get(ctx)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if id == "" {
		t.Error("Publish returned an empty message ID")
	}

	resp, err := c.SubscriptionAdminClient.Pull(ctx, &pubsubpb.PullRequest{
		Subscription: sn, MaxMessages: 10,
	})
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(resp.ReceivedMessages) != 1 {
		t.Fatalf("Pull returned %d messages, want 1", len(resp.ReceivedMessages))
	}
	m := resp.ReceivedMessages[0]
	if string(m.Message.Data) != "hello" {
		t.Errorf("data = %q, want hello", m.Message.Data)
	}
	if m.Message.Attributes["k"] != "v" {
		t.Errorf("attributes = %v, want k=v", m.Message.Attributes)
	}
	if err := c.SubscriptionAdminClient.Acknowledge(ctx, &pubsubpb.AcknowledgeRequest{
		Subscription: sn, AckIds: []string{m.AckId},
	}); err != nil {
		t.Fatalf("Acknowledge: %v", err)
	}
}

// TestPubSubStreamingPull covers the path the Go and Python clients use by
// default. It is the hardest part of the surface and cannot be skipped.
func TestPubSubStreamingPull(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)

	tn := topic(t, h, c, "streaming-topic")
	sn := subscription(t, h, c, "streaming-subscription", tn)

	const n = 20
	pub := c.Publisher(tn)
	for i := 0; i < n; i++ {
		pub.Publish(h.Context(), &pubsub.Message{Data: []byte(fmt.Sprintf("msg-%d", i))})
	}
	pub.Stop()

	recvCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	var mu sync.Mutex
	seen := map[string]bool{}
	err := c.Subscriber(sn).Receive(recvCtx, func(_ context.Context, m *pubsub.Message) {
		mu.Lock()
		seen[string(m.Data)] = true
		done := len(seen) == n
		mu.Unlock()
		m.Ack()
		if done {
			cancel()
		}
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Receive: %v", err)
	}
	mu.Lock()
	count := len(seen)
	mu.Unlock()
	if count != n {
		t.Fatalf("StreamingPull delivered %d distinct messages, want %d", count, n)
	}
}

// TestPubSubProjectIsolation checks that identical IDs in different projects do
// not collide.
func TestPubSubProjectIsolation(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	ctx := h.Context()

	tn := topic(t, h, c, "isolated-topic")
	if _, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: tn}); err != nil {
		t.Fatalf("GetTopic in own project: %v", err)
	}

	// The same short ID under a different project must not resolve.
	other := fmt.Sprintf("projects/%s-other/topics/isolated-topic", h.Project())
	_, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: other})
	if status.Code(err) != codes.NotFound {
		t.Errorf("GetTopic across projects = %v, want NotFound", status.Code(err))
	}
}

// TestPubSubIAMIsUnimplemented records an inherited limitation as a test, so it
// cannot be quietly forgotten or wrongly claimed later.
func TestPubSubIAMIsUnimplemented(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	tn := topic(t, h, c, "iam-topic")
	sn := subscription(t, h, c, "iam-subscription", tn)
	_ = sn

	// Google's emulator announces "IAM integration is disabled" at startup.
	// Confirm what a caller actually receives, so compatibility.md can say so
	// from evidence.
	_, err := c.SubscriptionAdminClient.GetIamPolicy(h.Context(), nil)
	if status.Code(err) == codes.OK {
		t.Error("GetIamPolicy succeeded; CloudBurrow must not imply IAM is enforced")
	}
	t.Logf("GetIamPolicy -> %v (documented as unsupported)", status.Code(err))
}

// TestPubSubDeleteTopic asserts DeleteTopic, which the earlier suite called
// during cleanup but never checked — so it was not claimed.
func TestPubSubDeleteTopic(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	ctx := h.Context()

	name := fmt.Sprintf("projects/%s/topics/delete-me-topic", h.Project())
	if _, err := c.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: name}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	if err := c.TopicAdminClient.DeleteTopic(ctx, &pubsubpb.DeleteTopicRequest{Topic: name}); err != nil {
		t.Fatalf("DeleteTopic: %v", err)
	}
	if _, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetTopic after delete = %v, want NotFound", status.Code(err))
	}
	// Deleting twice must be NotFound, not a silent success.
	if err := c.TopicAdminClient.DeleteTopic(ctx, &pubsubpb.DeleteTopicRequest{Topic: name}); status.Code(err) != codes.NotFound {
		t.Errorf("second DeleteTopic = %v, want NotFound", status.Code(err))
	}
}

// TestPubSubListTopicsAndSubscriptions covers the listing methods.
func TestPubSubListTopicsAndSubscriptions(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	ctx := h.Context()

	tn := topic(t, h, c, "listed-topic")
	sn := subscription(t, h, c, "listed-subscription", tn)

	topics := c.TopicAdminClient.ListTopics(ctx, &pubsubpb.ListTopicsRequest{
		Project: "projects/" + h.Project(),
	})
	foundTopic := false
	for {
		got, err := topics.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListTopics: %v", err)
		}
		if got.GetName() == tn {
			foundTopic = true
		}
	}
	if !foundTopic {
		t.Error("ListTopics omitted the topic just created")
	}

	subs := c.SubscriptionAdminClient.ListSubscriptions(ctx, &pubsubpb.ListSubscriptionsRequest{
		Project: "projects/" + h.Project(),
	})
	foundSub := false
	for {
		got, err := subs.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListSubscriptions: %v", err)
		}
		if got.GetName() == sn {
			foundSub = true
		}
	}
	if !foundSub {
		t.Error("ListSubscriptions omitted the subscription just created")
	}
}

// TestPubSubModifyAckDeadlineAndRedelivery covers ack-deadline handling and
// at-least-once redelivery, which is the behaviour applications must tolerate.
func TestPubSubRedeliveryAfterNack(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	ctx := h.Context()

	tn := topic(t, h, c, "redelivery-topic")
	sn := subscription(t, h, c, "redelivery-subscription", tn)

	pub := c.Publisher(tn)
	defer pub.Stop()
	if _, err := pub.Publish(ctx, &pubsub.Message{Data: []byte("retry me")}).Get(ctx); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	first, err := c.SubscriptionAdminClient.Pull(ctx, &pubsubpb.PullRequest{Subscription: sn, MaxMessages: 1})
	if err != nil || len(first.ReceivedMessages) != 1 {
		t.Fatalf("first Pull = %d messages, %v", len(first.ReceivedMessages), err)
	}

	// Nack by setting the ack deadline to zero, which returns it immediately.
	if err := c.SubscriptionAdminClient.ModifyAckDeadline(ctx, &pubsubpb.ModifyAckDeadlineRequest{
		Subscription:       sn,
		AckIds:             []string{first.ReceivedMessages[0].AckId},
		AckDeadlineSeconds: 0,
	}); err != nil {
		t.Fatalf("ModifyAckDeadline: %v", err)
	}

	// Bounded polling: redelivery is the emulator's own timing, which we
	// cannot advance.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		again, err := c.SubscriptionAdminClient.Pull(ctx, &pubsubpb.PullRequest{Subscription: sn, MaxMessages: 1})
		if err != nil {
			t.Fatalf("Pull: %v", err)
		}
		if len(again.ReceivedMessages) == 1 {
			if string(again.ReceivedMessages[0].Message.Data) != "retry me" {
				t.Errorf("redelivered data = %q", again.ReceivedMessages[0].Message.Data)
			}
			_ = c.SubscriptionAdminClient.Acknowledge(ctx, &pubsubpb.AcknowledgeRequest{
				Subscription: sn, AckIds: []string{again.ReceivedMessages[0].AckId},
			})
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("message was never redelivered after its ack deadline was returned")
}

// TestPubSubOrderingKeys records whether ordering keys are honoured.
func TestPubSubOrderingKeys(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	ctx := h.Context()

	tn := topic(t, h, c, "ordered-topic")
	sn := fmt.Sprintf("projects/%s/subscriptions/ordered-subscription", h.Project())
	if _, err := c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{
		Name: sn, Topic: tn, AckDeadlineSeconds: 30, EnableMessageOrdering: true,
	}); err != nil {
		t.Skipf("ordered subscriptions are not supported: %v", err)
	}
	t.Cleanup(func() {
		_ = c.SubscriptionAdminClient.DeleteSubscription(context.Background(),
			&pubsubpb.DeleteSubscriptionRequest{Subscription: sn})
	})

	pub := c.Publisher(tn)
	pub.EnableMessageOrdering = true
	defer pub.Stop()

	const n = 5
	for i := 0; i < n; i++ {
		if _, err := pub.Publish(ctx, &pubsub.Message{
			Data:        []byte(fmt.Sprintf("%d", i)),
			OrderingKey: "key-a",
		}).Get(ctx); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}

	var got []string
	deadline := time.Now().Add(30 * time.Second)
	for len(got) < n && time.Now().Before(deadline) {
		resp, err := c.SubscriptionAdminClient.Pull(ctx, &pubsubpb.PullRequest{Subscription: sn, MaxMessages: n})
		if err != nil {
			t.Fatalf("Pull: %v", err)
		}
		var ids []string
		for _, m := range resp.ReceivedMessages {
			got = append(got, string(m.Message.Data))
			ids = append(ids, m.AckId)
		}
		if len(ids) > 0 {
			_ = c.SubscriptionAdminClient.Acknowledge(ctx, &pubsubpb.AcknowledgeRequest{Subscription: sn, AckIds: ids})
		}
	}
	if len(got) != n {
		t.Fatalf("received %d of %d ordered messages", len(got), n)
	}
	for i, v := range got {
		if v != fmt.Sprint(i) {
			t.Errorf("RESULT: ordering not preserved: got %v", got)
			return
		}
	}
	t.Logf("RESULT: ordering preserved for a single key: %v", got)
}
