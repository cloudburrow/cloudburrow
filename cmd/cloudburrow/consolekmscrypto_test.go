package main

import (
	"context"
	"crypto/mlkem"
	"encoding/base64"
	"slices"
	"strings"
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
	for _, want := range []string{"EC_SIGN_P256_SHA256", "RSA_SIGN_PSS_2048_SHA256", "RSA_DECRYPT_OAEP_2048_SHA256", "HMAC_SHA256", "AES_256_GCM", "ML_KEM_768", "EC_SIGN_SECP256K1_SHA256",
		"PQ_SIGN_ML_DSA_65", "PQ_SIGN_ML_DSA_44_EXTERNAL_MU", "PQ_SIGN_SLH_DSA_SHA2_128S", "PQ_SIGN_HASH_SLH_DSA_SHA2_128S_SHA256", "KEM_XWING"} {
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

// secp256k1 (#1719) is created from the console at HSM, refused at
// SOFTWARE as Google refuses it, and its version signs and verifies.
func TestConsoleKMSSecp256k1(t *testing.T) {
	p, c, ring := kmsConsole(t)
	ctx := context.Background()
	if _, err := p.ActAtResult(ctx, "demo-proj", []string{ring}, "createkey",
		map[string]string{"cryptoKeyId": "sw", "purpose": "ASYMMETRIC_SIGN", "algorithm": "EC_SIGN_SECP256K1_SHA256", "protectionLevel": "SOFTWARE"}); err == nil {
		t.Error("a SOFTWARE secp256k1 key was created")
	}
	if _, err := p.ActAtResult(ctx, "demo-proj", []string{ring}, "createkey",
		map[string]string{"cryptoKeyId": "k1", "purpose": "ASYMMETRIC_SIGN", "algorithm": "EC_SIGN_SECP256K1_SHA256", "protectionLevel": "HSM"}); err != nil {
		t.Fatal(err)
	}
	k, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: ring + "/cryptoKeys/k1"})
	if err != nil || k.GetVersionTemplate().GetAlgorithm() != kmspb.CryptoKeyVersion_EC_SIGN_SECP256K1_SHA256 || k.GetVersionTemplate().GetProtectionLevel() != kmspb.ProtectionLevel_HSM {
		t.Fatalf("GetCryptoKey = %v, %v", k, err)
	}
	v := []string{ring, "k1", "1"}
	sig := kmsActResult(t, p, v, "sign", map[string]string{"data": "hello"})["Signature"]
	if got := kmsActResult(t, p, v, "verify", map[string]string{"data": "hello", "signature": sig})["Signature"]; got != "Valid" {
		t.Errorf("verify = %s", got)
	}
	if got := kmsActResult(t, p, v, "verify", map[string]string{"data": "other", "signature": sig})["Signature"]; got != "Invalid" {
		t.Errorf("verify of other data = %s", got)
	}
	pk, err := c.GetPublicKey(ctx, &kmspb.GetPublicKeyRequest{Name: ring + "/cryptoKeys/k1/cryptoKeyVersions/1"})
	if err != nil || kmsActResult(t, p, v, "publickey", map[string]string{"format": "PEM"})["Public key (PEM)"] != pk.GetPem() {
		t.Errorf("Get public key is not the SDK's PEM (%v)", err)
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

	// The post-quantum algorithms (#1946): Sign, then Verify against the
	// NIST_PQC key; X-Wing's public key in XWING_RAW_BYTES.
	// ML-DSA only where the build has crypto/mldsa (Go 1.27).
	for _, a := range append(slices.Clone(consoleMLDSAAlgorithms), "PQ_SIGN_SLH_DSA_SHA2_128S", "PQ_SIGN_HASH_SLH_DSA_SHA2_128S_SHA256") {
		v := mk(strings.ToLower(strings.ReplaceAll(a, "_", "-")), "ASYMMETRIC_SIGN", a)
		sig := kmsActResult(t, p, v, "sign", map[string]string{"data": "pq"})["Signature"]
		if got := kmsActResult(t, p, v, "verify", map[string]string{"data": "pq", "signature": sig})["Signature"]; got != "Valid" {
			t.Errorf("%s verify = %s", a, got)
		}
		if got := kmsActResult(t, p, v, "verify", map[string]string{"data": "other", "signature": sig})["Signature"]; got != "Invalid" {
			t.Errorf("%s verify of other data = %s", a, got)
		}
	}
	xw := mk("xwing", "KEY_ENCAPSULATION", "KEM_XWING")
	if got, _ := base64.StdEncoding.DecodeString(kmsActResult(t, p, xw, "publickey", map[string]string{"format": "XWING_RAW_BYTES"})["Public key (XWING_RAW_BYTES, base64)"]); len(got) != 1216 {
		t.Errorf("X-Wing public key = %d bytes, want 1216", len(got))
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

// Delete on a key ring's page is offered only while the ring has no keys,
// and deletes it through DeleteKeyRing (#1595).
func TestConsoleKMSDeleteKeyRing(t *testing.T) {
	p, c, ring := kmsConsole(t)
	ctx := context.Background()
	if got := kmsActionIDs(p, []string{ring}); !slices.Contains(got, "deletering") {
		t.Fatalf("an empty key ring does not offer Delete: %v", got)
	}
	if _, err := p.ActAtResult(ctx, "demo-proj", []string{ring}, "createkey", map[string]string{"cryptoKeyId": "k"}); err != nil {
		t.Fatal(err)
	}
	if got := kmsActionIDs(p, []string{ring}); slices.Contains(got, "deletering") {
		t.Errorf("a key ring with a key offers Delete: %v", got)
	}
	if err := p.ActAt(ctx, "demo-proj", []string{ring}, "deletering", nil); err == nil {
		t.Error("Delete of a key ring with a key succeeded")
	}
	other, err := p.Create(ctx, "demo-proj", map[string]string{"keyRingId": "empty", "location": "global"})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, "demo-proj", []string{other}, "deletering", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetKeyRing(ctx, &kmspb.GetKeyRingRequest{Name: "projects/demo-proj/locations/global/keyRings/empty"}); err == nil {
		t.Error("the SDK still reads the deleted key ring")
	}
}

// Create key's "Restrict key versions to import only" (#1946) makes an
// import_only key with no version, which the official client reads back.
func TestConsoleKMSCreateImportOnlyKey(t *testing.T) {
	p, c, ring := kmsConsole(t)
	if _, err := p.ActAtResult(context.Background(), "demo-proj", []string{ring}, "createkey",
		map[string]string{"cryptoKeyId": "io", "keyMaterial": kmsMaterialImported, "importOnly": "true"}); err != nil {
		t.Fatal(err)
	}
	k, err := c.GetCryptoKey(context.Background(), &kmspb.GetCryptoKeyRequest{Name: ring + "/cryptoKeys/io"})
	if err != nil || !k.GetImportOnly() || k.GetPrimary() != nil {
		t.Errorf("GetCryptoKey = %v, %v; want import_only with no version", k, err)
	}
}
