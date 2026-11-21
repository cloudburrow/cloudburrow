package vertexai

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	_ "golang.org/x/image/bmp"  // Document AI reads BMP
	_ "golang.org/x/image/tiff" // TIFF
	_ "golang.org/x/image/webp" // and WebP

	"cloud.google.com/go/documentai/apiv1/documentaipb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/cmek"
	"github.com/cloudburrow/cloudburrow/internal/embedding"
	"github.com/cloudburrow/cloudburrow/internal/lro"
	"github.com/cloudburrow/cloudburrow/internal/ocr"
)

// Document AI v1's DocumentProcessorService (#1799), on the local AI
// endpoint, gRPC and JSON (the JSON through the REST transcoder), as
// https://docs.cloud.google.com/document-ai/docs/reference/rest/v1/projects.locations.processors
// documents it (read 2026-11-19). It is what BigQuery ML's
// ML.PROCESS_DOCUMENT over a CLOUD_AI_DOCUMENT_V1 remote model calls
// (internal/bigqueryfront/modelservice.go), and what the official
// cloud.google.com/go/documentai client reaches with the endpoint's address.
// Nothing is sent to Google.
//
//   - Processor types: OCR_PROCESSOR (Enterprise Document OCR) and
//     LAYOUT_PARSER_PROCESSOR, in the us and eu locations, are served:
//     FetchProcessorTypes, ListProcessorTypes and GetProcessorType name them.
//     The custom processors are documentaicustom.go's. The Form Parser and
//     the specialized extractors (documentaiprebuilt.go) are served where
//     the instance runs a local generation model, and listed only there;
//     without one their CreateProcessor and ProcessDocument are
//     UNIMPLEMENTED. Every other Google type (the summarizer, ID proofing,
//     the splitters) has no local model: its CreateProcessor is
//     UNIMPLEMENTED, naming it; an unknown type is INVALID_ARGUMENT.
//   - Processors: CreateProcessor (a 16-hex-digit ID, ENABLED, with its
//     type's pretrained default version), GetProcessor, ListProcessors,
//     DeleteProcessor, EnableProcessor and DisableProcessor (operations,
//     done when returned), kept in <state-dir>/documentai/processors.json.
//   - ProcessDocument reads raw_document or inline_document content (or
//     the object its uri names, #1953), or a gcs_document from the
//     instance's Cloud Storage, of the documented
//     types (https://docs.cloud.google.com/document-ai/docs/file-types):
//     PDF (each page rendered by PDFium at 300 DPI), PNG, JPEG, GIF, TIFF
//     (its first page), BMP and WebP. Each page is read with Tesseract
//     (internal/ocr), the LSTM engine Google developed. HTML and Office
//     documents are UNIMPLEMENTED. The documented online limits are
//     enforced (https://docs.cloud.google.com/document-ai/limits, read
//     2026-11-21): 40 MB a file, and each processor type's pages a request
//     online, in imageless_mode and in batch (docAILimits: 15, 30 and 500
//     for the OCR processor; 10 for the Expense Parser; 2 for the US Driver
//     License Parser). BigQuery's ML.PROCESS_DOCUMENT reads up to 130 pages
//     (bigqueryml-syntax-process-document, "can't process documents with
//     more than 130 pages"), at most the processor's batch limit. Pages are
//     rendered one at a time and read by docAIReaders Tesseract instances
//     in parallel, so a long document is never held in memory whole. A
//     disabled processor is
//     FAILED_PRECONDITION. process_options' page_range selects pages;
//     ocr_config.enable_symbol adds symbols; layout_config.chunking_config
//     sets the chunk size; every other option is UNIMPLEMENTED rather
//     than ignored, as is schema_override. field_mask trims the Document
//     (documentaioutput.go, #1953).
//   - OCR_PROCESSOR answers Document.text and, a page each, pageNumber,
//     dimension (pixels), layout, blocks, paragraphs, lines and tokens
//     (each with textAnchor, confidence and boundingPoly, vertices and
//     normalizedVertices), from Tesseract's layout, and the page's image
//     (PNG; none in imageless_mode, #1930). LAYOUT_PARSER_PROCESSOR
//     answers documentLayout (a paragraph textBlock a Tesseract paragraph,
//     with blockId and pageSpan) and chunkedDocument (chunks of whole
//     paragraphs of at most chunk_size words, 500 by default, with
//     sourceBlockIds and pageSpan).
//
// UNVERIFIED (re-read 2026-11-19: the references define the fields, not
// these values): text anchors count Unicode code points (as the client
// samples slice Document.text); the layout parser's block types (Google's
// model also finds headings, lists and tables; here every block is a
// paragraph) and its default chunk size; the default processor version IDs;
// processor type categories; that processEndpoint is left empty (Google's
// names its regional host, which CloudBurrow never dials); the texts of the
// errors.

// docAIType is a served processor type.
type docAIType struct {
	category, version string
}

// docAITypes are the served types.
var docAITypes = map[string]docAIType{
	"OCR_PROCESSOR":           {category: "OCR", version: "pretrained-ocr-v2.0-2023-06-02"},
	"LAYOUT_PARSER_PROCESSOR": {category: "GENERAL", version: "pretrained-layout-parser-v1.0-2024-06-03"},
	// The custom processors (#2106, documentaicustom.go) have no version
	// until one is trained. The Form Parser and specialized extractors are
	// docAIPrebuilt's (documentaiprebuilt.go).
	"CUSTOM_EXTRACTION_PROCESSOR":     {category: "CUSTOM"},
	"CUSTOM_CLASSIFICATION_PROCESSOR": {category: "CUSTOM"},
	"CUSTOM_SPLITTING_PROCESSOR":      {category: "CUSTOM"},
}

// docAIUnserved are Google's other processor types, which have no local
// model.
var docAIUnserved = map[string]bool{
	"SUMMARIZER_PROCESSOR": true, "ID_PROOFING_PROCESSOR": true, "US_PASSPORT_PROCESSOR": true,
	"UTILITY_PROCESSOR": true, "FORM_W9_PROCESSOR": true, "LENDING_DOCUMENT_SPLIT_PROCESSOR": true,
	"PROCUREMENT_DOCUMENT_SPLIT_PROCESSOR": true,
}

// docAILocations are the served locations.
var docAILocations = map[string]bool{"us": true, "eu": true}

// Document AI's online limits.
const (
	docAIMaxBytes          = 40 << 20
	docAIDefaultChunkWords = 500
	// bigQueryMaxPages is ML.PROCESS_DOCUMENT's page limit.
	bigQueryMaxPages = 130
	// docAIReaders is how many pages are read with OCR at once.
	docAIReaders = 4
)

// docAIPageLimits are a processor type's pages a request: online, in
// imageless_mode online, and in batch, as
// https://docs.cloud.google.com/document-ai/limits lists them (read
// 2026-11-21).
type docAIPageLimits struct{ online, imageless, batch int }

var docAILimits = map[string]docAIPageLimits{
	"OCR_PROCESSOR":                   {15, 30, 500},
	"LAYOUT_PARSER_PROCESSOR":         {15, 30, 500},
	"FORM_PARSER_PROCESSOR":           {15, 30, 100},
	"CUSTOM_EXTRACTION_PROCESSOR":     {15, 30, 200},
	"CUSTOM_CLASSIFICATION_PROCESSOR": {15, 30, 200},
	"CUSTOM_SPLITTING_PROCESSOR":      {15, 30, 1000},
	"INVOICE_PROCESSOR":               {15, 30, 200},
	"EXPENSE_PROCESSOR":               {10, 10, 10},
	"BANK_STATEMENT_PROCESSOR":        {15, 30, 30},
	"FORM_W2_PROCESSOR":               {15, 15, 15},
	"PAYSTUB_PROCESSOR":               {15, 30, 50},
	"US_DRIVER_LICENSE_PROCESSOR":     {2, 2, 2},
}

func limitsOf(t string) docAIPageLimits {
	if l, ok := docAILimits[t]; ok {
		return l
	}
	return docAIPageLimits{15, 30, 500}
}

// docAIPageLimit is the pages a request may have, and the larger limit
// imageless_mode would give (0 when it gives none).
type docAIPageLimit struct {
	pages, imagelessPages int
	imageless             bool
}

// processDocumentCaller marks a :process request from BigQuery ML's
// ML.PROCESS_DOCUMENT, which reads up to bigQueryMaxPages pages: the front
// (internal/bigqueryfront/modelprocessdocument.go) sets callerHeader.
type processDocumentCaller struct{}

const (
	callerHeader          = "X-Cloudburrow-Caller"
	callerProcessDocument = "bigquery-ml.process-document"
)

func fromBigQuery(ctx context.Context) bool {
	v, _ := ctx.Value(processDocumentCaller{}).(bool)
	return v
}

// docAIStore is the processors and where they are kept.
type docAIStore struct {
	file       string
	processors map[string]*documentaipb.Processor
	// keys are the data keys of the processors with a customer-managed
	// key (#1950), wrapped by it, kept in processorkeys.json beside file.
	keys map[string]cmek.Sealed
	// custom are the custom processors' datasets and trained versions
	// (#2106), kept in custom.json beside file.
	custom map[string]*customProcessor
}

// keysFile is where the processors' wrapped data keys are kept.
func (d *docAIStore) keysFile() string {
	if d.file == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(d.file), "processorkeys.json")
}

// SetDocumentAIState keeps Document AI's processors in file, loading those
// it already holds.
func (s *Server) SetDocumentAIState(file string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.docai = &docAIStore{file: file, processors: map[string]*documentaipb.Processor{}}
	s.docai.keys = readSealed(s.docai.keysFile())
	s.docai.loadCustom()
	if b, err := os.ReadFile(file); err == nil {
		var p documentaipb.ListProcessorsResponse
		if protojson.Unmarshal(b, &p) == nil {
			for _, x := range p.GetProcessors() {
				s.docai.processors[x.GetName()] = x
			}
		}
	}
}

// docAI returns the store, made in memory when none was set.
func (s *Server) docAI() *docAIStore {
	if s.docai == nil {
		s.docai = &docAIStore{processors: map[string]*documentaipb.Processor{}, keys: map[string]cmek.Sealed{}, custom: map[string]*customProcessor{}}
	}
	if s.ops == nil {
		s.ops = lro.NewStore(nil)
	}
	return s.docai
}

// save writes the processors; the caller holds s.mu.
func (d *docAIStore) save() {
	if d.file == "" {
		return
	}
	p := &documentaipb.ListProcessorsResponse{}
	for _, x := range d.processors {
		p.Processors = append(p.Processors, x)
	}
	sort.Slice(p.Processors, func(i, j int) bool { return p.Processors[i].Name < p.Processors[j].Name })
	b, _ := protojson.Marshal(p)
	_ = os.MkdirAll(filepath.Dir(d.file), 0o755)
	tmp := d.file + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, d.file)
	}
	writeSealed(d.keysFile(), d.keys)
}

// documentAIService implements documentaipb.DocumentProcessorServiceServer.
type documentAIService struct {
	documentaipb.UnimplementedDocumentProcessorServiceServer
	s *Server
}

// docAIParent checks projects/{p}/locations/{l} and its location.
func docAIParent(name string) (string, string, error) {
	p, l, err := parseParent("parent", name)
	if err != nil {
		return "", "", err
	}
	if !docAILocations[l] {
		return "", "", apierror.InvalidArgument("Document AI location %q is not served: CloudBurrow serves us and eu", l)
	}
	return p, l, nil
}

func docAIProcessorType(parent, t string) *documentaipb.ProcessorType {
	dt, ok := docAITypes[t]
	if !ok {
		dt = docAIType{category: docAIPrebuilt[t].category, version: docAIPrebuilt[t].version}
	}
	var locs []*documentaipb.ProcessorType_LocationInfo
	for _, l := range []string{"eu", "us"} {
		locs = append(locs, &documentaipb.ProcessorType_LocationInfo{LocationId: l})
	}
	return &documentaipb.ProcessorType{Name: parent + "/processorTypes/" + t, Type: t, Category: dt.category,
		AvailableLocations: locs, AllowCreation: true}
}

// docAITypeNames are the served types: the Form Parser and specialized
// extractors only where the local generation model runs.
func (s *Server) docAITypeNames() []string {
	var names []string
	for t := range docAITypes {
		names = append(names, t)
	}
	if s.generativeAvailable() {
		for t := range docAIPrebuilt {
			names = append(names, t)
		}
	}
	sort.Strings(names)
	return names
}

func (d *documentAIService) FetchProcessorTypes(_ context.Context, req *documentaipb.FetchProcessorTypesRequest) (*documentaipb.FetchProcessorTypesResponse, error) {
	if _, _, err := docAIParent(req.GetParent()); err != nil {
		return nil, grpcError(err)
	}
	out := &documentaipb.FetchProcessorTypesResponse{}
	for _, t := range d.s.docAITypeNames() {
		out.ProcessorTypes = append(out.ProcessorTypes, docAIProcessorType(req.GetParent(), t))
	}
	return out, nil
}

func (d *documentAIService) ListProcessorTypes(_ context.Context, req *documentaipb.ListProcessorTypesRequest) (*documentaipb.ListProcessorTypesResponse, error) {
	if _, _, err := docAIParent(req.GetParent()); err != nil {
		return nil, grpcError(err)
	}
	var all []*documentaipb.ProcessorType
	for _, t := range d.s.docAITypeNames() {
		all = append(all, docAIProcessorType(req.GetParent(), t))
	}
	page, next, err := docAIPage(all, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, grpcError(err)
	}
	return &documentaipb.ListProcessorTypesResponse{ProcessorTypes: page, NextPageToken: next}, nil
}

func (d *documentAIService) GetProcessorType(_ context.Context, req *documentaipb.GetProcessorTypeRequest) (*documentaipb.ProcessorType, error) {
	p, l, t, err := parseChild("name", "processorTypes", req.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	parent := "projects/" + p + "/locations/" + l
	if _, _, err := docAIParent(parent); err != nil {
		return nil, grpcError(err)
	}
	if _, ok := docAITypes[t]; !ok && (!isPrebuiltType(t) || !d.s.generativeAvailable()) {
		return nil, grpcError(apierror.NotFound("Processor type %s not found", req.GetName()))
	}
	return docAIProcessorType(parent, t), nil
}

// docAIPage pages items by an offset token.
func docAIPage[T any](items []T, size int32, token string) ([]T, string, error) {
	start := 0
	if token != "" {
		n, err := strconv.Atoi(token)
		if err != nil || n < 0 || n > len(items) {
			return nil, "", apierror.InvalidArgument("Invalid page token %q", token)
		}
		start = n
	}
	if size <= 0 || size > 100 {
		size = 100
	}
	end := min(len(items), start+int(size))
	next := ""
	if end < len(items) {
		next = strconv.Itoa(end)
	}
	return items[start:end], next, nil
}

func (d *documentAIService) CreateProcessor(ctx context.Context, req *documentaipb.CreateProcessorRequest) (*documentaipb.Processor, error) {
	if _, _, err := docAIParent(req.GetParent()); err != nil {
		return nil, grpcError(err)
	}
	in := req.GetProcessor()
	t := in.GetType()
	if t == "" {
		return nil, grpcError(apierror.InvalidArgument("processor.type is required"))
	}
	if strings.TrimSpace(in.GetDisplayName()) == "" {
		return nil, grpcError(apierror.InvalidArgument("processor.display_name is required"))
	}
	dt, ok := docAITypes[t]
	if pt, pre := docAIPrebuilt[t]; pre {
		if err := d.s.prebuiltReady(t); err != nil {
			return nil, grpcError(err)
		}
		dt, ok = docAIType{category: pt.category, version: pt.version}, true
	}
	if !ok {
		if docAIUnserved[t] {
			return nil, grpcError(apierror.Unimplemented("Not implemented here: processor type %s has no local model (CloudBurrow serves "+
				"OCR_PROCESSOR, LAYOUT_PARSER_PROCESSOR, the custom processors and, with a local generation model, the Form Parser and "+
				"specialized extractors) and never calls Google (#1799)", t))
		}
		return nil, grpcError(apierror.InvalidArgument("Invalid processor type %q", t))
	}
	id := make([]byte, 8)
	_, _ = rand.Read(id)
	name := req.GetParent() + "/processors/" + hex.EncodeToString(id)
	p := &documentaipb.Processor{Name: name, Type: t, DisplayName: in.GetDisplayName(), State: documentaipb.Processor_ENABLED,
		CreateTime: timestamppb.Now()}
	if dt.version != "" {
		p.DefaultProcessorVersion = name + "/processorVersions/" + dt.version
	}
	var sealed cmek.Sealed
	if key := in.GetKmsKeyName(); key != "" {
		var err error
		if sealed, err = d.s.docAIKey(ctx, req.GetParent(), name, key); err != nil {
			return nil, grpcError(err)
		}
		p.KmsKeyName = key
	}
	d.s.mu.Lock()
	defer d.s.mu.Unlock()
	st := d.s.docAI()
	for _, x := range st.processors {
		if strings.HasPrefix(x.GetName(), req.GetParent()+"/") && x.GetDisplayName() == p.GetDisplayName() {
			return nil, grpcError(apierror.AlreadyExists("A processor with display name %q already exists", p.GetDisplayName()))
		}
	}
	st.processors[name] = p
	if p.GetKmsKeyName() != "" {
		st.keys[name] = sealed
	}
	st.save()
	return cloneProcessor(p), nil
}

func cloneProcessor(p *documentaipb.Processor) *documentaipb.Processor {
	b, _ := protojson.Marshal(p)
	out := &documentaipb.Processor{}
	_ = protojson.Unmarshal(b, out)
	return out
}

// processor looks a processor up by its name, or a processor version's
// (one of its type's served versions, documentaiversions.go); a version's
// lookup answers the processor with DefaultProcessorVersion set to that
// version, the one to read with.
func (s *Server) processor(name string) (*documentaipb.Processor, error) {
	pname, version, hasVersion := strings.Cut(name, "/processorVersions/")
	if _, _, _, err := parseChild("name", "processors", pname); err != nil {
		return nil, err
	}
	s.mu.Lock()
	p, ok := s.docAI().processors[pname]
	if ok {
		p = cloneProcessor(p)
	}
	s.mu.Unlock()
	if !ok {
		return nil, apierror.NotFound("Processor %s not found", pname)
	}
	if hasVersion && isCustomType(p.GetType()) {
		s.mu.Lock()
		v := s.docAI().customOf(pname).version(name)
		s.mu.Unlock()
		if v == nil || strings.Contains(version, "/") {
			return nil, apierror.NotFound("Processor version %s not found", name)
		}
		p.DefaultProcessorVersion = name
	} else if hasVersion {
		id := resolveVersion(p.GetType(), version)
		if id == "" {
			return nil, apierror.NotFound("Processor version %s not found", name)
		}
		p.DefaultProcessorVersion = pname + "/processorVersions/" + id
	}
	return p, nil
}

func (d *documentAIService) GetProcessor(_ context.Context, req *documentaipb.GetProcessorRequest) (*documentaipb.Processor, error) {
	if strings.Contains(req.GetName(), "/processorVersions/") {
		return nil, grpcError(apierror.InvalidArgument("name %q is not a processor", req.GetName()))
	}
	p, err := d.s.processor(req.GetName())
	return p, grpcError(err)
}

func (d *documentAIService) ListProcessors(_ context.Context, req *documentaipb.ListProcessorsRequest) (*documentaipb.ListProcessorsResponse, error) {
	if _, _, err := docAIParent(req.GetParent()); err != nil {
		return nil, grpcError(err)
	}
	d.s.mu.Lock()
	var all []*documentaipb.Processor
	for _, p := range d.s.docAI().processors {
		if strings.HasPrefix(p.GetName(), req.GetParent()+"/processors/") {
			all = append(all, cloneProcessor(p))
		}
	}
	d.s.mu.Unlock()
	sort.Slice(all, func(i, j int) bool { return all[i].GetName() < all[j].GetName() })
	page, next, err := docAIPage(all, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, grpcError(err)
	}
	return &documentaipb.ListProcessorsResponse{Processors: page, NextPageToken: next}, nil
}

// processorOp applies change to a processor and returns its done operation.
func (d *documentAIService) processorOp(name string, change func(st *docAIStore, p *documentaipb.Processor) error,
	meta func(*documentaipb.CommonOperationMetadata) any, resp any) (*longrunningpb.Operation, error) {
	pr, l, _, err := parseChild("name", "processors", name)
	if err != nil {
		return nil, grpcError(err)
	}
	d.s.mu.Lock()
	st := d.s.docAI()
	p, ok := st.processors[name]
	if !ok {
		d.s.mu.Unlock()
		return nil, grpcError(apierror.NotFound("Processor %s not found", name))
	}
	if err := change(st, p); err != nil {
		d.s.mu.Unlock()
		return nil, grpcError(err)
	}
	st.save()
	d.s.mu.Unlock()
	now := timestamppb.Now()
	op := d.s.newOp("projects/" + pr + "/locations/" + l)
	_ = d.s.ops.SetMetadata(op, meta(&documentaipb.CommonOperationMetadata{State: documentaipb.CommonOperationMetadata_SUCCEEDED,
		CreateTime: now, UpdateTime: now, Resource: name}))
	_ = d.s.ops.Succeed(op, resp)
	got, _ := d.s.ops.Get(op)
	out, err := opProto(got)
	return out, grpcError(err)
}

func (d *documentAIService) DeleteProcessor(_ context.Context, req *documentaipb.DeleteProcessorRequest) (*longrunningpb.Operation, error) {
	return d.processorOp(req.GetName(), func(st *docAIStore, p *documentaipb.Processor) error {
		delete(st.processors, p.GetName())
		delete(st.keys, p.GetName())
		if st.custom[p.GetName()] != nil {
			delete(st.custom, p.GetName())
			st.saveCustom()
		}
		return nil
	}, func(c *documentaipb.CommonOperationMetadata) any {
		return &documentaipb.DeleteProcessorMetadata{CommonMetadata: c}
	}, &emptypb.Empty{})
}

func (d *documentAIService) EnableProcessor(_ context.Context, req *documentaipb.EnableProcessorRequest) (*longrunningpb.Operation, error) {
	return d.processorOp(req.GetName(), func(_ *docAIStore, p *documentaipb.Processor) error {
		p.State = documentaipb.Processor_ENABLED
		return nil
	}, func(c *documentaipb.CommonOperationMetadata) any {
		return &documentaipb.EnableProcessorMetadata{CommonMetadata: c}
	}, &documentaipb.EnableProcessorResponse{})
}

func (d *documentAIService) DisableProcessor(_ context.Context, req *documentaipb.DisableProcessorRequest) (*longrunningpb.Operation, error) {
	return d.processorOp(req.GetName(), func(_ *docAIStore, p *documentaipb.Processor) error {
		p.State = documentaipb.Processor_DISABLED
		return nil
	}, func(c *documentaipb.CommonOperationMetadata) any {
		return &documentaipb.DisableProcessorMetadata{CommonMetadata: c}
	}, &documentaipb.DisableProcessorResponse{})
}

func (d *documentAIService) ProcessDocument(ctx context.Context, req *documentaipb.ProcessRequest) (*documentaipb.ProcessResponse, error) {
	doc, err := d.s.processDocument(ctx, req)
	if err != nil {
		return nil, grpcError(err)
	}
	// The local processors have no human review (#1953).
	review := humanReviewSkipped()
	if req.GetSkipHumanReview() {
		review.StateMessage = humanReviewSkippedByRequest
	}
	return &documentaipb.ProcessResponse{Document: doc, HumanReviewStatus: review}, nil
}

// processDocument runs a ProcessRequest.
func (s *Server) processDocument(ctx context.Context, req *documentaipb.ProcessRequest) (*documentaipb.Document, error) {
	p, err := s.processor(req.GetName())
	if err != nil {
		return nil, err
	}
	if p.GetState() != documentaipb.Processor_ENABLED {
		return nil, apierror.FailedPrecondition("Processor %s is not enabled: its state is %s", p.GetName(), p.GetState())
	}
	if err := s.docAIKeyUsable(ctx, p); err != nil {
		return nil, err
	}
	if isCustomType(p.GetType()) {
		if err := s.customReady(p); err != nil {
			return nil, err
		}
	}
	if err := s.prebuiltReady(p.GetType()); err != nil {
		return nil, err
	}
	if req.GetFieldMask() != nil {
		if err := checkDocumentMask(req.GetFieldMask()); err != nil {
			return nil, err
		}
	}
	opts := req.GetProcessOptions()
	if err := checkProcessOptions(p.GetType(), opts); err != nil {
		return nil, err
	}
	var content []byte
	var mimeType string
	switch src := req.GetSource().(type) {
	case *documentaipb.ProcessRequest_RawDocument:
		content, mimeType = src.RawDocument.GetContent(), src.RawDocument.GetMimeType()
	case *documentaipb.ProcessRequest_InlineDocument:
		content, mimeType = src.InlineDocument.GetContent(), src.InlineDocument.GetMimeType()
		if uri := src.InlineDocument.GetUri(); uri != "" && content == nil {
			// Document.uri: "Currently supports Google Cloud Storage URI of
			// the form gs://bucket_name/object_name" (#1953).
			bucket, object, ok := splitGCS(uri, false)
			if !ok {
				return nil, apierror.InvalidArgument("Invalid inline_document.uri %q: it must be gs://bucket_name/object_name", uri)
			}
			s.mu.RLock()
			objs := s.objects
			s.mu.RUnlock()
			if objs == nil {
				return nil, apierror.FailedPrecondition("inline_document.uri needs Cloud Storage, which this instance does not serve")
			}
			var ct string
			if content, ct, err = objs.Read(ctx, bucket, object, docAIMaxBytes+1); err != nil {
				return nil, err
			}
			if mimeType == "" {
				mimeType = ct
			}
		}
	case *documentaipb.ProcessRequest_GcsDocument:
		mimeType = src.GcsDocument.GetMimeType()
		bucket, object, ok := strings.Cut(strings.TrimPrefix(src.GcsDocument.GetGcsUri(), "gs://"), "/")
		if !strings.HasPrefix(src.GcsDocument.GetGcsUri(), "gs://") || !ok || object == "" {
			return nil, apierror.InvalidArgument("Invalid gcs_uri %q", src.GcsDocument.GetGcsUri())
		}
		s.mu.RLock()
		objs := s.objects
		s.mu.RUnlock()
		if objs == nil {
			return nil, apierror.FailedPrecondition("gcs_document needs Cloud Storage, which this instance does not serve")
		}
		if content, _, err = objs.Read(ctx, bucket, object, docAIMaxBytes+1); err != nil {
			return nil, err
		}
	default:
		return nil, apierror.InvalidArgument("A document is required: set raw_document, inline_document or gcs_document")
	}
	if len(content) == 0 {
		return nil, apierror.InvalidArgument("The document content is empty")
	}
	if len(content) > docAIMaxBytes {
		return nil, apierror.InvalidArgument("Document size %d bytes exceeds the limit of %d bytes for online processing", len(content), docAIMaxBytes)
	}
	l := limitsOf(p.GetType())
	limit := docAIPageLimit{pages: l.online}
	if l.imageless > l.online {
		limit.imagelessPages = l.imageless
	}
	images := !req.GetImagelessMode()
	switch {
	case fromBigQuery(ctx):
		// BigQuery's own limit, within the processor's batch limit; the
		// rows' results carry no page images.
		limit, images = docAIPageLimit{pages: min(bigQueryMaxPages, l.batch)}, false
	case req.GetImagelessMode():
		limit = docAIPageLimit{pages: l.imageless, imageless: true}
	}
	doc, err := readDocument(ctx, p, content, mimeType, opts, limit, images)
	if err != nil {
		return nil, err
	}
	if isCustomType(p.GetType()) {
		if err := s.applyCustom(ctx, p, doc); err != nil {
			return nil, err
		}
	}
	if err := s.applyPrebuilt(ctx, p.GetType(), doc); err != nil {
		return nil, err
	}
	maskDocument(doc, req.GetFieldMask())
	return doc, nil
}

// readDocument reads a document's pages, at most limit, with p's type;
// images keeps each page's image in the Document.
func readDocument(ctx context.Context, p *documentaipb.Processor, content []byte, mimeType string, opts *documentaipb.ProcessOptions,
	limit docAIPageLimit, images bool) (*documentaipb.Document, error) {
	layout := p.GetType() == "LAYOUT_PARSER_PROCESSOR"
	pages, err := readPages(ctx, content, mimeType, limit, opts, images && !layout)
	if err != nil {
		return nil, err
	}
	if layout {
		size := docAIDefaultChunkWords
		if cs := opts.GetLayoutConfig().GetChunkingConfig().GetChunkSize(); cs > 0 {
			size = int(cs)
		}
		return layoutDocument(mimeType, pages, size), nil
	}
	return ocrDocument(mimeType, pages, opts.GetOcrConfig().GetEnableSymbol()), nil
}

// docPage is a page read with OCR: its 1-based number, its size in pixels,
// its layout and, when kept, its image as PNG.
type docPage struct {
	n    int
	w, h int
	png  []byte
	read ocr.Result
}

// readPages renders the pages process_options selects, one at a time, and
// reads them with docAIReaders Tesseract instances at once: at most a few
// pages' pixels are held in memory, whatever the document's length.
func readPages(ctx context.Context, content []byte, mimeType string, limit docAIPageLimit, opts *documentaipb.ProcessOptions,
	images bool) ([]docPage, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type job struct {
		at  int
		img image.Image
	}
	jobs := make(chan job)
	var out []docPage
	var mu sync.Mutex
	var firstErr error
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
		cancel()
	}
	var wg sync.WaitGroup
	for range docAIReaders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if ctx.Err() != nil {
					continue
				}
				pg := &out[j.at]
				r, err := ocr.ReadLayout(ctx, j.img, embedding.OCRDPI)
				if err != nil {
					fail(apierror.Internal(err, "reading page %d", pg.n))
					continue
				}
				pg.read, pg.w, pg.h = r, j.img.Bounds().Dx(), j.img.Bounds().Dy()
				if images {
					var b bytes.Buffer
					if pngEncoder.Encode(&b, j.img) == nil {
						pg.png = b.Bytes()
					}
				}
			}
		}()
	}
	at := 0
	err := eachDocPage(content, mimeType, limit, func(count int) ([]int, error) {
		numbers, err := selectPages(count, opts)
		if err != nil {
			return nil, err
		}
		out = make([]docPage, len(numbers))
		idx := make([]int, len(numbers))
		for i, n := range numbers {
			out[i].n, idx[i] = n, n-1
		}
		return idx, nil
	}, func(_ int, img image.Image) error {
		select {
		case jobs <- job{at, img}:
			at++
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

// checkProcessOptions refuses the options that are not served.
func checkProcessOptions(t string, o *documentaipb.ProcessOptions) error {
	if o == nil {
		return nil
	}
	if o.GetSchemaOverride() != nil {
		return apierror.Unimplemented("Not implemented here: process_options.schema_override: %s extracts no schema", t)
	}
	if oc := o.GetOcrConfig(); oc != nil {
		if t != "OCR_PROCESSOR" {
			return apierror.InvalidArgument("process_options.ocr_config is for the OCR processor; this one is %s", t)
		}
		c := &documentaipb.OcrConfig{EnableSymbol: oc.GetEnableSymbol()}
		if b1, _ := protojson.Marshal(c); !bytes.Equal(b1, mustJSON(oc)) {
			return apierror.Unimplemented("Not implemented here: process_options.ocr_config other than enable_symbol " +
				"(hints, native PDF parsing, image quality scores, style info, premium features and advanced options are not served by local Tesseract OCR)")
		}
	}
	if lc := o.GetLayoutConfig(); lc != nil {
		if t != "LAYOUT_PARSER_PROCESSOR" {
			return apierror.InvalidArgument("process_options.layout_config is for the layout parser; this one is %s", t)
		}
		if lc.GetReturnImages() || lc.GetReturnBoundingBoxes() || lc.GetEnableImageAnnotation() || lc.GetEnableTableAnnotation() {
			return apierror.Unimplemented("Not implemented here: layout_config's images, bounding boxes, image and table annotation: " +
				"the local layout parser answers text blocks and chunks alone")
		}
		if cs := lc.GetChunkingConfig().GetChunkSize(); cs < 0 {
			return apierror.InvalidArgument("chunk_size must be positive")
		}
	}
	return nil
}

func mustJSON(m *documentaipb.OcrConfig) []byte {
	b, _ := protojson.Marshal(m)
	return b
}

// selectPages is the 1-based pages process_options.page_range selects.
func selectPages(n int, o *documentaipb.ProcessOptions) ([]int, error) {
	var out []int
	switch pr := o.GetPageRange().(type) {
	case *documentaipb.ProcessOptions_IndividualPageSelector_:
		seen := map[int]bool{}
		for _, p := range pr.IndividualPageSelector.GetPages() {
			if p < 1 || int(p) > n {
				return nil, apierror.InvalidArgument("Invalid page %d: the document has %d pages", p, n)
			}
			if !seen[int(p)] {
				seen[int(p)] = true
				out = append(out, int(p))
			}
		}
		sort.Ints(out)
	case *documentaipb.ProcessOptions_FromStart:
		if pr.FromStart < 1 {
			return nil, apierror.InvalidArgument("from_start must be positive")
		}
		for i := 1; i <= min(n, int(pr.FromStart)); i++ {
			out = append(out, i)
		}
	case *documentaipb.ProcessOptions_FromEnd:
		if pr.FromEnd < 1 {
			return nil, apierror.InvalidArgument("from_end must be positive")
		}
		for i := max(1, n-int(pr.FromEnd)+1); i <= n; i++ {
			out = append(out, i)
		}
	default:
		for i := 1; i <= n; i++ {
			out = append(out, i)
		}
	}
	return out, nil
}

// eachDocPage decodes a document's pages: want is given the page count and
// answers the 0-based pages to decode, which fn receives one at a time.
func eachDocPage(content []byte, mimeType string, limit docAIPageLimit, want func(count int) ([]int, error),
	fn func(index int, img image.Image) error) error {
	mt := strings.ToLower(strings.TrimSpace(strings.Split(mimeType, ";")[0]))
	switch mt {
	case "":
		return apierror.InvalidArgument("mime_type is required")
	case "application/pdf":
		count, err := embedding.EachPDFPageForOCR(content, limit.pages, want, fn)
		if errors.Is(err, embedding.ErrTooManyPages) {
			if limit.imageless {
				return apierror.InvalidArgument("Document pages in imageless mode exceed the limit: %d got %d.", limit.pages, count)
			}
			if limit.imagelessPages <= limit.pages {
				return apierror.InvalidArgument("Document pages exceed the limit: %d got %d.", limit.pages, count)
			}
			return apierror.InvalidArgument("Document pages in non-imageless mode exceed the limit: %d got %d. "+
				"Try using imageless mode to increase the limit to %d.", limit.pages, count, limit.imagelessPages)
		}
		var ae *apierror.Error
		if errors.As(err, &ae) {
			return err // want's refusal
		}
		if err != nil {
			return apierror.InvalidArgument("Unable to process the document: %v", err)
		}
		return nil
	case "image/png", "image/jpeg", "image/gif", "image/tiff", "image/bmp", "image/webp":
		img, _, err := image.Decode(bytes.NewReader(content))
		if err != nil {
			return apierror.InvalidArgument("Unable to process the document: the %s data could not be decoded", mt)
		}
		idx, err := want(1)
		if err != nil {
			return err
		}
		for _, i := range idx {
			if err := fn(i, img); err != nil {
				return err
			}
		}
		return nil
	case "text/html", "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return apierror.Unimplemented("Not implemented here: %s documents: the local processors read PDFs and images", mt)
	}
	return apierror.InvalidArgument("Unsupported input file format: %s", mimeType)
}

// docText builds Document.text, counting code points for text anchors.
type docText struct {
	b strings.Builder
	n int64
}

func (t *docText) add(s string) {
	t.b.WriteString(s)
	t.n += int64(utf8.RuneCountInString(s))
}

func anchor(start, end int64) *documentaipb.Document_TextAnchor {
	return &documentaipb.Document_TextAnchor{TextSegments: []*documentaipb.Document_TextAnchor_TextSegment{{StartIndex: start, EndIndex: end}}}
}

func docPoly(q ocr.Quad, w, h int) *documentaipb.BoundingPoly {
	bp := &documentaipb.BoundingPoly{}
	for _, p := range q {
		bp.Vertices = append(bp.Vertices, &documentaipb.Vertex{X: int32(p.X), Y: int32(p.Y)})
		nv := &documentaipb.NormalizedVertex{}
		if w > 0 && h > 0 {
			nv.X, nv.Y = float32(p.X)/float32(w), float32(p.Y)/float32(h)
		}
		bp.NormalizedVertices = append(bp.NormalizedVertices, nv)
	}
	return bp
}

func docLayout(start, end int64, conf float64, q ocr.Quad, w, h int) *documentaipb.Document_Page_Layout {
	return &documentaipb.Document_Page_Layout{TextAnchor: anchor(start, end), Confidence: float32(conf), BoundingPoly: docPoly(q, w, h),
		Orientation: documentaipb.Document_Page_Layout_PAGE_UP}
}

// wordText is a word's text and whether it ends its line.
func wordText(w ocr.Word) (string, bool) {
	var b strings.Builder
	end := false
	for _, s := range w.Symbols {
		b.WriteString(s.Text)
		end = end || s.LineEnd
	}
	return b.String(), end
}

// ocrDocument is the OCR processor's Document.
func ocrDocument(mimeType string, pages []docPage, symbols bool) *documentaipb.Document {
	doc := &documentaipb.Document{MimeType: mimeType}
	t := &docText{}
	for _, pg := range pages {
		n, r, w, h := pg.n, pg.read, pg.w, pg.h
		page := &documentaipb.Document_Page{PageNumber: int32(n),
			Dimension: &documentaipb.Document_Page_Dimension{Width: float32(w), Height: float32(h), Unit: "pixels"}}
		pageStart := t.n
		for _, blk := range r.Blocks {
			blockStart := t.n
			for _, para := range blk.Paragraphs {
				paraStart, lineStart := t.n, t.n
				var lineBox ocr.Quad
				lineConf, lineWords := 0.0, 0
				for i, word := range para.Words {
					text, eol := wordText(word)
					last := i == len(para.Words)-1
					wordStart := t.n
					t.add(text)
					space := !eol && !last
					if space {
						t.add(" ")
					} else {
						t.add("\n")
					}
					tok := &documentaipb.Document_Page_Token{Layout: docLayout(wordStart, t.n, word.Confidence, word.Box, w, h)}
					if space {
						tok.DetectedBreak = &documentaipb.Document_Page_Token_DetectedBreak{Type: documentaipb.Document_Page_Token_DetectedBreak_SPACE}
					}
					page.Tokens = append(page.Tokens, tok)
					lineBox = unionQuad(lineBox, word.Box, lineWords == 0)
					lineConf += word.Confidence
					lineWords++
					if eol || last {
						page.Lines = append(page.Lines, &documentaipb.Document_Page_Line{
							Layout: docLayout(lineStart, t.n, lineConf/float64(lineWords), lineBox, w, h)})
						lineStart, lineConf, lineWords = t.n, 0, 0
					}
				}
				page.Paragraphs = append(page.Paragraphs, &documentaipb.Document_Page_Paragraph{
					Layout: docLayout(paraStart, t.n, para.Confidence, para.Box, w, h)})
			}
			page.Blocks = append(page.Blocks, &documentaipb.Document_Page_Block{Layout: docLayout(blockStart, t.n, blk.Confidence, blk.Box, w, h)})
		}
		full := ocr.Quad{{0, 0}, {w, 0}, {w, h}, {0, h}}
		page.Layout = docLayout(pageStart, t.n, 1, full, w, h)
		if symbols {
			page.Symbols = docSymbols(r, pageStart, w, h)
		}
		if pg.png != nil {
			// The page's image, as Google's OCR answers it unless
			// imageless_mode is set (#1930).
			page.Image = &documentaipb.Document_Page_Image{Content: pg.png, MimeType: "image/png", Width: int32(w), Height: int32(h)}
		}
		doc.Pages = append(doc.Pages, page)
	}
	doc.Text = t.b.String()
	return doc
}

// docSymbols are a page's symbols, their anchors counted from the page's
// start as ocrDocument lays the text out.
func docSymbols(r ocr.Result, start int64, w, h int) []*documentaipb.Document_Page_Symbol {
	var out []*documentaipb.Document_Page_Symbol
	at := start
	for _, blk := range r.Blocks {
		for _, para := range blk.Paragraphs {
			for _, word := range para.Words {
				for _, sym := range word.Symbols {
					n := int64(utf8.RuneCountInString(sym.Text))
					out = append(out, &documentaipb.Document_Page_Symbol{Layout: docLayout(at, at+n, sym.Confidence, sym.Box, w, h)})
					at += n
				}
				at++ // the space or newline after the word
			}
		}
	}
	return out
}

var pngEncoder = png.Encoder{CompressionLevel: png.BestSpeed}

// unionQuad grows a box by q; first starts it.
func unionQuad(a, q ocr.Quad, first bool) ocr.Quad {
	if first {
		return q
	}
	minX, minY := min(a[0].X, q[0].X), min(a[0].Y, q[0].Y)
	maxX, maxY := max(a[2].X, q[2].X), max(a[2].Y, q[2].Y)
	return ocr.Quad{{minX, minY}, {maxX, minY}, {maxX, maxY}, {minX, maxY}}
}

// layoutDocument is the layout parser's Document: a paragraph block a
// Tesseract paragraph, and chunks of whole paragraphs.
func layoutDocument(mimeType string, pages []docPage, chunkWords int) *documentaipb.Document {
	type para struct {
		id    string
		text  string
		words int
		page  int32
	}
	var paras []para
	layout := &documentaipb.Document_DocumentLayout{}
	for _, pg := range pages {
		n := pg.n
		for _, blk := range pg.read.Blocks {
			for _, p := range blk.Paragraphs {
				var words []string
				for _, w := range p.Words {
					s, _ := wordText(w)
					words = append(words, s)
				}
				if len(words) == 0 {
					continue
				}
				id := strconv.Itoa(len(paras) + 1)
				text := strings.Join(words, " ")
				paras = append(paras, para{id: id, text: text, words: len(words), page: int32(n)})
				layout.Blocks = append(layout.Blocks, &documentaipb.Document_DocumentLayout_DocumentLayoutBlock{BlockId: id,
					PageSpan: &documentaipb.Document_DocumentLayout_DocumentLayoutBlock_LayoutPageSpan{PageStart: int32(n), PageEnd: int32(n)},
					Block: &documentaipb.Document_DocumentLayout_DocumentLayoutBlock_TextBlock{
						TextBlock: &documentaipb.Document_DocumentLayout_DocumentLayoutBlock_LayoutTextBlock{Text: text, Type: "paragraph"}}})
			}
		}
	}
	chunked := &documentaipb.Document_ChunkedDocument{}
	var cur *documentaipb.Document_ChunkedDocument_Chunk
	var texts []string
	words := 0
	flush := func() {
		if cur != nil {
			cur.Content = strings.Join(texts, "\n\n")
			chunked.Chunks = append(chunked.Chunks, cur)
		}
		cur, texts, words = nil, nil, 0
	}
	for _, p := range paras {
		if cur != nil && words+p.words > chunkWords {
			flush()
		}
		if cur == nil {
			cur = &documentaipb.Document_ChunkedDocument_Chunk{ChunkId: "c" + strconv.Itoa(len(chunked.Chunks)+1),
				PageSpan: &documentaipb.Document_ChunkedDocument_Chunk_ChunkPageSpan{PageStart: p.page, PageEnd: p.page}}
		}
		cur.SourceBlockIds = append(cur.SourceBlockIds, p.id)
		cur.PageSpan.PageEnd = p.page
		texts = append(texts, p.text)
		words += p.words
	}
	flush()
	return &documentaipb.Document{MimeType: mimeType, DocumentLayout: layout, ChunkedDocument: chunked}
}

// Document AI's customer-managed keys (#1950), as
// https://docs.cloud.google.com/document-ai/docs/cmek documents them (read
// 2026-11-19): processor.kms_key_name is set at CreateProcessor and never
// changed ("Once a processor has been created, you cannot change its
// encryption settings"); the key is in the processor's location, us-central1
// for the us multi-region and europe-west4 for eu; the Document AI Service
// Agent, service-PROJECT_NUMBER@gcp-sa-prod-dai-core.iam.gserviceaccount.com,
// needs roles/cloudkms.cryptoKeyEncrypterDecrypter on it. "The Processor
// resource: no user data, but a specified CMEK key must be valid": the
// processor gets a data key of its own, wrapped by the key's primary version
// and kept only wrapped (processorkeys.json), which its processor versions
// report as kms_key_name and kms_key_version_name. "API requests that access
// encrypted resources fail if the key is disabled or is unreachable":
// GetProcessorVersion, ListProcessorVersions, ProcessDocument and
// BatchProcessDocuments unwrap it first, and are FAILED_PRECONDITION while
// that version is disabled or destroyed. The served versions are pretrained,
// so no trained material is stored; ProcessDocument saves nothing.

// docAIKeyLocations are the key locations of Document AI's multi-regions.
var docAIKeyLocations = map[string]string{"us": "us-central1", "eu": "europe-west4"}

// docAIKey checks key for a new processor name of parent and wraps the
// processor's data key with it.
func (s *Server) docAIKey(ctx context.Context, parent, name, key string) (cmek.Sealed, error) {
	project, loc, _ := parseParent("parent", parent)
	if want := docAIKeyLocations[loc]; want != "" {
		loc = want
	}
	if err := cmek.CheckName("processor.kms_key_name", key, loc); err != nil {
		return cmek.Sealed{}, cmekError("processor "+name, err)
	}
	agent := cmek.ServiceAgent(project, docAIAgentDomain)
	keys := s.keysOf()
	if err := keys.Usable(ctx, project, agent, key); err != nil {
		return cmek.Sealed{}, cmekError("processor "+name, err)
	}
	sealed, err := keys.Seal(ctx, project, agent, key, nil, []byte(name))
	if err != nil {
		return cmek.Sealed{}, cmekError("processor "+name, err)
	}
	return sealed, nil
}

// docAIKeyUsable unwraps p's data key when it has a customer-managed key:
// a disabled or destroyed version refuses the request.
func (s *Server) docAIKeyUsable(ctx context.Context, p *documentaipb.Processor) error {
	if p.GetKmsKeyName() == "" {
		return nil
	}
	s.mu.Lock()
	sealed, ok := s.docAI().keys[p.GetName()]
	s.mu.Unlock()
	if !ok {
		return apierror.FailedPrecondition("Processor %s has no wrapped data key for its key %s", p.GetName(), p.GetKmsKeyName())
	}
	project, _, _, _ := parseChild("name", "processors", p.GetName())
	_, err := s.openSealed(ctx, project, cmek.ServiceAgent(project, docAIAgentDomain), "processor "+p.GetName(), p.GetName(), sealed)
	return err
}

// withKey gives a version of p its processor's key and key version.
func (s *Server) withKey(v *documentaipb.ProcessorVersion, p *documentaipb.Processor) *documentaipb.ProcessorVersion {
	if p.GetKmsKeyName() == "" {
		return v
	}
	s.mu.Lock()
	sealed := s.docAI().keys[p.GetName()]
	s.mu.Unlock()
	v.KmsKeyName, v.KmsKeyVersionName = p.GetKmsKeyName(), sealed.KeyVersion
	return v
}
