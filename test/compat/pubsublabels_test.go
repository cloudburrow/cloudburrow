//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strings"
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

// TestPubSubLabelsAreHeldToGoogleRules (#962): labels that break Google's
// rules (https://cloud.google.com/pubsub/docs/labels) are refused
// INVALID_ARGUMENT by CloudBurrow's front, through the official client and
// over REST, on create and on update, and nothing is stored: a key with an
// uppercase letter or starting with a digit, a value with a space, 65
// labels. 64 labels, 63-character keys and values, and international
// characters are taken and read back.
// covers: google.pubsub.v1.Publisher/CreateTopic, google.pubsub.v1.Subscriber/CreateSubscription, google.pubsub.v1.Publisher/UpdateTopic, google.pubsub.v1.Subscriber/UpdateSubscription
func TestPubSubLabelsAreHeldToGoogleRules(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	ctx := h.Context()
	project := "projects/" + h.Project()
	mask := func(p ...string) *fieldmaskpb.FieldMask { return &fieldmaskpb.FieldMask{Paths: p} }
	many := func(n int) map[string]string {
		l := map[string]string{}
		for i := range n {
			l[fmt.Sprintf("k%02d", i)] = "v"
		}
		return l
	}
	refused := project + "/topics/labels-refused"
	for _, bad := range []map[string]string{{"Env": "dev"}, {"1env": "dev"}, {"env": "a b"}, many(65)} {
		if _, err := c.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: refused, Labels: bad}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("CreateTopic with labels %v: %v, want INVALID_ARGUMENT", bad, err)
		}
	}
	if _, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: refused}); status.Code(err) != codes.NotFound {
		t.Errorf("a refused topic reads %v, want NOT_FOUND", err)
	}

	good := map[string]string{"k" + strings.Repeat("a", 62): strings.Repeat("v", 63), "größe": "日本", "team_a": "x-1", "empty": ""}
	tn := project + "/topics/labels-good"
	if _, err := c.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: tn, Labels: good}); err != nil {
		t.Fatalf("CreateTopic with labels Google takes: %v", err)
	}
	t.Cleanup(func() {
		_ = c.TopicAdminClient.DeleteTopic(context.Background(), &pubsubpb.DeleteTopicRequest{Topic: tn})
	})
	if tp, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: tn}); err != nil || !maps.Equal(tp.GetLabels(), good) {
		t.Errorf("GetTopic = %v, %v; want %v", tp.GetLabels(), err, good)
	}
	if _, err := c.TopicAdminClient.UpdateTopic(ctx, &pubsubpb.UpdateTopicRequest{
		Topic: &pubsubpb.Topic{Name: tn, Labels: map[string]string{"env": "Prod"}}, UpdateMask: mask("labels")}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("UpdateTopic with an uppercase value: %v, want INVALID_ARGUMENT", err)
	}
	if tp, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: tn}); err != nil || !maps.Equal(tp.GetLabels(), good) {
		t.Errorf("after the refused update GetTopic = %v, %v; want %v", tp.GetLabels(), err, good)
	}

	sn := project + "/subscriptions/labels-rules"
	if _, err := c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sn, Topic: tn, Labels: many(65)}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateSubscription with 65 labels: %v, want INVALID_ARGUMENT", err)
	}
	if _, err := c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sn, Topic: tn, Labels: many(64)}); err != nil {
		t.Fatalf("CreateSubscription with 64 labels: %v", err)
	}
	t.Cleanup(func() {
		_ = c.SubscriptionAdminClient.DeleteSubscription(context.Background(), &pubsubpb.DeleteSubscriptionRequest{Subscription: sn})
	})
	if _, err := c.SubscriptionAdminClient.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{
		Subscription: &pubsubpb.Subscription{Name: sn, Labels: map[string]string{"a.b": "c"}, AckDeadlineSeconds: 42},
		UpdateMask:   mask("labels", "ack_deadline_seconds")}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("UpdateSubscription with a dot in a key: %v, want INVALID_ARGUMENT", err)
	}
	if s, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sn}); err != nil ||
		len(s.GetLabels()) != 64 || s.GetAckDeadlineSeconds() == 42 {
		t.Errorf("after the refused update the subscription reads %d labels, ack %d, %v; want 64 and the deadline unchanged",
			len(s.GetLabels()), s.GetAckDeadlineSeconds(), err)
	}

	// REST, as gcloud and Terraform speak it.
	rest := func(method, path, body string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, "http://"+h.Endpoint(EnvPubSub)+"/v1/"+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var e struct {
			Error struct{ Status string } `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return resp.StatusCode, e.Error.Status
	}
	if code, st := rest(http.MethodPut, refused, `{"labels":{"Env":"dev"}}`); code != http.StatusBadRequest || st != "INVALID_ARGUMENT" {
		t.Errorf("REST PUT a topic with an uppercase key = %d %s, want 400 INVALID_ARGUMENT", code, st)
	}
	if code, st := rest(http.MethodPatch, sn, `{"subscription":{"labels":{"k":"`+strings.Repeat("v", 64)+`"}},"updateMask":"labels"}`); code != http.StatusBadRequest || st != "INVALID_ARGUMENT" {
		t.Errorf("REST PATCH a subscription with a 64-character value = %d %s, want 400 INVALID_ARGUMENT", code, st)
	}
	if _, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: refused}); status.Code(err) != codes.NotFound {
		t.Errorf("the topic refused over REST reads %v, want NOT_FOUND", err)
	}
}
