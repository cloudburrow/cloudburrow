//go:build browser

package browser

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	documentai "cloud.google.com/go/documentai/apiv1"
	"cloud.google.com/go/documentai/apiv1/documentaipb"
	speech "cloud.google.com/go/speech/apiv2"
	"cloud.google.com/go/speech/apiv2/speechpb"
	"github.com/chromedp/chromedp"
)

// openCloudStorageTranscription opens Transcriptions' New transcription
// and chooses Google's Cloud storage audio source (#1930), and workspace as
// its Workspace (#2084).
func (p *tab) openCloudStorageTranscription(workspace string) {
	p.t.Helper()
	p.waitFor(`document.querySelector(".modal") === null`)
	p.waitFor(`(() => { const b = document.querySelector("#list-action-transcribe"); return !!b && b.getClientRects().length > 0; })()`)
	p.run(chromedp.Click(chromedp.CSS("#list-action-transcribe")))
	p.waitFor(`document.querySelector('.modal.is-open input[name="f-source-choice"][value="Cloud storage"]') !== null`)
	p.eval(`(() => { document.querySelector('.modal.is-open input[name="f-source-choice"][value="Cloud storage"]').click(); return true; })()`, nil)
	p.waitFor(`document.querySelector(".modal.is-open #f-uri").getClientRects().length > 0`)
	p.chooseWorkspace(workspace)
}

// TestSpeechNewTranscriptionIsGooglesThroughThePage (#1930), in the run shard
// with Cloud Storage: Transcriptions' New transcription is Google's, its
// Audio configuration (Local upload, the default, or Cloud storage) and
// then, after Continue, its Transcription options (Recognizer, Spoken
// language, Model), ending in Submit. A Cloud storage file is sent with no
// upload, and BatchRecognize's refusal of a missing object is shown.
func TestSpeechNewTranscriptionIsGooglesThroughThePage(t *testing.T) {
	needService(t, "speech-transcriptions")
	sc, _ := storageSDK(t)
	c, err := speech.NewClient(context.Background(), localAIDial(t)...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	project := uniqueProject(t)
	rid := fmt.Sprintf("browser-new-%d", time.Now().UnixNano()%1e6)
	parent := "projects/" + project + "/locations/global"
	op, err := c.CreateRecognizer(ctx, &speechpb.CreateRecognizerRequest{Parent: parent, RecognizerId: rid,
		Recognizer: &speechpb.Recognizer{DefaultRecognitionConfig: &speechpb.RecognitionConfig{Model: "chirp", LanguageCodes: []string{"en-US"},
			DecodingConfig: &speechpb.RecognitionConfig_AutoDecodingConfig{AutoDecodingConfig: &speechpb.AutoDetectDecodingConfig{}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = c.DeleteRecognizer(context.Background(), &speechpb.DeleteRecognizerRequest{Name: parent + "/recognizers/" + rid})
	})
	bucket := project + "-new"
	if err := sc.Bucket(bucket).Create(ctx, project, nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { _ = sc.Bucket(bucket).Delete(context.Background()) })

	p := open(t)
	p.navigate("/speech/transcriptions?project=" + project)
	p.waitFor(settled)
	p.waitFor(`(() => { const b = document.querySelector("#list-action-transcribe"); return !!b && b.getClientRects().length > 0; })()`)
	p.run(chromedp.Click(chromedp.CSS("#list-action-transcribe")))
	p.waitFor(`document.querySelector(".modal.is-open #f-file") !== null`)
	var form struct {
		Title, Submit, Source string
		Steps                 []string
		File, URI, Options    bool
	}
	read := `(() => { const m = document.querySelector(".modal.is-open"), shown = (s) => { const e = m.querySelector(s); return !!e && e.getClientRects().length > 0; };
		return { Title: m.querySelector("#action-title").textContent, Submit: m.querySelector('button[type="submit"]').textContent,
			Source: (m.querySelector('input[name="f-source-choice"]:checked') || {}).value || "",
			Steps: [...m.querySelectorAll(".create-step-title")].map((s) => s.textContent),
			File: shown("#f-file"), URI: shown("#f-uri"), Options: shown("#f-recognizer") }; })()`
	p.eval(read, &form)
	if form.Title != "New transcription" || form.Submit != "Submit" || form.Source != "Local upload" ||
		strings.Join(form.Steps, "|") != "Audio configuration|Transcription options" || !form.File || form.URI || form.Options {
		t.Errorf("New transcription opens as %+v", form)
	}
	p.eval(`(() => { document.querySelector('.modal.is-open input[name="f-source-choice"][value="Cloud storage"]').click(); return true; })()`, nil)
	p.waitFor(`document.querySelector(".modal.is-open #f-uri").getClientRects().length > 0`)
	p.setField(".modal.is-open #f-uri", "gs://"+bucket+"/missing.flac")
	p.chooseWorkspace(bucket)
	p.clickText(".modal.is-open .create-step-continue button", "Continue")
	p.waitFor(`document.querySelector(".modal.is-open #f-recognizer").getClientRects().length > 0`)
	p.eval(read, &form)
	if form.File || !form.Options {
		t.Errorf("after Cloud storage and Continue: %+v", form)
	}
	var labels []string
	p.eval(`(() => { const s = [...document.querySelectorAll(".modal.is-open .create-step")];
		return [...s[s.length - 1].querySelectorAll("label")].map((l) => l.textContent.replace("*", "").trim()).filter(Boolean); })()`, &labels)
	if j := strings.Join(labels, "|"); !strings.Contains(j, "Recognizer") || !strings.Contains(j, "Spoken language") || !strings.Contains(j, "Model") {
		t.Errorf("Transcription options: %q", labels)
	}
	// long, which the supported languages table offers in global for
	// en-US; chirp_3 is us's and eu's only (#2127), refused here.
	p.eval(`(() => { const s = document.querySelector(".modal.is-open #f-model"); s.value = "long"; s.dispatchEvent(new Event("change", { bubbles: true })); return s.value === "long"; })()`, nil)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	if msg := p.modalRefusal(); !strings.Contains(msg, "not found") {
		t.Errorf("a missing Cloud storage object said %q", msg)
	}
	p.forgive() // the refusal's 400, provoked on purpose
}

// TestDocumentAIUploadTestOptionsThroughThePage (#1930), in the run shard:
// a processor's Upload test document carries Google's process options, the
// processor version and pages, and the OCR processor's symbols; with them
// the result shows the page's tokens and symbols, each boxed over the
// page image.
func TestDocumentAIUploadTestOptionsThroughThePage(t *testing.T) {
	needService(t, "documentai")
	c, err := documentai.NewDocumentProcessorClient(context.Background(), localAIDial(t)...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	project := uniqueProject(t)
	proc, err := c.CreateProcessor(ctx, &documentaipb.CreateProcessorRequest{Parent: "projects/" + project + "/locations/us",
		Processor: &documentaipb.Processor{Type: "OCR_PROCESSOR", DisplayName: "browser-options"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteProcessor(context.Background(), &documentaipb.DeleteProcessorRequest{Name: proc.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	id := proc.GetName()[strings.LastIndex(proc.GetName(), "/")+1:]
	p := open(t)
	p.navigate("/document-ai/processors/" + id + "?project=" + project)
	p.waitFor(settled)
	where := p.findAction("Upload test document")
	if where == "" {
		t.Fatal("the processor's page offers no Upload test document")
	}
	p.clickText(where, "Upload test document")
	p.waitFor(`document.querySelector(".modal.is-open #f-file") !== null`)
	var fields []string
	p.eval(`[...document.querySelectorAll(".modal.is-open [id^='f-']")].map((e) => e.id)`, &fields)
	if j := strings.Join(fields, ","); !strings.Contains(j, "f-version") || !strings.Contains(j, "f-pages") || !strings.Contains(j, "f-enableSymbol") {
		t.Errorf("Upload test document's fields: %q", fields)
	}
	scan, _ := filepath.Abs("../../internal/ocr/testdata/scan.png")
	p.run(chromedp.SetUploadFiles(chromedp.CSS(`.modal.is-open #f-file`), []string{scan}))
	p.setField(".modal.is-open #f-pages", "1")
	p.eval(`(() => { const b = document.querySelector(".modal.is-open #f-enableSymbol"); if (!b.checked) b.click(); return true; })()`, nil)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	// The result opens its analysis page (#2084).
	p.waitFor(`location.pathname.includes("/analysis/") ||
		[...document.querySelectorAll(".modal.is-open .form-error")].some((e) => !e.hidden && e.textContent)`)
	p.waitFor(`document.querySelector("#action-result-table") !== null || document.querySelector(".modal.is-open") !== null`)
	var res struct {
		Rows  string
		Boxes int
	}
	p.eval(`(() => ({ Rows: (document.querySelector("#action-result-table") || {}).textContent || "",
		Boxes: document.querySelectorAll("#view .result-box").length }))()`, &res)
	if !strings.Contains(res.Rows, "Token") || !strings.Contains(res.Rows, "Symbol") || res.Boxes == 0 {
		t.Errorf("with symbols, the result has %d boxes and rows %.200q", res.Boxes, res.Rows)
	}
}

// TestDocumentAIAndSpeechRound3MeetWCAG (#1930): Upload test document's
// options, and New transcription's two steps, in the run shard.
func TestDocumentAIAndSpeechRound3MeetWCAG(t *testing.T) {
	needService(t, "documentai")
	needService(t, "speech-transcriptions")
	dc, err := documentai.NewDocumentProcessorClient(context.Background(), localAIDial(t)...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dc.Close() }()
	sc, err := speech.NewClient(context.Background(), localAIDial(t)...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sc.Close() }()
	ctx := context.Background()
	project := uniqueProject(t)
	proc, err := dc.CreateProcessor(ctx, &documentaipb.CreateProcessorRequest{Parent: "projects/" + project + "/locations/us",
		Processor: &documentaipb.Processor{Type: "LAYOUT_PARSER_PROCESSOR", DisplayName: "a11y-layout"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := dc.DeleteProcessor(context.Background(), &documentaipb.DeleteProcessorRequest{Name: proc.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	parent := "projects/" + project + "/locations/global"
	op, err := sc.CreateRecognizer(ctx, &speechpb.CreateRecognizerRequest{Parent: parent, RecognizerId: "a11y-new",
		Recognizer: &speechpb.Recognizer{DefaultRecognitionConfig: &speechpb.RecognitionConfig{Model: "chirp", LanguageCodes: []string{"en-US"},
			DecodingConfig: &speechpb.RecognitionConfig_AutoDecodingConfig{AutoDecodingConfig: &speechpb.AutoDetectDecodingConfig{}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = sc.DeleteRecognizer(context.Background(), &speechpb.DeleteRecognizerRequest{Name: parent + "/recognizers/a11y-new"})
	})
	id := proc.GetName()[strings.LastIndex(proc.GetName(), "/")+1:]
	const uploadButton = `[...document.querySelectorAll("#view .page-actions button")].find((b) => b.textContent.trim() === "Upload test document")`
	upload := screen("layout parser's Upload test document dialog", "/document-ai/processors/"+id, project)
	upload.ready = settled + ` && !!` + uploadButton
	upload.then = uploadButton + `.click()`
	// The dialog opens after a read; its Cloud storage choice and Continue
	// are made as soon as it is open, before it is measured.
	options := screen("New transcription's Transcription options dialog", "/speech/transcriptions", project)
	options.ready = settled + ` && document.querySelector("#list-action-transcribe") !== null`
	options.then = `document.querySelector("#list-action-transcribe").click();
		const step = () => { const r = document.querySelector('.modal.is-open input[name="f-source-choice"][value="Cloud storage"]');
			if (!r) return setTimeout(step, 10);
			r.click();
			const m = document.querySelector(".modal.is-open"), set = (s, v) => { const f = m.querySelector(s);
				f.value = v; f.dispatchEvent(new Event("input", { bubbles: true })); f.dispatchEvent(new Event("change", { bubbles: true })); };
			set("#f-uri", "gs://a11y-audio/a.flac"); set("#f-workspace", "New workspace"); set("#f-workspaceBucket", "a11y-workspace");
			m.querySelector(".create-step-continue button").click(); };
		step()`
	auditScreens(t, []a11yPage{upload, options})
}
