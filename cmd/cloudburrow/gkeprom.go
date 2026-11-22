package main

// Kubernetes Engine's metric packages and Managed Service for Prometheus
// (#2100), into Cloud Monitoring's prometheus.googleapis.com metrics.
//
// GKE collects, while each package is on (re-read 2026-11-22:
// https://docs.cloud.google.com/kubernetes-engine/docs/how-to/configure-metrics):
//
//   - the control plane's metrics (APISERVER, SCHEDULER, CONTROLLER_MANAGER):
//     the curated lists of how-to/control-plane-metrics, read from each
//     component's /metrics on the control plane node;
//   - kube state metrics (STORAGE, POD, ... ; gkeksm.go);
//   - cAdvisor's and the kubelet's (CADVISOR, KUBELET): the curated lists
//     of how-to/cadvisor-kubelet-metrics, read from every node's kubelet
//     (/metrics/cadvisor and /metrics, through the API server's node proxy);
//   - with managed collection on, every target its PodMonitoring and
//     ClusterPodMonitoring resources select
//     (https://docs.cloud.google.com/stackdriver/docs/managed-prometheus/setup-managed):
//     the CRDs (monitoring.googleapis.com/v1) and the gmp-system and
//     gmp-public namespaces are put in the cluster; each endpoint of each
//     selected Running pod is scraped at its interval (one minute by
//     default) through the API server's pod proxy, with its
//     metricRelabeling, its targetLabels and the mandatory labels
//     (project_id, location, cluster, namespace, job: the resource's name,
//     instance: POD:PORT); a resource GKE has accepted has its
//     ConfigurationCreateSuccess condition True.
//
// The gkePromCollector does this from this process, as Google's own
// control plane collection is outside the cluster.

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/relabel"

	"github.com/cloudburrow/cloudburrow/internal/images"
	"github.com/cloudburrow/cloudburrow/internal/k8s"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/service/gke"
	"github.com/cloudburrow/cloudburrow/internal/service/monitoring"
)

// gkeControlPlaneMetrics are the control plane packages' curated metrics,
// by component: its port on the control plane node and the names kept.
var gkeControlPlaneMetrics = map[string]struct {
	port int
	job  string
	keep []string
}{
	"apiserver": {6443, "kube-apiserver", []string{
		"apiserver_admission_controller_admission_duration_seconds", "apiserver_admission_step_admission_duration_seconds",
		"apiserver_admission_webhook_admission_duration_seconds", "apiserver_current_inflight_requests",
		"apiserver_flowcontrol_current_executing_seats", "apiserver_flowcontrol_current_inqueue_requests",
		"apiserver_flowcontrol_nominal_limit_seats", "apiserver_flowcontrol_priority_level_seat_utilization",
		"apiserver_flowcontrol_rejected_requests_total", "apiserver_flowcontrol_request_wait_duration_seconds",
		"apiserver_longrunning_requests", "apiserver_request_duration_seconds", "apiserver_request_total",
		"apiserver_resource_size_estimate_bytes", "apiserver_response_sizes", "apiserver_storage_events_received_total",
		"apiserver_storage_objects", "apiserver_storage_size_bytes", "apiserver_watch_events_sizes",
		"apiserver_watch_events_total", "apiserver_watch_list_duration_seconds"}},
	"scheduler": {10259, "kube-scheduler", []string{
		"kube_pod_resource_limit", "kube_pod_resource_request", "scheduler_pending_pods",
		"scheduler_pod_scheduling_duration_seconds", "scheduler_pod_scheduling_sli_duration_seconds",
		"scheduler_preemption_attempts_total", "scheduler_preemption_victims",
		"scheduler_scheduling_attempt_duration_seconds", "scheduler_schedule_attempts_total"}},
	"controller-manager": {10257, "kube-controller-manager", []string{"node_collector_evictions_total"}},
}

// gkeCAdvisorMetrics and gkeKubeletMetrics are the cAdvisor/Kubelet
// packages' curated metrics. The container_pressure_* ones are GKE 1.37's
// and the served versions' cAdvisor has them only where the kernel has
// pressure stall information.
var (
	gkeCAdvisorMetrics = []string{
		"container_cpu_cfs_periods_total", "container_cpu_cfs_throttled_periods_total", "container_cpu_usage_seconds_total",
		"container_fs_limit_bytes", "container_fs_read_seconds_total", "container_fs_reads_bytes_total", "container_fs_reads_total",
		"container_fs_usage_bytes", "container_fs_write_seconds_total", "container_fs_writes_bytes_total", "container_fs_writes_total",
		"container_memory_rss", "container_memory_working_set_bytes", "container_network_receive_bytes_total",
		"container_network_receive_packets_dropped_total", "container_network_receive_packets_total",
		"container_network_transmit_bytes_total", "container_network_transmit_packets_dropped_total",
		"container_network_transmit_packets_total", "container_pressure_cpu_stalled_seconds_total",
		"container_pressure_cpu_waiting_seconds_total", "container_pressure_io_stalled_seconds_total",
		"container_pressure_io_waiting_seconds_total", "container_pressure_memory_stalled_seconds_total",
		"container_pressure_memory_waiting_seconds_total"}
	gkeKubeletMetrics = []string{
		"kubelet_certificate_manager_server_ttl_seconds", "kubelet_node_name", "kubelet_pleg_relist_duration_seconds",
		"kubelet_pod_worker_duration_seconds", "kubelet_running_containers", "kubelet_running_pods",
		"kubelet_runtime_operations_total", "kubelet_volume_stats_available_bytes", "kubelet_volume_stats_capacity_bytes",
		"kubelet_volume_stats_inodes", "kubelet_volume_stats_inodes_free", "kubelet_volume_stats_inodes_used",
		"kubelet_volume_stats_used_bytes"}
)

func keepSet(names []string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

// gkePromCollector collects the served clusters' metric packages and
// managed collection's targets.
type gkePromCollector struct {
	targets func() []gke.TelemetryTarget
	kubectl func(ctx context.Context, kind string, args ...string) (string, error)
	// apply applies a manifest to a kind cluster.
	apply  func(ctx context.Context, kind, manifest string) error
	docker func(ctx context.Context, args ...string) (string, error)
	record func(...monitoring.PromPoint)
	every  time.Duration
	now    func() time.Time

	starts *promStarts
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
	// due is when each source is read next, by key.
	due map[string]time.Time
	// crds are the clusters (by kind name) the CRDs were applied to.
	crds map[string]bool
}

func newGKEPromCollector(dir string, gkeSvc *gkeService, mon *monitoringService) *gkePromCollector {
	if gkeSvc == nil || mon == nil {
		return nil
	}
	kubectl := gkeKubectl(dir)
	return &gkePromCollector{
		targets: func() []gke.TelemetryTarget {
			if api := gkeSvc.API(); api != nil {
				return api.Telemetry()
			}
			return nil
		},
		kubectl: kubectl,
		apply: func(ctx context.Context, kind, manifest string) error {
			return kubeRunner(filepath.Join(dir, kind, "kubeconfig"), "").Apply(ctx, manifest, k8s.ApplyOptions{ServerSide: true, ForceConflicts: true})
		},
		docker: func(ctx context.Context, args ...string) (string, error) {
			return images.ExecRunner{}.Run(ctx, "docker", args...)
		},
		record: mon.promMetrics().Record,
		every:  gkeMetricsInterval(),
		now:    time.Now,
		starts: newPromStarts(),
		due:    map[string]time.Time{},
		crds:   map[string]bool{},
	}
}

func (c *gkePromCollector) Name() string { return "gke-prometheus" }

func (c *gkePromCollector) register(coord *lifecycle.Coordinator) {
	if c != nil {
		coord.Register(c)
	}
}

// promTick is how often the collector looks for what is due.
const promTick = 2 * time.Second

func (c *gkePromCollector) Start(ctx context.Context) error {
	if c == nil {
		return nil
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	c.mu.Lock()
	c.cancel, c.done = cancel, done
	c.mu.Unlock()
	go func() {
		defer close(done)
		t := time.NewTicker(promTick)
		defer t.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-t.C:
				c.sweep(runCtx)
			}
		}
	}()
	return nil
}

func (c *gkePromCollector) Stop(ctx context.Context) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	cancel, done := c.cancel, c.done
	c.cancel = nil
	c.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
	case <-ctx.Done():
	}
	return nil
}

// isDue is whether key is due at now, and if so sets its next time.
func (c *gkePromCollector) isDue(key string, now time.Time, every time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if d, ok := c.due[key]; ok && now.Before(d) {
		return false
	}
	c.due[key] = now.Add(every)
	return true
}

// sweep reads what is due of every cluster.
func (c *gkePromCollector) sweep(ctx context.Context) {
	now := c.now()
	var wg sync.WaitGroup
	for _, t := range c.targets() {
		if !t.SystemMetrics && !t.ManagedPrometheus {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.collect(ctx, t, now)
		}()
	}
	wg.Wait()
}

func (c *gkePromCollector) collect(ctx context.Context, t gke.TelemetryTarget, now time.Time) {
	cctx, cancel := context.WithTimeout(ctx, 50*time.Second)
	defer cancel()
	base := func(job, instance, namespace string) map[string]string {
		return map[string]string{"project_id": t.Project, "location": t.Location, "cluster": t.Cluster,
			"namespace": namespace, "job": job, "instance": instance}
	}
	if t.SystemMetrics && c.isDue(t.Kind+"/packages", now, c.every) {
		cp := t.Kind + "-control-plane"
		for comp, m := range gkeControlPlaneMetrics {
			if !t.ControlPlaneMetrics[comp] {
				continue
			}
			out, err := c.docker(cctx, "exec", cp, "curl", "-sSfk", "--max-time", "20",
				"--cert", "/etc/kubernetes/pki/apiserver-kubelet-client.crt", "--key", "/etc/kubernetes/pki/apiserver-kubelet-client.key",
				"https://127.0.0.1:"+strconv.Itoa(m.port)+"/metrics")
			if err != nil {
				continue
			}
			c.ingest(out, promScrape{resource: base(m.job, cp+":"+strconv.Itoa(m.port), "kube-system"), keep: keepSet(m.keep)}, now)
		}
		if t.CAdvisor || t.Kubelet {
			if raw, err := c.kubectl(cctx, t.Kind, "get", "nodes", "-o", "jsonpath={.items[*].metadata.name}"); err == nil {
				for _, node := range strings.Fields(raw) {
					if t.CAdvisor {
						if out, err := c.kubectl(cctx, t.Kind, "get", "--raw", "/api/v1/nodes/"+node+"/proxy/metrics/cadvisor"); err == nil {
							c.ingest(out, promScrape{resource: base("kubelet", node+":10250", ""), keep: keepSet(gkeCAdvisorMetrics),
								extra: map[string]string{"node": node}}, now)
						}
					}
					if t.Kubelet {
						if out, err := c.kubectl(cctx, t.Kind, "get", "--raw", "/api/v1/nodes/"+node+"/proxy/metrics"); err == nil {
							c.ingest(out, promScrape{resource: base("kubelet", node+":10250", ""), keep: keepSet(gkeKubeletMetrics),
								extra: map[string]string{"node": node}}, now)
						}
					}
				}
			}
		}
		if core, jobset := ksmResources(t.KubeState); core != "" || jobset != "" {
			for _, res := range []string{core, jobset} {
				if res == "" {
					continue
				}
				if raw, err := c.kubectl(cctx, t.Kind, "get", res, "-A", "-o", "json"); err == nil {
					c.record(ksmPoints(t, []byte(raw), now)...)
				}
			}
		}
	}
	if t.ManagedPrometheus {
		c.managed(cctx, t, now, base)
	} else {
		c.mu.Lock()
		delete(c.crds, t.Kind)
		c.mu.Unlock()
	}
}

func (c *gkePromCollector) ingest(text string, sc promScrape, now time.Time) {
	pts, err := promReadings(strings.NewReader(text), sc, c.starts, now)
	if err == nil {
		c.record(pts...)
	}
}

// ---- managed collection ----

// gmpManifest is managed collection's namespaces and CRDs.
//
//go:embed gkegmp.yaml
var gmpManifest string

// gmpMonitoring is what is read of a PodMonitoring or ClusterPodMonitoring.
type gmpMonitoring struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name       string `json:"name"`
		Namespace  string `json:"namespace"`
		Generation int64  `json:"generation"`
	} `json:"metadata"`
	Spec struct {
		Selector  gmpSelector   `json:"selector"`
		Endpoints []gmpEndpoint `json:"endpoints"`
		// TargetLabels: fromPod copies pod labels; metadata names the
		// target's metadata labels kept.
		TargetLabels struct {
			FromPod []struct {
				From string `json:"from"`
				To   string `json:"to"`
			} `json:"fromPod"`
			Metadata *[]string `json:"metadata"`
		} `json:"targetLabels"`
	} `json:"spec"`
	Status struct {
		ObservedGeneration int64 `json:"observedGeneration"`
		Conditions         []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
	} `json:"status"`
}

type gmpSelector struct {
	MatchLabels      map[string]string `json:"matchLabels"`
	MatchExpressions []struct {
		Key      string   `json:"key"`
		Operator string   `json:"operator"`
		Values   []string `json:"values"`
	} `json:"matchExpressions"`
}

func (s gmpSelector) matches(ls map[string]string) bool {
	for k, v := range s.MatchLabels {
		if ls[k] != v {
			return false
		}
	}
	for _, e := range s.MatchExpressions {
		v, has := ls[e.Key]
		in := false
		for _, x := range e.Values {
			if x == v {
				in = true
			}
		}
		switch e.Operator {
		case "In":
			if !has || !in {
				return false
			}
		case "NotIn":
			if has && in {
				return false
			}
		case "Exists":
			if !has {
				return false
			}
		case "DoesNotExist":
			if has {
				return false
			}
		default:
			return false
		}
	}
	return true
}

type gmpEndpoint struct {
	Port             json.RawMessage `json:"port"`
	Path             string          `json:"path"`
	Scheme           string          `json:"scheme"`
	Interval         string          `json:"interval"`
	MetricRelabeling []struct {
		SourceLabels []string `json:"sourceLabels"`
		Separator    *string  `json:"separator"`
		Regex        *string  `json:"regex"`
		Modulus      uint64   `json:"modulus"`
		TargetLabel  string   `json:"targetLabel"`
		Replacement  *string  `json:"replacement"`
		Action       string   `json:"action"`
	} `json:"metricRelabeling"`
}

// port is the endpoint's port, a name or a number.
func (e gmpEndpoint) port() string {
	var n int
	if json.Unmarshal(e.Port, &n) == nil {
		return strconv.Itoa(n)
	}
	var s string
	_ = json.Unmarshal(e.Port, &s)
	return s
}

// interval is the endpoint's scrape interval, a minute by default.
func (e gmpEndpoint) interval() time.Duration {
	if d, err := model.ParseDuration(e.Interval); err == nil && time.Duration(d) >= time.Second {
		return time.Duration(d)
	}
	return time.Minute
}

// relabelConfigs are the endpoint's metricRelabeling rules.
func (e gmpEndpoint) relabelConfigs() ([]*relabel.Config, error) {
	var out []*relabel.Config
	for _, r := range e.MetricRelabeling {
		cfg := relabel.DefaultRelabelConfig
		for _, l := range r.SourceLabels {
			cfg.SourceLabels = append(cfg.SourceLabels, model.LabelName(l))
		}
		if r.Separator != nil {
			cfg.Separator = *r.Separator
		}
		if r.Regex != nil {
			re, err := relabel.NewRegexp(*r.Regex)
			if err != nil {
				return nil, err
			}
			cfg.Regex = re
		}
		cfg.Modulus, cfg.TargetLabel = r.Modulus, r.TargetLabel
		if r.Replacement != nil {
			cfg.Replacement = *r.Replacement
		}
		if r.Action != "" {
			cfg.Action = relabel.Action(strings.ToLower(r.Action))
		}
		cfg.NameValidationScheme = model.UTF8Validation
		if err := cfg.Validate(model.UTF8Validation); err != nil {
			return nil, err
		}
		c := cfg
		out = append(out, &c)
	}
	return out, nil
}

// gmpPod is what is read of a pod.
type gmpPod struct {
	Metadata struct {
		Name            string            `json:"name"`
		Namespace       string            `json:"namespace"`
		Labels          map[string]string `json:"labels"`
		OwnerReferences []struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"ownerReferences"`
	} `json:"metadata"`
	Spec struct {
		NodeName   string `json:"nodeName"`
		Containers []struct {
			Name  string `json:"name"`
			Ports []struct {
				Name          string `json:"name"`
				ContainerPort int    `json:"containerPort"`
			} `json:"ports"`
		} `json:"containers"`
	} `json:"spec"`
	Status struct {
		Phase string `json:"phase"`
		PodIP string `json:"podIP"`
	} `json:"status"`
}

// container is the container whose port p is (by name or number).
func (p gmpPod) container(port string) (string, bool) {
	for _, c := range p.Spec.Containers {
		for _, cp := range c.Ports {
			if cp.Name == port || strconv.Itoa(cp.ContainerPort) == port {
				return c.Name, true
			}
		}
	}
	_, err := strconv.Atoi(port)
	return "", err == nil
}

// managed serves managed collection on cluster t: the CRDs and
// namespaces, the resources' status, and their targets' scrapes.
func (c *gkePromCollector) managed(ctx context.Context, t gke.TelemetryTarget, now time.Time, base func(job, instance, namespace string) map[string]string) {
	c.mu.Lock()
	applied := c.crds[t.Kind]
	c.mu.Unlock()
	if !applied {
		if err := c.apply(ctx, t.Kind, gmpManifest); err != nil {
			return
		}
		c.mu.Lock()
		c.crds[t.Kind] = true
		c.mu.Unlock()
	}
	raw, err := c.kubectl(ctx, t.Kind, "get", "podmonitorings.monitoring.googleapis.com,clusterpodmonitorings.monitoring.googleapis.com", "-A", "-o", "json")
	if err != nil {
		// The CRDs went with a re-created cluster: applied again next time.
		c.mu.Lock()
		delete(c.crds, t.Kind)
		c.mu.Unlock()
		return
	}
	var mons struct {
		Items []gmpMonitoring `json:"items"`
	}
	if json.Unmarshal([]byte(raw), &mons) != nil || len(mons.Items) == 0 {
		return
	}
	podsRaw, err := c.kubectl(ctx, t.Kind, "get", "pods", "-A", "-o", "json")
	if err != nil {
		return
	}
	var pods struct {
		Items []gmpPod `json:"items"`
	}
	if json.Unmarshal([]byte(podsRaw), &pods) != nil {
		return
	}
	sort.Slice(mons.Items, func(i, j int) bool { return mons.Items[i].Metadata.Name < mons.Items[j].Metadata.Name })
	for _, m := range mons.Items {
		cluster := m.Kind == "ClusterPodMonitoring"
		c.markAccepted(ctx, t.Kind, m)
		for i, ep := range m.Spec.Endpoints {
			cfgs, err := ep.relabelConfigs()
			if err != nil {
				continue
			}
			key := fmt.Sprintf("%s/%s/%s/%s/%d", t.Kind, m.Kind, m.Metadata.Namespace, m.Metadata.Name, i)
			if !c.isDue(key, now, ep.interval()) {
				continue
			}
			port := ep.port()
			path := ep.Path
			if path == "" {
				path = "/metrics"
			}
			scheme := ""
			if strings.EqualFold(ep.Scheme, "https") {
				scheme = "https:"
			}
			for _, p := range pods.Items {
				if p.Status.Phase != "Running" || p.Status.PodIP == "" || (!cluster && p.Metadata.Namespace != m.Metadata.Namespace) ||
					!m.Spec.Selector.matches(p.Metadata.Labels) {
					continue
				}
				ctr, ok := p.container(port)
				if !ok {
					continue
				}
				out, err := c.kubectl(ctx, t.Kind, "get", "--raw",
					"/api/v1/namespaces/"+p.Metadata.Namespace+"/pods/"+scheme+p.Metadata.Name+":"+port+"/proxy"+path)
				if err != nil {
					continue
				}
				c.ingest(out, promScrape{resource: base(m.Metadata.Name, p.Metadata.Name+":"+port, p.Metadata.Namespace),
					relabel: cfgs, extra: gmpTargetLabels(m, p, ctr)}, now)
			}
		}
	}
}

// gmpTargetLabels are the labels a target's series get: its metadata
// labels (pod and container by default; node too when named) and the pod
// labels fromPod copies.
func gmpTargetLabels(m gmpMonitoring, p gmpPod, container string) map[string]string {
	meta := []string{"pod", "container"}
	if m.Spec.TargetLabels.Metadata != nil {
		meta = *m.Spec.TargetLabels.Metadata
	}
	out := map[string]string{}
	for _, k := range meta {
		switch k {
		case "pod":
			out["pod"] = p.Metadata.Name
		case "container":
			if container != "" {
				out["container"] = container
			}
		case "node":
			out["node"] = p.Spec.NodeName
		case "top_level_controller_name", "top_level_controller_type":
			if len(p.Metadata.OwnerReferences) > 0 {
				o := p.Metadata.OwnerReferences[0]
				if k == "top_level_controller_name" {
					out[k] = o.Name
				} else {
					out[k] = strings.ToLower(o.Kind)
				}
			}
		}
	}
	for _, f := range m.Spec.TargetLabels.FromPod {
		if v, ok := p.Metadata.Labels[f.From]; ok {
			to := f.To
			if to == "" {
				to = f.From
			}
			out[to] = v
		}
	}
	return out
}

// markAccepted sets a resource's ConfigurationCreateSuccess condition,
// once per generation.
func (c *gkePromCollector) markAccepted(ctx context.Context, kind string, m gmpMonitoring) {
	if m.Status.ObservedGeneration == m.Metadata.Generation {
		for _, cond := range m.Status.Conditions {
			if cond.Type == "ConfigurationCreateSuccess" && cond.Status == "True" {
				return
			}
		}
	}
	at := c.now().UTC().Format(time.RFC3339)
	patch, _ := json.Marshal(map[string]any{"status": map[string]any{
		"observedGeneration": m.Metadata.Generation,
		"conditions": []map[string]any{{"type": "ConfigurationCreateSuccess", "status": "True",
			"lastUpdateTime": at, "lastTransitionTime": at}}}})
	args := []string{"patch", strings.ToLower(m.Kind) + "s.monitoring.googleapis.com", m.Metadata.Name, "--subresource=status", "--type=merge", "-p", string(patch)}
	if m.Metadata.Namespace != "" {
		args = append(args, "-n", m.Metadata.Namespace)
	}
	_, _ = c.kubectl(ctx, kind, args...)
}
