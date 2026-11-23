//go:build compat

package compat

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	networksecurity "cloud.google.com/go/networksecurity/apiv1"
	"cloud.google.com/go/networksecurity/apiv1/networksecuritypb"
	networkservices "cloud.google.com/go/networkservices/apiv1"
	"cloud.google.com/go/networkservices/apiv1/networkservicespb"
	"cloud.google.com/go/security/privateca/apiv1/privatecapb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

// TestSecureWebProxyTLSInspectionThroughTheGoClients (#2160): TLS
// inspection, as secure-web-proxy/docs/enable-tls-inspection sets it up,
// with the official Go clients: a CA pool and an enabled root CA in
// Certificate Authority Service, a TLS inspection policy on the pool, a
// gateway security policy naming it with rules whose tlsInspectionEnabled
// is true (one denies request.path '/secret'), and a gateway. A CONNECT
// to a local TLS origin is decrypted: the client is handed a leaf for the
// host issued by the pool's CA (and recorded in the Enterprise pool),
// /secret is refused with 403 inside the tunnel, and an allowed request
// is re-encrypted to the origin, whose self-signed certificate the public
// CA set does not hold, so it gets 502 (the policy's trustConfig is not
// honoured, as Google's reference says). Every target is on loopback.
func TestSecureWebProxyTLSInspectionThroughTheGoClients(t *testing.T) {
	h := New(t)
	computeAddr := h.Endpoint(EnvCompute) // skips without it
	h.Endpoint(EnvPrivateCA)              // skips without it
	cas := privateCAClients(t, h)["grpc"]
	opts := computeOpts(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	project := h.Project()
	sfx := time.Now().UnixNano() % 1e6
	n := func(s string) string { return fmt.Sprintf("swpt-%s-%d", s, sfx) }
	const region = "us-west1"
	parent := "projects/" + project + "/locations/" + region

	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "origin "+r.URL.Path)
	}))
	defer origin.Close()
	_, port, _ := net.SplitHostPort(origin.Listener.Addr().String())

	// The CA pool and its root CA.
	pop, err := cas.CreateCaPool(ctx, &privatecapb.CreateCaPoolRequest{Parent: parent, CaPoolId: n("pool"),
		CaPool: &privatecapb.CaPool{Tier: privatecapb.CaPool_ENTERPRISE}})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	isCA := true
	cop, err := cas.CreateCertificateAuthority(ctx, &privatecapb.CreateCertificateAuthorityRequest{Parent: pool.GetName(), CertificateAuthorityId: "root",
		CertificateAuthority: &privatecapb.CertificateAuthority{Type: privatecapb.CertificateAuthority_SELF_SIGNED,
			Config: &privatecapb.CertificateConfig{
				SubjectConfig: &privatecapb.CertificateConfig_SubjectConfig{Subject: &privatecapb.Subject{CommonName: "SWP Inspection Root", Organization: "CloudBurrow"}},
				X509Config: &privatecapb.X509Parameters{CaOptions: &privatecapb.X509Parameters_CaOptions{IsCa: &isCA},
					KeyUsage: &privatecapb.KeyUsage{BaseKeyUsage: &privatecapb.KeyUsage_KeyUsageOptions{CertSign: true, CrlSign: true}}}},
			Lifetime: durationpb.New(10 * 24 * time.Hour),
			KeySpec:  &privatecapb.CertificateAuthority_KeyVersionSpec{KeyVersion: &privatecapb.CertificateAuthority_KeyVersionSpec_Algorithm{Algorithm: privatecapb.CertificateAuthority_EC_P256_SHA256}}}})
	if err != nil {
		t.Fatal(err)
	}
	ca, err := cop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	eop, err := cas.EnableCertificateAuthority(ctx, &privatecapb.EnableCertificateAuthorityRequest{Name: ca.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		if op, err := cas.DisableCertificateAuthority(c, &privatecapb.DisableCertificateAuthorityRequest{Name: ca.GetName()}); err == nil {
			_, _ = op.Wait(c)
		}
		if op, err := cas.DeleteCertificateAuthority(c, &privatecapb.DeleteCertificateAuthorityRequest{Name: ca.GetName(), SkipGracePeriod: true,
			IgnoreActiveCertificates: true}); err == nil {
			_, _ = op.Wait(c)
		}
		if op, err := cas.DeleteCaPool(c, &privatecapb.DeleteCaPoolRequest{Name: pool.GetName(), IgnoreDependentResources: true}); err == nil {
			_ = op.Wait(c)
		}
	})
	roots := x509.NewCertPool()
	for _, p := range ca.GetPemCaCertificates() {
		roots.AppendCertsFromPEM([]byte(p))
	}

	// The network, its subnetwork and its proxy-only subnet.
	nets, err := computeapi.NewNetworksRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nets.Close() })
	subs, err := computeapi.NewSubnetworksRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = subs.Close() })
	wait := func(what string, op *computeapi.Operation, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if err := op.Wait(ctx); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	op, err := nets.Insert(ctx, &computepb.InsertNetworkRequest{Project: project, NetworkResource: &computepb.Network{
		Name: proto.String(n("net")), AutoCreateSubnetworks: proto.Bool(false)}})
	wait("the network", op, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return nets.Delete(c, &computepb.DeleteNetworkRequest{Project: project, Network: n("net")})
	})
	netRel := "projects/" + project + "/global/networks/" + n("net")
	for _, sn := range []*computepb.Subnetwork{
		{Name: proto.String(n("sub")), IpCidrRange: proto.String("10.79.0.0/24"), Network: proto.String(netRel)},
		{Name: proto.String(n("proxy")), IpCidrRange: proto.String("10.80.0.0/23"), Network: proto.String(netRel),
			Purpose: proto.String("REGIONAL_MANAGED_PROXY"), Role: proto.String("ACTIVE")},
	} {
		op, err := subs.Insert(ctx, &computepb.InsertSubnetworkRequest{Project: project, Region: region, SubnetworkResource: sn})
		wait("the subnetwork "+sn.GetName(), op, err)
		name := sn.GetName()
		later(t, func(c context.Context) (*computeapi.Operation, error) {
			return subs.Delete(c, &computepb.DeleteSubnetworkRequest{Project: project, Region: region, Subnetwork: name})
		})
	}

	// The TLS inspection policy, the gateway security policy and its rules.
	ns, err := networksecurity.NewRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ns.Close() })
	top, err := ns.CreateTlsInspectionPolicy(ctx, &networksecuritypb.CreateTlsInspectionPolicyRequest{Parent: parent, TlsInspectionPolicyId: n("tls"),
		TlsInspectionPolicy: &networksecuritypb.TlsInspectionPolicy{CaPool: pool.GetName()}})
	if err != nil {
		t.Fatal(err)
	}
	tip, err := top.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gop, err := ns.CreateGatewaySecurityPolicy(ctx, &networksecuritypb.CreateGatewaySecurityPolicyRequest{Parent: parent, GatewaySecurityPolicyId: n("pol"),
		GatewaySecurityPolicy: &networksecuritypb.GatewaySecurityPolicy{TlsInspectionPolicy: tip.GetName()}})
	if err != nil {
		t.Fatal(err)
	}
	pol, err := gop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rule := func(id string, prio int32, allow bool, app string) {
		t.Helper()
		bp := networksecuritypb.GatewaySecurityPolicyRule_ALLOW
		if !allow {
			bp = networksecuritypb.GatewaySecurityPolicyRule_DENY
		}
		rop, err := ns.CreateGatewaySecurityPolicyRule(ctx, &networksecuritypb.CreateGatewaySecurityPolicyRuleRequest{Parent: pol.GetName(),
			GatewaySecurityPolicyRuleId: id, GatewaySecurityPolicyRule: &networksecuritypb.GatewaySecurityPolicyRule{Enabled: true, Priority: prio,
				Profile:        &networksecuritypb.GatewaySecurityPolicyRule_BasicProfile_{BasicProfile: bp},
				SessionMatcher: "host() == 'localhost'", ApplicationMatcher: app, TlsInspectionEnabled: true}})
		if err != nil {
			t.Fatalf("rule %s: %v", id, err)
		}
		if _, err := rop.Wait(ctx); err != nil {
			t.Fatalf("rule %s: %v", id, err)
		}
	}
	rule("deny-secret", 1, false, "request.path.startsWith('/secret')")
	rule("allow-rest", 2, true, "")
	t.Cleanup(func() {
		c := context.Background()
		for _, id := range []string{"deny-secret", "allow-rest"} {
			if op, err := ns.DeleteGatewaySecurityPolicyRule(c, &networksecuritypb.DeleteGatewaySecurityPolicyRuleRequest{Name: pol.GetName() + "/rules/" + id}); err == nil {
				_ = op.Wait(c)
			}
		}
		if op, err := ns.DeleteGatewaySecurityPolicy(c, &networksecuritypb.DeleteGatewaySecurityPolicyRequest{Name: pol.GetName()}); err == nil {
			_ = op.Wait(c)
		}
		if op, err := ns.DeleteTlsInspectionPolicy(c, &networksecuritypb.DeleteTlsInspectionPolicyRequest{Name: tip.GetName()}); err == nil {
			_ = op.Wait(c)
		}
	})

	// The gateway.
	nsv, err := networkservices.NewRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nsv.Close() })
	gwop, err := nsv.CreateGateway(ctx, &networkservicespb.CreateGatewayRequest{Parent: parent, GatewayId: n("swp"),
		Gateway: &networkservicespb.Gateway{Type: networkservicespb.Gateway_SECURE_WEB_GATEWAY, Ports: []int32{443},
			GatewaySecurityPolicy: pol.GetName(), Network: netRel, Subnetwork: "projects/" + project + "/regions/" + region + "/subnetworks/" + n("sub"),
			RoutingMode: networkservicespb.Gateway_EXPLICIT_ROUTING_MODE}})
	if err != nil {
		t.Fatal(err)
	}
	gw, err := gwop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := nsv.DeleteGateway(context.Background(), &networkservicespb.DeleteGatewayRequest{Name: gw.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	resp, err := http.Get("http://" + computeAddr + "/v1/" + gw.GetName())
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Address string `json:"cloudburrowAddress"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&raw)
	_ = resp.Body.Close()
	if raw.Address == "" {
		t.Fatal("the gateway's JSON has no cloudburrowAddress")
	}

	// CONNECT localhost:PORT: the client trusts only the pool's CA.
	pu, _ := url.Parse("http://" + raw.Address)
	var leaf *x509.Certificate
	tr := &http.Transport{Proxy: http.ProxyURL(pu), TLSClientConfig: &tls.Config{RootCAs: roots,
		VerifyConnection: func(cs tls.ConnectionState) error { leaf = cs.PeerCertificates[0]; return nil }}}
	c := &http.Client{Transport: tr, Timeout: 30 * time.Second}
	get := func(path string) (int, string) {
		t.Helper()
		r, err := c.Get("https://localhost:" + port + path)
		if err != nil {
			t.Fatalf("GET %s through the inspecting proxy: %v", path, err)
		}
		defer func() { _ = r.Body.Close() }()
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(b)
	}
	if code, body := get("/secret/x"); code != 403 {
		t.Errorf("GET /secret/x: %d %q, want 403 from the applicationMatcher rule", code, body)
	}
	if leaf == nil || leaf.Issuer.CommonName != "SWP Inspection Root" || leaf.VerifyHostname("localhost") != nil {
		t.Fatalf("the interception leaf: %v", leaf)
	}
	if code, body := get("/hello"); code != 502 {
		t.Errorf("GET /hello to an origin outside the public CA set: %d %q, want 502", code, body)
	}

	// The leaf is a certificate of the Enterprise pool.
	found := false
	for it := cas.ListCertificates(ctx, &privatecapb.ListCertificatesRequest{Parent: pool.GetName()}); ; {
		cert, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		found = found || pemSerialIs(cert.GetPemCertificate(), leaf)
	}
	if !found {
		t.Error("the pool lists no certificate for the interception leaf")
	}

	// Without inspection the client would see the origin's own
	// certificate; a client that does not trust the pool's CA refuses it.
	cn, err := net.DialTimeout("tcp", raw.Address, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cn.Close() }()
	_ = cn.SetDeadline(time.Now().Add(20 * time.Second))
	fmt.Fprintf(cn, "CONNECT localhost:%s HTTP/1.1\r\nHost: localhost:%s\r\n\r\n", port, port)
	br := bufio.NewReader(cn)
	if r, err := http.ReadResponse(br, nil); err != nil || r.StatusCode != 200 {
		t.Fatalf("CONNECT: %v %v", r, err)
	}
	tc := tls.Client(&swpBufConn{Conn: cn, r: br}, &tls.Config{ServerName: "localhost"})
	if err := tc.Handshake(); err == nil {
		t.Error("a client without the pool's CA trusted the interception leaf")
	}
}

// pemSerialIs is whether a PEM certificate is leaf.
func pemSerialIs(pemCert string, leaf *x509.Certificate) bool {
	b, _ := pem.Decode([]byte(pemCert))
	return leaf != nil && b != nil && bytes.Equal(b.Bytes, leaf.Raw)
}

type swpBufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *swpBufConn) Read(p []byte) (int, error) { return b.r.Read(p) }
