package main

import (
	"context"
	"strings"
	"testing"
)

// TestConsoleSpeechAdaptation (#1930): Custom classes and Phrase sets'
// Create, list, page, Edit, Delete (listed Deleted, with Undelete) and
// Undelete, and the batch transcription's refusals.
func TestConsoleSpeechAdaptation(t *testing.T) {
	ctx := context.Background()
	s := localAIForConsole(t)
	sp := speechSvc{defaultProject: "p1", addr: s.Addr}
	cc := speechCustomClassesProvider{s: sp}
	ps := speechPhraseSetsProvider{s: sp}

	if _, err := cc.Create(ctx, "p1", map[string]string{"name": "pets", "region": "us", "displayName": "Pets", "items": "cat\n\ndog\n"}); err != nil {
		t.Fatal(err)
	}
	l, _ := cc.List(ctx, "p1")
	if len(l.Items) != 1 || l.Items[0].Status != "Active" || l.Items[0].Fields["Items"] != "2" || l.Items[0].Fields["Region"] != "us" {
		t.Fatalf("Custom classes: %+v", l)
	}
	acts := cc.DetailActions(ctx, "p1", []string{"pets"})
	if len(acts) != 2 || acts[0].ID != "edit" {
		t.Fatalf("actions: %+v", acts)
	}
	vals := map[string]string{"displayName": "Animals", "items": "cat\ndog\nrabbit"}
	for _, f := range acts[0].Fields {
		if f.Name == "etag" {
			vals["etag"] = f.Default
		}
	}
	if err := cc.ActAt(ctx, "p1", []string{"pets"}, "edit", vals); err != nil {
		t.Fatal(err)
	}
	page, _ := cc.Detail(ctx, "p1", []string{"pets"})
	if propValue(page.Summary, "Display name") != "Animals" || page.Sections[0].Listing.Total != 3 {
		t.Errorf("after Edit: %+v", page)
	}
	if err := cc.ActAt(ctx, "p1", []string{"pets"}, "delete", nil); err != nil {
		t.Fatal(err)
	}
	if l, _ = cc.List(ctx, "p1"); l.Items[0].Status != "Deleted" || len(cc.Actions(l.Items[0])) != 1 {
		t.Fatalf("after Delete: %+v", l.Items)
	}
	if err := cc.Act(ctx, "p1", "pets", "undelete"); err != nil {
		t.Fatal(err)
	}

	if _, err := ps.Create(ctx, "p1", map[string]string{"name": "names", "displayName": "Names", "phrases": "CloudBurrow | 15\nSweetTech", "boost": "5"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ps.Create(ctx, "p1", map[string]string{"name": "too-much", "phrases": "x", "boost": "30"}); err == nil || !strings.Contains(err.Error(), "boost") {
		t.Errorf("boost 30: %v", err)
	}
	pl, _ := ps.List(ctx, "p1")
	if len(pl.Items) != 1 || pl.Items[0].Fields["Boost"] != "5" || pl.Items[0].Fields["Region"] != "global" {
		t.Fatalf("Phrase sets: %+v", pl)
	}
	pp, _ := ps.Detail(ctx, "p1", []string{"names"})
	if ph := pp.Sections[0].Listing; ph.Total != 2 || ph.Items[0].Fields["Boost"] != "15" || ph.Items[1].Fields["Boost"] != "-" {
		t.Errorf("phrases: %+v", ph)
	}
	pacts := ps.DetailActions(ctx, "p1", []string{"names"})
	if !strings.Contains(pacts[0].Fields[1].Default, "CloudBurrow | 15") {
		t.Errorf("edit form: %+v", pacts[0].Fields)
	}
	if err := ps.ActAt(ctx, "p1", []string{"names"}, "delete", nil); err != nil {
		t.Fatal(err)
	}
	if err := ps.ActAt(ctx, "p1", []string{"names"}, "undelete", nil); err != nil {
		t.Fatal(err)
	}

	tr := speechTranscriptionsProvider{s: sp, held: &speechTranscriptions{}}
	if _, err := tr.ActAtResult(ctx, "p1", nil, "batch", map[string]string{"recognizer": "nope", "uri": "gs://b/a.wav"}); err == nil {
		t.Error("an unknown recognizer transcribed")
	}
}
