package vertexai

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"strings"
	"testing"

	"cloud.google.com/go/documentai/apiv1/documentaipb"
	beta "cloud.google.com/go/documentai/apiv1beta3/documentaipb"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
)

// textPNG renders lines at 12 points, 300 DPI: a generated scan.
func textPNG(t *testing.T, lines ...string) []byte {
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
	_ = png.Encode(&b, g)
	return b.Bytes()
}

// labeled is a Document whose entities label each value's first
// occurrence in text.
func labeled(text string, values map[string]string) *documentaipb.Document {
	d := &documentaipb.Document{Text: text}
	r := []rune(text)
	for typ, v := range values {
		i := strings.Index(text, v)
		st := len([]rune(text[:i]))
		d.Entities = append(d.Entities, &documentaipb.Document_Entity{Type: typ, MentionText: v,
			TextAnchor: anchor(int64(st), int64(st+len([]rune(v))))})
		_ = r
	}
	return d
}

func classDoc(text, class string) *documentaipb.Document {
	return &documentaipb.Document{Text: text, Entities: []*documentaipb.Document_Entity{{Type: class, Confidence: 1}}}
}

func putJSON(fb *fakeBucket, name string, d *documentaipb.Document) {
	b, _ := protojson.Marshal(d)
	fb.objs["b/"+name], fb.ct["b/"+name] = b, "application/json"
}

// TestDocumentAICustomProcessors (#2106): a Custom Extractor and a Custom
// Classifier, each with a dataset (configured, schema, labeled imports in
// the training and test splits), trained, evaluated (automatically and on
// request), deployed, made default and run on a generated scan; a Custom
// Splitter trained and evaluated; the refusals in between.
func TestDocumentAICustomProcessors(t *testing.T) {
	ctx := context.Background()
	d := newDocAI(t, t.TempDir()+"/processors.json")
	fb := &fakeBucket{objs: map[string][]byte{}, ct: map[string]string{}}
	d.s.UseStorage(fb)
	ds := &documentService{s: d.s}

	// --- extractor
	ext := createProcessor(t, d, "CUSTOM_EXTRACTION_PROCESSOR", "ext")
	if ext.GetDefaultProcessorVersion() != "" {
		t.Errorf("a custom processor has a default version %q", ext.GetDefaultProcessorVersion())
	}
	set := func(name string, i int, inv, total string) {
		putJSON(fb, name, labeled("ACME SUPPLIES\nInvoice number: "+inv+"\nDate: 2026-0"+string(rune('1'+i))+"-11\nTotal: "+total+"\n",
			map[string]string{"invoice_id": inv, "total_amount": total}))
	}
	for i, v := range [][2]string{{"INV-1001", "12.50"}, {"INV-1002", "310.00"}, {"INV-1003", "7.25"}} {
		set("ext/train/"+string(rune('a'+i))+".json", i, v[0], v[1])
	}
	set("ext/test/a.json", 4, "INV-2001", "48.10")
	if _, err := ds.ImportDocuments(ctx, &beta.ImportDocumentsRequest{Dataset: ext.GetName() + "/dataset"}); code(err) != codes.FailedPrecondition {
		t.Errorf("import before UpdateDataset: %v", err)
	}
	if _, err := ds.UpdateDataset(ctx, &beta.UpdateDatasetRequest{Dataset: &beta.Dataset{Name: ext.GetName() + "/dataset",
		StorageSource: &beta.Dataset_GcsManagedConfig{GcsManagedConfig: &beta.Dataset_GCSManagedConfig{GcsPrefix: &beta.GcsPrefix{GcsUriPrefix: "gs://b/ds"}}}}}); err != nil {
		t.Fatal(err)
	}
	schema := &beta.DocumentSchema{EntityTypes: []*beta.DocumentSchema_EntityType{{Name: "invoice", BaseTypes: []string{"document"},
		Properties: []*beta.DocumentSchema_EntityType_Property{{Name: "invoice_id", ValueType: "string", OccurrenceType: beta.DocumentSchema_EntityType_Property_REQUIRED_ONCE},
			{Name: "total_amount", ValueType: "money", OccurrenceType: beta.DocumentSchema_EntityType_Property_OPTIONAL_ONCE}}}}}
	if _, err := ds.UpdateDatasetSchema(ctx, &beta.UpdateDatasetSchemaRequest{DatasetSchema: &beta.DatasetSchema{Name: ext.GetName() + "/dataset/datasetSchema", DocumentSchema: schema}}); err != nil {
		t.Fatal(err)
	}
	imp := func(p *documentaipb.Processor, prefix string, split beta.DatasetSplitType, typ string) {
		t.Helper()
		op, err := ds.ImportDocuments(ctx, &beta.ImportDocumentsRequest{Dataset: p.GetName() + "/dataset", BatchDocumentsImportConfigs: []*beta.ImportDocumentsRequest_BatchDocumentsImportConfig{{
			SplitTypeConfig:  &beta.ImportDocumentsRequest_BatchDocumentsImportConfig_DatasetSplit{DatasetSplit: split},
			BatchInputConfig: &beta.BatchDocumentsInputConfig{Source: &beta.BatchDocumentsInputConfig_GcsPrefix{GcsPrefix: &beta.GcsPrefix{GcsUriPrefix: "gs://b/" + prefix}}},
			DocumentType:     typ}}})
		if err != nil || !op.GetDone() {
			t.Fatalf("ImportDocuments %s: %v %v", prefix, op, err)
		}
	}
	imp(ext, "ext/train/", beta.DatasetSplitType_DATASET_SPLIT_TRAIN, "")
	imp(ext, "ext/test/", beta.DatasetSplitType_DATASET_SPLIT_TEST, "")
	list, err := ds.ListDocuments(ctx, &beta.ListDocumentsRequest{Dataset: ext.GetName() + "/dataset", Filter: "SplitType=DATASET_SPLIT_TRAIN AND LabelingState=DOCUMENT_LABELED", ReturnTotalSize: true})
	if err != nil || list.GetTotalSize() != 3 || !strings.HasPrefix(list.GetDocumentMetadata()[0].GetDocumentId().GetGcsManagedDocId().GetGcsUri(), "gs://b/ds/") {
		t.Fatalf("ListDocuments: %v %v", list, err)
	}
	got, err := ds.GetDocument(ctx, &beta.GetDocumentRequest{Dataset: ext.GetName() + "/dataset", DocumentId: list.GetDocumentMetadata()[0].GetDocumentId()})
	if err != nil || len(got.GetDocument().GetEntities()) != 2 {
		t.Fatalf("GetDocument: %v %v", got, err)
	}

	train := func(p *documentaipb.Processor) string {
		t.Helper()
		op, err := d.TrainProcessorVersion(ctx, &documentaipb.TrainProcessorVersionRequest{Parent: p.GetName(),
			ProcessorVersion: &documentaipb.ProcessorVersion{DisplayName: "v1"}})
		if err != nil || !op.GetDone() {
			t.Fatalf("TrainProcessorVersion: %v %v", op, err)
		}
		var r documentaipb.TrainProcessorVersionResponse
		_ = op.GetResponse().UnmarshalTo(&r)
		return r.GetProcessorVersion()
	}
	v := train(ext)
	pv, err := d.GetProcessorVersion(ctx, &documentaipb.GetProcessorVersionRequest{Name: v})
	if err != nil || pv.GetState() != documentaipb.ProcessorVersion_UNDEPLOYED || pv.GetLatestEvaluation().GetAggregateMetrics().GetF1Score() != 1 {
		t.Fatalf("trained version: %v %v", pv, err)
	}
	if _, err := d.ProcessDocument(ctx, &documentaipb.ProcessRequest{Name: v, Source: &documentaipb.ProcessRequest_RawDocument{RawDocument: &documentaipb.RawDocument{
		Content: textPNG(t, "x"), MimeType: "image/png"}}}); code(err) != codes.FailedPrecondition {
		t.Errorf("process with an undeployed version: %v", err)
	}
	if op, err := d.DeployProcessorVersion(ctx, &documentaipb.DeployProcessorVersionRequest{Name: v}); err != nil || !op.GetDone() {
		t.Fatal(op, err)
	}
	if _, err := d.SetDefaultProcessorVersion(ctx, &documentaipb.SetDefaultProcessorVersionRequest{Processor: ext.GetName(), DefaultProcessorVersion: v}); err != nil {
		t.Fatal(err)
	}
	res, err := d.ProcessDocument(ctx, &documentaipb.ProcessRequest{Name: ext.GetName(), Source: &documentaipb.ProcessRequest_RawDocument{RawDocument: &documentaipb.RawDocument{
		Content: textPNG(t, "ACME SUPPLIES", "Invoice number: INV-7788", "Date: 2026-03-02", "Total: 64.20"), MimeType: "image/png"}}})
	if err != nil {
		t.Fatal(err)
	}
	vals := map[string]string{}
	for _, e := range res.GetDocument().GetEntities() {
		vals[e.GetType()] = e.GetMentionText()
	}
	if vals["invoice_id"] != "INV-7788" || vals["total_amount"] != "64.20" {
		t.Errorf("extracted %v from %q", vals, res.GetDocument().GetText())
	}
	if _, err := d.DeleteProcessorVersion(ctx, &documentaipb.DeleteProcessorVersionRequest{Name: v}); code(err) != codes.FailedPrecondition {
		t.Errorf("delete the default: %v", err)
	}
	evs, err := d.ListEvaluations(ctx, &documentaipb.ListEvaluationsRequest{Parent: v})
	if err != nil || len(evs.GetEvaluations()) != 1 {
		t.Fatalf("ListEvaluations: %v %v", evs, err)
	}

	// --- classifier
	cls := createProcessor(t, d, "CUSTOM_CLASSIFICATION_PROCESSOR", "cls")
	if _, err := ds.UpdateDataset(ctx, &beta.UpdateDatasetRequest{Dataset: &beta.Dataset{Name: cls.GetName() + "/dataset",
		StorageSource: &beta.Dataset_UnmanagedDatasetConfig_{UnmanagedDatasetConfig: &beta.Dataset_UnmanagedDatasetConfig{}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := ds.UpdateDatasetSchema(ctx, &beta.UpdateDatasetSchemaRequest{DatasetSchema: &beta.DatasetSchema{Name: cls.GetName() + "/dataset/datasetSchema",
		DocumentSchema: &beta.DocumentSchema{EntityTypes: []*beta.DocumentSchema_EntityType{{Name: "invoice"}, {Name: "receipt"}}}}}); err != nil {
		t.Fatal(err)
	}
	for i, s := range []string{"Invoice number 1 amount due payment terms net 30", "Invoice bill to customer amount due by", "Invoice remit payment due"} {
		putJSON(fb, "cls/inv/train"+string(rune('a'+i))+".json", &documentaipb.Document{Text: s})
	}
	for i, s := range []string{"Receipt thank you for shopping cash change", "Receipt store cashier paid card change", "Receipt thank you visit again"} {
		putJSON(fb, "cls/rec/train"+string(rune('a'+i))+".json", &documentaipb.Document{Text: s})
	}
	imp(cls, "cls/inv/", beta.DatasetSplitType_DATASET_SPLIT_TRAIN, "invoice")
	imp(cls, "cls/rec/", beta.DatasetSplitType_DATASET_SPLIT_TRAIN, "receipt")
	if _, err := d.TrainProcessorVersion(ctx, &documentaipb.TrainProcessorVersionRequest{Parent: cls.GetName(),
		ProcessorVersion: &documentaipb.ProcessorVersion{DisplayName: "v1"}}); code(err) != codes.InvalidArgument {
		t.Errorf("training without test documents: %v", err)
	}
	putJSON(fb, "cls/test/inv.json", classDoc("Invoice amount due payment", "invoice"))
	putJSON(fb, "cls/test/rec.json", classDoc("Receipt thank you change", "receipt"))
	imp(cls, "cls/test/", beta.DatasetSplitType_DATASET_SPLIT_TEST, "")
	cv := train(cls)
	op, err := d.EvaluateProcessorVersion(ctx, &documentaipb.EvaluateProcessorVersionRequest{ProcessorVersion: cv})
	if err != nil {
		t.Fatal(err)
	}
	var er documentaipb.EvaluateProcessorVersionResponse
	_ = op.GetResponse().UnmarshalTo(&er)
	ev, err := d.GetEvaluation(ctx, &documentaipb.GetEvaluationRequest{Name: er.GetEvaluation()})
	if err != nil || ev.GetDocumentCounters().GetEvaluatedDocumentsCount() != 2 || ev.GetAllEntitiesMetrics().GetConfidenceLevelMetrics()[0].GetMetrics().GetF1Score() != 1 ||
		ev.GetEntityMetrics()["receipt"] == nil {
		t.Fatalf("GetEvaluation: %v %v", ev, err)
	}
	if _, err := d.DeployProcessorVersion(ctx, &documentaipb.DeployProcessorVersionRequest{Name: cv}); err != nil {
		t.Fatal(err)
	}
	res, err = d.ProcessDocument(ctx, &documentaipb.ProcessRequest{Name: cv, Source: &documentaipb.ProcessRequest_RawDocument{RawDocument: &documentaipb.RawDocument{
		Content: textPNG(t, "RECEIPT", "Thank you for shopping", "Paid by card, change 0.00"), MimeType: "image/png"}}})
	if err != nil || len(res.GetDocument().GetEntities()) != 2 || res.GetDocument().GetEntities()[0].GetType() != "receipt" {
		t.Fatalf("classified: %v %v", res.GetDocument().GetEntities(), err)
	}

	// --- splitter: a three-page packet, an invoice then a two-page receipt.
	spl := createProcessor(t, d, "CUSTOM_SPLITTING_PROCESSOR", "spl")
	packet := func(texts []string, ents map[string][]int64) *documentaipb.Document {
		doc := &documentaipb.Document{}
		for _, t := range texts {
			st := int64(len([]rune(doc.Text)))
			doc.Text += t + "\n"
			doc.Pages = append(doc.Pages, &documentaipb.Document_Page{PageNumber: int32(len(doc.Pages) + 1),
				Layout: &documentaipb.Document_Page_Layout{TextAnchor: anchor(st, int64(len([]rune(doc.Text))))}})
		}
		for typ, pages := range ents {
			e := &documentaipb.Document_Entity{Type: typ, PageAnchor: &documentaipb.Document_PageAnchor{}}
			for _, p := range pages {
				e.PageAnchor.PageRefs = append(e.PageAnchor.PageRefs, &documentaipb.Document_PageAnchor_PageRef{Page: p})
			}
			doc.Entities = append(doc.Entities, e)
		}
		return doc
	}
	pk := packet([]string{"Invoice number amount due payment", "Receipt thank you shopping", "Receipt items change cash"},
		map[string][]int64{"invoice": {0}, "receipt": {1, 2}})
	trainDoc, _ := protojson.Marshal(pk)
	fb.objs["b/spl/train.json"], fb.ct["b/spl/train.json"] = trainDoc, "application/json"
	fb.objs["b/spl/test.json"], fb.ct["b/spl/test.json"] = trainDoc, "application/json"
	op, err = d.TrainProcessorVersion(ctx, &documentaipb.TrainProcessorVersionRequest{Parent: spl.GetName(),
		ProcessorVersion: &documentaipb.ProcessorVersion{DisplayName: "v1"},
		DocumentSchema:   &documentaipb.DocumentSchema{EntityTypes: []*documentaipb.DocumentSchema_EntityType{{Name: "invoice"}, {Name: "receipt"}}},
		InputData: &documentaipb.TrainProcessorVersionRequest_InputData{
			TrainingDocuments: &documentaipb.BatchDocumentsInputConfig{Source: &documentaipb.BatchDocumentsInputConfig_GcsDocuments{GcsDocuments: &documentaipb.GcsDocuments{Documents: []*documentaipb.GcsDocument{{GcsUri: "gs://b/spl/train.json", MimeType: "application/json"}}}}},
			TestDocuments:     &documentaipb.BatchDocumentsInputConfig{Source: &documentaipb.BatchDocumentsInputConfig_GcsDocuments{GcsDocuments: &documentaipb.GcsDocuments{Documents: []*documentaipb.GcsDocument{{GcsUri: "gs://b/spl/test.json", MimeType: "application/json"}}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	var tr documentaipb.TrainProcessorVersionResponse
	_ = op.GetResponse().UnmarshalTo(&tr)
	sv, _ := d.GetProcessorVersion(ctx, &documentaipb.GetProcessorVersionRequest{Name: tr.GetProcessorVersion()})
	if sv.GetLatestEvaluation().GetAggregateMetrics().GetF1Score() != 1 {
		t.Errorf("splitter evaluation: %v", sv.GetLatestEvaluation())
	}

	// The state survives a restart.
	d2 := newDocAI(t, d.s.docai.file)
	if v2, err := d2.GetProcessorVersion(ctx, &documentaipb.GetProcessorVersionRequest{Name: v}); err != nil || v2.GetState() != documentaipb.ProcessorVersion_DEPLOYED {
		t.Errorf("after a restart: %v %v", v2, err)
	}
}
