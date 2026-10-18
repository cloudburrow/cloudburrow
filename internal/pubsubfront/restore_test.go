package pubsubfront

import (
	"context"
	"net"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/pubsub/v2/pstest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

// restartEmulator stops the fake emulator and starts a new, empty one on
// its port once the front has seen it end, as it sees a process's end at
// once.
func restartEmulator(t *testing.T, f *Front, fake *pstest.Server, port int) *pstest.Server {
	t.Helper()
	_ = fake.Close()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, serving := f.readGen(); !serving {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the front never saw the emulator end")
		}
	}
	return pstest.NewServerWithPort(port)
}

const restoreAvro = `{"type":"record","name":"R","fields":[{"name":"n","type":"int"}]}`

// freePort is a loopback port nothing listens on.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// TestRestoreAfterTheEmulatorRestarts (#1608): the emulator restarting
// empty under the front, as Google's does on OutOfMemoryError, gets back
// every topic, subscription and schema, each as it was, before the front
// serves another call; and publish and pull work on them.
func TestRestoreAfterTheEmulatorRestarts(t *testing.T) {
	port := freePort(t)
	fake := pstest.NewServerWithPort(port)
	t.Cleanup(func() { _ = fake.Close() }) // the last one
	upstream := "127.0.0.1:" + strconv.Itoa(port)
	r := runFront(t, upstream, filepath.Join(t.TempDir(), "state.json"))
	ctx := context.Background()
	topics, subs, schemas := r.client.TopicAdminClient, r.client.SubscriptionAdminClient, pubsubpb.NewSchemaServiceClient(r.conn)

	schema := "projects/" + project + "/schemas/kept"
	s1, err := schemas.CreateSchema(ctx, &pubsubpb.CreateSchemaRequest{Parent: "projects/" + project, SchemaId: "kept",
		Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_AVRO, Definition: restoreAvro}})
	if err != nil {
		t.Fatalf("CreateSchema: %v", err)
	}
	if _, err := schemas.CommitSchema(ctx, &pubsubpb.CommitSchemaRequest{Name: schema,
		Schema: &pubsubpb.Schema{Name: schema, Type: pubsubpb.Schema_AVRO, Definition: restoreAvro}}); err != nil {
		t.Fatalf("CommitSchema: %v", err)
	}
	topic := "projects/" + project + "/topics/kept"
	if _, err := topics.CreateTopic(ctx, &pubsubpb.Topic{Name: topic, Labels: map[string]string{"team": "a"},
		MessageRetentionDuration: durationpb.New(48 * time.Hour)}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	bound := "projects/" + project + "/topics/bound"
	if _, err := topics.CreateTopic(ctx, &pubsubpb.Topic{Name: bound, SchemaSettings: &pubsubpb.SchemaSettings{
		Schema: schema, Encoding: pubsubpb.Encoding_JSON, FirstRevisionId: s1.GetRevisionId()}}); err != nil {
		t.Fatalf("CreateTopic bound: %v", err)
	}
	sub := "projects/" + project + "/subscriptions/kept"
	want := &pubsubpb.Subscription{Name: sub, Topic: topic, AckDeadlineSeconds: 42, Filter: `attributes.k = "v"`,
		RetainAckedMessages: true, MessageRetentionDuration: durationpb.New(2 * 24 * time.Hour),
		Labels: map[string]string{"env": "test"}, EnableMessageOrdering: true,
		ExpirationPolicy: &pubsubpb.ExpirationPolicy{Ttl: durationpb.New(10 * 24 * time.Hour)}}
	if _, err := subs.CreateSubscription(ctx, want); err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}
	gone := "projects/" + project + "/subscriptions/gone"
	if _, err := subs.CreateSubscription(ctx, &pubsubpb.Subscription{Name: gone, Topic: topic}); err != nil {
		t.Fatal(err)
	}
	if err := subs.DeleteSubscription(ctx, &pubsubpb.DeleteSubscriptionRequest{Subscription: gone}); err != nil {
		t.Fatal(err)
	}
	before, err := subs.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sub})
	if err != nil {
		t.Fatal(err)
	}
	beforeTopic, err := topics.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: topic})
	if err != nil {
		t.Fatal(err)
	}

	// The emulator ends, and a new one starts empty on its port.
	fake = restartEmulator(t, r.front, fake, port)

	// The first call after the restart already finds everything: calls
	// wait while the front creates it again.
	got, err := subs.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sub})
	if err != nil {
		t.Fatalf("GetSubscription after the restart: %v", err)
	}
	if !proto.Equal(got, before) {
		t.Errorf("after the restart the subscription reads\n%v\nwant\n%v", got, before)
	}
	if gt, err := topics.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: topic}); err != nil || !proto.Equal(gt, beforeTopic) {
		t.Errorf("after the restart the topic reads %v, %v; want %v", gt, err, beforeTopic)
	}
	if bt, err := topics.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: bound}); err != nil || bt.GetSchemaSettings().GetSchema() != schema {
		t.Errorf("after the restart the bound topic reads %v, %v", bt, err)
	}
	if _, err := subs.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: gone}); status.Code(err) != codes.NotFound {
		t.Errorf("the deleted subscription after the restart: %v, want NotFound", err)
	}
	if revs, err := schemas.ListSchemaRevisions(ctx, &pubsubpb.ListSchemaRevisionsRequest{Name: schema}); err != nil || len(revs.GetSchemas()) != 2 {
		t.Errorf("after the restart the schema's revisions are %v, %v; want 2", revs, err)
	}

	// Publish and pull work on what came back.
	if _, err := r.client.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: "projects/" + project + "/topics/plain"}); err != nil {
		t.Fatal(err)
	}
	if _, err := subs.CreateSubscription(ctx, &pubsubpb.Subscription{Name: "projects/" + project + "/subscriptions/plain",
		Topic: "projects/" + project + "/topics/plain"}); err != nil {
		t.Fatal(err)
	}
	if _, err := topics.Publish(ctx, &pubsubpb.PublishRequest{Topic: topic,
		Messages: []*pubsubpb.PubsubMessage{{Data: []byte("x"), Attributes: map[string]string{"k": "v"}}}}); err != nil {
		t.Fatalf("Publish after the restart: %v", err)
	}
	pulled, err := subs.Pull(ctx, &pubsubpb.PullRequest{Subscription: sub, MaxMessages: 1})
	if err != nil || len(pulled.GetReceivedMessages()) != 1 || string(pulled.GetReceivedMessages()[0].GetMessage().GetData()) != "x" {
		t.Fatalf("Pull after the restart: %v, %v", pulled, err)
	}

	// A second restart brings back what was made after the first.
	fake = restartEmulator(t, r.front, fake, port)
	if _, err = subs.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: "projects/" + project + "/subscriptions/plain"}); err != nil {
		t.Fatalf("the subscription made after the first restart, after the second: %v", err)
	}
}

// TestRestoredDefinitionsOutliveTheFront (#1608): the definitions are in
// the state file, so a front that restarts along with the emulator (both
// containers) finds a new, empty emulator and creates them again.
func TestRestoredDefinitionsOutliveTheFront(t *testing.T) {
	port := freePort(t)
	fake := pstest.NewServerWithPort(port)
	upstream := "127.0.0.1:" + strconv.Itoa(port)
	path := filepath.Join(t.TempDir(), "state.json")
	r := runFront(t, upstream, path)
	ctx := context.Background()
	topic := "projects/" + project + "/topics/both"
	if _, err := r.client.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.client.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{
		Name: "projects/" + project + "/subscriptions/both", Topic: topic, AckDeadlineSeconds: 30}); err != nil {
		t.Fatal(err)
	}
	r.stop()
	_ = fake.Close()
	fake = pstest.NewServerWithPort(port)
	t.Cleanup(func() { _ = fake.Close() })

	r2 := runFront(t, upstream, path)
	s, err := r2.client.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{
		Subscription: "projects/" + project + "/subscriptions/both"})
	if err != nil || s.GetAckDeadlineSeconds() != 30 || s.GetTopic() != topic {
		t.Fatalf("after both restarted: %v, %v", s, err)
	}
}

// TestMutationTargets: the calls that change a definition, and the
// resource each names, over gRPC and REST.
func TestMutationTargets(t *testing.T) {
	enc := func(m proto.Message) frame { b, _ := proto.Marshal(m); return b }
	for _, c := range []struct {
		method     string
		fr         frame
		kind, name string
	}{
		{publisher + "CreateTopic", enc(&pubsubpb.Topic{Name: "projects/p/topics/t"}), kindTopic, "projects/p/topics/t"},
		{publisher + "Publish", enc(&pubsubpb.PublishRequest{Topic: "projects/p/topics/t"}), "", ""},
		{subscriber + "UpdateSubscription", enc(&pubsubpb.UpdateSubscriptionRequest{Subscription: &pubsubpb.Subscription{Name: "projects/p/subscriptions/s"}}),
			kindSub, "projects/p/subscriptions/s"},
		{subscriber + "Acknowledge", enc(&pubsubpb.AcknowledgeRequest{Subscription: "projects/p/subscriptions/s"}), "", ""},
		{schemaService + "CreateSchema", enc(&pubsubpb.CreateSchemaRequest{Parent: "projects/p", SchemaId: "x"}), kindSchema, "projects/p/schemas/x"},
		{subscriber + "DeleteSnapshot", enc(&pubsubpb.DeleteSnapshotRequest{Snapshot: "projects/p/snapshots/n"}), kindSnapshot, "projects/p/snapshots/n"},
	} {
		if k, n := mutationTarget(c.method, c.fr); k != c.kind || n != c.name {
			t.Errorf("mutationTarget(%s) = %q %q, want %q %q", c.method, k, n, c.kind, c.name)
		}
	}
	for _, c := range []struct {
		method, path, schemaID string
		kind, name             string
	}{
		{"PUT", "/v1/projects/p/topics/t", "", kindTopic, "projects/p/topics/t"},
		{"POST", "/v1/projects/p/topics/t:publish", "", "", ""},
		{"GET", "/v1/projects/p/topics/t", "", "", ""},
		{"PATCH", "/v1/projects/p/subscriptions/s", "", kindSub, "projects/p/subscriptions/s"},
		{"POST", "/v1/projects/p/subscriptions/s:pull", "", "", ""},
		{"POST", "/v1/projects/p/subscriptions/s:modifyPushConfig", "", kindSub, "projects/p/subscriptions/s"},
		{"POST", "/v1/projects/p/schemas", "x", kindSchema, "projects/p/schemas/x"},
		{"POST", "/v1/projects/p/schemas/x:commit", "", kindSchema, "projects/p/schemas/x"},
		{"DELETE", "/v1/projects/p/snapshots/n", "", kindSnapshot, "projects/p/snapshots/n"},
	} {
		if k, n := restMutationTarget(c.method, c.path, c.schemaID); k != c.kind || n != c.name {
			t.Errorf("restMutationTarget(%s %s) = %q %q, want %q %q", c.method, c.path, k, n, c.kind, c.name)
		}
	}
}
