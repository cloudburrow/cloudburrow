package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	pubsub "cloud.google.com/go/pubsub/v2"
	apiv1 "cloud.google.com/go/pubsub/v2/apiv1"
	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// pubsubClients are the official admin and schema clients of addr.
func pubsubClients(t *testing.T, addr, project string) (*pubsub.Client, *apiv1.SchemaClient) {
	t.Helper()
	ctx := context.Background()
	opts := []option.ClientOption{option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
	c, err := pubsub.NewClient(ctx, project, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	sc, err := apiv1.NewSchemaClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	return c, sc
}

const resetAvro = `{"type":"record","name":"R","fields":[{"name":"a","type":"string"}]}`

// A Pub/Sub project reset deletes the project's schemas too (#889), after
// the topics that name them, and leaves another project's alone; the same
// schema ID can then be created again.
func TestPubSubResetDeletesSchemas(t *testing.T) {
	_, addr := pubsubBehindFront(t)
	ctx := context.Background()
	c, sc := pubsubClients(t, addr, "reset-proj")
	for _, project := range []string{"reset-proj", "other-proj"} {
		s, err := sc.CreateSchema(ctx, &pubsubpb.CreateSchemaRequest{Parent: "projects/" + project, SchemaId: "orders",
			Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_AVRO, Definition: resetAvro}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: "projects/" + project + "/topics/typed",
			SchemaSettings: &pubsubpb.SchemaSettings{Schema: s.GetName(), Encoding: pubsubpb.Encoding_JSON}}); err != nil {
			t.Fatal(err)
		}
	}
	r := &pubsubResetter{tunnel: forwarderAt(t, addr)}
	if err := r.ResetProject(ctx, "reset-proj"); err != nil {
		t.Fatal(err)
	}
	if _, err := sc.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: "projects/reset-proj/schemas/orders"}); status.Code(err) != codes.NotFound {
		t.Errorf("the reset project's schema reads %v, want NOT_FOUND", err)
	}
	if _, err := sc.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: "projects/other-proj/schemas/orders"}); err != nil {
		t.Errorf("another project's schema: %v", err)
	}
	if _, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: "projects/other-proj/topics/typed"}); err != nil {
		t.Errorf("another project's topic: %v", err)
	}
	if _, err := sc.CreateSchema(ctx, &pubsubpb.CreateSchemaRequest{Parent: "projects/reset-proj", SchemaId: "orders",
		Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_AVRO, Definition: resetAvro}}); err != nil {
		t.Errorf("creating the schema again after the reset: %v", err)
	}
}

// An expiration policy set by UpdateSubscription, which the front keeps
// (#891), is what `state save` captures and `state load` recreates.
func TestPubSubStateKeepsAnUpdatedExpiration(t *testing.T) {
	_, addr := pubsubBehindFront(t)
	ctx := context.Background()
	const project = "expiry-proj"
	c, _ := pubsubClients(t, addr, project)
	topic := "projects/" + project + "/topics/t"
	sub := "projects/" + project + "/subscriptions/s"
	if _, err := c.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sub, Topic: topic,
		MessageRetentionDuration: durationpb.New(24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SubscriptionAdminClient.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{
		Subscription: &pubsubpb.Subscription{Name: sub, ExpirationPolicy: &pubsubpb.ExpirationPolicy{Ttl: durationpb.New(72 * time.Hour)}},
		UpdateMask:   &fieldmaskpb.FieldMask{Paths: []string{"expiration_policy"}}}); err != nil {
		t.Fatal(err)
	}
	p := &pubsubSnapshotter{tunnel: forwarderAt(t, addr), projects: func() []string { return nil }}
	entries := memEntries{}
	if err := p.Export(ctx, entries); err != nil {
		t.Fatal(err)
	}
	var saved pubsubState
	if err := json.Unmarshal(entries[pubsubEntry], &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Subscriptions) != 1 {
		t.Fatalf("saved %s", entries[pubsubEntry])
	}
	var s pubsubpb.Subscription
	if err := protojson.Unmarshal(saved.Subscriptions[0], &s); err != nil || s.GetExpirationPolicy().GetTtl().AsDuration() != 72*time.Hour {
		t.Errorf("saved %s, %v; want the updated 3-day ttl", saved.Subscriptions[0], err)
	}
	if err := p.Import(ctx, entries); err != nil {
		t.Fatal(err)
	}
	got, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sub})
	if err != nil || got.GetExpirationPolicy().GetTtl().AsDuration() != 72*time.Hour {
		t.Errorf("loaded, it reads %v, %v; want the updated 3-day ttl", got.GetExpirationPolicy(), err)
	}
}
