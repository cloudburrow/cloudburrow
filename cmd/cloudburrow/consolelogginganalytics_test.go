package main

import (
	"context"
	"strings"
	"testing"
)

// Log Analytics (#1863): the Log views panel lists each log view of a
// bucket with Log Analytics on, by the name a query gives it, and a query
// naming `PROJECT.LOCATION.BUCKET.VIEW` is rewritten to the view of the
// bucket's linked dataset; a bucket without Log Analytics, without a link,
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
	if _, err := p.rewrite(ctx, "SELECT 1 FROM "+ref); err == nil || !strings.Contains(err.Error(), "Create link") {
		t.Errorf("a bucket with no link = %v; want a refusal naming Create link", err)
	}
	if _, err := p.rewrite(ctx, "SELECT 1"); err == nil || !strings.Contains(err.Error(), "PROJECT_ID.LOCATION.BUCKET_ID.LOG_VIEW_ID") {
		t.Errorf("a query naming no log view = %v", err)
	}
	if _, err := p.rewrite(ctx, "SELECT 1 FROM `proj-1.global._Default._AllLogs`"); err == nil || !strings.Contains(err.Error(), "not upgraded") {
		t.Errorf("_Default without Log Analytics = %v", err)
	}
	if err := buckets.ActAt(ctx, project, []string{name}, "create-link", map[string]string{"linkId": "app-link"}); err != nil {
		t.Fatal(err)
	}
	got, err := p.rewrite(ctx, "SELECT severity FROM "+ref+" WHERE severity = 'ERROR'")
	if err != nil || got != "SELECT severity FROM `proj-1.app_link._AllLogs` WHERE severity = 'ERROR'" {
		t.Errorf("rewrite = %q, %v", got, err)
	}
	if _, err := p.rewrite(ctx, "SELECT 1 FROM `proj-1.global.app.nope`"); err == nil || !strings.Contains(err.Error(), "no log view nope") {
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
