package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"cloud.google.com/go/documentai/apiv1/documentaipb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// TestConsoleDocumentAIUploadTestOptions (#1930): Upload test document
// carries Google's test panel's options, the processor version, the pages
// and the processor's own config (the OCR processor's symbols, the layout
// parser's Edit Layout Config chunk size), and sends them to
// ProcessDocument.
func TestConsoleDocumentAIUploadTestOptions(t *testing.T) {
	ctx := context.Background()
	s := localAIForConsole(t)
	procs := docAIProcessorsProvider{d: docAI{defaultProject: "p1", addr: s.Addr}}
	id, err := procs.Create(ctx, "p1", map[string]string{"type": "OCR_PROCESSOR", "name": "ocr", "region": "us"})
	if err != nil {
		t.Fatal(err)
	}
	var up console.Action
	for _, a := range procs.DetailActions(ctx, "p1", []string{id}) {
		if a.ID == "upload-test" {
			up = a
		}
	}
	names := []string{}
	for _, f := range up.Fields {
		names = append(names, f.Name)
	}
	if strings.Join(names, ",") != "file,version,pages,enableSymbol" || len(up.Fields[1].Options) != 3 || up.Fields[1].Section != "Process options" {
		t.Fatalf("Upload test document's fields: %+v", up.Fields)
	}
	scan, err := os.ReadFile("../../internal/ocr/testdata/scan.png")
	if err != nil {
		t.Fatal(err)
	}
	out, err := procs.ActAtFile(ctx, "p1", []string{id}, "upload-test", map[string]string{"version": "pretrained-ocr-v2.1-2024-08-07",
		"pages": "1", "enableSymbol": "true"}, console.UploadedFile{Name: "scan.png", Reader: bytes.NewReader(scan)})
	if err != nil {
		t.Fatalf("Upload test document with options: %v", err)
	}
	kinds := map[string]int{}
	for _, r := range out.Items {
		kinds[r.Fields["Element"]]++
	}
	if kinds["Token"] == 0 || kinds["Symbol"] == 0 || kinds["Line"] == 0 {
		t.Errorf("rows by element: %v", kinds)
	}
	if _, err := procs.ActAtFile(ctx, "p1", []string{id}, "upload-test", map[string]string{"pages": "2"},
		console.UploadedFile{Name: "scan.png", Reader: bytes.NewReader(scan)}); err == nil || !strings.Contains(err.Error(), "page 2") {
		t.Errorf("page 2 of a one-page document: %v", err)
	}

	lp, err := procs.Create(ctx, "p1", map[string]string{"type": "LAYOUT_PARSER_PROCESSOR", "name": "layout", "region": "us"})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range procs.DetailActions(ctx, "p1", []string{lp}) {
		if a.ID == "upload-test" && a.Fields[len(a.Fields)-1].Name != "chunkSize" {
			t.Errorf("the layout parser's Upload test document: %+v", a.Fields)
		}
	}
	out, err = procs.ActAtFile(ctx, "p1", []string{lp}, "upload-test", map[string]string{"chunkSize": "5"},
		console.UploadedFile{Name: "scan.png", Reader: bytes.NewReader(scan)})
	if err != nil {
		t.Fatal(err)
	}
	chunks := 0
	for _, r := range out.Items {
		if strings.HasPrefix(r.Fields["Element"], "Chunk ") {
			// A chunk is whole paragraphs, so a small chunk size is a
			// chunk a paragraph.
			chunks++
		}
	}
	if chunks < 2 || out.Text == "" {
		t.Errorf("chunks %d, text %q", chunks, out.Text)
	}
}

// TestDocAIProcessOptions: the options' wire form, and their refusals.
func TestDocAIProcessOptions(t *testing.T) {
	ocr := &documentaipb.Processor{Name: "projects/p/locations/us/processors/x", Type: "OCR_PROCESSOR"}
	name, opts, err := docAIProcessOptions(ocr, map[string]string{"version": "v1", "pages": " 1, 3 ", "enableSymbol": "true", "chunkSize": "9"})
	if err != nil || name != ocr.Name+"/processorVersions/v1" || len(opts.GetIndividualPageSelector().GetPages()) != 2 ||
		!opts.GetOcrConfig().GetEnableSymbol() || opts.GetLayoutConfig() != nil {
		t.Errorf("OCR options: %q %+v %v", name, opts, err)
	}
	if name, opts, err := docAIProcessOptions(ocr, map[string]string{}); err != nil || name != ocr.Name || opts != nil {
		t.Errorf("no options: %q %+v %v", name, opts, err)
	}
	if _, _, err := docAIProcessOptions(ocr, map[string]string{"pages": "1,x"}); err == nil {
		t.Error("pages 1,x accepted")
	}
	lp := &documentaipb.Processor{Name: "n", Type: "LAYOUT_PARSER_PROCESSOR"}
	if _, opts, err := docAIProcessOptions(lp, map[string]string{"chunkSize": "40", "enableSymbol": "true"}); err != nil ||
		opts.GetLayoutConfig().GetChunkingConfig().GetChunkSize() != 40 || opts.GetOcrConfig() != nil {
		t.Errorf("layout options: %+v %v", opts, err)
	}
	if _, _, err := docAIProcessOptions(lp, map[string]string{"chunkSize": "0"}); err == nil {
		t.Error("chunk size 0 accepted")
	}
}

// TestConsoleSpeechNewTranscription (#1930): Google's New Transcription,
// its Audio configuration (Local upload or Cloud storage) then Continue to
// its Transcription options (Recognizer, Spoken language, Model) and
// Submit; the options go over the recognizer's config through config_mask.
func TestConsoleSpeechNewTranscription(t *testing.T) {
	a := newTranscriptionAction([]string{"rec"})
	if a.Steps != "continue" || a.Submit != "Submit" || a.Fields[0].Name != "source" ||
		a.Fields[1].VisibleWhen["source"] != speechSourceLocal || a.Fields[2].VisibleWhen["source"] != speechSourceGCS ||
		a.Fields[4].Section != "Transcription options" || a.Fields[5].Label != "Spoken language" || a.Fields[6].Options[0] != "" {
		t.Errorf("New transcription: %+v", a)
	}
	cfg, mask := speechOverrides(map[string]string{"model": "chirp_3", "language": "en-US, fr-FR"})
	if cfg.GetModel() != "chirp_3" || len(cfg.GetLanguageCodes()) != 2 || strings.Join(mask.GetPaths(), ",") != "model,language_codes" {
		t.Errorf("overrides: %+v %+v", cfg, mask)
	}
	if cfg, mask := speechOverrides(map[string]string{"model": " "}); cfg != nil || mask != nil {
		t.Errorf("no overrides: %+v %+v", cfg, mask)
	}

	ctx := context.Background()
	s := localAIForConsole(t)
	sp := speechSvc{defaultProject: "p1", addr: s.Addr}
	tr := speechTranscriptionsProvider{s: sp, held: &speechTranscriptions{}}
	if _, err := (speechRecognizersProvider{s: sp}).Create(ctx, "p1", map[string]string{"name": "rec-one", "region": "global",
		"model": "chirp", "language": "en-US"}); err != nil {
		t.Fatal(err)
	}
	// A Local upload goes to the upload route; a Cloud storage file to
	// BatchRecognize, which needs the instance's Cloud Storage.
	if err := tr.ActAt(ctx, "p1", nil, "transcribe", map[string]string{"source": speechSourceLocal, "recognizer": "rec-one"}); err == nil ||
		!strings.Contains(err.Error(), "takes an audio file") {
		t.Errorf("Local upload with no file: %v", err)
	}
	if _, err := tr.ActAtResult(ctx, "p1", nil, "transcribe", map[string]string{"source": speechSourceGCS, "recognizer": "rec-one",
		"uri": "gs://b/a.wav", "model": "chirp_3"}); err == nil {
		t.Error("a Cloud storage file transcribed with no Cloud Storage")
	}
	// A transcription's page is Google's Transcription details, naming
	// the model and language that made it.
	tr.held.list = append(tr.held.list, speechTranscription{id: "transcription-1", project: "p1", recognizer: "rec-one",
		file: "a.wav", text: "hello", model: "chirp_3", language: "en-US", result: &console.Listing{}})
	d, _ := tr.Detail(ctx, "p1", []string{"transcription-1"})
	if d.Heading != "Transcription details" || propValue(d.Summary, "Model") != "chirp_3" || propValue(d.Summary, "Spoken language") != "en-US" {
		t.Errorf("Transcription details: %+v", d)
	}
	if l, _ := tr.List(ctx, "p1"); len(l.Items) != 1 || l.Items[0].Fields["Model"] != "chirp_3" {
		t.Errorf("Transcriptions: %+v", l.Items)
	}
}
