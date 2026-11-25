//go:build compat

package compat

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/dataflow/apiv1beta3/dataflowpb"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/spanner"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// covers: google.dataflow.v1beta3.TemplatesService/GetTemplate, google.dataflow.v1beta3.TemplatesService/LaunchTemplate
//
// TestDataflowProvidedTemplates (#2215): Google-provided templates, as the
// provided-templates guides run them, from Google's own paths
// (gs://dataflow-templates-REGION/latest/NAME), which the instance serves
// from its build of Google's DataflowTemplates sources (never from
// Google's Cloud Storage):
//   - Word_Count through `gcloud dataflow jobs run --gcs-location` counts
//     a Cloud Storage file's words into Cloud Storage;
//   - GCS_Text_to_BigQuery (templates/provided/cloud-storage-to-bigquery)
//     loads a CSV file through a JavaScript UDF into a new BigQuery table;
//   - PubSub_Subscription_to_BigQuery and PubSub_to_BigQuery
//     (templates/provided/pubsub-to-bigquery) stream a subscription's or
//     topic's JSON messages into an existing BigQuery table, and drain.
//
// GetTemplate answers each template's metadata (its parameters), and a
// name Google does not provide is not a template.
func TestDataflowProvidedTemplates(t *testing.T) {
	h, _, c := dataflowEnv(t)
	h.Endpoint(EnvStorage)
	h.Endpoint(EnvBigQuery)
	h.Endpoint(EnvPubSub)
	bq, project := bigqueryClient(t, h)
	ps := pubsubClient(t, h)
	sc := storageClient(t, h)
	// The first use builds DataflowTemplates with Maven.
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Minute)
	defer cancel()
	const base = "gs://dataflow-templates-us-central1/latest/"
	for _, name := range []string{"Word_Count", "GCS_Text_to_BigQuery", "PubSub_Subscription_to_BigQuery"} {
		tm, err := c.tmpl.GetTemplate(ctx, &dataflowpb.GetTemplateRequest{ProjectId: project, Location: "us-central1",
			Template: &dataflowpb.GetTemplateRequest_GcsPath{GcsPath: base + name}})
		if err != nil {
			t.Fatalf("GetTemplate(%s): %v", name, err)
		}
		if tm.GetTemplateType() != dataflowpb.GetTemplateResponse_LEGACY || len(tm.GetMetadata().GetParameters()) == 0 {
			t.Errorf("GetTemplate(%s) = %v; want a LEGACY template with its parameters", name, tm)
		}
	}
	if _, err := c.tmpl.GetTemplate(ctx, &dataflowpb.GetTemplateRequest{ProjectId: project, Location: "us-central1",
		Template: &dataflowpb.GetTemplateRequest_GcsPath{GcsPath: base + "No_Such_Template"}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("GetTemplate of an unknown provided template = %v; want INVALID_ARGUMENT", err)
	}

	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	bucket := fmt.Sprintf("%s-dataflow-prov-%s", h.Project(), suffix)
	dataflowBucket(t, ctx, sc, h.Project(), bucket)
	put := func(object, text string) {
		t.Helper()
		w := sc.Bucket(bucket).Object(object).NewWriter(ctx)
		if _, err := io.WriteString(w, text); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("Word_Count", func(t *testing.T) {
		put("in/words.txt", "the cat and the hat\nthe end\n")
		run := gcloudDataflow(t, h, project)
		name := "provided-wc-" + suffix
		out, err := run("dataflow", "jobs", "run", name, "--gcs-location="+base+"Word_Count", "--region=us-central1",
			"--parameters=inputFile=gs://"+bucket+"/in/words.txt,output=gs://"+bucket+"/out/counts", "--format=json")
		if err != nil {
			t.Fatalf("jobs run: %v\n%s", err, out)
		}
		var launched struct{ ID, Name string }
		if err := json.Unmarshal([]byte(out), &launched); err != nil || launched.ID == "" || launched.Name != name {
			t.Fatalf("jobs run printed %s (%v); want the job", out, err)
		}
		j := waitDataflowJob(t, ctx, c, project, launched.ID, dataflowpb.JobState_JOB_STATE_DONE, dataflowpb.JobState_JOB_STATE_FAILED)
		if j.CurrentState != dataflowpb.JobState_JOB_STATE_DONE {
			t.Fatalf("Word_Count's job is %s:\n%s", j.CurrentState, jobMessages(t, ctx, c, project, launched.ID))
		}
		if l := j.GetLabels()["goog-dataflow-provided-template-name"]; l != "word_count" {
			t.Errorf("the job's goog-dataflow-provided-template-name label is %q; want word_count", l)
		}
		got := readPrefix(t, ctx, sc, bucket, "out/counts")
		want := []string{"and: 1", "cat: 1", "end: 1", "hat: 1", "the: 3"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("Word_Count wrote %v; want %v", got, want)
		}
	})

	t.Run("GCS_Text_to_BigQuery", func(t *testing.T) {
		ds := "provided_text_" + suffix
		if err := bq.Dataset(ds).Create(ctx, nil); err != nil {
			t.Fatal(err)
		}
		put("text/people.csv", "Ada,36\nGrace,45\n")
		put("text/schema.json", `{"BigQuery Schema": [{"name": "name", "type": "STRING"}, {"name": "age", "type": "INTEGER"}]}`)
		put("text/udf.js", "function transform(line) {\n  var v = line.split(',');\n  return JSON.stringify({name: v[0].toUpperCase(), age: parseInt(v[1])});\n}\n")
		resp, err := c.tmpl.LaunchTemplate(ctx, &dataflowpb.LaunchTemplateRequest{ProjectId: project, Location: "us-central1",
			Template: &dataflowpb.LaunchTemplateRequest_GcsPath{GcsPath: base + "GCS_Text_to_BigQuery"},
			LaunchParameters: &dataflowpb.LaunchTemplateParameters{JobName: "provided-text-" + suffix, Parameters: map[string]string{
				"inputFilePattern":                    "gs://" + bucket + "/text/people.csv",
				"JSONPath":                            "gs://" + bucket + "/text/schema.json",
				"outputTable":                         project + ":" + ds + ".people",
				"bigQueryLoadingTemporaryDirectory":   "gs://" + bucket + "/bqtmp",
				"javascriptTextTransformGcsPath":      "gs://" + bucket + "/text/udf.js",
				"javascriptTextTransformFunctionName": "transform",
			}}})
		if err != nil {
			t.Fatalf("LaunchTemplate: %v", err)
		}
		id := resp.GetJob().GetId()
		j := waitDataflowJob(t, ctx, c, project, id, dataflowpb.JobState_JOB_STATE_DONE, dataflowpb.JobState_JOB_STATE_FAILED)
		if j.CurrentState != dataflowpb.JobState_JOB_STATE_DONE {
			t.Fatalf("GCS_Text_to_BigQuery's job is %s:\n%s", j.CurrentState, jobMessages(t, ctx, c, project, id))
		}
		if got := providedRows(t, ctx, bq, "SELECT name, age FROM `"+project+"."+ds+".people` ORDER BY name"); got != "ADA=36,GRACE=45" {
			t.Errorf("the table has %s; want ADA=36,GRACE=45", got)
		}
	})
	for _, tc := range []struct{ name, param string }{{"PubSub_Subscription_to_BigQuery", "inputSubscription"}, {"PubSub_to_BigQuery", "inputTopic"}} {
		t.Run(tc.name, func(t *testing.T) {
			ds := fmt.Sprintf("provided_ps_%s_%s", strings.ToLower(tc.param), suffix)
			if err := bq.Dataset(ds).Create(ctx, nil); err != nil {
				t.Fatal(err)
			}
			schema := bigquery.Schema{{Name: "name", Type: bigquery.StringFieldType}, {Name: "age", Type: bigquery.IntegerFieldType}}
			if err := bq.Dataset(ds).Table("people").Create(ctx, &bigquery.TableMetadata{Schema: schema}); err != nil {
				t.Fatal(err)
			}
			topic := fmt.Sprintf("projects/%s/topics/prov-%s-%s", project, strings.ToLower(tc.param), suffix)
			sub := fmt.Sprintf("projects/%s/subscriptions/prov-%s-%s", project, strings.ToLower(tc.param), suffix)
			if _, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
				t.Fatal(err)
			}
			input := topic
			if tc.param == "inputSubscription" {
				if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sub, Topic: topic}); err != nil {
					t.Fatal(err)
				}
				input = sub
			}
			resp, err := c.tmpl.LaunchTemplate(ctx, &dataflowpb.LaunchTemplateRequest{ProjectId: project, Location: "us-central1",
				Template: &dataflowpb.LaunchTemplateRequest_GcsPath{GcsPath: base + tc.name},
				LaunchParameters: &dataflowpb.LaunchTemplateParameters{JobName: "provided-ps-" + strings.ToLower(tc.param) + "-" + suffix, Parameters: map[string]string{
					tc.param:          input,
					"outputTableSpec": project + ":" + ds + ".people",
				}, Environment: &dataflowpb.RuntimeEnvironment{TempLocation: "gs://" + bucket + "/tmp"}}})
			if err != nil {
				t.Fatalf("LaunchTemplate: %v", err)
			}
			id := resp.GetJob().GetId()
			j := waitDataflowJob(t, ctx, c, project, id, dataflowpb.JobState_JOB_STATE_RUNNING, dataflowpb.JobState_JOB_STATE_FAILED)
			if j.CurrentState != dataflowpb.JobState_JOB_STATE_RUNNING || j.Type != dataflowpb.JobType_JOB_TYPE_STREAMING {
				t.Fatalf("%s's job is %s %s:\n%s", tc.name, j.CurrentState, j.Type, jobMessages(t, ctx, c, project, id))
			}
			// A topic read's subscription is the job's own, made as it
			// starts: what is published before it reads is not its.
			want := "Ada=36,Grace=45"
			for deadline := time.Now().Add(10 * time.Minute); ; {
				for _, m := range []string{`{"name": "Ada", "age": 36}`, `{"name": "Grace", "age": 45}`} {
					if _, err := ps.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: topic, Messages: []*pubsubpb.PubsubMessage{{Data: []byte(m)}}}); err != nil {
						t.Fatal(err)
					}
				}
				time.Sleep(15 * time.Second)
				got := providedRows(t, ctx, bq, "SELECT DISTINCT name, age FROM `"+project+"."+ds+".people` ORDER BY name")
				if got == want {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("the table has %s; want %s:\n%s", got, want, jobMessages(t, ctx, c, project, id))
				}
				if tc.param == "inputSubscription" {
					time.Sleep(15 * time.Second)
				}
			}
			if _, err := c.jobs.UpdateJob(ctx, &dataflowpb.UpdateJobRequest{ProjectId: project, Location: "us-central1", JobId: id,
				Job: &dataflowpb.Job{RequestedState: dataflowpb.JobState_JOB_STATE_DRAINED}}); err != nil {
				t.Fatalf("drain: %v", err)
			}
			if j := waitDataflowJob(t, ctx, c, project, id, dataflowpb.JobState_JOB_STATE_DRAINED, dataflowpb.JobState_JOB_STATE_FAILED, dataflowpb.JobState_JOB_STATE_CANCELLED); j.CurrentState != dataflowpb.JobState_JOB_STATE_DRAINED {
				t.Fatalf("%s's job ended %s:\n%s", tc.name, j.CurrentState, jobMessages(t, ctx, c, project, id))
			}
		})
	}
}

// TestDataflowProvidedTemplatesMore (#2215): more of Google's commonly
// used classic templates, from the same DataflowTemplates build and paths
// as TestDataflowProvidedTemplates:
//   - GCS_Text_to_Cloud_PubSub (templates/provided/cloud-storage-to-pubsub)
//     publishes each line of a Cloud Storage file to a topic;
//   - Cloud_PubSub_to_GCS_Text (templates/provided/pubsub-to-cloud-storage-text)
//     windows a topic's messages into text files, written when the
//     drained job closes its window;
//   - Bulk_Compress_GCS_Files and Bulk_Decompress_GCS_Files
//     (templates/provided/bulk-compress-cloud-storage, ...-decompress-...)
//     gzip a bucket's files and back;
//   - Spanner_to_GCS_Text (templates/provided/cloud-spanner-to-cloud-storage)
//     exports a Spanner table as CSV, from the instance's Spanner.
func TestDataflowProvidedTemplatesMore(t *testing.T) {
	h, _, c := dataflowEnv(t)
	h.Endpoint(EnvStorage)
	h.Endpoint(EnvPubSub)
	spannerAddr := h.Endpoint(EnvSpanner)
	project := h.Project()
	ps := pubsubClient(t, h)
	sc := storageClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Minute)
	defer cancel()
	const base = "gs://dataflow-templates-us-central1/latest/"
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	bucket := fmt.Sprintf("%s-dataflow-prov2-%s", project, suffix)
	dataflowBucket(t, ctx, sc, project, bucket)
	put := func(object, text string) {
		t.Helper()
		w := sc.Bucket(bucket).Object(object).NewWriter(ctx)
		if _, err := io.WriteString(w, text); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	launch := func(t *testing.T, name, job string, params map[string]string) string {
		t.Helper()
		resp, err := c.tmpl.LaunchTemplate(ctx, &dataflowpb.LaunchTemplateRequest{ProjectId: project, Location: "us-central1",
			Template: &dataflowpb.LaunchTemplateRequest_GcsPath{GcsPath: base + name},
			LaunchParameters: &dataflowpb.LaunchTemplateParameters{JobName: job + "-" + suffix, Parameters: params,
				Environment: &dataflowpb.RuntimeEnvironment{TempLocation: "gs://" + bucket + "/tmp"}}})
		if err != nil {
			t.Fatalf("LaunchTemplate(%s): %v", name, err)
		}
		return resp.GetJob().GetId()
	}
	done := func(t *testing.T, name, id string) {
		t.Helper()
		if j := waitDataflowJob(t, ctx, c, project, id, dataflowpb.JobState_JOB_STATE_DONE, dataflowpb.JobState_JOB_STATE_FAILED); j.CurrentState != dataflowpb.JobState_JOB_STATE_DONE {
			t.Fatalf("%s's job is %s:\n%s", name, j.CurrentState, jobMessages(t, ctx, c, project, id))
		}
	}
	topicSub := func(t *testing.T, id string) (string, string) {
		t.Helper()
		topic, sub := fmt.Sprintf("projects/%s/topics/%s-%s", project, id, suffix), fmt.Sprintf("projects/%s/subscriptions/%s-%s", project, id, suffix)
		if _, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
			t.Fatal(err)
		}
		if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sub, Topic: topic}); err != nil {
			t.Fatal(err)
		}
		return topic, sub
	}

	t.Run("GCS_Text_to_Cloud_PubSub", func(t *testing.T) {
		put("topubsub/lines.txt", "alpha\nbeta\n")
		topic, sub := topicSub(t, "prov-text-ps")
		done(t, "GCS_Text_to_Cloud_PubSub", launch(t, "GCS_Text_to_Cloud_PubSub", "provided-text-ps", map[string]string{
			"inputFilePattern": "gs://" + bucket + "/topubsub/*.txt", "outputTopic": topic}))
		var got []string
		for deadline := time.Now().Add(time.Minute); len(got) < 2 && time.Now().Before(deadline); {
			r, err := ps.SubscriptionAdminClient.Pull(ctx, &pubsubpb.PullRequest{Subscription: sub, MaxMessages: 10})
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range r.ReceivedMessages {
				got = append(got, string(m.Message.Data))
			}
		}
		sort.Strings(got)
		if strings.Join(got, ",") != "alpha,beta" {
			t.Errorf("the topic received %v; want alpha, beta", got)
		}
	})

	t.Run("Cloud_PubSub_to_GCS_Text", func(t *testing.T) {
		topic, _ := topicSub(t, "prov-ps-text")
		id := launch(t, "Cloud_PubSub_to_GCS_Text", "provided-ps-text", map[string]string{
			"inputTopic": topic, "outputDirectory": "gs://" + bucket + "/pstext/", "outputFilenamePrefix": "messages-",
			"userTempLocation": "gs://" + bucket + "/pstexttmp/"})
		j := waitDataflowJob(t, ctx, c, project, id, dataflowpb.JobState_JOB_STATE_RUNNING, dataflowpb.JobState_JOB_STATE_FAILED)
		if j.CurrentState != dataflowpb.JobState_JOB_STATE_RUNNING || j.Type != dataflowpb.JobType_JOB_TYPE_STREAMING {
			t.Fatalf("the job is %s %s:\n%s", j.CurrentState, j.Type, jobMessages(t, ctx, c, project, id))
		}
		// The job's subscription to the topic is made as it starts reading:
		// publish until its read has counted one.
		for deadline := time.Now().Add(5 * time.Minute); ; {
			if _, err := ps.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: topic, Messages: []*pubsubpb.PubsubMessage{{Data: []byte("kiwi")}}}); err != nil {
				t.Fatal(err)
			}
			time.Sleep(10 * time.Second)
			if read(t, ctx, c, project, id) > 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the job read none of the messages:\n%s", jobMessages(t, ctx, c, project, id))
			}
		}
		if _, err := c.jobs.UpdateJob(ctx, &dataflowpb.UpdateJobRequest{ProjectId: project, Location: "us-central1", JobId: id,
			Job: &dataflowpb.Job{RequestedState: dataflowpb.JobState_JOB_STATE_DRAINED}}); err != nil {
			t.Fatalf("drain: %v", err)
		}
		if j := waitDataflowJob(t, ctx, c, project, id, dataflowpb.JobState_JOB_STATE_DRAINED, dataflowpb.JobState_JOB_STATE_FAILED, dataflowpb.JobState_JOB_STATE_CANCELLED); j.CurrentState != dataflowpb.JobState_JOB_STATE_DRAINED {
			t.Fatalf("the job ended %s:\n%s", j.CurrentState, jobMessages(t, ctx, c, project, id))
		}
		got := readPrefix(t, ctx, sc, bucket, "pstext/messages-")
		if len(got) == 0 || got[0] != "kiwi" {
			t.Errorf("the windowed files hold %v; want the kiwi messages", got)
		}
	})

	t.Run("Bulk_Compress_and_Decompress_GCS_Files", func(t *testing.T) {
		put("bulk/in/a.txt", "the cat sat\n")
		put("bulk/in/b.txt", "on the mat\n")
		done(t, "Bulk_Compress_GCS_Files", launch(t, "Bulk_Compress_GCS_Files", "provided-compress", map[string]string{
			"inputFilePattern": "gs://" + bucket + "/bulk/in/*.txt", "outputDirectory": "gs://" + bucket + "/bulk/gz",
			"outputFailureFile": "gs://" + bucket + "/bulk/compress-failed.csv", "compression": "GZIP"}))
		r, err := sc.Bucket(bucket).Object("bulk/gz/a.txt.gz").NewReader(ctx)
		if err != nil {
			t.Fatalf("the compressed a.txt.gz: %v", err)
		}
		gz, err := gzip.NewReader(r)
		if err != nil {
			t.Fatalf("a.txt.gz is not gzip: %v", err)
		}
		b, _ := io.ReadAll(gz)
		r.Close()
		if string(b) != "the cat sat\n" {
			t.Errorf("a.txt.gz holds %q", b)
		}
		done(t, "Bulk_Decompress_GCS_Files", launch(t, "Bulk_Decompress_GCS_Files", "provided-decompress", map[string]string{
			"inputFilePattern": "gs://" + bucket + "/bulk/gz/*.gz", "outputDirectory": "gs://" + bucket + "/bulk/out",
			"outputFailureFile": "gs://" + bucket + "/bulk/decompress-failed.csv"}))
		if got := readPrefix(t, ctx, sc, bucket, "bulk/out/"); strings.Join(got, ",") != "on the mat,the cat sat" {
			t.Errorf("the decompressed files hold %v", got)
		}
	})

	t.Run("Spanner_to_GCS_Text", func(t *testing.T) {
		spInstance, spDB := "provided-"+suffix, "fruit"
		db := spannerDatabase(t, ctx, spannerAddr, project, spInstance, spDB, "CREATE TABLE Fruit (Name STRING(64) NOT NULL, N INT64) PRIMARY KEY (Name)")
		sp, err := spanner.NewClient(ctx, db, spannerOpts(spannerAddr)...)
		if err != nil {
			t.Fatal(err)
		}
		defer sp.Close()
		if _, err := sp.Apply(ctx, []*spanner.Mutation{spanner.Insert("Fruit", []string{"Name", "N"}, []any{"apple", 3}),
			spanner.Insert("Fruit", []string{"Name", "N"}, []any{"kiwi", 2})}); err != nil {
			t.Fatal(err)
		}
		done(t, "Spanner_to_GCS_Text", launch(t, "Spanner_to_GCS_Text", "provided-spanner-text", map[string]string{
			"spannerProjectId": project, "spannerInstanceId": spInstance, "spannerDatabaseId": spDB, "spannerTable": "Fruit",
			"textWritePrefix": "gs://" + bucket + "/spanner/fruit"}))
		if got := readPrefix(t, ctx, sc, bucket, "spanner/fruit"); strings.Join(got, ",") != `"apple",3,"kiwi",2` &&
			strings.Join(got, ",") != "apple,3,kiwi,2" {
			t.Errorf("the export holds %q; want the table's two rows as CSV", got)
		}
	})
}

// read is the count of elements a job's first step has output so far
// (its largest ElementCount), 0 for none.
func read(t *testing.T, ctx context.Context, c dataflowClients, project, id string) float64 {
	t.Helper()
	m, err := c.metrics.GetJobMetrics(ctx, &dataflowpb.GetJobMetricsRequest{ProjectId: project, Location: "us-central1", JobId: id})
	if err != nil {
		return 0
	}
	n := 0.0
	for _, u := range m.Metrics {
		if u.GetName().GetName() == "ElementCount" && u.GetScalar().GetNumberValue() > n {
			n = u.GetScalar().GetNumberValue()
		}
	}
	return n
}

// readPrefix is the lines of the bucket's objects under prefix, sorted.
func readPrefix(t *testing.T, ctx context.Context, sc *storage.Client, bucket, prefix string) []string {
	t.Helper()
	var lines []string
	it := sc.Bucket(bucket).Objects(ctx, &storage.Query{Prefix: prefix})
	for {
		a, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		r, err := sc.Bucket(bucket).Object(a.Name).NewReader(ctx)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range strings.Split(string(b), "\n") {
			if l != "" {
				lines = append(lines, l)
			}
		}
	}
	sort.Strings(lines)
	return lines
}

// providedRows is a query's rows as a=b pairs of their first two columns,
// joined; "" on a failed query (the table not yet written).
func providedRows(t *testing.T, ctx context.Context, bq *bigquery.Client, sql string) string {
	t.Helper()
	it, err := bq.Query(sql).Read(ctx)
	if err != nil {
		return ""
	}
	var got []string
	for {
		var r []bigquery.Value
		err := it.Next(&r)
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return ""
		}
		got = append(got, fmt.Sprintf("%v=%v", r[0], r[1]))
	}
	return strings.Join(got, ",")
}
