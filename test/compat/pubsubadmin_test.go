//go:build compat

package compat

import (
	"context"
	"fmt"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	vkit "cloud.google.com/go/pubsub/v2/apiv1"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The Pub/Sub admin surface the earlier suites left Unknown (#693), and the
// Schema service through its generated client. Refusals are asserted with
// the emulator's exact error, so a newer emulator that starts serving one
// fails here and the docs are corrected rather than left understating it.

// snapshot creates a snapshot of sub and removes it afterwards.
func snapshot(t *testing.T, h *Harness, c *pubsub.Client, id, sub string) string {
	t.Helper()
	name := fmt.Sprintf("projects/%s/snapshots/%s", h.Project(), id)
	if _, err := c.SubscriptionAdminClient.CreateSnapshot(h.Context(), &pubsubpb.CreateSnapshotRequest{
		Name: name, Subscription: sub,
	}); err != nil {
		t.Fatalf("CreateSnapshot %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = c.SubscriptionAdminClient.DeleteSnapshot(context.Background(), &pubsubpb.DeleteSnapshotRequest{Snapshot: name})
	})
	return name
}

// drain reads an iterator to its end.
func drain[T any](t *testing.T, what string, next func() (T, error)) []T {
	t.Helper()
	var out []T
	for {
		v, err := next()
		if err == iterator.Done {
			return out
		}
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		out = append(out, v)
	}
}

// wantRefused asserts err is exactly the status the emulator returned when
// this was measured.
func wantRefused(t *testing.T, what string, err error, code codes.Code, msg string) {
	t.Helper()
	st, _ := status.FromError(err)
	if err == nil || st.Code() != code || st.Message() != msg {
		t.Fatalf("%s = %v; the emulator refused it with %s %q when this was recorded — if it now works, promote it in docs/compatibility.md", what, err, code, msg)
	}
}

// TestPubSubUpdateTopic changes a topic in place and re-reads it, since a
// response echoing the request proves nothing. The emulator accepts
// message_retention_duration; a labels mask, which it refuses, CloudBurrow's
// front applies (TestPubSubUpdateLabels, #949).
// covers: google.pubsub.v1.Publisher/UpdateTopic
func TestPubSubUpdateTopic(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	ctx := h.Context()

	tn := topic(t, h, c, "update-topic")
	if _, err := c.TopicAdminClient.UpdateTopic(ctx, &pubsubpb.UpdateTopicRequest{
		Topic:      &pubsubpb.Topic{Name: tn, MessageRetentionDuration: durationpb.New(2 * time.Hour)},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"message_retention_duration"}},
	}); err != nil {
		t.Fatalf("UpdateTopic(message_retention_duration): %v", err)
	}
	got, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: tn})
	if err != nil {
		t.Fatalf("GetTopic: %v", err)
	}
	if d := got.GetMessageRetentionDuration().AsDuration(); d != 2*time.Hour {
		t.Errorf("message retention after UpdateTopic = %v, want 2h", d)
	}
}

// TestPubSubTopicAndSnapshotListings covers the listings that go through a
// topic and the project's snapshot listing.
// covers: google.pubsub.v1.Publisher/ListTopicSubscriptions, google.pubsub.v1.Publisher/ListTopicSnapshots, google.pubsub.v1.Subscriber/ListSnapshots
func TestPubSubTopicAndSnapshotListings(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	ctx := h.Context()

	tn := topic(t, h, c, "listings-topic")
	sn := subscription(t, h, c, "listings-subscription", tn)
	snap := snapshot(t, h, c, "listings-snapshot", sn)

	subs := c.TopicAdminClient.ListTopicSubscriptions(ctx, &pubsubpb.ListTopicSubscriptionsRequest{Topic: tn})
	if got := drain(t, "ListTopicSubscriptions", subs.Next); !contains(got, sn) {
		t.Errorf("ListTopicSubscriptions = %v, want it to include %s", got, sn)
	}

	snaps := c.TopicAdminClient.ListTopicSnapshots(ctx, &pubsubpb.ListTopicSnapshotsRequest{Topic: tn})
	if got := drain(t, "ListTopicSnapshots", snaps.Next); !contains(got, snap) {
		t.Errorf("ListTopicSnapshots = %v, want it to include %s", got, snap)
	}

	all := c.SubscriptionAdminClient.ListSnapshots(ctx, &pubsubpb.ListSnapshotsRequest{Project: "projects/" + h.Project()})
	var names []string
	for _, s := range drain(t, "ListSnapshots", all.Next) {
		names = append(names, s.GetName())
	}
	if !contains(names, snap) {
		t.Errorf("ListSnapshots = %v, want it to include %s", names, snap)
	}
}

// TestPubSubUpdateSnapshot (#1406): the emulator refuses UpdateSnapshot
// ("Method google.pubsub.v1.Subscriber/UpdateSnapshot is unimplemented");
// CloudBurrow's front applies its labels and an earlier expire_time, which
// GetSnapshot and ListSnapshots then read back, and refuses a mask naming
// anything else, as Google does.
// covers: google.pubsub.v1.Subscriber/UpdateSnapshot, google.pubsub.v1.Subscriber/GetSnapshot
func TestPubSubUpdateSnapshot(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	ctx := h.Context()

	tn := topic(t, h, c, "update-snapshot-topic")
	sn := subscription(t, h, c, "update-snapshot-subscription", tn)
	snap := snapshot(t, h, c, "update-snapshot", sn)

	cur, err := c.SubscriptionAdminClient.GetSnapshot(ctx, &pubsubpb.GetSnapshotRequest{Snapshot: snap})
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	// The front's clock, which other tests advance, is what "in the future"
	// is measured against.
	expire := advancePubSubClock(t, h, 0).Add(time.Hour).Truncate(time.Second)
	paths := []string{"labels", "expire_time"}
	if cur.GetExpireTime() == nil || !expire.Before(cur.GetExpireTime().AsTime()) {
		paths = []string{"labels"}
	}
	got, err := c.SubscriptionAdminClient.UpdateSnapshot(ctx, &pubsubpb.UpdateSnapshotRequest{
		Snapshot:   &pubsubpb.Snapshot{Name: snap, Labels: map[string]string{"k": "v"}, ExpireTime: timestamppb.New(expire)},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: paths},
	})
	if err != nil {
		t.Fatalf("UpdateSnapshot: %v", err)
	}
	if got.GetLabels()["k"] != "v" {
		t.Errorf("UpdateSnapshot answered labels %v, want k=v", got.GetLabels())
	}
	read, err := c.SubscriptionAdminClient.GetSnapshot(ctx, &pubsubpb.GetSnapshotRequest{Snapshot: snap})
	if err != nil || read.GetLabels()["k"] != "v" {
		t.Errorf("GetSnapshot after UpdateSnapshot = %v, %v; want label k=v", read, err)
	}
	if len(paths) == 2 && !read.GetExpireTime().AsTime().Equal(expire) {
		t.Errorf("GetSnapshot expire_time = %v, want %v", read.GetExpireTime().AsTime(), expire)
	}
	var labelled bool
	for _, s := range drain(t, "ListSnapshots", c.SubscriptionAdminClient.ListSnapshots(ctx,
		&pubsubpb.ListSnapshotsRequest{Project: "projects/" + h.Project()}).Next) {
		labelled = labelled || (s.GetName() == snap && s.GetLabels()["k"] == "v")
	}
	if !labelled {
		t.Errorf("ListSnapshots does not name %s with its updated labels", snap)
	}
	if _, err := c.SubscriptionAdminClient.UpdateSnapshot(ctx, &pubsubpb.UpdateSnapshotRequest{
		Snapshot:   &pubsubpb.Snapshot{Name: snap, Topic: tn},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"topic"}},
	}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("UpdateSnapshot(topic) = %v, want INVALID_ARGUMENT", err)
	}
}

// TestPubSubModifyPushConfig turns a pull subscription into a push one and
// back, re-reading the subscription each time.
// covers: google.pubsub.v1.Subscriber/ModifyPushConfig
func TestPubSubModifyPushConfig(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	ctx := h.Context()

	tn := topic(t, h, c, "push-config-topic")
	sn := subscription(t, h, c, "push-config-subscription", tn)

	// Nothing is published, so the emulator never calls this endpoint.
	const endpoint = "http://127.0.0.1:9/push"
	if err := c.SubscriptionAdminClient.ModifyPushConfig(ctx, &pubsubpb.ModifyPushConfigRequest{
		Subscription: sn, PushConfig: &pubsubpb.PushConfig{PushEndpoint: endpoint},
	}); err != nil {
		t.Fatalf("ModifyPushConfig(push): %v", err)
	}
	got, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sn})
	if err != nil {
		t.Fatalf("GetSubscription: %v", err)
	}
	if ep := got.GetPushConfig().GetPushEndpoint(); ep != endpoint {
		t.Errorf("push endpoint = %q, want %q", ep, endpoint)
	}

	// An empty push config makes it a pull subscription again.
	if err := c.SubscriptionAdminClient.ModifyPushConfig(ctx, &pubsubpb.ModifyPushConfigRequest{
		Subscription: sn, PushConfig: &pubsubpb.PushConfig{},
	}); err != nil {
		t.Fatalf("ModifyPushConfig(pull): %v", err)
	}
	got, err = c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sn})
	if err != nil {
		t.Fatalf("GetSubscription: %v", err)
	}
	if ep := got.GetPushConfig().GetPushEndpoint(); ep != "" {
		t.Errorf("push endpoint after reverting to pull = %q, want empty", ep)
	}
}

// TestPubSubDetachSubscription (#1406): the emulator refuses
// DetachSubscription ("Method google.pubsub.v1.Publisher/DetachSubscription
// is unimplemented"); CloudBurrow's front carries it out as Google documents
// it. The subscription reads back detached, on "_deleted-topic_", and is
// still listed; its topic no longer lists it; a pull fails
// FAILED_PRECONDITION; a message published afterwards reaches a sibling
// subscription and not it; and it can be deleted.
// covers: google.pubsub.v1.Publisher/DetachSubscription
func TestPubSubDetachSubscription(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	ctx := h.Context()

	tn := topic(t, h, c, "detach-topic")
	sn := subscription(t, h, c, "detach-subscription", tn)
	sibling := subscription(t, h, c, "detach-sibling", tn)

	if _, err := c.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: tn,
		Messages: []*pubsubpb.PubsubMessage{{Data: []byte("before")}}}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if _, err := c.TopicAdminClient.DetachSubscription(ctx, &pubsubpb.DetachSubscriptionRequest{Subscription: sn}); err != nil {
		t.Fatalf("DetachSubscription: %v", err)
	}
	got, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sn})
	if err != nil || !got.GetDetached() || got.GetTopic() != "_deleted-topic_" {
		t.Fatalf("GetSubscription after detach = %v, %v; want detached on _deleted-topic_", got, err)
	}
	if got := drain(t, "ListTopicSubscriptions", c.TopicAdminClient.ListTopicSubscriptions(ctx,
		&pubsubpb.ListTopicSubscriptionsRequest{Topic: tn}).Next); contains(got, sn) || !contains(got, sibling) {
		t.Errorf("ListTopicSubscriptions = %v; want %s without %s", got, sibling, sn)
	}
	var names []string
	for _, s := range drain(t, "ListSubscriptions", c.SubscriptionAdminClient.ListSubscriptions(ctx,
		&pubsubpb.ListSubscriptionsRequest{Project: "projects/" + h.Project()}).Next) {
		names = append(names, s.GetName())
	}
	if !contains(names, sn) {
		t.Errorf("ListSubscriptions = %v; want the detached %s still listed", names, sn)
	}
	if _, err := c.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: tn,
		Messages: []*pubsubpb.PubsubMessage{{Data: []byte("after")}}}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	_, err = c.SubscriptionAdminClient.Pull(ctx, &pubsubpb.PullRequest{Subscription: sn, MaxMessages: 10})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Pull from the detached subscription = %v, want FAILED_PRECONDITION", err)
	}
	seen := map[string]bool{}
	for i := 0; i < 20 && !seen["after"]; i++ {
		resp, err := c.SubscriptionAdminClient.Pull(ctx, &pubsubpb.PullRequest{Subscription: sibling, MaxMessages: 10})
		if err != nil {
			t.Fatalf("Pull from the sibling: %v", err)
		}
		for _, m := range resp.GetReceivedMessages() {
			seen[string(m.GetMessage().GetData())] = true
		}
	}
	if !seen["after"] {
		t.Errorf("the sibling subscription never received the message published after the detach: %v", seen)
	}
	if err := c.SubscriptionAdminClient.DeleteSubscription(ctx, &pubsubpb.DeleteSubscriptionRequest{Subscription: sn}); err != nil {
		t.Fatalf("DeleteSubscription of the detached subscription: %v", err)
	}
	if _, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sn}); status.Code(err) != codes.NotFound {
		t.Errorf("GetSubscription after delete = %v, want NOT_FOUND", err)
	}
}

func schemaClient(t *testing.T, h *Harness) *vkit.SchemaClient {
	t.Helper()
	// pubsubClient sets PUBSUB_EMULATOR_HOST, which the generated schema
	// client honours through the same hook as the admin clients: insecure
	// transport, no credentials.
	_ = pubsubClient(t, h)
	c, err := vkit.NewSchemaClient(h.Context())
	if err != nil {
		t.Fatalf("NewSchemaClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

const (
	avroSchemaV1 = `{"type":"record","name":"Burrow","fields":[{"name":"name","type":"string"}]}`
	avroSchemaV2 = `{"type":"record","name":"Burrow","fields":[{"name":"name","type":"string"},{"name":"depth","type":"int","default":0}]}`
)

// TestPubSubSchemaService drives every SchemaService RPC through the
// official generated client, apiv1.SchemaClient: validate, create, get,
// list, validate a message, commit a revision, list revisions, roll back,
// delete a revision and delete the schema.
// covers: google.pubsub.v1.SchemaService/ValidateSchema, google.pubsub.v1.SchemaService/CreateSchema, google.pubsub.v1.SchemaService/GetSchema, google.pubsub.v1.SchemaService/ListSchemas, google.pubsub.v1.SchemaService/ValidateMessage
// covers: google.pubsub.v1.SchemaService/CommitSchema, google.pubsub.v1.SchemaService/ListSchemaRevisions, google.pubsub.v1.SchemaService/RollbackSchema, google.pubsub.v1.SchemaService/DeleteSchemaRevision, google.pubsub.v1.SchemaService/DeleteSchema
func TestPubSubSchemaService(t *testing.T) {
	h := New(t)
	sc := schemaClient(t, h)
	ctx := h.Context()
	parent := "projects/" + h.Project()
	const id = "burrow-schema"
	name := parent + "/schemas/" + id

	if _, err := sc.ValidateSchema(ctx, &pubsubpb.ValidateSchemaRequest{Parent: parent,
		Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_AVRO, Definition: avroSchemaV1}}); err != nil {
		t.Fatalf("ValidateSchema(valid): %v", err)
	}
	if _, err := sc.ValidateSchema(ctx, &pubsubpb.ValidateSchemaRequest{Parent: parent,
		Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_AVRO, Definition: `{"type":"record"`}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("ValidateSchema(malformed) = %v, want InvalidArgument", err)
	}

	v1, err := sc.CreateSchema(ctx, &pubsubpb.CreateSchemaRequest{Parent: parent, SchemaId: id,
		Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_AVRO, Definition: avroSchemaV1}})
	if err != nil {
		t.Fatalf("CreateSchema: %v", err)
	}
	t.Cleanup(func() { _ = sc.DeleteSchema(context.Background(), &pubsubpb.DeleteSchemaRequest{Name: name}) })
	if v1.GetName() != name || v1.GetRevisionId() == "" {
		t.Fatalf("CreateSchema = name %q revision %q, want %q with a revision", v1.GetName(), v1.GetRevisionId(), name)
	}

	got, err := sc.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: name, View: pubsubpb.SchemaView_FULL})
	if err != nil {
		t.Fatalf("GetSchema: %v", err)
	}
	if got.GetDefinition() != avroSchemaV1 || got.GetType() != pubsubpb.Schema_AVRO {
		t.Errorf("GetSchema = %v %q, want the AVRO definition just created", got.GetType(), got.GetDefinition())
	}

	var listed []string
	for _, s := range drain(t, "ListSchemas", sc.ListSchemas(ctx, &pubsubpb.ListSchemasRequest{Parent: parent}).Next) {
		listed = append(listed, s.GetName())
	}
	if !contains(listed, name) {
		t.Errorf("ListSchemas = %v, want it to include %s", listed, name)
	}

	if _, err := sc.ValidateMessage(ctx, &pubsubpb.ValidateMessageRequest{Parent: parent,
		SchemaSpec: &pubsubpb.ValidateMessageRequest_Name{Name: name},
		Message:    []byte(`{"name":"x"}`), Encoding: pubsubpb.Encoding_JSON}); err != nil {
		t.Errorf("ValidateMessage(conforming): %v", err)
	}
	if _, err := sc.ValidateMessage(ctx, &pubsubpb.ValidateMessageRequest{Parent: parent,
		SchemaSpec: &pubsubpb.ValidateMessageRequest_Name{Name: name},
		Message:    []byte(`{"nope":1}`), Encoding: pubsubpb.Encoding_JSON}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("ValidateMessage(non-conforming) = %v, want InvalidArgument", err)
	}

	v2, err := sc.CommitSchema(ctx, &pubsubpb.CommitSchemaRequest{Name: name,
		Schema: &pubsubpb.Schema{Name: name, Type: pubsubpb.Schema_AVRO, Definition: avroSchemaV2}})
	if err != nil {
		t.Fatalf("CommitSchema: %v", err)
	}
	if v2.GetRevisionId() == "" || v2.GetRevisionId() == v1.GetRevisionId() {
		t.Fatalf("CommitSchema revision = %q, want a new one (v1 is %q)", v2.GetRevisionId(), v1.GetRevisionId())
	}

	var revs []string
	for _, s := range drain(t, "ListSchemaRevisions", sc.ListSchemaRevisions(ctx, &pubsubpb.ListSchemaRevisionsRequest{Name: name}).Next) {
		revs = append(revs, s.GetRevisionId())
	}
	if !contains(revs, v1.GetRevisionId()) || !contains(revs, v2.GetRevisionId()) {
		t.Errorf("ListSchemaRevisions = %v, want both %s and %s", revs, v1.GetRevisionId(), v2.GetRevisionId())
	}

	rolled, err := sc.RollbackSchema(ctx, &pubsubpb.RollbackSchemaRequest{Name: name, RevisionId: v1.GetRevisionId()})
	if err != nil {
		t.Fatalf("RollbackSchema: %v", err)
	}
	if rolled.GetDefinition() != avroSchemaV1 {
		t.Errorf("RollbackSchema definition = %q, want v1's", rolled.GetDefinition())
	}

	if _, err := sc.DeleteSchemaRevision(ctx, &pubsubpb.DeleteSchemaRevisionRequest{Name: name + "@" + v2.GetRevisionId()}); err != nil {
		t.Fatalf("DeleteSchemaRevision: %v", err)
	}
	var after []string
	for _, s := range drain(t, "ListSchemaRevisions", sc.ListSchemaRevisions(ctx, &pubsubpb.ListSchemaRevisionsRequest{Name: name}).Next) {
		after = append(after, s.GetRevisionId())
	}
	if contains(after, v2.GetRevisionId()) {
		t.Errorf("revision %s still listed after DeleteSchemaRevision: %v", v2.GetRevisionId(), after)
	}

	if err := sc.DeleteSchema(ctx, &pubsubpb.DeleteSchemaRequest{Name: name}); err != nil {
		t.Fatalf("DeleteSchema: %v", err)
	}
	if _, err := sc.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetSchema after DeleteSchema = %v, want NotFound", err)
	}
}

// TestPubSubDeadLetterDelivery nacks a message on a subscription with
// max_delivery_attempts=5 until it reaches the dead-letter topic's
// subscription. TestDeadLetterPolicyIsRecorded proves only that the policy
// round-trips; this proves the routing.
func TestPubSubDeadLetterDelivery(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	ctx := h.Context()

	mainTopic := topic(t, h, c, "dlq-delivery-main")
	deadTopic := topic(t, h, c, "dlq-delivery-dead")
	// Created before any message is dead-lettered, or there is nothing to
	// receive it on.
	deadSub := subscription(t, h, c, "dlq-delivery-dead-sub", deadTopic)
	sn := fmt.Sprintf("projects/%s/subscriptions/dlq-delivery-sub", h.Project())
	if _, err := c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{
		Name: sn, Topic: mainTopic, AckDeadlineSeconds: 10,
		DeadLetterPolicy: &pubsubpb.DeadLetterPolicy{DeadLetterTopic: deadTopic, MaxDeliveryAttempts: 5},
	}); err != nil {
		t.Fatalf("CreateSubscription with a dead-letter policy: %v", err)
	}
	t.Cleanup(func() {
		_ = c.SubscriptionAdminClient.DeleteSubscription(context.Background(), &pubsubpb.DeleteSubscriptionRequest{Subscription: sn})
	})

	pub := c.Publisher(mainTopic)
	defer pub.Stop()
	if _, err := pub.Publish(ctx, &pubsub.Message{Data: []byte("dead letter me")}).Get(ctx); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// ReturnImmediately, or an empty Pull holds the loop until its own
	// deadline and a 5-attempt run takes most of a minute.
	pull := func(sub string) []*pubsubpb.ReceivedMessage {
		t.Helper()
		resp, err := c.SubscriptionAdminClient.Pull(ctx, &pubsubpb.PullRequest{
			Subscription: sub, MaxMessages: 1, ReturnImmediately: true})
		if err != nil {
			t.Fatalf("Pull %s: %v", sub, err)
		}
		return resp.GetReceivedMessages()
	}

	var attempts []int32
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		for _, m := range pull(sn) {
			attempts = append(attempts, m.GetDeliveryAttempt())
			if err := c.SubscriptionAdminClient.ModifyAckDeadline(ctx, &pubsubpb.ModifyAckDeadlineRequest{
				Subscription: sn, AckIds: []string{m.AckId}, AckDeadlineSeconds: 0,
			}); err != nil {
				t.Fatalf("nack: %v", err)
			}
		}
		if dead := pull(deadSub); len(dead) == 1 {
			m := dead[0].GetMessage()
			if string(m.GetData()) != "dead letter me" {
				t.Errorf("dead-lettered data = %q", m.GetData())
			}
			if got := m.GetAttributes()["CloudPubSubDeadLetterSourceDeliveryCount"]; got != "5" {
				t.Errorf("CloudPubSubDeadLetterSourceDeliveryCount = %q, want 5", got)
			}
			if len(attempts) != 5 || attempts[0] != 1 || attempts[4] != 5 {
				t.Errorf("delivery attempts before dead-lettering = %v, want [1 2 3 4 5]", attempts)
			}
			_ = c.SubscriptionAdminClient.Acknowledge(ctx, &pubsubpb.AcknowledgeRequest{Subscription: deadSub, AckIds: []string{dead[0].AckId}})
			t.Logf("dead-lettered after deliveries %v; attributes %v", attempts, m.GetAttributes())
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("not dead-lettered within 20s; delivery attempts seen %v", attempts)
}
