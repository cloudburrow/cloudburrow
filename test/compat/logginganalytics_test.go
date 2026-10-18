//go:build compat

package compat

import (
	"errors"
	"fmt"
	"net/http"
	"os"
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
	"google.golang.org/protobuf/types/known/structpb"
)

// covers: google.logging.v2.ConfigServiceV2/CreateLink
// covers: google.logging.v2.ConfigServiceV2/GetLink
// covers: google.logging.v2.ConfigServiceV2/ListLinks
// covers: google.logging.v2.ConfigServiceV2/DeleteLink
// covers: google.logging.v2.LoggingServiceV2/WriteLogEntries
// covers: bigquery.datasets.get, bigquery.tables.list, bigquery.tables.get, bigquery.jobs.insert, bigquery.jobs.query
//
// TestLoggingLogAnalyticsLinkedDataset (#1423), with the official Logging
// ConfigClient and Client and the official BigQuery client: entries routed
// by a sink into a bucket with Log Analytics on are read back through the
// _AllLogs view of the bucket's linked dataset, in Log Analytics' schema,
// both those written before CreateLink and one written after; a link on a
// bucket without Log Analytics is FAILED_PRECONDITION and a second link of
// one ID ALREADY_EXISTS; the dataset refuses writes (403); DeleteLink
// deletes it.
func TestLoggingLogAnalyticsLinkedDataset(t *testing.T) {
	h := New(t)
	shardEndpoint(h, envSinkLogging, EnvLogging)
	ctx := h.Context()
	bq, project := bigqueryClient(t, h)
	cfg, err := vlogging.NewConfigClient(ctx, loggingClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	defer cfg.Close()
	lc, err := vlogging.NewClient(ctx, loggingClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	defer lc.Close()

	n := time.Now().UnixNano()
	parent := "projects/" + project
	global := parent + "/locations/global"
	bucketID, plainID := fmt.Sprintf("la-%d", n), fmt.Sprintf("plain-%d", n)
	logID := fmt.Sprintf("analytics-%d", n)
	logName := parent + "/logs/" + logID
	linkID := fmt.Sprintf("la_link_%d", n)

	bucket, err := cfg.CreateBucket(ctx, &loggingpb.CreateBucketRequest{Parent: global, BucketId: bucketID,
		Bucket: &loggingpb.LogBucket{AnalyticsEnabled: true}})
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	t.Cleanup(func() { _ = cfg.DeleteBucket(ctx, &loggingpb.DeleteBucketRequest{Name: bucket.GetName()}) })
	plain, err := cfg.CreateBucket(ctx, &loggingpb.CreateBucketRequest{Parent: global, BucketId: plainID, Bucket: &loggingpb.LogBucket{}})
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	t.Cleanup(func() { _ = cfg.DeleteBucket(ctx, &loggingpb.DeleteBucketRequest{Name: plain.GetName()}) })
	sink, err := cfg.CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: parent, Sink: &loggingpb.LogSink{
		Name: fmt.Sprintf("la-sink-%d", n), Destination: "logging.googleapis.com/" + bucket.GetName(),
		Filter: fmt.Sprintf("logName=%q", logName)}})
	if err != nil {
		t.Fatalf("CreateSink: %v", err)
	}
	t.Cleanup(func() {
		_ = cfg.DeleteSink(ctx, &loggingpb.DeleteSinkRequest{SinkName: parent + "/sinks/" + sink.GetName()})
	})

	write := func(e *loggingpb.LogEntry) {
		t.Helper()
		e.LogName = logName
		e.Resource = &mrpb.MonitoredResource{Type: "global", Labels: map[string]string{"project_id": project}}
		if _, err := lc.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{Entries: []*loggingpb.LogEntry{e}}); err != nil {
			t.Fatalf("WriteLogEntries: %v", err)
		}
	}
	js, _ := structpb.NewStruct(map[string]any{"msg": "before the link", "code": 7})
	write(&loggingpb.LogEntry{Severity: ltype.LogSeverity_ERROR, Labels: map[string]string{"env": "test"},
		Payload: &loggingpb.LogEntry_JsonPayload{JsonPayload: js}})
	write(&loggingpb.LogEntry{Severity: ltype.LogSeverity_INFO, Payload: &loggingpb.LogEntry_TextPayload{TextPayload: "plain text"}})

	if _, err := cfg.CreateLink(ctx, &loggingpb.CreateLinkRequest{Parent: plain.GetName(), LinkId: linkID}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("CreateLink on a bucket without Log Analytics = %v; want FAILED_PRECONDITION", err)
	}
	op, err := cfg.CreateLink(ctx, &loggingpb.CreateLinkRequest{Parent: bucket.GetName(), LinkId: linkID,
		Link: &loggingpb.Link{Description: "Log Analytics"}})
	if err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	link, err := op.Wait(ctx)
	if err != nil || link.GetBigqueryDataset().GetDatasetId() != "bigquery.googleapis.com/"+parent+"/datasets/"+linkID {
		t.Fatalf("CreateLink.Wait = %v, %v", link, err)
	}
	linkDeleted := false
	t.Cleanup(func() {
		if !linkDeleted {
			if op, err := cfg.DeleteLink(ctx, &loggingpb.DeleteLinkRequest{Name: link.GetName()}); err == nil {
				_ = op.Wait(ctx)
			}
		}
	})
	if _, err := cfg.CreateLink(ctx, &loggingpb.CreateLinkRequest{Parent: bucket.GetName(), LinkId: linkID}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("second CreateLink = %v; want ALREADY_EXISTS", err)
	}
	if got, err := cfg.GetLink(ctx, &loggingpb.GetLinkRequest{Name: link.GetName()}); err != nil || got.GetLifecycleState() != loggingpb.LifecycleState_ACTIVE {
		t.Errorf("GetLink = %v, %v", got, err)
	}
	it := cfg.ListLinks(ctx, &loggingpb.ListLinksRequest{Parent: bucket.GetName()})
	if l, err := it.Next(); err != nil || l.GetName() != link.GetName() {
		t.Errorf("ListLinks = %v, %v", l, err)
	}

	ds := bq.Dataset(linkID)
	if _, err := ds.Metadata(ctx); err != nil {
		t.Fatalf("the linked dataset: %v", err)
	}
	var tables []string
	for ti := ds.Tables(ctx); ; {
		tb, err := ti.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("tables.list: %v", err)
		}
		tables = append(tables, tb.TableID)
	}
	if strings.Join(tables, ",") != "_AllLogs" {
		t.Errorf("the linked dataset's tables = %v; want [_AllLogs]", tables)
	}
	if md, err := ds.Table("_AllLogs").Metadata(ctx); err != nil || md.Type != bigquery.ViewTable {
		t.Errorf("_AllLogs = %v, %v; want a view", md, err)
	} else {
		cols := map[string]bigquery.FieldType{}
		for _, f := range md.Schema {
			cols[f.Name] = f.Type
		}
		for name, typ := range map[string]bigquery.FieldType{"timestamp": bigquery.TimestampFieldType, "receive_timestamp": bigquery.TimestampFieldType,
			"severity": bigquery.StringFieldType, "severity_number": bigquery.IntegerFieldType, "log_name": bigquery.StringFieldType,
			"log_id": bigquery.StringFieldType, "insert_id": bigquery.StringFieldType, "resource": bigquery.RecordFieldType,
			"json_payload": bigquery.JSONFieldType, "text_payload": bigquery.StringFieldType, "labels": bigquery.JSONFieldType,
			"http_request": bigquery.RecordFieldType, "trace": bigquery.StringFieldType, "span_id": bigquery.StringFieldType} {
			if cols[name] != typ {
				t.Errorf("_AllLogs column %s is %q; want %s", name, cols[name], typ)
			}
		}
	}

	type row struct {
		Severity string              `bigquery:"severity"`
		Number   int64               `bigquery:"severity_number"`
		LogID    string              `bigquery:"log_id"`
		Resource string              `bigquery:"resource_type"`
		Msg      bigquery.NullString `bigquery:"msg"`
		Env      bigquery.NullString `bigquery:"env"`
		Text     bigquery.NullString `bigquery:"text_payload"`
	}
	read := func() ([]row, error) {
		q := bq.Query(fmt.Sprintf("SELECT severity, severity_number, log_id, resource.type AS resource_type, "+
			"JSON_VALUE(json_payload.msg) AS msg, JSON_VALUE(labels.env) AS env, text_payload "+
			"FROM `%s.%s._AllLogs` WHERE log_id = @log ORDER BY timestamp, insert_id", project, linkID))
		q.Parameters = []bigquery.QueryParameter{{Name: "log", Value: logID}}
		ri, err := q.Read(ctx)
		if err != nil {
			return nil, err
		}
		var out []row
		for {
			var r row
			err := ri.Next(&r)
			if errors.Is(err, iterator.Done) {
				return out, nil
			}
			if err != nil {
				return nil, err
			}
			out = append(out, r)
		}
	}
	rows, err := read()
	if err != nil {
		t.Fatalf("query of _AllLogs: %v", err)
	}
	if len(rows) != 2 || rows[0].Severity != "ERROR" || rows[0].Number != 500 || rows[0].Msg.StringVal != "before the link" ||
		rows[0].Env.StringVal != "test" || rows[0].LogID != logID || rows[0].Resource != "global" ||
		rows[1].Text.StringVal != "plain text" || rows[1].Severity != "INFO" {
		t.Fatalf("_AllLogs rows = %+v", rows)
	}

	js2, _ := structpb.NewStruct(map[string]any{"msg": "after the link"})
	write(&loggingpb.LogEntry{Severity: ltype.LogSeverity_WARNING, Payload: &loggingpb.LogEntry_JsonPayload{JsonPayload: js2}})
	eventually(t, "the entry written after the link in _AllLogs", func() error {
		rows, err := read()
		if err != nil {
			return err
		}
		if len(rows) != 3 || rows[2].Msg.StringVal != "after the link" {
			return fmt.Errorf("rows = %+v", rows)
		}
		return nil
	})

	forbidden := func(what string, err error) {
		t.Helper()
		var ge *googleapi.Error
		if !errors.As(err, &ge) || ge.Code != http.StatusForbidden {
			t.Errorf("%s in the linked dataset = %v; want 403", what, err)
		}
	}
	forbidden("tables.insert", ds.Table("mine").Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{{Name: "a", Type: bigquery.StringFieldType}}}))
	forbidden("tables.delete", ds.Table("_AllLogs").Delete(ctx))
	_, err = ds.Update(ctx, bigquery.DatasetMetadataToUpdate{Description: "mine"}, "")
	forbidden("datasets.patch", err)
	forbidden("datasets.delete", ds.DeleteWithContents(ctx))
	q := bq.Query(fmt.Sprintf("CREATE TABLE `%s.%s.mine` (a STRING)", project, linkID))
	if _, err := q.Read(ctx); err == nil {
		t.Error("CREATE TABLE in the linked dataset succeeded")
	}
	if rows, err := read(); err != nil || len(rows) != 3 {
		t.Errorf("after the refused writes, _AllLogs = %v, %v", rows, err)
	}

	dop, err := cfg.DeleteLink(ctx, &loggingpb.DeleteLinkRequest{Name: link.GetName()})
	if err != nil {
		t.Fatalf("DeleteLink: %v", err)
	}
	if err := dop.Wait(ctx); err != nil {
		t.Fatalf("DeleteLink.Wait: %v", err)
	}
	linkDeleted = true
	var ge *googleapi.Error
	if _, err := ds.Metadata(ctx); !errors.As(err, &ge) || ge.Code != http.StatusNotFound {
		t.Errorf("the dataset after DeleteLink = %v; want 404", err)
	}
}

// TestGcloudLoggingLinks (#1423): `gcloud logging buckets create
// --enable-analytics`, `gcloud logging links create`, `describe`, `list`
// and `delete`, through the harness (egress blocked); the linked dataset
// gcloud makes is read with the official BigQuery client.
func TestGcloudLoggingLinks(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	// gcloud logging's override is Cloud Logging's: SINK_LOGGING in the
	// emulators shard, beside BigQuery (#1304).
	runShardEndpoint(h, envSinkLogging, EnvLogging)
	bq, project := bigqueryClient(t, h)
	g := newGcloudSession(t, h)
	n := time.Now().UnixNano()
	bucketID, linkID := fmt.Sprintf("gla-%d", n), fmt.Sprintf("gla_link_%d", n)
	p := "--project=" + project
	g.must("logging", "buckets", "create", bucketID, "--location=global", "--enable-analytics", p)
	t.Cleanup(func() { _, _ = g.run(nil, "logging", "buckets", "delete", bucketID, "--location=global", "--quiet", p) })
	// A log view made with gcloud is a view of the linked dataset (#1435).
	g.must("logging", "views", "create", "errors", "--bucket="+bucketID, "--location=global", "--log-filter=severity>=ERROR", p)
	g.must("logging", "links", "create", linkID, "--bucket="+bucketID, "--location=global", "--description=gcloud", p)
	out := g.must("logging", "links", "describe", linkID, "--bucket="+bucketID, "--location=global", p, "--format=value(bigqueryDataset.datasetId)")
	if want := "bigquery.googleapis.com/projects/" + project + "/datasets/" + linkID; strings.TrimSpace(out) != want {
		t.Errorf("gcloud logging links describe = %q; want %s", out, want)
	}
	if out := g.must("logging", "links", "list", "--bucket="+bucketID, "--location=global", p, "--format=value(name)"); !strings.Contains(out, linkID) {
		t.Errorf("gcloud logging links list = %q", out)
	}
	for _, v := range []string{"_AllLogs", "errors"} {
		if md, err := bq.Dataset(linkID).Table(v).Metadata(ctx); err != nil || md.Type != bigquery.ViewTable {
			t.Errorf("%s of the gcloud link = %v, %v", v, md, err)
		}
	}
	g.must("logging", "links", "delete", linkID, "--bucket="+bucketID, "--location=global", "--quiet", p)
	var ge *googleapi.Error
	if _, err := bq.Dataset(linkID).Metadata(ctx); !errors.As(err, &ge) || ge.Code != http.StatusNotFound {
		t.Errorf("the dataset after gcloud logging links delete = %v; want 404", err)
	}
}

// linkProject is the project a Logging link's bucket must be in: the one the
// instance's BigQuery serves, where the link makes its dataset (#1423).
func linkProject(t *testing.T) string {
	t.Helper()
	p := strings.TrimSpace(os.Getenv(EnvBigQueryProject))
	if p == "" {
		t.Skipf("%s is not set: a Logging link makes a dataset in the BigQuery project", EnvBigQueryProject)
	}
	return p
}
