package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/cluster"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/service/compute"
	"github.com/cloudburrow/cloudburrow/internal/service/gke"
	"github.com/cloudburrow/cloudburrow/internal/telemetry"
	grpctransport "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// gkeService serves the Kubernetes Engine API (#1913) in the CLI process,
// gRPC and JSON on one port as container.googleapis.com serves both. Each
// cluster it makes is a kind cluster of its own, beside the instance's,
// whose kubeconfig and kind configuration are in the instance's gke
// directory. An ephemeral instance deletes them when it stops; a persistent
// one keeps them, and `cloudburrow delete` removes them with the instance.
type gkeService struct {
	cfg       config.Config
	tracing   *telemetry.Tracing
	calls     grpctransport.Observer
	interpose []grpc.UnaryServerInterceptor
	requests  func(rest.Request)
	// wrapJSON wraps the JSON API (IAM enforcement, #1879); nil is none.
	wrapJSON func(http.Handler) http.Handler
	server   *grpctransport.Server
	api      *gke.Server
	// networks are Compute Engine's (#1936), nil without compute.
	networks func() gke.Networks
}

func newGKEService(cfg config.Config) *gkeService {
	if !gkeEnabled(cfg) {
		return nil
	}
	return &gkeService{cfg: cfg}
}

// gkeEnabled is whether `up` binds the Kubernetes Engine API's port.
func gkeEnabled(cfg config.Config) bool {
	return serviceEnabled(cfg, config.ServiceGKE) && cfg.Endpoints.GKE >= 0
}

func (s *gkeService) register(coord *lifecycle.Coordinator) {
	if s != nil {
		coord.Register(s)
	}
}

func (s *gkeService) Name() string { return "gke" }

// API is the server, available after Start.
func (s *gkeService) API() *gke.Server {
	if s == nil {
		return nil
	}
	return s.api
}

// clusterAccess reaches a running cluster's Kubernetes API, for
// Eventarc's GKE destinations (#1877).
func (s *gkeService) clusterAccess(name string) (runadapter.GKEAccess, bool) {
	api := s.API()
	if api == nil {
		return runadapter.GKEAccess{}, false
	}
	a, ok := api.ClusterAccess(name)
	return runadapter.GKEAccess(a), ok
}

// Addr is the bound address.
func (s *gkeService) Addr() string {
	if s == nil || s.server == nil {
		return ""
	}
	return s.server.Addr()
}

// gkeDir holds each Kubernetes Engine cluster's kubeconfig.
func gkeDir(cfg config.Config) string { return filepath.Join(cfg.InstanceDir(), "gke") }

// gkeVersion is the Kubernetes Engine version a kind node image runs:
// kindest/node:v1.36.4 is 1.36.4-gke.0, the build number naming no
// Google release.
func gkeVersion(image string) string {
	ref := image
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	tag := ref[strings.LastIndex(ref, ":")+1:]
	return strings.TrimPrefix(tag, "v") + "-gke.0"
}

// gkeVersions are the versions served, newest (the instance's node image)
// first, then the pinned node image one minor below it, which a cluster
// can be made on and upgraded from (#2069). An instance on another node
// image serves the previous one only if it is older.
func gkeVersions(image string) []gke.Version {
	out := []gke.Version{{GKE: gkeVersion(image), Image: image}}
	prev := config.GKEPreviousNodeImage
	if gkeVersionLess(gkeVersion(prev), gkeVersion(image)) {
		out = append(out, gke.Version{GKE: gkeVersion(prev), Image: prev})
	}
	return out
}

// gkeBackend adapts internal/cluster's GKE to the API's Backend.
type gkeBackend struct{ g *cluster.GKE }

func (b gkeBackend) Create(ctx context.Context, kind, image string, standard bool) (gke.Access, error) {
	a, err := b.g.Create(ctx, kind, image, standard)
	return gke.Access(a), err
}

func (b gkeBackend) Resize(ctx context.Context, kind, image, pool string, n int) error {
	return b.g.Resize(ctx, kind, image, pool, n)
}

func (b gkeBackend) UpgradeControlPlane(ctx context.Context, kind, image string) error {
	return b.g.UpgradeControlPlane(ctx, kind, image)
}

func (b gkeBackend) PoolNodes(ctx context.Context, kind, pool string) ([]gke.Node, error) {
	nodes, err := b.g.PoolNodes(ctx, kind, pool)
	out := make([]gke.Node, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, gke.Node{Pool: pool, Name: n.Name, Image: n.Image})
	}
	return out, err
}

func (b gkeBackend) AddNode(ctx context.Context, kind, image, pool string) (string, error) {
	return b.g.AddNode(ctx, kind, image, pool)
}

func (b gkeBackend) CordonNode(ctx context.Context, kind, name string) error {
	return b.g.CordonNode(ctx, kind, name)
}

func (b gkeBackend) UncordonNode(ctx context.Context, kind, name string) error {
	return b.g.UncordonNode(ctx, kind, name)
}

func (b gkeBackend) DrainNode(ctx context.Context, kind, name string) error {
	return b.g.DrainNode(ctx, kind, name)
}

func (b gkeBackend) ForceDrainNode(ctx context.Context, kind, name string) error {
	return b.g.ForceDrainNode(ctx, kind, name)
}

func (b gkeBackend) RemoveNode(ctx context.Context, kind, name string) error {
	return b.g.RemoveNode(ctx, kind, name)
}

func (b gkeBackend) Get(ctx context.Context, kind, path string) ([]byte, error) {
	return b.g.Get(ctx, kind, path)
}

func (b gkeBackend) SetLegacyAbac(ctx context.Context, kind string, enabled bool) error {
	return b.g.SetLegacyAbac(ctx, kind, enabled)
}

func (b gkeBackend) SetNetworkPolicy(ctx context.Context, kind string, enabled bool) error {
	return b.g.SetNetworkPolicy(ctx, kind, enabled)
}

func (b gkeBackend) RepairNode(ctx context.Context, kind, image, pool, name string) error {
	return b.g.RepairNode(ctx, kind, image, pool, name)
}

func (b gkeBackend) StartCredentialRotation(ctx context.Context, kind string) (gke.Access, error) {
	a, err := b.g.StartCredentialRotation(ctx, kind)
	return gke.Access(a), err
}

func (b gkeBackend) CompleteCredentialRotation(ctx context.Context, kind string) (gke.Access, error) {
	a, err := b.g.CompleteCredentialRotation(ctx, kind)
	return gke.Access(a), err
}

func (b gkeBackend) SetEmulatedVersion(ctx context.Context, kind, minor string) error {
	return b.g.SetEmulatedVersion(ctx, kind, minor)
}

func (b gkeBackend) PlaceNodes(ctx context.Context, kind, pool string, zones []string) error {
	return b.g.PlaceNodes(ctx, kind, pool, zones)
}

func (b gkeBackend) ConfigureNodes(ctx context.Context, kind, pool string, labels map[string]string, taints []gke.NodeTaint, apply bool) error {
	ts := make([]cluster.NodeTaint, 0, len(taints))
	for _, t := range taints {
		ts = append(ts, cluster.NodeTaint{Key: t.Key, Value: t.Value, Effect: t.Effect})
	}
	return b.g.ConfigureNodes(ctx, kind, pool, labels, ts, apply)
}

func (b gkeBackend) ConfigureKubelet(ctx context.Context, kind, pool string, settings map[string]any) error {
	return b.g.ConfigureKubelet(ctx, kind, pool, settings)
}

func (b gkeBackend) ConfigureMachine(ctx context.Context, kind, pool string, m gke.NodeMachine) error {
	return b.g.ConfigureMachine(ctx, kind, pool, cluster.NodeMachine(m))
}

func (b gkeBackend) RollbackControlPlane(ctx context.Context, kind, image string) error {
	return b.g.RollbackControlPlane(ctx, kind, image)
}

func (b gkeBackend) KeepEndpoint(ctx context.Context, kind string) error {
	return b.g.KeepEndpoint(ctx, kind)
}

func (b gkeBackend) Delete(ctx context.Context, kind string) error { return b.g.Delete(ctx, kind) }

func (b gkeBackend) FreeBytes() (uint64, error) { return b.g.FreeBytes() }

// gkeMinFree is the free disk below which a cluster or node is refused:
// CLOUDBURROW_GKE_MIN_FREE_GB, or gke.DefaultMinFreeBytes.
func gkeMinFree() uint64 {
	if v := os.Getenv("CLOUDBURROW_GKE_MIN_FREE_GB"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 32); err == nil && n > 0 {
			return n << 30
		}
	}
	return gke.DefaultMinFreeBytes
}

func (s *gkeService) Start(ctx context.Context) error {
	state := ""
	if s.cfg.Mode == config.ModePersistent {
		state = filepath.Join(s.cfg.InstanceDir(), "gke.json")
	}
	api, err := gke.NewServer(gkeBackend{g: &cluster.GKE{Dir: gkeDir(s.cfg)}}, gke.Options{
		Instance:     s.cfg.ClusterName(),
		State:        state,
		Versions:     gkeVersions(s.cfg.Cluster.NodeImage),
		MinFreeBytes: gkeMinFree(),
		Networks:     s.networks,
		MachineType: func(name string) (gke.MachineShape, bool) {
			m, ok := compute.LookupMachineType(name)
			return gke.MachineShape{CPUs: m.CPUs, MemoryMiB: m.MemoryMb, Shared: m.Shared}, ok
		},
	})
	if err != nil {
		return err
	}
	s.api = api
	s.server = grpctransport.New(net.JoinHostPort(s.cfg.BindAddress, strconv.Itoa(s.cfg.Endpoints.GKE)))
	s.server.Observe(s.calls)
	if s.tracing != nil {
		s.server.ServerOptions(s.tracing.ServerOptions()...)
	}
	for _, i := range s.interpose {
		s.server.Interpose(i)
	}
	var jsonAPI http.Handler = gke.NewRESTHandler(api)
	if s.wrapJSON != nil {
		jsonAPI = s.wrapJSON(jsonAPI)
	}
	if s.requests != nil {
		jsonAPI = rest.Observe(jsonAPI, s.requests)
	}
	s.server.ServeHTTP(jsonAPI)
	if err := s.server.Register(func(g *grpc.Server) { api.Register(g) }); err != nil {
		return err
	}
	return s.server.Start(ctx)
}

// Stop stops serving; an ephemeral instance's clusters are deleted, as the
// rest of its state is.
func (s *gkeService) Stop(ctx context.Context) error {
	var errs []error
	if s.server != nil {
		errs = append(errs, s.server.Stop(ctx))
	}
	if s.api != nil {
		s.api.Close()
		if s.cfg.Mode != config.ModePersistent {
			dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
			errs = append(errs, s.api.DeleteAll(dctx, ""))
			cancel()
		}
	}
	return errors.Join(errs...)
}

// gkeResetter deletes the Kubernetes Engine clusters, and their kind
// clusters.
type gkeResetter struct{ svc *gkeService }

func (r *gkeResetter) Name() string { return "gke" }

func (r *gkeResetter) Reset(ctx context.Context) error { return r.ResetProject(ctx, "") }

func (r *gkeResetter) ResetProject(ctx context.Context, project string) error {
	api := r.svc.API()
	if api == nil {
		return errors.New("the Kubernetes Engine API has not started")
	}
	return api.DeleteAll(ctx, project)
}

// deleteGKEClusters removes the kind clusters this instance's Kubernetes
// Engine API made, which `cloudburrow delete` deletes with the instance's
// own: only those with a directory in its gke directory, named as
// gke.KindName names them.
func deleteGKEClusters(ctx context.Context, cfg config.Config, stdout io.Writer) error {
	entries, err := os.ReadDir(gkeDir(cfg))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	own := regexp.MustCompile("^" + regexp.QuoteMeta(cfg.ClusterName()) + "-gke-[0-9a-f]{8}$")
	g := &cluster.GKE{Dir: gkeDir(cfg)}
	var errs []error
	for _, e := range entries {
		if !e.IsDir() || !own.MatchString(e.Name()) {
			continue
		}
		if err := g.Delete(ctx, e.Name()); err != nil {
			errs = append(errs, err)
			continue
		}
		fmt.Fprintf(stdout, "deleted Kubernetes Engine cluster %s\n", e.Name())
	}
	_ = os.Remove(filepath.Join(cfg.InstanceDir(), "gke.json"))
	return errors.Join(errs...)
}
