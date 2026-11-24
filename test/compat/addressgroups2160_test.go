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
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
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

// covers: google.cloud.networksecurity.v1.AddressGroupService/CreateAddressGroup, google.cloud.networksecurity.v1.AddressGroupService/GetAddressGroup, google.cloud.networksecurity.v1.AddressGroupService/ListAddressGroups, google.cloud.networksecurity.v1.AddressGroupService/UpdateAddressGroup, google.cloud.networksecurity.v1.AddressGroupService/AddAddressGroupItems, google.cloud.networksecurity.v1.AddressGroupService/RemoveAddressGroupItems, google.cloud.networksecurity.v1.AddressGroupService/CloneAddressGroupItems, google.cloud.networksecurity.v1.AddressGroupService/ListAddressGroupReferences, google.cloud.networksecurity.v1.AddressGroupService/DeleteAddressGroup, compute.projects.setCloudArmorTier
//
// TestCloudArmorAddressGroupsThroughTheGoClient (#2160), the official
// networksecurity and compute REST clients, as armor/docs/address-groups-overview
// and the rules language reference set them up: the project enrolled in
// Cloud Armor Enterprise (projects.setCloudArmorTier), an IPv4 address
// group for Cloud Armor, and a backend security policy whose rule denies
// evaluateAddressGroup(GROUP, origin.ip); real requests through the load
// balancer's local port pass until the group holds 127.0.0.1, are 403
// while it does, and pass again once the item is removed. The item calls,
// references, and the documented refusals (a standard-tier project, a
// type mismatch, a capacity over the limit, a referenced group's
// deletion).
func TestCloudArmorAddressGroupsThroughTheGoClient(t *testing.T) {
	h := New(t)
	l := newLB2160(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, "origin %s", r.URL.Path) }))
	ctx, project, n, opts := l.ctx, l.project, l.n, computeOpts(t, h)
	ags, err := networksecurity.NewAddressGroupRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ags.Close() })
	sps, _ := computeapi.NewSecurityPoliciesRESTClient(ctx, opts...)
	projects, _ := computeapi.NewProjectsRESTClient(ctx, opts...)
	g := "projects/" + project + "/locations/global"

	op, err := ags.CreateAddressGroup(ctx, &networksecuritypb.CreateAddressGroupRequest{Parent: g, AddressGroupId: n("bad"),
		AddressGroup: &networksecuritypb.AddressGroup{Type: networksecuritypb.AddressGroup_IPV4, Capacity: 10,
			Purpose: []networksecuritypb.AddressGroup_Purpose{networksecuritypb.AddressGroup_CLOUD_ARMOR}, Items: []string{"198.51.100.0/24"}}})
	if err != nil {
		t.Fatal(err)
	}
	ag, err := op.Wait(ctx)
	if err != nil || ag.GetName() != g+"/addressGroups/"+n("bad") {
		t.Fatalf("CreateAddressGroup: %v %v", ag, err)
	}
	t.Cleanup(func() {
		if op, err := ags.DeleteAddressGroup(context.Background(), &networksecuritypb.DeleteAddressGroupRequest{Name: ag.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	if _, err := ags.CreateAddressGroup(ctx, &networksecuritypb.CreateAddressGroupRequest{Parent: g, AddressGroupId: n("v6"),
		AddressGroup: &networksecuritypb.AddressGroup{Type: networksecuritypb.AddressGroup_IPV4, Capacity: 1, Items: []string{"2001:db8::/32"}}}); err == nil {
		t.Error("an IPv6 item in an IPv4 group was accepted")
	}
	if _, err := ags.CreateAddressGroup(ctx, &networksecuritypb.CreateAddressGroupRequest{Parent: g, AddressGroupId: n("big"),
		AddressGroup: &networksecuritypb.AddressGroup{Type: networksecuritypb.AddressGroup_IPV4, Capacity: 1001}}); err == nil {
		t.Error("a DEFAULT group of capacity 1,001 was accepted")
	}
	if got, err := ags.GetAddressGroup(ctx, &networksecuritypb.GetAddressGroupRequest{Name: ag.GetName()}); err != nil || got.GetCapacity() != 10 {
		t.Fatalf("GetAddressGroup: %v %v", got, err)
	}
	found := false
	for it := ags.ListAddressGroups(ctx, &networksecuritypb.ListAddressGroupsRequest{Parent: g}); ; {
		x, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		found = found || x.GetName() == ag.GetName()
	}
	if !found {
		t.Error("ListAddressGroups: the group is missing")
	}
	uop, err := ags.UpdateAddressGroup(ctx, &networksecuritypb.UpdateAddressGroupRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}},
		AddressGroup: &networksecuritypb.AddressGroup{Name: ag.GetName(), Description: "known bad"}})
	if err != nil {
		t.Fatal(err)
	}
	if ag, err = uop.Wait(ctx); err != nil || ag.GetDescription() != "known bad" || len(ag.GetItems()) != 1 {
		t.Fatalf("UpdateAddressGroup: %v %v", ag, err)
	}

	// A standard-tier project cannot use address groups.
	rule := &computepb.SecurityPolicyRule{Priority: proto.Int32(100), Action: proto.String("deny(403)"),
		Match: &computepb.SecurityPolicyRuleMatcher{Expr: &computepb.Expr{Expression: proto.String("evaluateAddressGroup('" + n("bad") + "', origin.ip)")}}}
	setTier := func(c context.Context, tier string) error {
		op, err := projects.SetCloudArmorTier(c, &computepb.SetCloudArmorTierProjectRequest{Project: project,
			ProjectsSetCloudArmorTierRequestResource: &computepb.ProjectsSetCloudArmorTierRequest{CloudArmorTier: proto.String(tier)}})
		if err != nil {
			return err
		}
		return op.Wait(c)
	}
	if err := setTier(ctx, "CA_STANDARD"); err != nil {
		t.Fatal(err)
	}
	pol := &computepb.SecurityPolicy{Name: proto.String(n("ag")), Rules: []*computepb.SecurityPolicyRule{rule}}
	if _, err := sps.Insert(ctx, &computepb.InsertSecurityPolicyRequest{Project: project, SecurityPolicyResource: pol}); computeCode(err) != http.StatusBadRequest {
		t.Fatalf("a standard-tier project's address group rule: %v", err)
	}
	if err := setTier(ctx, "CA_ENTERPRISE_PAYGO"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = setTier(context.Background(), "CA_STANDARD") })
	if p, err := projects.Get(ctx, &computepb.GetProjectRequest{Project: project}); err != nil || p.GetCloudArmorTier() != "CA_ENTERPRISE_PAYGO" {
		t.Fatalf("projects.get cloudArmorTier: %v %v", p.GetCloudArmorTier(), err)
	}
	op2, err := sps.Insert(ctx, &computepb.InsertSecurityPolicyRequest{Project: project, SecurityPolicyResource: pol})
	l.do("the security policy", op2, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return sps.Delete(c, &computepb.DeleteSecurityPolicyRequest{Project: project, SecurityPolicy: n("ag")})
	})
	op2, err = l.bss.SetSecurityPolicy(ctx, &computepb.SetSecurityPolicyBackendServiceRequest{Project: project, BackendService: n("app"),
		SecurityPolicyReferenceResource: &computepb.SecurityPolicyReference{SecurityPolicy: proto.String("global/securityPolicies/" + n("ag"))}})
	l.do("setSecurityPolicy", op2, err)
	t.Cleanup(func() {
		if op, err := l.bss.SetSecurityPolicy(context.Background(), &computepb.SetSecurityPolicyBackendServiceRequest{Project: project, BackendService: n("app"),
			SecurityPolicyReferenceResource: &computepb.SecurityPolicyReference{SecurityPolicy: proto.String("")}}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	if code, body, _ := l.get("/x", nil); code != 200 {
		t.Fatalf("127.0.0.1 outside the group: %d %q", code, body)
	}
	aop, err := ags.AddAddressGroupItems(ctx, &networksecuritypb.AddAddressGroupItemsRequest{AddressGroup: ag.GetName(), Items: []string{"127.0.0.1/32"}})
	if err != nil {
		t.Fatal(err)
	}
	if ag, err = aop.Wait(ctx); err != nil || len(ag.GetItems()) != 2 {
		t.Fatalf("AddAddressGroupItems: %v %v", ag, err)
	}
	if code, _, _ := l.get("/x", nil); code != 403 {
		t.Fatalf("127.0.0.1 in the group: %d, want 403", code)
	}
	refs := ags.ListAddressGroupReferences(ctx, &networksecuritypb.ListAddressGroupReferencesRequest{AddressGroup: ag.GetName()})
	if r, err := refs.Next(); err != nil || r.GetRulePriority() != 100 || !strings.HasSuffix(r.GetSecurityPolicy(), "/securityPolicies/"+n("ag")) {
		t.Fatalf("ListAddressGroupReferences: %v %v", r, err)
	}
	if _, err := ags.DeleteAddressGroup(ctx, &networksecuritypb.DeleteAddressGroupRequest{Name: ag.GetName()}); err == nil {
		t.Error("a referenced address group was deleted")
	}
	// Clone into a second group, then empty the first of 127.0.0.1.
	cop, err := ags.CreateAddressGroup(ctx, &networksecuritypb.CreateAddressGroupRequest{Parent: g, AddressGroupId: n("copy"),
		AddressGroup: &networksecuritypb.AddressGroup{Type: networksecuritypb.AddressGroup_IPV4, Capacity: 10}})
	if err != nil {
		t.Fatal(err)
	}
	cp, err := cop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := ags.DeleteAddressGroup(context.Background(), &networksecuritypb.DeleteAddressGroupRequest{Name: cp.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	clop, err := ags.CloneAddressGroupItems(ctx, &networksecuritypb.CloneAddressGroupItemsRequest{AddressGroup: cp.GetName(), SourceAddressGroup: ag.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if cp, err = clop.Wait(ctx); err != nil || len(cp.GetItems()) != 2 {
		t.Fatalf("CloneAddressGroupItems: %v %v", cp, err)
	}
	rop, err := ags.RemoveAddressGroupItems(ctx, &networksecuritypb.RemoveAddressGroupItemsRequest{AddressGroup: ag.GetName(), Items: []string{"127.0.0.1/32"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := l.get("/x", nil); code != 200 {
		t.Fatalf("after RemoveAddressGroupItems: %d", code)
	}
	// Threat intelligence reads Google's feeds: refused.
	ti := &computepb.SecurityPolicy{Name: proto.String(n("ti")), Rules: []*computepb.SecurityPolicyRule{{Priority: proto.Int32(1), Action: proto.String("deny(403)"),
		Match: &computepb.SecurityPolicyRuleMatcher{Expr: &computepb.Expr{Expression: proto.String("evaluateThreatIntelligence('iplist-known-malicious-ips')")}}}}}
	if _, err := sps.Insert(ctx, &computepb.InsertSecurityPolicyRequest{Project: project, SecurityPolicyResource: ti}); computeCode(err) != http.StatusNotImplemented {
		t.Errorf("evaluateThreatIntelligence: %v", err)
	}
}

// backendTLSOrigin is an HTTPS origin on 127.0.0.1 with a certificate
// from ca for name; it asks for a client certificate and answers the SNI
// it got and the client certificate's common name.
func backendTLSOrigin(t *testing.T, ca mtlsCA, name string) *httptest.Server {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca.cert, &k.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cn := ""
		if len(r.TLS.PeerCertificates) > 0 {
			cn = r.TLS.PeerCertificates[0].Subject.CommonName
		}
		fmt.Fprintf(w, "sni=%s client=%s", r.TLS.ServerName, cn)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: k}}, ClientAuth: tls.RequestClientCert}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// pemPair is a certificate and its key as PEM.
func pemPair(t *testing.T, c tls.Certificate) (string, string) {
	t.Helper()
	kd, err := x509.MarshalPKCS8PrivateKey(c.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Certificate[0]})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd}))
}

// covers: google.cloud.networksecurity.v1.NetworkSecurity/CreateBackendAuthenticationConfig, google.cloud.networksecurity.v1.NetworkSecurity/GetBackendAuthenticationConfig, google.cloud.networksecurity.v1.NetworkSecurity/ListBackendAuthenticationConfigs, google.cloud.networksecurity.v1.NetworkSecurity/UpdateBackendAuthenticationConfig, google.cloud.networksecurity.v1.NetworkSecurity/DeleteBackendAuthenticationConfig, compute.backendServices.patch
//
// TestBackendMutualTLSThroughTheGoClient (#2160), the official
// certificatemanager, networksecurity and compute REST clients, as
// load-balancing/docs/backend-mtls-setup sets it up: a trust config of a
// test CA, a CLIENT_AUTH Certificate Manager certificate, a backend
// authentication config of both, and an HTTPS backend service whose
// tlsSettings name it with an SNI. Real requests through the load
// balancer's local port reach an HTTPS origin on 127.0.0.1 whose
// certificate the CA signed: it sees the SNI and the load balancer's
// client certificate; with an SNI the certificate does not name, or a
// trust config of another CA, the load balancer refuses the backend and
// answers 502.
func TestBackendMutualTLSThroughTheGoClient(t *testing.T) {
	h := New(t)
	l := newLB2160(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, "origin %s", r.URL.Path) }))
	ctx, project, n, opts := l.ctx, l.project, l.n, computeOpts(t, h)
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
	g := "projects/" + project + "/locations/global"
	ca, other := newMTLSCA(t, "backend-root"), newMTLSCA(t, "backend-other")
	origin := backendTLSOrigin(t, ca, "backend.example.com")

	trust := func(id string, c mtlsCA) string {
		op, err := cm.CreateTrustConfig(ctx, &certificatemanagerpb.CreateTrustConfigRequest{Parent: g, TrustConfigId: id,
			TrustConfig: &certificatemanagerpb.TrustConfig{TrustStores: []*certificatemanagerpb.TrustConfig_TrustStore{{
				TrustAnchors: []*certificatemanagerpb.TrustConfig_TrustAnchor{{Kind: &certificatemanagerpb.TrustConfig_TrustAnchor_PemCertificate{PemCertificate: c.pem}}}}}}})
		if err != nil {
			t.Fatal(err)
		}
		tc, err := op.Wait(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if op, err := cm.DeleteTrustConfig(context.Background(), &certificatemanagerpb.DeleteTrustConfigRequest{Name: tc.GetName()}); err == nil {
				_ = op.Wait(context.Background())
			}
		})
		return tc.GetName()
	}
	tc, otherTC := trust(n("btc"), ca), trust(n("btc-other"), other)
	crt, key := pemPair(t, ca.client(t, "spiffe://example.org/ns/lb"))
	cop, err := cm.CreateCertificate(ctx, &certificatemanagerpb.CreateCertificateRequest{Parent: g, CertificateId: n("client"),
		Certificate: &certificatemanagerpb.Certificate{Scope: certificatemanagerpb.Certificate_CLIENT_AUTH,
			Type: &certificatemanagerpb.Certificate_SelfManaged{SelfManaged: &certificatemanagerpb.Certificate_SelfManagedCertificate{PemCertificate: crt, PemPrivateKey: key}}}})
	if err != nil {
		t.Fatal(err)
	}
	cert, err := cop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := cm.DeleteCertificate(context.Background(), &certificatemanagerpb.DeleteCertificateRequest{Name: cert.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	bop, err := ns.CreateBackendAuthenticationConfig(ctx, &networksecuritypb.CreateBackendAuthenticationConfigRequest{Parent: g, BackendAuthenticationConfigId: n("bac"),
		BackendAuthenticationConfig: &networksecuritypb.BackendAuthenticationConfig{TrustConfig: tc, ClientCertificate: cert.GetName()}})
	if err != nil {
		t.Fatal(err)
	}
	bac, err := bop.Wait(ctx)
	if err != nil || bac.GetEtag() == "" {
		t.Fatalf("CreateBackendAuthenticationConfig: %v %v", bac, err)
	}
	t.Cleanup(func() {
		if op, err := ns.DeleteBackendAuthenticationConfig(context.Background(), &networksecuritypb.DeleteBackendAuthenticationConfigRequest{Name: bac.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	if _, err := ns.CreateBackendAuthenticationConfig(ctx, &networksecuritypb.CreateBackendAuthenticationConfigRequest{Parent: g, BackendAuthenticationConfigId: n("bad"),
		BackendAuthenticationConfig: &networksecuritypb.BackendAuthenticationConfig{}}); err == nil {
		t.Error("a config without a trust config or PUBLIC_ROOTS was accepted")
	}
	if got, err := ns.GetBackendAuthenticationConfig(ctx, &networksecuritypb.GetBackendAuthenticationConfigRequest{Name: bac.GetName()}); err != nil || got.GetClientCertificate() != cert.GetName() {
		t.Fatalf("GetBackendAuthenticationConfig: %v %v", got, err)
	}
	found := false
	for it := ns.ListBackendAuthenticationConfigs(ctx, &networksecuritypb.ListBackendAuthenticationConfigsRequest{Parent: g}); ; {
		x, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		found = found || x.GetName() == bac.GetName()
	}
	if !found {
		t.Error("ListBackendAuthenticationConfigs: the config is missing")
	}

	// The HTTPS backend service of the origin, behind the URL map.
	host, port, _ := net.SplitHostPort(origin.Listener.Addr().String())
	p, _ := strconv.Atoi(port)
	zone := "us-central1-a"
	negs, _ := computeapi.NewNetworkEndpointGroupsRESTClient(ctx, opts...)
	hcs, _ := computeapi.NewHealthChecksRESTClient(ctx, opts...)
	op, err := negs.Insert(ctx, &computepb.InsertNetworkEndpointGroupRequest{Project: project, Zone: zone,
		NetworkEndpointGroupResource: &computepb.NetworkEndpointGroup{Name: proto.String(n("tlsorigin")),
			NetworkEndpointType: proto.String("NON_GCP_PRIVATE_IP_PORT"), Network: proto.String("global/networks/default"), DefaultPort: proto.Int32(int32(p))}})
	l.do("the HTTPS origin's network endpoint group", op, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return negs.Delete(c, &computepb.DeleteNetworkEndpointGroupRequest{Project: project, Zone: zone, NetworkEndpointGroup: n("tlsorigin")})
	})
	op, err = negs.AttachNetworkEndpoints(ctx, &computepb.AttachNetworkEndpointsNetworkEndpointGroupRequest{Project: project, Zone: zone,
		NetworkEndpointGroup: n("tlsorigin"), NetworkEndpointGroupsAttachEndpointsRequestResource: &computepb.NetworkEndpointGroupsAttachEndpointsRequest{
			NetworkEndpoints: []*computepb.NetworkEndpoint{{IpAddress: proto.String(host), Port: proto.Int32(int32(p))}}}})
	l.do("the HTTPS origin's endpoint", op, err)
	// Health checks do not authenticate backends: a TCP check.
	op, err = hcs.Insert(ctx, &computepb.InsertHealthCheckRequest{Project: project, HealthCheckResource: &computepb.HealthCheck{
		Name: proto.String(n("tcp")), Type: proto.String("TCP"), CheckIntervalSec: proto.Int32(2), TimeoutSec: proto.Int32(2),
		TcpHealthCheck: &computepb.TCPHealthCheck{PortSpecification: proto.String("USE_SERVING_PORT")}}})
	l.do("the TCP health check", op, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return hcs.Delete(c, &computepb.DeleteHealthCheckRequest{Project: project, HealthCheck: n("tcp")})
	})
	ref := "//networksecurity.googleapis.com/" + bac.GetName()
	op, err = l.bss.Insert(ctx, &computepb.InsertBackendServiceRequest{Project: project, BackendServiceResource: &computepb.BackendService{
		Name: proto.String(n("tls")), Protocol: proto.String("HTTPS"), LoadBalancingScheme: proto.String("EXTERNAL_MANAGED"),
		HealthChecks: []string{"global/healthChecks/" + n("tcp")},
		Backends: []*computepb.Backend{{Group: proto.String("zones/" + zone + "/networkEndpointGroups/" + n("tlsorigin")),
			BalancingMode: proto.String("RATE"), MaxRatePerEndpoint: proto.Float32(100)}},
		TlsSettings: &computepb.BackendServiceTlsSettings{Sni: proto.String("backend.example.com"), AuthenticationConfig: proto.String(ref)}}})
	l.do("the HTTPS backend service", op, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return l.bss.Delete(c, &computepb.DeleteBackendServiceRequest{Project: project, BackendService: n("tls")})
	})
	if got, err := l.bss.Get(ctx, &computepb.GetBackendServiceRequest{Project: project, BackendService: n("tls")}); err != nil ||
		got.GetTlsSettings().GetAuthenticationConfig() != ref {
		t.Fatalf("the backend service's tlsSettings: %v %v", got.GetTlsSettings(), err)
	}
	if _, err := ns.DeleteBackendAuthenticationConfig(ctx, &networksecuritypb.DeleteBackendAuthenticationConfigRequest{Name: bac.GetName()}); err == nil {
		t.Error("a backend authentication config in use was deleted")
	}
	op, err = l.ums.Patch(ctx, &computepb.PatchUrlMapRequest{Project: project, UrlMap: n("map"),
		UrlMapResource: &computepb.UrlMap{DefaultService: proto.String("global/backendServices/" + n("tls"))}})
	l.do("the URL map to the HTTPS backend", op, err)
	t.Cleanup(func() {
		if op, err := l.ums.Patch(context.Background(), &computepb.PatchUrlMapRequest{Project: project, UrlMap: n("map"),
			UrlMapResource: &computepb.UrlMap{DefaultService: proto.String("global/backendServices/" + n("app"))}}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	var code int
	var body string
	waitFor(t, "the HTTPS origin through backend mTLS", 2*time.Minute, func() bool {
		code, body, _ = l.get("/", nil)
		return code == 200
	})
	if body != "sni=backend.example.com client=client" {
		t.Fatalf("backend mTLS: %q", body)
	}
	patchTLS := func(ts *computepb.BackendServiceTlsSettings) {
		t.Helper()
		op, err := l.bss.Patch(ctx, &computepb.PatchBackendServiceRequest{Project: project, BackendService: n("tls"),
			BackendServiceResource: &computepb.BackendService{TlsSettings: ts}})
		l.do("tlsSettings", op, err)
	}
	patchTLS(&computepb.BackendServiceTlsSettings{Sni: proto.String("other.example.com"), AuthenticationConfig: proto.String(ref)})
	if code, body, _ = l.get("/", nil); code != 502 {
		t.Errorf("an SNI the backend's certificate does not name: %d %q", code, body)
	}
	patchTLS(&computepb.BackendServiceTlsSettings{Sni: proto.String("other.example.com"), AuthenticationConfig: proto.String(ref),
		SubjectAltNames: []*computepb.BackendServiceTlsSettingsSubjectAltName{{DnsName: proto.String("backend.example.com")}}})
	if code, body, _ = l.get("/", nil); code != 200 || body != "sni=other.example.com client=client" {
		t.Errorf("subjectAltNames over the SNI: %d %q", code, body)
	}
	// Another CA's trust config: the backend is refused.
	upd, err := ns.UpdateBackendAuthenticationConfig(ctx, &networksecuritypb.UpdateBackendAuthenticationConfigRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"trust_config"}},
		BackendAuthenticationConfig: &networksecuritypb.BackendAuthenticationConfig{Name: bac.GetName(), TrustConfig: otherTC}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upd.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if code, body, _ = l.get("/", nil); code != 502 {
		t.Errorf("a backend of another CA: %d %q", code, body)
	}
}
