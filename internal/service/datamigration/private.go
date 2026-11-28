package datamigration

import (
	"context"
	"net/netip"
	"strings"

	dmspb "cloud.google.com/go/clouddms/apiv1/clouddmspb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Private connections (projects.locations.privateConnections, read
// 2026-11-25): a VPC peering between Database Migration Service and a VPC
// network (vpcPeeringConfig: vpcName, and subnet, "A free subnet for
// peering. (CIDR of /29)"), which a source profile's privateConnectivity
// names. Here every destination's server already reaches the instance's
// network, so a private connection is the record of that peering: its VPC
// network is checked against Compute Engine's when compute is served, and
// its subnet must not overlap another private connection's on the same
// network.

func (s *Server) privateLocked(name string) (*dmspb.PrivateConnection, error) {
	if privateRE.FindStringSubmatch(name) == nil {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/privateConnections/{private_connection}", name)
	}
	p, ok := s.privates[name]
	if !ok {
		return nil, apierror.NotFound("Resource '%s' was not found", name)
	}
	return p, nil
}

func (s *Server) CreatePrivateConnection(_ context.Context, req *dmspb.CreatePrivateConnectionRequest) (*longrunningpb.Operation, error) {
	if _, _, err := checkParent("parent", req.GetParent()); err != nil {
		return nil, err
	}
	if err := checkID("private_connection", req.GetPrivateConnectionId()); err != nil {
		return nil, err
	}
	if err := checkRequestID(req.GetRequestId()); err != nil {
		return nil, err
	}
	pc := clone(req.GetPrivateConnection())
	if pc == nil {
		return nil, apierror.InvalidArgument("private_connection is required")
	}
	cfg := pc.GetVpcPeeringConfig()
	if cfg == nil {
		return nil, apierror.InvalidArgument("private_connection.vpc_peering_config is required")
	}
	vpc := cfg.GetVpcName()
	if !strings.HasPrefix(vpc, "projects/") || !strings.Contains(vpc, "/global/networks/") {
		return nil, apierror.InvalidArgument("private_connection.vpc_peering_config.vpc_name %q must be projects/{project}/global/networks/{network}", vpc)
	}
	prefix, err := netip.ParsePrefix(cfg.GetSubnet())
	if err != nil || !prefix.Addr().Is4() || prefix.Bits() != 29 || prefix.Masked() != prefix {
		return nil, apierror.InvalidArgument("private_connection.vpc_peering_config.subnet %q must be a free /29 IPv4 range", cfg.GetSubnet())
	}
	if s.opts.Network != nil && !req.GetSkipValidation() && !s.opts.Network(vpc) {
		return nil, apierror.NotFound("The VPC network %s was not found", vpc)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.replay(req.GetRequestId()); ok {
		return op, nil
	}
	name := req.GetParent() + "/privateConnections/" + req.GetPrivateConnectionId()
	if _, ok := s.privates[name]; ok {
		return nil, apierror.AlreadyExists("Resource '%s' already exists", name)
	}
	for _, o := range s.privates {
		oc := o.GetVpcPeeringConfig()
		if oc.GetVpcName() != vpc {
			continue
		}
		if op, err := netip.ParsePrefix(oc.GetSubnet()); err == nil && op.Overlaps(prefix) {
			return nil, failedPrecondition("the subnet %s overlaps %s, the subnet of the private connection %s on %s", cfg.GetSubnet(), oc.GetSubnet(), o.GetName(), vpc)
		}
	}
	now := ts(s.now())
	pc.Name, pc.CreateTime, pc.UpdateTime, pc.State, pc.Error = name, now, now, dmspb.PrivateConnection_CREATED, nil
	s.privates[name] = pc
	return s.done(req.GetParent(), "create", name, req.GetRequestId(), clone(pc))
}

func (s *Server) GetPrivateConnection(_ context.Context, req *dmspb.GetPrivateConnectionRequest) (*dmspb.PrivateConnection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.privateLocked(req.GetName())
	if err != nil {
		return nil, err
	}
	return clone(p), nil
}

func (s *Server) ListPrivateConnections(_ context.Context, req *dmspb.ListPrivateConnectionsRequest) (*dmspb.ListPrivateConnectionsResponse, error) {
	if _, _, err := checkParent("parent", req.GetParent()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	var all []*dmspb.PrivateConnection
	for _, k := range sortedKeys(s.privates, req.GetParent()+"/privateConnections/") {
		all = append(all, clone(s.privates[k]))
	}
	s.mu.Unlock()
	out, next, err := listPage("dms-private:"+req.GetParent(), req.GetFilter(), req.GetOrderBy(), all, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &dmspb.ListPrivateConnectionsResponse{PrivateConnections: out, NextPageToken: next}, nil
}

func (s *Server) DeletePrivateConnection(_ context.Context, req *dmspb.DeletePrivateConnectionRequest) (*longrunningpb.Operation, error) {
	if err := checkRequestID(req.GetRequestId()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.replay(req.GetRequestId()); ok {
		return op, nil
	}
	p, err := s.privateLocked(req.GetName())
	if err != nil {
		return nil, err
	}
	for _, k := range sortedKeys(s.profiles, "") {
		pr := s.profiles[k]
		if pr.GetMysql() != nil && nestedStr(pr.GetMysql(), "private_connectivity", "private_connection") == p.GetName() ||
			pr.GetPostgresql() != nil && nestedStr(pr.GetPostgresql(), "private_connectivity", "private_connection") == p.GetName() {
			return nil, failedPrecondition("private connection %s is used by the connection profile %s", p.GetName(), pr.GetName())
		}
	}
	delete(s.privates, p.GetName())
	delete(s.policies, p.GetName())
	return s.done(parentOf(p.GetName()), "delete", p.GetName(), req.GetRequestId(), &emptyResp)
}
