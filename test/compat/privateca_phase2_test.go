//go:build compat

package compat

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	privateca "cloud.google.com/go/security/privateca/apiv1"
	"cloud.google.com/go/security/privateca/apiv1/privatecapb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// TestPrivateCASubordinatesTemplatesAndCRLs (#1834, phase 2) drives
// Certificate Authority Service through the official client, over gRPC and
// REST:
//   - a pool publishing its CA certificates and CRLs, and a root CA in it,
//     whose access URLs serve its certificate and a CRL it signed;
//   - a subordinate CA created AWAITING_USER_ACTIVATION, whose CSR
//     (FetchCertificateAuthorityCsr) the root signs through CreateCertificate
//     and which ActivateCertificateAuthority makes STAGED with the root as
//     its issuer, then ENABLED, issuing a leaf that verifies through it to
//     the root and carries the subordinate's AIA and CRL URLs; its full
//     subject is kept;
//   - a subordinate signed by a third-party root (pem_issuer_chain); a
//     certificate for another key is refused;
//   - a certificate template's predefined values and identity constraints
//     shape a certificate; it is got, listed, updated and deleted, its IAM
//     policy set, and a template in another location is refused;
//   - revoking the leaf regenerates the subordinate's CRL, which lists it
//     in the resource (Get/List) and at its access URL; its labels are
//     updated and its IAM policy set.
//
// covers: google.cloud.security.privateca.v1.CertificateAuthorityService/ActivateCertificateAuthority, google.cloud.security.privateca.v1.CertificateAuthorityService/FetchCertificateAuthorityCsr, google.cloud.security.privateca.v1.CertificateAuthorityService/CreateCertificateTemplate, google.cloud.security.privateca.v1.CertificateAuthorityService/GetCertificateTemplate, google.cloud.security.privateca.v1.CertificateAuthorityService/ListCertificateTemplates, google.cloud.security.privateca.v1.CertificateAuthorityService/UpdateCertificateTemplate, google.cloud.security.privateca.v1.CertificateAuthorityService/DeleteCertificateTemplate, google.cloud.security.privateca.v1.CertificateAuthorityService/GetCertificateRevocationList, google.cloud.security.privateca.v1.CertificateAuthorityService/ListCertificateRevocationLists, google.cloud.security.privateca.v1.CertificateAuthorityService/UpdateCertificateRevocationList
func TestPrivateCASubordinatesTemplatesAndCRLs(t *testing.T) {
	h := New(t)
	clients := privateCAClients(t, h)
	addr := h.Endpoint(EnvPrivateCA)
	for _, transport := range []string{"grpc", "rest"} {
		t.Run(transport, func(t *testing.T) {
			privateCAPhase2(t, h, clients[transport], transport, addr)
		})
	}
}

// fetchPublished GETs a published URL, which must be this instance's.
func fetchPublished(t *testing.T, addr, url string) []byte {
	t.Helper()
	if !strings.HasPrefix(url, "http://"+addr+"/") {
		t.Fatalf("access URL %q is not served by the instance at %s", url, addr)
	}
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", url, resp.StatusCode, b)
	}
	return b
}

func parseCRL(t *testing.T, b []byte) *x509.RevocationList {
	t.Helper()
	if blk, _ := pem.Decode(b); blk != nil {
		b = blk.Bytes
	}
	crl, err := x509.ParseRevocationList(b)
	if err != nil {
		t.Fatalf("parse CRL: %v", err)
	}
	return crl
}

func certOf(t *testing.T, p string) *x509.Certificate {
	t.Helper()
	blk, _ := pem.Decode([]byte(p))
	if blk == nil {
		t.Fatalf("not PEM: %q", p)
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func caConfig(cn string) *privatecapb.CertificateConfig {
	isCA := true
	return &privatecapb.CertificateConfig{
		SubjectConfig: &privatecapb.CertificateConfig_SubjectConfig{Subject: &privatecapb.Subject{CommonName: cn, Organization: "CloudBurrow"}},
		X509Config: &privatecapb.X509Parameters{CaOptions: &privatecapb.X509Parameters_CaOptions{IsCa: &isCA},
			KeyUsage: &privatecapb.KeyUsage{BaseKeyUsage: &privatecapb.KeyUsage_KeyUsageOptions{CertSign: true, CrlSign: true}}},
	}
}

func privateCAPhase2(t *testing.T, h *Harness, c *privateca.CertificateAuthorityClient, transport, addr string) {
	ctx := h.Context()
	loc := "projects/" + h.Project() + "/locations/us-central1"
	suffix := fmt.Sprintf("%s-%d", transport, time.Now().UnixNano()%1e9)
	rootPool, subPool := loc+"/caPools/p2root-"+suffix, loc+"/caPools/p2sub-"+suffix
	for _, p := range []string{rootPool, subPool} {
		op, err := c.CreateCaPool(ctx, &privatecapb.CreateCaPoolRequest{Parent: loc, CaPoolId: p[strings.LastIndex(p, "/")+1:], CaPool: &privatecapb.CaPool{
			Tier:              privatecapb.CaPool_ENTERPRISE,
			PublishingOptions: &privatecapb.CaPool_PublishingOptions{PublishCaCert: true, PublishCrl: true},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := op.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var cleanup []string
	defer func() {
		for _, n := range cleanup {
			if op, err := c.DisableCertificateAuthority(ctx, &privatecapb.DisableCertificateAuthorityRequest{Name: n}); err == nil {
				_, _ = op.Wait(ctx)
			}
			if op, err := c.DeleteCertificateAuthority(ctx, &privatecapb.DeleteCertificateAuthorityRequest{Name: n, SkipGracePeriod: true, IgnoreActiveCertificates: true}); err == nil {
				_, _ = op.Wait(ctx)
			}
		}
		for _, p := range []string{subPool, rootPool} {
			if op, err := c.DeleteCaPool(ctx, &privatecapb.DeleteCaPoolRequest{Name: p}); err == nil {
				_ = op.Wait(ctx)
			}
		}
	}()
	// The root, enabled, publishing its certificate and an empty CRL.
	rop, err := c.CreateCertificateAuthority(ctx, &privatecapb.CreateCertificateAuthorityRequest{Parent: rootPool, CertificateAuthorityId: "root",
		CertificateAuthority: &privatecapb.CertificateAuthority{Type: privatecapb.CertificateAuthority_SELF_SIGNED, Config: caConfig("Phase 2 Root"),
			Lifetime: durationpb.New(365 * 24 * time.Hour),
			KeySpec:  &privatecapb.CertificateAuthority_KeyVersionSpec{KeyVersion: &privatecapb.CertificateAuthority_KeyVersionSpec_Algorithm{Algorithm: privatecapb.CertificateAuthority_EC_P256_SHA256}}}})
	if err != nil {
		t.Fatal(err)
	}
	root, err := rop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cleanup = append(cleanup, root.GetName())
	if eop, err := c.EnableCertificateAuthority(ctx, &privatecapb.EnableCertificateAuthorityRequest{Name: root.GetName()}); err != nil {
		t.Fatal(err)
	} else if root, err = eop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	rootCert := certOf(t, root.GetPemCaCertificates()[0])
	urls := root.GetAccessUrls()
	if urls.GetCaCertificateAccessUrl() == "" || len(urls.GetCrlAccessUrls()) != 1 {
		t.Fatalf("root access URLs = %v; want the CA certificate's and one CRL's", urls)
	}
	if got := fetchPublished(t, addr, urls.GetCaCertificateAccessUrl()); !strings.Contains(string(got), strings.TrimSpace(root.GetPemCaCertificates()[0])) {
		t.Errorf("the published CA certificate is not the root's: %s", got)
	}
	if crl := parseCRL(t, fetchPublished(t, addr, urls.GetCrlAccessUrls()[0])); crl.CheckSignatureFrom(rootCert) != nil || len(crl.RevokedCertificateEntries) != 0 {
		t.Errorf("the root's published CRL: signature %v, %d entries; want the root's signature and none", crl.CheckSignatureFrom(rootCert), len(crl.RevokedCertificateEntries))
	}

	// A subordinate issued by the root, as gcloud and Terraform activate one.
	isCA := true
	fullSubject := &privatecapb.Subject{CommonName: "Phase 2 Sub", Organization: "CloudBurrow", OrganizationalUnit: "Eng", CountryCode: "US",
		Province: "NY", Locality: "New York", StreetAddress: "1 Main St", PostalCode: "10001"}
	sop, err := c.CreateCertificateAuthority(ctx, &privatecapb.CreateCertificateAuthorityRequest{Parent: subPool, CertificateAuthorityId: "sub",
		CertificateAuthority: &privatecapb.CertificateAuthority{Type: privatecapb.CertificateAuthority_SUBORDINATE,
			Config: &privatecapb.CertificateConfig{SubjectConfig: &privatecapb.CertificateConfig_SubjectConfig{Subject: fullSubject},
				X509Config: &privatecapb.X509Parameters{CaOptions: &privatecapb.X509Parameters_CaOptions{IsCa: &isCA, MaxIssuerPathLength: new(int32)},
					KeyUsage: &privatecapb.KeyUsage{BaseKeyUsage: &privatecapb.KeyUsage_KeyUsageOptions{CertSign: true, CrlSign: true}}}},
			Lifetime: durationpb.New(90 * 24 * time.Hour),
			KeySpec:  &privatecapb.CertificateAuthority_KeyVersionSpec{KeyVersion: &privatecapb.CertificateAuthority_KeyVersionSpec_Algorithm{Algorithm: privatecapb.CertificateAuthority_RSA_PKCS1_2048_SHA256}}}})
	if err != nil {
		t.Fatal(err)
	}
	sub, err := sop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cleanup = append([]string{sub.GetName()}, cleanup...)
	if sub.GetState() != privatecapb.CertificateAuthority_AWAITING_USER_ACTIVATION || len(sub.GetPemCaCertificates()) != 0 {
		t.Fatalf("a new subordinate is %s with %d certificates; want AWAITING_USER_ACTIVATION and none", sub.GetState(), len(sub.GetPemCaCertificates()))
	}
	_, err = c.CreateCertificate(ctx, &privatecapb.CreateCertificateRequest{Parent: subPool, CertificateId: "early",
		Certificate: &privatecapb.Certificate{Lifetime: durationpb.New(time.Hour), CertificateConfig: &privatecapb.Certificate_PemCsr{PemCsr: "x"}}})
	wantPrivateCACode(t, "issue from a pool whose only CA awaits activation", err, codes.FailedPrecondition)
	csrResp, err := c.FetchCertificateAuthorityCsr(ctx, &privatecapb.FetchCertificateAuthorityCsrRequest{Name: sub.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.FetchCertificateAuthorityCsr(ctx, &privatecapb.FetchCertificateAuthorityCsrRequest{Name: root.GetName()})
	wantPrivateCACode(t, "the CSR of an enabled root", err, codes.FailedPrecondition)
	signed, err := c.CreateCertificate(ctx, &privatecapb.CreateCertificateRequest{Parent: rootPool, CertificateId: "sub-cert", IssuingCertificateAuthorityId: "root",
		Certificate: &privatecapb.Certificate{Lifetime: durationpb.New(90 * 24 * time.Hour), CertificateConfig: &privatecapb.Certificate_PemCsr{PemCsr: csrResp.GetPemCsr()}}})
	if err != nil {
		t.Fatal(err)
	}
	if sc := certOf(t, signed.GetPemCertificate()); !sc.IsCA || !sc.MaxPathLenZero {
		t.Errorf("the subordinate's certificate: CA %v, path length zero %v; want the CA config its CSR carries", sc.IsCA, sc.MaxPathLenZero)
	}
	aop, err := c.ActivateCertificateAuthority(ctx, &privatecapb.ActivateCertificateAuthorityRequest{Name: sub.GetName(), PemCaCertificate: signed.GetPemCertificate(),
		SubordinateConfig: &privatecapb.SubordinateConfig{SubordinateConfig: &privatecapb.SubordinateConfig_CertificateAuthority{CertificateAuthority: root.GetName()}}})
	if err != nil {
		t.Fatal(err)
	}
	if sub, err = aop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if sub.GetState() != privatecapb.CertificateAuthority_STAGED || len(sub.GetPemCaCertificates()) != 2 ||
		sub.GetSubordinateConfig().GetCertificateAuthority() != root.GetName() {
		t.Fatalf("the activated subordinate is %s with %d certificates, issuer %q", sub.GetState(), len(sub.GetPemCaCertificates()), sub.GetSubordinateConfig().GetCertificateAuthority())
	}
	if got := sub.GetCaCertificateDescriptions()[0].GetSubjectDescription().GetSubject(); got.GetStreetAddress() != "1 Main St" || got.GetPostalCode() != "10001" ||
		got.GetOrganizationalUnit() != "Eng" || got.GetProvince() != "NY" || got.GetLocality() != "New York" || got.GetCountryCode() != "US" {
		t.Errorf("the subordinate's subject is %v; want every field of %v", got, fullSubject)
	}
	if eop, err := c.EnableCertificateAuthority(ctx, &privatecapb.EnableCertificateAuthorityRequest{Name: sub.GetName()}); err != nil {
		t.Fatal(err)
	} else if sub, err = eop.Wait(ctx); err != nil {
		t.Fatal(err)
	}

	// A template: client auth only, the subject not passed through.
	tmplName := loc + "/certificateTemplates/p2tmpl-" + suffix
	top, err := c.CreateCertificateTemplate(ctx, &privatecapb.CreateCertificateTemplateRequest{Parent: loc, CertificateTemplateId: tmplName[strings.LastIndex(tmplName, "/")+1:],
		CertificateTemplate: &privatecapb.CertificateTemplate{Description: "clients",
			PredefinedValues: &privatecapb.X509Parameters{KeyUsage: &privatecapb.KeyUsage{
				BaseKeyUsage:     &privatecapb.KeyUsage_KeyUsageOptions{DigitalSignature: true},
				ExtendedKeyUsage: &privatecapb.KeyUsage_ExtendedKeyUsageOptions{ClientAuth: true}}},
			IdentityConstraints: &privatecapb.CertificateIdentityConstraints{AllowSubjectPassthrough: new(bool), AllowSubjectAltNamesPassthrough: &isCA},
			MaximumLifetime:     durationpb.New(24 * time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := top.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if op, err := c.DeleteCertificateTemplate(ctx, &privatecapb.DeleteCertificateTemplateRequest{Name: tmplName}); err == nil {
			_ = op.Wait(ctx)
		}
	}()
	if got, err := c.GetCertificateTemplate(ctx, &privatecapb.GetCertificateTemplateRequest{Name: tmplName}); err != nil || got.GetDescription() != "clients" {
		t.Fatalf("GetCertificateTemplate = %v, %v", got, err)
	}
	found := false
	for it := c.ListCertificateTemplates(ctx, &privatecapb.ListCertificateTemplatesRequest{Parent: loc}); ; {
		tt, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		found = found || tt.GetName() == tmplName
	}
	if !found {
		t.Errorf("ListCertificateTemplates does not list %s", tmplName)
	}
	if uop, err := c.UpdateCertificateTemplate(ctx, &privatecapb.UpdateCertificateTemplateRequest{
		CertificateTemplate: &privatecapb.CertificateTemplate{Name: tmplName, Description: "client certificates"},
		UpdateMask:          &fieldmaskpb.FieldMask{Paths: []string{"description"}}}); err != nil {
		t.Fatal(err)
	} else if got, err := uop.Wait(ctx); err != nil || got.GetDescription() != "client certificates" {
		t.Errorf("UpdateCertificateTemplate = %v, %v", got, err)
	}
	if _, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: tmplName, Policy: &iampb.Policy{Bindings: []*iampb.Binding{
		{Role: "roles/privateca.templateUser", Members: []string{"user:a@example.com"}}}}}); err != nil {
		t.Fatal(err)
	}
	if pol, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: tmplName}); err != nil || len(pol.GetBindings()) != 1 {
		t.Errorf("the template's IAM policy = %v, %v", pol, err)
	}

	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	pubDER, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	cfg := &privatecapb.CertificateConfig{
		SubjectConfig: &privatecapb.CertificateConfig_SubjectConfig{Subject: &privatecapb.Subject{CommonName: "dropped"},
			SubjectAltName: &privatecapb.SubjectAltNames{DnsNames: []string{"client.example.com"}}},
		X509Config: &privatecapb.X509Parameters{KeyUsage: &privatecapb.KeyUsage{ExtendedKeyUsage: &privatecapb.KeyUsage_ExtendedKeyUsageOptions{ServerAuth: true}}},
		PublicKey:  &privatecapb.PublicKey{Format: privatecapb.PublicKey_PEM, Key: pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})},
	}
	_, err = c.CreateCertificate(ctx, &privatecapb.CreateCertificateRequest{Parent: subPool, CertificateId: "wrongloc",
		Certificate: &privatecapb.Certificate{Lifetime: durationpb.New(time.Hour), CertificateTemplate: "projects/" + h.Project() + "/locations/europe-west1/certificateTemplates/x",
			CertificateConfig: &privatecapb.Certificate_Config{Config: cfg}}})
	wantPrivateCACode(t, "a template in another location", err, codes.InvalidArgument)
	leaf, err := c.CreateCertificate(ctx, &privatecapb.CreateCertificateRequest{Parent: subPool, CertificateId: "leaf",
		Certificate: &privatecapb.Certificate{Lifetime: durationpb.New(72 * time.Hour), CertificateTemplate: tmplName,
			CertificateConfig: &privatecapb.Certificate_Config{Config: cfg}}})
	if err != nil {
		t.Fatal(err)
	}
	lc := certOf(t, leaf.GetPemCertificate())
	if lc.Subject.CommonName != "" || len(lc.ExtKeyUsage) != 1 || lc.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth ||
		lc.NotAfter.Sub(lc.NotBefore) > 24*time.Hour || leaf.GetCertificateTemplate() != tmplName {
		t.Errorf("the templated leaf: subject %q, EKU %v, lifetime %s; want no subject, client auth only, at most 24h",
			lc.Subject, lc.ExtKeyUsage, lc.NotAfter.Sub(lc.NotBefore))
	}
	inter := x509.NewCertPool()
	inter.AddCert(certOf(t, sub.GetPemCaCertificates()[0]))
	roots := x509.NewCertPool()
	roots.AddCert(rootCert)
	if _, err := lc.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, DNSName: "client.example.com", KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Errorf("the leaf does not verify through the subordinate to the root: %v", err)
	}
	if su := sub.GetAccessUrls(); len(lc.CRLDistributionPoints) != 1 || lc.CRLDistributionPoints[0] != su.GetCrlAccessUrls()[0] ||
		len(lc.IssuingCertificateURL) != 1 || lc.IssuingCertificateURL[0] != su.GetCaCertificateAccessUrl() {
		t.Errorf("the leaf names CRL %v and issuer %v; want the subordinate's %v", lc.CRLDistributionPoints, lc.IssuingCertificateURL, su)
	}

	// Revoking the leaf regenerates the subordinate's CRL.
	if _, err := c.RevokeCertificate(ctx, &privatecapb.RevokeCertificateRequest{Name: leaf.GetName(), Reason: privatecapb.RevocationReason_KEY_COMPROMISE}); err != nil {
		t.Fatal(err)
	}
	var crls []*privatecapb.CertificateRevocationList
	for it := c.ListCertificateRevocationLists(ctx, &privatecapb.ListCertificateRevocationListsRequest{Parent: sub.GetName()}); ; {
		r, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		crls = append(crls, r)
	}
	if len(crls) != 1 {
		t.Fatalf("the subordinate has %d CRLs, want 1", len(crls))
	}
	crl, err := c.GetCertificateRevocationList(ctx, &privatecapb.GetCertificateRevocationListRequest{Name: crls[0].GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if crl.GetState() != privatecapb.CertificateRevocationList_ACTIVE || crl.GetSequenceNumber() < 2 || len(crl.GetRevokedCertificates()) != 1 ||
		crl.GetRevokedCertificates()[0].GetCertificate() != leaf.GetName() || crl.GetAccessUrl() != sub.GetAccessUrls().GetCrlAccessUrls()[0] {
		t.Errorf("the CRL after a revocation = %v", crl)
	}
	pub := parseCRL(t, fetchPublished(t, addr, crl.GetAccessUrl()))
	if err := pub.CheckSignatureFrom(certOf(t, sub.GetPemCaCertificates()[0])); err != nil || len(pub.RevokedCertificateEntries) != 1 ||
		pub.RevokedCertificateEntries[0].SerialNumber.Cmp(lc.SerialNumber) != 0 || pub.RevokedCertificateEntries[0].ReasonCode != 1 {
		t.Errorf("the published CRL: signature %v, entries %+v; want the leaf's serial with keyCompromise", err, pub.RevokedCertificateEntries)
	}
	if uop, err := c.UpdateCertificateRevocationList(ctx, &privatecapb.UpdateCertificateRevocationListRequest{
		CertificateRevocationList: &privatecapb.CertificateRevocationList{Name: crl.GetName(), Labels: map[string]string{"env": "test"}},
		UpdateMask:                &fieldmaskpb.FieldMask{Paths: []string{"labels"}}}); err != nil {
		t.Fatal(err)
	} else if got, err := uop.Wait(ctx); err != nil || got.GetLabels()["env"] != "test" {
		t.Errorf("UpdateCertificateRevocationList = %v, %v", got, err)
	}
	if _, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: crl.GetName(), Policy: &iampb.Policy{Bindings: []*iampb.Binding{
		{Role: "roles/privateca.auditor", Members: []string{"user:a@example.com"}}}}}); err != nil {
		t.Errorf("SetIamPolicy on the CRL: %v", err)
	}

	// A subordinate signed by a third-party root.
	extKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	extTmpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "External Root"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(365 * 24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	extDER, _ := x509.CreateCertificate(rand.Reader, extTmpl, extTmpl, &extKey.PublicKey, extKey)
	extRoot, _ := x509.ParseCertificate(extDER)
	xop, err := c.CreateCertificateAuthority(ctx, &privatecapb.CreateCertificateAuthorityRequest{Parent: subPool, CertificateAuthorityId: "external",
		CertificateAuthority: &privatecapb.CertificateAuthority{Type: privatecapb.CertificateAuthority_SUBORDINATE, Config: caConfig("Externally Issued"),
			Lifetime: durationpb.New(30 * 24 * time.Hour),
			KeySpec:  &privatecapb.CertificateAuthority_KeyVersionSpec{KeyVersion: &privatecapb.CertificateAuthority_KeyVersionSpec_Algorithm{Algorithm: privatecapb.CertificateAuthority_EC_P384_SHA384}}}})
	if err != nil {
		t.Fatal(err)
	}
	ext, err := xop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cleanup = append([]string{ext.GetName()}, cleanup...)
	xcsr, err := c.FetchCertificateAuthorityCsr(ctx, &privatecapb.FetchCertificateAuthorityCsrRequest{Name: ext.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode([]byte(xcsr.GetPemCsr()))
	req, err := x509.ParseCertificateRequest(blk.Bytes)
	if err != nil || req.CheckSignature() != nil || req.Subject.CommonName != "Externally Issued" {
		t.Fatalf("the CSR: %v, subject %q", err, req.Subject)
	}
	sign := func(pub any) string {
		tm := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: req.Subject, NotBefore: time.Now().Add(-time.Minute),
			NotAfter: time.Now().Add(30 * 24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
		der, err := x509.CreateCertificate(rand.Reader, tm, extRoot, pub, extKey)
		if err != nil {
			t.Fatal(err)
		}
		return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	}
	chain := &privatecapb.SubordinateConfig{SubordinateConfig: &privatecapb.SubordinateConfig_PemIssuerChain{PemIssuerChain: &privatecapb.SubordinateConfig_SubordinateConfigChain{
		PemCertificates: []string{string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: extDER}))}}}}
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	_, err = c.ActivateCertificateAuthority(ctx, &privatecapb.ActivateCertificateAuthorityRequest{Name: ext.GetName(), PemCaCertificate: sign(&other.PublicKey), SubordinateConfig: chain})
	wantPrivateCACode(t, "activate with a certificate for another key", err, codes.InvalidArgument)
	xa, err := c.ActivateCertificateAuthority(ctx, &privatecapb.ActivateCertificateAuthorityRequest{Name: ext.GetName(), PemCaCertificate: sign(req.PublicKey), SubordinateConfig: chain})
	if err != nil {
		t.Fatal(err)
	}
	if ext, err = xa.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if ext.GetState() != privatecapb.CertificateAuthority_STAGED || len(ext.GetPemCaCertificates()) != 2 || len(ext.GetSubordinateConfig().GetPemIssuerChain().GetPemCertificates()) != 1 {
		t.Errorf("the externally issued subordinate is %s with %d certificates", ext.GetState(), len(ext.GetPemCaCertificates()))
	}
	_, err = c.ActivateCertificateAuthority(ctx, &privatecapb.ActivateCertificateAuthorityRequest{Name: ext.GetName(), PemCaCertificate: sign(req.PublicKey), SubordinateConfig: chain})
	wantPrivateCACode(t, "activate twice", err, codes.FailedPrecondition)
}
