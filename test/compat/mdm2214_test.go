//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	aiplatform "cloud.google.com/go/aiplatform/apiv1"
	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
)

// Not yet annotated covers: (it has not run); once it passes it covers
// the MDM job lifecycle methods.
//
// TestAgentPlatformDeploymentMonitoringRuns (#2214), with the official
// aiplatform v1 Go clients (gRPC): the model server is deployed on an
// endpoint, a ModelDeploymentMonitoringJob watches it for skew from a
// training dataset in Cloud Storage and for drift, predictions on drifted
// data are logged, and with the instance's clock moved past the job's
// one-hour monitor interval (CloudBurrow's clock:advance) the job runs: its
// skew stats name the feature x with an anomaly. The job is paused (Delete
// of a running job is FAILED_PRECONDITION first), resumed, paused again and
// deleted.
func TestAgentPlatformDeploymentMonitoringRuns(t *testing.T) {
	h := New(t)
	endpointAddr := runShardEndpoint(h, envRunLocalAI, EnvPrediction)
	scaleShardEndpoint(h, envScaleRun, EnvRun) // a serving container's start: the scale shard (#1821)
	shardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	if strings.TrimSpace(endpointAddr) == "" {
		t.Fatal("no Agent Platform endpoint")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	buildModelServer(t, ctx)
	image := pushModelServer(t, ctx, h)
	mc, ec, pc := modelRegistryClients(t, h)
	jc, err := aiplatform.NewJobClient(ctx, gardenOpts(h, EnvPrediction)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = jc.Close() })
	sc := storageClient(t, h)
	parent := runParent(h)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1e6)

	bkt := h.Project() + "-mdm-" + suffix
	if err := sc.Bucket(bkt).Create(ctx, h.Project(), nil); err != nil && status.Code(err) != codes.AlreadyExists && !strings.Contains(err.Error(), "409") {
		t.Fatalf("create bucket: %v", err)
	}
	putArtifact(t, ctx, sc, bkt, "model/model.json", `{"weight":2,"bias":0,"name":"mdm"}`)
	putArtifact(t, ctx, sc, bkt, "train.csv", "x\n1\n2\n3\n4\n")

	modelID := "mdm-" + suffix
	up, err := mc.UploadModel(ctx, &aiplatformpb.UploadModelRequest{Parent: parent, ModelId: modelID, Model: &aiplatformpb.Model{
		DisplayName: "mdm", ArtifactUri: "gs://" + bkt + "/model", ContainerSpec: &aiplatformpb.ModelContainerSpec{ImageUri: image,
			PredictRoute: "/v1/models/linear:predict", HealthRoute: "/v1/models/linear", Ports: []*aiplatformpb.Port{{ContainerPort: 7080}}}}})
	if err != nil {
		t.Fatalf("UploadModel: %v", err)
	}
	if _, err := up.Wait(ctx); err != nil {
		t.Fatalf("UploadModel operation: %v", err)
	}
	model := parent + "/models/" + modelID
	cop, err := ec.CreateEndpoint(ctx, &aiplatformpb.CreateEndpointRequest{Parent: parent, EndpointId: "mdm-" + suffix, Endpoint: &aiplatformpb.Endpoint{DisplayName: "mdm"}})
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	ep, err := cop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateEndpoint operation: %v", err)
	}
	endpoint := ep.GetName()
	t.Cleanup(func() {
		bg, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if e, err := ec.GetEndpoint(bg, &aiplatformpb.GetEndpointRequest{Name: endpoint}); err == nil {
			for _, d := range e.GetDeployedModels() {
				if op, err := ec.UndeployModel(bg, &aiplatformpb.UndeployModelRequest{Endpoint: endpoint, DeployedModelId: d.GetId()}); err == nil {
					_, _ = op.Wait(bg)
				}
			}
		}
		if op, err := ec.DeleteEndpoint(bg, &aiplatformpb.DeleteEndpointRequest{Name: endpoint}); err == nil {
			_ = op.Wait(bg)
		}
		if op, err := mc.DeleteModel(bg, &aiplatformpb.DeleteModelRequest{Name: model}); err == nil {
			_ = op.Wait(bg)
		}
	})
	dop, err := ec.DeployModel(ctx, &aiplatformpb.DeployModelRequest{Endpoint: endpoint, DeployedModel: &aiplatformpb.DeployedModel{
		Model: model, DisplayName: "mdm", PredictionResources: &aiplatformpb.DeployedModel_DedicatedResources{DedicatedResources: &aiplatformpb.DedicatedResources{
			MachineSpec: &aiplatformpb.MachineSpec{MachineType: "n1-standard-2"}, MinReplicaCount: 1, MaxReplicaCount: 1}}},
		TrafficSplit: map[string]int32{"0": 100}})
	if err != nil {
		t.Fatalf("DeployModel: %v", err)
	}
	dr, err := dop.Wait(ctx)
	if err != nil {
		t.Fatalf("DeployModel operation: %v", err)
	}
	dm := dr.GetDeployedModel().GetId()

	job := &aiplatformpb.ModelDeploymentMonitoringJob{DisplayName: "mdm-" + suffix, Endpoint: endpoint,
		ModelDeploymentMonitoringObjectiveConfigs: []*aiplatformpb.ModelDeploymentMonitoringObjectiveConfig{{DeployedModelId: dm,
			ObjectiveConfig: &aiplatformpb.ModelMonitoringObjectiveConfig{
				TrainingDataset: &aiplatformpb.ModelMonitoringObjectiveConfig_TrainingDataset{DataFormat: "csv",
					DataSource: &aiplatformpb.ModelMonitoringObjectiveConfig_TrainingDataset_GcsSource{GcsSource: &aiplatformpb.GcsSource{Uris: []string{"gs://" + bkt + "/train.csv"}}}},
				TrainingPredictionSkewDetectionConfig: &aiplatformpb.ModelMonitoringObjectiveConfig_TrainingPredictionSkewDetectionConfig{
					DefaultSkewThreshold: &aiplatformpb.ThresholdConfig{Threshold: &aiplatformpb.ThresholdConfig_Value{Value: 0.1}}},
				PredictionDriftDetectionConfig: &aiplatformpb.ModelMonitoringObjectiveConfig_PredictionDriftDetectionConfig{},
			}}},
		LoggingSamplingStrategy: &aiplatformpb.SamplingStrategy{RandomSampleConfig: &aiplatformpb.SamplingStrategy_RandomSampleConfig{SampleRate: 1}},
		// A two-hour window: the predictions below, made before the clock
		// moves an hour, are inside it.
		ModelDeploymentMonitoringScheduleConfig: &aiplatformpb.ModelDeploymentMonitoringScheduleConfig{MonitorInterval: durationpb.New(time.Hour),
			MonitorWindow: durationpb.New(2 * time.Hour)}}
	j, err := jc.CreateModelDeploymentMonitoringJob(ctx, &aiplatformpb.CreateModelDeploymentMonitoringJobRequest{Parent: parent, ModelDeploymentMonitoringJob: job})
	if err != nil {
		t.Fatalf("CreateModelDeploymentMonitoringJob: %v", err)
	}
	name := j.GetName()
	t.Cleanup(func() {
		bg := context.Background()
		_ = jc.PauseModelDeploymentMonitoringJob(bg, &aiplatformpb.PauseModelDeploymentMonitoringJobRequest{Name: name})
		if op, err := jc.DeleteModelDeploymentMonitoringJob(bg, &aiplatformpb.DeleteModelDeploymentMonitoringJobRequest{Name: name}); err == nil {
			_ = op.Wait(bg)
		}
	})
	get := func() *aiplatformpb.ModelDeploymentMonitoringJob {
		t.Helper()
		g, err := jc.GetModelDeploymentMonitoringJob(ctx, &aiplatformpb.GetModelDeploymentMonitoringJobRequest{Name: name})
		if err != nil {
			t.Fatalf("GetModelDeploymentMonitoringJob: %v", err)
		}
		return g
	}
	for deadline := time.Now().Add(time.Minute); get().GetState() != aiplatformpb.JobState_JOB_STATE_RUNNING; time.Sleep(time.Second) {
		if time.Now().After(deadline) {
			t.Fatalf("the job is %v, not RUNNING, a minute after it was created", get().GetState())
		}
	}

	for _, x := range []float64{50, 51, 52, 53} {
		inst, _ := structpb.NewValue(map[string]any{"x": x})
		resp, err := pc.Predict(ctx, &aiplatformpb.PredictRequest{Endpoint: endpoint, Instances: []*structpb.Value{inst}})
		if err != nil {
			t.Fatalf("Predict: %v", err)
		}
		if got := resp.GetPredictions()[0].GetNumberValue(); got != 2*x {
			t.Errorf("Predict(%v) = %v", x, got)
		}
	}

	// The clock, an hour on: the first run is due.
	resp, err := http.Post("http://"+stripScheme(endpointAddr)+"/cloudburrow/v1/clock:advance", "application/json", strings.NewReader(`{"duration": "3601s"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clock:advance = %d: %s", resp.StatusCode, body)
	}
	search := func() *aiplatformpb.ModelMonitoringStatsAnomalies {
		it := jc.SearchModelDeploymentMonitoringStatsAnomalies(ctx, &aiplatformpb.SearchModelDeploymentMonitoringStatsAnomaliesRequest{
			ModelDeploymentMonitoringJob: name, DeployedModelId: dm,
			Objectives: []*aiplatformpb.SearchModelDeploymentMonitoringStatsAnomaliesRequest_StatsAnomaliesObjective{{Type: aiplatformpb.ModelDeploymentMonitoringObjectiveType_RAW_FEATURE_SKEW}}})
		s, err := it.Next()
		if err != nil {
			return nil
		}
		return s
	}
	var skew *aiplatformpb.ModelMonitoringStatsAnomalies
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(2 * time.Second) {
		if skew = search(); skew != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no skew stats two minutes after the run was due; the job: %v", get())
		}
	}
	if skew.GetObjective() != aiplatformpb.ModelDeploymentMonitoringObjectiveType_RAW_FEATURE_SKEW || skew.GetDeployedModelId() != dm ||
		skew.GetAnomalyCount() < 1 || len(skew.GetFeatureStats()) != 1 || skew.GetFeatureStats()[0].GetFeatureDisplayName() != "x" {
		t.Errorf("skew stats = %v; want feature x with an anomaly", skew)
	}
	if g := get(); g.GetLatestMonitoringPipelineMetadata().GetRunTime() == nil || g.GetLatestMonitoringPipelineMetadata().GetStatus().GetCode() != 0 ||
		g.GetNextScheduleTime() == nil {
		t.Errorf("after its run the job = %v", g)
	}

	if _, err := jc.DeleteModelDeploymentMonitoringJob(ctx, &aiplatformpb.DeleteModelDeploymentMonitoringJobRequest{Name: name}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Delete of a running job = %v, want FAILED_PRECONDITION", err)
	}
	if err := jc.PauseModelDeploymentMonitoringJob(ctx, &aiplatformpb.PauseModelDeploymentMonitoringJobRequest{Name: name}); err != nil {
		t.Fatalf("PauseModelDeploymentMonitoringJob: %v", err)
	}
	if g := get(); g.GetState() != aiplatformpb.JobState_JOB_STATE_PAUSED || g.GetScheduleState() != aiplatformpb.ModelDeploymentMonitoringJob_OFFLINE {
		t.Errorf("paused job = %v %v", g.GetState(), g.GetScheduleState())
	}
	if err := jc.ResumeModelDeploymentMonitoringJob(ctx, &aiplatformpb.ResumeModelDeploymentMonitoringJobRequest{Name: name}); err != nil {
		t.Fatalf("ResumeModelDeploymentMonitoringJob: %v", err)
	}
	if g := get(); g.GetState() != aiplatformpb.JobState_JOB_STATE_RUNNING || g.GetNextScheduleTime() == nil {
		t.Errorf("resumed job = %v, next %v", g.GetState(), g.GetNextScheduleTime())
	}
	if err := jc.PauseModelDeploymentMonitoringJob(ctx, &aiplatformpb.PauseModelDeploymentMonitoringJobRequest{Name: name}); err != nil {
		t.Fatalf("PauseModelDeploymentMonitoringJob: %v", err)
	}
	op, err := jc.DeleteModelDeploymentMonitoringJob(ctx, &aiplatformpb.DeleteModelDeploymentMonitoringJobRequest{Name: name})
	if err != nil {
		t.Fatalf("DeleteModelDeploymentMonitoringJob: %v", err)
	}
	if err := op.Wait(ctx); err != nil {
		t.Fatalf("DeleteModelDeploymentMonitoringJob operation: %v", err)
	}
	if _, err := jc.GetModelDeploymentMonitoringJob(ctx, &aiplatformpb.GetModelDeploymentMonitoringJobRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("Get after Delete = %v, want NOT_FOUND", err)
	}
}

// Not yet annotated covers: (it has not run); once it passes it covers
// the ModelMonitoringService methods the SDK calls.
//
// TestPythonAIPlatformModelMonitoring (#2214): the Python Vertex AI SDK's
// Model Monitoring v2 (vertexai.resources.preview.ml_monitoring,
// testdata/aiplatformpy/monitor.py) creates a ModelMonitor of an uploaded
// model with a training dataset in the instance's Cloud Storage and a
// drift objective, runs a job on a drifted target dataset that SUCCEEDS,
// reads its stats (age and country) and the alerts the thresholds raise,
// and deletes the monitor with force. The SDK runs as
// TestPythonAIPlatformUploadAndDeploy's does, on an internal Docker
// network whose one route is a relay to the instance's Vertex AI port, so
// nothing it does can reach Google.
func TestPythonAIPlatformModelMonitoring(t *testing.T) {
	h := New(t)
	runShardEndpoint(h, envRunLocalAI, EnvPrediction)
	shardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	endpoint := h.Endpoint(EnvPrediction)
	creds := os.Getenv("CLOUDBURROW_TEST_CREDENTIALS")
	if creds == "" {
		t.Skip("CLOUDBURROW_TEST_CREDENTIALS is not set: the SDK needs the instance's ADC fixture")
	}
	sc := storageClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, "docker", "build", "-t", aiplatformPyImage, "testdata/aiplatformpy")
	build.Dir = moduleRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the Python SDK fixture: %v\n%s", err, out)
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1e6)
	bkt := h.Project() + "-mm-python-" + suffix
	if err := sc.Bucket(bkt).Create(ctx, h.Project(), nil); err != nil && status.Code(err) != codes.AlreadyExists && !strings.Contains(err.Error(), "409") {
		t.Fatalf("create bucket: %v", err)
	}
	putArtifact(t, ctx, sc, bkt, "train.csv", "age,country\n30,us\n31,us\n32,fr\n33,fr\n")
	putArtifact(t, ctx, sc, bkt, "serve.jsonl", `{"age":60,"country":"us"}`+"\n"+`{"age":61,"country":"us"}`+"\n"+`{"age":62,"country":"us"}`+"\n")
	_, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	network := internalNetwork(t, ctx, aiplatformPyImage, port)
	run := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", network,
		"-v", filepath.Join(moduleRoot(t), "testdata", "aiplatformpy", "monitor.py")+":/flow/monitor.py:ro",
		"-v", creds+":/creds/adc.json:ro",
		"-e", "CB_ENDPOINT=http://host.docker.internal:"+port,
		"-e", "CB_PROJECT="+h.Project(), "-e", "CB_LOCATION=us-central1",
		"-e", "CB_CREDENTIALS=/creds/adc.json", "-e", "CB_MODEL_NAME=python-mm-"+suffix, "-e", "CB_BUCKET="+bkt,
		"-e", "GOOGLE_CLOUD_UNIVERSE_DOMAIN=cloudburrow.localhost",
		aiplatformPyImage, "/flow/monitor.py")
	out, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("the Python SDK flow: %v\n%s", err, out)
	}
	var res struct {
		Model, Monitor, Job, State string
		JobsListed                 bool `json:"jobs_listed"`
		Stats, Alerts              []string
		TotalAlerts                int `json:"total_alerts"`
		Deleted                    bool
	}
	line := ""
	for _, l := range strings.Split(string(out), "\n") {
		if s, ok := strings.CutPrefix(l, "RESULT "); ok {
			line = s
		}
	}
	if err := json.Unmarshal([]byte(line), &res); err != nil {
		t.Fatalf("the flow printed no result (%v):\n%s", err, out)
	}
	t.Logf("Python SDK flow: %s", line)
	parent := runParent(h)
	if !strings.HasPrefix(res.Monitor, parent+"/modelMonitors/") || !strings.HasPrefix(res.Job, res.Monitor+"/modelMonitoringJobs/") {
		t.Errorf("monitor %s, job %s", res.Monitor, res.Job)
	}
	if res.State != "JOB_STATE_SUCCEEDED" || !res.JobsListed {
		t.Errorf("job state %s, listed %v", res.State, res.JobsListed)
	}
	if strings.Join(res.Stats, ",") != "age,country" {
		t.Errorf("search_metrics stats %v; want age and country", res.Stats)
	}
	if res.TotalAlerts < 1 || len(res.Alerts) < 1 {
		t.Errorf("search_alerts = %v (%d); want the drifted features' alerts", res.Alerts, res.TotalAlerts)
	}
	if !res.Deleted {
		t.Error("the monitor is still there after delete(force=True)")
	}
}
