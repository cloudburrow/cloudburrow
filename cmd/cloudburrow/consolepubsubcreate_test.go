package main

import (
	"context"
	"strings"
	"testing"
	"time"

	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/pubsub/v2/pstest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Create topic sets the topic's retention and schema, and Create
// subscription every option the emulator keeps and acts on; the official
// client reads each back as the form gave it (#852). The two options the
// emulator stores and does not act on, an expiration and exactly-once
// delivery, are not on the form, and its help says why.
func TestPubSubCreateOptionsReachTheAPI(t *testing.T) {
	ctx := context.Background()
	const project = "create-opts"
	addr, c := pubsubEditFixture(t, project)
	p := pubsubProvider{endpoint: addr}

	topic, err := p.Create(ctx, project, map[string]string{"name": "orders", "messageRetention": "36h"})
	if err != nil {
		t.Fatalf("create topic: %v", err)
	}
	got, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: topic})
	if err != nil || got.GetMessageRetentionDuration().AsDuration() != 36*time.Hour {
		t.Fatalf("the topic reads %v, %v; want a 36h retention", got.GetMessageRetentionDuration(), err)
	}
	dead, err := p.Create(ctx, project, map[string]string{"name": "orders-dead"})
	if err != nil {
		t.Fatal(err)
	}

	var sub []string
	for _, a := range p.DetailActions(ctx, project, []string{topic}) {
		if a.ID == actCreateSubscription {
			for _, f := range a.Fields {
				sub = append(sub, f.Name)
				if f.Name == "maxDeliveryAttempts" && !strings.Contains(f.Help, pubsubNotOfferedAtCreate) {
					t.Errorf("the form does not say why expiration and exactly-once are not offered: %q", f.Help)
				}
			}
		}
	}
	for _, want := range []string{"name", "pushEndpoint", "ackDeadline", "messageRetention", "retainAcked",
		"messageOrdering", "filter", "minBackoff", "maxBackoff", "deadLetterTopic", "maxDeliveryAttempts"} {
		if !strings.Contains(" "+strings.Join(sub, " ")+" ", " "+want+" ") {
			t.Errorf("Create subscription does not offer %q: %v", want, sub)
		}
	}
	for _, absent := range []string{"expiration", "exactlyOnce"} {
		if strings.Contains(" "+strings.Join(sub, " ")+" ", " "+absent+" ") {
			t.Errorf("Create subscription offers %q, which the emulator does not act on", absent)
		}
	}

	if _, err := p.ActAtResult(ctx, project, []string{topic}, actCreateSubscription, map[string]string{
		"name": "orders-eu", "ackDeadline": "30", "messageRetention": "2d", "retainAcked": "true",
		"messageOrdering": "true", "filter": `attributes.region = "eu"`, "minBackoff": "5s", "maxBackoff": "1m",
		"deadLetterTopic": dead, "maxDeliveryAttempts": "7",
	}); err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	s, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{
		Subscription: "projects/" + project + "/subscriptions/orders-eu"})
	if err != nil {
		t.Fatal(err)
	}
	if s.GetTopic() != topic || s.GetAckDeadlineSeconds() != 30 || s.GetMessageRetentionDuration().AsDuration() != 48*time.Hour ||
		!s.GetRetainAckedMessages() || !s.GetEnableMessageOrdering() || s.GetFilter() != `attributes.region = "eu"` ||
		s.GetRetryPolicy().GetMinimumBackoff().AsDuration() != 5*time.Second ||
		s.GetRetryPolicy().GetMaximumBackoff().AsDuration() != time.Minute ||
		s.GetDeadLetterPolicy().GetDeadLetterTopic() != dead || s.GetDeadLetterPolicy().GetMaxDeliveryAttempts() != 7 {
		t.Errorf("the subscription reads back as %v", s)
	}

	// The form's defaults make a plain pull subscription.
	if _, err := p.ActAtResult(ctx, project, []string{topic}, actCreateSubscription, map[string]string{
		"name": "orders-plain", "ackDeadline": "10", "messageRetention": "7d", "retainAcked": "false",
		"messageOrdering": "false", "maxDeliveryAttempts": "5",
	}); err != nil {
		t.Fatalf("create a plain subscription: %v", err)
	}
	plain, _ := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{
		Subscription: "projects/" + project + "/subscriptions/orders-plain"})
	if plain.GetFilter() != "" || plain.GetRetryPolicy() != nil || plain.GetDeadLetterPolicy() != nil ||
		plain.GetEnableMessageOrdering() || plain.GetPushConfig().GetPushEndpoint() != "" {
		t.Errorf("the form's defaults made %v; want a plain pull subscription", plain)
	}

	for _, bad := range []map[string]string{
		{"name": "b1", "messageRetention": "a week"},
		{"name": "b2", "ackDeadline": "5"},
		{"name": "b3", "minBackoff": "soon"},
		{"name": "b4", "deadLetterTopic": dead, "maxDeliveryAttempts": "many"},
		{"name": ""},
	} {
		if _, err := p.ActAtResult(ctx, project, []string{topic}, actCreateSubscription, bad); err == nil {
			t.Errorf("create subscription %v was accepted", bad)
		}
	}
	if _, err := p.Create(ctx, project, map[string]string{"name": "t2", "schema": "s", "schemaEncoding": "XML"}); err == nil {
		t.Error("a schema encoding other than JSON or BINARY was accepted")
	}
}

// A schema named by its ID is sent as the project's schema, with the chosen
// encoding; one given in full is sent as given (#852).
func TestPubSubTopicFormNamesTheSchema(t *testing.T) {
	for _, tc := range []struct{ schema, enc, want string }{
		{"orders-v1", "BINARY", "projects/p/schemas/orders-v1"},
		{"projects/q/schemas/x", "", "projects/q/schemas/x"},
	} {
		topic, err := topicFromForm("p", "projects/p/topics/t", map[string]string{"schema": tc.schema, "schemaEncoding": tc.enc})
		if err != nil {
			t.Fatal(err)
		}
		wantEnc := pubsubpb.Encoding_JSON
		if tc.enc == "BINARY" {
			wantEnc = pubsubpb.Encoding_BINARY
		}
		if got := topic.GetSchemaSettings(); got.GetSchema() != tc.want || got.GetEncoding() != wantEnc {
			t.Errorf("schema %q, encoding %q: sent %v", tc.schema, tc.enc, got)
		}
	}
	topic, err := topicFromForm("p", "projects/p/topics/t", map[string]string{"schemaEncoding": "JSON"})
	if err != nil || topic.GetSchemaSettings() != nil {
		t.Errorf("no schema sent %v, %v; want no schema settings", topic.GetSchemaSettings(), err)
	}
}

// The emulator answers a filter it cannot parse with UNKNOWN, which the
// official client retries by default until the deadline. The console sends
// the create once and says it was the filter (#852).
func TestPubSubCreateSubscriptionDoesNotRetryAFilterRefusal(t *testing.T) {
	const project = "filter-proj"
	addr, c := pubsubEditFixture(t, project, pstest.WithErrorInjection("CreateSubscription", codes.Unknown,
		"Application error processing RPC"))
	p := pubsubProvider{endpoint: addr}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	topic := "projects/" + project + "/topics/t"
	if _, err := c.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := p.ActAtResult(ctx, project, []string{topic}, actCreateSubscription,
		map[string]string{"name": "s1", "filter": "nonsense ==="})
	if status.Code(err) != codes.Unknown || !strings.Contains(err.Error(), pubsubFilterRefusedAtCreate) {
		t.Errorf("a refused filter = %v; want UNKNOWN, prefixed with the filter's explanation", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the refusal took %v: the create was retried", d)
	}
}
