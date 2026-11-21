package vertexai

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	"cloud.google.com/go/documentai/apiv1/documentaipb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Document AI's BatchProcessDocuments (#1930), from and to this instance's
// Cloud Storage, as the processors/batchProcess reference and
// https://docs.cloud.google.com/document-ai/docs/send-request#batch-process
// document it (read 2026-11-19):
//
//   - input_documents is gcs_documents (each gcs_uri with its mime_type) or
//     gcs_prefix (every object under gcs_uri_prefix, its type the object's
//     content type); document_output_config.gcs_output_config.gcs_uri is
//     where each result goes, as Google lays it out:
//     {gcs_uri}/{operation ID}/{input index}/{input file name}-0.json, a
//     Document in JSON.
//   - The limits of https://docs.cloud.google.com/document-ai/limits: 5,000
//     files a request, 1 GB a file, 500 pages a document; process_options
//     as ProcessDocument takes them. A disabled processor is
//     FAILED_PRECONDITION before the operation starts.
//   - The operation runs in the background: its BatchProcessMetadata goes
//     RUNNING, then SUCCEEDED with an individual_process_status a document
//     (its input_gcs_source, status and output_gcs_destination), a
//     document's failure kept in its status; when every document fails, the
//     operation fails with the first one's error.
//   - gcs_output_config.field_mask trims each Document and sharding_config
//     splits it into {file name}-{shard index}.json shards
//     (documentaioutput.go, #1953); skip_human_review is accepted, there
//     being no human review: each document's human_review_status is
//     SKIPPED.
//
// UNVERIFIED (re-read 2026-11-19: the batchProcess reference and the send-request page do not say): that one failed document leaves the operation SUCCEEDED, and
// the texts of the errors.

const (
	docAIBatchFiles    = 5000
	docAIBatchBytes    = 1 << 30
	docAIBatchMaxPages = 500
)

type batchInput struct {
	uri, mimeType string
}

func (d *documentAIService) BatchProcessDocuments(ctx context.Context, req *documentaipb.BatchProcessRequest) (*longrunningpb.Operation, error) {
	op, err := d.s.batchProcess(ctx, req)
	return op, grpcError(err)
}

func (s *Server) batchProcess(ctx context.Context, req *documentaipb.BatchProcessRequest) (*longrunningpb.Operation, error) {
	p, err := s.processor(req.GetName())
	if err != nil {
		return nil, err
	}
	review := humanReviewSkipped()
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
	if req.GetSkipHumanReview() {
		review.StateMessage = humanReviewSkippedByRequest
	}
	if err := checkProcessOptions(p.GetType(), req.GetProcessOptions()); err != nil {
		return nil, err
	}
	out := req.GetDocumentOutputConfig().GetGcsOutputConfig()
	if out == nil || out.GetGcsUri() == "" {
		return nil, apierror.InvalidArgument("document_output_config.gcs_output_config.gcs_uri is required")
	}
	if err := checkDocumentMask(out.GetFieldMask()); err != nil {
		return nil, err
	}
	if err := checkSharding(out.GetShardingConfig()); err != nil {
		return nil, err
	}
	outBucket, outPrefix, ok := splitGCS(out.GetGcsUri(), true)
	if !ok {
		return nil, apierror.InvalidArgument("Invalid gcs_uri %q in gcs_output_config", out.GetGcsUri())
	}
	objs, w, err := s.storageRW("Batch processing")
	if err != nil {
		return nil, err
	}
	var inputs []batchInput
	switch {
	case req.GetInputDocuments().GetGcsDocuments() != nil:
		for _, g := range req.GetInputDocuments().GetGcsDocuments().GetDocuments() {
			if _, _, ok := splitGCS(g.GetGcsUri(), false); !ok {
				return nil, apierror.InvalidArgument("Invalid gcs_uri %q", g.GetGcsUri())
			}
			if g.GetMimeType() == "" {
				return nil, apierror.InvalidArgument("mime_type is required for %s", g.GetGcsUri())
			}
			inputs = append(inputs, batchInput{g.GetGcsUri(), g.GetMimeType()})
		}
	case req.GetInputDocuments().GetGcsPrefix() != nil:
		bucket, prefix, ok := splitGCS(req.GetInputDocuments().GetGcsPrefix().GetGcsUriPrefix(), true)
		if !ok {
			return nil, apierror.InvalidArgument("Invalid gcs_uri_prefix %q", req.GetInputDocuments().GetGcsPrefix().GetGcsUriPrefix())
		}
		lister, ok := objs.(ObjectLister)
		if !ok {
			return nil, apierror.FailedPrecondition("gcs_prefix needs Cloud Storage listing, which this instance does not serve")
		}
		names, err := lister.List(ctx, bucket, prefix)
		if err != nil {
			return nil, err
		}
		for _, n := range names {
			if !strings.HasSuffix(n, "/") {
				inputs = append(inputs, batchInput{uri: "gs://" + bucket + "/" + n})
			}
		}
	default:
		return nil, apierror.InvalidArgument("input_documents is required: set gcs_documents or gcs_prefix")
	}
	if len(inputs) == 0 {
		return nil, apierror.InvalidArgument("input_documents names no documents")
	}
	if len(inputs) > docAIBatchFiles {
		return nil, apierror.InvalidArgument("Files per batch processing request exceed the limit: %d got %d", docAIBatchFiles, len(inputs))
	}
	pr, l, _, _ := parseChild("name", "processors", strings.SplitN(p.GetName(), "/processorVersions/", 2)[0])
	opName := s.newOp("projects/" + pr + "/locations/" + l)
	opID := path.Base(opName)
	created := timestamppb.Now()
	meta := &documentaipb.BatchProcessMetadata{State: documentaipb.BatchProcessMetadata_RUNNING, CreateTime: created, UpdateTime: created}
	_ = s.ops.SetMetadata(opName, meta)
	dst := batchOutput{bucket: outBucket, prefix: strings.TrimSuffix(outPrefix, "/"), mask: out.GetFieldMask(),
		sharding: out.GetShardingConfig(), review: review}
	go s.runBatch(opName, opID, p, req.GetProcessOptions(), inputs, objs, w, dst, created)
	got, _ := s.ops.Get(opName)
	return opProto(got)
}

// batchOutput is where and how a batch writes its Documents (#1953).
type batchOutput struct {
	bucket, prefix string
	mask           *fieldmaskpb.FieldMask
	sharding       *documentaipb.DocumentOutputConfig_GcsOutputConfig_ShardingConfig
	review         *documentaipb.HumanReviewStatus
}

// runBatch processes each input and writes its Document.
func (s *Server) runBatch(opName, opID string, p *documentaipb.Processor, opts *documentaipb.ProcessOptions, inputs []batchInput,
	objs Objects, w ObjectWriter, out batchOutput, created *timestamppb.Timestamp) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()
	var statuses []*documentaipb.BatchProcessMetadata_IndividualProcessStatus
	var firstErr error
	ok := 0
	for i, in := range inputs {
		dest := fmt.Sprintf("%s/%d", opID, i)
		if out.prefix != "" {
			dest = out.prefix + "/" + dest
		}
		st := &documentaipb.BatchProcessMetadata_IndividualProcessStatus{InputGcsSource: in.uri,
			OutputGcsDestination: "gs://" + out.bucket + "/" + dest, HumanReviewStatus: out.review}
		err := s.batchOne(ctx, p, opts, in, objs, w, out, dest)
		if err != nil {
			e := apierror.From(err)
			st.Status = &status.Status{Code: int32(e.Code), Message: e.Message}
			st.OutputGcsDestination = ""
			if firstErr == nil {
				firstErr = err
			}
		} else {
			st.Status = &status.Status{}
			ok++
		}
		statuses = append(statuses, st)
		_ = s.ops.SetMetadata(opName, &documentaipb.BatchProcessMetadata{State: documentaipb.BatchProcessMetadata_RUNNING,
			CreateTime: created, UpdateTime: timestamppb.Now(), IndividualProcessStatuses: statuses})
	}
	// As the send-request page's example reads: "Processed 1 document(s)
	// successfully".
	meta := &documentaipb.BatchProcessMetadata{State: documentaipb.BatchProcessMetadata_SUCCEEDED, StateMessage: fmt.Sprintf("Processed %d document(s) successfully", ok),
		CreateTime: created, UpdateTime: timestamppb.Now(), IndividualProcessStatuses: statuses}
	if ok == 0 {
		meta.State, meta.StateMessage = documentaipb.BatchProcessMetadata_FAILED, apierror.From(firstErr).Message
		_ = s.ops.SetMetadata(opName, meta)
		_ = s.ops.Fail(opName, firstErr)
		return
	}
	_ = s.ops.SetMetadata(opName, meta)
	_ = s.ops.Succeed(opName, &documentaipb.BatchProcessResponse{})
}

// batchOne processes one input into dest/{name}-{shard}.json.
func (s *Server) batchOne(ctx context.Context, p *documentaipb.Processor, opts *documentaipb.ProcessOptions, in batchInput,
	objs Objects, w ObjectWriter, out batchOutput, dest string) error {
	bucket, object, _ := splitGCS(in.uri, false)
	content, ct, err := objs.Read(ctx, bucket, object, docAIBatchBytes+1)
	if err != nil {
		return err
	}
	if len(content) > docAIBatchBytes {
		return apierror.InvalidArgument("Document size %d bytes exceeds the limit of %d bytes for batch processing", len(content), docAIBatchBytes)
	}
	if len(content) == 0 {
		return apierror.InvalidArgument("The document %s is empty", in.uri)
	}
	mt := in.mimeType
	if mt == "" {
		mt = ct
	}
	doc, err := readDocument(ctx, p, content, mt, opts, docAIBatchMaxPages, false)
	if err != nil {
		return err
	}
	if isCustomType(p.GetType()) {
		if err := s.applyCustom(ctx, p, doc); err != nil {
			return err
		}
	}
	base := path.Base(object)
	if i := strings.LastIndex(base, "."); i > 0 {
		base = base[:i]
	}
	for i, sh := range shardDocument(doc, out.sharding) {
		maskDocument(sh, out.mask)
		b, err := protojson.Marshal(sh)
		if err != nil {
			return apierror.Internal(err, "encoding the document")
		}
		if err := w.Write(ctx, out.bucket, fmt.Sprintf("%s/%s-%d.json", dest, base, i), "application/json", b); err != nil {
			return err
		}
	}
	return nil
}
