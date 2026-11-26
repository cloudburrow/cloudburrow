//go:build compat

package compat

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	aiplatform "cloud.google.com/go/aiplatform/apiv1"
	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	aiplatformbeta "cloud.google.com/go/aiplatform/apiv1beta1"
	betapb "cloud.google.com/go/aiplatform/apiv1beta1/aiplatformpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// covers: google.cloud.aiplatform.v1beta1.ModelMonitoringService/CreateModelMonitor, google.cloud.aiplatform.v1beta1.ModelMonitoringService/GetModelMonitor, google.cloud.aiplatform.v1beta1.ModelMonitoringService/ListModelMonitors, google.cloud.aiplatform.v1beta1.ModelMonitoringService/UpdateModelMonitor, google.cloud.aiplatform.v1beta1.ModelMonitoringService/DeleteModelMonitor, google.cloud.aiplatform.v1beta1.ModelMonitoringService/CreateModelMonitoringJob, google.cloud.aiplatform.v1beta1.ModelMonitoringService/GetModelMonitoringJob, google.cloud.aiplatform.v1beta1.ModelMonitoringService/ListModelMonitoringJobs, google.cloud.aiplatform.v1beta1.ModelMonitoringService/DeleteModelMonitoringJob, google.cloud.aiplatform.v1beta1.ModelMonitoringService/SearchModelMonitoringStats, google.cloud.aiplatform.v1beta1.ModelMonitoringService/SearchModelMonitoringAlerts, google.cloud.aiplatform.v1beta1.ScheduleService/CreateSchedule, google.cloud.aiplatform.v1beta1.ScheduleService/GetSchedule, google.cloud.aiplatform.v1beta1.ScheduleService/ListSchedules, google.cloud.aiplatform.v1beta1.ScheduleService/PauseSchedule, google.cloud.aiplatform.v1beta1.ScheduleService/ResumeSchedule, google.cloud.aiplatform.v1beta1.ScheduleService/DeleteSchedule
//
// TestAgentPlatformModelMonitoring (#2214): through the official aiplatform
// v1beta1 Go clients (gRPC), a ModelMonitor of a Model Registry model is
// created with a training dataset in Cloud Storage, a monitoring job runs
// on a drifted target dataset on this instance and succeeds, its feature
// statistics (L-infinity for the categorical feature, Jensen-Shannon for the
// numeric one) and the alerts its thresholds raise are searched, a schedule
// of runs (create_model_monitoring_job_request) is created, listed with its
// filter, paused, resumed and deleted, and the monitor is read over REST,
// updated and deleted.
func TestAgentPlatformModelMonitoring(t *testing.T) {
	h := New(t)
	endpoint := runShardEndpoint(h, envRunLocalAI, EnvPrediction)
	shardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)
	opts := gardenOpts(h, EnvPrediction)
	if strings.TrimSpace(endpoint) == "" {
		t.Fatal("no Agent Platform endpoint")
	}
	mm, err := aiplatformbeta.NewModelMonitoringClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mm.Close() })
	sched, err := aiplatformbeta.NewScheduleClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sched.Close() })
	mc, _, _ := modelRegistryClients(t, h)
	sc := storageClient(t, h)
	parent := runParent(h)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1e6)

	bkt := h.Project() + "-model-monitoring"
	if err := sc.Bucket(bkt).Create(ctx, h.Project(), nil); err != nil && status.Code(err) != codes.AlreadyExists && !strings.Contains(err.Error(), "409") {
		t.Fatalf("create bucket: %v", err)
	}
	putArtifact(t, ctx, sc, bkt, "train.csv", "age,country\n30,us\n31,us\n32,fr\n33,fr\n")
	putArtifact(t, ctx, sc, bkt, "serve/part-1.jsonl", `{"age":60,"country":"us"}`+"\n"+`{"age":61,"country":"us"}`+"\n")
	putArtifact(t, ctx, sc, bkt, "serve/part-2.jsonl", `{"age":62,"country":"us"}`+"\n")

	up, err := mc.UploadModel(ctx, &aiplatformpb.UploadModelRequest{Parent: parent, ModelId: "monitored-" + suffix,
		Model: &aiplatformpb.Model{DisplayName: "monitored", ContainerSpec: &aiplatformpb.ModelContainerSpec{ImageUri: "us-docker.pkg.dev/p/r/serve:1"}}})
	if err != nil {
		t.Fatalf("UploadModel: %v", err)
	}
	if _, err := up.Wait(ctx); err != nil {
		t.Fatalf("UploadModel operation: %v", err)
	}
	model := parent + "/models/monitored-" + suffix
	t.Cleanup(func() {
		if op, err := mc.DeleteModel(context.Background(), &aiplatformpb.DeleteModelRequest{Name: model}); err == nil {
			_ = op.Wait(context.Background())
		}
	})

	gcs := func(uri string, f betapb.ModelMonitoringInput_ModelMonitoringDataset_ModelMonitoringGcsSource_DataFormat) *betapb.ModelMonitoringInput {
		return &betapb.ModelMonitoringInput{Dataset: &betapb.ModelMonitoringInput_ColumnizedDataset{ColumnizedDataset: &betapb.ModelMonitoringInput_ModelMonitoringDataset{
			DataLocation: &betapb.ModelMonitoringInput_ModelMonitoringDataset_GcsSource{GcsSource: &betapb.ModelMonitoringInput_ModelMonitoringDataset_ModelMonitoringGcsSource{GcsUri: uri, Format: f}}}}}
	}
	cond := &betapb.ModelMonitoringAlertCondition{Condition: &betapb.ModelMonitoringAlertCondition_Threshold{Threshold: 0.2}}
	monitor := &betapb.ModelMonitor{DisplayName: "compat monitor " + suffix,
		ModelMonitoringTarget: &betapb.ModelMonitor_ModelMonitoringTarget{Source: &betapb.ModelMonitor_ModelMonitoringTarget_VertexModel{
			VertexModel: &betapb.ModelMonitor_ModelMonitoringTarget_VertexModelSource{Model: model, ModelVersionId: "1"}}},
		TrainingDataset: gcs("gs://"+bkt+"/train.csv", betapb.ModelMonitoringInput_ModelMonitoringDataset_ModelMonitoringGcsSource_CSV),
		ModelMonitoringSchema: &betapb.ModelMonitoringSchema{FeatureFields: []*betapb.ModelMonitoringSchema_FieldSchema{
			{Name: "age", DataType: "integer"}, {Name: "country", DataType: "categorical"}}},
		OutputSpec: &betapb.ModelMonitoringOutputSpec{GcsBaseDirectory: &betapb.GcsDestination{OutputUriPrefix: "gs://" + bkt + "/results"}},
		DefaultObjective: &betapb.ModelMonitor_TabularObjective{TabularObjective: &betapb.ModelMonitoringObjectiveSpec_TabularObjective{
			FeatureDriftSpec: &betapb.ModelMonitoringObjectiveSpec_DataDriftSpec{CategoricalMetricType: "l_infinity", NumericMetricType: "jensen_shannon_divergence",
				DefaultCategoricalAlertCondition: cond, DefaultNumericAlertCondition: cond}}},
	}

	// Refusals: a missing model, a bad ID, an unsupported metric.
	missing := &betapb.ModelMonitor{ModelMonitoringTarget: &betapb.ModelMonitor_ModelMonitoringTarget{Source: &betapb.ModelMonitor_ModelMonitoringTarget_VertexModel{
		VertexModel: &betapb.ModelMonitor_ModelMonitoringTarget_VertexModelSource{Model: parent + "/models/missing-" + suffix}}}}
	if _, err := mm.CreateModelMonitor(ctx, &betapb.CreateModelMonitorRequest{Parent: parent, ModelMonitor: missing}); status.Code(err) != codes.NotFound {
		t.Errorf("CreateModelMonitor of a missing model = %v, want NOT_FOUND", err)
	}
	if _, err := mm.CreateModelMonitor(ctx, &betapb.CreateModelMonitorRequest{Parent: parent, ModelMonitor: monitor, ModelMonitorId: "Not_Valid"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateModelMonitor with a bad ID = %v, want INVALID_ARGUMENT", err)
	}

	op, err := mm.CreateModelMonitor(ctx, &betapb.CreateModelMonitorRequest{Parent: parent, ModelMonitor: monitor, ModelMonitorId: "compat-" + suffix})
	if err != nil {
		t.Fatalf("CreateModelMonitor: %v", err)
	}
	got, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateModelMonitor operation: %v", err)
	}
	name := parent + "/modelMonitors/compat-" + suffix
	if got.GetName() != name || got.GetCreateTime() == nil {
		t.Fatalf("CreateModelMonitor = %v", got)
	}
	t.Cleanup(func() {
		if op, err := mm.DeleteModelMonitor(context.Background(), &betapb.DeleteModelMonitorRequest{Name: name, Force: true}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	if g, err := mm.GetModelMonitor(ctx, &betapb.GetModelMonitorRequest{Name: name}); err != nil || g.GetDisplayName() != monitor.GetDisplayName() {
		t.Errorf("GetModelMonitor = %v, %v", g, err)
	}
	found := false
	for it := mm.ListModelMonitors(ctx, &betapb.ListModelMonitorsRequest{Parent: parent, Filter: fmt.Sprintf("display_name=%q", monitor.GetDisplayName())}); ; {
		m, err := it.Next()
		if err != nil {
			break
		}
		found = found || m.GetName() == name
	}
	if !found {
		t.Errorf("ListModelMonitors with a display_name filter did not list %s", name)
	}

	// A run on the drifted serving data, read with a wildcard.
	job, err := mm.CreateModelMonitoringJob(ctx, &betapb.CreateModelMonitoringJobRequest{Parent: name, ModelMonitoringJobId: "run-" + suffix,
		ModelMonitoringJob: &betapb.ModelMonitoringJob{DisplayName: "compat run", ModelMonitoringSpec: &betapb.ModelMonitoringSpec{ObjectiveSpec: &betapb.ModelMonitoringObjectiveSpec{
			TargetDataset: gcs("gs://"+bkt+"/serve/*.jsonl", betapb.ModelMonitoringInput_ModelMonitoringDataset_ModelMonitoringGcsSource_JSONL)}}}})
	if err != nil {
		t.Fatalf("CreateModelMonitoringJob: %v", err)
	}
	if job.GetState() != betapb.JobState_JOB_STATE_PENDING {
		t.Errorf("a new job is %v, want JOB_STATE_PENDING", job.GetState())
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		job, err = mm.GetModelMonitoringJob(ctx, &betapb.GetModelMonitoringJobRequest{Name: job.GetName()})
		if err != nil {
			t.Fatalf("GetModelMonitoringJob: %v", err)
		}
		if s := job.GetState(); s != betapb.JobState_JOB_STATE_PENDING && s != betapb.JobState_JOB_STATE_RUNNING || time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if job.GetState() != betapb.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("the job ended %v: %v", job.GetState(), job.GetJobExecutionDetail())
	}
	if d := job.GetJobExecutionDetail(); len(d.GetBaselineDatasets()) != 1 || d.GetBaselineDatasets()[0].GetLocation() != "gs://"+bkt+"/train.csv" ||
		d.GetObjectiveStatus()["raw-feature-drift"].GetCode() != 0 {
		t.Errorf("job_execution_detail = %v", d)
	}
	jobs := 0
	for it := mm.ListModelMonitoringJobs(ctx, &betapb.ListModelMonitoringJobsRequest{Parent: name}); ; {
		if _, err := it.Next(); err != nil {
			break
		}
		jobs++
	}
	if jobs != 1 {
		t.Errorf("ListModelMonitoringJobs listed %d jobs, want 1", jobs)
	}

	stats := map[string]*betapb.ModelMonitoringStatsDataPoint{}
	for it := mm.SearchModelMonitoringStats(ctx, &betapb.SearchModelMonitoringStatsRequest{ModelMonitor: name}); ; {
		s, err := it.Next()
		if err != nil {
			break
		}
		if ts := s.GetTabularStats(); ts.GetObjectiveType() == "raw-feature-drift" && len(ts.GetDataPoints()) > 0 {
			stats[ts.GetStatsName()] = ts.GetDataPoints()[0]
		}
	}
	if p := stats["country"]; p.GetAlgorithm() != "l_infinity" || p.GetCurrentStats().GetDistributionValue().GetDistributionDeviation() != 0.5 || !p.GetHasAnomaly() || p.GetThresholdValue() != 0.2 {
		t.Errorf("country's statistics = %v; want l_infinity 0.5, an anomaly over 0.2", p)
	}
	if p := stats["age"]; p.GetAlgorithm() != "jensen_shannon_divergence" || !p.GetHasAnomaly() {
		t.Errorf("age's statistics = %v; want a Jensen-Shannon anomaly", p)
	}
	alerts, err := mm.SearchModelMonitoringAlerts(ctx, &betapb.SearchModelMonitoringAlertsRequest{ModelMonitor: name, ModelMonitoringJob: job.GetName()}).Next()
	if err != nil || alerts.GetAnomaly().GetModelMonitoringJob() != job.GetName() || alerts.GetObjectiveType() != "raw-feature-drift" {
		t.Errorf("SearchModelMonitoringAlerts = %v, %v", alerts, err)
	}
	if r, err := sc.Bucket(bkt).Object("results/compat-" + suffix + "/run-" + suffix + "/stats.json").Attrs(ctx); err != nil || r.Size == 0 {
		t.Errorf("the job's results in its output directory: %v, %v", r, err)
	}

	// A schedule of runs.
	s, err := sched.CreateSchedule(ctx, &betapb.CreateScheduleRequest{Parent: parent, Schedule: &betapb.Schedule{DisplayName: "compat monitoring " + suffix,
		TimeSpecification: &betapb.Schedule_Cron{Cron: "TZ=America/New_York 0 * * * *"}, MaxConcurrentRunCount: 1,
		Request: &betapb.Schedule_CreateModelMonitoringJobRequest{CreateModelMonitoringJobRequest: &betapb.CreateModelMonitoringJobRequest{Parent: name,
			ModelMonitoringJob: &betapb.ModelMonitoringJob{ModelMonitoringSpec: &betapb.ModelMonitoringSpec{ObjectiveSpec: &betapb.ModelMonitoringObjectiveSpec{
				TargetDataset: gcs("gs://"+bkt+"/serve/*.jsonl", betapb.ModelMonitoringInput_ModelMonitoringDataset_ModelMonitoringGcsSource_JSONL)}}}}}}})
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	if s.GetState() != betapb.Schedule_ACTIVE || s.GetNextRunTime() == nil || s.GetCreateModelMonitoringJobRequest().GetParent() != name {
		t.Errorf("CreateSchedule = %v", s)
	}
	listed := false
	for it := sched.ListSchedules(ctx, &betapb.ListSchedulesRequest{Parent: parent, Filter: "create_model_monitoring_job_request:*"}); ; {
		x, err := it.Next()
		if err != nil {
			break
		}
		listed = listed || x.GetName() == s.GetName()
	}
	if !listed {
		t.Errorf("ListSchedules create_model_monitoring_job_request:* did not list %s", s.GetName())
	}
	if err := sched.PauseSchedule(ctx, &betapb.PauseScheduleRequest{Name: s.GetName()}); err != nil {
		t.Fatalf("PauseSchedule: %v", err)
	}
	if g, _ := sched.GetSchedule(ctx, &betapb.GetScheduleRequest{Name: s.GetName()}); g.GetState() != betapb.Schedule_PAUSED {
		t.Errorf("a paused schedule is %v", g.GetState())
	}
	if err := sched.ResumeSchedule(ctx, &betapb.ResumeScheduleRequest{Name: s.GetName()}); err != nil {
		t.Fatalf("ResumeSchedule: %v", err)
	}
	if op, err := mm.DeleteModelMonitor(ctx, &betapb.DeleteModelMonitorRequest{Name: name}); status.Code(err) != codes.FailedPrecondition {
		if err == nil {
			_ = op.Wait(ctx)
		}
		t.Errorf("DeleteModelMonitor with a schedule = %v, want FAILED_PRECONDITION", err)
	}
	dop, err := sched.DeleteSchedule(ctx, &betapb.DeleteScheduleRequest{Name: s.GetName()})
	if err != nil {
		t.Fatalf("DeleteSchedule: %v", err)
	}
	if err := dop.Wait(ctx); err != nil {
		t.Fatalf("DeleteSchedule operation: %v", err)
	}

	// REST: the same monitor as JSON.
	resp, err := http.Get("http://" + stripScheme(endpoint) + "/v1beta1/" + name)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /v1beta1/%s = %d", name, resp.StatusCode)
	}

	if jop, err := mm.DeleteModelMonitoringJob(ctx, &betapb.DeleteModelMonitoringJobRequest{Name: job.GetName()}); err != nil {
		t.Errorf("DeleteModelMonitoringJob: %v", err)
	} else if err := jop.Wait(ctx); err != nil {
		t.Errorf("DeleteModelMonitoringJob operation: %v", err)
	}
	monitor.Name, monitor.DisplayName = name, "renamed "+suffix
	uop, err := mm.UpdateModelMonitor(ctx, &betapb.UpdateModelMonitorRequest{ModelMonitor: monitor, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}})
	if err != nil {
		t.Fatalf("UpdateModelMonitor: %v", err)
	}
	if u, err := uop.Wait(ctx); err != nil || u.GetDisplayName() != "renamed "+suffix {
		t.Errorf("UpdateModelMonitor = %v, %v", u, err)
	}
	delOp, err := mm.DeleteModelMonitor(ctx, &betapb.DeleteModelMonitorRequest{Name: name})
	if err != nil {
		t.Fatalf("DeleteModelMonitor: %v", err)
	}
	if err := delOp.Wait(ctx); err != nil {
		t.Fatalf("DeleteModelMonitor operation: %v", err)
	}
	if _, err := mm.GetModelMonitor(ctx, &betapb.GetModelMonitorRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetModelMonitor after delete = %v, want NOT_FOUND", err)
	}
}

// covers: google.cloud.aiplatform.v1.JobService/CreateDataLabelingJob, google.cloud.aiplatform.v1.JobService/GetDataLabelingJob, google.cloud.aiplatform.v1.JobService/ListDataLabelingJobs, google.cloud.aiplatform.v1.JobService/DeleteDataLabelingJob, google.cloud.aiplatform.v1.JobService/CancelDataLabelingJob, google.cloud.aiplatform.v1.JobService/CreateModelDeploymentMonitoringJob, google.cloud.aiplatform.v1.JobService/ListModelDeploymentMonitoringJobs, google.cloud.aiplatform.v1.JobService/GetModelDeploymentMonitoringJob
//
// TestAgentPlatformDataLabelingAndDeploymentMonitoringRefusals (#2214):
// through the official aiplatform v1 JobClient, a data labeling job is
// refused (Google shut the Data Labeling Service down on October 3, 2024),
// none is listed and a named one is NOT_FOUND; a deployment monitoring job
// on an endpoint with no such deployed model, or with a monitoring
// interval under Google's one hour, is INVALID_ARGUMENT, and a missing job
// is NOT_FOUND.
func TestAgentPlatformDataLabelingAndDeploymentMonitoringRefusals(t *testing.T) {
	h := New(t)
	runShardEndpoint(h, envRunLocalAI, EnvPrediction)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	jc, err := aiplatform.NewJobClient(ctx, gardenOpts(h, EnvPrediction)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = jc.Close() })
	_, ec, _ := modelRegistryClients(t, h)
	parent := runParent(h)
	if _, err := jc.CreateDataLabelingJob(ctx, &aiplatformpb.CreateDataLabelingJobRequest{Parent: parent, DataLabelingJob: &aiplatformpb.DataLabelingJob{
		DisplayName: "labels", Datasets: []string{parent + "/datasets/1"}, LabelerCount: 1, InstructionUri: "gs://b/i.pdf", InputsSchemaUri: "gs://b/s.yaml"}}); status.Code(err) != codes.FailedPrecondition ||
		!strings.Contains(status.Convert(err).Message(), "shut down") {
		t.Errorf("CreateDataLabelingJob = %v, want FAILED_PRECONDITION: shut down", err)
	}
	if _, err := jc.ListDataLabelingJobs(ctx, &aiplatformpb.ListDataLabelingJobsRequest{Parent: parent}).Next(); err == nil {
		t.Error("ListDataLabelingJobs listed a job")
	}
	name := parent + "/dataLabelingJobs/123"
	if _, err := jc.GetDataLabelingJob(ctx, &aiplatformpb.GetDataLabelingJobRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetDataLabelingJob = %v, want NOT_FOUND", err)
	}
	if err := jc.CancelDataLabelingJob(ctx, &aiplatformpb.CancelDataLabelingJobRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("CancelDataLabelingJob = %v, want NOT_FOUND", err)
	}
	if _, err := jc.DeleteDataLabelingJob(ctx, &aiplatformpb.DeleteDataLabelingJobRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("DeleteDataLabelingJob = %v, want NOT_FOUND", err)
	}

	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1e6)
	eop, err := ec.CreateEndpoint(ctx, &aiplatformpb.CreateEndpointRequest{Parent: parent, EndpointId: "mdm-" + suffix, Endpoint: &aiplatformpb.Endpoint{DisplayName: "mdm"}})
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if _, err := eop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	ep := parent + "/endpoints/mdm-" + suffix
	t.Cleanup(func() {
		if op, err := ec.DeleteEndpoint(context.Background(), &aiplatformpb.DeleteEndpointRequest{Name: ep}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	job := &aiplatformpb.ModelDeploymentMonitoringJob{DisplayName: "watch", Endpoint: ep,
		ModelDeploymentMonitoringObjectiveConfigs: []*aiplatformpb.ModelDeploymentMonitoringObjectiveConfig{{DeployedModelId: "404",
			ObjectiveConfig: &aiplatformpb.ModelMonitoringObjectiveConfig{PredictionDriftDetectionConfig: &aiplatformpb.ModelMonitoringObjectiveConfig_PredictionDriftDetectionConfig{}}}},
		ModelDeploymentMonitoringScheduleConfig: &aiplatformpb.ModelDeploymentMonitoringScheduleConfig{MonitorInterval: durationpb.New(time.Hour)}}
	if _, err := jc.CreateModelDeploymentMonitoringJob(ctx, &aiplatformpb.CreateModelDeploymentMonitoringJobRequest{Parent: parent, ModelDeploymentMonitoringJob: job}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a job of a model not deployed = %v, want INVALID_ARGUMENT", err)
	}
	job.ModelDeploymentMonitoringObjectiveConfigs = nil
	if _, err := jc.CreateModelDeploymentMonitoringJob(ctx, &aiplatformpb.CreateModelDeploymentMonitoringJobRequest{Parent: parent, ModelDeploymentMonitoringJob: job}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a job with no objective configs = %v, want INVALID_ARGUMENT", err)
	}
	if _, err := jc.GetModelDeploymentMonitoringJob(ctx, &aiplatformpb.GetModelDeploymentMonitoringJobRequest{Name: parent + "/modelDeploymentMonitoringJobs/404"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetModelDeploymentMonitoringJob of a missing job = %v, want NOT_FOUND", err)
	}
	if _, err := jc.ListModelDeploymentMonitoringJobs(ctx, &aiplatformpb.ListModelDeploymentMonitoringJobsRequest{Parent: parent, Filter: `display_name="watch-` + suffix + `"`}).Next(); err == nil {
		t.Error("ListModelDeploymentMonitoringJobs listed a job that was never created")
	}
}
