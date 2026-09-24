//go:build actionselftest

// Package actionselftest runs after the setup-cloudburrow action, with nothing
// but the environment it exported (#282). No endpoint, project or credential
// is passed in code: if these clients reach CloudBurrow, the action set the
// job up the way `cloudburrow env` sets up a developer's shell.
package actionselftest

import (
	"context"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/storage"
)

// requireEnv fails rather than skips: a self-test that skipped because the
// action exported nothing would pass exactly when the action is broken.
func requireEnv(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		if os.Getenv(n) == "" {
			t.Fatalf("%s is not set; setup-cloudburrow did not export it", n)
		}
	}
}

func TestTheExportedEnvironmentReachesStorageAndPubSub(t *testing.T) {
	requireEnv(t, "STORAGE_EMULATOR_HOST", "PUBSUB_EMULATOR_HOST", "GOOGLE_CLOUD_PROJECT")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	project := os.Getenv("GOOGLE_CLOUD_PROJECT")
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e9)

	sc, err := storage.NewClient(ctx)
	if err != nil {
		t.Fatalf("storage.NewClient: %v", err)
	}
	defer sc.Close()
	bucket := sc.Bucket("selftest-" + suffix)
	if err := bucket.Create(ctx, project, nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	w := bucket.Object("hello.txt").NewWriter(ctx)
	_, _ = io.WriteString(w, "hello from the action self-test")
	if err := w.Close(); err != nil {
		t.Fatalf("write object: %v", err)
	}
	r, err := bucket.Object("hello.txt").NewReader(ctx)
	if err != nil {
		t.Fatalf("read object: %v", err)
	}
	got, _ := io.ReadAll(r)
	_ = r.Close()
	if string(got) != "hello from the action self-test" {
		t.Errorf("read back %q", got)
	}

	pc, err := pubsub.NewClient(ctx, project)
	if err != nil {
		t.Fatalf("pubsub.NewClient: %v", err)
	}
	defer pc.Close()
	topic := fmt.Sprintf("projects/%s/topics/selftest-%s", project, suffix)
	sub := fmt.Sprintf("projects/%s/subscriptions/selftest-%s", project, suffix)
	if _, err := pc.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatalf("create topic: %v", err)
	}
	if _, err := pc.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sub, Topic: topic}); err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	id, err := pc.Publisher(topic).Publish(ctx, &pubsub.Message{Data: []byte("ping")}).Get(ctx)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	recvCtx, stop := context.WithTimeout(ctx, 30*time.Second)
	defer stop()
	var received string
	_ = pc.Subscriber(sub).Receive(recvCtx, func(_ context.Context, m *pubsub.Message) {
		received = string(m.Data)
		m.Ack()
		stop()
	})
	if received != "ping" {
		t.Errorf("message %s was published but %q was received", id, received)
	}
}
