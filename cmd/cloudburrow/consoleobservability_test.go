package main

import (
	"context"
	"strings"
	"testing"
)

// TestConsoleObservabilitySettingsAndBuckets (#2117): Observability
// Settings shows the project's service account and sets the default
// storage location; Observability Buckets creates _Trace there, shows its
// Spans dataset and _AllSpans view, edits it, and Reprovision works once.
func TestConsoleObservabilitySettingsAndBuckets(t *testing.T) {
	_, svc := loggingConsole(t)
	ctx := context.Background()
	const project = "proj-1"
	sp := observabilitySettingsProvider{loggingAdminScreen{svc: svc}}
	l, err := sp.List(ctx, project)
	if err != nil || len(l.Items) != 1 || !strings.HasSuffix(l.Items[0].Fields["Service account"], "@gcp-sa-observability.iam.gserviceaccount.com") {
		t.Fatalf("settings list = %+v, %v", l, err)
	}
	name := l.Items[0].Name
	if d, err := sp.Detail(ctx, project, strings.Split(name, "/")); err != nil || d.Edit == nil {
		t.Fatalf("settings detail = %+v, %v", d, err)
	}
	if err := sp.Edit(ctx, project, strings.Split(name, "/"), map[string]string{"defaultStorageLocation": "eu"}); err != nil {
		t.Fatalf("settings edit: %v", err)
	}
	if err := sp.Edit(ctx, project, strings.Split(name, "/"), map[string]string{"defaultStorageLocation": "atlantis"}); err == nil {
		t.Error("an unsupported location was taken")
	}
	if l, _ := sp.List(ctx, project); l.Items[0].Fields["Default storage location"] != "eu" {
		t.Errorf("after edit = %+v", l.Items)
	}

	bp := observabilityBucketsProvider{loggingAdminScreen{svc: svc}}
	b, err := bp.Create(ctx, project, map[string]string{"id": "_Trace", "location": "eu", "displayName": "Traces"})
	if err != nil || b != "projects/proj-1/locations/eu/buckets/_Trace" {
		t.Fatalf("create = %q, %v", b, err)
	}
	if l, err := bp.List(ctx, project); err != nil || len(l.Items) != 1 || l.Items[0].Fields["Location"] != "eu" {
		t.Errorf("buckets list = %+v, %v", l, err)
	}
	d, err := bp.Detail(ctx, project, strings.Split(b, "/"))
	if err != nil || d.Edit == nil || len(d.Sections) != 1 {
		t.Fatalf("bucket detail = %+v, %v", d, err)
	}
	var data []string
	for _, p := range d.Sections[0].Groups[1].Properties {
		data = append(data, p.Label+"="+p.Value)
	}
	joined := strings.Join(data, " ")
	if !strings.Contains(joined, "/datasets/Spans") || !strings.Contains(joined, "/views/_AllSpans") || !strings.Contains(joined, "Linked BigQuery dataset=None") {
		t.Errorf("datasets, views and links = %v", data)
	}
	if err := bp.Edit(ctx, project, strings.Split(b, "/"), map[string]string{"displayName": "T", "description": "spans"}); err != nil {
		t.Fatalf("bucket edit: %v", err)
	}
	if err := bp.Edit(ctx, project, strings.Split(b, "/"), map[string]string{"displayName": "T", "linkId": "spans"}); err == nil {
		t.Error("a link was made on an instance without BigQuery")
	}
	if err := bp.Act(ctx, project, b, reprovisionAction); err != nil {
		t.Fatalf("reprovision: %v", err)
	}
	if err := bp.Act(ctx, project, b, reprovisionAction); err == nil {
		t.Error("a second reprovision was taken")
	}
}
