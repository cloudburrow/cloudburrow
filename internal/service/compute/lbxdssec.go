package compute

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	routerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	matcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// Proxyless gRPC security (#2160), as Cloud Service Mesh's "Set up service
// security with proxyless gRPC" (service-mesh/docs/service-routing/
// security-proxyless-setup) and gRPC's xDS-based security (gRFC A29) and
// xDS bootstrap credentials (gRFC A65) describe it (read 2026-11-22):
//
//   - the bootstrap names a certificate provider instance,
//     google_cloud_private_spiffe, of the file_watcher plugin
//     (certificate_file, private_key_file, ca_certificate_file,
//     refresh_interval 600s); a client TLS policy's clientCertificate and
//     serverValidationCa and a server TLS policy's serverCertificate and
//     mtlsPolicy.clientValidationCa name it as a certificateProviderInstance
//     pluginInstance;
//   - a backend service's securitySettings.clientTlsPolicy (and
//     subjectAltNames) reaches the client as its cluster's
//     UpstreamTlsContext: the identity certificate and the CA of the
//     named instances, and the server's SAN that must match;
//   - an endpoint policy of type GRPC_SERVER whose endpointMatcher takes
//     the server's node metadata labels and whose trafficPortSelector its
//     port puts its serverTlsPolicy on the server's listener
//     (grpc/server?xds.resource.listening_address=IP:PORT, the
//     bootstrap's server_listener_resource_name_template) as a
//     DownstreamTlsContext, requiring a client certificate under mTLS; a
//     server no endpoint policy takes gets a plaintext listener, so it
//     serves;
//   - the xDS API itself is reached over TLS: Google's is
//     trafficdirector.googleapis.com:443 with google_default channel
//     credentials; here the bootstrap's channel_creds are of type tls
//     (ca_certificate_file naming this instance's CA), and the Compute
//     Engine port takes the TLS handshake beside its plaintext HTTP
//     (XDSServerTLS), so a client given insecure credentials still works.
//
// The workload credentials the file_watcher plugin reads are issued here
// from the load balancing CA (WriteXDSCredentials): one SPIFFE identity
// per project (spiffe://PROJECT.svc.id.goog/ns/default/sa/default, the
// form the page's subjectAltNames use), where Google's come from the
// workload's CA Service pool.

// XDSCertProviderInstance is the certificate provider instance Google's
// bootstrap generator writes.
const XDSCertProviderInstance = "google_cloud_private_spiffe"

// xdsServerListenerPrefix is the name of a server's listener before its
// listening address.
const xdsServerListenerPrefix = "grpc/server?xds.resource.listening_address="

// XDSPolicies are the Network Security and Network Services policies the
// xDS API delivers to proxyless gRPC. The NetSec option may implement it;
// without it, no security is delivered. Its methods are called with the
// Compute Engine API's lock held, so they must not call back into it.
type XDSPolicies interface {
	// XDSClientTLS is a client TLS policy, by name; ok is false when there
	// is none.
	XDSClientTLS(name string) (XDSClientTLS, bool)
	// XDSServerTLS is a server TLS policy, by name.
	XDSServerTLS(name string) (XDSServerTLS, bool)
	// XDSEndpointPolicies are a project's endpoint policies.
	XDSEndpointPolicies(project string) []XDSEndpointPolicy
}

// XDSClientTLS is a client TLS policy as a proxyless client applies it.
type XDSClientTLS struct {
	// CertInstance is clientCertificate's plugin instance, "" for none
	// (TLS without a client certificate).
	CertInstance string
	// CAInstances are serverValidationCa's plugin instances.
	CAInstances []string
	SNI         string
}

// XDSServerTLS is a server TLS policy as a proxyless server applies it.
type XDSServerTLS struct {
	// AllowOpen is allowOpen: plaintext is accepted beside TLS.
	AllowOpen bool
	// CertInstance is serverCertificate's plugin instance, "" for none.
	CertInstance string
	// MTLS is whether mtlsPolicy is set; CAInstances are its
	// clientValidationCa's plugin instances.
	MTLS        bool
	CAInstances []string
}

// XDSEndpointPolicy is an endpoint policy (networkservices
// endpointPolicies).
type XDSEndpointPolicy struct {
	Name string
	// Type is GRPC_SERVER or SIDECAR_PROXY; gRPC ignores SIDECAR_PROXY.
	Type string
	// MatchAll is metadataLabelMatchCriteria MATCH_ALL (else MATCH_ANY).
	MatchAll bool
	Labels   map[string]string
	// Ports are trafficPortSelector's ports; none takes every port.
	Ports           []string
	ServerTLSPolicy string
}

func (s *Server) xdsPolicies() XDSPolicies {
	p, _ := s.opts.NetSec.(XDSPolicies)
	return p
}

func providerInstance(name string) *tlsv3.CertificateProviderPluginInstance {
	return &tlsv3.CertificateProviderPluginInstance{InstanceName: name, CertificateName: "DEFAULT"}
}

// xdsUpstreamTLSLocked is a backend service's cluster TLS, nil without a
// client TLS policy.
func (s *Server) xdsUpstreamTLSLocked(bs obj) *corev3.TransportSocket {
	ss := objOf(bs["securitySettings"])
	ref := str(ss, "clientTlsPolicy")
	pol := s.xdsPolicies()
	if ref == "" || pol == nil {
		return nil
	}
	ct, ok := pol.XDSClientTLS(strings.TrimPrefix(ref, "//networksecurity.googleapis.com/"))
	if !ok {
		return nil
	}
	common := &tlsv3.CommonTlsContext{}
	if ct.CertInstance != "" {
		common.TlsCertificateProviderInstance = providerInstance(ct.CertInstance)
	}
	if len(ct.CAInstances) > 0 {
		vc := &tlsv3.CertificateValidationContext{CaCertificateProviderInstance: providerInstance(ct.CAInstances[0])}
		for _, san := range strs(ss, "subjectAltNames") {
			vc.MatchSubjectAltNames = append(vc.MatchSubjectAltNames, &matcherv3.StringMatcher{MatchPattern: &matcherv3.StringMatcher_Exact{Exact: san}})
		}
		common.ValidationContextType = &tlsv3.CommonTlsContext_ValidationContext{ValidationContext: vc}
	}
	a, err := anypb.New(&tlsv3.UpstreamTlsContext{CommonTlsContext: common, Sni: ct.SNI})
	if err != nil {
		return nil
	}
	return &corev3.TransportSocket{Name: "envoy.transport_sockets.tls", ConfigType: &corev3.TransportSocket_TypedConfig{TypedConfig: a}}
}

// nodeLabels are a node's metadata as endpoint policies match them.
func nodeLabels(node *corev3.Node) map[string]string {
	out := map[string]string{}
	for k, v := range node.GetMetadata().GetFields() {
		if sv, ok := v.GetKind().(*structpb.Value_StringValue); ok {
			out[k] = sv.StringValue
		}
	}
	return out
}

// endpointPolicyFor is the endpoint policy of a GRPC_SERVER that takes a
// server with labels on port: the first by name among the matches.
func endpointPolicyFor(eps []XDSEndpointPolicy, labels map[string]string, port string) (XDSEndpointPolicy, bool) {
	sort.Slice(eps, func(i, j int) bool { return eps[i].Name < eps[j].Name })
	for _, ep := range eps {
		if ep.Type != "GRPC_SERVER" {
			continue
		}
		if len(ep.Ports) > 0 {
			ok := false
			for _, p := range ep.Ports {
				if p == port || p == "*" {
					ok = true
				}
			}
			if !ok {
				continue
			}
		}
		n := 0
		for k, v := range ep.Labels {
			if labels[k] == v {
				n++
			}
		}
		if len(ep.Labels) > 0 && (ep.MatchAll && n != len(ep.Labels) || !ep.MatchAll && n == 0) {
			continue
		}
		return ep, true
	}
	return XDSEndpointPolicy{}, false
}

// xdsServerListenerLocked is the listener of an xDS-enabled gRPC server:
// its own address, routes that take every call to the server, and the TLS
// of the endpoint policy that takes it.
func (s *Server) xdsServerListenerLocked(name string, node *corev3.Node) *listenerv3.Listener {
	addr := strings.TrimPrefix(name, xdsServerListenerPrefix)
	host, ps, err := net.SplitHostPort(addr)
	if err != nil {
		return nil
	}
	port, err := strconv.Atoi(ps)
	if err != nil {
		return nil
	}
	rc := &routev3.RouteConfiguration{Name: "inbound|" + addr, VirtualHosts: []*routev3.VirtualHost{{Name: "inbound", Domains: []string{"*"},
		Routes: []*routev3.Route{{Match: &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
			Action: &routev3.Route_NonForwardingAction{NonForwardingAction: &routev3.NonForwardingAction{}}}}}}}
	router, _ := anypb.New(&routerv3.Router{})
	hcm, err := anypb.New(&hcmv3.HttpConnectionManager{
		RouteSpecifier: &hcmv3.HttpConnectionManager_RouteConfig{RouteConfig: rc},
		HttpFilters:    []*hcmv3.HttpFilter{{Name: "envoy.filters.http.router", ConfigType: &hcmv3.HttpFilter_TypedConfig{TypedConfig: router}}},
	})
	if err != nil {
		return nil
	}
	fc := &listenerv3.FilterChain{Name: "inbound", Filters: []*listenerv3.Filter{{Name: "envoy.filters.network.http_connection_manager",
		ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: hcm}}}}
	if pol := s.xdsPolicies(); pol != nil {
		project := ""
		if p := strings.Split(node.GetId(), "/"); len(p) > 1 && p[0] == "projects" {
			project = p[1]
		}
		if ep, ok := endpointPolicyFor(pol.XDSEndpointPolicies(project), nodeLabels(node), ps); ok && ep.ServerTLSPolicy != "" {
			if st, ok := pol.XDSServerTLS(strings.TrimPrefix(ep.ServerTLSPolicy, "//networksecurity.googleapis.com/")); ok && st.CertInstance != "" {
				common := &tlsv3.CommonTlsContext{TlsCertificateProviderInstance: providerInstance(st.CertInstance)}
				dt := &tlsv3.DownstreamTlsContext{CommonTlsContext: common}
				if st.MTLS && len(st.CAInstances) > 0 {
					common.ValidationContextType = &tlsv3.CommonTlsContext_ValidationContext{ValidationContext: &tlsv3.CertificateValidationContext{
						CaCertificateProviderInstance: providerInstance(st.CAInstances[0])}}
					dt.RequireClientCertificate = wrapperspb.Bool(true)
				}
				if a, err := anypb.New(dt); err == nil {
					fc.TransportSocket = &corev3.TransportSocket{Name: "envoy.transport_sockets.tls", ConfigType: &corev3.TransportSocket_TypedConfig{TypedConfig: a}}
				}
			}
		}
	}
	return &listenerv3.Listener{Name: name,
		Address: &corev3.Address{Address: &corev3.Address_SocketAddress{SocketAddress: &corev3.SocketAddress{Address: host,
			PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: uint32(port)}}}},
		FilterChains: []*listenerv3.FilterChain{fc}}
}

// XDSFiles are the credential files a TLS bootstrap names.
type XDSFiles struct {
	// CA is the CA that signed the xDS server's certificate
	// (channel_creds' ca_certificate_file).
	CA string
	// Certificate, PrivateKey and CACertificates are the workload's
	// credentials, which the file_watcher plugin reads.
	Certificate, PrivateKey, CACertificates string
}

// xdsWorkloadID is a project's workload identity.
func xdsWorkloadID(project string) string {
	return "spiffe://" + project + ".svc.id.goog/ns/default/sa/default"
}

// issue signs tpl with the load balancing CA, returning the certificate
// and key as PEM, and the CA's PEM.
func (s *Server) issueFromLBCA(tpl *x509.Certificate) (certPEM, keyPEM, caPEM string, err error) {
	s.mu.Lock()
	ca, caKey, caPEM, err := s.lbCALocked()
	if err == nil {
		err = s.saveLocked()
	}
	s.mu.Unlock()
	if err != nil {
		return "", "", "", err
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", "", err
	}
	tpl.SerialNumber = serial()
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &k.PublicKey, caKey)
	if err != nil {
		return "", "", "", err
	}
	kd, _ := x509.MarshalECPrivateKey(k)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd})), caPEM, nil
}

// xdsCredMu keeps two writers of the credential files apart.
var xdsCredMu sync.Mutex

// WriteXDSCredentials writes, in dir (once; kept while valid), the CA of
// the xDS server and a project's workload credentials, and answers their
// paths.
func (s *Server) WriteXDSCredentials(dir, project string) (XDSFiles, error) {
	if dir == "" || project == "" {
		return XDSFiles{}, errors.New("no credential directory or project")
	}
	wd := filepath.Join(dir, project, "workload-spiffe-credentials")
	f := XDSFiles{CA: filepath.Join(dir, "xds-ca.pem"), Certificate: filepath.Join(wd, "certificates.pem"),
		PrivateKey: filepath.Join(wd, "private_key.pem"), CACertificates: filepath.Join(wd, "ca_certificates.pem")}
	xdsCredMu.Lock()
	defer xdsCredMu.Unlock()
	if b, err := os.ReadFile(f.Certificate); err == nil {
		if blk, _ := pem.Decode(b); blk != nil {
			if c, err := x509.ParseCertificate(blk.Bytes); err == nil && time.Until(c.NotAfter) > 24*time.Hour {
				if _, err := os.Stat(f.CA); err == nil {
					return f, nil
				}
			}
		}
	}
	now := s.opts.Now()
	u, _ := url.Parse(xdsWorkloadID(project))
	cert, key, ca, err := s.issueFromLBCA(&x509.Certificate{Subject: pkix.Name{Organization: []string{project}}, URIs: []*url.URL{u},
		NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(0, 0, 30), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}})
	if err != nil {
		return XDSFiles{}, err
	}
	if err := os.MkdirAll(wd, 0o700); err != nil {
		return XDSFiles{}, err
	}
	for p, b := range map[string]string{f.CA: ca, f.CACertificates: ca, f.Certificate: cert, f.PrivateKey: key} {
		if err := os.WriteFile(p, []byte(b), 0o600); err != nil {
			return XDSFiles{}, err
		}
	}
	return f, nil
}

// XDSServerTLS is the TLS configuration the Compute Engine port takes the
// xDS API's TLS handshakes with: a certificate from the load balancing CA
// for hosts (names or IP addresses), offering HTTP/2.
func (s *Server) XDSServerTLS(hosts []string) (*tls.Config, error) {
	now := s.opts.Now()
	tpl := &x509.Certificate{Subject: pkix.Name{CommonName: "CloudBurrow xDS"}, NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(1, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	for _, h := range append([]string{"localhost", "127.0.0.1"}, hosts...) {
		if ip := net.ParseIP(h); ip != nil {
			tpl.IPAddresses = append(tpl.IPAddresses, ip)
		} else if h != "" {
			tpl.DNSNames = append(tpl.DNSNames, h)
		}
	}
	cert, key, _, err := s.issueFromLBCA(tpl)
	if err != nil {
		return nil, err
	}
	kp, err := tls.X509KeyPair([]byte(cert), []byte(key))
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{kp}, NextProtos: []string{"h2"}, MinVersion: tls.VersionTLS12}, nil
}

// XDSBootstrapTLS is a bootstrap file naming this server at addr over TLS,
// with the file_watcher certificate provider of the workload credentials
// and the server listener template, as Google's generator writes it with
// security enabled; meta are extra node metadata (the labels endpoint
// policies match), and instanceIP is INSTANCE_IP.
func XDSBootstrapTLS(addr, project, network string, f XDSFiles, meta map[string]string, instanceIP string) string {
	if network == "" {
		network = "default"
	}
	md := map[string]any{"TRAFFICDIRECTOR_NETWORK_NAME": network, "TRAFFICDIRECTOR_GCP_PROJECT_NUMBER": project}
	if instanceIP != "" {
		md["INSTANCE_IP"] = instanceIP
	}
	for k, v := range meta {
		md[k] = v
	}
	b := map[string]any{
		"xds_servers": []any{map[string]any{"server_uri": addr,
			"channel_creds":   []any{map[string]any{"type": "tls", "config": map[string]any{"ca_certificate_file": f.CA}}},
			"server_features": []any{"xds_v3"}}},
		"node": map[string]any{"id": "projects/" + project + "/networks/" + network + "/nodes/cloudburrow", "cluster": "cloudburrow",
			"metadata": md, "locality": map[string]any{"zone": "us-central1-a"}},
		"certificate_providers": map[string]any{XDSCertProviderInstance: map[string]any{"plugin_name": "file_watcher",
			"config": map[string]any{"certificate_file": f.Certificate, "private_key_file": f.PrivateKey,
				"ca_certificate_file": f.CACertificates, "refresh_interval": "600s"}}},
		"server_listener_resource_name_template": xdsServerListenerPrefix + "%s",
	}
	out, _ := json.Marshal(b)
	return string(out)
}

// serveXDSBootstrap answers GET /cloudburrow/xds/bootstrap?project=P&network=N:
// over TLS, with the workload credentials' file_watcher certificate
// provider, when this server has a credential directory and a project is
// named, else (or with creds=insecure) the plaintext bootstrap. Each
// node-metadata=KEY=VALUE adds node metadata, as the bootstrap
// generator's --node-metadata does, and instance-ip sets INSTANCE_IP.
func (s *Server) serveXDSBootstrap(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	project, network := q.Get("project"), q.Get("network")
	w.Header().Set("Content-Type", "application/json")
	if s.opts.XDSCredentialDir == "" || project == "" || q.Get("creds") == "insecure" {
		_, _ = w.Write([]byte(XDSBootstrap(r.Host, project, network)))
		return
	}
	f, err := s.WriteXDSCredentials(s.opts.XDSCredentialDir, project)
	if err != nil {
		http.Error(w, "the workload credentials could not be written: "+err.Error(), http.StatusInternalServerError)
		return
	}
	meta := map[string]string{}
	for _, kv := range q["node-metadata"] {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			meta[k] = v
		}
	}
	_, _ = w.Write([]byte(XDSBootstrapTLS(r.Host, project, network, f, meta, q.Get("instance-ip"))))
}
