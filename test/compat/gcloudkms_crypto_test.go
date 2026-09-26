//go:build compat

package compat

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// TestGcloudKMSEncryptDecrypt (#427): data round-trips through real gcloud,
// configured by gcloud-setup, with integrity verification on (gcloud's
// default). gcloud then sends plaintextCrc32c, and additionalAuthenticatedDataCrc32c
// = crc32c("") even without AAD, and refuses a response unless both are
// verified, ciphertextCrc32c matches and the name is a version of the key;
// so this is the real-tool test of #411's rule that a supplied CRC of 0 is
// present. Encrypting with --version sends a version name, which is
// UNVERIFIED for Google (council §6).
//
// unverified: google.cloud.kms.v1.KeyManagementService/Encrypt FAILED_PRECONDITION: a DISABLED primary, a DISABLED named version, or a key with no primary
// unverified: google.cloud.kms.v1.KeyManagementService/Decrypt FAILED_PRECONDITION: a DISABLED or DESTROY_SCHEDULED version
// unverified: google.cloud.kms.v1.KeyManagementService/Decrypt INVALID_ARGUMENT: a wrong AAD, a tampered ciphertext, or another key's ciphertext
func TestGcloudKMSEncryptDecrypt(t *testing.T) {
	h := New(t)
	run, project := gcloudKMS(t, h)
	c := kmsClients(t, h)["grpc"]
	ctx := h.Context()
	ringID := "gcx-" + h.Project()
	ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + project + "/locations/global", KeyRingId: ringID})
	if err != nil {
		t.Fatal(err)
	}
	key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: key.GetName()}); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	file := func(name string, b []byte) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if b != nil {
			if err := os.WriteFile(p, b, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return p
	}
	read := func(p string) []byte {
		t.Helper()
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	where := []string{"--key", "k", "--keyring", ringID, "--location", "global"}
	gc := func(args ...string) string {
		t.Helper()
		out, err := run(append(args, where...)...)
		if err != nil {
			t.Fatalf("gcloud %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	plaintext := []byte("GCLOUD-PLAINTEXT-MARKER-7c1a")
	p := file("p", plaintext)
	aad := file("aad", []byte("gcloud-aad"))
	wrong := file("wrong-aad", []byte("not-the-aad"))

	// Without AAD, then with it.
	gc("kms", "encrypt", "--plaintext-file", p, "--ciphertext-file", file("c", nil))
	gc("kms", "decrypt", "--ciphertext-file", file("c", nil), "--plaintext-file", file("out", nil))
	if got := read(file("out", nil)); !bytes.Equal(got, plaintext) {
		t.Errorf("round trip without AAD = %q", got)
	}
	gc("kms", "encrypt", "--plaintext-file", p, "--ciphertext-file", file("ca", nil), "--additional-authenticated-data-file", aad)
	gc("kms", "decrypt", "--ciphertext-file", file("ca", nil), "--plaintext-file", file("outa", nil), "--additional-authenticated-data-file", aad)
	if got := read(file("outa", nil)); !bytes.Equal(got, plaintext) {
		t.Errorf("round trip with AAD = %q", got)
	}
	out, err := run(append([]string{"kms", "decrypt", "--ciphertext-file", file("ca", nil), "--plaintext-file", "-",
		"--additional-authenticated-data-file", wrong}, where...)...)
	if err == nil || strings.Contains(out, string(plaintext)) {
		t.Errorf("decrypt with the wrong AAD = %v:\n%s; want a failure without the plaintext", err, out)
	}

	// Across clients.
	dec, err := c.Decrypt(ctx, &kmspb.DecryptRequest{Name: key.GetName(), Ciphertext: read(file("c", nil))})
	if err != nil || !bytes.Equal(dec.GetPlaintext(), plaintext) {
		t.Errorf("gRPC decrypt of gcloud's ciphertext = %q, %v", dec.GetPlaintext(), err)
	}
	enc, err := c.Encrypt(ctx, &kmspb.EncryptRequest{Name: key.GetName(), Plaintext: plaintext})
	if err != nil {
		t.Fatal(err)
	}
	gc("kms", "decrypt", "--ciphertext-file", file("cg", enc.GetCiphertext()), "--plaintext-file", file("outg", nil))
	if got := read(file("outg", nil)); !bytes.Equal(got, plaintext) {
		t.Errorf("gcloud decrypt of gRPC's ciphertext = %q", got)
	}

	// --version 2, a version that is not the primary: its ciphertext still
	// decrypts once the primary, version 1, is disabled.
	gc("kms", "encrypt", "--version", "2", "--plaintext-file", p, "--ciphertext-file", file("c2", nil))
	if _, err := c.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{
		CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: key.GetName() + "/cryptoKeyVersions/1", State: kmspb.CryptoKeyVersion_DISABLED},
		UpdateMask:       &fieldmaskpb.FieldMask{Paths: []string{"state"}}}); err != nil {
		t.Fatal(err)
	}
	gc("kms", "decrypt", "--ciphertext-file", file("c2", nil), "--plaintext-file", file("out2", nil))
	if got := read(file("out2", nil)); !bytes.Equal(got, plaintext) {
		t.Errorf("version 2's ciphertext after disabling version 1 = %q", got)
	}
	// And once version 2 is disabled too, it does not.
	if out, err := run("kms", "keys", "versions", "disable", "2", "--key", "k", "--keyring", ringID, "--location", "global"); err != nil {
		t.Fatalf("versions disable 2: %v\n%s", err, out)
	}
	if out, err := run(append([]string{"kms", "decrypt", "--ciphertext-file", file("c2", nil), "--plaintext-file", "-"}, where...)...); err == nil ||
		strings.Contains(out, string(plaintext)) {
		t.Errorf("decrypt with the version disabled = %v:\n%s; want a failure", err, out)
	}
}
