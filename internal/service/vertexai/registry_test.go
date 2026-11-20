package vertexai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/structpb"
)

type fakeRunner struct {
	mu      sync.Mutex
	runs    map[string]ContainerRun
	removed []string
}

func (f *fakeRunner) Run(_ context.Context, _ string, spec ContainerRun) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.runs == nil {
		f.runs = map[string]ContainerRun{}
	}
	f.runs[spec.ID] = spec
	return "http://" + spec.ID + ".example", nil
}

func (f *fakeRunner) Remove(_ context.Context, _, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, id)
	delete(f.runs, id)
	return nil
}

type fakeArtifacts struct{ copies [][2]string }

func (f *fakeArtifacts) Copy(_ context.Context, _, src, dst string) error {
	f.copies = append(f.copies, [2]string{src, dst})
	return nil
}
func (f *fakeArtifacts) ManagedBucket(p string) string { return "managed-" + p }

func envOf(run ContainerRun) map[string]string {
	m := map[string]string{}
	for _, e := range run.Env {
		m[e.Name] = e.Value
	}
	return m
}

func TestRegistryUploadDeployPredict(t *testing.T) {
	// The "cluster": answers each deployed model's predict route, by Host.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req struct{ Instances []json.RawMessage }
		_ = json.Unmarshal(b, &req)
		preds := make([]string, len(req.Instances))
		for i := range preds {
			preds[i] = `"` + strings.SplitN(r.Host, ".", 2)[0] + `"`
		}
		_, _ = io.WriteString(w, `{"predictions":[`+strings.Join(preds, ",")+`]}`)
	}))
	defer backend.Close()
	s := NewServer(nil)
	s.SetEndpoints(nil, func() string { return strings.TrimPrefix(backend.URL, "http://") }, nil)
	run, art := &fakeRunner{}, &fakeArtifacts{}
	state := filepath.Join(t.TempDir(), "registry.json")
	s.SetRegistry(state, run, art)
	ctx := context.Background()
	ms, es, ps := &modelService{s: s}, &endpointService{s: s}, &predictionService{s: s}
	parent := "projects/p/locations/us-central1"

	spec := &aiplatformpb.ModelContainerSpec{ImageUri: "dev.local/m:1", PredictRoute: "/predict", HealthRoute: "/health",
		Ports: []*aiplatformpb.Port{{ContainerPort: 9000}}, Env: []*aiplatformpb.EnvVar{{Name: "X", Value: "1"}}}
	if _, err := ms.UploadModel(ctx, &aiplatformpb.UploadModelRequest{Parent: parent, Model: &aiplatformpb.Model{DisplayName: "m",
		ContainerSpec: &aiplatformpb.ModelContainerSpec{ImageUri: "x", Env: []*aiplatformpb.EnvVar{{Name: "AIP_X", Value: "1"}}}}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("AIP_ env = %v, want INVALID_ARGUMENT", err)
	}
	op, err := ms.UploadModel(ctx, &aiplatformpb.UploadModelRequest{Parent: parent, ModelId: "mymodel",
		Model: &aiplatformpb.Model{DisplayName: "m", ArtifactUri: "gs://b/model", ContainerSpec: spec}})
	if err != nil || !op.GetDone() {
		t.Fatalf("UploadModel = %v, %v", op, err)
	}
	if len(art.copies) != 1 || art.copies[0][1] != "gs://managed-p/models/us-central1/mymodel/1" {
		t.Fatalf("copies = %v", art.copies)
	}
	// A second version, with an alias, and default moved by alias merge.
	if _, err := ms.UploadModel(ctx, &aiplatformpb.UploadModelRequest{Parent: parent, ParentModel: parent + "/models/mymodel",
		Model: &aiplatformpb.Model{DisplayName: "m", ContainerSpec: spec, VersionAliases: []string{"golden"}}}); err != nil {
		t.Fatal(err)
	}
	v, err := ms.GetModel(ctx, &aiplatformpb.GetModelRequest{Name: parent + "/models/mymodel@golden"})
	if err != nil || v.GetVersionId() != "2" {
		t.Fatalf("GetModel@golden = %v, %v", v, err)
	}
	if v, _ := ms.GetModel(ctx, &aiplatformpb.GetModelRequest{Name: parent + "/models/mymodel"}); v.GetVersionId() != "1" {
		t.Fatalf("default version = %s", v.GetVersionId())
	}
	if _, err := ms.MergeVersionAliases(ctx, &aiplatformpb.MergeVersionAliasesRequest{Name: parent + "/models/mymodel@1",
		VersionAliases: []string{"-default"}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("removing default = %v", err)
	}
	if _, err := ms.MergeVersionAliases(ctx, &aiplatformpb.MergeVersionAliasesRequest{Name: parent + "/models/mymodel@2",
		VersionAliases: []string{"default"}}); err != nil {
		t.Fatal(err)
	}
	vs, _ := ms.ListModelVersions(ctx, &aiplatformpb.ListModelVersionsRequest{Name: parent + "/models/mymodel"})
	if len(vs.GetModels()) != 2 || len(vs.GetModels()[0].GetVersionAliases()) != 0 {
		t.Fatalf("versions = %v", vs)
	}
	if _, err := ms.UpdateModel(ctx, &aiplatformpb.UpdateModelRequest{Model: &aiplatformpb.Model{Name: parent + "/models/mymodel", Description: "d"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}}); err != nil {
		t.Fatal(err)
	}

	eop, err := es.CreateEndpoint(ctx, &aiplatformpb.CreateEndpointRequest{Parent: parent, EndpointId: "ep1", Endpoint: &aiplatformpb.Endpoint{DisplayName: "e"}})
	if err != nil || !eop.GetDone() {
		t.Fatal(err)
	}
	ep := parent + "/endpoints/ep1"
	deploy := func(id, model string, split map[string]int32) {
		t.Helper()
		op, err := es.DeployModel(ctx, &aiplatformpb.DeployModelRequest{Endpoint: ep, TrafficSplit: split, DeployedModel: &aiplatformpb.DeployedModel{
			Id: id, Model: model, PredictionResources: &aiplatformpb.DeployedModel_DedicatedResources{DedicatedResources: &aiplatformpb.DedicatedResources{
				MachineSpec: &aiplatformpb.MachineSpec{MachineType: "n1-standard-2"}, MinReplicaCount: 1}}}})
		if err != nil {
			t.Fatal(err)
		}
		waitOp(t, s, op.GetName())
	}
	deploy("111", parent+"/models/mymodel@1", nil)
	got, _ := es.GetEndpoint(ctx, &aiplatformpb.GetEndpointRequest{Name: ep})
	if got.GetTrafficSplit()["111"] != 100 {
		t.Fatalf("traffic = %v", got.GetTrafficSplit())
	}
	env := envOf(run.runs[workloadID(ep, "111")])
	if env["AIP_STORAGE_URI"] != "gs://managed-p/models/us-central1/mymodel/1" || env["AIP_HTTP_PORT"] != "9000" ||
		env["AIP_ENDPOINT_ID"] != "ep1" || env["AIP_DEPLOYED_MODEL_ID"] != "111" || env["X"] != "1" || env["AIP_PREDICT_ROUTE"] != "/predict" {
		t.Fatalf("env = %v", env)
	}
	deploy("222", parent+"/models/mymodel@2", map[string]int32{"111": 0, "0": 100})
	got, _ = es.GetEndpoint(ctx, &aiplatformpb.GetEndpointRequest{Name: ep})
	if len(got.GetDeployedModels()) != 2 || got.GetTrafficSplit()["222"] != 100 || got.GetTrafficSplit()["111"] != 0 {
		t.Fatalf("endpoint = %v", got)
	}
	resp, err := ps.Predict(ctx, &aiplatformpb.PredictRequest{Endpoint: ep, Instances: []*structpb.Value{structpb.NewNumberValue(1)}})
	if err != nil || resp.GetDeployedModelId() != "222" || resp.GetPredictions()[0].GetStringValue() != workloadID(ep, "222") {
		t.Fatalf("Predict = %v, %v", resp, err)
	}
	// Model and version deletes refused while deployed.
	if _, err := ms.DeleteModel(ctx, &aiplatformpb.DeleteModelRequest{Name: parent + "/models/mymodel"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("DeleteModel deployed = %v", err)
	}
	if _, err := es.UndeployModel(ctx, &aiplatformpb.UndeployModelRequest{Endpoint: ep, DeployedModelId: "222"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("undeploy with traffic = %v", err)
	}
	if _, err := es.UpdateEndpoint(ctx, &aiplatformpb.UpdateEndpointRequest{Endpoint: &aiplatformpb.Endpoint{Name: ep,
		TrafficSplit: map[string]int32{"111": 100}}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"traffic_split"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := es.UndeployModel(ctx, &aiplatformpb.UndeployModelRequest{Endpoint: ep, DeployedModelId: "222"}); err != nil {
		t.Fatal(err)
	}
	if _, err := es.DeleteEndpoint(ctx, &aiplatformpb.DeleteEndpointRequest{Name: ep}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("delete with deployed = %v", err)
	}

	// State survives a restart.
	s2 := NewServer(nil)
	s2.SetRegistry(state, run, art)
	got2, err := (&endpointService{s: s2}).GetEndpoint(ctx, &aiplatformpb.GetEndpointRequest{Name: ep})
	if err != nil || len(got2.GetDeployedModels()) != 1 {
		t.Fatalf("after restart = %v, %v", got2, err)
	}
	if _, err := es.UndeployModel(ctx, &aiplatformpb.UndeployModelRequest{Endpoint: ep, DeployedModelId: "111"}); err != nil {
		t.Fatal(err)
	}
	if len(run.runs) != 0 {
		t.Fatalf("workloads left: %v", run.runs)
	}
	if _, err := ms.DeleteModelVersion(ctx, &aiplatformpb.DeleteModelVersionRequest{Name: parent + "/models/mymodel@2"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("delete default version = %v", err)
	}
	if _, err := ms.DeleteModelVersion(ctx, &aiplatformpb.DeleteModelVersionRequest{Name: parent + "/models/mymodel@1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.DeleteModel(ctx, &aiplatformpb.DeleteModelRequest{Name: parent + "/models/mymodel"}); err != nil {
		t.Fatal(err)
	}
}

func waitOp(t *testing.T, s *Server, name string) {
	t.Helper()
	op, err := (&operations{s: s}).WaitOperation(context.Background(), &longrunningpb.WaitOperationRequest{Name: name})
	if err != nil || !op.GetDone() || op.GetError() != nil {
		t.Fatalf("operation %s = %v, %v", name, op, err)
	}
}
