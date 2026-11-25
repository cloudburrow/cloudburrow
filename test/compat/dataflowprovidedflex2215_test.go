//go:build compat

package compat

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/dataflow/apiv1beta3/dataflowpb"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/storage"
)

// covers: google.dataflow.v1beta3.FlexTemplatesService/LaunchFlexTemplate, google.dataflow.v1beta3.TemplatesService/GetTemplate
//
// TestDataflowProvidedFlexTemplates (#2215): Google-provided Flex
// Templates, from Google's spec paths
// (gs://dataflow-templates-REGION/latest/flex/NAME), which the instance
// serves from its build of the DataflowTemplates v2 module
// googlecloud-to-googlecloud (never from Google's Cloud Storage or
// registries):
//   - GCS_Text_to_BigQuery_Flex (templates/provided/cloud-storage-to-bigquery,
//     the Flex variant) through `gcloud dataflow flex-template run`, loading
//     a CSV file through a JavaScript UDF into a new BigQuery table;
//   - PubSub_to_BigQuery_Flex (templates/provided/pubsub-to-bigquery)
//     through LaunchFlexTemplate, streaming a subscription's JSON messages
//     into a table, then drained;
//   - Cloud_PubSub_to_GCS_Text_Flex (templates/provided/pubsub-to-text,
//     the Flex variant) through LaunchFlexTemplate, windowing a
//     subscription's messages into text files, then drained.
//
// GetTemplate answers a FLEX template with its parameters, and a classic
// template's name under flex/ is not a template.
func TestDataflowProvidedFlexTemplates(t *testing.T) {
	h, _, c := dataflowEnv(t)
	h.Endpoint(EnvStorage)
	h.Endpoint(EnvBigQuery)
	h.Endpoint(EnvPubSub)
	bq, project := bigqueryClient(t, h)
	ps := pubsubClient(t, h)
	sc := storageClient(t, h)
	// The first use builds the v2 module with Maven.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Minute)
	defer cancel()
	const base = "gs://dataflow-templates-us-central1/latest/flex/"
	tm, err := c.tmpl.GetTemplate(ctx, &dataflowpb.GetTemplateRequest{ProjectId: project, Location: "us-central1",
		Template: &dataflowpb.GetTemplateRequest_GcsPath{GcsPath: base + "GCS_Text_to_BigQuery_Flex"}})
	if err != nil {
		t.Fatalf("GetTemplate(GCS_Text_to_BigQuery_Flex): %v", err)
	}
	params := map[string]bool{}
	for _, p := range tm.GetMetadata().GetParameters() {
		params[p.GetName()] = true
	}
	if tm.GetTemplateType() != dataflowpb.GetTemplateResponse_FLEX || !params["inputFilePattern"] || !params["outputTable"] {
		t.Errorf("GetTemplate(GCS_Text_to_BigQuery_Flex) = %v; want a FLEX template with its parameters", tm)
	}
	if _, err := c.tmpl.GetTemplate(ctx, &dataflowpb.GetTemplateRequest{ProjectId: project, Location: "us-central1",
		Template: &dataflowpb.GetTemplateRequest_GcsPath{GcsPath: base + "Word_Count"}}); err == nil {
		t.Error("GetTemplate of a classic template's name under flex/ succeeded")
	}

	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	bucket := fmt.Sprintf("%s-dataflow-provflex-%s", h.Project(), suffix)
	dataflowBucket(t, ctx, sc, h.Project(), bucket)
	put := bucketWriter(t, ctx, sc, bucket)

	t.Run("GCS_Text_to_BigQuery_Flex", func(t *testing.T) {
		ds := "provided_flex_text_" + suffix
		if err := bq.Dataset(ds).Create(ctx, nil); err != nil {
			t.Fatal(err)
		}
		put("text/people.csv", "Ada,36\nGrace,45\n")
		put("text/schema.json", `{"BigQuery Schema": [{"name": "name", "type": "STRING"}, {"name": "age", "type": "INTEGER"}]}`)
		put("text/udf.js", "function transform(line) {\n  var v = line.split(',');\n  return JSON.stringify({name: v[0].toUpperCase(), age: parseInt(v[1])});\n}\n")
		gc := gcloudDataflow(t, h, project)
		name := "provided-flex-text-" + suffix
		out, err := gc("dataflow", "flex-template", "run", name, "--region=us-central1",
			"--template-file-gcs-location="+base+"GCS_Text_to_BigQuery_Flex", "--temp-location=gs://"+bucket+"/tmp",
			"--parameters=inputFilePattern=gs://"+bucket+"/text/people.csv,JSONPath=gs://"+bucket+"/text/schema.json"+
				",outputTable="+project+":"+ds+".people,bigQueryLoadingTemporaryDirectory=gs://"+bucket+"/bqtmp"+
				",javascriptTextTransformGcsPath=gs://"+bucket+"/text/udf.js,javascriptTextTransformFunctionName=transform",
			"--format=value(job.id)")
		id := strings.TrimSpace(out)
		if err != nil || id == "" || strings.ContainsAny(id, " \n") {
			t.Fatalf("flex-template run: %v\n%s", err, out)
		}
		j := waitDataflowJob(t, ctx, c, project, id, dataflowpb.JobState_JOB_STATE_DONE, dataflowpb.JobState_JOB_STATE_FAILED)
		if j.CurrentState != dataflowpb.JobState_JOB_STATE_DONE || j.Name != name {
			t.Fatalf("GCS_Text_to_BigQuery_Flex's job is %s %s:\n%s", j.Name, j.CurrentState, jobMessages(t, ctx, c, project, id))
		}
		if l := j.GetLabels()["goog-dataflow-provided-template-type"]; l != "flex" {
			t.Errorf("the job's goog-dataflow-provided-template-type label is %q; want flex", l)
		}
		if got := providedRows(t, ctx, bq, "SELECT name, age FROM `"+project+"."+ds+".people` ORDER BY name"); got != "ADA=36,GRACE=45" {
			t.Errorf("the table has %s; want ADA=36,GRACE=45", got)
		}
	})

	t.Run("PubSub_to_BigQuery_Flex", func(t *testing.T) {
		ds := "provided_flex_ps_" + suffix
		if err := bq.Dataset(ds).Create(ctx, nil); err != nil {
			t.Fatal(err)
		}
		schema := bigquery.Schema{{Name: "name", Type: bigquery.StringFieldType}, {Name: "age", Type: bigquery.IntegerFieldType}}
		if err := bq.Dataset(ds).Table("people").Create(ctx, &bigquery.TableMetadata{Schema: schema}); err != nil {
			t.Fatal(err)
		}
		topic := fmt.Sprintf("projects/%s/topics/provflex-%s", project, suffix)
		sub := fmt.Sprintf("projects/%s/subscriptions/provflex-%s", project, suffix)
		if _, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
			t.Fatal(err)
		}
		if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sub, Topic: topic}); err != nil {
			t.Fatal(err)
		}
		resp, err := c.flex.LaunchFlexTemplate(ctx, &dataflowpb.LaunchFlexTemplateRequest{ProjectId: project, Location: "us-central1",
			LaunchParameter: &dataflowpb.LaunchFlexTemplateParameter{JobName: "provided-flex-ps-" + suffix,
				Template:    &dataflowpb.LaunchFlexTemplateParameter_ContainerSpecGcsPath{ContainerSpecGcsPath: base + "PubSub_to_BigQuery_Flex"},
				Parameters:  map[string]string{"inputSubscription": sub, "outputTableSpec": project + ":" + ds + ".people"},
				Environment: &dataflowpb.FlexTemplateRuntimeEnvironment{TempLocation: "gs://" + bucket + "/tmp"}}})
		if err != nil {
			t.Fatalf("LaunchFlexTemplate: %v", err)
		}
		id := resp.GetJob().GetId()
		j := waitDataflowJob(t, ctx, c, project, id, dataflowpb.JobState_JOB_STATE_RUNNING, dataflowpb.JobState_JOB_STATE_FAILED)
		if j.CurrentState != dataflowpb.JobState_JOB_STATE_RUNNING || j.Type != dataflowpb.JobType_JOB_TYPE_STREAMING {
			t.Fatalf("PubSub_to_BigQuery_Flex's job is %s %s:\n%s", j.CurrentState, j.Type, jobMessages(t, ctx, c, project, id))
		}
		want := "Ada=36,Grace=45"
		for deadline := time.Now().Add(10 * time.Minute); ; {
			for _, m := range []string{`{"name": "Ada", "age": 36}`, `{"name": "Grace", "age": 45}`} {
				if _, err := ps.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: topic, Messages: []*pubsubpb.PubsubMessage{{Data: []byte(m)}}}); err != nil {
					t.Fatal(err)
				}
			}
			time.Sleep(30 * time.Second)
			got := providedRows(t, ctx, bq, "SELECT DISTINCT name, age FROM `"+project+"."+ds+".people` ORDER BY name")
			if got == want {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the table has %s; want %s:\n%s", got, want, jobMessages(t, ctx, c, project, id))
			}
		}
		if _, err := c.jobs.UpdateJob(ctx, &dataflowpb.UpdateJobRequest{ProjectId: project, Location: "us-central1", JobId: id,
			Job: &dataflowpb.Job{RequestedState: dataflowpb.JobState_JOB_STATE_DRAINED}}); err != nil {
			t.Fatalf("drain: %v", err)
		}
		if j := waitDataflowJob(t, ctx, c, project, id, dataflowpb.JobState_JOB_STATE_DRAINED, dataflowpb.JobState_JOB_STATE_FAILED, dataflowpb.JobState_JOB_STATE_CANCELLED); j.CurrentState != dataflowpb.JobState_JOB_STATE_DRAINED {
			t.Fatalf("the job ended %s:\n%s", j.CurrentState, jobMessages(t, ctx, c, project, id))
		}
	})

	t.Run("Cloud_PubSub_to_GCS_Text_Flex", func(t *testing.T) {
		topic := fmt.Sprintf("projects/%s/topics/provflextext-%s", project, suffix)
		sub := fmt.Sprintf("projects/%s/subscriptions/provflextext-%s", project, suffix)
		if _, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
			t.Fatal(err)
		}
		if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sub, Topic: topic}); err != nil {
			t.Fatal(err)
		}
		resp, err := c.flex.LaunchFlexTemplate(ctx, &dataflowpb.LaunchFlexTemplateRequest{ProjectId: project, Location: "us-central1",
			LaunchParameter: &dataflowpb.LaunchFlexTemplateParameter{JobName: "provided-flex-pstext-" + suffix,
				Template: &dataflowpb.LaunchFlexTemplateParameter_ContainerSpecGcsPath{ContainerSpecGcsPath: base + "Cloud_PubSub_to_GCS_Text_Flex"},
				Parameters: map[string]string{"inputSubscription": sub, "outputDirectory": "gs://" + bucket + "/flextext/",
					"outputFilenamePrefix": "messages-", "userTempLocation": "gs://" + bucket + "/flextexttmp/"},
				Environment: &dataflowpb.FlexTemplateRuntimeEnvironment{TempLocation: "gs://" + bucket + "/tmp"}}})
		if err != nil {
			t.Fatalf("LaunchFlexTemplate: %v", err)
		}
		id := resp.GetJob().GetId()
		j := waitDataflowJob(t, ctx, c, project, id, dataflowpb.JobState_JOB_STATE_RUNNING, dataflowpb.JobState_JOB_STATE_FAILED)
		if j.CurrentState != dataflowpb.JobState_JOB_STATE_RUNNING || j.Type != dataflowpb.JobType_JOB_TYPE_STREAMING {
			t.Fatalf("Cloud_PubSub_to_GCS_Text_Flex's job is %s %s:\n%s", j.CurrentState, j.Type, jobMessages(t, ctx, c, project, id))
		}
		// Publish until the window step (WINDOWDURATION Window) has counted
		// a message, then drain: the window's file is written as the job
		// drains.
		for deadline := time.Now().Add(5 * time.Minute); ; {
			if _, err := ps.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: topic, Messages: []*pubsubpb.PubsubMessage{{Data: []byte("fig")}}}); err != nil {
				t.Fatal(err)
			}
			time.Sleep(10 * time.Second)
			if read(t, ctx, c, project, id, " Window") > 0 {
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
		if got := readPrefix(t, ctx, sc, bucket, "flextext/messages-"); len(got) == 0 || got[0] != "fig" {
			// What the write left (its temporary files, under
			// userTempLocation, say which of its stages did not run).
			var left []string
			it := sc.Bucket(bucket).Objects(ctx, &storage.Query{Prefix: "flextext"})
			for a, err := it.Next(); err == nil; a, err = it.Next() {
				left = append(left, a.Name)
			}
			t.Errorf("the windowed files hold %v; want the fig messages (objects: %v):\n%s", got, left, jobMessages(t, ctx, c, project, id))
		}
	})
}
