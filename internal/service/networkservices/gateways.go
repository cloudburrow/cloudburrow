package networkservices

import (
	"context"
	"encoding/binary"
	"net"
	"net/textproto"
	"regexp"
	"sort"
	"strings"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	nsvpb "cloud.google.com/go/networkservices/apiv1/networkservicespb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// --- Gateways --------------------------------------------------------------

// maxSWGPorts and maxSWGPerRegion are Secure Web Proxy's limits: "Gateways
// of type 'SECURE_WEB_GATEWAY' are limited to 5 ports" (the Gateway
// reference) and 5 instances per project per region (secure-web-proxy/docs/quotas).
const (
	maxSWGPorts     = 5
	maxSWGPerRegion = 5
)

var (
	scopeRE   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,63}$`)
	networkRE = regexp.MustCompile(`^projects/[^/]+/global/networks/[^/]+$`)
	subnetRE  = regexp.MustCompile(`^projects/[^/]+/regions/([^/]+)/subnetworks/[^/]+$`)
	gspRE     = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/gatewaySecurityPolicies/[^/]+$`)
	stpRE     = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/serverTlsPolicies/[^/]+$`)
	certRE    = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/certificates/[^/]+$`)
)

// relRef is a resource reference as its relative name: a full URL
// (https://www.googleapis.com/compute/v1/projects/...,
// //networksecurity.googleapis.com/projects/...) or the name itself.
func relRef(ref string) string {
	if i := strings.Index(ref, "projects/"); i >= 0 {
		return ref[i:]
	}
	return ref
}

func (s *Server) checkGateway(m proto.Message, name string) error {
	g := m.(*nsvpb.Gateway)
	loc := locationOf(name)
	h := s.hooksNow()
	if len(g.GetDescription()) > 1024 {
		return apierror.InvalidArgument("Invalid value for field 'description': it must be at most 1024 characters long.")
	}
	if g.GetScope() != "" && !scopeRE.MatchString(g.GetScope()) {
		return apierror.InvalidArgument("Invalid value for field 'scope': '%s'. It should start with a letter, can only have letters, numbers and hyphens, and be at most 64 characters long.", g.GetScope())
	}
	if g.GetServerTlsPolicy() != "" {
		ref := relRef(g.GetServerTlsPolicy())
		if !stpRE.MatchString(ref) {
			return apierror.InvalidArgument("Invalid value for field 'server_tls_policy': '%s'. It must be projects/{project}/locations/{location}/serverTlsPolicies/{server_tls_policy}.", g.GetServerTlsPolicy())
		}
		if h.ServerTLSPolicyExists != nil && !h.ServerTLSPolicyExists(ref) {
			return apierror.NotFound("ServerTlsPolicy '%s' not found.", ref)
		}
	}
	for _, p := range g.GetPorts() {
		if p < 1 || p > 65535 {
			return apierror.InvalidArgument("Invalid value for field 'ports': %d. Ports must be between 1 and 65535.", p)
		}
	}
	switch g.GetType() {
	case nsvpb.Gateway_SECURE_WEB_GATEWAY:
		return s.checkSWG(g, name, h)
	case nsvpb.Gateway_OPEN_MESH:
		if loc != "global" {
			return apierror.InvalidArgument("Gateways of type OPEN_MESH must be in the location 'global', not '%s'.", loc)
		}
		if g.GetScope() == "" {
			return apierror.InvalidArgument("Invalid argument: scope is required for gateways of type OPEN_MESH.")
		}
		if len(g.GetPorts()) == 0 {
			return apierror.InvalidArgument("Invalid argument: ports is required.")
		}
		for _, f := range []struct {
			set  bool
			name string
		}{{len(g.GetAddresses()) > 0, "addresses"}, {g.GetAllPorts(), "all_ports"}, {len(g.GetCertificateUrls()) > 0, "certificate_urls"},
			{g.GetGatewaySecurityPolicy() != "", "gateway_security_policy"}, {g.GetNetwork() != "", "network"}, {g.GetSubnetwork() != "", "subnetwork"},
			{g.GetRoutingMode() != nsvpb.Gateway_EXPLICIT_ROUTING_MODE, "routing_mode"}, {g.GetAllowGlobalAccess(), "allow_global_access"}} {
			if f.set {
				return apierror.InvalidArgument("Invalid argument: %s applies only to gateways of type SECURE_WEB_GATEWAY.", f.name)
			}
		}
		return nil
	}
	return apierror.InvalidArgument("Invalid argument: type is required: OPEN_MESH or SECURE_WEB_GATEWAY.")
}

func (s *Server) checkSWG(g *nsvpb.Gateway, name string, h Hooks) error {
	loc := locationOf(name)
	if loc == "global" {
		return apierror.InvalidArgument("Gateways of type SECURE_WEB_GATEWAY must be in a region, not in the location 'global'.")
	}
	if g.GetAllPorts() && len(g.GetPorts()) > 0 {
		return apierror.InvalidArgument("Invalid argument: ports and all_ports are mutually exclusive.")
	}
	if !g.GetAllPorts() && len(g.GetPorts()) == 0 {
		return apierror.InvalidArgument("Invalid argument: ports is required.")
	}
	if len(g.GetPorts()) > maxSWGPorts {
		return apierror.InvalidArgument("Invalid value for field 'ports': gateways of type SECURE_WEB_GATEWAY are limited to %d ports.", maxSWGPorts)
	}
	if len(g.GetAddresses()) > 1 {
		return apierror.InvalidArgument("Invalid value for field 'addresses': a gateway receives traffic on zero or one address.")
	}
	for _, a := range g.GetAddresses() {
		ip := net.ParseIP(a)
		if ip == nil {
			return apierror.InvalidArgument("Invalid value for field 'addresses': '%s' is not an IP address.", a)
		}
		if ip.To4() == nil {
			return apierror.InvalidArgument("Invalid value for field 'addresses': '%s'. Secure Web Proxy supports IPv4 only.", a)
		}
	}
	if g.GetIpVersion() == nsvpb.Gateway_IPV6 {
		return apierror.InvalidArgument("Invalid value for field 'ip_version': IPV6. Secure Web Proxy supports IPv4 only.")
	}
	if g.GetNetwork() == "" {
		return apierror.InvalidArgument("Invalid argument: network is required for gateways of type SECURE_WEB_GATEWAY.")
	}
	netw := relRef(g.GetNetwork())
	if !networkRE.MatchString(netw) {
		return apierror.InvalidArgument("Invalid value for field 'network': '%s'. It must be projects/{project}/global/networks/{network}.", g.GetNetwork())
	}
	if h.Network != nil && !h.Network(netw) {
		return apierror.NotFound("The network '%s' was not found.", netw)
	}
	if g.GetSubnetwork() == "" {
		return apierror.InvalidArgument("Invalid argument: subnetwork is required for gateways of type SECURE_WEB_GATEWAY.")
	}
	sub := relRef(g.GetSubnetwork())
	mm := subnetRE.FindStringSubmatch(sub)
	if mm == nil {
		return apierror.InvalidArgument("Invalid value for field 'subnetwork': '%s'. It must be projects/{project}/regions/{region}/subnetworks/{subnetwork}.", g.GetSubnetwork())
	}
	if mm[1] != loc {
		return apierror.InvalidArgument("Invalid value for field 'subnetwork': '%s'. The subnetwork must be in the gateway's region '%s'.", g.GetSubnetwork(), loc)
	}
	if h.Subnetwork != nil {
		n, ok := h.Subnetwork(sub)
		if !ok {
			return apierror.NotFound("The subnetwork '%s' was not found.", sub)
		}
		if relRef(n) != netw {
			return apierror.InvalidArgument("Invalid value for field 'subnetwork': '%s' is not in the network '%s'.", sub, netw)
		}
	}
	if h.ProxyOnlySubnet != nil && !h.ProxyOnlySubnet(netw, loc) {
		return apierror.FailedPrecondition("The network '%s' has no active proxy-only subnet (purpose REGIONAL_MANAGED_PROXY) in the region '%s', which Secure Web Proxy needs.", netw, loc)
	}
	if p := g.GetGatewaySecurityPolicy(); p != "" {
		ref := relRef(p)
		m := gspRE.FindStringSubmatch(ref)
		if m == nil {
			return apierror.InvalidArgument("Invalid value for field 'gateway_security_policy': '%s'. It must be projects/{project}/locations/{location}/gatewaySecurityPolicies/{gateway_security_policy}.", p)
		}
		if m[2] != loc {
			return apierror.InvalidArgument("Invalid value for field 'gateway_security_policy': '%s'. The policy must be in the gateway's region '%s'.", p, loc)
		}
		if h.GatewaySecurityPolicyExists != nil && !h.GatewaySecurityPolicyExists(ref) {
			return apierror.NotFound("GatewaySecurityPolicy '%s' not found.", ref)
		}
	}
	for i, c := range g.GetCertificateUrls() {
		ref := relRef(c)
		m := certRE.FindStringSubmatch(ref)
		if m == nil {
			return apierror.InvalidArgument("Invalid value for field 'certificate_urls[%d]': '%s'. It must be projects/{project}/locations/{location}/certificates/{certificate}.", i, c)
		}
		if m[2] != loc {
			return apierror.InvalidArgument("Invalid value for field 'certificate_urls[%d]': '%s'. The certificate must be in the gateway's region '%s'.", i, c, loc)
		}
		if h.CertificateExists != nil && !h.CertificateExists(ref) {
			return apierror.NotFound("Certificate '%s' not found.", ref)
		}
	}
	return nil
}

// allocAddressLocked gives an SWG gateway without an address one from its
// subnetwork, as "When no address is provided, an IP from the subnetwork is
// allocated": the highest host address no other gateway has, of the range
// the hook reports.
func (s *Server) allocAddressLocked(g *nsvpb.Gateway, cidr string) {
	_, n, err := net.ParseCIDR(cidr)
	if err != nil || n.IP.To4() == nil {
		return
	}
	used := map[string]bool{}
	for _, m := range s.res {
		if o, ok := m.(*nsvpb.Gateway); ok {
			for _, a := range o.GetAddresses() {
				used[a] = true
			}
		}
	}
	base := binary.BigEndian.Uint32(n.IP.To4())
	ones, bits := n.Mask.Size()
	size := uint32(1) << uint(bits-ones)
	for off := size - 2; off >= 2 && off < size; off-- {
		ip := make(net.IP, 4)
		binary.BigEndian.PutUint32(ip, base+off)
		if !used[ip.String()] {
			g.Addresses = []string{ip.String()}
			return
		}
	}
}

// SubnetRange is set by the wiring: a subnetwork's primary range, for an
// address allocated to a gateway.
type SubnetRange func(rel string) (cidr string, ok bool)

// UseSubnetRange sets how a gateway's address is allocated.
func (s *Server) UseSubnetRange(f SubnetRange) {
	s.mu.Lock()
	s.subnetRange = f
	s.mu.Unlock()
}

func (s *Server) CreateGateway(_ context.Context, req *nsvpb.CreateGatewayRequest) (*longrunningpb.Operation, error) {
	g, _ := proto.Clone(req.GetGateway()).(*nsvpb.Gateway)
	if g == nil {
		g = &nsvpb.Gateway{}
	}
	g.SelfLink = ""
	if g.GetType() == nsvpb.Gateway_SECURE_WEB_GATEWAY {
		if g.GetIpVersion() == nsvpb.Gateway_IP_VERSION_UNSPECIFIED {
			g.IpVersion = nsvpb.Gateway_IPV4
		}
		project, loc, err := parseParent(req.GetParent())
		if err == nil && loc != "global" {
			s.mu.Lock()
			n := 0
			for k, m := range s.res {
				if o, ok := m.(*nsvpb.Gateway); ok && o.GetType() == nsvpb.Gateway_SECURE_WEB_GATEWAY && projectOf(k) == project && locationOf(k) == loc {
					n++
				}
			}
			if len(g.GetAddresses()) == 0 && s.subnetRange != nil && g.GetSubnetwork() != "" {
				if cidr, ok := s.subnetRange(relRef(g.GetSubnetwork())); ok {
					s.allocAddressLocked(g, cidr)
				}
			}
			s.mu.Unlock()
			if n >= maxSWGPerRegion {
				return nil, status.Errorf(codes.ResourceExhausted, "Quota exceeded for quota metric 'Secure Web Proxy instances' and limit 'Secure Web Proxy instances per project per region' of service 'networkservices.googleapis.com' for consumer 'project:%s': %d.", project, maxSWGPerRegion)
			}
		}
	}
	return s.create(req.GetParent(), collGateways, "gateway_id", req.GetGatewayId(), g, func(m proto.Message, name string) error {
		m.(*nsvpb.Gateway).SelfLink = "https://networkservices.googleapis.com/v1/" + name
		return s.checkGateway(m, name)
	})
}

func (s *Server) GetGateway(_ context.Context, req *nsvpb.GetGatewayRequest) (*nsvpb.Gateway, error) {
	m, err := s.get(collGateways, req.GetName())
	if err != nil {
		return nil, err
	}
	return m.(*nsvpb.Gateway), nil
}

func (s *Server) ListGateways(_ context.Context, req *nsvpb.ListGatewaysRequest) (*nsvpb.ListGatewaysResponse, error) {
	l, err := s.list(req.GetParent(), collGateways)
	if err != nil {
		return nil, err
	}
	items := make([]*nsvpb.Gateway, len(l))
	for i, m := range l {
		items[i] = m.(*nsvpb.Gateway)
	}
	page, next, err := aipfilter.ListPage("gw:"+req.GetParent(), "", "", items, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, err
	}
	return &nsvpb.ListGatewaysResponse{Gateways: page, NextPageToken: next}, nil
}

// gatewayUpdatable are the fields an update may change; the rest
// (type, ports, addresses, network, subnetwork, routing mode, global
// access) are immutable ("Ports ... not changeable later").
var gatewayUpdatable = []string{"description", "labels", "server_tls_policy", "certificate_urls", "gateway_security_policy", "envoy_headers"}

func (s *Server) UpdateGateway(_ context.Context, req *nsvpb.UpdateGatewayRequest) (*longrunningpb.Operation, error) {
	return s.update(collGateways, req.GetGateway(), req.GetUpdateMask(), gatewayUpdatable, s.checkGateway)
}

func (s *Server) DeleteGateway(_ context.Context, req *nsvpb.DeleteGatewayRequest) (*longrunningpb.Operation, error) {
	return s.delete(collGateways, req.GetName())
}

// --- Authorization extensions ---------------------------------------------

var bsRE = regexp.MustCompile(`^projects/[^/]+/(global|regions/([a-z0-9-]+))/backendServices/[^/]+$`)

func (s *Server) checkAuthzExt(m proto.Message, name string) error {
	e := m.(*nsvpb.AuthzExtension)
	loc := locationOf(name)
	if e.GetService() == "" {
		return apierror.InvalidArgument("Invalid argument: service is required.")
	}
	if len(e.GetDescription()) > 1024 {
		return apierror.InvalidArgument("Invalid value for field 'description': it must be at most 1024 characters long.")
	}
	if strings.Contains(e.GetService(), "/backendServices/") {
		ref := relRef(e.GetService())
		mm := bsRE.FindStringSubmatch(ref)
		if mm == nil {
			return apierror.InvalidArgument("Invalid value for field 'service': '%s'. It must be a backend service: https://www.googleapis.com/compute/v1/projects/{project}/regions/{region}/backendServices/{backendService} or .../global/backendServices/{backendService}.", e.GetService())
		}
		if (loc == "global") != (mm[1] == "global") || loc != "global" && mm[2] != loc {
			return apierror.InvalidArgument("Invalid value for field 'service': '%s'. The backend service must be in the extension's location '%s'.", e.GetService(), loc)
		}
		if e.GetAuthority() == "" {
			return apierror.InvalidArgument("Invalid argument: authority is required when service points to a backend service.")
		}
		if e.GetLoadBalancingScheme() == nsvpb.LoadBalancingScheme_LOAD_BALANCING_SCHEME_UNSPECIFIED {
			return apierror.InvalidArgument("Invalid argument: load_balancing_scheme is required when service points to a backend service: INTERNAL_MANAGED or EXTERNAL_MANAGED.")
		}
		if f := s.hooksNow().BackendService; f != nil {
			sch, ok := f(ref)
			if !ok {
				return apierror.NotFound("The backend service '%s' was not found.", ref)
			}
			if sch != e.GetLoadBalancingScheme().String() {
				return apierror.InvalidArgument("Invalid value for field 'service': '%s'. Its load balancing scheme %s is not the extension's %s.", ref, sch, e.GetLoadBalancingScheme())
			}
		}
	}
	if e.GetTimeout() == nil {
		return apierror.InvalidArgument("Invalid argument: timeout is required.")
	}
	if d := e.GetTimeout().AsDuration(); d < 10*time.Millisecond || d > 10*time.Second {
		return apierror.InvalidArgument("Invalid value for field 'timeout': %s. The timeout must be between 10-10000 milliseconds.", d)
	}
	if loc == "global" && e.GetWireFormat() != nsvpb.WireFormat_EXT_PROC_GRPC && e.GetWireFormat() != nsvpb.WireFormat_WIRE_FORMAT_UNSPECIFIED {
		return apierror.InvalidArgument("Invalid value for field 'wire_format': %s. Global AuthzExtension resources use the EXT_PROC_GRPC wire format.", e.GetWireFormat())
	}
	for i, h := range e.GetForwardHeaders() {
		if h == "" || textproto.CanonicalMIMEHeaderKey(h) == "" || strings.ContainsAny(h, " :\t\r\n") {
			return apierror.InvalidArgument("Invalid value for field 'forward_headers[%d]': '%s' is not an HTTP header name.", i, h)
		}
	}
	return nil
}

func (s *Server) CreateAuthzExtension(_ context.Context, req *nsvpb.CreateAuthzExtensionRequest) (*longrunningpb.Operation, error) {
	e, _ := proto.Clone(req.GetAuthzExtension()).(*nsvpb.AuthzExtension)
	if e == nil {
		e = &nsvpb.AuthzExtension{}
	}
	if e.GetWireFormat() == nsvpb.WireFormat_WIRE_FORMAT_UNSPECIFIED {
		e.WireFormat = nsvpb.WireFormat_EXT_PROC_GRPC
	}
	return s.create(req.GetParent(), collAuthzExt, "authz_extension_id", req.GetAuthzExtensionId(), e, s.checkAuthzExt)
}

func (s *Server) GetAuthzExtension(_ context.Context, req *nsvpb.GetAuthzExtensionRequest) (*nsvpb.AuthzExtension, error) {
	m, err := s.get(collAuthzExt, req.GetName())
	if err != nil {
		return nil, err
	}
	return m.(*nsvpb.AuthzExtension), nil
}

func (s *Server) ListAuthzExtensions(_ context.Context, req *nsvpb.ListAuthzExtensionsRequest) (*nsvpb.ListAuthzExtensionsResponse, error) {
	l, err := s.list(req.GetParent(), collAuthzExt)
	if err != nil {
		return nil, err
	}
	items := make([]*nsvpb.AuthzExtension, len(l))
	for i, m := range l {
		items[i] = m.(*nsvpb.AuthzExtension)
	}
	page, next, err := aipfilter.ListPage("aze:"+req.GetParent(), req.GetFilter(), req.GetOrderBy(), items, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, err
	}
	return &nsvpb.ListAuthzExtensionsResponse{AuthzExtensions: page, NextPageToken: next}, nil
}

func (s *Server) UpdateAuthzExtension(_ context.Context, req *nsvpb.UpdateAuthzExtensionRequest) (*longrunningpb.Operation, error) {
	return s.update(collAuthzExt, req.GetAuthzExtension(), req.GetUpdateMask(),
		[]string{"description", "labels", "authority", "service", "timeout", "fail_open", "metadata", "forward_headers", "forward_attributes", "wire_format"},
		s.checkAuthzExt)
}

func (s *Server) DeleteAuthzExtension(_ context.Context, req *nsvpb.DeleteAuthzExtensionRequest) (*longrunningpb.Operation, error) {
	return s.delete(collAuthzExt, req.GetName())
}

// AuthzExtensionInfo is an authorization extension as an authorization
// policy's CUSTOM action calls it.
type AuthzExtensionInfo struct {
	Name string
	// Service is the callout's backend service, as its relative name
	// (projects/P/regions/R/backendServices/B), or the service as given
	// when it is no backend service.
	Service             string
	Authority           string
	LoadBalancingScheme string
	// WireFormat is EXT_PROC_GRPC (the default) or EXT_AUTHZ_GRPC.
	WireFormat        string
	FailOpen          bool
	Timeout           time.Duration
	ForwardHeaders    []string
	ForwardAttributes []string
	// Metadata is the extension's metadata, as JSON-like values, for the
	// metadata_context namespace com.google.authz_extension.<name>.
	Metadata map[string]any
}

// AuthzExtension is an authorization extension by name
// (projects/P/locations/L/authzExtensions/X, or a full URL ending so).
func (s *Server) AuthzExtension(name string) (AuthzExtensionInfo, bool) {
	name = relRef(name)
	s.mu.Lock()
	m, ok := s.res[name]
	s.mu.Unlock()
	e, _ := m.(*nsvpb.AuthzExtension)
	if !ok || e == nil {
		return AuthzExtensionInfo{}, false
	}
	svc := e.GetService()
	if strings.Contains(svc, "/backendServices/") {
		svc = relRef(svc)
	}
	return AuthzExtensionInfo{Name: name, Service: svc, Authority: e.GetAuthority(), LoadBalancingScheme: e.GetLoadBalancingScheme().String(),
		WireFormat: e.GetWireFormat().String(), FailOpen: e.GetFailOpen(), Timeout: e.GetTimeout().AsDuration(),
		ForwardHeaders: append([]string(nil), e.GetForwardHeaders()...), ForwardAttributes: append([]string(nil), e.GetForwardAttributes()...),
		Metadata: e.GetMetadata().AsMap()}, true
}

// AuthzExtensionExists is whether an authorization extension exists.
func (s *Server) AuthzExtensionExists(name string) bool {
	_, ok := s.AuthzExtension(name)
	return ok
}

// GatewaySecurityPolicyUsers are the gateways that name a gateway
// security policy.
func (s *Server) GatewaySecurityPolicyUsers(policy string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k, m := range s.res {
		if g, ok := m.(*nsvpb.Gateway); ok && relRef(g.GetGatewaySecurityPolicy()) == policy {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
