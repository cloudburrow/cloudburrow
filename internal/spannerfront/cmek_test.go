package spannerfront

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	"cloud.google.com/go/spanner/apiv1/spannerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/cmek"
	"github.com/cloudburrow/cloudburrow/internal/service/kms"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

const cmekProject = "projects/my-proj"

func newSpannerKey(t *testing.T, k *kms.Server) string {
	t.Helper()
	ctx := context.Background()
	ring, err := k.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: cmekProject + "/locations/us-central1", KeyRingId: "ring"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := k.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "spanner",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	return key.GetName()
}

func setSpannerKeyVersion(t *testing.T, k *kms.Server, key string, st kmspb.CryptoKeyVersion_CryptoKeyVersionState) {
	t.Helper()
	if _, err := k.UpdateCryptoKeyVersion(context.Background(), &kmspb.UpdateCryptoKeyVersionRequest{
		CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: key + "/cryptoKeyVersions/1", State: st},
		UpdateMask:       &fieldmaskpb.FieldMask{Paths: []string{"state"}}}); err != nil {
		t.Fatal(err)
	}
}

func newKeyedFront(t *testing.T) (*Instances, *kms.Server, string) {
	t.Helper()
	s, err := New("127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	k := kms.NewServer(store.NewMemory())
	s.UseKeys(cmek.Keys{KMS: func() cmek.KMS { return cmek.InProcess(k) }})
	return s, k, newSpannerKey(t, k)
}

func wantKeyRefusal(t *testing.T, what string, err error) {
	t.Helper()
	if status.Code(err) != codes.FailedPrecondition || !strings.HasPrefix(status.Convert(err).Message(), errKeyText) {
		t.Fatalf("%s: %v, want FAILED_PRECONDITION %q", what, err, errKeyText)
	}
}

// TestCMEKBackupSealed (#1950): a customer-managed backup keeps its rows
// sealed with the key, in memory and in its copy file, reports the key
// version, and cannot be restored while the version is disabled.
func TestCMEKBackupSealed(t *testing.T) {
	ctx := context.Background()
	s, k, key := newKeyedFront(t)
	name := cmekProject + "/instances/i/backups/b"
	sn := &snapshot{dialect: databasepb.DatabaseDialect_GOOGLE_STANDARD_SQL, ddl: []string{"CREATE TABLE t (k STRING(10)) PRIMARY KEY (k)"},
		tables: []tableRows{{name: "t", columns: []string{"k"}, rows: []*structpb.ListValue{{Values: []*structpb.Value{structpb.NewStringValue("top-secret-row")}}}}}}
	sealed, err := s.sealSnapshot(ctx, "CreateBackup", name, []string{key}, sn)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := encodeSealed(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("top-secret-row")) || bytes.Contains(raw, []byte("CREATE TABLE")) {
		t.Fatal("the sealed copy holds the rows or schema in the clear")
	}
	back, err := decodeSealed(raw)
	if err != nil || back == nil {
		t.Fatalf("decode: %v", err)
	}
	pb := &databasepb.Backup{Name: name, Database: cmekProject + "/instances/i/databases/d", ExpireTime: timestamppb.New(time.Now().Add(time.Hour))}
	setBackupEncryption(pb, back)
	if pb.GetEncryptionInfo().GetEncryptionType() != databasepb.EncryptionInfo_CUSTOMER_MANAGED_ENCRYPTION ||
		pb.GetEncryptionInfo().GetKmsKeyVersion() != key+"/cryptoKeyVersions/1" || len(pb.GetEncryptionInformation()) != 1 {
		t.Fatalf("encryption_info %v", pb.GetEncryptionInfo())
	}
	b := &backup{pb: pb, sealed: back}
	got, err := s.openSnapshot(ctx, "RestoreDatabase", name, b)
	if err != nil || got.tables[0].rows[0].GetValues()[0].GetStringValue() != "top-secret-row" {
		t.Fatalf("open: %v %v", got, err)
	}
	s.mu.Lock()
	s.backups[name] = b
	s.mu.Unlock()

	setSpannerKeyVersion(t, k, key, kmspb.CryptoKeyVersion_DISABLED)
	_, err = databases{s: s}.RestoreDatabase(ctx, &databasepb.RestoreDatabaseRequest{Parent: cmekProject + "/instances/i", DatabaseId: "restored",
		Source: &databasepb.RestoreDatabaseRequest_Backup{Backup: name}})
	wantKeyRefusal(t, "restore with the version disabled", err)
	setSpannerKeyVersion(t, k, key, kmspb.CryptoKeyVersion_ENABLED)
	if _, err := s.openSnapshot(ctx, "RestoreDatabase", name, b); err != nil {
		t.Fatalf("open with the version enabled again: %v", err)
	}
	// A backup sealed under one name does not open as another.
	if _, err := s.openSnapshot(ctx, "CopyBackup", name+"x", b); err == nil {
		t.Fatal("opened under another name")
	}
}

// TestCMEKDatabaseAccess (#1950): a customer-managed database's data calls
// are claimed and refused while its key version is disabled.
func TestCMEKDatabaseAccess(t *testing.T) {
	ctx := context.Background()
	s, k, key := newKeyedFront(t)
	db := cmekProject + "/instances/i/databases/d"
	req := httptest.NewRequest("POST", "/google.spanner.v1.Spanner/CreateSession", nil)
	req.Header.Set("Content-Type", "application/grpc")
	if s.Claims(req) {
		t.Fatal("data calls claimed with no customer-managed database")
	}
	rec, err := s.databaseKeyRecord(ctx, "CreateDatabase", db, []string{key})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.dbKeys[db] = rec
	s.mu.Unlock()
	if !s.Claims(req) {
		t.Fatal("data calls not claimed with a customer-managed database")
	}
	out := &databasepb.Database{Name: db}
	s.mu.Lock()
	s.overlayKeys(out)
	s.mu.Unlock()
	if out.GetEncryptionConfig().GetKmsKeyName() != key || len(out.GetEncryptionInfo()) != 1 ||
		out.GetEncryptionInfo()[0].GetKmsKeyVersion() != key+"/cryptoKeyVersions/1" {
		t.Fatalf("database %v", out)
	}
	if err := s.databaseAccessible(ctx, db); err != nil {
		t.Fatal(err)
	}
	setSpannerKeyVersion(t, k, key, kmspb.CryptoKeyVersion_DISABLED)
	_, err = cacheData{s: s}.CreateSession(ctx, &spannerpb.CreateSessionRequest{Database: db})
	wantKeyRefusal(t, "CreateSession with the version disabled", err)
	_, err = cacheData{s: s}.ExecuteSql(ctx, &spannerpb.ExecuteSqlRequest{Session: db + "/sessions/x", Sql: "SELECT 1"})
	wantKeyRefusal(t, "ExecuteSql with the version disabled", err)
	setSpannerKeyVersion(t, k, key, kmspb.CryptoKeyVersion_ENABLED)
	if _, err := k.DestroyCryptoKeyVersion(ctx, &kmspb.DestroyCryptoKeyVersionRequest{Name: key + "/cryptoKeyVersions/1"}); err != nil {
		t.Fatal(err)
	}
	wantKeyRefusal(t, "with the version scheduled for destruction", s.databaseAccessible(ctx, db))
}

func TestCMEKResolveKeys(t *testing.T) {
	key := cmekProject + "/locations/us-central1/keyRings/r/cryptoKeys/k"
	inherited := []string{"inherited"}
	for _, c := range []struct {
		typ  string
		keys []string
		want []string
		code codes.Code
	}{
		{"USE_DATABASE_ENCRYPTION", nil, inherited, codes.OK},
		{"ENCRYPTION_TYPE_UNSPECIFIED", nil, inherited, codes.OK},
		{"GOOGLE_DEFAULT_ENCRYPTION", nil, nil, codes.OK},
		{"CUSTOMER_MANAGED_ENCRYPTION", []string{key}, []string{key}, codes.OK},
		{"CUSTOMER_MANAGED_ENCRYPTION", nil, nil, codes.InvalidArgument},
		{"GOOGLE_DEFAULT_ENCRYPTION", []string{key}, nil, codes.InvalidArgument},
		{"USE_CONFIG_DEFAULT_OR_BACKUP_ENCRYPTION", []string{key}, nil, codes.InvalidArgument},
	} {
		got, err := resolveKeys("CreateBackup", c.typ, c.keys, inherited)
		if status.Code(err) != c.code || strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s %v: %v %v", c.typ, c.keys, got, err)
		}
	}
	if _, err := keyNames("CreateBackup", key, []string{key}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("both kms_key_name and kms_key_names: %v", err)
	}
	if err := checkKeyNames("CreateBackup", []string{"nope"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a bad key name: %v", err)
	}
}

// TestCMEKWithoutKMS: a key on an instance without Cloud KMS is refused.
func TestCMEKWithoutKMS(t *testing.T) {
	s, err := New("127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.wrapKeys(context.Background(), "CreateDatabase", cmekProject+"/instances/i/databases/d",
		[]string{cmekProject + "/locations/us-central1/keyRings/r/cryptoKeys/k"}, nil)
	wantKeyRefusal(t, "no Cloud KMS", err)
}
