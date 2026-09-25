//go:build compat

package compat

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/storage"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// These are the operations #12, #13 and #14 left unclaimed: implemented, but
// never driven by a client, and therefore never promotable off `Planned`.
// "Called is not the same as verified" cuts both ways — an operation nothing
// exercised is an operation nobody knows works.

// --- Cloud Storage (#12) ----------------------------------------------

// TestBucketUpdateChangesMetadata covers buckets.patch, which the Go client
// reaches through Bucket.Update, to the documented patch semantics: each
// patch changes what it names and keeps what it omits, on a fresh read, not
// just in the response. fake-gcs-server discards labels and resets an
// omitted defaultEventBasedHold (#321, #374), so its row stays Partial until
// the builtin server replaces it (#519); this runs against the builtin one.
// covers: storage.buckets.patch
func TestBucketUpdateChangesMetadata(t *testing.T) {
	builtinOnly(t, "labels are discarded and an omitted field is reset, #374")
	h := New(t)
	c := storageClient(t, h)
	ctx := h.Context()

	bucket := h.Project() + "-patch"
	if err := c.Bucket(bucket).Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { _ = c.Bucket(bucket).Delete(context.Background()) })

	if _, err := c.Bucket(bucket).Update(ctx, storage.BucketAttrsToUpdate{DefaultEventBasedHold: true}); err != nil {
		t.Fatalf("Bucket.Update(defaultEventBasedHold): %v", err)
	}
	var labels storage.BucketAttrsToUpdate
	labels.SetLabel("env", "dev")
	if _, err := c.Bucket(bucket).Update(ctx, labels); err != nil {
		t.Fatalf("Bucket.Update(labels): %v", err)
	}
	attrs, err := c.Bucket(bucket).Attrs(ctx)
	if err != nil {
		t.Fatalf("Attrs: %v", err)
	}
	if attrs.Labels["env"] != "dev" {
		t.Errorf("labels = %v on a fresh read; want env=dev kept", attrs.Labels)
	}
	if !attrs.DefaultEventBasedHold {
		t.Error("a patch that omitted defaultEventBasedHold reset it; a patch keeps what it omits")
	}
}

// TestObjectCopyAndRewrite covers objects.rewrite, which the Go client uses
// for both CopierFrom and large cross-bucket copies.
//
// It is not a `copy` alias: rewrite is token-driven and may need several
// calls, which is exactly why it needed its own test.
// covers: storage.objects.rewrite
func TestObjectCopyAndRewrite(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	ctx := h.Context()

	src := h.Project() + "-rw-src"
	dst := h.Project() + "-rw-dst"
	for _, b := range []string{src, dst} {
		if err := c.Bucket(b).Create(ctx, h.Project(), nil); err != nil {
			t.Fatalf("create bucket %s: %v", b, err)
		}
		name := b
		t.Cleanup(func() { _ = c.Bucket(name).Delete(context.Background()) })
	}

	const payload = "content that will be rewritten"
	w := c.Bucket(src).Object("original.txt").NewWriter(ctx)
	if _, err := w.Write([]byte(payload)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	t.Cleanup(func() { _ = c.Bucket(src).Object("original.txt").Delete(context.Background()) })

	// Same bucket.
	if _, err := c.Bucket(src).Object("copy.txt").
		CopierFrom(c.Bucket(src).Object("original.txt")).Run(ctx); err != nil {
		t.Fatalf("CopierFrom within a bucket: %v", err)
	}
	t.Cleanup(func() { _ = c.Bucket(src).Object("copy.txt").Delete(context.Background()) })

	// Across buckets, which is the path that actually uses rewrite.
	if _, err := c.Bucket(dst).Object("moved.txt").
		CopierFrom(c.Bucket(src).Object("original.txt")).Run(ctx); err != nil {
		t.Fatalf("CopierFrom across buckets: %v", err)
	}
	t.Cleanup(func() { _ = c.Bucket(dst).Object("moved.txt").Delete(context.Background()) })

	got := readObject(t, ctx, c, dst, "moved.txt")
	if got != payload {
		t.Errorf("rewritten object = %q, want %q", got, payload)
	}
}

// TestMultipartUploadStoresMetadataWithContent covers
// uploadType=multipart, which is the path the client takes when an object
// carries metadata alongside its bytes.
func TestMultipartUploadStoresMetadataWithContent(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	ctx := h.Context()

	bucket := h.Project() + "-multipart"
	if err := c.Bucket(bucket).Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { _ = c.Bucket(bucket).Delete(context.Background()) })

	obj := c.Bucket(bucket).Object("with-metadata.txt")
	w := obj.NewWriter(ctx)
	// Setting attributes on the writer is what makes the client send a
	// multipart body rather than a media upload.
	w.ContentType = "text/plain; charset=utf-8"
	w.Metadata = map[string]string{"origin": "compat-test", "purpose": "multipart"}
	w.CacheControl = "no-store"
	if _, err := w.Write([]byte("multipart body")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	t.Cleanup(func() { _ = obj.Delete(context.Background()) })

	attrs, err := obj.Attrs(ctx)
	if err != nil {
		t.Fatalf("Attrs: %v", err)
	}
	// The metadata must survive the upload, or the multipart body was parsed
	// as content and the object is wrong in a way a read would not reveal.
	if attrs.Metadata["origin"] != "compat-test" {
		t.Errorf("custom metadata = %v, want it preserved", attrs.Metadata)
	}
	if !strings.HasPrefix(attrs.ContentType, "text/plain") {
		t.Errorf("content type = %q", attrs.ContentType)
	}
	if got := readObject(t, ctx, c, bucket, "with-metadata.txt"); got != "multipart body" {
		t.Errorf("content = %q; the metadata part may have been stored as content", got)
	}
}

// TestObjectMetadataUpdate covers objects.patch through ObjectHandle.Update.
// covers: storage.objects.patch
func TestObjectMetadataUpdate(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	ctx := h.Context()

	bucket := h.Project() + "-objpatch"
	if err := c.Bucket(bucket).Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { _ = c.Bucket(bucket).Delete(context.Background()) })

	obj := c.Bucket(bucket).Object("patch-me.txt")
	w := obj.NewWriter(ctx)
	if _, err := w.Write([]byte("body")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = obj.Delete(context.Background()) })

	updated, err := obj.Update(ctx, storage.ObjectAttrsToUpdate{
		ContentType: "application/json",
		Metadata:    map[string]string{"stage": "patched"},
	})
	if err != nil {
		t.Fatalf("Object.Update: %v", err)
	}
	if updated.ContentType != "application/json" {
		t.Errorf("content type = %q", updated.ContentType)
	}

	attrs, err := obj.Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if attrs.Metadata["stage"] != "patched" {
		t.Errorf("the patch did not persist: %v", attrs.Metadata)
	}
	// The bytes must be untouched: a metadata patch that rewrote content
	// would corrupt an object silently.
	if got := readObject(t, ctx, c, bucket, "patch-me.txt"); got != "body" {
		t.Errorf("content changed during a metadata patch: %q", got)
	}
}

// TestResumableUploadCompletes covers the resumable path, which the client
// takes for anything above its chunk threshold.
func TestResumableUploadCompletes(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	ctx := h.Context()

	bucket := h.Project() + "-resumable"
	if err := c.Bucket(bucket).Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { _ = c.Bucket(bucket).Delete(context.Background()) })

	obj := c.Bucket(bucket).Object("large.bin")
	w := obj.NewWriter(ctx)
	// A small chunk size forces several chunks from a modest payload, so the
	// resumable protocol is genuinely exercised rather than completed in one
	// request.
	w.ChunkSize = 256 * 1024
	payload := bytes.Repeat([]byte("0123456789abcdef"), 64*1024) // 1 MiB
	if _, err := w.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	t.Cleanup(func() { _ = obj.Delete(context.Background()) })

	attrs, err := obj.Attrs(ctx)
	if err != nil {
		t.Fatalf("Attrs: %v", err)
	}
	if attrs.Size != int64(len(payload)) {
		t.Errorf("size = %d, want %d", attrs.Size, len(payload))
	}

	r, err := obj.NewReader(ctx)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("a multi-chunk upload round-tripped %d bytes, want %d", len(got), len(payload))
	}
}

// TestBucketIAMIsRefusedRatherThanStubbed proves the non-goal is honest: an
// unimplemented IAM call must fail, not return a plausible empty policy that
// a caller would read as "no bindings".
func TestBucketIAMIsRefusedRatherThanStubbed(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	ctx := h.Context()

	bucket := h.Project() + "-iam"
	if err := c.Bucket(bucket).Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { _ = c.Bucket(bucket).Delete(context.Background()) })

	policy, err := c.Bucket(bucket).IAM().Policy(ctx)
	if err == nil {
		// If it ever succeeds, an empty policy is the dangerous answer: it
		// reads as "no bindings" rather than "not implemented".
		t.Fatalf("IAM().Policy() succeeded and returned %v; an unimplemented "+
			"call must fail rather than return a plausible empty policy", policy)
	}
	t.Logf("bucket IAM is refused: %v", err)
}

func readObject(t *testing.T, ctx context.Context, c *storage.Client, bucket, name string) string {
	t.Helper()
	r, err := c.Bucket(bucket).Object(name).NewReader(ctx)
	if err != nil {
		t.Fatalf("NewReader %s/%s: %v", bucket, name, err)
	}
	defer r.Close()
	body, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read %s/%s: %v", bucket, name, err)
	}
	return string(body)
}

// --- Pub/Sub (#13, #14) -----------------------------------------------

// TestSubscriptionGetUpdateDelete covers the three subscription operations
// that were implemented and never driven by a client.
// covers: google.pubsub.v1.Subscriber/GetSubscription, google.pubsub.v1.Subscriber/UpdateSubscription, google.pubsub.v1.Subscriber/DeleteSubscription
func TestSubscriptionGetUpdateDelete(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	ctx := h.Context()

	topicName := topic(t, h, c, "sub-ops-topic-"+h.Project())
	subName := fmt.Sprintf("projects/%s/subscriptions/sub-ops-%s", h.Project(), h.Project())

	created, err := c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{
		Name: subName, Topic: topicName, AckDeadlineSeconds: 10,
	})
	if err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}
	t.Cleanup(func() {
		_ = c.SubscriptionAdminClient.DeleteSubscription(context.Background(),
			&pubsubpb.DeleteSubscriptionRequest{Subscription: subName})
	})
	if created.GetAckDeadlineSeconds() != 10 {
		t.Errorf("ack deadline = %d, want 10", created.GetAckDeadlineSeconds())
	}

	got, err := c.SubscriptionAdminClient.GetSubscription(ctx,
		&pubsubpb.GetSubscriptionRequest{Subscription: subName})
	if err != nil {
		t.Fatalf("GetSubscription: %v", err)
	}
	if got.GetTopic() != topicName {
		t.Errorf("topic = %q, want %q", got.GetTopic(), topicName)
	}

	updated, err := c.SubscriptionAdminClient.UpdateSubscription(ctx,
		&pubsubpb.UpdateSubscriptionRequest{
			Subscription: &pubsubpb.Subscription{Name: subName, AckDeadlineSeconds: 30},
			UpdateMask:   &fieldmaskpb.FieldMask{Paths: []string{"ack_deadline_seconds"}},
		})
	if err != nil {
		t.Fatalf("UpdateSubscription: %v", err)
	}
	if updated.GetAckDeadlineSeconds() != 30 {
		t.Errorf("updated ack deadline = %d, want 30", updated.GetAckDeadlineSeconds())
	}
	// The change must survive a fresh read, not only appear in the response.
	reread, err := c.SubscriptionAdminClient.GetSubscription(ctx,
		&pubsubpb.GetSubscriptionRequest{Subscription: subName})
	if err != nil {
		t.Fatal(err)
	}
	if reread.GetAckDeadlineSeconds() != 30 {
		t.Errorf("the update did not persist: %d", reread.GetAckDeadlineSeconds())
	}

	if err := c.SubscriptionAdminClient.DeleteSubscription(ctx,
		&pubsubpb.DeleteSubscriptionRequest{Subscription: subName}); err != nil {
		t.Fatalf("DeleteSubscription: %v", err)
	}
	_, err = c.SubscriptionAdminClient.GetSubscription(ctx,
		&pubsubpb.GetSubscriptionRequest{Subscription: subName})
	if status.Code(err) != codes.NotFound {
		t.Errorf("after deletion GetSubscription returned %v, want NotFound", err)
	}
}

// TestDeadLetterPolicyIsRecorded covers dead-letter configuration.
//
// Whether messages are actually routed to the dead-letter topic after
// max_delivery_attempts is a separate claim, and this test does not make it —
// it proves only that the configuration round-trips.
func TestDeadLetterPolicyIsRecorded(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	ctx := h.Context()

	main := topic(t, h, c, "dlq-main-"+h.Project())
	dead := topic(t, h, c, "dlq-dead-"+h.Project())
	subName := fmt.Sprintf("projects/%s/subscriptions/dlq-sub-%s", h.Project(), h.Project())

	created, err := c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{
		Name: subName, Topic: main, AckDeadlineSeconds: 10,
		DeadLetterPolicy: &pubsubpb.DeadLetterPolicy{
			DeadLetterTopic: dead, MaxDeliveryAttempts: 5,
		},
	})
	if err != nil {
		// A backend that does not support dead-letter policies must refuse
		// rather than accept and drop the setting, which would leave a
		// caller believing their messages are protected.
		t.Skipf("dead-letter policies are not accepted by this backend: %v", err)
	}
	t.Cleanup(func() {
		_ = c.SubscriptionAdminClient.DeleteSubscription(context.Background(),
			&pubsubpb.DeleteSubscriptionRequest{Subscription: subName})
	})

	if created.GetDeadLetterPolicy().GetDeadLetterTopic() != dead {
		t.Fatalf("dead-letter topic was dropped: %+v", created.GetDeadLetterPolicy())
	}
	got, err := c.SubscriptionAdminClient.GetSubscription(ctx,
		&pubsubpb.GetSubscriptionRequest{Subscription: subName})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetDeadLetterPolicy().GetMaxDeliveryAttempts() != 5 {
		t.Errorf("max delivery attempts did not persist: %+v", got.GetDeadLetterPolicy())
	}
	t.Logf("dead-letter policy recorded: topic=%s attempts=%d",
		got.GetDeadLetterPolicy().GetDeadLetterTopic(),
		got.GetDeadLetterPolicy().GetMaxDeliveryAttempts())
}

// TestRetryPolicyRoundTrips covers subscription retry configuration.
func TestRetryPolicyRoundTrips(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	ctx := h.Context()

	topicName := topic(t, h, c, "retry-topic-"+h.Project())
	subName := fmt.Sprintf("projects/%s/subscriptions/retry-sub-%s", h.Project(), h.Project())

	created, err := c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{
		Name: subName, Topic: topicName, AckDeadlineSeconds: 10,
		RetryPolicy: &pubsubpb.RetryPolicy{
			MinimumBackoff: durationpb.New(2_000_000_000),
			MaximumBackoff: durationpb.New(60_000_000_000),
		},
	})
	if err != nil {
		t.Skipf("retry policies are not accepted by this backend: %v", err)
	}
	t.Cleanup(func() {
		_ = c.SubscriptionAdminClient.DeleteSubscription(context.Background(),
			&pubsubpb.DeleteSubscriptionRequest{Subscription: subName})
	})
	if created.GetRetryPolicy().GetMinimumBackoff().AsDuration() == 0 {
		t.Errorf("retry policy was dropped: %+v", created.GetRetryPolicy())
	}
}

// TestSnapshotsAndSeekAreSupported.
//
// The matrix listed snapshots as "likely out of scope for the first release".
// A test written to confirm they were refused found that they work — which is
// the point of driving every operation rather than reasoning about it.
// Understating support is a smaller harm than overstating it, but it is still
// wrong: a developer avoids a feature they could have used.
// covers: google.pubsub.v1.Subscriber/CreateSnapshot, google.pubsub.v1.Subscriber/GetSnapshot, google.pubsub.v1.Subscriber/Seek, google.pubsub.v1.Subscriber/DeleteSnapshot
func TestSnapshotsAndSeekAreSupported(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	ctx := h.Context()

	topicName := topic(t, h, c, "snap-topic-"+h.Project())
	subName := fmt.Sprintf("projects/%s/subscriptions/snap-sub-%s", h.Project(), h.Project())
	if _, err := c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{
		Name: subName, Topic: topicName, AckDeadlineSeconds: 10,
	}); err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}
	t.Cleanup(func() {
		_ = c.SubscriptionAdminClient.DeleteSubscription(context.Background(),
			&pubsubpb.DeleteSubscriptionRequest{Subscription: subName})
	})

	snapName := fmt.Sprintf("projects/%s/snapshots/snap-%s", h.Project(), h.Project())
	snap, err := c.SubscriptionAdminClient.CreateSnapshot(ctx, &pubsubpb.CreateSnapshotRequest{
		Name: snapName, Subscription: subName,
	})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	t.Cleanup(func() {
		_ = c.SubscriptionAdminClient.DeleteSnapshot(context.Background(),
			&pubsubpb.DeleteSnapshotRequest{Snapshot: snapName})
	})
	if snap.GetName() != snapName {
		t.Errorf("snapshot name = %q, want %q", snap.GetName(), snapName)
	}
	if snap.GetTopic() != topicName {
		t.Errorf("snapshot topic = %q, want %q", snap.GetTopic(), topicName)
	}

	got, err := c.SubscriptionAdminClient.GetSnapshot(ctx,
		&pubsubpb.GetSnapshotRequest{Snapshot: snapName})
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	if got.GetName() != snapName {
		t.Errorf("GetSnapshot returned %q", got.GetName())
	}

	// Seeking to the snapshot must be accepted; whether unacked messages are
	// genuinely replayed is a stronger claim this test does not make.
	if _, err := c.SubscriptionAdminClient.Seek(ctx, &pubsubpb.SeekRequest{
		Subscription: subName,
		Target:       &pubsubpb.SeekRequest_Snapshot{Snapshot: snapName},
	}); err != nil {
		t.Fatalf("Seek to snapshot: %v", err)
	}
	t.Logf("snapshots and seek are supported: %s", snapName)

	if err := c.SubscriptionAdminClient.DeleteSnapshot(ctx,
		&pubsubpb.DeleteSnapshotRequest{Snapshot: snapName}); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}
	_, err = c.SubscriptionAdminClient.GetSnapshot(ctx,
		&pubsubpb.GetSnapshotRequest{Snapshot: snapName})
	if status.Code(err) != codes.NotFound {
		t.Errorf("after deletion GetSnapshot returned %v, want NotFound", err)
	}
}
