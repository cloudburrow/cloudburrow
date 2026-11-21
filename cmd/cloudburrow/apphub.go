package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/grpc"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/iameval"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/service/apphub"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/telemetry"
	grpctransport "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// appHubService serves App Hub (#2110) in the CLI process, gRPC and JSON
// on one port. It discovers the resources this instance serves of the
// types Google's supported-resources lists: Cloud Run services and jobs,
// and Kubernetes Engine clusters.
type appHubService struct {
	cfg       config.Config
	tracing   *telemetry.Tracing
	calls     grpctransport.Observer
	interpose []grpc.UnaryServerInterceptor
	requests  func(rest.Request)
	// wrapJSON wraps the JSON API (IAM enforcement); nil is none.
	wrapJSON func(http.Handler) http.Handler
	// run is Cloud Run, whose services and jobs are discovered; nil
	// without it.
	run *runService
	// gke is Kubernetes Engine, whose clusters are discovered; nil without
	// it.
	gke *gkeService
	// registry is Resource Manager's: projects, numbers and app-enabled
	// folders.
	registry func() *resourcemanager.Registry
	server   *grpctransport.Server
	api      *apphub.Server
}

func newAppHubService(cfg config.Config) *appHubService {
	if !appHubEnabled(cfg) {
		return nil
	}
	return &appHubService{cfg: cfg}
}

// appHubEnabled is whether `up` binds the App Hub port.
func appHubEnabled(cfg config.Config) bool {
	return serviceEnabled(cfg, config.ServiceAppHub) && cfg.Endpoints.AppHub >= 0
}

func (s *appHubService) register(coord *lifecycle.Coordinator) {
	if s != nil {
		coord.Register(s)
	}
}

func (s *appHubService) Name() string { return "apphub" }

// API is the server, available after Start.
func (s *appHubService) API() *apphub.Server {
	if s == nil {
		return nil
	}
	return s.api
}

// Addr is the bound address.
func (s *appHubService) Addr() string {
	if s == nil || s.server == nil {
		return ""
	}
	return s.server.Addr()
}

func (s *appHubService) Start(ctx context.Context) error {
	state := ""
	if s.cfg.Mode == config.ModePersistent {
		state = filepath.Join(s.cfg.InstanceDir(), "apphub.json")
	}
	opts := apphub.Options{Discover: s.discover}
	if s.registry != nil {
		opts.Projects = appHubProjects{registry: s.registry}
	}
	api, err := apphub.NewServer(state, opts)
	if err != nil {
		return err
	}
	s.api = api
	s.server = grpctransport.New(net.JoinHostPort(s.cfg.BindAddress, strconv.Itoa(s.cfg.Endpoints.AppHub)))
	s.server.Observe(s.calls)
	if s.tracing != nil {
		s.server.ServerOptions(s.tracing.ServerOptions()...)
	}
	for _, i := range s.interpose {
		s.server.Interpose(i)
	}
	var jsonAPI http.Handler = apphub.NewRESTHandler(api)
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

func (s *appHubService) Stop(ctx context.Context) error {
	if s.server == nil {
		return nil
	}
	return s.server.Stop(ctx)
}

// discover lists project's resources of the supported types: Cloud Run
// services (EXCLUSIVE services) and jobs (workloads), and Kubernetes Engine
// clusters (SHARED services), with supported-resources' URIs.
func (s *appHubService) discover(ctx context.Context, project string) ([]apphub.Resource, error) {
	var out []apphub.Resource
	if s.run != nil {
		if a := s.run.adapter.Load(); a != nil {
			services, jobs, pools, err := a.Names(ctx)
			if err != nil {
				return nil, err
			}
			for _, n := range services {
				if r, ok := runResource(n, project, "services"); ok {
					out = append(out, r)
				}
			}
			for _, n := range jobs {
				if r, ok := runResource(n, project, "jobs"); ok {
					out = append(out, r)
				}
			}
			for _, n := range pools {
				if r, ok := runResource(n, project, "workerPools"); ok {
					out = append(out, r)
				}
			}
		}
	}
	if api := s.gke.API(); api != nil {
		l, err := api.ListClusters(ctx, &containerpb.ListClustersRequest{Parent: "projects/" + project + "/locations/-"})
		if err != nil {
			return nil, err
		}
		for _, c := range l.GetClusters() {
			loc, zone := c.GetLocation(), ""
			if strings.Count(loc, "-") == 2 {
				// A zonal cluster: its region, and its zone.
				zone, loc = loc, loc[:strings.LastIndex(loc, "-")]
			}
			cluster := "//container.googleapis.com/projects/" + project + "/locations/" + c.GetLocation() + "/clusters/" + c.GetName()
			out = append(out, apphub.Resource{URI: cluster, Project: project, Location: loc, Zone: zone, Shared: true})
			if c.GetStatus() == containerpb.Cluster_RUNNING {
				out = append(out, s.inCluster(ctx, "projects/"+project+"/locations/"+c.GetLocation()+"/clusters/"+c.GetName(), cluster, project, loc, zone)...)
			}
		}
	}
	return out, nil
}

// runResource is the App Hub resource of a Cloud Run service, job or
// worker pool name (projects/P/locations/L/{kind}/N) in project: a service
// is a service, a job and a worker pool workloads (supported-resources).
func runResource(name, project, kind string) (apphub.Resource, bool) {
	p := strings.Split(name, "/")
	if len(p) != 6 || p[0] != "projects" || p[1] != project || p[2] != "locations" || p[4] != kind {
		return apphub.Resource{}, false
	}
	return apphub.Resource{Workload: kind != "services", URI: "//run.googleapis.com/" + name, Project: project, Location: p[3]}, true
}

// appHubK8sKinds are the Kubernetes objects of a GKE cluster that
// supported-resources lists, with the collection path of each one's URI
// under //container.googleapis.com/.../clusters/C/k8s/namespaces/N/:
// Services, Ingresses and Gateways as services, Deployments,
// StatefulSets, DaemonSets and CronJobs as workloads.
var appHubK8sKinds = []struct {
	list, uri string
	workload  bool
}{
	{"/api/v1/services", "services", false},
	{"/apis/networking.k8s.io/v1/ingresses", "networking.k8s.io/ingresses", false},
	{"/apis/gateway.networking.k8s.io/v1/gateways", "apis/gateway.networking.k8s.io/gateways", false},
	{"/apis/apps/v1/deployments", "apps/deployments", true},
	{"/apis/apps/v1/statefulsets", "apps/statefulsets", true},
	{"/apis/apps/v1/daemonsets", "apps/daemonsets", true},
	{"/apis/batch/v1/cronjobs", "batch/cronjobs", true},
}

// appHubSystemNamespace is a namespace whose objects are the cluster's own,
// not the user's: Kubernetes' kube-*, and GKE's gke-* and gmp-*, and
// kind's local-path-storage (UNVERIFIED: supported-resources does not say
// which namespaces App Hub skips).
// Docs re-read 2026-11-21: https://docs.cloud.google.com/app-hub/docs/supported-resources.
func appHubSystemNamespace(ns string) bool {
	return strings.HasPrefix(ns, "kube-") || strings.HasPrefix(ns, "gke-") || strings.HasPrefix(ns, "gmp-") || ns == "local-path-storage"
}

// inCluster lists a running cluster's Kubernetes objects of the supported
// kinds, through its API server as its client certificate. The default
// namespace's kubernetes Service, the API server's own, is not the
// user's and is left out (UNVERIFIED). A cluster whose API does not
// answer contributes none, and a kind it does not serve (no Gateway API)
// is skipped.
// Docs re-read 2026-11-21: https://docs.cloud.google.com/app-hub/docs/supported-resources.
func (s *appHubService) inCluster(ctx context.Context, name, cluster, project, loc, zone string) []apphub.Resource {
	a, ok := s.gke.clusterAccess(name)
	if !ok {
		return nil
	}
	hc, err := runadapter.GKEClient(a)
	if err != nil {
		return nil
	}
	hc.Timeout = 10 * time.Second
	var out []apphub.Resource
	for _, k := range appHubK8sKinds {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+a.Endpoint+k.list, nil)
		if err != nil {
			continue
		}
		resp, err := hc.Do(req)
		if err != nil {
			return out
		}
		var l struct {
			Items []struct {
				Metadata struct{ Name, Namespace string } `json:"metadata"`
			} `json:"items"`
		}
		err = json.NewDecoder(resp.Body).Decode(&l)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || err != nil {
			continue
		}
		for _, it := range l.Items {
			ns, n := it.Metadata.Namespace, it.Metadata.Name
			if appHubSystemNamespace(ns) || ns == "default" && n == "kubernetes" && k.uri == "services" {
				continue
			}
			out = append(out, apphub.Resource{Workload: k.workload, URI: cluster + "/k8s/namespaces/" + ns + "/" + k.uri + "/" + n,
				Project: project, Location: loc, Zone: zone})
		}
	}
	return out
}

// appHubProjects resolves App Hub's projects and app-enabled folders
// through Resource Manager.
type appHubProjects struct {
	registry func() *resourcemanager.Registry
}

func (p appHubProjects) Project(ref string) (string, int64, bool) {
	reg := p.registry()
	if reg == nil {
		return "", 0, false
	}
	if _, err := strconv.ParseInt(ref, 10, 64); err == nil {
		all, err := reg.List()
		if err != nil {
			return "", 0, false
		}
		for _, pr := range all {
			if n, ok := reg.NumberOf(pr.ProjectID); ok && strconv.FormatInt(n, 10) == ref {
				return pr.ProjectID, n, pr.State == resourcemanager.StateActive
			}
		}
		return "", 0, false
	}
	// Every project this instance answers for is registered when it is
	// first used, as the rest of the instance does.
	pr, err := reg.EnsureExists(ref)
	if err != nil || pr.State != resourcemanager.StateActive {
		return "", 0, false
	}
	n, _ := reg.NumberOf(pr.ProjectID)
	return pr.ProjectID, n, true
}

func (p appHubProjects) ManagedFolder(project string) string {
	reg := p.registry()
	if reg == nil {
		return ""
	}
	pr, err := reg.Get(project)
	if err != nil || !strings.HasPrefix(pr.Parent, "folders/") {
		return ""
	}
	f, err := reg.Folder(pr.Parent)
	if err != nil || f.ManagementProject == "" {
		return ""
	}
	if f.ManagementProject == pr.Name || f.ManagementProject == "projects/"+pr.ProjectID {
		return f.Name
	}
	return ""
}

func (p appHubProjects) FolderProjects(folder string) []string {
	reg := p.registry()
	if reg == nil {
		return nil
	}
	all, err := reg.Active()
	if err != nil {
		return nil
	}
	id := strings.TrimPrefix(folder, "folders/")
	var out []string
	for _, pr := range all {
		anc, err := reg.Ancestry(pr.ProjectID)
		if err != nil {
			continue
		}
		for _, a := range anc {
			if a.Type == "folder" && a.ID == id {
				out = append(out, pr.ProjectID)
				break
			}
		}
	}
	return out
}

// appHubResetter deletes the attachments, applications and registrations.
type appHubResetter struct{ svc *appHubService }

func (r *appHubResetter) Name() string { return "apphub" }

func (r *appHubResetter) Reset(ctx context.Context) error { return r.ResetProject(ctx, "") }

func (r *appHubResetter) ResetProject(_ context.Context, project string) error {
	api := r.svc.API()
	if api == nil {
		return errors.New("App Hub has not started")
	}
	return api.ResetProject(project)
}

// useIAMEnforcementAppHub checks App Hub's callers (#2110): each method's
// documented permissions (iameval.AppHub), over gRPC and JSON, with its
// applications' own policies.
func useIAMEnforcementAppHub(e *iameval.Enforcer, s *appHubService) {
	if s == nil {
		return
	}
	s.interpose = append(s.interpose, e.Interceptor(iameval.AppHub))
	s.wrapJSON = jsonEnforcer(e, iameval.AppHub, apphub.Mixins())
	e.Policies[iameval.AppHub.Name] = iameval.GRPCPolicyGetter(func() func(context.Context, *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
		api := s.API()
		if api == nil {
			return nil
		}
		return api.IAMPolicy().GetIamPolicy
	})
}
