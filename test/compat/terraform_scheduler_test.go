//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"
	"time"

	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	schedulerpb "cloud.google.com/go/scheduler/apiv1/schedulerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// covers: google.cloud.scheduler.v1.CloudScheduler/CreateJob, google.cloud.scheduler.v1.CloudScheduler/GetJob, google.cloud.scheduler.v1.CloudScheduler/UpdateJob, google.cloud.scheduler.v1.CloudScheduler/PauseJob, google.cloud.scheduler.v1.CloudScheduler/ResumeJob, google.cloud.scheduler.v1.CloudScheduler/DeleteJob
//
// TestTerraformSchedulerAndSubscription (#591): a google_pubsub_topic, a
// google_pubsub_subscription on it and a google_cloud_scheduler_job that
// publishes to it apply through `cloudburrow terraform` with the official
// hashicorp/google provider, plan clean, take a change to the job (its
// description and schedule, a PATCH, and pausing it, a :pause), plan clean
// again, and destroy, all with egress blocked. The provider reaches Cloud
// Scheduler over its v1 JSON API, served by the shared transcoder on the
// scheduler's gRPC port, and Pub/Sub through the emulator's REST surface.
// Each step is read back with the official gRPC clients, and the event log
// shows the scheduler requests arrived as JSON.
//
// A change to the topic's and the subscription's labels applies in place
// (#949): the emulator refuses a labels mask ("labels is not a known
// Subscription field", and the same for a Topic), and CloudBurrow's front
// applies it instead. The provider sends its mask in the URL, camelCase,
// with bigqueryConfig in every subscription update
// (updateMask=ackDeadlineSeconds,labels,bigqueryConfig); since #928 the front
// reads it, leaves out the bigqueryConfig that sets nothing, keeps the
// labels, and sends the emulator the rest in the body, snake_case. The
// official client reads the new labels and ack deadline back, and the next
// plan is clean.
func TestTerraformSchedulerAndSubscription(t *testing.T) {
	testTerraformSchedulerAndSubscription(t, "terraform")
}

// TestTofuSchedulerAndSubscription is the same module under OpenTofu, which
// the wrapper runs with --binary tofu. It skips when neither EnvTofu nor
// tofu on PATH names a binary.
func TestTofuSchedulerAndSubscription(t *testing.T) {
	testTerraformSchedulerAndSubscription(t, tofuBinary(t))
}

func testTerraformSchedulerAndSubscription(t *testing.T, binary string) {
	h := New(t)
	m := newTFModuleWith(t, binary)
	sched := schedulerClient(t, h) // skips without CLOUDBURROW_TEST_SCHEDULER
	ps := pubsubClient(t, h)
	ctx := h.Context()
	control := h.Endpoint(EnvControl)
	project := h.Project()
	topic := "projects/" + project + "/topics/tf-sched"
	sub := "projects/" + project + "/subscriptions/tf-sub"
	job := "projects/" + project + "/locations/us-central1/jobs/tf-job"

	module := func(ack int, label, description, schedule string, paused bool) string {
		return fmt.Sprintf(`
resource "google_pubsub_topic" "t" {
  project = %[1]q
  name    = "tf-sched"
  labels  = { env = %[3]q }
}
resource "google_pubsub_subscription" "s" {
  project              = %[1]q
  name                 = "tf-sub"
  topic                = google_pubsub_topic.t.id
  ack_deadline_seconds = %[2]d
  labels               = { env = %[3]q }
}
resource "google_cloud_scheduler_job" "j" {
  project     = %[1]q
  region      = "us-central1"
  name        = "tf-job"
  description = %[4]q
  schedule    = %[5]q
  time_zone   = "Etc/UTC"
  paused      = %[6]t
  pubsub_target {
    topic_name = google_pubsub_topic.t.id
    data       = base64encode("tick")
    attributes = { source = "terraform" }
  }
}
`, project, ack, label, description, schedule, paused)
	}
	check := func(when string, ack int, label, description, schedule string, state schedulerpb.Job_State) {
		t.Helper()
		s, err := ps.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sub})
		if err != nil {
			t.Fatalf("%s, GetSubscription: %v", when, err)
		}
		if s.GetTopic() != topic || s.GetAckDeadlineSeconds() != int32(ack) || s.GetLabels()["env"] != label {
			t.Errorf("%s, the subscription = topic %q, ack %d, labels %v", when, s.GetTopic(), s.GetAckDeadlineSeconds(), s.GetLabels())
		}
		tp, err := ps.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: topic})
		if err != nil || tp.GetLabels()["env"] != label {
			t.Errorf("%s, the topic's labels = %v, %v; want env=%s", when, tp.GetLabels(), err, label)
		}
		j, err := sched.GetJob(ctx, &schedulerpb.GetJobRequest{Name: job})
		if err != nil {
			t.Fatalf("%s, GetJob: %v", when, err)
		}
		if p := j.GetPubsubTarget(); j.GetDescription() != description || j.GetSchedule() != schedule || j.GetState() != state ||
			p.GetTopicName() != topic || string(p.GetData()) != "tick" || p.GetAttributes()["source"] != "terraform" {
			t.Errorf("%s, the job = description %q, schedule %q, state %v, target %v", when, j.GetDescription(), j.GetSchedule(), j.GetState(), p)
		}
	}

	began := time.Now()
	m.write(module(20, "local", "every five minutes", "*/5 * * * *", false))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", 20, "local", "every five minutes", "*/5 * * * *", schedulerpb.Job_ENABLED)
	m.planClean()

	changes := time.Now()
	m.write(module(20, "local", "hourly", "0 * * * *", true))
	m.apply()
	check("after the change", 20, "local", "hourly", "0 * * * *", schedulerpb.Job_PAUSED)
	var sent []string
	for _, e := range events(t, h, control, "scheduler", changes) {
		if e.Detail["transport"] == "http" && !strings.HasPrefix(e.Target, "GET ") {
			sent = append(sent, e.Target)
		}
	}
	if want := []string{"PATCH /v1/" + job, "POST /v1/" + job + ":pause"}; !contains(sent, want[0]) || !contains(sent, want[1]) {
		t.Errorf("the change sent %q; want %q", sent, want)
	}
	m.planClean()

	// Labels and the ack deadline, changed in place (#949).
	m.write(module(30, "changed", "hourly", "0 * * * *", true))
	m.apply()
	check("after the labels change", 30, "changed", "hourly", "0 * * * *", schedulerpb.Job_PAUSED)
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := sched.GetJob(ctx, &schedulerpb.GetJobRequest{Name: job}); status.Code(err) != codes.NotFound {
		t.Errorf("GetJob after destroy: %v; want NotFound", err)
	}
	if _, err := ps.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sub}); status.Code(err) != codes.NotFound {
		t.Errorf("GetSubscription after destroy: %v; want NotFound", err)
	}
	if _, err := ps.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: topic}); status.Code(err) != codes.NotFound {
		t.Errorf("GetTopic after destroy: %v; want NotFound", err)
	}

	// Every scheduler request the provider made was JSON on the scheduler's
	// port: the create, the reads, the update, the pause and the delete.
	seen := map[string]bool{}
	for _, e := range events(t, h, control, "scheduler", began) {
		if e.Detail["transport"] != "http" {
			continue
		}
		verb, path, _ := strings.Cut(e.Target, " ")
		switch {
		case verb == "POST" && path == "/v1/projects/"+project+"/locations/us-central1/jobs":
			seen["create"] = true
		case verb == "GET" && path == "/v1/"+job:
			seen["read"] = true
		case verb == "DELETE" && path == "/v1/"+job:
			seen["delete"] = true
		}
	}
	for _, want := range []string{"create", "read", "delete"} {
		if !seen[want] {
			t.Errorf("the event log has no JSON request to %s the job", want)
		}
	}
}
