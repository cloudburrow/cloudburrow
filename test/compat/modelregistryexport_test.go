//go:build compat

package compat

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	aiplatform "cloud.google.com/go/aiplatform/apiv1"
	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

// exportModel runs ExportModel and returns its operation's output info.
func exportModel(ctx context.Context, mc *aiplatform.ModelClient, name string, oc *aiplatformpb.ExportModelRequest_OutputConfig) (*aiplatformpb.ExportModelOperationMetadata_OutputInfo, error) {
	op, err := mc.ExportModel(ctx, &aiplatformpb.ExportModelRequest{Name: name, OutputConfig: oc})
	if err != nil {
		return nil, err
	}
	if _, err := op.Wait(ctx); err != nil {
		return nil, err
	}
	md, err := op.Metadata()
	if err != nil {
		return nil, err
	}
	return md.GetOutputInfo(), nil
}

// covers: google.cloud.aiplatform.v1.ModelService/ExportModel, google.cloud.aiplatform.v1.ModelService/UploadModel, google.cloud.aiplatform.v1.ModelService/GetModel, google.cloud.aiplatform.v1.EndpointService/DeployModel, google.cloud.aiplatform.v1.PredictionService/Predict
//
// TestModelRegistryExportsTheImageToArtifactRegistry (#1940): through the
// official aiplatform and artifactregistry Go clients, ExportModel's
// output_config.image_destination copies a model version's serving image
// into this instance's Artifact Registry, as Google documents for the
// custom-trained format: an image the registry holds (pushed with crane)
// is copied by digest to another repository under the destination's name
// and tag ("latest" when it has none), and a locally built dev.local image
// is pushed from Docker. image_output_uri names the image. A model with no
// artifacts lists [IMAGE] as its exportable contents and refuses an
// artifact export; a gcr.io destination, a missing repository and a
// non-Artifact Registry path are refused. Google's prebuilt prediction
// containers map onto this registry: a model whose image is
// us-docker.pkg.dev/vertex-ai/prediction/... is refused at DeployModel
// while the local registry does not hold it, and, once an image is
// exported there, is deployed from it, pulled through the nodes' registry
// mirror for the us multi-region, and answers Predict; nothing is pulled
// from Google.
func TestModelRegistryExportsTheImageToArtifactRegistry(t *testing.T) {
	h := New(t)
	scaleShardEndpoint(h, envScaleRun, EnvRun) // a container start: the scale shard (#1821)
	shardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	mc, ec, pc := modelRegistryClients(t, h)
	ar, _ := arClient(t, h)
	sc := storageClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	parent := runParent(h)
	p := h.Project()
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)

	src, dst := "export-src-"+suffix, "export-dst-"+suffix
	for _, id := range []string{src, dst} {
		formatRepo(t, ctx, ar, parent, id, &arpb.Repository{Format: arpb.Repository_DOCKER})
	}
	_, port, _ := net.SplitHostPort(h.Endpoint(EnvRun))
	opts := registryOptions(h)
	local := func(repo, image string) string {
		return "us-central1-docker.pkg.localhost:" + port + "/" + p + "/" + repo + "/" + image
	}
	img, _ := random.Image(512, 2)
	if err := crane.Push(img, local(src, "server")+":v1", opts...); err != nil {
		t.Fatalf("push the serving image: %v", err)
	}
	want, _ := img.Digest()
	host := "us-central1-docker.pkg.dev/" + p + "/"

	upload := func(id, image, artifacts string) string {
		t.Helper()
		op, err := mc.UploadModel(ctx, &aiplatformpb.UploadModelRequest{Parent: parent, ModelId: id, Model: &aiplatformpb.Model{
			DisplayName: id, ArtifactUri: artifacts, ContainerSpec: &aiplatformpb.ModelContainerSpec{ImageUri: image,
				PredictRoute: "/v1/models/linear:predict", HealthRoute: "/v1/models/linear", Ports: []*aiplatformpb.Port{{ContainerPort: 7080}}}}})
		if err != nil {
			t.Fatalf("UploadModel %s: %v", id, err)
		}
		resp, err := op.Wait(ctx)
		if err != nil {
			t.Fatalf("UploadModel %s .Wait: %v", id, err)
		}
		t.Cleanup(func() {
			if op, err := mc.DeleteModel(context.Background(), &aiplatformpb.DeleteModelRequest{Name: resp.GetModel()}); err == nil {
				_ = op.Wait(context.Background())
			}
		})
		return resp.GetModel()
	}

	// An image the registry holds, to another repository, with no tag.
	bare := upload("export-image-"+suffix, host+src+"/server:v1", "")
	m, err := mc.GetModel(ctx, &aiplatformpb.GetModelRequest{Name: bare})
	if err != nil {
		t.Fatal(err)
	}
	if f := m.GetSupportedExportFormats(); len(f) != 1 || f[0].GetId() != "custom-trained" ||
		fmt.Sprint(f[0].GetExportableContents()) != "[IMAGE]" {
		t.Errorf("supported_export_formats = %v; want custom-trained [IMAGE]", f)
	}
	info, err := exportModel(ctx, mc, bare, &aiplatformpb.ExportModelRequest_OutputConfig{ExportFormatId: "custom-trained",
		ImageDestination: &aiplatformpb.ContainerRegistryDestination{OutputUri: host + dst + "/exported/model"}})
	if err != nil {
		t.Fatalf("ExportModel image_destination: %v", err)
	}
	if info.GetImageOutputUri() != host+dst+"/exported/model:latest" || info.GetArtifactOutputUri() != "" {
		t.Errorf("output_info = %v; want image_output_uri %s:latest", info, host+dst+"/exported/model")
	}
	if d, err := crane.Digest(local(dst, "exported/model")+":latest", opts...); err != nil || d != want.String() {
		t.Errorf("the exported image's digest = %s, %v; want %s", d, err, want)
	}
	if _, err := ar.GetDockerImage(ctx, &arpb.GetDockerImageRequest{
		Name: parent + "/repositories/" + dst + "/dockerImages/exported%2Fmodel@" + want.String()}); err != nil {
		t.Errorf("GetDockerImage of the export: %v", err)
	}
	for _, c := range []struct {
		oc   *aiplatformpb.ExportModelRequest_OutputConfig
		code codes.Code
	}{
		{&aiplatformpb.ExportModelRequest_OutputConfig{ExportFormatId: "custom-trained",
			ArtifactDestination: &aiplatformpb.GcsDestination{OutputUriPrefix: "gs://" + p + "-exports"}}, codes.FailedPrecondition},
		{&aiplatformpb.ExportModelRequest_OutputConfig{ExportFormatId: "custom-trained",
			ImageDestination: &aiplatformpb.ContainerRegistryDestination{OutputUri: "gcr.io/" + p + "/model:1"}}, codes.Unimplemented},
		{&aiplatformpb.ExportModelRequest_OutputConfig{ExportFormatId: "custom-trained",
			ImageDestination: &aiplatformpb.ContainerRegistryDestination{OutputUri: host + "no-such-repo/model:1"}}, codes.NotFound},
		{&aiplatformpb.ExportModelRequest_OutputConfig{ExportFormatId: "custom-trained",
			ImageDestination: &aiplatformpb.ContainerRegistryDestination{OutputUri: "docker.io/library/model:1"}}, codes.InvalidArgument},
		{&aiplatformpb.ExportModelRequest_OutputConfig{ExportFormatId: "tf-saved-model",
			ImageDestination: &aiplatformpb.ContainerRegistryDestination{OutputUri: host + dst + "/m:1"}}, codes.InvalidArgument},
	} {
		if _, err := exportModel(ctx, mc, bare, c.oc); status.Code(err) != c.code {
			t.Errorf("ExportModel %v = %v; want %v", c.oc, err, c.code)
		}
	}

	// A locally built image, with artifacts, to Google's prebuilt path.
	buildModelServer(t, ctx)
	bkt := p + "-vertex-export"
	if err := sc.Bucket(bkt).Create(ctx, p, nil); err != nil && status.Code(err) != codes.AlreadyExists && !strings.Contains(err.Error(), "409") {
		t.Fatalf("create bucket: %v", err)
	}
	putArtifact(t, ctx, sc, bkt, "linear/model.json", `{"weight":4,"bias":2,"name":"prebuilt-path"}`)
	withArt := upload("export-local-"+suffix, modelServerImage, "gs://"+bkt+"/linear")

	prebuilt := "us-docker.pkg.dev/vertex-ai/prediction/sklearn-cpu.1-6:latest"
	pre := upload("prebuilt-"+suffix, prebuilt, "gs://"+bkt+"/linear")
	epOp, err := ec.CreateEndpoint(ctx, &aiplatformpb.CreateEndpointRequest{Parent: parent, Endpoint: &aiplatformpb.Endpoint{DisplayName: "prebuilt"}})
	if err != nil {
		t.Fatal(err)
	}
	ep, err := epOp.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := ec.UndeployModel(context.Background(), &aiplatformpb.UndeployModelRequest{Endpoint: ep.GetName(),
			DeployedModelId: "1940"}); err == nil {
			_, _ = op.Wait(context.Background())
		}
		if op, err := ec.DeleteEndpoint(context.Background(), &aiplatformpb.DeleteEndpointRequest{Name: ep.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	deploy := func() error {
		op, err := ec.DeployModel(ctx, &aiplatformpb.DeployModelRequest{Endpoint: ep.GetName(), DeployedModel: &aiplatformpb.DeployedModel{
			Id: "1940", Model: pre, PredictionResources: &aiplatformpb.DeployedModel_DedicatedResources{DedicatedResources: &aiplatformpb.DedicatedResources{
				MachineSpec: &aiplatformpb.MachineSpec{MachineType: "n1-standard-2"}, MinReplicaCount: 1, MaxReplicaCount: 1}}},
			TrafficSplit: map[string]int32{"0": 100}})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	}
	if err := deploy(); status.Code(err) != codes.FailedPrecondition || !strings.Contains(status.Convert(err).Message(), "prebuilt") {
		t.Fatalf("DeployModel of a prebuilt container the registry does not hold = %v; want FAILED_PRECONDITION", err)
	}

	formatRepo(t, ctx, ar, "projects/vertex-ai/locations/us", "prediction", &arpb.Repository{Format: arpb.Repository_DOCKER})
	info, err = exportModel(ctx, mc, withArt, &aiplatformpb.ExportModelRequest_OutputConfig{ExportFormatId: "custom-trained",
		ArtifactDestination: &aiplatformpb.GcsDestination{OutputUriPrefix: "gs://" + bkt + "/exports"},
		ImageDestination:    &aiplatformpb.ContainerRegistryDestination{OutputUri: prebuilt}})
	if err != nil {
		t.Fatalf("ExportModel of a dev.local image with its artifacts: %v", err)
	}
	if info.GetImageOutputUri() != prebuilt || !strings.HasPrefix(info.GetArtifactOutputUri(), "gs://"+bkt+"/exports/model-export-local-"+suffix+"/custom-trained/") {
		t.Errorf("output_info = %v", info)
	}
	if err := deploy(); err != nil {
		t.Fatalf("DeployModel of the prebuilt path the registry now holds: %v", err)
	}
	resp, err := pc.Predict(ctx, &aiplatformpb.PredictRequest{Endpoint: ep.GetName(),
		Instances: []*structpb.Value{structpb.NewNumberValue(3)}})
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	if got := resp.GetPredictions(); len(got) != 1 || got[0].GetNumberValue() != 14 || resp.GetDeployedModelId() != "1940" {
		t.Errorf("Predict = %v (deployed model %s); want [14] from 1940", got, resp.GetDeployedModelId())
	}
	if resp.GetModel() != pre || resp.GetModelVersionId() != "1" || resp.GetModelDisplayName() != "prebuilt-"+suffix {
		t.Errorf("Predict's model = %s@%s (%s); want %s@1", resp.GetModel(), resp.GetModelVersionId(), resp.GetModelDisplayName(), pre)
	}
}
