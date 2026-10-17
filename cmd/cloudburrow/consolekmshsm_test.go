package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"strings"
	"testing"

	kmsapi "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
)

// The single-tenant HSM screen (#1469): Create instance, Create proposal
// registering three keys, Approve with each key's signed challenge, Execute;
// the official client reads the instance ACTIVE, and Create key makes an
// HSM_SINGLE_TENANT key in it. An unsigned approval is refused.
func TestConsoleKMSSingleTenantHsm(t *testing.T) {
	ctx := context.Background()
	kp, c := kmsEditFixture(t)
	p := kmsHsmProvider{svc: kp.svc}
	hc, err := kmsapi.NewHsmManagementClient(ctx, clientOpts(kp.svc.Addr())...)
	if err != nil {
		t.Fatal(err)
	}
	defer hc.Close()
	const project = "hsm-console"
	inst, err := p.Create(ctx, project, map[string]string{"singleTenantHsmInstanceId": "i", "location": "us-central1", "totalApproverCount": "3"})
	if err != nil {
		t.Fatal(err)
	}
	if l, err := p.List(ctx, project); err != nil || len(l.Items) != 1 || l.Items[0].Fields["State"] != "PENDING_TWO_FACTOR_AUTH_REGISTRATION" {
		t.Fatalf("List = %+v, %v", l, err)
	}
	var keys []*rsa.PrivateKey
	var pems []string
	for i := 0; i < 3; i++ {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		der, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
		keys, pems = append(keys, k), append(pems, string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})))
	}
	if err := p.ActAt(ctx, project, []string{inst}, "propose", map[string]string{"proposal": "reg", "operationType": "register_2fa_keys",
		"requiredApproverCount": "2", "twoFactorPublicKeyPems": strings.Join(pems, "")}); err != nil {
		t.Fatal(err)
	}
	prop, err := hc.GetSingleTenantHsmInstanceProposal(ctx, &kmspb.GetSingleTenantHsmInstanceProposalRequest{Name: inst + "/proposals/reg"})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, project, []string{inst}, "approve", map[string]string{"proposal": "reg", "publicKeyPem": pems[0],
		"signedChallenge": base64.StdEncoding.EncodeToString([]byte("not a signature"))}); err == nil {
		t.Error("an unsigned approval was accepted")
	}
	for _, ch := range prop.GetQuorumParameters().GetChallenges() {
		for i, pm := range pems {
			if ch.GetPublicKeyPem() != pm {
				continue
			}
			d := sha256.Sum256(ch.GetChallenge())
			sig, err := rsa.SignPKCS1v15(rand.Reader, keys[i], crypto.SHA256, d[:])
			if err != nil {
				t.Fatal(err)
			}
			if err := p.ActAt(ctx, project, []string{inst}, "approve", map[string]string{"proposal": "reg", "publicKeyPem": pm,
				"signedChallenge": base64.StdEncoding.EncodeToString(sig)}); err != nil {
				t.Fatalf("approve: %v", err)
			}
		}
	}
	if err := p.ActAt(ctx, project, []string{inst}, "execute", map[string]string{"proposal": "reg"}); err != nil {
		t.Fatal(err)
	}
	got, err := hc.GetSingleTenantHsmInstance(ctx, &kmspb.GetSingleTenantHsmInstanceRequest{Name: inst})
	if err != nil || got.GetState() != kmspb.SingleTenantHsmInstance_ACTIVE {
		t.Fatalf("instance = %v, %v", got, err)
	}
	d, err := p.Detail(ctx, project, []string{inst})
	if err != nil || d.Unavailable != "" || len(d.Sections) < 2 || len(d.Sections[1].Listing.Items) != 1 {
		t.Fatalf("Detail = %+v, %v", d, err)
	}
	ring, err := kp.Create(ctx, project, map[string]string{"keyRingId": "r", "location": "us-central1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kp.ActAtResult(ctx, project, []string{ring}, "createkey", map[string]string{"cryptoKeyId": "k", "protectionLevel": "HSM_SINGLE_TENANT", "cryptoKeyBackend": inst}); err != nil {
		t.Fatalf("create key: %v", err)
	}
	k, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: ring + "/cryptoKeys/k"})
	if err != nil || k.GetCryptoKeyBackend() != inst || k.GetPrimary().GetProtectionLevel() != kmspb.ProtectionLevel_HSM_SINGLE_TENANT {
		t.Fatalf("GetCryptoKey = %v, %v", k, err)
	}
}

// consoleSign signs the challenge cs has for pm with k, base64 for the form.
func consoleSign(t *testing.T, k *rsa.PrivateKey, pm string, cs []*kmspb.Challenge) string {
	t.Helper()
	for _, ch := range cs {
		if ch.GetPublicKeyPem() == pm {
			d := sha256.Sum256(ch.GetChallenge())
			sig, err := rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, d[:])
			if err != nil {
				t.Fatal(err)
			}
			return base64.StdEncoding.EncodeToString(sig)
		}
	}
	t.Fatal("no challenge for the key")
	return ""
}

// Trusted key wrapping from the console (#1521): an instance with key
// portability; Create key makes an AES_WRAPPING key and a RAW_ENCRYPT_DECRYPT
// key with Trusted wrapping; an upgrade_key_trust proposal, approved with
// the required and quorum challenges, makes the wrapping version
// hsm_trusted; Export trusted-wrapped on the version's page and Import
// trusted-wrapped version on another key's page carry the key across, and
// the official client decrypts with the copy what the original encrypted.
// Create import job offers HSM_SINGLE_TENANT with the instance.
func TestConsoleKMSTrustedKeyWrapping(t *testing.T) {
	ctx := context.Background()
	kp, c := kmsEditFixture(t)
	p := kmsHsmProvider{svc: kp.svc}
	hc, err := kmsapi.NewHsmManagementClient(ctx, clientOpts(kp.svc.Addr())...)
	if err != nil {
		t.Fatal(err)
	}
	defer hc.Close()
	const project = "hsm-trust"
	inst, err := p.Create(ctx, project, map[string]string{"singleTenantHsmInstanceId": "i", "location": "us-central1", "totalApproverCount": "3", "keyPortabilityEnabled": "true"})
	if err != nil {
		t.Fatal(err)
	}
	var keys []*rsa.PrivateKey
	var pems []string
	for i := 0; i < 3; i++ {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		der, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
		keys, pems = append(keys, k), append(pems, string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})))
	}
	act := func(id string, values map[string]string) {
		t.Helper()
		if err := p.ActAt(ctx, project, []string{inst}, id, values); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	act("propose", map[string]string{"proposal": "reg", "operationType": "register_2fa_keys", "requiredApproverCount": "2", "twoFactorPublicKeyPems": strings.Join(pems, "")})
	reg, err := hc.GetSingleTenantHsmInstanceProposal(ctx, &kmspb.GetSingleTenantHsmInstanceProposalRequest{Name: inst + "/proposals/reg"})
	if err != nil {
		t.Fatal(err)
	}
	for i := range keys {
		act("approve", map[string]string{"proposal": "reg", "publicKeyPem": pems[i], "signedChallenge": consoleSign(t, keys[i], pems[i], reg.GetQuorumParameters().GetChallenges())})
	}
	act("execute", map[string]string{"proposal": "reg"})

	ring, err := kp.Create(ctx, project, map[string]string{"keyRingId": "r", "location": "us-central1"})
	if err != nil {
		t.Fatal(err)
	}
	createKey := func(values map[string]string) {
		t.Helper()
		values["protectionLevel"], values["cryptoKeyBackend"] = "HSM_SINGLE_TENANT", inst
		if _, err := kp.ActAtResult(ctx, project, []string{ring}, "createkey", values); err != nil {
			t.Fatalf("create key %s: %v", values["cryptoKeyId"], err)
		}
	}
	createKey(map[string]string{"cryptoKeyId": "wrap", "purpose": "AES_WRAPPING", "algorithm": "AES_256_KWP"})
	createKey(map[string]string{"cryptoKeyId": "raw", "purpose": "RAW_ENCRYPT_DECRYPT", "algorithm": "AES_256_GCM", "trustedWrappingEnabled": "true"})
	createKey(map[string]string{"cryptoKeyId": "copy", "purpose": "RAW_ENCRYPT_DECRYPT", "algorithm": "AES_256_GCM", "skipInitialVersionCreation": "true"})
	wv := ring + "/cryptoKeys/wrap/cryptoKeyVersions/1"

	act("propose", map[string]string{"proposal": "up", "operationType": "upgrade_key_trust", "cryptoKeyVersion": wv, "twoFactorPublicKeyPems": pems[2]})
	up, err := hc.GetSingleTenantHsmInstanceProposal(ctx, &kmspb.GetSingleTenantHsmInstanceProposalRequest{Name: inst + "/proposals/up"})
	if err != nil {
		t.Fatal(err)
	}
	rq := up.GetRequiredActionQuorumParameters()
	act("approve", map[string]string{"proposal": "up", "kind": "required", "publicKeyPem": pems[2], "signedChallenge": consoleSign(t, keys[2], pems[2], rq.GetRequiredChallenges())})
	for i := 0; i < 2; i++ {
		act("approve", map[string]string{"proposal": "up", "kind": "quorum", "publicKeyPem": pems[i], "signedChallenge": consoleSign(t, keys[i], pems[i], rq.GetQuorumChallenges())})
	}
	act("execute", map[string]string{"proposal": "up"})
	if v, err := c.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: wv}); err != nil || !v.GetHsmTrusted() {
		t.Fatalf("wrapping version = %v, %v", v, err)
	}

	has := func(path []string, id string) bool {
		for _, a := range kp.DetailActions(ctx, project, path) {
			if a.ID == id {
				return true
			}
		}
		return false
	}
	if !has([]string{ring, "raw", "1"}, "exporttrusted") || !has([]string{ring, "copy"}, "importtrusted") || has([]string{ring, "wrap", "1"}, "exporttrusted") {
		t.Fatal("Export trusted-wrapped or Import trusted-wrapped version is not offered where it should be")
	}
	res, err := kp.ActAtResult(ctx, project, []string{ring, "raw", "1"}, "exporttrusted", map[string]string{"wrappingKey": wv})
	if err != nil {
		t.Fatal(err)
	}
	wrapped := res.Items[0].Fields["Wrapped key"]
	if _, err := kp.ActAtResult(ctx, project, []string{ring, "copy"}, "importtrusted", map[string]string{"importingKey": wv, "algorithm": "AES_256_GCM", "material": wrapped}); err != nil {
		t.Fatal(err)
	}
	enc, err := c.RawEncrypt(ctx, &kmspb.RawEncryptRequest{Name: ring + "/cryptoKeys/raw/cryptoKeyVersions/1", Plaintext: []byte("carried")})
	if err != nil {
		t.Fatal(err)
	}
	dec, err := c.RawDecrypt(ctx, &kmspb.RawDecryptRequest{Name: ring + "/cryptoKeys/copy/cryptoKeyVersions/1", Ciphertext: enc.GetCiphertext(), InitializationVector: enc.GetInitializationVector()})
	if err != nil || string(dec.GetPlaintext()) != "carried" {
		t.Fatalf("RawDecrypt with the imported copy = %v, %v", dec, err)
	}

	if _, err := kp.ActAtResult(ctx, project, []string{ring}, "createimportjob", map[string]string{"importJobId": "j", "importMethod": "RSA_OAEP_3072_SHA256_AES_256",
		"protectionLevel": "HSM_SINGLE_TENANT", "cryptoKeyBackend": inst}); err != nil {
		t.Fatal(err)
	}
	if j, err := c.GetImportJob(ctx, &kmspb.GetImportJobRequest{Name: ring + "/importJobs/j"}); err != nil || j.GetCryptoKeyBackend() != inst {
		t.Fatalf("import job = %v, %v", j, err)
	}
}
