//go:build compat

package compat

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/pubsub/v2"
	vpubsub "cloud.google.com/go/pubsub/v2/apiv1"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// BigQuery subscriptions writing change data capture (#1999), as
// https://docs.cloud.google.com/pubsub/docs/bigquery ("Change data
// capture") documents it, through the official Pub/Sub and BigQuery
// clients: messages with _CHANGE_TYPE and _CHANGE_SEQUENCE_NUMBER, with
// use_table_schema and with use_topic_schema, then the table queried.

func TestPubSubBigQuerySubscriptionChangeDataCapture(t *testing.T) {
	h := New(t)
	bq, bqProject := bigqueryClient(t, h)
	ctx := h.Context()
	// Pub/Sub alone: no Cloud Storage (exportClients asks for it).
	t.Setenv("PUBSUB_EMULATOR_HOST", runShardEndpoint(h, envSinkPubSub, EnvPubSub))
	ps, err := pubsub.NewClient(ctx, h.Project())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	ds := bq.Dataset("pscdc_" + strings.ReplaceAll(h.Project(), "-", "_"))
	if err := ds.Create(ctx, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ds.DeleteWithContents(ctx) })
	keyed := cdcTable(t, h, ds, "keyed", true)
	avroKeyed := cdcTable(t, h, ds, "avro_keyed", true)
	nokey := cdcTable(t, h, ds, "nokey", false)
	plain := ds.Table("plain")
	if err := plain.Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{{Name: "data", Type: bigquery.StringFieldType}}}); err != nil {
		t.Fatal(err)
	}
	tableID := func(tb *bigquery.Table) string { return bqProject + "." + tb.DatasetID + "." + tb.TableID }
	sql := func(tb *bigquery.Table) string { return "`" + tableID(tb) + "`" }
	topic := func(name string) string {
		n := "projects/" + h.Project() + "/topics/" + name
		if _, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: n}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	publish := func(topic string, data ...string) {
		t.Helper()
		var msgs []*pubsubpb.PubsubMessage
		for _, d := range data {
			msgs = append(msgs, &pubsubpb.PubsubMessage{Data: []byte(d)})
		}
		if _, err := ps.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: topic, Messages: msgs}); err != nil {
			t.Fatal(err)
		}
	}
	sub := func(name, topic string, cfg *pubsubpb.BigQueryConfig, dl *pubsubpb.DeadLetterPolicy) (*pubsubpb.Subscription, error) {
		return ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{
			Name: "projects/" + h.Project() + "/subscriptions/" + name, Topic: topic, BigqueryConfig: cfg, DeadLetterPolicy: dl})
	}
	settle := func(tb *bigquery.Table, want map[int64]string) {
		t.Helper()
		var got map[int64]string
		eventually1874(t, tb.TableID+" rows", 90*time.Second, func() bool {
			got = cdcTableRows(t, h, bq, sql(tb))
			return reflect.DeepEqual(got, want)
		})
		// And stays so: no message is written later.
		time.Sleep(3 * time.Second)
		if got = cdcTableRows(t, h, bq, sql(tb)); !reflect.DeepEqual(got, want) {
			t.Errorf("%s rows later = %v, want %v", tb.TableID, got, want)
		}
	}

	// use_table_schema: the changes of JSON messages, ordered by their
	// sequence numbers whatever order they are delivered in.
	tt := topic("cdc-json")
	if _, err := sub("cdc-json", tt, &pubsubpb.BigQueryConfig{Table: tableID(keyed), UseTableSchema: true}, nil); err != nil {
		t.Fatal(err)
	}
	publish(tt,
		`{"id":1,"name":"a","_CHANGE_TYPE":"UPSERT","_CHANGE_SEQUENCE_NUMBER":"1"}`,
		`{"id":2,"name":"b","_CHANGE_TYPE":"UPSERT","_CHANGE_SEQUENCE_NUMBER":"1"}`,
		`{"id":3,"name":"c","_CHANGE_TYPE":"UPSERT","_CHANGE_SEQUENCE_NUMBER":"1"}`)
	settle(keyed, map[int64]string{1: "a", 2: "b", 3: "c"})
	publish(tt,
		`{"id":1,"name":"a2","_CHANGE_TYPE":"UPSERT","_CHANGE_SEQUENCE_NUMBER":"2"}`,
		`{"id":2,"_CHANGE_TYPE":"DELETE","_CHANGE_SEQUENCE_NUMBER":"2"}`,
		`{"id":1,"name":"stale","_CHANGE_TYPE":"UPSERT","_CHANGE_SEQUENCE_NUMBER":"1/FF"}`,
		`{"id":2,"name":"back","_CHANGE_TYPE":"UPSERT","_CHANGE_SEQUENCE_NUMBER":"1"}`,
		`{"id":4,"name":"d","_CHANGE_TYPE":"UPSERT"}`)
	settle(keyed, map[int64]string{1: "a2", 3: "c", 4: "d"})

	// use_topic_schema: an Avro schema with the pseudo-columns.
	sc, err := vpubsub.NewSchemaClient(ctx, option.WithEndpoint(runShardEndpoint(h, envSinkPubSub, EnvPubSub)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	schema, err := sc.CreateSchema(ctx, &pubsubpb.CreateSchemaRequest{Parent: "projects/" + h.Project(), SchemaId: "cdc",
		Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_AVRO, Definition: `{"type":"record","name":"Change","fields":[` +
			`{"name":"id","type":"long"},{"name":"name","type":"string"},` +
			`{"name":"_CHANGE_TYPE","type":"string"},{"name":"_CHANGE_SEQUENCE_NUMBER","type":"string"}]}`}})
	if err != nil {
		t.Fatal(err)
	}
	at := "projects/" + h.Project() + "/topics/cdc-avro"
	if _, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: at,
		SchemaSettings: &pubsubpb.SchemaSettings{Schema: schema.GetName(), Encoding: pubsubpb.Encoding_JSON}}); err != nil {
		t.Fatal(err)
	}
	// "the BigQuery table must have a primary key": refused when the
	// topic's schema has _CHANGE_TYPE.
	if _, err := sub("cdc-avro-nokey", at, &pubsubpb.BigQueryConfig{Table: tableID(nokey), UseTopicSchema: true}, nil); status.Code(err) != codes.InvalidArgument ||
		!strings.Contains(err.Error(), "primary key") {
		t.Errorf("use_topic_schema with _CHANGE_TYPE to a table without a primary key: %v, want InvalidArgument", err)
	}
	if _, err := sub("cdc-avro", at, &pubsubpb.BigQueryConfig{Table: tableID(avroKeyed), UseTopicSchema: true}, nil); err != nil {
		t.Fatal(err)
	}
	publish(at,
		`{"id":10,"name":"ten","_CHANGE_TYPE":"UPSERT","_CHANGE_SEQUENCE_NUMBER":"A"}`,
		`{"id":11,"name":"eleven","_CHANGE_TYPE":"UPSERT","_CHANGE_SEQUENCE_NUMBER":"A"}`)
	settle(avroKeyed, map[int64]string{10: "ten", 11: "eleven"})
	publish(at,
		`{"id":10,"name":"TEN","_CHANGE_TYPE":"UPSERT","_CHANGE_SEQUENCE_NUMBER":"B"}`,
		`{"id":11,"name":"","_CHANGE_TYPE":"DELETE","_CHANGE_SEQUENCE_NUMBER":"B"}`,
		`{"id":10,"name":"old","_CHANGE_TYPE":"UPSERT","_CHANGE_SEQUENCE_NUMBER":"9"}`)
	settle(avroKeyed, map[int64]string{10: "TEN"})

	// Without use_table_schema or use_topic_schema there is no change data
	// capture: the message is the data column's value, as it is.
	pt := topic("cdc-plain")
	if _, err := sub("cdc-plain", pt, &pubsubpb.BigQueryConfig{Table: tableID(plain)}, nil); err != nil {
		t.Fatal(err)
	}
	raw := `{"id":1,"_CHANGE_TYPE":"DELETE"}`
	publish(pt, raw)
	eventually1874(t, "the plain row", 60*time.Second, func() bool {
		it, err := bq.Query("SELECT data FROM " + sql(plain)).Read(ctx)
		if err != nil {
			return false
		}
		var row []bigquery.Value
		return it.Next(&row) == nil && row[0] == raw
	})

	// use_table_schema, a table without a primary key: the change is not
	// written, and is dead-lettered saying why; as are a _CHANGE_TYPE that
	// is neither UPSERT nor DELETE and a malformed sequence number.
	dead := topic("cdc-dead")
	deadSub := "projects/" + h.Project() + "/subscriptions/cdc-dead"
	if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: deadSub, Topic: dead}); err != nil {
		t.Fatal(err)
	}
	nt := topic("cdc-nokey")
	if _, err := sub("cdc-nokey", nt, &pubsubpb.BigQueryConfig{Table: tableID(nokey), UseTableSchema: true},
		&pubsubpb.DeadLetterPolicy{DeadLetterTopic: dead, MaxDeliveryAttempts: 5}); err != nil {
		t.Fatal(err)
	}
	publish(nt, `{"id":1,"name":"a","_CHANGE_TYPE":"UPSERT"}`)
	kt := topic("cdc-bad")
	if _, err := sub("cdc-bad", kt, &pubsubpb.BigQueryConfig{Table: tableID(keyed), UseTableSchema: true},
		&pubsubpb.DeadLetterPolicy{DeadLetterTopic: dead, MaxDeliveryAttempts: 5}); err != nil {
		t.Fatal(err)
	}
	publish(kt, `{"id":50,"name":"x","_CHANGE_TYPE":"INSERT"}`, `{"id":51,"name":"y","_CHANGE_TYPE":"UPSERT","_CHANGE_SEQUENCE_NUMBER":"XYZ"}`)
	reasons := map[string]bool{}
	eventually1874(t, "three dead letters", 120*time.Second, func() bool {
		r, err := ps.SubscriptionAdminClient.Pull(ctx, &pubsubpb.PullRequest{Subscription: deadSub, MaxMessages: 10})
		if err != nil {
			return false
		}
		var ids []string
		for _, m := range r.GetReceivedMessages() {
			reasons[m.GetMessage().GetAttributes()["CloudPubSubDeadLetterSourceDeliveryErrorMessage"]] = true
			ids = append(ids, m.GetAckId())
		}
		if len(ids) > 0 {
			_ = ps.SubscriptionAdminClient.Acknowledge(ctx, &pubsubpb.AcknowledgeRequest{Subscription: deadSub, AckIds: ids})
		}
		return len(reasons) >= 3
	})
	for _, want := range []string{"primary key", "CHANGE_TYPE", "_CHANGE_SEQUENCE_NUMBER"} {
		found := false
		for r := range reasons {
			if strings.Contains(r, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("no dead letter says %q: %v", want, reasons)
		}
	}
	if got := cdcTableRows(t, h, bq, sql(nokey)); len(got) != 0 {
		t.Errorf("the table without a primary key has rows %v", got)
	}
	if got := cdcTableRows(t, h, bq, sql(keyed)); !reflect.DeepEqual(got, map[int64]string{1: "a2", 3: "c", 4: "d"}) {
		t.Errorf("the refused changes were written: %v", got)
	}
}
