//go:build compat

package compat

import (
	"maps"
	"testing"
	"time"

	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// TestPubSubUpdateLabels (#949): the emulator refuses a labels mask on
// UpdateTopic and UpdateSubscription ("labels is not a known Topic field",
// "... Subscription field"), and CloudBurrow's front applies it instead,
// through the official client: labels alone, and labels with a field the
// emulator takes (a topic's retention, a subscription's ack deadline), are
// both applied, and GetTopic, ListTopics, GetSubscription and
// ListSubscriptions read the new labels; an update naming labels with none
// clears them; labels on a missing resource are NOT_FOUND.
// covers: google.pubsub.v1.Publisher/UpdateTopic, google.pubsub.v1.Subscriber/UpdateSubscription, google.pubsub.v1.Publisher/ListTopics, google.pubsub.v1.Subscriber/ListSubscriptions
func TestPubSubUpdateLabels(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	ctx := h.Context()
	project := "projects/" + h.Project()
	tn := topic(t, h, c, "labels-topic")
	sn := subscription(t, h, c, "labels-sub", tn)
	mask := func(p ...string) *fieldmaskpb.FieldMask { return &fieldmaskpb.FieldMask{Paths: p} }

	// --- topic
	env := map[string]string{"env": "dev"}
	got, err := c.TopicAdminClient.UpdateTopic(ctx, &pubsubpb.UpdateTopicRequest{
		Topic: &pubsubpb.Topic{Name: tn, Labels: env}, UpdateMask: mask("labels")})
	if err != nil || !maps.Equal(got.GetLabels(), env) {
		t.Fatalf("UpdateTopic(labels) = %v, %v; want %v", got.GetLabels(), err, env)
	}
	both := map[string]string{"env": "prod", "team": "a"}
	if _, err := c.TopicAdminClient.UpdateTopic(ctx, &pubsubpb.UpdateTopicRequest{
		Topic:      &pubsubpb.Topic{Name: tn, Labels: both, MessageRetentionDuration: durationpb.New(3 * time.Hour)},
		UpdateMask: mask("labels", "message_retention_duration")}); err != nil {
		t.Fatalf("UpdateTopic(labels, message_retention_duration): %v", err)
	}
	tp, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: tn})
	if err != nil || !maps.Equal(tp.GetLabels(), both) || tp.GetMessageRetentionDuration().AsDuration() != 3*time.Hour {
		t.Errorf("GetTopic = %v %v, %v; want %v and 3h", tp.GetLabels(), tp.GetMessageRetentionDuration(), err, both)
	}
	listed := false
	it := c.TopicAdminClient.ListTopics(ctx, &pubsubpb.ListTopicsRequest{Project: project})
	for {
		l, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListTopics: %v", err)
		}
		if l.GetName() == tn {
			listed = true
			if !maps.Equal(l.GetLabels(), both) {
				t.Errorf("ListTopics lists %v, want %v", l.GetLabels(), both)
			}
		}
	}
	if !listed {
		t.Errorf("ListTopics does not list %s", tn)
	}
	if got, err := c.TopicAdminClient.UpdateTopic(ctx, &pubsubpb.UpdateTopicRequest{
		Topic: &pubsubpb.Topic{Name: tn}, UpdateMask: mask("labels")}); err != nil || len(got.GetLabels()) != 0 {
		t.Errorf("UpdateTopic clearing the labels = %v, %v; want none", got.GetLabels(), err)
	}
	if tp, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: tn}); err != nil || len(tp.GetLabels()) != 0 {
		t.Errorf("GetTopic after clearing = %v, %v; want no labels", tp.GetLabels(), err)
	}
	if _, err := c.TopicAdminClient.UpdateTopic(ctx, &pubsubpb.UpdateTopicRequest{
		Topic: &pubsubpb.Topic{Name: project + "/topics/labels-missing", Labels: env}, UpdateMask: mask("labels")}); status.Code(err) != codes.NotFound {
		t.Errorf("UpdateTopic(labels) of a missing topic = %v, want NOT_FOUND", err)
	}

	// --- subscription
	s, err := c.SubscriptionAdminClient.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{
		Subscription: &pubsubpb.Subscription{Name: sn, Labels: env}, UpdateMask: mask("labels")})
	if err != nil || !maps.Equal(s.GetLabels(), env) {
		t.Fatalf("UpdateSubscription(labels) = %v, %v; want %v", s.GetLabels(), err, env)
	}
	if _, err := c.SubscriptionAdminClient.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{
		Subscription: &pubsubpb.Subscription{Name: sn, Labels: both, AckDeadlineSeconds: 45},
		UpdateMask:   mask("ack_deadline_seconds", "labels")}); err != nil {
		t.Fatalf("UpdateSubscription(ack_deadline_seconds, labels): %v", err)
	}
	s, err = c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sn})
	if err != nil || !maps.Equal(s.GetLabels(), both) || s.GetAckDeadlineSeconds() != 45 {
		t.Errorf("GetSubscription = %v ack %d, %v; want %v and 45", s.GetLabels(), s.GetAckDeadlineSeconds(), err, both)
	}
	listed = false
	sit := c.SubscriptionAdminClient.ListSubscriptions(ctx, &pubsubpb.ListSubscriptionsRequest{Project: project})
	for {
		l, err := sit.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListSubscriptions: %v", err)
		}
		if l.GetName() == sn {
			listed = true
			if !maps.Equal(l.GetLabels(), both) {
				t.Errorf("ListSubscriptions lists %v, want %v", l.GetLabels(), both)
			}
		}
	}
	if !listed {
		t.Errorf("ListSubscriptions does not list %s", sn)
	}
	if s, err := c.SubscriptionAdminClient.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{
		Subscription: &pubsubpb.Subscription{Name: sn}, UpdateMask: mask("labels")}); err != nil || len(s.GetLabels()) != 0 {
		t.Errorf("UpdateSubscription clearing the labels = %v, %v; want none", s.GetLabels(), err)
	}
	if _, err := c.SubscriptionAdminClient.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{
		Subscription: &pubsubpb.Subscription{Name: project + "/subscriptions/labels-missing", Labels: env},
		UpdateMask:   mask("labels")}); status.Code(err) != codes.NotFound {
		t.Errorf("UpdateSubscription(labels) of a missing subscription = %v, want NOT_FOUND", err)
	}
}
