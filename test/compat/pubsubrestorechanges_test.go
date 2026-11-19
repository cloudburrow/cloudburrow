//go:build compat

package compat

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	pubsub "cloud.google.com/go/pubsub/v2"
	apiv1 "cloud.google.com/go/pubsub/v2/apiv1"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// changeBeforeRestart deletes and updates a definition of every kind the
// Pub/Sub front keeps for an emulator restart, before
// TestPubSubDefinitionsSurviveAnEmulatorRestart restarts the emulator, and
// returns the check run after it: on GCP a deleted topic, subscription,
// snapshot, schema or schema revision stays deleted, with its IAM policy;
// an update (UpdateTopic, UpdateSubscription, UpdateSnapshot, CommitSchema,
// RollbackSchema) and the IAM policies set stay as they were; and a schema
// or topic deleted and created again is the new one.
func changeBeforeRestart(t *testing.T, ctx context.Context, c *pubsub.Client, sch *apiv1.SchemaClient,
	project string, name func(kind, id string) string) (check func()) {
	t.Helper()
	const (
		avro1 = `{"type":"record","name":"R","fields":[{"name":"a","type":"int"}]}`
		avro2 = `{"type":"record","name":"R","fields":[{"name":"a","type":"int"},{"name":"b","type":"string","default":""}]}`
		avro3 = `{"type":"record","name":"R","fields":[{"name":"a","type":"int"},{"name":"c","type":"long","default":0}]}`
	)
	createSchema := func(id, def string) *pubsubpb.Schema {
		t.Helper()
		s, err := sch.CreateSchema(ctx, &pubsubpb.CreateSchemaRequest{Parent: project, SchemaId: id,
			Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_AVRO, Definition: def}})
		if err != nil {
			t.Fatalf("CreateSchema %s: %v", id, err)
		}
		n := s.GetName()
		t.Cleanup(func() { _ = sch.DeleteSchema(context.Background(), &pubsubpb.DeleteSchemaRequest{Name: n}) })
		return s
	}
	commit := func(n, def string) *pubsubpb.Schema {
		t.Helper()
		s, err := sch.CommitSchema(ctx, &pubsubpb.CommitSchemaRequest{Name: n,
			Schema: &pubsubpb.Schema{Name: n, Type: pubsubpb.Schema_AVRO, Definition: def}})
		if err != nil {
			t.Fatalf("CommitSchema %s: %v", n, err)
		}
		return s
	}
	createTopic := func(tp *pubsubpb.Topic) {
		t.Helper()
		if _, err := c.TopicAdminClient.CreateTopic(ctx, tp); err != nil {
			t.Fatalf("CreateTopic %s: %v", tp.GetName(), err)
		}
		n := tp.GetName()
		t.Cleanup(func() {
			_ = c.TopicAdminClient.DeleteTopic(context.Background(), &pubsubpb.DeleteTopicRequest{Topic: n})
		})
	}
	createSub := func(s *pubsubpb.Subscription) {
		t.Helper()
		if _, err := c.SubscriptionAdminClient.CreateSubscription(ctx, s); err != nil {
			t.Fatalf("CreateSubscription %s: %v", s.GetName(), err)
		}
		n := s.GetName()
		t.Cleanup(func() {
			_ = c.SubscriptionAdminClient.DeleteSubscription(context.Background(), &pubsubpb.DeleteSubscriptionRequest{Subscription: n})
		})
	}
	createSnapshot := func(n, sub string) {
		t.Helper()
		if _, err := c.SubscriptionAdminClient.CreateSnapshot(ctx, &pubsubpb.CreateSnapshotRequest{Name: n, Subscription: sub}); err != nil {
			t.Fatalf("CreateSnapshot %s: %v", n, err)
		}
		t.Cleanup(func() {
			_ = c.SubscriptionAdminClient.DeleteSnapshot(context.Background(), &pubsubpb.DeleteSnapshotRequest{Snapshot: n})
		})
	}
	policy := &iampb.Policy{Bindings: []*iampb.Binding{{Role: "roles/pubsub.viewer", Members: []string{"user:ada@example.com"}}}}
	setIAM := func(resource string) *iampb.Policy {
		t.Helper()
		p, err := sch.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: resource, Policy: policy})
		if err != nil {
			t.Fatalf("SetIamPolicy %s: %v", resource, err)
		}
		return p
	}

	// Deleted: a schema, a topic, a subscription and a snapshot, each with
	// an IAM policy set first.
	goneSchema, goneTopic, goneSub, goneSnap := name("schemas", "gone"), name("topics", "gone"), name("subscriptions", "gone"), name("snapshots", "gone")
	createSchema("restore1608-gone", avro1)
	createTopic(&pubsubpb.Topic{Name: goneTopic})
	createSub(&pubsubpb.Subscription{Name: goneSub, Topic: goneTopic})
	createSnapshot(goneSnap, goneSub)
	for _, n := range []string{goneSchema, goneTopic, goneSub, goneSnap} {
		setIAM(n)
	}
	if err := c.SubscriptionAdminClient.DeleteSnapshot(ctx, &pubsubpb.DeleteSnapshotRequest{Snapshot: goneSnap}); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}
	if err := c.SubscriptionAdminClient.DeleteSubscription(ctx, &pubsubpb.DeleteSubscriptionRequest{Subscription: goneSub}); err != nil {
		t.Fatalf("DeleteSubscription: %v", err)
	}
	if err := c.TopicAdminClient.DeleteTopic(ctx, &pubsubpb.DeleteTopicRequest{Topic: goneTopic}); err != nil {
		t.Fatalf("DeleteTopic: %v", err)
	}
	if err := sch.DeleteSchema(ctx, &pubsubpb.DeleteSchemaRequest{Name: goneSchema}); err != nil {
		t.Fatalf("DeleteSchema: %v", err)
	}

	// A topic whose schema is deleted names "_deleted-schema_".
	orphanSchema, orphanTopic := name("schemas", "orphan"), name("topics", "orphan")
	createSchema("restore1608-orphan", avro1)
	createTopic(&pubsubpb.Topic{Name: orphanTopic, SchemaSettings: &pubsubpb.SchemaSettings{Schema: orphanSchema, Encoding: pubsubpb.Encoding_JSON}})
	if err := sch.DeleteSchema(ctx, &pubsubpb.DeleteSchemaRequest{Name: orphanSchema}); err != nil {
		t.Fatalf("DeleteSchema %s: %v", orphanSchema, err)
	}
	publishOrphan := func() string {
		_, err := c.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: orphanTopic,
			Messages: []*pubsubpb.PubsubMessage{{Data: []byte(`{"a": 1}`)}}})
		return status.Code(err).String() + ": " + status.Convert(err).Message()
	}
	orphanPublish := publishOrphan()
	t.Logf("a publish to a topic whose schema is deleted: %s", orphanPublish)

	// Deleted and created again, different.
	againSchema, againTopic := name("schemas", "again"), name("topics", "again")
	createSchema("restore1608-again", avro1)
	commit(againSchema, avro2)
	setIAM(againSchema)
	if err := sch.DeleteSchema(ctx, &pubsubpb.DeleteSchemaRequest{Name: againSchema}); err != nil {
		t.Fatalf("DeleteSchema %s: %v", againSchema, err)
	}
	createSchema("restore1608-again", avro3)
	createTopic(&pubsubpb.Topic{Name: againTopic, Labels: map[string]string{"v": "1"}})
	setIAM(againTopic)
	if err := c.TopicAdminClient.DeleteTopic(ctx, &pubsubpb.DeleteTopicRequest{Topic: againTopic}); err != nil {
		t.Fatalf("DeleteTopic %s: %v", againTopic, err)
	}
	createTopic(&pubsubpb.Topic{Name: againTopic, Labels: map[string]string{"v": "2"}})

	// A schema's revisions: committed, one deleted, one rolled back to.
	revsSchema := name("schemas", "revs")
	r1 := createSchema("restore1608-revs", avro1)
	r2 := commit(revsSchema, avro2)
	commit(revsSchema, avro3)
	if _, err := sch.DeleteSchemaRevision(ctx, &pubsubpb.DeleteSchemaRevisionRequest{Name: revsSchema + "@" + r2.GetRevisionId()}); err != nil {
		t.Fatalf("DeleteSchemaRevision: %v", err)
	}
	if _, err := sch.RollbackSchema(ctx, &pubsubpb.RollbackSchemaRequest{Name: revsSchema, RevisionId: r1.GetRevisionId()}); err != nil {
		t.Fatalf("RollbackSchema: %v", err)
	}

	// Updated: a topic, a subscription and a snapshot.
	updTopic, updSub, updSnap := name("topics", "upd"), name("subscriptions", "upd"), name("snapshots", "upd")
	createTopic(&pubsubpb.Topic{Name: updTopic, Labels: map[string]string{"v": "1"}})
	createSub(&pubsubpb.Subscription{Name: updSub, Topic: updTopic, AckDeadlineSeconds: 10, Labels: map[string]string{"v": "1"}})
	createSnapshot(updSnap, updSub)
	if _, err := c.TopicAdminClient.UpdateTopic(ctx, &pubsubpb.UpdateTopicRequest{
		Topic:      &pubsubpb.Topic{Name: updTopic, Labels: map[string]string{"v": "2"}, MessageRetentionDuration: durationpb.New(30 * time.Hour)},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels", "message_retention_duration"}}}); err != nil {
		t.Fatalf("UpdateTopic: %v", err)
	}
	if _, err := c.SubscriptionAdminClient.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{
		Subscription: &pubsubpb.Subscription{Name: updSub, AckDeadlineSeconds: 33, Labels: map[string]string{"v": "2"},
			ExpirationPolicy:    &pubsubpb.ExpirationPolicy{Ttl: durationpb.New(10 * 24 * time.Hour)},
			RetainAckedMessages: true},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"ack_deadline_seconds", "labels", "expiration_policy", "retain_acked_messages"}}}); err != nil {
		t.Fatalf("UpdateSubscription: %v", err)
	}
	if _, err := c.SubscriptionAdminClient.UpdateSnapshot(ctx, &pubsubpb.UpdateSnapshotRequest{
		Snapshot:   &pubsubpb.Snapshot{Name: updSnap, Labels: map[string]string{"v": "2"}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}}); err != nil {
		t.Fatalf("UpdateSnapshot: %v", err)
	}
	policies := map[string]*iampb.Policy{}
	for _, n := range []string{updTopic, updSub, updSnap, revsSchema} {
		policies[n] = setIAM(n)
	}

	type state struct {
		topics    map[string]*pubsubpb.Topic
		subs      map[string]*pubsubpb.Subscription
		snapshots map[string]*pubsubpb.Snapshot
		schemas   map[string][]string // each schema's revisions' definitions, as listed
		listed    []string            // every topic, subscription, snapshot and schema listed
	}
	read := func() state {
		t.Helper()
		s := state{topics: map[string]*pubsubpb.Topic{}, subs: map[string]*pubsubpb.Subscription{},
			snapshots: map[string]*pubsubpb.Snapshot{}, schemas: map[string][]string{}}
		for _, n := range []string{orphanTopic, againTopic, updTopic} {
			tp, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: n})
			if err != nil {
				t.Fatalf("GetTopic %s: %v", n, err)
			}
			s.topics[n] = tp
		}
		sub, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: updSub})
		if err != nil {
			t.Fatalf("GetSubscription %s: %v", updSub, err)
		}
		s.subs[updSub] = sub
		snap, err := c.SubscriptionAdminClient.GetSnapshot(ctx, &pubsubpb.GetSnapshotRequest{Snapshot: updSnap})
		if err != nil {
			t.Fatalf("GetSnapshot %s: %v", updSnap, err)
		}
		s.snapshots[updSnap] = snap
		for _, n := range []string{againSchema, revsSchema} {
			it := sch.ListSchemaRevisions(ctx, &pubsubpb.ListSchemaRevisionsRequest{Name: n, View: pubsubpb.SchemaView_FULL})
			for {
				r, err := it.Next()
				if err == iterator.Done {
					break
				}
				if err != nil {
					t.Fatalf("ListSchemaRevisions %s: %v", n, err)
				}
				s.schemas[n] = append(s.schemas[n], r.GetDefinition())
			}
		}
		lt := c.TopicAdminClient.ListTopics(ctx, &pubsubpb.ListTopicsRequest{Project: project})
		for tp, err := lt.Next(); err == nil; tp, err = lt.Next() {
			s.listed = append(s.listed, tp.GetName())
		}
		ls := c.SubscriptionAdminClient.ListSubscriptions(ctx, &pubsubpb.ListSubscriptionsRequest{Project: project})
		for x, err := ls.Next(); err == nil; x, err = ls.Next() {
			s.listed = append(s.listed, x.GetName())
		}
		lsn := c.SubscriptionAdminClient.ListSnapshots(ctx, &pubsubpb.ListSnapshotsRequest{Project: project})
		for x, err := lsn.Next(); err == nil; x, err = lsn.Next() {
			s.listed = append(s.listed, x.GetName())
		}
		lsc := sch.ListSchemas(ctx, &pubsubpb.ListSchemasRequest{Parent: project})
		for x, err := lsc.Next(); err == nil; x, err = lsc.Next() {
			s.listed = append(s.listed, x.GetName())
		}
		return s
	}
	before := read()
	if got := before.schemas[revsSchema]; len(got) != 3 {
		t.Fatalf("before the restart %s has the revisions %q, want 3", revsSchema, got)
	}
	if got := before.schemas[againSchema]; len(got) != 1 || got[0] != avro3 {
		t.Fatalf("before the restart %s has the revisions %q, want the new one alone", againSchema, got)
	}

	return func() {
		t.Helper()
		after := read()
		for n, want := range before.topics {
			if !proto.Equal(after.topics[n], want) {
				t.Errorf("after the emulator restarted, the topic reads\n%v\nwant\n%v", after.topics[n], want)
			}
		}
		for n, want := range before.subs {
			if !proto.Equal(after.subs[n], want) {
				t.Errorf("after the emulator restarted, the updated subscription reads\n%v\nwant\n%v", after.subs[n], want)
			}
		}
		for n, want := range before.snapshots {
			got := after.snapshots[n]
			if got.GetTopic() != want.GetTopic() || fmt.Sprint(got.GetLabels()) != fmt.Sprint(want.GetLabels()) ||
				got.GetExpireTime().AsTime().After(want.GetExpireTime().AsTime()) {
				t.Errorf("after the emulator restarted, the updated snapshot reads %v; want %v, expiring no later", got, want)
			}
		}
		for n, want := range before.schemas {
			if got := after.schemas[n]; !slices.Equal(got, want) {
				t.Errorf("after the emulator restarted, %s has the revisions\n%q\nwant\n%q", n, got, want)
			}
		}
		for _, n := range []string{goneSchema, goneTopic, goneSub, goneSnap} {
			if slices.Contains(after.listed, n) {
				t.Errorf("after the emulator restarted, the deleted %s is listed again", n)
			}
			if _, err := sch.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: n}); status.Code(err) != codes.NotFound {
				t.Errorf("after the emulator restarted, GetIamPolicy on the deleted %s: %v; want NOT_FOUND", n, err)
			}
		}
		if _, err := sch.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: goneSchema}); status.Code(err) != codes.NotFound {
			t.Errorf("after the emulator restarted, GetSchema on the deleted schema: %v; want NOT_FOUND", err)
		}
		if _, err := sch.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: orphanSchema}); status.Code(err) != codes.NotFound {
			t.Errorf("after the emulator restarted, GetSchema on the deleted schema %s: %v; want NOT_FOUND", orphanSchema, err)
		}
		if _, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: goneTopic}); status.Code(err) != codes.NotFound {
			t.Errorf("after the emulator restarted, GetTopic on the deleted topic: %v; want NOT_FOUND", err)
		}
		if _, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: goneSub}); status.Code(err) != codes.NotFound {
			t.Errorf("after the emulator restarted, GetSubscription on the deleted subscription: %v; want NOT_FOUND", err)
		}
		if _, err := c.SubscriptionAdminClient.GetSnapshot(ctx, &pubsubpb.GetSnapshotRequest{Snapshot: goneSnap}); status.Code(err) != codes.NotFound {
			t.Errorf("after the emulator restarted, GetSnapshot on the deleted snapshot: %v; want NOT_FOUND", err)
		}
		if got := publishOrphan(); got != orphanPublish {
			t.Errorf("after the emulator restarted, a publish to %s, whose schema is deleted, gives %v; want %v, as before", orphanTopic, got, orphanPublish)
		}
		// Created again: no IAM policy of the deleted one's.
		for _, n := range []string{againSchema, againTopic} {
			p, err := sch.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: n})
			if err != nil || len(p.GetBindings()) != 0 {
				t.Errorf("after the emulator restarted, the IAM policy of %s, created again, is %v, %v; want empty", n, p, err)
			}
		}
		for n, want := range policies {
			p, err := sch.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: n})
			if err != nil || !proto.Equal(p, want) {
				t.Errorf("after the emulator restarted, the IAM policy of %s is %v, %v; want %v", n, p, err, want)
			}
		}
	}
}
