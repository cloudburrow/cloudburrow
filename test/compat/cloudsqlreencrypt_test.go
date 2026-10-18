//go:build compat

package compat

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	kmspb "cloud.google.com/go/kms/apiv1/kmspb"
	sqladmin "google.golang.org/api/sqladmin/v1"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// TestCloudSQLReencrypt (#1614): through the official Go client, a CMEK
// instance whose CryptoKey has a new primary version is re-encrypted with
// instances.reencrypt, and again with gcloud sql instances reencrypt: the
// operation is REENCRYPT, diskEncryptionStatus names the new version, and
// with the old version disabled the instance stays RUNNABLE and restarts.
// backupReencryptionConfig is 501 (backups here are not encrypted with the
// key), and a data key whose version is disabled is not re-encrypted.
// covers: sql.instances.reencrypt, sql.instances.insert, sql.instances.get, sql.instances.restart, sql.instances.delete, google.cloud.kms.v1.KeyManagementService/CreateCryptoKeyVersion, google.cloud.kms.v1.KeyManagementService/UpdateCryptoKeyPrimaryVersion
func TestCloudSQLReencrypt(t *testing.T) {
	h := New(t)
	svc := sqlAdminService(t, h)
	shardEndpoint(h, envCMEKKMS, EnvKMS)
	kc := kmsClients(t, h)["grpc"]
	if os.Getenv(EnvCloudSQL) == "" || os.Getenv(EnvKMS) == "" {
		t.Skip("needs --services cloudsql,kms")
	}
	ctx := h.Context()
	project := h.Project()
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	key, version := sqlKey(t, h, "reenc-"+suffix)
	name := "reenc-" + suffix
	op, err := svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: "POSTGRES_17",
		Settings:                    &sqladmin.Settings{Tier: "db-custom-1-3840"},
		DiskEncryptionConfiguration: &sqladmin.DiskEncryptionConfiguration{KmsKeyName: key}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		version(kmspb.CryptoKeyVersion_ENABLED)
		if op, err := svc.Instances.Delete(project, name).Do(); err == nil {
			waitSQLOp(t, svc, project, op)
		}
	}()
	waitSQLOp(t, svc, project, op)

	v2, err := kc.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: key})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kc.UpdateCryptoKeyPrimaryVersion(ctx, &kmspb.UpdateCryptoKeyPrimaryVersionRequest{Name: key,
		CryptoKeyVersionId: v2.GetName()[len(key+"/cryptoKeyVersions/"):]}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = kc.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{
			CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: v2.GetName(), State: kmspb.CryptoKeyVersion_ENABLED},
			UpdateMask:       &fieldmaskpb.FieldMask{Paths: []string{"state"}}})
	}()
	_, err = svc.Instances.Reencrypt(project, name, &sqladmin.InstancesReencryptRequest{
		BackupReencryptionConfig: &sqladmin.BackupReencryptionConfig{BackupLimit: 5, BackupType: "AUTOMATED"}}).Do()
	wantCode(t, "reencrypt with backupReencryptionConfig", err, 501, "backupReencryptionConfig")

	op, err = svc.Instances.Reencrypt(project, name, &sqladmin.InstancesReencryptRequest{}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if op.OperationType != "REENCRYPT" {
		t.Errorf("operationType %q, want REENCRYPT", op.OperationType)
	}
	waitSQLOp(t, svc, project, op)
	in, err := svc.Instances.Get(project, name).Do()
	if err != nil {
		t.Fatal(err)
	}
	if in.DiskEncryptionStatus == nil || in.DiskEncryptionStatus.KmsKeyVersionName != v2.GetName() {
		t.Fatalf("after reencrypt: %+v, want %s", in.DiskEncryptionStatus, v2.GetName())
	}

	// gcloud sql instances reencrypt, through the configuration
	// `cloudburrow gcloud-setup` writes, onto a third primary version.
	v3, err := kc.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: key})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kc.UpdateCryptoKeyPrimaryVersion(ctx, &kmspb.UpdateCryptoKeyPrimaryVersionRequest{Name: key,
		CryptoKeyVersionId: v3.GetName()[len(key+"/cryptoKeyVersions/"):]}); err != nil {
		t.Fatal(err)
	}
	g := newGcloudSession(t, h)
	g.must("sql", "instances", "reencrypt", name, "--project", project, "--quiet")
	if out := g.must("sql", "instances", "describe", name, "--project", project,
		"--format=value(diskEncryptionStatus.kmsKeyVersionName)"); !strings.Contains(out, v3.GetName()) {
		t.Fatalf("after gcloud sql instances reencrypt: %s, want %s", out, v3.GetName())
	}
	defer func() {
		_, _ = kc.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{
			CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: v3.GetName(), State: kmspb.CryptoKeyVersion_ENABLED},
			UpdateMask:       &fieldmaskpb.FieldMask{Paths: []string{"state"}}})
	}()

	// The version that wrapped the data key before is no longer needed.
	version(kmspb.CryptoKeyVersion_DISABLED)
	if in, err = svc.Instances.Get(project, name).Do(); err != nil || in.State != "RUNNABLE" {
		t.Fatalf("with the old version disabled: %v %s %v", err, in.State, in.SuspensionReason)
	}
	op, err = svc.Instances.Restart(project, name).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOp(t, svc, project, op)

	// With the version that wraps the data key now disabled, and version
	// 1 enabled, the instance is suspended again: the key was checked, not
	// skipped.
	if _, err := kc.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{
		CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: v3.GetName(), State: kmspb.CryptoKeyVersion_DISABLED},
		UpdateMask:       &fieldmaskpb.FieldMask{Paths: []string{"state"}}}); err != nil {
		t.Fatal(err)
	}
	version(kmspb.CryptoKeyVersion_ENABLED)
	if in, err = svc.Instances.Get(project, name).Do(); err != nil || in.State != "SUSPENDED" {
		t.Errorf("with the new version disabled: %v %s", err, in.State)
	}
	_, err = svc.Instances.Reencrypt(project, name, &sqladmin.InstancesReencryptRequest{}).Do()
	wantSQLRefusal(t, "reencrypt with the wrapping version disabled", err, key)
}
