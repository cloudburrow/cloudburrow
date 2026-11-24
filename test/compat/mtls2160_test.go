//go:build compat

package compat

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	certificatemanager "cloud.google.com/go/certificatemanager/apiv1"
	"cloud.google.com/go/certificatemanager/apiv1/certificatemanagerpb"
	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	networksecurity "cloud.google.com/go/networksecurity/apiv1"
	"cloud.google.com/go/networksecurity/apiv1/networksecuritypb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// mtlsCA is a test certificate authority: its certificate as PEM and its
// key, which signs client certificates.
type mtlsCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  string
}

func newMTLSCA(t *testing.T, cn string) mtlsCA {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn}, IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return mtlsCA{c, k, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
}

func (ca mtlsCA) client(t *testing.T, spiffe string) tls.Certificate {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	u, _ := url.Parse(spiffe)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "client"}, URIs: []*url.URL{u},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, KeyUsage: x509.KeyUsageDigitalSignature,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca.cert, &k.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}
}

// covers: google.cloud.networksecurity.v1.NetworkSecurity/CreateServerTlsPolicy, google.cloud.networksecurity.v1.NetworkSecurity/GetServerTlsPolicy, google.cloud.networksecurity.v1.NetworkSecurity/ListServerTlsPolicies, google.cloud.networksecurity.v1.NetworkSecurity/UpdateServerTlsPolicy, google.cloud.networksecurity.v1.NetworkSecurity/DeleteServerTlsPolicy, google.cloud.networksecurity.v1.NetworkSecurity/CreateAuthzPolicy, google.cloud.networksecurity.v1.NetworkSecurity/GetAuthzPolicy, google.cloud.networksecurity.v1.NetworkSecurity/ListAuthzPolicies, google.cloud.networksecurity.v1.NetworkSecurity/UpdateAuthzPolicy, google.cloud.networksecurity.v1.NetworkSecurity/DeleteAuthzPolicy, google.cloud.networksecurity.v1.NetworkSecurity/CreateClientTlsPolicy, google.cloud.networksecurity.v1.NetworkSecurity/GetClientTlsPolicy, google.cloud.networksecurity.v1.NetworkSecurity/ListClientTlsPolicies, google.cloud.networksecurity.v1.NetworkSecurity/UpdateClientTlsPolicy, google.cloud.networksecurity.v1.NetworkSecurity/DeleteClientTlsPolicy, google.cloud.networksecurity.v1.NetworkSecurity/CreateAuthorizationPolicy, google.cloud.networksecurity.v1.NetworkSecurity/GetAuthorizationPolicy, google.cloud.networksecurity.v1.NetworkSecurity/ListAuthorizationPolicies, google.cloud.networksecurity.v1.NetworkSecurity/UpdateAuthorizationPolicy, google.cloud.networksecurity.v1.NetworkSecurity/DeleteAuthorizationPolicy, google.cloud.certificatemanager.v1.CertificateManager/CreateTrustConfig, google.cloud.certificatemanager.v1.CertificateManager/GetTrustConfig, google.cloud.certificatemanager.v1.CertificateManager/ListTrustConfigs, google.cloud.certificatemanager.v1.CertificateManager/UpdateTrustConfig, google.cloud.certificatemanager.v1.CertificateManager/DeleteTrustConfig, compute.targetHttpsProxies.patch
//
// TestMutualTLSAndAuthorizationThroughALoadBalancer (#2160), the official
// certificatemanager, networksecurity and compute REST clients, as
// load-balancing/docs/https/setting-up-mtls-global and
// load-balancing/docs/auth-policy sets them up: a trust config of a test
// CA, a server TLS policy that rejects invalid client certificates on an
// external Application Load Balancer's target HTTPS proxy, and an
// authorization policy that denies /admin on its forwarding rule. Real
// HTTPS requests through the rule's local port: a client certificate of
// the CA passes, none or another CA's ends the handshake, /admin is 403;
// with ALLOW_INVALID_OR_MISSING_CLIENT_CERT a client without one passes,
// and the backend's custom headers say so. The client TLS and classic
// authorization policies' methods, and the documented refusals.
func TestMutualTLSAndAuthorizationThroughALoadBalancer(t *testing.T) {
	h := New(t)
	l := newLB2160(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "present=%s verified=%s error=%s spiffe=%s", r.Header.Get("X-Client-Cert-Present"),
			r.Header.Get("X-Client-Cert-Verified"), r.Header.Get("X-Client-Cert-Error"), r.Header.Get("X-Client-Cert-Spiffe"))
	}))
	ctx, opts := l.ctx, computeOpts(t, h)
	cm, err := certificatemanager.NewRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cm.Close() })
	ns, err := networksecurity.NewRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ns.Close() })
	g := "projects/" + l.project + "/locations/global"
	ca, other := newMTLSCA(t, "mtls-root"), newMTLSCA(t, "mtls-other")

	// The trust config, then the server TLS policy that names it.
	top, err := cm.CreateTrustConfig(ctx, &certificatemanagerpb.CreateTrustConfigRequest{Parent: g, TrustConfigId: l.n("tc"),
		TrustConfig: &certificatemanagerpb.TrustConfig{TrustStores: []*certificatemanagerpb.TrustConfig_TrustStore{{
			TrustAnchors: []*certificatemanagerpb.TrustConfig_TrustAnchor{{Kind: &certificatemanagerpb.TrustConfig_TrustAnchor_PemCertificate{PemCertificate: ca.pem}}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	tc, err := top.Wait(ctx)
	if err != nil || len(tc.GetTrustStores()) != 1 || tc.GetEtag() == "" {
		t.Fatalf("trust config: %v %v", tc, err)
	}
	t.Cleanup(func() {
		if op, err := cm.DeleteTrustConfig(context.Background(), &certificatemanagerpb.DeleteTrustConfigRequest{Name: tc.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	if got, err := cm.GetTrustConfig(ctx, &certificatemanagerpb.GetTrustConfigRequest{Name: tc.GetName()}); err != nil || got.GetName() != tc.GetName() {
		t.Fatalf("GetTrustConfig: %v %v", got, err)
	}
	found := false
	for it := cm.ListTrustConfigs(ctx, &certificatemanagerpb.ListTrustConfigsRequest{Parent: g}); ; {
		x, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		found = found || x.GetName() == tc.GetName()
	}
	if !found {
		t.Error("ListTrustConfigs: the trust config is missing")
	}
	uop, err := cm.UpdateTrustConfig(ctx, &certificatemanagerpb.UpdateTrustConfigRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}},
		TrustConfig: &certificatemanagerpb.TrustConfig{Name: tc.GetName(), Description: "mTLS test"}})
	if err != nil {
		t.Fatal(err)
	}
	if tc, err = uop.Wait(ctx); err != nil || tc.GetDescription() != "mTLS test" {
		t.Fatalf("UpdateTrustConfig: %v %v", tc, err)
	}

	if _, err := ns.CreateServerTlsPolicy(ctx, &networksecuritypb.CreateServerTlsPolicyRequest{Parent: g, ServerTlsPolicyId: l.n("bad"),
		ServerTlsPolicy: &networksecuritypb.ServerTlsPolicy{MtlsPolicy: &networksecuritypb.ServerTlsPolicy_MTLSPolicy{
			ClientValidationMode:        networksecuritypb.ServerTlsPolicy_MTLSPolicy_REJECT_INVALID,
			ClientValidationTrustConfig: g + "/trustConfigs/" + l.n("none")}}}); computeCode(err) != http.StatusNotFound {
		t.Errorf("a missing trust config: %v", err)
	}
	sop, err := ns.CreateServerTlsPolicy(ctx, &networksecuritypb.CreateServerTlsPolicyRequest{Parent: g, ServerTlsPolicyId: l.n("stp"),
		ServerTlsPolicy: &networksecuritypb.ServerTlsPolicy{MtlsPolicy: &networksecuritypb.ServerTlsPolicy_MTLSPolicy{
			ClientValidationMode:        networksecuritypb.ServerTlsPolicy_MTLSPolicy_REJECT_INVALID,
			ClientValidationTrustConfig: tc.GetName()}}})
	if err != nil {
		t.Fatal(err)
	}
	stp, err := sop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := ns.DeleteServerTlsPolicy(context.Background(), &networksecuritypb.DeleteServerTlsPolicyRequest{Name: stp.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	if got, err := ns.GetServerTlsPolicy(ctx, &networksecuritypb.GetServerTlsPolicyRequest{Name: stp.GetName()}); err != nil ||
		got.GetMtlsPolicy().GetClientValidationTrustConfig() != tc.GetName() {
		t.Fatalf("GetServerTlsPolicy: %v %v", got, err)
	}
	n := 0
	for it := ns.ListServerTlsPolicies(ctx, &networksecuritypb.ListServerTlsPoliciesRequest{Parent: g}); ; {
		_, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n == 0 {
		t.Error("ListServerTlsPolicies: none")
	}

	// The target HTTPS proxy with a self-managed certificate and the
	// server TLS policy, and its forwarding rule.
	crt, key := compatSelfSigned(t, "mtls.example.com")
	certs, _ := computeapi.NewSslCertificatesRESTClient(ctx, opts...)
	op, err := certs.Insert(ctx, &computepb.InsertSslCertificateRequest{Project: l.project, SslCertificateResource: &computepb.SslCertificate{
		Name: proto.String(l.n("crt")), Certificate: proto.String(crt), PrivateKey: proto.String(key)}})
	l.do("the SSL certificate", op, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return certs.Delete(c, &computepb.DeleteSslCertificateRequest{Project: l.project, SslCertificate: l.n("crt")})
	})
	thps, _ := computeapi.NewTargetHttpsProxiesRESTClient(ctx, opts...)
	op, err = thps.Insert(ctx, &computepb.InsertTargetHttpsProxyRequest{Project: l.project, TargetHttpsProxyResource: &computepb.TargetHttpsProxy{
		Name: proto.String(l.n("https")), UrlMap: proto.String("global/urlMaps/" + l.n("map")),
		SslCertificates: []string{"global/sslCertificates/" + l.n("crt")}}})
	l.do("the target HTTPS proxy", op, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return thps.Delete(c, &computepb.DeleteTargetHttpsProxyRequest{Project: l.project, TargetHttpsProxy: l.n("https")})
	})
	// As setting-up-mtls-global attaches it: export, add serverTlsPolicy,
	// import (a patch).
	ref := "//networksecurity.googleapis.com/" + stp.GetName()
	proxy, err := thps.Get(ctx, &computepb.GetTargetHttpsProxyRequest{Project: l.project, TargetHttpsProxy: l.n("https")})
	if err != nil {
		t.Fatal(err)
	}
	op, err = thps.Patch(ctx, &computepb.PatchTargetHttpsProxyRequest{Project: l.project, TargetHttpsProxy: l.n("https"),
		TargetHttpsProxyResource: &computepb.TargetHttpsProxy{ServerTlsPolicy: proto.String(ref), Fingerprint: proxy.Fingerprint}})
	l.do("the server TLS policy's attachment", op, err)
	if proxy, err = thps.Get(ctx, &computepb.GetTargetHttpsProxyRequest{Project: l.project, TargetHttpsProxy: l.n("https")}); err != nil || proxy.GetServerTlsPolicy() != ref {
		t.Fatalf("the proxy's serverTlsPolicy: %v %v", proxy.GetServerTlsPolicy(), err)
	}
	if _, err := ns.DeleteServerTlsPolicy(ctx, &networksecuritypb.DeleteServerTlsPolicyRequest{Name: stp.GetName()}); computeCode(err) != http.StatusBadRequest {
		t.Errorf("deleting the policy a proxy uses: %v", err)
	}
	if _, err := cm.DeleteTrustConfig(ctx, &certificatemanagerpb.DeleteTrustConfigRequest{Name: tc.GetName()}); computeCode(err) != http.StatusBadRequest {
		t.Errorf("deleting the trust config a policy uses: %v", err)
	}
	gfrs, _ := computeapi.NewGlobalForwardingRulesRESTClient(ctx, opts...)
	op, err = gfrs.Insert(ctx, &computepb.InsertGlobalForwardingRuleRequest{Project: l.project, ForwardingRuleResource: &computepb.ForwardingRule{
		Name: proto.String(l.n("https")), Target: proto.String("global/targetHttpsProxies/" + l.n("https")), PortRange: proto.String("443"),
		LoadBalancingScheme: proto.String("EXTERNAL_MANAGED"), IPProtocol: proto.String("TCP")}})
	l.do("the HTTPS forwarding rule", op, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return gfrs.Delete(c, &computepb.DeleteGlobalForwardingRuleRequest{Project: l.project, ForwardingRule: l.n("https")})
	})
	addr := ruleAddress(t, l.compute, l.project, "global/forwardingRules/"+l.n("https"))
	get := func(cert *tls.Certificate, path string) (int, string, error) {
		cfg := &tls.Config{ServerName: "mtls.example.com", InsecureSkipVerify: true} //nolint:gosec // the proxy's certificate is self-signed
		if cert != nil {
			cfg.Certificates = []tls.Certificate{*cert}
		}
		c := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}, Timeout: 10 * time.Second}
		resp, err := c.Get("https://" + addr + path)
		if err != nil {
			return 0, "", err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), nil
	}
	good, bad := ca.client(t, "spiffe://example.org/ns/good"), other.client(t, "spiffe://example.org/ns/bad")
	waitFor(t, "the HTTPS rule", 2*time.Minute, func() bool {
		code, _, err := get(&good, "/")
		return err == nil && code == 200
	})
	if _, _, err := get(nil, "/"); err == nil {
		t.Error("REJECT_INVALID: a client without a certificate got through")
	}
	if _, _, err := get(&bad, "/"); err == nil {
		t.Error("REJECT_INVALID: another CA's certificate got through")
	}

	// An authorization policy denies /admin on the forwarding rule.
	ruleURL := "https://www.googleapis.com/compute/v1/projects/" + l.project + "/global/forwardingRules/" + l.n("https")
	aop, err := ns.CreateAuthzPolicy(ctx, &networksecuritypb.CreateAuthzPolicyRequest{Parent: g, AuthzPolicyId: l.n("deny"),
		AuthzPolicy: &networksecuritypb.AuthzPolicy{Action: networksecuritypb.AuthzPolicy_DENY,
			Target: &networksecuritypb.AuthzPolicy_Target{LoadBalancingScheme: networksecuritypb.AuthzPolicy_EXTERNAL_MANAGED, Resources: []string{ruleURL}},
			HttpRules: []*networksecuritypb.AuthzPolicy_AuthzRule{{To: &networksecuritypb.AuthzPolicy_AuthzRule_To{
				Operations: []*networksecuritypb.AuthzPolicy_AuthzRule_To_RequestOperation{{
					Paths: []*networksecuritypb.AuthzPolicy_AuthzRule_StringMatch{{MatchPattern: &networksecuritypb.AuthzPolicy_AuthzRule_StringMatch_Prefix{Prefix: "/admin"}}}}}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	azp, err := aop.Wait(ctx)
	if err != nil || azp.GetPolicyProfile() != networksecuritypb.AuthzPolicy_REQUEST_AUTHZ {
		t.Fatalf("authz policy: %v %v", azp, err)
	}
	t.Cleanup(func() {
		if op, err := ns.DeleteAuthzPolicy(context.Background(), &networksecuritypb.DeleteAuthzPolicyRequest{Name: azp.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	if code, _, err := get(&good, "/admin/panel"); err != nil || code != http.StatusForbidden {
		t.Errorf("/admin under a DENY policy: %d %v", code, err)
	}
	if code, _, err := get(&good, "/public"); err != nil || code != 200 {
		t.Errorf("/public: %d %v", code, err)
	}
	if got, err := ns.GetAuthzPolicy(ctx, &networksecuritypb.GetAuthzPolicyRequest{Name: azp.GetName()}); err != nil || got.GetAction() != networksecuritypb.AuthzPolicy_DENY {
		t.Fatalf("GetAuthzPolicy: %v %v", got, err)
	}
	if it := ns.ListAuthzPolicies(ctx, &networksecuritypb.ListAuthzPoliciesRequest{Parent: g}); true {
		if _, err := it.Next(); err != nil {
			t.Errorf("ListAuthzPolicies: %v", err)
		}
	}
	// An ALLOW policy for 10.0.0.0/8 only: the local client is denied.
	upd, err := ns.UpdateAuthzPolicy(ctx, &networksecuritypb.UpdateAuthzPolicyRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"action", "http_rules"}},
		AuthzPolicy: &networksecuritypb.AuthzPolicy{Name: azp.GetName(), Action: networksecuritypb.AuthzPolicy_ALLOW,
			HttpRules: []*networksecuritypb.AuthzPolicy_AuthzRule{{From: &networksecuritypb.AuthzPolicy_AuthzRule_From{
				Sources: []*networksecuritypb.AuthzPolicy_AuthzRule_From_RequestSource{{IpBlocks: []*networksecuritypb.AuthzPolicy_AuthzRule_IpBlock{{Prefix: "10.0.0.0", Length: 8}}}}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upd.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if code, _, err := get(&good, "/public"); err != nil || code != http.StatusForbidden {
		t.Errorf("an ALLOW policy that does not match: %d %v", code, err)
	}
	dop, err := ns.DeleteAuthzPolicy(ctx, &networksecuritypb.DeleteAuthzPolicyRequest{Name: azp.GetName()})
	if err != nil || dop.Wait(ctx) != nil {
		t.Fatalf("DeleteAuthzPolicy: %v", err)
	}

	// The permissive mode, and the backend's mTLS headers.
	op, err = l.bss.Patch(ctx, &computepb.PatchBackendServiceRequest{Project: l.project, BackendService: l.n("app"),
		BackendServiceResource: &computepb.BackendService{CustomRequestHeaders: []string{"X-Client-Cert-Present:{client_cert_present}",
			"X-Client-Cert-Verified:{client_cert_chain_verified}", "X-Client-Cert-Error:{client_cert_error}", "X-Client-Cert-Spiffe:{client_cert_spiffe_id}"}}})
	l.do("the backend's custom headers", op, err)
	usop, err := ns.UpdateServerTlsPolicy(ctx, &networksecuritypb.UpdateServerTlsPolicyRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"mtls_policy"}},
		ServerTlsPolicy: &networksecuritypb.ServerTlsPolicy{Name: stp.GetName(), MtlsPolicy: &networksecuritypb.ServerTlsPolicy_MTLSPolicy{
			ClientValidationMode:        networksecuritypb.ServerTlsPolicy_MTLSPolicy_ALLOW_INVALID_OR_MISSING_CLIENT_CERT,
			ClientValidationTrustConfig: tc.GetName()}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := usop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if code, body, err := get(nil, "/"); err != nil || code != 200 || !strings.Contains(body, "present=false verified=false error=client_cert_not_provided") {
		t.Errorf("no certificate, permissive: %d %q %v", code, body, err)
	}
	if code, body, err := get(&bad, "/"); err != nil || code != 200 || !strings.Contains(body, "present=true verified=false error=client_cert_validation_failed spiffe=spiffe://example.org/ns/bad") {
		t.Errorf("another CA's certificate, permissive: %d %q %v", code, body, err)
	}
	if code, body, err := get(&good, "/"); err != nil || code != 200 || !strings.Contains(body, "present=true verified=true error= spiffe=spiffe://example.org/ns/good") {
		t.Errorf("a valid certificate, permissive: %d %q %v", code, body, err)
	}
	// A client TLS policy, and a classic authorization policy.
	cop, err := ns.CreateClientTlsPolicy(ctx, &networksecuritypb.CreateClientTlsPolicyRequest{Parent: g, ClientTlsPolicyId: l.n("ctp"),
		ClientTlsPolicy: &networksecuritypb.ClientTlsPolicy{Sni: "backend.example.com", ClientCertificate: &networksecuritypb.CertificateProvider{
			Type: &networksecuritypb.CertificateProvider_CertificateProviderInstance{CertificateProviderInstance: &networksecuritypb.CertificateProviderInstance{PluginInstance: "google_cloud_private_spiffe"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	ctp, err := cop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ns.GetClientTlsPolicy(ctx, &networksecuritypb.GetClientTlsPolicyRequest{Name: ctp.GetName()}); err != nil || got.GetSni() != "backend.example.com" {
		t.Fatalf("GetClientTlsPolicy: %v %v", got, err)
	}
	if _, err := ns.ListClientTlsPolicies(ctx, &networksecuritypb.ListClientTlsPoliciesRequest{Parent: g}).Next(); err != nil {
		t.Errorf("ListClientTlsPolicies: %v", err)
	}
	if op, err := ns.UpdateClientTlsPolicy(ctx, &networksecuritypb.UpdateClientTlsPolicyRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"sni"}},
		ClientTlsPolicy: &networksecuritypb.ClientTlsPolicy{Name: ctp.GetName(), Sni: "b2.example.com"}}); err != nil {
		t.Error(err)
	} else if got, err := op.Wait(ctx); err != nil || got.GetSni() != "b2.example.com" {
		t.Errorf("UpdateClientTlsPolicy: %v %v", got, err)
	}
	if op, err := ns.DeleteClientTlsPolicy(ctx, &networksecuritypb.DeleteClientTlsPolicyRequest{Name: ctp.GetName()}); err != nil || op.Wait(ctx) != nil {
		t.Errorf("DeleteClientTlsPolicy: %v", err)
	}
	apop, err := ns.CreateAuthorizationPolicy(ctx, &networksecuritypb.CreateAuthorizationPolicyRequest{Parent: g, AuthorizationPolicyId: l.n("ap"),
		AuthorizationPolicy: &networksecuritypb.AuthorizationPolicy{Action: networksecuritypb.AuthorizationPolicy_ALLOW,
			Rules: []*networksecuritypb.AuthorizationPolicy_Rule{{Sources: []*networksecuritypb.AuthorizationPolicy_Rule_Source{{Principals: []string{"spiffe://example.org/*"}}},
				Destinations: []*networksecuritypb.AuthorizationPolicy_Rule_Destination{{Hosts: []string{"*.example.com"}, Ports: []uint32{443}, Methods: []string{"GET"}}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	ap, err := apop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ns.CreateAuthorizationPolicy(ctx, &networksecuritypb.CreateAuthorizationPolicyRequest{Parent: g, AuthorizationPolicyId: l.n("ap2"),
		AuthorizationPolicy: &networksecuritypb.AuthorizationPolicy{Action: networksecuritypb.AuthorizationPolicy_ALLOW,
			Rules: []*networksecuritypb.AuthorizationPolicy_Rule{{Destinations: []*networksecuritypb.AuthorizationPolicy_Rule_Destination{{Hosts: []string{"*"}}}}}}}); computeCode(err) != http.StatusBadRequest {
		t.Errorf("a destination without ports: %v", err)
	}
	if got, err := ns.GetAuthorizationPolicy(ctx, &networksecuritypb.GetAuthorizationPolicyRequest{Name: ap.GetName()}); err != nil || len(got.GetRules()) != 1 {
		t.Fatalf("GetAuthorizationPolicy: %v %v", got, err)
	}
	if _, err := ns.ListAuthorizationPolicies(ctx, &networksecuritypb.ListAuthorizationPoliciesRequest{Parent: g}).Next(); err != nil {
		t.Errorf("ListAuthorizationPolicies: %v", err)
	}
	if op, err := ns.UpdateAuthorizationPolicy(ctx, &networksecuritypb.UpdateAuthorizationPolicyRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"action"}},
		AuthorizationPolicy: &networksecuritypb.AuthorizationPolicy{Name: ap.GetName(), Action: networksecuritypb.AuthorizationPolicy_DENY}}); err != nil {
		t.Error(err)
	} else if got, err := op.Wait(ctx); err != nil || got.GetAction() != networksecuritypb.AuthorizationPolicy_DENY {
		t.Errorf("UpdateAuthorizationPolicy: %v %v", got, err)
	}
	if op, err := ns.DeleteAuthorizationPolicy(ctx, &networksecuritypb.DeleteAuthorizationPolicyRequest{Name: ap.GetName()}); err != nil || op.Wait(ctx) != nil {
		t.Errorf("DeleteAuthorizationPolicy: %v", err)
	}
}
