package main

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/service/logging"
)

// Log Analytics (#1863): the Log views panel lists each log view of a
// bucket with Log Analytics on, by the name a query gives it, and a query
// naming `PROJECT.LOCATION.BUCKET.VIEW` is rewritten to the view of the
// bucket's linked dataset, or, with no link (#2021), to a query dataset
// made for the query and dropped after it; a bucket without Log Analytics,
// or a view it does not have, is refused before BigQuery is asked.
func TestLogAnalyticsRewritesLogViewsToTheLinkedDataset(t *testing.T) {
	_, svc := loggingConsole(t)
	made := map[string]bool{}
	svc.admin.UseLinkedDatasets(consoleLinked(made))
	ctx := context.Background()
	const project = "proj-1"
	buckets := loggingBucketsProvider{loggingAdminScreen{svc: svc}}
	p := loggingAnalyticsProvider{loggingAdminScreen: loggingAdminScreen{svc: svc}}
	name, err := buckets.Create(ctx, project, map[string]string{"name": "app", "location": "global", "retention": "30", "analytics": "true"})
	if err != nil {
		t.Fatal(err)
	}
	const ref = "`proj-1.global.app._AllLogs`"
	got, drops, err := p.rewrite(ctx, "SELECT 1 FROM "+ref+" JOIN "+ref+" USING (insert_id)", "fallback")
	if err != nil || len(drops) != 1 {
		t.Fatalf("a bucket with no link = %q, %d drops, %v; want one query dataset", got, len(drops), err)
	}
	ds := strings.TrimPrefix(strings.Split(got, "`")[1], "proj-1.")
	ds = strings.TrimSuffix(ds, "._AllLogs")
	if !strings.HasPrefix(ds, logging.AnalyticsDatasetPrefix) || !made["proj-1:"+ds] || strings.Count(got, ds) != 2 {
		t.Errorf("the query dataset of %q = %q (made %v)", got, ds, made)
	}
	drops[0]()
	if made["proj-1:"+ds] {
		t.Error("the query dataset was not dropped")
	}
	if _, _, err := p.rewrite(ctx, "SELECT 1", ""); err == nil || !strings.Contains(err.Error(), "PROJECT_ID.LOCATION.BUCKET_ID.LOG_VIEW_ID") {
		t.Errorf("a query naming no log view = %v", err)
	}
	if _, _, err := p.rewrite(ctx, "SELECT 1 FROM `proj-1.global._Default._AllLogs`", ""); err == nil || !strings.Contains(err.Error(), "not upgraded") {
		t.Errorf("_Default without Log Analytics = %v", err)
	}
	if err := buckets.ActAt(ctx, project, []string{name}, "create-link", map[string]string{"linkId": "app-link"}); err != nil {
		t.Fatal(err)
	}
	got, _, err = p.rewrite(ctx, "SELECT severity FROM "+ref+" WHERE severity = 'ERROR'", "")
	if err != nil || got != "SELECT severity FROM `proj-1.app_link._AllLogs` WHERE severity = 'ERROR'" {
		t.Errorf("rewrite = %q, %v", got, err)
	}
	if _, _, err := p.rewrite(ctx, "SELECT 1 FROM `proj-1.global.app.nope`", ""); err == nil || !strings.Contains(err.Error(), "no log view nope") {
		t.Errorf("an absent view = %v", err)
	}
	if _, err := p.Query(ctx, project, nil, "DELETE FROM "+ref+" WHERE true"); err == nil {
		t.Error("a DELETE was run")
	}

	l, err := p.List(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Items) != 1 || l.Items[0].Name != "proj-1.global.app._AllLogs" || !strings.Contains(l.Items[0].Fields["Linked dataset in BigQuery"], "app_link") {
		t.Errorf("the Log views panel = %+v", l.Items)
	}
}
