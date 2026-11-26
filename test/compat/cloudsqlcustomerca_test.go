//go:build compat

package compat

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha1" //nolint:gosec // Cloud SQL names certificates by SHA-1 fingerprint
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/cloudsqlconn"
	privateca "cloud.google.com/go/security/privateca/apiv1"
	"cloud.google.com/go/security/privateca/apiv1/privatecapb"
	sqladmin "google.golang.org/api/sqladmin/v1"
	"google.golang.org/protobuf/types/known/durationpb"
)

func sqlCAConfig(cn string) *privatecapb.CertificateConfig {
	isCA := true
	return &privatecapb.CertificateConfig{
		SubjectConfig: &privatecapb.CertificateConfig_SubjectConfig{Subject: &privatecapb.Subject{CommonName: cn, Organization: "Example"}},
		X509Config: &privatecapb.X509Parameters{CaOptions: &privatecapb.X509Parameters_CaOptions{IsCa: &isCA},
			KeyUsage: &privatecapb.KeyUsage{BaseKeyUsage: &privatecapb.KeyUsage_KeyUsageOptions{CertSign: true, CrlSign: true}}},
	}
}

// sqlCAHierarchy makes, through the official client, a root CA in pool
// <id>-roots and a subordinate CA under it in pool <id>-issuing, both
// enabled, and pool <id>-empty with no CA, in us-central1. It returns the
// root's certificate and the issuing pool's name.
func sqlCAHierarchy(t *testing.T, ctx context.Context, c *privateca.CertificateAuthorityClient, project, id string) (*x509.Certificate, string) {
	t.Helper()
	return sqlCAHierarchyFor(t, ctx, c, project, id, 5*365*24*time.Hour)
}

// sqlCAHierarchyFor is sqlCAHierarchy with an issuing CA valid for
// subLife: the server certificates it issues expire with it.
func sqlCAHierarchyFor(t *testing.T, ctx context.Context, c *privateca.CertificateAuthorityClient, project, id string, subLife time.Duration) (*x509.Certificate, string) {
	t.Helper()
	loc := "projects/" + project + "/locations/us-central1"
	for _, p := range []string{"roots", "issuing", "empty"} {
		op, err := c.CreateCaPool(ctx, &privatecapb.CreateCaPoolRequest{Parent: loc, CaPoolId: id + "-" + p,
			CaPool: &privatecapb.CaPool{Tier: privatecapb.CaPool_ENTERPRISE}})
		if err == nil {
			_, err = op.Wait(ctx)
		}
		if err != nil {
			t.Fatalf("create pool %s: %v", p, err)
		}
		name := loc + "/caPools/" + id + "-" + p
		t.Cleanup(func() {
			if op, err := c.DeleteCaPool(context.Background(), &privatecapb.DeleteCaPoolRequest{Name: name, IgnoreDependentResources: true}); err == nil {
				_ = op.Wait(context.Background())
			}
		})
	}
	key := &privatecapb.CertificateAuthority_KeyVersionSpec{KeyVersion: &privatecapb.CertificateAuthority_KeyVersionSpec_Algorithm{
		Algorithm: privatecapb.CertificateAuthority_EC_P256_SHA256}}
	rootPool, issuing := loc+"/caPools/"+id+"-roots", loc+"/caPools/"+id+"-issuing"
	root, sub := rootPool+"/certificateAuthorities/root", issuing+"/certificateAuthorities/sub"
	op, err := c.CreateCertificateAuthority(ctx, &privatecapb.CreateCertificateAuthorityRequest{Parent: rootPool, CertificateAuthorityId: "root",
		CertificateAuthority: &privatecapb.CertificateAuthority{Type: privatecapb.CertificateAuthority_SELF_SIGNED,
			Config: sqlCAConfig("Example Root"), KeySpec: key, Lifetime: durationpb.New(10 * 365 * 24 * time.Hour)}})
	if err == nil {
		_, err = op.Wait(ctx)
	}
	if err != nil {
		t.Fatalf("create the root CA: %v", err)
	}
	if op, err := c.EnableCertificateAuthority(ctx, &privatecapb.EnableCertificateAuthorityRequest{Name: root}); err != nil {
		t.Fatal(err)
	} else if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	op, err = c.CreateCertificateAuthority(ctx, &privatecapb.CreateCertificateAuthorityRequest{Parent: issuing, CertificateAuthorityId: "sub",
		CertificateAuthority: &privatecapb.CertificateAuthority{Type: privatecapb.CertificateAuthority_SUBORDINATE,
			Config: sqlCAConfig("Example Issuing"), KeySpec: key, Lifetime: durationpb.New(subLife)}})
	if err == nil {
		_, err = op.Wait(ctx)
	}
	if err != nil {
		t.Fatalf("create the subordinate CA: %v", err)
	}
	csr, err := c.FetchCertificateAuthorityCsr(ctx, &privatecapb.FetchCertificateAuthorityCsrRequest{Name: sub})
	if err != nil {
		t.Fatal(err)
	}
	signed, err := c.CreateCertificate(ctx, &privatecapb.CreateCertificateRequest{Parent: rootPool, CertificateId: "sub",
		Certificate: &privatecapb.Certificate{Lifetime: durationpb.New(subLife),
			CertificateConfig: &privatecapb.Certificate_PemCsr{PemCsr: csr.GetPemCsr()}}})
	if err != nil {
		t.Fatalf("sign the subordinate's CSR: %v", err)
	}
	aop, err := c.ActivateCertificateAuthority(ctx, &privatecapb.ActivateCertificateAuthorityRequest{Name: sub,
		PemCaCertificate: signed.GetPemCertificate(), SubordinateConfig: &privatecapb.SubordinateConfig{
			SubordinateConfig: &privatecapb.SubordinateConfig_CertificateAuthority{CertificateAuthority: root}}})
	if err == nil {
		_, err = aop.Wait(ctx)
	}
	if err != nil {
		t.Fatalf("activate the subordinate CA: %v", err)
	}
	if ca, err := c.GetCertificateAuthority(ctx, &privatecapb.GetCertificateAuthorityRequest{Name: sub}); err != nil {
		t.Fatal(err)
	} else if ca.GetState() != privatecapb.CertificateAuthority_ENABLED {
		if op, err := c.EnableCertificateAuthority(ctx, &privatecapb.EnableCertificateAuthorityRequest{Name: sub}); err != nil {
			t.Fatal(err)
		} else if _, err := op.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// The root as the pool publishes it.
	fetched, err := c.FetchCaCerts(ctx, &privatecapb.FetchCaCertsRequest{CaPool: rootPool})
	if err != nil || len(fetched.GetCaCerts()) == 0 || len(fetched.GetCaCerts()[0].GetCertificates()) == 0 {
		t.Fatalf("fetchCaCerts: %v %v", err, fetched)
	}
	b, _ := pem.Decode([]byte(fetched.GetCaCerts()[0].GetCertificates()[0]))
	if b == nil {
		t.Fatal("fetchCaCerts answers no PEM")
	}
	rootCert, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return rootCert, issuing
}

// connectorPeerChain dials the instance through the official Go
// connector and returns the certificates its server presented, which the
// connector verified against connect.get's serverCaCert and DNS name.
func connectorPeerChain(t *testing.T, h *Harness, connName string) []*x509.Certificate {
	t.Helper()
	ctx, cancel := context.WithTimeout(h.Context(), time.Minute)
	defer cancel()
	d, err := cloudsqlconn.NewDialer(ctx, connectorOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	c, err := d.Dial(ctx, connName)
	if err != nil {
		t.Fatalf("the official connector cannot reach %s: %v", connName, err)
	}
	defer c.Close()
	// The connector's net.Conn wraps the *tls.Conn it handshook, in its
	// embedded Conn field.
	v := reflect.ValueOf(c)
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if tc, ok := v.Interface().(*tls.Conn); ok {
			return tc.ConnectionState().PeerCertificates
		}
		v = v.Elem()
	}
	f := v.FieldByName("Conn")
	if !f.IsValid() {
		t.Fatalf("the connector's connection %T has no Conn", c)
	}
	tc, ok := f.Interface().(*tls.Conn)
	if !ok {
		t.Fatalf("the connector's connection wraps %T, not a *tls.Conn", f.Interface())
	}
	return tc.ConnectionState().PeerCertificates
}

// verifyToRoot checks that chain verifies against root alone, the
// presented intermediates as intermediates, for dns and server auth.
func verifyToRoot(t *testing.T, chain []*x509.Certificate, root *x509.Certificate, dns string) {
	t.Helper()
	roots, inter := x509.NewCertPool(), x509.NewCertPool()
	roots.AddCert(root)
	for _, c := range chain[1:] {
		inter.AddCert(c)
	}
	got, err := chain[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, DNSName: dns,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	if err != nil {
		t.Fatalf("the server certificate does not verify to the CA pool's root: %v", err)
	}
	if last := got[0][len(got[0])-1]; !last.Equal(root) {
		t.Fatalf("the chain ends at %s, not the root", last.Subject)
	}
}

// TestCloudSQLCustomerManagedCA (#1834): through the official clients, an
// instance created with settings.ipConfiguration.serverCaMode
// CUSTOMER_MANAGED_CAS_CA and serverCaPool, a pool of Certificate Authority
// Service whose CA is a subordinate under a root of another pool, has its
// server certificate issued by that pool: an EC P-384 certificate with the
// instance's DNS name, listed among the pool's certificates. connect.get
// answers the mode and the CA chain, listServerCertificates and
// listServerCas the pool's CA, and the official Go connector reaches the
// server, which presents the certificate with the subordinate, so the
// chain verifies to the pool's root (fetchCaCerts). addServerCertificate
// and rotateServerCertificate rotate it to a new certificate of the pool,
// still under the root, and the connector (pgx) still runs SELECT 1. The
// pool is checked as Cloud SQL documents (format, region, existence, an
// enabled CA), the instance cannot go back to GOOGLE_MANAGED_INTERNAL_CA,
// and it moves to GOOGLE_MANAGED_CAS_CA with serverCaPool "".
// covers: sql.instances.insert, sql.instances.get, sql.instances.patch, sql.instances.delete, sql.connect.get, sql.connect.generateEphemeral, sql.instances.ListServerCertificates, sql.instances.listServerCas, sql.instances.addServerCertificate, sql.instances.RotateServerCertificate, google.cloud.security.privateca.v1.CertificateAuthorityService/ListCertificates, google.cloud.security.privateca.v1.CertificateAuthorityService/FetchCaCerts
func TestCloudSQLCustomerManagedCA(t *testing.T) {
	h := New(t)
	svc := sqlAdminService(t, h)
	// Certificate Authority Service as the sql shard's SQL_PRIVATECA, so
	// its own suite stays in its shard.
	shardEndpoint(h, envSQLPrivateCA, EnvPrivateCA)
	cas := privateCAClients(t, h)["grpc"]
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql,privateca")
	}
	ctx := h.Context()
	project := h.Project()
	n := time.Now().UnixNano() % 1e6
	root, pool := sqlCAHierarchy(t, ctx, cas, project, fmt.Sprintf("sqlca-%d", n))
	name := fmt.Sprintf("cmca-%d", n)
	ic := func(mode, pool string) *sqladmin.IpConfiguration {
		return &sqladmin.IpConfiguration{ServerCaMode: mode, ServerCaPool: pool, ForceSendFields: []string{"ServerCaPool"}}
	}
	inst := func(n string, c *sqladmin.IpConfiguration) *sqladmin.DatabaseInstance {
		return &sqladmin.DatabaseInstance{Name: n, DatabaseVersion: "POSTGRES_17", RootPassword: "Root-pass1", Region: "us-central1",
			Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", IpConfiguration: c}}
	}

	// The pool, as Cloud SQL checks it.
	loc := "projects/" + project + "/locations/"
	for _, c := range []struct {
		what, pool string
		code       int
		want       string
	}{
		{"no pool", "", 400, "serverCaPool"},
		{"a pool ID alone", "issuing", 400, "Invalid format for server CA pool"},
		{"a pool in another region", loc + "europe-west1/caPools/x", 400, "must be in the same region as the instance"},
		{"a pool that does not exist", loc + "us-central1/caPools/nope", 404, "not found"},
		{"a pool with no enabled CA", strings.TrimSuffix(pool, "-issuing") + "-empty", 400, "There are no enabled CAs in the CaPool"},
	} {
		_, err := svc.Instances.Insert(project, inst(name+"-bad", ic("CUSTOMER_MANAGED_CAS_CA", c.pool))).Do()
		wantCode(t, c.what, err, c.code, c.want)
	}

	in := createSQLInstance(t, svc, project, inst(name, ic("CUSTOMER_MANAGED_CAS_CA", pool)))
	if in.Settings.IpConfiguration == nil || in.Settings.IpConfiguration.ServerCaMode != "CUSTOMER_MANAGED_CAS_CA" ||
		in.Settings.IpConfiguration.ServerCaPool != pool || in.DnsName == "" {
		t.Fatalf("the instance: ipConfiguration %+v dnsName %q", in.Settings.IpConfiguration, in.DnsName)
	}
	dns := strings.TrimSuffix(in.DnsName, ".")
	cs, err := svc.Connect.Get(project, name).Do()
	if err != nil {
		t.Fatal(err)
	}
	if cs.ServerCaMode != "CUSTOMER_MANAGED_CAS_CA" || len(cs.DnsNames) != 1 || strings.Count(cs.ServerCaCert.Cert, "BEGIN CERTIFICATE") != 2 {
		t.Fatalf("connect settings: mode %s dnsNames %+v chain of %d", cs.ServerCaMode, cs.DnsNames,
			strings.Count(cs.ServerCaCert.Cert, "BEGIN CERTIFICATE"))
	}

	chain := connectorPeerChain(t, h, in.ConnectionName)
	verifyToRoot(t, chain, root, dns)
	if k, ok := chain[0].PublicKey.(*ecdsa.PublicKey); !ok || k.Curve != elliptic.P384() {
		t.Errorf("the server certificate's key is %T, want EC P-384", chain[0].PublicKey)
	}
	if len(chain) != 2 || chain[1].Subject.CommonName != "Example Issuing" {
		t.Errorf("the server presents %d certificates; want its own and the subordinate CA's", len(chain))
	}
	if fp := presentedServerCert(t, svc, project, name); fp != fingerprintOf(chain[0]) {
		t.Errorf("generateEphemeralCert's certificate reaches a server presenting %s, the connector saw %s", fp, fingerprintOf(chain[0]))
	}
	issued := func() int {
		t.Helper()
		resp, err := cas.ListCertificates(ctx, &privatecapb.ListCertificatesRequest{Parent: pool}).Next()
		if err != nil {
			t.Fatalf("the pool's certificates: %v", err)
		}
		if !strings.Contains(resp.GetName(), "/certificates/cloudsql-"+name+"-") {
			t.Errorf("the pool's certificate %s is not the instance's", resp.GetName())
		}
		count := 0
		it := cas.ListCertificates(ctx, &privatecapb.ListCertificatesRequest{Parent: pool})
		for {
			if _, err := it.Next(); err != nil {
				return count
			}
			count++
		}
	}
	if got := issued(); got != 1 {
		t.Errorf("the pool has %d certificates, want the instance's one", got)
	}

	list, err := svc.Instances.ListServerCertificates(project, name).Do()
	if err != nil || len(list.ServerCerts) != 1 || len(list.CaCerts) != 1 || list.ActiveVersion != fingerprintOf(chain[0]) ||
		list.CaCerts[0].CommonName != "Example Issuing" {
		t.Fatalf("listServerCertificates: %v %+v", err, list)
	}
	if lsc, err := svc.Instances.ListServerCas(project, name).Do(); err != nil || len(lsc.Certs) != 1 || lsc.Certs[0].CommonName != "Example Issuing" {
		t.Errorf("listServerCas: %v %+v", err, lsc)
	}
	_, err = svc.Instances.AddServerCa(project, name).Do()
	wantCode(t, "addServerCa of a customer-managed instance", err, 400, "addServerCertificate")

	db := sqlEngine{}.root(t, h, in.ConnectionName)
	query := func(what string) {
		t.Helper()
		eventually(t, what, func() error {
			var one int
			return db.QueryRow("SELECT 1").Scan(&one)
		})
	}
	query("SELECT 1 through the official connector")

	// Rotation: the pool issues the upcoming certificate.
	op, err := svc.Instances.AddServerCertificate(project, name).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOpDone(t, svc, project, op)
	if got := issued(); got != 2 {
		t.Errorf("after addServerCertificate the pool has %d certificates, want 2", got)
	}
	op, err = svc.Instances.RotateServerCertificate(project, name, &sqladmin.InstancesRotateServerCertificateRequest{}).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOpDone(t, svc, project, op)
	after := connectorPeerChain(t, h, in.ConnectionName)
	if fingerprintOf(after[0]) == fingerprintOf(chain[0]) {
		t.Error("the rotation did not change the server certificate")
	}
	verifyToRoot(t, after, root, dns)
	db = sqlEngine{}.root(t, h, in.ConnectionName)
	query("SELECT 1 after the rotation, through a new connector")

	// Leaving the customer's CA.
	_, err = svc.Instances.Patch(project, name, &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{
		IpConfiguration: ic("GOOGLE_MANAGED_INTERNAL_CA", "")}}).Do()
	wantCode(t, "back to the per-instance CA", err, 400, "GOOGLE_MANAGED_INTERNAL_CA")
	op, err = svc.Instances.Patch(project, name, &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{
		IpConfiguration: ic("GOOGLE_MANAGED_CAS_CA", "")}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOpDone(t, svc, project, op)
	if cs, err := svc.Connect.Get(project, name).Do(); err != nil || cs.ServerCaMode != "GOOGLE_MANAGED_CAS_CA" {
		t.Fatalf("after the switch to the shared CA: %v %+v", err, cs)
	}
	if shared := connectorPeerChain(t, h, in.ConnectionName); shared[0].CheckSignatureFrom(after[1]) == nil {
		t.Error("after the switch the server certificate is still the pool's")
	}
}

func fingerprintOf(c *x509.Certificate) string {
	fp := sha1.Sum(c.Raw) //nolint:gosec // Cloud SQL names certificates by SHA-1 fingerprint
	return hex.EncodeToString(fp[:])
}
