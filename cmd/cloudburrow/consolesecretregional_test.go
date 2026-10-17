package main

import (
	"context"
	"strings"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"

	"github.com/cloudburrow/cloudburrow/internal/service/kms"
	"github.com/cloudburrow/cloudburrow/internal/service/secrets"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestConsoleRegionalSecret (#1519): Create secret with a Location makes a
// regional secret, with a key in that location; the list shows it with its
// location beside the project's global secrets; its page, versions, Show
// value, Add version, its permissions and Delete all reach it by its
// regional name.
func TestConsoleRegionalSecret(t *testing.T) {
	p := secretsFixture(t)
	ctx := context.Background()
	st := p.svc.Store()
	k := kms.NewServer(store.NewMemory())
	st.SetKeyService(func() secrets.KeyService { return k })
	loc := "projects/demo-p/locations/us-central1"
	if _, err := k.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "r", KeyRing: &kmspb.KeyRing{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := k.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: loc + "/keyRings/r", CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}}); err != nil {
		t.Fatal(err)
	}
	key := loc + "/keyRings/r/cryptoKeys/k"
	if _, err := p.Create(ctx, "demo", map[string]string{"secretId": "r", "payload": "x", "location": "us-central1",
		"replication": replicationUserManaged, "locations": "us-east1"}); err == nil {
		t.Error("a regional secret with a replication policy was accepted")
	}
	name, err := p.Create(ctx, "demo", map[string]string{"secretId": "r", "payload": "regional", "location": "us-central1", "kmsKeys": key})
	if err != nil {
		t.Fatal(err)
	}
	if name != "projects/demo/locations/us-central1/secrets/r" {
		t.Fatalf("created %q", name)
	}
	if _, err := p.Create(ctx, "demo", map[string]string{"secretId": "r", "payload": "global"}); err != nil {
		t.Fatalf("a global secret of the same ID: %v", err)
	}
	l, err := p.List(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]string{}
	for _, it := range l.Items {
		found[it.Name] = it.Fields["Location"]
	}
	if found[name] != "us-central1" || found["projects/demo/secrets/r"] != "global" {
		t.Errorf("listed %v", found)
	}
	d, err := p.Detail(ctx, "demo", []string{name})
	if err != nil {
		t.Fatal(err)
	}
	if text := detailText(d); !strings.Contains(text, key) || !strings.Contains(text, "regional secret") {
		t.Errorf("the page = %s", text)
	}
	if err := p.ActAt(ctx, "demo", []string{name}, "addversion", map[string]string{"payload": "two"}); err != nil {
		t.Fatal(err)
	}
	if _, value, err := p.Reveal(ctx, "demo", []string{name, "2"}); err != nil || value != "two" {
		t.Fatalf("Reveal = %q, %v", value, err)
	}
	if _, value, err := p.Reveal(ctx, "demo", []string{"projects/demo/secrets/r", "1"}); err != nil || value != "global" {
		t.Fatalf("Reveal of the global secret = %q, %v", value, err)
	}
	vd, err := p.Detail(ctx, "demo", []string{name, "1"})
	if err != nil || !strings.Contains(detailText(vd), key+"/cryptoKeyVersions/1") {
		t.Errorf("the version page = %s, %v", detailText(vd), err)
	}
	if _, iamName, err := p.secretIAM("demo", []string{name}); err != nil || iamName != name {
		t.Errorf("secretIAM = %q, %v", iamName, err)
	}
	if err := p.Delete(ctx, "demo", name); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetSecret("demo", "r"); err != nil {
		t.Errorf("deleting the regional secret removed the global one: %v", err)
	}
}
