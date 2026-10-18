//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	kmspb "cloud.google.com/go/kms/apiv1/kmspb"
	backupdr "google.golang.org/api/backupdr/v1"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// TestBackupDRVaultEncryptionConfig (#1501): through the official Go
// clients, a backup vault created with encryptionConfig.kmsKeyName, a
// CryptoKey of this instance's Cloud KMS in the vault's location, answers
// it; a key in another location is 400. A backup into the vault is taken
// while the key's version is enabled; with it disabled, triggerBackup is
// refused FAILED_PRECONDITION naming the key; enabled again, it works.
// resourceBackupConfigs.list answers the instance's config: vaulted, with
// the association, its vault and the rule's last successful backup.
// covers: backupdr.projects.locations.backupVaults.create, backupdr.projects.locations.backupVaults.get, backupdr.projects.locations.backupPlans.create, backupdr.projects.locations.backupPlanAssociations.create, backupdr.projects.locations.backupPlanAssociations.triggerBackup, backupdr.projects.locations.resourceBackupConfigs.list, google.cloud.kms.v1.KeyManagementService/UpdateCryptoKeyVersion
func TestBackupDRVaultEncryptionConfig(t *testing.T) {
	h := New(t)
	shardEndpoint(h, envCMEKKMS, EnvKMS)
	bdr := backupDRService(t, h)
	kc := kmsClients(t, h)["grpc"]
	if os.Getenv(EnvBackupDR) == "" || os.Getenv(EnvKMS) == "" {
		t.Skip("needs --services backupdr,kms")
	}
	if os.Getenv(EnvMySQL) == "" {
		t.Skip("needs --services cloudsql-mysql")
	}
	sqlAdminService(t, h)
	wait := backupDRWaiter(t, bdr)
	ctx := h.Context()
	project := h.Project()
	loc := "projects/" + project + "/locations/us-central1"
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	ring, err := kc.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "bdr-" + suffix})
	if err != nil {
		t.Fatal(err)
	}
	ck, err := kc.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "vault",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	key := ck.GetName()

	_, err = bdr.Projects.Locations.BackupVaults.Create("projects/"+project+"/locations/europe-west1", &backupdr.BackupVault{
		BackupMinimumEnforcedRetentionDuration: "86400s", EncryptionConfig: &backupdr.EncryptionConfig{KmsKeyName: key}}).
		BackupVaultId("cmek-elsewhere-" + suffix).Do()
	wantBackupDRCode(t, "a key in another location", err, http.StatusBadRequest, "location")

	vaultID := "cmek-" + suffix
	vaultName := loc + "/backupVaults/" + vaultID
	wait(bdr.Projects.Locations.BackupVaults.Create(loc, &backupdr.BackupVault{BackupMinimumEnforcedRetentionDuration: "86400s",
		EncryptionConfig: &backupdr.EncryptionConfig{KmsKeyName: key}}).BackupVaultId(vaultID).Do())
	defer func() {
		if op, err := bdr.Projects.Locations.BackupVaults.Delete(vaultName).Force(true).Do(); err == nil {
			wait(op, nil)
		}
	}()
	v, err := bdr.Projects.Locations.BackupVaults.Get(vaultName).Do()
	if err != nil || v.EncryptionConfig == nil || v.EncryptionConfig.KmsKeyName != key {
		t.Fatalf("backupVaults.get: %+v %v", v, err)
	}

	planName := loc + "/backupPlans/cmek-" + suffix
	wait(bdr.Projects.Locations.BackupPlans.Create(loc, &backupdr.BackupPlan{BackupVault: vaultName,
		ResourceType: "sqladmin.googleapis.com/Instance", BackupRules: dailyRule(), LogRetentionDays: 7}).BackupPlanId("cmek-" + suffix).Do())
	defer func() {
		if op, err := bdr.Projects.Locations.BackupPlans.Delete(planName).Do(); err == nil {
			wait(op, nil)
		}
	}()
	assocName := loc + "/backupPlanAssociations/cmek-" + suffix
	wait(bdr.Projects.Locations.BackupPlanAssociations.Create(loc, &backupdr.BackupPlanAssociation{BackupPlan: planName,
		Resource: "projects/" + project + "/instances/mysql", ResourceType: "sqladmin.googleapis.com/Instance"}).
		BackupPlanAssociationId("cmek-" + suffix).Do())
	defer func() {
		if op, err := bdr.Projects.Locations.BackupPlanAssociations.Delete(assocName).Do(); err == nil {
			wait(op, nil)
		}
	}()
	wait(bdr.Projects.Locations.BackupPlanAssociations.TriggerBackup(assocName,
		&backupdr.TriggerBackupRequest{RuleId: "daily"}).Do())

	// resourceBackupConfigs.list: the instance, vaulted in this vault by
	// this association, with its rule's last successful backup.
	rbc, err := bdr.Projects.Locations.ResourceBackupConfigs.List(loc).Do()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range rbc.ResourceBackupConfigs {
		if c.TargetResourceDisplayName != "mysql" {
			continue
		}
		if c.TargetResourceType != "CLOUD_SQL_INSTANCE" || !c.Vaulted || !c.BackupConfigured {
			t.Errorf("the instance's resource backup config: %+v", c)
		}
		for _, d := range c.BackupConfigsDetails {
			if d.Type == "BACKUPDR_BACKUP_PLAN" && d.BackupConfigSource == assocName && d.BackupVault == vaultName && d.LatestSuccessfulBackupTime != "" &&
				d.BackupDrPlanConfig != nil && len(d.BackupDrPlanConfig.BackupDrPlanRules) == 1 {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("resourceBackupConfigs.list has no BACKUPDR config of the instance in %s: %d configs", vaultName, len(rbc.ResourceBackupConfigs))
	}

	version := func(st kmspb.CryptoKeyVersion_CryptoKeyVersionState) {
		t.Helper()
		if _, err := kc.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{
			CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: key + "/cryptoKeyVersions/1", State: st},
			UpdateMask:       &fieldmaskpb.FieldMask{Paths: []string{"state"}}}); err != nil {
			t.Fatalf("UpdateCryptoKeyVersion %v: %v", st, err)
		}
	}
	version(kmspb.CryptoKeyVersion_DISABLED)
	_, err = bdr.Projects.Locations.BackupPlanAssociations.TriggerBackup(assocName, &backupdr.TriggerBackupRequest{RuleId: "daily"}).Do()
	wantBackupDRCode(t, "a backup while the vault's key is disabled", err, http.StatusBadRequest, key)
	version(kmspb.CryptoKeyVersion_ENABLED)
	wait(bdr.Projects.Locations.BackupPlanAssociations.TriggerBackup(assocName,
		&backupdr.TriggerBackupRequest{RuleId: "daily"}).Do())
}
