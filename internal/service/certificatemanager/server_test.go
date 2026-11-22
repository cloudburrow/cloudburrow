package certificatemanager

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	cmpb "cloud.google.com/go/certificatemanager/apiv1/certificatemanagerpb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

var _ compute.CertificateMaps = (*Server)(nil)

// selfSigned is a P-256 certificate for names and its key, as PEM.
func selfSigned(t *testing.T, names ...string) (string, string) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kd, _ := x509.MarshalECPrivateKey(k)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}))
}

// localCA issues as compute.Server.IssueLocalCertificate does.
type localCA struct{ n int }

func (c *localCA) IssueLocalCertificate(domains []string) (string, string, time.Time, error) {
	c.n++
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(int64(c.n + 10)), Subject: pkix.Name{CommonName: domains[0]}, DNSNames: domains,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(0, 0, 90)}
	der, _ := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	kd, _ := x509.MarshalECPrivateKey(k)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd})), tpl.NotAfter, nil
}

func code(err error) codes.Code { return status.Code(err) }

func wait(t *testing.T, op *longrunningpb.Operation, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if !op.GetDone() || op.GetError() != nil {
		t.Fatalf("operation: %v", op)
	}
}

const g = "projects/p/locations/global"

func TestCertificatesMapsAndHandshakes(t *testing.T) {
	ctx := context.Background()
	state := filepath.Join(t.TempDir(), "cm.json")
	s, err := New(Options{State: state})
	if err != nil {
		t.Fatal(err)
	}
	ca := &localCA{}
	s.UseIssuer(ca)
	var served []Target
	s.UseTargets(func(string) []Target { return served })

	crt, key := selfSigned(t, "www.example.com")
	op, err := s.CreateCertificate(ctx, &cmpb.CreateCertificateRequest{Parent: g, CertificateId: "www",
		Certificate: &cmpb.Certificate{Type: &cmpb.Certificate_SelfManaged{SelfManaged: &cmpb.Certificate_SelfManagedCertificate{PemCertificate: crt, PemPrivateKey: key}}}})
	wait(t, op, err)
	c, err := s.GetCertificate(ctx, &cmpb.GetCertificateRequest{Name: g + "/certificates/www"})
	if err != nil || c.GetPemCertificate() != crt || len(c.GetSanDnsnames()) != 1 || c.GetSelfManaged().GetPemPrivateKey() != "" {
		t.Fatalf("self-managed: %v %v", c, err)
	}
	// A key of another certificate is refused.
	_, otherKey := selfSigned(t, "x.example.com")
	if _, err := s.CreateCertificate(ctx, &cmpb.CreateCertificateRequest{Parent: g, CertificateId: "bad",
		Certificate: &cmpb.Certificate{Type: &cmpb.Certificate_SelfManaged{SelfManaged: &cmpb.Certificate_SelfManagedCertificate{PemCertificate: crt, PemPrivateKey: otherKey}}}}); code(err) != codes.InvalidArgument {
		t.Errorf("mismatched key: %v", err)
	}
	// Load-balancer authorization: no wildcards, at most 5 domains.
	if _, err := s.CreateCertificate(ctx, &cmpb.CreateCertificateRequest{Parent: g, CertificateId: "wild",
		Certificate: &cmpb.Certificate{Type: &cmpb.Certificate_Managed{Managed: &cmpb.Certificate_ManagedCertificate{Domains: []string{"*.example.com"}}}}}); code(err) != codes.InvalidArgument {
		t.Errorf("wildcard without DNS authorization: %v", err)
	}
	op, err = s.CreateCertificate(ctx, &cmpb.CreateCertificateRequest{Parent: g, CertificateId: "lb",
		Certificate: &cmpb.Certificate{Type: &cmpb.Certificate_Managed{Managed: &cmpb.Certificate_ManagedCertificate{Domains: []string{"lb.example.com"}}}}})
	wait(t, op, err)
	if c, _ := s.GetCertificate(ctx, &cmpb.GetCertificateRequest{Name: g + "/certificates/lb"}); c.GetManaged().GetState() != cmpb.Certificate_ManagedCertificate_PROVISIONING {
		t.Fatalf("unserved LB-authorized certificate: %v", c)
	}
	// DNS authorization: the CNAME, and a wildcard certificate ACTIVE at once.
	op, err = s.CreateDnsAuthorization(ctx, &cmpb.CreateDnsAuthorizationRequest{Parent: g, DnsAuthorizationId: "ex",
		DnsAuthorization: &cmpb.DnsAuthorization{Domain: "example.com"}})
	wait(t, op, err)
	a, _ := s.GetDnsAuthorization(ctx, &cmpb.GetDnsAuthorizationRequest{Name: g + "/dnsAuthorizations/ex"})
	rr := a.GetDnsResourceRecord()
	if a.GetType() != cmpb.DnsAuthorization_FIXED_RECORD || rr.GetName() != "_acme-challenge.example.com." || rr.GetType() != "CNAME" ||
		!strings.HasSuffix(rr.GetData(), ".4.authorize.certificatemanager.goog.") {
		t.Fatalf("dns authorization: %v", a)
	}
	op, err = s.CreateCertificate(ctx, &cmpb.CreateCertificateRequest{Parent: g, CertificateId: "wild",
		Certificate: &cmpb.Certificate{Type: &cmpb.Certificate_Managed{Managed: &cmpb.Certificate_ManagedCertificate{
			Domains: []string{"*.example.com", "example.com"}, DnsAuthorizations: []string{g + "/dnsAuthorizations/ex"}}}}})
	wait(t, op, err)
	c, _ = s.GetCertificate(ctx, &cmpb.GetCertificateRequest{Name: g + "/certificates/wild"})
	if c.GetManaged().GetState() != cmpb.Certificate_ManagedCertificate_ACTIVE || c.GetPemCertificate() == "" ||
		c.GetManaged().GetAuthorizationAttemptInfo()[0].GetState() != cmpb.Certificate_ManagedCertificate_AuthorizationAttemptInfo_AUTHORIZED {
		t.Fatalf("DNS-authorized certificate: %v", c)
	}
	if _, err := s.DeleteDnsAuthorization(ctx, &cmpb.DeleteDnsAuthorizationRequest{Name: g + "/dnsAuthorizations/ex"}); code(err) != codes.FailedPrecondition {
		t.Errorf("deleting a DNS authorization in use: %v", err)
	}

	// A map: www.example.com, a wildcard and the PRIMARY entry.
	op, err = s.CreateCertificateMap(ctx, &cmpb.CreateCertificateMapRequest{Parent: g, CertificateMapId: "m"})
	wait(t, op, err)
	m := g + "/certificateMaps/m"
	for id, e := range map[string]*cmpb.CertificateMapEntry{
		"www":     {Match: &cmpb.CertificateMapEntry_Hostname{Hostname: "www.example.com"}, Certificates: []string{g + "/certificates/www"}},
		"wild":    {Match: &cmpb.CertificateMapEntry_Hostname{Hostname: "*.example.com"}, Certificates: []string{g + "/certificates/wild"}},
		"primary": {Match: &cmpb.CertificateMapEntry_Matcher_{Matcher: cmpb.CertificateMapEntry_PRIMARY}, Certificates: []string{g + "/certificates/lb"}},
	} {
		op, err := s.CreateCertificateMapEntry(ctx, &cmpb.CreateCertificateMapEntryRequest{Parent: m, CertificateMapEntryId: id, CertificateMapEntry: e})
		wait(t, op, err)
	}
	if _, err := s.CreateCertificateMapEntry(ctx, &cmpb.CreateCertificateMapEntryRequest{Parent: m, CertificateMapEntryId: "p2",
		CertificateMapEntry: &cmpb.CertificateMapEntry{Match: &cmpb.CertificateMapEntry_Matcher_{Matcher: cmpb.CertificateMapEntry_PRIMARY}}}); code(err) != codes.AlreadyExists {
		t.Errorf("a second PRIMARY entry: %v", err)
	}
	if _, err := s.CreateCertificateMapEntry(ctx, &cmpb.CreateCertificateMapEntryRequest{Parent: m, CertificateMapEntryId: "five",
		CertificateMapEntry: &cmpb.CertificateMapEntry{Match: &cmpb.CertificateMapEntry_Hostname{Hostname: "five.example.org"},
			Certificates: []string{g + "/certificates/www", g + "/certificates/www", g + "/certificates/www", g + "/certificates/www", g + "/certificates/www"}}}); code(err) != codes.InvalidArgument {
		t.Errorf("five certificates: %v", err)
	}
	if !s.MapExists("//certificatemanager.googleapis.com/"+m) || s.MapExists(g+"/certificateMaps/none") {
		t.Error("MapExists")
	}
	cn := func(sni string) string {
		cs := s.MapCertificates(m, sni)
		if len(cs) == 0 || cs[0].Leaf == nil {
			return ""
		}
		return cs[0].Leaf.Subject.CommonName
	}
	if got := cn("www.example.com"); got != "www.example.com" {
		t.Errorf("exact: %q", got)
	}
	if got := cn("api.example.com"); got != "*.example.com" {
		t.Errorf("wildcard: %q", got)
	}
	// The PRIMARY entry's LB-authorized certificate is issued once a
	// handshake is served by it: the first gets none, a later one gets it.
	got := cn("other.test")
	for end := time.Now().Add(5 * time.Second); got == "" && time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		got = cn("other.test")
	}
	if got != "lb.example.com" {
		t.Errorf("primary: %q", got)
	}
	if c, _ := s.GetCertificate(ctx, &cmpb.GetCertificateRequest{Name: g + "/certificates/lb"}); c.GetManaged().GetState() != cmpb.Certificate_ManagedCertificate_ACTIVE ||
		len(c.GetUsedBy()) != 1 {
		t.Errorf("served LB-authorized certificate: %v", c)
	}
	// gclbTargets, and the refusals while the map is used.
	served = []Target{{Map: "//certificatemanager.googleapis.com/" + m, HTTPSProxy: "projects/p/global/targetHttpsProxies/px",
		IPs: []IPConfig{{IP: "127.0.0.1", Ports: []uint32{443}}}}}
	gm, _ := s.GetCertificateMap(ctx, &cmpb.GetCertificateMapRequest{Name: m})
	if len(gm.GetGclbTargets()) != 1 || gm.GetGclbTargets()[0].GetTargetHttpsProxy() != "projects/p/global/targetHttpsProxies/px" {
		t.Errorf("gclbTargets: %v", gm)
	}
	if _, err := s.DeleteCertificateMap(ctx, &cmpb.DeleteCertificateMapRequest{Name: m}); code(err) != codes.FailedPrecondition {
		t.Errorf("deleting a map in use: %v", err)
	}
	if _, err := s.DeleteCertificate(ctx, &cmpb.DeleteCertificateRequest{Name: g + "/certificates/www"}); code(err) != codes.FailedPrecondition {
		t.Errorf("deleting a certificate in use: %v", err)
	}
	// Update: labels; managed fields are immutable.
	op, err = s.UpdateCertificateMapEntry(ctx, &cmpb.UpdateCertificateMapEntryRequest{
		CertificateMapEntry: &cmpb.CertificateMapEntry{Name: m + "/certificateMapEntries/www", Labels: map[string]string{"a": "b"}},
		UpdateMask:          &fieldmaskpb.FieldMask{Paths: []string{"labels"}}})
	wait(t, op, err)
	if _, err := s.UpdateCertificate(ctx, &cmpb.UpdateCertificateRequest{Certificate: &cmpb.Certificate{Name: g + "/certificates/lb"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"managed"}}}); code(err) != codes.InvalidArgument {
		t.Errorf("updating managed: %v", err)
	}

	// State survives a restart.
	s2, err := New(Options{State: state})
	if err != nil {
		t.Fatal(err)
	}
	if cs := s2.MapCertificates(m, "www.example.com"); len(cs) != 1 {
		t.Errorf("after restore: %d certificates", len(cs))
	}
	l, err := s2.ListCertificates(ctx, &cmpb.ListCertificatesRequest{Parent: g, Filter: `name:"wild"`})
	if err != nil || len(l.GetCertificates()) != 1 {
		t.Errorf("filtered list: %v %v", l, err)
	}
}

func TestIssuanceConfigsAndTrustConfigs(t *testing.T) {
	ctx := context.Background()
	s, _ := New(Options{})
	s.UseIssuer(&localCA{})
	ic := &cmpb.CertificateIssuanceConfig{Lifetime: durationpb.New(30 * 24 * time.Hour), RotationWindowPercentage: 100,
		KeyAlgorithm: cmpb.CertificateIssuanceConfig_ECDSA_P256,
		CertificateAuthorityConfig: &cmpb.CertificateIssuanceConfig_CertificateAuthorityConfig{Kind: &cmpb.CertificateIssuanceConfig_CertificateAuthorityConfig_CertificateAuthorityServiceConfig_{
			CertificateAuthorityServiceConfig: &cmpb.CertificateIssuanceConfig_CertificateAuthorityConfig_CertificateAuthorityServiceConfig{CaPool: "projects/p/locations/us-central1/caPools/pool"}}}}
	if _, err := s.CreateCertificateIssuanceConfig(ctx, &cmpb.CreateCertificateIssuanceConfigRequest{Parent: g, CertificateIssuanceConfigId: "ic", CertificateIssuanceConfig: ic}); code(err) != codes.InvalidArgument {
		t.Errorf("rotation 100: %v", err)
	}
	ic.RotationWindowPercentage = 34
	op, err := s.CreateCertificateIssuanceConfig(ctx, &cmpb.CreateCertificateIssuanceConfigRequest{Parent: g, CertificateIssuanceConfigId: "ic", CertificateIssuanceConfig: ic})
	wait(t, op, err)
	op, err = s.CreateCertificate(ctx, &cmpb.CreateCertificateRequest{Parent: g, CertificateId: "private",
		Certificate: &cmpb.Certificate{Type: &cmpb.Certificate_Managed{Managed: &cmpb.Certificate_ManagedCertificate{
			Domains: []string{"*.internal.example"}, IssuanceConfig: g + "/certificateIssuanceConfigs/ic"}}}})
	wait(t, op, err)
	if c, _ := s.GetCertificate(ctx, &cmpb.GetCertificateRequest{Name: g + "/certificates/private"}); c.GetManaged().GetState() != cmpb.Certificate_ManagedCertificate_ACTIVE {
		t.Errorf("private PKI certificate: %v", c)
	}
	if got := s.Certificates([]string{g + "/certificates/private"}); len(got) != 1 {
		t.Errorf("Certificates: %d", len(got))
	}
	crt, _ := selfSigned(t, "ca.example")
	op, err = s.CreateTrustConfig(ctx, &cmpb.CreateTrustConfigRequest{Parent: g, TrustConfigId: "tc", TrustConfig: &cmpb.TrustConfig{
		TrustStores: []*cmpb.TrustConfig_TrustStore{{TrustAnchors: []*cmpb.TrustConfig_TrustAnchor{{Kind: &cmpb.TrustConfig_TrustAnchor_PemCertificate{PemCertificate: crt}}}}}}})
	wait(t, op, err)
	tc, _ := s.GetTrustConfig(ctx, &cmpb.GetTrustConfigRequest{Name: g + "/trustConfigs/tc"})
	if tc.GetEtag() == "" {
		t.Error("trust config etag")
	}
	if _, err := s.DeleteTrustConfig(ctx, &cmpb.DeleteTrustConfigRequest{Name: tc.GetName(), Etag: "stale"}); code(err) != codes.Aborted {
		t.Errorf("stale etag: %v", err)
	}
	if _, err := s.CreateCertificateMap(ctx, &cmpb.CreateCertificateMapRequest{Parent: "projects/p/locations/us-central1", CertificateMapId: "m"}); code(err) != codes.InvalidArgument {
		t.Errorf("regional map: %v", err)
	}
}

// TestRESTOnTheComputePort: the JSON API's routes, and Handles' split of
// /v1/projects/P/locations with Serverless VPC Access.
func TestRESTOnTheComputePort(t *testing.T) {
	s, _ := New(Options{})
	srv := httptest.NewServer(NewRESTHandler(s))
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/"+g+"/certificateMaps?certificateMapId=web", "application/json", strings.NewReader(`{"description":"d"}`))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("create: %v %v", resp, err)
	}
	resp.Body.Close()
	op := ""
	for k := range s.ops {
		op = k
	}
	req := func(path string) *http.Request { r, _ := http.NewRequest("GET", path, nil); return r }
	for path, want := range map[string]bool{
		"/v1/" + g + "/certificateMaps/web":                 true,
		"/v1/projects/p/locations/us-central1/certificates": true,
		"/v1/" + op: true,
		"/v1/projects/p/locations/us-central1/operations/x":                  false,
		"/v1/projects/p/locations/global":                                    true,
		"/v1/projects/p/locations":                                           false,
		"/v1/projects/p/locations/us-central1/connectors":                    false,
		"/v1/projects/p/locations/us-central1":                               false,
		"/compute/v1/projects/p/global/targetHttpsProxies":                   false,
		"/v1/" + g + "/certificateMaps/web/certificateMapEntries?pageSize=1": true,
	} {
		if got := s.Handles(req(path)); got != want {
			t.Errorf("Handles(%s) = %v", path, got)
		}
	}
	resp, _ = http.Get(srv.URL + "/v1/" + g + "/certificateMaps/web")
	if resp.StatusCode != 200 {
		t.Errorf("get: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// lockingCA takes a lock its caller may hold, as the Compute Engine API's
// IssueLocalCertificate takes compute's.
type lockingCA struct {
	mu *sync.Mutex
	localCA
}

func (c *lockingCA) IssueLocalCertificate(domains []string) (string, string, time.Time, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.localCA.IssueLocalCertificate(domains)
}

// TestNoDeadlockWithComputesLock: Compute asks for a map's certificates
// while it holds its own lock, which the CA takes; the answer never waits
// for the CA.
func TestNoDeadlockWithComputesLock(t *testing.T) {
	ctx := context.Background()
	var computeMu sync.Mutex
	s, _ := New(Options{})
	s.UseIssuer(&lockingCA{mu: &computeMu})
	op, err := s.CreateCertificate(ctx, &cmpb.CreateCertificateRequest{Parent: g, CertificateId: "lb",
		Certificate: &cmpb.Certificate{Type: &cmpb.Certificate_Managed{Managed: &cmpb.Certificate_ManagedCertificate{Domains: []string{"lb.example.com"}}}}})
	wait(t, op, err)
	op, err = s.CreateCertificateMap(ctx, &cmpb.CreateCertificateMapRequest{Parent: g, CertificateMapId: "m"})
	wait(t, op, err)
	op, err = s.CreateCertificateMapEntry(ctx, &cmpb.CreateCertificateMapEntryRequest{Parent: g + "/certificateMaps/m", CertificateMapEntryId: "p",
		CertificateMapEntry: &cmpb.CertificateMapEntry{Match: &cmpb.CertificateMapEntry_Matcher_{Matcher: cmpb.CertificateMapEntry_PRIMARY}, Certificates: []string{g + "/certificates/lb"}}})
	wait(t, op, err)
	done := make(chan int)
	computeMu.Lock()
	go func() {
		done <- len(s.MapCertificates(g+"/certificateMaps/m", "")) + len(s.Certificates([]string{g + "/certificates/lb"}))
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("MapCertificates waited for the CA's lock")
	}
	computeMu.Unlock()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if len(s.MapCertificates(g+"/certificateMaps/m", "")) == 1 {
			return
		}
	}
	t.Fatal("the certificate was never issued")
}
