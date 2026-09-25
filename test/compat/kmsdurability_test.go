//go:build compat

package compat

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// Cloud KMS across stop and up (#418). A lost key makes every earlier
// ciphertext undecryptable, so CI measures it: TestKMSRestartSetup runs alone
// just before `stop`, and TestKMSAcrossRestart after each `up`.
const (
	envKMSProbe       = "CLOUDBURROW_TEST_KMS_PROBE"
	envKMSExpect      = "CLOUDBURROW_TEST_KMS_EXPECT"
	kmsProbeProject   = "kms-restart-probe"
	kmsProbePlaintext = "written-before-stop"
	kmsProbeAAD       = "probe-aad"
)

// kmsProbe is what the setup leaves for the probe, in the file envKMSProbe names.
type kmsProbe struct {
	Ring        string    `json:"ring"`
	Key         string    `json:"key"`
	Ciphertext  []byte    `json:"ciphertext"`
	DestroyTime time.Time `json:"destroyTime"`
}

// TestKMSRestartSetup leaves a key with an ENABLED primary v1, a DISABLED v2
// and a DESTROY_SCHEDULED v3, and a v1 ciphertext, for TestKMSAcrossRestart.
func TestKMSRestartSetup(t *testing.T) {
	path := os.Getenv(envKMSProbe)
	if path == "" {
		t.Skipf("%s is not set: CI runs this alone, just before stop", envKMSProbe)
	}
	h := New(t)
	ctx := h.Context()
	c := kmsClients(t, h)["grpc"]
	ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + kmsProbeProject + "/locations/global", KeyRingId: "restart"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "probe",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := c.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: key.GetName()}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{
		CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: key.GetName() + "/cryptoKeyVersions/2", State: kmspb.CryptoKeyVersion_DISABLED},
		UpdateMask:       &fieldmaskpb.FieldMask{Paths: []string{"state"}}}); err != nil {
		t.Fatal(err)
	}
	v3, err := c.DestroyCryptoKeyVersion(ctx, &kmspb.DestroyCryptoKeyVersionRequest{Name: key.GetName() + "/cryptoKeyVersions/3"})
	if err != nil || v3.GetState() != kmspb.CryptoKeyVersion_DESTROY_SCHEDULED {
		t.Fatalf("DestroyCryptoKeyVersion = %v, %v", v3, err)
	}
	enc, err := c.Encrypt(ctx, &kmspb.EncryptRequest{Name: key.GetName(), Plaintext: []byte(kmsProbePlaintext), AdditionalAuthenticatedData: []byte(kmsProbeAAD)})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(kmsProbe{Ring: ring.GetName(), Key: key.GetName(), Ciphertext: enc.GetCiphertext(), DestroyTime: v3.GetDestroyTime().AsTime()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestKMSAcrossRestart: after stop and up in persistent mode the key, its
// version states and its ciphertext are intact; in ephemeral mode the ring is
// gone. KMS keeps its Secrets in the managed namespace, which an ephemeral up
// keeps, so the ephemeral run deletes what earlier runs left (#481; #418
// measured the ring surviving before that).
//
// unverified: google.cloud.kms.v1.KeyManagementService/GetKeyRing NOT_FOUND: a key ring that did not survive an ephemeral restart
func TestKMSAcrossRestart(t *testing.T) {
	expect := os.Getenv(envKMSExpect)
	if expect == "" {
		t.Skipf("%s is not set: this runs only after CI restarts the instance", envKMSExpect)
	}
	b, err := os.ReadFile(os.Getenv(envKMSProbe))
	if err != nil {
		t.Fatalf("the probe TestKMSRestartSetup left: %v", err)
	}
	var p kmsProbe
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	h := New(t)
	ctx := h.Context()
	c := kmsClients(t, h)["grpc"]
	switch expect {
	case "present":
		dec, err := c.Decrypt(ctx, &kmspb.DecryptRequest{Name: p.Key, Ciphertext: p.Ciphertext, AdditionalAuthenticatedData: []byte(kmsProbeAAD)})
		if err != nil || !bytes.Equal(dec.GetPlaintext(), []byte(kmsProbePlaintext)) {
			t.Fatalf("Decrypt of the pre-stop ciphertext = %q, %v; want %q", dec.GetPlaintext(), err, kmsProbePlaintext)
		}
		if v, err := c.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: p.Key + "/cryptoKeyVersions/2"}); err != nil || v.GetState() != kmspb.CryptoKeyVersion_DISABLED {
			t.Errorf("v2 = %v, %v; want DISABLED", v.GetState(), err)
		}
		v3, err := c.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: p.Key + "/cryptoKeyVersions/3"})
		if err != nil || v3.GetState() != kmspb.CryptoKeyVersion_DESTROY_SCHEDULED || !v3.GetDestroyTime().AsTime().Equal(p.DestroyTime) {
			t.Errorf("v3 = %v at %v, %v; want DESTROY_SCHEDULED at %v", v3.GetState(), v3.GetDestroyTime().AsTime(), err, p.DestroyTime)
		}
		if k, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: p.Key}); err != nil || k.GetPrimary().GetName() != p.Key+"/cryptoKeyVersions/1" {
			t.Errorf("primary = %v, %v; want version 1", k.GetPrimary().GetName(), err)
		}
		if v4, err := c.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: p.Key}); err != nil || v4.GetName() != p.Key+"/cryptoKeyVersions/4" {
			t.Errorf("the next version = %v, %v; want number 4", v4.GetName(), err)
		}
	case "absent":
		if _, err := c.GetKeyRing(ctx, &kmspb.GetKeyRingRequest{Name: p.Ring}); status.Code(err) != codes.NotFound {
			t.Fatalf("ephemeral mode after stop/up: GetKeyRing = %v; want NotFound (#481)", err)
		}
	default:
		t.Fatalf("%s must be present or absent, not %q", envKMSExpect, expect)
	}
}
