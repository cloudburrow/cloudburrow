//go:build compat

package compat

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"cloud.google.com/go/security/privateca/apiv1/privatecapb"
)

// TestGcloudPrivateCA (#1834): `gcloud privateca`, through the
// configuration `cloudburrow gcloud-setup` writes and with egress to Google
// blocked, creates two CA pools, a root CA (--auto-enable), a subordinate
// issued by it from the other pool (gcloud fetches its CSR, has the root's
// pool sign it and activates it), a certificate template, and a
// certificate with a generated key and the template; describes and lists
// them; revokes the certificate; and disables and deletes the CAs and
// pools. The official client checks the results.
// covers: google.cloud.security.privateca.v1.CertificateAuthorityService/CreateCaPool, google.cloud.security.privateca.v1.CertificateAuthorityService/CreateCertificateAuthority, google.cloud.security.privateca.v1.CertificateAuthorityService/EnableCertificateAuthority, google.cloud.security.privateca.v1.CertificateAuthorityService/FetchCertificateAuthorityCsr, google.cloud.security.privateca.v1.CertificateAuthorityService/ActivateCertificateAuthority, google.cloud.security.privateca.v1.CertificateAuthorityService/CreateCertificateTemplate, google.cloud.security.privateca.v1.CertificateAuthorityService/CreateCertificate, google.cloud.security.privateca.v1.CertificateAuthorityService/RevokeCertificate
func TestGcloudPrivateCA(t *testing.T) {
	h := New(t)
	c := privateCAClients(t, h)["grpc"] // skips without CLOUDBURROW_TEST_PRIVATECA
	ctx := h.Context()
	g := newGcloudSession(t, h)
	n := time.Now().UnixNano() % 1e6
	rootPool, subPool, tmpl := fmt.Sprintf("groot-%d", n), fmt.Sprintf("gsub-%d", n), fmt.Sprintf("gtmpl-%d", n)
	loc := []string{"--location", "us-central1", "--project", g.project, "--quiet"}
	// certificates commands take --issuer-location, not --location.
	runIn := func(flags []string, args ...string) string {
		t.Helper()
		out, err := g.run(nil, append(append([]string{"privateca"}, args...), flags...)...)
		if err != nil {
			t.Fatalf("gcloud privateca %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	run := func(args ...string) string { t.Helper(); return runIn(loc, args...) }
	certFlags := []string{"--project", g.project, "--quiet"}
	parent := "projects/" + g.project + "/locations/us-central1"

	run("pools", "create", rootPool, "--tier", "enterprise")
	run("pools", "create", subPool, "--tier", "enterprise")
	defer func() {
		for _, p := range []string{subPool, rootPool} {
			_, _ = g.run(nil, append([]string{"privateca", "pools", "delete", p, "--ignore-dependent-resources"}, loc...)...)
		}
	}()
	if out := run("pools", "list", "--format", "value(name)"); !strings.Contains(out, rootPool) || !strings.Contains(out, subPool) {
		t.Errorf("pools list: %s", out)
	}
	run("roots", "create", "root", "--pool", rootPool, "--subject", "CN=gcloud Root, O=CloudBurrow", "--key-algorithm", "ec-p256-sha256", "--auto-enable")
	defer func() {
		_, _ = g.run(nil, append([]string{"privateca", "roots", "disable", "root", "--pool", rootPool}, loc...)...)
		_, _ = g.run(nil, append([]string{"privateca", "roots", "delete", "root", "--pool", rootPool, "--skip-grace-period", "--ignore-active-certificates"}, loc...)...)
	}()
	if out := run("roots", "describe", "root", "--pool", rootPool, "--format", "value(state)"); strings.TrimSpace(out) != "ENABLED" {
		t.Errorf("roots describe: %s", out)
	}

	run("subordinates", "create", "sub", "--pool", subPool, "--issuer-pool", rootPool, "--issuer-location", "us-central1",
		"--subject", "CN=gcloud Sub, O=CloudBurrow", "--key-algorithm", "rsa-pkcs1-2048-sha256", "--auto-enable")
	defer func() {
		_, _ = g.run(nil, append([]string{"privateca", "subordinates", "disable", "sub", "--pool", subPool}, loc...)...)
		_, _ = g.run(nil, append([]string{"privateca", "subordinates", "delete", "sub", "--pool", subPool, "--skip-grace-period", "--ignore-active-certificates"}, loc...)...)
	}()
	sub, err := c.GetCertificateAuthority(ctx, &privatecapb.GetCertificateAuthorityRequest{Name: parent + "/caPools/" + subPool + "/certificateAuthorities/sub"})
	if err != nil || sub.GetState() != privatecapb.CertificateAuthority_ENABLED || len(sub.GetPemCaCertificates()) != 2 {
		t.Fatalf("the subordinate gcloud made = %v, %v; want ENABLED with its certificate and the root's", sub, err)
	}
	if out := run("subordinates", "list", "--pool", subPool, "--format", "value(name)"); !strings.Contains(out, "sub") {
		t.Errorf("subordinates list: %s", out)
	}

	values := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(values, []byte("keyUsage:\n  baseKeyUsage:\n    digitalSignature: true\n  extendedKeyUsage:\n    clientAuth: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("templates", "create", tmpl, "--description", "gcloud clients", "--predefined-values-file", values, "--no-copy-subject", "--copy-sans")
	defer func() { _, _ = g.run(nil, append([]string{"privateca", "templates", "delete", tmpl}, loc...)...) }()
	if out := run("templates", "describe", tmpl, "--format", "value(description)"); strings.TrimSpace(out) != "gcloud clients" {
		t.Errorf("templates describe: %s", out)
	}

	dir := t.TempDir()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	csrDER, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: []string{"gcloud.example.com"}}, key)
	csr := filepath.Join(dir, "req.csr")
	if err := os.WriteFile(csr, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}), 0o644); err != nil {
		t.Fatal(err)
	}
	certFile := filepath.Join(dir, "cert.pem")
	runIn(certFlags, "certificates", "create", "gcert", "--issuer-pool", subPool, "--issuer-location", "us-central1", "--csr", csr,
		"--cert-output-file", certFile, "--template", parent+"/certificateTemplates/"+tmpl, "--validity", "P1D")
	b, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(b)
	leaf, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM([]byte(sub.GetPemCaCertificates()[1]))
	inter := x509.NewCertPool()
	inter.AppendCertsFromPEM([]byte(sub.GetPemCaCertificates()[0]))
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, DNSName: "gcloud.example.com",
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Errorf("the certificate gcloud wrote does not verify through the subordinate: %v", err)
	}
	runIn(certFlags, "certificates", "revoke", "--certificate", "gcert", "--issuer-pool", subPool, "--issuer-location", "us-central1", "--reason", "key_compromise")
	got, err := c.GetCertificate(ctx, &privatecapb.GetCertificateRequest{Name: parent + "/caPools/" + subPool + "/certificates/gcert"})
	if err != nil || got.GetRevocationDetails().GetRevocationState() != privatecapb.RevocationReason_KEY_COMPROMISE {
		t.Errorf("the certificate after gcloud revoke = %v, %v", got.GetRevocationDetails(), err)
	}
}

// TestTerraformPrivateCA (#1834): google_privateca_ca_pool (two),
// _certificate_authority (a root, and a subordinate the provider has the
// root sign and activates), _certificate_template and _certificate (from a
// config with a public key, under the template) apply with egress to Google
// blocked; the official client reads them; the plan is clean; destroy
// removes them.
// covers: google.cloud.security.privateca.v1.CertificateAuthorityService/CreateCaPool, google.cloud.security.privateca.v1.CertificateAuthorityService/GetCaPool, google.cloud.security.privateca.v1.CertificateAuthorityService/DeleteCaPool, google.cloud.security.privateca.v1.CertificateAuthorityService/CreateCertificateAuthority, google.cloud.security.privateca.v1.CertificateAuthorityService/GetCertificateAuthority, google.cloud.security.privateca.v1.CertificateAuthorityService/EnableCertificateAuthority, google.cloud.security.privateca.v1.CertificateAuthorityService/DisableCertificateAuthority, google.cloud.security.privateca.v1.CertificateAuthorityService/DeleteCertificateAuthority, google.cloud.security.privateca.v1.CertificateAuthorityService/FetchCertificateAuthorityCsr, google.cloud.security.privateca.v1.CertificateAuthorityService/ActivateCertificateAuthority, google.cloud.security.privateca.v1.CertificateAuthorityService/CreateCertificateTemplate, google.cloud.security.privateca.v1.CertificateAuthorityService/GetCertificateTemplate, google.cloud.security.privateca.v1.CertificateAuthorityService/DeleteCertificateTemplate, google.cloud.security.privateca.v1.CertificateAuthorityService/CreateCertificate, google.cloud.security.privateca.v1.CertificateAuthorityService/GetCertificate
func TestTerraformPrivateCA(t *testing.T) {
	h := New(t)
	c := privateCAClients(t, h)["grpc"] // skips without CLOUDBURROW_TEST_PRIVATECA
	ctx := h.Context()
	m := newTFModule(t)
	n := time.Now().UnixNano() % 1e6
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	pubDER, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	pub := strings.TrimSpace(string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})))
	m.write(fmt.Sprintf(`
resource "google_privateca_ca_pool" "root" {
  project  = %[1]q
  location = "us-central1"
  name     = "tfroot-%[2]d"
  tier     = "ENTERPRISE"
  publishing_options {
    publish_ca_cert = true
    publish_crl     = true
  }
}
resource "google_privateca_ca_pool" "sub" {
  project  = %[1]q
  location = "us-central1"
  name     = "tfsub-%[2]d"
  tier     = "ENTERPRISE"
}
resource "google_privateca_certificate_authority" "root" {
  project                                = %[1]q
  location                               = "us-central1"
  pool                                   = google_privateca_ca_pool.root.name
  certificate_authority_id               = "root"
  deletion_protection                    = false
  skip_grace_period                      = true
  ignore_active_certificates_on_deletion = true
  lifetime                               = "864000s"
  config {
    subject_config {
      subject {
        organization = "CloudBurrow"
        common_name  = "Terraform Root"
      }
    }
    x509_config {
      ca_options {
        is_ca = true
      }
      key_usage {
        base_key_usage {
          cert_sign = true
          crl_sign  = true
        }
        extended_key_usage {}
      }
    }
  }
  key_spec {
    algorithm = "EC_P256_SHA256"
  }
}
resource "google_privateca_certificate_authority" "sub" {
  project                                = %[1]q
  location                               = "us-central1"
  pool                                   = google_privateca_ca_pool.sub.name
  certificate_authority_id               = "sub"
  type                                   = "SUBORDINATE"
  deletion_protection                    = false
  skip_grace_period                      = true
  ignore_active_certificates_on_deletion = true
  lifetime                               = "432000s"
  subordinate_config {
    certificate_authority = google_privateca_certificate_authority.root.name
  }
  config {
    subject_config {
      subject {
        organization = "CloudBurrow"
        common_name  = "Terraform Sub"
      }
    }
    x509_config {
      ca_options {
        is_ca = true
      }
      key_usage {
        base_key_usage {
          cert_sign = true
          crl_sign  = true
        }
        extended_key_usage {}
      }
    }
  }
  key_spec {
    algorithm = "EC_P256_SHA256"
  }
}
resource "google_privateca_certificate_template" "t" {
  project     = %[1]q
  location    = "us-central1"
  name        = "tftmpl-%[2]d"
  description = "terraform servers"
  predefined_values {
    ca_options {
      is_ca = false
    }
    key_usage {
      base_key_usage {
        digital_signature = true
      }
      extended_key_usage {
        server_auth = true
      }
    }
  }
}
resource "google_privateca_certificate" "c" {
  project               = %[1]q
  location              = "us-central1"
  pool                  = google_privateca_ca_pool.sub.name
  name                  = "tfcert"
  certificate_authority = google_privateca_certificate_authority.sub.certificate_authority_id
  certificate_template  = google_privateca_certificate_template.t.id
  lifetime              = "3600s"
  config {
    subject_config {
      subject {
        common_name  = "tf.example.com"
        organization = "CloudBurrow"
      }
      subject_alt_name {
        dns_names = ["tf.example.com"]
      }
    }
    x509_config {
      ca_options {
        is_ca = false
      }
      key_usage {
        base_key_usage {}
        extended_key_usage {}
      }
    }
    public_key {
      format = "PEM"
      key    = base64encode(%[3]q)
    }
  }
}
`, m.project, n, pub+"\n"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	parent := "projects/" + m.project + "/locations/us-central1"
	sub, err := c.GetCertificateAuthority(ctx, &privatecapb.GetCertificateAuthorityRequest{Name: fmt.Sprintf("%s/caPools/tfsub-%d/certificateAuthorities/sub", parent, n)})
	if err != nil || sub.GetState() != privatecapb.CertificateAuthority_ENABLED || len(sub.GetPemCaCertificates()) != 2 {
		t.Fatalf("the subordinate Terraform made = %v, %v; want ENABLED, signed by the root", sub, err)
	}
	crt, err := c.GetCertificate(ctx, &privatecapb.GetCertificateRequest{Name: fmt.Sprintf("%s/caPools/tfsub-%d/certificates/tfcert", parent, n)})
	if err != nil || crt.GetIssuerCertificateAuthority() != sub.GetName() ||
		!crt.GetCertificateDescription().GetX509Description().GetKeyUsage().GetExtendedKeyUsage().GetServerAuth() {
		t.Fatalf("the certificate Terraform made = %v, %v; want it issued by the subordinate, server auth from the template", crt, err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := c.GetCaPool(ctx, &privatecapb.GetCaPoolRequest{Name: fmt.Sprintf("%s/caPools/tfroot-%d", parent, n)}); err == nil {
		t.Errorf("the root pool survived destroy")
	}
}

// TestGcloudPrivateCAKMSKeyBucketAndCMEK (#1834): with egress to Google
// blocked, `gcloud privateca pools create --encryption-key` makes a CMEK
// pool, and `gcloud privateca roots create --kms-key-version --bucket`
// makes a root CA in it whose key is a Cloud KMS version and which
// publishes to a Cloud Storage bucket. Before the create gcloud asks
// Resource Manager and Cloud KMS testIamPermissions, reads the bucket,
// has Service Usage v1beta1 generateServiceIdentity name the Certificate
// Authority Service agent, and grants it roles/cloudkms.signerVerifier and
// roles/viewer on the key and roles/storage.objectAdmin and
// roles/storage.legacyBucketReader on the bucket: the official clients
// find those bindings, the CA's public key is the KMS version's, its
// certificate is the bucket's object, and a certificate gcloud issues
// from the pool verifies under it.
// covers: google.cloud.security.privateca.v1.CertificateAuthorityService/CreateCaPool, google.cloud.security.privateca.v1.CertificateAuthorityService/CreateCertificateAuthority, google.cloud.security.privateca.v1.CertificateAuthorityService/EnableCertificateAuthority, google.cloud.security.privateca.v1.CertificateAuthorityService/CreateCertificate
func TestGcloudPrivateCAKMSKeyBucketAndCMEK(t *testing.T) {
	h := New(t)
	// The sql shard serves Certificate Authority Service, Cloud KMS and
	// Cloud Storage together, under its own names; the gcloud session
	// reads the shared ones.
	shardEndpoint(h, envSQLPrivateCA, EnvPrivateCA)
	shardEndpoint(h, envCMEKKMS, EnvKMS)
	shardEndpoint(h, envNoPubSubStorage, EnvStorage)
	c := privateCAClients(t, h)["grpc"]
	n := time.Now().UnixNano() % 1e6
	k, version, cmek := privateCAKMS(t, h, fmt.Sprintf("gring-%d", n), kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256)
	sc := storageClient(t, h)
	ctx := h.Context()
	bucketName := fmt.Sprintf("%s-gca-%d", h.Project(), n)
	bh := sc.Bucket(bucketName)
	if err := bh.Create(ctx, h.Project(), nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { emptyAndDelete(context.Background(), bh) })
	g := newGcloudSession(t, h)
	pool := fmt.Sprintf("gcmek-%d", n)
	loc := []string{"--location", "us-central1", "--project", g.project, "--quiet"}
	run := func(args ...string) string {
		t.Helper()
		out, err := g.run(nil, append(append([]string{"privateca"}, args...), loc...)...)
		if err != nil {
			t.Fatalf("gcloud privateca %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	parent := "projects/" + g.project + "/locations/us-central1"

	run("pools", "create", pool, "--tier", "enterprise", "--encryption-key", cmek)
	defer func() {
		_, _ = g.run(nil, append([]string{"privateca", "pools", "delete", pool, "--ignore-dependent-resources"}, loc...)...)
	}()
	p, err := c.GetCaPool(ctx, &privatecapb.GetCaPoolRequest{Name: parent + "/caPools/" + pool})
	if err != nil || p.GetEncryptionSpec().GetCloudKmsKey() != cmek {
		t.Fatalf("the pool gcloud made = %v, %v; want encryption_spec %s", p, err, cmek)
	}
	if out, err := g.run(nil, append([]string{"privateca", "pools", "create", pool + "-devops", "--tier", "devops", "--encryption-key", cmek}, loc...)...); err == nil {
		t.Errorf("a DevOps pool with --encryption-key was made: %s", out)
	}

	out := run("roots", "create", "root", "--pool", pool, "--subject", "CN=gcloud KMS Root, O=CloudBurrow",
		"--kms-key-version", version, "--bucket", bucketName, "--auto-enable")
	if !strings.Contains(out, "Created Certificate Authority") {
		t.Errorf("roots create: %s", out)
	}
	defer func() {
		_, _ = g.run(nil, append([]string{"privateca", "roots", "disable", "root", "--pool", pool}, loc...)...)
		_, _ = g.run(nil, append([]string{"privateca", "roots", "delete", "root", "--pool", pool, "--skip-grace-period", "--ignore-active-certificates"}, loc...)...)
	}()
	ca, err := c.GetCertificateAuthority(ctx, &privatecapb.GetCertificateAuthorityRequest{Name: parent + "/caPools/" + pool + "/certificateAuthorities/root"})
	if err != nil || ca.GetState() != privatecapb.CertificateAuthority_ENABLED || ca.GetKeySpec().GetCloudKmsKeyVersion() != version || ca.GetGcsBucket() != bucketName {
		t.Fatalf("the root gcloud made = %v, %v; want ENABLED, key %s, bucket %s", ca, err, version, bucketName)
	}
	pk, err := k.GetPublicKey(ctx, &kmspb.GetPublicKeyRequest{Name: version})
	if err != nil {
		t.Fatal(err)
	}
	root, err := parsePEM(ca.GetPemCaCertificates()[0])
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := pem.Decode([]byte(pk.GetPem())); want == nil || !bytes.Equal(root.RawSubjectPublicKeyInfo, want.Bytes) {
		t.Errorf("the root's public key is not the Cloud KMS version's")
	}

	// The bindings gcloud granted the service agent, as Google keeps them.
	agentRE := regexp.MustCompile(`^serviceAccount:service-[0-9]+@gcp-sa-privateca\.iam\.gserviceaccount\.com$`)
	agent := ""
	has := func(what string, bindings map[string][]string, roles ...string) {
		t.Helper()
		for _, r := range roles {
			i := slices.IndexFunc(bindings[r], agentRE.MatchString)
			if i < 0 || (agent != "" && bindings[r][i] != agent) {
				t.Errorf("%s: the Certificate Authority Service agent %s is not granted %s: %v", what, agent, r, bindings)
				continue
			}
			agent = bindings[r][i]
		}
	}
	kp, err := k.ResourceIAM(strings.Split(version, "/cryptoKeyVersions/")[0]).Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	kb := map[string][]string{}
	for _, r := range kp.Roles() {
		kb[string(r)] = kp.Members(r)
	}
	has("the signing key's policy", kb, "roles/cloudkms.signerVerifier", "roles/viewer")
	bp, err := bh.IAM().Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bb := map[string][]string{}
	for _, r := range bp.Roles() {
		bb[string(r)] = bp.Members(r)
	}
	has("the bucket's policy", bb, "roles/storage.objectAdmin", "roles/storage.legacyBucketReader")

	obj := strings.TrimPrefix(ca.GetAccessUrls().GetCaCertificateAccessUrl(), "http://")
	if _, after, ok := strings.Cut(obj, "/"+bucketName+"/"); !ok {
		t.Errorf("the CA certificate's access URL %s is not an object of %s", ca.GetAccessUrls().GetCaCertificateAccessUrl(), bucketName)
	} else if r, err := bh.Object(after).NewReader(ctx); err != nil {
		t.Errorf("the published CA certificate %s: %v", after, err)
	} else {
		b, _ := io.ReadAll(r)
		_ = r.Close()
		if strings.TrimSpace(string(b)) != strings.TrimSpace(ca.GetPemCaCertificates()[0]) {
			t.Errorf("the published CA certificate is not the CA's")
		}
	}

	dir := t.TempDir()
	certFile := filepath.Join(dir, "cert.pem")
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	csrDER, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: []string{"kms.example.com"}}, key)
	csr := filepath.Join(dir, "req.csr")
	if err := os.WriteFile(csr, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := g.run(nil, "privateca", "certificates", "create", "gkcert", "--issuer-pool", pool, "--issuer-location", "us-central1",
		"--csr", csr, "--cert-output-file", certFile, "--validity", "P1D", "--project", g.project, "--quiet"); err != nil {
		t.Fatalf("gcloud privateca certificates create: %v\n%s", err, out)
	}
	b, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := parsePEM(string(b))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "kms.example.com", KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		t.Errorf("the certificate gcloud issued in the CMEK pool does not verify under the KMS-held root: %v", err)
	}
}
