//go:build compat

package compat

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	vlogging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	auditpb "google.golang.org/genproto/googleapis/cloud/audit"
	bqlogging "google.golang.org/genproto/googleapis/cloud/bigquery/logging/v1"
	ltype "google.golang.org/genproto/googleapis/logging/type"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// bqSinkClients are the official Logging clients and the BigQuery client
// of a BigQuery sink test; it skips where Logging or BigQuery is not
// served, before anything is called.
func bqSinkClients(t *testing.T, h *Harness) (*bigquery.Client, string, *vlogging.Client, *vlogging.ConfigClient) {
	t.Helper()
	bq, project := bigqueryClient(t, h)
	opts := []option.ClientOption{option.WithEndpoint(runShardEndpoint(h, envSinkLogging, EnvLogging)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
	lc, err := vlogging.NewClient(h.Context(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lc.Close() })
	cc, err := vlogging.NewConfigClient(h.Context(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return bq, project, lc, cc
}

// sinkDataset makes a dataset, deleted with its tables at the end.
func sinkDataset(t *testing.T, h *Harness, bq *bigquery.Client, id string) {
	t.Helper()
	if err := bq.Dataset(id).Create(h.Context(), &bigquery.DatasetMetadata{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bq.Dataset(id).DeleteWithContents(context.Background()) })
}

// sinkQueryRows reads q until it returns want rows or 30 seconds passed.
func sinkQueryRows(t *testing.T, h *Harness, bq *bigquery.Client, q string, want int) []map[string]bigquery.Value {
	t.Helper()
	var rows []map[string]bigquery.Value
	var lastErr error
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
		it, err := bq.Query(q).Read(h.Context())
		if err != nil {
			lastErr = err
			continue
		}
		rows = nil
		for {
			var r map[string]bigquery.Value
			err := it.Next(&r)
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			rows = append(rows, r)
		}
		if len(rows) == want {
			return rows
		}
	}
	t.Fatalf("%s: %d rows, want %d, last error %v", q, len(rows), want, lastErr)
	return nil
}

// checkExportError checks one export_errors row against Google's columns.
func checkExportError(t *testing.T, r map[string]bigquery.Value, sink, logName, insertID, value string) {
	t.Helper()
	detail, _ := r["schemaErrorDetail"].(string)
	entry, _ := r["logEntry"].(string)
	if r["sink"] != sink || r["logName"] != logName || r["insertId"] != insertID || !strings.Contains(entry, value) ||
		!strings.Contains(detail, "has changed type") || r["resourceType"] != "global" || r["timestamp"] == nil {
		t.Errorf("export_errors row %v", r)
	}
}

// covers: google.logging.v2.LoggingServiceV2/WriteLogEntries, google.logging.v2.ConfigServiceV2/CreateSink
//
// TestLoggingBigQuerySinkExportErrorsAndProtoPayload (#1451), to
// docs.cloud.google.com/logging/docs/export/bigquery: through sinks made
// with the official ConfigClient, an entry written with the official Client
// that would change a column's type is not in its table but in the
// dataset's error table, export_errors_YYYYMMDD for date-sharded tables and
// export_errors for partitioned ones, with the columns Google lists
// (logEntry, schemaErrorDetail, sink, logName, timestamp, severity,
// insertId, resourceType); an audit log protoPayload is the
// protopayload_auditlog column, its request a JSON string requestJson and
// its BigQuery serviceData servicedata_v1_bigquery; httpRequest's cache
// fields are columns. Read with the official BigQuery client.
func TestLoggingBigQuerySinkExportErrorsAndProtoPayload(t *testing.T) {
	h := New(t)
	bq, project, lc, cc := bqSinkClients(t, h)
	ctx := h.Context()
	suffix := fmt.Sprint(time.Now().UnixNano() % 1_000_000_000)
	logID, auditID := "xerrlog"+suffix, "xauditlog"+suffix
	parent := "projects/" + project
	sharded, parted := "xerrds"+suffix, "xerrdsp"+suffix
	sinkDataset(t, h, bq, sharded)
	sinkDataset(t, h, bq, parted)
	filter := fmt.Sprintf(`logName="%s/logs/%s"`, parent, logID)
	for id, sk := range map[string]*loggingpb.LogSink{
		"xerr-sharded-" + suffix: {Destination: "bigquery.googleapis.com/projects/" + project + "/datasets/" + sharded, Filter: filter},
		"xerr-parted-" + suffix: {Destination: "bigquery.googleapis.com/projects/" + project + "/datasets/" + parted, Filter: filter,
			Options: &loggingpb.LogSink_BigqueryOptions{BigqueryOptions: &loggingpb.BigQueryOptions{UsePartitionedTables: true}}},
		"xerr-audit-" + suffix: {Destination: "bigquery.googleapis.com/projects/" + project + "/datasets/" + sharded,
			Filter: fmt.Sprintf(`logName="%s/logs/%s"`, parent, auditID)},
	} {
		sk.Name = id
		if _, err := cc.CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: parent, Sink: sk}); err != nil {
			t.Fatalf("CreateSink %s: %v", id, err)
		}
		id := id
		t.Cleanup(func() {
			_ = cc.DeleteSink(context.Background(), &loggingpb.DeleteSinkRequest{SinkName: parent + "/sinks/" + id})
		})
	}

	now := time.Now().UTC()
	num, _ := structpb.NewStruct(map[string]any{"n": 1})
	str, _ := structpb.NewStruct(map[string]any{"n": "one" + suffix})
	req, _ := structpb.NewStruct(map[string]any{"name": "t" + suffix})
	sd, err := anypb.New(&bqlogging.AuditData{Request: &bqlogging.AuditData_TableInsertRequest{TableInsertRequest: &bqlogging.TableInsertRequest{
		Resource: &bqlogging.Table{TableName: &bqlogging.TableName{ProjectId: project, DatasetId: "d", TableId: "t" + suffix}}}}})
	if err != nil {
		t.Fatal(err)
	}
	audit, err := anypb.New(&auditpb.AuditLog{ServiceName: "bigquery.googleapis.com", MethodName: "tableservice.insert",
		ResourceName: "projects/" + project + "/datasets/d/tables/t" + suffix, Status: &status.Status{Code: 0, Message: "ok"},
		AuthenticationInfo: &auditpb.AuthenticationInfo{PrincipalEmail: "dev@example.com"}, Request: req, ServiceData: sd})
	if err != nil {
		t.Fatal(err)
	}
	res := &mrpb.MonitoredResource{Type: "global", Labels: map[string]string{"project_id": project}}
	if _, err := lc.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{Resource: res, Entries: []*loggingpb.LogEntry{
		{LogName: parent + "/logs/" + logID, Payload: &loggingpb.LogEntry_JsonPayload{JsonPayload: num}, Timestamp: timestamppb.New(now), InsertId: "a" + suffix,
			HttpRequest: &ltype.HttpRequest{RequestMethod: "GET", Status: 200, CacheLookup: true, CacheHit: true, CacheFillBytes: 12, Latency: durationpb.New(1500 * time.Millisecond)}},
		{LogName: parent + "/logs/" + logID, Payload: &loggingpb.LogEntry_JsonPayload{JsonPayload: str}, Timestamp: timestamppb.New(now), InsertId: "b" + suffix},
		{LogName: parent + "/logs/" + auditID, Payload: &loggingpb.LogEntry_ProtoPayload{ProtoPayload: audit}, Timestamp: timestamppb.New(now), InsertId: "c" + suffix},
	}}); err != nil {
		t.Fatal(err)
	}

	day := now.Format("20060102")
	for ds, tables := range map[string][2]string{sharded: {logID + "_" + day, "export_errors_" + day}, parted: {logID, "export_errors"}} {
		sink := parent + "/sinks/xerr-sharded-" + suffix
		if ds == parted {
			sink = parent + "/sinks/xerr-parted-" + suffix
		}
		// The table holds the first entry only, with httpRequest's cache fields.
		rows := sinkQueryRows(t, h, bq, fmt.Sprintf("SELECT insertId, jsonPayload.n AS n, httpRequest.cacheHit AS hit, httpRequest.cacheLookup AS lkp, httpRequest.cacheFillBytes AS fill, httpRequest.latency AS lat FROM `%s.%s.%s`",
			project, ds, tables[0]), 1)
		if r := rows[0]; r["insertId"] != "a"+suffix || r["n"] != 1.0 || r["hit"] != true || r["lkp"] != true || r["fill"] != int64(12) || r["lat"] != 1.5 {
			t.Errorf("%s.%s: %v", ds, tables[0], r)
		}
		// The mismatched one is in the error table.
		rows = sinkQueryRows(t, h, bq, fmt.Sprintf("SELECT logEntry, schemaErrorDetail, sink, logName, timestamp, severity, insertId, resourceType FROM `%s.%s.%s`",
			project, ds, tables[1]), 1)
		checkExportError(t, rows[0], sink, parent+"/logs/"+logID, "b"+suffix, "one"+suffix)
	}

	// The audit log's protoPayload.
	rows := sinkQueryRows(t, h, bq, fmt.Sprintf("SELECT p.methodName AS m, p.serviceName AS s, p.authenticationInfo.principalEmail AS who, p.status.message AS st, p.requestJson AS rq, "+
		"p.servicedata_v1_bigquery.tableInsertRequest.resource.tableName.tableId AS tid FROM (SELECT protopayload_auditlog AS p FROM `%s.%s.%s_%s`)", project, sharded, auditID, day), 1)
	r := rows[0]
	if r["m"] != "tableservice.insert" || r["s"] != "bigquery.googleapis.com" || r["who"] != "dev@example.com" || r["st"] != "ok" ||
		r["rq"] != `{"name":"t`+suffix+`"}` || r["tid"] != "t"+suffix {
		t.Errorf("protopayload_auditlog: %v", r)
	}
}

// TestGcloudLoggingBigQuerySinkExportErrors (#1451): a BigQuery sink made
// with `gcloud logging sinks create`, through the harness (egress blocked);
// of two entries written with `gcloud logging write --payload-type=json`
// whose field n is a number and then a string, the second is in the
// dataset's export_errors_YYYYMMDD table, read with the official BigQuery
// client.
func TestGcloudLoggingBigQuerySinkExportErrors(t *testing.T) {
	h := New(t)
	bq, project, _, _ := bqSinkClients(t, h) // skips without Logging or BigQuery
	g := newGcloudSession(t, h)
	suffix := fmt.Sprint(time.Now().UnixNano() % 1_000_000_000)
	logID, ds, sink := "gxerrlog"+suffix, "gxerrds"+suffix, "gxerr-"+suffix
	sinkDataset(t, h, bq, ds)
	g.must("logging", "sinks", "create", sink, "bigquery.googleapis.com/projects/"+project+"/datasets/"+ds,
		fmt.Sprintf(`--log-filter=logName="projects/%s/logs/%s"`, project, logID), "--project="+project)
	t.Cleanup(func() { _, _ = g.run(nil, "logging", "sinks", "delete", sink, "--project="+project, "--quiet") })
	g.must("logging", "write", logID, `{"n": 1}`, "--payload-type=json", "--project="+project)
	g.must("logging", "write", logID, `{"n": "one`+suffix+`"}`, "--payload-type=json", "--project="+project)
	// The table's day is the entries' timestamp's, by the Logging clock,
	// which earlier tests advance: an hour ahead, it was the next day's
	// table near midnight UTC (#1568).
	day := advanceLoggingClock(t, h, 0).UTC().Format("20060102")
	rows := sinkQueryRows(t, h, bq, fmt.Sprintf("SELECT jsonPayload.n AS n FROM `%s.%s.%s_%s`", project, ds, logID, day), 1)
	if rows[0]["n"] != 1.0 {
		t.Errorf("the table holds %v", rows)
	}
	rows = sinkQueryRows(t, h, bq, fmt.Sprintf("SELECT logEntry, schemaErrorDetail, sink, logName, timestamp, severity, insertId, resourceType FROM `%s.%s.export_errors_%s`",
		project, ds, day), 1)
	if r := rows[0]; r["sink"] != "projects/"+project+"/sinks/"+sink || r["logName"] != "projects/"+project+"/logs/"+logID ||
		!strings.Contains(fmt.Sprint(r["logEntry"]), "one"+suffix) || !strings.Contains(fmt.Sprint(r["schemaErrorDetail"]), "has changed type") {
		t.Errorf("export_errors row %v", r)
	}
}

// covers: google.logging.v2.ConfigServiceV2/CreateSink, google.logging.v2.ConfigServiceV2/GetSink, google.logging.v2.ConfigServiceV2/DeleteSink
//
// TestTerraformLoggingBigQuerySinkExportErrors (#1451): a
// google_logging_project_sink to a google_bigquery_dataset with
// bigquery_options.use_partitioned_tables applies with the official
// hashicorp/google provider through `cloudburrow terraform` (egress
// blocked) and plans clean; of two entries written with the official
// Client whose field n is a number and then a string, the second is in the
// dataset's export_errors table, read with the official BigQuery client.
func TestTerraformLoggingBigQuerySinkExportErrors(t *testing.T) {
	h := New(t)
	bq, project, lc, _ := bqSinkClients(t, h) // skips without Logging or BigQuery
	m := newTFModule(t)
	ctx := h.Context()
	suffix := fmt.Sprint(time.Now().UnixNano() % 1_000_000_000)
	logID, ds := "tfxerrlog"+suffix, "tfxerrds"+suffix
	m.write(fmt.Sprintf(`
resource "google_bigquery_dataset" "d" {
  project                    = %[1]q
  dataset_id                 = %[2]q
  location                   = "US"
  delete_contents_on_destroy = true
}
resource "google_logging_project_sink" "bq" {
  project                = %[1]q
  name                   = "tf-xerr-%[3]s"
  destination            = "bigquery.googleapis.com/projects/%[1]s/datasets/${google_bigquery_dataset.d.dataset_id}"
  filter                 = "logName=\"projects/%[1]s/logs/%[4]s\""
  unique_writer_identity = true
  bigquery_options {
    use_partitioned_tables = true
  }
}
`, project, ds, suffix, logID))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	m.planClean()

	num, _ := structpb.NewStruct(map[string]any{"n": 1})
	str, _ := structpb.NewStruct(map[string]any{"n": "one" + suffix})
	if _, err := lc.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{LogName: "projects/" + project + "/logs/" + logID,
		Resource: &mrpb.MonitoredResource{Type: "global"}, Entries: []*loggingpb.LogEntry{
			{Payload: &loggingpb.LogEntry_JsonPayload{JsonPayload: num}, InsertId: "a" + suffix},
			{Payload: &loggingpb.LogEntry_JsonPayload{JsonPayload: str}, InsertId: "b" + suffix}}}); err != nil {
		t.Fatal(err)
	}
	sinkQueryRows(t, h, bq, fmt.Sprintf("SELECT insertId FROM `%s.%s.%s`", project, ds, logID), 1)
	rows := sinkQueryRows(t, h, bq, fmt.Sprintf("SELECT logEntry, schemaErrorDetail, sink, logName, timestamp, severity, insertId, resourceType FROM `%s.%s.export_errors`",
		project, ds), 1)
	checkExportError(t, rows[0], "projects/"+project+"/sinks/tf-xerr-"+suffix, "projects/"+project+"/logs/"+logID, "b"+suffix, "one"+suffix)
}
