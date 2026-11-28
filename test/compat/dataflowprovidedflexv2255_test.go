//go:build compat

package compat

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/dataflow/apiv1beta3/dataflowpb"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/storage"
	"github.com/linkedin/goavro/v2"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// covers: google.dataflow.v1beta3.FlexTemplatesService/LaunchFlexTemplate
//
// TestDataflowProvidedFlexTemplatesV2 (#2255): more of the v2 module
// googlecloud-to-googlecloud's Flex Templates, each launched through
// LaunchFlexTemplate at Google's path and run to its output:
//   - Cloud_PubSub_to_Avro_Flex (templates/provided/pubsub-to-avro)
//     windows a subscription's messages into Avro files of
//     AvroPubsubMessageRecord, written as the job drains;
//   - PubSub_Avro_to_BigQuery (templates/provided/pubsub-avro-to-bigquery)
//     writes Avro-encoded messages, by the schema it is given, into a
//     BigQuery table it creates;
//   - PubSub_Proto_to_BigQuery_Flex (templates/provided/pubsub-proto-to-
//     bigquery) writes protobuf messages, by a descriptor set in Cloud
//     Storage, into a table whose schema it derives from the message;
//   - Stream_GCS_Text_to_BigQuery_Flex (templates/provided/gcs-text-to-
//     bigquery-stream) watches a file pattern and writes each new file's
//     lines, through a JavaScript UDF, into BigQuery;
//   - BigQuery_to_Bigtable (templates/provided/bigquery-to-bigtable)
//     writes a table's rows into Bigtable, keyed by a column.
func TestDataflowProvidedFlexTemplatesV2(t *testing.T) {
	h, _, c := dataflowEnv(t)
	h.Endpoint(EnvStorage)
	h.Endpoint(EnvBigQuery)
	h.Endpoint(EnvPubSub)
	bq, project := bigqueryClient(t, h)
	ps := pubsubClient(t, h)
	sc := storageClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Minute)
	defer cancel()
	const base = "gs://dataflow-templates-us-central1/latest/flex/"
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	bucket := fmt.Sprintf("%s-dataflow-provflexv-%s", project, suffix)
	dataflowBucket(t, ctx, sc, project, bucket)
	put := bucketWriter(t, ctx, sc, bucket)
	launch := func(t *testing.T, name, job string, p map[string]string) string {
		t.Helper()
		resp, err := c.flex.LaunchFlexTemplate(ctx, &dataflowpb.LaunchFlexTemplateRequest{ProjectId: project, Location: "us-central1",
			LaunchParameter: &dataflowpb.LaunchFlexTemplateParameter{JobName: job + "-" + suffix,
				Template:    &dataflowpb.LaunchFlexTemplateParameter_ContainerSpecGcsPath{ContainerSpecGcsPath: base + name},
				Parameters:  p,
				Environment: &dataflowpb.FlexTemplateRuntimeEnvironment{TempLocation: "gs://" + bucket + "/tmp"}}})
		if err != nil {
			t.Fatalf("LaunchFlexTemplate(%s): %v", name, err)
		}
		return resp.GetJob().GetId()
	}
	running := func(t *testing.T, name, id string) {
		t.Helper()
		j := waitDataflowJob(t, ctx, c, project, id, dataflowpb.JobState_JOB_STATE_RUNNING, dataflowpb.JobState_JOB_STATE_FAILED)
		if j.CurrentState != dataflowpb.JobState_JOB_STATE_RUNNING || j.Type != dataflowpb.JobType_JOB_TYPE_STREAMING {
			t.Fatalf("%s's job is %s %s:\n%s", name, j.CurrentState, j.Type, jobMessages(t, ctx, c, project, id))
		}
	}
	end := func(t *testing.T, id string, state dataflowpb.JobState) {
		t.Helper()
		if _, err := c.jobs.UpdateJob(ctx, &dataflowpb.UpdateJobRequest{ProjectId: project, Location: "us-central1", JobId: id,
			Job: &dataflowpb.Job{RequestedState: state}}); err != nil {
			t.Fatalf("UpdateJob(%s): %v", state, err)
		}
		if j := waitDataflowJob(t, ctx, c, project, id, state, dataflowpb.JobState_JOB_STATE_FAILED, dataflowpb.JobState_JOB_STATE_CANCELLED); j.CurrentState != state {
			t.Fatalf("the job ended %s:\n%s", j.CurrentState, jobMessages(t, ctx, c, project, id))
		}
	}
	topicSub := func(t *testing.T, name string) (string, string) {
		t.Helper()
		topic := fmt.Sprintf("projects/%s/topics/%s-%s", project, name, suffix)
		sub := fmt.Sprintf("projects/%s/subscriptions/%s-%s", project, name, suffix)
		if _, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
			t.Fatal(err)
		}
		if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sub, Topic: topic}); err != nil {
			t.Fatal(err)
		}
		return topic, sub
	}
	publish := func(t *testing.T, topic string, data ...[]byte) {
		t.Helper()
		var msgs []*pubsubpb.PubsubMessage
		for _, d := range data {
			msgs = append(msgs, &pubsubpb.PubsubMessage{Data: d})
		}
		if _, err := ps.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: topic, Messages: msgs}); err != nil {
			t.Fatal(err)
		}
	}
	// rowsWithin polls a query until it answers want, publishing (or
	// otherwise feeding) with feed between polls.
	rowsWithin := func(t *testing.T, id, sql, want string, feed func()) {
		t.Helper()
		got := ""
		for deadline := time.Now().Add(6 * time.Minute); ; {
			if feed != nil {
				feed()
			}
			time.Sleep(10 * time.Second)
			if got = providedRows(t, ctx, bq, sql); got == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("the table has %q; want %q:\n%s", got, want, jobMessages(t, ctx, c, project, id))
			}
		}
	}

	t.Run("Cloud_PubSub_to_Avro_Flex", func(t *testing.T) {
		topic, sub := topicSub(t, "provflexavro")
		id := launch(t, "Cloud_PubSub_to_Avro_Flex", "provided-ps-avro", map[string]string{"inputSubscription": sub,
			"outputDirectory": "gs://" + bucket + "/psavro/", "avroTempDirectory": "gs://" + bucket + "/psavrotmp/",
			"outputFilenamePrefix": "messages-", "windowDuration": "1m"})
		running(t, "Cloud_PubSub_to_Avro_Flex", id)
		for deadline := time.Now().Add(5 * time.Minute); ; {
			publish(t, topic, []byte("fig"))
			time.Sleep(10 * time.Second)
			if read(t, ctx, c, project, id, " Window") > 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the job read none of the messages:\n%s", jobMessages(t, ctx, c, project, id))
			}
		}
		end(t, id, dataflowpb.JobState_JOB_STATE_DRAINED)
		var got []string
		it := sc.Bucket(bucket).Objects(ctx, &storage.Query{Prefix: "psavro/messages-"})
		for {
			a, err := it.Next()
			if errors.Is(err, iterator.Done) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			b := objectBytes(t, ctx, sc, bucket, a.Name)
			ocf, err := goavro.NewOCFReader(bytes.NewReader(b))
			if err != nil {
				t.Fatalf("%s is not an Avro file: %v", a.Name, err)
			}
			for ocf.Scan() {
				rec, err := ocf.Read()
				if err != nil {
					t.Fatal(err)
				}
				m, _ := rec.(map[string]any)
				msg, _ := m["message"].([]byte)
				got = append(got, string(msg))
			}
		}
		if len(got) == 0 || got[0] != "fig" {
			t.Errorf("the Avro files hold messages %q; want fig", got)
		}
	})

	t.Run("PubSub_Avro_to_BigQuery", func(t *testing.T) {
		topic, sub := topicSub(t, "provflexavrobq")
		dead, _ := topicSub(t, "provflexavrobqdead")
		schema := `{"type": "record", "name": "Fruit", "fields": [{"name": "name", "type": "string"}, {"name": "n", "type": "long"}]}`
		put("psavrobq/fruit.avsc", schema)
		codec, err := goavro.NewCodec(schema)
		if err != nil {
			t.Fatal(err)
		}
		ds := "provided_flex_avro_" + suffix
		if err := bq.Dataset(ds).Create(ctx, nil); err != nil {
			t.Fatal(err)
		}
		id := launch(t, "PubSub_Avro_to_BigQuery", "provided-ps-avro-bq", map[string]string{"inputSubscription": sub,
			"schemaPath": "gs://" + bucket + "/psavrobq/fruit.avsc", "outputTableSpec": project + ":" + ds + ".fruit", "outputTopic": dead})
		running(t, "PubSub_Avro_to_BigQuery", id)
		kiwi, err := codec.BinaryFromNative(nil, map[string]any{"name": "kiwi", "n": int64(4)})
		if err != nil {
			t.Fatal(err)
		}
		published := false
		rowsWithin(t, id, "SELECT name, n FROM `"+project+"."+ds+".fruit` GROUP BY name, n", "kiwi=4", func() {
			if !published || read(t, ctx, c, project, id, "Read Avro records") == 0 {
				publish(t, topic, kiwi)
				published = true
			}
		})
		end(t, id, dataflowpb.JobState_JOB_STATE_CANCELLED)
	})

	t.Run("PubSub_Proto_to_BigQuery_Flex", func(t *testing.T) {
		topic, sub := topicSub(t, "provflexproto")
		dead, _ := topicSub(t, "provflexprotodead")
		fd := &descriptorpb.FileDescriptorProto{Name: proto.String("cbfruit.proto"), Package: proto.String("cbtest"), Syntax: proto.String("proto3"),
			MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Fruit"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("name"), Number: proto.Int32(1), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
					Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), JsonName: proto.String("name")},
				{Name: proto.String("n"), Number: proto.Int32(2), Type: descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
					Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), JsonName: proto.String("n")}}}}}
		set, err := proto.Marshal(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{fd}})
		if err != nil {
			t.Fatal(err)
		}
		put("psproto/fruit.pb", string(set))
		f, err := protodesc.NewFile(fd, nil)
		if err != nil {
			t.Fatal(err)
		}
		msg := dynamicpb.NewMessage(f.Messages().ByName("Fruit"))
		msg.Set(f.Messages().ByName("Fruit").Fields().ByName("name"), protoreflect.ValueOfString("plum"))
		msg.Set(f.Messages().ByName("Fruit").Fields().ByName("n"), protoreflect.ValueOfInt64(7))
		plum, err := proto.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		ds := "provided_flex_proto_" + suffix
		if err := bq.Dataset(ds).Create(ctx, nil); err != nil {
			t.Fatal(err)
		}
		id := launch(t, "PubSub_Proto_to_BigQuery_Flex", "provided-ps-proto-bq", map[string]string{"inputSubscription": sub,
			"protoSchemaPath": "gs://" + bucket + "/psproto/fruit.pb", "fullMessageName": "cbtest.Fruit", "preserveProtoFieldNames": "true",
			"outputTableSpec": project + ":" + ds + ".fruit", "outputTopic": dead})
		running(t, "PubSub_Proto_to_BigQuery_Flex", id)
		published := false
		rowsWithin(t, id, "SELECT name, n FROM `"+project+"."+ds+".fruit` GROUP BY name, n", "plum=7", func() {
			if !published || read(t, ctx, c, project, id, "Read From Pubsub") == 0 {
				publish(t, topic, plum)
				published = true
			}
		})
		end(t, id, dataflowpb.JobState_JOB_STATE_CANCELLED)
	})

	t.Run("Stream_GCS_Text_to_BigQuery_Flex", func(t *testing.T) {
		ds := "provided_flex_stream_" + suffix
		if err := bq.Dataset(ds).Create(ctx, nil); err != nil {
			t.Fatal(err)
		}
		// This template's schema file is a TableSchema, a top-level
		// "fields" array (its requirements), not the batch templates'
		// "BigQuery Schema".
		put("stream/schema.json", `{"fields": [{"name": "name", "type": "STRING"}, {"name": "age", "type": "INTEGER"}]}`)
		put("stream/udf.js", "function transform(line) {\n  var v = line.split(',');\n  return JSON.stringify({name: v[0].toUpperCase(), age: parseInt(v[1])});\n}\n")
		id := launch(t, "Stream_GCS_Text_to_BigQuery_Flex", "provided-stream-text-bq", map[string]string{
			"inputFilePattern": "gs://" + bucket + "/stream/in/*.csv", "JSONPath": "gs://" + bucket + "/stream/schema.json",
			"outputTable": project + ":" + ds + ".people", "bigQueryLoadingTemporaryDirectory": "gs://" + bucket + "/streambqtmp",
			"javascriptTextTransformGcsPath": "gs://" + bucket + "/stream/udf.js", "javascriptTextTransformFunctionName": "transform"})
		running(t, "Stream_GCS_Text_to_BigQuery_Flex", id)
		// A file that appears while the job runs is read.
		put("stream/in/people.csv", "Ada,36\nGrace,45\n")
		rowsWithin(t, id, "SELECT name, age FROM `"+project+"."+ds+".people` GROUP BY name, age ORDER BY name", "ADA=36,GRACE=45", nil)
		end(t, id, dataflowpb.JobState_JOB_STATE_CANCELLED)
	})

	t.Run("BigQuery_to_Bigtable", func(t *testing.T) {
		h := h.For(t)
		ds := "provided_flex_bt_" + suffix
		if err := bq.Dataset(ds).Create(ctx, nil); err != nil {
			t.Fatal(err)
		}
		schema := bigquery.Schema{{Name: "id", Type: bigquery.StringFieldType}, {Name: "color", Type: bigquery.StringFieldType}}
		if err := bq.Dataset(ds).Table("fruit").Create(ctx, &bigquery.TableMetadata{Schema: schema}); err != nil {
			t.Fatal(err)
		}
		q := bq.Query("INSERT INTO `" + project + "." + ds + ".fruit` (id, color) VALUES ('apple', 'red'), ('kiwi', 'green')")
		if _, err := q.Read(ctx); err != nil {
			t.Fatal(err)
		}
		table := "provbt" + suffix
		_, btc := bigtableTable(t, h, table)
		done(t, ctx, c, project, "BigQuery_to_Bigtable", launch(t, "BigQuery_to_Bigtable", "provided-bq-bt", map[string]string{
			"readIdColumn": "id", "inputTableSpec": project + ":" + ds + ".fruit", "bigtableWriteInstanceId": bigtableInstance,
			"bigtableWriteProjectId": h.Project(), "bigtableWriteTableId": table, "bigtableWriteColumnFamily": "info"}))
		var got []string
		if err := btc.Open(table).ReadRows(ctx, bigtable.InfiniteRange(""), func(r bigtable.Row) bool {
			for _, it := range r["info"] {
				got = append(got, r.Key()+"/"+it.Column+"="+string(it.Value))
			}
			return true
		}); err != nil {
			t.Fatal(err)
		}
		sort.Strings(got)
		if want := "apple/info:color=red,kiwi/info:color=green"; strings.Join(got, ",") != want {
			t.Errorf("the Bigtable table holds %v; want %s", got, want)
		}
	})
}

// objectBytes is an object's content.
func objectBytes(t *testing.T, ctx context.Context, sc *storage.Client, bucket, name string) []byte {
	t.Helper()
	r, err := sc.Bucket(bucket).Object(name).NewReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
