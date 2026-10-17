//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	kmspb "cloud.google.com/go/kms/apiv1/kmspb"
	sqladmin "google.golang.org/api/sqladmin/v1"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// sqlKey makes a CryptoKey in us-central1 of this instance's Cloud KMS and
// returns its name and a function that sets its first version's state.
func sqlKey(t *testing.T, h *Harness, ring string) (string, func(kmspb.CryptoKeyVersion_CryptoKeyVersionState)) {
	t.Helper()
	kc := kmsClients(t, h)["grpc"]
	ctx := h.Context()
	r, err := kc.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + h.Project() + "/locations/us-central1", KeyRingId: ring})
	if err != nil {
		t.Fatal(err)
	}
	ck, err := kc.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: r.GetName(), CryptoKeyId: "sql",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	key := ck.GetName()
	return key, func(st kmspb.CryptoKeyVersion_CryptoKeyVersionState) {
		t.Helper()
		if _, err := kc.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{
			CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: key + "/cryptoKeyVersions/1", State: st},
			UpdateMask:       &fieldmaskpb.FieldMask{Paths: []string{"state"}}}); err != nil {
			t.Fatalf("UpdateCryptoKeyVersion %v: %v", st, err)
		}
	}
}

// wantSQLRefusal wants a 400 or 501 naming field.
func wantSQLRefusal(t *testing.T, what string, err error, field string) {
	t.Helper()
	if err == nil || (!strings.Contains(err.Error(), "400") && !strings.Contains(err.Error(), "501")) || !strings.Contains(err.Error(), field) {
		t.Errorf("%s: %v; want 400 or 501 naming %s", what, err, field)
	}
}

// TestCloudSQLFieldsNeverSilentlyDropped (#1538): through the official Go
// client, an instance created with diskEncryptionConfiguration, a
// CryptoKey of this instance's Cloud KMS in its region, answers the key and
// diskEncryptionStatus' version; a key in another region is 400, and a key
// change is 400. While the key's version is disabled the instance is
// SUSPENDED (KMS_KEY_ISSUE), a restart is refused naming the key, and the
// connectors' connectSettings is refused; enabled again it is RUNNABLE and
// restarts. Fields CloudBurrow does not apply are refused by name: a
// settings.connectorEnforcement REQUIRED, a password validation policy, a
// SQL Server setting, a database role on users.insert and a PHYSICAL
// backup run.
// covers: sql.instances.insert, sql.instances.get, sql.instances.patch, sql.instances.restart, sql.instances.delete, sql.connect.get, sql.users.insert, sql.backupRuns.insert, google.cloud.kms.v1.KeyManagementService/UpdateCryptoKeyVersion
func TestCloudSQLFieldsNeverSilentlyDropped(t *testing.T) {
	h := New(t)
	svc := sqlAdminService(t, h)
	kmsClients(t, h)
	if os.Getenv(EnvCloudSQL) == "" || os.Getenv(EnvKMS) == "" {
		t.Skip("needs --services cloudsql,kms")
	}
	project := h.Project()
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	key, version := sqlKey(t, h, "sql-"+suffix)
	tier := &sqladmin.Settings{Tier: "db-custom-1-3840"}

	for what, in := range map[string]*sqladmin.DatabaseInstance{
		"settings.connectorEnforcement": {Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", ConnectorEnforcement: "REQUIRED"}},
		"settings.passwordValidationPolicy": {Settings: &sqladmin.Settings{Tier: "db-custom-1-3840",
			PasswordValidationPolicy: &sqladmin.PasswordValidationPolicy{EnablePasswordPolicy: true, MinLength: 12}}},
		"settings.timeZone":                {Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", TimeZone: "UTC"}},
		"databaseCenterIntegrationEnabled": {Settings: tier, DatabaseCenterIntegrationEnabled: true},
		"kmsKeyName": {Settings: tier, DiskEncryptionConfiguration: &sqladmin.DiskEncryptionConfiguration{
			KmsKeyName: "projects/" + project + "/locations/europe-west1/keyRings/r/cryptoKeys/k"}},
	} {
		in.Name, in.DatabaseVersion = "refused-"+suffix, "POSTGRES_17"
		_, err := svc.Instances.Insert(project, in).Do()
		wantSQLRefusal(t, "insert with "+what, err, what)
	}
	_, err := svc.Users.Insert(project, "postgres", &sqladmin.User{Name: "roles-" + suffix, DatabaseRoles: []string{"pg_read_all_data"}}).Do()
	wantSQLRefusal(t, "users.insert with databaseRoles", err, "databaseRoles")
	_, err = svc.BackupRuns.Insert(project, "postgres", &sqladmin.BackupRun{BackupKind: "PHYSICAL"}).Do()
	wantSQLRefusal(t, "backupRuns.insert of a PHYSICAL backup", err, "backupKind")

	name := "cmek-" + suffix
	op, err := svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: "POSTGRES_17", Settings: tier,
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
	in, err := svc.Instances.Get(project, name).Do()
	if err != nil {
		t.Fatal(err)
	}
	if in.State != "RUNNABLE" || in.DiskEncryptionConfiguration == nil || in.DiskEncryptionConfiguration.KmsKeyName != key ||
		in.DiskEncryptionStatus == nil || in.DiskEncryptionStatus.KmsKeyVersionName != key+"/cryptoKeyVersions/1" {
		t.Fatalf("instance: %s %+v %+v", in.State, in.DiskEncryptionConfiguration, in.DiskEncryptionStatus)
	}
	_, err = svc.Instances.Patch(project, name, &sqladmin.DatabaseInstance{
		DiskEncryptionConfiguration: &sqladmin.DiskEncryptionConfiguration{KmsKeyName: key + "x"}}).Do()
	wantHTTP(t, "a key change", err, http.StatusBadRequest)

	version(kmspb.CryptoKeyVersion_DISABLED)
	if in, err = svc.Instances.Get(project, name).Do(); err != nil || in.State != "SUSPENDED" ||
		len(in.SuspensionReason) != 1 || in.SuspensionReason[0] != "KMS_KEY_ISSUE" {
		t.Errorf("with the key disabled: %v %s %v", err, in.State, in.SuspensionReason)
	}
	_, err = svc.Instances.Restart(project, name).Do()
	wantSQLRefusal(t, "a restart with the key disabled", err, key)
	_, err = svc.Connect.Get(project, name).Do()
	wantHTTP(t, "connectSettings with the key disabled", err, http.StatusPreconditionFailed)

	version(kmspb.CryptoKeyVersion_ENABLED)
	if in, err = svc.Instances.Get(project, name).Do(); err != nil || in.State != "RUNNABLE" {
		t.Errorf("with the key enabled again: %v %s", err, in.State)
	}
	op, err = svc.Instances.Restart(project, name).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOp(t, svc, project, op)
}

// TestTerraformSQLDatabaseInstanceEncryptionKey (#1538): a
// google_sql_database_instance with encryption_key_name, a CryptoKey of
// this instance's Cloud KMS, applies with the official hashicorp/google
// provider through `cloudburrow terraform`, the instance answers the key,
// the second plan is empty, and destroy removes it.
// covers: sql.instances.insert, sql.instances.get, sql.instances.delete, sql.operations.get
func TestTerraformSQLDatabaseInstanceEncryptionKey(t *testing.T) {
	h := New(t)
	svc := sqlAdminService(t, h)
	kmsClients(t, h)
	if os.Getenv(EnvCloudSQL) == "" || os.Getenv(EnvKMS) == "" {
		t.Skip("needs --services cloudsql,kms")
	}
	m := newTFModule(t)
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	key, _ := sqlKey(t, h, "tf-sql-"+suffix)
	name := "tf-cmek-" + suffix
	m.write(fmt.Sprintf(`
resource "google_sql_database_instance" "main" {
  project             = %q
  name                = %q
  region              = "us-central1"
  database_version    = "POSTGRES_17"
  deletion_protection = false
  encryption_key_name = %q
  settings {
    tier = "db-custom-1-3840"
  }
}
`, m.project, name, key))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	in, err := svc.Instances.Get(m.project, name).Do()
	if err != nil {
		t.Fatal(err)
	}
	if in.State != "RUNNABLE" || in.DiskEncryptionConfiguration == nil || in.DiskEncryptionConfiguration.KmsKeyName != key {
		t.Errorf("after apply: %s %+v", in.State, in.DiskEncryptionConfiguration)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	_, err = svc.Instances.Get(m.project, name).Do()
	wantHTTP(t, "instances.get after destroy", err, http.StatusNotFound)
}
