package kms

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// fakeHierarchy puts demo-project under folders/2 under folders/1.
type fakeHierarchy struct{}

func (fakeHierarchy) Ancestors(project string) ([]string, error) {
	if project != "projects/demo-project" {
		return nil, errors.New("no such project")
	}
	return []string{"folders/2", "folders/1", "organizations/9"}, nil
}

func (fakeHierarchy) FolderExists(name string) error {
	if name == "folders/1" || name == "folders/2" {
		return nil
	}
	return errors.New("no such folder")
}

type extraClients struct {
	kms     *kms.KeyManagementClient
	ekm     *kms.EkmClient
	autokey *kms.AutokeyClient
	admin   *kms.AutokeyAdminClient
}

func extras(t *testing.T) extraClients {
	t.Helper()
	srv := NewServer(store.NewMemory())
	srv.UseHierarchy(fakeHierarchy{})
	g := grpc.NewServer()
	srv.Register(g)
	srv.Operations().Register(g)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(g.Stop)
	opts := []option.ClientOption{option.WithEndpoint(ln.Addr().String()), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
	ctx := context.Background()
	var out extraClients
	if out.kms, err = kms.NewKeyManagementClient(ctx, opts...); err != nil {
		t.Fatal(err)
	}
	if out.ekm, err = kms.NewEkmClient(ctx, opts...); err != nil {
		t.Fatal(err)
	}
	if out.autokey, err = kms.NewAutokeyClient(ctx, opts...); err != nil {
		t.Fatal(err)
	}
	if out.admin, err = kms.NewAutokeyAdminClient(ctx, opts...); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = out.kms.Close(); _ = out.ekm.Close(); _ = out.autokey.Close(); _ = out.admin.Close() })
	return out
}

func TestHSMKeysBehaveAsSoftwareOnesWithoutAttestation(t *testing.T) {
	c := extras(t).kms
	ctx := context.Background()
	ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "hsm"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k", CryptoKey: &kmspb.CryptoKey{
		Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, VersionTemplate: &kmspb.CryptoKeyVersionTemplate{ProtectionLevel: kmspb.ProtectionLevel_HSM},
		KeyAccessJustificationsPolicy: &kmspb.KeyAccessJustificationsPolicy{AllowedAccessReasons: []kmspb.AccessReason{kmspb.AccessReason_CUSTOMER_INITIATED_ACCESS}}}})
	if err != nil || key.GetVersionTemplate().GetProtectionLevel() != kmspb.ProtectionLevel_HSM || key.GetPrimary().GetProtectionLevel() != kmspb.ProtectionLevel_HSM ||
		len(key.GetKeyAccessJustificationsPolicy().GetAllowedAccessReasons()) != 1 {
		t.Fatalf("CreateCryptoKey(HSM) = %v, %v", key, err)
	}
	enc, err := c.Encrypt(ctx, &kmspb.EncryptRequest{Name: key.GetName(), Plaintext: []byte("hi")})
	if err != nil || enc.GetProtectionLevel() != kmspb.ProtectionLevel_HSM {
		t.Fatalf("Encrypt = %v, %v", enc, err)
	}
	// The policy is not enforced: no reason is attached, and decrypt works.
	if dec, err := c.Decrypt(ctx, &kmspb.DecryptRequest{Name: key.GetName(), Ciphertext: enc.GetCiphertext()}); err != nil || string(dec.GetPlaintext()) != "hi" {
		t.Fatalf("Decrypt = %v, %v", dec, err)
	}
	v, err := c.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: key.GetName()})
	if err != nil || v.GetProtectionLevel() != kmspb.ProtectionLevel_HSM {
		t.Fatalf("CreateCryptoKeyVersion = %v, %v", v, err)
	}
	it := c.ListCryptoKeyVersions(ctx, &kmspb.ListCryptoKeyVersionsRequest{Parent: key.GetName(), View: kmspb.CryptoKeyVersion_FULL})
	for {
		got, err := it.Next()
		if err != nil {
			break
		}
		if got.GetAttestation() != nil {
			t.Errorf("%s has an attestation; none is made without an HSM", got.GetName())
		}
	}
	upd, err := c.UpdateCryptoKey(ctx, &kmspb.UpdateCryptoKeyRequest{CryptoKey: &kmspb.CryptoKey{Name: key.GetName()},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"key_access_justifications_policy"}}})
	if err != nil || upd.GetKeyAccessJustificationsPolicy() != nil {
		t.Errorf("clearing the policy = %v, %v", upd, err)
	}
	if _, err := c.UpdateCryptoKey(ctx, &kmspb.UpdateCryptoKeyRequest{CryptoKey: &kmspb.CryptoKey{Name: key.GetName(),
		VersionTemplate: &kmspb.CryptoKeyVersionTemplate{ProtectionLevel: kmspb.ProtectionLevel_HSM}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"version_template.protection_level"}}}); err != nil {
		t.Errorf("the same level = %v", err)
	}
	// EXTERNAL needs skip_initial_version_creation, and EXTERNAL_VPC a
	// connection (external_test.go, #1469); HSM_SINGLE_TENANT stays 501.
	for pl, want := range map[kmspb.ProtectionLevel]codes.Code{kmspb.ProtectionLevel_EXTERNAL: codes.InvalidArgument,
		kmspb.ProtectionLevel_EXTERNAL_VPC: codes.InvalidArgument, kmspb.ProtectionLevel_HSM_SINGLE_TENANT: codes.Unimplemented} {
		_, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "x", CryptoKey: &kmspb.CryptoKey{
			Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, VersionTemplate: &kmspb.CryptoKeyVersionTemplate{ProtectionLevel: pl}}})
		if status.Code(err) != want {
			t.Errorf("%s = %v, want %s", pl, err, want)
		}
	}
}

func selfSigned(t *testing.T) []byte {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(0xabc), Subject: pkix.Name{CommonName: "ekm.example"},
		DNSNames: []string{"ekm.example"}, NotBefore: time.Unix(1700000000, 0), NotAfter: time.Unix(1900000000, 0)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func TestEkmConnectionsAndConfig(t *testing.T) {
	c := extras(t).ekm
	ctx := context.Background()
	in := &kmspb.EkmConnection{ServiceResolvers: []*kmspb.EkmConnection_ServiceResolver{{
		ServiceDirectoryService: "projects/demo-project/locations/us-central1/namespaces/ns/services/ekm",
		Hostname:                "ekm.example", ServerCertificates: []*kmspb.Certificate{{RawDer: selfSigned(t)}}}}}
	conn, err := c.CreateEkmConnection(ctx, &kmspb.CreateEkmConnectionRequest{Parent: loc, EkmConnectionId: "c", EkmConnection: in})
	if err != nil {
		t.Fatal(err)
	}
	cert := conn.GetServiceResolvers()[0].GetServerCertificates()[0]
	if !cert.GetParsed() || cert.GetSerialNumber() != "ABC" || cert.GetSubject() != "CN=ekm.example" || conn.GetKeyManagementMode() != kmspb.EkmConnection_MANUAL || conn.GetEtag() == "" {
		t.Errorf("CreateEkmConnection = %v", conn)
	}
	if _, err := c.CreateEkmConnection(ctx, &kmspb.CreateEkmConnectionRequest{Parent: loc, EkmConnectionId: "c", EkmConnection: in}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("a second create = %v", err)
	}
	if _, err := c.CreateEkmConnection(ctx, &kmspb.CreateEkmConnectionRequest{Parent: loc, EkmConnectionId: "d",
		EkmConnection: &kmspb.EkmConnection{KeyManagementMode: kmspb.EkmConnection_CLOUD_KMS}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("CLOUD_KMS without crypto_space_path = %v", err)
	}
	upd, err := c.UpdateEkmConnection(ctx, &kmspb.UpdateEkmConnectionRequest{EkmConnection: &kmspb.EkmConnection{Name: conn.GetName(), Etag: conn.GetEtag(),
		KeyManagementMode: kmspb.EkmConnection_CLOUD_KMS, CryptoSpacePath: "v0/longlived/a1"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"key_management_mode", "crypto_space_path"}}})
	if err != nil || upd.GetKeyManagementMode() != kmspb.EkmConnection_CLOUD_KMS || upd.GetEtag() == conn.GetEtag() {
		t.Fatalf("UpdateEkmConnection = %v, %v", upd, err)
	}
	if _, err := c.UpdateEkmConnection(ctx, &kmspb.UpdateEkmConnectionRequest{EkmConnection: &kmspb.EkmConnection{Name: conn.GetName(), Etag: conn.GetEtag()},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"crypto_space_path"}}}); status.Code(err) != codes.Aborted {
		t.Errorf("a stale etag = %v", err)
	}
	it := c.ListEkmConnections(ctx, &kmspb.ListEkmConnectionsRequest{Parent: loc})
	if got, err := it.Next(); err != nil || got.GetName() != conn.GetName() {
		t.Errorf("ListEkmConnections = %v, %v", got, err)
	}
	cfg, err := c.GetEkmConfig(ctx, &kmspb.GetEkmConfigRequest{Name: loc + "/ekmConfig"})
	if err != nil || cfg.GetDefaultEkmConnection() != "" {
		t.Errorf("GetEkmConfig = %v, %v", cfg, err)
	}
	if cfg, err = c.UpdateEkmConfig(ctx, &kmspb.UpdateEkmConfigRequest{EkmConfig: &kmspb.EkmConfig{Name: loc + "/ekmConfig", DefaultEkmConnection: conn.GetName()},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"default_ekm_connection"}}}); err != nil || cfg.GetDefaultEkmConnection() != conn.GetName() {
		t.Errorf("UpdateEkmConfig = %v, %v", cfg, err)
	}
	if _, err := c.UpdateEkmConfig(ctx, &kmspb.UpdateEkmConfigRequest{EkmConfig: &kmspb.EkmConfig{Name: loc + "/ekmConfig", DefaultEkmConnection: loc + "/ekmConnections/absent"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"default_ekm_connection"}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a missing default = %v", err)
	}
	if _, err := c.VerifyConnectivity(ctx, &kmspb.VerifyConnectivityRequest{Name: conn.GetName()}); err != nil {
		t.Errorf("VerifyConnectivity = %v", err)
	}
	if _, err := c.VerifyConnectivity(ctx, &kmspb.VerifyConnectivityRequest{Name: loc + "/ekmConnections/absent"}); status.Code(err) != codes.NotFound {
		t.Errorf("VerifyConnectivity of an absent connection = %v", err)
	}
}

func TestAutokeyProvisionsAnHSMKeyInTheKeyProject(t *testing.T) {
	cs := extras(t)
	ctx := context.Background()
	region := "projects/demo-project/locations/us-central1"
	if _, err := cs.autokey.CreateKeyHandle(ctx, &kmspb.CreateKeyHandleRequest{Parent: region,
		KeyHandle: &kmspb.KeyHandle{ResourceTypeSelector: "storage.googleapis.com/Bucket"}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("CreateKeyHandle unconfigured = %v", err)
	}
	cfg, err := cs.admin.GetAutokeyConfig(ctx, &kmspb.GetAutokeyConfigRequest{Name: "folders/1/autokeyConfig"})
	if err != nil || cfg.GetState() != kmspb.AutokeyConfig_UNINITIALIZED {
		t.Fatalf("GetAutokeyConfig = %v, %v", cfg, err)
	}
	cfg, err = cs.admin.UpdateAutokeyConfig(ctx, &kmspb.UpdateAutokeyConfigRequest{AutokeyConfig: &kmspb.AutokeyConfig{Name: "folders/1/autokeyConfig", KeyProject: "projects/key-project"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"key_project"}}})
	if err != nil || cfg.GetState() != kmspb.AutokeyConfig_ACTIVE || cfg.GetKeyProject() != "projects/key-project" {
		t.Fatalf("UpdateAutokeyConfig = %v, %v", cfg, err)
	}
	if _, err := cs.admin.UpdateAutokeyConfig(ctx, &kmspb.UpdateAutokeyConfigRequest{AutokeyConfig: &kmspb.AutokeyConfig{Name: "folders/7/autokeyConfig", KeyProject: "projects/key-project"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"key_project"}}}); status.Code(err) != codes.NotFound {
		t.Errorf("a folder that does not exist = %v", err)
	}
	if _, err := cs.admin.UpdateAutokeyConfig(ctx, &kmspb.UpdateAutokeyConfigRequest{AutokeyConfig: &kmspb.AutokeyConfig{Name: "folders/1/autokeyConfig", Etag: "stale"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"key_project"}}}); status.Code(err) != codes.Aborted {
		t.Errorf("a stale etag = %v", err)
	}
	eff, err := cs.admin.ShowEffectiveAutokeyConfig(ctx, &kmspb.ShowEffectiveAutokeyConfigRequest{Parent: "projects/demo-project"})
	if err != nil || eff.GetKeyProject() != "projects/key-project" {
		t.Fatalf("ShowEffectiveAutokeyConfig = %v, %v", eff, err)
	}
	op, err := cs.autokey.CreateKeyHandle(ctx, &kmspb.CreateKeyHandleRequest{Parent: region, KeyHandleId: "h",
		KeyHandle: &kmspb.KeyHandle{ResourceTypeSelector: "storage.googleapis.com/Bucket"}})
	if err != nil {
		t.Fatal(err)
	}
	h, err := op.Wait(ctx)
	if err != nil || h.GetName() != region+"/keyHandles/h" || !strings.HasPrefix(h.GetKmsKey(), "projects/key-project/locations/us-central1/keyRings/autokey/cryptoKeys/storage-bucket-") {
		t.Fatalf("KeyHandle = %v, %v", h, err)
	}
	key, err := cs.kms.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: h.GetKmsKey()})
	if err != nil || key.GetVersionTemplate().GetProtectionLevel() != kmspb.ProtectionLevel_HSM || key.GetRotationPeriod().AsDuration() != 365*24*time.Hour ||
		key.GetPurpose() != kmspb.CryptoKey_ENCRYPT_DECRYPT || key.GetPrimary() == nil {
		t.Errorf("the Autokey key = %v, %v", key, err)
	}
	if got, err := cs.autokey.GetKeyHandle(ctx, &kmspb.GetKeyHandleRequest{Name: h.GetName()}); err != nil || got.GetKmsKey() != h.GetKmsKey() {
		t.Errorf("GetKeyHandle = %v, %v", got, err)
	}
	// A second handle reuses the key ring.
	if op, err := cs.autokey.CreateKeyHandle(ctx, &kmspb.CreateKeyHandleRequest{Parent: region,
		KeyHandle: &kmspb.KeyHandle{ResourceTypeSelector: "compute.googleapis.com/Disk"}}); err != nil {
		t.Fatal(err)
	} else if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	it := cs.autokey.ListKeyHandles(ctx, &kmspb.ListKeyHandlesRequest{Parent: region, Filter: `resource_type_selector="storage.googleapis.com/Bucket"`})
	n := 0
	for {
		if _, err := it.Next(); err != nil {
			break
		}
		n++
	}
	if n != 1 {
		t.Errorf("filtered ListKeyHandles = %d handles, want 1", n)
	}
	// The project's own config wins over its folders'.
	if _, err := cs.admin.UpdateAutokeyConfig(ctx, &kmspb.UpdateAutokeyConfigRequest{AutokeyConfig: &kmspb.AutokeyConfig{Name: "projects/demo-project/autokeyConfig",
		KeyProjectResolutionMode: kmspb.AutokeyConfig_RESOURCE_PROJECT}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"key_project_resolution_mode"}}}); err != nil {
		t.Fatal(err)
	}
	if eff, err := cs.admin.ShowEffectiveAutokeyConfig(ctx, &kmspb.ShowEffectiveAutokeyConfigRequest{Parent: "projects/demo-project"}); err != nil || eff.GetKeyProject() != "projects/demo-project" {
		t.Errorf("RESOURCE_PROJECT = %v, %v", eff, err)
	}
	if _, err := cs.autokey.CreateKeyHandle(ctx, &kmspb.CreateKeyHandleRequest{Parent: region, KeyHandleId: "bad",
		KeyHandle: &kmspb.KeyHandle{ResourceTypeSelector: "bucket"}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a malformed selector = %v", err)
	}
}
