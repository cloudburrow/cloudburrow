//go:build compat

package compat

import (
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
