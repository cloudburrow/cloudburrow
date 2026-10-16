//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/protobuf/encoding/protojson"
)

// pullSinkMessages pulls from sub until want messages arrived or 30
// seconds passed, and decodes each as a LogEntry.
func pullSinkMessages(t *testing.T, h *Harness, ps *pubsub.Client, sub string, want int) []*loggingpb.LogEntry {
	t.Helper()
	var out []*loggingpb.LogEntry
	for deadline := time.Now().Add(30 * time.Second); len(out) < want && time.Now().Before(deadline); {
		resp, err := ps.SubscriptionAdminClient.Pull(h.Context(), &pubsubpb.PullRequest{Subscription: sub, MaxMessages: 10})
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range resp.GetReceivedMessages() {
			var e loggingpb.LogEntry
			if err := protojson.Unmarshal(m.GetMessage().GetData(), &e); err != nil {
				t.Fatalf("message data is not LogEntry JSON: %v", err)
			}
			if m.GetMessage().GetAttributes()["logging.googleapis.com/timestamp"] == "" {
				t.Errorf("message without logging.googleapis.com/timestamp: %v", m.GetMessage().GetAttributes())
			}
			out = append(out, &e)
		}
	}
	if len(out) != want {
		t.Fatalf("pulled %d messages from %s, want %d", len(out), sub, want)
	}
	return out
}

// TestGcloudLoggingSinksToEachDestination (#1404): `gcloud logging sinks
// create` for a Cloud Storage bucket, a Pub/Sub topic, a BigQuery dataset
// (--use-partitioned-tables) and another project, through the harness
// (egress blocked); `sinks describe` reads each one's destination and the
// project's Logging service agent as writerIdentity, and an entry written
// by `gcloud logging write` arrives at the topic, read with the official
// Pub/Sub client.
func TestGcloudLoggingSinksToEachDestination(t *testing.T) {
	h := New(t)
	_, project := bigqueryClient(t, h) // skips without CLOUDBURROW_TEST_BIGQUERY
	_, _, ps, _ := sinkClients(t, h)
	g := newGcloudSession(t, h)
	suffix := fmt.Sprint(time.Now().UnixNano() % 1_000_000_000)
	logID := "gsinklog" + suffix
	topicName := topic(t, h, ps, "gsink-topic-"+suffix)
	sub := subscription(t, h, ps, "gsink-sub-"+suffix, topicName)
	filter := fmt.Sprintf(`logName="projects/%s/logs/%s"`, project, logID)
	writer := "serviceAccount:service-" + project + "@gcp-sa-logging.iam.gserviceaccount.com"
	for name, args := range map[string][]string{
		"gs-gcs-" + suffix:     {"storage.googleapis.com/gsink-bucket-" + suffix},
		"gs-topic-" + suffix:   {"pubsub.googleapis.com/" + topicName},
		"gs-bq-" + suffix:      {"bigquery.googleapis.com/projects/" + project + "/datasets/gsinkds" + suffix, "--use-partitioned-tables"},
		"gs-project-" + suffix: {"logging.googleapis.com/projects/gsink-other-" + suffix},
	} {
		g.must(append(append([]string{"logging", "sinks", "create", name}, args...), "--log-filter="+filter, "--project="+project)...)
		name := name
		t.Cleanup(func() { _, _ = g.run(nil, "logging", "sinks", "delete", name, "--project="+project, "--quiet") })
		got := g.must("logging", "sinks", "describe", name, "--project="+project, "--format=value(destination,writerIdentity)")
		if !strings.Contains(got, args[0]+"\t"+writer) {
			t.Errorf("sinks describe %s = %q, want %s and %s", name, got, args[0], writer)
		}
		if name == "gs-bq-"+suffix {
			if got := g.must("logging", "sinks", "describe", name, "--project="+project, "--format=value(bigqueryOptions.usePartitionedTables)"); strings.TrimSpace(got) != "True" {
				t.Errorf("sinks describe %s usePartitionedTables = %q", name, got)
			}
		}
	}
	g.must("logging", "write", logID, "from gcloud "+suffix, "--project="+project)
	got := pullSinkMessages(t, h, ps, sub, 1)
	if got[0].GetTextPayload() != "from gcloud "+suffix || got[0].GetLogName() != "projects/"+project+"/logs/"+logID {
		t.Errorf("the message's entry is %v", got[0])
	}
}

// covers: google.logging.v2.ConfigServiceV2/CreateSink, google.logging.v2.ConfigServiceV2/GetSink, google.logging.v2.ConfigServiceV2/DeleteSink
//
// TestTerraformLoggingSinkDestinations (#1404): google_logging_project_sink
// to a google_storage_bucket, a google_pubsub_topic, a
// google_bigquery_dataset (bigquery_options.use_partitioned_tables) and
// another project apply with the official hashicorp/google provider through
// `cloudburrow terraform` (egress blocked), each with
// unique_writer_identity, and grant the sink's writer_identity on its
// destination (google_storage_bucket_iam_member,
// google_pubsub_topic_iam_member, google_bigquery_dataset_iam_member), as
// Google's docs tell; the plan after apply is clean. An entry written with
// the official Client arrives at the topic and is in the table, read with
// the official Pub/Sub and BigQuery clients.
func TestTerraformLoggingSinkDestinations(t *testing.T) {
	h := New(t)
	bq, project := bigqueryClient(t, h) // skips without CLOUDBURROW_TEST_BIGQUERY
	lc, cc, ps, _ := sinkClients(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	suffix := fmt.Sprint(time.Now().UnixNano() % 1_000_000_000)
	logID := "tfsinklog" + suffix
	ds := "tfsinkds" + suffix
	topicID := "tf-sink-topic-" + suffix
	filter := fmt.Sprintf(`logName=\"projects/%s/logs/%s\"`, project, logID)
	m.write(fmt.Sprintf(`
resource "google_storage_bucket" "b" {
  project       = %[1]q
  name          = "tf-sink-bucket-%[2]s"
  location      = "US"
  force_destroy = true
}
resource "google_pubsub_topic" "t" {
  project = %[1]q
  name    = %[3]q
}
resource "google_bigquery_dataset" "d" {
  project                    = %[1]q
  dataset_id                 = %[4]q
  location                   = "US"
  delete_contents_on_destroy = true
}
resource "google_logging_project_sink" "gcs" {
  project                = %[1]q
  name                   = "tf-gcs-%[2]s"
  destination            = "storage.googleapis.com/${google_storage_bucket.b.name}"
  filter                 = "%[5]s"
  unique_writer_identity = true
}
resource "google_logging_project_sink" "topic" {
  project                = %[1]q
  name                   = "tf-topic-%[2]s"
  destination            = "pubsub.googleapis.com/${google_pubsub_topic.t.id}"
  filter                 = "%[5]s"
  unique_writer_identity = true
}
resource "google_logging_project_sink" "bq" {
  project                = %[1]q
  name                   = "tf-bq-%[2]s"
  destination            = "bigquery.googleapis.com/projects/%[1]s/datasets/${google_bigquery_dataset.d.dataset_id}"
  filter                 = "%[5]s"
  unique_writer_identity = true
  bigquery_options {
    use_partitioned_tables = true
  }
}
resource "google_logging_project_sink" "project" {
  project                = %[1]q
  name                   = "tf-project-%[2]s"
  destination            = "logging.googleapis.com/projects/tf-sink-other-%[2]s"
  filter                 = "%[5]s"
  unique_writer_identity = true
}
resource "google_storage_bucket_iam_member" "gcs" {
  bucket = google_storage_bucket.b.name
  role   = "roles/storage.objectCreator"
  member = google_logging_project_sink.gcs.writer_identity
}
resource "google_pubsub_topic_iam_member" "topic" {
  project = %[1]q
  topic   = google_pubsub_topic.t.name
  role    = "roles/pubsub.publisher"
  member  = google_logging_project_sink.topic.writer_identity
}
resource "google_bigquery_dataset_iam_member" "bq" {
  project    = %[1]q
  dataset_id = google_bigquery_dataset.d.dataset_id
  role       = "roles/bigquery.dataEditor"
  member     = google_logging_project_sink.bq.writer_identity
}
`, project, suffix, topicID, ds, filter))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	m.planClean()

	parent := "projects/" + project
	writer := "serviceAccount:service-" + project + "@gcp-sa-logging.iam.gserviceaccount.com"
	for _, id := range []string{"tf-gcs-", "tf-topic-", "tf-bq-", "tf-project-"} {
		sk, err := cc.GetSink(ctx, &loggingpb.GetSinkRequest{SinkName: parent + "/sinks/" + id + suffix})
		if err != nil {
			t.Fatalf("GetSink %s: %v", id, err)
		}
		if sk.GetWriterIdentity() != writer {
			t.Errorf("sink %s writer_identity %q, want %q", id, sk.GetWriterIdentity(), writer)
		}
	}
	sub := subscription(t, h, ps, "tf-sink-sub-"+suffix, "projects/"+project+"/topics/"+topicID)
	if _, err := lc.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{LogName: parent + "/logs/" + logID,
		Resource: &mrpb.MonitoredResource{Type: "global"},
		Entries:  []*loggingpb.LogEntry{{Payload: &loggingpb.LogEntry_TextPayload{TextPayload: "from terraform " + suffix}}}}); err != nil {
		t.Fatal(err)
	}
	if got := pullSinkMessages(t, h, ps, sub, 1); got[0].GetTextPayload() != "from terraform "+suffix {
		t.Errorf("the message's entry is %v", got[0])
	}
	q := fmt.Sprintf("SELECT textPayload FROM `%s.%s.%s`", project, ds, logID)
	var text string
	for deadline := time.Now().Add(30 * time.Second); text == "" && time.Now().Before(deadline); time.Sleep(time.Second) {
		it, err := bq.Query(q).Read(ctx)
		if err != nil {
			continue
		}
		var row []bigquery.Value
		if err := it.Next(&row); err == nil {
			text, _ = row[0].(string)
		}
	}
	if text != "from terraform "+suffix {
		t.Errorf("%s read %q", q, text)
	}
}
