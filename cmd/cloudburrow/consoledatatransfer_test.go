package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/sched"
	"github.com/cloudburrow/cloudburrow/internal/service/datatransfer"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

type consoleDTExec struct{ queries int }

func (e *consoleDTExec) DatasetLocation(context.Context, string, string) (string, error) {
	return "US", nil
}

func (e *consoleDTExec) RunQuery(context.Context, datatransfer.QueryJob) (string, int64, error) {
	e.queries++
	return "job_1", 1, nil
}

func (e *consoleDTExec) CopyTables(context.Context, datatransfer.CopyJob) ([]string, error) {
	return []string{"t"}, nil
}

// TestConsoleDataTransfer (#1948): Data transfers and Scheduled queries
// list, create, run now, backfill, disable, delete a run and delete,
// through the service's own methods.
func TestConsoleDataTransfer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := sched.NewFakeClock(time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC))
	ex := &consoleDTExec{}
	api, err := datatransfer.NewServer(store.NewMemory(), clock, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	svc := &dataTransferSvc{api: api}
	sq := dataTransferProvider{svc: svc, only: datatransfer.ScheduledQuery}
	all := dataTransferProvider{svc: svc}
	if sq.ID() != "bigquery-scheduled-queries" || all.ID() != "bigquery-transfers" {
		t.Fatalf("ids %s %s", sq.ID(), all.ID())
	}
	name, err := sq.Create(ctx, "p-test", map[string]string{"display_name": "nightly", "schedule": "every 24 hours",
		"dataset": "ds", "query": "SELECT @run_date", "table": "out_{run_date}", "write_disposition": "WRITE_TRUNCATE"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := all.Create(ctx, "p-test", map[string]string{"source": datatransfer.CrossRegionCopy, "display_name": "copy",
		"dataset": "dst", "source_project": "p-test", "source_dataset": "src"}); err != nil {
		t.Fatal(err)
	}
	l, err := sq.List(ctx, "p-test")
	if err != nil || len(l.Items) != 1 || l.Items[0].Fields["Source"] != "Scheduled Query" {
		t.Fatalf("scheduled queries: %+v, %v", l, err)
	}
	if l, _ := all.List(ctx, "p-test"); len(l.Items) != 2 {
		t.Fatalf("data transfers: %+v", l.Items)
	}
	if err := sq.ActAt(ctx, "p-test", []string{name}, "run", nil); err != nil {
		t.Fatal(err)
	}
	if err := sq.ActAt(ctx, "p-test", []string{name}, "backfill", map[string]string{"start": "2026-02-01", "end": "2026-02-03"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		api.Tick(ctx)
	}
	if ex.queries != 3 {
		t.Fatalf("%d queries ran, want 3", ex.queries)
	}
	d, err := sq.Detail(ctx, "p-test", []string{name})
	if err != nil || d.Sections[0].ID != "runs" || len(d.Sections[0].Listing.Items) != 3 {
		t.Fatalf("detail: %+v, %v", d, err)
	}
	acts := sq.DetailActions(ctx, "p-test", []string{name})
	var del string
	for _, a := range acts {
		if a.ID == "delete-run" {
			del = a.Fields[0].Options[0]
		}
	}
	if del == "" {
		t.Fatalf("no Delete run in %+v", acts)
	}
	if err := sq.ActAt(ctx, "p-test", []string{name}, "delete-run", map[string]string{"run": del}); err != nil {
		t.Fatal(err)
	}
	if err := sq.ActAt(ctx, "p-test", []string{name}, "disable", nil); err != nil {
		t.Fatal(err)
	}
	if l, _ := sq.List(ctx, "p-test"); l.Items[0].Status != "DISABLED" {
		t.Fatalf("after disable: %+v", l.Items[0])
	}
	if err := sq.Delete(ctx, "other", name); err == nil || !strings.Contains(err.Error(), "not a transfer") {
		t.Fatalf("cross-project delete: %v", err)
	}
	if err := sq.Delete(ctx, "p-test", name); err != nil {
		t.Fatal(err)
	}
}

// TestDataTransferRepeatFrequency (#2094): Create transfer's Repeat
// frequency, Google's Hours, Days, Weeks, Months, Custom and On-demand,
// as the schedule the service runs.
func TestDataTransferRepeatFrequency(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		repeat, frequency, custom, want string
		onDemand, fails                 bool
	}{
		{repeat: "Hours", frequency: "6", want: "every 6 hours"},
		{repeat: "Days", frequency: "2", want: "every 48 hours"},
		{repeat: "Weeks", frequency: "1", want: "every 168 hours"},
		{repeat: "Months", frequency: "1", want: "1 of month 00:00"},
		{repeat: "Months", frequency: "3", want: "1 of jan,apr,jul,oct 00:00"},
		{repeat: "Months", frequency: "5", fails: true},
		{repeat: "Days", frequency: "0", fails: true},
		{repeat: "Custom", custom: "every mon 09:00", want: "every mon 09:00"},
		{repeat: "", custom: "every 24 hours", want: "every 24 hours"},
		{repeat: "On-demand", onDemand: true},
		{repeat: "Yearly", frequency: "1", fails: true},
	} {
		got, onDemand, err := dtRepeat(map[string]string{"repeat": c.repeat, "frequency": c.frequency, "schedule": c.custom})
		if (err != nil) != c.fails || got != c.want || onDemand != c.onDemand {
			t.Errorf("%s every %s: %q, on-demand %v, %v", c.repeat, c.frequency, got, onDemand, err)
		}
	}
}
