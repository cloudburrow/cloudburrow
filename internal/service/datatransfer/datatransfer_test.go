package datatransfer

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	dtpb "cloud.google.com/go/bigquery/datatransfer/apiv1/datatransferpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/sched"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestScheduleNext(t *testing.T) {
	t.Parallel()
	anchor := at("2026-03-02T10:17:00Z") // a Monday
	for _, c := range []struct{ sched, after, want string }{
		{"every 24 hours", "2026-03-02T10:17:00Z", "2026-03-03T10:17:00Z"},
		{"every 3 hours", "2026-03-02T11:00:00Z", "2026-03-02T13:17:00Z"},
		{"every 15 minutes", "2026-03-02T10:20:00Z", "2026-03-02T10:32:00Z"},
		{"every monday 09:00", "2026-03-02T10:00:00Z", "2026-03-09T09:00:00Z"},
		{"every mon 23:30", "2026-03-02T10:00:00Z", "2026-03-02T23:30:00Z"},
		{"every mon,wed 09:03", "2026-03-02T10:00:00Z", "2026-03-04T09:03:00Z"},
		{"every day 00:00", "2026-03-02T10:00:00Z", "2026-03-03T00:00:00Z"},
		{"1st sunday of sep,oct,nov 00:00", "2026-03-02T10:00:00Z", "2026-09-06T00:00:00Z"},
		{"last friday of month 18:00", "2026-03-02T10:00:00Z", "2026-03-27T18:00:00Z"},
		{"1,15 of month 09:00", "2026-03-02T10:00:00Z", "2026-03-15T09:00:00Z"},
		{"every 2 hours synchronized", "2026-03-02T10:17:00Z", "2026-03-02T12:00:00Z"},
		{"every 30 minutes from 08:00 to 09:00", "2026-03-02T10:00:00Z", "2026-03-03T08:00:00Z"},
		{"every hour on mon,tue", "2026-03-03T23:30:00Z", "2026-03-09T00:17:00Z"},
	} {
		s, err := ParseSchedule(c.sched)
		if err != nil {
			t.Errorf("%q: %v", c.sched, err)
			continue
		}
		if got := s.Next(at(c.after), anchor); !got.Equal(at(c.want)) {
			t.Errorf("%q after %s = %s, want %s", c.sched, c.after, got.Format(time.RFC3339), c.want)
		}
	}
	for _, bad := range []string{"", "sometimes", "every 0 hours", "every funday 09:00", "every day 25:00", "every 2 hours from 08:00 to 09:00 synchronized"} {
		if _, err := ParseSchedule(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestRenderTableTemplate(t *testing.T) {
	t.Parallel()
	rt := at("2018-02-15T00:01:00Z")
	for tmpl, want := range map[string]string{
		`mytable_{run_time|"%Y%m%d"}`:        "mytable_20180215",
		`mytable_{run_time+25h|"%Y%m%d"}`:    "mytable_20180216",
		`mytable_{run_time-1h|"%Y%m%d"}`:     "mytable_20180214",
		`mytable_{run_time+1.5h|"%Y%m%d%H"}`: "mytable_2018021501",
		`mytable_{run_date}`:                 "mytable_20180215",
		`plain`:                              "plain",
	} {
		got, err := RenderTableTemplate(tmpl, rt)
		if err != nil || got != want {
			t.Errorf("%s = %q, %v; want %q", tmpl, got, err, want)
		}
	}
	for _, bad := range []string{`t_{run_time +1h|"%Y"}`, `t_{run_time+1d|"%Y"}`, `t_{oops}`, `t_{run_date`} {
		if _, err := RenderTableTemplate(bad, rt); err == nil {
			t.Errorf("%s rendered", bad)
		}
	}
}

type fakeExec struct {
	mu      sync.Mutex
	queries []QueryJob
	copies  []CopyJob
	fail    error
}

func (f *fakeExec) DatasetLocation(_ context.Context, _, dataset string) (string, error) {
	if dataset == "missing" {
		return "", apierror.NotFound("Not found: Dataset")
	}
	return "US", nil
}

func (f *fakeExec) RunQuery(_ context.Context, j QueryJob) (string, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, j)
	return "job1", 3, f.fail
}

func (f *fakeExec) CopyTables(_ context.Context, j CopyJob) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.copies = append(f.copies, j)
	return []string{"a", "b"}, f.fail
}

type fakePub struct {
	mu   sync.Mutex
	msgs []map[string]string
}

func (p *fakePub) Publish(_ context.Context, _ string, _ []byte, attrs map[string]string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.msgs = append(p.msgs, attrs)
	return nil
}

func newTestServer(t *testing.T) (*Server, *sched.FakeClock, *fakeExec, *fakePub) {
	t.Helper()
	clock := sched.NewFakeClock(at("2026-03-02T10:00:00Z"))
	ex, pub := &fakeExec{}, &fakePub{}
	s, err := NewServer(store.NewMemory(), clock, ex, pub)
	if err != nil {
		t.Fatal(err)
	}
	return s, clock, ex, pub
}

func queryConfig(params map[string]any) *dtpb.TransferConfig {
	p, _ := structpb.NewStruct(params)
	return &dtpb.TransferConfig{DisplayName: "q", DataSourceId: ScheduledQuery, Params: p, Schedule: "every 24 hours",
		Destination: &dtpb.TransferConfig_DestinationDatasetId{DestinationDatasetId: "ds"}}
}

func TestScheduledQueryRunsOnTheClock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, clock, ex, pub := newTestServer(t)
	cfg := queryConfig(map[string]any{"query": "SELECT @run_date AS d", "destination_table_name_template": `out_{run_time|"%Y%m%d"}`, "write_disposition": "WRITE_TRUNCATE"})
	cfg.NotificationPubsubTopic = "projects/p-test/topics/done"
	got, err := s.CreateTransferConfig(ctx, &dtpb.CreateTransferConfigRequest{Parent: "projects/p-test", TransferConfig: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.GetName(), "projects/"+ProjectNumber("p-test")+"/locations/us/transferConfigs/") || got.GetDatasetRegion() != "us" {
		t.Fatalf("name %s region %s", got.GetName(), got.GetDatasetRegion())
	}
	if !got.GetNextRunTime().AsTime().Equal(at("2026-03-03T10:00:00Z")) {
		t.Fatalf("next run %v", got.GetNextRunTime().AsTime())
	}
	s.Tick(ctx)
	if len(ex.queries) != 0 {
		t.Fatal("ran before it was due")
	}
	clock.Advance(24 * time.Hour)
	s.Tick(ctx)
	if len(ex.queries) != 1 || ex.queries[0].DestTable != "out_20260303" || ex.queries[0].WriteDisposition != "WRITE_TRUNCATE" {
		t.Fatalf("queries %+v", ex.queries)
	}
	runs, _ := s.ListTransferRuns(ctx, &dtpb.ListTransferRunsRequest{Parent: got.GetName()})
	if len(runs.GetTransferRuns()) != 1 || runs.GetTransferRuns()[0].GetState() != dtpb.TransferState_SUCCEEDED {
		t.Fatalf("runs %v", runs)
	}
	logs, _ := s.ListTransferLogs(ctx, &dtpb.ListTransferLogsRequest{Parent: runs.GetTransferRuns()[0].GetName()})
	if len(logs.GetTransferMessages()) < 3 {
		t.Fatalf("logs %v", logs)
	}
	if len(pub.msgs) != 1 || pub.msgs[0]["eventType"] != "TRANSFER_RUN_FINISHED" {
		t.Fatalf("notifications %v", pub.msgs)
	}

	// A manual run for a past time, and a backfill of three days.
	if _, err := s.StartManualTransferRuns(ctx, &dtpb.StartManualTransferRunsRequest{Parent: got.GetName(),
		Time: &dtpb.StartManualTransferRunsRequest_RequestedTimeRange{RequestedTimeRange: &dtpb.StartManualTransferRunsRequest_TimeRange{
			StartTime: timestamppb.New(at("2026-02-01T00:00:00Z")), EndTime: timestamppb.New(at("2026-02-04T00:00:00Z"))}}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		s.Tick(ctx)
	}
	if len(ex.queries) != 4 || ex.queries[1].DestTable != "out_20260201" {
		t.Fatalf("backfill queries %+v", ex.queries)
	}
	if _, err := s.StartManualTransferRuns(ctx, &dtpb.StartManualTransferRunsRequest{Parent: got.GetName(),
		Time: &dtpb.StartManualTransferRunsRequest_RequestedRunTime{RequestedRunTime: timestamppb.New(at("2027-01-01T00:00:00Z"))}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("future run time: %v", err)
	}
}

func TestFailedRunEmailsAndStates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _, ex, _ := newTestServer(t)
	ex.fail = apierror.InvalidArgument("Syntax error")
	cfg := queryConfig(map[string]any{"query": "SELEC", "destination_table_name_template": "t"})
	cfg.EmailPreferences = &dtpb.EmailPreferences{EnableFailureEmail: true}
	cfg.ScheduleOptions = &dtpb.ScheduleOptions{DisableAutoScheduling: true}
	got, err := s.CreateTransferConfig(ctx, &dtpb.CreateTransferConfigRequest{Parent: "projects/p-test/locations/us", TransferConfig: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetNextRunTime() != nil {
		t.Fatal("auto scheduling disabled but a next run is set")
	}
	if _, err := s.StartManualTransferRuns(ctx, &dtpb.StartManualTransferRunsRequest{Parent: got.GetName()}); err != nil {
		t.Fatal(err)
	}
	s.Tick(ctx)
	c, _ := s.GetTransferConfig(ctx, &dtpb.GetTransferConfigRequest{Name: got.GetName()})
	if c.GetState() != dtpb.TransferState_FAILED || c.GetError().GetCode() != int32(codes.InvalidArgument) {
		t.Fatalf("config state %v error %v", c.GetState(), c.GetError())
	}
	if len(s.Emails()) != 1 {
		t.Fatalf("emails %v", s.Emails())
	}
}

func TestValidation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _, _, _ := newTestServer(t)
	for name, c := range map[string]*dtpb.TransferConfig{
		"no query":        queryConfig(map[string]any{"destination_table_name_template": "t"}),
		"no template":     queryConfig(map[string]any{"query": "SELECT 1"}),
		"bad disposition": queryConfig(map[string]any{"query": "SELECT 1", "destination_table_name_template": "t", "write_disposition": "WRITE_EMPTY"}),
		"too often": func() *dtpb.TransferConfig {
			c := queryConfig(map[string]any{"query": "SELECT 1", "destination_table_name_template": "t"})
			c.Schedule = "every 1 minutes"
			return c
		}(),
		"copy too often": func() *dtpb.TransferConfig {
			p, _ := structpb.NewStruct(map[string]any{"source_project_id": "p-src", "source_dataset_id": "src"})
			return &dtpb.TransferConfig{DataSourceId: CrossRegionCopy, Params: p, Schedule: "every 6 hours",
				Destination: &dtpb.TransferConfig_DestinationDatasetId{DestinationDatasetId: "ds"}}
		}(),
		"unknown source": {DataSourceId: "google_ads"},
		"bad schedule": func() *dtpb.TransferConfig {
			c := queryConfig(map[string]any{"query": "SELECT 1", "destination_table_name_template": "t"})
			c.Schedule = "whenever"
			return c
		}(),
	} {
		if _, err := s.CreateTransferConfig(ctx, &dtpb.CreateTransferConfigRequest{Parent: "projects/p-test", TransferConfig: c}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v, want INVALID_ARGUMENT", name, err)
		}
	}
	missing := queryConfig(map[string]any{"query": "SELECT 1", "destination_table_name_template": "t"})
	missing.Destination = &dtpb.TransferConfig_DestinationDatasetId{DestinationDatasetId: "missing"}
	if _, err := s.CreateTransferConfig(ctx, &dtpb.CreateTransferConfigRequest{Parent: "projects/p-test", TransferConfig: missing}); status.Code(err) != codes.NotFound {
		t.Errorf("missing dataset: %v", err)
	}
	if _, err := s.GetTransferConfig(ctx, &dtpb.GetTransferConfigRequest{Name: "projects/p-test/locations/us/transferConfigs/nope"}); status.Code(err) != codes.NotFound {
		t.Errorf("get missing: %v", err)
	}
	if r, err := s.CheckValidCreds(ctx, &dtpb.CheckValidCredsRequest{Name: "projects/p-test/locations/us/dataSources/scheduled_query"}); err != nil || !r.GetHasValidCreds() {
		t.Errorf("CheckValidCreds = %v, %v", r, err)
	}
	if _, err := s.CheckValidCreds(ctx, &dtpb.CheckValidCredsRequest{Name: "projects/p-test/dataSources/google_ads"}); status.Code(err) != codes.NotFound {
		t.Errorf("CheckValidCreds of an unserved source: %v", err)
	}
	// schedule_options_v2, as bq sends it, under bq's "locations/-".
	v2 := queryConfig(map[string]any{"query": "SELECT 1", "destination_table_name_template": "t"})
	v2.Schedule = ""
	v2.ScheduleOptionsV2 = &dtpb.ScheduleOptionsV2{Schedule: &dtpb.ScheduleOptionsV2_TimeBasedSchedule{
		TimeBasedSchedule: &dtpb.TimeBasedSchedule{Schedule: "every 2 hours"}}}
	gotV2, err := s.CreateTransferConfig(ctx, &dtpb.CreateTransferConfigRequest{Parent: "projects/p-test/locations/-", TransferConfig: v2})
	if err != nil || gotV2.GetSchedule() != "every 2 hours" || gotV2.GetNextRunTime() == nil || !strings.Contains(gotV2.GetName(), "/locations/us/") {
		t.Errorf("create with time_based_schedule = %v, %v", gotV2, err)
	}
	v2.ScheduleOptionsV2 = &dtpb.ScheduleOptionsV2{Schedule: &dtpb.ScheduleOptionsV2_ManualSchedule{ManualSchedule: &dtpb.ManualSchedule{}}}
	if got, err := s.CreateTransferConfig(ctx, &dtpb.CreateTransferConfigRequest{Parent: "projects/p-test", TransferConfig: v2}); err != nil ||
		!got.GetScheduleOptions().GetDisableAutoScheduling() || got.GetNextRunTime() != nil {
		t.Errorf("create with manual_schedule = %v, %v", got, err)
	}
	v2.ScheduleOptionsV2 = &dtpb.ScheduleOptionsV2{Schedule: &dtpb.ScheduleOptionsV2_EventDrivenSchedule{EventDrivenSchedule: &dtpb.EventDrivenSchedule{}}}
	if _, err := s.CreateTransferConfig(ctx, &dtpb.CreateTransferConfigRequest{Parent: "projects/p-test", TransferConfig: v2}); status.Code(err) != codes.Unimplemented {
		t.Errorf("create with event_driven_schedule: %v", err)
	}
	// A parent by number resolves through ResolveProjects.
	s.ResolveProjects(func(n string) (string, bool) { return "p-test", n == ProjectNumber("p-test") })
	byNumber, err := s.CreateTransferConfig(ctx, &dtpb.CreateTransferConfigRequest{Parent: "projects/" + ProjectNumber("p-test"),
		TransferConfig: queryConfig(map[string]any{"query": "SELECT 1", "destination_table_name_template": "t"})})
	if err != nil {
		t.Fatal(err)
	}
	// And the config is found by the project's ID too.
	byID := strings.Replace(byNumber.GetName(), ProjectNumber("p-test"), "p-test", 1)
	if _, err := s.GetTransferConfig(ctx, &dtpb.GetTransferConfigRequest{Name: byID}); err != nil {
		t.Errorf("get by project ID: %v", err)
	}
	if _, err := s.CreateTransferConfig(ctx, &dtpb.CreateTransferConfigRequest{Parent: "projects/999",
		TransferConfig: queryConfig(map[string]any{"query": "SELECT 1", "destination_table_name_template": "t"})}); status.Code(err) != codes.NotFound {
		t.Errorf("unknown project number: %v", err)
	}
}

func TestCopyAndPatchAndDelete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, clock, ex, _ := newTestServer(t)
	p, _ := structpb.NewStruct(map[string]any{"source_project_id": "p-src", "source_dataset_id": "src", "overwrite_destination_table": "true"})
	got, err := s.CreateTransferConfig(ctx, &dtpb.CreateTransferConfigRequest{Parent: "projects/p-test", TransferConfig: &dtpb.TransferConfig{
		DisplayName: "copy", DataSourceId: CrossRegionCopy, Params: p,
		Destination: &dtpb.TransferConfig_DestinationDatasetId{DestinationDatasetId: "dst"}}})
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(24 * time.Hour)
	s.Tick(ctx)
	if len(ex.copies) != 1 || !ex.copies[0].Overwrite || ex.copies[0].SourceDataset != "src" {
		t.Fatalf("copies %+v", ex.copies)
	}
	// Patch through the location-less name.
	short := strings.Replace(got.GetName(), "/locations/us", "", 1)
	up, err := s.UpdateTransferConfig(ctx, &dtpb.UpdateTransferConfigRequest{
		TransferConfig: &dtpb.TransferConfig{Name: short, DisplayName: "renamed", Disabled: true},
		UpdateMask:     &fieldmaskpb.FieldMask{Paths: []string{"displayName", "disabled"}}})
	if err != nil {
		t.Fatal(err)
	}
	if up.GetDisplayName() != "renamed" || up.GetNextRunTime() != nil {
		t.Fatalf("patched %v", up)
	}
	list, _ := s.ListTransferConfigs(ctx, &dtpb.ListTransferConfigsRequest{Parent: "projects/p-test", DataSourceIds: []string{CrossRegionCopy}})
	if len(list.GetTransferConfigs()) != 1 {
		t.Fatalf("list %v", list)
	}
	if _, err := s.DeleteTransferConfig(ctx, &dtpb.DeleteTransferConfigRequest{Name: got.GetName()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetTransferConfig(ctx, &dtpb.GetTransferConfigRequest{Name: got.GetName()}); status.Code(err) != codes.NotFound {
		t.Fatalf("after delete: %v", err)
	}
}

func TestPersistsAcrossRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := store.NewMemory()
	clock := sched.NewFakeClock(at("2026-03-02T10:00:00Z"))
	s, _ := NewServer(db, clock, &fakeExec{}, nil)
	got, err := s.CreateTransferConfig(ctx, &dtpb.CreateTransferConfigRequest{Parent: "projects/p-test",
		TransferConfig: queryConfig(map[string]any{"query": "SELECT 1", "destination_table_name_template": "t"})})
	if err != nil {
		t.Fatal(err)
	}
	s2, err := NewServer(db, clock, &fakeExec{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.GetTransferConfig(ctx, &dtpb.GetTransferConfigRequest{Name: got.GetName()}); err != nil {
		t.Fatal(err)
	}
}

func TestRoutesBindTheAPI(t *testing.T) {
	t.Parallel()
	want := map[string]bool{"CreateTransferConfig": false, "StartManualTransferRuns": false, "ListTransferLogs": false, "ListLocations": false}
	for _, r := range Routes() {
		for k := range want {
			if r.MethodName == k {
				want[k] = true
			}
		}
	}
	for k, ok := range want {
		if !ok {
			t.Errorf("no route for %s", k)
		}
	}
}
