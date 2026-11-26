package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	alloydbpb "cloud.google.com/go/alloydb/apiv1/alloydbpb"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/alloydb"
)

// TestConsoleAlloyDBInsights2253 holds round 3's pages to Google's without
// a server: System insights' Log errors card on each view, saying why it
// has no count without Cloud Logging (the events timeline's USER_CREATE
// needs a serving primary: TestAlloyDBInsightsRound3ThroughThePage2253); each instance's Tags subtab beside its Queries; the query and tag
// details' paths; and a sampled plan's operations from auto_explain's JSON.
func TestConsoleAlloyDBInsights2253(t *testing.T) {
	ctx := context.Background()
	api, err := alloydb.NewServer("", "", alloydb.Options{ConnectorHost: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	svc := &alloyDBService{api: api, engine: newAlloyDBEngine(config.Config{}, nil)}
	metrics, _, _ := monitoringAdminFixture(t)
	p := alloyDBProvider{svc: svc, metrics: metrics}
	_, _ = p.Create(ctx, "p1", map[string]string{"clusterId": "c1", "password": "pw", "version": "POSTGRES_16", "region": "us-central1",
		"network": "default", "instanceId": "primary", "cpu": "2"})
	name := "projects/p1/locations/us-central1/clusters/c1"
	d, err := p.Detail(ctx, "p1", []string{name})
	if err != nil {
		t.Fatal(err)
	}
	var si console.Section
	for _, s := range d.Sections {
		if s.ID == "system-insights" {
			si = s
		}
	}
	cards := 0
	for _, tab := range si.Tabs {
		for _, g := range tab.Groups {
			for _, pr := range g.Properties {
				if pr.Label == "Log errors" {
					cards++
					if !strings.Contains(pr.Value, "Cloud Logging is not served") || pr.Link != "" {
						t.Errorf("Log errors without Cloud Logging = %+v", pr)
					}
				}
			}
		}
	}
	if cards != len(si.Tabs) || cards == 0 {
		t.Errorf("Log errors is on %d of %d System insights views", cards, len(si.Tabs))
	}
	// The Tags subtab, beside an instance's Queries (no instance serves
	// here, so one is given).
	in := &alloydbpb.Instance{Name: name + "/instances/primary", State: alloydbpb.Instance_READY, InstanceType: alloydbpb.Instance_PRIMARY,
		QueryInsightsConfig: &alloydbpb.Instance_QueryInsightsInstanceConfig{RecordApplicationTags: proto.Bool(true)}}
	tab := alloyDBTagsTab(api, &alloydbpb.Cluster{Name: name}, in)
	if tab.ID != "qt-primary" || !strings.Contains(tab.Listing.Prompt, "No readings yet") {
		t.Errorf("the Tags subtab = %+v", tab)
	}
	tags := tab.Listing
	if tags.NameColumn != "Action" || strings.Join(tags.Columns, "|") != "Controller|Framework|Route|Application|DB Driver|Database|Load by total time|Avg execution time (ms)|Times called" {
		t.Errorf("the Tags table's columns = %s %v", tags.NameColumn, tags.Columns)
	}
	// The details' paths are served (the instance's server is not, here).
	for _, path := range [][]string{{name, "query", "primary", "123"}, {name, "tag", "primary", "abc"}, {name, "query", "primary", "123", "plan", "1"}} {
		dd, err := p.Detail(ctx, "p1", path)
		if err != nil || dd.Unavailable == "" && len(dd.Sections) == 0 {
			t.Errorf("Detail(%v) = %+v, %v", path, dd, err)
		}
	}

	// A plan sample's operations, depth-first, each with its latency,
	// rows, loops, cost and shared blocks.
	plan := `{"Query Text":"SELECT name FROM t WHERE id < 5","Planning Time":0.1,"Plan":{"Node Type":"Limit","Startup Cost":0,"Total Cost":9.5,
		"Plan Rows":4,"Actual Total Time":0.05,"Actual Rows":4,"Actual Loops":1,"Shared Hit Blocks":2,"Shared Read Blocks":0,
		"Plans":[{"Node Type":"Index Scan","Relation Name":"t","Index Name":"t_pkey","Startup Cost":0.1,"Total Cost":9.4,"Plan Rows":4,
		"Actual Total Time":0.04,"Actual Rows":4,"Actual Loops":1,"Shared Hit Blocks":2,"Shared Read Blocks":0}]}}`
	page := alloyDBPlanPage(alloydb.PlanSample{At: time.Unix(1, 0), Database: "db", User: "u", QueryID: 7, DurationMs: 0.2, Plan: json.RawMessage(plan)})
	ops := page.Sections[0].Listing.Items
	if len(ops) != 2 || ops[0].Name != "Limit" || ops[1].Name != "→ Index Scan" || ops[1].Fields["Relation"] != "t t_pkey" ||
		ops[1].Fields["Latency (ms)"] != "0.040" || ops[1].Fields["Rows returned"] != "4" || ops[1].Fields["Cost"] != "0.10..9.40" ||
		ops[0].Fields["Shared hit blocks"] != "2" || ops[0].Fields["Loops"] != "1" {
		t.Errorf("plan operations = %+v", ops)
	}
	if page.Summary[0].Value != "SELECT name FROM t WHERE id < 5" {
		t.Errorf("plan summary = %+v", page.Summary)
	}
}
