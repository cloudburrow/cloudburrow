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
// gke-system and config-management-system namespaces
// (https://docs.cloud.google.com/kubernetes-engine/docs/concepts/about-logs);
// workload logs the rest. The control plane's own logs, with the
// APISERVER, SCHEDULER and CONTROLLER_MANAGER components, are on the
// k8s_control_plane_component resource, log container.googleapis.com/<component>.
//
// gkeLogFeeder does that for the served clusters: it lists each cluster's
// pods, follows every container's log file on the kind node that runs it
// (the container runtime records each line's stream there, as runlogs.go
// reads Cloud Run's), and writes each line through the served Logging's
// WriteLogEntries. Each entry also carries GKE's labels: the pod's labels
// as k8s-pod/<key>, and the node as compute.googleapis.com/resource_name
// (the Logging agent's, not on the pages above: UNVERIFIED against a
// document). A line of stderr is severity ERROR and of stdout INFO unless
// it is structured JSON naming its own (the agent's, UNVERIFIED). When a
// cluster's logging, or the component a container belongs to, is turned
// off its files are followed no longer, and what is written meanwhile is
// never shipped.

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	ltype "google.golang.org/genproto/googleapis/logging/type"

	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/service/gke"
	"github.com/cloudburrow/cloudburrow/internal/service/logging"
)

// gkeControlPlane are the control plane's static pods' components (their
// "component" label) and GKE's component names; etcd's logs are never
// shipped.
var gkeControlPlane = map[string]string{
	"kube-apiserver": "apiserver", "kube-scheduler": "scheduler", "kube-controller-manager": "controller-manager", "etcd": "",
}

// gkePodList reads `kubectl get pods -A -o json`: each container of each
// pod, the node it runs on, and what its entries are labelled with, for
// target t. Only what t ships is listed; the category is the key of the
// time its shipping started ("system", "workload" or a control plane
// component).
func gkeLogTargets(t gke.TelemetryTarget, raw []byte) ([]gkeLogSource, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name      string            `json:"name"`
				Namespace string            `json:"namespace"`
				UID       string            `json:"uid"`
				Labels    map[string]string `json:"labels"`
			} `json:"metadata"`
			Spec struct {
				NodeName       string                  `json:"nodeName"`
				Containers     []struct{ Name string } `json:"containers"`
				InitContainers []struct{ Name string } `json:"initContainers"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	var out []gkeLogSource
	for _, p := range list.Items {
		m := p.Metadata
		if p.Spec.NodeName == "" {
			continue
		}
		category := "workload"
		if gke.SystemNamespaces[m.Namespace] {
			category = "system"
		}
		component := ""
		if m.Namespace == "kube-system" && m.Labels["tier"] == "control-plane" {
			c, ok := gkeControlPlane[m.Labels["component"]]
			if ok {
				if c == "" {
					continue
				}
				component, category = c, c
			}
		}
		switch {
		case component != "":
			if !t.ControlPlaneLogs[component] {
				continue
			}
		case category == "system":
			if !t.SystemLogs {
				continue
			}
		default:
			if !t.WorkloadLogs {
				continue
			}
		}
		var names []string
		for _, c := range p.Spec.InitContainers {
			names = append(names, c.Name)
		}
		for _, c := range p.Spec.Containers {
			names = append(names, c.Name)
		}
		for _, c := range names {
			src := gkeLogSource{node: p.Spec.NodeName, category: category, dir: podLogDir(m.Namespace, m.Name, m.UID) + "/" + c,
				runLogSource: runLogSource{podUID: m.UID, pod: m.Name, container: c, project: t.Project,
					labels: map[string]string{"compute.googleapis.com/resource_name": p.Spec.NodeName}}}
			if component != "" {
				src.logName = "container.googleapis.com/" + component
				src.resource = &monitoredres.MonitoredResource{Type: "k8s_control_plane_component", Labels: map[string]string{
					"project_id": t.Project, "location": t.Location, "cluster_name": t.Cluster,
					"component_name": component, "component_location": t.Location}}
				src.labels = map[string]string{}
			} else {
				src.resource = &monitoredres.MonitoredResource{Type: "k8s_container", Labels: map[string]string{
					"project_id": t.Project, "location": t.Location, "cluster_name": t.Cluster,
					"namespace_name": m.Namespace, "pod_name": m.Name, "container_name": c}}
				for k, v := range m.Labels {
					src.labels["k8s-pod/"+k] = v
				}
			}
			out = append(out, src)
		}
	}
	return out, nil
}

// gkeLogSource is one container's log directory on a node, and its
// entries' log, resource and labels.
type gkeLogSource struct {
	runLogSource
	node, category, dir string
	// logName is the log of a control plane component; empty is stdout or
	// stderr by stream.
	logName string
}

// gkeLogEntry is the entry of one line of a stream.
func gkeLogEntry(s gkeLogSource, stream string, ts time.Time, line string) *loggingpb.LogEntry {
	log := s.logName
	if log == "" {
		log = stream
	}
	e := runLogEntry(s.runLogSource, log, ts, line)
	if e.Severity == ltype.LogSeverity_DEFAULT {
		e.Severity = ltype.LogSeverity_INFO
		if stream == "stderr" {
			e.Severity = ltype.LogSeverity_ERROR
		}
	}
	return e
}

// gkeLogFeeder follows the served clusters' containers into Cloud Logging.
type gkeLogFeeder struct {
	targets func() []gke.TelemetryTarget
	kubectl func(ctx context.Context, kind string, args ...string) (string, error)
	write   func(context.Context, *loggingpb.WriteLogEntriesRequest) (*loggingpb.WriteLogEntriesResponse, error)
	docker  func(ctx context.Context, args ...string) (io.ReadCloser, func() error, error)
	now     func() time.Time
	every   time.Duration

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
	// since is when each cluster's category ("<kind>/<category>") started
	// to be shipped: lines before it are never written.
	since   map[string]time.Time
	follows map[string]*runLogFollow
	// seen is when each cluster (by kind) was first found shipping: the
	// start of a category whose cluster's logging configuration has not
	// changed since this process started (TelemetryTarget.LogsSince).
	seen map[string]time.Time
}

func newGKELogFeeder(dir string, gkeSvc *gkeService, svc *loggingService) *gkeLogFeeder {
	if gkeSvc == nil || svc == nil {
		return nil
	}
	return &gkeLogFeeder{
		targets: func() []gke.TelemetryTarget {
			if api := gkeSvc.API(); api != nil {
				return api.Telemetry()
			}
			return nil
		},
		kubectl: gkeKubectl(dir),
		write: func(ctx context.Context, req *loggingpb.WriteLogEntriesRequest) (*loggingpb.WriteLogEntriesResponse, error) {
			return svc.api.WriteLogEntries(ctx, req)
		},
		docker: dockerStream, now: time.Now, every: runLogSweep,
		since: map[string]time.Time{}, follows: map[string]*runLogFollow{}, seen: map[string]time.Time{},
	}
}

func (f *gkeLogFeeder) Name() string { return "gke-logs" }

func (f *gkeLogFeeder) register(coord *lifecycle.Coordinator) {
	if f != nil {
		coord.Register(f)
	}
}

func (f *gkeLogFeeder) Start(ctx context.Context) error {
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

func (f *gkeLogFeeder) Stop(ctx context.Context) error {
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

// sweep follows every log file of what is shipped now, and stops
// following the rest.
func (f *gkeLogFeeder) sweep(ctx context.Context) {
	type file struct {
		kind string
		src  gkeLogSource
	}
	var files []file
	listed := map[string]bool{} // clusters whose files were listed
	// A category found shipping now starts when its cluster's logging was
	// last changed, or else when the cluster was first found shipping: not
	// when this sweep, its listing done, first sees a file of it. A pod
	// made just after logging was turned back on has written its first
	// lines by then, and they were dropped (main run 45023269046,
	// TestGKELoggingAndMonitoring: "logs are not shipped again after
	// SYSTEM_COMPONENTS and WORKLOADS").
	start := f.now()
	starts := map[string]time.Time{}
	for _, t := range f.targets() {
		if !t.SystemLogs && !t.WorkloadLogs && len(t.ControlPlaneLogs) == 0 {
			continue
		}
		starts[t.Kind] = t.LogsSince
		raw, err := f.kubectl(ctx, t.Kind, "get", "pods", "-A", "-o", "json")
		if err != nil {
			continue
		}
		srcs, err := gkeLogTargets(t, []byte(raw))
		if err != nil {
			continue
		}
		byNode := map[string]map[string]gkeLogSource{}
		for _, s := range srcs {
			if byNode[s.node] == nil {
				byNode[s.node] = map[string]gkeLogSource{}
			}
			byNode[s.node][s.dir] = s
		}
		ok := true
		for node, dirs := range byNode {
			names, err := f.nodeLogFiles(ctx, node)
			if err != nil {
				ok = false
				continue
			}
			for _, n := range names {
				if s, found := dirs[n[:max(strings.LastIndex(n, "/"), 0)]]; found {
					files = append(files, file{kind: t.Kind, src: s})
					files[len(files)-1].src.dir = n
				}
			}
		}
		if ok {
			listed[t.Kind] = true
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for kind, changed := range starts {
		if _, ok := f.seen[kind]; !ok {
			f.seen[kind] = start
		}
		if changed.IsZero() || changed.After(start) {
			starts[kind] = f.seen[kind]
		}
	}
	for kind := range f.seen {
		if _, ok := starts[kind]; !ok {
			delete(f.seen, kind)
		}
	}
	seen := map[string]bool{}
	categories := map[string]bool{}
	for _, fl := range files {
		cat := fl.kind + "/" + fl.src.category
		categories[cat] = true
		if _, ok := f.since[cat]; !ok {
			f.since[cat] = starts[fl.kind]
		}
		key := fl.kind + "\x00" + fl.src.node + "\x00" + fl.src.dir
		seen[key] = true
		st := f.follows[key]
		if st == nil {
			st = &runLogFollow{last: f.since[cat]}
			f.follows[key] = st
		}
		if st.running {
			continue
		}
		fctx, cancel := context.WithCancel(ctx)
		st.running, st.cancel = true, cancel
		go f.follow(fctx, fl.src, st)
	}
	for key, st := range f.follows {
		kind := key[:strings.Index(key, "\x00")]
		if !seen[key] && (listed[kind] || !f.shipping(kind)) {
			if st.cancel != nil {
				st.cancel()
			}
			delete(f.follows, key)
		}
	}
	// A category no longer shipped starts again from when it is turned
	// back on.
	for cat := range f.since {
		kind := cat[:strings.Index(cat, "/")]
		if !categories[cat] && (listed[kind] || !f.shipping(kind)) {
			delete(f.since, cat)
		}
	}
}

// shipping is whether kind is one of the clusters whose logs are shipped;
// f.mu is held, and the targets read again.
func (f *gkeLogFeeder) shipping(kind string) bool {
	for _, t := range f.targets() {
		if t.Kind == kind && (t.SystemLogs || t.WorkloadLogs || len(t.ControlPlaneLogs) > 0) {
			return true
		}
	}
	return false
}

// nodeLogFiles lists a node's container log files.
func (f *gkeLogFeeder) nodeLogFiles(ctx context.Context, node string) ([]string, error) {
	out, wait, err := f.docker(ctx, "exec", node, "find", "/var/log/pods", "-name", "*.log")
	if err != nil {
		return nil, err
	}
	raw, rerr := io.ReadAll(out)
	if err := wait(); err != nil {
		return nil, err
	}
	if rerr != nil {
		return nil, rerr
	}
	return strings.Fields(string(raw)), nil
}

// follow reads one log file for a while, writing each line newer than the
// last one written; the next sweep continues it.
func (f *gkeLogFeeder) follow(ctx context.Context, s gkeLogSource, st *runLogFollow) {
	f.mu.Lock()
	since, offset := st.last, st.offset
	f.mu.Unlock()
	last, n := since, int64(0)
	if out, wait, err := f.docker(ctx, "exec", s.node, "timeout", runLogTailFor,
		"tail", "-c", "+"+strconv.FormatInt(offset+1, 10), "-F", s.dir); err == nil {
		last, n = f.read(ctx, out, s, since)
		_ = wait()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	st.running = false
	st.offset += n
	if last.After(st.last) {
		st.last = last
	}
}

// read writes the entries of a CRI log stream newer than since, while ctx
// lasts.
func (f *gkeLogFeeder) read(ctx context.Context, r io.Reader, s gkeLogSource, since time.Time) (time.Time, int64) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), logging.MaxEntryBytes)
	last, n := since, int64(0)
	partial := map[string]string{}
	for sc.Scan() {
		if ctx.Err() != nil {
			break
		}
		n += int64(len(sc.Bytes())) + 1
		ts, stream, line, complete, ok := parseCRILine(sc.Text())
		if !ok {
			continue
		}
		if !complete {
			partial[stream] += line
			continue
		}
		line, partial[stream] = partial[stream]+line, ""
		if !ts.After(since) {
			continue
		}
		if ts.After(last) {
			last = ts
		}
		_, _ = f.write(ctx, &loggingpb.WriteLogEntriesRequest{Entries: []*loggingpb.LogEntry{gkeLogEntry(s, stream, ts, line)}})
	}
	return last, n
}
