package main

import (
	"context"
	"strings"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/service/kms"
	"github.com/cloudburrow/cloudburrow/internal/service/secrets"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestConsoleSecretCMEK (#1453): Create secret takes a customer-managed key,
// encrypted through the instance's Cloud KMS; the secret's page shows it and
// changes it (Change encryption keys); a version's page shows the key
// version; and Show value fails while the key version is disabled.
func TestConsoleSecretCMEK(t *testing.T) {
	p := secretsFixture(t)
	ctx := context.Background()
	st := p.svc.Store()
	k := kms.NewServer(store.NewMemory())
	st.SetKeyService(func() secrets.KeyService { return k })
	ring := "projects/demo-p/locations/global/keyRings/r"
	if _, err := k.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/demo-p/locations/global", KeyRingId: "r", KeyRing: &kmspb.KeyRing{}}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"k", "k2"} {
		if _, err := k.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring, CryptoKeyId: id,
			CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}}); err != nil {
			t.Fatal(err)
		}
	}
	key := ring + "/cryptoKeys/k"
	if _, err := p.Create(ctx, "demo", map[string]string{"secretId": "c", "payload": "s3cret",
		"kmsKeys": "projects/demo-p/locations/us-east1/keyRings/r/cryptoKeys/k"}); err == nil {
		t.Error("a regional key on automatic replication was accepted")
	}
	if _, err := p.Create(ctx, "demo", map[string]string{"secretId": "c", "payload": "s3cret", "kmsKeys": key}); err != nil {
		t.Fatal(err)
	}
	sec, _ := st.GetSecret("demo", "c")
	d, err := p.Detail(ctx, "demo", []string{sec.Name})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detailText(d), key) {
		t.Errorf("the page does not show the key: %s", detailText(d))
	}
	found := false
	for _, a := range p.DetailActions(ctx, "demo", []string{sec.Name}) {
		found = found || a.ID == actSecretKeys
	}
	if !found {
		t.Fatal("no Change encryption keys action")
	}
	vd, err := p.Detail(ctx, "demo", []string{sec.Name, "1"})
	if err != nil {
		t.Fatal(err)
	}
	if text := detailText(vd); !strings.Contains(text, key+"/cryptoKeyVersions/1") || !strings.Contains(text, "6 bytes") {
		t.Errorf("the version page does not show the key version and size: %s", text)
	}
	if _, value, err := p.Reveal(ctx, "demo", []string{sec.Name, "1"}); err != nil || value != "s3cret" {
		t.Fatalf("Reveal = %q, %v", value, err)
	}
	if _, err := k.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{
		CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: key + "/cryptoKeyVersions/1", State: kmspb.CryptoKeyVersion_DISABLED},
		UpdateMask:       &fieldmaskpb.FieldMask{Paths: []string{"state"}}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Reveal(ctx, "demo", []string{sec.Name, "1"}); err == nil || !strings.Contains(err.Error(), "DISABLED") {
		t.Errorf("Reveal with the key disabled = %v", err)
	}

	if err := p.ActAt(ctx, "demo", []string{sec.Name}, actSecretKeys, map[string]string{"kmsKeys": ring + "/cryptoKeys/k2"}); err != nil {
		t.Fatal(err)
	}
	if sec, _ = st.GetSecret("demo", "c"); sec.KMSKeys[0] != ring+"/cryptoKeys/k2" {
		t.Errorf("keys = %v", sec.KMSKeys)
	}
	if err := p.ActAt(ctx, "demo", []string{sec.Name}, actSecretKeys, map[string]string{"kmsKeys": ""}); err != nil {
		t.Fatal(err)
	}
	if sec, _ = st.GetSecret("demo", "c"); sec.KMSKeys != nil {
		t.Errorf("keys after removing = %v", sec.KMSKeys)
	}
}
