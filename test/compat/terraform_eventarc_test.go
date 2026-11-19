//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	eventarc "cloud.google.com/go/eventarc/apiv1"
	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"
	"cloud.google.com/go/pubsub/v2"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestTerraformEventarcTrigger (#1877): a google_eventarc_trigger, with a
// messagePublished matching_criteria, a Cloud Run destination with a path
// and a transport topic, through `cloudburrow terraform` with egress
// blocked (it sets eventarc_custom_endpoint to the Run port's
// /eventarc/v1/). The official client reads it back as Terraform holds it;
// the plan is clean; new labels and a new path apply in place; the plan is
// clean again; destroy deletes it.
func TestTerraformEventarcTrigger(t *testing.T) {
	h := New(t)
	pubsubAddr := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_PUBSUB", EnvPubSub)
	rc := runClient(t, h)
	m := newTFModule(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	ec, err := eventarc.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ec.Close() })
	t.Setenv("PUBSUB_EMULATOR_HOST", pubsubAddr)
	pc, err := pubsub.NewClient(ctx, h.Project())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	tn := topic(t, h, pc, "tf-eventarc")
	parent := runParent(h)
	service := "tf-eventarc"
	sop, err := rc.CreateService(ctx, &runpb.CreateServiceRequest{Parent: parent, ServiceId: service, Service: &runpb.Service{
		Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{{Image: "ghcr.io/knative/helloworld-go:latest"}}}}})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	t.Cleanup(func() {
		dctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = rc.DeleteService(dctx, &runpb.DeleteServiceRequest{Name: parent + "/services/" + service})
	})
	if _, err := sop.Wait(ctx); err != nil {
		t.Fatalf("the service never became ready: %v", err)
	}
	name := parent + "/triggers/tf-trigger"
	t.Cleanup(func() {
		if op, err := ec.DeleteTrigger(context.Background(), &eventarcpb.DeleteTriggerRequest{Name: name, AllowMissing: true}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})
	module := func(team, path string) string {
		return fmt.Sprintf(`
resource "google_eventarc_trigger" "t" {
  project  = %[1]q
  location = "us-central1"
  name     = "tf-trigger"
  labels   = { team = %[2]q }
  matching_criteria {
    attribute = "type"
    value     = "google.cloud.pubsub.topic.v1.messagePublished"
  }
  destination {
    cloud_run_service {
      service = %[3]q
      region  = "us-central1"
      path    = %[4]q
    }
  }
  transport {
    pubsub {
      topic = %[5]q
    }
  }
}

output "read" {
  value = "${google_eventarc_trigger.t.uid}|${google_eventarc_trigger.t.transport[0].pubsub[0].subscription}"
}
`, h.Project(), team, service, path, tn)
	}
	check := func(when, team, path string) {
		t.Helper()
		tr, err := ec.GetTrigger(ctx, &eventarcpb.GetTriggerRequest{Name: name})
		if err != nil || tr.GetLabels()["team"] != team || tr.GetDestination().GetCloudRun().GetPath() != path ||
			tr.GetTransport().GetPubsub().GetTopic() != tn {
			t.Fatalf("%s, the trigger = %v, %v; want team=%s path=%s", when, tr, err, team, path)
		}
		m.must("refresh", "-input=false", "-no-color")
		var got string
		if err := json.Unmarshal([]byte(m.must("output", "-json", "read")), &got); err != nil {
			t.Fatalf("%s, the outputs: %v", when, err)
		}
		if want := tr.GetUid() + "|" + tr.GetTransport().GetPubsub().GetSubscription(); got != want {
			t.Errorf("%s, Terraform reads %q; the official client %q", when, got, want)
		}
	}
	m.write(module("events", "/a"))
	m.must("init", "-input=false", "-no-color")
	if out := m.must("apply", "-auto-approve", "-input=false", "-no-color"); !strings.Contains(out, "1 added, 0 changed, 0 destroyed") {
		t.Errorf("first apply:\n%s", out)
	}
	check("after create", "events", "/a")
	m.planClean()
	m.write(module("platform", "/b"))
	if out := m.must("apply", "-auto-approve", "-input=false", "-no-color"); !strings.Contains(out, "0 added, 1 changed, 0 destroyed") {
		t.Errorf("second apply:\n%s", out)
	}
	check("after update", "platform", "/b")
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := ec.GetTrigger(ctx, &eventarcpb.GetTriggerRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetTrigger after destroy: %v; want NotFound", err)
	}
}
