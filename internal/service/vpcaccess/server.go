// Package vpcaccess serves the Serverless VPC Access API v1
// (vpcaccess.googleapis.com, #2140) in the CLI process, on the Compute
// Engine API's port, over JSON as Google serves it: connectors in a
// project's regions, their long-running operations and the locations.
//
// A connector is checked as Google checks it
// (https://docs.cloud.google.com/vpc/docs/reference/vpcaccess/rest/v1/projects.locations.connectors
// and vpc/docs/configure-serverless-vpc-access, read 2026-11-22): its
// network exists, its ipCidrRange is an unused /28 of the private ranges
// that no subnet, allocated range or other connector of the network
// overlaps, or its subnet is an existing /28 subnet in the connector's
// region that no VM and no other connector uses; minInstances 2 to 9,
// maxInstances 3 to 10, minInstances below maxInstances; the machine type
// f1-micro, e2-micro or e2-standard-4. It is READY when its create
// operation is done, which is at once: no connector VMs run, so Cloud Run
// records the connector it is given (internal/adapter/run) without
// carrying traffic through it (docs/gaps.md).
package vpcaccess

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/vpcaccess/apiv1/vpcaccesspb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// Networks is what a connector's network and subnet are checked against:
// the Compute Engine API's (compute.Server). Nil checks nothing but the
// fields' own rules.
type Networks interface {
	HasNetwork(ref, project string) bool
	Conflict(network string, n *net.IPNet) string
	SubnetRange(ref, project, region string) (network, cidr string, ok bool)
	SubnetUsers(rel string) []string
}

// Options configure a Server.
type Options struct {
	// State is the JSON file connectors and operations are kept in; empty
	// keeps them in memory.
	State string
	// Now is time.Now when nil.
	Now func() time.Time
	// Networks are the Compute Engine networks.
	Networks Networks
}

// Server is the Serverless VPC Access API.
type Server struct {
	vpcaccesspb.UnimplementedVpcAccessServiceServer
	opts Options

	mu         sync.Mutex
	connectors map[string]*vpcaccesspb.Connector
	ops        map[string]*longrunningpb.Operation

	// umu guards uses, a copy of connectors that NetworkUsers reads:
	// Compute Engine asks it with its own lock held, and connectors are
	// checked against Compute Engine with mu held, so NetworkUsers must
	// not take mu.
	umu  sync.Mutex
	uses map[string]*vpcaccesspb.Connector
}

// New returns a Server with its state restored.
func New(opts Options) (*Server, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	s := &Server{opts: opts, connectors: map[string]*vpcaccesspb.Connector{}, ops: map[string]*longrunningpb.Operation{}}
	if err := s.restore(); err != nil {
		return nil, err
	}
	s.refreshUsesLocked()
	return s, nil
}

var (
	parentRE    = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)$`)
	connectorRE = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/connectors/([^/]+)$`)
	// connectorIDRE is the API's rule for connector_id.
	connectorIDRE = regexp.MustCompile(`^[a-z][-a-z0-9]{0,23}[a-z0-9]$`)
	private       = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}
)

// MachineTypes are the connector instance types Google offers, the
// default first.
var MachineTypes = []string{"e2-micro", "f1-micro", "e2-standard-4"}

// mbpsPerInstance is the throughput each instance stands for in
// minThroughput and maxThroughput, which "refers to the expected
// throughput when using an e2-micro machine type" (REST reference,
// Connector): 100 Mbps, whatever the machine type.
const mbpsPerInstance = 100

func validLocation(r string) bool { return compute.IsRegion(r) }

func (s *Server) CreateConnector(_ context.Context, req *vpcaccesspb.CreateConnectorRequest) (*longrunningpb.Operation, error) {
	m := parentRE.FindStringSubmatch(req.GetParent())
	if m == nil {
		return nil, apierror.InvalidArgument("parent %q must be projects/{project}/locations/{location}", req.GetParent())
	}
	project, region := m[1], m[2]
	if !validLocation(region) {
		return nil, apierror.InvalidArgument("Location '%s' is not a valid region.", region)
	}
	id := req.GetConnectorId()
	if !connectorIDRE.MatchString(id) {
		return nil, apierror.InvalidArgument("Invalid value for field 'connector_id': '%s'. Connector ID must follow the pattern ^[a-z][-a-z0-9]{0,23}[a-z0-9]$.", id)
	}
	c, _ := proto.Clone(req.GetConnector()).(*vpcaccesspb.Connector)
	if c == nil {
		c = &vpcaccesspb.Connector{}
	}
	name := req.GetParent() + "/connectors/" + id
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.connectors[name]; ok {
		return nil, apierror.AlreadyExists("Connector '%s' already exists.", name)
	}
	if err := s.checkLocked(project, region, name, c); err != nil {
		return nil, err
	}
	c.Name, c.State, c.ConnectedProjects = name, vpcaccesspb.Connector_READY, nil
	s.connectors[name] = c
	return s.opLocked(req.GetParent(), "CreateConnector", name, c)
}

// checkLocked validates c, filling its defaults.
func (s *Server) checkLocked(project, region, name string, c *vpcaccesspb.Connector) error {
	nets := s.opts.Networks
	if c.GetSubnet() != nil && c.GetSubnet().GetName() != "" {
		if c.GetIpCidrRange() != "" {
			return apierror.InvalidArgument("Only one of ip_cidr_range and subnet may be set.")
		}
		sp := c.GetSubnet().GetProjectId()
		if sp == "" {
			sp = project
		}
		rel := "projects/" + sp + "/regions/" + region + "/subnetworks/" + c.GetSubnet().GetName()
		if nets != nil {
			network, cidr, ok := nets.SubnetRange(rel, sp, region)
			if !ok {
				return apierror.InvalidArgument("Subnetwork '%s' was not found in region '%s' of project '%s'.", c.GetSubnet().GetName(), region, sp)
			}
			if n, ok := compute.ParseCIDR(cidr); !ok || maskBits(n) != 28 {
				return apierror.InvalidArgument("Subnetwork '%s' has the range %s: a connector's subnet must have a /28 primary IPv4 range.", c.GetSubnet().GetName(), cidr)
			}
			if users := nets.SubnetUsers(rel); len(users) > 0 {
				return apierror.FailedPrecondition("Subnetwork '%s' is used by %s: a connector's subnet must be its own.", c.GetSubnet().GetName(), users[0])
			}
			if c.GetNetwork() != "" && lastSeg(c.GetNetwork()) != lastSeg(network) {
				return apierror.InvalidArgument("Subnetwork '%s' is in network '%s', not '%s'.", c.GetSubnet().GetName(), lastSeg(network), c.GetNetwork())
			}
			if sp == project {
				c.Network = lastSeg(network)
			}
		}
		for k, o := range s.connectors {
			if k != name && o.GetSubnet().GetName() == c.GetSubnet().GetName() && subnetProject(o, k) == sp && regionOf(k) == region {
				return apierror.FailedPrecondition("Subnetwork '%s' is already used by connector '%s'.", c.GetSubnet().GetName(), k)
			}
		}
		c.Subnet.ProjectId = sp
	} else {
		c.Subnet = nil
		if c.GetNetwork() == "" {
			return apierror.InvalidArgument("Either network and ip_cidr_range, or subnet, must be set.")
		}
		if c.GetIpCidrRange() == "" {
			return apierror.InvalidArgument("ip_cidr_range must be set with network.")
		}
		n, ok := compute.ParseCIDR(c.GetIpCidrRange())
		if !ok || n.String() != c.GetIpCidrRange() {
			return apierror.InvalidArgument("Invalid IP CIDR range '%s': it must be an IPv4 range in CIDR notation whose address is the first of the range, such as 10.8.0.0/28.", c.GetIpCidrRange())
		}
		if maskBits(n) != 28 {
			return apierror.InvalidArgument("Invalid IP CIDR range '%s': a connector's range must be a /28.", c.GetIpCidrRange())
		}
		inPrivate := false
		for _, p := range private {
			if pn, _ := compute.ParseCIDR(p); pn.Contains(n.IP) {
				inPrivate = true
			}
		}
		if !inPrivate {
			return apierror.InvalidArgument("Invalid IP CIDR range '%s': it must be in one of the private ranges %s.", c.GetIpCidrRange(), strings.Join(private, ", "))
		}
		network := "projects/" + project + "/global/networks/" + lastSeg(c.GetNetwork())
		if nets != nil {
			if !nets.HasNetwork(network, project) {
				return apierror.InvalidArgument("Network '%s' was not found in project '%s'.", lastSeg(c.GetNetwork()), project)
			}
			if what := nets.Conflict(network, n); what != "" {
				return apierror.InvalidArgument("Invalid IP CIDR range '%s': it conflicts with %s of network '%s'.", c.GetIpCidrRange(), what, lastSeg(c.GetNetwork()))
			}
		}
		for k, o := range s.connectors {
			if k == name || o.GetIpCidrRange() == "" || o.GetNetwork() != lastSeg(c.GetNetwork()) || projectOf(k) != project {
				continue
			}
			if on, ok := compute.ParseCIDR(o.GetIpCidrRange()); ok && compute.Overlaps(n, on) {
				return apierror.InvalidArgument("Invalid IP CIDR range '%s': it conflicts with connector '%s' (%s).", c.GetIpCidrRange(), lastSeg(k), o.GetIpCidrRange())
			}
		}
		c.Network = lastSeg(c.GetNetwork())
	}
	switch c.GetMachineType() {
	case "":
		c.MachineType = "e2-micro"
	case "e2-micro", "f1-micro", "e2-standard-4":
	default:
		return apierror.InvalidArgument("Invalid value for field 'machine_type': '%s'. Must be one of f1-micro, e2-micro or e2-standard-4.", c.GetMachineType())
	}
	per := int32(mbpsPerInstance)
	// Throughput is the older way to size a connector: min 200 to 900 and
	// max 300 to 1000 Mbps, multiples of 100, each instance of e2-micro
	// 100 Mbps; instances win when both are set (REST reference, Connector).
	if c.GetMinInstances() == 0 && c.GetMinThroughput() != 0 {
		t := c.GetMinThroughput()
		if t%100 != 0 || t < 200 || t > 900 {
			return apierror.InvalidArgument("Invalid value for field 'min_throughput': %d. Must be a multiple of 100 from 200 through 900.", t)
		}
		c.MinInstances = t / 100
	}
	if c.GetMaxInstances() == 0 && c.GetMaxThroughput() != 0 {
		t := c.GetMaxThroughput()
		if t%100 != 0 || t < 300 || t > 1000 {
			return apierror.InvalidArgument("Invalid value for field 'max_throughput': %d. Must be a multiple of 100 from 300 through 1000.", t)
		}
		c.MaxInstances = t / 100
	}
	if c.GetMinInstances() == 0 {
		c.MinInstances = 2
	}
	if c.GetMaxInstances() == 0 {
		c.MaxInstances = 10
	}
	if c.GetMinInstances() < 2 || c.GetMinInstances() > 9 {
		return apierror.InvalidArgument("Invalid value for field 'min_instances': %d. Must be between 2 and 9.", c.GetMinInstances())
	}
	if c.GetMaxInstances() < 3 || c.GetMaxInstances() > 10 {
		return apierror.InvalidArgument("Invalid value for field 'max_instances': %d. Must be between 3 and 10.", c.GetMaxInstances())
	}
	if c.GetMinInstances() >= c.GetMaxInstances() {
		return apierror.InvalidArgument("min_instances (%d) must be lower than max_instances (%d).", c.GetMinInstances(), c.GetMaxInstances())
	}
	c.MinThroughput, c.MaxThroughput = c.GetMinInstances()*per, c.GetMaxInstances()*per
	return nil
}

func maskBits(n *net.IPNet) int { b, _ := n.Mask.Size(); return b }

func lastSeg(s string) string { return s[strings.LastIndex(s, "/")+1:] }

func projectOf(name string) string { return strings.Split(name, "/")[1] }

func regionOf(name string) string { return strings.Split(name, "/")[3] }

func subnetProject(c *vpcaccesspb.Connector, name string) string {
	if p := c.GetSubnet().GetProjectId(); p != "" {
		return p
	}
	return projectOf(name)
}

func (s *Server) GetConnector(_ context.Context, req *vpcaccesspb.GetConnectorRequest) (*vpcaccesspb.Connector, error) {
	if !connectorRE.MatchString(req.GetName()) {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/connectors/{connector}", req.GetName())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.connectors[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("Connector '%s' was not found.", req.GetName())
	}
	return proto.Clone(c).(*vpcaccesspb.Connector), nil
}

func (s *Server) ListConnectors(_ context.Context, req *vpcaccesspb.ListConnectorsRequest) (*vpcaccesspb.ListConnectorsResponse, error) {
	m := parentRE.FindStringSubmatch(req.GetParent())
	if m == nil {
		return nil, apierror.InvalidArgument("parent %q must be projects/{project}/locations/{location}", req.GetParent())
	}
	if m[2] != "-" && !validLocation(m[2]) {
		return nil, apierror.InvalidArgument("Location '%s' is not a valid region.", m[2])
	}
	s.mu.Lock()
	var names []string
	for k := range s.connectors {
		if strings.HasPrefix(k, req.GetParent()+"/connectors/") ||
			(m[2] == "-" && strings.HasPrefix(k, "projects/"+m[1]+"/locations/")) {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	page, next, err := paging.Page("connectors:"+req.GetParent(), names, req.GetPageToken(), int(req.GetPageSize()))
	out := &vpcaccesspb.ListConnectorsResponse{NextPageToken: next}
	for _, k := range page {
		out.Connectors = append(out.Connectors, proto.Clone(s.connectors[k]).(*vpcaccesspb.Connector))
	}
	s.mu.Unlock()
	if err != nil {
		return nil, apierror.InvalidArgument("invalid page_token")
	}
	return out, nil
}

func (s *Server) DeleteConnector(_ context.Context, req *vpcaccesspb.DeleteConnectorRequest) (*longrunningpb.Operation, error) {
	m := connectorRE.FindStringSubmatch(req.GetName())
	if m == nil {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/connectors/{connector}", req.GetName())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.connectors[req.GetName()]; !ok {
		return nil, apierror.NotFound("Connector '%s' was not found.", req.GetName())
	}
	delete(s.connectors, req.GetName())
	return s.opLocked("projects/"+m[1]+"/locations/"+m[2], "DeleteConnector", req.GetName(), &emptypb.Empty{})
}

// Connector is the connector name (projects/P/locations/R/connectors/C),
// for Cloud Run's vpcAccess.connector; false when there is none.
func (s *Server) Connector(name string) (*vpcaccesspb.Connector, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.connectors[name]
	if !ok {
		return nil, false
	}
	return proto.Clone(c).(*vpcaccesspb.Connector), true
}

// ResetProject forgets a project's connectors and operations, or every
// project's when project is "".
func (s *Server) ResetProject(project string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.connectors {
		if project == "" || projectOf(k) == project {
			delete(s.connectors, k)
		}
	}
	for k := range s.ops {
		if project == "" || projectOf(k) == project {
			delete(s.ops, k)
		}
	}
	return s.saveLocked()
}

// opLocked records a done operation of method on target, answering resp,
// and saves.
func (s *Server) opLocked(parent, method, target string, resp proto.Message) (*longrunningpb.Operation, error) {
	now := timestamppb.New(s.opts.Now())
	md, err := anypb.New(&vpcaccesspb.OperationMetadata{Method: "google.cloud.vpcaccess.v1.VpcAccessService." + method,
		CreateTime: now, EndTime: now, Target: target})
	if err != nil {
		return nil, apierror.Internal(err, "operation metadata")
	}
	r, err := anypb.New(resp)
	if err != nil {
		return nil, apierror.Internal(err, "operation response")
	}
	name := fmt.Sprintf("%s/operations/%x", parent, s.opts.Now().UnixNano())
	for s.ops[name] != nil {
		name += "0"
	}
	op := &longrunningpb.Operation{Name: name, Metadata: md, Done: true, Result: &longrunningpb.Operation_Response{Response: r}}
	s.ops[name] = op
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return proto.Clone(op).(*longrunningpb.Operation), nil
}

type operations struct {
	longrunningpb.UnimplementedOperationsServer
	s *Server
}

func (o *operations) GetOperation(_ context.Context, req *longrunningpb.GetOperationRequest) (*longrunningpb.Operation, error) {
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	op := o.s.ops[req.GetName()]
	if op == nil {
		return nil, apierror.NotFound("Operation '%s' was not found.", req.GetName())
	}
	return proto.Clone(op).(*longrunningpb.Operation), nil
}

func (o *operations) ListOperations(_ context.Context, req *longrunningpb.ListOperationsRequest) (*longrunningpb.ListOperationsResponse, error) {
	if !parentRE.MatchString(req.GetName()) {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}", req.GetName())
	}
	o.s.mu.Lock()
	var names []string
	for k := range o.s.ops {
		if strings.HasPrefix(k, req.GetName()+"/operations/") {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	page, next, err := paging.Page("ops:"+req.GetName(), names, req.GetPageToken(), int(req.GetPageSize()))
	out := &longrunningpb.ListOperationsResponse{NextPageToken: next}
	for _, k := range page {
		out.Operations = append(out.Operations, proto.Clone(o.s.ops[k]).(*longrunningpb.Operation))
	}
	o.s.mu.Unlock()
	if err != nil {
		return nil, apierror.InvalidArgument("invalid page_token")
	}
	return out, nil
}

// state is the saved file: each message as its protojson.
type state struct {
	Connectors map[string]json.RawMessage `json:"connectors"`
	Operations map[string]json.RawMessage `json:"operations"`
}

func (s *Server) restore() error {
	if s.opts.State == "" {
		return nil
	}
	b, err := os.ReadFile(s.opts.State)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("vpcaccess: %w", err)
	}
	var st state
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("vpcaccess: %s: %w", s.opts.State, err)
	}
	for k, raw := range st.Connectors {
		c := &vpcaccesspb.Connector{}
		if err := protojson.Unmarshal(raw, c); err != nil {
			return fmt.Errorf("vpcaccess: connector %s: %w", k, err)
		}
		s.connectors[k] = c
	}
	for k, raw := range st.Operations {
		op := &longrunningpb.Operation{}
		if err := protojson.Unmarshal(raw, op); err != nil {
			return fmt.Errorf("vpcaccess: operation %s: %w", k, err)
		}
		s.ops[k] = op
	}
	return nil
}

func (s *Server) saveLocked() error {
	s.refreshUsesLocked()
	if s.opts.State == "" {
		return nil
	}
	st := state{Connectors: map[string]json.RawMessage{}, Operations: map[string]json.RawMessage{}}
	for k, c := range s.connectors {
		b, err := protojson.Marshal(c)
		if err != nil {
			return apierror.Internal(err, "save")
		}
		st.Connectors[k] = b
	}
	for k, op := range s.ops {
		b, err := protojson.Marshal(op)
		if err != nil {
			return apierror.Internal(err, "save")
		}
		st.Operations[k] = b
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return apierror.Internal(err, "save")
	}
	if err := os.MkdirAll(filepath.Dir(s.opts.State), 0o755); err != nil {
		return apierror.Internal(err, "save")
	}
	tmp := s.opts.State + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return apierror.Internal(err, "save")
	}
	if err := os.Rename(tmp, s.opts.State); err != nil {
		return apierror.Internal(err, "save")
	}
	return nil
}
