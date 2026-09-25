//go:build compat

package compat

import (
	"context"
	"strconv"
	"testing"
	"time"

	pubsub "cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/storage"
)

// Notification events the builtin server emits itself (#506), to
// docs.cloud.google.com/storage/docs/pubsub-notifications. fake-gcs-server
// cannot archive (it has no versioning in persistent mode) and its DELETE
// and METADATA_UPDATE rows are Partial, so these run against the builtin
// server.

// receiveN pulls up to n messages within the time allowed.
func receiveN(t *testing.T, c *pubsub.Client, subscription string, n int, within time.Duration) []*pubsub.Message {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	var got []*pubsub.Message
	_ = c.Subscriber(subscription).Receive(ctx, func(_ context.Context, m *pubsub.Message) {
		m.Ack()
		copied := *m
		got = append(got, &copied)
		if len(got) >= n {
			cancel()
		}
	})
	return got
}

func notifiedBucket(t *testing.T, h *Harness, sc *storage.Client, ps *pubsub.Client, suffix string, attrs *storage.BucketAttrs, types ...string) (*storage.BucketHandle, string) {
	t.Helper()
	ctx := h.Context()
	topicID := suffix + "-" + h.Project()
	topicName := topic(t, h, ps, topicID)
	sub := subscription(t, h, ps, topicID+"-sub", topicName)
	bh := sc.Bucket(h.Project() + "-" + suffix)
	if err := bh.Create(ctx, h.Project(), attrs); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { emptyAndDelete(context.Background(), bh) })
	if _, err := bh.AddNotification(ctx, &storage.Notification{
		TopicProjectID: h.Project(), TopicID: topicID, PayloadFormat: storage.JSONPayload, EventTypes: types,
	}); err != nil {
		t.Fatal(err)
	}
	return bh, sub
}

// TestNotificationArchiveOnVersionedOverwrite: overwriting a live version
// on a versioned bucket sends OBJECT_ARCHIVE for the old one, with
// overwrittenByGeneration naming the new one.
// covers: storage.notifications.insert
func TestNotificationArchiveOnVersionedOverwrite(t *testing.T) {
	builtinOnly(t, "no object versioning in persistent mode, so no OBJECT_ARCHIVE, #374")
	h := New(t)
	sc, ps := storageClient(t, h), pubsubClient(t, h)
	ctx := h.Context()
	bh, sub := notifiedBucket(t, h, sc, ps, "archive", &storage.BucketAttrs{VersioningEnabled: true}, storage.ObjectArchiveEvent)
	first := putObject(t, ctx, bh.Object("doc.txt"), "one")
	second := putObject(t, ctx, bh.Object("doc.txt"), "two")
	msgs := receiveN(t, ps, sub, 1, 60*time.Second)
	if len(msgs) != 1 {
		t.Fatalf("got %d OBJECT_ARCHIVE messages, want 1", len(msgs))
	}
	a := msgs[0].Attributes
	if a["eventType"] != "OBJECT_ARCHIVE" || a["objectGeneration"] != strconv.FormatInt(first.Generation, 10) || a["overwrittenByGeneration"] != strconv.FormatInt(second.Generation, 10) {
		t.Errorf("archive attributes = %v; want the first generation, overwritten by the second", a)
	}
}

// TestNotificationDeleteAndMetadataUpdate: a metadata patch sends
// OBJECT_METADATA_UPDATE and a delete OBJECT_DELETE.
// covers: storage.notifications.insert
func TestNotificationDeleteAndMetadataUpdate(t *testing.T) {
	builtinOnly(t, "fake-gcs-server's DELETE and METADATA_UPDATE rows are Partial")
	h := New(t)
	sc, ps := storageClient(t, h), pubsubClient(t, h)
	ctx := h.Context()
	bh, sub := notifiedBucket(t, h, sc, ps, "delmeta", nil, storage.ObjectMetadataUpdateEvent, storage.ObjectDeleteEvent)
	o := bh.Object("doc.txt")
	putObject(t, ctx, o, "x")
	if _, err := o.Update(ctx, storage.ObjectAttrsToUpdate{Metadata: map[string]string{"k": "v"}}); err != nil {
		t.Fatal(err)
	}
	if err := o.Delete(ctx); err != nil {
		t.Fatal(err)
	}
	msgs := receiveN(t, ps, sub, 2, 60*time.Second)
	seen := map[string]bool{}
	for _, m := range msgs {
		seen[m.Attributes["eventType"]] = true
	}
	if !seen["OBJECT_METADATA_UPDATE"] || !seen["OBJECT_DELETE"] || len(msgs) != 2 {
		t.Errorf("events = %v; want one OBJECT_METADATA_UPDATE and one OBJECT_DELETE", seen)
	}
}
