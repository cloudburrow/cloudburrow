package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"strings"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	privatecaapi "cloud.google.com/go/security/privateca/apiv1"
	"cloud.google.com/go/security/privateca/apiv1/privatecapb"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

func privateCAFixture(t *testing.T) (privateCAProvider, *privatecaapi.CertificateAuthorityClient) {
	t.Helper()
	ctx := context.Background()
	svc := &privateCAService{cfg: config.Config{BindAddress: "127.0.0.1"}}
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Stop(context.Background()) })
	c, err := privatecaapi.NewCertificateAuthorityClient(ctx, clientOpts(svc.Addr())...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return privateCAProvider{svc: svc}, c
}

func privateCAActions(acts []console.Action) string {
	var ids []string
	for _, a := range acts {
		ids = append(ids, a.ID)
	}
	return strings.Join(ids, ",")
}

// The Certificate Authority Service page (#1834): Create CA pool, Create
// CA, Enable, Request certificate from a CSR and from a config, Revoke,
// Edit labels, Disable, Delete and Undelete, each read back with the
// official client; the issued certificate verifies against the CA's.
func TestConsolePrivateCAPoolsCAsAndCertificates(t *testing.T) {
	ctx := context.Background()
	p, c := privateCAFixture(t)
	const project = "pca-console"
	pool, err := p.Create(ctx, project, map[string]string{"caPoolId": "pool", "location": "us-central1", "tier": "ENTERPRISE",
		"maximumLifetimeDays": "90", "allowCsr": "true", "allowConfig": "true", "labels": `{"env":"dev"}`})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := c.GetCaPool(ctx, &privatecapb.GetCaPoolRequest{Name: pool}); err != nil || got.GetLabels()["env"] != "dev" ||
		got.GetIssuancePolicy().GetMaximumLifetime().GetSeconds() != 90*86400 {
		t.Fatalf("GetCaPool = %v, %v", got, err)
	}
	if l, err := p.List(ctx, project); err != nil || len(l.Items) != 1 || l.Items[0].Fields["Tier"] != "ENTERPRISE" {
		t.Fatalf("List = %+v, %v", l, err)
	}
	if l, _ := p.List(ctx, "other-project"); len(l.Items) != 0 {
		t.Errorf("another project lists %d pools", len(l.Items))
	}
	if err := p.ActAt(ctx, project, []string{pool}, "createca", map[string]string{"certificateAuthorityId": "root", "type": "SELF_SIGNED",
		"commonName": "Console Root", "organization": "CloudBurrow", "algorithm": "EC_P256_SHA256", "lifetimeDays": "365"}); err != nil {
		t.Fatal(err)
	}
	caPath := []string{pool, "certificateAuthorities", "root"}
	if got := privateCAActions(p.DetailActions(ctx, project, caPath)); got != "enable,disable,delete,labels" {
		t.Errorf("a STAGED CA offers %s", got)
	}
	if err := p.ActAt(ctx, project, caPath, "enable", nil); err != nil {
		t.Fatal(err)
	}
	ca, err := c.GetCertificateAuthority(ctx, &privatecapb.GetCertificateAuthorityRequest{Name: pool + "/certificateAuthorities/root"})
	if err != nil || ca.GetState() != privatecapb.CertificateAuthority_ENABLED || ca.GetKeySpec().GetAlgorithm() != privatecapb.CertificateAuthority_EC_P256_SHA256 {
		t.Fatalf("GetCertificateAuthority = %v, %v", ca, err)
	}

	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	csrDER, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "svc.example.com"},
		DNSNames: []string{"svc.example.com"}}, key)
	csr := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}))
	if err := p.ActAt(ctx, project, []string{pool}, "requestcsr", map[string]string{"certificateId": "from-csr", "pemCsr": csr, "lifetimeDays": "30"}); err != nil {
		t.Fatal(err)
	}
	pubDER, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	pub := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
	if err := p.ActAt(ctx, project, []string{pool}, "requestconfig", map[string]string{"certificateId": "from-config", "commonName": "cfg.example.com",
		"dnsNames": "cfg.example.com, alt.example.com", "publicKey": pub, "usage": "server_auth,client_auth", "lifetimeDays": "30"}); err != nil {
		t.Fatal(err)
	}
	crt, err := c.GetCertificate(ctx, &privatecapb.GetCertificateRequest{Name: pool + "/certificates/from-config"})
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode([]byte(crt.GetPemCertificate()))
	leaf, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM([]byte(ca.GetPemCaCertificates()[0]))
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "alt.example.com", KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Errorf("the certificate does not verify against the CA: %v", err)
	}

	d, err := p.Detail(ctx, project, []string{pool})
	if err != nil || len(d.Sections) < 4 || d.Sections[0].Listing.Total != 1 || d.Sections[1].Listing.Total != 2 ||
		!strings.Contains(d.Sections[3].Text, "BEGIN CERTIFICATE") {
		t.Fatalf("pool page = %+v, %v", d, err)
	}
	if d.Sections[1].Listing.Items[0].Opens[1] != "certificates" {
		t.Errorf("a certificate row opens %v", d.Sections[1].Listing.Items[0].Opens)
	}
	certPath := []string{pool, "certificates", "from-csr"}
	if err := p.ActAt(ctx, project, certPath, "revoke", map[string]string{"reason": "KEY_COMPROMISE"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.GetCertificate(ctx, &privatecapb.GetCertificateRequest{Name: pool + "/certificates/from-csr"}); got.GetRevocationDetails().GetRevocationState() != privatecapb.RevocationReason_KEY_COMPROMISE {
		t.Errorf("revocation = %v", got.GetRevocationDetails())
	}
	if got := privateCAActions(p.DetailActions(ctx, project, certPath)); got != "labels" {
		t.Errorf("a revoked certificate offers %s", got)
	}
	if err := p.ActAt(ctx, project, certPath, "labels", map[string]string{"labels": `{"team":"a"}`}); err != nil {
		t.Fatal(err)
	}
	if cd, err := p.Detail(ctx, project, certPath); err != nil || cd.Summary[0].Value != "Revoked" {
		t.Errorf("certificate page = %+v, %v", cd, err)
	}

	if err := p.ActAt(ctx, project, caPath, "disable", nil); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, project, caPath, "delete", map[string]string{}); err == nil {
		t.Error("a CA with an active certificate was deleted without ignore_active_certificates")
	}
	if err := p.ActAt(ctx, project, caPath, "delete", map[string]string{"ignoreActiveCertificates": "true"}); err != nil {
		t.Fatal(err)
	}
	if got := privateCAActions(p.DetailActions(ctx, project, caPath)); got != "undelete" {
		t.Errorf("a DELETED CA offers %s", got)
	}
	if err := p.ActAt(ctx, project, caPath, "undelete", nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.GetCertificateAuthority(ctx, &privatecapb.GetCertificateAuthorityRequest{Name: pool + "/certificateAuthorities/root"}); got.GetState() != privatecapb.CertificateAuthority_DISABLED {
		t.Errorf("undeleted CA is %s, want DISABLED", got.GetState())
	}
	if err := p.ActAt(ctx, project, caPath, "delete", map[string]string{"ignoreActiveCertificates": "true", "skipGracePeriod": "true"}); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, project, []string{pool}, "labels", map[string]string{"labels": ""}); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, project, []string{pool}, "deletepool", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetCaPool(ctx, &privatecapb.GetCaPoolRequest{Name: pool}); err == nil {
		t.Error("the pool is still there after Delete pool")
	}
	// Another project's pool is not opened from this one's page.
	if d, _ := p.Detail(ctx, "other-project", []string{pool}); d.Unavailable == "" {
		t.Error("another project's pool opened")
	}
}

// The Permissions tab of a CA pool reads and writes the policy the
// official client's GetIamPolicy returns.
func TestConsolePrivateCAPoolPermissions(t *testing.T) {
	ctx := context.Background()
	p, c := privateCAFixture(t)
	const project = "pca-iam"
	pool, err := p.Create(ctx, project, map[string]string{"caPoolId": "pool", "location": "europe-west1", "tier": "DEVOPS"})
	if err != nil {
		t.Fatal(err)
	}
	if p.PolicyOn(ctx, []string{pool}) == nil || p.PolicyOn(ctx, []string{pool, "certificateAuthorities", "x"}) != nil {
		t.Fatal("the Permissions tab is not on the pool's page alone")
	}
	pol, err := p.GetPolicy(ctx, project, []string{pool})
	if err != nil {
		t.Fatal(err)
	}
	pol.Bindings = append(pol.Bindings, console.Binding{Role: "roles/privateca.certificateRequester", Members: []string{"user:a@example.com"}})
	if err := p.SetPolicy(ctx, project, []string{pool}, pol); err != nil {
		t.Fatal(err)
	}
	got, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: pool})
	if err != nil || len(got.GetBindings()) != 1 || got.GetBindings()[0].GetMembers()[0] != "user:a@example.com" {
		t.Fatalf("GetIamPolicy = %v, %v", got, err)
	}
	d, _ := p.Detail(ctx, project, []string{pool})
	if !strings.Contains(d.Sections[1].Listing.Prompt, "DevOps") {
		t.Errorf("a DevOps pool's certificates section says %q", d.Sections[1].Listing.Prompt)
	}
}
