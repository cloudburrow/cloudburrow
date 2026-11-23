// Package dataproc serves the Dataproc API's serverless surface
// (dataproc.googleapis.com v1, #2156): Serverless for Apache Spark, now
// Managed Service for Apache Spark in Google's docs
// (https://docs.cloud.google.com/managed-spark/docs/reference/rest, read
// 2026-11-22). Its batches (PySpark, Spark, SparkR and Spark SQL), its
// interactive sessions (Spark Connect) and its session templates, with
// the operations they return and the locations they are in.
//
// A batch and a session run on the CLI's Apache Spark runner
// (internal/sparkproc), which runs BigQuery's stored procedures for Apache
// Spark too: the runtime version's pinned apache/spark image, Google's
// BigQuery connector for Spark on its class path, on a Docker network
// with no route out but the instance's BigQuery. A batch's files are read
// from the instance's Cloud Storage and its driver's output written back
// to it (runtimeInfo.outputUri); its lines go to the instance's Cloud
// Logging as the cloud_dataproc_batch resource. A session's Spark Connect
// server is reached through this API's own port (bridge.go): gRPC whose
// requests name the session by its UUID, and the TCP-over-WebSocket bridge
// Google's Spark Connect client (dataproc-spark-connect) tunnels through.
package dataproc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	dataprocpb "cloud.google.com/go/dataproc/v2/apiv1/dataprocpb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/locations"
	"github.com/cloudburrow/cloudburrow/internal/sparkproc"
)

// Engine runs Spark: sparkproc.Docker, through Docker.
type Engine interface {
	Run(ctx context.Context, job sparkproc.Job) (sparkproc.Result, error)
	StartConnect(ctx context.Context, job sparkproc.Job) (Conn, error)
}

// Conn is a session's running Spark Connect server (*sparkproc.Connect).
type Conn interface {
	Address() string
	Done() <-chan struct{}
	Result() sparkproc.Result
	Stop()
}

// Docker is the Engine of a sparkproc.Docker.
type Docker struct{ D *sparkproc.Docker }

func (d Docker) Run(ctx context.Context, job sparkproc.Job) (sparkproc.Result, error) {
	return d.D.Run(ctx, job)
}

func (d Docker) StartConnect(ctx context.Context, job sparkproc.Job) (Conn, error) {
	c, err := d.D.StartConnect(ctx, job)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// Storage is the instance's Cloud Storage, which holds a batch's files
// and its driver's output.
type Storage interface {
	// Read reads gs://bucket/object.
	Read(ctx context.Context, uri string) ([]byte, error)
	// Write writes gs://bucket/object.
	Write(ctx context.Context, uri string, data []byte) error
	// EnsureBucket makes the bucket in project unless it exists.
	EnsureBucket(ctx context.Context, project, bucket, location string) error
}

// Options configure a Server.
type Options struct {
	// State is the file the resources are kept in; "" keeps them in
	// memory.
	State string
	// Engine runs the workloads; nil fails each batch and session with
	// Spark's unavailability.
	Engine Engine
	// Virtual starts Dataproc on GKE clusters, or is nil when the
	// instance serves no Kubernetes Engine.
	Virtual VirtualEngine
	// Storage is the instance's Cloud Storage, or nil when it serves none.
	Storage Storage
	// ProjectNumber is a project's number, for the staging bucket's name.
	ProjectNumber func(project string) string
	// Caller is the calling principal's email, a resource's creator.
	Caller func(ctx context.Context) string
	// Workers bounds the batches run at once (0: 2); the rest wait
	// PENDING.
	Workers int
	// HistoryDir is the host directory the batches' and sessions' Spark
	// event logs are written to (sparkproc.Docker.History), which the
	// instance's History Server reads; "" serves no Spark UI for them.
	HistoryDir string
	// InstanceID names the instance's History Server's containers.
	InstanceID string
	// Logf receives the server's notes, or is nil.
	Logf func(format string, args ...any)
}

// Server is the API.
type Server struct {
	dataprocpb.UnimplementedBatchControllerServer
	dataprocpb.UnimplementedSessionControllerServer
	dataprocpb.UnimplementedSessionTemplateControllerServer
	dataprocpb.UnimplementedClusterControllerServer
	dataprocpb.UnimplementedJobControllerServer
	dataprocpb.UnimplementedAutoscalingPolicyServiceServer
	dataprocpb.UnimplementedWorkflowTemplateServiceServer
	dataprocpb.UnimplementedNodeGroupControllerServer

	opts Options
	now  func() time.Time
	sem  chan struct{}

	mu        sync.Mutex
	batches   map[string]*dataprocpb.Batch
	sessions  map[string]*dataprocpb.Session
	templates map[string]*dataprocpb.SessionTemplate
	ops       map[string]*longrunningpb.Operation
	requests  map[string]string // request_id key -> operation name
	cancels   map[string]context.CancelFunc
	connects  map[string]Conn          // session name -> its server
	kernels   map[string]gatewayKernel // kernel gateway kernel ID -> its session
	activity  map[string]time.Time     // session name -> last Spark Connect call
	clusters  map[string]*dataprocpb.Cluster
	nodes     map[string]Nodes // cluster name -> its containers
	jobs      map[string]*dataprocpb.Job
	jobOps    map[string]string // job name -> its SubmitJobAsOperation operation
	policies  map[string]*dataprocpb.AutoscalingPolicy
	workflows map[string][]*dataprocpb.WorkflowTemplate // every version, the current last
	history   *historyServer
	apps      map[string]string // workload ID to Spark application ID (resolveApp)
	wfCancel  map[string]bool   // workflow operations cancelled
	// driverNext is each cluster's next driver node (placeDriverLocked).
	driverNext map[string]int
	stop       chan struct{} // closed by Close
	closed     bool
	wg         sync.WaitGroup

	bridgeOnce sync.Once
	bridgeLn   *bridgeListener
	bridgeSrv  *grpc.Server
}

// Locations are the regions the API answers, as Serverless for Apache
// Spark is regional.
func Locations() []string { return append([]string(nil), locations.Regions...) }

func validLocation(l string) bool {
	for _, r := range locations.Regions {
		if r == l {
			return true
		}
	}
	return false
}

// NewServer is the API over opts.
func NewServer(opts Options) (*Server, error) {
	if opts.Workers <= 0 {
		opts.Workers = 2
	}
	s := &Server{opts: opts, now: time.Now, sem: make(chan struct{}, opts.Workers),
		batches: map[string]*dataprocpb.Batch{}, sessions: map[string]*dataprocpb.Session{},
		templates: map[string]*dataprocpb.SessionTemplate{}, ops: map[string]*longrunningpb.Operation{},
		requests: map[string]string{}, cancels: map[string]context.CancelFunc{},
		connects: map[string]Conn{}, activity: map[string]time.Time{}, kernels: map[string]gatewayKernel{},
		clusters: map[string]*dataprocpb.Cluster{}, nodes: map[string]Nodes{}, jobs: map[string]*dataprocpb.Job{},
		jobOps: map[string]string{}, policies: map[string]*dataprocpb.AutoscalingPolicy{}, workflows: map[string][]*dataprocpb.WorkflowTemplate{}, wfCancel: map[string]bool{}, stop: make(chan struct{})}
	if err := s.load(); err != nil {
		return nil, err
	}
	s.restartClusters()
	return s, nil
}

func (s *Server) logf(format string, args ...any) {
	if s.opts.Logf != nil {
		s.opts.Logf(format, args...)
	}
}

// Close stops every running batch and session.
func (s *Server) Close() {
	s.mu.Lock()
	if !s.closed {
		close(s.stop)
	}
	s.closed = true
	cancels := make([]context.CancelFunc, 0, len(s.cancels))
	for _, c := range s.cancels {
		cancels = append(cancels, c)
	}
	connects := make([]Conn, 0, len(s.connects))
	for _, c := range s.connects {
		connects = append(connects, c)
	}
	s.mu.Unlock()
	for _, c := range cancels {
		c()
	}
	for _, c := range connects {
		c.Stop()
	}
	s.mu.Lock()
	nodes := make([]Nodes, 0, len(s.nodes))
	for _, n := range s.nodes {
		nodes = append(nodes, n)
	}
	s.nodes = map[string]Nodes{}
	hist := s.history
	s.mu.Unlock()
	for _, n := range nodes {
		n.Delete()
	}
	if hist != nil {
		hist.close()
	}
	s.wg.Wait()
	if s.bridgeSrv != nil {
		s.bridgeSrv.Stop()
	}
}

// Reset forgets project's resources ("" every project's), stopping what
// runs.
func (s *Server) Reset(project string) error {
	prefix := "projects/" + project + "/"
	match := func(name string) bool { return project == "" || strings.HasPrefix(name, prefix) }
	s.mu.Lock()
	var stop []Conn
	for name, c := range s.cancels {
		if match(name) {
			c()
		}
	}
	for name, c := range s.connects {
		if match(name) {
			stop = append(stop, c)
			delete(s.connects, name)
		}
	}
	var drop []Nodes
	for name, n := range s.nodes {
		if match(name) {
			drop = append(drop, n)
			delete(s.nodes, name)
		}
	}
	defer func() {
		for _, n := range drop {
			n.Delete()
		}
	}()
	for k := range s.clusters {
		if match(k) {
			delete(s.clusters, k)
		}
	}
	for k := range s.jobs {
		if match(k) {
			delete(s.jobs, k)
			delete(s.jobOps, k)
		}
	}
	for k := range s.policies {
		if match(k) {
			delete(s.policies, k)
		}
	}
	for k := range s.workflows {
		if match(k) {
			delete(s.workflows, k)
		}
	}
	for _, m := range []any{s.batches, s.sessions, s.templates, s.ops} {
		switch m := m.(type) {
		case map[string]*dataprocpb.Batch:
			for k := range m {
				if match(k) {
					delete(m, k)
				}
			}
		case map[string]*dataprocpb.Session:
			for k := range m {
				if match(k) {
					delete(m, k)
				}
			}
		case map[string]*dataprocpb.SessionTemplate:
			for k := range m {
				if match(k) {
					delete(m, k)
				}
			}
		case map[string]*longrunningpb.Operation:
			for k := range m {
				if match(k) {
					delete(m, k)
				}
			}
		}
	}
	for k, op := range s.requests {
		if match(op) {
			delete(s.requests, k)
		}
	}
	err := s.saveLocked()
	s.mu.Unlock()
	for _, c := range stop {
		c.Stop()
	}
	return err
}

// ---- names ----

var (
	projectRE  = regexp.MustCompile(`^projects/([^/]+)$`)
	parentRE   = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)$`)
	batchRE    = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/batches/([^/]+)$`)
	sessionRE  = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/sessions/([^/]+)$`)
	templateRE = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/sessionTemplates/([^/]+)$`)
	opRE       = regexp.MustCompile(`^projects/([^/]+)/(regions|locations)/([^/]+)/operations/([^/]+)$`)
	// idRE is the batch and session IDs' rule: "This value must be 4-63
	// characters. Valid characters are /[a-z][0-9]-/." (the API reference).
	idRE = regexp.MustCompile(`^[a-z0-9-]{4,63}$`)
	// templateIDRE: a runtime profile's ID is 4 to 63 lowercase letters,
	// digits and hyphens, starting and ending with a letter or digit
	// (create-serverless-sessions-templates, read 2026-11-22).
	templateIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,61}[a-z0-9]$`)
	// requestIDRE: "The value must contain only letters (a-z, A-Z),
	// numbers (0-9), underscores (_), and hyphens (-). The maximum length
	// is 40 characters."
	requestIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)
)

func checkParent(parent string) (project, location string, err error) {
	m := parentRE.FindStringSubmatch(parent)
	if m == nil {
		return "", "", apierror.InvalidArgument("parent %q must be projects/{project}/locations/{location}", parent)
	}
	if !validLocation(m[2]) {
		return "", "", apierror.InvalidArgument("Location %q is not a Dataproc region", m[2])
	}
	return m[1], m[2], nil
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func lastSegment(name string) string { return name[strings.LastIndex(name, "/")+1:] }

// opName is an operation's name: Dataproc's are under regions/.
func opName(project, location string) string {
	return "projects/" + project + "/regions/" + location + "/operations/" + newUUID()
}

// ---- runtime versions ----

// RuntimeVersions are the served runtime versions, as the versions page
// lists them (https://docs.cloud.google.com/managed-spark/docs/concepts/versions/serverless-versions,
// read 2026-11-22): 1.2, 2.2 and 2.3 LTS run Spark 3.5, 3.0 Spark 4.0 and
// 4.0 Spark 4.2.
var RuntimeVersions = []string{"1.2", "2.2", "2.3", "3.0", "4.0"}

// DefaultRuntime is the version a workload with none runs on: "2.2 LTS
// ... This is the default version" (the versions page).
const DefaultRuntime = "2.2"

// runtimeVersion is the MAJOR.MINOR a workload asks for ("" the default),
// or an INVALID_ARGUMENT for one not served.
func runtimeVersion(v string) (string, error) {
	if v == "" {
		return DefaultRuntime, nil
	}
	mm := v
	if parts := strings.Split(v, "."); len(parts) > 2 {
		mm = parts[0] + "." + parts[1]
	}
	for _, r := range RuntimeVersions {
		if r == mm {
			return v, nil
		}
	}
	return "", apierror.InvalidArgument("Unsupported runtime version %q. Supported versions are: %s", v, strings.Join(RuntimeVersions, ", "))
}

// ---- persistence ----

type persisted struct {
	Batches   []json.RawMessage   `json:"batches"`
	Sessions  []json.RawMessage   `json:"sessions"`
	Templates []json.RawMessage   `json:"templates"`
	Ops       []json.RawMessage   `json:"operations"`
	Requests  map[string]string   `json:"requests"`
	Clusters  []json.RawMessage   `json:"clusters,omitempty"`
	Jobs      []json.RawMessage   `json:"jobs,omitempty"`
	JobNames  []string            `json:"jobNames,omitempty"`
	JobOps    map[string]string   `json:"jobOperations,omitempty"`
	Policies  []json.RawMessage   `json:"autoscalingPolicies,omitempty"`
	Workflows [][]json.RawMessage `json:"workflowTemplates,omitempty"`
}

func (s *Server) load() error {
	if s.opts.State == "" {
		return nil
	}
	b, err := os.ReadFile(s.opts.State)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var p persisted
	if err := json.Unmarshal(b, &p); err != nil {
		return fmt.Errorf("read %s: %w", s.opts.State, err)
	}
	now := timestamppb.New(s.now())
	for _, raw := range p.Batches {
		var x dataprocpb.Batch
		if err := protojson.Unmarshal(raw, &x); err == nil {
			// A batch the CLI's last run left running ran in a container
			// that run removed: it failed.
			if !batchDone(x.State) {
				x.StateHistory = append(x.StateHistory, &dataprocpb.Batch_StateHistory{State: x.State, StateMessage: x.StateMessage, StateStartTime: x.StateTime})
				x.State, x.StateMessage, x.StateTime = dataprocpb.Batch_FAILED, "The batch was stopped when the instance stopped.", now
			}
			s.batches[x.Name] = &x
		}
	}
	for _, raw := range p.Sessions {
		var x dataprocpb.Session
		if err := protojson.Unmarshal(raw, &x); err == nil {
			if !sessionDone(x.State) {
				x.StateHistory = append(x.StateHistory, &dataprocpb.Session_SessionStateHistory{State: x.State, StateMessage: x.StateMessage, StateStartTime: x.StateTime})
				x.State, x.StateMessage, x.StateTime = dataprocpb.Session_TERMINATED, "The session was terminated when the instance stopped.", now
				if x.RuntimeInfo != nil {
					x.RuntimeInfo.Endpoints = nil
				}
			}
			s.sessions[x.Name] = &x
		}
	}
	for _, raw := range p.Templates {
		var x dataprocpb.SessionTemplate
		if err := protojson.Unmarshal(raw, &x); err == nil {
			s.templates[x.Name] = &x
		}
	}
	for _, raw := range p.Ops {
		var x longrunningpb.Operation
		if err := protojson.Unmarshal(raw, &x); err == nil {
			s.ops[x.Name] = &x
		}
	}
	for k, v := range p.Requests {
		s.requests[k] = v
	}
	for _, raw := range p.Clusters {
		var x dataprocpb.Cluster
		if err := protojson.Unmarshal(raw, &x); err == nil {
			s.clusters[clusterKey(x.ProjectId, regionOfCluster(&x), x.ClusterName)] = &x
		}
	}
	for i, raw := range p.Jobs {
		var x dataprocpb.Job
		if i < len(p.JobNames) && protojson.Unmarshal(raw, &x) == nil {
			if !jobDone(&x) {
				// Its driver ran in a container that run removed.
				if x.Status != nil {
					x.StatusHistory = append(x.StatusHistory, x.Status)
				}
				x.Status = &dataprocpb.JobStatus{State: dataprocpb.JobStatus_ERROR, Details: "The job was stopped when the instance stopped.", StateStartTime: now}
				x.Done = true
			}
			s.jobs[p.JobNames[i]] = &x
		}
	}
	for k, v := range p.JobOps {
		s.jobOps[k] = v
	}
	for _, raw := range p.Policies {
		var x dataprocpb.AutoscalingPolicy
		if err := protojson.Unmarshal(raw, &x); err == nil {
			s.policies[policyKeyOf(x.Name)] = &x
		}
	}
	for _, versions := range p.Workflows {
		var vs []*dataprocpb.WorkflowTemplate
		for _, raw := range versions {
			var x dataprocpb.WorkflowTemplate
			if err := protojson.Unmarshal(raw, &x); err == nil {
				vs = append(vs, &x)
			}
		}
		if len(vs) > 0 {
			s.workflows[workflowKeyOf(vs[0].Name)] = vs
		}
	}
	// Operations left running end with their resource.
	for name, op := range s.ops {
		if !op.Done {
			s.finishOpLocked(name, nil, apierror.Aborted("The operation was stopped when the instance stopped."))
		}
	}
	return nil
}

func (s *Server) saveLocked() error {
	if s.opts.State == "" {
		return nil
	}
	var p persisted
	p.Requests = s.requests
	m := protojson.MarshalOptions{}
	for _, k := range sortedKeys(s.batches) {
		b, _ := m.Marshal(s.batches[k])
		p.Batches = append(p.Batches, b)
	}
	for _, k := range sortedKeys(s.sessions) {
		b, _ := m.Marshal(s.sessions[k])
		p.Sessions = append(p.Sessions, b)
	}
	for _, k := range sortedKeys(s.templates) {
		b, _ := m.Marshal(s.templates[k])
		p.Templates = append(p.Templates, b)
	}
	for _, k := range sortedKeys(s.ops) {
		b, _ := m.Marshal(s.ops[k])
		p.Ops = append(p.Ops, b)
	}
	for _, k := range sortedKeys(s.clusters) {
		b, _ := m.Marshal(s.clusters[k])
		p.Clusters = append(p.Clusters, b)
	}
	for _, k := range sortedKeys(s.jobs) {
		b, _ := m.Marshal(s.jobs[k])
		p.Jobs, p.JobNames = append(p.Jobs, b), append(p.JobNames, k)
	}
	p.JobOps = s.jobOps
	for _, k := range sortedKeys(s.policies) {
		b, _ := m.Marshal(s.policies[k])
		p.Policies = append(p.Policies, b)
	}
	for _, k := range sortedKeys(s.workflows) {
		var vs []json.RawMessage
		for _, v := range s.workflows[k] {
			b, _ := m.Marshal(v)
			vs = append(vs, b)
		}
		p.Workflows = append(p.Workflows, vs)
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.opts.State), 0o755); err != nil {
		return err
	}
	tmp := s.opts.State + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.opts.State)
}

func (s *Server) save() {
	if err := s.saveLocked(); err != nil {
		s.logf("dataproc: save state: %v", err)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func clone[T proto.Message](m T) T { return proto.Clone(m).(T) }

func (s *Server) caller(ctx context.Context) string {
	if s.opts.Caller != nil {
		if c := s.opts.Caller(ctx); c != "" {
			return c
		}
	}
	return ""
}

// restartClusters starts again the clusters the CLI's last run left
// running: their containers were removed when it stopped.
func (s *Server) restartClusters() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, c := range s.clusters {
		switch c.GetStatus().GetState() {
		case dataprocpb.ClusterStatus_DELETING:
			delete(s.clusters, name)
		case dataprocpb.ClusterStatus_STOPPING:
			s.setClusterStateLocked(c, dataprocpb.ClusterStatus_STOPPED, "")
		case dataprocpb.ClusterStatus_STOPPED, dataprocpb.ClusterStatus_ERROR:
		default:
			s.wg.Add(1)
			go s.startCluster(name, "", true)
		}
	}
	s.save()
}
