//go:build compat

package compat

import (
	"testing"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestFirestoreDocumentMetrics (#2048): Firestore's document metrics, as
// Google's metrics list publishes them, count what the official Firestore
// client did: 3 documents created and one updated, one deleted and one
// delete of a missing document (a no-op, not counted), one lookup and a
// query returning two documents.
//
// covers: google.monitoring.v3.MetricService/ListTimeSeries
func TestFirestoreDocumentMetrics(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvFirestore)
	mc := monitoringClient(t, h)
	fc := firestoreClient(t, h)
	ctx := h.Context()
	col := fc.Collection("metrics-" + time.Now().Format("150405.000000"))
	for i := 1; i <= 3; i++ {
		if _, err := col.Doc(string(rune('a'+i-1))).Set(ctx, map[string]any{"n": i}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := col.Doc("a").Set(ctx, map[string]any{"n": 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := col.Doc("c").Delete(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := col.Doc("missing").Delete(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := col.Doc("b").Get(ctx); err != nil {
		t.Fatal(err)
	}
	docs, err := col.Where("n", ">", 0).Documents(ctx).GetAll()
	if err != nil || len(docs) != 2 {
		t.Fatalf("the query = %d documents, %v; want 2", len(docs), err)
	}

	end := time.Now().Add(2 * time.Minute)
	iv := &monitoringpb.TimeInterval{StartTime: timestamppb.New(end.Add(-30 * time.Minute)), EndTime: timestamppb.New(end)}
	db := ` AND resource.type = "firestore.googleapis.com/Database" AND resource.labels.database_id = "(default)"`
	sum := func(metric, label, value string) int64 {
		f := `metric.type = "firestore.googleapis.com/document/` + metric + `"` + db
		if label != "" {
			f += ` AND metric.labels.` + label + ` = "` + value + `"`
		}
		var n int64
		for _, ts := range listAll(t, mc.ListTimeSeries(ctx, &monitoringpb.ListTimeSeriesRequest{Name: "projects/" + h.Project(), Filter: f, Interval: iv})) {
			if ts.GetResource().GetLabels()["resource_container"] != h.Project() {
				t.Errorf("%s: resource %v, want resource_container %s", metric, ts.GetResource(), h.Project())
			}
			for _, p := range ts.GetPoints() {
				n += p.GetValue().GetInt64Value()
			}
		}
		return n
	}
	// Other tests in the project may have written too: at least ours.
	if n := sum("write_ops_count", "op", "CREATE"); n < 3 {
		t.Errorf("write_ops_count CREATE = %d, want at least 3", n)
	}
	if n := sum("write_ops_count", "op", "UPDATE"); n < 1 {
		t.Errorf("write_ops_count UPDATE = %d, want at least 1", n)
	}
	if n := sum("delete_ops_count", "", ""); n < 1 {
		t.Errorf("delete_ops_count = %d, want at least 1", n)
	}
	if n := sum("read_ops_count", "type", "LOOKUP"); n < 1 {
		t.Errorf("read_ops_count LOOKUP = %d, want at least 1", n)
	}
	if n := sum("read_ops_count", "type", "QUERY"); n < 2 {
		t.Errorf("read_ops_count QUERY = %d, want at least 2", n)
	}
	md, err := mc.GetMetricDescriptor(ctx, &monitoringpb.GetMetricDescriptorRequest{
		Name: "projects/" + h.Project() + "/metricDescriptors/firestore.googleapis.com/document/read_ops_count"})
	if err != nil || md.GetDisplayName() != "Document reads" {
		t.Errorf("GetMetricDescriptor(read_ops_count) = %v, %v; want Google's display name Document reads", md, err)
	}
}
