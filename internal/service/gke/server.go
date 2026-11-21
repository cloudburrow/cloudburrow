// Package gke serves the Kubernetes Engine API (container.googleapis.com v1,
// google.container.v1.ClusterManager, #1913): clusters, node pools and the
// operations that make them.
//
// Every cluster is a real local Kubernetes cluster that a Backend makes (a
// kind cluster of its own, wired in cmd/cloudburrow from internal/cluster),
// and every node of a Standard cluster's node pools is a node of it, so
// resizing a pool adds or removes Kubernetes nodes. The package itself
// never runs a container: it knows endpoints and certificates, not pods.
//
// Records are kept in memory and, in persistent mode, in a JSON file of the
// instance's directory, so the clusters an instance made are known again
// after a restart.
package gke

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// Access is how a client reaches a cluster's Kubernetes API: its endpoint
// (host:port, since the local API server is not on 443) and the
// base64-encoded PEM certificates the Kubernetes Engine API returns in
// MasterAuth.
type Access struct {
	Endpoint   string
	CACert     string
	ClientCert string
	ClientKey  string
}

// Node is one node a Backend made for a node pool.
type Node struct {
	Pool string
	Name string
	// Image is the node image it runs.
	Image string
}

// Backend makes and removes the local clusters behind the API. Its names
// are the Server's: kind is the backing cluster's name, which the Server
// derives from the instance and the cluster's resource name.
type Backend interface {
	// Create makes the cluster's control plane. A Standard cluster's
	// control plane takes no workloads (its pools' nodes do); an Autopilot
	// cluster's does, since Autopilot has no node pools to manage.
	Create(ctx context.Context, kind, image string, standard bool) (Access, error)
	// Resize makes pool have n nodes, adding or removing nodes, with the
	// pool's labels (cloud.google.com/gke-nodepool among them).
	Resize(ctx context.Context, kind, image, pool string, n int) error
	// Delete removes the cluster and every node of it.
	Delete(ctx context.Context, kind string) error
	// FreeBytes is the free disk space where clusters are made.
	FreeBytes() (uint64, error)
}

// Version is a Kubernetes Engine version served: its GKE name and the node
// image that runs it.
type Version struct {
	GKE   string
	Image string
}

// DefaultMinFreeBytes is the free disk space below which CreateCluster and
// growing a node pool answer RESOURCE_EXHAUSTED: a kind cluster's nodes
// each keep a containerd store of their own.
const DefaultMinFreeBytes = 20 << 30

// MaxNodesPerCluster bounds a local cluster's worker nodes, a limit of this
// machine rather than of Google's (a cluster there may have 15,000).
const MaxNodesPerCluster = 9

// MaxClusters bounds the clusters one instance runs at once.
const MaxClusters = 5

// Options configure a Server.
type Options struct {
	// Instance is the instance's own kind cluster name, the prefix of
	// every backing cluster's name so two instances never share one.
	Instance string
	// ProjectNumber resolves a project ID to its number for selfLinks;
	// nil uses the ID.
	ProjectNumber func(project string) string
	// State is the JSON file the records are kept in; empty keeps them in
	// memory only.
	State string
	// Versions are the versions served, the first the default.
	Versions []Version
	// MinFreeBytes is DefaultMinFreeBytes when zero.
	MinFreeBytes uint64
	// Now is time.Now when nil.
	Now func() time.Time
	// Networks are the Compute Engine networks a cluster's network and
	// subnetwork are checked against (vpc.go, #1936); nil, or nil from it,
	// checks none.
	Networks func() Networks
	// AutoUpgradeInterval is how often automatic upgrades are started
	// (upgrade.go): a minute when zero, never when negative.
	AutoUpgradeInterval time.Duration
	// AutoscaleInterval is how often the cluster autoscaler and auto-repair
	// look at the clusters (autoscale.go): ten seconds when zero, never
	// when negative.
	AutoscaleInterval time.Duration
	// ScaleDownUnneeded is DefaultScaleDownUnneeded when zero.
	ScaleDownUnneeded time.Duration
	// RepairAfter is DefaultRepairAfter when zero.
	RepairAfter time.Duration
}

// Server is the Kubernetes Engine API.
type Server struct {
	containerpb.UnimplementedClusterManagerServer

	opts    Options
	backend Backend
	bg      context.Context
	stop    context.CancelFunc
	wg      sync.WaitGroup

	mu       sync.Mutex
	clusters map[string]*entry   // by projects/P/locations/L/clusters/C
	ops      map[string]*opEntry // by projects/P/locations/L/operations/ID
	seq      int

	// clockOffset is how far the Kubernetes Engine clock is ahead of
	// Options.Now, in nanoseconds (clock.go, #2076).
	clockOffset atomic.Int64
}

// entry is one cluster and what the API never returns of it.
type entry struct {
	cluster *containerpb.Cluster
	project string
	kind    string
	access  Access
	// busy is the operation running on the cluster: Google runs one at a
	// time and refuses another.
	busy string
	// soak ends a blue-green upgrade's pool soak early, by pool.
	soak map[string]chan struct{}
	// scale is the cluster autoscaler's memory of the cluster.
	scale *autoscaleState
}

type opEntry struct {
	op      *containerpb.Operation
	project string
	cancel  context.CancelFunc
	// pause, when set, stops the work gracefully at its next step instead
	// of cancelling it: a node pool upgrade's nodes that have started
	// complete (CancelOperation, RollbackNodePoolUpgrade, #2076).
	pause     chan struct{}
	pauseOnce sync.Once
	// pool is the node pool a node pool upgrade works on.
	pool *containerpb.NodePool
	// done is closed when the work has ended and done has run.
	done chan struct{}
}

// stopGracefully asks o's work to stop at its next step.
func (o *opEntry) stopGracefully() { o.pauseOnce.Do(func() { close(o.pause) }) }

// NewServer returns a Server over backend, with its records restored.
func NewServer(backend Backend, opts Options) (*Server, error) {
	if len(opts.Versions) == 0 {
		return nil, errors.New("gke: no Kubernetes version to serve")
	}
	if opts.MinFreeBytes == 0 {
		opts.MinFreeBytes = DefaultMinFreeBytes
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	bg, stop := context.WithCancel(context.Background())
	s := &Server{opts: opts, backend: backend, bg: bg, stop: stop,
		clusters: map[string]*entry{}, ops: map[string]*opEntry{}}
	if err := s.restore(); err != nil {
		stop()
		return nil, err
	}
	if opts.AutoUpgradeInterval == 0 {
		opts.AutoUpgradeInterval = time.Minute
	}
	if _, ok := backend.(Upgrader); ok && opts.AutoUpgradeInterval > 0 {
		s.wg.Add(1)
		go s.autoUpgradeLoop(opts.AutoUpgradeInterval)
	}
	if opts.AutoscaleInterval == 0 {
		opts.AutoscaleInterval = 10 * time.Second
	}
	if _, ok := backend.(Controller); ok && opts.AutoscaleInterval > 0 {
		s.wg.Add(1)
		go s.autoscaleLoop(opts.AutoscaleInterval)
	}
	return s, nil
}

// Register serves the API on g.
func (s *Server) Register(g grpc.ServiceRegistrar) {
	containerpb.RegisterClusterManagerServer(g, s)
}

// Transcoder is the JSON API's configuration, with nothing registered.
func Transcoder() *rest.Transcoder {
	return &rest.Transcoder{Packages: []string{"google.container.v1"}, SnakeCaseMasks: true}
}

// Routes is every bound route.
func Routes() []rest.Route { return Transcoder().Routes() }

// NewRESTHandler is the JSON API over s, on the gRPC port.
func NewRESTHandler(s *Server) http.Handler {
	t := Transcoder()
	s.Register(t)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A node pool's instance group managers (compute.go).
		if strings.HasPrefix(r.URL.Path, "/compute/") {
			s.serveCompute(w, r)
			return
		}
		if r.URL.Path == ClockAdvancePath {
			s.serveClock(w, r)
			return
		}
		t.ServeHTTP(w, r)
	})
}

// Close stops the operations still running and waits for them.
func (s *Server) Close() {
	s.stop()
	s.wg.Wait()
}

// KindName is the backing cluster's name for a cluster: the instance's
// own cluster name (which carries the ownership prefix), "-gke-", then a
// hash of the resource name, short enough for the node containers' host
// names.
func KindName(instanceCluster, name string) string {
	sum := sha256.Sum256([]byte(name))
	return instanceCluster + "-gke-" + hex.EncodeToString(sum[:])[:8]
}

// Owned lists the backing clusters' names, for the instance's cleanup.
func (s *Server) Owned() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, e := range s.clusters {
		out = append(out, e.kind)
	}
	sort.Strings(out)
	return out
}

// DeleteAll deletes every cluster of project ("" for every project) and its
// backing cluster, waiting for each, for `cloudburrow reset` and for an
// ephemeral instance's stop.
func (s *Server) DeleteAll(ctx context.Context, project string) error {
	s.mu.Lock()
	var gone []*entry
	for k, e := range s.clusters {
		if project == "" || e.project == project {
			gone = append(gone, e)
			delete(s.clusters, k)
		}
	}
	for k, o := range s.ops {
		if project == "" || o.project == project {
			if o.cancel != nil {
				o.cancel()
			}
			delete(s.ops, k)
		}
	}
	s.mu.Unlock()
	var errs []error
	for _, e := range gone {
		if err := s.backend.Delete(ctx, e.kind); err != nil {
			errs = append(errs, err)
		}
	}
	s.mu.Lock()
	err := s.saveLocked()
	s.mu.Unlock()
	return errors.Join(append(errs, err)...)
}

// ---- persistence ----

type savedCluster struct {
	Project    string          `json:"project"`
	Kind       string          `json:"kind"`
	Access     Access          `json:"access"`
	Cluster    json.RawMessage `json:"cluster"`
	Operations []string        `json:"-"`
}

type savedState struct {
	Clusters   []savedCluster    `json:"clusters"`
	Operations []json.RawMessage `json:"operations"`
	OpProjects []string          `json:"operationProjects"`
	Seq        int               `json:"seq"`
}

func (s *Server) restore() error {
	if s.opts.State == "" {
		return nil
	}
	b, err := os.ReadFile(s.opts.State)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read Kubernetes Engine state: %w", err)
	}
	var st savedState
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("decode Kubernetes Engine state: %w", err)
	}
	for _, sc := range st.Clusters {
		c := &containerpb.Cluster{}
		if err := protojson.Unmarshal(sc.Cluster, c); err != nil {
			return fmt.Errorf("decode a Kubernetes Engine cluster: %w", err)
		}
		// An operation that was running when the instance stopped did not
		// finish: the cluster is in error until it is deleted.
		if c.Status == containerpb.Cluster_PROVISIONING || c.Status == containerpb.Cluster_RECONCILING {
			c.Status = containerpb.Cluster_ERROR
			c.StatusMessage = "The operation was interrupted by a restart of the CloudBurrow instance."
		}
		s.clusters[resourceName(sc.Project, c.Location, c.Name)] = &entry{cluster: c, project: sc.Project, kind: sc.Kind, access: sc.Access}
	}
	for i, raw := range st.Operations {
		op := &containerpb.Operation{}
		if err := protojson.Unmarshal(raw, op); err != nil {
			return fmt.Errorf("decode a Kubernetes Engine operation: %w", err)
		}
		if op.Status != containerpb.Operation_DONE {
			op.Status = containerpb.Operation_DONE
			op.Error = status.New(codes.Aborted, "The operation was interrupted by a restart of the CloudBurrow instance.").Proto()
			op.EndTime = s.stamp()
		}
		project := ""
		if i < len(st.OpProjects) {
			project = st.OpProjects[i]
		}
		s.ops[opName(project, op.Location, op.Name)] = &opEntry{op: op, project: project}
	}
	s.seq = st.Seq
	return nil
}

// saveLocked writes the records; s.mu is held.
func (s *Server) saveLocked() error {
	if s.opts.State == "" {
		return nil
	}
	var st savedState
	st.Seq = s.seq
	keys := make([]string, 0, len(s.clusters))
	for k := range s.clusters {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		e := s.clusters[k]
		raw, err := protojson.Marshal(e.cluster)
		if err != nil {
			return err
		}
		st.Clusters = append(st.Clusters, savedCluster{Project: e.project, Kind: e.kind, Access: e.access, Cluster: raw})
	}
	okeys := make([]string, 0, len(s.ops))
	for k := range s.ops {
		okeys = append(okeys, k)
	}
	sort.Strings(okeys)
	for _, k := range okeys {
		raw, err := protojson.Marshal(s.ops[k].op)
		if err != nil {
			return err
		}
		st.Operations = append(st.Operations, raw)
		st.OpProjects = append(st.OpProjects, s.ops[k].project)
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.opts.State), 0o700); err != nil {
		return err
	}
	tmp := s.opts.State + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.opts.State)
}

// ---- names ----

func resourceName(project, location, cluster string) string {
	return "projects/" + project + "/locations/" + location + "/clusters/" + cluster
}

func opName(project, location, id string) string {
	return "projects/" + project + "/locations/" + location + "/operations/" + id
}

func (s *Server) projectNumber(project string) string {
	if s.opts.ProjectNumber != nil {
		if n := s.opts.ProjectNumber(project); n != "" {
			return n
		}
	}
	return project
}

func (s *Server) selfLink(project, location, rest string) string {
	return "https://container.googleapis.com/v1/projects/" + s.projectNumber(project) + "/" + locationKind(location) + "/" + location + "/" + rest
}

// locationKind is the selfLink's collection, zones or locations, as Google
// writes zonal clusters' links under zones/.
func locationKind(location string) string {
	if isZone(location) {
		return "zones"
	}
	return "locations"
}

func (s *Server) stamp() string {
	return s.now().UTC().Format(time.RFC3339Nano)
}

// parsed is a request's resource, from its name or its deprecated fields.
type parsed struct {
	project, location, cluster, pool, op string
}

// parseName reads projects/P/locations/L[/clusters/C[/nodePools/N]] or
// .../operations/O, or the deprecated project_id, zone and IDs.
func parseName(name, project, zone, cluster, pool, op string, want string) (parsed, error) {
	if name != "" {
		parts := strings.Split(name, "/")
		var p parsed
		if len(parts) < 4 || parts[0] != "projects" || parts[2] != "locations" || parts[1] == "" || parts[3] == "" {
			return p, status.Errorf(codes.InvalidArgument, "Invalid resource name %q.", name)
		}
		p.project, p.location = parts[1], parts[3]
		rest := parts[4:]
		for len(rest) >= 2 {
			switch rest[0] {
			case "clusters":
				p.cluster = rest[1]
			case "nodePools":
				p.pool = rest[1]
			case "operations":
				p.op = rest[1]
			default:
				return p, status.Errorf(codes.InvalidArgument, "Invalid resource name %q.", name)
			}
			rest = rest[2:]
		}
		if len(rest) != 0 {
			return p, status.Errorf(codes.InvalidArgument, "Invalid resource name %q.", name)
		}
		return p, p.require(want, name)
	}
	p := parsed{project: project, location: zone, cluster: cluster, pool: pool, op: op}
	if p.project == "" {
		return p, status.Error(codes.InvalidArgument, "Required \"name\" or \"project_id\" is missing.")
	}
	if p.location == "" {
		return p, status.Error(codes.InvalidArgument, "Required \"name\" or \"zone\" is missing.")
	}
	return p, p.require(want, "")
}

func (p parsed) require(want, name string) error {
	missing := func(f string) error {
		if name != "" {
			return status.Errorf(codes.InvalidArgument, "Invalid resource name %q: it has no %s.", name, f)
		}
		return status.Errorf(codes.InvalidArgument, "Required %q is missing.", f)
	}
	switch want {
	case "cluster":
		if p.cluster == "" {
			return missing("cluster_id")
		}
	case "pool":
		if p.cluster == "" {
			return missing("cluster_id")
		}
		if p.pool == "" {
			return missing("node_pool_id")
		}
	case "op":
		if p.op == "" {
			return missing("operation_id")
		}
	}
	return nil
}

// notFound is Google's NOT_FOUND for a resource.
func notFound(name string) error {
	return status.Errorf(codes.NotFound, "Not found: %s.", name)
}

// lookup finds a cluster; s.mu is held.
func (s *Server) lookupLocked(p parsed) (*entry, error) {
	name := resourceName(p.project, p.location, p.cluster)
	e := s.clusters[name]
	if e == nil {
		return nil, notFound(name)
	}
	return e, nil
}

// ---- operations ----

// startOp records a RUNNING operation on target and runs work in the
// background; done receives work's error. s.mu is held.
func (s *Server) startOpLocked(p parsed, typ containerpb.Operation_Type, target string, e *entry,
	work func(ctx context.Context) error, done func(err error)) *containerpb.Operation {
	s.seq++
	now := s.now()
	id := "operation-" + strconv.FormatInt(now.UnixMilli(), 10) + "-" + shortHash(fmt.Sprint(p, s.seq))
	op := &containerpb.Operation{
		Name:          id,
		Zone:          p.location,
		Location:      p.location,
		OperationType: typ,
		Status:        containerpb.Operation_RUNNING,
		SelfLink:      s.selfLink(p.project, p.location, "operations/"+id),
		TargetLink:    target,
		StartTime:     now.UTC().Format(time.RFC3339Nano),
	}
	ctx, cancel := context.WithCancel(s.bg)
	ctx = context.WithValue(ctx, opKey{}, op)
	oe := &opEntry{op: op, project: p.project, cancel: cancel, done: make(chan struct{})}
	s.ops[opName(p.project, p.location, id)] = oe
	if e != nil {
		e.busy = id
	}
	_ = s.saveLocked()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		defer close(oe.done)
		err := work(ctx)
		if err == nil && ctx.Err() != nil {
			err = ctx.Err()
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if e != nil && e.busy == id {
			e.busy = ""
		}
		op.Status = containerpb.Operation_DONE
		op.EndTime = s.stamp()
		if errors.Is(err, errPaused) {
			// Paused at the end of the maintenance window: done, not
			// failed; the next window resumes it.
			op.StatusMessage = pausedMessage
		} else if err != nil {
			st, ok := status.FromError(err)
			if !ok || errors.Is(err, context.Canceled) {
				if errors.Is(err, context.Canceled) {
					st = status.New(codes.Canceled, "Operation was cancelled.")
				} else {
					st = status.New(codes.Internal, err.Error())
				}
			}
			op.Error = st.Proto()
			op.StatusMessage = st.Message()
		}
		if done != nil {
			done(err)
		}
		_ = s.saveLocked()
	}()
	return proto.Clone(op).(*containerpb.Operation)
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

// GetOperation returns an operation.
func (s *Server) GetOperation(_ context.Context, req *containerpb.GetOperationRequest) (*containerpb.Operation, error) {
	p, err := parseName(req.GetName(), req.GetProjectId(), req.GetZone(), "", "", req.GetOperationId(), "op")
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.ops[opName(p.project, p.location, p.op)]
	if o == nil {
		return nil, notFound(opName(p.project, p.location, p.op))
	}
	return proto.Clone(o.op).(*containerpb.Operation), nil
}

// ListOperations lists a location's operations, or every location's for
// "-".
func (s *Server) ListOperations(_ context.Context, req *containerpb.ListOperationsRequest) (*containerpb.ListOperationsResponse, error) {
	p, err := parseName(req.GetParent(), req.GetProjectId(), req.GetZone(), "", "", "", "")
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := &containerpb.ListOperationsResponse{}
	for _, o := range s.ops {
		if o.project == p.project && (p.location == "-" || o.op.Location == p.location) {
			resp.Operations = append(resp.Operations, proto.Clone(o.op).(*containerpb.Operation))
		}
	}
	sort.Slice(resp.Operations, func(i, j int) bool { return resp.Operations[i].StartTime < resp.Operations[j].StartTime })
	return resp, nil
}

// CancelOperation stops a running operation: its work is cancelled and it
// ends with CANCELLED. A done operation cannot be cancelled.
func (s *Server) CancelOperation(_ context.Context, req *containerpb.CancelOperationRequest) (*emptyResp, error) {
	p, err := parseName(req.GetName(), req.GetProjectId(), req.GetZone(), "", "", req.GetOperationId(), "op")
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.ops[opName(p.project, p.location, p.op)]
	if o == nil {
		return nil, notFound(opName(p.project, p.location, p.op))
	}
	if o.op.Status == containerpb.Operation_DONE {
		return nil, status.Errorf(codes.FailedPrecondition, "Operation %s is already done.", p.op)
	}
	if o.pause != nil {
		// A node pool upgrade: "Cancelling pauses the upgrade, stopping
		// GKE from upgrading new nodes ... Nodes that have started the
		// upgrade complete it"; a blue-green one "is paused in its
		// current phase", which "can be used at all phases except the
		// Delete blue pool phase" (re-read 2026-11-20:
		// concepts/node-pool-upgrade-strategies).
		if o.pool.GetUpdateInfo().GetBlueGreenInfo().GetPhase() == containerpb.NodePool_UpdateInfo_BlueGreenInfo_DELETING_BLUE_POOL {
			// UNVERIFIED wording (re-read 2026-11-20: concepts/node-pool-upgrade-strategies).
			return nil, status.Errorf(codes.FailedPrecondition, "Node pool %q is deleting its blue nodes: a blue-green upgrade cannot be cancelled in the DELETING_BLUE_POOL phase.", o.pool.GetName())
		}
		o.op.Status = containerpb.Operation_ABORTING
		o.stopGracefully()
		return &emptyResp{}, nil
	}
	o.op.Status = containerpb.Operation_ABORTING
	if o.cancel != nil {
		o.cancel()
	}
	return &emptyResp{}, nil
}
