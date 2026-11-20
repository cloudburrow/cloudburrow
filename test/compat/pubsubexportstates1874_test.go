//go:build compat

package compat

// Pub/Sub export subscriptions, second part (#1874), through the official
// clients: each documented failure state of a BigQuery and a Cloud Storage
// subscription reached and recovered from, with messages kept in the
// backlog (and not dead-lettered) meanwhile; use_topic_schema with
// write_metadata and drop_unknown_fields on BigQuery and Avro on Cloud
// Storage; the batching limits and the exact object names; and the same
// subscriptions from gcloud and Terraform.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/storage"
	"github.com/linkedin/goavro/v2"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	vpubsub "cloud.google.com/go/pubsub/v2/apiv1"
)

// exportState reads a subscription's export state, as its name.
func exportState(t *testing.T, h *Harness, ps *pubsub.Client, sub string) string {
	t.Helper()
	s, err := ps.SubscriptionAdminClient.GetSubscription(h.Context(), &pubsubpb.GetSubscriptionRequest{Subscription: sub})
	if err != nil {
		return err.Error()
	}
	if s.GetBigqueryConfig() != nil {
		return s.GetBigqueryConfig().GetState().String()
	}
	return s.GetCloudStorageConfig().GetState().String()
}

func waitExportState(t *testing.T, h *Harness, ps *pubsub.Client, sub, want string) {
	t.Helper()
	last := ""
	for end := time.Now().Add(45 * time.Second); time.Now().Before(end); time.Sleep(250 * time.Millisecond) {
		if last = exportState(t, h, ps, sub); last == want {
			return
		}
	}
	t.Fatalf("%s: state %s, want %s", sub, last, want)
}

// noDeadLetters checks nothing reached a dead-letter subscription.
func noDeadLetters(t *testing.T, h *Harness, ps *pubsub.Client, sub, when string) {
	t.Helper()
	r, err := ps.SubscriptionAdminClient.Pull(h.Context(), &pubsubpb.PullRequest{Subscription: sub, MaxMessages: 10, ReturnImmediately: true}) //nolint:staticcheck
	if err != nil {
		t.Fatal(err)
	}
	if n := len(r.GetReceivedMessages()); n != 0 {
		t.Errorf("%s: %d messages dead-lettered; Google sends none while the subscription is in error", when, n)
	}
}

func newTopicWithDeadLetter(t *testing.T, h *Harness, ps *pubsub.Client, id string, policy *pubsubpb.MessageStoragePolicy) (topic, deadSub string) {
	t.Helper()
	ctx := h.Context()
	topic = "projects/" + h.Project() + "/topics/" + id
	if _, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic, MessageStoragePolicy: policy}); err != nil {
		t.Fatal(err)
	}
	dead := topic + "-dead"
	if _, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: dead}); err != nil {
		t.Fatal(err)
	}
	deadSub = "projects/" + h.Project() + "/subscriptions/" + id + "-dead"
	if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: deadSub, Topic: dead}); err != nil {
		t.Fatal(err)
	}
	return topic, deadSub
}

func publish1874(t *testing.T, h *Harness, ps *pubsub.Client, topic string, data ...string) {
	t.Helper()
	var ms []*pubsubpb.PubsubMessage
	for _, d := range data {
		ms = append(ms, &pubsubpb.PubsubMessage{Data: []byte(d), Attributes: map[string]string{"k": "v"}})
	}
	if _, err := ps.TopicAdminClient.Publish(h.Context(), &pubsubpb.PublishRequest{Topic: topic, Messages: ms}); err != nil {
		t.Fatal(err)
	}
}

func bqDataset1874(t *testing.T, h *Harness, bq *bigquery.Client, suffix, location string) *bigquery.Dataset {
	t.Helper()
	ds := bq.Dataset("ps_" + strings.ReplaceAll(h.Project(), "-", "_") + "_" + suffix)
	if err := ds.Create(h.Context(), &bigquery.DatasetMetadata{Location: location}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ds.DeleteWithContents(h.Context()) })
	return ds
}

func bqRows1874(t *testing.T, h *Harness, tb *bigquery.Table) []map[string]bigquery.Value {
	t.Helper()
	var out []map[string]bigquery.Value
	it := tb.Read(h.Context())
	for {
		var row map[string]bigquery.Value
		if err := it.Next(&row); err != nil {
			return out
		}
		out = append(out, row)
	}
}

var dataSchema1874 = bigquery.Schema{{Name: "data", Type: bigquery.StringFieldType}}

// TestPubSubBigQuerySubscriptionStatesRecover (#1874): a BigQuery
// subscription whose table is deleted reads NOT_FOUND, one whose table no
// longer fits reads SCHEMA_MISMATCH, and each returns to ACTIVE once the
// table is back, as Google's troubleshooting page says ("the subscription
// eventually returns to a healthy state"); meanwhile messages "remain in
// the subscription backlog" and are not dead-lettered, and are written
// once the table is back.
// covers: google.pubsub.v1.Subscriber/CreateSubscription, google.pubsub.v1.Subscriber/GetSubscription
func TestPubSubBigQuerySubscriptionStatesRecover(t *testing.T) {
	h := New(t)
	bq, bqProject := bigqueryClient(t, h)
	ps, _ := exportClients(t, h)
	ctx := h.Context()
	ds := bqDataset1874(t, h, bq, "states", "")
	tb := ds.Table("t")
	create := func(s bigquery.Schema) {
		t.Helper()
		if err := tb.Create(ctx, &bigquery.TableMetadata{Schema: s}); err != nil {
			t.Fatal(err)
		}
	}
	create(dataSchema1874)
	topic, deadSub := newTopicWithDeadLetter(t, h, ps, "bq-states", nil)
	sub := "projects/" + h.Project() + "/subscriptions/bq-states"
	if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sub, Topic: topic,
		BigqueryConfig:   &pubsubpb.BigQueryConfig{Table: bqProject + "." + ds.DatasetID + ".t"},
		DeadLetterPolicy: &pubsubpb.DeadLetterPolicy{DeadLetterTopic: topic + "-dead", MaxDeliveryAttempts: 5}}); err != nil {
		t.Fatal(err)
	}
	waitExportState(t, h, ps, sub, "ACTIVE")

	// NOT_FOUND, and back.
	if err := tb.Delete(ctx); err != nil {
		t.Fatal(err)
	}
	waitExportState(t, h, ps, sub, "NOT_FOUND")
	publish1874(t, h, ps, topic, "while-missing")
	time.Sleep(8 * time.Second)
	noDeadLetters(t, h, ps, deadSub, "table missing")
	create(dataSchema1874)
	waitExportState(t, h, ps, sub, "ACTIVE")
	eventually1874(t, "the backlog written once the table is back", 60*time.Second, func() bool {
		rows := bqRows1874(t, h, tb)
		return len(rows) == 1 && rows[0]["data"] == "while-missing"
	})

	// SCHEMA_MISMATCH: the table loses its data column; and back.
	if err := tb.Delete(ctx); err != nil {
		t.Fatal(err)
	}
	create(bigquery.Schema{{Name: "other", Type: bigquery.StringFieldType}})
	waitExportState(t, h, ps, sub, "SCHEMA_MISMATCH")
	publish1874(t, h, ps, topic, "while-mismatched")
	time.Sleep(8 * time.Second)
	noDeadLetters(t, h, ps, deadSub, "table mismatched")
	if err := tb.Delete(ctx); err != nil {
		t.Fatal(err)
	}
	create(dataSchema1874)
	waitExportState(t, h, ps, sub, "ACTIVE")
	eventually1874(t, "the backlog written once the table fits", 60*time.Second, func() bool {
		rows := bqRows1874(t, h, tb)
		return len(rows) == 1 && rows[0]["data"] == "while-mismatched"
	})
}

// TestPubSubBigQuerySubscriptionPermissionDenied (#1874): with the BigQuery
// API disabled in the subscription's project the subscription reads
// PERMISSION_DENIED (Google: "bigquery.googleapis.com API is not enabled
// for the project"), and ACTIVE again once it is enabled, the message
// written then.
// covers: google.pubsub.v1.Subscriber/GetSubscription
func TestPubSubBigQuerySubscriptionPermissionDenied(t *testing.T) {
	h := New(t)
	bq, bqProject := bigqueryClient(t, h)
	ps, _ := exportClients(t, h)
	sw := newAPISwitch(t, h, h.Project(), "bigquery.googleapis.com")
	ctx := h.Context()
	ds := bqDataset1874(t, h, bq, "denied", "")
	tb := ds.Table("t")
	if err := tb.Create(ctx, &bigquery.TableMetadata{Schema: dataSchema1874}); err != nil {
		t.Fatal(err)
	}
	topic, _ := newTopicWithDeadLetter(t, h, ps, "bq-denied", nil)
	sub := "projects/" + h.Project() + "/subscriptions/bq-denied"
	if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sub, Topic: topic,
		BigqueryConfig: &pubsubpb.BigQueryConfig{Table: bqProject + "." + ds.DatasetID + ".t"}}); err != nil {
		t.Fatal(err)
	}
	sw.disable()
	waitExportState(t, h, ps, sub, "PERMISSION_DENIED")
	publish1874(t, h, ps, topic, "while-denied")
	sw.enable()
	waitExportState(t, h, ps, sub, "ACTIVE")
	eventually1874(t, "the message written once the API is enabled", 60*time.Second, func() bool {
		rows := bqRows1874(t, h, tb)
		return len(rows) == 1 && rows[0]["data"] == "while-denied"
	})
}

// TestPubSubBigQuerySubscriptionInTransit (#1874): a topic whose message
// storage policy sets enforce_in_transit, with a dataset outside its
// allowed regions, reads IN_TRANSIT_LOCATION_RESTRICTION ("Cannot write to
// the destination because enforce_in_transit is set to true and the
// destination locations are not in the allowed regions"), and ACTIVE once
// the dataset is in an allowed region.
// covers: google.pubsub.v1.Subscriber/GetSubscription
func TestPubSubBigQuerySubscriptionInTransit(t *testing.T) {
	h := New(t)
	bq, bqProject := bigqueryClient(t, h)
	ps, _ := exportClients(t, h)
	ctx := h.Context()
	ds := bqDataset1874(t, h, bq, "transit", "us-central1")
	tb := ds.Table("t")
	if err := tb.Create(ctx, &bigquery.TableMetadata{Schema: dataSchema1874}); err != nil {
		t.Fatal(err)
	}
	topic, _ := newTopicWithDeadLetter(t, h, ps, "bq-transit",
		&pubsubpb.MessageStoragePolicy{AllowedPersistenceRegions: []string{"us-east1"}, EnforceInTransit: true})
	// The destination moves into the allowed region; the topic's policy
	// moving instead is TestPubSubUpdateTopicStoragePolicyRecoversInTransit
	// (#2000).
	sub := "projects/" + h.Project() + "/subscriptions/bq-transit"
	if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sub, Topic: topic,
		BigqueryConfig: &pubsubpb.BigQueryConfig{Table: bqProject + "." + ds.DatasetID + ".t"}}); err != nil {
		t.Fatal(err)
	}
	waitExportState(t, h, ps, sub, "IN_TRANSIT_LOCATION_RESTRICTION")
	publish1874(t, h, ps, topic, "held")
	// Moved into the allowed region: the dataset made again in us-east1.
	if err := ds.DeleteWithContents(ctx); err != nil {
		t.Fatal(err)
	}
	if err := ds.Create(ctx, &bigquery.DatasetMetadata{Location: "us-east1"}); err != nil {
		t.Fatal(err)
	}
	if err := tb.Create(ctx, &bigquery.TableMetadata{Schema: dataSchema1874}); err != nil {
		t.Fatal(err)
	}
	waitExportState(t, h, ps, sub, "ACTIVE")
	eventually1874(t, "the message written once the region is allowed", 60*time.Second, func() bool {
		return len(bqRows1874(t, h, tb)) == 1
	})
}

func schemaClient1874(t *testing.T, h *Harness) *vpubsub.SchemaClient {
	t.Helper()
	sc, err := vpubsub.NewSchemaClient(h.Context(), option.WithEndpoint(runShardEndpoint(h, envSinkPubSub, EnvPubSub)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	return sc
}

func avroTopic1874(t *testing.T, h *Harness, ps *pubsub.Client, id, definition string) string {
	t.Helper()
	ctx := h.Context()
	s, err := schemaClient1874(t, h).CreateSchema(ctx, &pubsubpb.CreateSchemaRequest{Parent: "projects/" + h.Project(), SchemaId: id,
		Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_AVRO, Definition: definition}})
	if err != nil {
		t.Fatal(err)
	}
	topic := "projects/" + h.Project() + "/topics/" + id
	if _, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic,
		SchemaSettings: &pubsubpb.SchemaSettings{Schema: s.GetName(), Encoding: pubsubpb.Encoding_JSON}}); err != nil {
		t.Fatal(err)
	}
	return topic
}

// TestPubSubBigQuerySubscriptionTopicSchemaMetadata (#1874): use_topic_schema
// with write_metadata and drop_unknown_fields writes the topic schema's
// fields to their columns, drops the one the table lacks, and writes
// subscription_name, message_id, publish_time and attributes (a JSON
// column); without drop_unknown_fields the same table is refused.
// covers: google.pubsub.v1.Subscriber/CreateSubscription, google.pubsub.v1.Publisher/Publish
func TestPubSubBigQuerySubscriptionTopicSchemaMetadata(t *testing.T) {
	h := New(t)
	bq, bqProject := bigqueryClient(t, h)
	ps, _ := exportClients(t, h)
	ctx := h.Context()
	ds := bqDataset1874(t, h, bq, "schema", "")
	tb := ds.Table("t")
	if err := tb.Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{
		{Name: "name", Type: bigquery.StringFieldType}, {Name: "n", Type: bigquery.IntegerFieldType},
		{Name: "subscription_name", Type: bigquery.StringFieldType}, {Name: "message_id", Type: bigquery.StringFieldType},
		{Name: "publish_time", Type: bigquery.TimestampFieldType}, {Name: "attributes", Type: bigquery.JSONFieldType}}}); err != nil {
		t.Fatal(err)
	}
	topic := avroTopic1874(t, h, ps, "bq-schema", `{"type":"record","name":"R","fields":[{"name":"name","type":"string"},`+
		`{"name":"n","type":"long"},{"name":"extra","type":"string"}]}`)
	table := bqProject + "." + ds.DatasetID + ".t"
	if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{
		Name: "projects/" + h.Project() + "/subscriptions/bq-schema-strict", Topic: topic,
		BigqueryConfig: &pubsubpb.BigQueryConfig{Table: table, UseTopicSchema: true, WriteMetadata: true}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a schema field the table lacks, without drop_unknown_fields: %v, want InvalidArgument", err)
	}
	sub := "projects/" + h.Project() + "/subscriptions/bq-schema"
	if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sub, Topic: topic,
		BigqueryConfig: &pubsubpb.BigQueryConfig{Table: table, UseTopicSchema: true, WriteMetadata: true, DropUnknownFields: true}}); err != nil {
		t.Fatal(err)
	}
	r, err := ps.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: topic, Messages: []*pubsubpb.PubsubMessage{
		{Data: []byte(`{"name":"a","n":7,"extra":"dropped"}`), Attributes: map[string]string{"k": "v"}}}})
	if err != nil {
		t.Fatal(err)
	}
	var row map[string]bigquery.Value
	eventually1874(t, "the row", 60*time.Second, func() bool {
		rows := bqRows1874(t, h, tb)
		if len(rows) == 1 {
			row = rows[0]
		}
		return len(rows) == 1
	})
	var attrs map[string]string
	_ = json.Unmarshal([]byte(fmt.Sprint(row["attributes"])), &attrs)
	if row["name"] != "a" || row["n"] != int64(7) || row["subscription_name"] != sub || row["message_id"] != r.GetMessageIds()[0] ||
		attrs["k"] != "v" || row["publish_time"] == nil {
		t.Errorf("row = %v", row)
	}
}

// storageBucket1874 creates a bucket, emptied and deleted at the end.
func storageBucket1874(t *testing.T, h *Harness, sc *storage.Client, suffix, location string) *storage.BucketHandle {
	t.Helper()
	ctx := h.Context()
	b := sc.Bucket(h.Project() + "-" + suffix)
	if err := b.Create(ctx, h.Project(), &storage.BucketAttrs{Location: location}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		it := b.Objects(ctx, nil)
		for {
			o, err := it.Next()
			if err != nil {
				break
			}
			_ = b.Object(o.Name).Delete(ctx)
		}
		_ = b.Delete(ctx)
	})
	return b
}

func objectNames1874(h *Harness, b *storage.BucketHandle, prefix string) []string {
	var out []string
	it := b.Objects(h.Context(), &storage.Query{Prefix: prefix})
	for {
		o, err := it.Next()
		if err == iterator.Done || err != nil {
			return out
		}
		out = append(out, o.Name)
	}
}

func readObject1874(t *testing.T, h *Harness, b *storage.BucketHandle, name string) []byte {
	t.Helper()
	r, err := b.Object(name).NewReader(h.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	body, _ := io.ReadAll(r)
	return body
}

// TestPubSubCloudStorageSubscriptionBatchLimits (#1874): max_messages and
// max_bytes each close a batch before max_duration, and an object's name
// is exactly <prefix><YYYY-MM-DDThh:mm:ss+00:00>_<uuid><suffix>.
// covers: google.pubsub.v1.Subscriber/CreateSubscription, google.pubsub.v1.Publisher/Publish
func TestPubSubCloudStorageSubscriptionBatchLimits(t *testing.T) {
	h := New(t)
	ps, sc := exportClients(t, h)
	ctx := h.Context()
	b := storageBucket1874(t, h, sc, "limits", "")
	countTopic, _ := newTopicWithDeadLetter(t, h, ps, "gcs-count", nil)
	bytesTopic, _ := newTopicWithDeadLetter(t, h, ps, "gcs-bytes", nil)
	for _, s := range []*pubsubpb.Subscription{
		{Name: "projects/" + h.Project() + "/subscriptions/gcs-count", Topic: countTopic,
			CloudStorageConfig: &pubsubpb.CloudStorageConfig{Bucket: b.BucketName(), FilenamePrefix: "count/", MaxMessages: 1000, MaxDuration: durationpb.New(10 * time.Minute)}},
		{Name: "projects/" + h.Project() + "/subscriptions/gcs-bytes", Topic: bytesTopic,
			CloudStorageConfig: &pubsubpb.CloudStorageConfig{Bucket: b.BucketName(), FilenamePrefix: "p_", FilenameSuffix: "_s", MaxBytes: 1000, MaxDuration: durationpb.New(10 * time.Minute)}},
	} {
		if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	msgs := make([]string, 1000)
	for i := range msgs {
		msgs[i] = fmt.Sprintf("m%04d", i)
	}
	publish1874(t, h, ps, countTopic, msgs...)
	big := strings.Repeat("x", 600)
	publish1874(t, h, ps, bytesTopic, big, big)
	eventually1874(t, "the max_messages object", 60*time.Second, func() bool { return len(objectNames1874(h, b, "count/")) == 1 })
	if lines := strings.Split(string(readObject1874(t, h, b, objectNames1874(h, b, "count/")[0])), "\n"); len(lines) != 1000 {
		t.Errorf("the max_messages object holds %d messages, want 1000", len(lines))
	}
	eventually1874(t, "the max_bytes object", 60*time.Second, func() bool { return len(objectNames1874(h, b, "p_")) == 1 })
	name := objectNames1874(h, b, "p_")[0]
	re := regexp.MustCompile(`^p_\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\+00:00_[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}_s$`)
	if !re.MatchString(name) {
		t.Errorf("object name %q, want p_<YYYY-MM-DDThh:mm:ss+00:00>_<uuid>_s", name)
	}
	if got := string(readObject1874(t, h, b, name)); got != big+"\n"+big {
		t.Errorf("the max_bytes object holds %d bytes, want both messages", len(got))
	}
}

// TestPubSubCloudStorageSubscriptionStatesRecover (#1874): a deleted bucket
// reads NOT_FOUND ("Bucket not found ... Recreate the bucket"), and a
// bucket outside an enforce_in_transit topic's regions
// IN_TRANSIT_LOCATION_RESTRICTION; each returns to ACTIVE when fixed, and
// the messages held meanwhile, never dead-lettered, are written then.
// covers: google.pubsub.v1.Subscriber/GetSubscription
func TestPubSubCloudStorageSubscriptionStatesRecover(t *testing.T) {
	h := New(t)
	ps, sc := exportClients(t, h)
	ctx := h.Context()
	b := storageBucket1874(t, h, sc, "gone", "")
	topic, deadSub := newTopicWithDeadLetter(t, h, ps, "gcs-gone", nil)
	sub := "projects/" + h.Project() + "/subscriptions/gcs-gone"
	if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sub, Topic: topic,
		CloudStorageConfig: &pubsubpb.CloudStorageConfig{Bucket: b.BucketName(), MaxDuration: durationpb.New(time.Minute)},
		DeadLetterPolicy:   &pubsubpb.DeadLetterPolicy{DeadLetterTopic: topic + "-dead", MaxDeliveryAttempts: 5}}); err != nil {
		t.Fatal(err)
	}
	waitExportState(t, h, ps, sub, "ACTIVE")
	if err := b.Delete(ctx); err != nil {
		t.Fatal(err)
	}
	waitExportState(t, h, ps, sub, "NOT_FOUND")
	publish1874(t, h, ps, topic, "held")
	advanceExportClock(t, h, 2*time.Minute)
	time.Sleep(8 * time.Second)
	noDeadLetters(t, h, ps, deadSub, "bucket missing")
	if err := b.Create(ctx, h.Project(), nil); err != nil {
		t.Fatal(err)
	}
	waitExportState(t, h, ps, sub, "ACTIVE")
	eventually1874(t, "the held message written", 60*time.Second, func() bool {
		advanceExportClock(t, h, 2*time.Minute)
		n := objectNames1874(h, b, "")
		return len(n) == 1 && string(readObject1874(t, h, b, n[0])) == "held"
	})

	east := storageBucket1874(t, h, sc, "east", "US-EAST1")
	transit, _ := newTopicWithDeadLetter(t, h, ps, "gcs-transit",
		&pubsubpb.MessageStoragePolicy{AllowedPersistenceRegions: []string{"us-central1"}, EnforceInTransit: true})
	tsub := "projects/" + h.Project() + "/subscriptions/gcs-transit"
	if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: tsub, Topic: transit,
		CloudStorageConfig: &pubsubpb.CloudStorageConfig{Bucket: east.BucketName(), MaxDuration: durationpb.New(time.Minute)}}); err != nil {
		t.Fatal(err)
	}
	waitExportState(t, h, ps, tsub, "IN_TRANSIT_LOCATION_RESTRICTION")
	publish1874(t, h, ps, transit, "east")
	// Moved into the topic's one allowed region: the bucket made again in
	// us-central1.
	if err := east.Delete(ctx); err != nil {
		t.Fatal(err)
	}
	if err := east.Create(ctx, h.Project(), &storage.BucketAttrs{Location: "US-CENTRAL1"}); err != nil {
		t.Fatal(err)
	}
	waitExportState(t, h, ps, tsub, "ACTIVE")
	eventually1874(t, "the message written once the region is allowed", 60*time.Second, func() bool {
		advanceExportClock(t, h, 2*time.Minute)
		return len(objectNames1874(h, east, "")) == 1
	})
}

// TestPubSubCloudStorageSubscriptionAvroTopicSchema (#1874): Avro output with
// use_topic_schema and write_metadata writes the topic's Avro record with
// subscription_name, message_id, publish_time and attributes as top-level
// fields; a topic without a schema, or one whose record has a field named
// as a metadata field, is refused.
// covers: google.pubsub.v1.Subscriber/CreateSubscription, google.pubsub.v1.Publisher/Publish
func TestPubSubCloudStorageSubscriptionAvroTopicSchema(t *testing.T) {
	h := New(t)
	ps, sc := exportClients(t, h)
	ctx := h.Context()
	b := storageBucket1874(t, h, sc, "avro-schema", "")
	avro := func(topic string, meta bool) *pubsubpb.CloudStorageConfig {
		return &pubsubpb.CloudStorageConfig{Bucket: b.BucketName(), MaxDuration: durationpb.New(time.Minute),
			OutputFormat: &pubsubpb.CloudStorageConfig_AvroConfig_{AvroConfig: &pubsubpb.CloudStorageConfig_AvroConfig{UseTopicSchema: true, WriteMetadata: meta}}}
	}
	plain, _ := newTopicWithDeadLetter(t, h, ps, "gcs-noschema", nil)
	if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{
		Name: "projects/" + h.Project() + "/subscriptions/gcs-noschema", Topic: plain, CloudStorageConfig: avro(plain, false)}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("use_topic_schema on a topic without a schema: %v, want InvalidArgument", err)
	}
	clash := avroTopic1874(t, h, ps, "gcs-clash", `{"type":"record","name":"C","fields":[{"name":"message_id","type":"string"}]}`)
	if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{
		Name: "projects/" + h.Project() + "/subscriptions/gcs-clash", Topic: clash, CloudStorageConfig: avro(clash, true)}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("write_metadata with a schema field named message_id: %v, want InvalidArgument", err)
	}
	topic := avroTopic1874(t, h, ps, "gcs-schema", `{"type":"record","name":"R","namespace":"ex","fields":[{"name":"name","type":"string"},{"name":"n","type":"long"}]}`)
	sub := "projects/" + h.Project() + "/subscriptions/gcs-schema"
	if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sub, Topic: topic, CloudStorageConfig: avro(topic, true)}); err != nil {
		t.Fatal(err)
	}
	r, err := ps.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: topic, Messages: []*pubsubpb.PubsubMessage{
		{Data: []byte(`{"name":"a","n":7}`), Attributes: map[string]string{"k": "v"}}}})
	if err != nil {
		t.Fatal(err)
	}
	eventually1874(t, "the Avro object", 60*time.Second, func() bool {
		advanceExportClock(t, h, 2*time.Minute)
		return len(objectNames1874(h, b, "")) == 1
	})
	ocf, err := goavro.NewOCFReader(bytes.NewReader(readObject1874(t, h, b, objectNames1874(h, b, "")[0])))
	if err != nil {
		t.Fatal(err)
	}
	if s := ocf.Codec().Schema(); !strings.Contains(s, `"ex.R"`) && !strings.Contains(s, `"name":"R"`) {
		t.Errorf("the object's schema %s, want the topic's record ex.R", s)
	}
	n := 0
	for ocf.Scan() {
		rec, err := ocf.Read()
		if err != nil {
			t.Fatal(err)
		}
		m := rec.(map[string]any)
		attrs, _ := m["attributes"].(map[string]any)
		if m["name"] != "a" || m["n"] != int64(7) || m["subscription_name"] != sub || m["message_id"] != r.GetMessageIds()[0] ||
			attrs["k"] != "v" || m["publish_time"] == nil {
			t.Errorf("record = %v", m)
		}
		n++
	}
	if n != 1 {
		t.Errorf("%d records, want 1", n)
	}
}

// TestGcloudPubSubExportSubscriptions (#1874): `gcloud pubsub subscriptions
// create` with --bigquery-table and --write-metadata, and with
// --cloud-storage-bucket and its file name, batching and Avro flags; each
// reads back ACTIVE, and a message published with gcloud reaches the table.
// covers: google.pubsub.v1.Subscriber/CreateSubscription, google.pubsub.v1.Subscriber/GetSubscription
func TestGcloudPubSubExportSubscriptions(t *testing.T) {
	h := New(t)
	bq, bqProject := bigqueryClient(t, h)
	shardEndpoint(h, envSinkPubSub, EnvPubSub)
	shardEndpoint(h, envSinkStorage, EnvStorage)
	ps, sc := exportClients(t, h)
	ctx := h.Context()
	ds := bqDataset1874(t, h, bq, "gcloud", "")
	tb := ds.Table("t")
	if err := tb.Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{
		{Name: "data", Type: bigquery.StringFieldType}, {Name: "subscription_name", Type: bigquery.StringFieldType},
		{Name: "message_id", Type: bigquery.StringFieldType}, {Name: "publish_time", Type: bigquery.TimestampFieldType},
		{Name: "attributes", Type: bigquery.StringFieldType}}}); err != nil {
		t.Fatal(err)
	}
	b := storageBucket1874(t, h, sc, "gcloud", "")
	g := newGcloudSession(t, h)
	p := "--project=" + h.Project()
	g.must("pubsub", "topics", "create", "gcloud-exports", p)
	g.must("pubsub", "subscriptions", "create", "gcloud-bq", p, "--topic=gcloud-exports",
		"--bigquery-table="+bqProject+"."+ds.DatasetID+".t", "--write-metadata")
	g.must("pubsub", "subscriptions", "create", "gcloud-gcs", p, "--topic=gcloud-exports",
		"--cloud-storage-bucket="+b.BucketName(), "--cloud-storage-file-prefix=g/", "--cloud-storage-file-suffix=.avro",
		"--cloud-storage-file-datetime-format=YYYY-MM-DD/hh_mm_ssZ", "--cloud-storage-max-bytes=2MB",
		"--cloud-storage-max-duration=2m", "--cloud-storage-max-messages=1000",
		"--cloud-storage-output-format=avro", "--cloud-storage-write-metadata")
	if got := strings.TrimSpace(g.must("pubsub", "subscriptions", "describe", "gcloud-bq", p, "--format=value(bigqueryConfig.state,bigqueryConfig.writeMetadata)")); got != "ACTIVE\tTrue" {
		t.Errorf("gcloud describes the BigQuery subscription as %q, want ACTIVE and writeMetadata", got)
	}
	s, err := ps.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: "projects/" + h.Project() + "/subscriptions/gcloud-gcs"})
	if err != nil {
		t.Fatal(err)
	}
	cs := s.GetCloudStorageConfig()
	if cs.GetState() != pubsubpb.CloudStorageConfig_ACTIVE || cs.GetFilenamePrefix() != "g/" || cs.GetFilenameSuffix() != ".avro" ||
		cs.GetFilenameDatetimeFormat() != "YYYY-MM-DD/hh_mm_ssZ" || cs.GetMaxBytes() != 2<<20 || cs.GetMaxDuration().AsDuration() != 2*time.Minute ||
		cs.GetMaxMessages() != 1000 || !cs.GetAvroConfig().GetWriteMetadata() {
		t.Errorf("the Cloud Storage subscription gcloud created reads %v", cs)
	}
	g.must("pubsub", "topics", "publish", "gcloud-exports", p, "--message=from-gcloud", "--attribute=k=v")
	eventually1874(t, "the row", 60*time.Second, func() bool {
		rows := bqRows1874(t, h, tb)
		return len(rows) == 1 && rows[0]["data"] == "from-gcloud" && rows[0]["attributes"] == `{"k":"v"}`
	})
	eventually1874(t, "the object", 60*time.Second, func() bool {
		advanceExportClock(t, h, 3*time.Minute)
		n := objectNames1874(h, b, "g/")
		return len(n) == 1 && regexp.MustCompile(`^g/\d{4}-\d{2}-\d{2}/\d{2}_\d{2}_\d{2}Z_[0-9a-f-]{36}\.avro$`).MatchString(n[0])
	})
}

// TestTerraformPubSubExportSubscriptions (#1874): google_pubsub_subscription
// with bigquery_config (use_table_schema, drop_unknown_fields) and with
// cloud_storage_config (file names, batching, avro_config) applies, plans
// clean, delivers, and destroys.
// covers: google.pubsub.v1.Subscriber/CreateSubscription, google.pubsub.v1.Subscriber/GetSubscription, google.pubsub.v1.Subscriber/DeleteSubscription
func TestTerraformPubSubExportSubscriptions(t *testing.T) {
	h := New(t)
	bq, bqProject := bigqueryClient(t, h)
	shardEndpoint(h, envSinkPubSub, EnvPubSub)
	shardEndpoint(h, envSinkStorage, EnvStorage)
	ps, sc := exportClients(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	ds := bqDataset1874(t, h, bq, "tf", "")
	tb := ds.Table("t")
	if err := tb.Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{{Name: "name", Type: bigquery.StringFieldType}}}); err != nil {
		t.Fatal(err)
	}
	b := storageBucket1874(t, h, sc, "tf", "")
	m.write(fmt.Sprintf(`
resource "google_pubsub_topic" "t" {
  project = %[1]q
  name    = "tf-exports"
}
resource "google_pubsub_subscription" "bq" {
  project = %[1]q
  name    = "tf-exports-bq"
  topic   = google_pubsub_topic.t.id
  bigquery_config {
    table               = %[2]q
    use_table_schema    = true
    drop_unknown_fields = true
  }
}
resource "google_pubsub_subscription" "gcs" {
  project = %[1]q
  name    = "tf-exports-gcs"
  topic   = google_pubsub_topic.t.id
  cloud_storage_config {
    bucket                   = %[3]q
    filename_prefix          = "tf/"
    filename_suffix          = ".avro"
    filename_datetime_format = "YYYY-MM-DD/hh_mm_ssZ"
    max_bytes                = 1000000
    max_duration             = "120s"
    max_messages             = 1000
    avro_config {
      write_metadata = true
    }
  }
}
`, h.Project(), bqProject+"."+ds.DatasetID+".t", b.BucketName()))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	m.planClean()
	publish1874(t, h, ps, "projects/"+h.Project()+"/topics/tf-exports", `{"name":"tf","unknown":1}`)
	eventually1874(t, "the row", 60*time.Second, func() bool {
		rows := bqRows1874(t, h, tb)
		return len(rows) == 1 && rows[0]["name"] == "tf"
	})
	eventually1874(t, "the object", 60*time.Second, func() bool {
		advanceExportClock(t, h, 3*time.Minute)
		return len(objectNames1874(h, b, "tf/")) == 1
	})
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := ps.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: "projects/" + h.Project() + "/subscriptions/tf-exports-bq"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetSubscription after destroy: %v; want NotFound", err)
	}
}
