package gke

import (
	"context"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Logging and monitoring (#2100): a cluster's logs go to Cloud Logging and
// its system metrics to Cloud Monitoring while they are enabled, as GKE
// ships them; the shippers are cmd/cloudburrow's gkelogs.go and
// gkemetrics.go, which read Telemetry.
//
// The services, from the REST reference (re-read 2026-11-21,
// https://docs.cloud.google.com/kubernetes-engine/docs/reference/rest/v1/projects.locations.clusters/setLogging):
// "logging.googleapis.com/kubernetes", "none", and the legacy
// "logging.googleapis.com", "no longer available as of GKE 1.15"; an empty
// value is logging.googleapis.com/kubernetes. Monitoring's are the same
// with monitoring.googleapis.com.
//
// The components: logging ships SYSTEM_COMPONENTS (the system namespaces'
// pods: "kube-system, istio-system, knative-serving, gke-system, and
// config-management-system", re-read 2026-11-21:
// https://docs.cloud.google.com/kubernetes-engine/docs/concepts/about-logs),
// WORKLOADS (every other namespace's), and APISERVER, SCHEDULER and
// CONTROLLER_MANAGER (the control plane's own); monitoring ships
// SYSTEM_COMPONENTS, the kubelet's readings ("If you choose to send
// metrics to Cloud Monitoring, you must send system metrics", re-read
// 2026-11-21:
// https://docs.cloud.google.com/kubernetes-engine/docs/how-to/config-logging-monitoring).
// Monitoring's other components are Google's metric packages, each
// collected through Managed Service for Prometheus into
// prometheus.googleapis.com metrics on the prometheus_target resource
// (re-read 2026-11-22: how-to/configure-metrics, how-to/control-plane-metrics,
// how-to/kube-state-metrics, how-to/cadvisor-kubelet-metrics): the control
// plane's (APISERVER, SCHEDULER, CONTROLLER_MANAGER), kube state metrics
// (STORAGE, POD, DEPLOYMENT, STATEFULSET, DAEMONSET, HPA, JOBSET, which
// need managed collection), cAdvisor and the kubelet's (CADVISOR, KUBELET,
// which need it too) and NVIDIA DCGM (DCGM, GPU nodes', of which a local
// cluster has none). The collector is cmd/cloudburrow's gkeprom.go.
// Logging's other components have no local source and answer
// UNIMPLEMENTED.

const (
	loggingKubernetes    = "logging.googleapis.com/kubernetes"
	loggingLegacy        = "logging.googleapis.com"
	monitoringKubernetes = "monitoring.googleapis.com/kubernetes"
	monitoringLegacy     = "monitoring.googleapis.com"
	telemetryNone        = "none"
)

// servedLogging are the logging components shipped locally.
var servedLogging = map[containerpb.LoggingComponentConfig_Component]bool{
	containerpb.LoggingComponentConfig_SYSTEM_COMPONENTS:  true,
	containerpb.LoggingComponentConfig_WORKLOADS:          true,
	containerpb.LoggingComponentConfig_APISERVER:          true,
	containerpb.LoggingComponentConfig_SCHEDULER:          true,
	containerpb.LoggingComponentConfig_CONTROLLER_MANAGER: true,
}

// kubeStateComponents are the kube state metrics packages.
var kubeStateComponents = map[containerpb.MonitoringComponentConfig_Component]bool{
	containerpb.MonitoringComponentConfig_STORAGE: true, containerpb.MonitoringComponentConfig_POD: true,
	containerpb.MonitoringComponentConfig_DEPLOYMENT: true, containerpb.MonitoringComponentConfig_STATEFULSET: true,
	containerpb.MonitoringComponentConfig_DAEMONSET: true, containerpb.MonitoringComponentConfig_HPA: true,
	containerpb.MonitoringComponentConfig_JOBSET: true,
}

// defaultMonitoring are a new cluster's monitoring components: system
// metrics, and, managed collection being on by default (Standard 1.27,
// Autopilot 1.25), kube state metrics ("on by default for Standard
// clusters from 1.29.2-gke.2000"; JobSet's "by default on new Standard
// and Autopilot clusters" from 1.32.1-gke.1357001) and cAdvisor/Kubelet
// ("enabled by default when you create a cluster running version
// 1.29.3-gke.1093000 or later"), re-read 2026-11-22.
var defaultMonitoring = []containerpb.MonitoringComponentConfig_Component{
	containerpb.MonitoringComponentConfig_SYSTEM_COMPONENTS,
	containerpb.MonitoringComponentConfig_STORAGE, containerpb.MonitoringComponentConfig_HPA,
	containerpb.MonitoringComponentConfig_POD, containerpb.MonitoringComponentConfig_DAEMONSET,
	containerpb.MonitoringComponentConfig_DEPLOYMENT, containerpb.MonitoringComponentConfig_STATEFULSET,
	containerpb.MonitoringComponentConfig_JOBSET,
	containerpb.MonitoringComponentConfig_CADVISOR, containerpb.MonitoringComponentConfig_KUBELET,
}

// SystemNamespaces are the namespaces whose pods' logs are system logs.
var SystemNamespaces = map[string]bool{
	"kube-system": true, "istio-system": true, "knative-serving": true,
	"gke-system": true, "config-management-system": true,
}

// TelemetryTarget is a running cluster and what is shipped of it.
type TelemetryTarget struct {
	// Name is the cluster's resource name; Project, Location and Cluster
	// its parts, the k8s_* resources' labels.
	Name, Project, Location, Cluster string
	// Kind is the backing kind cluster's name.
	Kind string
	// SystemLogs, WorkloadLogs and ControlPlaneLogs (by component name:
	// apiserver, scheduler, controller-manager) are the logs shipped.
	SystemLogs, WorkloadLogs bool
	ControlPlaneLogs         map[string]bool
	// SystemMetrics is whether system metrics are shipped.
	SystemMetrics bool
	// ControlPlaneMetrics are the control plane's metrics packages on (by
	// component: apiserver, scheduler, controller-manager); KubeState the
	// kube state metrics packages on (by component name: STORAGE, POD, ...);
	// CAdvisor, Kubelet and DCGM the others.
	ControlPlaneMetrics     map[string]bool
	KubeState               map[string]bool
	CAdvisor, Kubelet, DCGM bool
	// ManagedPrometheus is whether managed collection is on: the
	// PodMonitoring and ClusterPodMonitoring resources are served and
	// their targets scraped.
	ManagedPrometheus bool
	// LoggingChanged is when the cluster's logging was last changed by
	// this server, zero for none since it started: a log category turned
	// on is shipped from then, so a line a pod writes as soon as the
	// change's operation is done is not lost to the shipper's next sweep.
	LoggingChanged time.Time
}

// Telemetry lists the running clusters and what of each is shipped.
func (s *Server) Telemetry() []TelemetryTarget {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []TelemetryTarget
	for name, e := range s.clusters {
		c := e.cluster
		if e.access.Endpoint == "" {
			continue
		}
		switch c.GetStatus() {
		case containerpb.Cluster_RUNNING, containerpb.Cluster_RECONCILING, containerpb.Cluster_DEGRADED:
		default:
			continue
		}
		t := TelemetryTarget{Name: name, Project: e.project, Location: c.GetLocation(), Cluster: c.GetName(), Kind: e.kind,
			ControlPlaneLogs: map[string]bool{}, LoggingChanged: e.loggingChanged}
		if c.GetLoggingService() != telemetryNone {
			for _, comp := range c.GetLoggingConfig().GetComponentConfig().GetEnableComponents() {
				switch comp {
				case containerpb.LoggingComponentConfig_SYSTEM_COMPONENTS:
					t.SystemLogs = true
				case containerpb.LoggingComponentConfig_WORKLOADS:
					t.WorkloadLogs = true
				case containerpb.LoggingComponentConfig_APISERVER:
					t.ControlPlaneLogs["apiserver"] = true
				case containerpb.LoggingComponentConfig_SCHEDULER:
					t.ControlPlaneLogs["scheduler"] = true
				case containerpb.LoggingComponentConfig_CONTROLLER_MANAGER:
					t.ControlPlaneLogs["controller-manager"] = true
				}
			}
		}
		t.ControlPlaneMetrics, t.KubeState = map[string]bool{}, map[string]bool{}
		if c.GetMonitoringService() != telemetryNone {
			for _, comp := range c.GetMonitoringConfig().GetComponentConfig().GetEnableComponents() {
				switch comp {
				case containerpb.MonitoringComponentConfig_SYSTEM_COMPONENTS:
					t.SystemMetrics = true
				case containerpb.MonitoringComponentConfig_APISERVER:
					t.ControlPlaneMetrics["apiserver"] = true
				case containerpb.MonitoringComponentConfig_SCHEDULER:
					t.ControlPlaneMetrics["scheduler"] = true
				case containerpb.MonitoringComponentConfig_CONTROLLER_MANAGER:
					t.ControlPlaneMetrics["controller-manager"] = true
				case containerpb.MonitoringComponentConfig_CADVISOR:
					t.CAdvisor = true
				case containerpb.MonitoringComponentConfig_KUBELET:
					t.Kubelet = true
				case containerpb.MonitoringComponentConfig_DCGM:
					t.DCGM = true
				default:
					if kubeStateComponents[comp] {
						t.KubeState[comp.String()] = true
					}
				}
			}
		}
		t.ManagedPrometheus = c.GetMonitoringConfig().GetManagedPrometheusConfig().GetEnabled()
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// defaultTelemetry gives a new cluster GKE's defaults, and keeps a
// cluster's services and component lists in step: system and workload
// logs, and system metrics, with the Kubernetes-native services. A
// cluster made with one of the two (a service or a config) has the other
// follow it.
func defaultTelemetry(c *containerpb.Cluster) error {
	if err := checkLoggingService(c.GetLoggingService()); err != nil {
		return err
	}
	if err := checkMonitoringService(c.GetMonitoringService()); err != nil {
		return err
	}
	if lc := c.GetLoggingConfig().GetComponentConfig(); lc != nil {
		if err := checkLoggingComponents(lc.GetEnableComponents()); err != nil {
			return err
		}
		c.LoggingService = loggingServiceFor(lc.GetEnableComponents())
	} else {
		c.LoggingConfig = loggingConfigFor(c.GetLoggingService())
	}
	if c.GetMonitoringConfig() == nil {
		c.MonitoringConfig = &containerpb.MonitoringConfig{}
	}
	if c.MonitoringConfig.ManagedPrometheusConfig == nil {
		// Managed collection is on by default.
		c.MonitoringConfig.ManagedPrometheusConfig = &containerpb.ManagedPrometheusConfig{Enabled: c.GetMonitoringService() != telemetryNone}
	}
	if err := checkManagedPrometheus(c); err != nil {
		return err
	}
	if mc := c.GetMonitoringConfig().GetComponentConfig(); mc != nil {
		if err := checkMonitoringComponents(c, mc.GetEnableComponents()); err != nil {
			return err
		}
		c.MonitoringService = monitoringServiceFor(mc.GetEnableComponents())
	} else {
		c.MonitoringConfig.ComponentConfig = monitoringComponentsFor(c.GetMonitoringService())
		if c.GetMonitoringService() != telemetryNone && c.MonitoringConfig.GetManagedPrometheusConfig().GetEnabled() {
			c.MonitoringConfig.ComponentConfig.EnableComponents = append([]containerpb.MonitoringComponentConfig_Component(nil), defaultMonitoring...)
		}
	}
	if c.LoggingService == "" {
		c.LoggingService = loggingKubernetes
	}
	if c.MonitoringService == "" {
		c.MonitoringService = monitoringKubernetes
	}
	if c.GetAutopilot().GetEnabled() && c.MonitoringService == telemetryNone {
		return status.Error(codes.InvalidArgument, "For GKE Autopilot clusters, you cannot disable the collection of system metrics.")
	}
	return nil
}

func checkLoggingService(v string) error {
	switch v {
	case "", loggingKubernetes, telemetryNone:
		return nil
	case loggingLegacy:
		return status.Error(codes.InvalidArgument, "The legacy Cloud Logging service (logging.googleapis.com) is no longer available as of GKE 1.15: use logging.googleapis.com/kubernetes.")
	}
	return status.Errorf(codes.InvalidArgument, "Invalid logging service %q: use logging.googleapis.com/kubernetes or none.", v)
}

func checkMonitoringService(v string) error {
	switch v {
	case "", monitoringKubernetes, telemetryNone:
		return nil
	case monitoringLegacy:
		return status.Error(codes.InvalidArgument, "The legacy Cloud Monitoring service (monitoring.googleapis.com) is no longer available as of GKE 1.15: use monitoring.googleapis.com/kubernetes.")
	}
	return status.Errorf(codes.InvalidArgument, "Invalid monitoring service %q: use monitoring.googleapis.com/kubernetes or none.", v)
}

func checkLoggingComponents(comps []containerpb.LoggingComponentConfig_Component) error {
	var other []string
	system := false
	for _, c := range comps {
		if c == containerpb.LoggingComponentConfig_SYSTEM_COMPONENTS {
			system = true
		}
		if !servedLogging[c] {
			other = append(other, c.String())
		}
	}
	if len(other) > 0 {
		return status.Errorf(codes.Unimplemented, "Not available locally: logging of %s.", strings.Join(other, ", "))
	}
	if len(comps) > 0 && !system {
		// UNVERIFIED wording: the other components are shipped beside the
		// system logs (docs re-read 2026-11-21: nothing more is published).
		return status.Error(codes.InvalidArgument, "SYSTEM_COMPONENTS logging must be enabled to enable other logging components.")
	}
	return nil
}

// checkManagedPrometheus refuses turning managed collection off where GKE
// does: on Autopilot ("You can't turn off managed collection", Autopilot
// 1.25 or later, re-read 2026-11-22: managed-prometheus/setup-managed).
func checkManagedPrometheus(c *containerpb.Cluster) error {
	if c.GetAutopilot().GetEnabled() && !c.GetMonitoringConfig().GetManagedPrometheusConfig().GetEnabled() {
		// UNVERIFIED wording, re-read 2026-11-22: managed-prometheus/setup-managed.
		return status.Error(codes.InvalidArgument, "For GKE Autopilot clusters, you can't turn off Managed Service for Prometheus managed collection.")
	}
	return nil
}

func checkMonitoringComponents(c *containerpb.Cluster, comps []containerpb.MonitoringComponentConfig_Component) error {
	var managed []string
	system := false
	for _, m := range comps {
		switch {
		case m == containerpb.MonitoringComponentConfig_SYSTEM_COMPONENTS:
			system = true
		case kubeStateComponents[m], m == containerpb.MonitoringComponentConfig_CADVISOR, m == containerpb.MonitoringComponentConfig_KUBELET:
			managed = append(managed, m.String())
		case m == containerpb.MonitoringComponentConfig_COMPONENT_UNSPECIFIED:
			return status.Error(codes.InvalidArgument, "monitoring_config.component_config.enable_components: COMPONENT_UNSPECIFIED is not a component.")
		}
	}
	if len(comps) > 0 && !system {
		return status.Error(codes.InvalidArgument, "If you choose to send metrics to Cloud Monitoring, you must send system metrics (SYSTEM_COMPONENTS).")
	}
	if len(managed) > 0 && !c.GetMonitoringConfig().GetManagedPrometheusConfig().GetEnabled() {
		// Documented requirement ("Google Cloud Managed Service for
		// Prometheus managed collection must be enabled", how-to/kube-state-metrics
		// and how-to/cadvisor-kubelet-metrics, re-read 2026-11-22);
		// UNVERIFIED wording.
		return status.Errorf(codes.InvalidArgument, "Monitoring of %s needs Managed Service for Prometheus managed collection: enable it (managed_prometheus_config.enabled).", strings.Join(managed, ", "))
	}
	if len(comps) == 0 && c.GetAutopilot().GetEnabled() {
		return status.Error(codes.InvalidArgument, "For GKE Autopilot clusters, you cannot disable the collection of system metrics.")
	}
	return nil
}

func loggingServiceFor(comps []containerpb.LoggingComponentConfig_Component) string {
	if len(comps) == 0 {
		return telemetryNone
	}
	return loggingKubernetes
}

func monitoringServiceFor(comps []containerpb.MonitoringComponentConfig_Component) string {
	if len(comps) == 0 {
		return telemetryNone
	}
	return monitoringKubernetes
}

func loggingConfigFor(service string) *containerpb.LoggingConfig {
	cfg := &containerpb.LoggingConfig{ComponentConfig: &containerpb.LoggingComponentConfig{}}
	if service != telemetryNone {
		cfg.ComponentConfig.EnableComponents = []containerpb.LoggingComponentConfig_Component{
			containerpb.LoggingComponentConfig_SYSTEM_COMPONENTS, containerpb.LoggingComponentConfig_WORKLOADS}
	}
	return cfg
}

func monitoringComponentsFor(service string) *containerpb.MonitoringComponentConfig {
	cfg := &containerpb.MonitoringComponentConfig{}
	if service != telemetryNone {
		cfg.EnableComponents = []containerpb.MonitoringComponentConfig_Component{containerpb.MonitoringComponentConfig_SYSTEM_COMPONENTS}
	}
	return cfg
}

// telemetryChange is a change of a cluster's logging or monitoring.
type telemetryChange struct {
	loggingService, monitoringService string
	logging                           *containerpb.LoggingConfig
	monitoring                        *containerpb.MonitoringConfig
}

// telemetryFields are the ClusterUpdate fields of a telemetryChange.
var telemetryFields = map[string]bool{
	"desired_logging_service": true, "desired_monitoring_service": true,
	"desired_logging_config": true, "desired_monitoring_config": true,
}

// telemetryUpdate is u's logging and monitoring change, when u has one;
// set names u's fields. A telemetry change is the only change of its
// update.
func telemetryUpdate(u *containerpb.ClusterUpdate, set []string) (*telemetryChange, error) {
	n := 0
	for _, f := range set {
		if telemetryFields[f] {
			n++
		}
	}
	if n == 0 {
		return nil, nil
	}
	if n != len(set) {
		return nil, status.Error(codes.InvalidArgument, "The logging and monitoring fields must be the only changes of their update.")
	}
	return &telemetryChange{loggingService: u.GetDesiredLoggingService(), monitoringService: u.GetDesiredMonitoringService(),
		logging: u.GetDesiredLoggingConfig(), monitoring: u.GetDesiredMonitoringConfig()}, nil
}

// SetLoggingService sets the cluster's logging service: none stops its
// logs from being shipped.
func (s *Server) SetLoggingService(_ context.Context, req *containerpb.SetLoggingServiceRequest) (*containerpb.Operation, error) {
	p, err := parseName(req.GetName(), req.GetProjectId(), req.GetZone(), req.GetClusterId(), "", "", "cluster")
	if err != nil {
		return nil, err
	}
	v := req.GetLoggingService()
	if v == "" {
		v = loggingKubernetes
	}
	return s.changeTelemetry(p, &telemetryChange{loggingService: v})
}

// SetMonitoringService sets the cluster's monitoring service: none stops
// its metrics from being shipped.
func (s *Server) SetMonitoringService(_ context.Context, req *containerpb.SetMonitoringServiceRequest) (*containerpb.Operation, error) {
	p, err := parseName(req.GetName(), req.GetProjectId(), req.GetZone(), req.GetClusterId(), "", "", "cluster")
	if err != nil {
		return nil, err
	}
	v := req.GetMonitoringService()
	if v == "" {
		v = monitoringKubernetes
	}
	return s.changeTelemetry(p, &telemetryChange{monitoringService: v})
}

// changeTelemetry checks and applies ch in an UPDATE_CLUSTER operation;
// the shippers follow the record.
func (s *Server) changeTelemetry(p parsed, ch *telemetryChange) (*containerpb.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.readyLocked(p)
	if err != nil {
		return nil, err
	}
	next := proto.Clone(e.cluster).(*containerpb.Cluster)
	if ch.loggingService != "" {
		if err := checkLoggingService(ch.loggingService); err != nil {
			return nil, err
		}
		next.LoggingService = ch.loggingService
		next.LoggingConfig = loggingConfigFor(ch.loggingService)
	}
	if ch.monitoringService != "" {
		if err := checkMonitoringService(ch.monitoringService); err != nil {
			return nil, err
		}
		next.MonitoringService = ch.monitoringService
		if next.MonitoringConfig == nil {
			next.MonitoringConfig = &containerpb.MonitoringConfig{}
		}
		next.MonitoringConfig.ComponentConfig = monitoringComponentsFor(ch.monitoringService)
	}
	if lc := ch.logging.GetComponentConfig(); lc != nil {
		if err := checkLoggingComponents(lc.GetEnableComponents()); err != nil {
			return nil, err
		}
		next.LoggingConfig = &containerpb.LoggingConfig{ComponentConfig: proto.Clone(lc).(*containerpb.LoggingComponentConfig)}
		next.LoggingService = loggingServiceFor(lc.GetEnableComponents())
	}
	if m := ch.monitoring; m != nil && m.GetAdvancedDatapathObservabilityConfig() != nil {
		return nil, status.Error(codes.Unimplemented, "Not available locally: advanced datapath observability (GKE Dataplane V2's Hubble).")
	}
	if mp := ch.monitoring.GetManagedPrometheusConfig(); mp != nil {
		if next.MonitoringConfig == nil {
			next.MonitoringConfig = &containerpb.MonitoringConfig{}
		}
		next.MonitoringConfig.ManagedPrometheusConfig = proto.Clone(mp).(*containerpb.ManagedPrometheusConfig)
		if err := checkManagedPrometheus(next); err != nil {
			return nil, err
		}
	}
	if mc := ch.monitoring.GetComponentConfig(); mc != nil {
		if next.MonitoringConfig == nil {
			next.MonitoringConfig = &containerpb.MonitoringConfig{}
		}
		next.MonitoringConfig.ComponentConfig = proto.Clone(mc).(*containerpb.MonitoringComponentConfig)
		next.MonitoringService = monitoringServiceFor(mc.GetEnableComponents())
	}
	// The packages managed collection serves need it, whichever of the
	// two the update changed.
	if err := checkMonitoringComponents(next, next.GetMonitoringConfig().GetComponentConfig().GetEnableComponents()); err != nil && (ch.monitoring != nil) {
		return nil, err
	}
	if next.GetAutopilot().GetEnabled() && next.MonitoringService == telemetryNone {
		return nil, status.Error(codes.InvalidArgument, "For GKE Autopilot clusters, you cannot disable the collection of system metrics.")
	}
	c := e.cluster
	return s.startSettingLocked(p, e, containerpb.Operation_UPDATE_CLUSTER, func(context.Context) error { return nil }, func() {
		if !proto.Equal(c.GetLoggingConfig(), next.LoggingConfig) || c.GetLoggingService() != next.LoggingService {
			e.loggingChanged = s.now()
		}
		c.LoggingService, c.LoggingConfig = next.LoggingService, next.LoggingConfig
		c.MonitoringService, c.MonitoringConfig = next.MonitoringService, next.MonitoringConfig
	}), nil
}
