package compute

import (
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
)

// Delta (incremental) xDS for proxyless gRPC and Envoy (#2160), as the xDS
// REST and gRPC protocol describes it
// (envoyproxy.io/docs/envoy/latest/api-docs/xds_protocol, "Incremental
// xDS", which Cloud Service Mesh's xDS API serves beside the state of the
// world one): each DeltaDiscoveryRequest subscribes to or unsubscribes
// from resource names of a type (an empty first subscription to listeners
// or clusters, or the name "*", is the wildcard: every one), and may give
// the versions the client already holds (initial_resource_versions, on a
// reconnect); the server answers with only the resources that are new or
// changed since what the client holds, each with its own version, and
// names in removed_resources those it held that no longer exist (or that
// it asked for and do not exist). An ACK or NACK (a response_nonce with no
// subscription change) needs no answer; a change to the resources is
// pushed to every open stream within a second.

// deltaType is one type's subscription on a delta stream.
type deltaType struct {
	wildcard bool
	names    map[string]bool
	held     map[string]string // name -> version the client holds
	absent   map[string]bool   // names already reported as removed
}

// DeltaAggregatedResources serves the incremental protocol.
func (x *xdsServer) DeltaAggregatedResources(st discoveryv3.AggregatedDiscoveryService_DeltaAggregatedResourcesServer) error {
	var mu sync.Mutex
	types := map[string]*deltaType{}
	nonce := 0
	var node *corev3.Node // the client's, from its first request
	send := func(typ string) error {
		mu.Lock()
		dt := types[typ]
		nd := node
		if dt == nil {
			mu.Unlock()
			return nil
		}
		var names []string
		for n := range dt.names {
			names = append(names, n)
		}
		wildcard := dt.wildcard
		mu.Unlock()
		if wildcard {
			names = append(names, x.s.xdsAllNames(typ)...)
		}
		names = uniqSorted(names)
		have := x.s.xdsNamedResources(typ, names, nd)
		mu.Lock()
		defer mu.Unlock()
		resp := &discoveryv3.DeltaDiscoveryResponse{TypeUrl: typ}
		exists := map[string]bool{}
		for _, r := range have {
			exists[r.name] = true
			delete(dt.absent, r.name)
			if dt.held[r.name] == r.version {
				continue
			}
			dt.held[r.name] = r.version
			resp.Resources = append(resp.Resources, &discoveryv3.Resource{Name: r.name, Version: r.version, Resource: r.res})
		}
		for n := range dt.held {
			if !exists[n] {
				delete(dt.held, n)
				dt.absent[n] = true
				resp.RemovedResources = append(resp.RemovedResources, n)
			}
		}
		// A name asked for by name that does not exist is reported as
		// removed once, so the client need not wait for its timer.
		for n := range dt.names {
			if !exists[n] && !dt.absent[n] {
				dt.absent[n] = true
				resp.RemovedResources = append(resp.RemovedResources, n)
			}
		}
		if len(resp.Resources) == 0 && len(resp.RemovedResources) == 0 {
			return nil
		}
		sort.Strings(resp.RemovedResources)
		nonce++
		resp.Nonce = strconv.Itoa(nonce)
		resp.SystemVersionInfo = resp.Nonce
		return st.Send(resp)
	}
	errc := make(chan error, 1)
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-st.Context().Done():
				return
			case <-t.C:
				mu.Lock()
				ts := make([]string, 0, len(types))
				for k := range types {
					ts = append(ts, k)
				}
				mu.Unlock()
				for _, typ := range ts {
					if err := send(typ); err != nil {
						select {
						case errc <- err:
						default:
						}
						return
					}
				}
			}
		}
	}()
	for {
		req, err := st.Recv()
		if err != nil {
			return err
		}
		select {
		case err := <-errc:
			return err
		default:
		}
		if req.GetNode() != nil {
			mu.Lock()
			node = req.GetNode()
			mu.Unlock()
		}
		typ := req.GetTypeUrl()
		switch typ {
		case typeListener, typeRoute, typeCluster, typeEndpoint:
		default:
			continue
		}
		mu.Lock()
		dt := types[typ]
		first := dt == nil
		if first {
			dt = &deltaType{names: map[string]bool{}, held: map[string]string{}, absent: map[string]bool{}}
			types[typ] = dt
			for n, v := range req.GetInitialResourceVersions() {
				dt.held[n], dt.names[n] = v, true
			}
			if len(req.GetResourceNamesSubscribe()) == 0 && len(req.GetInitialResourceVersions()) == 0 && (typ == typeListener || typ == typeCluster) {
				dt.wildcard = true
			}
		}
		changed := first
		for _, n := range req.GetResourceNamesSubscribe() {
			if n == "*" {
				changed = changed || !dt.wildcard
				dt.wildcard = true
				continue
			}
			if !dt.names[n] {
				dt.names[n], changed = true, true
			}
		}
		for _, n := range req.GetResourceNamesUnsubscribe() {
			if n == "*" {
				dt.wildcard = false
				changed = true
				continue
			}
			if dt.names[n] {
				delete(dt.names, n)
				changed = true
			}
			// The client drops what it unsubscribed from.
			delete(dt.held, n)
			delete(dt.absent, n)
		}
		mu.Unlock()
		if !changed {
			// An ACK or NACK of a response.
			continue
		}
		if err := send(typ); err != nil {
			return err
		}
	}
}

// xdsAllNames are every resource name of a type, for a wildcard
// subscription: the clusters of every global INTERNAL_SELF_MANAGED backend
// service, and the listeners HOST:PORT of every host a target gRPC
// proxy's URL map names exactly (a host pattern with "*" is no name).
func (s *Server) xdsAllNames(typ string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	switch typ {
	case typeCluster:
		for k, o := range s.res {
			if strings.Contains(k, "/global/backendServices/") && str(o, "loadBalancingScheme") == "INTERNAL_SELF_MANAGED" {
				out = append(out, xdsClusterName(o))
			}
		}
	case typeListener:
		for k, r := range s.res {
			if !strings.Contains(k, "/global/forwardingRules/") || str(r, "loadBalancingScheme") != "INTERNAL_SELF_MANAGED" ||
				!strings.Contains(str(r, "target"), "/targetGrpcProxies/") {
				continue
			}
			port := str(r, "portRange")
			if i := strings.Index(port, "-"); i > 0 {
				port = port[:i]
			}
			if port == "" {
				continue
			}
			um := s.res[Rel(str(s.res[Rel(str(r, "target"), "")], "urlMap"), "")]
			for _, hr := range listOf(um["hostRules"]) {
				for _, h := range strs(objOf(hr), "hosts") {
					if !strings.Contains(h, "*") {
						out = append(out, net.JoinHostPort(h, port))
					}
				}
			}
		}
	}
	return out
}

func uniqSorted(l []string) []string {
	sort.Strings(l)
	out := l[:0]
	for i, v := range l {
		if i == 0 || v != l[i-1] {
			out = append(out, v)
		}
	}
	return out
}
