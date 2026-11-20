//go:build compat

package compat

// Pub/Sub follow-ups of #1874 through the official clients: an UpdateTopic
// of message_storage_policy (#2000), which recovers an
// IN_TRANSIT_LOCATION_RESTRICTION export subscription, and a Cloud Storage
// subscription's PERMISSION_DENIED while the Cloud Storage API is disabled.

import (
	"slices"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// covers: google.pubsub.v1.Publisher/UpdateTopic, google.pubsub.v1.Publisher/GetTopic, google.pubsub.v1.Subscriber/GetSubscription
//
// TestPubSubUpdateTopicStoragePolicyRecoversInTransit (#2000): a topic
// whose message storage policy enforces in transit to us-east1 leaves a
// BigQuery subscription to a us-central1 dataset
// IN_TRANSIT_LOCATION_RESTRICTION; an UpdateTopic of
// message_storage_policy adding us-central1 reads back from Get and the
// update, and the subscription is ACTIVE again, its held message written;
// a zone or a multi-region is refused INVALID_ARGUMENT, and a policy
// without us-central1 restricts it again.
func TestPubSubUpdateTopicStoragePolicyRecoversInTransit(t *testing.T) {
	h := New(t)
	bq, bqProject := bigqueryClient(t, h)
	ps, _ := exportClients(t, h)
	ctx := h.Context()
	ds := bqDataset1874(t, h, bq, "update2000", "us-central1")
	tb := ds.Table("t")
	if err := tb.Create(ctx, &bigquery.TableMetadata{Schema: dataSchema1874}); err != nil {
		t.Fatal(err)
	}
	topic, _ := newTopicWithDeadLetter(t, h, ps, "bq-update2000",
		&pubsubpb.MessageStoragePolicy{AllowedPersistenceRegions: []string{"us-east1"}, EnforceInTransit: true})
	sub := "projects/" + h.Project() + "/subscriptions/bq-update2000"
	if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sub, Topic: topic,
		BigqueryConfig: &pubsubpb.BigQueryConfig{Table: bqProject + "." + ds.DatasetID + ".t"}}); err != nil {
		t.Fatal(err)
	}
	waitExportState(t, h, ps, sub, "IN_TRANSIT_LOCATION_RESTRICTION")
	publish1874(t, h, ps, topic, "held")

	mask := &fieldmaskpb.FieldMask{Paths: []string{"message_storage_policy"}}
	update := func(regions ...string) (*pubsubpb.Topic, error) {
		return ps.TopicAdminClient.UpdateTopic(ctx, &pubsubpb.UpdateTopicRequest{Topic: &pubsubpb.Topic{Name: topic,
			MessageStoragePolicy: &pubsubpb.MessageStoragePolicy{AllowedPersistenceRegions: regions, EnforceInTransit: true}}, UpdateMask: mask})
	}
	for _, bad := range []string{"us-central1-a", "us"} {
		if _, err := update(bad); status.Code(err) != codes.InvalidArgument {
			t.Errorf("UpdateTopic to %q: %v, want INVALID_ARGUMENT", bad, err)
		}
	}
	want := []string{"us-east1", "us-central1"}
	got, err := update(want...)
	if err != nil || !slices.Equal(got.GetMessageStoragePolicy().GetAllowedPersistenceRegions(), want) || !got.GetMessageStoragePolicy().GetEnforceInTransit() {
		t.Fatalf("UpdateTopic(message_storage_policy) = %v, %v; want %v enforced", got.GetMessageStoragePolicy(), err, want)
	}
	read, err := ps.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: topic})
	if err != nil || !slices.Equal(read.GetMessageStoragePolicy().GetAllowedPersistenceRegions(), want) {
		t.Errorf("GetTopic = %v, %v; want %v", read.GetMessageStoragePolicy(), err, want)
	}
	waitExportState(t, h, ps, sub, "ACTIVE")
	eventually1874(t, "the held message written once the region is allowed", 60*time.Second, func() bool {
		rows := bqRows1874(t, h, tb)
		return len(rows) == 1 && rows[0]["data"] == "held"
	})

	if _, err := update("us-east1"); err != nil {
		t.Fatal(err)
	}
	waitExportState(t, h, ps, sub, "IN_TRANSIT_LOCATION_RESTRICTION")
}

// covers: google.pubsub.v1.Subscriber/GetSubscription
//
// TestPubSubCloudStorageSubscriptionPermissionDenied (#1874): with the
// Cloud Storage API disabled in the subscription's project the
// subscription reads PERMISSION_DENIED, its message kept, and ACTIVE again
// once the API is enabled, the message written then.
func TestPubSubCloudStorageSubscriptionPermissionDenied(t *testing.T) {
	h := New(t)
	ps, sc := exportClients(t, h)
	sw := newAPISwitch(t, h, h.Project(), "storage.googleapis.com")
	ctx := h.Context()
	b := storageBucket1874(t, h, sc, "denied", "")
	topic, _ := newTopicWithDeadLetter(t, h, ps, "gcs-denied", nil)
	sub := "projects/" + h.Project() + "/subscriptions/gcs-denied"
	if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sub, Topic: topic,
		CloudStorageConfig: &pubsubpb.CloudStorageConfig{Bucket: b.BucketName(), MaxDuration: durationpb.New(time.Minute)}}); err != nil {
		t.Fatal(err)
	}
	waitExportState(t, h, ps, sub, "ACTIVE")
	sw.disable()
	waitExportState(t, h, ps, sub, "PERMISSION_DENIED")
	publish1874(t, h, ps, topic, "while-denied")
	advanceExportClock(t, h, 2*time.Minute)
	time.Sleep(4 * time.Second)
	if n := objectNames1874(h, b, ""); len(n) != 0 {
		t.Fatalf("objects written while the API is disabled: %v", n)
	}
	sw.enable()
	waitExportState(t, h, ps, sub, "ACTIVE")
	eventually1874(t, "the message written once the API is enabled", 60*time.Second, func() bool {
		advanceExportClock(t, h, 2*time.Minute)
		n := objectNames1874(h, b, "")
		return len(n) == 1 && string(readObject1874(t, h, b, n[0])) == "while-denied"
	})
}
