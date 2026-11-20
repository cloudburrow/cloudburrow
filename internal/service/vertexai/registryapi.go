package vertexai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// The gRPC (and, through the transcoder, REST) methods of the Model
// Registry and Endpoints (#1940); registry.go keeps the resources.

// regEndpoint returns the registry when name is one of its endpoints and
// not a Model Garden deployment.
func (s *Server) regEndpoint(name string) *registry {
	r := s.registry()
	if r == nil {
		return nil
	}
	p, l, id, err := parseChild("name", "endpoints", name)
	if err != nil {
		return nil
	}
	if s.gardenEndpoint(p, l, id) != nil {
		return nil
	}
	if r.hasEndpoint(p, l, id) || s.modelGarden() == nil {
		return r
	}
	return nil
}

// regModel returns the registry when name is one of its models.
func (s *Server) regModel(name string) *registry {
	r := s.registry()
	if r == nil {
		return nil
	}
	p, l, id, _, err := parseModelName("name", name)
	if err != nil {
		return nil
	}
	if r.has(p, l, id) {
		return r
	}
	if g := s.modelGarden(); g != nil {
		if _, err := g.Model(p, l, id); err == nil {
			return nil
		}
	}
	return r
}

func (s *Server) needRegistry() (*registry, error) {
	if r := s.registry(); r != nil {
		return r, nil
	}
	return nil, apierror.Unimplemented("the Model Registry and Endpoints are not served on this instance: deployed models run in " +
		"the cluster through Cloud Run (start with --services including run)")
}

// opDone returns a completed operation on parent.
func (s *Server) opDone(parent string, meta, resp proto.Message) (*longrunningpb.Operation, error) {
	name := s.newOp(parent)
	_ = s.ops.SetMetadata(name, meta)
	_ = s.ops.Succeed(name, resp)
	op, _ := s.ops.Get(name)
	out, err := opProto(op)
	return out, grpcError(err)
}

func nowMeta() *aiplatformpb.GenericOperationMetadata {
	now := time.Now()
	return genericMeta(now, now)
}

// ---- list filters and pages

// listFilter is the subset of AIP-160 the Models and Endpoints lists take:
// display_name="x", labels.k="v" and labels.k:*, joined by AND.
type listFilter struct {
	display string
	labels  map[string]string // "*" means present
	hasName bool
}

func parseListFilter(f string) (listFilter, error) {
	out := listFilter{labels: map[string]string{}}
	f = strings.TrimSpace(f)
	if f == "" {
		return out, nil
	}
	for _, c := range strings.Split(f, " AND ") {
		c = strings.TrimSpace(c)
		unq := func(v string) string { return strings.Trim(strings.TrimSpace(v), `"`) }
		switch {
		case strings.HasPrefix(c, "labels.") && strings.HasSuffix(c, ":*"):
			out.labels[strings.TrimSuffix(strings.TrimPrefix(c, "labels."), ":*")] = "*"
		case strings.HasPrefix(c, "labels.") && strings.Contains(c, "="):
			k, v, _ := strings.Cut(strings.TrimPrefix(c, "labels."), "=")
			out.labels[strings.TrimSpace(k)] = unq(v)
		case strings.HasPrefix(c, "display_name") && strings.Contains(c, "="):
			_, v, _ := strings.Cut(c, "=")
			out.display, out.hasName = unq(v), true
		default:
			return out, apierror.InvalidArgument("filter %q is not supported: the filter takes display_name=\"...\", "+
				"labels.KEY=\"VALUE\" and labels.KEY:*, joined by AND", c)
		}
	}
	return out, nil
}

func (f listFilter) match(display string, labels map[string]string) bool {
	if f.hasName && display != f.display {
		return false
	}
	for k, v := range f.labels {
		got, ok := labels[k]
		if !ok || (v != "*" && got != v) {
			return false
		}
	}
	return true
}

type listItem = proto.Message

// pageOf returns one page of items: page_size (100 by default, 1000 at
// most) from the offset page_token carries.
func pageOf(items []listItem, size int32, token string) ([]listItem, string, error) {
	off := 0
	if token != "" {
		b, err := base64.RawURLEncoding.DecodeString(token)
		n, err2 := strconv.Atoi(string(b))
		if err != nil || err2 != nil || n < 0 || n > len(items) {
			return nil, "", apierror.InvalidArgument("page_token %q is invalid", token)
		}
		off = n
	}
	if size <= 0 {
		size = 100
	}
	if size > 1000 {
		size = 1000
	}
	end := off + int(size)
	if end >= len(items) {
		return items[off:], "", nil
	}
	return items[off:end], base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(end))), nil
}

// ---- ModelService

func (ms *modelService) UploadModel(ctx context.Context, req *aiplatformpb.UploadModelRequest) (*longrunningpb.Operation, error) {
	r, err := ms.s.needRegistry()
	if err != nil {
		return nil, grpcError(err)
	}
	resp, err := r.upload(ctx, req)
	if err != nil {
		return nil, grpcError(err)
	}
	return ms.s.opDone(resp.GetModel(), &aiplatformpb.UploadModelOperationMetadata{GenericMetadata: nowMeta()}, resp)
}

func (ms *modelService) UpdateModel(_ context.Context, req *aiplatformpb.UpdateModelRequest) (*aiplatformpb.Model, error) {
	r, err := ms.s.needRegistry()
	if err != nil {
		return nil, grpcError(err)
	}
	if req.GetModel() == nil {
		return nil, grpcError(apierror.InvalidArgument("model is required"))
	}
	out, err := r.update(req.GetModel(), req.GetUpdateMask().GetPaths())
	return out, grpcError(err)
}

func (ms *modelService) ListModelVersions(_ context.Context, req *aiplatformpb.ListModelVersionsRequest) (*aiplatformpb.ListModelVersionsResponse, error) {
	r, err := ms.s.needRegistry()
	if err != nil {
		return nil, grpcError(err)
	}
	vs, err := r.versions(req.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	filter, err := parseListFilter(req.GetFilter())
	if err != nil {
		return nil, grpcError(err)
	}
	var items []listItem
	for _, v := range vs {
		if filter.match(v.GetDisplayName(), v.GetLabels()) {
			items = append(items, v)
		}
	}
	page, next, err := pageOf(items, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, grpcError(err)
	}
	out := &aiplatformpb.ListModelVersionsResponse{NextPageToken: next}
	for _, v := range page {
		out.Models = append(out.Models, v.(*aiplatformpb.Model))
	}
	return out, nil
}

func (ms *modelService) DeleteModelVersion(_ context.Context, req *aiplatformpb.DeleteModelVersionRequest) (*longrunningpb.Operation, error) {
	r, err := ms.s.needRegistry()
	if err != nil {
		return nil, grpcError(err)
	}
	if err := r.deleteVersion(req.GetName()); err != nil {
		return nil, grpcError(err)
	}
	base, _, _ := strings.Cut(req.GetName(), "@")
	return ms.s.doneDelete(base)
}

func (ms *modelService) MergeVersionAliases(_ context.Context, req *aiplatformpb.MergeVersionAliasesRequest) (*aiplatformpb.Model, error) {
	r, err := ms.s.needRegistry()
	if err != nil {
		return nil, grpcError(err)
	}
	out, err := r.mergeAliases(req.GetName(), req.GetVersionAliases())
	return out, grpcError(err)
}

// CopyModel copies a model version into another location (or the same),
// as a new model or a new version of parent_model; its artifacts are
// copied too.
func (ms *modelService) CopyModel(ctx context.Context, req *aiplatformpb.CopyModelRequest) (*longrunningpb.Operation, error) {
	r, err := ms.s.needRegistry()
	if err != nil {
		return nil, grpcError(err)
	}
	if req.GetEncryptionSpec() != nil {
		return nil, grpcError(apierror.Unimplemented("CopyModel encryption_spec (CMEK) is not supported"))
	}
	src, err := r.get(req.GetSourceModel())
	if err != nil {
		return nil, grpcError(err)
	}
	m := proto.Clone(src).(*aiplatformpb.Model)
	m.Name, m.VersionId, m.DeployedModels = "", "", nil
	m.VersionAliases = nil
	if sp, sl, sid, _, err := parseModelName("source_model", src.GetName()); err == nil {
		r.mu.Lock()
		m.ArtifactUri = r.storageURI(sp, sl, sid, src.GetVersionId())
		r.mu.Unlock()
	}
	up := &aiplatformpb.UploadModelRequest{Parent: req.GetParent(), Model: m}
	switch d := req.GetDestinationModel().(type) {
	case *aiplatformpb.CopyModelRequest_ModelId:
		up.ModelId = d.ModelId
	case *aiplatformpb.CopyModelRequest_ParentModel:
		up.ParentModel = d.ParentModel
	}
	resp, err := r.upload(ctx, up)
	if err != nil {
		return nil, grpcError(err)
	}
	return ms.s.opDone(resp.GetModel(), &aiplatformpb.CopyModelOperationMetadata{GenericMetadata: nowMeta()},
		&aiplatformpb.CopyModelResponse{Model: resp.GetModel(), ModelVersionId: resp.GetModelVersionId()})
}

// ExportModel exports a version in the "custom-trained" format: its
// artifacts, the directory as it was uploaded, to a gs:// prefix, and its
// serving image to an Artifact Registry path in this instance's registry.
func (ms *modelService) ExportModel(ctx context.Context, req *aiplatformpb.ExportModelRequest) (*longrunningpb.Operation, error) {
	r, err := ms.s.needRegistry()
	if err != nil {
		return nil, grpcError(err)
	}
	v, err := r.get(req.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	oc := req.GetOutputConfig()
	if f := oc.GetExportFormatId(); f != "custom-trained" {
		return nil, grpcError(apierror.InvalidArgument("output_config.export_format_id %q is not supported for this model; "+
			"its supported_export_formats is [custom-trained]", f))
	}
	if oc.GetArtifactDestination() == nil && oc.GetImageDestination() == nil {
		return nil, grpcError(apierror.InvalidArgument("output_config must set artifact_destination or image_destination"))
	}
	p, l, id, _, _ := parseModelName("name", v.GetName())
	info := &aiplatformpb.ExportModelOperationMetadata_OutputInfo{}
	var dst, src string
	if oc.GetArtifactDestination() != nil {
		dst = strings.TrimSuffix(oc.GetArtifactDestination().GetOutputUriPrefix(), "/")
		if !strings.HasPrefix(dst, "gs://") {
			return nil, grpcError(apierror.InvalidArgument("output_config.artifact_destination.output_uri_prefix must be a gs:// URI"))
		}
		r.mu.Lock()
		src = r.storageURI(p, l, id, v.GetVersionId())
		r.mu.Unlock()
		if src == "" {
			return nil, grpcError(apierror.FailedPrecondition("Model %s@%s has no artifacts to export (it was uploaded without artifact_uri; "+
				"its supported_export_formats' exportable_contents is [IMAGE])", v.GetName(), v.GetVersionId()))
		}
	}
	image := ""
	if d := oc.GetImageDestination(); d != nil {
		if image = strings.TrimSpace(d.GetOutputUri()); image == "" {
			return nil, grpcError(apierror.InvalidArgument("output_config.image_destination.output_uri is required"))
		}
	}
	r.mu.Lock()
	images := r.images
	r.mu.Unlock()
	if image != "" && images == nil {
		return nil, grpcError(apierror.FailedPrecondition("output_config.image_destination needs Artifact Registry, which is not " +
			"served on this instance (start with --services including run)"))
	}
	if src != "" {
		out := dst + "/model-" + id + "/custom-trained/" + time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
		if err := r.artifacts.Copy(ctx, p, src, out); err != nil {
			return nil, grpcError(err)
		}
		info.ArtifactOutputUri = out
	}
	if image != "" {
		out, err := images.Export(ctx, v.GetContainerSpec().GetImageUri(), image)
		if err != nil {
			return nil, grpcError(err)
		}
		info.ImageOutputUri = out
	}
	return ms.s.opDone(v.GetName(), &aiplatformpb.ExportModelOperationMetadata{GenericMetadata: nowMeta(), OutputInfo: info},
		&aiplatformpb.ExportModelResponse{})
}

func (ms *modelService) ImportModelEvaluation(_ context.Context, req *aiplatformpb.ImportModelEvaluationRequest) (*aiplatformpb.ModelEvaluation, error) {
	r, err := ms.s.needRegistry()
	if err != nil {
		return nil, grpcError(err)
	}
	out, err := r.importEvaluation(req.GetParent(), req.GetModelEvaluation())
	return out, grpcError(err)
}

func (ms *modelService) GetModelEvaluation(_ context.Context, req *aiplatformpb.GetModelEvaluationRequest) (*aiplatformpb.ModelEvaluation, error) {
	r, err := ms.s.needRegistry()
	if err != nil {
		return nil, grpcError(err)
	}
	out, err := r.evaluation(req.GetName())
	return out, grpcError(err)
}

func (ms *modelService) ListModelEvaluations(_ context.Context, req *aiplatformpb.ListModelEvaluationsRequest) (*aiplatformpb.ListModelEvaluationsResponse, error) {
	r, err := ms.s.needRegistry()
	if err != nil {
		return nil, grpcError(err)
	}
	if req.GetFilter() != "" {
		return nil, grpcError(apierror.Unimplemented("ListModelEvaluations filter is not supported"))
	}
	es, err := r.evaluations(req.GetParent())
	if err != nil {
		return nil, grpcError(err)
	}
	var items []listItem
	for _, e := range es {
		items = append(items, e)
	}
	page, next, err := pageOf(items, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, grpcError(err)
	}
	out := &aiplatformpb.ListModelEvaluationsResponse{NextPageToken: next}
	for _, e := range page {
		out.ModelEvaluations = append(out.ModelEvaluations, e.(*aiplatformpb.ModelEvaluation))
	}
	return out, nil
}

// ---- EndpointService

func (es *endpointService) CreateEndpoint(_ context.Context, req *aiplatformpb.CreateEndpointRequest) (*longrunningpb.Operation, error) {
	r, err := es.s.needRegistry()
	if err != nil {
		return nil, grpcError(err)
	}
	e, err := r.createEndpoint(req.GetParent(), req.GetEndpointId(), req.GetEndpoint())
	if err != nil {
		return nil, grpcError(err)
	}
	return es.s.opDone(e.GetName(), &aiplatformpb.CreateEndpointOperationMetadata{GenericMetadata: nowMeta()}, e)
}

func (es *endpointService) UpdateEndpoint(_ context.Context, req *aiplatformpb.UpdateEndpointRequest) (*aiplatformpb.Endpoint, error) {
	r := es.s.regEndpoint(req.GetEndpoint().GetName())
	if r == nil {
		if _, err := es.s.needRegistry(); err != nil {
			return nil, grpcError(err)
		}
		return nil, grpcError(apierror.FailedPrecondition("Endpoint %s is a Model Garden deployment; its one deployed model "+
			"takes all its traffic, and it is not updatable", req.GetEndpoint().GetName()))
	}
	out, err := r.updateEndpoint(req.GetEndpoint(), req.GetUpdateMask().GetPaths())
	return out, grpcError(err)
}

func (es *endpointService) UpdateEndpointLongRunning(ctx context.Context, req *aiplatformpb.UpdateEndpointLongRunningRequest) (*longrunningpb.Operation, error) {
	r := es.s.regEndpoint(req.GetEndpoint().GetName())
	if r == nil {
		return nil, grpcError(apierror.NotFound("Endpoint %s not found", req.GetEndpoint().GetName()))
	}
	// The long-running form replaces every updatable field.
	out, err := r.updateEndpoint(req.GetEndpoint(), []string{"display_name", "description", "labels", "traffic_split"})
	if err != nil {
		return nil, grpcError(err)
	}
	return es.s.opDone(out.GetName(), &aiplatformpb.UpdateEndpointOperationMetadata{GenericMetadata: nowMeta()}, out)
}

// DeployModel records the deployed model and returns an operation that
// completes when its container, running in the cluster, answers its health
// route; the deployed model then joins the endpoint with its traffic.
func (es *endpointService) DeployModel(ctx context.Context, req *aiplatformpb.DeployModelRequest) (*longrunningpb.Operation, error) {
	r := es.s.regEndpoint(req.GetEndpoint())
	if r == nil {
		if _, err := es.s.needRegistry(); err != nil {
			return nil, grpcError(err)
		}
		return nil, grpcError(apierror.FailedPrecondition("Endpoint %s is a Model Garden deployment; deploy another model on an "+
			"endpoint made with CreateEndpoint", req.GetEndpoint()))
	}
	e, dm, run, err := r.prepareDeploy(req)
	if err != nil {
		return nil, grpcError(err)
	}
	name := es.s.newOp(req.GetEndpoint())
	created := time.Now()
	_ = es.s.ops.SetMetadata(name, &aiplatformpb.DeployModelOperationMetadata{GenericMetadata: genericMeta(created, created),
		DeploymentStage: aiplatformpb.DeploymentStage_STARTING_MODEL_SERVER})
	go func() {
		out, err := r.deploy(context.Background(), req, e, dm, run)
		stage := aiplatformpb.DeploymentStage_DEPLOYMENT_TERMINATED
		_ = es.s.ops.SetMetadata(name, &aiplatformpb.DeployModelOperationMetadata{GenericMetadata: genericMeta(created, time.Now()),
			DeploymentStage: stage})
		if err != nil {
			_ = es.s.ops.Fail(name, err)
			return
		}
		_ = es.s.ops.Succeed(name, &aiplatformpb.DeployModelResponse{DeployedModel: out})
	}()
	op, _ := es.s.ops.Get(name)
	out, err := opProto(op)
	return out, grpcError(err)
}

func (s *Server) regUndeploy(ctx context.Context, r *registry, req *aiplatformpb.UndeployModelRequest) (*longrunningpb.Operation, error) {
	if err := r.undeploy(ctx, req); err != nil {
		return nil, grpcError(err)
	}
	return s.opDone(req.GetEndpoint(), &aiplatformpb.UndeployModelOperationMetadata{GenericMetadata: nowMeta()},
		&aiplatformpb.UndeployModelResponse{})
}

func (es *endpointService) MutateDeployedModel(_ context.Context, req *aiplatformpb.MutateDeployedModelRequest) (*longrunningpb.Operation, error) {
	r := es.s.regEndpoint(req.GetEndpoint())
	if r == nil {
		return nil, grpcError(apierror.NotFound("Endpoint %s not found", req.GetEndpoint()))
	}
	dm, err := r.mutate(req, req.GetUpdateMask().GetPaths())
	if err != nil {
		return nil, grpcError(err)
	}
	return es.s.opDone(req.GetEndpoint(), &aiplatformpb.MutateDeployedModelOperationMetadata{GenericMetadata: nowMeta()},
		&aiplatformpb.MutateDeployedModelResponse{DeployedModel: dm})
}

// ---- prediction

// regTarget returns where a request to a registry endpoint goes: the
// predict URL of the deployed model the traffic split picks, its ID, and
// the model version it serves (nil if it is gone).
func (s *Server) regTarget(project, location, endpoint string) (string, string, *aiplatformpb.Model, bool, error) {
	r := s.registry()
	if r == nil || !r.hasEndpoint(project, location, endpoint) {
		return "", "", nil, false, nil
	}
	dm, sv, err := r.pick(project, location, endpoint)
	if err != nil {
		return "", "", nil, true, err
	}
	model, _ := r.get(sv.Model)
	return sv.URL + sv.PredictRoute, dm, model, true, nil
}

// withDeployedModelID adds deployedModelId, and the model, modelVersionId
// and modelDisplayName of the version it serves, to a prediction
// response, as Vertex does before returning it to the client
// (PredictResponse's fields).
func withDeployedModelID(body []byte, id string, model *aiplatformpb.Model) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	set := func(k, v string) {
		if v != "" {
			b, _ := json.Marshal(v)
			m[k] = b
		}
	}
	set("deployedModelId", id)
	if model != nil {
		set("model", model.GetName())
		set("modelVersionId", model.GetVersionId())
		set("modelDisplayName", model.GetDisplayName())
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}
