//go:build browser

package browser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	documentai "cloud.google.com/go/documentai/apiv1"
	speech "cloud.google.com/go/speech/apiv2"
	"cloud.google.com/go/storage"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// picker is the Cloud Storage picker's dialog (#2097), opened over the
// form whose Browse opened it.
const picker = `document.querySelector('.modal[aria-labelledby="browse-title"]')`

// openForm opens the page action label's dialog without submitting it.
func (p *tab) openForm(label string) {
	p.t.Helper()
	p.waitFor(`document.querySelector(".modal") === null`)
	where := p.findAction(label)
	if where == "" {
		p.t.Fatalf("the page offers no %s", label)
	}
	p.clickText(where, label)
	p.waitFor(`document.querySelector(".modal.is-open form") !== null`)
	// Clicked where it comes to rest, not part-way through sliding in.
	p.waitFor(`document.getAnimations().every((a) => a.playState !== "running" || a.effect.getTiming().iterations === Infinity)`)
}

// browse opens field's Browse and waits for the picker's first rows.
func (p *tab) browse(field string) {
	p.t.Helper()
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open #browse-` + field)))
	p.waitFor(picker + ` !== null && ` + picker + `.querySelector("#browse-list .fs-row, #browse-list .unavailable") !== null`)
}

// pickerOpen opens the bucket or folder row named label and waits for
// what it holds.
func (p *tab) pickerOpen(label string) {
	p.t.Helper()
	p.run(chromedp.Click(chromedp.CSS(fmt.Sprintf(`.modal[aria-labelledby="browse-title"] button[aria-label=%q]`, "Open "+label))))
	p.waitFor(fmt.Sprintf(`(() => { const c = %s.querySelector(".browse-trail [aria-current]");
		return !!c && c.textContent === %q && %s.querySelector("#browse-list .fs-row, #browse-list .unavailable") !== null; })()`,
		picker, strings.TrimSuffix(label, "/"), picker))
}

// pickerChoose chooses the row named label, then Select; it returns the
// field's value afterwards.
func (p *tab) pickerChoose(field, label string) string {
	p.t.Helper()
	if label != "" {
		p.clickText(`.modal[aria-labelledby="browse-title"] .fs-row-link`, label)
		p.waitFor(fmt.Sprintf(`[...%s.querySelectorAll(".fs-row.is-selected .fs-row-link")].some((b) => b.textContent === %q)`, picker, label))
	}
	p.run(chromedp.Click(chromedp.CSS(`#browse-select`)))
	p.waitFor(picker + ` === null || ` + picker + `.classList.contains("is-closing")`)
	var v string
	p.eval(fmt.Sprintf(`document.querySelector(".modal.is-open #f-%s").value`, field), &v)
	return v
}

// pickerFixture is a bucket holding in/scan.png, results/keep.txt and
// audio/clip.flac, named for project.
func pickerFixture(t *testing.T, sc *storage.Client, project string) string {
	t.Helper()
	ctx := context.Background()
	bucket := project + "-pick"
	if err := sc.Bucket(bucket).Create(ctx, project, nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	emptyBucketAtEnd(t, sc, bucket)
	scan, err := os.ReadFile(filepath.Join("..", "..", "internal", "ocr", "testdata", "scan.png"))
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{"in/scan.png": scan, "results/keep.txt": []byte("kept"), "audio/clip.flac": []byte("fLaC")} {
		w := sc.Bucket(bucket).Object(name).NewWriter(ctx)
		w.ContentType = map[string]string{".png": "image/png", ".txt": "text/plain", "flac": "audio/flac"}[name[len(name)-4:]]
		_, _ = w.Write(body)
		if err := w.Close(); err != nil {
			t.Fatalf("upload %s: %v", name, err)
		}
	}
	return bucket
}

// TestStoragePickerThroughDocumentAIAndSpeech (#2097), in the run shard
// with Cloud Storage beside the local AI endpoint: each gs:// field of
// Document AI and Speech-to-Text has Google's Browse, the shared Cloud
// Storage picker. Batch process's Source path ("Select folder or object")
// opens a bucket, then its in/ folder, and chooses scan.png; its
// Destination path ("Select folder") chooses the results/ folder, and the
// batch, submitted, writes there (the official storage client reads it
// back). A custom processor's Import documents chooses its train/ folder
// and imports it. New transcription's Audio file ("Select object") opens a
// bucket when its name is chosen, because a bucket is not an object, and
// chooses audio/clip.flac; its Output location's Select with nothing
// chosen takes the bucket shown; New workspace's bucket fills the bare
// name. The Filter narrows the rows, and Cancel leaves the field as it was.
func TestStoragePickerThroughDocumentAIAndSpeech(t *testing.T) {
	needService(t, "documentai")
	needService(t, "speech-transcriptions")
	sc, _ := storageSDK(t)
	ctx := context.Background()
	dc, err := documentai.NewDocumentProcessorClient(ctx, localAIDial(t)...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dc.Close() }()
	project := uniqueProject(t)
	bucket := pickerFixture(t, sc, project)
	id := newDocAIProcessor(t, dc, project, "OCR_PROCESSOR", "browser-pick")

	p := open(t)
	p.navigate("/document-ai/processors/" + id + "?project=" + project)
	p.waitFor(settled)
	p.openForm("Batch process")
	p.browse("input")
	var title string
	p.eval(picker+`.querySelector("#browse-title").textContent`, &title)
	if title != "Select folder or object" {
		t.Errorf("Source path's picker is titled %q", title)
	}
	p.pickerOpen(bucket)
	// The Filter narrows the bucket's folders.
	p.setField("#browse-filter", "res")
	var shown string
	p.eval(`[...document.querySelectorAll("#browse-list .fs-row-link")].map((b) => b.textContent).join(",")`, &shown)
	if shown != "results/" {
		t.Errorf("Filter res shows %q", shown)
	}
	p.setField("#browse-filter", "")
	p.pickerOpen("in/")
	if v := p.pickerChoose("input", "scan.png"); v != "gs://"+bucket+"/in/scan.png" {
		t.Fatalf("Source path chose %q", v)
	}
	p.browse("output")
	p.pickerOpen(bucket)
	if v := p.pickerChoose("output", "results/"); v != "gs://"+bucket+"/results/" {
		t.Fatalf("Destination path chose %q", v)
	}
	// Cancel leaves the value chosen.
	p.browse("output")
	p.clickText(`.modal[aria-labelledby="browse-title"] .modal-actions button`, "Cancel")
	p.waitFor(picker + ` === null || ` + picker + `.classList.contains("is-closing")`)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitForWithin(`document.querySelector("#action-result-table") !== null ||
		[...document.querySelectorAll(".modal.is-open .form-error")].some((e) => !e.hidden && e.textContent)`, 60e9)
	var got string
	p.eval(`(document.querySelector("#action-result-table") || {}).textContent || [...document.querySelectorAll(".modal.is-open .form-error")].map((e) => e.textContent).join(" ")`, &got)
	if !strings.Contains(got, "Succeeded") || !strings.Contains(got, "gs://"+bucket+"/results/") {
		t.Fatalf("Batch process answered %q", got)
	}
	p.clickText(".modal.is-open .modal-actions button", "Close")
	p.waitFor(`document.querySelector(".modal") === null`)
	n := 0
	for it := sc.Bucket(bucket).Objects(ctx, &storage.Query{Prefix: "results/"}); ; {
		a, err := it.Next()
		if err != nil {
			break
		}
		if strings.HasSuffix(a.Name, ".json") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the batch wrote %d Documents under results/", n)
	}

	// A custom processor's Import documents.
	f := newCustomExtractorFixture(t)
	p.navigate("/document-ai/processors/" + f.id + "?project=" + f.project)
	p.waitFor(settled)
	p.clickText("#view [role=tab]", "Build")
	p.pageAction("Configure dataset", map[string]string{"storage": "managed", "gcsPrefix": "gs://" + f.bucket + "/dataset"}, "")
	p.openForm("Import documents")
	p.browse("source")
	p.pickerOpen(f.bucket)
	if v := p.pickerChoose("source", "train/"); v != "gs://"+f.bucket+"/train/" {
		t.Fatalf("Import documents' Source path chose %q", v)
	}
	p.setField(".modal.is-open #f-split", "DATASET_SPLIT_TRAIN")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitForWithin(`document.querySelector("#action-result-table") !== null ||
		[...document.querySelectorAll(".modal.is-open .form-error")].some((e) => !e.hidden && e.textContent)`, 60e9)
	p.eval(`(document.querySelector("#action-result-table") || {}).textContent || [...document.querySelectorAll(".modal.is-open .form-error")].map((e) => e.textContent).join(" ")`, &got)
	if !strings.Contains(got, "Imported") {
		t.Fatalf("Import documents answered %q", got)
	}
	p.clickText(".modal.is-open .modal-actions button", "Close")
	p.waitFor(`document.querySelector(".modal") === null`)
	if docs := f.documents(t); docs["scan.png"] == "" {
		t.Errorf("the dataset holds %v", docs)
	}

	// New transcription.
	spc, err := speech.NewClient(ctx, localAIDial(t)...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = spc.Close() }()
	newSpeechRecognizer(t, spc, project, "browser-pick")
	p.navigate("/speech/transcriptions?project=" + project)
	p.waitFor(settled)
	p.openCloudStorageTranscription(bucket)
	p.browse("uri")
	p.eval(picker+`.querySelector("#browse-title").textContent`, &title)
	if title != "Select object" {
		t.Errorf("Audio file's picker is titled %q", title)
	}
	// A bucket's name opens it: an object is what is chosen here.
	p.clickText(`.modal[aria-labelledby="browse-title"] .fs-row-link`, bucket)
	p.waitFor(picker + `.querySelector(".browse-trail [aria-current]")?.textContent === "` + bucket + `"`)
	var disabled bool
	p.eval(`document.querySelector("#browse-select").disabled`, &disabled)
	if !disabled {
		t.Error("Select object offers Select with no object chosen")
	}
	p.pickerOpen("audio/")
	if v := p.pickerChoose("uri", "clip.flac"); v != "gs://"+bucket+"/audio/clip.flac" {
		t.Fatalf("Audio file chose %q", v)
	}
	p.browse("output")
	p.pickerOpen(bucket)
	if v := p.pickerChoose("output", ""); v != "gs://"+bucket+"/" {
		t.Fatalf("Output location's Select in the bucket chose %q", v)
	}
	// New workspace: a bucket's bare name.
	p.eval(`(() => { const s = document.querySelector(".modal.is-open #f-workspace"); s.value = "New workspace";
		s.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, nil)
	p.waitFor(`document.querySelector(".modal.is-open #f-workspaceBucket").getClientRects().length > 0`)
	p.browse("workspaceBucket")
	var opens int
	p.eval(picker+`.querySelectorAll(".browse-open").length`, &opens)
	if opens != 0 {
		t.Errorf("Select bucket offers %d folder openers", opens)
	}
	if v := p.pickerChoose("workspaceBucket", bucket); v != bucket {
		t.Fatalf("New workspace's bucket chose %q", v)
	}
}

// TestStoragePickerMeetsWCAG (#2097): the picker, open at a folder over
// Batch process, has every control named, meets contrast in the light and
// dark themes, is a named modal dialog that takes focus, keeps a Tab walk
// inside it, closes on Escape back to the form (which stays open, focus on
// its field), and reflows at 320px.
func TestStoragePickerMeetsWCAG(t *testing.T) {
	needService(t, "documentai")
	sc, _ := storageSDK(t)
	dc, err := documentai.NewDocumentProcessorClient(context.Background(), localAIDial(t)...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dc.Close() }()
	project := uniqueProject(t)
	bucket := pickerFixture(t, sc, project)
	id := newDocAIProcessor(t, dc, project, "OCR_PROCESSOR", "a11y-pick")
	show := func(p *tab) {
		p.navigate("/document-ai/processors/" + id + "?project=" + project)
		p.waitFor(settled)
		p.openForm("Batch process")
		p.browse("input")
		p.pickerOpen(bucket)
		p.clickText(`.modal[aria-labelledby="browse-title"] .fs-row-link`, "results/")
		p.eval(`new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(() => r(true))))`, nil, awaitPromise)
		p.waitFor(`document.getAnimations().every((a) => a.playState !== "running" || a.effect.getTiming().iterations === Infinity)`)
	}
	t.Run("names", func(t *testing.T) {
		p := open(t)
		show(p)
		for _, u := range p.unnamedControls() {
			t.Errorf("an unnamed %s", u)
		}
	})
	for _, theme := range []string{"light", "dark"} {
		t.Run("contrast/"+theme, func(t *testing.T) {
			p := open(t)
			p.run(reducedMotion())
			p.navigate("/")
			p.waitFor(settled)
			p.setTheme(theme)
			show(p)
			for _, fl := range p.contrastFailures() {
				t.Errorf("%s theme: %s", theme, fl)
			}
		})
	}
	t.Run("keyboard", func(t *testing.T) {
		p := open(t)
		p.run(reducedMotion())
		show(p)
		var d struct {
			Role, Modal, Name string
			Inside            bool
		}
		p.eval(`(() => { const d = `+picker+`;
			return { Role: d.getAttribute("role"), Modal: d.getAttribute("aria-modal"),
			         Name: document.getElementById(d.getAttribute("aria-labelledby")).textContent, Inside: d.contains(document.activeElement) }; })()`, &d)
		if d.Role != "dialog" || d.Modal != "true" || d.Name != "Select folder or object" || !d.Inside {
			t.Errorf("the picker is %+v", d)
		}
		for i, back := range []bool{false, false, false, false, false, false, false, true, true} {
			if back {
				p.run(chromedp.KeyEvent(kb.Tab, chromedp.KeyModifiers(chromedp.ModifierShift)))
			} else {
				p.run(chromedp.KeyEvent(kb.Tab))
			}
			var in bool
			p.eval(picker+`.contains(document.activeElement)`, &in)
			if !in {
				t.Fatalf("key %d of a Tab walk left the picker", i+1)
			}
		}
		// The rows are reached by keyboard and choose with Enter.
		p.eval(`document.querySelector("#browse-list .fs-row-link").focus()`, nil)
		p.run(chromedp.KeyEvent(kb.Enter))
		p.waitFor(picker + `.querySelector(".fs-row.is-selected") !== null`)
		p.run(chromedp.KeyEvent(kb.Escape))
		var after struct{ Picker, Form, Focus bool }
		p.eval(`new Promise((r) => setTimeout(() => r({ Picker: `+picker+` !== null && !`+picker+`.classList.contains("is-closing"),
			Form: document.querySelector(".modal.is-open #f-input") !== null,
			Focus: document.activeElement === document.querySelector("#browse-input") || document.activeElement === document.querySelector("#f-input") }), 400))`,
			&after, awaitPromise)
		if after.Picker || !after.Form || !after.Focus {
			t.Errorf("Escape left %+v", after)
		}
	})
	t.Run("reflow/320px", func(t *testing.T) {
		p := open(t)
		p.run(reducedMotion())
		// Opened, then narrowed: an emulated 320px viewport takes no
		// pointer clicks in this harness, and reflow is what the dialog
		// does at that width.
		show(p)
		p.run(chromedp.EmulateViewport(320, 720))
		p.eval(`new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(() => r(true))))`, nil, awaitPromise)
		for _, fl := range p.reflowFailures() {
			t.Errorf("at 320px: %s", fl)
		}
	})
}
