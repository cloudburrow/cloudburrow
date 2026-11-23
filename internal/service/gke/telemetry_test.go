package gke

import (
	"context"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"
)

// TestLoggingAndMonitoring (#2100): a new cluster ships system and workload
// logs and system metrics; the services and the component lists change
// together, through SetLoggingService, SetMonitoringService and
// UpdateCluster, and Telemetry follows them.
func TestLoggingAndMonitoring(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	s, _ := newCtlServer(t, &now)
	ctx := context.Background()
	const loc = "us-central1-a"
	op, err := s.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: "projects/p/locations/" + loc,
		Cluster: &containerpb.Cluster{Name: "c1", InitialNodeCount: 1}})
	mustDone(t, s, loc, op, err)
	name := "projects/p/locations/" + loc + "/clusters/c1"
	get := func() *containerpb.Cluster {
		t.Helper()
		c, err := s.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	target := func() TelemetryTarget {
		t.Helper()
		ts := s.Telemetry()
		if len(ts) != 1 {
			t.Fatalf("Telemetry = %+v, want one cluster", ts)
		}
		return ts[0]
	}
	c := get()
	if c.LoggingService != loggingKubernetes || c.MonitoringService != monitoringKubernetes {
		t.Errorf("services = %q, %q", c.LoggingService, c.MonitoringService)
	}
	if got := c.GetLoggingConfig().GetComponentConfig().GetEnableComponents(); len(got) != 2 {
		t.Errorf("logging components = %v, want SYSTEM_COMPONENTS and WORKLOADS", got)
	}
	if tt := target(); !tt.SystemLogs || !tt.WorkloadLogs || !tt.SystemMetrics || tt.Kind == "" || tt.Project != "p" || tt.Cluster != "c1" ||
		!tt.LoggingChanged.IsZero() {
		t.Errorf("target = %+v", tt)
	}

	_, err = s.SetLoggingService(ctx, &containerpb.SetLoggingServiceRequest{Name: name, LoggingService: "logging.googleapis.com"})
	wantCode(t, "the legacy logging service", err, codes.InvalidArgument)
	op, err = s.SetLoggingService(ctx, &containerpb.SetLoggingServiceRequest{Name: name, LoggingService: "none"})
	if got := mustDone(t, s, loc, op, err); got.OperationType != containerpb.Operation_UPDATE_CLUSTER {
		t.Errorf("operation type = %v", got.OperationType)
	}
	if c := get(); c.LoggingService != "none" || len(c.GetLoggingConfig().GetComponentConfig().GetEnableComponents()) != 0 {
		t.Errorf("after none: %q %v", c.LoggingService, c.GetLoggingConfig())
	}
	if tt := target(); tt.SystemLogs || tt.WorkloadLogs {
		t.Errorf("logs still shipped: %+v", tt)
	}
	// The change's time is the shippers' start for what it turns on: the
	// wall clock, which the container runtime stamps its lines with, even
	// once the maintenance windows' clock has been moved forward.
	now = now.Add(time.Minute)
	if _, err := s.AdvanceClock(72 * time.Hour); err != nil {
		t.Fatal(err)
	}
	op, err = s.SetLoggingService(ctx, &containerpb.SetLoggingServiceRequest{Name: name, LoggingService: loggingKubernetes})
	mustDone(t, s, loc, op, err)
	if tt := target(); !tt.LoggingChanged.Equal(now) || !tt.WorkloadLogs {
		t.Errorf("logging back on: changed %v, want the wall clock's %v (%+v)", tt.LoggingChanged, now, tt)
	}
	op, err = s.SetLoggingService(ctx, &containerpb.SetLoggingServiceRequest{Name: name, LoggingService: "none"})
	mustDone(t, s, loc, op, err)

	// gcloud's --logging=SYSTEM: system logs only.
	op, err = s.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name, Update: &containerpb.ClusterUpdate{
		DesiredLoggingConfig: &containerpb.LoggingConfig{ComponentConfig: &containerpb.LoggingComponentConfig{
			EnableComponents: []containerpb.LoggingComponentConfig_Component{containerpb.LoggingComponentConfig_SYSTEM_COMPONENTS}}}}})
	mustDone(t, s, loc, op, err)
	if tt := target(); !tt.SystemLogs || tt.WorkloadLogs {
		t.Errorf("--logging=SYSTEM: %+v", tt)
	}
	if c := get(); c.LoggingService != loggingKubernetes {
		t.Errorf("logging service = %q", c.LoggingService)
	}
	_, err = s.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name, Update: &containerpb.ClusterUpdate{
		DesiredLoggingConfig: &containerpb.LoggingConfig{ComponentConfig: &containerpb.LoggingComponentConfig{
			EnableComponents: []containerpb.LoggingComponentConfig_Component{containerpb.LoggingComponentConfig_WORKLOADS}}}}})
	wantCode(t, "workloads without system", err, codes.InvalidArgument)
	_, err = s.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name, Update: &containerpb.ClusterUpdate{
		DesiredLoggingConfig: &containerpb.LoggingConfig{ComponentConfig: &containerpb.LoggingComponentConfig{
			EnableComponents: []containerpb.LoggingComponentConfig_Component{containerpb.LoggingComponentConfig_SYSTEM_COMPONENTS,
				containerpb.LoggingComponentConfig_KCP_SSHD}}}}})
	wantCode(t, "a component with no local source", err, codes.Unimplemented)
	_, err = s.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name, Update: &containerpb.ClusterUpdate{
		DesiredLoggingService: "none", DesiredImageType: "COS_CONTAINERD"}})
	wantCode(t, "logging beside another change", err, codes.InvalidArgument)

	// Monitoring: none, then back; SYSTEM_COMPONENTS is required.
	op, err = s.SetMonitoringService(ctx, &containerpb.SetMonitoringServiceRequest{Name: name, MonitoringService: "none"})
	mustDone(t, s, loc, op, err)
	if tt := target(); tt.SystemMetrics {
		t.Errorf("metrics still shipped: %+v", tt)
	}
	_, err = s.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name, Update: &containerpb.ClusterUpdate{
		DesiredMonitoringConfig: &containerpb.MonitoringConfig{ComponentConfig: &containerpb.MonitoringComponentConfig{
			EnableComponents: []containerpb.MonitoringComponentConfig_Component{containerpb.MonitoringComponentConfig_POD}}}}})
	wantCode(t, "monitoring without system metrics", err, codes.InvalidArgument)
	op, err = s.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name, Update: &containerpb.ClusterUpdate{
		DesiredMonitoringConfig: &containerpb.MonitoringConfig{ComponentConfig: &containerpb.MonitoringComponentConfig{
			EnableComponents: []containerpb.MonitoringComponentConfig_Component{containerpb.MonitoringComponentConfig_SYSTEM_COMPONENTS}}}}})
	mustDone(t, s, loc, op, err)
	if c := get(); c.MonitoringService != monitoringKubernetes || !target().SystemMetrics {
		t.Errorf("monitoring back on: %q %+v", c.MonitoringService, target())
	}
}

func TestDefaultTelemetry(t *testing.T) {
	c := &containerpb.Cluster{LoggingService: "none"}
	if err := defaultTelemetry(c); err != nil {
		t.Fatal(err)
	}
	if len(c.GetLoggingConfig().GetComponentConfig().GetEnableComponents()) != 0 || c.MonitoringService != monitoringKubernetes {
		t.Errorf("logging none at create: %v", c)
	}
	c = &containerpb.Cluster{Autopilot: &containerpb.Autopilot{Enabled: true}, MonitoringService: "none"}
	wantCode(t, "Autopilot without metrics", defaultTelemetry(c), codes.InvalidArgument)
	c = &containerpb.Cluster{MonitoringService: "monitoring.googleapis.com"}
	wantCode(t, "legacy monitoring", defaultTelemetry(c), codes.InvalidArgument)
}
