package vertexai

import (
	"context"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/documentai/apiv1/documentaipb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Document AI's processor versions (#1930), as
// https://docs.cloud.google.com/document-ai/docs/manage-processor-versions
// and the processors.processorVersions REST reference document them (read
// 2026-11-19):
//
//   - Each served type has Google's pretrained (Google-managed) versions, the
//     stable ones the page lists: Enterprise Document OCR's
//     pretrained-ocr-v1.2-2022-11-10, pretrained-ocr-v2.0-2023-06-02 and
//     pretrained-ocr-v2.1-2024-08-07, and the Layout Parser's
//     pretrained-layout-parser-v1.0-2024-06-03. Every one is read with the same
//     local Tesseract engine: CloudBurrow has no copy of Google's models, so
//     the versions differ in name alone. "stable" names the newest stable
//     version, as the page describes the channel; "rc" is NOT_FOUND, the page
//     listing no release candidate for either type.
//   - GetProcessorVersion and ListProcessorVersions answer them, DEPLOYED and
//     google_managed. SetDefaultProcessorVersion (an operation) makes one the
//     processor's default, which ProcessDocument uses when a request names
//     the processor.
//   - "You cannot change the deployment status for pretrained processor
//     versions or the current default version" and "You cannot delete
//     pretrained processor versions or the current default version":
//     DeployProcessorVersion, UndeployProcessorVersion and
//     DeleteProcessorVersion of one are FAILED_PRECONDITION.
//   - TrainProcessorVersion: neither Enterprise Document OCR nor the Layout
//     Parser is a trainable processor (training is the custom extractor's,
//     classifier's and splitter's, which are not served): INVALID_ARGUMENT.
//     EvaluateProcessorVersion and the evaluations, which need a labelled
//     dataset, are in documentaioutput.go (#1953).
//
// UNVERIFIED (re-read 2026-11-19: the manage-processor-versions page and the processorVersions reference do not say): the version display names and create times (taken from the
// IDs), the codes and texts of the refusals, and that a refusal is the RPC's
// error rather than a failed operation.

// docAIVersion is a pretrained version.
type docAIVersion struct {
	id, display string
}

// docAIVersions are each served type's pretrained versions, oldest first;
// the last is the newest stable.
var docAIVersions = map[string][]docAIVersion{
	"OCR_PROCESSOR": {
		{"pretrained-ocr-v1.2-2022-11-10", "Google Stable v1.2"},
		{"pretrained-ocr-v2.0-2023-06-02", "Google Stable v2.0"},
		{"pretrained-ocr-v2.1-2024-08-07", "Google Stable v2.1"},
	},
	"LAYOUT_PARSER_PROCESSOR": {
		{"pretrained-layout-parser-v1.0-2024-06-03", "Google Stable v1.0"},
	},
}

// resolveVersion is the version ID that id names for a processor type
// ("stable" the newest), or "".
func resolveVersion(t, id string) string {
	vs := docAIVersions[t]
	if id == "stable" && len(vs) > 0 {
		return vs[len(vs)-1].id
	}
	for _, v := range vs {
		if v.id == id {
			return id
		}
	}
	return ""
}

func versionProto(p *documentaipb.Processor, v docAIVersion) *documentaipb.ProcessorVersion {
	out := &documentaipb.ProcessorVersion{Name: p.GetName() + "/processorVersions/" + v.id, DisplayName: v.display,
		State: documentaipb.ProcessorVersion_DEPLOYED, GoogleManaged: true}
	if i := strings.LastIndex(v.id, "-v"); i >= 0 {
		if j := strings.Index(v.id[i+2:], "-"); j >= 0 {
			if t, err := time.Parse("2006-01-02", v.id[i+2+j+1:]); err == nil {
				out.CreateTime = timestamppb.New(t)
			}
		}
	}
	return out
}

// processorVersion looks a version up by its full name.
func (s *Server) processorVersion(name string) (*documentaipb.Processor, docAIVersion, error) {
	pname, id, ok := strings.Cut(name, "/processorVersions/")
	if !ok || id == "" || strings.Contains(id, "/") {
		return nil, docAIVersion{}, apierror.InvalidArgument("Invalid processor version name %q", name)
	}
	p, err := s.processor(pname)
	if err != nil {
		return nil, docAIVersion{}, err
	}
	rid := resolveVersion(p.GetType(), id)
	for _, v := range docAIVersions[p.GetType()] {
		if v.id == rid {
			return p, v, nil
		}
	}
	return nil, docAIVersion{}, apierror.NotFound("Processor version %s not found", name)
}

func (d *documentAIService) GetProcessorVersion(ctx context.Context, req *documentaipb.GetProcessorVersionRequest) (*documentaipb.ProcessorVersion, error) {
	p, v, err := d.s.processorVersion(req.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	if err := d.s.docAIKeyUsable(ctx, p); err != nil {
		return nil, grpcError(err)
	}
	return d.s.withKey(versionProto(p, v), p), nil
}

func (d *documentAIService) ListProcessorVersions(ctx context.Context, req *documentaipb.ListProcessorVersionsRequest) (*documentaipb.ListProcessorVersionsResponse, error) {
	p, err := d.s.processor(req.GetParent())
	if err != nil {
		return nil, grpcError(err)
	}
	if strings.Contains(req.GetParent(), "/processorVersions/") {
		return nil, grpcError(apierror.InvalidArgument("parent %q is not a processor", req.GetParent()))
	}
	if err := d.s.docAIKeyUsable(ctx, p); err != nil {
		return nil, grpcError(err)
	}
	var all []*documentaipb.ProcessorVersion
	for _, v := range docAIVersions[p.GetType()] {
		all = append(all, d.s.withKey(versionProto(p, v), p))
	}
	// Newest first, as the console lists them.
	sort.SliceStable(all, func(i, j int) bool { return all[i].GetName() > all[j].GetName() })
	page, next, err := docAIPage(all, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, grpcError(err)
	}
	return &documentaipb.ListProcessorVersionsResponse{ProcessorVersions: page, NextPageToken: next}, nil
}

// pretrainedRefusal is the refusal of a change to a pretrained version.
func (d *documentAIService) pretrainedRefusal(name, what string) error {
	p, v, err := d.s.processorVersion(name)
	if err != nil {
		return err
	}
	full := p.GetName() + "/processorVersions/" + v.id
	if full == p.GetDefaultProcessorVersion() {
		return apierror.FailedPrecondition("Cannot %s processor version %s: it is the processor's default version", what, full)
	}
	return apierror.FailedPrecondition("Cannot %s processor version %s: it is a pretrained processor version", what, full)
}

func (d *documentAIService) DeployProcessorVersion(_ context.Context, req *documentaipb.DeployProcessorVersionRequest) (*longrunningpb.Operation, error) {
	return nil, grpcError(d.pretrainedRefusal(req.GetName(), "deploy"))
}

func (d *documentAIService) UndeployProcessorVersion(_ context.Context, req *documentaipb.UndeployProcessorVersionRequest) (*longrunningpb.Operation, error) {
	return nil, grpcError(d.pretrainedRefusal(req.GetName(), "undeploy"))
}

func (d *documentAIService) DeleteProcessorVersion(_ context.Context, req *documentaipb.DeleteProcessorVersionRequest) (*longrunningpb.Operation, error) {
	return nil, grpcError(d.pretrainedRefusal(req.GetName(), "delete"))
}

func (d *documentAIService) SetDefaultProcessorVersion(_ context.Context, req *documentaipb.SetDefaultProcessorVersionRequest) (*longrunningpb.Operation, error) {
	if req.GetDefaultProcessorVersion() == "" {
		return nil, grpcError(apierror.InvalidArgument("default_processor_version is required"))
	}
	pname, _, _ := strings.Cut(req.GetDefaultProcessorVersion(), "/processorVersions/")
	if pname != req.GetProcessor() {
		return nil, grpcError(apierror.InvalidArgument("default_processor_version %s is not a version of processor %s",
			req.GetDefaultProcessorVersion(), req.GetProcessor()))
	}
	_, v, err := d.s.processorVersion(req.GetDefaultProcessorVersion())
	if err != nil {
		return nil, grpcError(err)
	}
	return d.processorOp(req.GetProcessor(), func(_ *docAIStore, p *documentaipb.Processor) error {
		p.DefaultProcessorVersion = p.GetName() + "/processorVersions/" + v.id
		return nil
	}, func(c *documentaipb.CommonOperationMetadata) any {
		return &documentaipb.SetDefaultProcessorVersionMetadata{CommonMetadata: c}
	}, &documentaipb.SetDefaultProcessorVersionResponse{})
}

func (d *documentAIService) TrainProcessorVersion(_ context.Context, req *documentaipb.TrainProcessorVersionRequest) (*longrunningpb.Operation, error) {
	p, err := d.s.processor(req.GetParent())
	if err != nil {
		return nil, grpcError(err)
	}
	return nil, grpcError(apierror.InvalidArgument("Processor type %s does not support training: "+
		"training creates versions of custom extractors, classifiers and splitters", p.GetType()))
}
