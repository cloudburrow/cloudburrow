//go:build compat

package compat

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	eventarc "cloud.google.com/go/eventarc/apiv1"
	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"
	"cloud.google.com/go/pubsub/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestTerraformEventarcAdvanced (#1947): google_eventarc_message_bus (with
// a logging_config), google_eventarc_pipeline (a Pub/Sub topic
// destination, a JSON input format, a transformation and a retry policy),
// google_eventarc_enrollment (a CEL match) and
// google_eventarc_google_api_source, through `cloudburrow terraform` with
// egress blocked (eventarc_custom_endpoint is the Run port's
// /eventarc/v1/). The official client reads them back; the plan is clean;
// a new CEL match, labels and log severity apply in place; the plan is
// clean again; destroy deletes them.
func TestTerraformEventarcAdvanced(t *testing.T) {
	h := New(t)
	pubsubAddr := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_PUBSUB", EnvPubSub)
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
	tn := topic(t, h, pc, "tf-eventarc-adv")
	parent := runParent(h)
	module := func(match, team, severity string) string {
		return fmt.Sprintf(`
resource "google_eventarc_message_bus" "bus" {
  project        = %[1]q
  location       = "us-central1"
  message_bus_id = "tf-bus"
  display_name   = "orders"
  labels         = { team = %[3]q }
  logging_config {
    log_severity = %[4]q
  }
}

resource "google_eventarc_pipeline" "pipe" {
  project     = %[1]q
  location    = "us-central1"
  pipeline_id = "tf-pipe"
  destinations {
    topic = %[5]q
  }
  input_payload_format {
    json {}
  }
  mediations {
    transformation {
      transformation_template = "message.setField(\"data.via\", \"terraform\")"
    }
  }
  retry_policy {
    max_attempts = 4
  }
}

resource "google_eventarc_enrollment" "enr" {
  project       = %[1]q
  location      = "us-central1"
  enrollment_id = "tf-enr"
  message_bus   = google_eventarc_message_bus.bus.id
  destination   = google_eventarc_pipeline.pipe.id
  cel_match     = %[2]q
}

resource "google_eventarc_google_api_source" "src" {
  project              = %[1]q
  location             = "us-central1"
  google_api_source_id = "tf-src"
  destination          = google_eventarc_message_bus.bus.id
}
`, h.Project(), match, team, severity, tn)
	}
	t.Cleanup(func() {
		for _, del := range []func() error{
			func() error {
				_, err := ec.DeleteEnrollment(context.Background(), &eventarcpb.DeleteEnrollmentRequest{Name: parent + "/enrollments/tf-enr", AllowMissing: true})
				return err
			},
			func() error {
				_, err := ec.DeleteGoogleApiSource(context.Background(), &eventarcpb.DeleteGoogleApiSourceRequest{Name: parent + "/googleApiSources/tf-src", AllowMissing: true})
				return err
			},
			func() error {
				_, err := ec.DeletePipeline(context.Background(), &eventarcpb.DeletePipelineRequest{Name: parent + "/pipelines/tf-pipe", AllowMissing: true})
				return err
			},
			func() error {
				_, err := ec.DeleteMessageBus(context.Background(), &eventarcpb.DeleteMessageBusRequest{Name: parent + "/messageBuses/tf-bus", AllowMissing: true})
				return err
			},
		} {
			_ = del()
		}
	})
	check := func(when, match, team string, sev eventarcpb.LoggingConfig_LogSeverity) {
		t.Helper()
		b, err := ec.GetMessageBus(ctx, &eventarcpb.GetMessageBusRequest{Name: parent + "/messageBuses/tf-bus"})
		if err != nil || b.GetLabels()["team"] != team || b.GetLoggingConfig().GetLogSeverity() != sev || b.GetDisplayName() != "orders" {
			t.Fatalf("%s, the bus = %v, %v", when, b, err)
		}
		p, err := ec.GetPipeline(ctx, &eventarcpb.GetPipelineRequest{Name: parent + "/pipelines/tf-pipe"})
		if err != nil || p.GetDestinations()[0].GetTopic() != tn || p.GetRetryPolicy().GetMaxAttempts() != 4 ||
			!strings.Contains(p.GetMediations()[0].GetTransformation().GetTransformationTemplate(), "terraform") {
			t.Fatalf("%s, the pipeline = %v, %v", when, p, err)
		}
		e, err := ec.GetEnrollment(ctx, &eventarcpb.GetEnrollmentRequest{Name: parent + "/enrollments/tf-enr"})
		if err != nil || e.GetCelMatch() != match || e.GetMessageBus() != b.GetName() || e.GetDestination() != p.GetName() {
			t.Fatalf("%s, the enrollment = %v, %v", when, e, err)
		}
		s, err := ec.GetGoogleApiSource(ctx, &eventarcpb.GetGoogleApiSourceRequest{Name: parent + "/googleApiSources/tf-src"})
		if err != nil || s.GetDestination() != b.GetName() {
			t.Fatalf("%s, the Google API source = %v, %v", when, s, err)
		}
	}
	m.write(module(`message.type == "a"`, "events", "INFO"))
	m.must("init", "-input=false", "-no-color")
	if out := m.must("apply", "-auto-approve", "-input=false", "-no-color"); !strings.Contains(out, "4 added, 0 changed, 0 destroyed") {
		t.Errorf("first apply:\n%s", out)
	}
	check("after create", `message.type == "a"`, "events", eventarcpb.LoggingConfig_INFO)
	m.planClean()
	m.write(module(`message.type == "b"`, "platform", "DEBUG"))
	if out := m.must("apply", "-auto-approve", "-input=false", "-no-color"); !strings.Contains(out, "0 added, 2 changed, 0 destroyed") {
		t.Errorf("second apply:\n%s", out)
	}
	check("after update", `message.type == "b"`, "platform", eventarcpb.LoggingConfig_DEBUG)
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := ec.GetMessageBus(ctx, &eventarcpb.GetMessageBusRequest{Name: parent + "/messageBuses/tf-bus"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetMessageBus after destroy: %v; want NotFound", err)
	}
}
