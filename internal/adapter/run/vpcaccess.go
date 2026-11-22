package run

import (
	"encoding/json"
	"strings"

	"cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// VPC checks a service's or job's VPC egress (#2140) against this
// instance's Serverless VPC Access connectors and Compute Engine networks,
// as Google checks them
// (https://docs.cloud.google.com/run/docs/reference/rest/v2/VpcAccess,
// run/docs/configuring/vpc-connectors and vpc-direct-vpc, read
// 2026-11-22). Nil, without Compute Engine, records what it is given
// unchecked, as before.
type VPC interface {
	// ConnectorReady is nil when the connector (projects/P/locations/R/
	// connectors/C) exists and is READY.
	ConnectorReady(name string) error
	// HasNetwork reports whether the network ref exists, in project when
	// ref is a bare name.
	HasNetwork(ref, project string) bool
	// Subnetwork is the network of the subnetwork ref, in project and
	// region when ref is a bare name.
	Subnetwork(ref, project, region string) (network string, ok bool)
}

// WithVPC checks VPC access against v.
func (s *Server) WithVPC(v VPC) *Server {
	s.vpc = v
	return s
}

// checkVPC checks a template's vpcAccess, filling the connector's full
// name from a bare one, as gcloud does, and a Direct VPC egress
// interface's network from its subnetwork. resource is the service's or
// job's name (projects/P/locations/R/...).
func (s *Server) checkVPC(resource string, v *runpb.VpcAccess) error {
	if v == nil {
		return nil
	}
	p := strings.Split(resource, "/")
	if len(p) < 4 {
		return nil
	}
	project, region := p[1], p[3]
	switch v.GetEgress() {
	case runpb.VpcAccess_VPC_EGRESS_UNSPECIFIED, runpb.VpcAccess_ALL_TRAFFIC, runpb.VpcAccess_PRIVATE_RANGES_ONLY:
	default:
		return apierror.InvalidArgument("vpc_access.egress: %v is not a valid egress setting", v.GetEgress())
	}
	if v.GetConnector() != "" && len(v.GetNetworkInterfaces()) > 0 {
		return apierror.InvalidArgument("vpc_access: a VPC connector and network interfaces (Direct VPC egress) cannot both be set")
	}
	if len(v.GetNetworkInterfaces()) > 1 {
		return apierror.InvalidArgument("vpc_access.network_interfaces: at most one network interface is supported")
	}
	if s.vpc == nil {
		return nil
	}
	if c := v.GetConnector(); c != "" {
		if !strings.Contains(c, "/") {
			c = "projects/" + project + "/locations/" + region + "/connectors/" + c
			v.Connector = c
		}
		cp := strings.Split(c, "/")
		if len(cp) != 6 || cp[0] != "projects" || cp[2] != "locations" || cp[4] != "connectors" {
			return apierror.InvalidArgument("vpc_access.connector %q must be projects/{project}/locations/{location}/connectors/{connector}", c)
		}
		if cp[3] != region {
			return apierror.InvalidArgument("vpc_access.connector: connector %s is in region %s; it must be in the same region as the resource (%s)", cp[5], cp[3], region)
		}
		if err := s.vpc.ConnectorReady(c); err != nil {
			return err
		}
	}
	for _, ni := range v.GetNetworkInterfaces() {
		network, sub := ni.GetNetwork(), ni.GetSubnetwork()
		if network == "" && sub == "" {
			return apierror.InvalidArgument("vpc_access.network_interfaces: network or subnetwork must be set")
		}
		if network != "" && !s.vpc.HasNetwork(network, project) {
			return apierror.InvalidArgument("vpc_access.network_interfaces.network: network %q was not found in project %s", network, project)
		}
		if sub == "" {
			// "If you omit the subnet, the subnet with the same name as the
			// network is used" (run/docs/configuring/vpc-direct-vpc).
			sub = network[strings.LastIndex(network, "/")+1:]
		}
		subNet, ok := s.vpc.Subnetwork(sub, project, region)
		if !ok {
			return apierror.InvalidArgument("vpc_access.network_interfaces.subnetwork: subnetwork %q was not found in region %s", sub, region)
		}
		if network != "" && subNet[strings.LastIndex(subNet, "/")+1:] != network[strings.LastIndex(network, "/")+1:] {
			return apierror.InvalidArgument("vpc_access.network_interfaces: subnetwork %q is not in network %q", sub, network)
		}
		if network == "" {
			ni.Network = subNet[strings.LastIndex(subNet, "/")+1:]
		}
	}
	return nil
}

// annVPCEgress is on a revision's and an execution's pods when the
// template has vpcAccess (#2140): the Cloud Run resource's name and its
// vpcAccess as protojson, which cmd/cloudburrow/computerunegress.go reads
// to carry the pod's traffic onto the VPC network through a gateway.
const annVPCEgress = "cloudburrow.dev/vpc-egress"

// vpcEgressAnnotation is annVPCEgress's value, "" without vpcAccess.
func vpcEgressAnnotation(name string, v *runpb.VpcAccess) string {
	if v == nil || (v.GetConnector() == "" && len(v.GetNetworkInterfaces()) == 0) {
		return ""
	}
	raw, err := protojson.Marshal(v)
	if err != nil {
		return ""
	}
	out, _ := json.Marshal(map[string]any{"name": name, "vpcAccess": json.RawMessage(raw)})
	return string(out)
}
