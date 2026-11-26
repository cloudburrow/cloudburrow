package main

// A Dataflow job's workers reach the Cloud SQL private IPs of the VPC
// network they run on (#2215), as a Compute Engine VM on it does (#2141):
// the job's network or subnetwork option, else the project's network
// named default (docs.cloud.google.com/dataflow/docs/guides/
// specifying-networks, read 2026-11-25). Each instance's private IP and
// port, and its pooler's, are relayed to the instance's server on the
// host (beamrun.PrivateService); nothing else of the network is.

import (
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/beamrun"
	"github.com/cloudburrow/cloudburrow/internal/service/compute"
	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
)

// dataflowPrivateServices answers a job's private services: the Cloud
// SQL instances with a private IP on its network. sql is the Cloud SQL
// Admin API's server, and api Compute Engine's (nil when it is not
// served: a subnetwork is then not resolved, and a network is taken by
// its name).
func dataflowPrivateServices(sql func() *sqladmin.Server, api func() *compute.Server) func(project, network, subnetwork, region string) []beamrun.PrivateService {
	return func(project, network, subnetwork, region string) []beamrun.PrivateService {
		srv := sql()
		if srv == nil {
			return nil
		}
		rel := dataflowNetwork(project, network, subnetwork, region, api)
		if rel == "" {
			return nil
		}
		var out []beamrun.PrivateService
		for _, ep := range srv.PrivateEndpoints() {
			if ep.Network != rel || ep.HostAddr == "" {
				continue
			}
			name := "cloudsql " + ep.Instance
			out = append(out, beamrun.PrivateService{Name: name, IP: ep.IP, Port: sqlEnginePort(ep.DatabaseVersion), Target: ep.HostAddr})
			if ep.PoolerPort != 0 && ep.PoolerHostAddr != "" {
				out = append(out, beamrun.PrivateService{Name: name + " pooler", IP: ep.IP, Port: ep.PoolerPort, Target: ep.PoolerHostAddr})
			}
		}
		return out
	}
}

// dataflowNetwork is the relative name (projects/P/global/networks/N) of
// the network a job's workers run on: its subnetwork's network when it
// names one (a complete URL, regions/R/subnetworks/S, or a name in the
// job's region), else its network (a name, or a complete URL), else
// default; "" when its subnetwork is not one Compute Engine has.
func dataflowNetwork(project, network, subnetwork, region string, api func() *compute.Server) string {
	if subnetwork != "" {
		var c *compute.Server
		if api != nil {
			c = api()
		}
		if c == nil {
			return ""
		}
		n, ok := c.Subnetwork(subnetwork, project, region)
		if !ok {
			return ""
		}
		return compute.Rel(n, project)
	}
	if network == "" {
		network = "default"
	}
	rel := compute.Rel(network, project)
	if !strings.Contains(rel, "/") {
		rel = "projects/" + project + "/global/networks/" + rel
	}
	return rel
}

// computeAPIOf is s's Compute Engine API, or nil when Compute Engine is
// not served. runUp's compute is the compute host (#2209), which hides the
// compute package there.
func computeAPIOf(s *computeService) func() *compute.Server {
	if s == nil {
		return nil
	}
	return s.API
}
