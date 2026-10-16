//go:build compat

package compat

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// kmsOptions are the official clients' options for the KMS port, over
// gRPC or REST.
func kmsOptions(h *Harness, variant string) []option.ClientOption {
	addr := h.Endpoint(EnvKMS)
	if variant == "rest" {
		return []option.ClientOption{option.WithEndpoint("http://" + addr), option.WithoutAuthentication()}
	}
	return []option.ClientOption{option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
}

// TestKMSHSMKeysAndKeyAccessJustifications (#1452): an HSM key is created,
// used and listed as a SOFTWARE one is, over gRPC and REST, reporting HSM
// on the key, its versions and every crypto response; the FULL view has no
// attestation, since no HSM made one. key_access_justifications_policy is
// stored at create, changed and cleared by UpdateCryptoKey, and not
// enforced: Encrypt and Decrypt carry no justification and succeed.
// EXTERNAL and EXTERNAL_VPC stay UNIMPLEMENTED naming the field.
//
// covers: google.cloud.kms.v1.KeyManagementService/CreateCryptoKey, google.cloud.kms.v1.KeyManagementService/UpdateCryptoKey, google.cloud.kms.v1.KeyManagementService/Encrypt, google.cloud.kms.v1.KeyManagementService/Decrypt, google.cloud.kms.v1.KeyManagementService/ListCryptoKeyVersions, google.cloud.kms.v1.KeyManagementService/CreateCryptoKeyVersion
func TestKMSHSMKeysAndKeyAccessJustifications(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	for variant, c := range kmsClients(t, h) {
		t.Run(variant, func(t *testing.T) {
			ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + h.Project() + "/locations/us-central1", KeyRingId: "hsm-" + variant})
			if err != nil {
				t.Fatal(err)
			}
			policy := &kmspb.KeyAccessJustificationsPolicy{AllowedAccessReasons: []kmspb.AccessReason{kmspb.AccessReason_CUSTOMER_INITIATED_ACCESS}}
			key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k", CryptoKey: &kmspb.CryptoKey{
				Purpose:                       kmspb.CryptoKey_ENCRYPT_DECRYPT,
				VersionTemplate:               &kmspb.CryptoKeyVersionTemplate{ProtectionLevel: kmspb.ProtectionLevel_HSM, Algorithm: kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION},
				KeyAccessJustificationsPolicy: policy}})
			if err != nil {
				t.Fatal(err)
			}
			if key.GetVersionTemplate().GetProtectionLevel() != kmspb.ProtectionLevel_HSM || key.GetPrimary().GetProtectionLevel() != kmspb.ProtectionLevel_HSM ||
				len(key.GetKeyAccessJustificationsPolicy().GetAllowedAccessReasons()) != 1 {
				t.Errorf("CreateCryptoKey(HSM) = %v", key)
			}
			enc, err := c.Encrypt(ctx, &kmspb.EncryptRequest{Name: key.GetName(), Plaintext: []byte("hsm")})
			if err != nil || enc.GetProtectionLevel() != kmspb.ProtectionLevel_HSM {
				t.Fatalf("Encrypt = %v, %v", enc, err)
			}
			dec, err := c.Decrypt(ctx, &kmspb.DecryptRequest{Name: key.GetName(), Ciphertext: enc.GetCiphertext()})
			if err != nil || string(dec.GetPlaintext()) != "hsm" || dec.GetProtectionLevel() != kmspb.ProtectionLevel_HSM {
				t.Fatalf("Decrypt = %v, %v", dec, err)
			}
			if v, err := c.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: key.GetName()}); err != nil || v.GetProtectionLevel() != kmspb.ProtectionLevel_HSM {
				t.Errorf("CreateCryptoKeyVersion = %v, %v", v, err)
			}
			it := c.ListCryptoKeyVersions(ctx, &kmspb.ListCryptoKeyVersionsRequest{Parent: key.GetName(), View: kmspb.CryptoKeyVersion_FULL})
			n := 0
			for {
				v, err := it.Next()
				if errors.Is(err, iterator.Done) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				n++
				if v.GetProtectionLevel() != kmspb.ProtectionLevel_HSM || v.GetAttestation() != nil {
					t.Errorf("FULL view of %s = %v; want HSM and no attestation", v.GetName(), v)
				}
			}
			if n != 2 {
				t.Errorf("listed %d versions, want 2", n)
			}
			upd, err := c.UpdateCryptoKey(ctx, &kmspb.UpdateCryptoKeyRequest{CryptoKey: &kmspb.CryptoKey{Name: key.GetName(),
				KeyAccessJustificationsPolicy: &kmspb.KeyAccessJustificationsPolicy{AllowedAccessReasons: []kmspb.AccessReason{
					kmspb.AccessReason_CUSTOMER_INITIATED_SUPPORT, kmspb.AccessReason_GOOGLE_INITIATED_SERVICE}}},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"key_access_justifications_policy"}}})
			if err != nil || len(upd.GetKeyAccessJustificationsPolicy().GetAllowedAccessReasons()) != 2 {
				t.Fatalf("UpdateCryptoKey(policy) = %v, %v", upd, err)
			}
			// Not enforced: CUSTOMER_INITIATED_ACCESS is no longer allowed,
			// and a decrypt still succeeds.
			if _, err := c.Decrypt(ctx, &kmspb.DecryptRequest{Name: key.GetName(), Ciphertext: enc.GetCiphertext()}); err != nil {
				t.Errorf("Decrypt under a narrower policy = %v; the policy is not enforced", err)
			}
			upd, err = c.UpdateCryptoKey(ctx, &kmspb.UpdateCryptoKeyRequest{CryptoKey: &kmspb.CryptoKey{Name: key.GetName()},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"key_access_justifications_policy"}}})
			if err != nil || upd.GetKeyAccessJustificationsPolicy() != nil {
				t.Errorf("clearing the policy = %v, %v", upd, err)
			}
			for _, pl := range []kmspb.ProtectionLevel{kmspb.ProtectionLevel_EXTERNAL, kmspb.ProtectionLevel_EXTERNAL_VPC} {
				_, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "ext", CryptoKey: &kmspb.CryptoKey{
					Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, VersionTemplate: &kmspb.CryptoKeyVersionTemplate{ProtectionLevel: pl}}})
				if kmsCode(variant, err) != codes.Unimplemented || !strings.Contains(err.Error(), "protection_level") {
					t.Errorf("%s = %v, want UNIMPLEMENTED naming protection_level", pl, err)
				}
			}
		})
	}
}

// ekmCertificate is a self-signed leaf certificate for an EKM replica.
func ekmCertificate(t *testing.T) []byte {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(0x1452), Subject: pkix.Name{CommonName: "ekm.example.com"},
		DNSNames: []string{"ekm.example.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// TestKMSEkmConnectionsAreStored (#1452): EkmConnections and the EkmConfig
// are CRUD through the official EkmClient, over gRPC and REST. A connection
// is created with one service resolver and a server certificate, which comes
// back parsed; it is listed, read, and updated with its etag (a stale etag is
// ABORTED); the location's EkmConfig takes it as the default connection.
// VerifyConnectivity is UNIMPLEMENTED, since nothing reaches an EKM.
//
// covers: google.cloud.kms.v1.EkmService/CreateEkmConnection, google.cloud.kms.v1.EkmService/GetEkmConnection, google.cloud.kms.v1.EkmService/ListEkmConnections, google.cloud.kms.v1.EkmService/UpdateEkmConnection, google.cloud.kms.v1.EkmService/GetEkmConfig, google.cloud.kms.v1.EkmService/UpdateEkmConfig
// covers: google.cloud.kms.v1.EkmService/VerifyConnectivity (unimplemented)
func TestKMSEkmConnectionsAreStored(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	der := ekmCertificate(t)
	for _, variant := range []string{"grpc", "rest"} {
		t.Run(variant, func(t *testing.T) {
			newClient := kms.NewEkmClient
			if variant == "rest" {
				newClient = kms.NewEkmRESTClient
			}
			c, err := newClient(ctx, kmsOptions(h, variant)...)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			loc := "projects/" + h.Project() + "/locations/us-east1"
			if variant == "rest" {
				loc = "projects/" + h.Project() + "/locations/us-west1"
			}
			conn, err := c.CreateEkmConnection(ctx, &kmspb.CreateEkmConnectionRequest{Parent: loc, EkmConnectionId: "ekm",
				EkmConnection: &kmspb.EkmConnection{ServiceResolvers: []*kmspb.EkmConnection_ServiceResolver{{
					ServiceDirectoryService: "projects/" + h.Project() + "/locations/us-east1/namespaces/ekm/services/replica",
					Hostname:                "ekm.example.com",
					ServerCertificates:      []*kmspb.Certificate{{RawDer: der}},
				}}}})
			if err != nil {
				t.Fatal(err)
			}
			cert := conn.GetServiceResolvers()[0].GetServerCertificates()[0]
			if conn.GetName() != loc+"/ekmConnections/ekm" || conn.GetCreateTime() == nil || conn.GetEtag() == "" ||
				conn.GetKeyManagementMode() != kmspb.EkmConnection_MANUAL || !cert.GetParsed() || cert.GetSerialNumber() != "1452" ||
				cert.GetSubject() != "CN=ekm.example.com" || len(cert.GetSha256Fingerprint()) != 64 {
				t.Errorf("CreateEkmConnection = %v", conn)
			}
			if got, err := c.GetEkmConnection(ctx, &kmspb.GetEkmConnectionRequest{Name: conn.GetName()}); err != nil || got.GetEtag() != conn.GetEtag() {
				t.Errorf("GetEkmConnection = %v, %v", got, err)
			}
			if got, err := c.ListEkmConnections(ctx, &kmspb.ListEkmConnectionsRequest{Parent: loc}).Next(); err != nil || got.GetName() != conn.GetName() {
				t.Errorf("ListEkmConnections = %v, %v", got, err)
			}
			upd, err := c.UpdateEkmConnection(ctx, &kmspb.UpdateEkmConnectionRequest{
				EkmConnection: &kmspb.EkmConnection{Name: conn.GetName(), Etag: conn.GetEtag(), KeyManagementMode: kmspb.EkmConnection_CLOUD_KMS, CryptoSpacePath: "v0/longlived/crypto-space"},
				UpdateMask:    &fieldmaskpb.FieldMask{Paths: []string{"key_management_mode", "crypto_space_path"}}})
			if err != nil || upd.GetKeyManagementMode() != kmspb.EkmConnection_CLOUD_KMS || upd.GetCryptoSpacePath() != "v0/longlived/crypto-space" {
				t.Fatalf("UpdateEkmConnection = %v, %v", upd, err)
			}
			if _, err := c.UpdateEkmConnection(ctx, &kmspb.UpdateEkmConnectionRequest{
				EkmConnection: &kmspb.EkmConnection{Name: conn.GetName(), Etag: conn.GetEtag(), CryptoSpacePath: "v0/other"},
				UpdateMask:    &fieldmaskpb.FieldMask{Paths: []string{"crypto_space_path"}}}); kmsCode(variant, err) != codes.Aborted {
				t.Errorf("a stale etag = %v, want ABORTED", err)
			}
			cfg, err := c.GetEkmConfig(ctx, &kmspb.GetEkmConfigRequest{Name: loc + "/ekmConfig"})
			if err != nil || cfg.GetName() != loc+"/ekmConfig" || cfg.GetDefaultEkmConnection() != "" {
				t.Errorf("GetEkmConfig = %v, %v", cfg, err)
			}
			cfg, err = c.UpdateEkmConfig(ctx, &kmspb.UpdateEkmConfigRequest{EkmConfig: &kmspb.EkmConfig{Name: loc + "/ekmConfig", DefaultEkmConnection: conn.GetName()},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"default_ekm_connection"}}})
			if err != nil || cfg.GetDefaultEkmConnection() != conn.GetName() {
				t.Errorf("UpdateEkmConfig = %v, %v", cfg, err)
			}
			cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			_, err = c.VerifyConnectivity(cctx, &kmspb.VerifyConnectivityRequest{Name: conn.GetName()})
			cancel()
			if kmsCode(variant, err) != codes.Unimplemented {
				t.Errorf("VerifyConnectivity = %v, want UNIMPLEMENTED", err)
			}
		})
	}
}

// TestKMSAutokeyProvisionsKeysOnDemand (#1452): an AutokeyConfig on a new
// folder names the test project as the key project; a project made in that
// folder sees it through ShowEffectiveAutokeyConfig, and CreateKeyHandle
// there returns an operation (waited on through GetOperation) whose
// KeyHandle names a new HSM ENCRYPT_DECRYPT key, rotated yearly, in the key
// ring "autokey" of the key project and the handle's location. Through the
// official Autokey and AutokeyAdmin clients, over gRPC and REST.
//
// covers: google.cloud.kms.v1.AutokeyAdmin/UpdateAutokeyConfig, google.cloud.kms.v1.AutokeyAdmin/GetAutokeyConfig, google.cloud.kms.v1.AutokeyAdmin/ShowEffectiveAutokeyConfig
// covers: google.cloud.kms.v1.Autokey/CreateKeyHandle, google.cloud.kms.v1.Autokey/GetKeyHandle, google.cloud.kms.v1.Autokey/ListKeyHandles
func TestKMSAutokeyProvisionsKeysOnDemand(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	fc, _, pc := foldersClients(t, h)
	fop, err := fc.CreateFolder(ctx, &rmpb.CreateFolderRequest{Folder: &rmpb.Folder{Parent: localOrg, DisplayName: folderLabel(h, "autokey-")}})
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	folder, err := fop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	project := v1ID(h, "ak-")
	pop, err := pc.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: project, Parent: folder.GetName()}})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if _, err := pop.Wait(ctx); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	cfgName := folder.GetName() + "/autokeyConfig"
	keyProject := "projects/" + h.Project()
	cleanupAdmin := kmsAdminClient(t, h)
	t.Cleanup(func() {
		_, _ = cleanupAdmin.UpdateAutokeyConfig(context.Background(), &kmspb.UpdateAutokeyConfigRequest{AutokeyConfig: &kmspb.AutokeyConfig{Name: cfgName},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"key_project"}}})
	})
	for _, variant := range []string{"grpc", "rest"} {
		t.Run(variant, func(t *testing.T) {
			newAdmin, newAutokey := kms.NewAutokeyAdminClient, kms.NewAutokeyClient
			if variant == "rest" {
				newAdmin, newAutokey = kms.NewAutokeyAdminRESTClient, kms.NewAutokeyRESTClient
			}
			admin, err := newAdmin(ctx, kmsOptions(h, variant)...)
			if err != nil {
				t.Fatal(err)
			}
			defer admin.Close()
			ak, err := newAutokey(ctx, kmsOptions(h, variant)...)
			if err != nil {
				t.Fatal(err)
			}
			defer ak.Close()
			cfg, err := admin.UpdateAutokeyConfig(ctx, &kmspb.UpdateAutokeyConfigRequest{AutokeyConfig: &kmspb.AutokeyConfig{Name: cfgName, KeyProject: keyProject},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"key_project"}}})
			if err != nil || cfg.GetState() != kmspb.AutokeyConfig_ACTIVE || cfg.GetKeyProject() != keyProject || cfg.GetEtag() == "" {
				t.Fatalf("UpdateAutokeyConfig = %v, %v", cfg, err)
			}
			if got, err := admin.GetAutokeyConfig(ctx, &kmspb.GetAutokeyConfigRequest{Name: cfgName}); err != nil || got.GetKeyProject() != keyProject || got.GetEtag() != cfg.GetEtag() {
				t.Errorf("GetAutokeyConfig = %v, %v", got, err)
			}
			eff, err := admin.ShowEffectiveAutokeyConfig(ctx, &kmspb.ShowEffectiveAutokeyConfigRequest{Parent: "projects/" + project})
			if err != nil || eff.GetKeyProject() != keyProject {
				t.Fatalf("ShowEffectiveAutokeyConfig = %v, %v", eff, err)
			}
			parent := "projects/" + project + "/locations/us-central1"
			op, err := ak.CreateKeyHandle(ctx, &kmspb.CreateKeyHandleRequest{Parent: parent, KeyHandleId: "bucket-" + variant,
				KeyHandle: &kmspb.KeyHandle{ResourceTypeSelector: "storage.googleapis.com/Bucket"}})
			if err != nil {
				t.Fatal(err)
			}
			handle, err := op.Wait(ctx)
			if err != nil {
				t.Fatal(err)
			}
			wantRing := keyProject + "/locations/us-central1/keyRings/autokey/cryptoKeys/"
			if handle.GetName() != parent+"/keyHandles/bucket-"+variant || !strings.HasPrefix(handle.GetKmsKey(), wantRing) {
				t.Fatalf("KeyHandle = %v; want a key under %s", handle, wantRing)
			}
			kc := kmsClients(t, h)[variant]
			key, err := kc.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: handle.GetKmsKey()})
			if err != nil || key.GetPurpose() != kmspb.CryptoKey_ENCRYPT_DECRYPT || key.GetVersionTemplate().GetProtectionLevel() != kmspb.ProtectionLevel_HSM ||
				key.GetRotationPeriod().AsDuration() != 365*24*time.Hour || key.GetPrimary().GetState() != kmspb.CryptoKeyVersion_ENABLED {
				t.Errorf("the Autokey key = %v, %v", key, err)
			}
			if got, err := ak.GetKeyHandle(ctx, &kmspb.GetKeyHandleRequest{Name: handle.GetName()}); err != nil || got.GetKmsKey() != handle.GetKmsKey() {
				t.Errorf("GetKeyHandle = %v, %v", got, err)
			}
			found := false
			it := ak.ListKeyHandles(ctx, &kmspb.ListKeyHandlesRequest{Parent: parent, Filter: `resource_type_selector="storage.googleapis.com/Bucket"`})
			for {
				got, err := it.Next()
				if errors.Is(err, iterator.Done) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				found = found || got.GetName() == handle.GetName()
			}
			if !found {
				t.Errorf("ListKeyHandles did not list %s", handle.GetName())
			}
			// A project outside the folder has no Autokey.
			if _, err := ak.CreateKeyHandle(ctx, &kmspb.CreateKeyHandleRequest{Parent: "projects/" + h.Project() + "/locations/us-central1",
				KeyHandle: &kmspb.KeyHandle{ResourceTypeSelector: "storage.googleapis.com/Bucket"}}); kmsCode(variant, err) != codes.FailedPrecondition {
				t.Errorf("CreateKeyHandle without a config = %v, want FAILED_PRECONDITION", err)
			}
		})
	}
	t.Cleanup(func() {
		ctx := context.Background()
		if op, err := pc.DeleteProject(ctx, &rmpb.DeleteProjectRequest{Name: "projects/" + project}); err == nil {
			_, _ = op.Wait(ctx)
		}
		_, _ = fc.DeleteFolder(ctx, &rmpb.DeleteFolderRequest{Name: folder.GetName()})
	})
}

// kmsAdminClient is an AutokeyAdmin client over gRPC, closed at cleanup.
func kmsAdminClient(t *testing.T, h *Harness) *kms.AutokeyAdminClient {
	t.Helper()
	c, err := kms.NewAutokeyAdminClient(h.Context(), kmsOptions(h, "grpc")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func newEkmClient(h *Harness) (*kms.EkmClient, error) {
	return kms.NewEkmClient(h.Context(), kmsOptions(h, "grpc")...)
}

func newAutokeyClient(h *Harness) (*kms.AutokeyClient, error) {
	return kms.NewAutokeyClient(h.Context(), kmsOptions(h, "grpc")...)
}
