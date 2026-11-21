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
	"cloud.google.com/go/documentai/apiv1/documentaipb"
	documentaibeta "cloud.google.com/go/documentai/apiv1beta3"
	beta "cloud.google.com/go/documentai/apiv1beta3/documentaipb"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/encoding/protojson"
)

// customExtractorFixture is a Custom Extractor whose bucket holds a scan
// for its training split and a labeled Document JSON for its test split.
type customExtractorFixture struct {
	project, id, name, bucket string
	dc                        *documentai.DocumentProcessorClient
	ds                        *documentaibeta.DocumentClient
}

func newCustomExtractorFixture(t *testing.T) customExtractorFixture {
	t.Helper()
	needService(t, "documentai")
	sc, _ := storageSDK(t)
	ctx := context.Background()
	dc, err := documentai.NewDocumentProcessorClient(ctx, localAIDial(t)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dc.Close() })
	ds, err := documentaibeta.NewDocumentClient(ctx, localAIDial(t)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ds.Close() })
	f := customExtractorFixture{project: uniqueProject(t), dc: dc, ds: ds}
	f.id = newDocAIProcessor(t, dc, f.project, "CUSTOM_EXTRACTION_PROCESSOR", "browser-custom")
	f.name = "projects/" + f.project + "/locations/us/processors/" + f.id
	f.bucket = f.project + "-custom"
	if err := sc.Bucket(f.bucket).Create(ctx, f.project, nil); err != nil {
		t.Fatal(err)
	}
	emptyBucketAtEnd(t, sc, f.bucket)
	put := func(name, ct string, body []byte) {
		w := sc.Bucket(f.bucket).Object(name).NewWriter(ctx)
		w.ContentType = ct
		_, _ = w.Write(body)
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	scan, err := os.ReadFile(filepath.Join("..", "..", "internal", "ocr", "testdata", "scan.png"))
	if err != nil {
		t.Fatal(err)
	}
	put("train/scan.png", "image/png", scan)
	text := "INVOICE No. 2026-0999\nDate: 2 April 2026\nBill to: Contoso Ltd\nTotal due 512.00\n"
	i := strings.Index(text, "2026-0999")
	test, _ := protojson.Marshal(&documentaipb.Document{Text: text, Entities: []*documentaipb.Document_Entity{{Type: "invoice_id",
		MentionText: "2026-0999", TextAnchor: &documentaipb.Document_TextAnchor{TextSegments: []*documentaipb.Document_TextAnchor_TextSegment{
			{StartIndex: int64(i), EndIndex: int64(i + 9)}}}}}})
	put("test/a.json", "application/json", test)
	return f
}

// documents are the dataset's documents by display name, their IDs.
func (f customExtractorFixture) documents(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	it := f.ds.ListDocuments(context.Background(), &beta.ListDocumentsRequest{Dataset: f.name + "/dataset"})
	for {
		m, err := it.Next()
		if err == iterator.Done {
			return out
		}
		if err != nil {
			t.Fatalf("ListDocuments: %v", err)
		}
		out[m.GetDisplayName()] = m.GetDocumentId().GetGcsManagedDocId().GetCwDocId()
	}
}

// TestDocumentAICustomExtractorBuildTrainEvaluateThroughThePage (#2124):
// a Custom Extractor from its console page, each step read back through
// the official clients: Get started's Create new field and Edit document
// prompt (GetDatasetSchema), Build's Configure dataset (a Cloud Storage
// folder) and Import documents into the training split (a scan, OCR'd)
// and the test split (a labeled Document JSON); the scan's labeling page
// draws its page, and a box drawn over the invoice number with the
// pointer labels its words (GetDocument's entity, with its page anchor's
// box); Create new version trains, its evaluation's page has the labels'
// precision, recall and F1 and changes the confidence threshold; Deploy &
// use deploys the version and makes it the default (GetProcessor).
func TestDocumentAICustomExtractorBuildTrainEvaluateThroughThePage(t *testing.T) {
	f := newCustomExtractorFixture(t)
	ctx := context.Background()
	p := open(t)
	p.navigate("/document-ai/processors/" + f.id + "?project=" + f.project)
	p.waitFor(settled)
	p.clickText("#view [role=tab]", "Get started")
	p.pageAction("Create new field", map[string]string{"name": "invoice_id", "valueType": "string", "occurrence": "REQUIRED_ONCE"}, "")
	p.pageAction("Edit document prompt", map[string]string{"prompt": "Supplier invoices"}, "")
	sc, err := f.ds.GetDatasetSchema(ctx, &beta.GetDatasetSchemaRequest{Name: f.name + "/dataset/datasetSchema"})
	if err != nil || sc.GetDocumentSchema().GetDescription() != "Supplier invoices" || len(sc.GetDocumentSchema().GetEntityTypes()) != 1 ||
		sc.GetDocumentSchema().GetEntityTypes()[0].GetProperties()[0].GetName() != "invoice_id" {
		t.Fatalf("schema after Get started: %v %v", sc, err)
	}
	p.clickText("#view [role=tab]", "Build")
	p.pageAction("Configure dataset", map[string]string{"storage": "managed", "gcsPrefix": "gs://" + f.bucket + "/dataset"}, "")
	if got := p.pageAction("Import documents", map[string]string{"source": "gs://" + f.bucket + "/train/", "split": "DATASET_SPLIT_TRAIN"}, ""); !strings.Contains(got, "Imported") {
		t.Fatalf("Import documents answered %q", got)
	}
	if got := p.pageAction("Import documents", map[string]string{"source": "gs://" + f.bucket + "/test/", "split": "DATASET_SPLIT_TEST"}, ""); !strings.Contains(got, "Imported") {
		t.Fatalf("Import documents answered %q", got)
	}
	docs := f.documents(t)
	scanID := docs["scan.png"]
	if scanID == "" || docs["a.json"] == "" {
		t.Fatalf("the dataset holds %v", docs)
	}
	got, err := f.ds.GetDocument(ctx, &beta.GetDocumentRequest{Dataset: f.name + "/dataset", DocumentId: &beta.DocumentId{
		Type: &beta.DocumentId_GcsManagedDocId{GcsManagedDocId: &beta.DocumentId_GCSManagedDocumentId{CwDocId: scanID}}}})
	if err != nil {
		t.Fatal(err)
	}
	// The invoice number's token, to draw a box around.
	var box [4]float64
	doc := got.GetDocument()
	for _, tk := range doc.GetPages()[0].GetTokens() {
		segs := tk.GetLayout().GetTextAnchor().GetTextSegments()
		if len(segs) > 0 && strings.HasPrefix(doc.GetText()[segs[0].GetStartIndex():segs[0].GetEndIndex()], "2026-0417") {
			v := tk.GetLayout().GetBoundingPoly().GetNormalizedVertices()
			box = [4]float64{float64(v[0].GetX()), float64(v[0].GetY()), float64(v[2].GetX()), float64(v[2].GetY())}
		}
	}
	if box[2] == 0 {
		t.Fatalf("the scan has no token 2026-0417: %q", doc.GetText())
	}

	// The labeling page: draw a box over the invoice number.
	p.navigate("/document-ai/processors/" + f.id + "/documents/" + scanID + "?project=" + f.project)
	p.waitFor(settled + ` && document.querySelector(".result-page-frame.is-labeling img") !== null`)
	p.eval(fmt.Sprintf(`(() => { const fr = document.querySelector(".result-page-frame.is-labeling"); const r = fr.getBoundingClientRect();
		const ev = (type, fx, fy) => fr.dispatchEvent(new PointerEvent(type, { bubbles: true, button: 0, pointerId: 1,
			clientX: r.left + fx * r.width, clientY: r.top + fy * r.height }));
		ev("pointerdown", %[1]f - 0.01, %[2]f - 0.01); ev("pointermove", %[3]f + 0.01, %[4]f + 0.01); ev("pointerup", %[3]f + 0.01, %[4]f + 0.01);
		return true; })()`, box[0], box[1], box[2], box[3]), nil)
	p.waitFor(`document.querySelector(".modal.is-open #f-w") !== null && document.querySelector(".modal.is-open #f-w").value !== ""`)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	p.waitFor(settled + ` && document.querySelector(".result-box.kind-label") !== null`)
	got, err = f.ds.GetDocument(ctx, &beta.GetDocumentRequest{Dataset: f.name + "/dataset", DocumentId: &beta.DocumentId{
		Type: &beta.DocumentId_GcsManagedDocId{GcsManagedDocId: &beta.DocumentId_GCSManagedDocumentId{CwDocId: scanID}}}})
	if err != nil || len(got.GetDocument().GetEntities()) != 1 {
		t.Fatalf("GetDocument after labeling: %v %v", got.GetDocument().GetEntities(), err)
	}
	if e := got.GetDocument().GetEntities()[0]; e.GetType() != "invoice_id" || !strings.HasPrefix(e.GetMentionText(), "2026-0417") ||
		len(e.GetPageAnchor().GetPageRefs()[0].GetBoundingPoly().GetNormalizedVertices()) != 4 {
		t.Errorf("the drawn label: %v", e)
	}

	// Create new version trains and evaluates on the test split.
	p.navigate("/document-ai/processors/" + f.id + "?project=" + f.project)
	p.waitFor(settled)
	p.clickText("#view [role=tab]", "Build")
	if res := p.pageActionWithin("Create new version", map[string]string{"name": "browser-v1"}, "", 10*60e9); !strings.Contains(res, "browser-v1") {
		t.Fatalf("Create new version answered %q", res)
	}
	vers, err := f.dc.ListProcessorVersions(ctx, &documentaipb.ListProcessorVersionsRequest{Parent: f.name}).Next()
	if err != nil || vers.GetLatestEvaluation().GetEvaluation() == "" {
		t.Fatalf("the trained version: %v %v", vers, err)
	}
	vid := vers.GetName()[strings.LastIndex(vers.GetName(), "/")+1:]
	ev := vers.GetLatestEvaluation().GetEvaluation()
	eid := ev[strings.LastIndex(ev, "/")+1:]
	p.clickText("#view [role=tab]", "Evaluate & test")
	p.waitFor(rowReady(eid))
	p.navigate("/document-ai/processors/" + f.id + "/evaluations/" + vid + "/" + eid + "?project=" + f.project)
	p.waitFor(settled + ` && document.querySelector("#view tbody") !== null`)
	var page string
	p.eval(`document.querySelector("#view").textContent`, &page)
	for _, want := range []string{"All labels", "invoice_id", "Precision", "Recall", "F1 score", "optimal"} {
		if !strings.Contains(page, want) {
			t.Errorf("the evaluation page has no %q", want)
		}
	}
	p.pageAction("Confidence threshold", map[string]string{"threshold": "0.50"}, "")
	p.waitFor(`location.pathname.endsWith("/0.50")`)
	p.waitFor(settled + ` && document.querySelector("#view").textContent.includes("0.50, chosen")`)

	// Deploy & use: deploy, then set as default.
	p.navigate("/document-ai/processors/" + f.id + "?project=" + f.project)
	p.waitFor(settled)
	p.clickText("#view [role=tab]", "Deploy & use")
	p.run(chromedp.Click(chromedp.CSS(`#view tbody input[aria-label="Select ` + vid + `"]`)))
	p.eval(`[...document.querySelectorAll("#view button")].find((b) => b.textContent === "Deploy" && !b.disabled).click()`, nil)
	p.waitFor(`document.querySelector(".modal.is-open") !== null`)
	var needsName bool
	p.eval(`document.querySelector(".modal.is-open #confirm-input") !== null`, &needsName)
	if needsName {
		p.confirmByName(f.id)
	} else {
		p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	}
	p.waitFor(`document.querySelector(".modal") === null`)
	p.waitFor(settled)
	p.pageAction("Default version", map[string]string{"version": vid}, "")
	if pr, err := f.dc.GetProcessor(ctx, &documentaipb.GetProcessorRequest{Name: f.name}); err != nil || pr.GetDefaultProcessorVersion() != vers.GetName() {
		t.Errorf("GetProcessor after Deploy & use: %v %v", pr.GetDefaultProcessorVersion(), err)
	}
}

// TestDocumentAICustomProcessorPagesMeetWCAG (#2124): a Custom Extractor's
// Get started, Build, Deploy & use and Evaluate & test tabs, a document's
// labeling page with its page image, and Create new version's dialog.
func TestDocumentAICustomProcessorPagesMeetWCAG(t *testing.T) {
	f := newCustomExtractorFixture(t)
	ctx := context.Background()
	if _, err := f.ds.UpdateDatasetSchema(ctx, &beta.UpdateDatasetSchemaRequest{DatasetSchema: &beta.DatasetSchema{Name: f.name + "/dataset/datasetSchema",
		DocumentSchema: &beta.DocumentSchema{EntityTypes: []*beta.DocumentSchema_EntityType{{Name: "invoice", BaseTypes: []string{"document"},
			Properties: []*beta.DocumentSchema_EntityType_Property{{Name: "invoice_id", ValueType: "string",
				OccurrenceType: beta.DocumentSchema_EntityType_Property_REQUIRED_ONCE}}}}}}}); err != nil {
		t.Fatal(err)
	}
	op, err := f.ds.UpdateDataset(ctx, &beta.UpdateDatasetRequest{Dataset: &beta.Dataset{Name: f.name + "/dataset",
		StorageSource: &beta.Dataset_UnmanagedDatasetConfig_{UnmanagedDatasetConfig: &beta.Dataset_UnmanagedDatasetConfig{}}}})
	if err == nil {
		_, err = op.Wait(ctx)
	}
	if err != nil {
		t.Fatal(err)
	}
	iop, err := f.ds.ImportDocuments(ctx, &beta.ImportDocumentsRequest{Dataset: f.name + "/dataset", BatchDocumentsImportConfigs: []*beta.ImportDocumentsRequest_BatchDocumentsImportConfig{{
		SplitTypeConfig:  &beta.ImportDocumentsRequest_BatchDocumentsImportConfig_DatasetSplit{DatasetSplit: beta.DatasetSplitType_DATASET_SPLIT_TRAIN},
		BatchInputConfig: &beta.BatchDocumentsInputConfig{Source: &beta.BatchDocumentsInputConfig_GcsPrefix{GcsPrefix: &beta.GcsPrefix{GcsUriPrefix: "gs://" + f.bucket + "/train/"}}}}}})
	if err == nil {
		_, err = iop.Wait(ctx)
	}
	if err != nil {
		t.Fatal(err)
	}
	scanID := f.documents(t)["scan.png"]
	base := "/document-ai/processors/" + f.id
	tab := func(name, label string) a11yPage {
		s := screen("custom processor's "+label, base, f.project)
		s.then = fmt.Sprintf(`[...document.querySelectorAll("#view [role=tab]")].find((t) => t.textContent.trim() === %q).click()`, label)
		_ = name
		return s
	}
	label := screen("a document's labeling page", base+"/documents/"+scanID, f.project)
	label.ready = settled + ` && document.querySelector(".result-page-frame img") !== null`
	train := tab("", "Build")
	train.name = "Create new version's dialog"
	train.then += `; setTimeout(() => [...document.querySelectorAll("#view button")].find((b) => b.textContent.trim() === "Create new version").click(), 50)`
	auditScreens(t, []a11yPage{tab("", "Get started"), tab("", "Build"), tab("", "Deploy & use"), tab("", "Evaluate & test"), label, train})
}
