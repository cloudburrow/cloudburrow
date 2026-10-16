package main

// Cloud Run logs in Cloud Logging (#1298).
//
// On Google, a revision's and a job task's container output is written to
// Cloud Logging by the platform: log run.googleapis.com/stdout or
// run.googleapis.com/stderr, monitored resource cloud_run_revision
// (service_name, revision_name, location, configuration_name) or
// cloud_run_job (job_name, location), the entry labels instanceId and, for a
// task, run.googleapis.com/execution_name, task_index and task_attempt. A
// line that is a JSON object is a jsonPayload, its severity, message and
// logging.googleapis.com/* fields read as Google's structured logging
// documents them; any other line is a textPayload with severity DEFAULT.
//
// runLogFeeder does that while Cloud Run and Cloud Logging are both served.
// It lists the workload namespace's pods with the Knative Services and batch
// Jobs the adapter made (whose annotations hold the Cloud Run name, and so
// the project and location), follows each user container's log file on the
// kind node, where the container runtime records which stream each line
// came from (the pod log API merges the two, and its stream parameter needs
// the PodLogsQuerySplitStreams feature gate, off in the cluster), and writes
// every line through the same WriteLogEntries the API serves, so exclusions
// apply and the entries reach ListLogEntries, gcloud logging read and the
// console's Logs Explorer.

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	ltype "google.golang.org/genproto/googleapis/logging/type"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/k8s"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/resource"
	"github.com/cloudburrow/cloudburrow/internal/service/logging"
)

// The two logs Cloud Run writes, as URL-encoded in a log name.
const (
	runStdout = "run.googleapis.com/stdout"
	runStderr = "run.googleapis.com/stderr"
)

// runLogSweep is how often the feeder looks for new pods. A job task that
// runs for less than this is still read, since a finished pod is kept until
// its execution is deleted.
const runLogSweep = 2 * time.Second

// runLogSource is one container's output to be read: which pod, which
// container, and what its entries are labelled with.
type runLogSource struct {
	podUID, pod, container string
	// terminal is true when the pod had finished when it was listed, so one
	// read gets everything it will ever write.
	terminal bool
	started  time.Time
	resource *monitoredres.MonitoredResource
	labels   map[string]string
	project  string
}

func (s runLogSource) key(stream string) string { return s.podUID + "/" + s.container + "/" + stream }

// runLogTargets reads the pods of Cloud Run revisions and job executions out
// of `kubectl get pods,services.serving.knative.dev,jobs.batch -o json`.
// A pod of a Knative Service or Job that the adapter did not make (no Cloud
// Run name annotation) is left out, and so is the queue-proxy sidecar, which
// is Knative's, not the application's.
func runLogTargets(raw []byte) ([]runLogSource, error) {
	var list struct {
		Items []struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name        string            `json:"name"`
				UID         string            `json:"uid"`
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Spec struct {
				Containers []struct {
					Name string `json:"name"`
				} `json:"containers"`
			} `json:"spec"`
			Status struct {
				Phase     string `json:"phase"`
				StartTime string `json:"startTime"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	services := map[string]string{} // Knative Service name -> Cloud Run name
	jobs := map[string]string{}     // batch Job name -> execution name
	for _, it := range list.Items {
		name := it.Metadata.Annotations["cloudburrow.dev/cloud-run-name"]
		if name == "" || it.Metadata.Labels[k8s.OwnedLabel] != "true" {
			continue
		}
		switch it.Kind {
		case "Service":
			services[it.Metadata.Name] = name
		case "Job":
			jobs[it.Metadata.Name] = name
		}
	}
	var out []runLogSource
	for _, it := range list.Items {
		if it.Kind != "Pod" {
			continue
		}
		l := it.Metadata.Labels
		src := runLogSource{podUID: it.Metadata.UID, pod: it.Metadata.Name,
			terminal: it.Status.Phase == "Succeeded" || it.Status.Phase == "Failed",
			labels:   map[string]string{"instanceId": it.Metadata.UID}}
		src.started, _ = time.Parse(time.RFC3339, it.Status.StartTime)
		switch {
		case services[l["serving.knative.dev/service"]] != "":
			n, err := resource.Parse(services[l["serving.knative.dev/service"]])
			if err != nil {
				continue
			}
			src.project = n.Project
			// The revision as the Cloud Run API names it, and the service's
			// ID as its configuration, not the Knative names (#1400).
			kname := l["serving.knative.dev/service"]
			src.resource = &monitoredres.MonitoredResource{Type: "cloud_run_revision", Labels: map[string]string{
				"project_id": n.Project, "location": n.Location, "service_name": n.ID,
				"revision_name":      runadapter.RevisionID(services[kname], kname, l["serving.knative.dev/revision"]),
				"configuration_name": n.ID}}
		case jobs[l["batch.kubernetes.io/job-name"]] != "":
			exec := jobs[l["batch.kubernetes.io/job-name"]]
			n, err := resource.Parse(exec)
			if err != nil || n.ParentID == "" {
				continue
			}
			src.project = n.Project
			src.resource = &monitoredres.MonitoredResource{Type: "cloud_run_job", Labels: map[string]string{
				"project_id": n.Project, "location": n.Location, "job_name": n.ParentID}}
			src.labels["run.googleapis.com/execution_name"] = n.ID
			src.labels["run.googleapis.com/task_index"] = orValue(it.Metadata.Annotations["batch.kubernetes.io/job-completion-index"], "0")
			src.labels["run.googleapis.com/task_attempt"] = orValue(it.Metadata.Annotations["batch.kubernetes.io/job-index-failure-count"], "0")
		default:
			continue
		}
		for _, c := range it.Spec.Containers {
			if c.Name == "queue-proxy" {
				continue
			}
			s := src
			s.container = c.Name
			s.labels = cloneLabels(src.labels)
			out = append(out, s)
		}
	}
	return out, nil
}

func orValue(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func cloneLabels(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// runLogEntry makes the entry Cloud Run writes for one line of a stream.
func runLogEntry(src runLogSource, stream string, ts time.Time, line string) *loggingpb.LogEntry {
	e := &loggingpb.LogEntry{
		LogName:   "projects/" + src.project + "/logs/" + url.PathEscape(stream),
		Resource:  src.resource,
		Timestamp: timestamppb.New(ts),
		Labels:    cloneLabels(src.labels),
		Severity:  ltype.LogSeverity_DEFAULT,
	}
	var obj map[string]any
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "{") && json.Unmarshal([]byte(trimmed), &obj) == nil {
		structured(e, obj)
		if p, err := structpb.NewStruct(obj); err == nil {
			e.Payload = &loggingpb.LogEntry_JsonPayload{JsonPayload: p}
			return e
		}
	}
	e.Payload = &loggingpb.LogEntry_TextPayload{TextPayload: line}
	return e
}

// structured reads the special fields of Google's structured logging out of
// a JSON line into the entry, removing each it uses, as the logging agent
// does; message and every other field stay in the payload.
func structured(e *loggingpb.LogEntry, obj map[string]any) {
	if s, ok := obj["severity"].(string); ok {
		if v, ok := ltype.LogSeverity_value[strings.ToUpper(s)]; ok {
			e.Severity = ltype.LogSeverity(v)
			delete(obj, "severity")
		} else if strings.EqualFold(s, "WARN") {
			e.Severity = ltype.LogSeverity_WARNING
			delete(obj, "severity")
		}
	}
	if m, ok := obj["logging.googleapis.com/labels"].(map[string]any); ok {
		for k, v := range m {
			if s, ok := v.(string); ok {
				e.Labels[k] = s
			}
		}
		delete(obj, "logging.googleapis.com/labels")
	}
	if s, ok := obj["logging.googleapis.com/trace"].(string); ok {
		e.Trace = s
		delete(obj, "logging.googleapis.com/trace")
	}
	if s, ok := obj["logging.googleapis.com/spanId"].(string); ok {
		e.SpanId = s
		delete(obj, "logging.googleapis.com/spanId")
	}
	if b, ok := obj["logging.googleapis.com/trace_sampled"].(bool); ok {
		e.TraceSampled = b
		delete(obj, "logging.googleapis.com/trace_sampled")
	}
	if s, ok := obj["logging.googleapis.com/insertId"].(string); ok {
		e.InsertId = s
		delete(obj, "logging.googleapis.com/insertId")
	}
	for _, f := range []string{"time", "timestamp"} {
		if s, ok := obj[f].(string); ok {
			if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
				e.Timestamp = timestamppb.New(t)
				delete(obj, f)
				break
			}
		}
	}
}

// runLogFeeder follows Cloud Run containers into Cloud Logging.
type runLogFeeder struct {
	kube      *k8s.Runner
	node      string
	namespace string
	write     func(context.Context, *loggingpb.WriteLogEntriesRequest) (*loggingpb.WriteLogEntriesResponse, error)
	// docker runs `docker args...` and returns its stdout as it is written,
	// and wait; a variable for tests.
	docker func(ctx context.Context, args ...string) (io.ReadCloser, func() error, error)

	// since is when the feeder started: a file already there at the first
	// sweep is read from that time, so a restarted instance does not write
	// its lines twice.
	since time.Time
	first bool

	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	follows map[string]*runLogFollow
}

// runLogFollow is one log file being read: a container's run (the runtime
// writes 0.log, then 1.log after a restart).
type runLogFollow struct {
	cancel  context.CancelFunc
	running bool
	last    time.Time
	// offset is how many bytes of the file have been read.
	offset int64
}

// runLogTailFor bounds each tail on the node. Killing `docker exec` does
// not end the process it started in the container, so each read ends by
// itself and the next sweep continues from the bytes already read.
const runLogTailFor = "30"

func newRunLogFeeder(kubeconfig, node string, svc *loggingService) *runLogFeeder {
	if kubeconfig == "" || svc == nil {
		return nil
	}
	return &runLogFeeder{kube: kubeRunner(kubeconfig, ""), node: node, namespace: runadapter.WorkloadNamespace,
		write: svc.api.WriteLogEntries, docker: dockerStream, follows: map[string]*runLogFollow{}, first: true}
}

func dockerStream(ctx context.Context, args ...string) (io.ReadCloser, func() error, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	return out, cmd.Wait, nil
}

func (f *runLogFeeder) Name() string { return "run-logs" }

func (f *runLogFeeder) register(coord *lifecycle.Coordinator) {
	if f != nil {
		coord.Register(f)
	}
}

func (f *runLogFeeder) Start(ctx context.Context) error {
	if f == nil {
		return nil
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	f.mu.Lock()
	f.cancel, f.done, f.since = cancel, done, time.Now()
	f.mu.Unlock()
	go func() {
		defer close(done)
		t := time.NewTicker(runLogSweep)
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

func (f *runLogFeeder) Stop(ctx context.Context) error {
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

// podLogDir is where the container runtime writes a pod's container logs on
// the node: /var/log/pods/<namespace>_<pod>_<uid>/<container>/<restart>.log.
func podLogDir(namespace, pod, uid string) string {
	return "/var/log/pods/" + namespace + "_" + pod + "_" + uid
}

// runLogFiles lists the log files on the node of the sources' containers.
func (f *runLogFeeder) runLogFiles(ctx context.Context) ([]string, error) {
	out, wait, err := f.docker(ctx, "exec", f.node, "find", "/var/log/pods", "-path", "/var/log/pods/"+f.namespace+"_*", "-name", "*.log")
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

func (f *runLogFeeder) sweep(ctx context.Context) {
	raw, err := f.kube.Do(ctx, "", "-n", f.namespace, "get", "pods,services.serving.knative.dev,jobs.batch", "-o", "json")
	if err != nil {
		return
	}
	srcs, err := runLogTargets([]byte(raw))
	if err != nil {
		return
	}
	files, err := f.runLogFiles(ctx)
	if err != nil {
		return
	}
	byDir := map[string]runLogSource{}
	for _, s := range srcs {
		byDir[podLogDir(f.namespace, s.pod, s.podUID)+"/"+s.container] = s
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	first := f.first
	f.first = false
	seen := map[string]bool{}
	for _, file := range files {
		s, ok := byDir[file[:max(strings.LastIndex(file, "/"), 0)]]
		if !ok {
			continue
		}
		seen[file] = true
		st := f.follows[file]
		if st == nil {
			st = &runLogFollow{}
			if first {
				st.last = f.since
			}
			f.follows[file] = st
		}
		if st.running {
			continue
		}
		fctx, cancel := context.WithCancel(ctx)
		st.running, st.cancel = true, cancel
		go f.follow(fctx, s, file, st)
	}
	// A file no longer there (its pod deleted) is followed no longer.
	for file, st := range f.follows {
		if !seen[file] {
			if st.cancel != nil {
				st.cancel()
			}
			delete(f.follows, file)
		}
	}
}

// follow reads one log file until it is stopped, writing each line newer
// than the last one written.
func (f *runLogFeeder) follow(ctx context.Context, s runLogSource, file string, st *runLogFollow) {
	f.mu.Lock()
	since, offset := st.last, st.offset
	f.mu.Unlock()
	if offset > 0 {
		// Only the bytes after offset are read: none of them is written yet.
		since = time.Time{}
	}
	last, n := since, int64(0)
	if out, wait, err := f.docker(ctx, "exec", f.node, "timeout", runLogTailFor,
		"tail", "-c", "+"+strconv.FormatInt(offset+1, 10), "-F", file); err == nil {
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

// read writes the entries of a CRI log stream: each line is
// "<RFC3339Nano> <stdout|stderr> <F|P> <text>", P marking a line the runtime
// split, continued by the next of the same stream.
func (f *runLogFeeder) read(ctx context.Context, r io.Reader, s runLogSource, since time.Time) (time.Time, int64) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), logging.MaxEntryBytes)
	last, n := since, int64(0)
	partial := map[string]string{}
	for sc.Scan() {
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
		log := runStdout
		if stream == "stderr" {
			log = runStderr
		}
		_, _ = f.write(ctx, &loggingpb.WriteLogEntriesRequest{Entries: []*loggingpb.LogEntry{runLogEntry(s, log, ts, line)}})
	}
	return last, n
}

// parseCRILine splits one line of a container runtime log file.
func parseCRILine(l string) (ts time.Time, stream, text string, complete, ok bool) {
	parts := strings.SplitN(l, " ", 4)
	if len(parts) < 3 {
		return time.Time{}, "", "", false, false
	}
	t, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil || (parts[1] != "stdout" && parts[1] != "stderr") {
		return time.Time{}, "", "", false, false
	}
	if len(parts) == 4 {
		text = parts[3]
	}
	return t, parts[1], text, parts[2] != "P", true
}
