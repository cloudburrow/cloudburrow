package apphub

import (
	"context"
	"sort"

	apphubpb "cloud.google.com/go/apphub/apiv1/apphubpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// discovered are the resources in host's boundary, by URI. It calls
// Discover without holding mu.
func (s *Server) discovered(ctx context.Context, host string) (map[string]Resource, error) {
	s.mu.Lock()
	projects := s.boundaryProjects(host)
	s.mu.Unlock()
	out := map[string]Resource{}
	if s.opts.Discover == nil {
		return out, nil
	}
	for _, p := range projects {
		rs, err := s.opts.Discover(ctx, p)
		if err != nil {
			return nil, apierror.Wrap(err)
		}
		for _, r := range rs {
			out[r.URI] = r
		}
	}
	return out, nil
}

func discoveredService(host string, r Resource) *apphubpb.DiscoveredService {
	return &apphubpb.DiscoveredService{
		Name:              "projects/" + host + "/locations/" + r.Location + "/discoveredServices/" + discoveredID(r.URI),
		ServiceReference:  &apphubpb.ServiceReference{Uri: r.URI},
		ServiceProperties: serviceProperties(r),
	}
}

func serviceProperties(r Resource) *apphubpb.ServiceProperties {
	p := &apphubpb.ServiceProperties{GcpProject: "projects/" + r.Project, Location: r.Location, Zone: r.Zone,
		RegistrationType: &apphubpb.RegistrationType{Type: apphubpb.RegistrationType_EXCLUSIVE}}
	if r.Shared {
		p.RegistrationType.Type = apphubpb.RegistrationType_SHARED
	}
	if r.Principal != "" {
		p.Identity = &apphubpb.Identity{Principal: r.Principal}
	}
	return p
}

func discoveredWorkload(host string, r Resource) *apphubpb.DiscoveredWorkload {
	return &apphubpb.DiscoveredWorkload{
		Name:               "projects/" + host + "/locations/" + r.Location + "/discoveredWorkloads/" + discoveredID(r.URI),
		WorkloadReference:  &apphubpb.WorkloadReference{Uri: r.URI},
		WorkloadProperties: workloadProperties(r),
	}
}

func workloadProperties(r Resource) *apphubpb.WorkloadProperties {
	p := &apphubpb.WorkloadProperties{GcpProject: "projects/" + r.Project, Location: r.Location, Zone: r.Zone}
	if r.Principal != "" {
		p.Identity = &apphubpb.Identity{Principal: r.Principal}
	}
	return p
}

// inLocation are host's discovered resources in loc, services or
// workloads, ordered by URI. A discovered resource lives in its own
// location (UNVERIFIED: whether a global listing also holds the regional
// ones; Terraform's google_apphub_discovered_service example looks a
// regional forwarding rule up in its region).
func (s *Server) inLocation(ctx context.Context, host, loc string, workloads bool) ([]Resource, error) {
	all, err := s.discovered(ctx, host)
	if err != nil {
		return nil, err
	}
	var out []Resource
	for _, r := range all {
		if r.Workload == workloads && r.Location == loc {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].URI < out[j].URI })
	return out, nil
}

func (s *Server) ListDiscoveredServices(ctx context.Context, req *apphubpb.ListDiscoveredServicesRequest) (*apphubpb.ListDiscoveredServicesResponse, error) {
	host, loc, err := checkParent("parent", req.GetParent())
	if err != nil {
		return nil, err
	}
	rs, err := s.inLocation(ctx, host, loc, false)
	if err != nil {
		return nil, err
	}
	all := make([]*apphubpb.DiscoveredService, 0, len(rs))
	for _, r := range rs {
		all = append(all, discoveredService(host, r))
	}
	out, next, err := listPage("apphub-ds:"+req.GetParent(), req.GetFilter(), req.GetOrderBy(), all, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &apphubpb.ListDiscoveredServicesResponse{DiscoveredServices: out, NextPageToken: next}, nil
}

// findDiscovered is host's discovered resource named name, or NOT_FOUND.
func (s *Server) findDiscovered(ctx context.Context, name string, workload bool) (string, Resource, error) {
	re, what := discSvcRE, "DiscoveredService"
	if workload {
		re, what = discWlRE, "DiscoveredWorkload"
	}
	m := re.FindStringSubmatch(name)
	if m == nil {
		return "", Resource{}, apierror.InvalidArgument("name %q is not a %s name", name, what)
	}
	if !validLocation(m[2]) {
		return "", Resource{}, apierror.InvalidArgument("location %q is not an App Hub location", m[2])
	}
	rs, err := s.inLocation(ctx, m[1], m[2], workload)
	if err != nil {
		return "", Resource{}, err
	}
	for _, r := range rs {
		if discoveredID(r.URI) == m[3] {
			return m[1], r, nil
		}
	}
	return "", Resource{}, apierror.NotFound("%s %s not found", what, name)
}

func (s *Server) GetDiscoveredService(ctx context.Context, req *apphubpb.GetDiscoveredServiceRequest) (*apphubpb.DiscoveredService, error) {
	host, r, err := s.findDiscovered(ctx, req.GetName(), false)
	if err != nil {
		return nil, err
	}
	return discoveredService(host, r), nil
}

// LookupDiscoveredService finds a discovered service by its resource URI:
// the response is empty when there is none (UNVERIFIED: as
// lookupServiceProjectAttachment documents it).
func (s *Server) LookupDiscoveredService(ctx context.Context, req *apphubpb.LookupDiscoveredServiceRequest) (*apphubpb.LookupDiscoveredServiceResponse, error) {
	host, loc, err := checkParent("parent", req.GetParent())
	if err != nil {
		return nil, err
	}
	if req.GetUri() == "" {
		return nil, apierror.InvalidArgument("uri is required")
	}
	rs, err := s.inLocation(ctx, host, loc, false)
	if err != nil {
		return nil, err
	}
	out := &apphubpb.LookupDiscoveredServiceResponse{}
	for _, r := range rs {
		if r.URI == req.GetUri() {
			out.DiscoveredService = discoveredService(host, r)
		}
	}
	return out, nil
}

func (s *Server) ListDiscoveredWorkloads(ctx context.Context, req *apphubpb.ListDiscoveredWorkloadsRequest) (*apphubpb.ListDiscoveredWorkloadsResponse, error) {
	host, loc, err := checkParent("parent", req.GetParent())
	if err != nil {
		return nil, err
	}
	rs, err := s.inLocation(ctx, host, loc, true)
	if err != nil {
		return nil, err
	}
	all := make([]*apphubpb.DiscoveredWorkload, 0, len(rs))
	for _, r := range rs {
		all = append(all, discoveredWorkload(host, r))
	}
	out, next, err := listPage("apphub-dw:"+req.GetParent(), req.GetFilter(), req.GetOrderBy(), all, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &apphubpb.ListDiscoveredWorkloadsResponse{DiscoveredWorkloads: out, NextPageToken: next}, nil
}

func (s *Server) GetDiscoveredWorkload(ctx context.Context, req *apphubpb.GetDiscoveredWorkloadRequest) (*apphubpb.DiscoveredWorkload, error) {
	host, r, err := s.findDiscovered(ctx, req.GetName(), true)
	if err != nil {
		return nil, err
	}
	return discoveredWorkload(host, r), nil
}

func (s *Server) LookupDiscoveredWorkload(ctx context.Context, req *apphubpb.LookupDiscoveredWorkloadRequest) (*apphubpb.LookupDiscoveredWorkloadResponse, error) {
	host, loc, err := checkParent("parent", req.GetParent())
	if err != nil {
		return nil, err
	}
	if req.GetUri() == "" {
		return nil, apierror.InvalidArgument("uri is required")
	}
	rs, err := s.inLocation(ctx, host, loc, true)
	if err != nil {
		return nil, err
	}
	out := &apphubpb.LookupDiscoveredWorkloadResponse{}
	for _, r := range rs {
		if r.URI == req.GetUri() {
			out.DiscoveredWorkload = discoveredWorkload(host, r)
		}
	}
	return out, nil
}
