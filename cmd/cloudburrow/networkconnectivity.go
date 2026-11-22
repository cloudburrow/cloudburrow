package main

// The Network Connectivity API's service connectivity automation (#2141)
// is served on the Compute Engine port, over gRPC and JSON: its policies
// and maps make Compute Engine's Private Service Connect endpoints, in
// process, and Cloud SQL's automatic endpoints (pscAutoConnections) are a
// map of its own. Its paths (/v1/...) never meet Compute Engine's
// (/compute/v1/...).

import (
	"errors"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/grpchttp"
	"github.com/cloudburrow/cloudburrow/internal/service/compute"
	"github.com/cloudburrow/cloudburrow/internal/service/networkconnectivity"
	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
)

// ncCompute is Compute Engine as the Network Connectivity API asks for it.
type ncCompute struct{ api *compute.Server }

func (c ncCompute) HasNetwork(ref, project string) bool { return c.api.HasNetwork(ref, project) }

func (c ncCompute) Subnetwork(ref, project, region string) (string, bool) {
	return c.api.Subnetwork(ref, project, region)
}

func (c ncCompute) CreateAutoEndpoint(e networkconnectivity.Endpoint) (networkconnectivity.EndpointResult, error) {
	r, err := c.api.CreateAutoEndpoint(compute.AutoEndpoint{Project: e.Project, Region: e.Region, Name: e.Name, Network: e.Network,
		Subnetworks: e.Subnetworks, Target: e.Target, GlobalAccess: e.GlobalAccess, Description: e.Description})
	if errors.Is(err, compute.ErrNoSubnetAddress) || (err != nil && strings.Contains(err.Error(), "exhausted")) {
		err = errors.Join(networkconnectivity.ErrNoAddress, err)
	}
	return networkconnectivity.EndpointResult{ForwardingRule: r.ForwardingRule, IP: r.IP, Subnetwork: r.Subnetwork,
		PSCConnectionID: r.PSCConnectionID, Operation: r.Operation}, err
}

func (c ncCompute) DeleteAutoEndpoint(fr string) error { return c.api.DeleteAutoEndpoint(fr) }

// startNetworkConnectivity makes the API's server over api, and the
// handler serving it beside Compute Engine's (computeAPI) on one port:
// gRPC and the JSON paths go to it. The port serves h2c for the gRPC.
func (s *computeService) startNetworkConnectivity(api *compute.Server, computeAPI http.Handler) (http.Handler, error) {
	state := ""
	if s.cfg.Mode == config.ModePersistent {
		state = filepath.Join(s.cfg.InstanceDir(), "networkconnectivity.json")
	}
	nc, err := networkconnectivity.NewServer(networkconnectivity.Options{State: state,
		Compute: func() networkconnectivity.Compute { return ncCompute{api: api} }})
	if err != nil {
		return nil, err
	}
	s.nc = nc
	g := grpchttp.NewServer()
	nc.Register(g)
	s.ncGRPC = g
	rest := networkconnectivity.NewRESTHandler(nc)
	both := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
			g.ServeHTTP(w, r)
			return
		}
		if networkconnectivity.IsPath(r) {
			rest.ServeHTTP(w, r)
			return
		}
		computeAPI.ServeHTTP(w, r)
	})
	return both, nil
}

// NetworkConnectivity is the Network Connectivity API, available after
// Start; nil without compute.
func (s *computeService) NetworkConnectivity() *networkconnectivity.Server {
	if s == nil {
		return nil
	}
	return s.nc
}

// stopNetworkConnectivity stops the gRPC server after the port's.
func (s *computeService) stopNetworkConnectivity() {
	if s.ncGRPC != nil {
		s.ncGRPC.Stop()
	}
}

// sqlAutoConnections are Cloud SQL's automatic endpoints, through the
// Network Connectivity API, when Compute Engine is served.
type sqlAutoConnections struct{ svc *computeService }

func (a sqlAutoConnections) SetAutoConnections(attachment, project, instance string, consumers []sqladmin.AutoConsumer) error {
	nc := a.svc.NetworkConnectivity()
	if nc == nil {
		return nil
	}
	cs := make([]networkconnectivity.Consumer, 0, len(consumers))
	for _, c := range consumers {
		cs = append(cs, networkconnectivity.Consumer{Network: c.Network, Project: c.Project})
	}
	return nc.SetProducer(networkconnectivity.CloudSQLServiceClass, attachment, project, instance, cs)
}

func (a sqlAutoConnections) AutoConnections(attachment string) []sqladmin.AutoConnection {
	nc := a.svc.NetworkConnectivity()
	if nc == nil {
		return nil
	}
	var out []sqladmin.AutoConnection
	for _, c := range nc.Producer(attachment) {
		out = append(out, sqladmin.AutoConnection{Network: c.Network, Project: c.Project, IP: c.IP, Status: c.Status,
			NetworkStatus: c.NetworkStatus, ForwardingRule: c.ForwardingRule})
	}
	return out
}
