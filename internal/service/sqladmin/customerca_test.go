package sqladmin

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // Cloud SQL names certificates by SHA-1 fingerprint
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/security/privateca/apiv1/privatecapb"
	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/cloudburrow/cloudburrow/internal/service/privateca"
)

// testCAS is CertificateAuthorityService over an in-memory privateca
// server, as cmd/cloudburrow's sqlAdminCAS is over the instance's.
type testCAS struct{ api *privateca.API }

func (c testCAS) EnabledCAs(ctx context.Context, pool string) (int, error) {
	if _, err := c.api.GetCaPool(ctx, &privatecapb.GetCaPoolRequest{Name: pool}); err != nil {
		return 0, err
	}
	resp, err := c.api.ListCertificateAuthorities(ctx, &privatecapb.ListCertificateAuthoritiesRequest{Parent: pool})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, ca := range resp.GetCertificateAuthorities() {
		if ca.GetState() == privatecapb.CertificateAuthority_ENABLED {
			n++
		}
	}
	return n, nil
}

func (c testCAS) IssueCertificate(ctx context.Context, pool string, r CertificateRequest) (IssuedCertificate, error) {
	cert, err := c.api.CreateCertificate(ctx, &privatecapb.CreateCertificateRequest{Parent: pool, CertificateId: r.ID,
		Certificate: &privatecapb.Certificate{Lifetime: durationpb.New(r.Lifetime),
			CertificateConfig: &privatecapb.Certificate_Config{Config: &privatecapb.CertificateConfig{
				PublicKey: &privatecapb.PublicKey{Key: r.PublicKeyPEM, Format: privatecapb.PublicKey_PEM},
				SubjectConfig: &privatecapb.CertificateConfig_SubjectConfig{Subject: &privatecapb.Subject{CommonName: r.CommonName},
					SubjectAltName: &privatecapb.SubjectAltNames{DnsNames: r.DNSNames}},
				X509Config: &privatecapb.X509Parameters{CaOptions: &privatecapb.X509Parameters_CaOptions{IsCa: proto.Bool(false)},
					KeyUsage: &privatecapb.KeyUsage{BaseKeyUsage: &privatecapb.KeyUsage_KeyUsageOptions{DigitalSignature: true},
						ExtendedKeyUsage: &privatecapb.KeyUsage_ExtendedKeyUsageOptions{ServerAuth: true}}},
			}}}})
	if err != nil {
		return IssuedCertificate{}, err
	}
	return IssuedCertificate{Name: cert.GetName(), CertPEM: cert.GetPemCertificate(), ChainPEM: cert.GetPemCertificateChain()}, nil
}

func caConfig(cn string) *privatecapb.CertificateConfig {
	return &privatecapb.CertificateConfig{
		SubjectConfig: &privatecapb.CertificateConfig_SubjectConfig{Subject: &privatecapb.Subject{CommonName: cn, Organization: "Example"}},
		X509Config: &privatecapb.X509Parameters{CaOptions: &privatecapb.X509Parameters_CaOptions{IsCa: proto.Bool(true)},
			KeyUsage: &privatecapb.KeyUsage{BaseKeyUsage: &privatecapb.KeyUsage_KeyUsageOptions{CertSign: true, CrlSign: true}}},
	}
}

// casHierarchy makes a root CA in pool roots and a subordinate CA under it
// in pool issuing, both enabled, and an empty pool, all in us-central1;
// it returns the root's certificate.
func casHierarchy(t *testing.T, api *privateca.API) *x509.Certificate {
	t.Helper()
	ctx := context.Background()
	loc := "projects/proj/locations/us-central1"
	for _, p := range []string{"roots", "issuing", "empty"} {
		if _, err := api.CreateCaPool(ctx, &privatecapb.CreateCaPoolRequest{Parent: loc, CaPoolId: p,
			CaPool: &privatecapb.CaPool{Tier: privatecapb.CaPool_ENTERPRISE}}); err != nil {
			t.Fatal(err)
		}
	}
	key := &privatecapb.CertificateAuthority_KeyVersionSpec{KeyVersion: &privatecapb.CertificateAuthority_KeyVersionSpec_Algorithm{
		Algorithm: privatecapb.CertificateAuthority_EC_P256_SHA256}}
	root := loc + "/caPools/roots/certificateAuthorities/root"
	if _, err := api.CreateCertificateAuthority(ctx, &privatecapb.CreateCertificateAuthorityRequest{Parent: loc + "/caPools/roots",
		CertificateAuthorityId: "root", CertificateAuthority: &privatecapb.CertificateAuthority{Type: privatecapb.CertificateAuthority_SELF_SIGNED,
			Config: caConfig("Example Root"), KeySpec: key, Lifetime: durationpb.New(10 * 365 * 24 * time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.EnableCertificateAuthority(ctx, &privatecapb.EnableCertificateAuthorityRequest{Name: root}); err != nil {
		t.Fatal(err)
	}
	sub := loc + "/caPools/issuing/certificateAuthorities/sub"
	if _, err := api.CreateCertificateAuthority(ctx, &privatecapb.CreateCertificateAuthorityRequest{Parent: loc + "/caPools/issuing",
		CertificateAuthorityId: "sub", CertificateAuthority: &privatecapb.CertificateAuthority{Type: privatecapb.CertificateAuthority_SUBORDINATE,
			Config: caConfig("Example Issuing"), KeySpec: key, Lifetime: durationpb.New(5 * 365 * 24 * time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	csr, err := api.FetchCertificateAuthorityCsr(ctx, &privatecapb.FetchCertificateAuthorityCsrRequest{Name: sub})
	if err != nil {
		t.Fatal(err)
	}
	signed, err := api.CreateCertificate(ctx, &privatecapb.CreateCertificateRequest{Parent: loc + "/caPools/roots", CertificateId: "sub",
		Certificate: &privatecapb.Certificate{Lifetime: durationpb.New(5 * 365 * 24 * time.Hour),
			CertificateConfig: &privatecapb.Certificate_PemCsr{PemCsr: csr.GetPemCsr()}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.ActivateCertificateAuthority(ctx, &privatecapb.ActivateCertificateAuthorityRequest{Name: sub,
		PemCaCertificate: signed.GetPemCertificate(), SubordinateConfig: &privatecapb.SubordinateConfig{
			SubordinateConfig: &privatecapb.SubordinateConfig_CertificateAuthority{CertificateAuthority: root}}}); err != nil {
		t.Fatal(err)
	}
	if ca, _ := api.GetCertificateAuthority(ctx, &privatecapb.GetCertificateAuthorityRequest{Name: sub}); ca.GetState() != privatecapb.CertificateAuthority_ENABLED {
		if _, err := api.EnableCertificateAuthority(ctx, &privatecapb.EnableCertificateAuthorityRequest{Name: sub}); err != nil {
			t.Fatal(err)
		}
	}
	rc, err := api.GetCertificateAuthority(ctx, &privatecapb.GetCertificateAuthorityRequest{Name: root})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := pem.Decode([]byte(rc.GetPemCaCertificates()[0]))
	cert, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// peerChain is the certificates the listener presents for instance, over
// a TLS connection that trusts the connect settings' serverCaCert, as the
// connectors do.
func peerChain(t *testing.T, svc *sqladmin.Service, connAddr, instance string) []*x509.Certificate {
	t.Helper()
	cs, err := svc.Connect.Get("proj", instance).Do()
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(cs.ServerCaCert.Cert)) {
		t.Fatal("serverCaCert holds no certificate")
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	eph, err := svc.Connect.GenerateEphemeralCert("proj", instance, &sqladmin.GenerateEphemeralCertRequest{
		PublicKey: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))}).Do()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := pem.Decode([]byte(eph.EphemeralCert.Cert))
	c, err := tls.Dial("tcp", connAddr, &tls.Config{
		MinVersion: tls.VersionTLS13, RootCAs: pool, ServerName: strings.TrimSuffix(cs.DnsNames[0].Name, "."),
		Certificates: []tls.Certificate{{Certificate: [][]byte{b.Bytes}, PrivateKey: key}},
	})
	if err != nil {
		t.Fatalf("TLS to %s: %v", instance, err)
	}
	defer c.Close()
	return c.ConnectionState().PeerCertificates
}

// verifiesToRoot checks that the presented chain verifies against root
// alone, the presented intermediates as intermediates.
func verifiesToRoot(t *testing.T, chain []*x509.Certificate, root *x509.Certificate, dns string) {
	t.Helper()
	roots, inter := x509.NewCertPool(), x509.NewCertPool()
	roots.AddCert(root)
	for _, c := range chain[1:] {
		inter.AddCert(c)
	}
	got, err := chain[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, DNSName: dns,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	if err != nil {
		t.Fatalf("the server certificate does not verify to the pool's root: %v", err)
	}
	if last := got[0][len(got[0])-1]; !last.Equal(root) {
		t.Fatalf("the chain ends at %s, not the root", last.Subject)
	}
}

func fp(c *x509.Certificate) string {
	s := sha1.Sum(c.Raw) //nolint:gosec
	return hex.EncodeToString(s[:])
}

// TestSQLCustomerManagedCA (#1834): a CUSTOMER_MANAGED_CAS_CA instance's
// server certificate is issued by its serverCaPool's CA, a subordinate
// under a root of another pool, as an EC P-384 certificate with the
// instance's DNS name; the listener presents it with the subordinate, so
// it verifies to the root; it is a certificate of the pool; rotation
// requests a new one from the pool; the pool is checked as Cloud SQL
// documents; and the instance cannot go back to the internal CA.
func TestSQLCustomerManagedCA(t *testing.T) {
	cas, err := privateca.NewServer("")
	if err != nil {
		t.Fatal(err)
	}
	root := casHierarchy(t, cas.API())
	srv := NewServer("127.0.0.1:0", newFake())
	if err := srv.EnableConnector("127.0.0.1:0", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	srv.EnableInstanceWrites(newFakeProv(), "")
	srv.UseCertificateAuthorityService(testCAS{api: cas.API()})
	ctx := context.Background()
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop(ctx)
	connAddr := srv.conn.ln.Addr().String()
	api := "http://" + srv.Addr() + "/"
	svc, err := sqladmin.NewService(ctx, option.WithEndpoint(api), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	base := api + "v1/projects/proj/instances"
	pool := "projects/proj/locations/us-central1/caPools/issuing"
	body := func(name, version, ic string) string {
		return `{"name":"` + name + `","databaseVersion":"` + version + `","settings":{"tier":"t","ipConfiguration":{` + ic + `}}}`
	}
	for _, c := range []struct {
		ic, version string
		code        int
		want        string
	}{
		{`"serverCaMode":"CUSTOMER_MANAGED_CAS_CA"`, "POSTGRES_17", 400, "serverCaPool"},
		{`"serverCaMode":"CUSTOMER_MANAGED_CAS_CA","serverCaPool":"issuing"`, "POSTGRES_17", 400, "Invalid format for server CA pool"},
		{`"serverCaMode":"CUSTOMER_MANAGED_CAS_CA","serverCaPool":"projects/proj/locations/europe-west1/caPools/issuing"`, "POSTGRES_17", 400,
			"must be in the same region as the instance"},
		{`"serverCaMode":"CUSTOMER_MANAGED_CAS_CA","serverCaPool":"projects/proj/locations/us-central1/caPools/nope"`, "POSTGRES_17", 404, "not found"},
		{`"serverCaMode":"CUSTOMER_MANAGED_CAS_CA","serverCaPool":"projects/proj/locations/us-central1/caPools/empty"`, "POSTGRES_17", 400,
			"There are no enabled CAs in the CaPool"},
		{`"serverCaMode":"GOOGLE_MANAGED_CAS_CA","serverCaPool":"` + pool + `"`, "POSTGRES_17", 400, "serverCaPool"},
		{`"serverCertificateRotationMode":"SOMETIMES"`, "POSTGRES_17", 400, "serverCertificateRotationMode"},
	} {
		if code, resp := call(t, "POST", base, body("bad", c.version, c.ic)); code != c.code || !strings.Contains(resp, c.want) {
			t.Errorf("insert with %s = %d %s; want %d %q", c.ic, code, resp, c.code, c.want)
		}
	}
	if code, resp := call(t, "POST", base, body("cm", "POSTGRES_17",
		`"serverCaMode":"CUSTOMER_MANAGED_CAS_CA","serverCaPool":"`+pool+`","serverCertificateRotationMode":"AUTOMATIC_ROTATION_DURING_MAINTENANCE"`)); code != 200 {
		t.Fatalf("insert = %d %s", code, resp)
	}
	in := getRunnable(t, base+"/cm")
	if in["dnsName"] == nil {
		t.Errorf("a customer-managed instance answers no DNS name: %v", in)
	}
	got, err := svc.Instances.Get("proj", "cm").Do()
	if err != nil || got.Settings.IpConfiguration.ServerCaPool != pool || got.Settings.IpConfiguration.ServerCertificateRotationMode != "AUTOMATIC_ROTATION_DURING_MAINTENANCE" {
		t.Fatalf("get: %v %+v", err, got)
	}
	cs, err := svc.Connect.Get("proj", "cm").Do()
	if err != nil || cs.ServerCaMode != customerCA || len(cs.DnsNames) != 1 || strings.Count(cs.ServerCaCert.Cert, "BEGIN CERTIFICATE") != 2 {
		t.Fatalf("connect settings: %v %+v", err, cs)
	}

	chain := peerChain(t, svc, connAddr, "cm")
	dns := strings.TrimSuffix(cs.DnsNames[0].Name, ".")
	verifiesToRoot(t, chain, root, dns)
	leaf := chain[0]
	if k, ok := leaf.PublicKey.(*ecdsa.PublicKey); !ok || k.Curve != elliptic.P384() {
		t.Errorf("the server key is %T, want EC P-384", leaf.PublicKey)
	}
	if len(chain) != 2 || chain[1].Subject.CommonName != "Example Issuing" {
		t.Errorf("presented %d certificates; want the server's and the subordinate's", len(chain))
	}
	// It is the pool's certificate.
	certs, err := cas.API().ListCertificates(ctx, &privatecapb.ListCertificatesRequest{Parent: pool})
	if err != nil || len(certs.GetCertificates()) != 1 || !strings.Contains(certs.GetCertificates()[0].GetName(), "/certificates/cloudsql-cm-") {
		t.Fatalf("the pool's certificates: %v %v", err, certs)
	}

	list, err := svc.Instances.ListServerCertificates("proj", "cm").Do()
	if err != nil || len(list.ServerCerts) != 1 || len(list.CaCerts) != 1 || list.ActiveVersion != fp(leaf) ||
		list.CaCerts[0].CommonName != "Example Issuing" {
		t.Fatalf("list: %v %+v", err, list)
	}
	if lsc, err := svc.Instances.ListServerCas("proj", "cm").Do(); err != nil || len(lsc.Certs) != 1 || lsc.Certs[0].CommonName != "Example Issuing" {
		t.Errorf("listServerCas: %v %+v", err, lsc)
	}
	if _, err := svc.Instances.AddServerCa("proj", "cm").Do(); err == nil || !strings.Contains(err.Error(), "400") {
		t.Errorf("addServerCa of a customer-managed instance: %v", err)
	}

	// Rotation: the pool issues the upcoming version.
	if _, err := svc.Instances.AddServerCertificate("proj", "cm").Do(); err != nil {
		t.Fatal(err)
	}
	if certs, _ := cas.API().ListCertificates(ctx, &privatecapb.ListCertificatesRequest{Parent: pool}); len(certs.GetCertificates()) != 2 {
		t.Errorf("after add the pool has %d certificates, want 2", len(certs.GetCertificates()))
	}
	if _, err := svc.Instances.RotateServerCertificate("proj", "cm", &sqladmin.InstancesRotateServerCertificateRequest{}).Do(); err != nil {
		t.Fatal(err)
	}
	after := peerChain(t, svc, connAddr, "cm")
	if fp(after[0]) == fp(leaf) {
		t.Error("the rotation did not change the server certificate")
	}
	verifiesToRoot(t, after, root, dns)

	// Back to the internal CA is refused; to the shared CA needs serverCaPool "".
	if code, resp := call(t, "PATCH", base+"/cm", `{"settings":{"ipConfiguration":{"serverCaMode":"GOOGLE_MANAGED_INTERNAL_CA","serverCaPool":""}}}`); code != 400 {
		t.Errorf("patch to the internal CA = %d %s", code, resp)
	}
	if code, resp := call(t, "PATCH", base+"/cm", `{"settings":{"ipConfiguration":{"serverCaMode":"GOOGLE_MANAGED_CAS_CA"}}}`); code != 400 ||
		!strings.Contains(resp, "serverCaPool") {
		t.Errorf("patch to the shared CA keeping the pool = %d %s", code, resp)
	}
	// A patch that names neither keeps them.
	if code, resp := call(t, "PATCH", base+"/cm", `{"settings":{"ipConfiguration":{"ipv4Enabled":true}}}`); code != 200 {
		t.Fatalf("patch = %d %s", code, resp)
	}
	if m := srv.serverCaMode("cm"); m != customerCA {
		t.Fatalf("after a patch of ipv4Enabled the mode is %s", m)
	}
	if code, resp := call(t, "PATCH", base+"/cm", `{"settings":{"ipConfiguration":{"serverCaMode":"GOOGLE_MANAGED_CAS_CA","serverCaPool":""}}}`); code != 200 {
		t.Fatalf("patch to the shared CA = %d %s", code, resp)
	}
	if cs, err := svc.Connect.Get("proj", "cm").Do(); err != nil || cs.ServerCaMode != casCA {
		t.Fatalf("after the switch: %v %+v", err, cs)
	}
	shared := peerChain(t, svc, connAddr, "cm")
	if shared[0].Issuer.CommonName != "CloudBurrow Cloud SQL Server CA us-central1" {
		t.Errorf("after the switch the server certificate is issued by %s", shared[0].Issuer)
	}
	// And to the customer's pool again, from the shared CA.
	if code, resp := call(t, "PATCH", base+"/cm", `{"settings":{"ipConfiguration":{"serverCaMode":"CUSTOMER_MANAGED_CAS_CA","serverCaPool":"`+pool+`"}}}`); code != 200 {
		t.Fatalf("patch to the customer's pool = %d %s", code, resp)
	}
	verifiesToRoot(t, peerChain(t, svc, connAddr, "cm"), root, dns)

	// MySQL before 8.0.30 has no customer-managed CA.
	if _, err := checkServerCA(ctx, testCAS{api: cas.API()}, nil, Spec{Name: "x", Project: "proj", DatabaseVersion: "MYSQL_5_7",
		Settings: map[string]any{"ipConfiguration": map[string]any{"serverCaMode": customerCA, "serverCaPool": pool}}}); err == nil ||
		!strings.Contains(err.Error(), "MySQL 8.0.30") {
		t.Errorf("MYSQL_5_7: %v", err)
	}
	// Without Certificate Authority Service the pool cannot be used.
	if _, err := checkServerCA(ctx, nil, nil, Spec{Name: "x", Project: "proj", DatabaseVersion: "POSTGRES_17",
		Settings: map[string]any{"ipConfiguration": map[string]any{"serverCaMode": customerCA, "serverCaPool": pool}}}); err == nil ||
		!strings.Contains(err.Error(), "privateca") {
		t.Errorf("without privateca: %v", err)
	}
}
