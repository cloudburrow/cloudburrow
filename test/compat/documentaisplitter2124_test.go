//go:build compat

package compat

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"cloud.google.com/go/documentai/apiv1/documentaipb"
	beta "cloud.google.com/go/documentai/apiv1beta3/documentaipb"
	"google.golang.org/protobuf/encoding/protojson"
)

// textPDF is a PDF of one page per entry, each line of it in 24-point
// Helvetica: a generated multi-page file.
func textPDF(pages ...[]string) []byte {
	var b bytes.Buffer
	var offs []int
	obj := func(body string) {
		offs = append(offs, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", len(offs), body)
	}
	b.WriteString("%PDF-1.4\n")
	n := len(pages)
	// 1 catalog, 2 pages, 3 font, then a page and its content each.
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	var kids []string
	for i := range pages {
		kids = append(kids, fmt.Sprintf("%d 0 R", 4+2*i))
	}
	obj(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), n))
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	for i, lines := range pages {
		obj(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>", 5+2*i))
		var s strings.Builder
		s.WriteString("BT /F1 24 Tf 72 700 Td 32 TL\n")
		for _, l := range lines {
			fmt.Fprintf(&s, "(%s) Tj T*\n", l)
		}
		s.WriteString("ET")
		obj(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", s.Len(), s.String()))
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offs)+1)
	for _, o := range offs {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offs)+1, xref)
	return b.Bytes()
}

// TestDocumentAICustomSplitterThroughTheOfficialClient (#2124): a Custom
// Splitter through the official documentai clients (v1 for the processor
// and its versions, v1beta3 for its dataset), gRPC: an unmanaged dataset
// with two document types; labeled multi-page Documents (each subdocument
// an entity with its page_anchor) imported into the training and test
// splits; TrainProcessorVersion (generative where the instance serves a
// local model, its labeled documents the examples, else the counting
// model) evaluated automatically on the test split; deployed and made the
// default, ProcessDocument of a three-page PDF (an invoice, then a
// two-page receipt) answers the subdocuments, each with its type,
// confidence and pages, covering every page once.
//
// covers: google.cloud.documentai.v1.DocumentProcessorService/TrainProcessorVersion, google.cloud.documentai.v1.DocumentProcessorService/ProcessDocument, google.cloud.documentai.v1.DocumentProcessorService/DeployProcessorVersion, google.cloud.documentai.v1beta3.DocumentService/ImportDocuments
func TestDocumentAICustomSplitterThroughTheOfficialClient(t *testing.T) {
	h := New(t)
	if h.Endpoint(EnvLocalAI) == "" {
		t.Skip("this instance serves no local AI endpoint")
	}
	if h.Endpoint(EnvStorage) == "" {
		t.Skip("this instance serves no Cloud Storage")
	}
	g, _ := documentAIClients(t, h)
	dg, _ := datasetClients(t, h)
	sc := storageClient(t, h)
	ctx := h.Context()
	bh := bucket(t, h, sc)
	gs := "gs://" + bh.BucketName()

	// packet is a labeled file: its pages' texts, and each subdocument's type and pages.
	packet := func(pages []string, subs ...any) *documentaipb.Document {
		d := &documentaipb.Document{}
		for _, p := range pages {
			st := int64(len([]rune(d.Text)))
			d.Text += p + "\n"
			d.Pages = append(d.Pages, &documentaipb.Document_Page{PageNumber: int32(len(d.Pages) + 1),
				Layout: &documentaipb.Document_Page_Layout{TextAnchor: &documentaipb.Document_TextAnchor{
					TextSegments: []*documentaipb.Document_TextAnchor_TextSegment{{StartIndex: st, EndIndex: int64(len([]rune(d.Text)))}}}}})
		}
		for i := 0; i < len(subs); i += 2 {
			e := &documentaipb.Document_Entity{Type: subs[i].(string), Confidence: 1, PageAnchor: &documentaipb.Document_PageAnchor{}}
			for _, p := range subs[i+1].([]int64) {
				e.PageAnchor.PageRefs = append(e.PageAnchor.PageRefs, &documentaipb.Document_PageAnchor_PageRef{Page: p})
			}
			d.Entities = append(d.Entities, e)
		}
		return d
	}
	put := func(name string, d *documentaipb.Document) {
		t.Helper()
		b, _ := protojson.Marshal(d)
		w := bh.Object(name).NewWriter(ctx)
		w.ContentType = "application/json"
		if _, err := w.Write(b); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("upload %s: %v", name, err)
		}
	}
	inv := "INVOICE Invoice number 4411 amount due payment terms"
	rec := "RECEIPT Thank you for shopping paid by card"
	rec2 := "RECEIPT items total change cash"
	put("spl/train/a.json", packet([]string{inv, rec, rec2}, "invoice", []int64{0}, "receipt", []int64{1, 2}))
	put("spl/train/b.json", packet([]string{rec, inv, "Invoice continued remit payment"}, "receipt", []int64{0}, "invoice", []int64{1, 2}))
	put("spl/test/a.json", packet([]string{inv, rec}, "invoice", []int64{0}, "receipt", []int64{1}))
	put("spl/test/b.json", packet([]string{rec2, inv}, "receipt", []int64{0}, "invoice", []int64{1}))

	p := createDocumentProcessor(t, h, g, h.Project(), "CUSTOM_SPLITTING_PROCESSOR")
	ds := p.GetName() + "/dataset"
	uop, err := dg.UpdateDataset(ctx, &beta.UpdateDatasetRequest{Dataset: &beta.Dataset{Name: ds,
		StorageSource: &beta.Dataset_UnmanagedDatasetConfig_{UnmanagedDatasetConfig: &beta.Dataset_UnmanagedDatasetConfig{}}}})
	if err != nil {
		t.Fatalf("UpdateDataset: %v", err)
	}
	if _, err := uop.Wait(ctx); err != nil {
		t.Fatalf("UpdateDataset: %v", err)
	}
	if _, err := dg.UpdateDatasetSchema(ctx, &beta.UpdateDatasetSchemaRequest{DatasetSchema: &beta.DatasetSchema{Name: ds + "/datasetSchema",
		DocumentSchema: &beta.DocumentSchema{Description: "Invoices and receipts, scanned into one file",
			EntityTypes: []*beta.DocumentSchema_EntityType{{Name: "invoice"}, {Name: "receipt"}}}}}); err != nil {
		t.Fatalf("UpdateDatasetSchema: %v", err)
	}
	for prefix, split := range map[string]beta.DatasetSplitType{"spl/train/": beta.DatasetSplitType_DATASET_SPLIT_TRAIN, "spl/test/": beta.DatasetSplitType_DATASET_SPLIT_TEST} {
		op, err := dg.ImportDocuments(ctx, &beta.ImportDocumentsRequest{Dataset: ds, BatchDocumentsImportConfigs: []*beta.ImportDocumentsRequest_BatchDocumentsImportConfig{{
			SplitTypeConfig:  &beta.ImportDocumentsRequest_BatchDocumentsImportConfig_DatasetSplit{DatasetSplit: split},
			BatchInputConfig: &beta.BatchDocumentsInputConfig{Source: &beta.BatchDocumentsInputConfig_GcsPrefix{GcsPrefix: &beta.GcsPrefix{GcsUriPrefix: gs + "/" + prefix}}}}}})
		if err != nil {
			t.Fatalf("ImportDocuments %s: %v", prefix, err)
		}
		if _, err := op.Wait(ctx); err != nil {
			t.Fatalf("ImportDocuments %s: %v", prefix, err)
		}
	}
	top, err := g.TrainProcessorVersion(ctx, &documentaipb.TrainProcessorVersionRequest{Parent: p.GetName(),
		ProcessorVersion: &documentaipb.ProcessorVersion{DisplayName: "splitter-v1"}})
	if err != nil {
		t.Fatalf("TrainProcessorVersion: %v", err)
	}
	tr, err := top.Wait(ctx)
	if err != nil {
		t.Fatalf("TrainProcessorVersion: %v", err)
	}
	pv, err := g.GetProcessorVersion(ctx, &documentaipb.GetProcessorVersionRequest{Name: tr.GetProcessorVersion()})
	if err != nil || pv.GetState() != documentaipb.ProcessorVersion_UNDEPLOYED || pv.GetLatestEvaluation().GetEvaluation() == "" {
		t.Fatalf("trained version: %v %v", pv, err)
	}
	t.Logf("splitter version %s (%s), F1 %.2f on the test split", pv.GetDisplayName(), pv.GetModelType(),
		pv.GetLatestEvaluation().GetAggregateMetrics().GetF1Score())
	ev, err := g.GetEvaluation(ctx, &documentaipb.GetEvaluationRequest{Name: pv.GetLatestEvaluation().GetEvaluation()})
	if err != nil || ev.GetDocumentCounters().GetEvaluatedDocumentsCount() != 2 || ev.GetEntityMetrics()["invoice"] == nil {
		t.Fatalf("GetEvaluation: %v %v", ev, err)
	}
	dop, err := g.DeployProcessorVersion(ctx, &documentaipb.DeployProcessorVersionRequest{Name: pv.GetName()})
	if err != nil {
		t.Fatalf("DeployProcessorVersion: %v", err)
	}
	if _, err := dop.Wait(ctx); err != nil {
		t.Fatalf("DeployProcessorVersion: %v", err)
	}
	sop, err := g.SetDefaultProcessorVersion(ctx, &documentaipb.SetDefaultProcessorVersionRequest{Processor: p.GetName(), DefaultProcessorVersion: pv.GetName()})
	if err != nil {
		t.Fatalf("SetDefaultProcessorVersion: %v", err)
	}
	if _, err := sop.Wait(ctx); err != nil {
		t.Fatalf("SetDefaultProcessorVersion: %v", err)
	}
	pdf := textPDF([]string{"INVOICE", "Invoice number 9001", "Amount due 120.00", "Payment terms net 30"},
		[]string{"RECEIPT", "Thank you for shopping", "Paid by card"}, []string{"RECEIPT", "Items total 18.40", "Change cash 0.00"})
	res, err := g.ProcessDocument(ctx, &documentaipb.ProcessRequest{Name: p.GetName(), Source: &documentaipb.ProcessRequest_RawDocument{
		RawDocument: &documentaipb.RawDocument{Content: pdf, MimeType: "application/pdf"}}})
	if err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}
	doc := res.GetDocument()
	if len(doc.GetPages()) != 3 {
		t.Fatalf("ProcessDocument read %d pages of 3: %q", len(doc.GetPages()), doc.GetText())
	}
	seen := map[int64]int{}
	var got []string
	for _, e := range doc.GetEntities() {
		if e.GetType() != "invoice" && e.GetType() != "receipt" || e.GetConfidence() <= 0 || e.GetConfidence() > 1 {
			t.Errorf("subdocument %v", e)
		}
		var pages []string
		for _, r := range e.GetPageAnchor().GetPageRefs() {
			seen[r.GetPage()]++
			pages = append(pages, fmt.Sprint(r.GetPage()))
		}
		got = append(got, e.GetType()+":"+strings.Join(pages, ","))
	}
	for i := int64(0); i < 3; i++ {
		if seen[i] != 1 {
			t.Errorf("page %d is in %d subdocuments: %v", i, seen[i], got)
		}
	}
	if strings.Join(got, " ") != "invoice:0 receipt:1,2" {
		t.Errorf("subdocuments %v, want invoice:0 receipt:1,2 (text %q)", got, doc.GetText())
	}
}
