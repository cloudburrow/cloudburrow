package main

import (
	"context"
	"crypto/mlkem"
	"encoding/base64"
	"slices"
	"testing"

	kmsapi "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

func kmsConsole(t *testing.T) (kmsProvider, *kmsapi.KeyManagementClient, string) {
	t.Helper()
	ctx := context.Background()
	svc := &kmsService{cfg: config.Config{BindAddress: "127.0.0.1"}, db: store.NewMemory()}
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Stop(context.Background()) })
	p := kmsProvider{svc: svc}
	ring, err := p.Create(ctx, "demo-proj", map[string]string{"keyRingId": "crypto", "location": "global"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := kmsapi.NewKeyManagementClient(ctx, clientOpts(svc.Addr())...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return p, c, ring
}

func kmsActionIDs(p kmsProvider, path []string) []string {
	var out []string
	for _, a := range p.DetailActions(context.Background(), "demo-proj", path) {
		out = append(out, a.ID)
	}
	return out
}

func kmsActResult(t *testing.T, p kmsProvider, path []string, action string, values map[string]string) map[string]string {
	t.Helper()
	l, err := p.ActAtResult(context.Background(), "demo-proj", path, action, values)
	if err != nil {
		t.Fatalf("%s: %v", action, err)
	}
	if l == nil || len(l.Items) != 1 {
		t.Fatalf("%s answered %v", action, l)
	}
	return l.Items[0].Fields
}

// Create key offers every implemented purpose and algorithm, and a key made
// with them is what the official client reads (#1212).
func TestConsoleKMSCreateKeyOffersEveryPurpose(t *testing.T) {
	p, c, ring := kmsConsole(t)
	var purpose, alg []string
	for _, f := range kmsCreateKeyFields() {
		switch f.Name {
		case "purpose":
			purpose = f.Options
		case "algorithm":
			alg = f.Options
		}
	}
	for _, want := range []string{"ASYMMETRIC_SIGN", "ASYMMETRIC_DECRYPT", "MAC", "RAW_ENCRYPT_DECRYPT", "KEY_ENCAPSULATION"} {
		if !slices.Contains(purpose, want) {
			t.Errorf("purpose options %v lack %s", purpose, want)
		}
	}
	for _, want := range []string{"EC_SIGN_P256_SHA256", "RSA_SIGN_PSS_2048_SHA256", "RSA_DECRYPT_OAEP_2048_SHA256", "HMAC_SHA256", "AES_256_GCM", "ML_KEM_768"} {
		if !slices.Contains(alg, want) {
			t.Errorf("algorithm options lack %s", want)
		}
	}
	if _, err := p.ActAtResult(context.Background(), "demo-proj", []string{ring}, "createkey",
		map[string]string{"cryptoKeyId": "sign", "purpose": "ASYMMETRIC_SIGN", "algorithm": "EC_SIGN_P256_SHA256"}); err != nil {
		t.Fatal(err)
	}
	k, err := c.GetCryptoKey(context.Background(), &kmspb.GetCryptoKeyRequest{Name: ring + "/cryptoKeys/sign"})
	if err != nil || k.GetPurpose() != kmspb.CryptoKey_ASYMMETRIC_SIGN || k.GetVersionTemplate().GetAlgorithm() != kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256 {
		t.Fatalf("GetCryptoKey = %v, %v", k, err)
	}
	if _, err := p.ActAtResult(context.Background(), "demo-proj", []string{ring}, "createkey",
		map[string]string{"cryptoKeyId": "noalg", "purpose": "MAC"}); err == nil {
		t.Error("a MAC key with no algorithm was created")
	}
}

// A version's page offers its purpose's operations, and each answers what
// the official client and standard crypto agree with (#1212).
func TestConsoleKMSVersionCryptoActions(t *testing.T) {
	p, c, ring := kmsConsole(t)
	ctx := context.Background()
	mk := func(id, purpose, alg string) []string {
		if _, err := p.ActAtResult(ctx, "demo-proj", []string{ring}, "createkey",
			map[string]string{"cryptoKeyId": id, "purpose": purpose, "algorithm": alg}); err != nil {
			t.Fatal(err)
		}
		return []string{ring, id, "1"}
	}

	sign := mk("sign", "ASYMMETRIC_SIGN", "RSA_SIGN_PSS_2048_SHA256")
	if got := kmsActionIDs(p, sign); !slices.Contains(got, "sign") || !slices.Contains(got, "verify") || !slices.Contains(got, "publickey") || slices.Contains(got, "makeprimary") {
		t.Errorf("a signing version offers %v", got)
	}
	if got := kmsActionIDs(p, sign[:2]); slices.Contains(got, "encrypt") {
		t.Errorf("a signing key offers Encrypt: %v", got)
	}
	sig := kmsActResult(t, p, sign, "sign", map[string]string{"data": "hello"})["Signature"]
	if got := kmsActResult(t, p, sign, "verify", map[string]string{"data": "hello", "signature": sig})["Signature"]; got != "Valid" {
		t.Errorf("verify = %s", got)
	}
	if got := kmsActResult(t, p, sign, "verify", map[string]string{"data": "other", "signature": sig})["Signature"]; got != "Invalid" {
		t.Errorf("verify of other data = %s", got)
	}
	pk, err := c.GetPublicKey(ctx, &kmspb.GetPublicKeyRequest{Name: ring + "/cryptoKeys/sign/cryptoKeyVersions/1"})
	if err != nil || kmsActResult(t, p, sign, "publickey", map[string]string{"format": "PEM"})["Public key (PEM)"] != pk.GetPem() {
		t.Errorf("Get public key is not the SDK's PEM (%v)", err)
	}

	dec := mk("dec", "ASYMMETRIC_DECRYPT", "RSA_DECRYPT_OAEP_2048_SHA256")
	ct := kmsActResult(t, p, dec, "pubencrypt", map[string]string{"plaintext": "secret"})["Ciphertext"]
	if got := kmsActResult(t, p, dec, "asymdecrypt", map[string]string{"ciphertext": ct})["Plaintext"]; got != "secret" {
		t.Errorf("decrypt = %q", got)
	}

	mac := mk("mac", "MAC", "HMAC_SHA256")
	m := kmsActResult(t, p, mac, "macsign", map[string]string{"data": "d"})["MAC"]
	if got := kmsActResult(t, p, mac, "macverify", map[string]string{"data": "d", "mac": m})["MAC"]; got != "Valid" {
		t.Errorf("mac verify = %s", got)
	}

	raw := mk("raw", "RAW_ENCRYPT_DECRYPT", "AES_256_GCM")
	enc := kmsActResult(t, p, raw, "rawencrypt", map[string]string{"plaintext": "raw text", "aad": "ctx"})
	if got := kmsActResult(t, p, raw, "rawdecrypt", map[string]string{"ciphertext": enc["Ciphertext"], "iv": enc["Initialization vector"], "aad": "ctx"})["Plaintext"]; got != "raw text" {
		t.Errorf("raw decrypt = %q", got)
	}

	kem := mk("kem", "KEY_ENCAPSULATION", "ML_KEM_768")
	ekb, _ := base64.StdEncoding.DecodeString(kmsActResult(t, p, kem, "publickey", map[string]string{"format": "NIST_PQC"})["Public key (NIST_PQC, base64)"])
	ek, err := mlkem.NewEncapsulationKey768(ekb)
	if err != nil {
		t.Fatal(err)
	}
	secret, kct := ek.Encapsulate()
	if got := kmsActResult(t, p, kem, "decapsulate", map[string]string{"ciphertext": base64.StdEncoding.EncodeToString(kct)})["Shared secret"]; got != base64.StdEncoding.EncodeToString(secret) {
		t.Error("decapsulate did not recover the secret")
	}

	if got := kmsActResult(t, p, []string{ring}, "randombytes", map[string]string{"length": "16"})["Random bytes"]; len(got) != 24 {
		t.Errorf("16 random bytes = %q", got)
	}
}

// Delete key is offered only once the key has no versions, and Delete version
// only for a DESTROYED version, as DeleteCryptoKey and DeleteCryptoKeyVersion
// require (#1212).
func TestConsoleKMSDeleteIsOfferedWhereAllowed(t *testing.T) {
	p, c, ring := kmsConsole(t)
	ctx := context.Background()
	if _, err := p.ActAtResult(ctx, "demo-proj", []string{ring}, "createkey", map[string]string{"cryptoKeyId": "k"}); err != nil {
		t.Fatal(err)
	}
	if got := kmsActionIDs(p, []string{ring, "k"}); slices.Contains(got, "deletekey") {
		t.Errorf("a key with a version offers Delete key: %v", got)
	}
	if got := kmsActionIDs(p, []string{ring, "k", "1"}); slices.Contains(got, "deleteversion") {
		t.Errorf("an ENABLED version offers Delete version: %v", got)
	}
	if _, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring, CryptoKeyId: "empty", SkipInitialVersionCreation: true,
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}}); err != nil {
		t.Fatal(err)
	}
	if got := kmsActionIDs(p, []string{ring, "empty"}); !slices.Contains(got, "deletekey") {
		t.Fatalf("a key with no versions does not offer Delete key: %v", got)
	}
	if err := p.ActAt(ctx, "demo-proj", []string{ring, "empty"}, "deletekey", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: ring + "/cryptoKeys/empty"}); err == nil {
		t.Error("the SDK still reads the deleted key")
	}
	if r, err := c.ListRetiredResources(ctx, &kmspb.ListRetiredResourcesRequest{Parent: "projects/demo-proj/locations/global"}).Next(); err != nil || r.GetOriginalResource() != ring+"/cryptoKeys/empty" {
		t.Errorf("ListRetiredResources = %v, %v", r, err)
	}
}
