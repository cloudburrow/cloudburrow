//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"cloud.google.com/go/spanner"
	database "cloud.google.com/go/spanner/admin/database/apiv1"
	databasepb "cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	instance "cloud.google.com/go/spanner/admin/instance/apiv1"
	instancepb "cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// spannerKeyText is Google's documented message for a key Spanner cannot
// use (docs.cloud.google.com/spanner/docs/cmek).
const spannerKeyText = "KMS key required by the Spanner resource is not accessible."

func wantSpannerKeyRefusal(t *testing.T, what string, err error) {
	t.Helper()
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(status.Convert(err).Message(), spannerKeyText) {
		t.Errorf("%s = %v, want FAILED_PRECONDITION %q", what, err, spannerKeyText)
	}
}

// TestSpannerCMEK (#1950): customer-managed encryption keys from the
// instance's Cloud KMS, with the official Go admin client, on an instance
// in regional-us-central1: CreateDatabase with encryption_config reports
// the key and its version in encryption_config and encryption_info; a key
// in another region is INVALID_ARGUMENT; a backup of the database uses its
// key by default (USE_DATABASE_ENCRYPTION), and one made with
// CUSTOMER_MANAGED_ENCRYPTION another key, both reporting the version in
// encryption_info; a copy keeps the source's key; a restore is encrypted
// with the backup's key. While the backup key's version is disabled, the
// restore and the copy are FAILED_PRECONDITION with Google's message;
// while the database key's version is disabled, the database's sessions
// are refused the same way; both work again once enabled.
//
// covers: google.spanner.admin.database.v1.DatabaseAdmin/CreateDatabase, google.spanner.admin.database.v1.DatabaseAdmin/GetDatabase, google.spanner.admin.database.v1.DatabaseAdmin/CreateBackup, google.spanner.admin.database.v1.DatabaseAdmin/CopyBackup, google.spanner.admin.database.v1.DatabaseAdmin/RestoreDatabase, google.spanner.admin.database.v1.DatabaseAdmin/GetBackup
func TestSpannerCMEK(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvSpanner)
	// Cloud KMS as the emulators shard's CMEK_KMS, so the KMS suite stays
	// in served.
	shardEndpoint(h, envCMEKKMS, EnvKMS)
	t.Setenv("SPANNER_EMULATOR_HOST", addr)
	ctx := h.Context()
	k := kmsClients(t, h)["grpc"]
	ring, err := k.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + h.Project() + "/locations/us-central1", KeyRingId: "spanner-cmek"})
	if err != nil {
		t.Fatalf("CreateKeyRing: %v", err)
	}
	var keys []string
	for _, n := range []string{"db", "backup"} {
		key, err := k.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: n,
			CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
		if err != nil {
			t.Fatalf("CreateCryptoKey %s: %v", n, err)
		}
		keys = append(keys, key.GetName())
	}
	dbKey, backupKey := keys[0], keys[1]
	setState := func(key string, st kmspb.CryptoKeyVersion_CryptoKeyVersionState) {
		t.Helper()
		if _, err := k.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{
			CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: key + "/cryptoKeyVersions/1", State: st},
			UpdateMask:       &fieldmaskpb.FieldMask{Paths: []string{"state"}}}); err != nil {
			t.Fatalf("set %s to %v: %v", key, st, err)
		}
	}

	ic, err := instance.NewInstanceAdminClient(ctx, spannerOpts(addr)...)
	if err != nil {
		t.Fatal(err)
	}
	defer ic.Close()
	inst := fmt.Sprintf("projects/%s/instances/cb-cmek", h.Project())
	iop, err := ic.CreateInstance(ctx, &instancepb.CreateInstanceRequest{Parent: "projects/" + h.Project(), InstanceId: "cb-cmek",
		Instance: &instancepb.Instance{Config: "projects/" + h.Project() + "/instanceConfigs/regional-us-central1", DisplayName: "CMEK", NodeCount: 1}})
	if err != nil && status.Code(err) != codes.AlreadyExists {
		t.Fatalf("CreateInstance: %v", err)
	}
	if err == nil {
		if _, err := iop.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = ic.DeleteInstance(ctx, &instancepb.DeleteInstanceRequest{Name: inst}) })
	dc, err := database.NewDatabaseAdminClient(ctx, spannerOpts(addr)...)
	if err != nil {
		t.Fatal(err)
	}
	defer dc.Close()

	if _, err := dc.CreateDatabase(ctx, &databasepb.CreateDatabaseRequest{Parent: inst, CreateStatement: "CREATE DATABASE wrongloc",
		EncryptionConfig: &databasepb.EncryptionConfig{KmsKeyName: "projects/" + h.Project() + "/locations/europe-west1/keyRings/r/cryptoKeys/k"}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateDatabase with a key in another region = %v, want INVALID_ARGUMENT", err)
	}
	op, err := dc.CreateDatabase(ctx, &databasepb.CreateDatabaseRequest{Parent: inst, CreateStatement: "CREATE DATABASE cmekdb",
		ExtraStatements:  []string{`CREATE TABLE Items (Id INT64 NOT NULL, Name STRING(MAX)) PRIMARY KEY (Id)`},
		EncryptionConfig: &databasepb.EncryptionConfig{KmsKeyName: dbKey}})
	if err != nil {
		t.Fatalf("CreateDatabase with a key: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	db := inst + "/databases/cmekdb"
	got, err := dc.GetDatabase(ctx, &databasepb.GetDatabaseRequest{Name: db})
	if err != nil || got.GetEncryptionConfig().GetKmsKeyName() != dbKey || len(got.GetEncryptionInfo()) != 1 ||
		got.GetEncryptionInfo()[0].GetKmsKeyVersion() != dbKey+"/cryptoKeyVersions/1" ||
		got.GetEncryptionInfo()[0].GetEncryptionType() != databasepb.EncryptionInfo_CUSTOMER_MANAGED_ENCRYPTION {
		t.Fatalf("GetDatabase = %v, %v", got, err)
	}
	c, err := spanner.NewClient(ctx, db, spannerOpts(addr)...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Apply(ctx, []*spanner.Mutation{spanner.Insert("Items", []string{"Id", "Name"}, []any{1, "one"})}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	c.Close()

	expire := timestamppb.New(time.Now().Add(48 * time.Hour))
	backup := func(id string, cfg *databasepb.CreateBackupEncryptionConfig) *databasepb.Backup {
		t.Helper()
		bop, err := dc.CreateBackup(ctx, &databasepb.CreateBackupRequest{Parent: inst, BackupId: id, EncryptionConfig: cfg,
			Backup: &databasepb.Backup{Database: db, ExpireTime: expire}})
		if err != nil {
			t.Fatalf("CreateBackup %s: %v", id, err)
		}
		b, err := bop.Wait(ctx)
		if err != nil {
			t.Fatalf("CreateBackup %s: %v", id, err)
		}
		return b
	}
	byDB := backup("by-db", nil)
	if byDB.GetEncryptionInfo().GetKmsKeyVersion() != dbKey+"/cryptoKeyVersions/1" ||
		byDB.GetEncryptionInfo().GetEncryptionType() != databasepb.EncryptionInfo_CUSTOMER_MANAGED_ENCRYPTION {
		t.Errorf("a backup by the database's encryption: encryption_info %v", byDB.GetEncryptionInfo())
	}
	own := backup("own", &databasepb.CreateBackupEncryptionConfig{EncryptionType: databasepb.CreateBackupEncryptionConfig_CUSTOMER_MANAGED_ENCRYPTION, KmsKeyName: backupKey})
	if own.GetEncryptionInfo().GetKmsKeyVersion() != backupKey+"/cryptoKeyVersions/1" {
		t.Errorf("a backup with its own key: encryption_info %v", own.GetEncryptionInfo())
	}
	if g := backup("google", &databasepb.CreateBackupEncryptionConfig{EncryptionType: databasepb.CreateBackupEncryptionConfig_GOOGLE_DEFAULT_ENCRYPTION}); g.GetEncryptionInfo().GetEncryptionType() != databasepb.EncryptionInfo_GOOGLE_DEFAULT_ENCRYPTION {
		t.Errorf("a Google-managed backup: encryption_info %v", g.GetEncryptionInfo())
	}
	if gb, err := dc.GetBackup(ctx, &databasepb.GetBackupRequest{Name: own.GetName()}); err != nil || gb.GetEncryptionInfo().GetKmsKeyVersion() != backupKey+"/cryptoKeyVersions/1" {
		t.Errorf("GetBackup = %v, %v", gb, err)
	}

	restore := func(id string) (*databasepb.Database, error) {
		rop, err := dc.RestoreDatabase(ctx, &databasepb.RestoreDatabaseRequest{Parent: inst, DatabaseId: id,
			Source: &databasepb.RestoreDatabaseRequest_Backup{Backup: own.GetName()}})
		if err != nil {
			return nil, err
		}
		return rop.Wait(ctx)
	}
	copyOwn := func(id string) (*databasepb.Backup, error) {
		cop, err := dc.CopyBackup(ctx, &databasepb.CopyBackupRequest{Parent: inst, BackupId: id, SourceBackup: own.GetName(), ExpireTime: expire})
		if err != nil {
			return nil, err
		}
		return cop.Wait(ctx)
	}

	setState(backupKey, kmspb.CryptoKeyVersion_DISABLED)
	_, err = restore("refused")
	wantSpannerKeyRefusal(t, "RestoreDatabase with the backup's key version disabled", err)
	_, err = copyOwn("refused-copy")
	wantSpannerKeyRefusal(t, "CopyBackup with the backup's key version disabled", err)
	setState(backupKey, kmspb.CryptoKeyVersion_ENABLED)

	cp, err := copyOwn("own-copy")
	if err != nil || cp.GetEncryptionInfo().GetKmsKeyVersion() != backupKey+"/cryptoKeyVersions/1" {
		t.Errorf("CopyBackup = %v, %v; want the source's key", cp.GetEncryptionInfo(), err)
	}
	if _, err := restore("restored"); err != nil {
		t.Fatalf("RestoreDatabase with the key enabled: %v", err)
	}
	r, err := dc.GetDatabase(ctx, &databasepb.GetDatabaseRequest{Name: inst + "/databases/restored"})
	if err != nil || r.GetEncryptionConfig().GetKmsKeyName() != backupKey || len(r.GetEncryptionInfo()) != 1 {
		t.Errorf("the restored database = %v, %v; want the backup's key", r, err)
	}
	if rows := spannerRows1950(t, addr, inst+"/databases/restored"); rows != "1=one" {
		t.Errorf("the restored rows = %q", rows)
	}

	setState(dbKey, kmspb.CryptoKeyVersion_DISABLED)
	c2, err := spanner.NewClient(ctx, db, spannerOpts(addr)...)
	if err == nil {
		_, err = c2.Single().ReadRow(ctx, "Items", spanner.Key{1}, []string{"Name"})
		c2.Close()
	}
	wantSpannerKeyRefusal(t, "a read of the database with its key version disabled", err)
	setState(dbKey, kmspb.CryptoKeyVersion_ENABLED)
	if rows := spannerRows1950(t, addr, db); rows != "1=one" {
		t.Errorf("the database's rows with its key enabled again = %q", rows)
	}
}

// spannerRows1950 reads Items' rows as "id=name", comma-separated.
func spannerRows1950(t *testing.T, addr, db string) string {
	t.Helper()
	ctx := t.Context()
	c, err := spanner.NewClient(ctx, db, spannerOpts(addr)...)
	if err != nil {
		t.Fatalf("spanner.NewClient(%s): %v", db, err)
	}
	defer c.Close()
	var out []string
	err = c.Single().Query(ctx, spanner.Statement{SQL: "SELECT CAST(Id AS STRING), Name FROM Items ORDER BY Id"}).Do(func(r *spanner.Row) error {
		var id, name string
		if err := r.Columns(&id, &name); err != nil {
			return err
		}
		out = append(out, id+"="+name)
		return nil
	})
	if err != nil {
		t.Fatalf("query %s: %v", db, err)
	}
	return strings.Join(out, ",")
}
