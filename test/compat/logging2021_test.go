//go:build compat

package compat

import (
	"fmt"
	"testing"
	"time"

	"cloud.google.com/go/logging"
	vlogging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/api/option"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestMonitoringSinkVolumeAndIngestion (#2021): a sink made with the
// official ConfigClient to a log bucket, and entries written with
// cloud.google.com/go/logging, are read back through the official
// Monitoring client as Google publishes the metrics: the logging_sink
// monitored resource (project_id, name, destination);
// logging.googleapis.com/exports/log_entry_count and exports/byte_count,
// DELTA on logging_sink, counting what the sink routed and nothing it did
// not match; billing/bytes_ingested, DELTA By on global by resource_type;
// and billing/monthly_bytes_ingested, a GAUGE of the month to date.
//
// covers: google.monitoring.v3.MetricService/ListTimeSeries, google.monitoring.v3.MetricService/GetMetricDescriptor, google.monitoring.v3.MetricService/GetMonitoredResourceDescriptor
func TestMonitoringSinkVolumeAndIngestion(t *testing.T) {
	h := New(t)
	c := monitoringClient(t, h)
	ctx := h.Context()
	lopts := []option.ClientOption{option.WithEndpoint(h.Endpoint(EnvLogging)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
	parent := "projects/" + h.Project()
	n := time.Now().UnixNano() % 1e6
	bucket, sink, log := fmt.Sprintf("vol-%d", n), fmt.Sprintf("vol-sink-%d", n), fmt.Sprintf("vol-log-%d", n)
	cc, err := vlogging.NewConfigClient(ctx, lopts...)
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	if _, err := cc.CreateBucket(ctx, &loggingpb.CreateBucketRequest{Parent: parent + "/locations/global", BucketId: bucket,
		Bucket: &loggingpb.LogBucket{RetentionDays: 30}}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	defer func() {
		_ = cc.DeleteBucket(ctx, &loggingpb.DeleteBucketRequest{Name: parent + "/locations/global/buckets/" + bucket})
	}()
	dest := "logging.googleapis.com/" + parent + "/locations/global/buckets/" + bucket
	if _, err := cc.CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: parent, Sink: &loggingpb.LogSink{Name: sink, Destination: dest,
		Filter: fmt.Sprintf(`logName = "%s/logs/%s" AND severity >= ERROR`, parent, log)}}); err != nil {
		t.Fatalf("CreateSink: %v", err)
	}
	defer func() { _ = cc.DeleteSink(ctx, &loggingpb.DeleteSinkRequest{SinkName: parent + "/sinks/" + sink}) }()

	rd, err := c.GetMonitoredResourceDescriptor(ctx, &monitoringpb.GetMonitoredResourceDescriptorRequest{Name: parent + "/monitoredResourceDescriptors/logging_sink"})
	if err != nil || len(rd.GetLabels()) != 3 || rd.GetLabels()[1].GetKey() != "name" || rd.GetLabels()[2].GetKey() != "destination" {
		t.Fatalf("the logging_sink resource = %v, %v; want project_id, name, destination", rd, err)
	}
	for metric, kind := range map[string]metricpb.MetricDescriptor_MetricKind{
		"exports/byte_count": metricpb.MetricDescriptor_DELTA, "exports/log_entry_count": metricpb.MetricDescriptor_DELTA,
		"exports/error_count": metricpb.MetricDescriptor_DELTA, "billing/bytes_ingested": metricpb.MetricDescriptor_DELTA,
		"billing/monthly_bytes_ingested": metricpb.MetricDescriptor_GAUGE,
	} {
		md, err := c.GetMetricDescriptor(ctx, &monitoringpb.GetMetricDescriptorRequest{Name: parent + "/metricDescriptors/logging.googleapis.com/" + metric})
		if err != nil || md.GetMetricKind() != kind || md.GetValueType() != metricpb.MetricDescriptor_INT64 {
			t.Errorf("%s = %v, %v; want %v INT64", metric, md, err, kind)
		}
	}

	lc, err := logging.NewClient(ctx, parent, lopts...)
	if err != nil {
		t.Fatal(err)
	}
	defer lc.Close()
	lg := lc.Logger(log, logging.CommonResource(&mrpb.MonitoredResource{Type: "global"}))
	for i, sev := range []logging.Severity{logging.Error, logging.Info, logging.Error, logging.Critical} {
		if err := lg.LogSync(ctx, logging.Entry{Severity: sev, Payload: fmt.Sprintf("volume %d", i)}); err != nil {
			t.Fatalf("LogSync: %v", err)
		}
	}

	end := time.Now().Add(2 * time.Minute)
	iv := &monitoringpb.TimeInterval{StartTime: timestamppb.New(end.Add(-15 * time.Minute)), EndTime: timestamppb.New(end)}
	sum := func(metric, filter string) (int64, []*monitoringpb.TimeSeries) {
		t.Helper()
		ts := listAll(t, c.ListTimeSeries(ctx, &monitoringpb.ListTimeSeriesRequest{Name: parent, Interval: iv,
			Filter: fmt.Sprintf(`metric.type = "logging.googleapis.com/%s"%s`, metric, filter)}))
		var n int64
		for _, s := range ts {
			for _, p := range s.GetPoints() {
				n += p.GetValue().GetInt64Value()
			}
		}
		return n, ts
	}
	mine := fmt.Sprintf(` AND resource.type = "logging_sink" AND resource.labels.name = %q`, sink)
	entries, ts := sum("exports/log_entry_count", mine)
	if entries != 3 || len(ts) != 1 || ts[0].GetResource().GetLabels()["destination"] != dest || ts[0].GetMetricKind() != metricpb.MetricDescriptor_DELTA {
		t.Errorf("exports/log_entry_count = %d over %v; want 3 entries on the sink", entries, ts)
	}
	if b, _ := sum("exports/byte_count", mine); b <= 0 {
		t.Errorf("exports/byte_count = %d; want the routed entries' bytes", b)
	}
	if e, _ := sum("exports/error_count", mine); e != 0 {
		t.Errorf("exports/error_count = %d; want 0", e)
	}
	ingested, ts := sum("billing/bytes_ingested", ` AND metric.labels.resource_type = "global"`)
	if ingested <= 0 || ts[0].GetResource().GetType() != "global" {
		t.Errorf("billing/bytes_ingested = %d over %v", ingested, ts)
	}
	monthly := listAll(t, c.ListTimeSeries(ctx, &monitoringpb.ListTimeSeriesRequest{Name: parent, Interval: iv,
		Filter: `metric.type = "logging.googleapis.com/billing/monthly_bytes_ingested"`,
		Aggregation: &monitoringpb.Aggregation{AlignmentPeriod: durationpb.New(time.Hour), PerSeriesAligner: monitoringpb.Aggregation_ALIGN_MAX,
			CrossSeriesReducer: monitoringpb.Aggregation_REDUCE_SUM}}))
	if len(monthly) != 1 || len(monthly[0].GetPoints()) == 0 || monthly[0].GetPoints()[0].GetValue().GetInt64Value() < ingested {
		t.Errorf("billing/monthly_bytes_ingested, max over the hour summed = %v; want at least %d", monthly, ingested)
	}
}
