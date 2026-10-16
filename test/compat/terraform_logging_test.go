//go:build compat

package compat

import (
	"fmt"
	"testing"

	vlogging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestTerraformLogging (#1246): a google_logging_metric (a distribution
// with a label and value extractor and explicit buckets), a
// google_logging_project_sink with an embedded exclusion, and a
// google_logging_project_exclusion apply with the official hashicorp/google
// provider through `cloudburrow terraform`, which sets
// logging_custom_endpoint to the JSON API on the logging port (#591). Each is
// read back through the official Go SDK's MetricsClient and ConfigClient,
// the second plan is empty, an in-place update of each lands, and after
// destroy each is NOT_FOUND. The exclusion drops entries only from what the
// instance stores. The sink's writer_identity is the project's Logging
// service agent (#1404); TestTerraformLoggingSinkDestinations grants it and
// sees entries delivered.
// covers: google.logging.v2.MetricsServiceV2/CreateLogMetric, google.logging.v2.MetricsServiceV2/GetLogMetric, google.logging.v2.MetricsServiceV2/UpdateLogMetric, google.logging.v2.MetricsServiceV2/DeleteLogMetric, google.logging.v2.ConfigServiceV2/CreateSink, google.logging.v2.ConfigServiceV2/GetSink, google.logging.v2.ConfigServiceV2/UpdateSink, google.logging.v2.ConfigServiceV2/DeleteSink, google.logging.v2.ConfigServiceV2/CreateExclusion, google.logging.v2.ConfigServiceV2/GetExclusion, google.logging.v2.ConfigServiceV2/UpdateExclusion, google.logging.v2.ConfigServiceV2/DeleteExclusion
func TestTerraformLogging(t *testing.T) {
	h := New(t)
	opts := loggingClientOptions(h) // skips without CLOUDBURROW_TEST_LOGGING
	m := newTFModule(t)
	ctx := h.Context()
	metrics, err := vlogging.NewMetricsClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer metrics.Close()
	config, err := vlogging.NewConfigClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer config.Close()

	project := h.Project()
	module := func(severity, description string) string {
		return fmt.Sprintf(`
resource "google_logging_metric" "latency" {
  project     = %[1]q
  name        = "tf-latency"
  description = %[3]q
  filter      = "resource.type=\"cloud_run_revision\" AND severity>=%[2]s"
  metric_descriptor {
    metric_kind = "DELTA"
    value_type  = "DISTRIBUTION"
    unit        = "ms"
    labels {
      key         = "status"
      value_type  = "STRING"
      description = "HTTP status"
    }
  }
  value_extractor = "EXTRACT(jsonPayload.latency_ms)"
  label_extractors = {
    "status" = "EXTRACT(jsonPayload.status)"
  }
  bucket_options {
    explicit_buckets {
      bounds = [10, 100, 1000]
    }
  }
}
resource "google_logging_project_sink" "s" {
  project                = %[1]q
  name                   = "tf-sink"
  description            = %[3]q
  destination            = "storage.googleapis.com/tf-logs-bucket"
  filter                 = "severity>=%[2]s"
  unique_writer_identity = true
  exclusions {
    name   = "no-gce-debug"
    filter = "resource.type=\"gce_instance\" AND severity=DEBUG"
  }
}
resource "google_logging_project_exclusion" "e" {
  project     = %[1]q
  name        = "tf-exclusion"
  description = %[3]q
  filter      = "severity<%[2]s"
}
`, project, severity, description)
	}

	parent := "projects/" + project
	check := func(when, severity, description string) {
		t.Helper()
		mt, err := metrics.GetLogMetric(ctx, &loggingpb.GetLogMetricRequest{MetricName: parent + "/metrics/tf-latency"})
		if err != nil {
			t.Fatalf("%s: GetLogMetric: %v", when, err)
		}
		wantFilter := fmt.Sprintf(`resource.type="cloud_run_revision" AND severity>=%s`, severity)
		if mt.GetFilter() != wantFilter || mt.GetDescription() != description ||
			mt.GetValueExtractor() != "EXTRACT(jsonPayload.latency_ms)" ||
			mt.GetLabelExtractors()["status"] != "EXTRACT(jsonPayload.status)" ||
			fmt.Sprint(mt.GetBucketOptions().GetExplicitBuckets().GetBounds()) != "[10 100 1000]" ||
			mt.GetMetricDescriptor().GetUnit() != "ms" || len(mt.GetMetricDescriptor().GetLabels()) != 1 {
			t.Errorf("%s: the metric is %v", when, mt)
		}
		sk, err := config.GetSink(ctx, &loggingpb.GetSinkRequest{SinkName: parent + "/sinks/tf-sink"})
		if err != nil {
			t.Fatalf("%s: GetSink: %v", when, err)
		}
		if sk.GetDestination() != "storage.googleapis.com/tf-logs-bucket" || sk.GetFilter() != "severity>="+severity ||
			sk.GetDescription() != description || len(sk.GetExclusions()) != 1 ||
			sk.GetExclusions()[0].GetName() != "no-gce-debug" {
			t.Errorf("%s: the sink is %v", when, sk)
		}
		ex, err := config.GetExclusion(ctx, &loggingpb.GetExclusionRequest{Name: parent + "/exclusions/tf-exclusion"})
		if err != nil {
			t.Fatalf("%s: GetExclusion: %v", when, err)
		}
		if ex.GetFilter() != "severity<"+severity || ex.GetDescription() != description || ex.GetDisabled() {
			t.Errorf("%s: the exclusion is %v", when, ex)
		}
	}

	m.write(module("ERROR", "first"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", "ERROR", "first")
	m.planClean()

	// An update of every resource in place: the metric's PUT, and the sink's
	// and exclusion's PATCH with an update mask.
	m.write(module("WARNING", "second"))
	m.apply()
	check("after the update", "WARNING", "second")
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := metrics.GetLogMetric(ctx, &loggingpb.GetLogMetricRequest{MetricName: parent + "/metrics/tf-latency"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetLogMetric after destroy: %v; want NotFound", err)
	}
	if _, err := config.GetSink(ctx, &loggingpb.GetSinkRequest{SinkName: parent + "/sinks/tf-sink"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetSink after destroy: %v; want NotFound", err)
	}
	if _, err := config.GetExclusion(ctx, &loggingpb.GetExclusionRequest{Name: parent + "/exclusions/tf-exclusion"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetExclusion after destroy: %v; want NotFound", err)
	}
}
