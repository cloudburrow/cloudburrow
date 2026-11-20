//go:build compat

package compat

// Pub/Sub export subscriptions and CMEK topics (#1874), through the official
// clients: a BigQuery subscription's rows are read back with the BigQuery
// client and a Cloud Storage subscription's objects with the Storage client.
// They run in the emulators shard, beside BigQuery, Cloud Storage and
// Cloud KMS, under the SINK_ names, as the Logging sink tests do.

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"cloud.google.com/go/pubsub/v2"
	vpubsub "cloud.google.com/go/pubsub/v2/apiv1"
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
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/pubsubfront"
)

// exportClients are the Pub/Sub and Storage clients of the export tests.
func exportClients(t *testing.T, h *Harness) (*pubsub.Client, *storage.Client) {
	t.Helper()
	t.Setenv("PUBSUB_EMULATOR_HOST", runShardEndpoint(h, envSinkPubSub, EnvPubSub))
	ps, err := pubsub.NewClient(h.Context(), h.Project())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	se := runShardEndpoint(h, envSinkStorage, EnvStorage)
	t.Setenv("STORAGE_EMULATOR_HOST", se)
	sc, err := storage.NewClient(h.Context(), option.WithoutAuthentication(), option.WithEndpoint(se+"/storage/v1/"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	return ps, sc
}

func advanceExportClock(t *testing.T, h *Harness, d time.Duration) {
	t.Helper()
	conn, err := grpc.NewClient(runShardEndpoint(h, envSinkPubSub, EnvPubSub), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var now timestamppb.Timestamp
	if err := conn.Invoke(h.Context(), pubsubfront.ClockMethod, durationpb.New(d), &now); err != nil {
		t.Fatalf("advance the Pub/Sub clock: %v", err)
	}
}

func eventually1874(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(250 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestPubSubBigQuerySubscriptionWritesRows(t *testing.T) {
	h := New(t)
	bq, bqProject := bigqueryClient(t, h)
	ps, _ := exportClients(t, h)
	ctx := h.Context()
	ds := bq.Dataset("ps_" + strings.ReplaceAll(h.Project(), "-", "_"))
	if err := ds.Create(ctx, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ds.DeleteWithContents(ctx) })
	plain := ds.Table("plain")
	if err := plain.Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{
		{Name: "data", Type: bigquery.StringFieldType},
		{Name: "subscription_name", Type: bigquery.StringFieldType},
		{Name: "message_id", Type: bigquery.StringFieldType},
		{Name: "publish_time", Type: bigquery.TimestampFieldType},
		{Name: "attributes", Type: bigquery.StringFieldType}}}); err != nil {
		t.Fatal(err)
	}
	typed := ds.Table("typed")
	if err := typed.Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{
		{Name: "name", Type: bigquery.StringFieldType}, {Name: "n", Type: bigquery.IntegerFieldType}}}); err != nil {
		t.Fatal(err)
	}
	tableID := func(tb *bigquery.Table) string { return bqProject + "." + tb.DatasetID + "." + tb.TableID }

	topic := "projects/" + h.Project() + "/topics/bq-topic"
	if _, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatal(err)
	}
	dead := "projects/" + h.Project() + "/topics/dead"
	if _, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: dead}); err != nil {
		t.Fatal(err)
	}
	deadSub := "projects/" + h.Project() + "/subscriptions/dead"
	if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: deadSub, Topic: dead}); err != nil {
		t.Fatal(err)
	}
	sub := "projects/" + h.Project() + "/subscriptions/bq-plain"
	s, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sub, Topic: topic,
		BigqueryConfig: &pubsubpb.BigQueryConfig{Table: tableID(plain), WriteMetadata: true}})
	if err != nil {
		t.Fatal(err)
	}
	if s.GetBigqueryConfig().GetState() != pubsubpb.BigQueryConfig_ACTIVE {
		t.Errorf("state = %v, want ACTIVE", s.GetBigqueryConfig().GetState())
	}
	strict := "projects/" + h.Project() + "/subscriptions/bq-table"
	if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: strict, Topic: topic,
		BigqueryConfig:   &pubsubpb.BigQueryConfig{Table: tableID(typed), UseTableSchema: true},
		DeadLetterPolicy: &pubsubpb.DeadLetterPolicy{DeadLetterTopic: dead, MaxDeliveryAttempts: 5}}); err != nil {
		t.Fatal(err)
	}
	// Refused as Google does: no data column, a missing table.
	if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{
		Name: "projects/" + h.Project() + "/subscriptions/bq-nodata", Topic: topic,
		BigqueryConfig: &pubsubpb.BigQueryConfig{Table: tableID(typed)}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a table without a data column: %v, want InvalidArgument", err)
	}
	if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{
		Name: "projects/" + h.Project() + "/subscriptions/bq-missing", Topic: topic,
		BigqueryConfig: &pubsubpb.BigQueryConfig{Table: bqProject + "." + ds.DatasetID + ".missing"}}); status.Code(err) != codes.NotFound {
		t.Errorf("a missing table: %v, want NotFound", err)
	}

	for _, d := range []string{`{"name":"a","n":1}`, `{"name":"b","extra":true}`} {
		if _, err := ps.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: topic,
			Messages: []*pubsubpb.PubsubMessage{{Data: []byte(d), Attributes: map[string]string{"k": "v"}}}}); err != nil {
			t.Fatal(err)
		}
	}
	count := func(tb *bigquery.Table) int {
		it := tb.Read(ctx)
		n := 0
		for {
			var row []bigquery.Value
			if err := it.Next(&row); err != nil {
				return n
			}
			n++
		}
	}
	eventually1874(t, "both messages in the data table", 60*time.Second, func() bool { return count(plain) == 2 })
	eventually1874(t, "the JSON message in the typed table", 60*time.Second, func() bool { return count(typed) == 1 })
	it := plain.Read(ctx)
	var row map[string]bigquery.Value
	if err := it.Next(&row); err != nil {
		t.Fatal(err)
	}
	if row["subscription_name"] != sub || row["attributes"] != `{"k":"v"}` || row["message_id"] == "" {
		t.Errorf("row = %v", row)
	}
	// The message with a field the table lacks goes to the dead-letter
	// topic, saying why.
	eventually1874(t, "the dead letter", 90*time.Second, func() bool {
		r, err := ps.SubscriptionAdminClient.Pull(ctx, &pubsubpb.PullRequest{Subscription: deadSub, MaxMessages: 1})
		if err != nil || len(r.GetReceivedMessages()) == 0 {
			return false
		}
		m := r.GetReceivedMessages()[0].GetMessage()
		if !strings.Contains(m.GetAttributes()["CloudPubSubDeadLetterSourceDeliveryErrorMessage"], "extra") {
			t.Errorf("dead letter attributes = %v", m.GetAttributes())
		}
		return true
	})
	// A deleted table: NOT_FOUND.
	if err := typed.Delete(ctx); err != nil {
		t.Fatal(err)
	}
	eventually1874(t, "the NOT_FOUND state", 30*time.Second, func() bool {
		s, err := ps.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: strict})
		return err == nil && s.GetBigqueryConfig().GetState() == pubsubpb.BigQueryConfig_NOT_FOUND
	})
}

func TestPubSubCloudStorageSubscriptionWritesObjects(t *testing.T) {
	h := New(t)
	ps, sc := exportClients(t, h)
	ctx := h.Context()
	bucket := sc.Bucket(h.Project() + "-exports")
	if err := bucket.Create(ctx, h.Project(), nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		it := bucket.Objects(ctx, nil)
		for {
			o, err := it.Next()
			if err != nil {
				break
			}
			_ = bucket.Object(o.Name).Delete(ctx)
		}
		_ = bucket.Delete(ctx)
	})
	topic := "projects/" + h.Project() + "/topics/gcs"
	if _, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatal(err)
	}
	text := "projects/" + h.Project() + "/subscriptions/gcs-text"
	s, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: text, Topic: topic,
		CloudStorageConfig: &pubsubpb.CloudStorageConfig{Bucket: bucket.BucketName(), FilenamePrefix: "text/", FilenameSuffix: ".txt",
			FilenameDatetimeFormat: "YYYY-MM-DD/hh_mm_ssZ",
			OutputFormat:           &pubsubpb.CloudStorageConfig_TextConfig_{TextConfig: &pubsubpb.CloudStorageConfig_TextConfig{}}}})
	if err != nil {
		t.Fatal(err)
	}
	if s.GetCloudStorageConfig().GetState() != pubsubpb.CloudStorageConfig_ACTIVE ||
		s.GetCloudStorageConfig().GetMaxDuration().AsDuration() != 5*time.Minute {
		t.Errorf("config = %v, want ACTIVE and the 5-minute default", s.GetCloudStorageConfig())
	}
	avro := "projects/" + h.Project() + "/subscriptions/gcs-avro"
	if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: avro, Topic: topic,
		CloudStorageConfig: &pubsubpb.CloudStorageConfig{Bucket: bucket.BucketName(), FilenamePrefix: "avro/",
			MaxDuration:  durationpb.New(time.Minute),
			OutputFormat: &pubsubpb.CloudStorageConfig_AvroConfig_{AvroConfig: &pubsubpb.CloudStorageConfig_AvroConfig{WriteMetadata: true}}}}); err != nil {
		t.Fatal(err)
	}
	for name, cfg := range map[string]*pubsubpb.CloudStorageConfig{
		"short": {Bucket: bucket.BucketName(), MaxDuration: durationpb.New(10 * time.Second)},
		"few":   {Bucket: bucket.BucketName(), MaxMessages: 5},
		"slash": {Bucket: bucket.BucketName(), FilenameSuffix: "x/"},
	} {
		if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{
			Name: "projects/" + h.Project() + "/subscriptions/gcs-" + name, Topic: topic, CloudStorageConfig: cfg}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v, want InvalidArgument", name, err)
		}
	}
	for _, d := range []string{"one", "two"} {
		if _, err := ps.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: topic,
			Messages: []*pubsubpb.PubsubMessage{{Data: []byte(d), Attributes: map[string]string{"k": d}}}}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(3 * time.Second)
	objects := func(prefix string) []string {
		var out []string
		it := bucket.Objects(ctx, &storage.Query{Prefix: prefix})
		for {
			o, err := it.Next()
			if errors.Is(err, iterator.Done) || err != nil {
				return out
			}
			out = append(out, o.Name)
		}
	}
	if n := len(objects("")); n != 0 {
		t.Fatalf("%d objects before max_duration", n)
	}
	advanceExportClock(t, h, 6*time.Minute)
	eventually1874(t, "the text object", 30*time.Second, func() bool { return len(objects("text/")) == 1 })
	eventually1874(t, "the Avro object", 30*time.Second, func() bool { return len(objects("avro/")) == 1 })
	name := objects("text/")[0]
	if !strings.HasSuffix(name, ".txt") || !strings.Contains(name, "Z_") {
		t.Errorf("object name %q, want text/YYYY-MM-DD/hh_mm_ssZ_<uuid>.txt", name)
	}
	read := func(n string) []byte {
		r, err := bucket.Object(n).NewReader(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		b, _ := io.ReadAll(r)
		return b
	}
	if got := string(read(name)); got != "one\ntwo" && got != "two\none" {
		t.Errorf("text object = %q, want the messages separated by a newline", got)
	}
	ocf, err := goavro.NewOCFReader(bytes.NewReader(read(objects("avro/")[0])))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for ocf.Scan() {
		rec, err := ocf.Read()
		if err != nil {
			t.Fatal(err)
		}
		m := rec.(map[string]any)
		if m["subscription_name"] != avro || m["message_id"] == "" {
			t.Errorf("Avro record = %v", m)
		}
		n++
	}
	if n != 2 {
		t.Errorf("%d Avro records, want 2", n)
	}
}

func TestPubSubCMEKTopic(t *testing.T) {
	h := New(t)
	shardEndpoint(h, envCMEKKMS, EnvKMS)
	ps, _ := exportClients(t, h)
	k := kmsClients(t, h)["grpc"]
	ctx := h.Context()
	ring, err := k.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + h.Project() + "/locations/global", KeyRingId: "pubsub"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := k.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "topics",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	topic := "projects/" + h.Project() + "/topics/cmek"
	if _, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic + "-missing",
		KmsKeyName: ring.GetName() + "/cryptoKeys/missing"}); status.Code(err) != codes.NotFound {
		t.Errorf("a missing key: %v, want NotFound", err)
	}
	if _, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic + "-bad", KmsKeyName: "not-a-key"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a malformed key name: %v, want InvalidArgument", err)
	}
	got, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic, KmsKeyName: key.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetKmsKeyName() != key.GetName() {
		t.Errorf("kms_key_name = %q, want %q", got.GetKmsKeyName(), key.GetName())
	}
	if _, err := ps.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: topic,
		Messages: []*pubsubpb.PubsubMessage{{Data: []byte("x")}}}); err != nil {
		t.Fatalf("publish with the key enabled: %v", err)
	}
	// Disabled: publishing fails FAILED_PRECONDITION, as Google documents.
	if _, err := k.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{
		CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: key.GetPrimary().GetName(), State: kmspb.CryptoKeyVersion_DISABLED},
		UpdateMask:       &fieldmaskpb.FieldMask{Paths: []string{"state"}}}); err != nil {
		t.Fatal(err)
	}
	eventually1874(t, "publishing to be refused", 15*time.Second, func() bool {
		_, err := ps.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: topic,
			Messages: []*pubsubpb.PubsubMessage{{Data: []byte("x")}}})
		return status.Code(err) == codes.FailedPrecondition
	})
	if _, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic + "-disabled", KmsKeyName: key.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("a topic with a disabled key: %v, want FailedPrecondition", err)
	}
	// Enabled again: publishing works again, on the same topic.
	if _, err := k.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{
		CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: key.GetPrimary().GetName(), State: kmspb.CryptoKeyVersion_ENABLED},
		UpdateMask:       &fieldmaskpb.FieldMask{Paths: []string{"state"}}}); err != nil {
		t.Fatal(err)
	}
	eventually1874(t, "publishing to work again", 15*time.Second, func() bool {
		_, err := ps.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: topic,
			Messages: []*pubsubpb.PubsubMessage{{Data: []byte("x")}}})
		return err == nil
	})
	sub := "projects/" + h.Project() + "/subscriptions/cmek"
	if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sub, Topic: topic}); err != nil {
		t.Fatal(err)
	}
	if _, err := ps.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: topic,
		Messages: []*pubsubpb.PubsubMessage{{Data: []byte("after")}}}); err != nil {
		t.Fatal(err)
	}
	r, err := ps.SubscriptionAdminClient.Pull(ctx, &pubsubpb.PullRequest{Subscription: sub, MaxMessages: 1})
	if err != nil || len(r.GetReceivedMessages()) != 1 || string(r.GetReceivedMessages()[0].GetMessage().GetData()) != "after" {
		t.Errorf("pull after the key is enabled again: %v, %v", r, err)
	}
}

func TestPubSubAvroSchemaRevisionTime(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	sc, err := vpubsub.NewSchemaClient(ctx, option.WithEndpoint(runShardEndpoint(h, envSinkPubSub, EnvPubSub)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	before := time.Now().Add(-time.Hour)
	s, err := sc.CreateSchema(ctx, &pubsubpb.CreateSchemaRequest{Parent: "projects/" + h.Project(), SchemaId: "rev",
		Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_AVRO, Definition: `{"type":"record","name":"R","fields":[{"name":"a","type":"string"}]}`}})
	if err != nil {
		t.Fatal(err)
	}
	if s.GetRevisionCreateTime().AsTime().Before(before) {
		t.Errorf("revision_create_time = %v, want now", s.GetRevisionCreateTime().AsTime())
	}
	got, err := sc.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: s.GetName(), View: pubsubpb.SchemaView_FULL})
	if err != nil {
		t.Fatal(err)
	}
	if !got.GetRevisionCreateTime().AsTime().Equal(s.GetRevisionCreateTime().AsTime()) {
		t.Errorf("read back %v, created %v", got.GetRevisionCreateTime().AsTime(), s.GetRevisionCreateTime().AsTime())
	}
}
