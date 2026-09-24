//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	pubsub "cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/storage"
)

// TestObjectUploadDeliversANotification is #79's acceptance criterion: an
// upload through the official Storage SDK produces a Pub/Sub message with the
// standard attributes and the official Storage Object payload, received
// through the official Pub/Sub SDK.
//
// Nothing publishes the event by hand. The storage backend emits it and
// CloudBurrow routes it to the topic the notificationConfig names.
func TestObjectUploadDeliversANotification(t *testing.T) {
	h := New(t)
	sc := storageClient(t, h)
	ps := pubsubClient(t, h)
	ctx := h.Context()

	topicID := "notify-" + h.Project()
	topicName := topic(t, h, ps, topicID)
	sub := subscription(t, h, ps, topicID+"-sub", topicName)

	bucket := h.Project() + "-notify"
	if err := sc.Bucket(bucket).Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { _ = sc.Bucket(bucket).Delete(context.Background()) })

	created, err := sc.Bucket(bucket).AddNotification(ctx, &storage.Notification{
		TopicProjectID: h.Project(),
		TopicID:        topicID,
		PayloadFormat:  storage.JSONPayload,
		EventTypes:     []string{storage.ObjectFinalizeEvent},
	})
	if err != nil {
		t.Fatalf("AddNotification: %v", err)
	}
	if created.ID == "" {
		t.Error("the created notification has no ID")
	}
	t.Logf("notification %s -> %s", created.ID, topicName)

	// Upload through the official SDK.
	w := sc.Bucket(bucket).Object("uploads/report.txt").NewWriter(ctx)
	if _, err := w.Write([]byte("hello notifications")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	msg := receiveOne(t, ps, sub, 60*time.Second)
	if msg == nil {
		t.Fatal("no notification arrived within 60s")
	}

	for _, want := range []struct{ key, value string }{
		{"eventType", "OBJECT_FINALIZE"},
		{"bucketId", bucket},
		{"objectId", "uploads/report.txt"},
		{"payloadFormat", "JSON_API_V1"},
	} {
		if got := msg.Attributes[want.key]; got != want.value {
			t.Errorf("attribute %s = %q, want %q (all: %v)", want.key, got, want.value, msg.Attributes)
		}
	}
	if msg.Attributes["objectGeneration"] == "" {
		t.Errorf("objectGeneration is missing: %v", msg.Attributes)
	}
	if !strings.Contains(msg.Attributes["notificationConfig"], created.ID) {
		t.Errorf("notificationConfig = %q, want it to name configuration %s",
			msg.Attributes["notificationConfig"], created.ID)
	}

	// The payload must be the Storage Object resource, not a shape of our own.
	var obj struct {
		Kind       string `json:"kind"`
		Name       string `json:"name"`
		Bucket     string `json:"bucket"`
		Size       string `json:"size"`
		Generation string `json:"generation"`
	}
	if err := json.Unmarshal(msg.Data, &obj); err != nil {
		t.Fatalf("payload is not JSON: %v\n%s", err, msg.Data)
	}
	if obj.Kind != "storage#object" {
		t.Errorf("payload kind = %q, want storage#object", obj.Kind)
	}
	if obj.Name != "uploads/report.txt" || obj.Bucket != bucket {
		t.Errorf("payload describes the wrong object: %+v", obj)
	}
	if obj.Generation == "" || obj.Size == "" {
		t.Errorf("payload is missing fields a client reads: %+v", obj)
	}
}

// TestNotificationFiltersAreHonoured proves a configuration's filters are
// applied, rather than recorded and ignored.
func TestNotificationFiltersAreHonoured(t *testing.T) {
	h := New(t)
	sc := storageClient(t, h)
	ps := pubsubClient(t, h)
	ctx := h.Context()

	topicID := "filtered-" + h.Project()
	topicName := topic(t, h, ps, topicID)
	sub := subscription(t, h, ps, topicID+"-sub", topicName)

	bucket := h.Project() + "-filtered"
	if err := sc.Bucket(bucket).Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { _ = sc.Bucket(bucket).Delete(context.Background()) })

	if _, err := sc.Bucket(bucket).AddNotification(ctx, &storage.Notification{
		TopicProjectID:   h.Project(),
		TopicID:          topicID,
		PayloadFormat:    storage.JSONPayload,
		EventTypes:       []string{storage.ObjectFinalizeEvent},
		ObjectNamePrefix: "watched/",
		CustomAttributes: map[string]string{"team": "platform"},
	}); err != nil {
		t.Fatalf("AddNotification: %v", err)
	}

	// Outside the prefix: must not be delivered.
	write(t, ctx, sc, bucket, "ignored/a.txt", "no")
	// Inside the prefix: must be delivered.
	write(t, ctx, sc, bucket, "watched/b.txt", "yes")

	msg := receiveOne(t, ps, sub, 60*time.Second)
	if msg == nil {
		t.Fatal("the matching upload produced no notification")
	}
	if msg.Attributes["objectId"] != "watched/b.txt" {
		t.Fatalf("the filtered-out object was delivered: %v", msg.Attributes)
	}
	if msg.Attributes["team"] != "platform" {
		t.Errorf("custom attributes were not applied: %v", msg.Attributes)
	}
}

// TestNotificationConfigsCRUDThroughTheSDK drives the management API the way
// a client does.
// covers: storage.notifications.insert, storage.notifications.list, storage.notifications.delete
func TestNotificationConfigsCRUDThroughTheSDK(t *testing.T) {
	h := New(t)
	sc := storageClient(t, h)
	ctx := h.Context()

	bucket := h.Project() + "-crud"
	if err := sc.Bucket(bucket).Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { _ = sc.Bucket(bucket).Delete(context.Background()) })

	created, err := sc.Bucket(bucket).AddNotification(ctx, &storage.Notification{
		TopicProjectID: h.Project(),
		TopicID:        "crud-topic",
		PayloadFormat:  storage.JSONPayload,
	})
	if err != nil {
		t.Fatalf("AddNotification: %v", err)
	}

	all, err := sc.Bucket(bucket).Notifications(ctx)
	if err != nil {
		t.Fatalf("Notifications: %v", err)
	}
	if _, ok := all[created.ID]; !ok {
		t.Errorf("the listing omitted %s: %v", created.ID, all)
	}

	if err := sc.Bucket(bucket).DeleteNotification(ctx, created.ID); err != nil {
		t.Fatalf("DeleteNotification: %v", err)
	}
	all, err = sc.Bucket(bucket).Notifications(ctx)
	if err != nil {
		t.Fatalf("Notifications after delete: %v", err)
	}
	if _, ok := all[created.ID]; ok {
		t.Errorf("%s survived deletion", created.ID)
	}
}

// An upload to a bucket with no configuration must not deliver anything —
// and must not break the upload either.
func TestUnconfiguredBucketStillUploads(t *testing.T) {
	h := New(t)
	sc := storageClient(t, h)
	ctx := h.Context()

	bucket := h.Project() + "-plain"
	if err := sc.Bucket(bucket).Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { _ = sc.Bucket(bucket).Delete(context.Background()) })

	write(t, ctx, sc, bucket, "a.txt", "content")

	r, err := sc.Bucket(bucket).Object("a.txt").NewReader(ctx)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	defer r.Close()
	buf := make([]byte, 64)
	n, _ := r.Read(buf)
	if string(buf[:n]) != "content" {
		t.Errorf("object content = %q", buf[:n])
	}
}

func write(t *testing.T, ctx context.Context, sc *storage.Client, bucket, name, body string) {
	t.Helper()
	w := sc.Bucket(bucket).Object(name).NewWriter(ctx)
	if _, err := w.Write([]byte(body)); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close %s: %v", name, err)
	}
}

// receiveOne pulls a single message, or returns nil when none arrives.
func receiveOne(t *testing.T, c *pubsub.Client, subscription string, within time.Duration) *pubsub.Message {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()

	var once sync.Once
	var got *pubsub.Message
	err := c.Subscriber(subscription).Receive(ctx, func(_ context.Context, m *pubsub.Message) {
		m.Ack()
		once.Do(func() {
			// The message is copied: the library reuses nothing, but a
			// pointer held past the callback is the kind of thing that works
			// until it does not.
			copied := *m
			got = &copied
			cancel()
		})
	})
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("receive from %s: %v", subscription, err)
	}
	return got
}
