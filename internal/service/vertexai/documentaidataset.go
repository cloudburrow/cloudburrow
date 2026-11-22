package vertexai

import (
	"context"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"

	"cloud.google.com/go/documentai/apiv1/documentaipb"
	beta "cloud.google.com/go/documentai/apiv1beta3/documentaipb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Document AI's datasets (#2106): v1beta3's DocumentService, as its
// reference documents it
// (https://docs.cloud.google.com/document-ai/docs/reference/rpc/google.cloud.documentai.v1beta3#documentservice,
// read 2026-11-21), on the custom processors (documentaicustom.go):
//
//   - A processor's dataset is {processor}/dataset, UNINITIALIZED until
//     UpdateDataset gives it storage: gcs_managed_config (each imported
//     document is written there as a Document in JSON, {gcs_prefix}/{id}.json)
//     or unmanaged_dataset_config (kept in CloudBurrow's state alone).
//     document_warehouse_config is UNIMPLEMENTED: Document AI Warehouse is
//     another product, not served.
//   - ImportDocuments reads batch_input_config's objects from the
//     instance's Cloud Storage: a Document in JSON (application/json, or a
//     .json name) is imported as it is, labeled when it has entities (the
//     labeling import); any other document is read with the OCR processor's
//     local Tesseract, unlabeled. dataset_split puts the batch in a split;
//     auto_split_config's training_split_ratio splits it, the first
//     documents (by name) training. document_type (a classifier's or
//     splitter's) labels each document of the batch with one entity of the
//     type, which the schema must have.
//   - GetDocument (read_mask as ProcessDocument's field_mask; page_range
//     keeps pages start to end, 1-based and inclusive), ListDocuments (the
//     filter keys SplitType, LabelingState, DisplayName with * wildcards,
//     EntityType and TagName, joined by AND, = or !=; skip,
//     return_total_size), BatchDeleteDocuments (individual_document_ids or
//     filter), GetDatasetSchema and UpdateDatasetSchema (the schema
//     TrainProcessorVersion uses when its request has none).
//
// UNVERIFIED (re-read 2026-11-21: the reference defines the fields, not
// these values): the managed documents' object names, page_range's
// numbering, the codes and texts of the errors, and that an import, a
// delete and an update are done when returned.

func betaToV1[T proto.Message](in proto.Message, out T) T {
	b, _ := protojson.Marshal(in)
	_ = loose.Unmarshal(b, out)
	return out
}

// documentService implements beta.DocumentServiceServer.
type documentService struct {
	beta.UnimplementedDocumentServiceServer
	s *Server
}

// datasetProcessor is the custom processor a dataset name is of.
func (d *documentService) datasetProcessor(name, suffix string) (*documentaipb.Processor, error) {
	pname, ok := strings.CutSuffix(name, suffix)
	if !ok {
		return nil, apierror.InvalidArgument("Invalid dataset name %q: it is projects/*/locations/*/processors/*%s", name, suffix)
	}
	p, err := d.s.processor(pname)
	if err != nil {
		return nil, err
	}
	if strings.Contains(pname, "/processorVersions/") {
		return nil, apierror.InvalidArgument("Invalid dataset name %q", name)
	}
	if !isCustomType(p.GetType()) {
		return nil, apierror.FailedPrecondition("Processor type %s has no dataset: datasets are the custom extractor's, classifier's and splitter's", p.GetType())
	}
	return p, nil
}

func (d *documentService) betaOp(p *documentaipb.Processor, resource string, meta func(*beta.CommonOperationMetadata) proto.Message, resp proto.Message) (*longrunningpb.Operation, error) {
	s := d.s
	pr, l, _, _ := parseChild("name", "processors", p.GetName())
	opName := s.newOp("projects/" + pr + "/locations/" + l)
	now := timestamppb.Now()
	_ = s.ops.SetMetadata(opName, meta(&beta.CommonOperationMetadata{State: beta.CommonOperationMetadata_SUCCEEDED,
		CreateTime: now, UpdateTime: now, Resource: resource}))
	_ = s.ops.Succeed(opName, resp)
	got, _ := s.ops.Get(opName)
	return opProto(got)
}

func datasetProto(p string, c *customProcessor) *beta.Dataset {
	ds := &beta.Dataset{Name: p + "/dataset", State: beta.Dataset_State(beta.Dataset_State_value[c.DatasetState])}
	switch {
	case c.GcsPrefix != "":
		ds.StorageSource = &beta.Dataset_GcsManagedConfig{GcsManagedConfig: &beta.Dataset_GCSManagedConfig{
			GcsPrefix: &beta.GcsPrefix{GcsUriPrefix: c.GcsPrefix}}}
	case c.Unmanaged:
		ds.StorageSource = &beta.Dataset_UnmanagedDatasetConfig_{UnmanagedDatasetConfig: &beta.Dataset_UnmanagedDatasetConfig{}}
	}
	return ds
}

func (d *documentService) UpdateDataset(_ context.Context, req *beta.UpdateDatasetRequest) (*longrunningpb.Operation, error) {
	op, err := d.updateDataset(req)
	return op, grpcError(err)
}

func (d *documentService) updateDataset(req *beta.UpdateDatasetRequest) (*longrunningpb.Operation, error) {
	in := req.GetDataset()
	p, err := d.datasetProcessor(in.GetName(), "/dataset")
	if err != nil {
		return nil, err
	}
	switch {
	case in.GetDocumentWarehouseConfig() != nil:
		return nil, apierror.Unimplemented("Not implemented here: document_warehouse_config: " +
			"Document AI Warehouse is not served; use gcs_managed_config or unmanaged_dataset_config")
	case in.GetGcsManagedConfig() != nil:
		if _, _, ok := splitGCS(in.GetGcsManagedConfig().GetGcsPrefix().GetGcsUriPrefix(), true); !ok {
			return nil, apierror.InvalidArgument("Invalid gcs_managed_config.gcs_prefix %q", in.GetGcsManagedConfig().GetGcsPrefix().GetGcsUriPrefix())
		}
	case in.GetUnmanagedDatasetConfig() != nil:
	default:
		return nil, apierror.InvalidArgument("dataset.storage_source is required: set gcs_managed_config or unmanaged_dataset_config")
	}
	d.s.mu.Lock()
	c := d.s.docAI().customOf(p.GetName())
	if in.GetGcsManagedConfig() != nil {
		c.GcsPrefix, c.Unmanaged = strings.TrimSuffix(in.GetGcsManagedConfig().GetGcsPrefix().GetGcsUriPrefix(), "/"), false
	} else {
		c.GcsPrefix, c.Unmanaged = "", true
	}
	c.DatasetState = "INITIALIZED"
	ds := datasetProto(p.GetName(), c)
	d.s.docAI().saveCustom()
	d.s.mu.Unlock()
	return d.betaOp(p, ds.GetName(), func(cm *beta.CommonOperationMetadata) proto.Message {
		return &beta.UpdateDatasetOperationMetadata{CommonMetadata: cm}
	}, ds)
}

func (d *documentService) ImportDocuments(ctx context.Context, req *beta.ImportDocumentsRequest) (*longrunningpb.Operation, error) {
	op, err := d.importDocuments(ctx, req)
	return op, grpcError(err)
}

// labelAs gives doc one entity of type t, covering its pages for a
// splitter.
func labelAs(doc *documentaipb.Document, t, kind string) {
	e := &documentaipb.Document_Entity{Type: t, Confidence: 1}
	if kind == "splitter" {
		e.PageAnchor = &documentaipb.Document_PageAnchor{}
		n := max(len(doc.GetPages()), 1)
		for i := 0; i < n; i++ {
			e.PageAnchor.PageRefs = append(e.PageAnchor.PageRefs, &documentaipb.Document_PageAnchor_PageRef{Page: int64(i)})
		}
	}
	doc.Entities = []*documentaipb.Document_Entity{e}
}

func (d *documentService) importDocuments(ctx context.Context, req *beta.ImportDocumentsRequest) (*longrunningpb.Operation, error) {
	p, err := d.datasetProcessor(req.GetDataset(), "/dataset")
	if err != nil {
		return nil, err
	}
	kind := docAICustomTypes[p.GetType()]
	d.s.mu.Lock()
	c := d.s.docAI().customOf(p.GetName())
	state, prefix := c.DatasetState, c.GcsPrefix
	var classes []string
	if c.Schema != nil {
		classes = classifierTypes(c.Schema)
	}
	d.s.mu.Unlock()
	if state != "INITIALIZED" {
		return nil, apierror.FailedPrecondition("Dataset %s is not initialized: give it storage with UpdateDataset first", req.GetDataset())
	}
	if len(req.GetBatchDocumentsImportConfigs()) == 0 {
		return nil, apierror.InvalidArgument("batch_documents_import_configs is required")
	}
	var w ObjectWriter
	if prefix != "" {
		if _, w, err = d.s.storageRW("A dataset in Cloud Storage"); err != nil {
			return nil, err
		}
	}
	var statuses []*beta.ImportDocumentsMetadata_IndividualImportStatus
	var added []*datasetDoc
	ocrProc := &documentaipb.Processor{Type: "OCR_PROCESSOR"}
	for _, cfg := range req.GetBatchDocumentsImportConfigs() {
		if t := cfg.GetDocumentType(); t != "" {
			if kind == "extractor" {
				return nil, apierror.InvalidArgument("document_type can only be used with a classifier or splitter processor")
			}
			if !slices.Contains(classes, t) {
				return nil, apierror.InvalidArgument("document_type %q is not a document type of the dataset's schema", t)
			}
		}
		inputs, objs, err := d.s.listInputs(ctx, betaToV1(cfg.GetBatchInputConfig(), &documentaipb.BatchDocumentsInputConfig{}))
		if err != nil {
			return nil, err
		}
		sort.Slice(inputs, func(i, j int) bool { return inputs[i].uri < inputs[j].uri })
		split := cfg.GetDatasetSplit().String()
		trainN := len(inputs)
		if a := cfg.GetAutoSplitConfig(); a != nil {
			r := a.GetTrainingSplitRatio()
			if r < 0 || r > 1 {
				return nil, apierror.InvalidArgument("training_split_ratio %v is not between 0 and 1", r)
			}
			trainN = int(float64(len(inputs))*float64(r) + 0.5)
		}
		for i, in := range inputs {
			st := &beta.ImportDocumentsMetadata_IndividualImportStatus{InputGcsSource: in.uri}
			statuses = append(statuses, st)
			bucket, object, _ := splitGCS(in.uri, false)
			content, ct, err := objs.Read(ctx, bucket, object, docAIBatchBytes+1)
			mt := in.mimeType
			if mt == "" {
				mt = ct
			}
			var doc *documentaipb.Document
			if err == nil {
				if strings.HasPrefix(mt, "application/json") || strings.HasSuffix(object, ".json") {
					doc = &documentaipb.Document{}
					if uerr := loose.Unmarshal(content, doc); uerr != nil {
						err = apierror.InvalidArgument("%s is not a Document in JSON: %v", in.uri, uerr)
					}
				} else {
					// The page images are kept, for the labeling tool's
					// document viewer (#2124).
					doc, err = readDocument(ctx, ocrProc, content, mt, nil, docAIBatchMaxPages, false)
				}
			}
			if err != nil {
				e := apierror.From(err)
				st.Status = &status.Status{Code: int32(e.Code), Message: e.Message}
				continue
			}
			if t := cfg.GetDocumentType(); t != "" {
				labelAs(doc, t, kind)
			}
			dd := &datasetDoc{ID: newHexID(), DisplayName: path.Base(object), Doc: doc, Split: split}
			if cfg.GetAutoSplitConfig() != nil {
				dd.Split = "DATASET_SPLIT_TEST"
				if i < trainN {
					dd.Split = "DATASET_SPLIT_TRAIN"
				}
			}
			if prefix != "" {
				b, _ := protojson.Marshal(doc)
				pb, po, _ := splitGCS(prefix+"/", true)
				obj := strings.TrimPrefix(po+dd.ID+".json", "/")
				if err := w.Write(ctx, pb, obj, "application/json", b); err != nil {
					e := apierror.From(err)
					st.Status = &status.Status{Code: int32(e.Code), Message: e.Message}
					continue
				}
				dd.GcsURI = "gs://" + pb + "/" + obj
			}
			st.Status = &status.Status{}
			st.OutputDocumentId = docID(dd)
			added = append(added, dd)
		}
	}
	d.s.mu.Lock()
	c = d.s.docAI().customOf(p.GetName())
	c.Docs = append(c.Docs, added...)
	d.s.docAI().saveCustom()
	d.s.mu.Unlock()
	return d.betaOp(p, req.GetDataset(), func(cm *beta.CommonOperationMetadata) proto.Message {
		return &beta.ImportDocumentsMetadata{CommonMetadata: cm, IndividualImportStatuses: statuses, TotalDocumentCount: int32(len(statuses))}
	}, &beta.ImportDocumentsResponse{})
}

func docID(dd *datasetDoc) *beta.DocumentId {
	return &beta.DocumentId{Type: &beta.DocumentId_GcsManagedDocId{GcsManagedDocId: &beta.DocumentId_GCSManagedDocumentId{
		GcsUri: dd.GcsURI, CwDocId: dd.ID}}}
}

// findDoc is the dataset document an ID names, by cw_doc_id or gcs_uri.
func findDoc(c *customProcessor, id *beta.DocumentId) (int, error) {
	g := id.GetGcsManagedDocId()
	if g == nil {
		return -1, apierror.InvalidArgument("document_id.gcs_managed_doc_id is required")
	}
	for i, dd := range c.Docs {
		if (g.GetCwDocId() != "" && dd.ID == g.GetCwDocId()) || (g.GetGcsUri() != "" && dd.GcsURI == g.GetGcsUri()) {
			return i, nil
		}
	}
	return -1, apierror.NotFound("Document %s%s not found", g.GetCwDocId(), g.GetGcsUri())
}

func (d *documentService) GetDocument(_ context.Context, req *beta.GetDocumentRequest) (*beta.GetDocumentResponse, error) {
	p, err := d.datasetProcessor(req.GetDataset(), "/dataset")
	if err != nil {
		return nil, grpcError(err)
	}
	if err := checkDocumentMask(req.GetReadMask()); err != nil {
		return nil, grpcError(err)
	}
	d.s.mu.Lock()
	c := d.s.docAI().customOf(p.GetName())
	i, err := findDoc(c, req.GetDocumentId())
	var doc *documentaipb.Document
	if err == nil {
		doc = proto.Clone(c.Docs[i].Doc).(*documentaipb.Document)
	}
	d.s.mu.Unlock()
	if err != nil {
		return nil, grpcError(err)
	}
	if r := req.GetPageRange(); r != nil {
		if r.GetStart() < 1 || r.GetEnd() < r.GetStart() {
			return nil, grpcError(apierror.InvalidArgument("Invalid page_range %d to %d: pages are numbered from 1", r.GetStart(), r.GetEnd()))
		}
		var keep []*documentaipb.Document_Page
		for i, pg := range doc.GetPages() {
			if n := int32(i + 1); n >= r.GetStart() && n <= r.GetEnd() {
				keep = append(keep, pg)
			}
		}
		doc.Pages = keep
	}
	maskDocument(doc, req.GetReadMask())
	return &beta.GetDocumentResponse{Document: betaToV1(doc, &beta.Document{})}, nil
}

// docFilter is a parsed ListDocuments filter.
type docFilter []struct {
	key, value string
	not        bool
}

var filterTerm = regexp.MustCompile(`^\s*(SplitType|LabelingState|DisplayName|EntityType|TagName)\s*(!=|=)\s*"?([^"]*?)"?\s*$`)

func parseDocFilter(f string) (docFilter, error) {
	var out docFilter
	if strings.TrimSpace(f) == "" {
		return nil, nil
	}
	seen := map[string]bool{}
	for _, term := range regexp.MustCompile(`\s+AND\s+`).Split(f, -1) {
		m := filterTerm.FindStringSubmatch(term)
		if m == nil {
			return nil, apierror.InvalidArgument("Invalid filter term %q: the keys are SplitType, LabelingState, DisplayName, EntityType and TagName, with = or !=, joined by AND", term)
		}
		if seen[m[1]] {
			return nil, apierror.InvalidArgument("Duplicate filter key %s", m[1])
		}
		seen[m[1]] = true
		out = append(out, struct {
			key, value string
			not        bool
		}{m[1], m[3], m[2] == "!="})
	}
	return out, nil
}

func labelingState(dd *datasetDoc) string {
	if dd.labeled() {
		return "DOCUMENT_LABELED"
	}
	return "DOCUMENT_UNLABELED"
}

func (f docFilter) match(dd *datasetDoc) bool {
	for _, t := range f {
		var ok bool
		switch t.key {
		case "SplitType":
			ok = dd.Split == t.value
		case "LabelingState":
			ok = labelingState(dd) == t.value
		case "DisplayName":
			re := "^" + strings.ReplaceAll(regexp.QuoteMeta(t.value), `\*`, ".*") + "$"
			ok = regexp.MustCompile(re).MatchString(dd.DisplayName)
		case "EntityType":
			for _, e := range dd.Doc.GetEntities() {
				ok = ok || e.GetType() == t.value
			}
		case "TagName":
			ok = false // CloudBurrow sets no tags.
		}
		if ok == t.not {
			return false
		}
	}
	return true
}

func (d *documentService) ListDocuments(_ context.Context, req *beta.ListDocumentsRequest) (*beta.ListDocumentsResponse, error) {
	p, err := d.datasetProcessor(req.GetDataset(), "/dataset")
	if err != nil {
		return nil, grpcError(err)
	}
	f, err := parseDocFilter(req.GetFilter())
	if err != nil {
		return nil, grpcError(err)
	}
	if req.GetSkip() < 0 || req.GetPageSize() < 0 {
		return nil, grpcError(apierror.InvalidArgument("page_size and skip must not be negative"))
	}
	d.s.mu.Lock()
	var all []*beta.DocumentMetadata
	for _, dd := range d.s.docAI().customOf(p.GetName()).Docs {
		if !f.match(dd) {
			continue
		}
		all = append(all, &beta.DocumentMetadata{DocumentId: docID(dd), PageCount: int32(len(dd.Doc.GetPages())),
			DatasetType: beta.DatasetSplitType(beta.DatasetSplitType_value[dd.Split]), DisplayName: dd.DisplayName,
			LabelingState: beta.DocumentLabelingState(beta.DocumentLabelingState_value[labelingState(dd)])})
	}
	d.s.mu.Unlock()
	total := len(all)
	if s := int(req.GetSkip()); s > 0 && req.GetPageToken() == "" {
		all = all[min(s, len(all)):]
	}
	page, next, err := docAIPage(all, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, grpcError(err)
	}
	out := &beta.ListDocumentsResponse{DocumentMetadata: page, NextPageToken: next}
	if req.GetReturnTotalSize() {
		out.TotalSize = int32(total)
	}
	return out, nil
}

func (d *documentService) BatchDeleteDocuments(_ context.Context, req *beta.BatchDeleteDocumentsRequest) (*longrunningpb.Operation, error) {
	p, err := d.datasetProcessor(req.GetDataset(), "/dataset")
	if err != nil {
		return nil, grpcError(err)
	}
	var statuses []*beta.BatchDeleteDocumentsMetadata_IndividualBatchDeleteStatus
	errs := 0
	d.s.mu.Lock()
	c := d.s.docAI().customOf(p.GetName())
	switch src := req.GetDatasetDocuments(); {
	case src.GetIndividualDocumentIds() != nil:
		for _, id := range src.GetIndividualDocumentIds().GetDocumentIds() {
			st := &beta.BatchDeleteDocumentsMetadata_IndividualBatchDeleteStatus{DocumentId: id, Status: &status.Status{}}
			if i, err := findDoc(c, id); err != nil {
				e := apierror.From(err)
				st.Status = &status.Status{Code: int32(e.Code), Message: e.Message}
				errs++
			} else {
				c.Docs = append(c.Docs[:i], c.Docs[i+1:]...)
			}
			statuses = append(statuses, st)
		}
	case src.GetFilter() != "":
		f, err := parseDocFilter(src.GetFilter())
		if err != nil {
			d.s.mu.Unlock()
			return nil, grpcError(err)
		}
		var keep []*datasetDoc
		for _, dd := range c.Docs {
			if f.match(dd) {
				statuses = append(statuses, &beta.BatchDeleteDocumentsMetadata_IndividualBatchDeleteStatus{DocumentId: docID(dd), Status: &status.Status{}})
				continue
			}
			keep = append(keep, dd)
		}
		c.Docs = keep
	default:
		d.s.mu.Unlock()
		return nil, grpcError(apierror.InvalidArgument("dataset_documents is required: set individual_document_ids or filter"))
	}
	d.s.docAI().saveCustom()
	d.s.mu.Unlock()
	op, err := d.betaOp(p, req.GetDataset(), func(cm *beta.CommonOperationMetadata) proto.Message {
		return &beta.BatchDeleteDocumentsMetadata{CommonMetadata: cm, IndividualBatchDeleteStatuses: statuses,
			TotalDocumentCount: int32(len(statuses)), ErrorDocumentCount: int32(errs)}
	}, &beta.BatchDeleteDocumentsResponse{})
	return op, grpcError(err)
}

func (d *documentService) GetDatasetSchema(_ context.Context, req *beta.GetDatasetSchemaRequest) (*beta.DatasetSchema, error) {
	p, err := d.datasetProcessor(req.GetName(), "/dataset/datasetSchema")
	if err != nil {
		return nil, grpcError(err)
	}
	d.s.mu.Lock()
	defer d.s.mu.Unlock()
	out := &beta.DatasetSchema{Name: req.GetName()}
	if sc := d.s.docAI().customOf(p.GetName()).Schema; sc != nil {
		out.DocumentSchema = betaToV1(sc, &beta.DocumentSchema{})
	}
	return out, nil
}

func (d *documentService) UpdateDatasetSchema(_ context.Context, req *beta.UpdateDatasetSchemaRequest) (*beta.DatasetSchema, error) {
	in := req.GetDatasetSchema()
	p, err := d.datasetProcessor(in.GetName(), "/dataset/datasetSchema")
	if err != nil {
		return nil, grpcError(err)
	}
	for _, path := range req.GetUpdateMask().GetPaths() {
		if path != "document_schema" && path != "documentSchema" {
			return nil, grpcError(apierror.InvalidArgument("Invalid update_mask path %q: the dataset schema's document_schema is updated", path))
		}
	}
	sc := betaToV1(in.GetDocumentSchema(), &documentaipb.DocumentSchema{})
	names := map[string]bool{}
	for _, et := range sc.GetEntityTypes() {
		if et.GetName() == "" {
			return nil, grpcError(apierror.InvalidArgument("Every entity type needs a name"))
		}
		if names[et.GetName()] {
			return nil, grpcError(apierror.InvalidArgument("Duplicate entity type %q", et.GetName()))
		}
		names[et.GetName()] = true
	}
	d.s.mu.Lock()
	c := d.s.docAI().customOf(p.GetName())
	c.Schema = sc
	d.s.docAI().saveCustom()
	d.s.mu.Unlock()
	return &beta.DatasetSchema{Name: in.GetName(), DocumentSchema: betaToV1(sc, &beta.DocumentSchema{})}, nil
}

// LabelDatasetDocument is the console's labeling (#2124): it replaces a
// dataset document's entities and, when split is set, its split
// (DATASET_SPLIT_TRAIN, DATASET_SPLIT_TEST or DATASET_SPLIT_UNASSIGNED).
// Google's labeling tool saves through a method the public DocumentService
// does not have (the roles page's documentai.datasets.updateDocuments,
// https://docs.cloud.google.com/iam/docs/roles-permissions/clouddocumentai,
// read 2026-11-21), so the console, which runs in this process, calls this
// instead of inventing a route. A managed document's JSON in Cloud Storage
// is rewritten.
func (s *Server) LabelDatasetDocument(ctx context.Context, processor, id string, entities []*documentaipb.Document_Entity, split string) error {
	switch split {
	case "", "DATASET_SPLIT_TRAIN", "DATASET_SPLIT_TEST", "DATASET_SPLIT_UNASSIGNED":
	default:
		return apierror.InvalidArgument("unknown split %q", split)
	}
	s.mu.Lock()
	c := s.docAI().customOf(processor)
	i, err := findDoc(c, &beta.DocumentId{Type: &beta.DocumentId_GcsManagedDocId{GcsManagedDocId: &beta.DocumentId_GCSManagedDocumentId{CwDocId: id}}})
	if err != nil {
		s.mu.Unlock()
		return err
	}
	dd := c.Docs[i]
	if entities != nil {
		dd.Doc = proto.Clone(dd.Doc).(*documentaipb.Document)
		dd.Doc.Entities = entities
	}
	if split != "" {
		dd.Split = split
	}
	doc, uri := proto.Clone(dd.Doc).(*documentaipb.Document), dd.GcsURI
	s.docAI().saveCustom()
	s.mu.Unlock()
	if uri == "" || entities == nil {
		return nil
	}
	_, w, err := s.storageRW("A dataset in Cloud Storage")
	if err != nil {
		return err
	}
	b, _ := protojson.Marshal(doc)
	bucket, object, _ := splitGCS(uri, false)
	return w.Write(ctx, bucket, object, "application/json", b)
}
