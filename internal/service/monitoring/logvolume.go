package monitoring

// Cloud Logging's sink and ingestion metrics (#2021), as Google's metrics
// list publishes them (docs.cloud.google.com/monitoring/api/metrics_gcp_i_o,
// read 2026-11-20):
//
//   - logging.googleapis.com/exports/byte_count, exports/log_entry_count
//     and exports/error_count: DELTA INT64 on the logging_sink resource
//     (project_id, name, destination), no metric labels;
//   - logging.googleapis.com/billing/bytes_ingested: DELTA INT64 By on
//     global, by resource_type;
//   - logging.googleapis.com/billing/monthly_bytes_ingested: GAUGE INT64 By
//     on global, by resource_type, the month-to-date total, which Google
//     samples every 1800 seconds: a point at each 30-minute boundary, and
//     one at each minute the total grew, so a new total is seen at once.
//
// The counts are Cloud Logging's own (internal/service/logging/volume.go),
// by the Logging clock, in one-minute intervals.

import (
	"sort"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	labelpb "google.golang.org/genproto/googleapis/api/label"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// SinkVolume is what one sink routed in one minute.
type SinkVolume struct {
	// End is the minute's end, (End-1m, End].
	End                    time.Time
	Sink, Destination      string // the sink's ID, and its destination
	Bytes, Entries, Errors int64
}

// Ingestion is what a project's log buckets took in one minute, for one
// monitored resource type.
type Ingestion struct {
	End          time.Time
	ResourceType string
	Bytes        int64
}

// LogVolumeSource is a LogSource that also counts what sinks route and log
// buckets take.
type LogVolumeSource interface {
	// SinkVolumes are what project's sinks routed in the minutes ending in
	// (from, to].
	SinkVolumes(project string, from, to time.Time) []SinkVolume
	// Ingested is what project's log buckets took in the minutes ending in
	// (from, to].
	Ingested(project string, from, to time.Time) []Ingestion
}

const (
	logExportBytes   = "logging.googleapis.com/exports/byte_count"
	logExportEntries = "logging.googleapis.com/exports/log_entry_count"
	logExportErrors  = "logging.googleapis.com/exports/error_count"
	logBytesIngested = "logging.googleapis.com/billing/bytes_ingested"
	logMonthlyBytes  = "logging.googleapis.com/billing/monthly_bytes_ingested"
	monthlySample    = 30 * time.Minute
)

func volumeMetric(t string) bool {
	switch t {
	case logExportBytes, logExportEntries, logExportErrors, logBytesIngested, logMonthlyBytes:
		return true
	}
	return false
}

func volumeDescriptors(src LogSource, project string) []*metricpb.MetricDescriptor {
	if _, ok := src.(LogVolumeSource); !ok {
		return nil
	}
	sink := []string{"logging_sink"}
	rtype := []*labelpb.LabelDescriptor{{Key: "resource_type", ValueType: labelpb.LabelDescriptor_STRING,
		Description: "The monitored resource type of the log entries."}}
	return []*metricpb.MetricDescriptor{
		{Name: descriptorName(project, logExportBytes), Type: logExportBytes, MetricKind: delta, ValueType: tInt, Unit: "By",
			DisplayName: "Exported log bytes", MonitoredResourceTypes: sink,
			Description: "Number of bytes in log entries that were exported. Sampled every 60 seconds."},
		{Name: descriptorName(project, logExportErrors), Type: logExportErrors, MetricKind: delta, ValueType: tInt, Unit: "1",
			DisplayName: "Exported log entries failures", MonitoredResourceTypes: sink,
			Description: "Number of log entries that failed to be exported. Sampled every 60 seconds."},
		{Name: descriptorName(project, logExportEntries), Type: logExportEntries, MetricKind: delta, ValueType: tInt, Unit: "1",
			DisplayName: "Exported log entries", MonitoredResourceTypes: sink,
			Description: "Number of log entries that were exported. Sampled every 60 seconds."},
		{Name: descriptorName(project, logBytesIngested), Type: logBytesIngested, MetricKind: delta, ValueType: tInt, Unit: "By",
			DisplayName: "Bytes streamed into log buckets", MonitoredResourceTypes: []string{"global"}, Labels: rtype,
			Description: "Number of bytes streamed into log buckets for indexing, querying, and analysis. Sampled every 60 seconds."},
		{Name: descriptorName(project, logMonthlyBytes), Type: logMonthlyBytes, MetricKind: gauge, ValueType: tInt, Unit: "By",
			DisplayName: "Monthly bytes streamed into log buckets", MonitoredResourceTypes: []string{"global"}, Labels: rtype,
			Description: "Month-to-date bytes streamed into log buckets. Sampled every 1800 seconds."},
	}
}

func int64Value(n int64) *monitoringpb.TypedValue {
	return &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: n}}
}

func volumeSeries(src LogVolumeSource, project, metricType string, start, end time.Time) []*monitoringpb.TimeSeries {
	from := start.Add(-logPointPeriod)
	groups := map[string]*monitoringpb.TimeSeries{}
	counts := map[string]map[time.Time]int64{}
	add := func(ts *monitoringpb.TimeSeries, t time.Time, n int64) {
		k := seriesKey(ts)
		if groups[k] == nil {
			groups[k], counts[k] = ts, map[time.Time]int64{}
		}
		counts[k][t] += n
	}
	switch metricType {
	case logExportBytes, logExportEntries, logExportErrors:
		unit := "1"
		if metricType == logExportBytes {
			unit = "By"
		}
		for _, v := range src.SinkVolumes(project, from, end) {
			n := v.Entries
			switch metricType {
			case logExportBytes:
				n = v.Bytes
			case logExportErrors:
				n = v.Errors
			}
			if n == 0 {
				continue
			}
			ts := &monitoringpb.TimeSeries{Metric: &metricpb.Metric{Type: metricType, Labels: map[string]string{}},
				Resource: &monitoredres.MonitoredResource{Type: "logging_sink",
					Labels: map[string]string{"project_id": project, "name": v.Sink, "destination": v.Destination}},
				MetricKind: delta, ValueType: tInt, Unit: unit}
			add(ts, v.End, n)
		}
		return countSeries(groups, counts)
	case logBytesIngested:
		for _, v := range src.Ingested(project, from, end) {
			add(ingestSeries(metricType, project, v.ResourceType, delta), v.End, v.Bytes)
		}
		return countSeries(groups, counts)
	}
	// The monthly gauge: the month-to-date total at each sample.
	monthStart := time.Date(start.UTC().Year(), start.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	ins := src.Ingested(project, monthStart.Add(-time.Nanosecond), end)
	byType := map[string][]Ingestion{}
	for _, v := range ins {
		byType[v.ResourceType] = append(byType[v.ResourceType], v)
	}
	var out []*monitoringpb.TimeSeries
	for rt, vs := range byType {
		sort.Slice(vs, func(i, j int) bool { return vs[i].End.Before(vs[j].End) })
		samples := map[time.Time]bool{}
		for t := start.Truncate(monthlySample); !t.After(end); t = t.Add(monthlySample) {
			if !t.Before(start) {
				samples[t] = true
			}
		}
		for _, v := range vs {
			if !v.End.Before(start) && !v.End.After(end) {
				samples[v.End] = true
			}
		}
		ts := ingestSeries(metricType, project, rt, gauge)
		for _, t := range sortedTimes(samples) {
			// The month the sample falls in, from its first instant.
			ms := time.Date(t.UTC().Year(), t.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
			var total int64
			for _, v := range vs {
				if v.End.After(ms) && !v.End.After(t) {
					total += v.Bytes
				}
			}
			if total == 0 {
				continue
			}
			ts.Points = append(ts.Points, &monitoringpb.Point{Interval: &monitoringpb.TimeInterval{EndTime: timestamppb.New(t)}, Value: int64Value(total)})
		}
		if len(ts.Points) > 0 {
			out = append(out, ts)
		}
	}
	return out
}

func ingestSeries(metricType, project, rtype string, kind metricpb.MetricDescriptor_MetricKind) *monitoringpb.TimeSeries {
	return &monitoringpb.TimeSeries{Metric: &metricpb.Metric{Type: metricType, Labels: map[string]string{"resource_type": rtype}},
		Resource:   &monitoredres.MonitoredResource{Type: "global", Labels: map[string]string{"project_id": project}},
		MetricKind: kind, ValueType: tInt, Unit: "By"}
}
