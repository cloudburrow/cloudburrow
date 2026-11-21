//go:build compat

package compat

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	vlogging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	ltype "google.golang.org/genproto/googleapis/logging/type"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// covers: google.logging.v2.ConfigServiceV2/CreateView, google.logging.v2.ConfigServiceV2/UpdateView
// unverified: google.logging.v2.ConfigServiceV2/CreateView INVALID_ARGUMENT: a log view filter outside the documented subset (docs re-read 2026-11-20, still unanswered: https://docs.cloud.google.com/logging/docs/logs-views)
// unverified: google.logging.v2.ConfigServiceV2/UpdateView INVALID_ARGUMENT: a log view filter outside the documented subset (docs re-read 2026-11-20, still unanswered: https://docs.cloud.google.com/logging/docs/logs-views)
//
// TestLoggingViewFilterSubset (#2028): through the official ConfigClient, a
// log view takes the filter Google's log views docs list
// (docs.cloud.google.com/logging/docs/logs-views): SOURCE(), LOG_ID(),
// resource.type, resource labels, labels and App Hub labels, with AND, OR
// and NOT on one term, and ListLogEntries through it returns what it
// matches. CreateView and UpdateView refuse, as INVALID_ARGUMENT, a filter
// on any other field (severity, logName), SEARCH, bare text and NOT on a
// compound statement; a refused update leaves the view as it was.
func TestLoggingViewFilterSubset(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	cc, err := vlogging.NewConfigClient(ctx, loggingClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	lc := loggingAPIClient(t, h)
	project := "projects/" + h.Project()
	n := time.Now().UnixNano()
	logID := fmt.Sprint("vsub-", n)
	id := fmt.Sprint("vsub_", n)
	b, err := cc.CreateBucket(ctx, &loggingpb.CreateBucketRequest{Parent: project + "/locations/global", BucketId: id,
		Bucket: &loggingpb.LogBucket{}})
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	sink, err := cc.CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: project, Sink: &loggingpb.LogSink{Name: id,
		Destination: "logging.googleapis.com/" + b.GetName(), Filter: fmt.Sprintf(`logName="%s/logs/%s"`, project, logID)}})
	if err != nil {
		t.Fatalf("CreateSink: %v", err)
	}
	allowed := fmt.Sprintf(`SOURCE("%s") AND LOG_ID("%s") AND (resource.type = "gce_instance" OR labels.tier = "web") AND NOT resource.labels.zone = "us-east1-b"`, project, logID)
	v, err := cc.CreateView(ctx, &loggingpb.CreateViewRequest{Parent: b.GetName(), ViewId: "subset",
		View: &loggingpb.LogView{Filter: allowed}})
	if err != nil {
		t.Fatalf("CreateView with %s: %v", allowed, err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_ = cc.DeleteSink(ctx, &loggingpb.DeleteSinkRequest{SinkName: sink.GetName()})
		_ = cc.DeleteView(ctx, &loggingpb.DeleteViewRequest{Name: v.GetName()})
		_ = cc.DeleteBucket(ctx, &loggingpb.DeleteBucketRequest{Name: b.GetName()})
	})

	for _, e := range []struct{ text, typ, zone, tier string }{
		{"gce-a", "gce_instance", "us-central1-a", ""},
		{"gce-b", "gce_instance", "us-east1-b", ""},
		{"web", "global", "", "web"},
		{"other", "global", "", "batch"},
	} {
		if _, err := lc.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{LogName: project + "/logs/" + logID,
			Resource: &mrpb.MonitoredResource{Type: e.typ, Labels: map[string]string{"zone": e.zone}},
			Entries: []*loggingpb.LogEntry{{Severity: ltype.LogSeverity_ERROR, Labels: map[string]string{"tier": e.tier},
				Payload: &loggingpb.LogEntry_TextPayload{TextPayload: e.text}}}}); err != nil {
			t.Fatalf("WriteLogEntries: %v", err)
		}
	}
	logFilter := fmt.Sprintf(`logName="%s/logs/%s"`, project, logID)
	eventually(t, "the view's entries", func() error {
		got := entryTextsFiltered(t, h, lc, logFilter, v.GetName())
		sort.Strings(got)
		if s := strings.Join(got, ","); s != "gce-a,web" {
			return fmt.Errorf("ListLogEntries through the view = %s; want gce-a,web", s)
		}
		return nil
	})

	for _, f := range []string{
		`severity >= ERROR`,
		fmt.Sprintf(`LOG_ID("%s") AND severity = ERROR`, logID),
		logFilter,
		`SEARCH("web")`,
		`web`,
		fmt.Sprintf(`NOT (LOG_ID("%s") AND resource.type = "global")`, logID),
	} {
		_, err := cc.CreateView(ctx, &loggingpb.CreateViewRequest{Parent: b.GetName(), ViewId: "refused",
			View: &loggingpb.LogView{Filter: f}})
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(status.Convert(err).Message(), "Invalid filter") {
			t.Errorf("CreateView with %s = %v; want INVALID_ARGUMENT, Invalid filter", f, err)
		}
		_, err = cc.UpdateView(ctx, &loggingpb.UpdateViewRequest{Name: v.GetName(), View: &loggingpb.LogView{Filter: f},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"filter"}}})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("UpdateView with %s = %v; want INVALID_ARGUMENT", f, err)
		}
	}
	if got, err := cc.GetView(ctx, &loggingpb.GetViewRequest{Name: v.GetName()}); err != nil || got.GetFilter() != allowed {
		t.Errorf("after the refused updates, GetView = %v, %v; want the filter %s", got, err, allowed)
	}
	if _, err := cc.GetView(ctx, &loggingpb.GetViewRequest{Name: b.GetName() + "/views/refused"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetView of a refused view = %v; want NOT_FOUND", err)
	}
}
