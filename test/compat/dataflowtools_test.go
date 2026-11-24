//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/dataflow/apiv1beta3/dataflowpb"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
)

// dataflowTemplate is a classic template the Python SDK staged
// (--template_location), from testdata/dataflowpy/streaming.py: a Pub/Sub
// subscription's words counted into a BigQuery table, which the tools'
// tests launch, see running and drain.
type dataflowTemplate struct {
	project, bucket, path, topic, sub, dataset string
}

func stageDataflowTemplate(t *testing.T, h *Harness, ctx context.Context, c dataflowClients) dataflowTemplate {
	t.Helper()
	storageAddr := h.Endpoint(EnvStorage)
	h.Endpoint(EnvPubSub)
	h.Endpoint(EnvBigQuery)
	dfAddr := h.Endpoint(EnvDataflow)
	bq, project := bigqueryClient(t, h)
	ps := pubsubClient(t, h)
	sc := storageClient(t, h)
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	tp := dataflowTemplate{project: project, bucket: fmt.Sprintf("%s-dataflow-tpl-%s", h.Project(), suffix),
		topic:   fmt.Sprintf("projects/%s/topics/tpl-words-%s", project, suffix),
		sub:     fmt.Sprintf("projects/%s/subscriptions/tpl-words-%s", project, suffix),
		dataset: "dataflow_tpl_" + suffix}
	dataflowBucket(t, ctx, sc, h.Project(), tp.bucket)
	if _, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: tp.topic}); err != nil {
		t.Fatal(err)
	}
	if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: tp.sub, Topic: tp.topic}); err != nil {
		t.Fatal(err)
	}
	if err := bq.Dataset(tp.dataset).Create(ctx, nil); err != nil {
		t.Fatal(err)
	}
	network := beamNetwork(t, ctx, map[int]string{9000: storageAddr, 80: dfAddr})
	tp.path = "gs://" + tp.bucket + "/templates/words"
	runBeamPython(t, ctx, network, project, append([]string{"/flow/streaming.py",
		"--subscription=" + tp.sub, "--table=" + project + ":" + tp.dataset + ".counts", "--template_location=" + tp.path},
		dataflowRunnerArgs(project, tp.bucket, "words-template")...)...)
	t.Cleanup(func() {
		// Whatever a test left running.
		it := c.jobs.ListJobs(context.Background(), &dataflowpb.ListJobsRequest{ProjectId: project, Location: "us-central1", Filter: dataflowpb.ListJobsRequest_ACTIVE})
		for {
			j, err := it.Next()
			if err != nil {
				break
			}
			_, _ = c.jobs.UpdateJob(context.Background(), &dataflowpb.UpdateJobRequest{ProjectId: project, Location: "us-central1", JobId: j.Id,
				Job: &dataflowpb.Job{RequestedState: dataflowpb.JobState_JOB_STATE_CANCELLED}})
		}
	})
	return tp
}

// gcloudDataflow is gcloud configured by `cloudburrow gcloud-setup` alone,
// its dataflow override checked, with the project the template's (the
// BigQuery emulator's one project).
func gcloudDataflow(t *testing.T, h *Harness, project string) func(args ...string) (string, error) {
	t.Helper()
	gcloud := gcloudBinary(t)
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	gdir := t.TempDir()
	name := gcloudSetup(t, cli, gdir)
	if conf, _ := os.ReadFile(filepath.Join(gdir, "configurations", "config_"+name)); !strings.Contains(string(conf), "dataflow = http://") {
		t.Fatalf("the configuration has no dataflow override:\n%s", conf)
	}
	return func(args ...string) (string, error) {
		cmd := exec.Command(gcloud, append(args, "--quiet", "--project", project)...)
		cmd.Env = append(gcloudTargetEnv(os.Environ()), "CLOUDSDK_CONFIG="+gdir, "CLOUDSDK_ACTIVE_CONFIG_NAME="+name)
		b, err := cmd.CombinedOutput()
		return string(b), err
	}
}

// covers: google.dataflow.v1beta3.TemplatesService/LaunchTemplate, google.dataflow.v1beta3.TemplatesService/GetTemplate
//
// TestGcloudDataflow (#2215): `gcloud dataflow jobs run` launches the
// classic template the Python SDK staged (templates.launch, validated
// first with a dry run through the Go client); `jobs list --status=active`,
// `jobs describe` and `jobs show` see the
// running streaming job; `jobs drain` drains it and `jobs cancel` of the
// drained job is refused.
func TestGcloudDataflow(t *testing.T) {
	h, _, c := dataflowEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	tp := stageDataflowTemplate(t, h, ctx, c)
	run := gcloudDataflow(t, h, tp.project)
	gc := func(args ...string) string {
		t.Helper()
		out, err := run(args...)
		if err != nil {
			t.Fatalf("gcloud %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}

	tm, err := c.tmpl.GetTemplate(ctx, &dataflowpb.GetTemplateRequest{ProjectId: tp.project, Location: "us-central1",
		Template: &dataflowpb.GetTemplateRequest_GcsPath{GcsPath: tp.path}})
	if err != nil || tm.GetTemplateType() != dataflowpb.GetTemplateResponse_LEGACY {
		t.Errorf("GetTemplate = %v, %v; want a LEGACY template", tm, err)
	}
	dry, err := c.tmpl.LaunchTemplate(ctx, &dataflowpb.LaunchTemplateRequest{ProjectId: tp.project, Location: "us-central1", ValidateOnly: true,
		Template:         &dataflowpb.LaunchTemplateRequest_GcsPath{GcsPath: tp.path},
		LaunchParameters: &dataflowpb.LaunchTemplateParameters{JobName: "dry-run"}})
	if err != nil || dry.GetJob() != nil && dry.GetJob().GetId() != "" {
		t.Errorf("LaunchTemplate(validateOnly) = %v, %v; want no job", dry, err)
	}

	name := fmt.Sprintf("gcloud-words-%d", time.Now().UnixNano()%1e6)
	out := gc("dataflow", "jobs", "run", name, "--gcs-location="+tp.path, "--region=us-central1",
		"--staging-location=gs://"+tp.bucket+"/staging", "--max-workers=2", "--format=json")
	var launched struct{ ID, Name, Type string }
	if err := json.Unmarshal([]byte(out), &launched); err != nil || launched.ID == "" || launched.Name != name {
		t.Fatalf("jobs run printed %s (%v); want the job", out, err)
	}
	id := launched.ID
	j := waitDataflowJob(t, ctx, c, tp.project, id, dataflowpb.JobState_JOB_STATE_RUNNING, dataflowpb.JobState_JOB_STATE_FAILED)
	if j.CurrentState != dataflowpb.JobState_JOB_STATE_RUNNING {
		t.Fatalf("the template's job is %s:\n%s", j.CurrentState, jobMessages(t, ctx, c, tp.project, id))
	}
	if j.Type != dataflowpb.JobType_JOB_TYPE_STREAMING || j.GetJobMetadata().GetSdkVersion().GetVersion() != "2.78.0" {
		t.Errorf("the template's job is %s with SDK %v; want streaming, Beam 2.78.0", j.Type, j.GetJobMetadata().GetSdkVersion())
	}

	if out := gc("dataflow", "jobs", "list", "--region=us-central1", "--status=active", "--format=value(id,state)"); !strings.Contains(out, id) || !strings.Contains(out, "Running") {
		t.Errorf("jobs list --status=active:\n%s", out)
	}
	var desc struct {
		ID, Name, CurrentState, Type string
	}
	if err := json.Unmarshal([]byte(gc("dataflow", "jobs", "describe", id, "--region=us-central1", "--format=json")), &desc); err != nil ||
		desc.CurrentState != "JOB_STATE_RUNNING" || desc.Type != "JOB_TYPE_STREAMING" || desc.Name != name {
		t.Errorf("jobs describe = %+v, %v", desc, err)
	}
	if out := gc("dataflow", "jobs", "show", id, "--region=us-central1"); !strings.Contains(out, "state: Running") || !strings.Contains(out, "type: Streaming") {
		t.Errorf("jobs show:\n%s", out)
	}
	gc("dataflow", "jobs", "drain", id, "--region=us-central1")
	j = waitDataflowJob(t, ctx, c, tp.project, id, dataflowpb.JobState_JOB_STATE_DRAINED, dataflowpb.JobState_JOB_STATE_FAILED, dataflowpb.JobState_JOB_STATE_CANCELLED)
	if j.CurrentState != dataflowpb.JobState_JOB_STATE_DRAINED {
		t.Fatalf("after jobs drain the job is %s:\n%s", j.CurrentState, jobMessages(t, ctx, c, tp.project, id))
	}
	if out, err := run("dataflow", "jobs", "cancel", id, "--region=us-central1"); err == nil && !strings.Contains(out, "Failed") {
		t.Errorf("jobs cancel of a drained job succeeded:\n%s", out)
	}
}

// TestTerraformDataflowJob (#2215): the hashicorp/google provider's
// google_dataflow_job through `cloudburrow terraform`
// (dataflow_custom_endpoint): apply launches the classic template the
// Python SDK staged, the streaming job runs, a plan is
// clean, and destroy drains the job, on_delete's default.
func TestTerraformDataflowJob(t *testing.T) {
	h, _, c := dataflowEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	tp := stageDataflowTemplate(t, h, ctx, c)
	m := newTFModule(t)
	name := fmt.Sprintf("tf-words-%d", time.Now().UnixNano()%1e6)
	module := func(workers int) string {
		return fmt.Sprintf(`
resource "google_dataflow_job" "j" {
  project           = %q
  region            = "us-central1"
  name              = %q
  template_gcs_path = %q
  temp_gcs_location = "gs://%s/tmp"
  max_workers       = %d
  labels            = { env = "test" }
}
output "id" {
  value = google_dataflow_job.j.job_id
}
`, tp.project, name, tp.path, tp.bucket, workers)
	}
	m.write(module(2))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	id := strings.TrimSpace(m.stdout("output", "-raw", "id"))
	if j := waitDataflowJob(t, ctx, c, tp.project, id, dataflowpb.JobState_JOB_STATE_RUNNING, dataflowpb.JobState_JOB_STATE_FAILED); j.CurrentState != dataflowpb.JobState_JOB_STATE_RUNNING {
		t.Fatalf("the job is %s:\n%s", j.CurrentState, jobMessages(t, ctx, c, tp.project, id))
	}
	j, err := c.jobs.GetJob(ctx, &dataflowpb.GetJobRequest{ProjectId: tp.project, Location: "us-central1", JobId: id, View: dataflowpb.JobView_JOB_VIEW_ALL})
	if err != nil || j.Name != name || j.Labels["env"] != "test" || j.Type != dataflowpb.JobType_JOB_TYPE_STREAMING {
		t.Fatalf("GetJob after apply = %v, %v", j, err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	j = waitDataflowJob(t, ctx, c, tp.project, id, dataflowpb.JobState_JOB_STATE_DRAINED, dataflowpb.JobState_JOB_STATE_CANCELLED, dataflowpb.JobState_JOB_STATE_FAILED)
	if j.CurrentState != dataflowpb.JobState_JOB_STATE_DRAINED {
		t.Errorf("after destroy the job is %s; want DRAINED (on_delete's default, drain)", j.CurrentState)
	}
}
