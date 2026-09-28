//go:build compat

package compat

import (
	"context"
	"strings"
	"testing"
	"time"

	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// covers: google.pubsub.v1.Publisher/CreateTopic, google.pubsub.v1.Publisher/ListTopics, google.pubsub.v1.Publisher/Publish, google.pubsub.v1.Subscriber/CreateSubscription, google.pubsub.v1.Subscriber/GetSubscription, google.pubsub.v1.Subscriber/UpdateSubscription, google.pubsub.v1.Subscriber/Pull, google.pubsub.v1.Subscriber/Acknowledge, google.pubsub.v1.Subscriber/DeleteSubscription, google.pubsub.v1.Publisher/DeleteTopic
//
// TestGcloudPubSub (#873): through the configuration `cloudburrow
// gcloud-setup` writes and nothing else, the real gcloud, which speaks
// Pub/Sub's REST API, creates and lists a topic on the Pub/Sub port the
// gRPC clients use (the front in front of the emulator serves both);
// `subscriptions create --expiration-period=1h` is refused, as Google
// refuses a ttl under a day, and `subscriptions update
// --message-retention-duration` above the ttl is refused, as Google refuses
// a ttl shorter than the retention; a subscription created with no
// expiration period describes with Google's 31-day default; and gcloud
// publishes, pulls with --auto-ack and deletes.
// The official gRPC client reads back what gcloud created.
func TestGcloudPubSub(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	g := newGcloudSession(t, h)
	ctx := h.Context()
	p := "--project=" + h.Project()
	topicName := "projects/" + h.Project() + "/topics/gcloud-ps-topic"
	subName := func(id string) string { return "projects/" + h.Project() + "/subscriptions/gcloud-ps-" + id }
	t.Cleanup(func() {
		for _, id := range []string{"hour", "kept", "default"} {
			_ = c.SubscriptionAdminClient.DeleteSubscription(context.Background(), &pubsubpb.DeleteSubscriptionRequest{Subscription: subName(id)})
		}
		_ = c.TopicAdminClient.DeleteTopic(context.Background(), &pubsubpb.DeleteTopicRequest{Topic: topicName})
	})

	g.must("pubsub", "topics", "create", "gcloud-ps-topic", p)
	if got := g.must("pubsub", "topics", "list", p, "--format=value(name)"); !strings.Contains(got, topicName) {
		t.Errorf("gcloud pubsub topics list does not show %s:\n%s", topicName, got)
	}
	if _, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: topicName}); err != nil {
		t.Errorf("the topic gcloud created reads %v over gRPC", err)
	}

	out, err := g.run(nil, "pubsub", "subscriptions", "create", "gcloud-ps-hour", p, "--topic=gcloud-ps-topic",
		"--expiration-period=1h", "--message-retention-duration=10m")
	if err == nil || !strings.Contains(out, "at least 1 day") {
		t.Errorf("subscriptions create --expiration-period=1h = %v:\n%s\nwant a refusal naming the 1-day minimum", err, out)
	}
	if _, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: subName("hour")}); status.Code(err) != codes.NotFound {
		t.Errorf("the refused subscription reads %v, want NOT_FOUND", err)
	}

	g.must("pubsub", "subscriptions", "create", "gcloud-ps-kept", p, "--topic=gcloud-ps-topic",
		"--expiration-period=2d", "--message-retention-duration=1d")
	s, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: subName("kept")})
	if err != nil || s.GetExpirationPolicy().GetTtl().AsDuration() != 48*time.Hour {
		t.Errorf("--expiration-period=2d reads back as %v, %v", s.GetExpirationPolicy(), err)
	}
	out, err = g.run(nil, "pubsub", "subscriptions", "update", "gcloud-ps-kept", p, "--message-retention-duration=3d")
	if err == nil || !strings.Contains(out, "must be at least as long as message_retention_duration") {
		t.Errorf("subscriptions update --message-retention-duration=3d, above the 2d ttl = %v:\n%s\nwant a refusal", err, out)
	}

	g.must("pubsub", "subscriptions", "create", "gcloud-ps-default", p, "--topic=gcloud-ps-topic")
	if got := strings.TrimSpace(g.must("pubsub", "subscriptions", "describe", "gcloud-ps-default", p,
		"--format=value(expirationPolicy.ttl)")); got != "2678400s" {
		t.Errorf("a subscription created with no expiration period describes a ttl of %q, want Google's 31 days (2678400s)", got)
	}

	g.must("pubsub", "topics", "publish", "gcloud-ps-topic", p, "--message=from-gcloud")
	var pulled string
	for i := 0; i < 10 && !strings.Contains(pulled, "from-gcloud"); i++ {
		pulled = g.must("pubsub", "subscriptions", "pull", "gcloud-ps-kept", p, "--auto-ack", "--format=value(message.data)")
	}
	if !strings.Contains(pulled, "from-gcloud") {
		t.Errorf("gcloud pubsub subscriptions pull gave %q", pulled)
	}

	for _, id := range []string{"kept", "default"} {
		g.must("pubsub", "subscriptions", "delete", "gcloud-ps-"+id, p)
		if _, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: subName(id)}); status.Code(err) != codes.NotFound {
			t.Errorf("after gcloud deleted %s, it reads %v", id, err)
		}
	}
	g.must("pubsub", "topics", "delete", "gcloud-ps-topic", p)
}
