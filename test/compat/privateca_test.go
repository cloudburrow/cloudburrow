//go:build compat

package compat

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
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	privateca "cloud.google.com/go/security/privateca/apiv1"
	"cloud.google.com/go/security/privateca/apiv1/privatecapb"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// EnvPrivateCA is the host address of Certificate Authority Service
// (privateca.googleapis.com, #1834), gRPC and JSON on one port.
const EnvPrivateCA = "CLOUDBURROW_TEST_PRIVATECA"

// privateCAClients is the official client over gRPC and over REST.
func privateCAClients(t *testing.T, h *Harness) map[string]*privateca.CertificateAuthorityClient {
	t.Helper()
	ctx := h.Context()
	addr := h.Endpoint(EnvPrivateCA)
	g, err := privateca.NewCertificateAuthorityClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	r, err := privateca.NewCertificateAuthorityRESTClient(ctx, option.WithEndpoint("http://"+addr), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close(); _ = r.Close() })
	return map[string]*privateca.CertificateAuthorityClient{"grpc": g, "rest": r}
}

// wantPrivateCACode checks a call's canonical code: the gRPC status, or
// over REST the JSON error's status, which is the code's name.
func wantPrivateCACode(t *testing.T, what string, err error, code codes.Code) {
	t.Helper()
	got := status.Code(err)
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		var body struct {
			Error struct {
				Status string `json:"status"`
			} `json:"error"`
		}
		got = codes.Unknown
		if json.Unmarshal([]byte(ge.Body), &body) == nil {
			var c codes.Code
			if c.UnmarshalJSON([]byte(`"`+body.Error.Status+`"`)) == nil {
				got = c
			}
		}
	}
	if got != code {
		t.Errorf("%s: %v (%s); want %s", what, err, got, code)
	}
}

// TestPrivateCAPoolsRootsAndCertificates (#1834) drives Certificate
// Authority Service through cloud.google.com/go/security/privateca/apiv1,
// over gRPC and over REST, waiting on every long-running operation:
//   - an ENTERPRISE CA pool is created, got, listed, and its labels updated;
//   - a root CA is created STAGED, is not used to issue, is enabled, and is
//     the pool's trust anchor (FetchCaCerts);
//   - a certificate from a CSR and one from a CertificateConfig are issued,
//     verify against the root for TLS server auth, carry the pool's
//     baseline key usage, are truncated to the pool's maximum lifetime, and
//     are got and listed; the second's labels are updated;
//   - a TLS handshake against a server holding the CSR's certificate
//     succeeds with the root as the only trusted CA;
//   - one certificate is revoked, and a second revocation is refused;
//   - an ENABLED CA cannot be deleted, nor one with active certificates;
//     disable, delete (DELETED, with expireTime 30 days on), undelete
//     (DISABLED), and delete with skip_grace_period;
//   - the pool's IAM policy is set and read back; and the pool is deleted.
//
// covers: google.cloud.security.privateca.v1.CertificateAuthorityService/CreateCertificate, google.cloud.security.privateca.v1.CertificateAuthorityService/GetCertificate, google.cloud.security.privateca.v1.CertificateAuthorityService/ListCertificates, google.cloud.security.privateca.v1.CertificateAuthorityService/RevokeCertificate, google.cloud.security.privateca.v1.CertificateAuthorityService/UpdateCertificate, google.cloud.security.privateca.v1.CertificateAuthorityService/CreateCertificateAuthority, google.cloud.security.privateca.v1.CertificateAuthorityService/DisableCertificateAuthority, google.cloud.security.privateca.v1.CertificateAuthorityService/EnableCertificateAuthority, google.cloud.security.privateca.v1.CertificateAuthorityService/GetCertificateAuthority, google.cloud.security.privateca.v1.CertificateAuthorityService/ListCertificateAuthorities, google.cloud.security.privateca.v1.CertificateAuthorityService/UndeleteCertificateAuthority, google.cloud.security.privateca.v1.CertificateAuthorityService/DeleteCertificateAuthority, google.cloud.security.privateca.v1.CertificateAuthorityService/UpdateCertificateAuthority, google.cloud.security.privateca.v1.CertificateAuthorityService/CreateCaPool, google.cloud.security.privateca.v1.CertificateAuthorityService/UpdateCaPool, google.cloud.security.privateca.v1.CertificateAuthorityService/GetCaPool, google.cloud.security.privateca.v1.CertificateAuthorityService/ListCaPools, google.cloud.security.privateca.v1.CertificateAuthorityService/DeleteCaPool, google.cloud.security.privateca.v1.CertificateAuthorityService/FetchCaCerts
// covers: google.cloud.security.privateca.v1.CertificateAuthorityService/ListCertificateTemplates (unimplemented)
func TestPrivateCAPoolsRootsAndCertificates(t *testing.T) {
	h := New(t)
	clients := privateCAClients(t, h)
	for _, transport := range []string{"grpc", "rest"} {
		t.Run(transport, func(t *testing.T) {
			privateCAFlow(t, h, clients[transport], transport)
		})
	}
}

func privateCAFlow(t *testing.T, h *Harness, c *privateca.CertificateAuthorityClient, transport string) {
	ctx := h.Context()
	loc := "projects/" + h.Project() + "/locations/us-central1"
	poolID := fmt.Sprintf("pool-%s-%d", transport, time.Now().UnixNano()%1e9)
	poolName := loc + "/caPools/" + poolID

	// Validation: a pool needs a tier, and an ID of [a-zA-Z0-9_-]{1,63}.
	_, err := c.CreateCaPool(ctx, &privatecapb.CreateCaPoolRequest{Parent: loc, CaPoolId: poolID, CaPool: &privatecapb.CaPool{}})
	wantPrivateCACode(t, "pool without tier", err, codes.InvalidArgument)
	_, err = c.CreateCaPool(ctx, &privatecapb.CreateCaPoolRequest{Parent: loc, CaPoolId: strings.Repeat("a", 64),
		CaPool: &privatecapb.CaPool{Tier: privatecapb.CaPool_ENTERPRISE}})
	wantPrivateCACode(t, "64-character pool ID", err, codes.InvalidArgument)

	pop, err := c.CreateCaPool(ctx, &privatecapb.CreateCaPoolRequest{Parent: loc, CaPoolId: poolID, CaPool: &privatecapb.CaPool{
		Tier: privatecapb.CaPool_ENTERPRISE,
		IssuancePolicy: &privatecapb.CaPool_IssuancePolicy{
			MaximumLifetime: durationpb.New(48 * time.Hour),
			BaselineValues: &privatecapb.X509Parameters{KeyUsage: &privatecapb.KeyUsage{
				BaseKeyUsage:     &privatecapb.KeyUsage_KeyUsageOptions{DigitalSignature: true, KeyEncipherment: true},
				ExtendedKeyUsage: &privatecapb.KeyUsage_ExtendedKeyUsageOptions{ServerAuth: true, ClientAuth: true},
			}},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pool.GetName() != poolName || pool.GetTier() != privatecapb.CaPool_ENTERPRISE {
		t.Fatalf("created pool = %v", pool)
	}
	if md, err := pop.Metadata(); err != nil || md.GetVerb() != "create" || md.GetTarget() != poolName {
		t.Errorf("create metadata = %v, %v", md, err)
	}
	_, err = c.CreateCaPool(ctx, &privatecapb.CreateCaPoolRequest{Parent: loc, CaPoolId: poolID, CaPool: &privatecapb.CaPool{Tier: privatecapb.CaPool_DEVOPS}})
	wantPrivateCACode(t, "duplicate pool", err, codes.AlreadyExists)
	if got, err := c.GetCaPool(ctx, &privatecapb.GetCaPoolRequest{Name: poolName}); err != nil || got.GetIssuancePolicy().GetMaximumLifetime().AsDuration() != 48*time.Hour {
		t.Errorf("GetCaPool = %v, %v", got, err)
	}
	found := false
	it := c.ListCaPools(ctx, &privatecapb.ListCaPoolsRequest{Parent: loc})
	for {
		p, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		found = found || p.GetName() == poolName
	}
	if !found {
		t.Errorf("ListCaPools does not list %s", poolName)
	}
	uop, err := c.UpdateCaPool(ctx, &privatecapb.UpdateCaPoolRequest{CaPool: &privatecapb.CaPool{Name: poolName, Labels: map[string]string{"env": "test"}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}})
	if err != nil {
		t.Fatal(err)
	}
	if up, err := uop.Wait(ctx); err != nil || up.GetLabels()["env"] != "test" {
		t.Errorf("UpdateCaPool = %v, %v", up, err)
	}

	// A root CA, created STAGED.
	caName := poolName + "/certificateAuthorities/root"
	isCA := true
	cop, err := c.CreateCertificateAuthority(ctx, &privatecapb.CreateCertificateAuthorityRequest{Parent: poolName, CertificateAuthorityId: "root",
		CertificateAuthority: &privatecapb.CertificateAuthority{
			Type: privatecapb.CertificateAuthority_SELF_SIGNED,
			Config: &privatecapb.CertificateConfig{
				SubjectConfig: &privatecapb.CertificateConfig_SubjectConfig{Subject: &privatecapb.Subject{CommonName: "CloudBurrow Test Root", Organization: "CloudBurrow"}},
				X509Config: &privatecapb.X509Parameters{
					CaOptions: &privatecapb.X509Parameters_CaOptions{IsCa: &isCA},
					KeyUsage:  &privatecapb.KeyUsage{BaseKeyUsage: &privatecapb.KeyUsage_KeyUsageOptions{CertSign: true, CrlSign: true}},
				},
			},
			Lifetime: durationpb.New(10 * 24 * time.Hour),
			KeySpec:  &privatecapb.CertificateAuthority_KeyVersionSpec{KeyVersion: &privatecapb.CertificateAuthority_KeyVersionSpec_Algorithm{Algorithm: privatecapb.CertificateAuthority_EC_P256_SHA256}},
		}})
	if err != nil {
		t.Fatal(err)
	}
	ca, err := cop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ca.GetName() != caName || ca.GetState() != privatecapb.CertificateAuthority_STAGED || len(ca.GetPemCaCertificates()) != 1 {
		t.Fatalf("created CA = %v", ca)
	}
	root := parsePEMCertificate(t, ca.GetPemCaCertificates()[0])
	if !root.IsCA || root.Subject.CommonName != "CloudBurrow Test Root" || root.CheckSignatureFrom(root) != nil {
		t.Errorf("root certificate: IsCA %v, subject %v", root.IsCA, root.Subject)
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "localhost"},
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}, key)
	if err != nil {
		t.Fatal(err)
	}
	csrPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}))
	csrCert := &privatecapb.Certificate{Lifetime: durationpb.New(7 * 24 * time.Hour), CertificateConfig: &privatecapb.Certificate_PemCsr{PemCsr: csrPEM}}

	// A STAGED CA does not issue from the pool.
	_, err = c.CreateCertificate(ctx, &privatecapb.CreateCertificateRequest{Parent: poolName, CertificateId: "early", Certificate: csrCert})
	wantPrivateCACode(t, "issue from a STAGED CA", err, codes.FailedPrecondition)
	eop, err := c.EnableCertificateAuthority(ctx, &privatecapb.EnableCertificateAuthorityRequest{Name: caName})
	if err != nil {
		t.Fatal(err)
	}
	if ca, err = eop.Wait(ctx); err != nil || ca.GetState() != privatecapb.CertificateAuthority_ENABLED {
		t.Fatalf("EnableCertificateAuthority = %v, %v", ca, err)
	}
	if got, err := c.GetCertificateAuthority(ctx, &privatecapb.GetCertificateAuthorityRequest{Name: caName}); err != nil || got.GetState() != privatecapb.CertificateAuthority_ENABLED {
		t.Errorf("GetCertificateAuthority = %v, %v", got, err)
	}
	cait := c.ListCertificateAuthorities(ctx, &privatecapb.ListCertificateAuthoritiesRequest{Parent: poolName})
	if got, err := cait.Next(); err != nil || got.GetName() != caName {
		t.Errorf("ListCertificateAuthorities = %v, %v", got, err)
	}
	anchors, err := c.FetchCaCerts(ctx, &privatecapb.FetchCaCertsRequest{CaPool: poolName})
	if err != nil || len(anchors.GetCaCerts()) != 1 || anchors.GetCaCerts()[0].GetCertificates()[0] != ca.GetPemCaCertificates()[0] {
		t.Errorf("FetchCaCerts = %v, %v", anchors, err)
	}

	// From a CSR: verifies for server auth, with the baseline's key usage,
	// cut to the pool's 48-hour maximum lifetime.
	_, err = c.CreateCertificate(ctx, &privatecapb.CreateCertificateRequest{Parent: poolName, Certificate: csrCert})
	wantPrivateCACode(t, "ENTERPRISE certificate without an ID", err, codes.InvalidArgument)
	crt, err := c.CreateCertificate(ctx, &privatecapb.CreateCertificateRequest{Parent: poolName, CertificateId: "from-csr", Certificate: csrCert})
	if err != nil {
		t.Fatal(err)
	}
	if crt.GetIssuerCertificateAuthority() != caName || crt.GetName() != poolName+"/certificates/from-csr" {
		t.Errorf("certificate = %v", crt)
	}
	leaf := parsePEMCertificate(t, crt.GetPemCertificate())
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "localhost", KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Errorf("the CSR certificate does not verify: %v", err)
	}
	if life := leaf.NotAfter.Sub(leaf.NotBefore); life != 48*time.Hour {
		t.Errorf("lifetime %v; want the pool's maximum, 48h", life)
	}
	if leaf.KeyUsage&x509.KeyUsageDigitalSignature == 0 || crt.GetCertificateDescription().GetSubjectDescription().GetSubject().GetCommonName() != "localhost" {
		t.Errorf("key usage %v, description %v", leaf.KeyUsage, crt.GetCertificateDescription())
	}
	if len(crt.GetPemCertificateChain()) != 1 || crt.GetPemCertificateChain()[0] != ca.GetPemCaCertificates()[0] {
		t.Errorf("chain = %v", crt.GetPemCertificateChain())
	}

	// A TLS server with it, trusted through the root alone.
	keyDER, _ := x509.MarshalECPrivateKey(key)
	pair, err := tls.X509KeyPair([]byte(crt.GetPemCertificate()), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()
	hc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}}
	port := ln.Addr().(*net.TCPAddr).Port
	resp, err := hc.Get(fmt.Sprintf("https://localhost:%d/", port))
	if err != nil {
		t.Errorf("TLS with the issued certificate: %v", err)
	} else {
		_ = resp.Body.Close()
	}

	// From a CertificateConfig.
	pub, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	pubDER, _ := x509.MarshalPKIXPublicKey(pub.Public())
	cfgCert, err := c.CreateCertificate(ctx, &privatecapb.CreateCertificateRequest{Parent: poolName, CertificateId: "from-config",
		IssuingCertificateAuthorityId: "root",
		Certificate: &privatecapb.Certificate{Lifetime: durationpb.New(time.Hour), CertificateConfig: &privatecapb.Certificate_Config{Config: &privatecapb.CertificateConfig{
			SubjectConfig: &privatecapb.CertificateConfig_SubjectConfig{Subject: &privatecapb.Subject{CommonName: "client"},
				SubjectAltName: &privatecapb.SubjectAltNames{EmailAddresses: []string{"client@example.com"}}},
			X509Config: &privatecapb.X509Parameters{},
			PublicKey:  &privatecapb.PublicKey{Format: privatecapb.PublicKey_PEM, Key: pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})},
		}}}})
	if err != nil {
		t.Fatal(err)
	}
	cl := parsePEMCertificate(t, cfgCert.GetPemCertificate())
	if _, err := cl.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil || cl.Subject.CommonName != "client" {
		t.Errorf("the config certificate: %v, subject %v", err, cl.Subject)
	}
	if got, err := c.GetCertificate(ctx, &privatecapb.GetCertificateRequest{Name: cfgCert.GetName()}); err != nil || got.GetPemCertificate() != cfgCert.GetPemCertificate() {
		t.Errorf("GetCertificate = %v, %v", got, err)
	}
	n := 0
	cit := c.ListCertificates(ctx, &privatecapb.ListCertificatesRequest{Parent: poolName})
	for {
		_, err := cit.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 2 {
		t.Errorf("ListCertificates listed %d; want 2", n)
	}
	if got, err := c.UpdateCertificate(ctx, &privatecapb.UpdateCertificateRequest{Certificate: &privatecapb.Certificate{Name: cfgCert.GetName(), Labels: map[string]string{"k": "v"}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}}); err != nil || got.GetLabels()["k"] != "v" {
		t.Errorf("UpdateCertificate = %v, %v", got, err)
	}

	// Revoke.
	rv, err := c.RevokeCertificate(ctx, &privatecapb.RevokeCertificateRequest{Name: cfgCert.GetName(), Reason: privatecapb.RevocationReason_KEY_COMPROMISE})
	if err != nil || rv.GetRevocationDetails().GetRevocationState() != privatecapb.RevocationReason_KEY_COMPROMISE {
		t.Errorf("RevokeCertificate = %v, %v", rv, err)
	}
	_, err = c.RevokeCertificate(ctx, &privatecapb.RevokeCertificateRequest{Name: cfgCert.GetName(), Reason: privatecapb.RevocationReason_SUPERSEDED})
	wantPrivateCACode(t, "second revocation", err, codes.FailedPrecondition)

	// Disable, delete, undelete, purge.
	_, err = c.DeleteCertificateAuthority(ctx, &privatecapb.DeleteCertificateAuthorityRequest{Name: caName})
	wantPrivateCACode(t, "delete an ENABLED CA", err, codes.FailedPrecondition)
	dop, err := c.DisableCertificateAuthority(ctx, &privatecapb.DisableCertificateAuthorityRequest{Name: caName})
	if err != nil {
		t.Fatal(err)
	}
	if ca, err = dop.Wait(ctx); err != nil || ca.GetState() != privatecapb.CertificateAuthority_DISABLED {
		t.Fatalf("DisableCertificateAuthority = %v, %v", ca, err)
	}
	_, err = c.CreateCertificate(ctx, &privatecapb.CreateCertificateRequest{Parent: poolName, CertificateId: "late", Certificate: csrCert})
	wantPrivateCACode(t, "issue from a DISABLED CA", err, codes.FailedPrecondition)
	_, err = c.DeleteCertificateAuthority(ctx, &privatecapb.DeleteCertificateAuthorityRequest{Name: caName})
	wantPrivateCACode(t, "delete a CA with an active certificate", err, codes.FailedPrecondition)
	delop, err := c.DeleteCertificateAuthority(ctx, &privatecapb.DeleteCertificateAuthorityRequest{Name: caName, IgnoreActiveCertificates: true})
	if err != nil {
		t.Fatal(err)
	}
	if ca, err = delop.Wait(ctx); err != nil || ca.GetState() != privatecapb.CertificateAuthority_DELETED ||
		ca.GetExpireTime().AsTime().Sub(ca.GetDeleteTime().AsTime()) != 30*24*time.Hour {
		t.Fatalf("DeleteCertificateAuthority = %v, %v", ca, err)
	}
	_, err = c.DeleteCaPool(ctx, &privatecapb.DeleteCaPoolRequest{Name: poolName})
	wantPrivateCACode(t, "delete a pool holding a CA in its grace period", err, codes.FailedPrecondition)
	udop, err := c.UndeleteCertificateAuthority(ctx, &privatecapb.UndeleteCertificateAuthorityRequest{Name: caName})
	if err != nil {
		t.Fatal(err)
	}
	if ca, err = udop.Wait(ctx); err != nil || ca.GetState() != privatecapb.CertificateAuthority_DISABLED {
		t.Fatalf("UndeleteCertificateAuthority = %v, %v", ca, err)
	}
	pop2, err := c.DeleteCertificateAuthority(ctx, &privatecapb.DeleteCertificateAuthorityRequest{Name: caName, IgnoreActiveCertificates: true, SkipGracePeriod: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pop2.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = c.GetCertificateAuthority(ctx, &privatecapb.GetCertificateAuthorityRequest{Name: caName})
	wantPrivateCACode(t, "get a purged CA", err, codes.NotFound)

	// IAM on the pool.
	pol, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: poolName, Policy: &iampb.Policy{Bindings: []*iampb.Binding{
		{Role: "roles/privateca.certificateRequester", Members: []string{"user:dev@example.com"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: poolName}); err != nil || len(got.GetBindings()) != 1 || string(got.GetEtag()) != string(pol.GetEtag()) {
		t.Errorf("GetIamPolicy = %v, %v", got, err)
	}

	// Phase 2 features answer 501.
	_, err = c.ListCertificateTemplates(ctx, &privatecapb.ListCertificateTemplatesRequest{Parent: loc}).Next()
	wantPrivateCACode(t, "ListCertificateTemplates", err, codes.Unimplemented)

	dpop, err := c.DeleteCaPool(ctx, &privatecapb.DeleteCaPoolRequest{Name: poolName})
	if err != nil {
		t.Fatal(err)
	}
	if err := dpop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = c.GetCaPool(ctx, &privatecapb.GetCaPoolRequest{Name: poolName})
	wantPrivateCACode(t, "get a deleted pool", err, codes.NotFound)
}

func parsePEMCertificate(t *testing.T, s string) *x509.Certificate {
	t.Helper()
	blk, _ := pem.Decode([]byte(s))
	if blk == nil {
		t.Fatalf("not PEM: %q", s)
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
