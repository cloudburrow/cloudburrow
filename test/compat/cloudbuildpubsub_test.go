//go:build compat

package compat

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// covers: google.devtools.cloudbuild.v1.CloudBuild/CreateBuildTrigger, google.devtools.cloudbuild.v1.CloudBuild/UpdateBuildTrigger, google.devtools.cloudbuild.v1.CloudBuild/DeleteBuildTrigger
//
// TestCloudBuildPubsubTrigger (#1506), with the official Cloud Build and
// Pub/Sub clients, on an instance with --services run,storage,pubsub: a
// trigger with pubsubConfig.topic is given a subscription on the topic
// (pubsubConfig.subscription, state OK), and a message published to the
// topic runs its build, whose substitutions bind $(body.message.data.tag)
// (the JSON in the message's data) and $(body.message.attributes.kind). An
// update to another topic moves the subscription; a delete removes it; a
// malformed topic is INVALID_ARGUMENT.
func TestCloudBuildPubsubTrigger(t *testing.T) {
	h := New(t)
	ps := pubsubClient(t, h)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required: builds run with the host's Docker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	t.Cleanup(cancel)
	cb := cloudBuildClient(ctx, t, h)
	parent := runParent(h)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	topic1 := topic(t, h, ps, "cb-trigger-"+suffix)
	topic2 := topic(t, h, ps, "cb-trigger2-"+suffix)
	bucket := uploadSource(ctx, t, h, "source.tar.gz", sourceBuildArchive(ctx, t))
	image := "us-central1-docker.pkg.dev/$PROJECT_ID/cb-repo/pubsub:${_TAG}-${_KIND}"
	want := "us-central1-docker.pkg.dev/" + h.Project() + "/cb-repo/pubsub:v7-release"

	if _, err := cb.CreateBuildTrigger(ctx, &cbpb.CreateBuildTriggerRequest{Parent: parent, ProjectId: h.Project(), Trigger: &cbpb.BuildTrigger{
		Name: "bad-" + suffix, PubsubConfig: &cbpb.PubsubConfig{Topic: "not-a-topic"},
		BuildTemplate: &cbpb.BuildTrigger_Build{Build: &cbpb.Build{Steps: []*cbpb.BuildStep{{Name: "gcr.io/cloud-builders/docker", Args: []string{"version"}}}}}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a malformed topic: %v; want InvalidArgument", err)
	}
	tr, err := cb.CreateBuildTrigger(ctx, &cbpb.CreateBuildTriggerRequest{Parent: parent, ProjectId: h.Project(), Trigger: &cbpb.BuildTrigger{
		Name: "ps-" + suffix, PubsubConfig: &cbpb.PubsubConfig{Topic: topic1},
		Substitutions: map[string]string{"_TAG": "$(body.message.data.tag)", "_KIND": "$(body.message.attributes.kind)"},
		BuildTemplate: &cbpb.BuildTrigger_Build{Build: &cbpb.Build{
			Source: &cbpb.Source{Source: &cbpb.Source_StorageSource{StorageSource: &cbpb.StorageSource{Bucket: bucket, Object: "source.tar.gz"}}},
			Steps:  []*cbpb.BuildStep{{Name: "gcr.io/cloud-builders/docker", Args: []string{"build", "--network", "cloudbuild", "-t", image, "."}}}}}}})
	if err != nil {
		t.Fatalf("CreateBuildTrigger: %v", err)
	}
	t.Cleanup(func() {
		_ = cb.DeleteBuildTrigger(context.Background(), &cbpb.DeleteBuildTriggerRequest{Name: tr.GetResourceName(), ProjectId: h.Project(), TriggerId: tr.GetId()})
	})
	sub := tr.GetPubsubConfig().GetSubscription()
	if !strings.HasPrefix(sub, "projects/"+h.Project()+"/subscriptions/") || tr.GetPubsubConfig().GetState() != cbpb.PubsubConfig_OK {
		t.Fatalf("pubsubConfig = %v", tr.GetPubsubConfig())
	}
	if s, err := ps.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sub}); err != nil || s.GetTopic() != topic1 {
		t.Fatalf("the trigger's subscription = %v, %v", s, err)
	}

	if _, err := ps.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: topic1, Messages: []*pubsubpb.PubsubMessage{{
		Data: []byte(`{"tag":"v7","action":"INSERT"}`), Attributes: map[string]string{"kind": "release"}}}}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	b := triggeredBuild(ctx, t, cb, parent, tr.GetId(), 1)
	if b.GetStatus() != cbpb.Build_SUCCESS || b.GetSteps()[0].GetArgs()[4] != want {
		t.Fatalf("the message's build = %v", b)
	}

	// Another topic moves the subscription; a delete removes it.
	next, err := cb.UpdateBuildTrigger(ctx, &cbpb.UpdateBuildTriggerRequest{ProjectId: h.Project(), TriggerId: tr.GetId(),
		Trigger:    &cbpb.BuildTrigger{ResourceName: tr.GetResourceName(), PubsubConfig: &cbpb.PubsubConfig{Topic: topic2}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"pubsub_config"}}})
	if err != nil {
		t.Fatalf("UpdateBuildTrigger: %v", err)
	}
	if s, err := ps.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: next.GetPubsubConfig().GetSubscription()}); err != nil || s.GetTopic() != topic2 {
		t.Errorf("after the update, the subscription = %v, %v", s, err)
	}
	if err := cb.DeleteBuildTrigger(ctx, &cbpb.DeleteBuildTriggerRequest{Name: tr.GetResourceName(), ProjectId: h.Project(), TriggerId: tr.GetId()}); err != nil {
		t.Fatalf("DeleteBuildTrigger: %v", err)
	}
	if _, err := ps.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sub}); status.Code(err) != codes.NotFound {
		t.Errorf("the subscription after the delete: %v; want NotFound", err)
	}
}

// covers: google.devtools.cloudbuild.v1.CloudBuild/CreateBuildTrigger, google.devtools.cloudbuild.v1.CloudBuild/GetBuildTrigger, google.devtools.cloudbuild.v1.CloudBuild/DeleteBuildTrigger
//
// TestTerraformCloudBuildPubsubTrigger (#1506): a google_cloudbuild_trigger
// with pubsub_config and an inline build applies through `cloudburrow
// terraform` with egress blocked; the official clients read back its
// subscription on the topic; the plan is clean; destroy removes the
// trigger and its subscription.
func TestTerraformCloudBuildPubsubTrigger(t *testing.T) {
	h := New(t)
	ps := pubsubClient(t, h)
	m := newTFModule(t)
	_ = runClient(t, h) // skips without CLOUDBURROW_TEST_RUN
	ctx := h.Context()
	cb := cloudBuildClient(ctx, t, h)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	tp := topic(t, h, ps, "tf-cb-"+suffix)
	m.write(fmt.Sprintf(`
resource "google_cloudbuild_trigger" "ps" {
  project  = %[1]q
  location = "us-central1"
  name     = "tf-ps-%[2]s"
  pubsub_config {
    topic = %[3]q
  }
  substitutions = { _TAG = "$(body.message.data.tag)" }
  build {
    step {
      name = "gcr.io/cloud-builders/docker"
      args = ["version"]
    }
  }
}
`, h.Project(), suffix, tp))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	var tr *cbpb.BuildTrigger
	for it := cb.ListBuildTriggers(ctx, &cbpb.ListBuildTriggersRequest{Parent: runParent(h), ProjectId: h.Project()}); ; {
		x, err := it.Next()
		if err != nil {
			break
		}
		if x.GetName() == "tf-ps-"+suffix {
			tr = x
		}
	}
	if tr == nil || tr.GetPubsubConfig().GetTopic() != tp || tr.GetPubsubConfig().GetSubscription() == "" {
		t.Fatalf("the trigger = %v", tr)
	}
	sub := tr.GetPubsubConfig().GetSubscription()
	if s, err := ps.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sub}); err != nil || s.GetTopic() != tp {
		t.Errorf("the trigger's subscription = %v, %v", s, err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := ps.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sub}); status.Code(err) != codes.NotFound {
		t.Errorf("the subscription after destroy: %v; want NotFound", err)
	}
}
