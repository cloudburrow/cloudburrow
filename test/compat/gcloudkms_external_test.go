//go:build compat

package compat

import (
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"
)

// TestGcloudKMSExternalKeys (#1469): gcloud kms, configured by
// `cloudburrow gcloud-setup` alone, follows create-external-key: an
// EXTERNAL key made with --skip-initial-version-creation, a version naming
// its --external-key-uri made --primary, encrypt and decrypt with it, and
// keys versions update moving the URI; then an EXTERNAL_VPC key whose
// --crypto-key-backend is an EKM connection, with a version naming its
// --ekm-connection-key-path. The official gRPC client checks each.
func TestGcloudKMSExternalKeys(t *testing.T) {
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
	const loc = "asia-southeast1"
	ring := "gc-ext-" + h.Project()
	at := []string{"--keyring", ring, "--location", loc}
	gc("kms", "keyrings", "create", ring, "--location", loc)
	gc(append([]string{"kms", "keys", "create", "sym", "--purpose", "encryption", "--protection-level", "external",
		"--skip-initial-version-creation", "--default-algorithm", "external-symmetric-encryption"}, at...)...)
	gc(append([]string{"kms", "keys", "versions", "create", "--key", "sym", "--external-key-uri", "https://ekm.example.com/v0/keys/gcloud", "--primary"}, at...)...)
	key := "projects/" + project + "/locations/" + loc + "/keyRings/" + ring + "/cryptoKeys/sym"
	k, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key})
	if err != nil || k.GetPrimary().GetProtectionLevel() != kmspb.ProtectionLevel_EXTERNAL ||
		k.GetPrimary().GetExternalProtectionLevelOptions().GetExternalKeyUri() != "https://ekm.example.com/v0/keys/gcloud" {
		t.Fatalf("GetCryptoKey = %v, %v", k, err)
	}
	dir := t.TempDir()
	pt, ct, back := filepath.Join(dir, "p"), filepath.Join(dir, "c"), filepath.Join(dir, "b")
	if err := os.WriteFile(pt, []byte("external via gcloud"), 0o600); err != nil {
		t.Fatal(err)
	}
	gc(append([]string{"kms", "encrypt", "--key", "sym", "--plaintext-file", pt, "--ciphertext-file", ct}, at...)...)
	gc(append([]string{"kms", "keys", "versions", "update", "1", "--key", "sym", "--external-key-uri", "https://ekm2.example.com/v0/keys/gcloud"}, at...)...)
	gc(append([]string{"kms", "decrypt", "--key", "sym", "--ciphertext-file", ct, "--plaintext-file", back}, at...)...)
	if b, err := os.ReadFile(back); err != nil || string(b) != "external via gcloud" {
		t.Errorf("decrypt after the URI moved = %q, %v", b, err)
	}
	v, err := c.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: key + "/cryptoKeyVersions/1"})
	if err != nil || v.GetExternalProtectionLevelOptions().GetExternalKeyUri() != "https://ekm2.example.com/v0/keys/gcloud" {
		t.Errorf("after keys versions update = %v, %v", v, err)
	}

	// EXTERNAL_VPC, on the location's default connection.
	certFile := filepath.Join(dir, "ekm.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ekmCertificate(t)}), 0o600); err != nil {
		t.Fatal(err)
	}
	ekmID := "gc-ext-" + h.Project()
	gc("kms", "ekm-connections", "create", ekmID, "--location", loc,
		"--service-directory-service", "projects/"+project+"/locations/"+loc+"/namespaces/ekm/services/replica",
		"--hostname", "ekm.example.com", "--server-certificates-files", certFile)
	conn := "projects/" + project + "/locations/" + loc + "/ekmConnections/" + ekmID
	gc("kms", "ekm-config", "update", "--location", loc, "--default-ekm-connection", conn)
	gc(append([]string{"kms", "keys", "create", "vpc", "--purpose", "encryption", "--protection-level", "external-vpc",
		"--skip-initial-version-creation", "--default-algorithm", "external-symmetric-encryption", "--crypto-key-backend", conn}, at...)...)
	gc(append([]string{"kms", "keys", "versions", "create", "--key", "vpc", "--ekm-connection-key-path", "v0/gcloud/key", "--primary"}, at...)...)
	vk, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: strings.TrimSuffix(key, "sym") + "vpc"})
	if err != nil || vk.GetCryptoKeyBackend() != conn || vk.GetPrimary().GetExternalProtectionLevelOptions().GetEkmConnectionKeyPath() != "v0/gcloud/key" ||
		vk.GetPrimary().GetProtectionLevel() != kmspb.ProtectionLevel_EXTERNAL_VPC {
		t.Errorf("the EXTERNAL_VPC key = %v, %v", vk, err)
	}
}
