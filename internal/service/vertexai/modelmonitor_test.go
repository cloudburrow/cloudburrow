package vertexai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	betapb "cloud.google.com/go/aiplatform/apiv1beta1/aiplatformpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestCompareFeatureDistances(t *testing.T) {
	rows := func(vals ...string) []map[string]any {
		var out []map[string]any
		for _, v := range vals {
			out = append(out, map[string]any{"f": v})
		}
		return out
	}
	d, err := compareFeature(monitorField{Name: "f"}, "", rows("a", "a", "b", "b"), rows("a", "a", "a", "a"))
	if err != nil || d.Algorithm != algoLInf || d.Distance != 0.5 {
		t.Fatalf("l_infinity = %+v, %v; want 0.5", d, err)
	}
	d, _ = compareFeature(monitorField{Name: "f"}, algoJS, rows("a", "a"), rows("b", "b"))
	if d.Distance != 1 {
		t.Fatalf("JS of disjoint = %g, want 1", d.Distance)
	}
	d, _ = compareFeature(monitorField{Name: "f", Numeric: true}, "", rows("1", "2", "3"), rows("1", "2", "3"))
	if d.Distance != 0 || d.Algorithm != algoJS {
		t.Fatalf("JS of equal = %+v", d)
	}
	if _, err := compareFeature(monitorField{Name: "f", Numeric: true}, algoLInf, rows("1"), rows("1")); err == nil {
		t.Fatal("l_infinity on a numeric feature was accepted")
	}
	if _, err := compareFeature(monitorField{Name: "f", Numeric: true}, "", rows("x"), rows("1")); err == nil {
		t.Fatal("a non-number in a numeric feature was accepted")
	}
}

// monitorServer is a server with a registry endpoint whose deployed model
// answers {"score": instance.x * 2}, and Cloud Storage in a fake runner.
func monitorServer(t *testing.T) (*Server, *fakePipelineRunner, string, string) {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req struct{ Instances []map[string]any }
		_ = json.Unmarshal(b, &req)
		var preds []string
		for range req.Instances {
			preds = append(preds, `{"label":"yes"}`)
		}
		_, _ = io.WriteString(w, `{"predictions":[`+strings.Join(preds, ",")+`]}`)
	}))
	t.Cleanup(backend.Close)
	s, runner := newPipelineServer(t)
	s.SetEndpoints(nil, func() string { return strings.TrimPrefix(backend.URL, "http://") }, nil)
	s.SetRegistry(filepath.Join(t.TempDir(), "registry.json"), &fakeRunner{}, &fakeArtifacts{})
	joinRegistry(t, s)
	ctx := context.Background()
	parent := "projects/p/locations/us-central1"
	ms, es := &modelService{s: s}, &endpointService{s: s}
	spec := &aiplatformpb.ModelContainerSpec{ImageUri: "dev.local/m:1", PredictRoute: "/predict", HealthRoute: "/health"}
	if _, err := ms.UploadModel(ctx, &aiplatformpb.UploadModelRequest{Parent: parent, ModelId: "churn", Model: &aiplatformpb.Model{DisplayName: "churn", ContainerSpec: spec}}); err != nil {
		t.Fatal(err)
	}
	if _, err := es.CreateEndpoint(ctx, &aiplatformpb.CreateEndpointRequest{Parent: parent, EndpointId: "ep1", Endpoint: &aiplatformpb.Endpoint{DisplayName: "e"}}); err != nil {
		t.Fatal(err)
	}
	op, err := es.DeployModel(ctx, &aiplatformpb.DeployModelRequest{Endpoint: parent + "/endpoints/ep1", DeployedModel: &aiplatformpb.DeployedModel{
		Id: "111", Model: parent + "/models/churn", PredictionResources: &aiplatformpb.DeployedModel_DedicatedResources{DedicatedResources: &aiplatformpb.DedicatedResources{
			MachineSpec: &aiplatformpb.MachineSpec{MachineType: "n1-standard-2"}, MinReplicaCount: 1}}}})
	if err != nil {
		t.Fatal(err)
	}
	waitOp(t, s, op.GetName())
	return s, runner, parent, parent + "/endpoints/ep1"
}

func waitMMJob(t *testing.T, x *modelMonitoringService, name string) *betapb.ModelMonitoringJob {
	t.Helper()
	for i := 0; i < 200; i++ {
		j, err := x.GetModelMonitoringJob(context.Background(), &betapb.GetModelMonitoringJobRequest{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		if j.GetState() != betapb.JobState_JOB_STATE_PENDING && j.GetState() != betapb.JobState_JOB_STATE_RUNNING {
			return j
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %s did not finish", name)
	return nil
}

func TestModelMonitorJobsStatsAlertsAndSchedules(t *testing.T) {
	s, runner, parent, _ := monitorServer(t)
	ctx := context.Background()
	runner.objects["b/train.csv"] = []byte("age,country\n30,us\n31,us\n32,fr\n33,fr\n")
	runner.objects["b/serve.jsonl"] = []byte(`{"age":60,"country":"us"}` + "\n" + `{"age":61,"country":"us"}` + "\n" + `{"age":62,"country":"us"}` + "\n")
	x := &modelMonitoringService{s: s}
	gcs := func(uri string, f betapb.ModelMonitoringInput_ModelMonitoringDataset_ModelMonitoringGcsSource_DataFormat) *betapb.ModelMonitoringInput {
		return &betapb.ModelMonitoringInput{Dataset: &betapb.ModelMonitoringInput_ColumnizedDataset{ColumnizedDataset: &betapb.ModelMonitoringInput_ModelMonitoringDataset{
			DataLocation: &betapb.ModelMonitoringInput_ModelMonitoringDataset_GcsSource{GcsSource: &betapb.ModelMonitoringInput_ModelMonitoringDataset_ModelMonitoringGcsSource{GcsUri: uri, Format: f}}}}}
	}
	cond := func(v float64) *betapb.ModelMonitoringAlertCondition {
		return &betapb.ModelMonitoringAlertCondition{Condition: &betapb.ModelMonitoringAlertCondition_Threshold{Threshold: v}}
	}
	mm := &betapb.ModelMonitor{DisplayName: "churn monitor",
		ModelMonitoringTarget: &betapb.ModelMonitor_ModelMonitoringTarget{Source: &betapb.ModelMonitor_ModelMonitoringTarget_VertexModel{
			VertexModel: &betapb.ModelMonitor_ModelMonitoringTarget_VertexModelSource{Model: parent + "/models/churn", ModelVersionId: "1"}}},
		TrainingDataset: gcs("gs://b/train.csv", betapb.ModelMonitoringInput_ModelMonitoringDataset_ModelMonitoringGcsSource_CSV),
		ModelMonitoringSchema: &betapb.ModelMonitoringSchema{FeatureFields: []*betapb.ModelMonitoringSchema_FieldSchema{
			{Name: "age", DataType: "integer"}, {Name: "country", DataType: "categorical"}}},
		OutputSpec: &betapb.ModelMonitoringOutputSpec{GcsBaseDirectory: &betapb.GcsDestination{OutputUriPrefix: "gs://b/out"}},
		DefaultObjective: &betapb.ModelMonitor_TabularObjective{TabularObjective: &betapb.ModelMonitoringObjectiveSpec_TabularObjective{
			FeatureDriftSpec: &betapb.ModelMonitoringObjectiveSpec_DataDriftSpec{DefaultCategoricalAlertCondition: cond(0.1), DefaultNumericAlertCondition: cond(0.1)}}},
	}
	bad := &betapb.ModelMonitor{ModelMonitoringTarget: &betapb.ModelMonitor_ModelMonitoringTarget{Source: &betapb.ModelMonitor_ModelMonitoringTarget_VertexModel{
		VertexModel: &betapb.ModelMonitor_ModelMonitoringTarget_VertexModelSource{Model: parent + "/models/nope"}}}}
	if _, err := x.CreateModelMonitor(ctx, &betapb.CreateModelMonitorRequest{Parent: parent, ModelMonitor: bad}); status.Code(err) != codes.NotFound {
		t.Fatalf("a monitor of a missing model = %v, want NOT_FOUND", err)
	}
	if _, err := x.CreateModelMonitor(ctx, &betapb.CreateModelMonitorRequest{Parent: parent, ModelMonitor: mm, ModelMonitorId: "Bad_ID"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad ID = %v", err)
	}
	op, err := x.CreateModelMonitor(ctx, &betapb.CreateModelMonitorRequest{Parent: parent, ModelMonitor: mm, ModelMonitorId: "churn-mon"})
	if err != nil || !op.GetDone() {
		t.Fatalf("CreateModelMonitor = %v, %v", op, err)
	}
	mon := parent + "/modelMonitors/churn-mon"
	if l, _ := x.ListModelMonitors(ctx, &betapb.ListModelMonitorsRequest{Parent: parent, Filter: `display_name="churn monitor"`}); len(l.GetModelMonitors()) != 1 {
		t.Fatalf("list = %v", l)
	}
	j, err := x.CreateModelMonitoringJob(ctx, &betapb.CreateModelMonitoringJobRequest{Parent: mon, ModelMonitoringJob: &betapb.ModelMonitoringJob{DisplayName: "run 1",
		ModelMonitoringSpec: &betapb.ModelMonitoringSpec{ObjectiveSpec: &betapb.ModelMonitoringObjectiveSpec{
			TargetDataset: gcs("gs://b/serve.jsonl", betapb.ModelMonitoringInput_ModelMonitoringDataset_ModelMonitoringGcsSource_JSONL)}}}})
	if err != nil || j.GetState() != betapb.JobState_JOB_STATE_PENDING {
		t.Fatalf("CreateModelMonitoringJob = %v, %v", j, err)
	}
	done := waitMMJob(t, x, j.GetName())
	if done.GetState() != betapb.JobState_JOB_STATE_SUCCEEDED || done.GetJobExecutionDetail().GetObjectiveStatus()[objFeatureDrift].GetCode() != 0 {
		t.Fatalf("job = %v", done)
	}
	stats, err := x.SearchModelMonitoringStats(ctx, &betapb.SearchModelMonitoringStatsRequest{ModelMonitor: mon})
	if err != nil || len(stats.GetMonitoringStats()) != 2 {
		t.Fatalf("stats = %v, %v", stats, err)
	}
	for _, st := range stats.GetMonitoringStats() {
		p := st.GetTabularStats().GetDataPoints()[0]
		if !p.GetHasAnomaly() || p.GetThresholdValue() != 0.1 || p.GetModelMonitoringJob() != j.GetName() {
			t.Fatalf("%s: point = %v", st.GetTabularStats().GetStatsName(), p)
		}
		if st.GetTabularStats().GetStatsName() == "country" && (p.GetAlgorithm() != algoLInf || p.GetCurrentStats().GetDistributionValue().GetDistributionDeviation() != 0.5) {
			t.Fatalf("country = %v", p)
		}
	}
	alerts, err := x.SearchModelMonitoringAlerts(ctx, &betapb.SearchModelMonitoringAlertsRequest{ModelMonitor: mon, StatsName: "age"})
	if err != nil || alerts.GetTotalNumberAlerts() != 1 || !strings.HasPrefix(alerts.GetModelMonitoringAlerts()[0].GetAnomaly().GetTabularAnomaly().GetAnomalyUri(), "gs://b/out/churn-mon/") {
		t.Fatalf("alerts = %v, %v", alerts, err)
	}
	if _, ok := runner.objects["b/out/churn-mon/"+lastSegment(j.GetName())+"/stats.json"]; !ok {
		t.Fatal("the job wrote no stats to the output directory")
	}

	// A schedule of runs, through v1beta1's ScheduleService.
	bs := &betaScheduleService{s: s}
	sc, err := bs.CreateSchedule(ctx, &betapb.CreateScheduleRequest{Parent: parent, Schedule: &betapb.Schedule{DisplayName: "hourly",
		TimeSpecification: &betapb.Schedule_Cron{Cron: "* * * * *"}, MaxConcurrentRunCount: 1,
		Request: &betapb.Schedule_CreateModelMonitoringJobRequest{CreateModelMonitoringJobRequest: &betapb.CreateModelMonitoringJobRequest{Parent: mon,
			ModelMonitoringJob: &betapb.ModelMonitoringJob{ModelMonitoringSpec: &betapb.ModelMonitoringSpec{ObjectiveSpec: &betapb.ModelMonitoringObjectiveSpec{
				TargetDataset: gcs("gs://b/serve.jsonl", betapb.ModelMonitoringInput_ModelMonitoringDataset_ModelMonitoringGcsSource_JSONL)}}}}}}})
	if err != nil || sc.GetCreateModelMonitoringJobRequest().GetParent() != mon {
		t.Fatalf("CreateSchedule = %v, %v", sc, err)
	}
	if l, err := bs.ListSchedules(ctx, &betapb.ListSchedulesRequest{Parent: parent, Filter: "create_model_monitoring_job_request:*"}); err != nil || len(l.GetSchedules()) != 1 {
		t.Fatalf("ListSchedules = %v, %v", l, err)
	}
	st := s.notebooks()
	st.mu.Lock()
	st.schedules[sc.GetName()].NextRunTime = st.schedules[sc.GetName()].CreateTime
	st.mu.Unlock()
	s.runDueSchedules()
	got, _ := bs.GetSchedule(ctx, &betapb.GetScheduleRequest{Name: sc.GetName()})
	run := got.GetLastScheduledRunResponse().GetRunResponse()
	if !strings.HasPrefix(run, mon+"/modelMonitoringJobs/") {
		t.Fatalf("the scheduled run = %q", run)
	}
	if rj := waitMMJob(t, x, run); rj.GetSchedule() != sc.GetName() || rj.GetScheduleTime() == nil || rj.GetDisplayName() != "hourly" {
		t.Fatalf("scheduled job = %v", rj)
	}
	if _, err := x.DeleteModelMonitor(ctx, &betapb.DeleteModelMonitorRequest{Name: mon}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("delete with schedules = %v", err)
	}
	if _, err := x.DeleteModelMonitor(ctx, &betapb.DeleteModelMonitorRequest{Name: mon, Force: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := bs.GetSchedule(ctx, &betapb.GetScheduleRequest{Name: sc.GetName()}); status.Code(err) != codes.NotFound {
		t.Fatalf("schedule after a forced delete = %v", err)
	}

	// The state survives a restart.
	s2 := NewServer(nil)
	s2.SetNotebookRuntimes(newFakeHost(), st.file)
	if _, err := (&modelMonitoringService{s: s2}).GetModelMonitoringJob(ctx, &betapb.GetModelMonitoringJobRequest{Name: j.GetName()}); status.Code(err) != codes.NotFound {
		t.Fatalf("a deleted monitor's job came back: %v", err)
	}
}

func TestModelDeploymentMonitoringJob(t *testing.T) {
	s, runner, parent, ep := monitorServer(t)
	ctx := context.Background()
	runner.objects["b/train.csv"] = []byte("x,color\n1,red\n2,red\n3,blue\n4,blue\n")
	x := &jobService{s: s}
	job := &aiplatformpb.ModelDeploymentMonitoringJob{DisplayName: "watch", Endpoint: ep,
		ModelDeploymentMonitoringObjectiveConfigs: []*aiplatformpb.ModelDeploymentMonitoringObjectiveConfig{{DeployedModelId: "111",
			ObjectiveConfig: &aiplatformpb.ModelMonitoringObjectiveConfig{
				TrainingDataset: &aiplatformpb.ModelMonitoringObjectiveConfig_TrainingDataset{DataFormat: "csv",
					DataSource: &aiplatformpb.ModelMonitoringObjectiveConfig_TrainingDataset_GcsSource{GcsSource: &aiplatformpb.GcsSource{Uris: []string{"gs://b/train.csv"}}}},
				TrainingPredictionSkewDetectionConfig: &aiplatformpb.ModelMonitoringObjectiveConfig_TrainingPredictionSkewDetectionConfig{},
				PredictionDriftDetectionConfig:        &aiplatformpb.ModelMonitoringObjectiveConfig_PredictionDriftDetectionConfig{},
			}}},
		ModelDeploymentMonitoringScheduleConfig: &aiplatformpb.ModelDeploymentMonitoringScheduleConfig{MonitorInterval: durationpb.New(30 * time.Minute)},
	}
	if _, err := x.CreateModelDeploymentMonitoringJob(ctx, &aiplatformpb.CreateModelDeploymentMonitoringJobRequest{Parent: parent, ModelDeploymentMonitoringJob: job}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("a 30-minute interval = %v, want INVALID_ARGUMENT", err)
	}
	job.ModelDeploymentMonitoringScheduleConfig.MonitorInterval = durationpb.New(time.Hour)
	j, err := x.CreateModelDeploymentMonitoringJob(ctx, &aiplatformpb.CreateModelDeploymentMonitoringJobRequest{Parent: parent, ModelDeploymentMonitoringJob: job})
	if err != nil || j.GetState() != aiplatformpb.JobState_JOB_STATE_PENDING || j.GetScheduleState() != aiplatformpb.ModelDeploymentMonitoringJob_OFFLINE ||
		j.GetBigqueryTables()[0].GetBigqueryTablePath() != "bq://p.model_deployment_monitoring_ep1.serving_predict" {
		t.Fatalf("Create = %v, %v", j, err)
	}
	if _, err := x.CreateModelDeploymentMonitoringJob(ctx, &aiplatformpb.CreateModelDeploymentMonitoringJobRequest{Parent: parent, ModelDeploymentMonitoringJob: job}); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("a second job on the endpoint = %v", err)
	}
	ps := &predictionService{s: s}
	for i := 0; i < 4; i++ {
		inst, _ := structpb.NewValue(map[string]any{"x": float64(50 + i), "color": "green"})
		if _, err := ps.Predict(ctx, &aiplatformpb.PredictRequest{Endpoint: ep, Instances: []*structpb.Value{inst}}); err != nil {
			t.Fatal(err)
		}
	}
	s.RunMDMJob(ctx, j.GetName(), time.Now().Add(time.Second))
	res, err := x.SearchModelDeploymentMonitoringStatsAnomalies(ctx, &aiplatformpb.SearchModelDeploymentMonitoringStatsAnomaliesRequest{
		ModelDeploymentMonitoringJob: j.GetName(), DeployedModelId: "111",
		Objectives: []*aiplatformpb.SearchModelDeploymentMonitoringStatsAnomaliesRequest_StatsAnomaliesObjective{{Type: aiplatformpb.ModelDeploymentMonitoringObjectiveType_RAW_FEATURE_SKEW}}})
	if err != nil || len(res.GetMonitoringStats()) != 1 {
		t.Fatalf("Search = %v, %v", res, err)
	}
	skew := res.GetMonitoringStats()[0]
	if skew.GetAnomalyCount() != 2 || len(skew.GetFeatureStats()) != 2 {
		t.Fatalf("skew = %v", skew)
	}
	for _, f := range skew.GetFeatureStats() {
		if f.GetThreshold().GetValue() != defaultMonitorThreshold {
			t.Fatalf("%s threshold = %v", f.GetFeatureDisplayName(), f.GetThreshold())
		}
	}
	if _, err := x.DeleteModelDeploymentMonitoringJob(ctx, &aiplatformpb.DeleteModelDeploymentMonitoringJobRequest{Name: j.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("delete before pause = %v", err)
	}
	if _, err := x.PauseModelDeploymentMonitoringJob(ctx, &aiplatformpb.PauseModelDeploymentMonitoringJobRequest{Name: j.GetName()}); err != nil {
		t.Fatal(err)
	}
	op, err := x.UpdateModelDeploymentMonitoringJob(ctx, &aiplatformpb.UpdateModelDeploymentMonitoringJobRequest{
		ModelDeploymentMonitoringJob: &aiplatformpb.ModelDeploymentMonitoringJob{Name: j.GetName(), DisplayName: "renamed"},
		UpdateMask:                   &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}})
	if err != nil || !op.GetDone() {
		t.Fatalf("Update = %v, %v", op, err)
	}
	if g, _ := x.GetModelDeploymentMonitoringJob(ctx, &aiplatformpb.GetModelDeploymentMonitoringJobRequest{Name: j.GetName()}); g.GetDisplayName() != "renamed" ||
		g.GetState() != aiplatformpb.JobState_JOB_STATE_PAUSED || g.GetLatestMonitoringPipelineMetadata().GetStatus().GetCode() != 0 {
		t.Fatalf("Get = %v", g)
	}
	if _, err := x.DeleteModelDeploymentMonitoringJob(ctx, &aiplatformpb.DeleteModelDeploymentMonitoringJobRequest{Name: j.GetName()}); err != nil {
		t.Fatal(err)
	}
}

func TestDataLabelingJobsAreShutDown(t *testing.T) {
	x := &jobService{s: NewServer(nil)}
	ctx := context.Background()
	if _, err := x.CreateDataLabelingJob(ctx, &aiplatformpb.CreateDataLabelingJobRequest{Parent: "projects/p/locations/us-central1",
		DataLabelingJob: &aiplatformpb.DataLabelingJob{DisplayName: "d"}}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("Create = %v", err)
	}
	if l, err := x.ListDataLabelingJobs(ctx, &aiplatformpb.ListDataLabelingJobsRequest{Parent: "projects/p/locations/us-central1"}); err != nil || len(l.GetDataLabelingJobs()) != 0 {
		t.Fatalf("List = %v, %v", l, err)
	}
	if _, err := x.GetDataLabelingJob(ctx, &aiplatformpb.GetDataLabelingJobRequest{Name: "projects/p/locations/us-central1/dataLabelingJobs/1"}); status.Code(err) != codes.NotFound {
		t.Fatalf("Get = %v", err)
	}
}
