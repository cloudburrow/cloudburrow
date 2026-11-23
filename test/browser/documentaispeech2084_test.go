//go:build browser

package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	documentai "cloud.google.com/go/documentai/apiv1"
	"cloud.google.com/go/documentai/apiv1/documentaipb"
	speech "cloud.google.com/go/speech/apiv2"
	"cloud.google.com/go/speech/apiv2/speechpb"
	"cloud.google.com/go/storage"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
	"google.golang.org/api/iterator"
)

// speechWorkspaceLabel is the label New workspace sets on its bucket.
const speechWorkspaceLabel = "speech-to-text-workspace"

// chooseWorkspace sets New transcription's Workspace (#2084): the bucket
// when it is already a workspace, else New workspace naming it.
func (p *tab) chooseWorkspace(bucket string) {
	p.t.Helper()
	p.waitFor(`document.querySelector(".modal.is-open #f-workspace") !== null`)
	p.eval(fmt.Sprintf(`(() => { const s = document.querySelector(".modal.is-open #f-workspace"), b = %q;
		s.value = [...s.options].some((o) => o.value === b) ? b : "New workspace";
		s.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, bucket), nil)
	var isNew bool
	p.eval(`document.querySelector(".modal.is-open #f-workspace").value === "New workspace"`, &isNew)
	if isNew {
		p.waitFor(`document.querySelector(".modal.is-open #f-workspaceBucket").getClientRects().length > 0`)
		p.setField(".modal.is-open #f-workspaceBucket", bucket)
	}
}

// newSpeechRecognizer creates a "long" recognizer in project's global
// location, deleted when the test ends. Not chirp: Google's supported
// languages table offers chirp in asia-southeast1, europe-west4 and
// us-central1 only, so since #2127 a transcription through a global chirp
// recognizer is refused with INVALID_ARGUMENT, as Google refuses it (main
// run 45147981577).
func newSpeechRecognizer(t *testing.T, c *speech.Client, project, id string) {
	t.Helper()
	ctx := context.Background()
	parent := "projects/" + project + "/locations/global"
	op, err := c.CreateRecognizer(ctx, &speechpb.CreateRecognizerRequest{Parent: parent, RecognizerId: id,
		Recognizer: &speechpb.Recognizer{DefaultRecognitionConfig: &speechpb.RecognitionConfig{Model: "long", LanguageCodes: []string{"en-US"},
			DecodingConfig: &speechpb.RecognitionConfig_AutoDecodingConfig{AutoDecodingConfig: &speechpb.AutoDetectDecodingConfig{}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = c.DeleteRecognizer(context.Background(), &speechpb.DeleteRecognizerRequest{Name: parent + "/recognizers/" + id})
	})
}

// emptyBucketAtEnd deletes bucket's objects and then the bucket when the
// test ends.
func emptyBucketAtEnd(t *testing.T, sc *storage.Client, bucket string) {
	t.Cleanup(func() {
		ctx := context.Background()
		it := sc.Bucket(bucket).Objects(ctx, nil)
		for {
			o, err := it.Next()
			if err != nil {
				break
			}
			_ = sc.Bucket(bucket).Object(o.Name).Delete(ctx)
		}
		_ = sc.Bucket(bucket).Delete(ctx)
	})
}

// workspaceObjects are the object names under transcriptions/ in bucket.
func workspaceObjects(t *testing.T, sc *storage.Client, bucket string) []string {
	t.Helper()
	var out []string
	it := sc.Bucket(bucket).Objects(context.Background(), &storage.Query{Prefix: "transcriptions/"})
	for {
		o, err := it.Next()
		if err == iterator.Done {
			return out
		}
		if err != nil {
			t.Fatalf("list the workspace: %v", err)
		}
		out = append(out, o.Name)
	}
}

// playerState is the Transcription details page's player.
type playerState struct {
	Src, Label, Status, Text string
	Ready                    int
	Duration, Time           float64
	Paused                   bool
	Error                    int
}

const readPlayer = `(() => { const a = document.querySelector("#media-player"); if (!a) return null;
	const s = a.querySelector("source");
	return { Src: s ? s.getAttribute("src") : "", Label: a.getAttribute("aria-label") || "",
		Status: (document.querySelector("#media-status") || {}).textContent || "",
		Text: (document.querySelector("#media-text") || {}).textContent || "",
		Ready: a.readyState, Duration: Number.isFinite(a.duration) ? a.duration : 0, Time: a.currentTime,
		Paused: a.paused, Error: a.error ? a.error.code : 0 }; })()`

// playByKeyboard Tabs to the player, presses Space on its play control and
// waits for the audio to be playing past its start (WCAG 2.1.1: the media
// controls are operable from the keyboard).
func (p *tab) playByKeyboard() playerState {
	p.t.Helper()
	p.waitFor(`(() => { const a = document.querySelector("#media-player"); return !!a && a.readyState >= 1; })()`)
	p.eval(`document.querySelector("#view h1").setAttribute("tabindex", "-1"), document.querySelector("#view h1").focus(), true`, nil)
	reached := false
	for i := 0; i < 80 && !reached; i++ {
		p.run(chromedp.KeyEvent(kb.Tab))
		p.eval(`document.activeElement === document.querySelector("#media-player")`, &reached)
	}
	if !reached {
		p.t.Fatal("Tab never reaches the audio player")
	}
	var ring bool
	p.eval(`(() => { const s = getComputedStyle(document.activeElement); return (s.outlineStyle !== "none" && parseFloat(s.outlineWidth) >= 1) || s.boxShadow !== "none"; })()`, &ring)
	if !ring {
		p.t.Error("the focused audio player draws no focus indicator")
	}
	p.run(chromedp.KeyEvent(" "))
	p.waitFor(`(() => { const a = document.querySelector("#media-player"); return !a.paused && a.currentTime > 0.3; })()`)
	var st playerState
	p.eval(readPlayer, &st)
	p.eval(`document.querySelector("#media-player").pause(), true`, nil)
	return st
}

// mediaFetch fetches the player's source as the browser does, with a range,
// and returns its status and type.
func (p *tab) mediaFetch(src string) (status int, ctype string) {
	p.t.Helper()
	var r struct {
		Status int
		Type   string
	}
	p.eval(fmt.Sprintf(`fetch(%q, { headers: { Range: "bytes=0-15" } }).then((r) => ({ Status: r.status, Type: r.headers.get("content-type") || "" }))`, src),
		&r, chromedp.EvalAwaitPromise)
	return r.Status, r.Type
}

// TestSpeechWorkspaceAndPlaybackThroughThePage (#2084), in the localai shard
// with Cloud Storage and the local models (CLOUDBURROW_TEST_LOCALAI): New
// transcription's Workspace > New workspace creates the bucket; a Local
// upload's audio, transcript.json and metadata.json are written there under
// transcriptions/{id}/, read back by the official storage client; Submit
// opens Transcription details, whose player plays the stored object from
// the keyboard, with the transcript under it; the Transcriptions page lists
// it from the workspace. A Cloud storage file then goes to the same
// workspace, chosen from the drop-down, and plays from its own object.
func TestSpeechWorkspaceAndPlaybackThroughThePage(t *testing.T) {
	needService(t, "speech-transcriptions")
	if os.Getenv(envLocalAI) == "" {
		t.Skip("transcription needs the local models: set " + envLocalAI)
	}
	sc, _ := storageSDK(t)
	c, err := speech.NewClient(context.Background(), localAIDial(t)...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	project := uniqueProject(t)
	q := "?project=" + project
	rid := fmt.Sprintf("browser-ws-%d", time.Now().UnixNano()%1e6)
	newSpeechRecognizer(t, c, project, rid)
	bucket := project + "-ws"
	emptyBucketAtEnd(t, sc, bucket)
	clip, _ := filepath.Abs("../../internal/speech/testdata/jfk.flac")

	// Two transcriptions, each allowed three minutes on a loaded runner,
	// and the playback between them: more than a tab's three minutes, which
	// main CI 44941828292 ran out in, waiting on the second.
	p := openWithin(t, 7*time.Minute)
	p.run(reducedMotion())
	p.navigate("/speech/transcriptions" + q)
	p.waitFor(settled)
	p.waitFor(`(() => { const b = document.querySelector("#list-action-transcribe"); return !!b && b.getClientRects().length > 0; })()`)
	p.run(chromedp.Click(chromedp.CSS("#list-action-transcribe")))
	p.waitFor(`document.querySelector(".modal.is-open #f-file") !== null`)
	var opts []string
	p.eval(`[...document.querySelector(".modal.is-open #f-workspace").options].map((o) => o.value)`, &opts)
	if strings.Join(opts, "|") != "New workspace" {
		t.Errorf("a project with no workspace offers %q", opts)
	}
	p.chooseWorkspace(bucket)
	p.run(chromedp.SetUploadFiles(chromedp.CSS(`.modal.is-open #f-file`), []string{clip}))
	p.clickText(".modal.is-open .create-step-continue button", "Continue")
	p.waitFor(`document.querySelector(".modal.is-open #f-recognizer").getClientRects().length > 0`)
	submitted := time.Now()
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	// whisper.cpp transcribes locally, slower beside other tests' runs.
	p.waitForWithin(`location.pathname.startsWith("/speech/transcriptions/transcription-") ||
		[...document.querySelectorAll(".modal.is-open .form-error")].some((e) => !e.hidden && e.textContent)`, 3*time.Minute)
	t.Logf("the Local upload's Submit answered in %s", time.Since(submitted).Round(time.Millisecond))
	var path string
	p.eval(`location.pathname`, &path)
	if !strings.HasPrefix(path, "/speech/transcriptions/transcription-") {
		t.Fatalf("Submit did not open Transcription details: %s", p.modalRefusal())
	}
	id := strings.TrimPrefix(path, "/speech/transcriptions/")
	p.waitFor(settled + ` && document.querySelector("#media-player") !== null`)

	// The workspace is a labelled bucket holding the transcription.
	attrs, err := sc.Bucket(bucket).Attrs(ctx)
	if err != nil || attrs.Labels[speechWorkspaceLabel] != "true" {
		t.Fatalf("the workspace bucket: %+v %v", attrs, err)
	}
	got := strings.Join(workspaceObjects(t, sc, bucket), ",")
	for _, want := range []string{"transcriptions/" + id + "/jfk.flac", "transcriptions/" + id + "/transcript.json",
		"transcriptions/" + id + "/metadata.json"} {
		if !strings.Contains(got, want) {
			t.Errorf("the workspace holds %s, not %s", got, want)
		}
	}
	r, err := sc.Bucket(bucket).Object("transcriptions/" + id + "/transcript.json").NewReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tj, _ := io.ReadAll(r)
	_ = r.Close()
	var transcript struct {
		Results []struct {
			Alternatives []struct{ Transcript string }
		}
	}
	if err := json.Unmarshal(tj, &transcript); err != nil || len(transcript.Results) == 0 ||
		!strings.Contains(strings.ToLower(string(tj)), "your country") {
		t.Errorf("transcript.json in the bucket: %v %.300s", err, tj)
	}

	// Transcription details: the summary, the transcript and the player of
	// the stored object.
	var summary string
	p.eval(`document.querySelector("#view").textContent`, &summary)
	if !strings.Contains(summary, "Transcription details") || !strings.Contains(summary, bucket) || !strings.Contains(summary, rid) {
		t.Errorf("Transcription details reads %.400q", summary)
	}
	var st playerState
	p.eval(readPlayer, &st)
	if !strings.Contains(strings.ToLower(st.Text), "your country") || !strings.Contains(st.Src, "/api/objects/storage/media?") ||
		!strings.Contains(st.Src, "jfk.flac") || st.Label != "Audio: jfk.flac" {
		t.Errorf("the player: %+v", st)
	}
	if code, ct := p.mediaFetch(st.Src); code != http.StatusPartialContent || ct != "audio/flac" {
		t.Errorf("the player's source answers a range with %d %q", code, ct)
	}
	st = p.playByKeyboard()
	if st.Duration < 10 || st.Error != 0 || !strings.Contains(st.Status, "seconds") {
		t.Errorf("playing the stored audio: %+v", st)
	}

	// The Transcriptions page lists it from the workspace.
	p.navigate("/speech/transcriptions" + q)
	p.waitFor(rowReady(id))
	var row string
	p.eval(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes(%q)).textContent`, id), &row)
	if !strings.Contains(row, bucket) || !strings.Contains(row, "jfk.flac") {
		t.Errorf("the transcription's row reads %q", row)
	}

	// A Cloud storage file, into the same workspace from the drop-down.
	data, _ := os.ReadFile(clip)
	w := sc.Bucket(bucket).Object("in/jfk.flac").NewWriter(ctx)
	w.ContentType = "audio/flac"
	_, _ = w.Write(data)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	p.openCloudStorageTranscription(bucket)
	p.eval(`[...document.querySelector(".modal.is-open #f-workspace").options].map((o) => o.value)`, &opts)
	if strings.Join(opts, "|") != bucket+"|New workspace" {
		t.Errorf("the Workspace drop-down offers %q", opts)
	}
	p.setField(".modal.is-open #f-uri", "gs://"+bucket+"/in/jfk.flac")
	submitted = time.Now()
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	// A refusal is shown in the dialog, as the Local upload's: waiting only
	// for the details page would wait out the tab's deadline and say nothing.
	p.waitForWithin(fmt.Sprintf(`location.pathname.startsWith("/speech/transcriptions/transcription-") && location.pathname !== %q ||
		[...document.querySelectorAll(".modal.is-open .form-error")].some((e) => !e.hidden && e.textContent)`, path), 3*time.Minute)
	t.Logf("the Cloud storage file's Submit answered in %s", time.Since(submitted).Round(time.Millisecond))
	var second string
	p.eval(`location.pathname`, &second)
	if second == path || !strings.HasPrefix(second, "/speech/transcriptions/transcription-") {
		t.Fatalf("Submit of a Cloud storage file did not open its Transcription details: %s", p.modalRefusal())
	}
	p.waitFor(settled + ` && document.querySelector("#media-player") !== null`)
	p.eval(readPlayer, &st)
	if !strings.Contains(st.Src, "name=in&name=jfk.flac") || !strings.Contains(strings.ToLower(st.Text), "your country") {
		t.Errorf("a Cloud storage file's player: %+v", st)
	}
	p.waitFor(`document.querySelector("#media-player").readyState >= 1`)
	if n := len(workspaceObjects(t, sc, bucket)); n != 5 {
		t.Errorf("the workspace holds %d objects after two transcriptions, want 5 (the upload's 3, the file's 2)", n)
	}
}

// TestDocumentAIAnalysisPageThroughThePage (#2084), in the run shard: the
// layout parser's Upload test document opens Google's layout parser
// analysis page, a page of its own under the processor, with the blocks
// organized by detected type and a Chunks tab; the breadcrumb leads back to
// the processor.
func TestDocumentAIAnalysisPageThroughThePage(t *testing.T) {
	needService(t, "documentai")
	c, err := documentai.NewDocumentProcessorClient(context.Background(), localAIDial(t)...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	project := uniqueProject(t)
	id := newDocAIProcessor(t, c, project, "LAYOUT_PARSER_PROCESSOR", "browser-analysis")
	p := open(t)
	p.navigate("/document-ai/processors/" + id + "?project=" + project)
	p.waitFor(settled)
	scan, _ := filepath.Abs("../../internal/ocr/testdata/scan.png")
	page := p.uploadTestDocument(scan, map[string]string{"chunkSize": "5"})
	if !strings.HasPrefix(page, "/document-ai/processors/"+id+"/analysis/") {
		t.Fatalf("Upload test document opened %q", page)
	}
	var view struct {
		Heading, Text, Rows string
		Tabs                []string
	}
	read := `(() => ({ Heading: document.querySelector("#view h1").textContent,
		Text: (document.querySelector("#action-result-text") || {}).textContent || "",
		Rows: (document.querySelector("#action-result-table") || {}).textContent || "",
		Tabs: [...document.querySelectorAll("#view [role=tab]")].map((t) => t.textContent.trim()) }))()`
	p.eval(read, &view)
	if strings.TrimSpace(view.Heading) != "Layout parser analysis" || strings.Join(view.Tabs, "|") != "Blocks|Chunks" ||
		!strings.Contains(view.Text, "INVOICE") || !strings.Contains(view.Rows, "Layout block (paragraph)") || strings.Contains(view.Rows, "Chunk c1") {
		t.Errorf("the analysis page: %+v", view)
	}
	var crumbs string
	p.eval(`(document.querySelector(".breadcrumb") || {}).textContent || ""`, &crumbs)
	if !strings.Contains(crumbs, "My processors") || !strings.Contains(crumbs, id) || !strings.Contains(crumbs, "scan.png") {
		t.Errorf("the breadcrumb reads %q", crumbs)
	}
	p.clickText("#view [role=tab]", "Chunks")
	p.waitFor(rowReady("c2"))
}

// newDocAIProcessor creates a processor in project's us location, deleted
// when the test ends, and returns its ID.
func newDocAIProcessor(t *testing.T, c *documentai.DocumentProcessorClient, project, typ, name string) string {
	t.Helper()
	proc, err := c.CreateProcessor(context.Background(), &documentaipb.CreateProcessorRequest{Parent: "projects/" + project + "/locations/us",
		Processor: &documentaipb.Processor{Type: typ, DisplayName: name}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteProcessor(context.Background(), &documentaipb.DeleteProcessorRequest{Name: proc.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	return proc.GetName()[strings.LastIndex(proc.GetName(), "/")+1:]
}

// uploadTestDocument submits Upload test document on a processor's page
// with file and the given fields set, and returns the analysis page it
// opens (#2084).
func (p *tab) uploadTestDocument(file string, values map[string]string) string {
	p.t.Helper()
	p.waitFor(`document.querySelector(".modal") === null`)
	where := p.findAction("Upload test document")
	if where == "" {
		p.t.Fatal("the processor's page offers no Upload test document")
	}
	p.clickText(where, "Upload test document")
	p.waitFor(`document.querySelector(".modal.is-open #f-file") !== null`)
	p.run(chromedp.SetUploadFiles(chromedp.CSS(`.modal.is-open #f-file`), []string{file}))
	for k, v := range values {
		p.setField(".modal.is-open #f-"+k, v)
	}
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`location.pathname.includes("/analysis/") ||
		[...document.querySelectorAll(".modal.is-open .form-error")].some((e) => !e.hidden && e.textContent)`)
	var path string
	p.eval(`location.pathname`, &path)
	if !strings.Contains(path, "/analysis/") {
		p.t.Fatalf("Upload test document was refused: %s", p.modalRefusal())
	}
	p.waitFor(settled + ` && document.querySelector("#action-result-text, #action-result-table") !== null`)
	return path
}

// TestDocumentAIAndSpeechRound4MeetWCAG (#2084): Transcription details with
// its player (in the keyboard walk, the media controls among its stops), the
// Transcriptions page listing a workspace, New transcription's Workspace
// step with New workspace chosen, and Document AI's analysis page and its
// Chunks tab, in the run shard with Cloud Storage. The transcription is a
// workspace's folder written through the official storage client, so this
// needs no local model.
func TestDocumentAIAndSpeechRound4MeetWCAG(t *testing.T) {
	needService(t, "documentai")
	needService(t, "speech-transcriptions")
	sc, _ := storageSDK(t)
	dc, err := documentai.NewDocumentProcessorClient(context.Background(), localAIDial(t)...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dc.Close() }()
	spc, err := speech.NewClient(context.Background(), localAIDial(t)...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = spc.Close() }()
	ctx := context.Background()
	project := uniqueProject(t)
	newSpeechRecognizer(t, spc, project, "a11y-ws")
	bucket := project + "-a11y-ws"
	if err := sc.Bucket(bucket).Create(ctx, project, &storage.BucketAttrs{Labels: map[string]string{speechWorkspaceLabel: "true"}}); err != nil {
		t.Fatal(err)
	}
	emptyBucketAtEnd(t, sc, bucket)
	tid := "transcription-20261120-120000-a11y0001"
	clip, err := os.ReadFile("../../internal/speech/testdata/jfk.flac")
	if err != nil {
		t.Fatal(err)
	}
	put := func(name, ct string, body []byte) {
		w := sc.Bucket(bucket).Object("transcriptions/" + tid + "/" + name).NewWriter(ctx)
		w.ContentType = ct
		_, _ = w.Write(body)
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	put("jfk.flac", "audio/flac", clip)
	put("transcript.json", "application/json", []byte(`{"results":[{"alternatives":[{"transcript":"And so my fellow Americans, ask not what your country can do for you","confidence":0.9}],"resultEndOffset":"11s","languageCode":"en-us"}]}`))
	meta, _ := json.Marshal(map[string]any{"id": tid, "recognizer": "a11y-ws", "model": "long", "language": "en-US",
		"source": "Local upload", "audioFile": "jfk.flac", "audioUri": "gs://" + bucket + "/transcriptions/" + tid + "/jfk.flac",
		"audioType": "audio/flac", "transcriptUri": "gs://" + bucket + "/transcriptions/" + tid + "/transcript.json",
		"createTime": "2026-11-20T12:00:00Z", "text": "And so my fellow Americans, ask not what your country can do for you"})
	put("metadata.json", "application/json", meta)

	// The analysis page is the console's, made by an upload.
	pid := newDocAIProcessor(t, dc, project, "LAYOUT_PARSER_PROCESSOR", "a11y-analysis")
	p := open(t)
	p.navigate("/document-ai/processors/" + pid + "?project=" + project)
	p.waitFor(settled)
	scan, _ := filepath.Abs("../../internal/ocr/testdata/scan.png")
	analysis := p.uploadTestDocument(scan, map[string]string{"chunkSize": "5"})

	// The details page plays the fixture's object.
	p.navigate("/speech/transcriptions/" + tid + "?project=" + project)
	p.waitFor(settled + ` && document.querySelector("#media-player") !== null`)
	var st playerState
	p.eval(readPlayer, &st)
	if !strings.Contains(st.Text, "your country") {
		t.Errorf("the fixture's Transcription details: %+v", st)
	}
	if st = p.playByKeyboard(); st.Duration < 10 {
		t.Errorf("the fixture's audio: %+v", st)
	}

	details := screen("Transcription details with its player", "/speech/transcriptions/"+tid, project)
	details.ready = settled + ` && (() => { const a = document.querySelector("#media-player"); return !!a && a.readyState >= 1; })()`
	list := screen("transcriptions in a workspace", "/speech/transcriptions", project)
	list.ready = rowReady(tid)
	newWS := screen("New transcription's New workspace dialog", "/speech/transcriptions", project)
	newWS.ready = settled + ` && document.querySelector("#list-action-transcribe") !== null`
	newWS.then = `document.querySelector("#list-action-transcribe").click();
		const pick = () => { const s = document.querySelector(".modal.is-open #f-workspace");
			if (!s) return setTimeout(pick, 10);
			s.value = "New workspace"; s.dispatchEvent(new Event("change", { bubbles: true })); };
		pick()`
	chunks := a11yPage{"analysis page's Chunks tab", analysis + "?project=" + project, settled + ` && document.querySelector("#view [role=tab]") !== null`,
		`[...document.querySelectorAll("#view [role=tab]")].find((t) => t.textContent.trim() === "Chunks").click()`}
	auditScreens(t, []a11yPage{details, list, newWS, {"layout parser analysis page", analysis + "?project=" + project,
		settled + ` && document.querySelector("#action-result-table") !== null`, ""}, chunks})
}
