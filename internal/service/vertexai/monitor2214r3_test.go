package vertexai

import (
	"context"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	betapb "cloud.google.com/go/aiplatform/apiv1beta1/aiplatformpb"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestMonitoringAnomaliesAreLogged(t *testing.T) {
	s := NewServer(nil)
	var mu sync.Mutex
	var entries []*loggingpb.LogEntry
	s.SetAnomalyLogger(func(_ context.Context, e *loggingpb.LogEntry) error {
		mu.Lock()
		defer mu.Unlock()
		entries = append(entries, e)
		return nil
	})
	at := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	j := &aiplatformpb.ModelDeploymentMonitoringJob{Name: "projects/p/locations/us-central1/modelDeploymentMonitoringJobs/9",
		ModelDeploymentMonitoringScheduleConfig: &aiplatformpb.ModelDeploymentMonitoringScheduleConfig{MonitorInterval: durationpb.New(time.Hour)},
		ModelMonitoringAlertConfig:              &aiplatformpb.ModelMonitoringAlertConfig{EnableLogging: true}}
	skew := aiplatformpb.ModelDeploymentMonitoringObjectiveType_RAW_FEATURE_SKEW
	stats := []*mdmStat{
		{Feature: "x", DeployedModel: "d", Objective: skew, Threshold: 0.3, Prediction: &aiplatformpb.FeatureStatsAnomaly{Score: 0.9}},
		{Feature: "w", DeployedModel: "d", Objective: skew, Threshold: 0.3, Prediction: &aiplatformpb.FeatureStatsAnomaly{Score: 0.5}},
		{Feature: "y", DeployedModel: "d", Objective: skew, Threshold: 0.3, Prediction: &aiplatformpb.FeatureStatsAnomaly{Score: 0.1}},
	}
	s.notifyMDMAnomalies(context.Background(), j, at, stats)
	if len(entries) != 1 {
		t.Fatalf("%d entries, want 1", len(entries))
	}
	e := entries[0]
	p := e.GetJsonPayload().GetFields()
	if e.GetLogName() != "projects/p/logs/aiplatform.googleapis.com%2Fmodel_monitoring_anomaly" || e.GetSeverity().String() != "WARNING" ||
		e.GetResource().GetType() != "aiplatform.googleapis.com/ModelDeploymentMonitoringJob" ||
		e.GetResource().GetLabels()["model_deployment_monitoring_job"] != "9" || e.GetResource().GetLabels()["location"] != "us-central1" ||
		p["anomalyObjective"].GetStringValue() != "RAW_FEATURE_SKEW" || p["totalAnomaliesCount"].GetNumberValue() != 2 ||
		p["deployedModelId"].GetStringValue() != "d" || p["modelDeploymentMonitoringJob"].GetStringValue() != j.GetName() ||
		p["startTime"].GetStringValue() != "2026-11-24T11:00:00Z" || p["endTime"].GetStringValue() != "2026-11-24T12:00:00Z" {
		t.Fatalf("entry = %v", e)
	}
	fa := p["featureAnomalies"].GetListValue().GetValues()
	if len(fa) != 2 || fa[0].GetStructValue().GetFields()["featureDisplayName"].GetStringValue() != "w" ||
		fa[1].GetStructValue().GetFields()["deviation"].GetNumberValue() != 0.9 {
		t.Fatalf("featureAnomalies = %v", fa)
	}
	// Without enable_logging nothing is written.
	j.ModelMonitoringAlertConfig.EnableLogging = false
	s.notifyMDMAnomalies(context.Background(), j, at, stats)
	if len(entries) != 1 {
		t.Fatalf("an entry was written without enable_logging")
	}

	// v2.
	job := "projects/p/locations/us-central1/modelMonitors/m1/modelMonitoringJobs/7"
	body, _ := structpb.NewValue(map[string]any{"distance": 0.5, "threshold": 0.2})
	alert := &mmAlert{Alert: &betapb.ModelMonitoringAlert{StatsName: "plan", ObjectiveType: objFeatureDrift, AlertTime: timestamppb.New(at),
		Anomaly: &betapb.ModelMonitoringAnomaly{Anomaly: &betapb.ModelMonitoringAnomaly_TabularAnomaly_{TabularAnomaly: &betapb.ModelMonitoringAnomaly_TabularAnomaly{Anomaly: body}}}}}
	s.notifyMonitorAlerts(context.Background(), job, &betapb.ModelMonitoringNotificationSpec{EnableCloudLogging: true}, []*mmAlert{alert})
	if len(entries) != 2 {
		t.Fatalf("%d entries after the v2 run, want 2", len(entries))
	}
	v2 := entries[1].GetJsonPayload().GetFields()
	if v2["modelMonitoringJob"].GetStringValue() != job || v2["anomalyObjective"].GetStringValue() != objFeatureDrift ||
		entries[1].GetResource().GetLabels()["model_monitor"] != "m1" ||
		v2["featureAnomalies"].GetListValue().GetValues()[0].GetStructValue().GetFields()["threshold"].GetNumberValue() != 0.2 {
		t.Fatalf("v2 entry = %v", entries[1])
	}
}
