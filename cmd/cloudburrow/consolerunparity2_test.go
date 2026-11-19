package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"
)

// TestRunJobCreateFormIsGoogles (#1852): Create job starts with Container
// image URL, Job name, Region and Number of tasks, has Google's Task timeout
// with a Time unit and the Parallelism choice, and ends in Execute job
// immediately; the edit form has neither Region nor Execute job immediately.
func TestRunJobCreateFormIsGoogles(t *testing.T) {
	_, fields := runJobsProvider{region: "europe-west1"}.CreateForm()
	var names []string
	for _, f := range fields {
		names = append(names, f.Name)
	}
	if !slices.Equal(names[:4], []string{"image", "name", "region", "taskCount"}) || names[len(names)-1] != "executeNow" {
		t.Errorf("field order = %v", names)
	}
	for _, want := range []string{"timeoutUnit", "parallelismMode"} {
		if !slices.Contains(names, want) {
			t.Errorf("no %s field in %v", want, names)
		}
	}
	et, err := runJobFormTemplate(map[string]string{"image": "busybox:1.36", "timeout": "10", "timeoutUnit": "minutes",
		"parallelismMode": runParallelLimit, "parallelism": "2", "taskCount": "4"})
	if err != nil {
		t.Fatal(err)
	}
	if et.GetTemplate().GetTimeout().AsDuration() != 10*time.Minute || et.GetParallelism() != 2 {
		t.Errorf("template = %v", et)
	}
	if et, _ = runJobFormTemplate(map[string]string{"image": "busybox:1.36", "parallelismMode": runParallelAll, "parallelism": "2"}); et.GetParallelism() != 0 {
		t.Errorf("Run as many tasks concurrently as possible sent parallelism %d", et.GetParallelism())
	}
	for _, bad := range []map[string]string{
		{"image": "busybox:1.36", "timeout": "169", "timeoutUnit": "hours"},
		{"image": "busybox:1.36", "parallelismMode": runParallelLimit},
		{"image": "busybox:1.36", "timeoutUnit": "days", "timeout": "1"},
	} {
		if _, err := runJobFormTemplate(bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	if v, u := runTimeoutText(90 * time.Second); v != "90" || u != "seconds" {
		t.Errorf("90s = %s %s", v, u)
	}
	if v, u := runTimeoutText(2 * time.Hour); v != "2" || u != "hours" {
		t.Errorf("2h = %s %s", v, u)
	}
	job := &runpb.Job{Name: "projects/p/locations/us-central1/jobs/j", Template: &runpb.ExecutionTemplate{
		Template: &runpb.TaskTemplate{Containers: []*runpb.Container{{Image: "busybox:1.36"}}}}}
	for _, f := range runJobEditForm(job).Fields {
		if f.Name == "region" || f.Name == "executeNow" {
			t.Errorf("the edit form has %s", f.Name)
		}
	}
}

// TestRunWorkerPoolFormsAreGoogles (#1852): Deploy container opens Create
// worker pool (image, name, region, then scaling), ending in Create; the
// edit is Edit and deploy new revision, Deploy worker pool revision, Deploy.
func TestRunWorkerPoolFormsAreGoogles(t *testing.T) {
	p := runWorkerPoolsProvider{region: "us-central1"}
	label, fields := p.CreateForm()
	if label != "Deploy container" || p.CreateTitle() != "Create worker pool" || p.CreateSubmit() != "Create" {
		t.Errorf("labels = %q %q %q", label, p.CreateTitle(), p.CreateSubmit())
	}
	if fields[0].Name != "image" || fields[1].Name != "name" || fields[2].Name != "region" || fields[3].Name != "scalingMode" {
		t.Errorf("order = %s %s %s %s", fields[0].Name, fields[1].Name, fields[2].Name, fields[3].Name)
	}
	wp := &runpb.WorkerPool{Name: "projects/p/locations/us-central1/workerPools/w",
		Template: &runpb.WorkerPoolRevisionTemplate{Containers: []*runpb.Container{{Image: "busybox:1.36"}}}}
	e := workerPoolEditForm(wp)
	if e.Label != "Edit and deploy new revision" || e.Title != "Deploy worker pool revision" || e.Submit != "Deploy" {
		t.Errorf("edit = %q %q %q", e.Label, e.Title, e.Submit)
	}
	for _, f := range e.Fields {
		if f.Name == "region" {
			t.Error("the edit form has Region")
		}
	}
}

// TestRunTriggersNeedPubSub (#1852): without Pub/Sub the Triggers tab says
// so and offers nothing.
func TestRunTriggersNeedPubSub(t *testing.T) {
	svc := &ksvcStatus{KName: "hello"}
	svc.Metadata.Labels = map[string]string{"cloudburrow.dev/owned": "true"}
	p := runProvider{namespace: "default", runAddr: fixedAddr("127.0.0.1:1")}
	sec := p.triggersSection(t.Context(), "demo", svc)
	if !strings.Contains(sec.Text, "does not serve Pub/Sub") || len(sec.Actions) != 0 {
		t.Errorf("Triggers without Pub/Sub = %+v", sec)
	}
	if a := p.triggerActions(t.Context(), "demo", svc); a != nil {
		t.Errorf("trigger actions without Pub/Sub = %v", a)
	}
}

// TestRunAddTriggerIsAnEventarcTrigger (#1877): Add trigger's values are
// an Eventarc trigger to the service: Pub/Sub on the chosen topic (or one
// Eventarc makes), Cloud Storage on a bucket, with the path and account.
func TestRunAddTriggerIsAnEventarcTrigger(t *testing.T) {
	tr, err := eventarcTrigger("demo", "hello", "us-central1", map[string]string{"eventType": runPubSubEvent, "topic": "orders",
		"path": "/events", "serviceAccount": "sa@demo.iam.gserviceaccount.com"})
	if err != nil || tr.GetTransport().GetPubsub().GetTopic() != "projects/demo/topics/orders" ||
		tr.GetDestination().GetCloudRun().GetService() != "hello" || tr.GetDestination().GetCloudRun().GetPath() != "/events" ||
		tr.GetServiceAccount() != "sa@demo.iam.gserviceaccount.com" || len(tr.GetEventFilters()) != 1 {
		t.Errorf("Pub/Sub trigger = %v, %v", tr, err)
	}
	if tr, err := eventarcTrigger("demo", "hello", "us-central1", map[string]string{"eventType": runPubSubEvent}); err != nil || tr.GetTransport() != nil {
		t.Errorf("Pub/Sub trigger with no topic = %v, %v", tr, err)
	}
	tr, err = eventarcTrigger("demo", "hello", "us-central1", map[string]string{"eventType": "google.cloud.storage.object.v1.finalized", "bucket": "b1"})
	if err != nil || len(tr.GetEventFilters()) != 2 || tr.GetEventFilters()[1].GetValue() != "b1" {
		t.Errorf("Cloud Storage trigger = %v, %v", tr, err)
	}
	for _, bad := range []map[string]string{
		{"eventType": "google.cloud.storage.object.v1.finalized"},
		{"eventType": runPubSubEvent, "bucket": "b1"},
		{"eventType": "google.cloud.audit.log.v1.written"},
	} {
		if _, err := eventarcTrigger("demo", "hello", "us-central1", bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}
