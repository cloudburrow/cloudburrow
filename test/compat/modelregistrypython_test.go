//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// pushModelServer pushes the model server fixture, built by
// buildModelServer, from Docker to a new DOCKER repository of the
// instance's Artifact Registry, as a user pushes a serving image, and
// returns its Artifact Registry URI; deployed models pull it through the
// nodes' registry mirror.
func pushModelServer(t *testing.T, ctx context.Context, h *Harness) string {
	t.Helper()
	ar, _ := arClient(t, h)
	repo := fmt.Sprintf("vertex-serving-%d", time.Now().UnixNano()%1e6)
	formatRepo(t, ctx, ar, runParent(h), repo, &arpb.Repository{Format: arpb.Repository_DOCKER})
	tarPath := filepath.Join(t.TempDir(), "modelserver.tar")
	if out, err := exec.CommandContext(ctx, "docker", "save", "-o", tarPath, modelServerImage).CombinedOutput(); err != nil {
		t.Fatalf("docker save %s: %v\n%s", modelServerImage, err, out)
	}
	tag, err := name.NewTag(modelServerImage)
	if err != nil {
		t.Fatal(err)
	}
	img, err := tarball.ImageFromPath(tarPath, &tag)
	if err != nil {
		t.Fatalf("read the saved model server: %v", err)
	}
	_, port, _ := net.SplitHostPort(h.Endpoint(EnvRun))
	if err := crane.Push(img, "us-central1-docker.pkg.localhost:"+port+"/"+h.Project()+"/"+repo+"/modelserver:test", registryOptions(h)...); err != nil {
		t.Fatalf("push the model server: %v", err)
	}
	return "us-central1-docker.pkg.dev/" + h.Project() + "/" + repo + "/modelserver:test"
}

// aiplatformPyImage is the Python Vertex AI SDK fixture
// (testdata/aiplatformpy), google-cloud-aiplatform pinned.
const aiplatformPyImage = "dev.local/cloudburrow-aiplatformpy:test"

// internalNetwork makes a Docker network with no route out (--internal)
// on which host.docker.internal is a relay to the host's port alone, and
// returns its name. A container on it reaches that port and nothing else:
// no other host port, no internet and no Google.
func internalNetwork(t *testing.T, ctx context.Context, relayImage, port string) string {
	t.Helper()
	arch, err := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Arch}}").Output()
	if err != nil {
		t.Fatalf("docker version: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(forwarderSource), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module relay\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	build := exec.CommandContext(ctx, "go", "build", "-o", "relay", ".")
	build.Dir = dir
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+strings.TrimSpace(string(arch)), "CGO_ENABLED=0", "GOFLAGS=")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the relay: %v\n%s", err, b)
	}
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e9)
	network, relay := "cb-net-"+suffix, "cb-relay-"+suffix
	if b, err := exec.CommandContext(ctx, "docker", "network", "create", "--internal", network).CombinedOutput(); err != nil {
		t.Fatalf("create the internal network: %v\n%s", err, b)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", network).Run() })
	args := []string{"run", "-d", "--name", relay, "-v", dir + ":/relay:ro", "--entrypoint", "/relay/relay"}
	if runtime.GOOS == "linux" {
		args = append(args, "--add-host", "host.docker.internal:host-gateway")
	}
	args = append(args, relayImage, ":"+port, "host.docker.internal:"+port)
	if b, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("start the relay: %v\n%s", err, b)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "-v", relay).Run() })
	for deadline := time.Now().Add(30 * time.Second); ; {
		logs, _ := exec.CommandContext(ctx, "docker", "logs", relay).CombinedOutput()
		if strings.Contains(string(logs), "ready") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the relay did not start:\n%s", logs)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if b, err := exec.CommandContext(ctx, "docker", "network", "connect", "--alias", "host.docker.internal", network, relay).CombinedOutput(); err != nil {
		t.Fatalf("connect the relay: %v\n%s", err, b)
	}
	return network
}

// covers: google.cloud.aiplatform.v1.ModelService/UploadModel, google.cloud.aiplatform.v1.ModelService/GetModel, google.cloud.aiplatform.v1.ModelService/ListModels, google.cloud.aiplatform.v1.ModelService/ListModelVersions, google.cloud.aiplatform.v1.ModelService/DeleteModelVersion, google.cloud.aiplatform.v1.ModelService/DeleteModel, google.cloud.aiplatform.v1.EndpointService/CreateEndpoint, google.cloud.aiplatform.v1.EndpointService/GetEndpoint, google.cloud.aiplatform.v1.EndpointService/ListEndpoints, google.cloud.aiplatform.v1.EndpointService/DeployModel, google.cloud.aiplatform.v1.EndpointService/UndeployModel, google.cloud.aiplatform.v1.EndpointService/DeleteEndpoint, google.cloud.aiplatform.v1.PredictionService/Predict
//
// TestPythonAIPlatformUploadAndDeploy (#1940): the Python Vertex AI SDK
// (google-cloud-aiplatform, testdata/aiplatformpy) runs Google's
// documented flow, aiplatform.Model.upload(...) of a custom serving
// container pushed to the instance's Artifact Registry, with an
// artifact_uri in the instance's Cloud Storage, a second
// version with an alias, then model.deploy(...), which creates the
// endpoint, deploys the model and waits on the operations, and
// endpoint.predict(...); then undeploys and deletes everything. The SDK
// runs in a container on an internal Docker network whose one route is a
// relay to the instance's Vertex AI port, over REST, with the instance's
// ADC fixture in the cloudburrow.localhost universe (docs/security.md), so
// nothing it does can reach Google (#1981).
func TestPythonAIPlatformUploadAndDeploy(t *testing.T) {
	h := New(t)
	scaleShardEndpoint(h, envScaleRun, EnvRun) // a container start: the scale shard (#1821)
	shardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	endpoint := h.Endpoint(EnvPrediction)
	creds := os.Getenv("CLOUDBURROW_TEST_CREDENTIALS")
	if creds == "" {
		t.Skip("CLOUDBURROW_TEST_CREDENTIALS is not set: the SDK needs the instance's ADC fixture")
	}
	sc := storageClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	buildModelServer(t, ctx)
	image := pushModelServer(t, ctx, h)
	build := exec.CommandContext(ctx, "docker", "build", "-t", aiplatformPyImage, "testdata/aiplatformpy")
	build.Dir = moduleRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the Python SDK fixture: %v\n%s", err, out)
	}

	bkt := h.Project() + "-vertex-python"
	if err := sc.Bucket(bkt).Create(ctx, h.Project(), nil); err != nil && status.Code(err) != codes.AlreadyExists && !strings.Contains(err.Error(), "409") {
		t.Fatalf("create bucket: %v", err)
	}
	putArtifact(t, ctx, sc, bkt, "linear/model.json", `{"weight":7,"bias":1,"name":"python-sdk"}`)

	_, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	network := internalNetwork(t, ctx, aiplatformPyImage, port)
	name := fmt.Sprintf("python-sdk-%d", time.Now().UnixNano()%1e6)
	run := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", network,
		"-v", filepath.Join(moduleRoot(t), "testdata", "aiplatformpy", "flow.py")+":/flow/flow.py:ro",
		"-v", creds+":/creds/adc.json:ro",
		"-e", "CB_ENDPOINT=http://host.docker.internal:"+port,
		"-e", "CB_PROJECT="+h.Project(), "-e", "CB_LOCATION=us-central1",
		"-e", "CB_CREDENTIALS=/creds/adc.json", "-e", "CB_MODEL_NAME="+name,
		"-e", "CB_ARTIFACT_URI=gs://"+bkt+"/linear", "-e", "CB_IMAGE="+image,
		"-e", "GOOGLE_CLOUD_UNIVERSE_DOMAIN=cloudburrow.localhost",
		aiplatformPyImage, "/flow/flow.py")
	out, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("the Python SDK flow: %v\n%s", err, out)
	}
	var res struct {
		Model, Endpoint, DeployedModelID, VersionID, SecondVersion, ModelVersionID string
		Versions, Deployed, VersionAliases                                         []string
		ExportFormats                                                              map[string][]string
		TrafficSplit                                                               map[string]int
		Predictions                                                                []float64
		EndpointListed, ModelListed, Cleaned                                       bool
	}
	line := ""
	for _, l := range strings.Split(string(out), "\n") {
		if s, ok := strings.CutPrefix(l, "RESULT "); ok {
			line = s
		}
	}
	if err := json.Unmarshal([]byte(strings.NewReplacer(`"deployed_model_id"`, `"DeployedModelID"`, `"model_version_id"`, `"ModelVersionID"`, `"version_id"`, `"VersionID"`,
		`"second_version"`, `"SecondVersion"`, `"version_aliases"`, `"VersionAliases"`, `"export_formats"`, `"ExportFormats"`,
		`"traffic_split"`, `"TrafficSplit"`, `"endpoint_listed"`, `"EndpointListed"`, `"model_listed"`, `"ModelListed"`).Replace(line)), &res); err != nil {
		t.Fatalf("the flow printed no result (%v):\n%s", err, out)
	}
	t.Logf("Python SDK flow: %s", line)
	parent := runParent(h)
	if !strings.HasPrefix(res.Model, parent+"/models/") || res.VersionID != "1" || strings.Join(res.VersionAliases, ",") != "default" {
		t.Errorf("Model.upload = %s version %s aliases %v", res.Model, res.VersionID, res.VersionAliases)
	}
	if got := res.ExportFormats["custom-trained"]; strings.Join(got, ",") != "ARTIFACT,IMAGE" {
		t.Errorf("supported_export_formats = %v; want custom-trained [ARTIFACT IMAGE]", res.ExportFormats)
	}
	if res.SecondVersion != "2" || strings.Join(res.Versions, ",") != "1,2" {
		t.Errorf("second version %s, versions %v", res.SecondVersion, res.Versions)
	}
	if !strings.HasPrefix(res.Endpoint, parent+"/endpoints/") || len(res.Deployed) != 1 || res.TrafficSplit[res.Deployed[0]] != 100 {
		t.Errorf("model.deploy = %s, deployed %v, traffic %v", res.Endpoint, res.Deployed, res.TrafficSplit)
	}
	if len(res.Predictions) != 2 || res.Predictions[0] != 15 || res.Predictions[1] != 36 || res.DeployedModelID != res.Deployed[0] {
		t.Errorf("endpoint.predict = %v from %s; want [15 36] from %v", res.Predictions, res.DeployedModelID, res.Deployed)
	}
	if res.ModelVersionID != "1" {
		t.Errorf("endpoint.predict's model_version_id = %q; want 1, the default version deployed", res.ModelVersionID)
	}
	if !res.EndpointListed || !res.ModelListed || !res.Cleaned {
		t.Errorf("listed endpoint %v, model %v; cleaned %v", res.EndpointListed, res.ModelListed, res.Cleaned)
	}
}
