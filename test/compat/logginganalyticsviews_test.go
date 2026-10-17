//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	vlogging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	ltype "google.golang.org/genproto/googleapis/logging/type"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// linkFixture is a bucket with Log Analytics on, a sink routing one log
// into it, and the clients a linked-dataset test needs (#1435).
type linkFixture struct {
	t               *testing.T
	ctx             context.Context
	bq              *bigquery.Client
	cfg             *vlogging.ConfigClient
	lc              *vlogging.Client
	project, bucket string
	logName, linkID string
	logID           string
	deletedLink     bool
	linkName        string
}

func newLinkFixture(t *testing.T, h *Harness, prefix string) *linkFixture {
	// Each expect waits up to a minute for the linked dataset to follow, and
	// a test makes several: h.Context()'s one minute for them all ran out
	// on a loaded machine (#1568).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	bq, project := bigqueryClient(t, h)
	cfg, err := vlogging.NewConfigClient(ctx, loggingClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cfg.Close() })
	lc, err := vlogging.NewClient(ctx, loggingClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lc.Close() })
	n := time.Now().UnixNano()
	f := &linkFixture{t: t, ctx: ctx, bq: bq, cfg: cfg, lc: lc, project: project,
		logID: fmt.Sprintf("%s-%d", prefix, n), linkID: fmt.Sprintf("%s_link_%d", prefix, n)}
	parent := "projects/" + project
	f.logName = parent + "/logs/" + f.logID
	b, err := cfg.CreateBucket(ctx, &loggingpb.CreateBucketRequest{Parent: parent + "/locations/global", BucketId: fmt.Sprintf("%s-%d", prefix, n),
		Bucket: &loggingpb.LogBucket{AnalyticsEnabled: true}})
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	f.bucket = b.GetName()
	t.Cleanup(func() { _ = cfg.DeleteBucket(context.Background(), &loggingpb.DeleteBucketRequest{Name: f.bucket}) })
	sink, err := cfg.CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: parent, Sink: &loggingpb.LogSink{
		Name: fmt.Sprintf("%s-sink-%d", prefix, n), Destination: "logging.googleapis.com/" + f.bucket,
		Filter: fmt.Sprintf("logName=%q", f.logName)}})
	if err != nil {
		t.Fatalf("CreateSink: %v", err)
	}
	t.Cleanup(func() {
		_ = cfg.DeleteSink(context.Background(), &loggingpb.DeleteSinkRequest{SinkName: parent + "/sinks/" + sink.GetName()})
	})
	return f
}

func (f *linkFixture) write(msg string, sev ltype.LogSeverity, at time.Time) {
	f.t.Helper()
	js, _ := structpb.NewStruct(map[string]any{"msg": msg})
	e := &loggingpb.LogEntry{LogName: f.logName, Severity: sev, Payload: &loggingpb.LogEntry_JsonPayload{JsonPayload: js},
		Resource: &mrpb.MonitoredResource{Type: "global", Labels: map[string]string{"project_id": f.project}}}
	if !at.IsZero() {
		e.Timestamp = timestamppb.New(at)
	}
	if _, err := f.lc.WriteLogEntries(f.ctx, &loggingpb.WriteLogEntriesRequest{Entries: []*loggingpb.LogEntry{e}}); err != nil {
		f.t.Fatalf("WriteLogEntries: %v", err)
	}
}

func (f *linkFixture) link() {
	f.t.Helper()
	op, err := f.cfg.CreateLink(f.ctx, &loggingpb.CreateLinkRequest{Parent: f.bucket, LinkId: f.linkID})
	if err != nil {
		f.t.Fatalf("CreateLink: %v", err)
	}
	l, err := op.Wait(f.ctx)
	if err != nil {
		f.t.Fatalf("CreateLink.Wait: %v", err)
	}
	f.linkName = l.GetName()
	f.t.Cleanup(func() {
		if op, err := f.cfg.DeleteLink(context.Background(), &loggingpb.DeleteLinkRequest{Name: f.linkName}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
}

// tables lists the linked dataset's tables.
func (f *linkFixture) tables() ([]string, error) {
	var out []string
	for it := f.bq.Dataset(f.linkID).Tables(f.ctx); ; {
		tb, err := it.Next()
		if errors.Is(err, iterator.Done) {
			slices.Sort(out)
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, tb.TableID)
	}
}

// msgs are json_payload.msg of the rows of the dataset's view, in time
// order.
func (f *linkFixture) msgs(view string) (string, error) {
	q := f.bq.Query(fmt.Sprintf("SELECT JSON_VALUE(json_payload.msg) AS msg FROM `%s.%s.%s` WHERE log_id = @log ORDER BY timestamp, insert_id",
		f.project, f.linkID, view))
	q.Parameters = []bigquery.QueryParameter{{Name: "log", Value: f.logID}}
	it, err := q.Read(f.ctx)
	if err != nil {
		return "", err
	}
	var out []string
	for {
		var r struct {
			Msg bigquery.NullString `bigquery:"msg"`
		}
		err := it.Next(&r)
		if errors.Is(err, iterator.Done) {
			return strings.Join(out, ","), nil
		}
		if err != nil {
			return "", err
		}
		out = append(out, r.Msg.StringVal)
	}
}

func (f *linkFixture) expect(when, view, want string) {
	f.t.Helper()
	eventually(f.t, when+": "+view, func() error {
		got, err := f.msgs(view)
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("%s holds %q; want %q", view, got, want)
		}
		return nil
	})
}

// covers: google.logging.v2.ConfigServiceV2/CreateLink
// covers: google.logging.v2.ConfigServiceV2/CreateView
// covers: google.logging.v2.ConfigServiceV2/UpdateView
// covers: google.logging.v2.ConfigServiceV2/DeleteView
// covers: google.logging.v2.ConfigServiceV2/UpdateBucket
// covers: google.logging.v2.LoggingServiceV2/DeleteLog
// covers: google.logging.v2.LoggingServiceV2/WriteLogEntries
// covers: bigquery.tables.list, bigquery.tables.get, bigquery.jobs.insert, bigquery.jobs.query
//
// TestLoggingLinkedDatasetViewsRetentionAndDeleteLog (#1435), with the
// official Logging ConfigClient and Client and the official BigQuery
// client: the linked dataset has a view per log view, holding what the
// view's filter matches, kept as views are created, updated and deleted;
// a shorter retention and DeleteLog remove rows from it as they remove
// entries from the bucket; a folder's or organization's bucket cannot have
// Log Analytics, so CreateLink on it is FAILED_PRECONDITION.
func TestLoggingLinkedDatasetViewsRetentionAndDeleteLog(t *testing.T) {
	h := New(t)
	shardEndpoint(h, envSinkLogging, EnvLogging)
	f := newLinkFixture(t, h, "lav")
	ctx := f.ctx
	f.write("old", ltype.LogSeverity_ERROR, time.Now().Add(-3*24*time.Hour))
	f.write("e1", ltype.LogSeverity_ERROR, time.Time{})
	f.write("i1", ltype.LogSeverity_INFO, time.Time{})
	if _, err := f.cfg.CreateView(ctx, &loggingpb.CreateViewRequest{Parent: f.bucket, ViewId: "errors",
		View: &loggingpb.LogView{Filter: "severity>=ERROR"}}); err != nil {
		t.Fatalf("CreateView: %v", err)
	}
	f.link()
	if got, err := f.tables(); err != nil || strings.Join(got, ",") != "_AllLogs,errors" {
		t.Errorf("the linked dataset's tables = %v, %v; want _AllLogs and errors", got, err)
	}
	if md, err := f.bq.Dataset(f.linkID).Table("errors").Metadata(ctx); err != nil || md.Type != bigquery.ViewTable {
		t.Errorf("errors = %v, %v; want a view", md, err)
	}
	f.expect("after CreateLink", "_AllLogs", "old,e1,i1")
	f.expect("after CreateLink", "errors", "old,e1")
	f.write("e2", ltype.LogSeverity_CRITICAL, time.Time{})
	f.expect("after a write", "errors", "old,e1,e2")

	// A view made after the link, and a changed filter.
	if _, err := f.cfg.CreateView(ctx, &loggingpb.CreateViewRequest{Parent: f.bucket, ViewId: "info",
		View: &loggingpb.LogView{Filter: "severity=INFO"}}); err != nil {
		t.Fatalf("CreateView after the link: %v", err)
	}
	f.expect("after CreateView", "info", "i1")
	if _, err := f.cfg.UpdateView(ctx, &loggingpb.UpdateViewRequest{Name: f.bucket + "/views/errors",
		View: &loggingpb.LogView{Filter: "severity=CRITICAL"}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"filter"}}}); err != nil {
		t.Fatalf("UpdateView: %v", err)
	}
	f.expect("after UpdateView", "errors", "e2")
	if err := f.cfg.DeleteView(ctx, &loggingpb.DeleteViewRequest{Name: f.bucket + "/views/info"}); err != nil {
		t.Fatalf("DeleteView: %v", err)
	}
	if got, err := f.tables(); err != nil || strings.Join(got, ",") != "_AllLogs,errors" {
		t.Errorf("after DeleteView, tables = %v, %v; want _AllLogs and errors", got, err)
	}

	// Retention: the entry 3 days old leaves with a 2-day retention.
	if _, err := f.cfg.UpdateBucket(ctx, &loggingpb.UpdateBucketRequest{Name: f.bucket, Bucket: &loggingpb.LogBucket{RetentionDays: 2},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"retention_days"}}}); err != nil {
		t.Fatalf("UpdateBucket: %v", err)
	}
	f.expect("after a 2-day retention", "_AllLogs", "e1,i1,e2")

	// DeleteLog removes the log's rows.
	if err := f.lc.DeleteLog(ctx, &loggingpb.DeleteLogRequest{LogName: f.logName}); err != nil {
		t.Fatalf("DeleteLog: %v", err)
	}
	f.expect("after DeleteLog", "_AllLogs", "")

	// A folder's or organization's bucket cannot be upgraded to Log
	// Analytics (docs.cloud.google.com/logging/docs/buckets).
	for _, p := range []string{"folders/1435", "organizations/1435"} {
		_, err := f.cfg.CreateLink(ctx, &loggingpb.CreateLinkRequest{Parent: p + "/locations/global/buckets/_Default", LinkId: "x"})
		switch {
		case status.Code(err) == codes.FailedPrecondition:
		case status.Code(err) == codes.Unimplemented && strings.Contains(err.Error(), "Resource Manager"),
			status.Code(err) == codes.NotFound:
			t.Logf("CreateLink on %s = %v (the instance has no such %s)", p, err, p)
		default:
			t.Errorf("CreateLink on %s's bucket = %v; want FAILED_PRECONDITION", p, err)
		}
	}
}

// covers: google.logging.v2.ConfigServiceV2/CreateLink
// covers: bigquery.tables.insert, bigquery.jobs.query
//
// TestLoggingLinkedDatasetAcrossRestarts (#1435): after the BigQuery
// emulator restarts, which loses what it held, the link's dataset is made
// again with its views and the bucket's entries; after the front's
// container restarts, the emulator still running, the dataset is still
// read-only (403).
func TestLoggingLinkedDatasetAcrossRestarts(t *testing.T) {
	h := New(t)
	shardEndpoint(h, envSinkLogging, EnvLogging)
	// The fixture first: it skips where BigQuery is not served, before the
	// restarter looks for the emulator's pod (main run 38946612957).
	f := newLinkFixture(t, h, "lar")
	restart := bigQueryEmulatorRestarter(t)
	f.write("before", ltype.LogSeverity_ERROR, time.Time{})
	if _, err := f.cfg.CreateView(f.ctx, &loggingpb.CreateViewRequest{Parent: f.bucket, ViewId: "errors",
		View: &loggingpb.LogView{Filter: "severity>=ERROR"}}); err != nil {
		t.Fatalf("CreateView: %v", err)
	}
	f.link()
	f.expect("before the restart", "errors", "before")

	restart()
	f.expect("after the emulator restart", "_AllLogs", "before")
	f.expect("after the emulator restart", "errors", "before")
	f.write("after", ltype.LogSeverity_ERROR, time.Time{})
	f.expect("a write after the emulator restart", "errors", "before,after")

	restartFront(t, "bigquery", "bigquery")
	eventually(t, "the dataset read-only after a front restart", func() error {
		err := f.bq.Dataset(f.linkID).Table("mine").Create(f.ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{{Name: "a", Type: bigquery.StringFieldType}}})
		var ge *googleapi.Error
		if errors.As(err, &ge) && ge.Code == http.StatusForbidden {
			return nil
		}
		if err == nil {
			_ = f.bq.Dataset(f.linkID).Table("mine").Delete(f.ctx)
			t.Fatal("tables.insert in the linked dataset after a front restart succeeded; want 403")
		}
		return err
	})
	f.expect("after the front restart", "errors", "before,after")
}
