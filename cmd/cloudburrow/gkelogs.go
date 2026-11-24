package main

// Kubernetes Engine logs in Cloud Logging (#2100).
//
// GKE ships a cluster's container logs to Cloud Logging while logging is
// enabled: logs written to standard out to projects/PROJECT_ID/logs/stdout,
// to standard error to projects/PROJECT_ID/logs/stderr, on the
// k8s_container resource (re-read 2026-11-21:
// https://docs.cloud.google.com/kubernetes-engine/docs/how-to/view-logs),
// whose labels are project_id, location, cluster_name, namespace_name,
// pod_name and container_name (https://docs.cloud.google.com/monitoring/api/resources).
// System logs are those of the kube-system, istio-system, knative-serving,
// gke-system and config-management-system namespaces; GKE collects them
// with a fluentbit-based agent it deploys on every node
// (https://docs.cloud.google.com/kubernetes-engine/docs/concepts/about-logs).
// The control plane's own logs, with the APISERVER, SCHEDULER and
// CONTROLLER_MANAGER components, are on the k8s_control_plane_component
// resource, log container.googleapis.com/<component>.
//
// The agent here is internal/logagent, run in each cluster as GKE runs its
// own: the kube-system DaemonSet fluentbit-gke, one pod per node, which
// reads the node's container log files and writes to the instance's Cloud
// Logging through the cloudburrow-host route pods reach the CLI by. Its
// image is built from the Linux builds this CLI embeds
// (internal/logagentimage) and loaded into every node; it is never pushed.
//
// gkeLogAgents keeps each served cluster's agent as its logging says: the
// fluentbit-gke-config ConfigMap names the categories shipped and since
// when, the DaemonSet runs while any is on, and is removed when logging is
// none. The agent keeps its position in each file on the node, so it ships
// across a restart of the CLI, which it keeps retrying while Cloud Logging
// is gone, and never ships a line twice or loses one.

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/images"
	"github.com/cloudburrow/cloudburrow/internal/k8s"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/logagent"
	"github.com/cloudburrow/cloudburrow/internal/logagentimage"
	"github.com/cloudburrow/cloudburrow/internal/service/gke"
)

// gkeLogSweep is how often each cluster's agent is reconciled.
const gkeLogSweep = 2 * time.Second

// gkeControlPlaneComponents are the control plane categories, by GKE's
// component names.
var gkeControlPlaneComponents = []string{"apiserver", "scheduler", "controller-manager"}

// gkeLogCategories are the categories t ships.
func gkeLogCategories(t gke.TelemetryTarget) map[string]bool {
	on := map[string]bool{logagent.System: t.SystemLogs, logagent.Workload: t.WorkloadLogs}
	for _, c := range gkeControlPlaneComponents {
		on[c] = t.ControlPlaneLogs[c]
	}
	return on
}

// gkeLogCluster is how a cluster's agent is reconciled: kubectl and kind
// on one kind cluster.
type gkeLogCluster interface {
	// Apply applies a manifest.
	Apply(ctx context.Context, kind, manifest string) error
	// Kubectl runs kubectl and returns its output.
	Kubectl(ctx context.Context, kind string, args ...string) (string, error)
	// Nodes are the kind node containers: their container IDs by name.
	Nodes(ctx context.Context, kind string) (map[string]string, error)
	// Image builds the agent image for the nodes' architecture.
	Image(ctx context.Context, kind string) (string, error)
	// Load loads ref into node unless it has it, and says whether it
	// loaded.
	Load(ctx context.Context, kind, node, ref string) (bool, error)
}

// gkeLogState is what has been applied to one cluster.
type gkeLogState struct {
	read    bool
	cfg     logagent.Config
	written string
	applied string
	removed bool
	// loaded are the node containers (by ID) that have the image.
	loaded map[string]bool
	image  string
}

// gkeLogAgents reconciles the served clusters' logging agents.
type gkeLogAgents struct {
	targets func() []gke.TelemetryTarget
	// endpoint is the instance's Cloud Logging as pods reach it,
	// http://host:port, or "" while it is not published to them.
	endpoint func() string
	cluster  gkeLogCluster
	now      func() time.Time
	every    time.Duration
	out      io.Writer

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
	state  map[string]*gkeLogState
	warned string
}

func newGKELogAgents(dir string, gkeSvc *gkeService, svc *loggingService, endpoint func() string, out io.Writer) *gkeLogAgents {
	if gkeSvc == nil || svc == nil {
		return nil
	}
	return &gkeLogAgents{
		targets: func() []gke.TelemetryTarget {
			if api := gkeSvc.API(); api != nil {
				return api.Telemetry()
			}
			return nil
		},
		endpoint: endpoint,
		cluster:  &gkeKindLogCluster{dir: dir, run: images.ExecRunner{}, images: map[string]string{}},
		now:      time.Now, every: gkeLogSweep, out: out,
		state: map[string]*gkeLogState{},
	}
}

func (f *gkeLogAgents) Name() string { return "gke-logs" }

func (f *gkeLogAgents) register(coord *lifecycle.Coordinator) {
	if f != nil {
		coord.Register(f)
	}
}

func (f *gkeLogAgents) Start(ctx context.Context) error {
	if f == nil {
		return nil
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	f.mu.Lock()
	f.cancel, f.done = cancel, done
	f.mu.Unlock()
	go func() {
		defer close(done)
		t := time.NewTicker(f.every)
		defer t.Stop()
		for {
			f.sweep(runCtx)
			select {
			case <-runCtx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return nil
}

// Stop ends the reconciling and leaves each agent running: it ships again
// once Cloud Logging is back.
func (f *gkeLogAgents) Stop(ctx context.Context) error {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	cancel, done := f.cancel, f.done
	f.cancel = nil
	f.mu.Unlock()
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

// sweep reconciles every running cluster's agent.
func (f *gkeLogAgents) sweep(ctx context.Context) {
	live := map[string]bool{}
	for _, t := range f.targets() {
		live[t.Kind] = true
		f.mu.Lock()
		st := f.state[t.Kind]
		if st == nil {
			st = &gkeLogState{loaded: map[string]bool{}}
			f.state[t.Kind] = st
		}
		f.mu.Unlock()
		if err := f.reconcile(ctx, t, st); err != nil && ctx.Err() == nil {
			f.warn(fmt.Sprintf("  warning: Kubernetes Engine cluster %s's logging agent: %v\n", t.Name, err))
		}
	}
	f.mu.Lock()
	for k := range f.state {
		if !live[k] {
			delete(f.state, k)
		}
	}
	f.mu.Unlock()
}

// warn prints a warning once until it changes.
func (f *gkeLogAgents) warn(msg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if msg != f.warned && f.out != nil {
		fmt.Fprint(f.out, msg)
	}
	f.warned = msg
}

// reconcile writes t's configuration, then runs its agent while any
// category is on and removes it when none is.
func (f *gkeLogAgents) reconcile(ctx context.Context, t gke.TelemetryTarget, st *gkeLogState) error {
	c := f.cluster
	if !st.read {
		// The configuration a previous run wrote, whose categories' times
		// are kept: the agent ran on while this process was gone.
		out, err := c.Kubectl(ctx, t.Kind, "get", "configmap", "-n", logagent.Namespace, logagent.ConfigMap,
			"--ignore-not-found", "-o", "jsonpath={.data.config\\.json}")
		if err != nil {
			return err
		}
		if s := strings.TrimSpace(out); s != "" {
			if cfg, err := logagent.ParseConfig(s); err == nil {
				st.cfg, st.written = cfg, cfg.Marshal()
			}
		}
		st.read = true
	}
	next := st.cfg.Next(gkeLogCategories(t), t.LoggingChanged, f.now())
	next.Project, next.Location, next.Cluster = t.Project, t.Location, t.Cluster
	if s := next.Marshal(); s != st.written {
		if err := c.Apply(ctx, t.Kind, logagent.ConfigManifest(next)); err != nil {
			return err
		}
		st.written = s
	}
	st.cfg = next
	if !next.Any() {
		if !st.removed {
			if _, err := c.Kubectl(ctx, t.Kind, "delete", "daemonset", "-n", logagent.Namespace, logagent.Name, "--ignore-not-found", "--wait=false"); err != nil {
				return err
			}
			st.removed, st.applied = true, ""
		}
		return nil
	}
	endpoint := f.endpoint()
	if endpoint == "" {
		return fmt.Errorf("the instance's Cloud Logging is not published to the cluster's pods yet")
	}
	if st.image == "" {
		ref, err := c.Image(ctx, t.Kind)
		if err != nil {
			return err
		}
		st.image = ref
	}
	nodes, err := c.Nodes(ctx, t.Kind)
	if err != nil {
		return err
	}
	ids := map[string]bool{}
	for name, id := range nodes {
		ids[id] = true
		if st.loaded[id] {
			continue
		}
		loaded, err := c.Load(ctx, t.Kind, name, st.image)
		if err != nil {
			return err
		}
		st.loaded[id] = true
		if loaded {
			// A pod the DaemonSet made before the image was there waits in
			// ErrImageNeverPull's back-off; made again, it starts now.
			_, _ = c.Kubectl(ctx, t.Kind, "delete", "pod", "-n", logagent.Namespace, "-l", "k8s-app="+logagent.Name,
				"--field-selector", "spec.nodeName="+name, "--ignore-not-found", "--wait=false")
		}
	}
	for id := range st.loaded {
		if !ids[id] {
			delete(st.loaded, id)
		}
	}
	m := logagent.Manifest(st.image, endpoint)
	if m != st.applied {
		if err := c.Apply(ctx, t.Kind, m); err != nil {
			return err
		}
		st.applied, st.removed = m, false
	}
	return nil
}

// gkeKindLogCluster is gkeLogCluster on the kind clusters in dir.
type gkeKindLogCluster struct {
	dir string
	run images.ExecRunner

	mu     sync.Mutex
	images map[string]string // by architecture
}

func (k *gkeKindLogCluster) kube(kind string) *k8s.Runner {
	return kubeRunner(filepath.Join(k.dir, kind, "kubeconfig"), "")
}

func (k *gkeKindLogCluster) Apply(ctx context.Context, kind, manifest string) error {
	return k.kube(kind).Apply(ctx, manifest, k8s.ApplyOptions{})
}

func (k *gkeKindLogCluster) Kubectl(ctx context.Context, kind string, args ...string) (string, error) {
	return k.kube(kind).Do(ctx, "", args...)
}

func (k *gkeKindLogCluster) Nodes(ctx context.Context, kind string) (map[string]string, error) {
	out, err := k.run.Run(ctx, "docker", "ps", "--filter", "label=io.x-k8s.kind.cluster="+kind, "--format", "{{.Names}} {{.ID}}")
	if err != nil {
		return nil, err
	}
	nodes := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) == 2 {
			nodes[f[0]] = f[1]
		}
	}
	return nodes, nil
}

func (k *gkeKindLogCluster) Image(ctx context.Context, kind string) (string, error) {
	loader := &images.Loader{ClusterName: kind, Runner: k.run}
	arch, err := loader.NodeArchitecture(ctx, filepath.Join(k.dir, kind, "kubeconfig"))
	if err != nil {
		return "", fmt.Errorf("the nodes' architecture: %w", err)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if ref := k.images[arch]; ref != "" {
		return ref, nil
	}
	ref, err := logagentimage.Build(ctx, k.run, arch)
	if err != nil {
		return "", fmt.Errorf("the logging agent image: %w", err)
	}
	k.images[arch] = ref
	return ref, nil
}

func (k *gkeKindLogCluster) Load(ctx context.Context, kind, node, ref string) (bool, error) {
	loader := &images.Loader{ClusterName: kind, Runner: k.run}
	if loader.NodeHas(ctx, node, ref) {
		return false, nil
	}
	if _, err := k.run.Run(ctx, "kind", "load", "docker-image", ref, "--name", kind, "--nodes", node); err != nil {
		return false, fmt.Errorf("load %s into %s: %w", ref, node, err)
	}
	return true, nil
}
