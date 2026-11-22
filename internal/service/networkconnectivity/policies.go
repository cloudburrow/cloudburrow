package networkconnectivity

import (
	"context"
	"strings"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	ncpb "cloud.google.com/go/networkconnectivity/apiv1/networkconnectivitypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Service connection policies. "You can create only one service
// connection policy for each combination of network, region, and service
// class"; "Only subnets, connection limit, and service instance scope can
// be updated"; deletion "is blocked while active Private Service Connect
// connections are associated with the policy"
// (configure-service-connection-policies, re-read 2026-11-22). Policies
// "must be deployed in the host project", so the network is the policy's
// project's. The error wordings are UNVERIFIED.

const policies = "serviceConnectionPolicies"

func (s *Server) CreateServiceConnectionPolicy(_ context.Context, req *ncpb.CreateServiceConnectionPolicyRequest) (*longrunningpb.Operation, error) {
	project, region, err := parent("parent", req.GetParent())
	if err != nil {
		return nil, err
	}
	if err := checkID("service_connection_policy_id", req.GetServiceConnectionPolicyId()); err != nil {
		return nil, err
	}
	in := req.GetServiceConnectionPolicy()
	if in == nil {
		return nil, apierror.InvalidArgument("service_connection_policy is required")
	}
	p := clone(in)
	p.Name = req.GetParent() + "/" + policies + "/" + req.GetServiceConnectionPolicyId()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.policies[p.Name]; ok {
		return nil, apierror.AlreadyExists("Resource '%s' already exists", p.Name)
	}
	if p.GetServiceClass() == "" {
		return nil, apierror.InvalidArgument("service_connection_policy.service_class is required")
	}
	net, ok := networkRel(p.GetNetwork())
	if !ok {
		return nil, apierror.InvalidArgument("service_connection_policy.network %q must be projects/{project}/global/networks/{network}", p.GetNetwork())
	}
	if np := strings.Split(net, "/")[1]; np != project {
		return nil, apierror.InvalidArgument("service_connection_policy.network %s must be a network of the policy's project, %s: a policy is made in the network's host project", net, project)
	}
	if c := s.computeAPI(); c != nil && !c.HasNetwork(net, project) {
		return nil, apierror.InvalidArgument("The network %s was not found", net)
	}
	p.Network = net
	for k, o := range s.policies {
		if strings.HasPrefix(k, req.GetParent()+"/") && o.GetNetwork() == net && o.GetServiceClass() == p.GetServiceClass() {
			return nil, apierror.AlreadyExists("Only one service connection policy can exist for network %s, region %s and service class %s: %s already does",
				net, region, p.GetServiceClass(), k)
		}
	}
	if err := s.checkPscConfigLocked(project, region, net, p.GetPscConfig()); err != nil {
		return nil, err
	}
	if p.PscConfig == nil {
		p.PscConfig = &ncpb.ServiceConnectionPolicy_PscConfig{}
	}
	now := timestamppb.New(s.now())
	p.CreateTime, p.UpdateTime = now, now
	p.Infrastructure = ncpb.Infrastructure_PSC
	p.PscConnections = nil
	p.Etag = newEtag()
	s.policies[p.Name] = p
	// A map waiting for this policy makes its endpoints now.
	s.reconcileLocked()
	return s.opLocked(req.GetParent(), p.Name, "create", s.policies[p.Name])
}

// checkPscConfigLocked checks a policy's subnetworks (each a regular
// subnet of the policy's network, in its region), its limit and its
// service instance scope.
func (s *Server) checkPscConfigLocked(project, region, network string, pc *ncpb.ServiceConnectionPolicy_PscConfig) error {
	if pc == nil {
		return nil
	}
	c := s.computeAPI()
	for i, sub := range pc.GetSubnetworks() {
		rel := computeRel(sub)
		parts := strings.Split(rel, "/")
		if len(parts) != 6 || parts[0] != "projects" || parts[2] != "regions" || parts[4] != "subnetworks" {
			return apierror.InvalidArgument("psc_config.subnetworks[%d] %q must be projects/{project}/regions/{region}/subnetworks/{subnetwork}", i, sub)
		}
		if parts[3] != region {
			return apierror.InvalidArgument("psc_config.subnetworks[%d] %s is not in the policy's region, %s", i, rel, region)
		}
		if c != nil {
			n, ok := c.Subnetwork(rel, project, region)
			if !ok {
				return apierror.InvalidArgument("The subnetwork %s was not found", rel)
			}
			if n != network {
				return apierror.InvalidArgument("psc_config.subnetworks[%d] %s is not a subnetwork of %s", i, rel, network)
			}
		}
		pc.Subnetworks[i] = rel
	}
	if pc.Limit != nil && pc.GetLimit() < 0 {
		return apierror.InvalidArgument("psc_config.limit must not be negative")
	}
	if pc.GetProducerInstanceLocation() == ncpb.ServiceConnectionPolicy_PscConfig_CUSTOM_RESOURCE_HIERARCHY_LEVELS {
		if len(pc.GetAllowedGoogleProducersResourceHierarchyLevel()) == 0 {
			return apierror.InvalidArgument("psc_config.allowed_google_producers_resource_hierarchy_level is required when producer_instance_location is CUSTOM_RESOURCE_HIERARCHY_LEVELS")
		}
	} else if len(pc.GetAllowedGoogleProducersResourceHierarchyLevel()) > 0 {
		return apierror.InvalidArgument("psc_config.allowed_google_producers_resource_hierarchy_level can be set only when producer_instance_location is CUSTOM_RESOURCE_HIERARCHY_LEVELS")
	}
	for i, l := range pc.GetAllowedGoogleProducersResourceHierarchyLevel() {
		if !(strings.HasPrefix(l, "projects/") || strings.HasPrefix(l, "folders/") || strings.HasPrefix(l, "organizations/")) || strings.Count(l, "/") != 1 {
			return apierror.InvalidArgument("psc_config.allowed_google_producers_resource_hierarchy_level[%d] %q must be projects/{id}, folders/{id} or organizations/{id}", i, l)
		}
	}
	return nil
}

func (s *Server) GetServiceConnectionPolicy(_ context.Context, req *ncpb.GetServiceConnectionPolicyRequest) (*ncpb.ServiceConnectionPolicy, error) {
	if _, _, err := child(req.GetName(), policies); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.policies[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("Resource '%s' was not found", req.GetName())
	}
	return clone(p), nil
}

func (s *Server) ListServiceConnectionPolicies(_ context.Context, req *ncpb.ListServiceConnectionPoliciesRequest) (*ncpb.ListServiceConnectionPoliciesResponse, error) {
	if !parentRE.MatchString(req.GetParent()) {
		return nil, apierror.InvalidArgument("parent %q must be projects/{project}/locations/{location}", req.GetParent())
	}
	s.mu.Lock()
	all := []*ncpb.ServiceConnectionPolicy{}
	for _, k := range s.listKeysLocked(s.policies, req.GetParent(), policies) {
		all = append(all, clone(s.policies[k]))
	}
	s.mu.Unlock()
	out, next, err := listPage("policies:"+req.GetParent(), all, req.GetFilter(), req.GetOrderBy(), req.GetPageToken(), req.GetPageSize(),
		func(p *ncpb.ServiceConnectionPolicy) string { return p.GetName() })
	if err != nil {
		return nil, err
	}
	return &ncpb.ListServiceConnectionPoliciesResponse{ServiceConnectionPolicies: out, NextPageToken: next}, nil
}

// listKeysLocked are the keys under parent's collection; a parent of
// location "-" lists every location's.
func (s *Server) listKeysLocked(m any, parentName, collection string) []string {
	var keys []string
	switch v := m.(type) {
	case map[string]*ncpb.ServiceConnectionPolicy:
		keys = sortedKeys(v, "")
	case map[string]*ncpb.ServiceConnectionMap:
		keys = sortedKeys(v, "")
	case map[string]*ncpb.ServiceClass:
		keys = sortedKeys(v, "")
	case map[string]*ncpb.ServiceConnectionToken:
		keys = sortedKeys(v, "")
	}
	project, location, _ := strings.Cut(strings.TrimPrefix(parentName, "projects/"), "/locations/")
	out := []string{}
	for _, k := range keys {
		kp, rest, _ := strings.Cut(strings.TrimPrefix(k, "projects/"), "/locations/")
		kl, tail, _ := strings.Cut(rest, "/")
		if kp == project && (location == "-" || kl == location) && strings.HasPrefix(tail, collection+"/") {
			out = append(out, k)
		}
	}
	return out
}

func (s *Server) UpdateServiceConnectionPolicy(_ context.Context, req *ncpb.UpdateServiceConnectionPolicyRequest) (*longrunningpb.Operation, error) {
	in := req.GetServiceConnectionPolicy()
	if in == nil {
		return nil, apierror.InvalidArgument("service_connection_policy is required")
	}
	parentName, _, err := child(in.GetName(), policies)
	if err != nil {
		return nil, err
	}
	project, region, _ := parent("parent", parentName)
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.policies[in.GetName()]
	if !ok {
		return nil, apierror.NotFound("Resource '%s' was not found", in.GetName())
	}
	if err := checkEtag(in.Etag, cur.Etag); err != nil {
		return nil, err
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		paths = []string{"description", "labels", "psc_config"}
	}
	next := clone(cur)
	if next.PscConfig == nil {
		next.PscConfig = &ncpb.ServiceConnectionPolicy_PscConfig{}
	}
	inPC := in.GetPscConfig()
	for _, path := range paths {
		switch path {
		case "description":
			next.Description = in.GetDescription()
		case "labels":
			next.Labels = in.GetLabels()
		case "psc_config", "pscConfig":
			if inPC == nil {
				inPC = &ncpb.ServiceConnectionPolicy_PscConfig{}
			}
			next.PscConfig = clone(inPC)
		case "psc_config.subnetworks", "pscConfig.subnetworks":
			next.PscConfig.Subnetworks = inPC.GetSubnetworks()
		case "psc_config.limit", "pscConfig.limit":
			next.PscConfig.Limit = inPC.Limit
		case "psc_config.producer_instance_location", "pscConfig.producerInstanceLocation":
			next.PscConfig.ProducerInstanceLocation = inPC.GetProducerInstanceLocation()
		case "psc_config.allowed_google_producers_resource_hierarchy_level", "pscConfig.allowedGoogleProducersResourceHierarchyLevel":
			next.PscConfig.AllowedGoogleProducersResourceHierarchyLevel = inPC.GetAllowedGoogleProducersResourceHierarchyLevel()
		case "network", "service_class", "serviceClass":
			return nil, apierror.InvalidArgument("%s cannot be updated: only the subnetworks, the connection limit and the service instance scope of a policy can be; "+
				"delete the policy and create a new one", path)
		default:
			return nil, apierror.InvalidArgument("update_mask path %q is not a field of ServiceConnectionPolicy that can be updated", path)
		}
	}
	if err := s.checkPscConfigLocked(project, region, next.GetNetwork(), next.PscConfig); err != nil {
		return nil, err
	}
	next.UpdateTime = timestamppb.New(s.now())
	next.Etag = newEtag()
	s.policies[next.Name] = next
	// A raised limit or a new subnet lets a waiting map connect.
	s.reconcileLocked()
	return s.opLocked(parentName, next.Name, "update", s.policies[next.Name])
}

func (s *Server) DeleteServiceConnectionPolicy(_ context.Context, req *ncpb.DeleteServiceConnectionPolicyRequest) (*longrunningpb.Operation, error) {
	parentName, _, err := child(req.GetName(), policies)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.policies[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("Resource '%s' was not found", req.GetName())
	}
	if err := checkEtag(req.Etag, cur.Etag); err != nil {
		return nil, err
	}
	if n := activeConnections(cur); n > 0 {
		return nil, apierror.FailedPrecondition("The service connection policy %s has %d active Private Service Connect connection(s): "+
			"decommission the service instances that use it before deleting it", req.GetName(), n)
	}
	delete(s.policies, req.GetName())
	s.reconcileLocked()
	return s.opLocked(parentName, req.GetName(), "delete", nil)
}
