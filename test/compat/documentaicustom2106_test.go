//go:build compat

package compat

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"strings"
	"testing"

	documentai "cloud.google.com/go/documentai/apiv1"
	"cloud.google.com/go/documentai/apiv1/documentaipb"
	documentaibeta "cloud.google.com/go/documentai/apiv1beta3"
	beta "cloud.google.com/go/documentai/apiv1beta3/documentaipb"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

// scanPNG renders lines at 12 points and 300 DPI: a generated scan.
func scanPNG(t *testing.T, lines ...string) []byte {
	t.Helper()
	f, err := opentype.Parse(goregular.TTF)
	if err != nil {
		t.Fatal(err)
	}
	face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: 12, DPI: 300, Hinting: font.HintingFull})
	if err != nil {
		t.Fatal(err)
	}
	defer face.Close()
	g := image.NewGray(image.Rect(0, 0, 1748, 150+len(lines)*75))
	draw.Draw(g, g.Bounds(), image.White, image.Point{}, draw.Src)
	dr := font.Drawer{Dst: g, Src: image.NewUniform(color.Black), Face: face}
	for i, l := range lines {
		dr.Dot = fixed.P(150, 150+i*75)
		dr.DrawString(l)
	}
	var b bytes.Buffer
	if err := png.Encode(&b, g); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// datasetClients are v1beta3's DocumentService clients, gRPC and REST.
func datasetClients(t *testing.T, h *Harness) (*documentaibeta.DocumentClient, *documentaibeta.DocumentClient) {
	t.Helper()
	addr := h.Endpoint(EnvLocalAI)
	g, err := documentaibeta.NewDocumentClient(h.Context(), option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatalf("NewDocumentClient: %v", err)
	}
	t.Cleanup(func() { _ = g.Close() })
	r, err := documentaibeta.NewDocumentRESTClient(h.Context(), option.WithEndpoint("http://"+addr), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("NewDocumentRESTClient: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return g, r
}

// TestDocumentAICustomProcessorsThroughTheOfficialClients (#2106): through
// the official documentai clients (v1 for processors, versions and
// evaluations; v1beta3 for datasets), over gRPC and REST, with the
// instance's Cloud Storage:
//
//   - A Custom Extractor: its dataset given managed Cloud Storage
//     (UpdateDataset) and a schema (UpdateDatasetSchema, GetDatasetSchema);
//     labeled Documents imported into the training and test splits
//     (ImportDocuments, each written under the managed prefix), listed with
//     a filter (ListDocuments), read (GetDocument with a read_mask);
//     TrainProcessorVersion trains a version, UNDEPLOYED, evaluated
//     automatically on the test split (F1 1); ProcessDocument with it is
//     FAILED_PRECONDITION until DeployProcessorVersion; made the default,
//     ProcessDocument of a generated scan answers the invoice number and
//     total; EvaluateProcessorVersion runs again (ListEvaluations two,
//     GetEvaluation with per-entity metrics); the default cannot be
//     undeployed or deleted.
//   - A Custom Classifier over REST: two document types, documents
//     auto-labeled by the import's document_type, auto-split; trained,
//     deployed and run on a scan, the most probable class first;
//     BatchDeleteDocuments by filter; uptrained from the first version.
//
// covers: google.cloud.documentai.v1.DocumentProcessorService/TrainProcessorVersion, google.cloud.documentai.v1.DocumentProcessorService/EvaluateProcessorVersion, google.cloud.documentai.v1.DocumentProcessorService/ListEvaluations, google.cloud.documentai.v1.DocumentProcessorService/GetEvaluation, google.cloud.documentai.v1.DocumentProcessorService/DeployProcessorVersion, google.cloud.documentai.v1.DocumentProcessorService/UndeployProcessorVersion, google.cloud.documentai.v1.DocumentProcessorService/DeleteProcessorVersion, google.cloud.documentai.v1.DocumentProcessorService/SetDefaultProcessorVersion, google.cloud.documentai.v1.DocumentProcessorService/ProcessDocument, google.cloud.documentai.v1beta3.DocumentService/UpdateDataset, google.cloud.documentai.v1beta3.DocumentService/ImportDocuments, google.cloud.documentai.v1beta3.DocumentService/GetDocument, google.cloud.documentai.v1beta3.DocumentService/ListDocuments, google.cloud.documentai.v1beta3.DocumentService/BatchDeleteDocuments, google.cloud.documentai.v1beta3.DocumentService/GetDatasetSchema, google.cloud.documentai.v1beta3.DocumentService/UpdateDatasetSchema
func TestDocumentAICustomProcessorsThroughTheOfficialClients(t *testing.T) {
	h := New(t)
	if h.Endpoint(EnvLocalAI) == "" {
		t.Skip("this instance serves no local AI endpoint")
	}
	if h.Endpoint(EnvStorage) == "" {
		t.Skip("this instance serves no Cloud Storage")
	}
	g, r := documentAIClients(t, h)
	dg, dr := datasetClients(t, h)
	sc := storageClient(t, h)
	ctx := h.Context()
	bh := bucket(t, h, sc)
	gs := "gs://" + bh.BucketName()
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
	invoice := func(id, total string) *documentaipb.Document {
		text := "ACME SUPPLIES\nInvoice number: " + id + "\nTotal: " + total + "\n"
		d := &documentaipb.Document{Text: text}
		for typ, v := range map[string]string{"invoice_id": id, "total_amount": total} {
			i := int64(strings.Index(text, v))
			d.Entities = append(d.Entities, &documentaipb.Document_Entity{Type: typ, MentionText: v,
				TextAnchor: &documentaipb.Document_TextAnchor{TextSegments: []*documentaipb.Document_TextAnchor_TextSegment{{StartIndex: i, EndIndex: i + int64(len(v))}}}})
		}
		return d
	}
	for i, v := range [][2]string{{"INV-1001", "12.50"}, {"INV-1002", "310.00"}, {"INV-1003", "7.25"}} {
		put("ext/train/"+string(rune('a'+i))+".json", invoice(v[0], v[1]))
	}
	put("ext/test/a.json", invoice("INV-2001", "48.10"))
	waitOp := func(what string, wait func() error) {
		t.Helper()
		if err := wait(); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	prefix := func(p string) *beta.BatchDocumentsInputConfig {
		return &beta.BatchDocumentsInputConfig{Source: &beta.BatchDocumentsInputConfig_GcsPrefix{GcsPrefix: &beta.GcsPrefix{GcsUriPrefix: gs + "/" + p}}}
	}

	// --- Custom Extractor, gRPC.
	ext := createDocumentProcessor(t, h, g, h.Project(), "CUSTOM_EXTRACTION_PROCESSOR")
	if ext.GetDefaultProcessorVersion() != "" {
		t.Errorf("a new custom processor has a default version: %s", ext.GetDefaultProcessorVersion())
	}
	ds := ext.GetName() + "/dataset"
	uop, err := dg.UpdateDataset(ctx, &beta.UpdateDatasetRequest{Dataset: &beta.Dataset{Name: ds,
		StorageSource: &beta.Dataset_GcsManagedConfig{GcsManagedConfig: &beta.Dataset_GCSManagedConfig{GcsPrefix: &beta.GcsPrefix{GcsUriPrefix: gs + "/managed"}}}}})
	if err != nil {
		t.Fatalf("UpdateDataset: %v", err)
	}
	dsOut, err := uop.Wait(ctx)
	if err != nil || dsOut.GetState() != beta.Dataset_INITIALIZED {
		t.Fatalf("UpdateDataset's operation: %v %v", dsOut, err)
	}
	if _, err := dg.UpdateDatasetSchema(ctx, &beta.UpdateDatasetSchemaRequest{DatasetSchema: &beta.DatasetSchema{Name: ds + "/datasetSchema",
		DocumentSchema: &beta.DocumentSchema{EntityTypes: []*beta.DocumentSchema_EntityType{{Name: "invoice", BaseTypes: []string{"document"},
			Properties: []*beta.DocumentSchema_EntityType_Property{
				{Name: "invoice_id", ValueType: "string", OccurrenceType: beta.DocumentSchema_EntityType_Property_REQUIRED_ONCE},
				{Name: "total_amount", ValueType: "money", OccurrenceType: beta.DocumentSchema_EntityType_Property_OPTIONAL_ONCE}}}}}}}); err != nil {
		t.Fatalf("UpdateDatasetSchema: %v", err)
	}
	if got, err := dr.GetDatasetSchema(ctx, &beta.GetDatasetSchemaRequest{Name: ds + "/datasetSchema"}); err != nil ||
		len(got.GetDocumentSchema().GetEntityTypes()[0].GetProperties()) != 2 {
		t.Fatalf("GetDatasetSchema: %v %v", got, err)
	}
	for split, p := range map[beta.DatasetSplitType]string{beta.DatasetSplitType_DATASET_SPLIT_TRAIN: "ext/train/", beta.DatasetSplitType_DATASET_SPLIT_TEST: "ext/test/"} {
		op, err := dg.ImportDocuments(ctx, &beta.ImportDocumentsRequest{Dataset: ds, BatchDocumentsImportConfigs: []*beta.ImportDocumentsRequest_BatchDocumentsImportConfig{{
			SplitTypeConfig: &beta.ImportDocumentsRequest_BatchDocumentsImportConfig_DatasetSplit{DatasetSplit: split}, BatchInputConfig: prefix(p)}}})
		if err != nil {
			t.Fatalf("ImportDocuments %s: %v", p, err)
		}
		waitOp("ImportDocuments' operation", func() error { _, err := op.Wait(ctx); return err })
	}
	it := dr.ListDocuments(ctx, &beta.ListDocumentsRequest{Dataset: ds, Filter: "SplitType=DATASET_SPLIT_TRAIN AND LabelingState=DOCUMENT_LABELED"})
	var ids []*beta.DocumentId
	for {
		m, err := it.Next()
		if err != nil {
			break
		}
		ids = append(ids, m.GetDocumentId())
	}
	if len(ids) != 3 || !strings.HasPrefix(ids[0].GetGcsManagedDocId().GetGcsUri(), gs+"/managed/") {
		t.Fatalf("ListDocuments: %v", ids)
	}
	if got, err := dg.GetDocument(ctx, &beta.GetDocumentRequest{Dataset: ds, DocumentId: ids[0]}); err != nil ||
		len(got.GetDocument().GetEntities()) != 2 || !strings.Contains(got.GetDocument().GetText(), "Invoice number") {
		t.Fatalf("GetDocument: %v %v", got, err)
	}
	train := func(c *documentai.DocumentProcessorClient, p *documentaipb.Processor, base string) string {
		t.Helper()
		op, err := c.TrainProcessorVersion(ctx, &documentaipb.TrainProcessorVersionRequest{Parent: p.GetName(),
			ProcessorVersion: &documentaipb.ProcessorVersion{DisplayName: "v1"}, BaseProcessorVersion: base})
		if err != nil {
			t.Fatalf("TrainProcessorVersion: %v", err)
		}
		res, err := op.Wait(ctx)
		if err != nil {
			t.Fatalf("TrainProcessorVersion's operation: %v", err)
		}
		return res.GetProcessorVersion()
	}
	v := train(g, ext, "")
	pv, err := r.GetProcessorVersion(ctx, &documentaipb.GetProcessorVersionRequest{Name: v})
	if err != nil || pv.GetState() != documentaipb.ProcessorVersion_UNDEPLOYED || pv.GetLatestEvaluation().GetAggregateMetrics().GetF1Score() != 1 {
		t.Fatalf("the trained version: %v %v", pv, err)
	}
	scan := func(lines ...string) *documentaipb.ProcessRequest_RawDocument {
		return &documentaipb.ProcessRequest_RawDocument{RawDocument: &documentaipb.RawDocument{Content: scanPNG(t, lines...), MimeType: "image/png"}}
	}
	if _, err := g.ProcessDocument(ctx, &documentaipb.ProcessRequest{Name: v, Source: scan("x")}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("ProcessDocument with an undeployed version: %v", err)
	}
	dop, err := g.DeployProcessorVersion(ctx, &documentaipb.DeployProcessorVersionRequest{Name: v})
	if err != nil {
		t.Fatalf("DeployProcessorVersion: %v", err)
	}
	waitOp("DeployProcessorVersion's operation", func() error { _, err := dop.Wait(ctx); return err })
	sop, err := g.SetDefaultProcessorVersion(ctx, &documentaipb.SetDefaultProcessorVersionRequest{Processor: ext.GetName(), DefaultProcessorVersion: v})
	if err != nil {
		t.Fatalf("SetDefaultProcessorVersion: %v", err)
	}
	waitOp("SetDefaultProcessorVersion's operation", func() error { _, err := sop.Wait(ctx); return err })
	res, err := g.ProcessDocument(ctx, &documentaipb.ProcessRequest{Name: ext.GetName(),
		Source: scan("ACME SUPPLIES", "Invoice number: INV-7788", "Date: 2026-03-02", "Total: 64.20")})
	if err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}
	vals := map[string]string{}
	for _, e := range res.GetDocument().GetEntities() {
		vals[e.GetType()] = e.GetMentionText()
	}
	if vals["invoice_id"] != "INV-7788" || vals["total_amount"] != "64.20" {
		t.Errorf("extracted %v from %q", vals, res.GetDocument().GetText())
	}
	eop, err := r.EvaluateProcessorVersion(ctx, &documentaipb.EvaluateProcessorVersionRequest{ProcessorVersion: v})
	if err != nil {
		t.Fatalf("EvaluateProcessorVersion: %v", err)
	}
	er, err := eop.Wait(ctx)
	if err != nil {
		t.Fatalf("EvaluateProcessorVersion's operation: %v", err)
	}
	ev, err := g.GetEvaluation(ctx, &documentaipb.GetEvaluationRequest{Name: er.GetEvaluation()})
	if err != nil || ev.GetDocumentCounters().GetEvaluatedDocumentsCount() != 1 || ev.GetEntityMetrics()["invoice_id"] == nil ||
		len(ev.GetAllEntitiesMetrics().GetConfidenceLevelMetricsExact()) == 0 {
		t.Fatalf("GetEvaluation: %v %v", ev, err)
	}
	n := 0
	for eit := r.ListEvaluations(ctx, &documentaipb.ListEvaluationsRequest{Parent: v}); ; n++ {
		if _, err := eit.Next(); err != nil {
			break
		}
	}
	if n != 2 {
		t.Errorf("ListEvaluations: %d, want the automatic one and the requested one", n)
	}
	if _, err := g.UndeployProcessorVersion(ctx, &documentaipb.UndeployProcessorVersionRequest{Name: v}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("UndeployProcessorVersion of the default: %v", err)
	}
	if _, err := g.DeleteProcessorVersion(ctx, &documentaipb.DeleteProcessorVersionRequest{Name: v}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DeleteProcessorVersion of the default: %v", err)
	}

	// --- Custom Classifier, REST.
	for i, s := range []string{"Invoice number 1 amount due payment terms net 30", "Invoice bill to customer amount due by", "Invoice remit payment due"} {
		put("cls/inv/"+string(rune('a'+i))+".json", &documentaipb.Document{Text: s})
	}
	for i, s := range []string{"Receipt thank you for shopping cash change", "Receipt store cashier paid card change", "Receipt thank you visit again"} {
		put("cls/rec/"+string(rune('a'+i))+".json", &documentaipb.Document{Text: s})
	}
	cls := createDocumentProcessor(t, h, r, h.Project(), "CUSTOM_CLASSIFICATION_PROCESSOR")
	cds := cls.GetName() + "/dataset"
	cop, err := dr.UpdateDataset(ctx, &beta.UpdateDatasetRequest{Dataset: &beta.Dataset{Name: cds,
		StorageSource: &beta.Dataset_UnmanagedDatasetConfig_{UnmanagedDatasetConfig: &beta.Dataset_UnmanagedDatasetConfig{}}}})
	if err != nil {
		t.Fatalf("UpdateDataset: %v", err)
	}
	waitOp("UpdateDataset's operation", func() error { _, err := cop.Wait(ctx); return err })
	if _, err := dr.UpdateDatasetSchema(ctx, &beta.UpdateDatasetSchemaRequest{DatasetSchema: &beta.DatasetSchema{Name: cds + "/datasetSchema",
		DocumentSchema: &beta.DocumentSchema{EntityTypes: []*beta.DocumentSchema_EntityType{{Name: "invoice"}, {Name: "receipt"}}}}}); err != nil {
		t.Fatalf("UpdateDatasetSchema: %v", err)
	}
	for typ, p := range map[string]string{"invoice": "cls/inv/", "receipt": "cls/rec/"} {
		op, err := dr.ImportDocuments(ctx, &beta.ImportDocumentsRequest{Dataset: cds, BatchDocumentsImportConfigs: []*beta.ImportDocumentsRequest_BatchDocumentsImportConfig{{
			SplitTypeConfig:  &beta.ImportDocumentsRequest_BatchDocumentsImportConfig_AutoSplitConfig_{AutoSplitConfig: &beta.ImportDocumentsRequest_BatchDocumentsImportConfig_AutoSplitConfig{TrainingSplitRatio: 0.67}},
			BatchInputConfig: prefix(p), DocumentType: typ}}})
		if err != nil {
			t.Fatalf("ImportDocuments %s: %v", p, err)
		}
		waitOp("ImportDocuments' operation", func() error { _, err := op.Wait(ctx); return err })
	}
	cv := train(r, cls, "")
	if pv, err := g.GetProcessorVersion(ctx, &documentaipb.GetProcessorVersionRequest{Name: cv}); err != nil || pv.GetLatestEvaluation() == nil {
		t.Fatalf("the classifier's version: %v %v", pv, err)
	}
	dop, err = r.DeployProcessorVersion(ctx, &documentaipb.DeployProcessorVersionRequest{Name: cv})
	if err != nil {
		t.Fatalf("DeployProcessorVersion: %v", err)
	}
	waitOp("DeployProcessorVersion's operation", func() error { _, err := dop.Wait(ctx); return err })
	res, err = r.ProcessDocument(ctx, &documentaipb.ProcessRequest{Name: cv, Source: scan("RECEIPT", "Thank you for shopping", "Paid by card, change 0.00")})
	if err != nil || len(res.GetDocument().GetEntities()) != 2 || res.GetDocument().GetEntities()[0].GetType() != "receipt" {
		t.Fatalf("classified: %v %v", res.GetDocument().GetEntities(), err)
	}
	up := train(r, cls, cv)
	if up == cv {
		t.Errorf("uptraining answered the base version")
	}
	bop, err := dr.BatchDeleteDocuments(ctx, &beta.BatchDeleteDocumentsRequest{Dataset: cds,
		DatasetDocuments: &beta.BatchDatasetDocuments{Criteria: &beta.BatchDatasetDocuments_Filter{Filter: "EntityType=receipt"}}})
	if err != nil {
		t.Fatalf("BatchDeleteDocuments: %v", err)
	}
	waitOp("BatchDeleteDocuments' operation", func() error { _, err := bop.Wait(ctx); return err })
	left := 0
	for lit := dg.ListDocuments(ctx, &beta.ListDocumentsRequest{Dataset: cds}); ; left++ {
		if _, err := lit.Next(); err != nil {
			break
		}
	}
	if left != 3 {
		t.Errorf("after deleting the receipts, %d documents", left)
	}
	uop2, err := g.UndeployProcessorVersion(ctx, &documentaipb.UndeployProcessorVersionRequest{Name: cv})
	if err != nil {
		t.Fatalf("UndeployProcessorVersion: %v", err)
	}
	waitOp("UndeployProcessorVersion's operation", func() error { _, err := uop2.Wait(ctx); return err })
	delop, err := g.DeleteProcessorVersion(ctx, &documentaipb.DeleteProcessorVersionRequest{Name: cv})
	if err != nil {
		t.Fatalf("DeleteProcessorVersion: %v", err)
	}
	waitOp("DeleteProcessorVersion's operation", func() error { return delop.Wait(ctx) })
	if _, err := g.GetProcessorVersion(ctx, &documentaipb.GetProcessorVersionRequest{Name: cv}); status.Code(err) != codes.NotFound {
		t.Errorf("a deleted version: %v", err)
	}
}
