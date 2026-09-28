//go:build compat

package compat

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

const resetSeedAvro = `{"type":"record","name":"Order","fields":[{"name":"n","type":"int"}]}`

// TestPubSubResetDeletesSchemas (#889): a project's Pub/Sub reset deletes its
// schemas along with the topics that name them, so GetSchema answers
// NOT_FOUND and a CreateSchema with the same ID succeeds where it answered
// ALREADY_EXISTS before; another project's schema is left alone.
// covers: google.pubsub.v1.SchemaService/CreateSchema, google.pubsub.v1.SchemaService/GetSchema
// unverified: google.pubsub.v1.SchemaService/GetSchema NOT_FOUND: a schema removed by a project-scoped /admin/reset
func TestPubSubResetDeletesSchemas(t *testing.T) {
	h, other := New(t), New(t)
	control := h.Endpoint(EnvControl)
	c := pubsubClient(t, h)
	sc := schemaClient(t, h)
	ctx := h.Context()
	create := func(project string) error {
		_, err := sc.CreateSchema(ctx, &pubsubpb.CreateSchemaRequest{Parent: "projects/" + project, SchemaId: "reset-schema",
			Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_AVRO, Definition: resetSeedAvro}})
		return err
	}
	mine := "projects/" + h.Project() + "/schemas/reset-schema"
	theirs := "projects/" + other.Project() + "/schemas/reset-schema"
	for _, p := range []string{h.Project(), other.Project()} {
		if err := create(p); err != nil {
			t.Fatalf("CreateSchema in %s: %v", p, err)
		}
	}
	t.Cleanup(func() {
		for _, n := range []string{mine, theirs} {
			_ = sc.DeleteSchema(context.Background(), &pubsubpb.DeleteSchemaRequest{Name: n})
		}
	})
	typed := "projects/" + h.Project() + "/topics/reset-typed"
	if _, err := c.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: typed,
		SchemaSettings: &pubsubpb.SchemaSettings{Schema: mine, Encoding: pubsubpb.Encoding_JSON}}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	if err := create(h.Project()); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("a second CreateSchema before the reset = %v; want ALREADY_EXISTS, or the test proves nothing", err)
	}

	if code, body := adminReset(t, control, "service=pubsub&project="+h.Project()); code != http.StatusOK {
		t.Fatalf("reset: %d %s", code, body)
	}
	if _, err := sc.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: mine}); status.Code(err) != codes.NotFound {
		t.Errorf("after the reset the project's schema reads %v; want NOT_FOUND", err)
	}
	if _, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: typed}); status.Code(err) != codes.NotFound {
		t.Errorf("after the reset the schema's topic reads %v; want NOT_FOUND", err)
	}
	if _, err := sc.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: theirs}); err != nil {
		t.Errorf("resetting %s removed another project's schema: %v", h.Project(), err)
	}
	if err := create(h.Project()); err != nil {
		t.Errorf("CreateSchema with the same ID after the reset = %v", err)
	}
}

// TestPubSubSeedSchemaAndBoundTopic (#890): a seed document declares an AVRO
// schema and a topic whose schemaSettings name it. The official clients read
// both back, a conforming message is published and a non-conforming one is
// refused INVALID_ARGUMENT, a repeat is a 409 and ifNotExists makes it a
// no-op. A Protocol Buffer schema is refused by name before anything is
// seeded.
// covers: google.pubsub.v1.SchemaService/GetSchema, google.pubsub.v1.Publisher/GetTopic, google.pubsub.v1.Publisher/Publish
func TestPubSubSeedSchemaAndBoundTopic(t *testing.T) {
	h := New(t)
	control := h.Endpoint(EnvControl)
	project := h.Project()
	c := pubsubClient(t, h)
	sc := schemaClient(t, h)
	ctx := h.Context()
	schema := "projects/" + project + "/schemas/seed-order"
	topic := "projects/" + project + "/topics/seed-typed"
	t.Cleanup(func() { adminReset(t, control, "service=pubsub&project="+project) })
	doc := func(skip bool) string {
		return fmt.Sprintf(`{"components": {"pubsub": {"ifNotExists": %t,
			"schemas": [{"name": %q, "type": "AVRO", "definition": %s}],
			"topics": [{"name": %q, "schemaSettings": {"schema": %q, "encoding": "JSON"}}]}}}`,
			skip, schema, strconv.Quote(resetSeedAvro), topic, schema)
	}

	proto := fmt.Sprintf(`{"components": {"pubsub": {"schemas": [{"name": "projects/%s/schemas/seed-proto",
		"type": "PROTOCOL_BUFFER", "definition": "syntax = \"proto3\"; message M { int32 n = 1; }"}],
		"topics": [{"name": "projects/%s/topics/seed-untouched"}]}}}`, project, project)
	if code, body := adminSeed(t, control, proto); code != http.StatusBadRequest {
		t.Errorf("a Protocol Buffer schema seeded %d %s; want 400", code, body)
	}
	if _, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: "projects/" + project + "/topics/seed-untouched"}); status.Code(err) != codes.NotFound {
		t.Errorf("a refused seed created its topic anyway: %v", err)
	}

	if code, body := adminSeed(t, control, doc(false)); code != http.StatusOK {
		t.Fatalf("seed: %d %s", code, body)
	}
	s, err := sc.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: schema, View: pubsubpb.SchemaView_FULL})
	if err != nil || s.GetType() != pubsubpb.Schema_AVRO || s.GetDefinition() != resetSeedAvro {
		t.Errorf("the seeded schema reads %v, %v", s, err)
	}
	got, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: topic})
	if err != nil || got.GetSchemaSettings().GetSchema() != schema || got.GetSchemaSettings().GetEncoding() != pubsubpb.Encoding_JSON {
		t.Fatalf("the seeded topic reads %v, %v", got, err)
	}
	publish := func(data string) error {
		_, err := c.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: topic,
			Messages: []*pubsubpb.PubsubMessage{{Data: []byte(data)}}})
		return err
	}
	if err := publish(`{"n":7}`); err != nil {
		t.Errorf("publishing a conforming message = %v", err)
	}
	if err := publish(`{"n":"not a number"}`); status.Code(err) != codes.InvalidArgument {
		t.Errorf("publishing a message the seeded schema refuses = %v; want INVALID_ARGUMENT", err)
	}

	if code, body := adminSeed(t, control, doc(false)); code != http.StatusConflict {
		t.Errorf("an identical second seed = %d %s; want 409", code, body)
	}
	if code, body := adminSeed(t, control, doc(true)); code != http.StatusOK {
		t.Errorf("a repeat with ifNotExists = %d %s", code, body)
	}
}

// TestPubSubExpirationCanBeUpdated (#891): the emulator refuses an
// UpdateSubscription of expiration_policy, and CloudBurrow's Pub/Sub front
// applies it instead. Through the official client, the new policy reads back
// from UpdateSubscription, GetSubscription and ListSubscriptions, and it is
// the one enforced: a subscription created with a one-day ttl and updated to
// three days is kept a day later and deleted once idle for three; one
// updated to never expire outlives its old ttl. A ttl under a day, or under
// the message retention, is refused and changes nothing, and a mixed update
// applies its other fields too.
// covers: google.pubsub.v1.Subscriber/UpdateSubscription, google.pubsub.v1.Subscriber/GetSubscription, google.pubsub.v1.Subscriber/ListSubscriptions
// unverified: google.pubsub.v1.Subscriber/UpdateSubscription INVALID_ARGUMENT: an expiration_policy.ttl under 1 day, or under message_retention_duration (Google documents the rule, not the code)
func TestPubSubExpirationCanBeUpdated(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	ctx := h.Context()
	topicName := topic(t, h, c, "expiry-update-topic")
	name := func(id string) string {
		return fmt.Sprintf("projects/%s/subscriptions/expiry-update-%s", h.Project(), id)
	}
	oneDay := &pubsubpb.ExpirationPolicy{Ttl: days(1)}
	for _, id := range []string{"longer", "never"} {
		n := name(id)
		if _, err := c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: n, Topic: topicName,
			ExpirationPolicy: oneDay, MessageRetentionDuration: days(1)}); err != nil {
			t.Fatalf("CreateSubscription %s: %v", n, err)
		}
		t.Cleanup(func() {
			_ = c.SubscriptionAdminClient.DeleteSubscription(context.Background(), &pubsubpb.DeleteSubscriptionRequest{Subscription: n})
		})
	}
	update := func(s *pubsubpb.Subscription, paths ...string) (*pubsubpb.Subscription, error) {
		return c.SubscriptionAdminClient.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{
			Subscription: s, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}})
	}
	ttl := func(s *pubsubpb.Subscription) time.Duration { return s.GetExpirationPolicy().GetTtl().AsDuration() }

	for why, s := range map[string]*pubsubpb.Subscription{
		"a ttl of 12 hours":             {Name: name("longer"), ExpirationPolicy: &pubsubpb.ExpirationPolicy{Ttl: durationpb.New(12 * time.Hour)}},
		"a ttl under the new retention": {Name: name("longer"), ExpirationPolicy: &pubsubpb.ExpirationPolicy{Ttl: days(2)}, MessageRetentionDuration: days(3)},
	} {
		if _, err := update(s, "expiration_policy", "message_retention_duration"); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: UpdateSubscription = %v, want INVALID_ARGUMENT", why, err)
		}
	}
	got, err := update(&pubsubpb.Subscription{Name: name("longer"), AckDeadlineSeconds: 45,
		ExpirationPolicy: &pubsubpb.ExpirationPolicy{Ttl: days(3)}}, "expiration_policy", "ack_deadline_seconds")
	if err != nil || ttl(got) != 72*time.Hour || got.GetAckDeadlineSeconds() != 45 {
		t.Fatalf("UpdateSubscription(expiration_policy, ack_deadline_seconds) = %v, %v; want a 3-day ttl and a 45s deadline", got, err)
	}
	if got, err := update(&pubsubpb.Subscription{Name: name("never"), ExpirationPolicy: &pubsubpb.ExpirationPolicy{}}, "expiration_policy"); err != nil ||
		got.GetExpirationPolicy() == nil || got.GetExpirationPolicy().GetTtl() != nil {
		t.Fatalf("UpdateSubscription to never expire = %v, %v", got.GetExpirationPolicy(), err)
	}
	read, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name("longer")})
	if err != nil || ttl(read) != 72*time.Hour || read.GetMessageRetentionDuration().AsDuration() != 24*time.Hour {
		t.Errorf("GetSubscription after the update = %v, %v; want a 3-day ttl and the 1-day retention unchanged", read, err)
	}
	listed := map[string]*pubsubpb.Subscription{}
	it := c.SubscriptionAdminClient.ListSubscriptions(ctx, &pubsubpb.ListSubscriptionsRequest{Project: "projects/" + h.Project()})
	for s, err := it.Next(); err == nil; s, err = it.Next() {
		listed[s.GetName()] = s
	}
	if s := listed[name("longer")]; ttl(s) != 72*time.Hour {
		t.Errorf("ListSubscriptions reads the updated subscription's policy as %v; want 3 days", s.GetExpirationPolicy())
	}
	if s := listed[name("never")]; s.GetExpirationPolicy() == nil || s.GetExpirationPolicy().GetTtl() != nil {
		t.Errorf("ListSubscriptions reads the never-expiring subscription's policy as %v", s.GetExpirationPolicy())
	}

	// Enforced: past the old one-day ttl both are kept; three days idle,
	// the updated ttl, removes the one and not the other.
	advancePubSubClock(t, h, 25*time.Hour)
	now := listedSubscriptions(t, h, c)
	if !now[name("longer")] || !now[name("never")] {
		t.Fatalf("25h after the updates, listed %v; want both kept past their old 1-day ttl", now)
	}
	advancePubSubClock(t, h, 48*time.Hour)
	now = listedSubscriptions(t, h, c)
	if now[name("longer")] || !now[name("never")] {
		t.Errorf("73h after the updates, longer listed = %v (want false, 3-day ttl), never listed = %v (want true)",
			now[name("longer")], now[name("never")])
	}
}
