package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/config"
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
			services, jobs, err := a.Names(ctx)
			if err != nil {
				return nil, err
			}
			for _, n := range services {
				if r, ok := runResource(n, project, false); ok {
					out = append(out, r)
				}
			}
			for _, n := range jobs {
				if r, ok := runResource(n, project, true); ok {
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
			out = append(out, apphub.Resource{URI: "//container.googleapis.com/projects/" + project + "/locations/" + c.GetLocation() + "/clusters/" + c.GetName(),
				Project: project, Location: loc, Zone: zone, Shared: true})
		}
	}
	return out, nil
}

// runResource is the App Hub resource of a Cloud Run service or job name
// (projects/P/locations/L/services/S or .../jobs/J) in project.
func runResource(name, project string, job bool) (apphub.Resource, bool) {
	p := strings.Split(name, "/")
	kind := "services"
	if job {
		kind = "jobs"
	}
	if len(p) != 6 || p[0] != "projects" || p[1] != project || p[2] != "locations" || p[4] != kind {
		return apphub.Resource{}, false
	}
	return apphub.Resource{Workload: job, URI: "//run.googleapis.com/" + name, Project: project, Location: p[3]}, true
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
