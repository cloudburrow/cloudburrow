package apphub

import (
	"context"
	"strings"

	apphubpb "cloud.google.com/go/apphub/apiv1/apphubpb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

func checkID(field, id string) error {
	if !idRE.MatchString(id) {
		return apierror.InvalidArgument("%s %q must be lower-case letters, digits and hyphens, starting with a letter, at most 63 characters", field, id)
	}
	return nil
}

func (s *Server) CreateApplication(_ context.Context, req *apphubpb.CreateApplicationRequest) (*longrunningpb.Operation, error) {
	host, loc, err := checkParent("parent", req.GetParent())
	if err != nil {
		return nil, err
	}
	if err := checkID("application_id", req.GetApplicationId()); err != nil {
		return nil, err
	}
	if err := checkRequestID(req.GetRequestId()); err != nil {
		return nil, err
	}
	if _, _, err := s.project(host); err != nil {
		return nil, err
	}
	in := req.GetApplication()
	// The scope is required and immutable; a GLOBAL application lives in
	// global and a REGIONAL one in a region (global-regional-applications).
	switch in.GetScope().GetType() {
	case apphubpb.Scope_GLOBAL:
		if loc != "global" {
			return nil, apierror.InvalidArgument("an application with scope GLOBAL must be created in the global location, not %q", loc)
		}
	case apphubpb.Scope_REGIONAL:
		if loc == "global" {
			return nil, apierror.InvalidArgument("an application with scope REGIONAL must be created in a region, not global")
		}
	default:
		return nil, apierror.InvalidArgument("application.scope.type is required: GLOBAL or REGIONAL")
	}
	if err := checkText("application", in.GetDisplayName(), in.GetDescription()); err != nil {
		return nil, err
	}
	if err := checkAttributes("application", in.GetAttributes()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.replay(req.GetRequestId()); ok {
		return op, nil
	}
	name := req.GetParent() + "/applications/" + req.GetApplicationId()
	if _, ok := s.apps[name]; ok {
		return nil, apierror.AlreadyExists("Application %s already exists", name)
	}
	now := timestamppb.New(s.now())
	a := &apphubpb.Application{Name: name, DisplayName: in.GetDisplayName(), Description: in.GetDescription(),
		Attributes: in.GetAttributes(), Scope: &apphubpb.Scope{Type: in.GetScope().GetType()},
		CreateTime: now, UpdateTime: now, Uid: newUID(), State: apphubpb.Application_ACTIVE}
	s.apps[name] = a
	return s.done(req.GetParent(), "create", name, req.GetRequestId(), clone(a))
}

// app is the application named; the caller holds mu.
func (s *Server) app(name string) (*apphubpb.Application, error) {
	if !appRE.MatchString(name) {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/applications/{application}", name)
	}
	a, ok := s.apps[name]
	if !ok {
		return nil, apierror.NotFound("Application %s not found", name)
	}
	return a, nil
}

func (s *Server) GetApplication(_ context.Context, req *apphubpb.GetApplicationRequest) (*apphubpb.Application, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.app(req.GetName())
	if err != nil {
		return nil, err
	}
	return clone(a), nil
}

func (s *Server) ListApplications(_ context.Context, req *apphubpb.ListApplicationsRequest) (*apphubpb.ListApplicationsResponse, error) {
	if _, _, err := checkParent("parent", req.GetParent()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	var all []*apphubpb.Application
	for _, k := range sortedKeys(s.apps, req.GetParent()+"/applications/") {
		all = append(all, clone(s.apps[k]))
	}
	s.mu.Unlock()
	out, next, err := listPage("apphub-apps:"+req.GetParent(), req.GetFilter(), req.GetOrderBy(), all, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &apphubpb.ListApplicationsResponse{Applications: out, NextPageToken: next}, nil
}

func (s *Server) UpdateApplication(_ context.Context, req *apphubpb.UpdateApplicationRequest) (*longrunningpb.Operation, error) {
	in := req.GetApplication()
	if err := checkRequestID(req.GetRequestId()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.replay(req.GetRequestId()); ok {
		return op, nil
	}
	cur, err := s.app(in.GetName())
	if err != nil {
		return nil, err
	}
	next := clone(cur)
	err = applyMask("an application", req.GetUpdateMask().GetPaths(), map[string]func(){
		"display_name": func() { next.DisplayName = in.GetDisplayName() },
		"description":  func() { next.Description = in.GetDescription() },
		"attributes":   func() { next.Attributes = in.GetAttributes() },
	}, func(p string) bool { return attributesPath(&next.Attributes, in.GetAttributes(), p) })
	if err != nil {
		return nil, err
	}
	if err := checkText("application", next.GetDisplayName(), next.GetDescription()); err != nil {
		return nil, err
	}
	if err := checkAttributes("application", next.GetAttributes()); err != nil {
		return nil, err
	}
	next.UpdateTime = timestamppb.New(s.now())
	s.apps[next.GetName()] = next
	return s.done(parentOf(next.GetName(), "/applications/"), "update", next.GetName(), req.GetRequestId(), clone(next))
}

func parentOf(name, sep string) string {
	i := strings.Index(name, sep)
	if i < 0 {
		return name
	}
	return name[:i]
}

// DeleteApplication deletes an application with no registered services
// or workloads (UNVERIFIED: the docs do not say whether Google refuses one
// that has them, or unregisters them).
func (s *Server) DeleteApplication(_ context.Context, req *apphubpb.DeleteApplicationRequest) (*longrunningpb.Operation, error) {
	if err := checkRequestID(req.GetRequestId()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.replay(req.GetRequestId()); ok {
		return op, nil
	}
	if _, err := s.app(req.GetName()); err != nil {
		return nil, err
	}
	if len(sortedKeys(s.services, req.GetName()+"/services/")) > 0 || len(sortedKeys(s.workloads, req.GetName()+"/workloads/")) > 0 {
		return nil, apierror.FailedPrecondition("Application %s has registered services or workloads: unregister them first", req.GetName())
	}
	delete(s.apps, req.GetName())
	delete(s.policies, req.GetName())
	return s.done(parentOf(req.GetName(), "/applications/"), "delete", req.GetName(), req.GetRequestId(), &emptypb.Empty{})
}
