package pubsubfront

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"github.com/linkedin/goavro/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

// fakeDestinations is a BigQuery table, a Cloud Storage bucket and a
// Cloud KMS key over HTTP.
type fakeDestinations struct {
	mu       sync.Mutex
	columns  []bqField
	rows     []map[string]any
	objects  map[string][]byte
	keyState string
}

func (d *fakeDestinations) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case r.URL.Path == "/bigquery/v2/projects/p/datasets/d/tables/t":
		_ = json.NewEncoder(w).Encode(map[string]any{"schema": map[string]any{"fields": d.columns}})
	case r.URL.Path == "/bigquery/v2/projects/p/datasets/d/tables/t/insertAll":
		var body struct {
			Rows []insertRow `json:"rows"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, row := range body.Rows {
			d.rows = append(d.rows, row.JSON)
		}
		_, _ = io.WriteString(w, `{}`)
	case r.URL.Path == "/storage/v1/b/bkt":
		_, _ = io.WriteString(w, `{"name":"bkt"}`)
	case r.URL.Path == "/upload/storage/v1/b/bkt/o":
		b, _ := io.ReadAll(r.Body)
		d.objects[r.URL.Query().Get("name")] = b
		_, _ = io.WriteString(w, `{}`)
	case strings.HasPrefix(r.URL.Path, "/v1/projects/p/locations/global/keyRings/r/cryptoKeys/k"):
		_ = json.NewEncoder(w).Encode(map[string]any{"purpose": "ENCRYPT_DECRYPT", "primary": map[string]any{"state": d.keyState}})
	default:
		http.NotFound(w, r)
	}
}

func exportFixture(t *testing.T) (*fixture, *fakeDestinations) {
	t.Helper()
	old, oldKey := exportPoll, keyCache
	exportPoll, keyCache = 20*time.Millisecond, 0
	t.Cleanup(func() { exportPoll, keyCache = old, oldKey })
	fx := newFixture(t)
	d := &fakeDestinations{objects: map[string][]byte{}, keyState: "ENABLED"}
	srv := httptest.NewServer(d)
	t.Cleanup(srv.Close)
	fx.front.UseExports(srv.URL, srv.URL, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); fx.front.RunExports(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return fx, d
}

func publish(t *testing.T, fx *fixture, topic string, msgs ...*pubsubpb.PubsubMessage) {
	t.Helper()
	if _, err := fx.client.TopicAdminClient.Publish(context.Background(), &pubsubpb.PublishRequest{Topic: topic, Messages: msgs}); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestBigQuerySubscriptionWritesDataAndMetadata(t *testing.T) {
	fx, d := exportFixture(t)
	d.columns = []bqField{{Name: "data", Type: "STRING"}, {Name: "subscription_name", Type: "STRING"},
		{Name: "message_id", Type: "STRING"}, {Name: "publish_time", Type: "TIMESTAMP"}, {Name: "attributes", Type: "JSON"}}
	topic := fx.topic(t, "bq")
	sub, err := fx.create(t, &pubsubpb.Subscription{Name: subName("bq"), Topic: topic,
		BigqueryConfig: &pubsubpb.BigQueryConfig{Table: "p.d.t", WriteMetadata: true}})
	if err != nil {
		t.Fatal(err)
	}
	if sub.GetBigqueryConfig().GetState() != pubsubpb.BigQueryConfig_ACTIVE {
		t.Errorf("state = %v, want ACTIVE", sub.GetBigqueryConfig().GetState())
	}
	publish(t, fx, topic, &pubsubpb.PubsubMessage{Data: []byte("hello"), Attributes: map[string]string{"k": "v"}})
	waitFor(t, "the row", func() bool { d.mu.Lock(); defer d.mu.Unlock(); return len(d.rows) == 1 })
	row := d.rows[0]
	if row["data"] != "hello" || row["subscription_name"] != subName("bq") || row["attributes"] != `{"k":"v"}` || row["message_id"] == "" {
		t.Errorf("row = %v", row)
	}
}

// A state load (StateLoadHeader) restores an export subscription whose
// bucket or table is gone, as GCP keeps one whose destination was deleted,
// and its state reads NOT_FOUND; a create without the header is refused,
// and the header excuses nothing else: Google's limits still apply.
func TestStateLoadRestoresAnExportWhoseDestinationIsGone(t *testing.T) {
	fx, _ := exportFixture(t)
	topic := fx.topic(t, "gone")
	load := metadata.AppendToOutgoingContext(context.Background(), StateLoadHeader, "1")
	gcs := &pubsubpb.Subscription{Name: subName("gcs-gone"), Topic: topic,
		CloudStorageConfig: &pubsubpb.CloudStorageConfig{Bucket: "none"}}
	if _, err := fx.create(t, proto.Clone(gcs).(*pubsubpb.Subscription)); status.Code(err) != codes.NotFound {
		t.Fatalf("a new subscription to a missing bucket: %v, want NotFound", err)
	}
	if _, err := fx.client.SubscriptionAdminClient.CreateSubscription(load, gcs); err != nil {
		t.Fatalf("a state load of a subscription whose bucket is gone: %v", err)
	}
	waitFor(t, "the restored subscription's NOT_FOUND state", func() bool {
		s, err := fx.client.SubscriptionAdminClient.GetSubscription(context.Background(), &pubsubpb.GetSubscriptionRequest{Subscription: gcs.GetName()})
		return err == nil && s.GetCloudStorageConfig().GetState() == pubsubpb.CloudStorageConfig_NOT_FOUND
	})
	bq := &pubsubpb.Subscription{Name: subName("bq-gone"), Topic: topic,
		BigqueryConfig: &pubsubpb.BigQueryConfig{Table: "p.d.missing"}}
	if _, err := fx.client.SubscriptionAdminClient.CreateSubscription(load, bq); err != nil {
		t.Errorf("a state load of a subscription whose table is gone: %v", err)
	}
	if _, err := fx.client.SubscriptionAdminClient.CreateSubscription(load, &pubsubpb.Subscription{Name: subName("gcs-short"), Topic: topic,
		CloudStorageConfig: &pubsubpb.CloudStorageConfig{Bucket: "none", MaxDuration: durationpb.New(30 * time.Second)}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a state load past max_duration's limit: %v, want InvalidArgument", err)
	}
}

func TestBigQuerySubscriptionChecksTheTable(t *testing.T) {
	fx, d := exportFixture(t)
	d.columns = []bqField{{Name: "other", Type: "STRING"}}
	topic := fx.topic(t, "bq")
	_, err := fx.create(t, &pubsubpb.Subscription{Name: subName("nodata"), Topic: topic,
		BigqueryConfig: &pubsubpb.BigQueryConfig{Table: "p.d.t"}})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("no data column: %v, want InvalidArgument", err)
	}
	_, err = fx.create(t, &pubsubpb.Subscription{Name: subName("missing"), Topic: topic,
		BigqueryConfig: &pubsubpb.BigQueryConfig{Table: "p.d.missing"}})
	if status.Code(err) != codes.NotFound {
		t.Errorf("missing table: %v, want NotFound", err)
	}
	_, err = fx.create(t, &pubsubpb.Subscription{Name: subName("both"), Topic: topic,
		BigqueryConfig: &pubsubpb.BigQueryConfig{Table: "p.d.t", UseTableSchema: true, UseTopicSchema: true}})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("both schemas: %v, want InvalidArgument", err)
	}
}

func TestBigQueryTableSchemaDropsOrDeadLetters(t *testing.T) {
	fx, d := exportFixture(t)
	d.columns = []bqField{{Name: "name", Type: "STRING"}, {Name: "n", Type: "INTEGER"}}
	topic := fx.topic(t, "bq")
	dead := fx.topic(t, "dead")
	if _, err := fx.create(t, &pubsubpb.Subscription{Name: subName("deadsub"), Topic: dead}); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.create(t, &pubsubpb.Subscription{Name: subName("strict"), Topic: topic,
		BigqueryConfig:   &pubsubpb.BigQueryConfig{Table: "p.d.t", UseTableSchema: true},
		DeadLetterPolicy: &pubsubpb.DeadLetterPolicy{DeadLetterTopic: dead, MaxDeliveryAttempts: 5}}); err != nil {
		t.Fatal(err)
	}
	publish(t, fx, topic, &pubsubpb.PubsubMessage{Data: []byte(`{"name":"a","n":1}`)},
		&pubsubpb.PubsubMessage{Data: []byte(`{"name":"b","extra":true}`)})
	waitFor(t, "the good row", func() bool { d.mu.Lock(); defer d.mu.Unlock(); return len(d.rows) == 1 })
	var got *pubsubpb.ReceivedMessage
	waitFor(t, "the dead letter", func() bool {
		r, err := fx.client.SubscriptionAdminClient.Pull(context.Background(), &pubsubpb.PullRequest{Subscription: subName("deadsub"), MaxMessages: 1})
		if err == nil && len(r.GetReceivedMessages()) > 0 {
			got = r.GetReceivedMessages()[0]
			return true
		}
		return false
	})
	if !strings.Contains(got.GetMessage().GetAttributes()[deadLetterErrorAttribute], "extra") {
		t.Errorf("dead letter attributes = %v", got.GetMessage().GetAttributes())
	}
}

func TestBigQueryTopicSchemaAvro(t *testing.T) {
	fx, d := exportFixture(t)
	d.columns = []bqField{{Name: "name", Type: "STRING"}, {Name: "when", Type: "TIMESTAMP"}, {Name: "color", Type: "INTEGER"}}
	revs := []*schemaRevisionForTest{{def: `{"type":"record","name":"R","fields":[{"name":"name","type":"string"},` +
		`{"name":"when","type":{"type":"long","logicalType":"timestamp-micros"}},` +
		`{"name":"color","type":{"type":"enum","name":"C","symbols":["RED","GREEN"]}}]}`}}
	root, err := parseAvroSchema(revs[0].def)
	if err != nil {
		t.Fatal(err)
	}
	codec, err := goavro.NewCodec(revs[0].def)
	if err != nil {
		t.Fatal(err)
	}
	s := &topicMsgSchema{enc: pubsubpb.Encoding_JSON, revisions: []schemaRevision{{root: root, codec: codec}}}
	cfg := &pubsubpb.BigQueryConfig{UseTopicSchema: true}
	if err := checkTable(cfg, d.columns, s); err != nil {
		t.Fatal(err)
	}
	row, err := s.decode([]byte(`{"name":"x","when":1700000000000000,"color":"GREEN"}`), d.columns)
	if err != nil {
		t.Fatal(err)
	}
	if row["name"] != "x" || row["color"] != 1 || row["when"] != "2023-11-14T22:13:20Z" {
		t.Errorf("row = %v", row)
	}
	if err := checkTable(cfg, []bqField{{Name: "name", Type: "INTEGER"}}, s); err == nil {
		t.Error("a string field in an INTEGER column was accepted")
	}
	if err := checkTable(&pubsubpb.BigQueryConfig{UseTopicSchema: true, DropUnknownFields: true}, []bqField{{Name: "name", Type: "STRING"}}, s); err != nil {
		t.Errorf("drop_unknown_fields: %v", err)
	}
	_ = fx
}

type schemaRevisionForTest struct{ def string }

func TestCloudStorageSubscriptionBatchesText(t *testing.T) {
	fx, d := exportFixture(t)
	topic := fx.topic(t, "gcs")
	sub, err := fx.create(t, &pubsubpb.Subscription{Name: subName("gcs"), Topic: topic,
		CloudStorageConfig: &pubsubpb.CloudStorageConfig{Bucket: "bkt", FilenamePrefix: "pre_", FilenameSuffix: "_suf",
			FilenameDatetimeFormat: "YYYY-MM-DD/hh_mm_ssZ",
			OutputFormat:           &pubsubpb.CloudStorageConfig_TextConfig_{TextConfig: &pubsubpb.CloudStorageConfig_TextConfig{}}}})
	if err != nil {
		t.Fatal(err)
	}
	if sub.GetCloudStorageConfig().GetMaxDuration().AsDuration() != 5*time.Minute {
		t.Errorf("max_duration = %v, want the default 5m", sub.GetCloudStorageConfig().GetMaxDuration())
	}
	publish(t, fx, topic, &pubsubpb.PubsubMessage{Data: []byte("one")}, &pubsubpb.PubsubMessage{Data: []byte("two")})
	time.Sleep(200 * time.Millisecond)
	d.mu.Lock()
	n := len(d.objects)
	d.mu.Unlock()
	if n != 0 {
		t.Fatalf("an object was written before max_duration")
	}
	fx.advance(t, 6*time.Minute)
	waitFor(t, "the object", func() bool { d.mu.Lock(); defer d.mu.Unlock(); return len(d.objects) == 1 })
	for name, body := range d.objects {
		if !strings.HasPrefix(name, "pre_") || !strings.HasSuffix(name, "_suf") || !strings.Contains(name, "Z_") {
			t.Errorf("object name %q", name)
		}
		if got := string(body); got != "one\ntwo" && got != "two\none" {
			t.Errorf("object = %q", body)
		}
	}
}

func TestCloudStorageSubscriptionAvroWithMetadata(t *testing.T) {
	fx, d := exportFixture(t)
	topic := fx.topic(t, "gcs")
	if _, err := fx.create(t, &pubsubpb.Subscription{Name: subName("avro"), Topic: topic,
		CloudStorageConfig: &pubsubpb.CloudStorageConfig{Bucket: "bkt", MaxDuration: durationpb.New(time.Minute),
			OutputFormat: &pubsubpb.CloudStorageConfig_AvroConfig_{AvroConfig: &pubsubpb.CloudStorageConfig_AvroConfig{WriteMetadata: true}}}}); err != nil {
		t.Fatal(err)
	}
	publish(t, fx, topic, &pubsubpb.PubsubMessage{Data: []byte("x"), Attributes: map[string]string{"a": "b"}})
	time.Sleep(200 * time.Millisecond)
	fx.advance(t, 2*time.Minute)
	waitFor(t, "the object", func() bool { d.mu.Lock(); defer d.mu.Unlock(); return len(d.objects) == 1 })
	for _, body := range d.objects {
		r, err := goavro.NewOCFReader(bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if !r.Scan() {
			t.Fatal("no record")
		}
		rec, err := r.Read()
		if err != nil {
			t.Fatal(err)
		}
		m := rec.(map[string]any)
		if string(m["data"].([]byte)) != "x" || m["subscription_name"] != subName("avro") || m["attributes"].(map[string]any)["a"] != "b" {
			t.Errorf("record = %v", m)
		}
	}
}

func TestCloudStorageSubscriptionLimits(t *testing.T) {
	fx, _ := exportFixture(t)
	topic := fx.topic(t, "gcs")
	for name, cfg := range map[string]*pubsubpb.CloudStorageConfig{
		"duration": {Bucket: "bkt", MaxDuration: durationpb.New(30 * time.Second)},
		"bytes":    {Bucket: "bkt", MaxBytes: 10},
		"messages": {Bucket: "bkt", MaxMessages: 10},
		"suffix":   {Bucket: "bkt", FilenameSuffix: "a/"},
		"format":   {Bucket: "bkt", FilenameDatetimeFormat: "YY-YYYY"},
	} {
		if _, err := fx.create(t, &pubsubpb.Subscription{Name: subName(name), Topic: topic, CloudStorageConfig: cfg}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v, want InvalidArgument", name, err)
		}
	}
	if _, err := fx.create(t, &pubsubpb.Subscription{Name: subName("nobucket"), Topic: topic,
		CloudStorageConfig: &pubsubpb.CloudStorageConfig{Bucket: "none"}}); status.Code(err) != codes.NotFound {
		t.Errorf("missing bucket: %v, want NotFound", err)
	}
}

func TestCMEKTopic(t *testing.T) {
	fx, d := exportFixture(t)
	key := "projects/p/locations/global/keyRings/r/cryptoKeys/k"
	name := "projects/" + project + "/topics/cmek"
	if _, err := fx.client.TopicAdminClient.CreateTopic(context.Background(), &pubsubpb.Topic{Name: name, KmsKeyName: "bad"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("bad key name: %v", err)
	}
	if _, err := fx.client.TopicAdminClient.CreateTopic(context.Background(),
		&pubsubpb.Topic{Name: name, KmsKeyName: "projects/p/locations/global/keyRings/r/cryptoKeys/missing"}); status.Code(err) != codes.NotFound {
		t.Errorf("missing key: %v", err)
	}
	d.mu.Lock()
	d.keyState = "DISABLED"
	d.mu.Unlock()
	if _, err := fx.client.TopicAdminClient.CreateTopic(context.Background(), &pubsubpb.Topic{Name: name, KmsKeyName: key}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("disabled key: %v", err)
	}
	d.mu.Lock()
	d.keyState = "ENABLED"
	d.mu.Unlock()
	if _, err := fx.client.TopicAdminClient.CreateTopic(context.Background(), &pubsubpb.Topic{Name: name, KmsKeyName: key}); err != nil {
		t.Fatal(err)
	}
}

func TestDatetimeFormat(t *testing.T) {
	for f, ok := range map[string]bool{"YYYY-MM-DDThh:mm:ss+00:00": true, "YYYY-MM-DD/hh_mm_ssZ": true, "YY-YYYY": false, "MMM": false,
		"YYYY-MM-DD": false, "YYYYMMDDhhmmss_": true} {
		if _, _, err := parseDatetimeFormat(f); (err == nil) != ok {
			t.Errorf("%q: %v", f, err)
		}
	}
	name := objectName(&pubsubpb.CloudStorageConfig{FilenamePrefix: "prod_", FilenameSuffix: "_archive"}, time.Date(2023, 9, 25, 4, 10, 0, 0, time.UTC))
	if !strings.HasPrefix(name, "prod_2023-09-25T04:10:00+00:00_") || !strings.HasSuffix(name, "_archive") {
		t.Errorf("name = %q", name)
	}
}
