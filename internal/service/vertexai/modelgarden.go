package vertexai

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	betapb "cloud.google.com/go/aiplatform/apiv1beta1/aiplatformpb"
	"cloud.google.com/go/documentai/apiv1/documentaipb"
	documentaibetapb "cloud.google.com/go/documentai/apiv1beta3/documentaipb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/speech/apiv2/speechpb"
	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/genproto/googleapis/api/httpbody"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/lro"
	"github.com/cloudburrow/cloudburrow/internal/modelgarden"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// Model Garden's Hugging Face deployments (#1840):
// google.cloud.aiplatform.v1.ModelGardenService.Deploy, the Endpoint and
// Model it creates (EndpointService Get, List, UndeployModel and Delete;
// ModelService Get, List and Delete), the operations they return, and
// prediction on the endpoint: PredictionService Predict, RawPredict and
// StreamRawPredict, and v1beta1 ChatCompletions, the OpenAI-compatible
// path. gRPC and JSON both, the JSON through the REST transcoder of the same
// implementations. Every other method of these services is UNIMPLEMENTED.

// SetModelGarden serves m's deployments; nil serves none.
func (s *Server) SetModelGarden(m *modelgarden.Manager) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.garden = m
	if s.ops == nil {
		s.ops = lro.NewStore(nil)
	}
}

func (s *Server) modelGarden() *modelgarden.Manager {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.garden
}

// gardenEndpoint reports whether endpoint is a Model Garden deployment.
func (s *Server) gardenEndpoint(project, location, endpoint string) *modelgarden.Manager {
	g := s.modelGarden()
	if g != nil && g.Has(project, location, endpoint) {
		return g
	}
	return nil
}

func gardenUnavailable() error {
	return apierror.Unimplemented("Model Garden deployments are not served on this instance: they need the Hugging Face " +
		"model cache and Docker (start with --services including run)")
}

// parent parses projects/{p}/locations/{l}.
func parseParent(field, name string) (string, string, error) {
	p := strings.Split(name, "/")
	if len(p) == 4 && p[0] == "projects" && p[2] == "locations" && p[1] != "" && p[3] != "" {
		return p[1], p[3], nil
	}
	return "", "", apierror.InvalidArgument("%s %q is not projects/{project}/locations/{location}", field, name)
}

// parseChild parses projects/{p}/locations/{l}/{collection}/{id}.
func parseChild(field, collection, name string) (string, string, string, error) {
	p := strings.Split(name, "/")
	if len(p) == 6 && p[0] == "projects" && p[2] == "locations" && p[4] == collection && p[1] != "" && p[3] != "" && p[5] != "" {
		return p[1], p[3], p[5], nil
	}
	return "", "", "", apierror.InvalidArgument("%s %q is not projects/{project}/locations/{location}/%s/{id}", field, name, collection)
}

// ---- operations

func (s *Server) newOp(parent string) string {
	return s.ops.CreateNamed(parent+"/operations/"+fmt.Sprint(time.Now().UnixNano()), parent).Name
}

func genericMeta(created, updated time.Time) *aiplatformpb.GenericOperationMetadata {
	return &aiplatformpb.GenericOperationMetadata{CreateTime: timestamppb.New(created), UpdateTime: timestamppb.New(updated)}
}

func opProto(op lro.Operation) (*longrunningpb.Operation, error) {
	out := &longrunningpb.Operation{Name: op.Name, Done: op.Done()}
	if m, ok := op.Metadata.(proto.Message); ok {
		a, err := anypb.New(m)
		if err != nil {
			return nil, err
		}
		out.Metadata = a
	}
	switch op.State {
	case lro.StateSucceeded:
		r, ok := op.Response.(proto.Message)
		if !ok {
			r = &emptypb.Empty{}
		}
		a, err := anypb.New(r)
		if err != nil {
			return nil, err
		}
		out.Result = &longrunningpb.Operation_Response{Response: a}
	case lro.StateFailed:
		st := apierror.From(op.Error).GRPCStatus().Proto()
		out.Result = &longrunningpb.Operation_Error{Error: st}
	}
	return out, nil
}

type operations struct {
	longrunningpb.UnimplementedOperationsServer
	s *Server
}

func (o *operations) GetOperation(_ context.Context, req *longrunningpb.GetOperationRequest) (*longrunningpb.Operation, error) {
	if o.s.ops == nil {
		return nil, grpcError(apierror.NotFound("operation %s not found", req.GetName()))
	}
	op, err := o.s.ops.Get(req.GetName())
	if err != nil {
		return nil, grpcError(apierror.NotFound("operation %s not found", req.GetName()))
	}
	out, err := opProto(op)
	return out, grpcError(err)
}

func (o *operations) WaitOperation(ctx context.Context, req *longrunningpb.WaitOperationRequest) (*longrunningpb.Operation, error) {
	timeout := 60 * time.Second
	if d := req.GetTimeout(); d != nil {
		timeout = d.AsDuration()
	}
	deadline := time.Now().Add(timeout)
	for {
		op, err := o.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: req.GetName()})
		if err != nil || op.GetDone() || time.Now().After(deadline) {
			return op, err
		}
		select {
		case <-ctx.Done():
			return nil, grpcError(ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// ---- ModelGardenService

type modelGardenService struct {
	aiplatformpb.UnimplementedModelGardenServiceServer
	s *Server
}

// Deploy validates the request, creates the Endpoint and Model, and returns
// an operation that completes when the engine answers its health route.
func (g *modelGardenService) Deploy(ctx context.Context, req *aiplatformpb.DeployRequest) (*longrunningpb.Operation, error) {
	m := g.s.modelGarden()
	if m == nil {
		return nil, grpcError(gardenUnavailable())
	}
	project, location, err := parseParent("destination", req.GetDestination())
	if err != nil {
		return nil, grpcError(err)
	}
	mc, ec, dc := req.GetModelConfig(), req.GetEndpointConfig(), req.GetDeployConfig()
	if req.GetPublisherModelName() != "" {
		return nil, grpcError(apierror.Unimplemented("deploying publisher_model_name %q is not served: CloudBurrow deploys Hugging Face "+
			"models (hugging_face_model_id) on vLLM and Text Embeddings Inference", req.GetPublisherModelName()))
	}
	if mc.GetContainerSpec() != nil {
		return nil, grpcError(apierror.Unimplemented("model_config.container_spec is not served: the serving container is the one " +
			"Model Garden picks for the model's task (vLLM or Text Embeddings Inference)"))
	}
	r := modelgarden.Request{Project: project, Location: location, HFModelID: req.GetHuggingFaceModelId(),
		AcceptEULA: mc.GetAcceptEula(), TokenSet: mc.GetHuggingFaceAccessToken() != "",
		ModelDisplayName: mc.GetModelDisplayName(), ModelUserID: mc.GetModelUserId(),
		EndpointDisplayName: ec.GetEndpointDisplayName(), EndpointUserID: ec.GetEndpointUserId(),
		DedicatedDisabled: ec.GetDedicatedEndpointDisabled(),
		MachineType:       dc.GetDedicatedResources().GetMachineSpec().GetMachineType(), Labels: dc.GetSystemLabels()}
	p, err := m.Prepare(ctx, r)
	if err != nil {
		return nil, grpcError(err)
	}
	name := g.s.newOp(req.GetDestination())
	created := time.Now()
	meta := func() *aiplatformpb.DeployOperationMetadata {
		return &aiplatformpb.DeployOperationMetadata{GenericMetadata: genericMeta(created, time.Now()),
			PublisherModel: p.PublisherModel, Destination: req.GetDestination(), ModelId: p.Model}
	}
	_ = g.s.ops.SetMetadata(name, meta())
	go func() {
		err := m.Run(context.Background(), project, location, p)
		_ = g.s.ops.SetMetadata(name, meta())
		if err != nil {
			_ = g.s.ops.Fail(name, err)
			return
		}
		_ = g.s.ops.Succeed(name, &aiplatformpb.DeployResponse{PublisherModel: p.PublisherModel, Endpoint: p.EndpointName, Model: p.ModelName})
	}()
	op, _ := g.s.ops.Get(name)
	out, err := opProto(op)
	return out, grpcError(err)
}

// DeployHF runs Deploy for the console, returning the operation name and
// the endpoint's resource name.
func (s *Server) DeployHF(ctx context.Context, project, location, hfID string, acceptEULA bool) (op, endpoint string, err error) {
	g := &modelGardenService{s: s}
	o, err := g.Deploy(ctx, &aiplatformpb.DeployRequest{
		Artifacts:   &aiplatformpb.DeployRequest_HuggingFaceModelId{HuggingFaceModelId: hfID},
		Destination: "projects/" + project + "/locations/" + location,
		ModelConfig: &aiplatformpb.DeployRequest_ModelConfig{AcceptEula: acceptEULA}})
	if err != nil {
		return "", "", err
	}
	md := &aiplatformpb.DeployOperationMetadata{}
	_ = o.GetMetadata().UnmarshalTo(md)
	for _, e := range s.modelGarden().Endpoints(project, location) {
		if e.Deployed != nil && e.Deployed.ModelID == md.GetModelId() {
			return o.GetName(), e.Name(), nil
		}
	}
	return o.GetName(), "", nil
}

// ---- EndpointService

type endpointService struct {
	aiplatformpb.UnimplementedEndpointServiceServer
	s *Server
}

func endpointProto(m *modelgarden.Manager, e modelgarden.Endpoint) *aiplatformpb.Endpoint {
	out := &aiplatformpb.Endpoint{Name: e.Name(), DisplayName: e.DisplayName, Labels: e.Labels,
		CreateTime: timestamppb.New(e.Created), UpdateTime: timestamppb.New(e.Updated),
		DedicatedEndpointEnabled: e.Dedicated}
	if d := e.Deployed; d != nil && d.State == modelgarden.StateReady {
		model, _ := m.Model(e.Project, e.Location, d.ModelID)
		out.DeployedModels = []*aiplatformpb.DeployedModel{{Id: d.ID, Model: model.Name(), ModelVersionId: "1",
			DisplayName: d.DisplayName, CreateTime: timestamppb.New(d.Created),
			PredictionResources: &aiplatformpb.DeployedModel_DedicatedResources{DedicatedResources: &aiplatformpb.DedicatedResources{
				MachineSpec: &aiplatformpb.MachineSpec{MachineType: d.MachineType}, MinReplicaCount: 1, MaxReplicaCount: 1}}}}
		out.TrafficSplit = map[string]int32{d.ID: 100}
	}
	return out
}

func (es *endpointService) garden() (*modelgarden.Manager, error) {
	if m := es.s.modelGarden(); m != nil {
		return m, nil
	}
	return nil, gardenUnavailable()
}

func (es *endpointService) GetEndpoint(_ context.Context, req *aiplatformpb.GetEndpointRequest) (*aiplatformpb.Endpoint, error) {
	if r := es.s.regEndpoint(req.GetName()); r != nil {
		e, err := r.getEndpoint(req.GetName())
		return e, grpcError(err)
	}
	m, err := es.garden()
	if err != nil {
		return nil, grpcError(err)
	}
	p, l, id, err := parseChild("name", "endpoints", req.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	e, err := m.Endpoint(p, l, id)
	if err != nil {
		return nil, grpcError(err)
	}
	return endpointProto(m, e), nil
}

var vertexEndpointFilter = []aipfilter.Option{
	aipfilter.Fields("endpoint", "display_name", "labels"),
	aipfilter.Computed("endpoint", func(m proto.Message) string {
		n := m.(*aiplatformpb.Endpoint).GetName()
		return n[strings.LastIndexByte(n, '/')+1:]
	}),
}

func (es *endpointService) ListEndpoints(_ context.Context, req *aiplatformpb.ListEndpointsRequest) (*aiplatformpb.ListEndpointsResponse, error) {
	m, r := es.s.modelGarden(), es.s.registry()
	if m == nil && r == nil {
		return nil, grpcError(gardenUnavailable())
	}
	p, l, err := parseParent("parent", req.GetParent())
	if err != nil {
		return nil, grpcError(err)
	}
	if strings.Contains(req.GetFilter(), "base_model_name") {
		return nil, grpcError(apierror.Unimplemented("ListEndpoints filter on base_model_name is not supported"))
	}
	// Model Garden's endpoints and the Model Registry's (#1940).
	var all []*aiplatformpb.Endpoint
	if m != nil {
		for _, e := range m.Endpoints(p, l) {
			all = append(all, endpointProto(m, e))
		}
	}
	if r != nil {
		all = append(all, r.listEndpoints(p, l)...)
	}
	// ListEndpointsRequest documents the filter's fields (endpoint, the ID;
	// display_name; labels) and order_by's (display_name, create_time,
	// update_time).
	all, err = aipfilter.Apply(req.GetFilter(), all, vertexEndpointFilter...)
	if err != nil {
		return nil, grpcError(err)
	}
	if err := aipfilter.SortBy(req.GetOrderBy(), all, aipfilter.Fields("display_name", "create_time", "update_time")); err != nil {
		return nil, grpcError(err)
	}
	items, next, err := aipfilter.Page("vertex-endpoints:"+req.GetParent()+"|"+req.GetFilter()+"|"+req.GetOrderBy(), all, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, grpcError(err)
	}
	return &aiplatformpb.ListEndpointsResponse{Endpoints: items, NextPageToken: next}, nil
}

func (es *endpointService) UndeployModel(ctx context.Context, req *aiplatformpb.UndeployModelRequest) (*longrunningpb.Operation, error) {
	if r := es.s.regEndpoint(req.GetEndpoint()); r != nil {
		return es.s.regUndeploy(ctx, r, req)
	}
	m, err := es.garden()
	if err != nil {
		return nil, grpcError(err)
	}
	p, l, id, err := parseChild("endpoint", "endpoints", req.GetEndpoint())
	if err != nil {
		return nil, grpcError(err)
	}
	if req.GetDeployedModelId() == "" {
		return nil, grpcError(apierror.InvalidArgument("deployed_model_id is required"))
	}
	if _, err := m.Endpoint(p, l, id); err != nil {
		return nil, grpcError(err)
	}
	if err := m.Undeploy(context.Background(), p, l, id, req.GetDeployedModelId()); err != nil {
		return nil, grpcError(err)
	}
	name := es.s.newOp(req.GetEndpoint())
	now := time.Now()
	_ = es.s.ops.SetMetadata(name, &aiplatformpb.UndeployModelOperationMetadata{GenericMetadata: genericMeta(now, now)})
	_ = es.s.ops.Succeed(name, &aiplatformpb.UndeployModelResponse{})
	op, _ := es.s.ops.Get(name)
	out, err := opProto(op)
	return out, grpcError(err)
}

func (es *endpointService) DeleteEndpoint(_ context.Context, req *aiplatformpb.DeleteEndpointRequest) (*longrunningpb.Operation, error) {
	if r := es.s.regEndpoint(req.GetName()); r != nil {
		if err := r.deleteEndpoint(req.GetName()); err != nil {
			return nil, grpcError(err)
		}
		return es.s.doneDelete(req.GetName())
	}
	m, err := es.garden()
	if err != nil {
		return nil, grpcError(err)
	}
	p, l, id, err := parseChild("name", "endpoints", req.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	if err := m.DeleteEndpoint(p, l, id); err != nil {
		return nil, grpcError(err)
	}
	return es.s.doneDelete(req.GetName())
}

func (s *Server) doneDelete(parent string) (*longrunningpb.Operation, error) {
	name := s.newOp(parent)
	now := time.Now()
	_ = s.ops.SetMetadata(name, &aiplatformpb.DeleteOperationMetadata{GenericMetadata: genericMeta(now, now)})
	_ = s.ops.Succeed(name, &emptypb.Empty{})
	op, _ := s.ops.Get(name)
	out, err := opProto(op)
	return out, grpcError(err)
}

// ---- ModelService

type modelService struct {
	aiplatformpb.UnimplementedModelServiceServer
	s *Server
}

func modelProto(m *modelgarden.Manager, x modelgarden.Model) *aiplatformpb.Model {
	eng, _ := modelgarden.EngineByName(x.Engine)
	spec := &aiplatformpb.ModelContainerSpec{ImageUri: x.Image, Args: x.Args, PredictRoute: eng.PredictRoute,
		HealthRoute: eng.HealthRoute, Ports: []*aiplatformpb.Port{{ContainerPort: 8080}}}
	for _, k := range sortedEnv(x.Env) {
		spec.Env = append(spec.Env, &aiplatformpb.EnvVar{Name: k, Value: x.Env[k]})
	}
	out := &aiplatformpb.Model{Name: x.Name(), DisplayName: x.DisplayName, VersionId: "1", VersionAliases: []string{"default"},
		CreateTime: timestamppb.New(x.Created), UpdateTime: timestamppb.New(x.Updated),
		VersionCreateTime: timestamppb.New(x.Created), VersionUpdateTime: timestamppb.New(x.Updated),
		ContainerSpec:                     spec,
		SupportedDeploymentResourcesTypes: []aiplatformpb.Model_DeploymentResourcesType{aiplatformpb.Model_DEDICATED_RESOURCES},
		ModelSourceInfo:                   &aiplatformpb.ModelSourceInfo{SourceType: aiplatformpb.ModelSourceInfo_MODEL_GARDEN},
		BaseModelSource: &aiplatformpb.Model_BaseModelSource{Source: &aiplatformpb.Model_BaseModelSource_ModelGardenSource{
			ModelGardenSource: &aiplatformpb.ModelGardenSource{PublicModelName: modelgarden.PublisherModel(x.HFModelID), VersionId: "001"}}},
		Labels: map[string]string{}}
	for _, e := range m.DeployedOn(x.Project, x.Location, x.ID) {
		if e.Deployed.State == modelgarden.StateReady {
			out.DeployedModels = append(out.DeployedModels, &aiplatformpb.DeployedModelRef{Endpoint: e.Name(), DeployedModelId: e.Deployed.ID})
		}
	}
	return out
}

func sortedEnv(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func (ms *modelService) garden() (*modelgarden.Manager, error) {
	if m := ms.s.modelGarden(); m != nil {
		return m, nil
	}
	return nil, gardenUnavailable()
}

func (ms *modelService) GetModel(_ context.Context, req *aiplatformpb.GetModelRequest) (*aiplatformpb.Model, error) {
	if r := ms.s.regModel(req.GetName()); r != nil {
		x, err := r.get(req.GetName())
		return x, grpcError(err)
	}
	m, err := ms.garden()
	if err != nil {
		return nil, grpcError(err)
	}
	name, _, _ := strings.Cut(req.GetName(), "@") // models/{id}@{version}: one version, 1
	p, l, id, err := parseChild("name", "models", name)
	if err != nil {
		return nil, grpcError(err)
	}
	x, err := m.Model(p, l, id)
	if err != nil {
		return nil, grpcError(err)
	}
	return modelProto(m, x), nil
}

func (ms *modelService) ListModels(_ context.Context, req *aiplatformpb.ListModelsRequest) (*aiplatformpb.ListModelsResponse, error) {
	m, r := ms.s.modelGarden(), ms.s.registry()
	if m == nil && r == nil {
		return nil, grpcError(gardenUnavailable())
	}
	p, l, err := parseParent("parent", req.GetParent())
	if err != nil {
		return nil, grpcError(err)
	}
	filter, err := parseListFilter(req.GetFilter())
	if err != nil {
		return nil, grpcError(err)
	}
	var all []*aiplatformpb.Model
	if m != nil {
		for _, x := range m.ModelList(p, l) {
			all = append(all, modelProto(m, x))
		}
	}
	if r != nil {
		all = append(all, r.list(p, l)...)
	}
	var items []listItem
	for _, x := range all {
		if filter.match(x.GetDisplayName(), x.GetLabels()) {
			items = append(items, x)
		}
	}
	page, next, err := pageOf(items, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, grpcError(err)
	}
	out := &aiplatformpb.ListModelsResponse{NextPageToken: next}
	for _, x := range page {
		out.Models = append(out.Models, x.(*aiplatformpb.Model))
	}
	return out, nil
}

func (ms *modelService) DeleteModel(_ context.Context, req *aiplatformpb.DeleteModelRequest) (*longrunningpb.Operation, error) {
	if r := ms.s.regModel(req.GetName()); r != nil {
		if err := r.delete(req.GetName()); err != nil {
			return nil, grpcError(err)
		}
		return ms.s.doneDelete(req.GetName())
	}
	m, err := ms.garden()
	if err != nil {
		return nil, grpcError(err)
	}
	p, l, id, err := parseChild("name", "models", req.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	if err := m.DeleteModel(p, l, id); err != nil {
		return nil, grpcError(err)
	}
	return ms.s.doneDelete(req.GetName())
}

// ---- v1beta1 ChatCompletions

type betaPrediction struct {
	betapb.UnimplementedPredictionServiceServer
	s *Server
}

func (b *betaPrediction) ChatCompletions(req *betapb.ChatCompletionsRequest, stream betapb.PredictionService_ChatCompletionsServer) error {
	t, err := parseTarget("endpoint", req.GetEndpoint())
	if err != nil {
		return grpcError(err)
	}
	if t.endpoint == "" {
		return grpcError(apierror.Unimplemented("ChatCompletions is served on Model Garden endpoints only"))
	}
	m := b.s.gardenEndpoint(t.project, t.location, t.endpoint)
	if m == nil {
		return grpcError(apierror.NotFound("endpoint %s not found", req.GetEndpoint()))
	}
	err = m.Stream(stream.Context(), t.project, t.location, t.endpoint, req.GetHttpBody().GetData(), false, func(ct string, data []byte) error {
		return sent(stream.Send(&httpbody.HttpBody{ContentType: ct, Data: data}))
	})
	return streamError(err)
}

// chatCompletionsREST serves POST /v1beta1/.../endpoints/{e}/chat/completions.
func (s *Server) chatCompletionsREST(w http.ResponseWriter, r *http.Request) {
	project, location, endpoint := r.PathValue("project"), r.PathValue("location"), r.PathValue("endpoint")
	body, err := readBody(r)
	if err != nil {
		apierror.WriteJSON(w, err)
		return
	}
	m := s.gardenEndpoint(project, location, endpoint)
	if m == nil {
		apierror.WriteJSON(w, apierror.NotFound("endpoint projects/%s/locations/%s/endpoints/%s not found", project, location, endpoint))
		return
	}
	flusher, _ := w.(http.Flusher)
	wrote := false
	err = m.Stream(r.Context(), project, location, endpoint, body, false, func(ct string, data []byte) error {
		if !wrote {
			if ct != "" {
				w.Header().Set("Content-Type", ct)
			}
			w.WriteHeader(http.StatusOK)
			wrote = true
		}
		_, err := w.Write(data)
		if flusher != nil {
			flusher.Flush()
		}
		return err
	})
	if err != nil && !wrote && r.Context().Err() == nil {
		apierror.WriteJSON(w, err)
	}
}

// ---- registration

// registerModelGarden registers the services on srv (a *grpc.Server or the
// REST transcoder).
func (s *Server) registerModelGarden(srv grpc.ServiceRegistrar) {
	aiplatformpb.RegisterModelGardenServiceServer(srv, &modelGardenService{s: s})
	aiplatformpb.RegisterEndpointServiceServer(srv, &endpointService{s: s})
	aiplatformpb.RegisterModelServiceServer(srv, &modelService{s: s})
}

func opRule(pattern string) *annotations.HttpRule {
	return &annotations.HttpRule{Selector: "google.longrunning.Operations.GetOperation",
		Pattern: &annotations.HttpRule_Get{Get: pattern}}
}

// restTranscoder serves the JSON surface of the registered services.
func (s *Server) restTranscoder() *rest.Transcoder {
	s.restOnce.Do(func() {
		t := &rest.Transcoder{Packages: []string{"google.cloud.aiplatform.v1", "google.cloud.documentai.v1", "google.cloud.documentai.v1beta3", "google.cloud.speech.v2"}, Mixins: []*annotations.HttpRule{
			opRule("/v2/{name=projects/*/locations/*/operations/*}"),
			opRule("/v1/{name=projects/*/locations/*/operations/*}"),
			opRule("/v1/{name=projects/*/locations/*/endpoints/*/operations/*}"),
			opRule("/v1/{name=projects/*/locations/*/models/*/operations/*}"),
			// gcloud ai endpoints deploy-model polls its operation under
			// v1beta1 (#1940); it is the same operation.
			opRule("/v1beta1/{name=projects/*/locations/*/operations/*}"),
			opRule("/v1beta1/{name=projects/*/locations/*/endpoints/*/operations/*}"),
			opRule("/v1beta1/{name=projects/*/locations/*/models/*/operations/*}"),
			// The Python SDK (google-cloud-aiplatform) polls under /ui,
			// the first binding aiplatform_v1.yaml gives GetOperation
			// (#1940); it is the same operation.
			opRule("/ui/{name=projects/*/locations/*/operations/*}"),
			opRule("/ui/{name=projects/*/locations/*/endpoints/*/operations/*}"),
			opRule("/ui/{name=projects/*/locations/*/models/*/operations/*}"),
		}}
		s.registerModelGarden(t)
		documentaipb.RegisterDocumentProcessorServiceServer(t, &documentAIService{s: s}) // documentai.go, #1799
		documentaibetapb.RegisterDocumentServiceServer(t, &documentService{s: s})        // documentaidataset.go, #2106
		speechpb.RegisterSpeechServer(t, &speechService{s: s})                           // speechrecognizers.go, #1799
		longrunningpb.RegisterOperationsServer(t, &operations{s: s})
		s.rest = t
	})
	return s.rest
}

// documentAIPath reports whether a path is Document AI's (documentai.go),
// served with or without Model Garden.
func documentAIPath(p string) bool {
	return strings.Contains(p, "/processors") || strings.Contains(p, "/processorTypes") ||
		strings.HasSuffix(p, ":fetchProcessorTypes") || strings.Contains(p, "/operations/")
}

// serveREST hands a request the routes above did not take to the transcoder,
// or answers the not-found body when Model Garden is not served.
func (s *Server) serveREST(w http.ResponseWriter, r *http.Request, body []byte) {
	v2 := strings.HasPrefix(r.URL.Path, "/v2/") && (strings.Contains(r.URL.Path, "/recognizers") || strings.Contains(r.URL.Path, "/customClasses") ||
		strings.Contains(r.URL.Path, "/phraseSets") || strings.Contains(r.URL.Path, "/operations/") || strings.HasSuffix(r.URL.Path, "/config"))
	// Document AI's datasets, v1beta3's DocumentService (#2106).
	v2 = v2 || (strings.HasPrefix(r.URL.Path, "/v1beta3/") && strings.Contains(r.URL.Path, "/processors/") && strings.Contains(r.URL.Path, "/dataset"))
	// gcloud ai endpoints deploy-model polls its operation under v1beta1
	// (#1940).
	// The Python SDK polls its operations under /ui (#1940).
	beta := r.Method == http.MethodGet && (strings.HasPrefix(r.URL.Path, "/v1beta1/") || strings.HasPrefix(r.URL.Path, "/ui/")) &&
		strings.Contains(r.URL.Path, "/operations/")
	if !v2 && ((!strings.HasPrefix(r.URL.Path, "/v1/") && !beta) ||
		(s.modelGarden() == nil && s.registry() == nil && !documentAIPath(r.URL.Path))) {
		s.notFound(w, r)
		return
	}
	if body != nil {
		r.Body = io.NopCloser(strings.NewReader(string(body)))
	}
	if r.Header.Get(callerHeader) == callerProcessDocument && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, ":process") {
		// BigQuery ML's ML.PROCESS_DOCUMENT (documentai.go's page limits).
		r = r.WithContext(context.WithValue(r.Context(), processDocumentCaller{}, true))
	}
	s.restTranscoder().ServeHTTP(w, r)
}
