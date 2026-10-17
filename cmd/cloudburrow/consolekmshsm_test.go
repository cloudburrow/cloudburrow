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
