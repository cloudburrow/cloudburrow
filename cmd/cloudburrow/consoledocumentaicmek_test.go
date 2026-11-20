package main

import (
	"context"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"

	"github.com/cloudburrow/cloudburrow/internal/cmek"
	"github.com/cloudburrow/cloudburrow/internal/service/kms"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestConsoleDocumentAIProcessorCMEK (#1950): Create processor's Advanced
// options carry Google's encryption choice; a customer-managed key reaches
// the processor, and its page shows the key.
func TestConsoleDocumentAIProcessorCMEK(t *testing.T) {
	ctx := context.Background()
	s := localAIForConsole(t)
	k := kms.NewServer(store.NewMemory())
	s.UseCMEK(cmek.Keys{KMS: func() cmek.KMS { return cmek.InProcess(k) }})
	ring, err := k.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/my-proj/locations/us-central1", KeyRingId: "r"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := k.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	procs := docAIProcessorsProvider{d: docAI{defaultProject: "p1", addr: s.Addr}}
	_, fields := procs.CreateForm()
	var enc, kf bool
	for _, f := range fields {
		enc = enc || f.Name == "encryption" && f.Section == "Advanced options" && f.Default == "google"
		kf = kf || f.Name == "kmsKeyName" && f.ShowWhen == "encryption=cmek" && f.Browse == "kms"
	}
	if !enc || !kf {
		t.Fatalf("Create processor's fields: %+v", fields)
	}
	id, err := procs.Create(ctx, "p1", map[string]string{"type": "OCR_PROCESSOR", "name": "enc", "region": "us",
		"encryption": "cmek", "kmsKeyName": key.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := procs.Detail(ctx, "p1", []string{id})
	found := false
	for _, p := range d.Summary {
		found = found || p.Label == "Encryption key" && p.Value == key.GetName()
	}
	if !found {
		t.Fatalf("processor page: %+v", d.Summary)
	}
	if _, err := procs.Create(ctx, "p1", map[string]string{"type": "OCR_PROCESSOR", "name": "plain", "region": "us",
		"encryption": "google", "kmsKeyName": key.GetName()}); err != nil {
		t.Fatalf("Google-managed ignores the key field: %v", err)
	}
}

// TestConsoleSpeechEncryptionSettings (#1950): Recognizers' Encryption
// settings set the region's Config key, and a recognizer created after it
// is encrypted with it; an empty key removes it.
func TestConsoleSpeechEncryptionSettings(t *testing.T) {
	ctx := context.Background()
	s := localAIForConsole(t)
	k := kms.NewServer(store.NewMemory())
	s.UseCMEK(cmek.Keys{KMS: func() cmek.KMS { return cmek.InProcess(k) }})
	ring, err := k.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/my-proj/locations/global", KeyRingId: "r"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := k.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	p := speechRecognizersProvider{s: speechSvc{defaultProject: "p1", addr: s.Addr}}
	acts := p.ListActions(ctx, "p1")
	if len(acts) != 1 || acts[0].Label != "Encryption settings" || acts[0].Fields[1].Browse != "kms" {
		t.Fatalf("list actions: %+v", acts)
	}
	if err := p.ActAt(ctx, "p1", nil, "encryption", map[string]string{"region": "global", "kmsKeyName": key.GetName()}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Create(ctx, "p1", map[string]string{"name": "enc-rec", "region": "global", "language": "en-US"}); err != nil {
		t.Fatal(err)
	}
	r, err := p.s.recognizer(ctx, "p1", "enc-rec")
	if err != nil || r.GetKmsKeyName() != key.GetName() || r.GetKmsKeyVersionName() != key.GetName()+"/cryptoKeyVersions/1" {
		t.Fatalf("recognizer after Encryption settings: %v %v", r, err)
	}
	if err := p.ActAt(ctx, "p1", nil, "encryption", map[string]string{"region": "global"}); err != nil {
		t.Fatalf("removing the key: %v", err)
	}
}
