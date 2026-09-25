//go:build compat

package compat

import (
	"bytes"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"github.com/tink-crypto/tink-go-gcpkms/v2/integration/gcpkms"
	"github.com/tink-crypto/tink-go/v2/aead"
	"github.com/tink-crypto/tink-go/v2/tink"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestKMSTinkEnvelopeEncryption (#416): Tink, through tink-go-gcpkms, as the
// acceptance client. Its remote AEAD sends both CRCs and fails unless the
// server verified both; its envelope AEAD wraps a local AES-256-GCM DEK with
// the KMS key and must still open after the key's primary rotates. Both of
// tink-go-gcpkms's transports are run: gRPC and REST share the KMS port.
func TestKMSTinkEnvelopeEncryption(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	addr := h.Endpoint(EnvKMS)
	admin := kmsClients(t, h)["grpc"]
	ring, err := admin.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + h.Project() + "/locations/global", KeyRingId: "tink-ring"})
	if err != nil {
		t.Fatalf("CreateKeyRing: %v", err)
	}
	for name, opts := range map[string][]gcpkms.Option{
		"grpc": {gcpkms.WithTransport(gcpkms.TransportGRPC), gcpkms.WithGoogleAPIClientOptions(option.WithEndpoint(addr),
			option.WithoutAuthentication(), option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))},
		"rest": {gcpkms.WithTransport(gcpkms.TransportREST), gcpkms.WithGoogleAPIClientOptions(option.WithEndpoint("http://"+addr+"/"),
			option.WithoutAuthentication())},
	} {
		t.Run(name, func(t *testing.T) {
			key, err := admin.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k-" + name,
				CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
			if err != nil {
				t.Fatalf("CreateCryptoKey: %v", err)
			}
			uri := "gcp-kms://" + key.GetName()
			client, err := gcpkms.NewClient(ctx, "gcp-kms://", opts...)
			if err != nil {
				t.Fatalf("gcpkms.NewClient: %v", err)
			}
			remote, err := client.GetAEAD(uri)
			if err != nil {
				t.Fatalf("GetAEAD: %v", err)
			}
			pt, ad := []byte("tink says hello"), []byte("associated data")
			ct, err := remote.Encrypt(pt, ad)
			if err != nil {
				t.Fatalf("remote AEAD Encrypt: %v", err)
			}
			if got, err := remote.Decrypt(ct, ad); err != nil || !bytes.Equal(got, pt) {
				t.Fatalf("remote AEAD Decrypt = %q, %v", got, err)
			}

			envelope := aead.NewKMSEnvelopeAEAD2(aead.AES256GCMKeyTemplate(), remote)
			sealed, err := envelope.Encrypt(pt, ad)
			if err != nil {
				t.Fatalf("envelope Encrypt: %v", err)
			}
			if _, err := admin.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: key.GetName()}); err != nil {
				t.Fatalf("CreateCryptoKeyVersion: %v", err)
			}
			if _, err := admin.UpdateCryptoKeyPrimaryVersion(ctx, &kmspb.UpdateCryptoKeyPrimaryVersionRequest{Name: key.GetName(), CryptoKeyVersionId: "2"}); err != nil {
				t.Fatalf("UpdateCryptoKeyPrimaryVersion: %v", err)
			}
			if got, err := envelope.Decrypt(sealed, ad); err != nil || !bytes.Equal(got, pt) {
				t.Errorf("the envelope sealed before rotation = %q, %v; want it to still open", got, err)
			}
			if again, err := envelope.Encrypt(pt, ad); err != nil {
				t.Errorf("envelope Encrypt after rotation: %v", err)
			} else if got, err := envelope.Decrypt(again, ad); err != nil || !bytes.Equal(got, pt) {
				t.Errorf("an envelope sealed after rotation = %q, %v", got, err)
			}

			// Tink wraps the KMS error, so only failure is asserted.
			tampered := bytes.Clone(sealed)
			tampered[len(tampered)-1] ^= 1
			for what, aeadUnderTest := range map[string]tink.AEAD{"remote": remote, "envelope": envelope} {
				c := ct
				if what == "envelope" {
					c = sealed
				}
				if _, err := aeadUnderTest.Decrypt(c, []byte("wrong associated data")); err == nil {
					t.Errorf("%s AEAD decrypted with the wrong associated data", what)
				}
			}
			if _, err := envelope.Decrypt(tampered, ad); err == nil {
				t.Error("the envelope AEAD decrypted a tampered ciphertext")
			}
		})
	}
}
