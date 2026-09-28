package main

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	pubsub "cloud.google.com/go/pubsub/v2"
	apiv1 "cloud.google.com/go/pubsub/v2/apiv1"
	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/pubsub/v2/pstest"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/cloudburrow/cloudburrow/internal/pubsubfront"
)

// pubsubBehindFront is an in-memory Pub/Sub behind the real front, with the
// push relay on, as the Pub/Sub pod runs them; it returns the front and its
// address.
func pubsubBehindFront(t *testing.T) (*pubsubfront.Front, string) {
	t.Helper()
	fake := pstest.NewServer()
	t.Cleanup(func() { _ = fake.Close() })
	f, err := pubsubfront.New(fake.Addr, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	rl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.RelayPushes(ctx, rl)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = f.Serve(ctx, l, time.Hour) }()
	t.Cleanup(func() { cancel(); <-done })
	return f, l.Addr().String()
}

// A Pub/Sub state save and load, through the front: topics, subscriptions
// with every setting and their real push endpoints, and schemas come back
// (snapshots, which the in-memory Pub/Sub lacks, are in the compat test), in a project CloudBurrow's registry never heard of, what was
// created after the save is gone, and each subscription's expiration clock
// is as it was when saved.
func TestPubSubStateRoundTrip(t *testing.T) {
	front, addr := pubsubBehindFront(t)
	ctx := context.Background()
	const project = "unregistered-proj"
	c, err := pubsub.NewClient(ctx, project, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sc, err := apiv1.NewSchemaClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()

	schema, err := sc.CreateSchema(ctx, &pubsubpb.CreateSchemaRequest{Parent: "projects/" + project, SchemaId: "orders-schema",
		Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_AVRO, Definition: `{"type":"record","name":"R","fields":[{"name":"a","type":"string"}]}`}})
	if err != nil {
		t.Fatal(err)
	}
	topicName := "projects/" + project + "/topics/orders"
	topic := &pubsubpb.Topic{Name: topicName, Labels: map[string]string{"team": "a"},
		MessageRetentionDuration: durationpb.New(2 * time.Hour),
		SchemaSettings:           &pubsubpb.SchemaSettings{Schema: schema.GetName(), Encoding: pubsubpb.Encoding_JSON}}
	if _, err := c.TopicAdminClient.CreateTopic(ctx, topic); err != nil {
		t.Fatal(err)
	}
	subs := []*pubsubpb.Subscription{
		{Name: "projects/" + project + "/subscriptions/pull", Topic: topicName, AckDeadlineSeconds: 30,
			EnableExactlyOnceDelivery: true, Filter: `attributes.k = "v"`, Labels: map[string]string{"x": "y"},
			ExpirationPolicy: &pubsubpb.ExpirationPolicy{Ttl: durationpb.New(48 * time.Hour)}, MessageRetentionDuration: durationpb.New(24 * time.Hour),
			RetryPolicy: &pubsubpb.RetryPolicy{MinimumBackoff: durationpb.New(5 * time.Second), MaximumBackoff: durationpb.New(time.Minute)}},
		{Name: "projects/" + project + "/subscriptions/push", Topic: topicName,
			PushConfig: &pubsubpb.PushConfig{PushEndpoint: "http://worker.example/push", Attributes: map[string]string{"x-goog-version": "v1"}}},
	}
	// What each reads back before the save, which is what it must read back
	// after the load.
	before := map[string]*pubsubpb.Subscription{}
	for _, s := range subs {
		got, err := c.SubscriptionAdminClient.CreateSubscription(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		before[s.Name] = got
	}
	front.Advance(ctx, 30*time.Hour)

	p := &pubsubSnapshotter{tunnel: forwarderAt(t, addr), projects: func() []string { return []string{"registered-proj"} }}
	entries := memEntries{}
	if err := p.Export(ctx, entries); err != nil {
		t.Fatal(err)
	}
	var saved pubsubState
	if err := json.Unmarshal(entries[pubsubEntry], &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Topics) != 1 || len(saved.Subscriptions) != 2 || len(saved.Schemas) != 1 {
		t.Fatalf("saved %d topics, %d subscriptions, %d schemas; want 1, 2, 1:\n%s",
			len(saved.Topics), len(saved.Subscriptions), len(saved.Schemas), entries[pubsubEntry])
	}
	for _, raw := range saved.Subscriptions {
		if strings.Contains(string(raw), "127.0.0.1") {
			t.Errorf("the archive holds the relay's endpoint: %s", raw)
		}
	}

	// After the save: one subscription gone, another added.
	if err := c.SubscriptionAdminClient.DeleteSubscription(ctx, &pubsubpb.DeleteSubscriptionRequest{Subscription: subs[1].Name}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{
		Name: "projects/" + project + "/subscriptions/later", Topic: topicName}); err != nil {
		t.Fatal(err)
	}
	if err := p.Import(ctx, entries); err != nil {
		t.Fatal(err)
	}

	for _, want := range subs {
		got, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: want.Name})
		if err != nil {
			t.Fatalf("%s after the load: %v", want.Name, err)
		}
		if w := before[want.Name]; !proto.Equal(w, got) {
			t.Errorf("%s came back as\n%v\nwant\n%v", want.Name, got, w)
		}
	}
	if _, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{
		Subscription: "projects/" + project + "/subscriptions/later"}); err == nil {
		t.Error("a subscription created after the save survived the load")
	}
	gotTopic, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: topicName})
	if err != nil || gotTopic.GetLabels()["team"] != "a" || gotTopic.GetSchemaSettings().GetSchema() != schema.GetName() ||
		gotTopic.GetMessageRetentionDuration().AsDuration() != 2*time.Hour {
		t.Errorf("the topic came back as %v, %v", gotTopic, err)
	}
	if s, err := sc.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: schema.GetName(), View: pubsubpb.SchemaView_FULL}); err != nil ||
		s.GetDefinition() != schema.GetDefinition() {
		t.Errorf("the schema came back as %v, %v", s, err)
	}

	// The clocks: the reads above were activity, so the saved idle time is
	// checked in the archive and on a second load, with nothing read after.
	if d := saved.IdleSeconds[subs[0].Name]; d < 30*3600 || d > 30*3600+60 {
		t.Errorf("saved idle %vs for %s, want 30h", d, subs[0].Name)
	}
	if err := p.Import(ctx, entries); err != nil {
		t.Fatal(err)
	}
	idle := front.Idle()
	if d := idle[subs[0].Name]; d < 30*time.Hour || d > 30*time.Hour+time.Minute {
		t.Errorf("after the load %s is idle %s, want 30h", subs[0].Name, d)
	}
	front.Advance(ctx, 19*time.Hour)
	if _, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: subs[0].Name}); err == nil {
		t.Errorf("%s, saved idle for 30h of a 48h ttl, survived 19h more", subs[0].Name)
	}
}
