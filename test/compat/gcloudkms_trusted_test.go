//go:build compat

package compat

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	kwp "github.com/tink-crypto/tink-go/v2/kwp/subtle"
)

// TestGcloudKMSTrustedKeyWrapping (#1521): gcloud, configured by
// `cloudburrow gcloud-setup` alone, drives trusted key wrapping in a
// single-tenant HSM: `single-tenant-hsm create --key-portability-enabled`
// and a register_2fa_keys proposal; `import-jobs create --protection-level
// hsm-single-tenant --single-tenant-hsm-instance`, through which `keys
// versions import --wrapped-key-file` brings a wrapping key known on
// premises into an `aes-wrapping` (`aes-256-kwp`) key; an upgrade_key_trust
// proposal (`--crypto-key-version-name`, `--two-factor-public-key-pem`)
// approved with `--required-challenge-replies` and
// `--quorum-challenge-replies`, then executed; `keys versions
// import-trusted-key-wrapped` of an EC key wrapped with AES-KWP on premises,
// which `kms asymmetric-sign` signs with; `keys versions
// export-trusted-key-wrapped`, whose file unwraps to the key imported; and
// `keys create --hsm-trusted-wrapping`, whose first version has
// trustedWrappingEnabled.
func TestGcloudKMSTrustedKeyWrapping(t *testing.T) {
	h := New(t)
	run, project := gcloudKMS(t, h)
	ctx := h.Context()
	hc, err := kms.NewHsmManagementClient(ctx, kmsOptions(h, "grpc")...)
	if err != nil {
		t.Fatal(err)
	}
	defer hc.Close()
	c := kmsClients(t, h)["grpc"]
	gc := func(args ...string) string {
		t.Helper()
		out, err := run(args...)
		if err != nil {
			t.Fatalf("gcloud %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	const loc = "us-east4"
	id := "gc-trust-" + h.Project()
	inst := "projects/" + project + "/locations/" + loc + "/singleTenantHsmInstances/" + id
	gc("kms", "single-tenant-hsm", "create", "--location", loc, "--total-approver-count", "3", "--single-tenant-hsm-instance-id", id, "--key-portability-enabled")

	dir := t.TempDir()
	write := func(name string, b []byte) string {
		t.Helper()
		f := filepath.Join(dir, name)
		if err := os.WriteFile(f, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return f
	}
	a := []hsmApprover{newHsmApprover(t), newHsmApprover(t), newHsmApprover(t)}
	var pemFiles []string
	for i, x := range a {
		pemFiles = append(pemFiles, write(fmt.Sprintf("rsapub%d.pem", i), []byte(x.pem)))
	}
	replies := func(id string, cs []*kmspb.Challenge, by ...int) string {
		t.Helper()
		var tuples []string
		for _, i := range by {
			f := write(fmt.Sprintf("%s-signed-%d.bin", id, i), a[i].sign(t, cs).GetSignedChallenge())
			tuples = append(tuples, fmt.Sprintf("('%s','%s')", f, pemFiles[i]))
		}
		return "[" + strings.Join(tuples, ",") + "]"
	}
	proposal := func(id string) *kmspb.SingleTenantHsmInstanceProposal {
		t.Helper()
		p, err := hc.GetSingleTenantHsmInstanceProposal(ctx, &kmspb.GetSingleTenantHsmInstanceProposalRequest{Name: inst + "/proposals/" + id})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	gc("kms", "single-tenant-hsm", "proposal", "create", inst, "--operation-type", "register_2fa_keys",
		"--single-tenant-hsm-instance-proposal-id", "register", "--required-approver-count", "2",
		"--two-factor-public-key-pems", strings.Join(pemFiles, ","))
	gc("kms", "single-tenant-hsm", "proposal", "approve", inst+"/proposals/register", "--quorum-challenge-replies",
		replies("register", proposal("register").GetQuorumParameters().GetChallenges(), 0, 1, 2))
	gc("kms", "single-tenant-hsm", "proposal", "execute", inst+"/proposals/register")

	ring := "gc-trust-" + h.Project()
	at := []string{"--keyring", ring, "--location", loc}
	ringName := "projects/" + project + "/locations/" + loc + "/keyRings/" + ring
	gc("kms", "keyrings", "create", ring, "--location", loc)
	gc(append([]string{"kms", "import-jobs", "create", "j", "--import-method", "rsa-oaep-3072-sha256-aes-256",
		"--protection-level", "hsm-single-tenant", "--single-tenant-hsm-instance", inst}, at...)...)
	job, err := c.GetImportJob(ctx, &kmspb.GetImportJobRequest{Name: ringName + "/importJobs/j"})
	if err != nil || job.GetCryptoKeyBackend() != inst {
		t.Fatalf("import job = %v, %v", job, err)
	}
	b, _ := pem.Decode([]byte(job.GetPublicKey().GetPem()))
	jobPub, err := x509.ParsePKIXPublicKey(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	wrap := func(kek, key []byte) []byte {
		t.Helper()
		w, err := kwp.NewKWP(kek)
		if err != nil {
			t.Fatal(err)
		}
		out, err := w.Wrap(key)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	kek, ephemeral := make([]byte, 32), make([]byte, 32)
	_, _ = rand.Read(kek)
	_, _ = rand.Read(ephemeral)
	wrappedEphemeral, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, jobPub.(*rsa.PublicKey), ephemeral, nil)
	if err != nil {
		t.Fatal(err)
	}
	stKey := []string{"--protection-level", "hsm-single-tenant", "--crypto-key-backend", inst}
	gc(append(append([]string{"kms", "keys", "create", "wrap", "--purpose", "aes-wrapping", "--default-algorithm", "aes-256-kwp", "--skip-initial-version-creation"}, stKey...), at...)...)
	gc(append([]string{"kms", "keys", "versions", "import", "--key", "wrap", "--import-job", "j", "--algorithm", "aes-256-kwp",
		"--wrapped-key-file", write("kek.wrapped", append(wrappedEphemeral, wrap(ephemeral, kek)...))}, at...)...)
	wv := ringName + "/cryptoKeys/wrap/cryptoKeyVersions/1"

	gc("kms", "single-tenant-hsm", "proposal", "create", inst, "--operation-type", "upgrade_key_trust",
		"--single-tenant-hsm-instance-proposal-id", "upgrade", "--crypto-key-version-name", wv, "--two-factor-public-key-pem", pemFiles[2])
	rq := proposal("upgrade").GetRequiredActionQuorumParameters()
	gc("kms", "single-tenant-hsm", "proposal", "approve", inst+"/proposals/upgrade",
		"--required-challenge-replies", replies("upgrade-required", rq.GetRequiredChallenges(), 2),
		"--quorum-challenge-replies", replies("upgrade-quorum", rq.GetQuorumChallenges(), 0, 1))
	gc("kms", "single-tenant-hsm", "proposal", "execute", inst+"/proposals/upgrade")
	if v, err := c.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: wv}); err != nil || !v.GetHsmTrusted() {
		t.Fatalf("after upgrade_key_trust = %v, %v", v, err)
	}

	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(ec)
	gc(append(append([]string{"kms", "keys", "create", "sig", "--purpose", "asymmetric-signing", "--default-algorithm", "ec-sign-p256-sha256", "--skip-initial-version-creation"}, stKey...), at...)...)
	gc(append([]string{"kms", "keys", "versions", "import-trusted-key-wrapped", "--key", "sig", "--importing-key-version", wv,
		"--algorithm", "ec-sign-p256-sha256", "--wrapped-key-file", write("ec.wrapped", wrap(kek, der))}, at...)...)
	data, sigFile := write("data", []byte("signed through gcloud")), filepath.Join(dir, "sig")
	gc(append([]string{"kms", "asymmetric-sign", "--key", "sig", "--version", "1", "--digest-algorithm", "sha256", "--input-file", data, "--signature-file", sigFile}, at...)...)
	sig, err := os.ReadFile(sigFile)
	digest := sha256.Sum256([]byte("signed through gcloud"))
	if err != nil || !ecdsa.VerifyASN1(&ec.PublicKey, digest[:], sig) {
		t.Errorf("the signature does not verify with the imported key: %v", err)
	}
	out := filepath.Join(dir, "exported")
	gc(append([]string{"kms", "keys", "versions", "export-trusted-key-wrapped", "1", "--key", "sig", "--wrapping-key-version", wv, "--wrapped-key-file", out}, at...)...)
	exported, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	w, _ := kwp.NewKWP(kek)
	if got, err := w.Unwrap(exported); err != nil || !bytes.Equal(got, der) {
		t.Errorf("the exported key does not unwrap to the PKCS #8 imported: %v", err)
	}

	gc(append(append([]string{"kms", "keys", "create", "raw", "--purpose", "raw-encryption", "--default-algorithm", "aes-256-gcm", "--hsm-trusted-wrapping"}, stKey...), at...)...)
	if got := gc(append([]string{"kms", "keys", "versions", "describe", "1", "--key", "raw", "--format", "value(trustedWrappingEnabled)"}, at...)...); strings.TrimSpace(got) != "True" {
		t.Errorf("trustedWrappingEnabled = %q, want True", got)
	}
}
