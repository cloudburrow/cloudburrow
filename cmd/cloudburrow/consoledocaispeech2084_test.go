package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/speech/apiv2/speechpb"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// TestConsoleSpeechWorkspaces (#2084): New Workspace makes a bucket of the
// served Cloud Storage a workspace (created when absent, labelled when it
// exists); a transcription is written there as metadata.json, its audio and
// transcript.json; the Transcriptions page lists it from the bucket, and
// Transcription details plays its audio object through the media route.
func TestConsoleSpeechWorkspaces(t *testing.T) {
	ctx := context.Background()
	st, c := newObjectOpsProvider(t)
	ws := speechWorkspaces{storage: st}
	if got, err := ws.list(ctx, "p"); err != nil || len(got) != 0 {
		t.Fatalf("workspaces before any: %v %v", got, err)
	}
	// New workspace on a new bucket, and on the existing "ops".
	if b, err := ws.chosen(ctx, "p", map[string]string{"workspace": speechNewWorkspace, "workspaceBucket": "speech-ws"}); err != nil || b != "speech-ws" {
		t.Fatalf("New workspace: %q %v", b, err)
	}
	if _, err := ws.chosen(ctx, "p", map[string]string{"workspace": speechNewWorkspace, "workspaceBucket": "ops"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := ws.list(ctx, "p"); strings.Join(got, ",") != "ops,speech-ws" {
		t.Errorf("workspaces: %v", got)
	}
	if _, err := ws.chosen(ctx, "p", map[string]string{"workspace": "other"}); err == nil {
		t.Error("a bucket that is not a workspace was accepted")
	}
	if _, err := ws.chosen(ctx, "p", map[string]string{"workspace": speechNewWorkspace, "workspaceBucket": "Bad Name"}); err == nil {
		t.Error("a bad bucket name was accepted")
	}
	if _, err := ws.chosen(ctx, "p", map[string]string{}); err == nil {
		t.Error("no workspace was accepted")
	}

	audio := []byte("RIFF....WAVEfmt fake audio")
	res := &speechpb.RecognizeResponse{Results: []*speechpb.SpeechRecognitionResult{{LanguageCode: "en-us",
		ResultEndOffset: durationpb.New(2 * time.Second),
		Alternatives:    []*speechpb.SpeechRecognitionAlternative{{Transcript: "hello world", Confidence: 0.9}}}}}
	now := time.Now().UTC()
	meta := &speechWorkspaceMeta{ID: newTranscriptionID(now), Recognizer: "rec-one", Model: "chirp_3", Language: "en-US",
		Source: speechSourceLocal, AudioFile: "clip.wav", AudioType: "audio/wav", Created: now, Text: transcriptText(res)}
	if err := ws.save(ctx, "speech-ws", meta, audio, res); err != nil {
		t.Fatal(err)
	}
	dir := speechTranscriptionsPrefix + meta.ID + "/"
	for _, name := range []string{"metadata.json", "clip.wav", "transcript.json"} {
		r, err := c.Bucket("speech-ws").Object(dir + name).NewReader(ctx)
		if err != nil {
			t.Fatalf("%s not in the workspace: %v", name, err)
		}
		b, _ := io.ReadAll(r)
		_ = r.Close()
		if name == "clip.wav" && (!bytes.Equal(b, audio) || r.Attrs.ContentType != "audio/wav") {
			t.Errorf("audio object: %q %q", b, r.Attrs.ContentType)
		}
		if name == "transcript.json" && !strings.Contains(string(b), "hello world") {
			t.Errorf("transcript.json: %s", b)
		}
	}
	// A Cloud storage file: its audio stays where it is, and the batch
	// output was written elsewhere.
	other := &speechWorkspaceMeta{ID: "transcription-older", Recognizer: "rec-one", Source: speechSourceGCS, AudioFile: "a.flac",
		AudioURI: "gs://ops/in/a.flac", AudioType: "audio/flac", Created: now.Add(-time.Hour), Output: "gs://ops/out/a_transcript.json",
		Text: "Written to gs://ops/out/a_transcript.json"}
	if err := ws.save(ctx, "ops", other, nil, nil); err != nil {
		t.Fatal(err)
	}

	tr := speechTranscriptionsProvider{s: speechSvc{defaultProject: "p"}, held: &speechTranscriptions{}, ws: ws}
	l, err := tr.List(ctx, "p")
	if err != nil || len(l.Items) != 2 || l.Items[0].Name != meta.ID || l.Items[0].Fields["Workspace"] != "speech-ws" ||
		l.Items[0].Fields["Transcript"] != "hello world" || l.Items[1].Name != "transcription-older" {
		t.Fatalf("Transcriptions from the workspaces: %+v %v", l, err)
	}
	d, _ := tr.Detail(ctx, "p", []string{meta.ID})
	if d.Heading != "Transcription details" || propValue(d.Summary, "Workspace") != "speech-ws" ||
		propValue(d.Summary, "Audio file") != "gs://speech-ws/"+dir+"clip.wav" || len(d.Sections) != 2 {
		t.Fatalf("Transcription details: %+v", d)
	}
	player := d.Sections[0]
	if player.Kind != console.KindMedia || player.Media == nil || player.Text != "hello world" ||
		player.Media.Src != console.MediaURL("storage", "p", []string{"speech-ws", "transcriptions", meta.ID, "clip.wav"}) ||
		player.Media.Type != "audio/wav" {
		t.Errorf("the player: %+v %+v", player, player.Media)
	}
	if rs := d.Sections[1].Listing; len(rs.Items) != 1 || rs.Items[0].Fields["Transcript"] != "hello world" {
		t.Errorf("Results: %+v", rs)
	}
	// The media route's store is Cloud Storage's: the player's object opens.
	obj, err := st.OpenObject(ctx, "p", []string{"speech-ws", "transcriptions", meta.ID, "clip.wav"})
	if err != nil || console.MediaType(obj.Name, obj.ContentType) != "audio/wav" {
		t.Errorf("the played object: %+v %v", obj, err)
	} else {
		_ = obj.Close()
	}
	d, _ = tr.Detail(ctx, "p", []string{"transcription-older"})
	if len(d.Sections) != 1 || d.Sections[0].Media.Src != console.MediaURL("storage", "p", []string{"ops", "in", "a.flac"}) ||
		propValue(d.Summary, "Output location") != "gs://ops/out/a_transcript.json" {
		t.Errorf("a Cloud storage file's details: %+v", d)
	}
	if d, _ := tr.Detail(ctx, "p", []string{"nope"}); d.Unavailable == "" {
		t.Error("an unknown transcription has a page")
	}
	// New transcription asks for the workspace first.
	f := workspaceFields([]string{"speech-ws"})
	if f[0].Name != "workspace" || strings.Join(f[0].Options, ",") != "speech-ws,"+speechNewWorkspace ||
		f[1].VisibleWhen["workspace"] != speechNewWorkspace || f[0].Section != "Audio configuration" {
		t.Errorf("Workspace fields: %+v", f)
	}
	if got, ok := gsPath("gs://b/a/b.wav"); !ok || strings.Join(got, "/") != "b/a/b.wav" {
		t.Errorf("gsPath: %v", got)
	}
	if _, ok := gsPath("gs://b"); ok {
		t.Error("gsPath of a bucket")
	}
}

// TestConsoleDocumentAIAnalysisPage (#2084): Upload test document's answer
// opens its analysis page, a page of its own at the processor's
// analysis/{id}: the document view, and the layout parser's chunks on a
// Chunks tab.
func TestConsoleDocumentAIAnalysisPage(t *testing.T) {
	ctx := context.Background()
	s := localAIForConsole(t)
	procs := docAIProcessorsProvider{d: docAI{defaultProject: "p1", addr: s.Addr}, analyses: &docAIAnalyses{}}
	lp, err := procs.Create(ctx, "p1", map[string]string{"type": "LAYOUT_PARSER_PROCESSOR", "name": "layout", "region": "us"})
	if err != nil {
		t.Fatal(err)
	}
	scan, err := os.ReadFile("../../internal/ocr/testdata/scan.png")
	if err != nil {
		t.Fatal(err)
	}
	out, err := procs.ActAtFile(ctx, "p1", []string{lp}, "upload-test", map[string]string{"chunkSize": "5"},
		console.UploadedFile{Name: "scan.png", Reader: bytes.NewReader(scan)})
	if err != nil {
		t.Fatal(err)
	}
	want := "/document-ai/processors/" + lp + "/analysis/1?project=p1"
	if out.Open != want {
		t.Fatalf("Open = %q, want %q", out.Open, want)
	}
	d, err := procs.Detail(ctx, "p1", []string{lp, "analysis", "1"})
	if err != nil || d.Unavailable != "" {
		t.Fatalf("analysis page: %+v %v", d, err)
	}
	if d.Heading != "Layout parser analysis" || d.Title != "scan.png" || propValue(d.Summary, "Processor") != lp ||
		len(d.Sections) != 2 || d.Sections[0].Kind != console.KindResult || d.Sections[0].Label != "Blocks" || d.Sections[1].ID != "chunks" {
		t.Fatalf("analysis page: %+v", d)
	}
	for _, r := range d.Sections[0].Listing.Items {
		if strings.HasPrefix(r.Fields["Element"], "Chunk ") {
			t.Errorf("a chunk in the document view: %+v", r)
		}
	}
	if len(d.Sections[1].Listing.Items) < 2 || d.Sections[0].Listing.Text == "" {
		t.Errorf("chunks %d, text %q", len(d.Sections[1].Listing.Items), d.Sections[0].Listing.Text)
	}
	if d, _ := procs.Detail(ctx, "p1", []string{lp, "analysis", "9"}); d.Unavailable == "" {
		t.Error("an unknown analysis has a page")
	}
	if d, _ := procs.Detail(ctx, "p2", []string{lp, "analysis", "1"}); d.Unavailable == "" {
		t.Error("another project's analysis has a page")
	}
	// Only the last docAIAnalysesKept are kept.
	a := &docAIAnalyses{}
	for i := 0; i < docAIAnalysesKept+3; i++ {
		a.add("p", "x", "OCR_PROCESSOR", "f.png", "n", &console.Listing{})
	}
	if _, ok := a.get("p", "x", "1"); ok {
		t.Error("the oldest analysis was kept past the limit")
	}
	if _, ok := a.get("p", "x", "23"); !ok {
		t.Error("the newest analysis is gone")
	}
}
