package vertexai

import (
	"context"
	"strings"

	"cloud.google.com/go/documentai/apiv1/documentaipb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// What Document AI answers around a processed Document (#1953), as Google's
// references document it (read 2026-11-19):
//
//   - field_mask, on ProcessRequest and on a batch's gcs_output_config:
//     "Specifies which fields to include in the Document output", "a
//     comma-separated list of fully qualified names of fields", such as
//     text,entities,pages.pageNumber
//     (https://docs.cloud.google.com/document-ai/docs/send-request). A path
//     names a Document field, or a field of each of its pages
//     (pages.{field}); every other field is left out. An unknown field is
//     INVALID_ARGUMENT.
//   - sharding_config (pages_per_shard, pages_overlap: "The number of
//     overlapping pages between consecutive shards") splits a batch's
//     Document into shards, each written as {file name}-{shard index}.json
//     with its shard_info: "The 0-based index of this shard", "Total number
//     of shards" and text_offset, "The index of the first character in text
//     in the overall document global text". A shard holds its pages, the
//     text they anchor (its anchors made relative to it), and, for the
//     layout parser, the blocks and chunks that start on its pages.
//   - Human review: the local processors have none, so every document's
//     human_review_status is SKIPPED, which the HumanReviewStatus reference
//     gives for "human review isn't enabled on the processor";
//     skip_human_review (deprecated) asks for what already happens.
//     ReviewDocument, which sends a document to Google's reviewers, is
//     UNIMPLEMENTED.
//   - Evaluations measure a version's entities against a labeled dataset
//     (https://docs.cloud.google.com/document-ai/docs/evaluate). Neither
//     Enterprise Document OCR nor the Layout Parser extracts entities, and
//     no dataset is served: EvaluateProcessorVersion is INVALID_ARGUMENT, a
//     version's ListEvaluations is empty and GetEvaluation NOT_FOUND.
//
// UNVERIFIED (re-read 2026-11-19: the pages cited, which do not say): which
// pages a shard holds when pages_overlap is set (the next shard's first
// pages_overlap pages are appended to each), whether Google trims
// shard_info with the field mask (it is kept here), the state message of a
// SKIPPED review, and the codes and texts of the refusals.

// humanReviewSkipped is the review status of every local document.
func humanReviewSkipped() *documentaipb.HumanReviewStatus {
	return &documentaipb.HumanReviewStatus{State: documentaipb.HumanReviewStatus_SKIPPED,
		StateMessage: "Human review is not enabled on this processor."}
}

// checkDocumentMask refuses a field mask path that names no field of a
// Document or, after "pages.", of a Page.
func checkDocumentMask(m *fieldmaskpb.FieldMask) error {
	doc := (&documentaipb.Document{}).ProtoReflect().Descriptor().Fields()
	page := (&documentaipb.Document_Page{}).ProtoReflect().Descriptor().Fields()
	for _, p := range m.GetPaths() {
		top, sub, nested := strings.Cut(p, ".")
		switch {
		case doc.ByName(protoreflect.Name(top)) == nil:
			return apierror.InvalidArgument("Invalid field mask path %q: Document has no field %q", p, top)
		case nested && top != "pages":
			return apierror.InvalidArgument("Invalid field mask path %q: only Document fields and pages.{field} are supported", p)
		case nested && (strings.Contains(sub, ".") || page.ByName(protoreflect.Name(sub)) == nil):
			return apierror.InvalidArgument("Invalid field mask path %q: Document.Page has no field %q", p, sub)
		}
	}
	return nil
}

// maskDocument keeps only the fields m names; a nil or empty mask keeps
// everything. Its paths are already checked.
func maskDocument(doc *documentaipb.Document, m *fieldmaskpb.FieldMask) {
	if doc == nil || len(m.GetPaths()) == 0 {
		return
	}
	keep, pageKeep := map[string]bool{}, map[string]bool{}
	wholePages := false
	for _, p := range m.GetPaths() {
		top, sub, nested := strings.Cut(p, ".")
		keep[top] = true
		if nested {
			pageKeep[sub] = true
		} else if top == "pages" {
			wholePages = true
		}
	}
	shard := doc.GetShardInfo()
	clearExcept(doc.ProtoReflect(), keep)
	if keep["pages"] && !wholePages {
		for _, pg := range doc.GetPages() {
			clearExcept(pg.ProtoReflect(), pageKeep)
		}
	}
	if shard != nil {
		doc.ShardInfo = shard
	}
}

// clearExcept clears every set field of m not named in keep.
func clearExcept(m protoreflect.Message, keep map[string]bool) {
	var drop []protoreflect.FieldDescriptor
	m.Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		if !keep[string(fd.Name())] {
			drop = append(drop, fd)
		}
		return true
	})
	for _, fd := range drop {
		m.Clear(fd)
	}
}

// checkSharding refuses a sharding config that cannot shard.
func checkSharding(c *documentaipb.DocumentOutputConfig_GcsOutputConfig_ShardingConfig) error {
	if c == nil {
		return nil
	}
	if c.GetPagesPerShard() <= 0 {
		return apierror.InvalidArgument("sharding_config.pages_per_shard must be positive, got %d", c.GetPagesPerShard())
	}
	if c.GetPagesOverlap() < 0 || c.GetPagesOverlap() >= c.GetPagesPerShard() {
		return apierror.InvalidArgument("sharding_config.pages_overlap must be at least 0 and less than pages_per_shard (%d), got %d",
			c.GetPagesPerShard(), c.GetPagesOverlap())
	}
	return nil
}

// shardDocument splits doc as c says; with no config it is the one
// document, without shard_info.
func shardDocument(doc *documentaipb.Document, c *documentaipb.DocumentOutputConfig_GcsOutputConfig_ShardingConfig) []*documentaipb.Document {
	if c == nil {
		return []*documentaipb.Document{doc}
	}
	per, overlap := int(c.GetPagesPerShard()), int(c.GetPagesOverlap())
	if len(doc.GetPages()) > 0 {
		return shardByPages(doc, per, overlap)
	}
	return shardLayout(doc, per, overlap)
}

// shardByPages shards an OCR Document: shard i holds pages
// [i*per, (i+1)*per+overlap) and the text they anchor.
func shardByPages(doc *documentaipb.Document, per, overlap int) []*documentaipb.Document {
	n := len(doc.GetPages())
	count := (n + per - 1) / per
	text := []rune(doc.GetText())
	var out []*documentaipb.Document
	for i := 0; i < count; i++ {
		lo, hi := i*per, min(n, (i+1)*per+overlap)
		sh := proto.Clone(doc).(*documentaipb.Document)
		sh.Pages = sh.Pages[lo:hi]
		start, end := int64(-1), int64(0)
		for _, pg := range sh.Pages {
			for _, seg := range pg.GetLayout().GetTextAnchor().GetTextSegments() {
				if start < 0 || seg.GetStartIndex() < start {
					start = seg.GetStartIndex()
				}
				end = max(end, seg.GetEndIndex())
			}
		}
		if start < 0 {
			start, end = 0, 0
		}
		start, end = min(start, int64(len(text))), min(end, int64(len(text)))
		sh.Text = string(text[start:end])
		for _, pg := range sh.Pages {
			shiftAnchors(pg.ProtoReflect(), start)
		}
		sh.ShardInfo = &documentaipb.Document_ShardInfo{ShardIndex: int64(i), ShardCount: int64(count), TextOffset: start}
		out = append(out, sh)
	}
	return out
}

// shardLayout shards a layout parser's Document by page number: shard i
// holds the blocks and chunks that start on pages
// [i*per+1, (i+1)*per+overlap].
func shardLayout(doc *documentaipb.Document, per, overlap int) []*documentaipb.Document {
	last := int32(1)
	for _, b := range doc.GetDocumentLayout().GetBlocks() {
		last = max(last, b.GetPageSpan().GetPageEnd())
	}
	for _, ch := range doc.GetChunkedDocument().GetChunks() {
		last = max(last, ch.GetPageSpan().GetPageEnd())
	}
	count := (int(last) + per - 1) / per
	var out []*documentaipb.Document
	for i := 0; i < count; i++ {
		lo, hi := int32(i*per+1), int32((i+1)*per+overlap)
		in := func(s *documentaipb.Document_DocumentLayout_DocumentLayoutBlock_LayoutPageSpan) bool {
			return s.GetPageStart() >= lo && s.GetPageStart() <= hi
		}
		sh := proto.Clone(doc).(*documentaipb.Document)
		if l := sh.GetDocumentLayout(); l != nil {
			var keep []*documentaipb.Document_DocumentLayout_DocumentLayoutBlock
			for _, b := range l.Blocks {
				if in(b.GetPageSpan()) {
					keep = append(keep, b)
				}
			}
			l.Blocks = keep
		}
		if cd := sh.GetChunkedDocument(); cd != nil {
			var keep []*documentaipb.Document_ChunkedDocument_Chunk
			for _, ch := range cd.Chunks {
				ps := ch.GetPageSpan()
				if ps.GetPageStart() >= lo && ps.GetPageStart() <= hi {
					keep = append(keep, ch)
				}
			}
			cd.Chunks = keep
		}
		sh.ShardInfo = &documentaipb.Document_ShardInfo{ShardIndex: int64(i), ShardCount: int64(count)}
		out = append(out, sh)
	}
	return out
}

// shiftAnchors moves every TextAnchor under m back by off code points.
func shiftAnchors(m protoreflect.Message, off int64) {
	if a, ok := m.Interface().(*documentaipb.Document_TextAnchor); ok {
		for _, s := range a.GetTextSegments() {
			s.StartIndex = max(0, s.GetStartIndex()-off)
			s.EndIndex = max(0, s.GetEndIndex()-off)
		}
		return
	}
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.Kind() != protoreflect.MessageKind && fd.Kind() != protoreflect.GroupKind {
			return true
		}
		switch {
		case fd.IsList():
			l := v.List()
			for i := 0; i < l.Len(); i++ {
				shiftAnchors(l.Get(i).Message(), off)
			}
		case fd.IsMap():
		default:
			shiftAnchors(v.Message(), off)
		}
		return true
	})
}

// --- evaluations and human review --------------------------------

// noEntitiesRefusal is why a served type cannot be evaluated.
func noEntitiesRefusal(p *documentaipb.Processor, what string) error {
	return apierror.InvalidArgument("Processor type %s does not support %s: %s is for custom extractors, classifiers and splitters, "+
		"whose versions are trained on a labeled dataset", p.GetType(), what, what)
}

func (d *documentAIService) EvaluateProcessorVersion(_ context.Context, req *documentaipb.EvaluateProcessorVersionRequest) (*longrunningpb.Operation, error) {
	p, _, err := d.s.processorVersion(req.GetProcessorVersion())
	if err != nil {
		return nil, grpcError(err)
	}
	return nil, grpcError(noEntitiesRefusal(p, "evaluation"))
}

// evaluationParent is the version an evaluation name or list parent is of.
func (d *documentAIService) evaluationParent(name string, isList bool) (string, error) {
	version, id, has := strings.Cut(name, "/evaluations/")
	if isList == has || (has && (id == "" || strings.Contains(id, "/"))) {
		return "", apierror.InvalidArgument("Invalid evaluation name %q", name)
	}
	if _, _, err := d.s.processorVersion(version); err != nil {
		return "", err
	}
	return version, nil
}

func (d *documentAIService) ListEvaluations(_ context.Context, req *documentaipb.ListEvaluationsRequest) (*documentaipb.ListEvaluationsResponse, error) {
	if _, err := d.evaluationParent(req.GetParent(), true); err != nil {
		return nil, grpcError(err)
	}
	if req.GetPageSize() < 0 {
		return nil, grpcError(apierror.InvalidArgument("page_size must not be negative"))
	}
	return &documentaipb.ListEvaluationsResponse{}, nil
}

func (d *documentAIService) GetEvaluation(_ context.Context, req *documentaipb.GetEvaluationRequest) (*documentaipb.Evaluation, error) {
	if _, err := d.evaluationParent(req.GetName(), false); err != nil {
		return nil, grpcError(err)
	}
	return nil, grpcError(apierror.NotFound("Evaluation %s not found", req.GetName()))
}

// ReviewDocument is refused whatever it names, as the coverage registry
// classes a method no request reaches.
func (d *documentAIService) ReviewDocument(context.Context, *documentaipb.ReviewDocumentRequest) (*longrunningpb.Operation, error) {
	return nil, grpcError(apierror.Unimplemented("Not implemented here: ReviewDocument: human review sends documents to " +
		"Google's reviewer workforce, which nothing local replaces; every document's human_review_status is SKIPPED"))
}

// humanReviewSkippedByRequest is a SKIPPED review's message when the
// request set skip_human_review.
const humanReviewSkippedByRequest = "Human review was skipped by the request."
