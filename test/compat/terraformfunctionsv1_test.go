//go:build compat

package compat

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fv1 "cloud.google.com/go/functions/apiv1/functionspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// covers: google.cloud.functions.v1.CloudFunctionsService/CreateFunction, google.cloud.functions.v1.CloudFunctionsService/GetFunction, google.cloud.functions.v1.CloudFunctionsService/UpdateFunction, google.cloud.functions.v1.CloudFunctionsService/DeleteFunction
//
// TestTerraformCloudFunctionsFunction (#1428): google_cloudfunctions_function
// through `cloudburrow terraform`, which points cloud_functions_custom_endpoint
// at the Run port, egress to Google blocked; one function with an HTTP
// trigger and one with a Pub/Sub event trigger. Ungated: from a source
// object that is not a zip, both fail to apply with this instance's build
// error, the v1 client reads each OFFLINE with the trigger Terraform sent,
// and destroy removes them. Gated as the Functions tests are:
// testdata/function, zipped into a bucket object, applies both ACTIVE,
// plans clean, takes an environment change in place, plans clean again
// and destroys.
func TestTerraformCloudFunctionsFunction(t *testing.T) {
	h := New(t)
	m := newTFModule(t)
	runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_PUBSUB", EnvPubSub)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	t.Cleanup(cancel)
	c := functionsV1Client(t, h)
	suffix := strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(m.project, "cb-test-"), "_", "-"))
	bucketName := "tf-fn1-src-" + suffix
	topic := "tf-fn1-events-" + suffix
	tn := "projects/" + m.project + "/topics/" + topic
	zipPath := filepath.Join(t.TempDir(), "function.zip")
	module := func(suffix, archive, mode string) string {
		if err := os.WriteFile(zipPath, []byte(archive), 0o644); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf(`
resource "google_storage_bucket" "src" {
  project       = %[1]q
  name          = %[2]q
  location      = "US"
  force_destroy = true
}
resource "google_storage_bucket_object" "zip" {
  name   = "function.zip"
  bucket = google_storage_bucket.src.name
  source = %[3]q
}
resource "google_pubsub_topic" "events" {
  project = %[1]q
  name    = %[4]q
}
resource "google_cloudfunctions_function" "http" {
  project               = %[1]q
  region                = "us-central1"
  name                  = "tf-v1-http%[5]s"
  runtime               = "go123"
  entry_point           = "Hello"
  available_memory_mb   = 256
  timeout               = 60
  source_archive_bucket = google_storage_bucket.src.name
  source_archive_object = google_storage_bucket_object.zip.name
  trigger_http          = true
  environment_variables = { MODE = %[6]q }
}
resource "google_cloudfunctions_function" "event" {
  project               = %[1]q
  region                = "us-central1"
  name                  = "tf-v1-event%[5]s"
  runtime               = "go123"
  entry_point           = "Hello"
  available_memory_mb   = 256
  source_archive_bucket = google_storage_bucket.src.name
  source_archive_object = google_storage_bucket_object.zip.name
  event_trigger {
    event_type = "google.pubsub.topic.publish"
    resource   = google_pubsub_topic.events.id
  }
}
`, m.project, bucketName, zipPath, topic, suffix, mode)
	}
	fnName := func(id string) string { return "projects/" + m.project + "/locations/us-central1/functions/" + id }

	m.write(module("-bad", "not a zip", "bad"))
	m.must("init", "-input=false", "-no-color")
	out, err := m.tf("apply", "-auto-approve", "-input=false", "-no-color")
	if err == nil || strings.Count(out, "Build failed") < 2 {
		t.Errorf("apply of a source that is not a zip = %v; want this instance's build error for both functions:\n%s", err, lastLines(out, 40))
	}
	src := "gs://" + bucketName + "/function.zip"
	if fn, err := c.GetFunction(ctx, &fv1.GetFunctionRequest{Name: fnName("tf-v1-http-bad")}); err != nil ||
		fn.GetStatus() != fv1.CloudFunctionStatus_OFFLINE || fn.GetHttpsTrigger() == nil || fn.GetSourceArchiveUrl() != src ||
		fn.GetRuntime() != "go123" || fn.GetEnvironmentVariables()["MODE"] != "bad" {
		t.Errorf("v1 GetFunction of the HTTP function Terraform created = %v, %v; want it OFFLINE with its httpsTrigger and source", fn, err)
	}
	if fn, err := c.GetFunction(ctx, &fv1.GetFunctionRequest{Name: fnName("tf-v1-event-bad")}); err != nil ||
		fn.GetStatus() != fv1.CloudFunctionStatus_OFFLINE || fn.GetEventTrigger().GetEventType() != "google.pubsub.topic.publish" ||
		fn.GetEventTrigger().GetResource() != tn {
		t.Errorf("v1 GetFunction of the event function Terraform created = %v, %v; want it OFFLINE with its topic", fn, err)
	}
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	for _, id := range []string{"tf-v1-http-bad", "tf-v1-event-bad"} {
		if _, err := c.GetFunction(ctx, &fv1.GetFunctionRequest{Name: fnName(id)}); status.Code(err) != codes.NotFound {
			t.Errorf("the failed function %s after destroy = %v; want NOT_FOUND", id, err)
		}
	}

	requireAmd64Functions(ctx, t)
	archive := string(zipFunction(t, filepath.Join(moduleRoot(t), "testdata", "function")))
	m.write(module("", archive, "first"))
	m.apply()
	for _, id := range []string{"tf-v1-http", "tf-v1-event"} {
		if fn, err := c.GetFunction(ctx, &fv1.GetFunctionRequest{Name: fnName(id)}); err != nil || fn.GetStatus() != fv1.CloudFunctionStatus_ACTIVE {
			t.Fatalf("after apply, %s = %v, %v; want it ACTIVE", id, fn, err)
		}
	}
	m.planClean()
	m.write(module("", archive, "second"))
	if out := m.must("apply", "-auto-approve", "-input=false", "-no-color"); !strings.Contains(out, "0 added, 1 changed, 0 destroyed") {
		t.Errorf("the change was not applied in place:\n%s", lastLines(out, 20))
	}
	if fn, err := c.GetFunction(ctx, &fv1.GetFunctionRequest{Name: fnName("tf-v1-http")}); err != nil ||
		fn.GetEnvironmentVariables()["MODE"] != "second" || fn.GetStatus() != fv1.CloudFunctionStatus_ACTIVE {
		t.Errorf("after the change, the function = %v, %v", fn, err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	for _, id := range []string{"tf-v1-http", "tf-v1-event"} {
		if _, err := c.GetFunction(ctx, &fv1.GetFunctionRequest{Name: fnName(id)}); status.Code(err) != codes.NotFound {
			t.Errorf("GetFunction of %s after destroy = %v; want NOT_FOUND", id, err)
		}
	}
}
