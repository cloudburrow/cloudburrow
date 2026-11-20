//go:build compat

package compat

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"github.com/cloudflare/circl/kem/xwing"
	tinkmldsa "github.com/tink-crypto/tink-go/v2/signature/mldsa"
	tinkslh "github.com/tink-crypto/tink-go/v2/signature/slhdsa"
)

// TestGcloudKMSPostQuantum (#1946): gcloud, configured by gcloud-setup
// alone, creates a key of every post-quantum algorithm with `keys create
// --default-algorithm`; `keys versions get-public-key --public-key-format
// nist-pqc` writes the FIPS 204/205 key, with which Tink verifies what
// `asymmetric-sign` signed (ML-DSA-65 and SLH-DSA); `--public-key-format
// xwing-raw-bytes` writes the X-Wing key CIRCL encapsulates under, and
// `kms decapsulate` writes the same secret; `keys create --import-only
// --skip-initial-version-creation` makes an import-only key; and `import-jobs
// add-iam-policy-binding` and `get-iam-policy` keep an import job's policy.
func TestGcloudKMSPostQuantum(t *testing.T) {
	h := New(t)
	run, project := gcloudKMS(t, h)
	c := kmsClients(t, h)["grpc"]
	ctx := h.Context()
	gc := func(args ...string) string {
		t.Helper()
		out, err := run(args...)
		if err != nil {
			t.Fatalf("gcloud %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	ringID := "gc-pq-" + strings.ToLower(time.Now().Format("150405.000"))
	ringID = strings.ReplaceAll(ringID, ".", "-")
	ring := "projects/" + project + "/locations/global/keyRings/" + ringID
	loc := []string{"--location", "global"}
	kr := append([]string{"--keyring", ringID}, loc...)
	gc(append([]string{"kms", "keyrings", "create", ringID}, loc...)...)
	dir := t.TempDir()
	file := func(name string) string { return filepath.Join(dir, name) }
	read := func(name string) []byte {
		t.Helper()
		b, err := os.ReadFile(file(name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	signing := []string{"pq-sign-ml-dsa-44", "pq-sign-ml-dsa-65", "pq-sign-ml-dsa-87", "pq-sign-ml-dsa-44-external-mu",
		"pq-sign-ml-dsa-65-external-mu", "pq-sign-ml-dsa-87-external-mu", "pq-sign-slh-dsa-sha2-128s", "pq-sign-hash-slh-dsa-sha2-128s-sha256"}
	for _, alg := range signing {
		gc(append([]string{"kms", "keys", "create", alg, "--purpose", "asymmetric-signing", "--default-algorithm", alg}, kr...)...)
		k, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: ring + "/cryptoKeys/" + alg})
		want := strings.ToUpper(strings.ReplaceAll(alg, "-", "_"))
		if err != nil || k.GetVersionTemplate().GetAlgorithm().String() != want {
			t.Errorf("keys create --default-algorithm %s = %v, %v", alg, k.GetVersionTemplate().GetAlgorithm(), err)
		}
	}
	data := []byte("signed through gcloud")
	if err := os.WriteFile(file("data"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	signed := func(key string) (pub, sig []byte) {
		t.Helper()
		gc(append([]string{"kms", "keys", "versions", "get-public-key", "1", "--key", key, "--public-key-format", "nist-pqc",
			"--output-file", file(key + ".pub")}, kr...)...)
		gc(append([]string{"kms", "asymmetric-sign", "--version", "1", "--key", key, "--input-file", file("data"),
			"--signature-file", file(key + ".sig")}, kr...)...)
		return read(key + ".pub"), read(key + ".sig")
	}
	pub, sig := signed("pq-sign-ml-dsa-65")
	mp, _ := tinkmldsa.NewParameters(tinkmldsa.MLDSA65, tinkmldsa.VariantNoPrefix)
	mk, err := tinkmldsa.NewPublicKey(pub, 0, mp)
	if err != nil {
		t.Fatalf("Tink refuses the nist-pqc ML-DSA-65 key: %v", err)
	}
	if err := tinkVerify(t, mk, sig, data); err != nil {
		t.Errorf("Tink does not verify gcloud's ML-DSA-65 signature: %v", err)
	}
	pub, sig = signed("pq-sign-slh-dsa-sha2-128s")
	sp, _ := tinkslh.NewParameters(tinkslh.SHA2, 64, tinkslh.SmallSignature, tinkslh.VariantNoPrefix)
	sk, err := tinkslh.NewPublicKey(pub, 0, sp)
	if err != nil {
		t.Fatalf("Tink refuses the nist-pqc SLH-DSA key: %v", err)
	}
	if err := tinkVerify(t, sk, sig, data); err != nil {
		t.Errorf("Tink does not verify gcloud's SLH-DSA signature: %v", err)
	}

	gc(append([]string{"kms", "keys", "create", "xw", "--purpose", "key-encapsulation", "--default-algorithm", "kem-xwing"}, kr...)...)
	gc(append([]string{"kms", "keys", "versions", "get-public-key", "1", "--key", "xw", "--public-key-format", "xwing-raw-bytes",
		"--output-file", file("xw.pub")}, kr...)...)
	xpk, err := xwing.Scheme().UnmarshalBinaryPublicKey(read("xw.pub"))
	if err != nil {
		t.Fatalf("CIRCL refuses gcloud's xwing-raw-bytes key: %v", err)
	}
	ct, ss, err := xwing.Scheme().Encapsulate(xpk)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file("ct"), ct, 0o600); err != nil {
		t.Fatal(err)
	}
	gc(append([]string{"kms", "decapsulate", "--version", "1", "--key", "xw", "--ciphertext-file", file("ct"),
		"--shared-secret-file", file("ss")}, kr...)...)
	if got := read("ss"); !bytes.Equal(got, ss) {
		t.Errorf("kms decapsulate wrote %x; want %x", got, ss)
	}

	gc(append([]string{"kms", "keys", "create", "io", "--purpose", "encryption", "--import-only", "--skip-initial-version-creation"}, kr...)...)
	if k, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: ring + "/cryptoKeys/io"}); err != nil || !k.GetImportOnly() || k.GetPrimary() != nil {
		t.Errorf("keys create --import-only = %v, %v", k, err)
	}

	const importer = "user:importer@example.com"
	gc(append([]string{"kms", "import-jobs", "create", "job", "--import-method", "rsa-oaep-3072-sha256", "--protection-level", "software"}, kr...)...)
	gc(append([]string{"kms", "import-jobs", "add-iam-policy-binding", "job", "--member", importer, "--role", "roles/cloudkms.importer"}, kr...)...)
	p, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: ring + "/importJobs/job"})
	if err != nil || len(p.GetBindings()) != 1 || p.GetBindings()[0].GetRole() != "roles/cloudkms.importer" || p.GetBindings()[0].GetMembers()[0] != importer {
		t.Errorf("the import job's policy = %v, %v", p, err)
	}
	if out := gc(append([]string{"kms", "import-jobs", "get-iam-policy", "job", "--format=value(bindings[0].role)"}, kr...)...); strings.TrimSpace(out) != "roles/cloudkms.importer" {
		t.Errorf("import-jobs get-iam-policy = %q", out)
	}
}
