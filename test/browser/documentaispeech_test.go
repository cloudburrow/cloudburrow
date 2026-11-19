//go:build browser

package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
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
	"github.com/chromedp/chromedp"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// localAIDial dials the instance's local AI endpoint, which Cloud Run
// starts (#1193) and which serves Document AI and Speech-to-Text (#1799),
// at the address the console's status reports, and never Google's.
func localAIDial(t *testing.T) []option.ClientOption {
	t.Helper()
	code, body := consoleDo(t, http.MethodGet, "/api/status", "")
	var st struct {
		Endpoints map[string]string `json:"endpoints"`
	}
	if code != http.StatusOK || json.Unmarshal([]byte(body), &st) != nil {
		t.Fatalf("GET /api/status = %d: %s", code, body)
	}
	addr := st.Endpoints["localai"]
	if host, _, err := net.SplitHostPort(addr); err != nil || (host != "localhost" && !net.ParseIP(host).IsLoopback()) {
		t.Fatalf("the status names no loopback local AI endpoint: %q", addr)
	}
	return []option.ClientOption{option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
}

// envLocalAI is set only where the local models are available, as for the
// compat suite's local AI tests: transcription fetches whisper.cpp's model.
const envLocalAI = "CLOUDBURROW_TEST_LOCALAI"

// fileAction opens a page action whose form holds a file, chooses file and
// submits it, and returns the answer's text and rows.
func (p *tab) fileAction(label, file string) (text, rows string) {
	p.t.Helper()
	p.waitFor(`document.querySelector(".modal") === null`)
	where := p.findAction(label)
	if where == "" {
		p.t.Fatalf("the page offers no %s", label)
	}
	p.clickText(where, label)
	p.waitFor(`document.querySelector(".modal.is-open #f-file") !== null`)
	p.run(chromedp.SetUploadFiles(chromedp.CSS(`.modal.is-open #f-file`), []string{file}))
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector("#action-result-text") !== null ||
		[...document.querySelectorAll(".modal.is-open .form-error")].some((e) => !e.hidden && e.textContent)`)
	var res struct{ Text, Rows, Error string }
	p.eval(`(() => { const x = document.querySelector("#action-result-text"), t = document.querySelector("#action-result-table");
		const e = [...document.querySelectorAll(".modal.is-open .form-error")].filter((e) => !e.hidden).map((e) => e.textContent).join(" ");
		return { Text: x ? x.textContent : "", Rows: t ? t.textContent : "", Error: e }; })()`, &res)
	if res.Error != "" {
		p.t.Fatalf("%s was refused: %s", label, res.Error)
	}
	p.clickText(".modal.is-open .modal-actions button", "Close")
	p.waitFor(`document.querySelector(".modal") === null`)
	return res.Text, res.Rows
}

// TestDocumentAIProcessorsThroughThePage (#1930), in the run shard, whose
// Cloud Run starts the local AI endpoint: Document AI's menu is Google's
// (Overview, Processor gallery, My processors); the gallery has Enterprise
// Document OCR Available and Invoice Parser Not available locally; Create
// processor on the OCR card's page makes a processor ListProcessors reads;
// Upload test document on its page shows the text ProcessDocument
// extracts from the scanned page, and its lines; Disable processor and
// Enable processor change the state GetProcessor reads; Delete removes it.
func TestDocumentAIProcessorsThroughThePage(t *testing.T) {
	needService(t, "documentai")
	c, err := documentai.NewDocumentProcessorClient(context.Background(), localAIDial(t)...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	project := uniqueProject(t)
	q := "?project=" + project
	name := fmt.Sprintf("browser-ocr-%d", time.Now().UnixNano()%1e6)

	p := open(t)
	p.navigate("/document-ai/processor-library" + q)
	p.waitFor(rowReady("Invoice Parser"))
	var menu []string
	p.eval(`[...document.querySelectorAll("#product-nav a")].map((a) => a.textContent.trim())`, &menu)
	if strings.Join(menu, "|") != "Overview|Processor gallery|My processors" {
		t.Errorf("Document AI's menu is %q", menu)
	}
	rowText := func(s string) string {
		var r string
		p.eval(fmt.Sprintf(`(() => { const r = [...document.querySelectorAll("#view tbody tr")].find((n) => n.textContent.includes(%q));
			return r ? r.textContent : ""; })()`, s), &r)
		return r
	}
	if r := rowText("Enterprise Document OCR"); !strings.Contains(r, "Available") || strings.Contains(r, "Not available") {
		t.Errorf("the OCR card reads %q", r)
	}
	if r := rowText("Invoice Parser"); !strings.Contains(r, "Not available locally") {
		t.Errorf("the Invoice Parser card reads %q", r)
	}

	p.clickText("#view tbody a", "Enterprise Document OCR")
	p.waitFor(`location.pathname === "/document-ai/processor-library/OCR_PROCESSOR"`)
	created := p.pageAction("Create processor", map[string]string{"name": name, "region": "us"}, "")
	var proc *documentaipb.Processor
	it := c.ListProcessors(ctx, &documentaipb.ListProcessorsRequest{Parent: "projects/" + project + "/locations/us"})
	for {
		pr, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListProcessors: %v", err)
		}
		if pr.GetDisplayName() == name {
			proc = pr
		}
	}
	if proc == nil || proc.GetType() != "OCR_PROCESSOR" {
		t.Fatalf("ListProcessors has no %s after Create processor (it answered %q)", name, created)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteProcessor(context.Background(), &documentaipb.DeleteProcessorRequest{Name: proc.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	id := proc.GetName()[strings.LastIndex(proc.GetName(), "/")+1:]
	if !strings.Contains(created, id) {
		t.Errorf("Create processor answered %q, not the processor %s", created, id)
	}

	p.navigate("/document-ai/processors" + q)
	p.waitFor(rowReady(id))
	if r := rowText(id); !strings.Contains(r, name) || !strings.Contains(r, "Enterprise Document OCR") || !strings.Contains(r, "Enabled") {
		t.Errorf("My processors' row reads %q", r)
	}
	p.clickText("#view tbody a", id)
	p.waitFor(fmt.Sprintf(`location.pathname === %q`, "/document-ai/processors/"+id))
	scan, err := filepath.Abs("../../internal/ocr/testdata/scan.png")
	if err != nil {
		t.Fatal(err)
	}
	text, rows := p.fileAction("Upload test document", scan)
	content, _ := os.ReadFile(scan)
	r, err := c.ProcessDocument(ctx, &documentaipb.ProcessRequest{Name: proc.GetName(),
		Source: &documentaipb.ProcessRequest_RawDocument{RawDocument: &documentaipb.RawDocument{Content: content, MimeType: "image/png"}}})
	if err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}
	if strings.TrimSpace(text) == "" || strings.TrimSpace(text) != strings.TrimSpace(r.GetDocument().GetText()) {
		t.Errorf("Upload test document shows %q; ProcessDocument reads %q", text, r.GetDocument().GetText())
	}
	if !strings.Contains(rows, "Line") || !strings.Contains(rows, "Block") {
		t.Errorf("the layout rows read %q", rows)
	}

	p.clickAction("Disable processor", "")
	p.waitFor(`[...document.querySelectorAll("#view button")].some((b) => b.textContent.trim() === "Enable processor")`)
	if got, err := c.GetProcessor(ctx, &documentaipb.GetProcessorRequest{Name: proc.GetName()}); err != nil || got.GetState() != documentaipb.Processor_DISABLED {
		t.Errorf("GetProcessor after Disable processor: %v %v", got.GetState(), err)
	}
	p.clickAction("Enable processor", "")
	p.waitFor(`[...document.querySelectorAll("#view button")].some((b) => b.textContent.trim() === "Disable processor")`)
	if got, err := c.GetProcessor(ctx, &documentaipb.GetProcessorRequest{Name: proc.GetName()}); err != nil || got.GetState() != documentaipb.Processor_ENABLED {
		t.Errorf("GetProcessor after Enable processor: %v %v", got.GetState(), err)
	}
	p.clickText(p.findAction("Delete processor"), "Delete processor")
	p.confirmByName(name)
	p.waitFor(`location.pathname === "/document-ai/processors"`)
	if _, err := c.GetProcessor(ctx, &documentaipb.GetProcessorRequest{Name: proc.GetName()}); err == nil {
		t.Errorf("GetProcessor still reads the processor after Delete processor")
	}
}

// TestSpeechRecognizersThroughThePage (#1930), in the run shard:
// Speech-to-Text's menu (Transcriptions, Recognizers); Create recognizer
// makes a recognizer GetRecognizer reads with its default config; Edit on
// its page changes the language and display name; Delete marks it
// DELETED, and Undelete on its row makes it ACTIVE again. Where the local
// models are available (CLOUDBURROW_TEST_LOCALAI), Transcribe and New
// transcription show the transcript of the JFK clip.
func TestSpeechRecognizersThroughThePage(t *testing.T) {
	needService(t, "speech-recognizers")
	c, err := speech.NewClient(context.Background(), localAIDial(t)...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	project := uniqueProject(t)
	q := "?project=" + project
	id := fmt.Sprintf("browser-rec-%d", time.Now().UnixNano()%1e6)
	full := "projects/" + project + "/locations/global/recognizers/" + id
	t.Cleanup(func() {
		_, _ = c.DeleteRecognizer(context.Background(), &speechpb.DeleteRecognizerRequest{Name: full, AllowMissing: true})
	})

	p := open(t)
	p.navigate("/speech/recognizers" + q)
	p.waitFor(settled)
	var menu []string
	p.eval(`[...document.querySelectorAll("#product-nav a")].map((a) => a.textContent.trim())`, &menu)
	if strings.Join(menu, "|") != "Transcriptions|Recognizers" {
		t.Errorf("Speech-to-Text's menu is %q", menu)
	}
	p.navigate("/speech/recognizers/create" + q)
	p.waitFor(`document.querySelector("#f-language") !== null`)
	p.submitForm(map[string]string{"name": id, "displayName": "Browser", "language": "en-US"}, "name")
	p.waitFor(`location.pathname !== "/speech/recognizers/create"`)
	r, err := c.GetRecognizer(ctx, &speechpb.GetRecognizerRequest{Name: full})
	if err != nil || r.GetDefaultRecognitionConfig().GetModel() != "chirp" || r.GetDisplayName() != "Browser" {
		t.Fatalf("GetRecognizer after Create recognizer: %v %v", r, err)
	}

	p.navigate("/speech/recognizers/" + id + q)
	p.waitFor(settled + ` && document.querySelector("#view").textContent.includes("Word timestamps")`)
	p.pageAction("Edit", map[string]string{"language": "fr-FR", "displayName": "Edited"}, "")
	if r, err = c.GetRecognizer(ctx, &speechpb.GetRecognizerRequest{Name: full}); err != nil ||
		r.GetDefaultRecognitionConfig().GetLanguageCodes()[0] != "fr-FR" || r.GetDisplayName() != "Edited" {
		t.Errorf("GetRecognizer after Edit: %v %v", r, err)
	}

	if os.Getenv(envLocalAI) != "" {
		p.pageAction("Edit", map[string]string{"language": "en-US"}, "")
		clip, _ := filepath.Abs("../../internal/speech/testdata/jfk.flac")
		text, _ := p.fileAction("Transcribe", clip)
		if !strings.Contains(strings.ToLower(text), "your country") {
			t.Errorf("Transcribe shows %q", text)
		}
		p.navigate("/speech/transcriptions" + q)
		p.waitFor(settled)
		text, _ = p.fileAction("New transcription", clip)
		if !strings.Contains(strings.ToLower(text), "your country") {
			t.Errorf("New transcription shows %q", text)
		}
		p.navigate("/speech/transcriptions" + q)
		p.waitFor(rowReady("jfk.flac"))
		p.navigate("/speech/recognizers/" + id + q)
		p.waitFor(settled)
	}

	p.clickText(p.findAction("Delete"), "Delete")
	p.confirmByName(id)
	p.waitFor(`document.querySelector(".modal") === null`)
	if r, err = c.GetRecognizer(ctx, &speechpb.GetRecognizerRequest{Name: full}); err != nil || r.GetState() != speechpb.Recognizer_DELETED {
		t.Fatalf("GetRecognizer after Delete: %v %v", r.GetState(), err)
	}
	p.navigate("/speech/recognizers" + q)
	p.waitFor(rowReady(id))
	p.eval(`document.querySelector("#view tbody tr .overflow-trigger:not(:disabled)").click()`, nil)
	p.clickText("#view .overflow-item", "Undelete")
	p.waitFor(`document.querySelector(".snackbar") !== null`)
	if r, err = c.GetRecognizer(ctx, &speechpb.GetRecognizerRequest{Name: full}); err != nil || r.GetState() != speechpb.Recognizer_ACTIVE {
		t.Errorf("GetRecognizer after Undelete: %v %v", r.GetState(), err)
	}
}

// TestDocumentAIAndSpeechScreensMeetWCAG (#1930): Document AI's Overview,
// Processor gallery, a type's page, My processors and its create form, a
// processor's page, and Speech-to-Text's Transcriptions, Recognizers, its
// create form and a recognizer's page, in the run shard.
func TestDocumentAIAndSpeechScreensMeetWCAG(t *testing.T) {
	needService(t, "documentai")
	needService(t, "speech-recognizers")
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
		Processor: &documentaipb.Processor{Type: "OCR_PROCESSOR", DisplayName: "a11y-ocr"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := dc.DeleteProcessor(context.Background(), &documentaipb.DeleteProcessorRequest{Name: proc.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	op, err := sc.CreateRecognizer(ctx, &speechpb.CreateRecognizerRequest{Parent: "projects/" + project + "/locations/global",
		RecognizerId: "a11y-rec", Recognizer: &speechpb.Recognizer{DefaultRecognitionConfig: &speechpb.RecognitionConfig{Model: "chirp",
			LanguageCodes: []string{"en-US"}, DecodingConfig: &speechpb.RecognitionConfig_AutoDecodingConfig{AutoDecodingConfig: &speechpb.AutoDetectDecodingConfig{}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = sc.DeleteRecognizer(context.Background(), &speechpb.DeleteRecognizerRequest{Name: "projects/" + project + "/locations/global/recognizers/a11y-rec"})
	})
	id := proc.GetName()[strings.LastIndex(proc.GetName(), "/")+1:]
	pages := []a11yPage{
		screen("Document AI overview", "/document-ai/overview", project),
		screen("processor gallery", "/document-ai/processor-library", project),
		screen("OCR processor type", "/document-ai/processor-library/OCR_PROCESSOR", project),
		screen("Invoice Parser type", "/document-ai/processor-library/INVOICE_PROCESSOR", project),
	}
	pages = append(pages, listAndCreate("my processors", "/document-ai/processors", project)...)
	pages = append(pages, screen("processor", "/document-ai/processors/"+id, project),
		screen("transcriptions", "/speech/transcriptions", project))
	pages = append(pages, listAndCreate("recognizers", "/speech/recognizers", project)...)
	pages = append(pages, screen("recognizer", "/speech/recognizers/a11y-rec", project))
	auditScreens(t, pages)
}
