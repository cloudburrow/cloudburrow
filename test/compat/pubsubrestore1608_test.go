//go:build compat

package compat

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	apiv1 "cloud.google.com/go/pubsub/v2/apiv1"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

// restartPubSubEmulator stops the Pub/Sub emulator's container alone, as
// its exit on OutOfMemoryError does (#1603), through the container runtime
// of the kind node the pod runs on, and waits for the kubelet to start it
// again, ready. The front's container is not touched.
func restartPubSubEmulator(t *testing.T) {
	t.Helper()
	kubeconfig := strings.TrimSpace(os.Getenv(envKubeconfig))
	if kubeconfig == "" {
		t.Skipf("%s is not set; the emulator's container is restarted through the cluster", envKubeconfig)
	}
	for _, bin := range []string{"docker", "kubectl"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is not on PATH; the emulator's container is stopped through the kind node", bin)
		}
	}
	ns := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_NAMESPACE"))
	if ns == "" {
		ns = "cloudburrow"
	}
	kc := func(ctx context.Context, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, "kubectl", append([]string{"--kubeconfig", kubeconfig, "-n", ns}, args...)...).CombinedOutput()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	before := readFrontPod(t, ctx, kc, "pubsub")
	front, emulator := before.containers["front"], before.containers["pubsub"]
	if front.id == "" || emulator.id == "" {
		t.Fatalf("the pubsub pod %s has containers %v; want front and pubsub", before.name, before.containers)
	}
	if out, err := exec.CommandContext(ctx, "docker", "exec", before.node, "crictl", "stop", emulator.id).CombinedOutput(); err != nil {
		t.Fatalf("crictl stop the emulator on %s: %v\n%s", before.node, err, out)
	}
	for {
		now := readFrontPod(t, ctx, kc, "pubsub")
		f, e := now.containers["front"], now.containers["pubsub"]
		if now.name != before.name {
			t.Fatalf("the pod was replaced (%s, then %s); only the emulator's container was to restart", before.name, now.name)
		}
		if f.restarts != front.restarts || f.id != front.id {
			t.Fatalf("the front's container restarted too: %+v, then %+v", front, f)
		}
		if e.restarts > emulator.restarts && e.ready {
			t.Logf("the emulator restarted alone: container %s, restarts %d", e.id, e.restarts)
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the emulator did not come back ready: %+v", e)
		case <-time.After(time.Second):
		}
	}
}

// TestPubSubDefinitionsSurviveAnEmulatorRestart (#1608): the emulator's
// container restarting, empty, as it does when its bounded heap fills
// (#1603), loses no topic, subscription, schema or snapshot: the front
// keeps their definitions and creates them again before it serves another
// call. Through the official clients: a topic with labels and a retention,
// an AVRO schema with two revisions and a topic bound to it, a pull
// subscription with a filter, ordering, a dead-letter policy and an updated
// expiration policy, a push subscription, and a snapshot all read back as
// they did; a message published before the restart and never acknowledged
// is gone (the emulator's memory, documented); and publishing and pulling
// work on what came back.
//
// covers: google.pubsub.v1.Publisher/GetTopic, google.pubsub.v1.Subscriber/GetSubscription, google.pubsub.v1.SchemaService/ListSchemaRevisions, google.pubsub.v1.Subscriber/GetSnapshot, google.pubsub.v1.Publisher/Publish, google.pubsub.v1.Subscriber/Pull
func TestPubSubDefinitionsSurviveAnEmulatorRestart(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	ctx := h.Context()
	sch, err := apiv1.NewSchemaClient(ctx, option.WithEndpoint(h.Endpoint(EnvPubSub)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer sch.Close()
	project := "projects/" + h.Project()
	name := func(kind, id string) string { return fmt.Sprintf("%s/%s/restore1608-%s", project, kind, id) }

	const avro = `{"type":"record","name":"R","fields":[{"name":"n","type":"int"}]}`
	schema := name("schemas", "schema")
	if _, err := sch.CreateSchema(ctx, &pubsubpb.CreateSchemaRequest{Parent: project, SchemaId: "restore1608-schema",
		Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_AVRO, Definition: avro}}); err != nil {
		t.Fatalf("CreateSchema: %v", err)
	}
	t.Cleanup(func() { _ = sch.DeleteSchema(context.Background(), &pubsubpb.DeleteSchemaRequest{Name: schema}) })
	if _, err := sch.CommitSchema(ctx, &pubsubpb.CommitSchemaRequest{Name: schema,
		Schema: &pubsubpb.Schema{Name: schema, Type: pubsubpb.Schema_AVRO,
			Definition: `{"type":"record","name":"R","fields":[{"name":"n","type":"int"},{"name":"m","type":"string","default":""}]}`}}); err != nil {
		t.Fatalf("CommitSchema: %v", err)
	}

	topicName, bound, dead := name("topics", "topic"), name("topics", "bound"), name("topics", "dead")
	for _, tp := range []*pubsubpb.Topic{
		{Name: topicName, Labels: map[string]string{"team": "restore"}, MessageRetentionDuration: durationpb.New(36 * time.Hour)},
		{Name: bound, SchemaSettings: &pubsubpb.SchemaSettings{Schema: schema, Encoding: pubsubpb.Encoding_JSON}},
		{Name: dead},
	} {
		if _, err := c.TopicAdminClient.CreateTopic(ctx, tp); err != nil {
			t.Fatalf("CreateTopic %s: %v", tp.GetName(), err)
		}
		n := tp.GetName()
		t.Cleanup(func() {
			_ = c.TopicAdminClient.DeleteTopic(context.Background(), &pubsubpb.DeleteTopicRequest{Topic: n})
		})
	}
	pull, push := name("subscriptions", "pull"), name("subscriptions", "push")
	for _, s := range []*pubsubpb.Subscription{
		{Name: pull, Topic: topicName, AckDeadlineSeconds: 45, Filter: `attributes.keep = "yes"`, EnableMessageOrdering: true,
			RetainAckedMessages: true, MessageRetentionDuration: durationpb.New(2 * 24 * time.Hour),
			Labels:           map[string]string{"env": "restore"},
			DeadLetterPolicy: &pubsubpb.DeadLetterPolicy{DeadLetterTopic: dead, MaxDeliveryAttempts: 7},
			RetryPolicy:      &pubsubpb.RetryPolicy{MinimumBackoff: durationpb.New(5 * time.Second), MaximumBackoff: durationpb.New(60 * time.Second)}},
		{Name: push, Topic: topicName, PushConfig: &pubsubpb.PushConfig{PushEndpoint: "http://localhost:1/never"}},
	} {
		if _, err := c.SubscriptionAdminClient.CreateSubscription(ctx, s); err != nil {
			t.Fatalf("CreateSubscription %s: %v", s.GetName(), err)
		}
		n := s.GetName()
		t.Cleanup(func() {
			_ = c.SubscriptionAdminClient.DeleteSubscription(context.Background(), &pubsubpb.DeleteSubscriptionRequest{Subscription: n})
		})
	}
	snapshot := name("snapshots", "snap")
	if _, err := c.SubscriptionAdminClient.CreateSnapshot(ctx, &pubsubpb.CreateSnapshotRequest{Name: snapshot, Subscription: pull,
		Labels: map[string]string{"kept": "yes"}}); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	t.Cleanup(func() {
		_ = c.SubscriptionAdminClient.DeleteSnapshot(context.Background(), &pubsubpb.DeleteSnapshotRequest{Snapshot: snapshot})
	})
	if _, err := c.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: topicName, Messages: []*pubsubpb.PubsubMessage{
		{Data: []byte("before"), Attributes: map[string]string{"keep": "yes"}, OrderingKey: "k"}}}); err != nil {
		t.Fatalf("Publish before the restart: %v", err)
	}

	read := func() (topics []*pubsubpb.Topic, subs []*pubsubpb.Subscription, revs int, snap *pubsubpb.Snapshot) {
		t.Helper()
		for _, n := range []string{topicName, bound, dead} {
			var tp *pubsubpb.Topic
			if err := untilServed(t, func() (err error) {
				tp, err = c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: n})
				return err
			}); err != nil {
				t.Fatalf("GetTopic %s: %v", n, err)
			}
			topics = append(topics, tp)
		}
		for _, n := range []string{pull, push} {
			s, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: n})
			if err != nil {
				t.Fatalf("GetSubscription %s: %v", n, err)
			}
			subs = append(subs, s)
		}
		it := sch.ListSchemaRevisions(ctx, &pubsubpb.ListSchemaRevisionsRequest{Name: schema})
		for {
			_, err := it.Next()
			if err != nil {
				break
			}
			revs++
		}
		snap, err := c.SubscriptionAdminClient.GetSnapshot(ctx, &pubsubpb.GetSnapshotRequest{Snapshot: snapshot})
		if err != nil {
			t.Fatalf("GetSnapshot: %v", err)
		}
		return topics, subs, revs, snap
	}
	topicsBefore, subsBefore, revsBefore, snapBefore := read()
	if revsBefore != 2 {
		t.Fatalf("before the restart the schema has %d revisions, want 2", revsBefore)
	}

	restartPubSubEmulator(t)

	topicsAfter, subsAfter, revsAfter, snapAfter := read()
	for i := range topicsBefore {
		if !proto.Equal(topicsBefore[i], topicsAfter[i]) {
			t.Errorf("after the emulator restarted, the topic reads\n%v\nwant\n%v", topicsAfter[i], topicsBefore[i])
		}
	}
	for i := range subsBefore {
		if !proto.Equal(subsBefore[i], subsAfter[i]) {
			t.Errorf("after the emulator restarted, the subscription reads\n%v\nwant\n%v", subsAfter[i], subsBefore[i])
		}
	}
	if revsAfter != 2 {
		t.Errorf("after the emulator restarted, the schema has %d revisions, want 2", revsAfter)
	}
	if snapAfter.GetTopic() != snapBefore.GetTopic() || fmt.Sprint(snapAfter.GetLabels()) != fmt.Sprint(snapBefore.GetLabels()) ||
		snapAfter.GetExpireTime().AsTime().After(snapBefore.GetExpireTime().AsTime()) {
		t.Errorf("after the emulator restarted, the snapshot reads %v; want %v, expiring no later", snapAfter, snapBefore)
	}

	// The unacknowledged message went with the emulator's memory; a new one
	// is published and pulled.
	if _, err := c.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: topicName, Messages: []*pubsubpb.PubsubMessage{
		{Data: []byte("after"), Attributes: map[string]string{"keep": "yes"}, OrderingKey: "k"}}}); err != nil {
		t.Fatalf("Publish after the restart: %v", err)
	}
	var got []string
	for deadline := time.Now().Add(30 * time.Second); len(got) == 0 && time.Now().Before(deadline); {
		r, err := c.SubscriptionAdminClient.Pull(ctx, &pubsubpb.PullRequest{Subscription: pull, MaxMessages: 10})
		if err != nil && status.Code(err) != codes.DeadlineExceeded {
			t.Fatalf("Pull after the restart: %v", err)
		}
		var ids []string
		for _, m := range r.GetReceivedMessages() {
			got = append(got, string(m.GetMessage().GetData()))
			ids = append(ids, m.GetAckId())
		}
		if len(ids) > 0 {
			if err := c.SubscriptionAdminClient.Acknowledge(ctx, &pubsubpb.AcknowledgeRequest{Subscription: pull, AckIds: ids}); err != nil {
				t.Fatalf("Acknowledge after the restart: %v", err)
			}
		}
	}
	if strings.Join(got, ",") != "after" {
		t.Errorf("after the restart, Pull gave %q; want only the message published since (the one before is lost with the emulator)", got)
	}
	// The bound topic takes a message of its schema.
	if _, err := c.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: bound, Messages: []*pubsubpb.PubsubMessage{
		{Data: []byte(`{"n": 1, "m": "x"}`)}}}); err != nil {
		t.Errorf("Publish a conforming message to the bound topic after the restart: %v", err)
	}
}
