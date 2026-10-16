package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	kmsapi "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// Create key offers HSM, and the key the official client reads back is at
// HSM (#1452).
func TestConsoleKMSCreateKeyAtHSM(t *testing.T) {
	ctx := context.Background()
	p, c := kmsEditFixture(t)
	const project = "hsm-console"
	ring, err := p.Create(ctx, project, map[string]string{"keyRingId": "r", "location": "us-central1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.ActAtResult(ctx, project, []string{ring}, "createkey", map[string]string{"cryptoKeyId": "k", "protectionLevel": "HSM"}); err != nil {
		t.Fatalf("create key: %v", err)
	}
	k, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: ring + "/cryptoKeys/k"})
	if err != nil || k.GetVersionTemplate().GetProtectionLevel() != kmspb.ProtectionLevel_HSM || k.GetPrimary().GetProtectionLevel() != kmspb.ProtectionLevel_HSM {
		t.Fatalf("GetCryptoKey = %v, %v", k, err)
	}
	if _, err := p.ActAtResult(ctx, project, []string{ring}, "createkey", map[string]string{"cryptoKeyId": "x", "protectionLevel": "EXTERNAL"}); err == nil {
		t.Error("an EXTERNAL key was created")
	}
}

func consoleEkmPEM(t *testing.T) string {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "ekm.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// The EKM connections screen creates, shows, edits and sets as default
// through the in-process EkmService, and the official EkmClient reads back
// what the forms did (#1452).
func TestConsoleKMSEkmConnections(t *testing.T) {
	ctx := context.Background()
	kp, _ := kmsEditFixture(t)
	p := kmsEkmProvider{svc: kp.svc}
	ec, err := kmsapi.NewEkmClient(ctx, clientOpts(kp.svc.Addr())...)
	if err != nil {
		t.Fatal(err)
	}
	defer ec.Close()
	const project = "ekm-console"
	name, err := p.Create(ctx, project, map[string]string{"ekmConnectionId": "c", "location": "us-east1",
		"serviceDirectoryService": "projects/" + project + "/locations/us-east1/namespaces/n/services/s",
		"hostname":                "ekm.test", "certificate": consoleEkmPEM(t), "keyManagementMode": "MANUAL"})
	if err != nil {
		t.Fatalf("Create EKM connection: %v", err)
	}
	l, err := p.List(ctx, project)
	if err != nil || len(l.Items) != 1 || l.Items[0].Fields["Hostname"] != "ekm.test" {
		t.Fatalf("List = %+v, %v", l, err)
	}
	d, err := p.Detail(ctx, project, []string{name})
	if err != nil || d.Unavailable != "" || !strings.Contains(consoleDetailText(d), "CN=ekm.test") {
		t.Fatalf("Detail = %+v, %v", d, err)
	}
	if err := p.ActAt(ctx, project, []string{name}, "edit", map[string]string{"keyManagementMode": "CLOUD_KMS"}); err == nil {
		t.Error("CLOUD_KMS without a crypto space path was accepted")
	}
	if err := p.ActAt(ctx, project, []string{name}, "edit", map[string]string{"keyManagementMode": "CLOUD_KMS", "cryptoSpacePath": "v0/space"}); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if err := p.ActAt(ctx, project, []string{name}, "setdefault", nil); err != nil {
		t.Fatalf("Set as default: %v", err)
	}
	got, err := ec.GetEkmConnection(ctx, &kmspb.GetEkmConnectionRequest{Name: name})
	if err != nil || got.GetKeyManagementMode() != kmspb.EkmConnection_CLOUD_KMS || got.GetCryptoSpacePath() != "v0/space" {
		t.Errorf("GetEkmConnection = %v, %v", got, err)
	}
	cfg, err := ec.GetEkmConfig(ctx, &kmspb.GetEkmConfigRequest{Name: "projects/" + project + "/locations/us-east1/ekmConfig"})
	if err != nil || cfg.GetDefaultEkmConnection() != name {
		t.Errorf("GetEkmConfig = %v, %v", cfg, err)
	}
	if d, _ := p.Detail(ctx, "other-project", []string{name}); d.Unavailable == "" {
		t.Error("another project's page opened the connection")
	}
}

// The Autokey screen edits the project's configuration, creates a key
// handle, which makes an HSM key, and lists it; the official clients read
// back what it did (#1452).
func TestConsoleKMSAutokey(t *testing.T) {
	ctx := context.Background()
	kp, c := kmsEditFixture(t)
	p := kmsAutokeyProvider{svc: kp.svc}
	admin, err := kmsapi.NewAutokeyAdminClient(ctx, clientOpts(kp.svc.Addr())...)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	const project = "autokey-console"
	cfgName := "projects/" + project + "/autokeyConfig"
	l, err := p.List(ctx, project)
	if err != nil || len(l.Items) != 1 || l.Items[0].Name != cfgName || !strings.Contains(l.Note, "Autokey is off") {
		t.Fatalf("List = %+v, %v", l, err)
	}
	if _, err := p.Create(ctx, project, map[string]string{"location": "us-central1", "resourceTypeSelector": "storage.googleapis.com/Bucket"}); err == nil {
		t.Error("a key handle was created with Autokey off")
	}
	if err := p.ActAt(ctx, project, []string{cfgName}, "editconfig", map[string]string{"keyProjectResolutionMode": "RESOURCE_PROJECT"}); err != nil {
		t.Fatalf("Edit configuration: %v", err)
	}
	got, err := admin.GetAutokeyConfig(ctx, &kmspb.GetAutokeyConfigRequest{Name: cfgName})
	if err != nil || got.GetKeyProjectResolutionMode() != kmspb.AutokeyConfig_RESOURCE_PROJECT || got.GetState() != kmspb.AutokeyConfig_ACTIVE {
		t.Fatalf("GetAutokeyConfig = %v, %v", got, err)
	}
	if _, err := p.Create(ctx, project, map[string]string{"location": "us-central1", "resourceTypeSelector": "storage.googleapis.com/Bucket", "keyHandleId": "h"}); err != nil {
		t.Fatalf("Create key handle: %v", err)
	}
	d, err := p.Detail(ctx, project, []string{cfgName})
	if err != nil || len(d.Sections) == 0 || d.Sections[0].ID != "keyhandles" || len(d.Sections[0].Listing.Items) != 1 {
		t.Fatalf("Detail = %+v, %v", d, err)
	}
	key := d.Sections[0].Listing.Items[0].Fields["Key"]
	k, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key})
	if err != nil || k.GetVersionTemplate().GetProtectionLevel() != kmspb.ProtectionLevel_HSM ||
		!strings.HasPrefix(key, "projects/"+project+"/locations/us-central1/keyRings/autokey/") {
		t.Errorf("the Autokey key %s = %v, %v", key, k, err)
	}
	if d, _ := p.Detail(ctx, project, []string{"folders/1/autokeyConfig"}); d.Unavailable == "" {
		t.Error("a configuration outside the project's chain opened")
	}
}

// consoleDetailText is every property value on a page, for a containment
// check.
func consoleDetailText(d console.Detail) string {
	var b strings.Builder
	for _, p := range d.Summary {
		b.WriteString(p.Value + "\n")
	}
	for _, s := range d.Sections {
		for _, g := range s.Groups {
			for _, p := range g.Properties {
				b.WriteString(p.Value + "\n")
			}
		}
	}
	return b.String()
}
