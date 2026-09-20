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
