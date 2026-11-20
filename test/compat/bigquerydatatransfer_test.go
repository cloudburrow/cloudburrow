//go:build compat

package compat

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	datatransfer "cloud.google.com/go/bigquery/datatransfer/apiv1"
	"cloud.google.com/go/bigquery/datatransfer/apiv1/datatransferpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// EnvDataTransfer is the host address of the BigQuery Data Transfer
// Service (bigquerydatatransfer.googleapis.com, #1948), gRPC and JSON on one
// port, served with BigQuery.
const EnvDataTransfer = "CLOUDBURROW_TEST_DATATRANSFER"

// dataTransferClients is the official client over gRPC and over REST.
func dataTransferClients(t *testing.T, h *Harness) (g, r *datatransfer.Client) {
	t.Helper()
	addr := h.Endpoint(EnvDataTransfer)
	ctx := context.Background()
	g, err := datatransfer.NewClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	r, err = datatransfer.NewRESTClient(ctx, option.WithEndpoint("http://"+addr), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close(); _ = r.Close() })
	return g, r
}

// waitRun polls a run until it finishes, failing unless it SUCCEEDED.
func waitRun(t *testing.T, c *datatransfer.Client, name string) *datatransferpb.TransferRun {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		run, err := c.GetTransferRun(context.Background(), &datatransferpb.GetTransferRunRequest{Name: name})
		if err != nil {
			t.Fatalf("GetTransferRun %s: %v", name, err)
		}
		switch run.GetState() {
		case datatransferpb.TransferState_SUCCEEDED:
			return run
		case datatransferpb.TransferState_FAILED, datatransferpb.TransferState_CANCELLED:
			t.Fatalf("run %s %s: %v", name, run.GetState(), run.GetErrorStatus())
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("run %s did not finish", name)
	return nil
}

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestBigQueryDataTransferScheduledQuery (#1948), official Go clients: the
// data sources list both served ones; a scheduled query's config is
// created, its manual run executes on the local BigQuery with @run_time and
// @run_date and writes the {run_time|"%Y%m%d"} table with WRITE_TRUNCATE;
// advancing the instance's clock past next_run_time fires the scheduled
// run; runs and their logs list; the config patches over REST, lists and
// deletes; a schedule more frequent than 5 minutes is INVALID_ARGUMENT.
//
// covers: google.cloud.bigquery.datatransfer.v1.DataTransferService/ListDataSources, google.cloud.bigquery.datatransfer.v1.DataTransferService/GetDataSource, google.cloud.bigquery.datatransfer.v1.DataTransferService/CreateTransferConfig, google.cloud.bigquery.datatransfer.v1.DataTransferService/GetTransferConfig, google.cloud.bigquery.datatransfer.v1.DataTransferService/ListTransferConfigs, google.cloud.bigquery.datatransfer.v1.DataTransferService/UpdateTransferConfig, google.cloud.bigquery.datatransfer.v1.DataTransferService/DeleteTransferConfig, google.cloud.bigquery.datatransfer.v1.DataTransferService/StartManualTransferRuns, google.cloud.bigquery.datatransfer.v1.DataTransferService/ScheduleTransferRuns, google.cloud.bigquery.datatransfer.v1.DataTransferService/GetTransferRun, google.cloud.bigquery.datatransfer.v1.DataTransferService/ListTransferRuns, google.cloud.bigquery.datatransfer.v1.DataTransferService/DeleteTransferRun, google.cloud.bigquery.datatransfer.v1.DataTransferService/ListTransferLogs, google.cloud.bigquery.datatransfer.v1.DataTransferService/EnrollDataSources, google.cloud.location.Locations/ListLocations, google.cloud.location.Locations/GetLocation
func TestBigQueryDataTransferScheduledQuery(t *testing.T) {
	h := New(t)
	g, r := dataTransferClients(t, h)
	bq, project := bigqueryClient(t, h)
	ctx := h.Context()
	ds, _ := seedOrders(t, h, bq)
	parent := "projects/" + project + "/locations/us"

	var sources []string
	it := g.ListDataSources(ctx, &datatransferpb.ListDataSourcesRequest{Parent: parent})
	for {
		d, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListDataSources: %v", err)
		}
		sources = append(sources, d.GetDataSourceId())
	}
	if strings.Join(sources, ",") != "cross_region_copy,scheduled_query" {
		t.Fatalf("data sources %v", sources)
	}
	src, err := r.GetDataSource(ctx, &datatransferpb.GetDataSourceRequest{Name: parent + "/dataSources/scheduled_query"})
	if err != nil || src.GetMinimumScheduleInterval().AsDuration() != 5*time.Minute {
		t.Fatalf("GetDataSource: %v, %v", src, err)
	}
	if err := g.EnrollDataSources(ctx, &datatransferpb.EnrollDataSourcesRequest{Name: parent, DataSourceIds: []string{"scheduled_query"}}); err != nil {
		t.Fatalf("EnrollDataSources: %v", err)
	}
	if _, err := g.GetLocation(ctx, &locationpb.GetLocationRequest{Name: parent}); err != nil {
		t.Fatalf("GetLocation: %v", err)
	}
	if _, err := g.ListLocations(ctx, &locationpb.ListLocationsRequest{Name: "projects/" + project}).Next(); err != nil {
		t.Fatalf("ListLocations: %v", err)
	}

	query := "SELECT @run_date AS d, @run_time AS t, COUNT(*) AS n FROM `" + ds.DatasetID + ".orders`"
	cfg := &datatransferpb.TransferConfig{
		DisplayName: "nightly", DataSourceId: "scheduled_query", Schedule: "every 24 hours",
		Destination: &datatransferpb.TransferConfig_DestinationDatasetId{DestinationDatasetId: ds.DatasetID},
		Params: mustStruct(t, map[string]any{"query": query,
			"destination_table_name_template": `daily_{run_time|"%Y%m%d"}`, "write_disposition": "WRITE_TRUNCATE"}),
	}
	_, err = g.CreateTransferConfig(ctx, &datatransferpb.CreateTransferConfigRequest{Parent: "projects/" + project,
		TransferConfig: &datatransferpb.TransferConfig{DataSourceId: "scheduled_query", Schedule: "every 1 minutes",
			Destination: cfg.Destination, Params: cfg.Params}})
	if code := status.Code(err); code != codes.InvalidArgument {
		t.Fatalf("every 1 minutes: %v, want INVALID_ARGUMENT", err)
	}
	created, err := g.CreateTransferConfig(ctx, &datatransferpb.CreateTransferConfigRequest{Parent: "projects/" + project, TransferConfig: cfg})
	if err != nil {
		t.Fatalf("CreateTransferConfig: %v", err)
	}
	t.Cleanup(func() {
		_ = g.DeleteTransferConfig(context.Background(), &datatransferpb.DeleteTransferConfigRequest{Name: created.GetName()})
	})
	// Named by the project's number, as Google names it.
	if !regexp.MustCompile(`^projects/[0-9]+/locations/us/transferConfigs/[a-f0-9-]+$`).MatchString(created.GetName()) || created.GetNextRunTime() == nil {
		t.Fatalf("created %v", created)
	}

	// A manual run for an hour ago, by the instance's clock.
	now := advanceBigQueryClock(t, h, 0)
	runTime := now.Add(-time.Hour).Truncate(time.Second)
	started, err := g.StartManualTransferRuns(ctx, &datatransferpb.StartManualTransferRunsRequest{Parent: created.GetName(),
		Time: &datatransferpb.StartManualTransferRunsRequest_RequestedRunTime{RequestedRunTime: timestamppb.New(runTime)}})
	if err != nil || len(started.GetRuns()) != 1 {
		t.Fatalf("StartManualTransferRuns: %v, %v", started, err)
	}
	waitRun(t, g, started.GetRuns()[0].GetName())
	readDaily := func(rt time.Time) []bigquery.Value {
		t.Helper()
		table := "daily_" + rt.UTC().Format("20060102")
		got := rows(t, ctx, bq, "SELECT CAST(d AS STRING), t, n FROM `"+ds.DatasetID+"."+table+"`")
		if len(got) != 1 {
			t.Fatalf("%s: %v, want one row", table, got)
		}
		return got[0]
	}
	row := readDaily(runTime)
	if row[0] != runTime.UTC().Format("2006-01-02") || !row[1].(time.Time).Equal(runTime) || row[2] != int64(4) {
		t.Fatalf("row %v, want @run_date %s, @run_time %s, 4 orders", row, runTime.Format("2006-01-02"), runTime)
	}
	logs := g.ListTransferLogs(ctx, &datatransferpb.ListTransferLogsRequest{Parent: started.GetRuns()[0].GetName()})
	if m, err := logs.Next(); err != nil || m.GetMessageText() == "" {
		t.Fatalf("ListTransferLogs: %v, %v", m, err)
	}

	// The schedule fires when the instance's clock passes next_run_time.
	next := created.GetNextRunTime().AsTime()
	advanceBigQueryClock(t, h, next.Sub(advanceBigQueryClock(t, h, 0))+time.Second)
	deadline := time.Now().Add(2 * time.Minute)
	var scheduled *datatransferpb.TransferRun
	for scheduled == nil && time.Now().Before(deadline) {
		runs := g.ListTransferRuns(ctx, &datatransferpb.ListTransferRunsRequest{Parent: created.GetName()})
		for {
			run, err := runs.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatalf("ListTransferRuns: %v", err)
			}
			if run.GetRunTime().AsTime().Equal(next) && run.GetState() == datatransferpb.TransferState_SUCCEEDED {
				scheduled = run
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if scheduled == nil {
		t.Fatalf("no SUCCEEDED run at next_run_time %s", next)
	}
	readDaily(next)

	// The deprecated backfill, then a run deleted.
	sched, err := g.ScheduleTransferRuns(ctx, &datatransferpb.ScheduleTransferRunsRequest{Parent: created.GetName(),
		StartTime: timestamppb.New(next.Add(-72 * time.Hour)), EndTime: timestamppb.New(next.Add(-47 * time.Hour))})
	if err != nil || len(sched.GetRuns()) != 2 {
		t.Fatalf("ScheduleTransferRuns: %v, %v", sched, err)
	}
	for _, run := range sched.GetRuns() {
		waitRun(t, g, run.GetName())
	}
	if err := g.DeleteTransferRun(ctx, &datatransferpb.DeleteTransferRunRequest{Name: sched.GetRuns()[0].GetName()}); err != nil {
		t.Fatalf("DeleteTransferRun: %v", err)
	}
	if _, err := g.GetTransferRun(ctx, &datatransferpb.GetTransferRunRequest{Name: sched.GetRuns()[0].GetName()}); status.Code(err) != codes.NotFound {
		t.Fatalf("deleted run: %v", err)
	}

	// Patch over REST, list, delete.
	patched, err := r.UpdateTransferConfig(ctx, &datatransferpb.UpdateTransferConfigRequest{
		TransferConfig: &datatransferpb.TransferConfig{Name: created.GetName(), DisplayName: "renamed",
			ScheduleOptions: &datatransferpb.ScheduleOptions{DisableAutoScheduling: true}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name", "schedule_options"}}})
	if err != nil || patched.GetDisplayName() != "renamed" || patched.GetNextRunTime() != nil {
		t.Fatalf("UpdateTransferConfig: %v, %v", patched, err)
	}
	found := false
	cfgs := r.ListTransferConfigs(ctx, &datatransferpb.ListTransferConfigsRequest{Parent: parent, DataSourceIds: []string{"scheduled_query"}})
	for {
		c, err := cfgs.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListTransferConfigs: %v", err)
		}
		found = found || c.GetName() == created.GetName()
	}
	if !found {
		t.Fatal("ListTransferConfigs does not list the config")
	}
	if err := r.DeleteTransferConfig(ctx, &datatransferpb.DeleteTransferConfigRequest{Name: created.GetName()}); err != nil {
		t.Fatalf("DeleteTransferConfig: %v", err)
	}
	if _, err := g.GetTransferConfig(ctx, &datatransferpb.GetTransferConfigRequest{Name: created.GetName()}); status.Code(err) != codes.NotFound {
		t.Fatalf("deleted config: %v", err)
	}
}

// TestBigQueryDataTransferDatasetCopy (#1948): a cross_region_copy config
// copies every table of the source dataset; a schedule under 12 hours is
// INVALID_ARGUMENT.
//
// covers: google.cloud.bigquery.datatransfer.v1.DataTransferService/CreateTransferConfig, google.cloud.bigquery.datatransfer.v1.DataTransferService/StartManualTransferRuns, google.cloud.bigquery.datatransfer.v1.DataTransferService/GetTransferRun, google.cloud.bigquery.datatransfer.v1.DataTransferService/DeleteTransferConfig
func TestBigQueryDataTransferDatasetCopy(t *testing.T) {
	h := New(t)
	g, _ := dataTransferClients(t, h)
	bq, project := bigqueryClient(t, h)
	ctx := h.Context()
	src, _ := seedOrders(t, h, bq)
	dst := bq.Dataset(src.DatasetID + "_copy")
	if err := dst.Create(ctx, &bigquery.DatasetMetadata{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dst.DeleteWithContents(context.Background()) })
	params := mustStruct(t, map[string]any{"source_project_id": project, "source_dataset_id": src.DatasetID, "overwrite_destination_table": "true"})
	dest := &datatransferpb.TransferConfig_DestinationDatasetId{DestinationDatasetId: dst.DatasetID}
	_, err := g.CreateTransferConfig(ctx, &datatransferpb.CreateTransferConfigRequest{Parent: "projects/" + project,
		TransferConfig: &datatransferpb.TransferConfig{DataSourceId: "cross_region_copy", Schedule: "every 6 hours", Params: params, Destination: dest}})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("every 6 hours: %v, want INVALID_ARGUMENT", err)
	}
	cfg, err := g.CreateTransferConfig(ctx, &datatransferpb.CreateTransferConfigRequest{Parent: "projects/" + project,
		TransferConfig: &datatransferpb.TransferConfig{DisplayName: "copy", DataSourceId: "cross_region_copy", Params: params, Destination: dest}})
	if err != nil {
		t.Fatalf("CreateTransferConfig: %v", err)
	}
	t.Cleanup(func() {
		_ = g.DeleteTransferConfig(context.Background(), &datatransferpb.DeleteTransferConfigRequest{Name: cfg.GetName()})
	})
	started, err := g.StartManualTransferRuns(ctx, &datatransferpb.StartManualTransferRunsRequest{Parent: cfg.GetName(),
		Time: &datatransferpb.StartManualTransferRunsRequest_RequestedRunTime{RequestedRunTime: timestamppb.New(advanceBigQueryClock(t, h, 0))}})
	if err != nil {
		t.Fatalf("StartManualTransferRuns: %v", err)
	}
	waitRun(t, g, started.GetRuns()[0].GetName())
	got := rows(t, ctx, bq, "SELECT COUNT(*) FROM `"+dst.DatasetID+".orders`")
	if len(got) != 1 || got[0][0] != int64(4) {
		t.Fatalf("copied orders: %v, want 4 rows", got)
	}
}
