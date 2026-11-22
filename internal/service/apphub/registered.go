package apphub

import (
	"context"

	apphubpb "cloud.google.com/go/apphub/apiv1/apphubpb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// The services and workloads registered to an application. Each copies its
// discovered resource's reference and properties; one whose resource is no
// longer in the host's boundary answers DETACHED (key-concepts).

// checkRegistration checks a discovered resource in disLoc may be
// registered to app (global-regional-applications): a REGIONAL
// application registers its own region's resources only; a MULTI_REGIONAL
// one its multi-region's and its constituent regions', never a global
// one's; a GLOBAL one any.
func checkRegistration(app *apphubpb.Application, disLoc string) error {
	m := appRE.FindStringSubmatch(app.GetName())
	switch app.GetScope().GetType() {
	case apphubpb.Scope_REGIONAL:
		if disLoc != m[2] {
			return apierror.InvalidArgument("regional application %s can only register resources in %s, not %s", app.GetName(), m[2], disLoc)
		}
	case ScopeMultiRegional:
		if !InMultiRegion(m[2], disLoc) {
			return apierror.InvalidArgument("multi-regional application %s can only register resources in the %s multi-region or its regions, not %s", app.GetName(), m[2], disLoc)
		}
	}
	return nil
}

func (s *Server) CreateService(ctx context.Context, req *apphubpb.CreateServiceRequest) (*longrunningpb.Operation, error) {
	if err := checkID("service_id", req.GetServiceId()); err != nil {
		return nil, err
	}
	if err := checkRequestID(req.GetRequestId()); err != nil {
		return nil, err
	}
	in := req.GetService()
	if in.GetDiscoveredService() == "" {
		return nil, apierror.InvalidArgument("service.discovered_service is required")
	}
	if err := checkText("service", in.GetDisplayName(), in.GetDescription()); err != nil {
		return nil, err
	}
	if err := checkAttributes("service", in.GetAttributes()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	app, err := s.app(req.GetParent())
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	host, r, err := s.findDiscovered(ctx, in.GetDiscoveredService(), false)
	if err != nil {
		return nil, err
	}
	am := appRE.FindStringSubmatch(app.GetName())
	if host != am[1] {
		return nil, apierror.InvalidArgument("service.discovered_service %q is not a discovered service of host project %s", in.GetDiscoveredService(), am[1])
	}
	if err := checkRegistration(app, r.Location); err != nil {
		return nil, err
	}
	// "Attributes cannot be set on shared services" (key-concepts).
	if r.Shared && in.GetAttributes() != nil {
		return nil, apierror.InvalidArgument("attributes cannot be set on a shared service")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.replay(req.GetRequestId()); ok {
		return op, nil
	}
	name := req.GetParent() + "/services/" + req.GetServiceId()
	if _, ok := s.services[name]; ok {
		return nil, apierror.AlreadyExists("Service %s already exists", name)
	}
	disName := discoveredService(host, r).GetName()
	for k, other := range s.services {
		if other.GetDiscoveredService() != disName {
			continue
		}
		if parentOf(k, "/services/") == req.GetParent() {
			return nil, apierror.AlreadyExists("discovered service %s is already registered to %s as %s", disName, req.GetParent(), k)
		}
		// An EXCLUSIVE service is registered to one application at a time
		// (key-concepts). The code is UNVERIFIED.
		// Docs re-read 2026-11-21: https://docs.cloud.google.com/app-hub/docs/key-concepts.
		if !r.Shared {
			return nil, apierror.AlreadyExists("discovered service %s is already registered as %s", disName, k)
		}
	}
	now := timestamppb.New(s.now())
	svc := &apphubpb.Service{Name: name, DisplayName: in.GetDisplayName(), Description: in.GetDescription(),
		ServiceReference: &apphubpb.ServiceReference{Uri: r.URI}, ServiceProperties: serviceProperties(r),
		Attributes: in.GetAttributes(), DiscoveredService: disName, CreateTime: now, UpdateTime: now,
		Uid: newUID(), State: apphubpb.Service_ACTIVE}
	s.services[name] = svc
	return s.done(parentOf(name, "/applications/"), "create", name, req.GetRequestId(), clone(svc))
}

// present are the URIs in host's boundary now.
func (s *Server) present(ctx context.Context, host string) (map[string]bool, error) {
	all, err := s.discovered(ctx, host)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for u := range all {
		out[u] = true
	}
	return out, nil
}

func serviceState(svc *apphubpb.Service, present map[string]bool) *apphubpb.Service {
	out := clone(svc)
	if !present[svc.GetServiceReference().GetUri()] {
		out.State = apphubpb.Service_DETACHED
	}
	return out
}

func workloadState(w *apphubpb.Workload, present map[string]bool) *apphubpb.Workload {
	out := clone(w)
	if !present[w.GetWorkloadReference().GetUri()] {
		out.State = apphubpb.Workload_DETACHED
	}
	return out
}

func (s *Server) GetService(ctx context.Context, req *apphubpb.GetServiceRequest) (*apphubpb.Service, error) {
	m := serviceRE.FindStringSubmatch(req.GetName())
	if m == nil {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/applications/{application}/services/{service}", req.GetName())
	}
	s.mu.Lock()
	svc, ok := s.services[req.GetName()]
	s.mu.Unlock()
	if !ok {
		return nil, apierror.NotFound("Service %s not found", req.GetName())
	}
	present, err := s.present(ctx, m[2])
	if err != nil {
		return nil, err
	}
	return serviceState(svc, present), nil
}

func (s *Server) ListServices(ctx context.Context, req *apphubpb.ListServicesRequest) (*apphubpb.ListServicesResponse, error) {
	s.mu.Lock()
	_, err := s.app(req.GetParent())
	var keep []*apphubpb.Service
	if err == nil {
		for _, k := range sortedKeys(s.services, req.GetParent()+"/services/") {
			keep = append(keep, s.services[k])
		}
	}
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	present, err := s.present(ctx, appRE.FindStringSubmatch(req.GetParent())[1])
	if err != nil {
		return nil, err
	}
	all := make([]*apphubpb.Service, 0, len(keep))
	for _, svc := range keep {
		all = append(all, serviceState(svc, present))
	}
	out, next, err := listPage("apphub-svc:"+req.GetParent(), req.GetFilter(), req.GetOrderBy(), all, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &apphubpb.ListServicesResponse{Services: out, NextPageToken: next}, nil
}

func (s *Server) UpdateService(_ context.Context, req *apphubpb.UpdateServiceRequest) (*longrunningpb.Operation, error) {
	in := req.GetService()
	if !serviceRE.MatchString(in.GetName()) {
		return nil, apierror.InvalidArgument("service.name %q is not a service name", in.GetName())
	}
	if err := checkRequestID(req.GetRequestId()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.replay(req.GetRequestId()); ok {
		return op, nil
	}
	cur, ok := s.services[in.GetName()]
	if !ok {
		return nil, apierror.NotFound("Service %s not found", in.GetName())
	}
	next := clone(cur)
	err := applyMask("a service", req.GetUpdateMask().GetPaths(), map[string]func(){
		"display_name": func() { next.DisplayName = in.GetDisplayName() },
		"description":  func() { next.Description = in.GetDescription() },
		"attributes":   func() { next.Attributes = in.GetAttributes() },
	}, func(p string) bool { return attributesPath(&next.Attributes, in.GetAttributes(), p) })
	if err != nil {
		return nil, err
	}
	if err := checkText("service", next.GetDisplayName(), next.GetDescription()); err != nil {
		return nil, err
	}
	if err := checkAttributes("service", next.GetAttributes()); err != nil {
		return nil, err
	}
	if next.GetAttributes() != nil && next.GetServiceProperties().GetRegistrationType().GetType() == apphubpb.RegistrationType_SHARED {
		return nil, apierror.InvalidArgument("attributes cannot be set on a shared service")
	}
	next.UpdateTime = timestamppb.New(s.now())
	s.services[next.GetName()] = next
	return s.done(parentOf(next.GetName(), "/applications/"), "update", next.GetName(), req.GetRequestId(), clone(next))
}

func (s *Server) DeleteService(_ context.Context, req *apphubpb.DeleteServiceRequest) (*longrunningpb.Operation, error) {
	if !serviceRE.MatchString(req.GetName()) {
		return nil, apierror.InvalidArgument("name %q is not a service name", req.GetName())
	}
	if err := checkRequestID(req.GetRequestId()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.replay(req.GetRequestId()); ok {
		return op, nil
	}
	if _, ok := s.services[req.GetName()]; !ok {
		return nil, apierror.NotFound("Service %s not found", req.GetName())
	}
	delete(s.services, req.GetName())
	return s.done(parentOf(req.GetName(), "/applications/"), "delete", req.GetName(), req.GetRequestId(), &emptypb.Empty{})
}

func (s *Server) CreateWorkload(ctx context.Context, req *apphubpb.CreateWorkloadRequest) (*longrunningpb.Operation, error) {
	if err := checkID("workload_id", req.GetWorkloadId()); err != nil {
		return nil, err
	}
	if err := checkRequestID(req.GetRequestId()); err != nil {
		return nil, err
	}
	in := req.GetWorkload()
	if in.GetDiscoveredWorkload() == "" {
		return nil, apierror.InvalidArgument("workload.discovered_workload is required")
	}
	if err := checkText("workload", in.GetDisplayName(), in.GetDescription()); err != nil {
		return nil, err
	}
	if err := checkAttributes("workload", in.GetAttributes()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	app, err := s.app(req.GetParent())
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	host, r, err := s.findDiscovered(ctx, in.GetDiscoveredWorkload(), true)
	if err != nil {
		return nil, err
	}
	am := appRE.FindStringSubmatch(app.GetName())
	if host != am[1] {
		return nil, apierror.InvalidArgument("workload.discovered_workload %q is not a discovered workload of host project %s", in.GetDiscoveredWorkload(), am[1])
	}
	if err := checkRegistration(app, r.Location); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.replay(req.GetRequestId()); ok {
		return op, nil
	}
	name := req.GetParent() + "/workloads/" + req.GetWorkloadId()
	if _, ok := s.workloads[name]; ok {
		return nil, apierror.AlreadyExists("Workload %s already exists", name)
	}
	disName := discoveredWorkload(host, r).GetName()
	for k, other := range s.workloads {
		if other.GetDiscoveredWorkload() == disName {
			// A workload is registered to one application at a time
			// (UNVERIFIED: supported-resources gives workloads no
			// registration type).
			// Docs re-read 2026-11-21: https://docs.cloud.google.com/app-hub/docs/supported-resources.
			return nil, apierror.AlreadyExists("discovered workload %s is already registered as %s", disName, k)
		}
	}
	now := timestamppb.New(s.now())
	w := &apphubpb.Workload{Name: name, DisplayName: in.GetDisplayName(), Description: in.GetDescription(),
		WorkloadReference: &apphubpb.WorkloadReference{Uri: r.URI}, WorkloadProperties: workloadProperties(r),
		Attributes: in.GetAttributes(), DiscoveredWorkload: disName, CreateTime: now, UpdateTime: now,
		Uid: newUID(), State: apphubpb.Workload_ACTIVE}
	s.workloads[name] = w
	return s.done(parentOf(name, "/applications/"), "create", name, req.GetRequestId(), clone(w))
}

func (s *Server) GetWorkload(ctx context.Context, req *apphubpb.GetWorkloadRequest) (*apphubpb.Workload, error) {
	m := workloadRE.FindStringSubmatch(req.GetName())
	if m == nil {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/applications/{application}/workloads/{workload}", req.GetName())
	}
	s.mu.Lock()
	w, ok := s.workloads[req.GetName()]
	s.mu.Unlock()
	if !ok {
		return nil, apierror.NotFound("Workload %s not found", req.GetName())
	}
	present, err := s.present(ctx, m[2])
	if err != nil {
		return nil, err
	}
	return workloadState(w, present), nil
}

func (s *Server) ListWorkloads(ctx context.Context, req *apphubpb.ListWorkloadsRequest) (*apphubpb.ListWorkloadsResponse, error) {
	s.mu.Lock()
	_, err := s.app(req.GetParent())
	var keep []*apphubpb.Workload
	if err == nil {
		for _, k := range sortedKeys(s.workloads, req.GetParent()+"/workloads/") {
			keep = append(keep, s.workloads[k])
		}
	}
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	present, err := s.present(ctx, appRE.FindStringSubmatch(req.GetParent())[1])
	if err != nil {
		return nil, err
	}
	all := make([]*apphubpb.Workload, 0, len(keep))
	for _, w := range keep {
		all = append(all, workloadState(w, present))
	}
	out, next, err := listPage("apphub-wl:"+req.GetParent(), req.GetFilter(), req.GetOrderBy(), all, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &apphubpb.ListWorkloadsResponse{Workloads: out, NextPageToken: next}, nil
}

func (s *Server) UpdateWorkload(_ context.Context, req *apphubpb.UpdateWorkloadRequest) (*longrunningpb.Operation, error) {
	in := req.GetWorkload()
	if !workloadRE.MatchString(in.GetName()) {
		return nil, apierror.InvalidArgument("workload.name %q is not a workload name", in.GetName())
	}
	if err := checkRequestID(req.GetRequestId()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.replay(req.GetRequestId()); ok {
		return op, nil
	}
	cur, ok := s.workloads[in.GetName()]
	if !ok {
		return nil, apierror.NotFound("Workload %s not found", in.GetName())
	}
	next := clone(cur)
	err := applyMask("a workload", req.GetUpdateMask().GetPaths(), map[string]func(){
		"display_name": func() { next.DisplayName = in.GetDisplayName() },
		"description":  func() { next.Description = in.GetDescription() },
		"attributes":   func() { next.Attributes = in.GetAttributes() },
	}, func(p string) bool { return attributesPath(&next.Attributes, in.GetAttributes(), p) })
	if err != nil {
		return nil, err
	}
	if err := checkText("workload", next.GetDisplayName(), next.GetDescription()); err != nil {
		return nil, err
	}
	if err := checkAttributes("workload", next.GetAttributes()); err != nil {
		return nil, err
	}
	next.UpdateTime = timestamppb.New(s.now())
	s.workloads[next.GetName()] = next
	return s.done(parentOf(next.GetName(), "/applications/"), "update", next.GetName(), req.GetRequestId(), clone(next))
}

func (s *Server) DeleteWorkload(_ context.Context, req *apphubpb.DeleteWorkloadRequest) (*longrunningpb.Operation, error) {
	if !workloadRE.MatchString(req.GetName()) {
		return nil, apierror.InvalidArgument("name %q is not a workload name", req.GetName())
	}
	if err := checkRequestID(req.GetRequestId()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.replay(req.GetRequestId()); ok {
		return op, nil
	}
	if _, ok := s.workloads[req.GetName()]; !ok {
		return nil, apierror.NotFound("Workload %s not found", req.GetName())
	}
	delete(s.workloads, req.GetName())
	return s.done(parentOf(req.GetName(), "/applications/"), "delete", req.GetName(), req.GetRequestId(), &emptypb.Empty{})
}
