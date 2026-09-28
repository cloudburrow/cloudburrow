package pubsubfront

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/pubsub/v2/pstest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// #949: an UpdateSubscription of labels, which the emulator refuses, is
// applied by the front. The emulator is never sent the path: an update of
// labels alone does not reach it, and one with other fields reaches it
// without labels. Every read through the front, Get, List and Update, names
// the new labels; an update with no labels clears them; a delete forgets
// them, so a subscription created again reads its own.
func TestUpdateSetsLabels(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	topic := fx.topic(t, "t")
	name := subName("labelled")
	if _, err := fx.create(t, &pubsubpb.Subscription{Name: name, Topic: topic, Labels: map[string]string{"env": "dev"}}); err != nil {
		t.Fatal(err)
	}
	admin := fx.client.SubscriptionAdminClient
	got, err := admin.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{
		Subscription: &pubsubpb.Subscription{Name: name, Labels: map[string]string{"env": "prod", "team": "a"}, AckDeadlineSeconds: 30},
		UpdateMask:   &fieldmaskpb.FieldMask{Paths: []string{"labels", "ack_deadline_seconds"}},
	})
	want := map[string]string{"env": "prod", "team": "a"}
	if err != nil || !maps.Equal(got.GetLabels(), want) || got.GetAckDeadlineSeconds() != 30 {
		t.Fatalf("UpdateSubscription(labels, ack) = %v %d, %v; want %v and 30", got.GetLabels(), got.GetAckDeadlineSeconds(), err, want)
	}
	s, err := admin.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name})
	if err != nil || !maps.Equal(s.GetLabels(), want) {
		t.Errorf("GetSubscription = %v, %v; want %v", s.GetLabels(), err, want)
	}
	it := admin.ListSubscriptions(ctx, &pubsubpb.ListSubscriptionsRequest{Project: "projects/" + project})
	for l, err := it.Next(); err == nil; l, err = it.Next() {
		if l.GetName() == name && !maps.Equal(l.GetLabels(), want) {
			t.Errorf("ListSubscriptions lists %v, want %v", l.GetLabels(), want)
		}
	}
	// The emulator took the deadline and never saw the labels.
	up, err := fx.upstream.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name})
	if err != nil || up.GetLabels()["env"] != "dev" || up.GetAckDeadlineSeconds() != 30 {
		t.Errorf("the emulator holds labels %v, ack %d, %v; want its own and the new deadline", up.GetLabels(), up.GetAckDeadlineSeconds(), err)
	}

	cleared, err := admin.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{
		Subscription: &pubsubpb.Subscription{Name: name}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}})
	if err != nil || len(cleared.GetLabels()) != 0 || cleared.GetAckDeadlineSeconds() != 30 {
		t.Errorf("an update with no labels = %v %d, %v; want none and the deadline kept", cleared.GetLabels(), cleared.GetAckDeadlineSeconds(), err)
	}

	if _, err := admin.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{
		Subscription: &pubsubpb.Subscription{Name: subName("missing"), Labels: want},
		UpdateMask:   &fieldmaskpb.FieldMask{Paths: []string{"labels"}}}); status.Code(err) != codes.NotFound {
		t.Errorf("labels on a missing subscription: %v, want NOT_FOUND", err)
	}

	if err := admin.DeleteSubscription(ctx, &pubsubpb.DeleteSubscriptionRequest{Subscription: name}); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.create(t, &pubsubpb.Subscription{Name: name, Topic: topic, Labels: map[string]string{"new": "1"}}); err != nil {
		t.Fatal(err)
	}
	s, err = admin.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name})
	if err != nil || !maps.Equal(s.GetLabels(), map[string]string{"new": "1"}) {
		t.Errorf("created again, the subscription reads %v, %v; want its own labels", s.GetLabels(), err)
	}
}

// #949: UpdateTopic of labels is applied the same way, alone or with the
// message retention the emulator takes, and DeleteTopic forgets them.
func TestUpdateTopicSetsLabels(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	name := "projects/" + project + "/topics/labelled"
	admin := fx.client.TopicAdminClient
	if _, err := admin.CreateTopic(ctx, &pubsubpb.Topic{Name: name, Labels: map[string]string{"env": "dev"}}); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"env": "prod"}
	got, err := admin.UpdateTopic(ctx, &pubsubpb.UpdateTopicRequest{
		Topic: &pubsubpb.Topic{Name: name, Labels: want}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}})
	if err != nil || !maps.Equal(got.GetLabels(), want) {
		t.Fatalf("UpdateTopic(labels) = %v, %v; want %v", got.GetLabels(), err, want)
	}
	want2 := map[string]string{"env": "prod", "tier": "1"}
	got, err = admin.UpdateTopic(ctx, &pubsubpb.UpdateTopicRequest{
		Topic:      &pubsubpb.Topic{Name: name, Labels: want2, MessageRetentionDuration: durationpb.New(48 * 3600e9)},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"message_retention_duration", "labels"}}})
	if err != nil || !maps.Equal(got.GetLabels(), want2) || got.GetMessageRetentionDuration().AsDuration().Hours() != 48 {
		t.Fatalf("UpdateTopic(retention, labels) = %v %v, %v; want %v and 48h", got.GetLabels(), got.GetMessageRetentionDuration(), err, want2)
	}
	tp, err := admin.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: name})
	if err != nil || !maps.Equal(tp.GetLabels(), want2) {
		t.Errorf("GetTopic = %v, %v; want %v", tp.GetLabels(), err, want2)
	}
	it := admin.ListTopics(ctx, &pubsubpb.ListTopicsRequest{Project: "projects/" + project})
	for l, err := it.Next(); err == nil; l, err = it.Next() {
		if l.GetName() == name && !maps.Equal(l.GetLabels(), want2) {
			t.Errorf("ListTopics lists %v, want %v", l.GetLabels(), want2)
		}
	}
	up, err := fx.upstream.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: name})
	if err != nil || up.GetLabels()["env"] != "dev" || up.GetMessageRetentionDuration().AsDuration().Hours() != 48 {
		t.Errorf("the emulator holds %v %v, %v; want its own labels and the new retention", up.GetLabels(), up.GetMessageRetentionDuration(), err)
	}
	if _, err := admin.UpdateTopic(ctx, &pubsubpb.UpdateTopicRequest{
		Topic: &pubsubpb.Topic{Name: name + "-missing", Labels: want}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}}); status.Code(err) != codes.NotFound {
		t.Errorf("labels on a missing topic: %v, want NOT_FOUND", err)
	}
	if err := admin.DeleteTopic(ctx, &pubsubpb.DeleteTopicRequest{Topic: name}); err != nil {
		t.Fatal(err)
	}
	if _, ok := fx.front.keptLabels(name); ok {
		t.Error("a deleted topic's labels are still kept")
	}
}

// #949 over REST: Terraform's PATCH of a topic's or a subscription's labels,
// its mask in the URL, is applied by the front. Labels alone never reach the
// emulator; with another path, the emulator is sent that path alone, in the
// body; every read names the kept labels.
func TestRESTPatchSetsLabels(t *testing.T) {
	up := newRESTUpstream(t)
	fx := newFixtureREST(t, up.addr())
	topic := fx.topic(t, "t")
	name := subName("tf")
	if _, err := fx.create(t, &pubsubpb.Subscription{Name: name, Topic: topic}); err != nil {
		t.Fatal(err)
	}
	subPath := "/v1/projects/" + project + "/subscriptions/tf"
	topicPath := "/v1/projects/" + project + "/topics/t"
	labelsOf := func(body map[string]any) map[string]any {
		l, _ := body["labels"].(map[string]any)
		return l
	}

	code, body := fx.rest(t, http.MethodPatch, topicPath+"?updateMask=labels", `{"topic":{"name":"`+topic+`","labels":{"env":"prod"}},"updateMask":"labels"}`)
	if code != http.StatusOK || labelsOf(body)["env"] != "prod" || body["name"] != topic {
		t.Errorf("PATCH a topic's labels = %d %v, want the topic with env=prod", code, body)
	}
	code, body = fx.rest(t, http.MethodPatch, subPath+"?updateMask=labels,bigqueryConfig", `{"subscription":{"labels":{"env":"prod"}}}`)
	if code != http.StatusOK || labelsOf(body)["env"] != "prod" {
		t.Errorf("PATCH a subscription's labels = %d %v, want env=prod", code, body)
	}
	if n := len(up.calls()); n != 0 {
		t.Errorf("the emulator was sent %d calls, want none", n)
	}

	code, _ = fx.rest(t, http.MethodPatch, subPath+"?updateMask=ackDeadlineSeconds,labels",
		`{"subscription":{"ackDeadlineSeconds":30,"labels":{"env":"test"}}}`)
	calls := up.calls()
	var sent map[string]any
	if len(calls) > 0 {
		_ = json.Unmarshal(calls[len(calls)-1].body, &sent)
	}
	if code != http.StatusOK || len(calls) != 1 || calls[0].query != "" || sent["updateMask"] != "ack_deadline_seconds" ||
		strings.Contains(string(calls[0].body), "labels") {
		t.Errorf("PATCH ack and labels = %d, sent %v; want the deadline alone, in the body", code, calls)
	}
	if l, _ := fx.front.keptLabels(name); l["env"] != "test" {
		t.Errorf("after the emulator took the rest, the front keeps %v, want env=test", l)
	}

	// The emulator's answers name the kept labels.
	up.answer(http.MethodGet, topicPath, `{"name":"`+topic+`","labels":{"env":"dev"}}`)
	up.answer(http.MethodGet, "/v1/projects/"+project+"/topics", `{"topics":[{"name":"`+topic+`"},{"name":"other"}]}`)
	up.answer(http.MethodGet, subPath, `{"name":"`+name+`","topic":"`+topic+`"}`)
	if code, body := fx.rest(t, http.MethodGet, topicPath, ""); code != http.StatusOK || labelsOf(body)["env"] != "prod" {
		t.Errorf("GET the topic = %d %v, want env=prod", code, body)
	}
	if code, body := fx.rest(t, http.MethodGet, "/v1/projects/"+project+"/topics", ""); code != http.StatusOK {
		t.Errorf("GET topics = %d", code)
	} else if ts, _ := body["topics"].([]any); len(ts) != 2 || labelsOf(ts[0].(map[string]any))["env"] != "prod" || ts[1].(map[string]any)["labels"] != nil {
		t.Errorf("GET topics = %v, want the topic with env=prod and the other as it came", body)
	}
	if code, body := fx.rest(t, http.MethodGet, subPath, ""); code != http.StatusOK || labelsOf(body)["env"] != "test" {
		t.Errorf("GET the subscription = %d %v, want env=test", code, body)
	}
	// A topic created again, or deleted, has its own labels.
	fx.rest(t, http.MethodPut, topicPath, `{"labels":{"fresh":"1"}}`)
	if _, ok := fx.front.keptLabels(topic); ok {
		t.Error("a topic created again over REST keeps the old labels")
	}
	fx.front.setLabels(topic, map[string]string{"k": "v"})
	fx.rest(t, http.MethodDelete, topicPath, "")
	if _, ok := fx.front.keptLabels(topic); ok {
		t.Error("a topic deleted over REST keeps its labels")
	}
}

// #949: kept labels survive a restart of the front alone, and a deleted
// resource's are dropped from the file.
func TestStateKeepsLabels(t *testing.T) {
	fake := pstest.NewServer()
	t.Cleanup(func() { _ = fake.Close() })
	path := filepath.Join(t.TempDir(), "state.json")
	ctx := context.Background()
	first := runFront(t, fake.Addr, path)
	topic := "projects/" + project + "/topics/t"
	if _, err := first.client.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"kept", "gone"} {
		if _, err := first.client.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: subName(id), Topic: topic}); err != nil {
			t.Fatal(err)
		}
		if _, err := first.client.SubscriptionAdminClient.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{
			Subscription: &pubsubpb.Subscription{Name: subName(id), Labels: map[string]string{"id": id}}, UpdateMask: fieldMask("labels")}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := first.client.TopicAdminClient.UpdateTopic(ctx, &pubsubpb.UpdateTopicRequest{
		Topic: &pubsubpb.Topic{Name: topic, Labels: map[string]string{"env": "prod"}}, UpdateMask: fieldMask("labels")}); err != nil {
		t.Fatal(err)
	}
	if err := first.client.SubscriptionAdminClient.DeleteSubscription(ctx, &pubsubpb.DeleteSubscriptionRequest{Subscription: subName("gone")}); err != nil {
		t.Fatal(err)
	}
	first.stop()

	second := runFront(t, fake.Addr, path)
	s, err := second.client.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: subName("kept")})
	if err != nil || s.GetLabels()["id"] != "kept" {
		t.Errorf("after the restart the subscription reads %v, %v; want id=kept", s.GetLabels(), err)
	}
	tp, err := second.client.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: topic})
	if err != nil || tp.GetLabels()["env"] != "prod" {
		t.Errorf("after the restart the topic reads %v, %v; want env=prod", tp.GetLabels(), err)
	}
	if _, ok := second.front.keptLabels(subName("gone")); ok {
		t.Error("a deleted subscription's labels came back from the file")
	}
}
