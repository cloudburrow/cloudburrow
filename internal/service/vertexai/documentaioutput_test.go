package vertexai

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/documentai/apiv1/documentaipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// TestDocumentAIFieldMaskAndURI (#1953): field_mask keeps the Document and
// page fields it names; a bad path is INVALID_ARGUMENT; inline_document.uri
// reads the object; the answer's human review is SKIPPED.
func TestDocumentAIFieldMaskAndURI(t *testing.T) {
	ctx := context.Background()
	scan, err := os.ReadFile("../../ocr/testdata/scan.png")
	if err != nil {
		t.Fatal(err)
	}
	d := newDocAI(t, "")
	d.s.UseStorage(&fakeBucket{objs: map[string][]byte{"in/scan.png": scan}, ct: map[string]string{"in/scan.png": "image/png"}})
	p := createProcessor(t, d, "OCR_PROCESSOR", "ocr")
	src := &documentaipb.ProcessRequest_InlineDocument{InlineDocument: &documentaipb.Document{
		Source: &documentaipb.Document_Uri{Uri: "gs://in/scan.png"}}}
	r, err := d.ProcessDocument(ctx, &documentaipb.ProcessRequest{Name: p.GetName(), Source: src, SkipHumanReview: true,
		FieldMask: &fieldmaskpb.FieldMask{Paths: []string{"text", "pages.page_number", "pages.dimension"}}})
	if err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}
	doc := r.GetDocument()
	if !strings.Contains(doc.GetText(), "INVOICE") || doc.GetMimeType() != "" || len(doc.GetPages()) != 1 {
		t.Fatalf("masked document: %.300v", doc)
	}
	pg := doc.Pages[0]
	if pg.GetPageNumber() != 1 || pg.GetDimension() == nil || pg.GetLayout() != nil || len(pg.GetLines()) != 0 || pg.GetImage() != nil {
		t.Errorf("masked page: %.300v", pg)
	}
	if r.GetHumanReviewStatus().GetState() != documentaipb.HumanReviewStatus_SKIPPED {
		t.Errorf("human review: %v", r.GetHumanReviewStatus())
	}
	for _, path := range []string{"nope", "pages.nope", "entities.type", "pages.layout.text_anchor"} {
		if _, err := d.ProcessDocument(ctx, &documentaipb.ProcessRequest{Name: p.GetName(), Source: src,
			FieldMask: &fieldmaskpb.FieldMask{Paths: []string{path}}}); code(err) != codes.InvalidArgument {
			t.Errorf("mask %q: %v", path, err)
		}
	}
	if _, err := d.ProcessDocument(ctx, &documentaipb.ProcessRequest{Name: p.GetName(), Source: &documentaipb.ProcessRequest_InlineDocument{
		InlineDocument: &documentaipb.Document{Source: &documentaipb.Document_Uri{Uri: "gs://in/missing.png"}}}}); code(err) != codes.NotFound {
		t.Errorf("a missing uri: %v", err)
	}
	if _, err := d.ProcessDocument(ctx, &documentaipb.ProcessRequest{Name: p.GetName(), Source: &documentaipb.ProcessRequest_InlineDocument{
		InlineDocument: &documentaipb.Document{Source: &documentaipb.Document_Uri{Uri: "https://example.com/a.png"}}}}); code(err) != codes.InvalidArgument {
		t.Errorf("a uri that is not gs://: %v", err)
	}
}

// TestDocumentAIBatchShards (#1953): sharding_config splits the seven-page
// PDF into shards of three pages overlapping by one, each with its
// shard_info and the text its pages anchor; field_mask trims each shard;
// skip_human_review is accepted.
func TestDocumentAIBatchShards(t *testing.T) {
	ctx := context.Background()
	pdf, err := os.ReadFile("../../embedding/testdata/seven-pages.pdf")
	if err != nil {
		t.Fatal(err)
	}
	d := newDocAI(t, "")
	fb := &fakeBucket{objs: map[string][]byte{"in/seven.pdf": pdf}, ct: map[string]string{"in/seven.pdf": "application/pdf"}}
	d.s.UseStorage(fb)
	p := createProcessor(t, d, "OCR_PROCESSOR", "ocr")
	in := &documentaipb.BatchDocumentsInputConfig{Source: &documentaipb.BatchDocumentsInputConfig_GcsDocuments{
		GcsDocuments: &documentaipb.GcsDocuments{Documents: []*documentaipb.GcsDocument{{GcsUri: "gs://in/seven.pdf", MimeType: "application/pdf"}}}}}
	gcs := &documentaipb.DocumentOutputConfig_GcsOutputConfig{GcsUri: "gs://out/r",
		ShardingConfig: &documentaipb.DocumentOutputConfig_GcsOutputConfig_ShardingConfig{PagesPerShard: 3, PagesOverlap: 1}}
	out := &documentaipb.DocumentOutputConfig{Destination: &documentaipb.DocumentOutputConfig_GcsOutputConfig_{GcsOutputConfig: gcs}}
	for _, bad := range []*documentaipb.DocumentOutputConfig_GcsOutputConfig_ShardingConfig{{}, {PagesPerShard: 2, PagesOverlap: 2}} {
		g := &documentaipb.DocumentOutputConfig_GcsOutputConfig{GcsUri: "gs://out/r", ShardingConfig: bad}
		if _, err := d.BatchProcessDocuments(ctx, &documentaipb.BatchProcessRequest{Name: p.GetName(), InputDocuments: in,
			DocumentOutputConfig: &documentaipb.DocumentOutputConfig{Destination: &documentaipb.DocumentOutputConfig_GcsOutputConfig_{GcsOutputConfig: g}}}); code(err) != codes.InvalidArgument {
			t.Errorf("sharding %v: %v", bad, err)
		}
	}
	whole, err := d.ProcessDocument(ctx, &documentaipb.ProcessRequest{Name: p.GetName(), Source: &documentaipb.ProcessRequest_RawDocument{
		RawDocument: &documentaipb.RawDocument{Content: pdf, MimeType: "application/pdf"}}})
	if err != nil {
		t.Fatal(err)
	}
	full := []rune(whole.GetDocument().GetText())
	op, err := d.BatchProcessDocuments(ctx, &documentaipb.BatchProcessRequest{Name: p.GetName() + "/processorVersions/pretrained-ocr-v1.2-2022-11-10",
		InputDocuments: in, DocumentOutputConfig: out, SkipHumanReview: true})
	if err != nil {
		t.Fatal(err)
	}
	meta := waitDocBatch(t, d, op.GetName())
	if meta.GetState() != documentaipb.BatchProcessMetadata_SUCCEEDED || meta.GetStateMessage() != "Processed 1 document(s) successfully" ||
		meta.IndividualProcessStatuses[0].GetHumanReviewStatus().GetState() != documentaipb.HumanReviewStatus_SKIPPED {
		t.Fatalf("metadata: %v", meta)
	}
	id := op.GetName()[strings.LastIndex(op.GetName(), "/")+1:]
	wantPages := [][]int32{{1, 2, 3, 4}, {4, 5, 6, 7}, {7}}
	for i, want := range wantPages {
		b, _, err := fb.Read(ctx, "out", "r/"+id+"/0/seven-"+string(rune('0'+i))+".json", 0)
		if err != nil {
			t.Fatalf("shard %d: %v", i, err)
		}
		var sh documentaipb.Document
		if err := protojson.Unmarshal(b, &sh); err != nil {
			t.Fatal(err)
		}
		var got []int32
		for _, pg := range sh.GetPages() {
			got = append(got, pg.GetPageNumber())
		}
		si := sh.GetShardInfo()
		if si.GetShardIndex() != int64(i) || si.GetShardCount() != 3 || len(got) != len(want) || got[0] != want[0] || got[len(got)-1] != want[len(want)-1] {
			t.Errorf("shard %d: pages %v, shard_info %v", i, got, si)
		}
		off := si.GetTextOffset()
		text := []rune(sh.GetText())
		if string(full[off:off+int64(len(text))]) != sh.GetText() || !strings.Contains(sh.GetText(), "Page "+string(rune('0'+want[0]))) {
			t.Errorf("shard %d text %q at %d", i, sh.GetText(), off)
		}
		seg := sh.Pages[0].GetLines()[0].GetLayout().GetTextAnchor().GetTextSegments()[0]
		if line := string(text[seg.GetStartIndex():seg.GetEndIndex()]); !strings.HasPrefix(line, "Page") {
			t.Errorf("shard %d's first line's anchor reads %q", i, line)
		}
	}
	if _, _, err := fb.Read(ctx, "out", "r/"+id+"/0/seven-3.json", 0); err == nil {
		t.Error("a fourth shard was written")
	}

	gcs.ShardingConfig = nil
	gcs.FieldMask = &fieldmaskpb.FieldMask{Paths: []string{"text"}}
	op, err = d.BatchProcessDocuments(ctx, &documentaipb.BatchProcessRequest{Name: p.GetName(), InputDocuments: in, DocumentOutputConfig: out})
	if err != nil {
		t.Fatal(err)
	}
	waitDocBatch(t, d, op.GetName())
	id = op.GetName()[strings.LastIndex(op.GetName(), "/")+1:]
	b, _, err := fb.Read(ctx, "out", "r/"+id+"/0/seven-0.json", 0)
	var doc documentaipb.Document
	if err != nil || protojson.Unmarshal(b, &doc) != nil || doc.GetText() != whole.GetDocument().GetText() || len(doc.GetPages()) != 0 || doc.GetShardInfo() != nil {
		t.Errorf("masked batch output: %v %.200s", err, b)
	}
	gcs.FieldMask = &fieldmaskpb.FieldMask{Paths: []string{"pages.nope"}}
	if _, err := d.BatchProcessDocuments(ctx, &documentaipb.BatchProcessRequest{Name: p.GetName(), InputDocuments: in, DocumentOutputConfig: out}); code(err) != codes.InvalidArgument {
		t.Errorf("a bad batch mask: %v", err)
	}
}

func waitDocBatch(t *testing.T, d *documentAIService, name string) *documentaipb.BatchProcessMetadata {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		got, _ := d.s.ops.Get(name)
		if got.Done() {
			return got.Metadata.(*documentaipb.BatchProcessMetadata)
		}
		if time.Now().After(deadline) {
			t.Fatal("batch did not finish")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestDocumentAIShardLayout: the layout parser's blocks and chunks go to the
// shard of the page they start on.
func TestDocumentAIShardLayout(t *testing.T) {
	span := func(n int32) *documentaipb.Document_DocumentLayout_DocumentLayoutBlock_LayoutPageSpan {
		return &documentaipb.Document_DocumentLayout_DocumentLayoutBlock_LayoutPageSpan{PageStart: n, PageEnd: n}
	}
	doc := &documentaipb.Document{DocumentLayout: &documentaipb.Document_DocumentLayout{}, ChunkedDocument: &documentaipb.Document_ChunkedDocument{}}
	for n := int32(1); n <= 5; n++ {
		doc.DocumentLayout.Blocks = append(doc.DocumentLayout.Blocks, &documentaipb.Document_DocumentLayout_DocumentLayoutBlock{PageSpan: span(n)})
		doc.ChunkedDocument.Chunks = append(doc.ChunkedDocument.Chunks, &documentaipb.Document_ChunkedDocument_Chunk{
			PageSpan: &documentaipb.Document_ChunkedDocument_Chunk_ChunkPageSpan{PageStart: n, PageEnd: n}})
	}
	shards := shardDocument(doc, &documentaipb.DocumentOutputConfig_GcsOutputConfig_ShardingConfig{PagesPerShard: 2})
	if len(shards) != 3 {
		t.Fatalf("%d shards", len(shards))
	}
	for i, want := range []int{2, 2, 1} {
		sh := shards[i]
		if len(sh.GetDocumentLayout().GetBlocks()) != want || len(sh.GetChunkedDocument().GetChunks()) != want ||
			sh.GetShardInfo().GetShardIndex() != int64(i) || sh.GetShardInfo().GetShardCount() != 3 {
			t.Errorf("shard %d: %v", i, sh)
		}
	}
	if len(doc.DocumentLayout.Blocks) != 5 {
		t.Error("sharding changed the document")
	}
}

// TestDocumentAIEvaluationsAndReview (#1953): no evaluation can be run or
// found for a served version; ReviewDocument is refused.
func TestDocumentAIEvaluationsAndReview(t *testing.T) {
	ctx := context.Background()
	d := newDocAI(t, "")
	p := createProcessor(t, d, "LAYOUT_PARSER_PROCESSOR", "lp")
	v := p.GetDefaultProcessorVersion()
	if _, err := d.EvaluateProcessorVersion(ctx, &documentaipb.EvaluateProcessorVersionRequest{ProcessorVersion: v}); code(err) != codes.InvalidArgument {
		t.Errorf("EvaluateProcessorVersion: %v", err)
	}
	if _, err := d.EvaluateProcessorVersion(ctx, &documentaipb.EvaluateProcessorVersionRequest{ProcessorVersion: p.GetName() + "/processorVersions/nope"}); code(err) != codes.NotFound {
		t.Errorf("EvaluateProcessorVersion of an unknown version: %v", err)
	}
	if l, err := d.ListEvaluations(ctx, &documentaipb.ListEvaluationsRequest{Parent: v}); err != nil || len(l.GetEvaluations()) != 0 {
		t.Errorf("ListEvaluations: %v %v", l, err)
	}
	if _, err := d.ListEvaluations(ctx, &documentaipb.ListEvaluationsRequest{Parent: p.GetName()}); code(err) != codes.InvalidArgument {
		t.Errorf("ListEvaluations of a processor: %v", err)
	}
	if _, err := d.GetEvaluation(ctx, &documentaipb.GetEvaluationRequest{Name: v + "/evaluations/e1"}); code(err) != codes.NotFound {
		t.Errorf("GetEvaluation: %v", err)
	}
	if _, err := d.ReviewDocument(ctx, &documentaipb.ReviewDocumentRequest{HumanReviewConfig: p.GetName() + "/humanReviewConfig"}); code(err) != codes.Unimplemented {
		t.Errorf("ReviewDocument: %v", err)
	}
}
