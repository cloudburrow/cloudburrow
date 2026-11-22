package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"strings"
	"testing"

	"cloud.google.com/go/security/privateca/apiv1/privatecapb"
)

// The 2026-11-22 round (#2144): Create CA pool's allowed key algorithms,
// encoding format and baseline values; the CA manager's Create CA with
// Select CA pool, the subject's OU, state and locality, and Initialized
// state Enabled; the Private certificate manager's Request a certificate
// from a CSR and by Manually enter details, whose generated key the
// certificate's page offers to download; Create template's predefined
// values and extension constraints. Each is read back with the official
// client.
func TestConsolePrivateCAGoogleRound(t *testing.T) {
	ctx := context.Background()
	p, c := privateCAFixture(t)
	const project = "pca-round"
	ext := base64.StdEncoding.EncodeToString([]byte{0x05, 0x00})
	pool, err := p.Create(ctx, project, map[string]string{"caPoolId": "pool", "location": "us-central1", "tier": "ENTERPRISE",
		"allowCsr": "true", "allowConfig": "true", "encodingFormat": "DER",
		"allowedKeyTypes":     `[{"type":"RSA","minModulus":"2048","maxModulus":"4096","curve":"ECDSA_P256"},{"type":"EC","curve":"ECDSA_P256"}]`,
		"specifyBaseKeyUsage": "true", "kuDigitalSignature": "true", "kuKeyEncipherment": "true",
		"writeExtendedKeyUsage": "true", "ekuServerAuth": "true", "ekuClientAuth": "true",
		"policyIds": "1.2.3.4", "aiaOcspServers": `[{"url":"http://ocsp.example.com"}]`,
		"includeCaOptions": "true", "restrictToCa": "false",
		"additionalExtensions": `[{"oid":"1.2.3.5","value":"` + ext + `","critical":"false"}]`})
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.GetCaPool(ctx, &privatecapb.GetCaPoolRequest{Name: pool})
	if err != nil {
		t.Fatal(err)
	}
	ip := got.GetIssuancePolicy()
	if got.GetPublishingOptions().GetEncodingFormat() != privatecapb.CaPool_PublishingOptions_DER || len(ip.GetAllowedKeyTypes()) != 2 ||
		ip.GetAllowedKeyTypes()[0].GetRsa().GetMinModulusSize() != 2048 || ip.GetAllowedKeyTypes()[1].GetEllipticCurve() == nil {
		t.Fatalf("the pool's publishing options and allowed key types = %v", got)
	}
	bv := ip.GetBaselineValues()
	if !bv.GetKeyUsage().GetBaseKeyUsage().GetKeyEncipherment() || !bv.GetKeyUsage().GetExtendedKeyUsage().GetClientAuth() ||
		len(bv.GetPolicyIds()) != 1 || bv.GetAiaOcspServers()[0] != "http://ocsp.example.com" || bv.GetCaOptions().GetIsCa() ||
		len(bv.GetAdditionalExtensions()) != 1 {
		t.Fatalf("the pool's baseline values = %v", bv)
	}

	cas := privateCACAsProvider{p}
	if f := cas.CreateFormFor(ctx, project); f[0].Name != "caPool" || len(f[0].Options) != 1 || f[0].Options[0] != pool {
		t.Fatalf("Create CA's Select CA pool offers %+v", f[0])
	}
	ca, err := cas.Create(ctx, project, map[string]string{"caPool": pool, "certificateAuthorityId": "root", "type": "SELF_SIGNED",
		"initialState": "ENABLED", "commonName": "Round Root", "organization": "CloudBurrow", "organizationalUnit": "Ops",
		"province": "Ontario", "locality": "Toronto", "countryCode": "CA", "algorithm": "EC_P256_SHA256", "lifetimeDays": "365"})
	if err != nil {
		t.Fatal(err)
	}
	gotCA, err := c.GetCertificateAuthority(ctx, &privatecapb.GetCertificateAuthorityRequest{Name: ca})
	if err != nil || gotCA.GetState() != privatecapb.CertificateAuthority_ENABLED {
		t.Fatalf("the CA made with Initialized state Enabled = %v, %v", gotCA.GetState(), err)
	}
	subj := gotCA.GetConfig().GetSubjectConfig().GetSubject()
	if subj.GetOrganizationalUnit() != "Ops" || subj.GetProvince() != "Ontario" || subj.GetLocality() != "Toronto" {
		t.Errorf("the CA's subject = %v", subj)
	}
	if l, err := cas.List(ctx, project); err != nil || len(l.Items) != 1 || l.Items[0].Status != "ENABLED" || l.Items[0].Fields["Type"] != "Root CA" {
		t.Fatalf("the CA manager lists %+v, %v", l.Items, err)
	}
	// Delete is offered on a disabled CA, in Google's dialog.
	stagedCA, err := cas.Create(ctx, project, map[string]string{"caPool": pool, "certificateAuthorityId": "staged", "type": "SELF_SIGNED",
		"commonName": "Staged", "algorithm": "EC_P256_SHA256", "lifetimeDays": "30"})
	if err != nil {
		t.Fatal(err)
	}
	acts := p.DetailActions(ctx, project, []string{pool, "certificateAuthorities", lastSegment(stagedCA)})
	var deleteLabels []string
	for _, a := range acts {
		if a.ID == "delete" {
			for _, f := range a.Fields {
				deleteLabels = append(deleteLabels, f.Label)
			}
			if a.Title != "Delete Certificate Authority" || a.Submit != "Confirm" {
				t.Errorf("Delete's dialog is %q ending in %q", a.Title, a.Submit)
			}
		}
	}
	if got := strings.Join(deleteLabels, "|"); got != "Delete this CA, even if there are active certificates|Skip the 30 day grace period and delete this CA immediately" {
		t.Errorf("Delete's checkboxes are %s", got)
	}

	certs := privateCACertsProvider{p}
	f := certs.CreateFormFor(ctx, project)
	var poolField, caField bool
	for _, x := range f {
		if x.Name == "caPool" && len(x.OptionsFor["us-central1"]) == 1 {
			poolField = true
		}
		if x.Name == "issuer" && strings.Join(x.OptionsFor[pool], ",") == ",root" {
			caField = true
		}
	}
	if !poolField || !caField {
		t.Fatalf("Request a certificate's fields = %+v", f)
	}
	name, err := certs.Create(ctx, project, map[string]string{"location": "us-central1", "caPool": pool, "useSpecificCa": "true", "issuer": "root",
		"method": "manual", "certificateId": "manual", "lifetimeDays": "7", "domains": `[{"domain":"a.example.com"},{"domain":"b.example.com"}]`,
		"usage": "client_auth", "keyAlgorithm": "EC_P256"})
	if err != nil {
		t.Fatal(err)
	}
	cert, err := c.GetCertificate(ctx, &privatecapb.GetCertificateRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode([]byte(cert.GetPemCertificate()))
	leaf, err := x509.ParseCertificate(blk.Bytes)
	// The pool's baseline adds Server authentication to the Client TLS chosen.
	if err != nil || strings.Join(leaf.DNSNames, ",") != "a.example.com,b.example.com" || !strings.Contains(fmtEKU(leaf.ExtKeyUsage), "client") {
		t.Fatalf("the certificate requested by Manually enter details has names %v and usages %v (%v)", leaf.DNSNames, leaf.ExtKeyUsage, err)
	}
	d, err := p.Detail(ctx, project, []string{pool, "certificates", "manual"})
	if err != nil {
		t.Fatal(err)
	}
	var key, download string
	for _, s := range d.Sections {
		if s.ID == "privatekey" {
			key = s.Text
		}
		if s.ID == "pem" {
			download = s.Download
		}
	}
	kb, _ := pem.Decode([]byte(key))
	if kb == nil || download != "manual.crt" {
		t.Fatalf("the certificate's page offers key %q and downloads %q", key, download)
	}
	priv, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !priv.(*ecdsa.PrivateKey).PublicKey.Equal(leaf.PublicKey) {
		t.Error("the private key the page offers is not the certificate's")
	}
	if l, err := certs.List(ctx, project); err != nil || len(l.Items) != 1 || l.Items[0].Status != "Active" || l.Items[0].Fields["Issuing CA"] != "root" {
		t.Fatalf("the Private certificate manager lists %+v, %v", l.Items, err)
	}
	if _, err := certs.Create(ctx, project, map[string]string{"location": "us-east1", "caPool": pool, "method": "csr", "pemCsr": "x"}); err == nil {
		t.Error("a request whose region is not the pool's was accepted")
	}

	tp := privateCATemplatesProvider{svc: p.svc}
	for mode, want := range map[string]func(*privatecapb.CertificateExtensionConstraints) bool{
		"copyAll": func(e *privatecapb.CertificateExtensionConstraints) bool { return e == nil },
		"dropAll": func(e *privatecapb.CertificateExtensionConstraints) bool {
			return e != nil && len(e.GetKnownExtensions()) == 0
		},
		"specific": func(e *privatecapb.CertificateExtensionConstraints) bool {
			return len(e.GetKnownExtensions()) == 1 && len(e.GetAdditionalExtensions()) == 1
		},
	} {
		tmpl, err := tp.Create(ctx, project, map[string]string{"certificateTemplateId": "t-" + strings.ToLower(mode), "location": "us-central1",
			"includePredefined": "true", "specifyBaseKeyUsage": "true", "kuDigitalSignature": "true",
			"extensionMode": mode, "knownExtensions": "BASE_KEY_USAGE", "customExtensions": "1.2.3.6",
			"allowSubjectPassthrough": "true", "allowSansPassthrough": "true"})
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		got, err := c.GetCertificateTemplate(ctx, &privatecapb.GetCertificateTemplateRequest{Name: tmpl})
		if err != nil || !got.GetPredefinedValues().GetKeyUsage().GetBaseKeyUsage().GetDigitalSignature() || !want(got.GetPassthroughExtensions()) {
			t.Errorf("%s: the template = %v, %v", mode, got, err)
		}
	}
}

// fmtEKU names extended key usages.
func fmtEKU(us []x509.ExtKeyUsage) string {
	var out []string
	for _, u := range us {
		switch u {
		case x509.ExtKeyUsageClientAuth:
			out = append(out, "client")
		case x509.ExtKeyUsageServerAuth:
			out = append(out, "server")
		}
	}
	return strings.Join(out, ",")
}
