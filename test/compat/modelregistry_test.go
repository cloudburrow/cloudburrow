//go:build compat

package compat

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	aiplatform "cloud.google.com/go/aiplatform/apiv1"
	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/genproto/googleapis/api/httpbody"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/structpb"
)

// modelServerImage is the Model Registry's custom serving container
// fixture (testdata/modelserver): it loads model.json from AIP_STORAGE_URI.
const modelServerImage = "dev.local/cloudburrow-modelserver:test"

// buildModelServer builds the fixture and loads it into the owned cluster.
func buildModelServer(t *testing.T, ctx context.Context) {
	t.Helper()
	for _, bin := range []string{"docker", "kind"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is required to build the model server fixture", bin)
		}
	}
	cluster := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_CLUSTER"))
	if cluster == "" {
		t.Skip("CLOUDBURROW_TEST_CLUSTER is not set; the fixture must be loaded into the owned cluster")
	}
	build := exec.CommandContext(ctx, "docker", "build", "-t", modelServerImage, "testdata/modelserver")
	build.Dir = moduleRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the model server fixture: %v\n%s", err, out)
	}
	if out, err := exec.CommandContext(ctx, "kind", "load", "docker-image", modelServerImage, "--name", cluster).CombinedOutput(); err != nil {
		t.Fatalf("load the model server fixture into %s: %v\n%s", cluster, err, out)
	}
}

func modelRegistryClients(t *testing.T, h *Harness) (*aiplatform.ModelClient, *aiplatform.EndpointClient, *aiplatform.PredictionClient) {
	t.Helper()
	opts := gardenOpts(h, EnvPrediction)
	mc, err := aiplatform.NewModelClient(h.Context(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mc.Close() })
	ec, err := aiplatform.NewEndpointClient(h.Context(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ec.Close() })
	return mc, ec, predictionClientAt(t, h, EnvPrediction)
}

func putArtifact(t *testing.T, ctx context.Context, c *storage.Client, bucket, name, body string) {
	t.Helper()
	w := c.Bucket(bucket).Object(name).NewWriter(ctx)
	if _, err := w.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("write gs://%s/%s: %v", bucket, name, err)
	}
}

// covers: google.cloud.aiplatform.v1.ModelService/UploadModel, google.cloud.aiplatform.v1.ModelService/GetModel, google.cloud.aiplatform.v1.ModelService/ListModels, google.cloud.aiplatform.v1.ModelService/UpdateModel, google.cloud.aiplatform.v1.ModelService/ListModelVersions, google.cloud.aiplatform.v1.ModelService/MergeVersionAliases, google.cloud.aiplatform.v1.ModelService/DeleteModelVersion, google.cloud.aiplatform.v1.ModelService/DeleteModel, google.cloud.aiplatform.v1.ModelService/CopyModel, google.cloud.aiplatform.v1.ModelService/ExportModel, google.cloud.aiplatform.v1.ModelService/ImportModelEvaluation, google.cloud.aiplatform.v1.ModelService/GetModelEvaluation, google.cloud.aiplatform.v1.ModelService/ListModelEvaluations, google.cloud.aiplatform.v1.EndpointService/CreateEndpoint, google.cloud.aiplatform.v1.EndpointService/GetEndpoint, google.cloud.aiplatform.v1.EndpointService/ListEndpoints, google.cloud.aiplatform.v1.EndpointService/UpdateEndpoint, google.cloud.aiplatform.v1.EndpointService/DeployModel, google.cloud.aiplatform.v1.EndpointService/MutateDeployedModel, google.cloud.aiplatform.v1.EndpointService/UndeployModel, google.cloud.aiplatform.v1.EndpointService/DeleteEndpoint, google.cloud.aiplatform.v1.PredictionService/Predict, google.cloud.aiplatform.v1.PredictionService/RawPredict, google.cloud.aiplatform.v1.PredictionService/Explain
//
// TestModelRegistryDeploysACustomContainer (#1940): through the official
// aiplatform Go clients, UploadModel registers a custom serving container
// whose artifactUri is a directory in the instance's Cloud Storage, and a
// second version under it with an alias; versions, aliases (the default
// moved by MergeVersionAliases), UpdateModel, the list filter, CopyModel,
// ExportModel and model evaluations behave as Google documents.
// CreateEndpoint and DeployModel run each version's container in the
// cluster, with AIP_STORAGE_URI naming a copy of that version's artifacts
// (the container loads its weights from it at startup) and the other AIP_*
// variables set; Predict answers with the deployedModelId the traffic
// split chose, RawPredict relays, Explain attributes with the model's
// explanation spec; UpdateEndpoint moves the traffic, MutateDeployedModel
// changes the replica counts, and undeploy and the deletes refuse what
// Google refuses before they succeed.
func TestModelRegistryDeploysACustomContainer(t *testing.T) {
	h := New(t)
	scaleShardEndpoint(h, envScaleRun, EnvRun) // minutes of container starts: the scale shard (#1821)
	shardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	mc, ec, pc := modelRegistryClients(t, h)
	sc := storageClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	buildModelServer(t, ctx)
	parent := runParent(h)

	// The artifacts: two versions of a linear model.
	bkt := h.Project() + "-vertex-artifacts"
	if err := sc.Bucket(bkt).Create(ctx, h.Project(), nil); err != nil && status.Code(err) != codes.AlreadyExists && !strings.Contains(err.Error(), "409") {
		t.Fatalf("create bucket: %v", err)
	}
	putArtifact(t, ctx, sc, bkt, "v1/model.json", `{"weight":3,"bias":1,"name":"linear-v1"}`)
	putArtifact(t, ctx, sc, bkt, "v2/model.json", `{"weight":10,"bias":0,"name":"linear-v2"}`)

	modelID := fmt.Sprintf("compat-linear-%d", time.Now().UnixNano()%1e6)
	spec := &aiplatformpb.ModelContainerSpec{ImageUri: modelServerImage, PredictRoute: "/v1/models/linear:predict",
		HealthRoute: "/v1/models/linear", Ports: []*aiplatformpb.Port{{ContainerPort: 7080}},
		Env: []*aiplatformpb.EnvVar{{Name: "MODEL_FLAVOUR", Value: "linear"}}}
	explain := &aiplatformpb.ExplanationSpec{
		Parameters: &aiplatformpb.ExplanationParameters{Method: &aiplatformpb.ExplanationParameters_SampledShapleyAttribution{
			SampledShapleyAttribution: &aiplatformpb.SampledShapleyAttribution{PathCount: 5}}},
		Metadata: &aiplatformpb.ExplanationMetadata{Inputs: map[string]*aiplatformpb.ExplanationMetadata_InputMetadata{"x": {}},
			Outputs: map[string]*aiplatformpb.ExplanationMetadata_OutputMetadata{"y": {}}}}

	// The AIP_ prefix is reserved.
	_, err := mc.UploadModel(ctx, &aiplatformpb.UploadModelRequest{Parent: parent, Model: &aiplatformpb.Model{DisplayName: "bad",
		ContainerSpec: &aiplatformpb.ModelContainerSpec{ImageUri: modelServerImage, Env: []*aiplatformpb.EnvVar{{Name: "AIP_X", Value: "1"}}}}})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("UploadModel with an AIP_ variable = %v, want INVALID_ARGUMENT", err)
	}

	up, err := mc.UploadModel(ctx, &aiplatformpb.UploadModelRequest{Parent: parent, ModelId: modelID, Model: &aiplatformpb.Model{
		DisplayName: "compat-linear", ArtifactUri: "gs://" + bkt + "/v1", ContainerSpec: spec, ExplanationSpec: explain,
		Labels: map[string]string{"team": "compat"}}})
	if err != nil {
		t.Fatalf("UploadModel: %v", err)
	}
	upr, err := up.Wait(ctx)
	if err != nil {
		t.Fatalf("UploadModel operation: %v", err)
	}
	model := parent + "/models/" + modelID
	if upr.GetModel() != model || upr.GetModelVersionId() != "1" {
		t.Fatalf("UploadModel = %v; want %s version 1", upr, model)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		for it := ec.ListEndpoints(cctx, &aiplatformpb.ListEndpointsRequest{Parent: parent, Filter: `labels.compat="model-registry"`}); ; {
			e, err := it.Next()
			if err != nil {
				break
			}
			if _, err := ec.UpdateEndpoint(cctx, &aiplatformpb.UpdateEndpointRequest{Endpoint: &aiplatformpb.Endpoint{Name: e.GetName()},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"traffic_split"}}}); err != nil {
				t.Logf("cleanup: clear traffic: %v", err)
			}
			for _, d := range e.GetDeployedModels() {
				if op, err := ec.UndeployModel(cctx, &aiplatformpb.UndeployModelRequest{Endpoint: e.GetName(), DeployedModelId: d.GetId()}); err == nil {
					_, _ = op.Wait(cctx)
				}
			}
			if op, err := ec.DeleteEndpoint(cctx, &aiplatformpb.DeleteEndpointRequest{Name: e.GetName()}); err == nil {
				_ = op.Wait(cctx)
			}
		}
		for _, m := range []string{model, model + "-copy"} {
			if op, err := mc.DeleteModel(cctx, &aiplatformpb.DeleteModelRequest{Name: m}); err == nil {
				_ = op.Wait(cctx)
			}
		}
	})

	m, err := mc.GetModel(ctx, &aiplatformpb.GetModelRequest{Name: model})
	if err != nil {
		t.Fatalf("GetModel: %v", err)
	}
	if m.GetVersionId() != "1" || strings.Join(m.GetVersionAliases(), ",") != "default" || m.GetContainerSpec().GetImageUri() != modelServerImage ||
		m.GetArtifactUri() != "gs://"+bkt+"/v1" {
		t.Errorf("GetModel = version %s, aliases %v, image %s, artifactUri %s", m.GetVersionId(), m.GetVersionAliases(),
			m.GetContainerSpec().GetImageUri(), m.GetArtifactUri())
	}

	// Version 2, with an alias.
	up, err = mc.UploadModel(ctx, &aiplatformpb.UploadModelRequest{Parent: parent, ParentModel: model, Model: &aiplatformpb.Model{
		DisplayName: "compat-linear", ArtifactUri: "gs://" + bkt + "/v2", ContainerSpec: spec, VersionAliases: []string{"challenger"},
		VersionDescription: "weight 10", Labels: map[string]string{"team": "compat"}}})
	if err != nil {
		t.Fatalf("UploadModel version 2: %v", err)
	}
	if upr, err = up.Wait(ctx); err != nil || upr.GetModelVersionId() != "2" {
		t.Fatalf("UploadModel version 2 = %v, %v", upr, err)
	}
	if v, err := mc.GetModel(ctx, &aiplatformpb.GetModelRequest{Name: model + "@challenger"}); err != nil || v.GetVersionId() != "2" {
		t.Errorf("GetModel @challenger = %v, %v; want version 2", v.GetVersionId(), err)
	}
	if v, err := mc.GetModel(ctx, &aiplatformpb.GetModelRequest{Name: model}); err != nil || v.GetVersionId() != "1" {
		t.Errorf("GetModel (default) = %v, %v; want version 1", v.GetVersionId(), err)
	}
	var versions []string
	for it := mc.ListModelVersions(ctx, &aiplatformpb.ListModelVersionsRequest{Name: model}); ; {
		v, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListModelVersions: %v", err)
		}
		versions = append(versions, v.GetVersionId())
	}
	if strings.Join(versions, ",") != "1,2" {
		t.Errorf("ListModelVersions = %v, want [1 2]", versions)
	}
	if _, err := mc.MergeVersionAliases(ctx, &aiplatformpb.MergeVersionAliasesRequest{Name: model + "@1", VersionAliases: []string{"challenger"}}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("an alias another version holds = %v, want ALREADY_EXISTS", err)
	}
	if _, err := mc.MergeVersionAliases(ctx, &aiplatformpb.MergeVersionAliasesRequest{Name: model + "@1", VersionAliases: []string{"-default"}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("removing the default alias = %v, want INVALID_ARGUMENT", err)
	}
	mv, err := mc.MergeVersionAliases(ctx, &aiplatformpb.MergeVersionAliasesRequest{Name: model + "@1", VersionAliases: []string{"golden"}})
	if err != nil || !hasAll(mv.GetVersionAliases(), "default", "golden") {
		t.Errorf("MergeVersionAliases golden = %v, %v", mv.GetVersionAliases(), err)
	}
	if _, err := mc.DeleteModelVersion(ctx, &aiplatformpb.DeleteModelVersionRequest{Name: model + "@1"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DeleteModelVersion of the default version = %v, want FAILED_PRECONDITION", err)
	}
	um, err := mc.UpdateModel(ctx, &aiplatformpb.UpdateModelRequest{Model: &aiplatformpb.Model{Name: model, Description: "a linear model"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}})
	if err != nil || um.GetDescription() != "a linear model" {
		t.Errorf("UpdateModel = %v, %v", um.GetDescription(), err)
	}
	found := false
	for it := mc.ListModels(ctx, &aiplatformpb.ListModelsRequest{Parent: parent, Filter: `labels.team="compat"`}); ; {
		x, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListModels: %v", err)
		}
		found = found || x.GetName() == model
	}
	if !found {
		t.Errorf("ListModels labels.team=compat does not list %s", model)
	}

	// Evaluations.
	metrics, _ := structpb.NewValue(map[string]any{"meanAbsoluteError": 0.25})
	ev, err := mc.ImportModelEvaluation(ctx, &aiplatformpb.ImportModelEvaluationRequest{Parent: model,
		ModelEvaluation: &aiplatformpb.ModelEvaluation{DisplayName: "holdout", Metrics: metrics,
			MetricsSchemaUri: "gs://google-cloud-aiplatform/schema/modelevaluation/regression_metrics_1.0.0.yaml"}})
	if err != nil {
		t.Fatalf("ImportModelEvaluation: %v", err)
	}
	if g, err := mc.GetModelEvaluation(ctx, &aiplatformpb.GetModelEvaluationRequest{Name: ev.GetName()}); err != nil || g.GetDisplayName() != "holdout" {
		t.Errorf("GetModelEvaluation = %v, %v", g, err)
	}
	if e, err := mc.ListModelEvaluations(ctx, &aiplatformpb.ListModelEvaluationsRequest{Parent: model}).Next(); err != nil || e.GetName() != ev.GetName() {
		t.Errorf("ListModelEvaluations = %v, %v", e, err)
	}

	// CopyModel and ExportModel copy the artifacts.
	cp, err := mc.CopyModel(ctx, &aiplatformpb.CopyModelRequest{Parent: parent, SourceModel: model + "@2",
		DestinationModel: &aiplatformpb.CopyModelRequest_ModelId{ModelId: modelID + "-copy"}})
	if err != nil {
		t.Fatalf("CopyModel: %v", err)
	}
	if cr, err := cp.Wait(ctx); err != nil || cr.GetModel() != model+"-copy" {
		t.Errorf("CopyModel = %v, %v", cr, err)
	}
	ex, err := mc.ExportModel(ctx, &aiplatformpb.ExportModelRequest{Name: model + "@2", OutputConfig: &aiplatformpb.ExportModelRequest_OutputConfig{
		ExportFormatId: "custom-trained", ArtifactDestination: &aiplatformpb.GcsDestination{OutputUriPrefix: "gs://" + bkt + "/exports"}}})
	if err != nil {
		t.Fatalf("ExportModel: %v", err)
	}
	if _, err := ex.Wait(ctx); err != nil {
		t.Fatalf("ExportModel operation: %v", err)
	}
	md, err := ex.Metadata()
	if err != nil {
		t.Fatal(err)
	}
	out := md.GetOutputInfo().GetArtifactOutputUri()
	obj := strings.TrimPrefix(out, "gs://"+bkt+"/") + "/model.json"
	if r, err := sc.Bucket(bkt).Object(obj).NewReader(ctx); err != nil {
		t.Errorf("the export %s has no model.json: %v", out, err)
	} else {
		_ = r.Close()
	}

	// An endpoint, version 1 deployed on it.
	cop, err := ec.CreateEndpoint(ctx, &aiplatformpb.CreateEndpointRequest{Parent: parent, Endpoint: &aiplatformpb.Endpoint{
		DisplayName: "compat-registry-endpoint", Labels: map[string]string{"compat": "model-registry"}}})
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	ep, err := cop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateEndpoint operation: %v", err)
	}
	endpoint := ep.GetName()
	epID := endpoint[strings.LastIndex(endpoint, "/")+1:]
	resources := &aiplatformpb.DeployedModel_DedicatedResources{DedicatedResources: &aiplatformpb.DedicatedResources{
		MachineSpec: &aiplatformpb.MachineSpec{MachineType: "n1-standard-2"}, MinReplicaCount: 1, MaxReplicaCount: 1}}
	dop, err := ec.DeployModel(ctx, &aiplatformpb.DeployModelRequest{Endpoint: endpoint, DeployedModel: &aiplatformpb.DeployedModel{
		Model: model + "@1", DisplayName: "linear-v1", PredictionResources: resources}, TrafficSplit: map[string]int32{"0": 100}})
	if err != nil {
		t.Fatalf("DeployModel: %v", err)
	}
	dr, err := dop.Wait(ctx)
	if err != nil {
		t.Fatalf("DeployModel operation: %v", err)
	}
	dm1 := dr.GetDeployedModel().GetId()
	if dm1 == "" || dr.GetDeployedModel().GetModelVersionId() != "1" {
		t.Fatalf("DeployModel = %v", dr.GetDeployedModel())
	}
	inst := func(xs ...float64) []*structpb.Value {
		var out []*structpb.Value
		for _, x := range xs {
			out = append(out, structpb.NewNumberValue(x))
		}
		return out
	}
	resp, err := pc.Predict(ctx, &aiplatformpb.PredictRequest{Endpoint: endpoint, Instances: inst(2, 5)})
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	if len(resp.GetPredictions()) != 2 || resp.GetPredictions()[0].GetNumberValue() != 7 || resp.GetPredictions()[1].GetNumberValue() != 16 {
		t.Errorf("Predict = %v; want [7 16], version 1's weights loaded from AIP_STORAGE_URI", resp.GetPredictions())
	}
	if resp.GetDeployedModelId() != dm1 {
		t.Errorf("Predict deployedModelId = %q, want %q", resp.GetDeployedModelId(), dm1)
	}
	envParams, _ := structpb.NewValue(map[string]any{"report": "env"})
	er, err := pc.Predict(ctx, &aiplatformpb.PredictRequest{Endpoint: endpoint, Instances: inst(0), Parameters: envParams})
	if err != nil {
		t.Fatalf("Predict report=env: %v", err)
	}
	env := er.GetPredictions()[0].GetStructValue().AsMap()
	want := map[string]string{"AIP_HTTP_PORT": "7080", "AIP_PREDICT_ROUTE": "/v1/models/linear:predict", "AIP_HEALTH_ROUTE": "/v1/models/linear",
		"AIP_ENDPOINT_ID": epID, "AIP_DEPLOYED_MODEL_ID": dm1, "AIP_MODE": "PREDICTION", "AIP_MODE_VERSION": "1.0.0",
		"AIP_MODEL_NAME": epID, "AIP_VERSION_NAME": dm1, "AIP_MACHINE_TYPE": "n1-standard-2", "AIP_FRAMEWORK": "CUSTOM_CONTAINER"}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("container env %s = %v, want %q", k, env[k], v)
		}
	}
	if su, _ := env["AIP_STORAGE_URI"].(string); !strings.HasPrefix(su, "gs://") || strings.HasPrefix(su, "gs://"+bkt+"/") {
		t.Errorf("AIP_STORAGE_URI = %q; want a copy in another bucket, as Google documents", su)
	}
	raw, err := pc.RawPredict(ctx, &aiplatformpb.RawPredictRequest{Endpoint: endpoint,
		HttpBody: &httpbody.HttpBody{ContentType: "application/json", Data: []byte(`{"instances":[1]}`)}})
	if err != nil || !strings.Contains(string(raw.GetData()), `"predictions":[4]`) {
		t.Errorf("RawPredict = %s, %v", raw.GetData(), err)
	}
	xr, err := pc.Explain(ctx, &aiplatformpb.ExplainRequest{Endpoint: endpoint, Instances: inst(2)})
	if err != nil {
		t.Errorf("Explain with the model's explanation spec: %v", err)
	} else if a := xr.GetExplanations()[0].GetAttributions()[0]; a.GetInstanceOutputValue() != 7 {
		t.Errorf("Explain attribution = %v", a)
	}

	// Version 2 beside it, with all the traffic.
	dop, err = ec.DeployModel(ctx, &aiplatformpb.DeployModelRequest{Endpoint: endpoint, DeployedModel: &aiplatformpb.DeployedModel{
		Model: model + "@challenger", DisplayName: "linear-v2", PredictionResources: resources},
		TrafficSplit: map[string]int32{"0": 100, dm1: 0}})
	if err != nil {
		t.Fatalf("DeployModel version 2: %v", err)
	}
	if dr, err = dop.Wait(ctx); err != nil {
		t.Fatalf("DeployModel version 2 operation: %v", err)
	}
	dm2 := dr.GetDeployedModel().GetId()
	ep, err = ec.GetEndpoint(ctx, &aiplatformpb.GetEndpointRequest{Name: endpoint})
	if err != nil || len(ep.GetDeployedModels()) != 2 || ep.GetTrafficSplit()[dm2] != 100 {
		t.Fatalf("GetEndpoint = %v, %v", ep, err)
	}
	if resp, err := pc.Predict(ctx, &aiplatformpb.PredictRequest{Endpoint: endpoint, Instances: inst(2)}); err != nil ||
		resp.GetPredictions()[0].GetNumberValue() != 20 || resp.GetDeployedModelId() != dm2 {
		t.Errorf("Predict after the split = %v, %v; want 20 from %s", resp, err, dm2)
	}
	if _, err := ec.UndeployModel(ctx, &aiplatformpb.UndeployModelRequest{Endpoint: endpoint, DeployedModelId: dm2}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("undeploying the model with the traffic = %v, want FAILED_PRECONDITION", err)
	}
	if _, err := mc.DeleteModel(ctx, &aiplatformpb.DeleteModelRequest{Name: model}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DeleteModel of a deployed model = %v, want FAILED_PRECONDITION", err)
	}
	if _, err := ec.DeleteEndpoint(ctx, &aiplatformpb.DeleteEndpointRequest{Name: endpoint}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DeleteEndpoint with deployed models = %v, want FAILED_PRECONDITION", err)
	}
	if _, err := ec.UpdateEndpoint(ctx, &aiplatformpb.UpdateEndpointRequest{Endpoint: &aiplatformpb.Endpoint{Name: endpoint,
		TrafficSplit: map[string]int32{dm1: 50, dm2: 60}}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"traffic_split"}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a split adding up to 110 = %v, want INVALID_ARGUMENT", err)
	}
	ue, err := ec.UpdateEndpoint(ctx, &aiplatformpb.UpdateEndpointRequest{Endpoint: &aiplatformpb.Endpoint{Name: endpoint,
		TrafficSplit: map[string]int32{dm1: 100}, DisplayName: "compat-registry-endpoint-2"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"traffic_split", "display_name"}}})
	if err != nil || ue.GetTrafficSplit()[dm1] != 100 || ue.GetDisplayName() != "compat-registry-endpoint-2" {
		t.Fatalf("UpdateEndpoint = %v, %v", ue, err)
	}
	if resp, err := pc.Predict(ctx, &aiplatformpb.PredictRequest{Endpoint: endpoint, Instances: inst(2)}); err != nil || resp.GetDeployedModelId() != dm1 {
		t.Errorf("Predict after UpdateEndpoint = %v, %v; want %s", resp, err, dm1)
	}
	mop, err := ec.MutateDeployedModel(ctx, &aiplatformpb.MutateDeployedModelRequest{Endpoint: endpoint,
		DeployedModel: &aiplatformpb.DeployedModel{Id: dm2, PredictionResources: &aiplatformpb.DeployedModel_DedicatedResources{
			DedicatedResources: &aiplatformpb.DedicatedResources{MaxReplicaCount: 3}}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"dedicated_resources.max_replica_count"}}})
	if err != nil {
		t.Fatalf("MutateDeployedModel: %v", err)
	}
	if mr, err := mop.Wait(ctx); err != nil || mr.GetDeployedModel().GetDedicatedResources().GetMaxReplicaCount() != 3 {
		t.Errorf("MutateDeployedModel = %v, %v", mr, err)
	}

	// Undeploy both, then delete.
	for _, id := range []string{dm2, dm1} {
		op, err := ec.UndeployModel(ctx, &aiplatformpb.UndeployModelRequest{Endpoint: endpoint, DeployedModelId: id})
		if err != nil {
			t.Fatalf("UndeployModel %s: %v", id, err)
		}
		if _, err := op.Wait(ctx); err != nil {
			t.Fatalf("UndeployModel %s operation: %v", id, err)
		}
	}
	if ep, err := ec.GetEndpoint(ctx, &aiplatformpb.GetEndpointRequest{Name: endpoint}); err != nil || len(ep.GetDeployedModels()) != 0 || len(ep.GetTrafficSplit()) != 0 {
		t.Errorf("GetEndpoint after undeploying = %v, %v", ep, err)
	}
	if _, err := pc.Predict(ctx, &aiplatformpb.PredictRequest{Endpoint: endpoint, Instances: inst(1)}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Predict with nothing deployed = %v, want FAILED_PRECONDITION", err)
	}
	dl, err := ec.DeleteEndpoint(ctx, &aiplatformpb.DeleteEndpointRequest{Name: endpoint})
	if err != nil {
		t.Fatalf("DeleteEndpoint: %v", err)
	}
	if err := dl.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := ec.GetEndpoint(ctx, &aiplatformpb.GetEndpointRequest{Name: endpoint}); status.Code(err) != codes.NotFound {
		t.Errorf("GetEndpoint after delete = %v, want NOT_FOUND", err)
	}
	dv, err := mc.MergeVersionAliases(ctx, &aiplatformpb.MergeVersionAliasesRequest{Name: model + "@2", VersionAliases: []string{"default"}})
	if err != nil || !hasAll(dv.GetVersionAliases(), "default", "challenger") {
		t.Fatalf("moving default to version 2 = %v, %v", dv.GetVersionAliases(), err)
	}
	dvo, err := mc.DeleteModelVersion(ctx, &aiplatformpb.DeleteModelVersionRequest{Name: model + "@1"})
	if err != nil {
		t.Fatalf("DeleteModelVersion: %v", err)
	}
	if err := dvo.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{model, model + "-copy"} {
		op, err := mc.DeleteModel(ctx, &aiplatformpb.DeleteModelRequest{Name: name})
		if err != nil {
			t.Fatalf("DeleteModel %s: %v", name, err)
		}
		if err := op.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := mc.GetModel(ctx, &aiplatformpb.GetModelRequest{Name: model}); status.Code(err) != codes.NotFound {
		t.Errorf("GetModel after delete = %v, want NOT_FOUND", err)
	}
}

func hasAll(list []string, want ...string) bool {
	set := map[string]bool{}
	for _, x := range list {
		set[x] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

// covers: google.cloud.aiplatform.v1.ModelGardenService/GetPublisherModel
//
// TestGetPublisherModelDescribesTheServedModels (#1940): the official
// ModelGardenClient reads each publisher model this instance serves at
// publishers/google/models/{model} (no model need be downloaded: it is the
// catalogue's description); a model it does not serve, gemini-2.0-flash,
// is NOT_FOUND, as Predict on it is, and a malformed name INVALID_ARGUMENT.
func TestGetPublisherModelDescribesTheServedModels(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvPrediction)
	c, err := aiplatform.NewModelGardenClient(h.Context(), gardenOpts(h, EnvPrediction)...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	for _, id := range []string{"embeddinggemma-300m-onnx-community", "gemma-4-e2b-it-community"} {
		m, err := c.GetPublisherModel(h.Context(), &aiplatformpb.GetPublisherModelRequest{Name: "publishers/google/models/" + id})
		if err != nil {
			t.Fatalf("GetPublisherModel %s: %v", id, err)
		}
		if m.GetName() != "publishers/google/models/"+id || m.GetVersionId() == "" ||
			m.GetOpenSourceCategory() == aiplatformpb.PublisherModel_OPEN_SOURCE_CATEGORY_UNSPECIFIED {
			t.Errorf("GetPublisherModel %s = %v", id, m)
		}
	}
	if _, err := c.GetPublisherModel(h.Context(), &aiplatformpb.GetPublisherModelRequest{Name: "publishers/google/models/gemini-2.0-flash"}); status.Code(err) != codes.NotFound {
		t.Errorf("an unserved model: %v, want NOT_FOUND", err)
	}
	if _, err := c.GetPublisherModel(h.Context(), &aiplatformpb.GetPublisherModelRequest{Name: "models/x"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a malformed name: %v, want INVALID_ARGUMENT", err)
	}
}
