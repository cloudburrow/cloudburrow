package monitoring

// Firestore's document metrics (#2048), from the counts the Firestore
// front keeps of the calls through it (internal/firestoreadmin, usage.go).
// Types, kinds, units and labels as Google's metrics list publishes them
// (docs.cloud.google.com/monitoring/api/metrics_gcp_d_h, "firestore",
// read 2026-11-20): the firestore.googleapis.com/Database resource's
// document/read_ops_count (type LOOKUP or QUERY), write_ops_count (op
// CREATE or UPDATE) and delete_ops_count, the older firestore_instance
// resource's read_count, write_count and delete_count (with module and
// version), and the query_stat/per_query distributions of results and
// documents scanned. Points are one-minute DELTA intervals.

import (
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/genproto/googleapis/api/distribution"
	labelpb "google.golang.org/genproto/googleapis/api/label"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/genproto/googleapis/api/monitoredres"
)

// FirestoreMinute is one database's counts in the minute that starts at
// Start.
type FirestoreMinute struct {
	Start             time.Time
	Project, Database string // the project ID and database ID
	Location          string
	Reads, Writes     map[string]int64
	Deletes           int64
	Queries           []FirestoreQueryMinute
}

// FirestoreQueryMinute is one method's query executions in a minute: the
// results and documents scanned of each.
type FirestoreQueryMinute struct {
	Method string
	// Results and Scanned are each execution's results and documents
	// scanned.
	Results, Scanned []float64
}

// FirestoreSource is the instance's Firestore front.
type FirestoreSource interface {
	// FirestoreMinutes are the minutes of project's databases that start
	// in [from, to].
	FirestoreMinutes(project string, from, to time.Time) ([]FirestoreMinute, error)
}

type firestoreBuiltin struct{ src FirestoreSource }

// NewFirestoreBuiltin is the built-in metrics of the instance's Firestore.
func NewFirestoreBuiltin(src FirestoreSource) Builtin { return firestoreBuiltin{src} }

const (
	fsReadOps    = "firestore.googleapis.com/document/read_ops_count"
	fsWriteOps   = "firestore.googleapis.com/document/write_ops_count"
	fsDeleteOps  = "firestore.googleapis.com/document/delete_ops_count"
	fsRead       = "firestore.googleapis.com/document/read_count"
	fsWrite      = "firestore.googleapis.com/document/write_count"
	fsDelete     = "firestore.googleapis.com/document/delete_count"
	fsResults    = "firestore.googleapis.com/query_stat/per_query/result_counts"
	fsScanned    = "firestore.googleapis.com/query_stat/per_query/scanned_documents_counts"
	fsDatabase   = "firestore.googleapis.com/Database"
	fsInstance   = "firestore_instance"
	fsSampleNote = " Sampled every 60 seconds. After sampling, data is not visible for up to 240 seconds."
)

func (b firestoreBuiltin) Descriptors(project string) []*metricpb.MetricDescriptor {
	str := func(k, d string) *labelpb.LabelDescriptor {
		return &labelpb.LabelDescriptor{Key: k, ValueType: labelpb.LabelDescriptor_STRING, Description: d}
	}
	mv := []*labelpb.LabelDescriptor{str("module", "The service/module name."), str("version", "The version name.")}
	sm := []*labelpb.LabelDescriptor{
		str("service", "The API service called, one of [firestore.googleapis.com, datastore.googleapis.com]."),
		str("method", "The API method called, for example, \"RunQuery\" or \"RunAggregationQuery\".")}
	md := func(t, display, desc, res string, vt metricpb.MetricDescriptor_ValueType, unit string, labels ...*labelpb.LabelDescriptor) *metricpb.MetricDescriptor {
		return &metricpb.MetricDescriptor{Name: descriptorName(project, t), Type: t, MetricKind: delta, ValueType: vt,
			Unit: unit, DisplayName: display, Description: desc, Labels: labels, MonitoredResourceTypes: []string{res}}
	}
	return []*metricpb.MetricDescriptor{
		md(fsReadOps, "Document reads", "The number of successful document reads from queries or lookups."+fsSampleNote, fsDatabase, tInt, "1",
			str("type", "The type of the read, one of [LOOKUP, QUERY].")),
		md(fsWriteOps, "Document writes", "The number of successful document writes."+fsSampleNote, fsDatabase, tInt, "1",
			str("op", "The operation name for the write, one of [CREATE, UPDATE].")),
		md(fsDeleteOps, "Document deletes", "The number of successful document deletes."+fsSampleNote, fsDatabase, tInt, "1"),
		md(fsRead, "Document Reads", "The number of successful document reads from queries or lookups."+fsSampleNote, fsInstance, tInt, "1",
			append(append([]*labelpb.LabelDescriptor{}, mv...), str("type", "The type of the read (e.g. 'LOOKUP', 'QUERY')."))...),
		md(fsWrite, "Document Writes", "The number of successful document writes."+fsSampleNote, fsInstance, tInt, "1",
			append(append([]*labelpb.LabelDescriptor{}, mv...), str("op", "The operation name for the write (e.g. 'CREATE', 'UPDATE')."))...),
		md(fsDelete, "Document Deletes", "The number of successful document deletes."+fsSampleNote, fsInstance, tInt, "1", mv...),
		md(fsResults, "Number of results yielded per query", "Distribution of number of results yielded per query.", fsDatabase, tDist, "{result}", sm...),
		md(fsScanned, "Number of documents scanned per query", "Distribution of number of documents scanned per query.", fsDatabase, tDist, "{document}", sm...),
	}
}

func (b firestoreBuiltin) Series(project, metricType string, start, end time.Time) ([]*monitoringpb.TimeSeries, error) {
	switch metricType {
	case fsReadOps, fsWriteOps, fsDeleteOps, fsRead, fsWrite, fsDelete, fsResults, fsScanned:
	default:
		return nil, nil
	}
	minutes, err := b.src.FirestoreMinutes(project, start.Add(-logPointPeriod), end)
	if err != nil {
		return nil, err
	}
	groups := map[string]*monitoringpb.TimeSeries{}
	counts := map[string]map[time.Time]int64{}
	dists := map[string]map[time.Time][]float64{}
	resource := func(m FirestoreMinute) *monitoredres.MonitoredResource {
		if metricType == fsRead || metricType == fsWrite || metricType == fsDelete {
			return &monitoredres.MonitoredResource{Type: fsInstance, Labels: map[string]string{"project_id": m.Project}}
		}
		return &monitoredres.MonitoredResource{Type: fsDatabase, Labels: map[string]string{
			"resource_container": m.Project, "location": m.Location, "database_id": m.Database}}
	}
	add := func(m FirestoreMinute, labels map[string]string, n int64) {
		if n == 0 {
			return
		}
		ts := &monitoringpb.TimeSeries{Metric: &metricpb.Metric{Type: metricType, Labels: labels}, Resource: resource(m),
			MetricKind: delta, ValueType: tInt, Unit: "1"}
		k := seriesKey(ts)
		if groups[k] == nil {
			groups[k], counts[k] = ts, map[time.Time]int64{}
		}
		// A minute's points end when it does.
		counts[k][m.Start.Add(logPointPeriod)] += n
	}
	legacy := func(l map[string]string) map[string]string {
		if metricType == fsRead || metricType == fsWrite || metricType == fsDelete {
			l["module"], l["version"] = "__unknown__", "__unknown__"
		}
		return l
	}
	for _, m := range minutes {
		pointEnd := m.Start.Add(logPointPeriod)
		if pointEnd.Before(start) || pointEnd.After(end) {
			continue
		}
		switch metricType {
		case fsReadOps, fsRead:
			for t, n := range m.Reads {
				add(m, legacy(map[string]string{"type": t}), n)
			}
		case fsWriteOps, fsWrite:
			for op, n := range m.Writes {
				add(m, legacy(map[string]string{"op": op}), n)
			}
		case fsDeleteOps, fsDelete:
			add(m, legacy(map[string]string{}), m.Deletes)
		case fsResults, fsScanned:
			for _, q := range m.Queries {
				vals := q.Results
				if metricType == fsScanned {
					vals = q.Scanned
				}
				if len(vals) == 0 {
					continue
				}
				ts := &monitoringpb.TimeSeries{Metric: &metricpb.Metric{Type: metricType, Labels: map[string]string{
					"service": "firestore.googleapis.com", "method": q.Method}}, Resource: resource(m),
					MetricKind: delta, ValueType: tDist, Unit: "{result}"}
				if metricType == fsScanned {
					ts.Unit = "{document}"
				}
				k := seriesKey(ts)
				if groups[k] == nil {
					groups[k], dists[k] = ts, map[time.Time][]float64{}
				}
				dists[k][pointEnd] = append(dists[k][pointEnd], vals...)
			}
		}
	}
	if metricType != fsResults && metricType != fsScanned {
		return countSeries(groups, counts), nil
	}
	// Powers of 4, as an exponential bucketing of counts.
	opts := &distribution.Distribution_BucketOptions{Options: &distribution.Distribution_BucketOptions_ExponentialBuckets{
		ExponentialBuckets: &distribution.Distribution_BucketOptions_Exponential{NumFiniteBuckets: 16, GrowthFactor: 4, Scale: 1}}}
	var out []*monitoringpb.TimeSeries
	for k, ts := range groups {
		for _, t := range sortedTimes(dists[k]) {
			ts.Points = append(ts.Points, minutePoint(t, distValue(newDistribution(opts, dists[k][t]))))
		}
		out = append(out, ts)
	}
	return out, nil
}
