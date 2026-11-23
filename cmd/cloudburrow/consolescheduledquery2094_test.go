package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/sched"
	"github.com/cloudburrow/cloudburrow/internal/service/datatransfer"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestStudioScheduledQueryPane (#2094): BigQuery Studio's New scheduled
// query pane saves a scheduled query, reads it back for Update scheduled
// query and updates it (On-demand, a new query); a scheduled query's page
// has Edit into the editor; refusals name the field.
func TestStudioScheduledQueryPane(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	api, err := datatransfer.NewServer(store.NewMemory(), sched.NewFakeClock(time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)), &consoleDTExec{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sq := dataTransferProvider{svc: &dataTransferSvc{api: api}, only: datatransfer.ScheduledQuery}
	call := func(c string, f scheduledQueryForm) (scheduledQueryForm, error) {
		b, _ := json.Marshal(f)
		out, err := sq.StudioCall(ctx, "p-test", c, b)
		if err != nil {
			return scheduledQueryForm{}, err
		}
		return out.(scheduledQueryForm), nil
	}
	if _, err := call("save", scheduledQueryForm{Query: "SELECT 1"}); err == nil || !strings.Contains(err.Error(), "Name for the scheduled query") {
		t.Errorf("a nameless save = %v", err)
	}
	if _, err := call("save", scheduledQueryForm{DisplayName: "x", Query: "SELECT 1", Schedule: "every day 01:00", Table: "t"}); err == nil {
		t.Error("a destination table without a dataset was saved")
	}
	made, err := call("save", scheduledQueryForm{DisplayName: "weekly", Query: "SELECT 1", Schedule: "every tuesday 09:30",
		StartTime: "2026-03-03T00:00:00Z", Dataset: "ds", Table: "out", PartitionField: "d", WriteDisposition: "WRITE_TRUNCATE", Email: true})
	if err != nil {
		t.Fatal(err)
	}
	got, err := call("get", scheduledQueryForm{Name: made.Name})
	if err != nil {
		t.Fatal(err)
	}
	if got.DisplayName != "weekly" || got.Schedule != "every tuesday 09:30" || got.Table != "out" || got.PartitionField != "d" ||
		got.WriteDisposition != "WRITE_TRUNCATE" || got.Dataset != "ds" || !got.Email || got.StartTime != "2026-03-03T00:00:00Z" || got.OnDemand {
		t.Errorf("read back %+v", got)
	}
	got.Query, got.OnDemand = "SELECT 2", true
	up, err := call("save", got)
	if err != nil {
		t.Fatal(err)
	}
	if up.Query != "SELECT 2" || !up.OnDemand || up.Name != made.Name {
		t.Errorf("updated %+v", up)
	}
	acts := sq.DetailActions(ctx, "p-test", []string{made.ID})
	if len(acts) == 0 || acts[0].Label != "Edit" || !strings.HasPrefix(acts[0].Href, "/bigquery-studio?scheduledQuery=") {
		t.Errorf("the page's actions %+v have no Edit into the editor", acts)
	}
	if _, err := (dataTransferProvider{svc: &dataTransferSvc{api: api}}).StudioCall(ctx, "p-test", "get", []byte("{}")); err == nil {
		t.Error("Data transfers answered the scheduled query pane")
	}
}
