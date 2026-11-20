//go:build compat

package compat

import (
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	documentai "cloud.google.com/go/documentai/apiv1"
	"cloud.google.com/go/documentai/apiv1/documentaipb"
	"cloud.google.com/go/storage"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// TestDocumentAIEvaluationsAndReview (#1953): through the official
// documentai client over gRPC and REST, a served version has no
// evaluations (ListEvaluations empty, GetEvaluation NOT_FOUND) and cannot
// be evaluated (EvaluateProcessorVersion INVALID_ARGUMENT: evaluation
// scores a custom processor's entities against a labeled dataset);
// ReviewDocument, Google's human review, is UNIMPLEMENTED.
//
// covers: google.cloud.documentai.v1.DocumentProcessorService/ListEvaluations, google.cloud.documentai.v1.DocumentProcessorService/GetEvaluation, google.cloud.documentai.v1.DocumentProcessorService/EvaluateProcessorVersion
func TestDocumentAIEvaluationsAndReview(t *testing.T) {
	h := New(t)
	g, r := documentAIClients(t, h)
	ctx := h.Context()
	p := createDocumentProcessor(t, h, g, h.Project(), "LAYOUT_PARSER_PROCESSOR")
	v := p.GetDefaultProcessorVersion()
	for transport, c := range map[string]*documentai.DocumentProcessorClient{"grpc": g, "rest": r} {
		it := c.ListEvaluations(ctx, &documentaipb.ListEvaluationsRequest{Parent: v})
		if e, err := it.Next(); err == nil || !strings.Contains(err.Error(), "no more items") {
			t.Errorf("%s ListEvaluations: %v %v", transport, e, err)
		}
		if _, err := c.GetEvaluation(ctx, &documentaipb.GetEvaluationRequest{Name: v + "/evaluations/e1"}); status.Code(err) != codes.NotFound &&
			!strings.Contains(fmt.Sprint(err), "404") {
			t.Errorf("%s GetEvaluation: %v", transport, err)
		}
		if _, err := c.EvaluateProcessorVersion(ctx, &documentaipb.EvaluateProcessorVersionRequest{ProcessorVersion: v}); status.Code(err) != codes.InvalidArgument &&
			!strings.Contains(fmt.Sprint(err), "400") {
			t.Errorf("%s EvaluateProcessorVersion: %v", transport, err)
		}
		if _, err := c.ReviewDocument(ctx, &documentaipb.ReviewDocumentRequest{HumanReviewConfig: p.GetName() + "/humanReviewConfig"}); status.Code(err) != codes.Unimplemented &&
			!strings.Contains(fmt.Sprint(err), "501") {
			t.Errorf("%s ReviewDocument: %v", transport, err)
		}
	}
	if _, err := g.EvaluateProcessorVersion(ctx, &documentaipb.EvaluateProcessorVersionRequest{ProcessorVersion: p.GetName() + "/processorVersions/nope"}); status.Code(err) != codes.NotFound {
		t.Errorf("EvaluateProcessorVersion of an unknown version: %v", err)
	}
}

// TestDocumentAIBatchShardsAndFieldMask (#1953): BatchProcessDocuments of
// two PDFs (gcs_documents) on a named version, with skip_human_review,
// sharding_config (two pages a shard) and field_mask, read back with the
// official storage client: the seven-page PDF in four shards and the
// two-page one in one, each {file name}-{shard}.json with its shard_info,
// its pages and the text they anchor, and nothing the mask leaves out; the
// metadata's per-document status with human review SKIPPED. Then
// ProcessDocument over REST with a camelCase field mask and an
// inline_document naming a gs:// object.
//
// covers: google.cloud.documentai.v1.DocumentProcessorService/BatchProcessDocuments, google.cloud.documentai.v1.DocumentProcessorService/ProcessDocument
func TestDocumentAIBatchShardsAndFieldMask(t *testing.T) {
	h := New(t)
	if h.Endpoint(EnvStorage) == "" {
		t.Skip("this instance serves no Cloud Storage")
	}
	g, r := documentAIClients(t, h)
	sc := storageClient(t, h)
	ctx := h.Context()
	bh := bucket(t, h, sc)
	for _, f := range []string{"seven-pages.pdf", "two-pages.pdf"} {
		b, err := os.ReadFile("../../internal/embedding/testdata/" + f)
		if err != nil {
			t.Fatal(err)
		}
		w := bh.Object("in/" + f).NewWriter(ctx)
		w.ContentType = "application/pdf"
		if _, err := w.Write(b); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("upload %s: %v", f, err)
		}
	}
	read := func(name string) []byte {
		t.Helper()
		rd, err := bh.Object(name).NewReader(ctx)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		defer func() { _ = rd.Close() }()
		b, err := io.ReadAll(rd)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	p := createDocumentProcessor(t, h, g, h.Project(), "OCR_PROCESSOR")
	gs := "gs://" + bh.BucketName()
	op, err := g.BatchProcessDocuments(ctx, &documentaipb.BatchProcessRequest{
		Name: p.GetName() + "/processorVersions/pretrained-ocr-v2.1-2024-08-07", SkipHumanReview: true,
		InputDocuments: &documentaipb.BatchDocumentsInputConfig{Source: &documentaipb.BatchDocumentsInputConfig_GcsDocuments{
			GcsDocuments: &documentaipb.GcsDocuments{Documents: []*documentaipb.GcsDocument{
				{GcsUri: gs + "/in/seven-pages.pdf", MimeType: "application/pdf"}, {GcsUri: gs + "/in/two-pages.pdf", MimeType: "application/pdf"}}}}},
		DocumentOutputConfig: &documentaipb.DocumentOutputConfig{Destination: &documentaipb.DocumentOutputConfig_GcsOutputConfig_{
			GcsOutputConfig: &documentaipb.DocumentOutputConfig_GcsOutputConfig{GcsUri: gs + "/out",
				FieldMask:      &fieldmaskpb.FieldMask{Paths: []string{"text", "pages.page_number", "pages.layout", "pages.lines"}},
				ShardingConfig: &documentaipb.DocumentOutputConfig_GcsOutputConfig_ShardingConfig{PagesPerShard: 2}}}}})
	if err != nil {
		t.Fatalf("BatchProcessDocuments: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("BatchProcessDocuments' operation: %v", err)
	}
	meta, err := op.Metadata()
	if err != nil || meta.GetState() != documentaipb.BatchProcessMetadata_SUCCEEDED || len(meta.GetIndividualProcessStatuses()) != 2 ||
		meta.GetStateMessage() != "Processed 2 document(s) successfully" {
		t.Fatalf("metadata: %v %v", meta, err)
	}
	id := op.Name()[strings.LastIndex(op.Name(), "/")+1:]
	for i, st := range meta.GetIndividualProcessStatuses() {
		if st.GetOutputGcsDestination() != fmt.Sprintf("%s/out/%s/%d", gs, id, i) || st.GetStatus().GetCode() != 0 ||
			st.GetHumanReviewStatus().GetState() != documentaipb.HumanReviewStatus_SKIPPED {
			t.Errorf("status %d: %v", i, st)
		}
	}
	var names []string
	it := bh.Objects(ctx, &storage.Query{Prefix: "out/" + id + "/"})
	for {
		a, err := it.Next()
		if err != nil {
			break
		}
		names = append(names, strings.TrimPrefix(a.Name, "out/"+id+"/"))
	}
	if strings.Join(names, ",") != "0/seven-pages-0.json,0/seven-pages-1.json,0/seven-pages-2.json,0/seven-pages-3.json,1/two-pages-0.json" {
		t.Fatalf("the batch wrote %q", names)
	}
	var joined string
	for i := 0; i < 4; i++ {
		var sh documentaipb.Document
		if err := protojson.Unmarshal(read(fmt.Sprintf("out/%s/0/seven-pages-%d.json", id, i)), &sh); err != nil {
			t.Fatal(err)
		}
		want := min(2, 7-2*i)
		si := sh.GetShardInfo()
		if si.GetShardIndex() != int64(i) || si.GetShardCount() != 4 || len(sh.GetPages()) != want ||
			sh.GetPages()[0].GetPageNumber() != int32(2*i+1) || si.GetTextOffset() != int64(len([]rune(joined))) {
			t.Errorf("shard %d: shard_info %v, %d pages", i, si, len(sh.GetPages()))
		}
		if sh.GetMimeType() != "" || sh.GetPages()[0].GetDimension() != nil || sh.GetPages()[0].GetImage() != nil || len(sh.GetPages()[0].GetLines()) == 0 {
			t.Errorf("shard %d kept what the mask leaves out, or lost what it names: %.300v", i, sh.GetPages()[0])
		}
		text := []rune(sh.GetText())
		seg := sh.GetPages()[0].GetLines()[0].GetLayout().GetTextAnchor().GetTextSegments()[0]
		if line := string(text[seg.GetStartIndex():seg.GetEndIndex()]); !strings.Contains(line, fmt.Sprintf("Page %d", 2*i+1)) {
			t.Errorf("shard %d's first line's anchor reads %q", i, line)
		}
		joined += sh.GetText()
	}

	// ProcessDocument over REST: the client sends the mask as
	// "text,pages.pageNumber", and the inline document only its uri.
	resp, err := r.ProcessDocument(ctx, &documentaipb.ProcessRequest{Name: p.GetName(),
		FieldMask: &fieldmaskpb.FieldMask{Paths: []string{"text", "pages.page_number"}},
		Source: &documentaipb.ProcessRequest_InlineDocument{InlineDocument: &documentaipb.Document{
			Source: &documentaipb.Document_Uri{Uri: gs + "/in/two-pages.pdf"}, MimeType: "application/pdf"}}})
	if err != nil {
		t.Fatalf("ProcessDocument with a field mask and a uri: %v", err)
	}
	doc := resp.GetDocument()
	if !strings.Contains(doc.GetText(), "sea turtle") || len(doc.GetPages()) != 2 || doc.GetPages()[1].GetPageNumber() != 2 ||
		doc.GetPages()[0].GetLayout() != nil || resp.GetHumanReviewStatus().GetState() != documentaipb.HumanReviewStatus_SKIPPED {
		t.Errorf("masked document %.300v, review %v", doc, resp.GetHumanReviewStatus())
	}
	if _, err := g.ProcessDocument(ctx, &documentaipb.ProcessRequest{Name: p.GetName(), FieldMask: &fieldmaskpb.FieldMask{Paths: []string{"pages.nope"}},
		Source: &documentaipb.ProcessRequest_InlineDocument{InlineDocument: &documentaipb.Document{
			Source: &documentaipb.Document_Uri{Uri: gs + "/in/two-pages.pdf"}, MimeType: "application/pdf"}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a mask naming no field: %v", err)
	}
}

// TestTerraformDocumentAIProcessorDefaultVersion (#1953): through
// `cloudburrow terraform`, which sets document_ai_custom_endpoint to the
// local AI endpoint, the provider's own sample (a google_document_ai_processor
// and a google_document_ai_processor_default_version naming the stable
// channel, its version under lifecycle.ignore_changes as the sample has it)
// applies; the official client reads the processor and its default, the
// newest stable version; a plan is clean; a second default version, named
// outright, replaces the first; and destroy deletes the processor.
//
// covers: google.cloud.documentai.v1.DocumentProcessorService/CreateProcessor, google.cloud.documentai.v1.DocumentProcessorService/GetProcessor, google.cloud.documentai.v1.DocumentProcessorService/SetDefaultProcessorVersion, google.cloud.documentai.v1.DocumentProcessorService/DeleteProcessor
func TestTerraformDocumentAIProcessorDefaultVersion(t *testing.T) {
	h := New(t)
	g, _ := documentAIClients(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	display := "tf-ocr-" + h.Project()
	module := func(version string) string {
		return fmt.Sprintf(`
resource "google_document_ai_processor" "processor" {
  location     = "us"
  display_name = %q
  type         = "OCR_PROCESSOR"
}

resource "google_document_ai_processor_default_version" "processor" {
  processor = google_document_ai_processor.processor.id
  version   = "${google_document_ai_processor.processor.id}/processorVersions/%s"

  lifecycle {
    ignore_changes = [version]
  }
}
`, display, version)
	}
	find := func() *documentaipb.Processor {
		t.Helper()
		it := g.ListProcessors(ctx, &documentaipb.ListProcessorsRequest{Parent: "projects/" + m.project + "/locations/us"})
		for {
			p, err := it.Next()
			if err != nil {
				return nil
			}
			if p.GetDisplayName() == display {
				return p
			}
		}
	}
	m.write(module("stable"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	p := find()
	if p == nil || p.GetType() != "OCR_PROCESSOR" || !strings.HasSuffix(p.GetDefaultProcessorVersion(), "/processorVersions/pretrained-ocr-v2.1-2024-08-07") {
		t.Fatalf("after apply, the processor is %v", p)
	}
	m.planClean()
	// A version named outright, without ignore_changes, is the one read
	// back, so the plan stays clean.
	m.write(strings.Replace(module("pretrained-ocr-v1.2-2022-11-10"), "ignore_changes = [version]", "ignore_changes = []", 1))
	m.apply()
	if got, err := g.GetProcessor(ctx, &documentaipb.GetProcessorRequest{Name: p.GetName()}); err != nil ||
		!strings.HasSuffix(got.GetDefaultProcessorVersion(), "/pretrained-ocr-v1.2-2022-11-10") {
		t.Errorf("after the second apply, the default is %v (%v)", got.GetDefaultProcessorVersion(), err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := g.GetProcessor(ctx, &documentaipb.GetProcessorRequest{Name: p.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("after destroy, GetProcessor: %v", err)
	}
}
