package compute

import (
	"crypto/sha256"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	routerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	cswrrv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/load_balancing_policies/client_side_weighted_round_robin/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	matcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// The xDS API of proxyless gRPC (#2160), as Cloud Service Mesh's
// proxyless gRPC pages describe it (service-mesh/docs/service-routing/
// proxyless-overview and set-up-proxyless-gce, read 2026-11-22): a gRPC
// client built with xDS support, its bootstrap file naming the xDS server
// (Google's is trafficdirector.googleapis.com:443; here the Compute Engine
// port, over HTTP/2 without TLS, given by XDSBootstrap), dials
// xds:///HOST:PORT, asks for the listener HOST:PORT over the Aggregated
// Discovery Service, and receives the routes of the URL map whose target
// gRPC proxy a global INTERNAL_SELF_MANAGED forwarding rule of PORT targets
// (the host rule matching HOST, else the default service), the backend
// services as EDS clusters and their healthy endpoints, which it then calls
// directly. A change to any of them is pushed to every open stream.

const (
	typeListener = "type.googleapis.com/envoy.config.listener.v3.Listener"
	typeRoute    = "type.googleapis.com/envoy.config.route.v3.RouteConfiguration"
	typeCluster  = "type.googleapis.com/envoy.config.cluster.v3.Cluster"
	typeEndpoint = "type.googleapis.com/envoy.config.endpoint.v3.ClusterLoadAssignment"
)

// XDSBootstrapPath answers a bootstrap file for this port.
const XDSBootstrapPath = "/cloudburrow/xds/bootstrap"

// XDSHandler serves the Aggregated Discovery Service as gRPC over HTTP/2
// (Content-Type application/grpc) on the Compute Engine port.
func (s *Server) XDSHandler() http.Handler {
	s.xdsOnce.Do(func() {
		g := grpc.NewServer()
		discoveryv3.RegisterAggregatedDiscoveryServiceServer(g, &xdsServer{s: s})
		s.xdsGRPC = g
	})
	return s.xdsGRPC
}

// XDSBootstrap is a gRPC xDS bootstrap file naming this server at addr,
// for GRPC_XDS_BOOTSTRAP or GRPC_XDS_BOOTSTRAP_CONFIG, as the
// trafficdirector bootstrap generator writes it for Google's.
func XDSBootstrap(addr, project, network string) string {
	if network == "" {
		network = "default"
	}
	return fmt.Sprintf(`{"xds_servers":[{"server_uri":%q,"channel_creds":[{"type":"insecure"}],"server_features":["xds_v3"]}],`+
		`"node":{"id":"projects/%s/networks/%s/nodes/cloudburrow","cluster":"cloudburrow","metadata":{"TRAFFICDIRECTOR_NETWORK_NAME":%q,"TRAFFICDIRECTOR_GCP_PROJECT_NUMBER":%q},`+
		`"locality":{"zone":"us-central1-a"}}}`, addr, project, network, network, project)
}

type xdsServer struct {
	discoveryv3.UnimplementedAggregatedDiscoveryServiceServer
	s *Server
}

// StreamAggregatedResources is the state of the world protocol: each
// request names the resources of a type the client wants; the server
// answers with all of them, and again whenever they change.
func (x *xdsServer) StreamAggregatedResources(st discoveryv3.AggregatedDiscoveryService_StreamAggregatedResourcesServer) error {
	var mu sync.Mutex
	subs := map[string][]string{} // type URL -> names
	sent := map[string]string{}   // type URL -> version sent
	nonce := 0
	send := func(typ string, force bool) error {
		mu.Lock()
		names := append([]string(nil), subs[typ]...)
		mu.Unlock()
		res, ver := x.s.xdsResources(typ, names)
		mu.Lock()
		if !force && sent[typ] == ver {
			mu.Unlock()
			return nil
		}
		sent[typ] = ver
		nonce++
		n := strconv.Itoa(nonce)
		mu.Unlock()
		return st.Send(&discoveryv3.DiscoveryResponse{VersionInfo: ver, Resources: res, TypeUrl: typ, Nonce: n})
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
				types := make([]string, 0, len(subs))
				for k := range subs {
					types = append(types, k)
				}
				mu.Unlock()
				for _, typ := range types {
					if err := send(typ, false); err != nil {
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
		typ := req.GetTypeUrl()
		switch typ {
		case typeListener, typeRoute, typeCluster, typeEndpoint:
		default:
			continue
		}
		names := append([]string(nil), req.GetResourceNames()...)
		sort.Strings(names)
		mu.Lock()
		changed := strings.Join(subs[typ], "\x00") != strings.Join(names, "\x00")
		_, known := subs[typ]
		subs[typ] = names
		// An ACK or NACK of what was sent, with the same names, needs
		// no answer.
		ack := known && !changed && req.GetResponseNonce() != ""
		mu.Unlock()
		if ack {
			continue
		}
		if err := send(typ, true); err != nil {
			return err
		}
	}
}

// xdsResources are the resources of a type for names, and a version that
// changes when they do.
func (s *Server) xdsResources(typ string, names []string) ([]*anypb.Any, string) {
	named := s.xdsNamedResources(typ, names)
	h := sha256.New()
	var out []*anypb.Any
	for _, r := range named {
		h.Write([]byte(r.version))
		out = append(out, r.res)
	}
	return out, fmt.Sprintf("%x", h.Sum(nil))[:16]
}

// xdsNamed is one resource with its name and own version.
type xdsNamed struct {
	name, version string
	res           *anypb.Any
}

// xdsNamedResources are the resources of a type for names that exist, in
// the order of names, each with a version that changes when it does.
func (s *Server) xdsNamedResources(typ string, names []string) []xdsNamed {
	s.mu.Lock()
	type nm struct {
		name string
		msg  proto.Message
	}
	var msgs []nm
	for _, n := range names {
		var m proto.Message
		switch typ {
		case typeListener:
			if l := s.xdsListenerLocked(n); l != nil {
				m = l
			}
		case typeRoute:
			// Routes are inline in the listeners.
		case typeCluster:
			if c := s.xdsClusterLocked(n); c != nil {
				m = c
			}
		case typeEndpoint:
			if e := s.xdsEndpointsLocked(n); e != nil {
				m = e
			}
		}
		if m != nil {
			msgs = append(msgs, nm{n, m})
		}
	}
	s.mu.Unlock()
	var out []xdsNamed
	for _, m := range msgs {
		a, err := anypb.New(m.msg)
		if err != nil {
			continue
		}
		b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(a)
		sum := sha256.Sum256(b)
		out = append(out, xdsNamed{name: m.name, version: fmt.Sprintf("%x", sum[:8]), res: a})
	}
	return out
}

// xdsClusterName is a backend service's cluster, as Google names them
// (cloud-internal-istio:cloud_mp_PROJECT_ID).
func xdsClusterName(bs obj) string {
	return "cloud-internal-istio:cloud_mp_" + projectOf(str(bs, "selfLink")) + "_" + fmt.Sprint(bs["id"])
}

func projectOf(selfLink string) string {
	p := strings.Split(Rel(selfLink, ""), "/")
	if len(p) > 1 {
		return p[1]
	}
	return ""
}

// xdsListenerLocked is the listener HOST:PORT: the URL map of the
// target gRPC proxy of a global INTERNAL_SELF_MANAGED rule of PORT whose
// host rules (or default service) take HOST.
func (s *Server) xdsListenerLocked(name string) *listenerv3.Listener {
	host, ps, err := net.SplitHostPort(name)
	if err != nil {
		host, ps = name, "80"
	}
	port, _ := strconv.Atoi(ps)
	keys := make([]string, 0)
	for k := range s.res {
		if strings.Contains(k, "/global/forwardingRules/") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		r := s.res[k]
		if str(r, "loadBalancingScheme") != "INTERNAL_SELF_MANAGED" || !strings.Contains(str(r, "target"), "/targetGrpcProxies/") {
			continue
		}
		if !portsOverlap(r, []int{port}) {
			continue
		}
		proxy := s.res[Rel(str(r, "target"), "")]
		um := s.res[Rel(str(proxy, "urlMap"), "")]
		if um == nil {
			continue
		}
		vh := s.xdsVirtualHostLocked(um, host)
		if vh == nil {
			continue
		}
		rc := &routev3.RouteConfiguration{Name: "URL_MAP/" + projectOf(str(um, "selfLink")) + "_" + str(um, "name"), VirtualHosts: []*routev3.VirtualHost{vh}}
		router, _ := anypb.New(&routerv3.Router{})
		hcm, _ := anypb.New(&hcmv3.HttpConnectionManager{
			RouteSpecifier: &hcmv3.HttpConnectionManager_RouteConfig{RouteConfig: rc},
			HttpFilters:    []*hcmv3.HttpFilter{{Name: "envoy.filters.http.router", ConfigType: &hcmv3.HttpFilter_TypedConfig{TypedConfig: router}}},
		})
		return &listenerv3.Listener{Name: name, ApiListener: &listenerv3.ApiListener{ApiListener: hcm}}
	}
	return nil
}

// xdsVirtualHostLocked is a URL map's routes for host: its matching host
// rule's path matcher, else its default service.
func (s *Server) xdsVirtualHostLocked(um obj, host string) *routev3.VirtualHost {
	var pm obj
	for _, e := range listOf(um["hostRules"]) {
		hr := objOf(e)
		for _, h := range strs(hr, "hosts") {
			if h == "*" || strings.EqualFold(h, host) || strings.HasPrefix(h, "*.") && strings.HasSuffix(strings.ToLower(host), strings.ToLower(h[1:])) {
				for _, p := range listOf(um["pathMatchers"]) {
					if str(objOf(p), "name") == str(hr, "pathMatcher") {
						pm = objOf(p)
					}
				}
			}
		}
		if pm != nil {
			break
		}
	}
	vh := &routev3.VirtualHost{Name: host, Domains: []string{host, host + ":*"}}
	def := str(um, "defaultService")
	if pm != nil {
		def = str(pm, "defaultService")
		for _, e := range listOf(pm["routeRules"]) {
			rr := objOf(e)
			action := s.xdsActionLocked(str(rr, "service"), objOf(rr["routeAction"]))
			if action == nil {
				continue
			}
			for _, m := range listOf(rr["matchRules"]) {
				mr := objOf(m)
				match := &routev3.RouteMatch{}
				switch {
				case str(mr, "fullPathMatch") != "":
					match.PathSpecifier = &routev3.RouteMatch_Path{Path: str(mr, "fullPathMatch")}
				case str(mr, "regexMatch") != "":
					match.PathSpecifier = &routev3.RouteMatch_SafeRegex{SafeRegex: &matcherv3.RegexMatcher{Regex: str(mr, "regexMatch")}}
				default:
					match.PathSpecifier = &routev3.RouteMatch_Prefix{Prefix: str(mr, "prefixMatch")}
				}
				if mr["ignoreCase"] == true {
					match.CaseSensitive = wrapperspb.Bool(false)
				}
				for _, he := range listOf(mr["headerMatches"]) {
					hm := objOf(he)
					h := &routev3.HeaderMatcher{Name: str(hm, "headerName"), InvertMatch: hm["invertMatch"] == true}
					switch {
					case str(hm, "exactMatch") != "":
						h.HeaderMatchSpecifier = &routev3.HeaderMatcher_StringMatch{StringMatch: &matcherv3.StringMatcher{MatchPattern: &matcherv3.StringMatcher_Exact{Exact: str(hm, "exactMatch")}}}
					case str(hm, "prefixMatch") != "":
						h.HeaderMatchSpecifier = &routev3.HeaderMatcher_StringMatch{StringMatch: &matcherv3.StringMatcher{MatchPattern: &matcherv3.StringMatcher_Prefix{Prefix: str(hm, "prefixMatch")}}}
					case str(hm, "suffixMatch") != "":
						h.HeaderMatchSpecifier = &routev3.HeaderMatcher_StringMatch{StringMatch: &matcherv3.StringMatcher{MatchPattern: &matcherv3.StringMatcher_Suffix{Suffix: str(hm, "suffixMatch")}}}
					case str(hm, "regexMatch") != "":
						h.HeaderMatchSpecifier = &routev3.HeaderMatcher_SafeRegexMatch{SafeRegexMatch: &matcherv3.RegexMatcher{Regex: str(hm, "regexMatch")}}
					case hm["presentMatch"] != nil:
						h.HeaderMatchSpecifier = &routev3.HeaderMatcher_PresentMatch{PresentMatch: hm["presentMatch"] == true}
					}
					match.Headers = append(match.Headers, h)
				}
				vh.Routes = append(vh.Routes, &routev3.Route{Match: match, Action: action})
			}
		}
		for _, e := range listOf(pm["pathRules"]) {
			pr := objOf(e)
			action := s.xdsActionLocked(str(pr, "service"), objOf(pr["routeAction"]))
			if action == nil {
				continue
			}
			for _, p := range strs(pr, "paths") {
				match := &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Path{Path: p}}
				if strings.HasSuffix(p, "*") {
					match.PathSpecifier = &routev3.RouteMatch_Prefix{Prefix: strings.TrimSuffix(p, "*")}
				}
				vh.Routes = append(vh.Routes, &routev3.Route{Match: match, Action: action})
			}
		}
	}
	if a := s.xdsActionLocked(def, nil); a != nil {
		vh.Routes = append(vh.Routes, &routev3.Route{Match: &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: ""}}, Action: a})
	}
	if len(vh.Routes) == 0 {
		return nil
	}
	return vh
}

// xdsActionLocked routes to a backend service, or to the route action's
// weighted backend services, with its timeout and retry policy.
func (s *Server) xdsActionLocked(service string, ra obj) *routev3.Route_Route {
	r := &routev3.RouteAction{}
	if w := listOf(ra["weightedBackendServices"]); len(w) > 0 {
		wc := &routev3.WeightedCluster{}
		for _, e := range w {
			b := s.res[Rel(str(objOf(e), "backendService"), "")]
			if b == nil {
				continue
			}
			wc.Clusters = append(wc.Clusters, &routev3.WeightedCluster_ClusterWeight{Name: xdsClusterName(b),
				Weight: wrapperspb.UInt32(uint32(numOr(objOf(e), "weight", 0)))})
		}
		if len(wc.Clusters) == 0 {
			return nil
		}
		r.ClusterSpecifier = &routev3.RouteAction_WeightedClusters{WeightedClusters: wc}
	} else {
		b := s.res[Rel(service, "")]
		if b == nil {
			return nil
		}
		r.ClusterSpecifier = &routev3.RouteAction_Cluster{Cluster: xdsClusterName(b)}
	}
	if t := objOf(ra["timeout"]); t != nil {
		sec, _ := strconv.ParseInt(fmt.Sprint(t["seconds"]), 10, 64)
		r.MaxStreamDuration = &routev3.RouteAction_MaxStreamDuration{MaxStreamDuration: durationpb.New(time.Duration(sec)*time.Second + time.Duration(numOr(t, "nanos", 0)))}
	}
	if rp := objOf(ra["retryPolicy"]); rp != nil {
		r.RetryPolicy = &routev3.RetryPolicy{RetryOn: strings.Join(strs(rp, "retryConditions"), ","),
			NumRetries: wrapperspb.UInt32(uint32(numOr(rp, "numRetries", 1)))}
	}
	return &routev3.Route_Route{Route: r}
}

// xdsBackendLocked is the backend service a cluster name stands for.
func (s *Server) xdsBackendLocked(name string) (string, obj) {
	for k, o := range s.res {
		if strings.Contains(k, "/global/backendServices/") && xdsClusterName(o) == name {
			return k, o
		}
	}
	return "", nil
}

func (s *Server) xdsClusterLocked(name string) *clusterv3.Cluster {
	_, bs := s.xdsBackendLocked(name)
	if bs == nil {
		return nil
	}
	lb := clusterv3.Cluster_ROUND_ROBIN
	switch str(bs, "localityLbPolicy") {
	case "RING_HASH":
		lb = clusterv3.Cluster_RING_HASH
	case "LEAST_REQUEST":
		lb = clusterv3.Cluster_LEAST_REQUEST
	}
	c := &clusterv3.Cluster{Name: name, ClusterDiscoveryType: &clusterv3.Cluster_Type{Type: clusterv3.Cluster_EDS},
		EdsClusterConfig: &clusterv3.Cluster_EdsClusterConfig{EdsConfig: &corev3.ConfigSource{
			ConfigSourceSpecifier: &corev3.ConfigSource_Ads{Ads: &corev3.AggregatedConfigSource{}}}},
		LbPolicy: lb}
	if str(bs, "localityLbPolicy") == "WEIGHTED_ROUND_ROBIN" {
		// The client weighs endpoints by the ORCA reports of its calls
		// (gRPC's weighted round robin), as Google's xDS API configures
		// it for WEIGHTED_ROUND_ROBIN.
		if wrr, err := anypb.New(&cswrrv3.ClientSideWeightedRoundRobin{}); err == nil {
			c.LbPolicy = clusterv3.Cluster_LOAD_BALANCING_POLICY_CONFIG
			c.LoadBalancingPolicy = &clusterv3.LoadBalancingPolicy{Policies: []*clusterv3.LoadBalancingPolicy_Policy{{
				TypedExtensionConfig: &corev3.TypedExtensionConfig{Name: "envoy.load_balancing_policies.client_side_weighted_round_robin", TypedConfig: wrr}}}}
		}
	}
	return c
}

// xdsEndpointsLocked is a cluster's endpoints, by zone, healthy ones
// HEALTHY and the rest UNHEALTHY.
func (s *Server) xdsEndpointsLocked(name string) *endpointv3.ClusterLoadAssignment {
	rel, _ := s.xdsBackendLocked(name)
	if rel == "" {
		return nil
	}
	set, ok := s.backendSetLocked(rel)
	if !ok {
		return nil
	}
	by := map[string][]*endpointv3.LbEndpoint{}
	for _, g := range set.groups {
		zone := ""
		if p := strings.Split(Rel(str(g.backend, "group"), ""), "/"); len(p) > 3 && p[2] == "zones" {
			zone = p[3]
		}
		healthy := map[string]bool{}
		for _, h := range g.healthy {
			healthy[h.id] = true
		}
		for _, ep := range g.endpoints {
			ip := ep.ip
			if ip == "" && ep.instance != "" {
				ip = str(objOf(firstOf(s.res[ep.instance]["networkInterfaces"])), "networkIP")
			}
			if ip == "" || ep.port == 0 {
				continue
			}
			hs := corev3.HealthStatus_HEALTHY
			if !healthy[ep.id] {
				hs = corev3.HealthStatus_UNHEALTHY
			}
			by[zone] = append(by[zone], &endpointv3.LbEndpoint{HealthStatus: hs, HostIdentifier: &endpointv3.LbEndpoint_Endpoint{Endpoint: &endpointv3.Endpoint{
				Address: &corev3.Address{Address: &corev3.Address_SocketAddress{SocketAddress: &corev3.SocketAddress{Address: ip,
					PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: uint32(ep.port)}}}}}}})
		}
	}
	cla := &endpointv3.ClusterLoadAssignment{ClusterName: name}
	zones := make([]string, 0, len(by))
	for z := range by {
		zones = append(zones, z)
	}
	sort.Strings(zones)
	for _, z := range zones {
		cla.Endpoints = append(cla.Endpoints, &endpointv3.LocalityLbEndpoints{Locality: &corev3.Locality{Zone: z},
			LbEndpoints: by[z], LoadBalancingWeight: wrapperspb.UInt32(100)})
	}
	return cla
}

func firstOf(v any) any {
	if l := listOf(v); len(l) > 0 {
		return l[0]
	}
	return nil
}
