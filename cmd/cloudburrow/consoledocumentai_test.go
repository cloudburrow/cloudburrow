package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/vertexai"
)

// localAIForConsole starts a local AI endpoint on a free port with its
// Document AI and Speech-to-Text state in a temporary directory.
func localAIForConsole(t *testing.T) *vertexai.Server {
	t.Helper()
	s := vertexai.NewServerOn("127.0.0.1:0", nil)
	dir := t.TempDir()
	s.SetDocumentAIState(filepath.Join(dir, "documentai", "processors.json"))
	s.SetSpeechState(filepath.Join(dir, "speech", "recognizers.json"))
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	return s
}

// TestConsoleDocumentAIProcessors (#1930): the Overview, the Processor
// gallery (the two served types Available with Create processor, the
// others Not available locally), My processors with Create, Disable,
// Enable, the processor's page with its version, Upload test document
// reading the scanned page's text and layout, and Delete, all through the
// official client on the local AI endpoint.
func TestConsoleDocumentAIProcessors(t *testing.T) {
	ctx := context.Background()
	s := localAIForConsole(t)
	d := docAI{defaultProject: "p1", addr: s.Addr}
	gallery := docAIGalleryProvider{d: d}
	procs := docAIProcessorsProvider{d: d}

	l, _ := gallery.List(ctx, "p1")
	if l.Unavailable != "" || len(l.Items) != len(docAIGallery) || !l.SummaryOnly || len(l.Summary) != len(docAIGallery) ||
		l.Summary[0].Section != "Digitize" || l.Summary[1].Section != "" || l.Summary[0].Properties[2].Link == "" {
		t.Fatalf("gallery: %+v", l)
	}
	for _, r := range l.Items {
		// The custom types are served since #2106.
		served := r.Fields["Type"] == "OCR_PROCESSOR" || r.Fields["Type"] == "LAYOUT_PARSER_PROCESSOR" || strings.HasPrefix(r.Fields["Type"], "CUSTOM_")
		if served != (r.Status == "Available") || (!served && r.Status != "Not available locally") || served != (len(r.Actions) == 1) {
			t.Errorf("gallery row %+v", r)
		}
	}
	det, _ := gallery.Detail(ctx, "p1", []string{"INVOICE_PROCESSOR"})
	if len(det.Sections) == 0 || !strings.Contains(det.Sections[0].Text, "Not available locally") ||
		len(gallery.DetailActions(ctx, "p1", []string{"INVOICE_PROCESSOR"})) != 0 {
		t.Errorf("Invoice Parser's page: %+v", det)
	}
	res, err := gallery.ActAtResult(ctx, "p1", []string{"LAYOUT_PARSER_PROCESSOR"}, "create-processor", map[string]string{"name": "layout", "region": "eu"})
	if err != nil || len(res.Items) != 1 || res.Items[0].Fields["Region"] != "eu" {
		t.Fatalf("Create processor from the gallery: %+v %v", res, err)
	}
	id, err := procs.Create(ctx, "p1", map[string]string{"type": "OCR_PROCESSOR", "name": "ocr", "region": "us"})
	if err != nil || len(id) != 16 {
		t.Fatalf("Create: %q %v", id, err)
	}
	l, _ = procs.List(ctx, "p1")
	if len(l.Items) != 2 || l.Items[0].Name != id || l.Items[0].Status != "Enabled" || l.Items[0].Fields["Type"] != "Enterprise Document OCR" {
		t.Fatalf("My processors: %+v", l.Items)
	}
	if err := procs.Act(ctx, "p1", id, "disable"); err != nil {
		t.Fatal(err)
	}
	if acts := procs.DetailActions(ctx, "p1", []string{id}); acts[len(acts)-2].ID != "enable" || acts[1].ID != "set-default-version" {
		t.Errorf("a disabled processor's actions: %+v", acts)
	}
	if err := procs.ActAt(ctx, "p1", []string{id}, "enable", nil); err != nil {
		t.Fatal(err)
	}
	page, _ := procs.Detail(ctx, "p1", []string{id})
	if page.Unavailable != "" || len(page.Sections) != 3 || len(page.Sections[1].Listing.Items) != 3 ||
		page.Sections[1].Listing.Items[0].Name != "pretrained-ocr-v2.1-2024-08-07" || len(page.Sections[1].Listing.SelectActions) != 3 {
		t.Errorf("the processor's page: %+v", page)
	}
	// Evaluate & test (#1953): the default version has no evaluations, and
	// the tab says why.
	if ev := page.Sections[2]; ev.ID != "evaluate" || ev.Unavailable != "" || len(ev.Listing.Items) != 0 || !strings.Contains(ev.Note, "cannot be evaluated") {
		t.Errorf("Evaluate & test: %+v", ev)
	}
	// Batch process offers the versions and pages per shard (#1953).
	var batch console.Action
	for _, a := range procs.DetailActions(ctx, "p1", []string{id}) {
		if a.ID == "batch-process" {
			batch = a
		}
	}
	if len(batch.Fields) != 4 || batch.Fields[2].Name != "version" || len(batch.Fields[2].Options) != 3 || batch.Fields[3].Name != "pagesPerShard" {
		t.Errorf("Batch process's fields: %+v", batch.Fields)
	}
	if _, err := procs.ActAtResult(ctx, "p1", []string{id}, "batch-process", map[string]string{"input": "gs://b/in/", "output": "gs://b/out",
		"pagesPerShard": "0"}); err == nil || !strings.Contains(err.Error(), "positive") {
		t.Errorf("Batch process with 0 pages per shard: %v", err)
	}
	// Manage versions: the default changes; Google refuses to undeploy or
	// delete a pretrained version.
	if err := procs.ActAt(ctx, "p1", []string{id}, "set-default-version", map[string]string{"version": "pretrained-ocr-v2.1-2024-08-07"}); err != nil {
		t.Fatal(err)
	}
	if page, _ = procs.Detail(ctx, "p1", []string{id}); page.Sections[1].Listing.Items[0].Fields["Default"] != "Yes" {
		t.Errorf("after Default version: %+v", page.Sections[1].Listing.Items)
	}
	if err := procs.ActAt(ctx, "p1", []string{id}, "delete-versions", map[string]string{"versions": "pretrained-ocr-v1.2-2022-11-10"}); err == nil ||
		!strings.Contains(err.Error(), "pretrained") {
		t.Errorf("Delete of a pretrained version: %v", err)
	}
	// Batch process needs the instance's Cloud Storage, which this
	// endpoint has none of: BatchProcessDocuments' refusal is shown.
	if _, err := procs.ActAtResult(ctx, "p1", []string{id}, "batch-process", map[string]string{"input": "gs://b/in/", "output": "gs://b/out"}); err == nil ||
		!strings.Contains(err.Error(), "Cloud Storage") {
		t.Errorf("Batch process with no Cloud Storage: %v", err)
	}
	scan, err := os.ReadFile("../../internal/ocr/testdata/scan.png")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile("../../internal/ocr/testdata/scan.txt")
	out, err := procs.ActAtFile(ctx, "p1", []string{id}, "upload-test", nil, console.UploadedFile{Name: "scan.png", Reader: bytes.NewReader(scan)})
	if err != nil {
		t.Fatalf("Upload test document: %v", err)
	}
	first := strings.Fields(string(want))[0]
	if !strings.Contains(out.Text, first) || len(out.Items) == 0 || out.Items[0].Fields["Bounding box"] == "" ||
		len(out.Pages) != 1 || !strings.HasPrefix(out.Pages[0].Image, "data:image/png;base64,") || len(out.Pages[0].Boxes) != len(out.Items) ||
		out.Pages[0].Boxes[0].W <= 0 || out.Pages[0].Boxes[0].Row != out.Items[0].Name {
		t.Errorf("Upload test document answered %q and %d rows", out.Text, len(out.Items))
	}
	if _, err := procs.ActAtFile(ctx, "p1", []string{id}, "upload-test", nil, console.UploadedFile{Name: "a.docx", Reader: strings.NewReader("x")}); err == nil {
		t.Error("a .docx was sent")
	}
	if err := procs.Delete(ctx, "p1", id); err != nil {
		t.Fatal(err)
	}
	if l, _ = procs.List(ctx, "p1"); len(l.Items) != 1 {
		t.Errorf("after Delete: %+v", l.Items)
	}
	ov, _ := docAIOverviewProvider{d: d}.List(ctx, "p1")
	if !ov.SummaryOnly || len(ov.Summary) != 2 || !strings.Contains(ov.Summary[0].Properties[1].Value, "1 processors") {
		t.Errorf("Overview: %+v", ov)
	}
	if l, _ := (docAIProcessorsProvider{d: docAI{addr: func() string { return "" }}}).List(ctx, "p1"); l.Unavailable == "" {
		t.Error("no endpoint, no Unavailable")
	}
}

// TestConsoleSpeechRecognizers (#1930): Create recognizer, the list and the
// recognizer's page, Edit, Delete (listed Deleted, with Undelete), Undelete,
// and New transcription's recognizer choice.
func TestConsoleSpeechRecognizers(t *testing.T) {
	ctx := context.Background()
	s := localAIForConsole(t)
	sp := speechSvc{defaultProject: "p1", addr: s.Addr}
	recs := speechRecognizersProvider{s: sp}
	tr := speechTranscriptionsProvider{s: sp, held: &speechTranscriptions{}}

	if acts := tr.ListActions(ctx, "p1"); len(acts) != 1 || acts[0].Href == "" {
		t.Errorf("New transcription with no recognizer: %+v", acts)
	}
	if _, err := recs.Create(ctx, "p1", map[string]string{"name": "rec-one", "region": "us", "displayName": "One",
		"model": "chirp", "language": "en-US", "wordTimeOffsets": "true"}); err != nil {
		t.Fatal(err)
	}
	l, _ := recs.List(ctx, "p1")
	if len(l.Items) != 1 || l.Items[0].Status != "Active" || l.Items[0].Fields["Region"] != "us" || l.Items[0].Fields["Model"] != "chirp" {
		t.Fatalf("Recognizers: %+v", l)
	}
	acts := recs.DetailActions(ctx, "p1", []string{"rec-one"})
	if len(acts) != 3 || acts[1].ID != "edit" {
		t.Fatalf("actions: %+v", acts)
	}
	vals := map[string]string{"displayName": "Renamed", "model": "chirp", "language": "fr-FR", "punctuation": "true"}
	for _, f := range acts[1].Fields {
		if f.Name == "etag" {
			vals["etag"] = f.Default
		}
	}
	if err := recs.ActAt(ctx, "p1", []string{"rec-one"}, "edit", vals); err != nil {
		t.Fatal(err)
	}
	page, _ := recs.Detail(ctx, "p1", []string{"rec-one"})
	cfg := page.Sections[0].Groups[0].Properties
	if !strings.Contains(propValue(page.Summary, "Display name"), "Renamed") || propValue(cfg, "Language") != "fr-FR" ||
		propValue(cfg, "Automatic punctuation") != "On" || propValue(cfg, "Word timestamps") != "Off" {
		t.Errorf("after Edit: %+v %+v", page.Summary, cfg)
	}
	if acts := tr.ListActions(ctx, "p1"); len(acts) != 1 || acts[0].ID != "transcribe" || len(acts[0].Fields) != 7 ||
		acts[0].Fields[4].Options[0] != "rec-one" {
		t.Errorf("New transcription: %+v", acts)
	}
	if err := recs.ActAt(ctx, "p1", []string{"rec-one"}, "delete", nil); err != nil {
		t.Fatal(err)
	}
	l, _ = recs.List(ctx, "p1")
	if len(l.Items) != 1 || l.Items[0].Status != "Deleted" || len(recs.Actions(l.Items[0])) != 1 {
		t.Fatalf("after Delete: %+v", l.Items)
	}
	if err := recs.Act(ctx, "p1", "rec-one", "undelete"); err != nil {
		t.Fatal(err)
	}
	if l, _ = recs.List(ctx, "p1"); l.Items[0].Status != "Active" {
		t.Errorf("after Undelete: %+v", l.Items)
	}
	if _, err := tr.ActAtFile(ctx, "p1", nil, "transcribe", map[string]string{"recognizer": "nope"},
		console.UploadedFile{Name: "a.wav", Reader: strings.NewReader("x")}); err == nil {
		t.Error("an unknown recognizer transcribed")
	}
	if l, _ := tr.List(ctx, "p1"); len(l.Items) != 0 {
		t.Errorf("Transcriptions: %+v", l.Items)
	}
}

func propValue(props []console.Property, label string) string {
	for _, p := range props {
		if p.Label == label {
			return p.Value
		}
	}
	return ""
}
