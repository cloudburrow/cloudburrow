package firestoreadmin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeKeys is a Cloud KMS of keys and versions.
type fakeKeys struct {
	mu       sync.Mutex
	keys     map[string]*kmspb.CryptoKey
	versions map[string]kmspb.CryptoKeyVersion_CryptoKeyVersionState
}

func (f *fakeKeys) GetCryptoKey(_ context.Context, in *kmspb.GetCryptoKeyRequest, _ ...grpc.CallOption) (*kmspb.CryptoKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k, ok := f.keys[in.GetName()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "%s not found", in.GetName())
	}
	out := &kmspb.CryptoKey{Name: k.Name, Purpose: k.Purpose}
	if k.Primary != nil {
		out.Primary = &kmspb.CryptoKeyVersion{Name: k.Primary.Name, State: f.versions[k.Primary.Name]}
	}
	return out, nil
}

func (f *fakeKeys) GetCryptoKeyVersion(_ context.Context, in *kmspb.GetCryptoKeyVersionRequest, _ ...grpc.CallOption) (*kmspb.CryptoKeyVersion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.versions[in.GetName()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "%s not found", in.GetName())
	}
	return &kmspb.CryptoKeyVersion{Name: in.GetName(), State: st}, nil
}

func (f *fakeKeys) set(v string, st kmspb.CryptoKeyVersion_CryptoKeyVersionState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.versions[v] = st
}

const (
	usKey   = "projects/p/locations/us/keyRings/r/cryptoKeys/k"
	usKeyV1 = usKey + "/cryptoKeyVersions/1"
)

func cmekAdmin(t *testing.T) (*Admin, *fakeKeys, *time.Time) {
	t.Helper()
	keys := &fakeKeys{keys: map[string]*kmspb.CryptoKey{
		usKey: {Name: usKey, Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, Primary: &kmspb.CryptoKeyVersion{Name: usKeyV1}},
		"projects/p/locations/us/keyRings/r/cryptoKeys/sign": {Name: "projects/p/locations/us/keyRings/r/cryptoKeys/sign",
			Purpose: kmspb.CryptoKey_ASYMMETRIC_SIGN},
		"projects/p/locations/us/keyRings/r/cryptoKeys/off": {Name: "projects/p/locations/us/keyRings/r/cryptoKeys/off",
			Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, Primary: &kmspb.CryptoKeyVersion{Name: "projects/p/locations/us/keyRings/r/cryptoKeys/off/cryptoKeyVersions/1"}},
	}, versions: map[string]kmspb.CryptoKeyVersion_CryptoKeyVersionState{
		usKeyV1: kmspb.CryptoKeyVersion_ENABLED,
		"projects/p/locations/us/keyRings/r/cryptoKeys/off/cryptoKeyVersions/1": kmspb.CryptoKeyVersion_DISABLED,
	}}
	a := newWith("")
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	a.useKeys(keys)
	return a, keys, &now
}

func create(a *Admin, id string, db *adminpb.Database) error {
	_, err := a.CreateDatabase(context.Background(), &adminpb.CreateDatabaseRequest{Parent: "projects/p", DatabaseId: id, Database: db})
	return err
}

func TestCMEKCreateChecksTheKey(t *testing.T) {
	a, _, _ := cmekAdmin(t)
	cases := []struct {
		key, loc string
		code     codes.Code
		msg      string
	}{
		{"not-a-key", "nam5", codes.InvalidArgument, "Invalid kms_key_name"},
		{usKey, "eur3", codes.InvalidArgument, "must use a key in europe"},
		{"projects/p/locations/us-east1/keyRings/r/cryptoKeys/k", "nam5", codes.InvalidArgument, "must use a key in us"},
		{"projects/p/locations/us/keyRings/r/cryptoKeys/missing", "nam5", codes.FailedPrecondition, "does not exist"},
		{"projects/p/locations/us/keyRings/r/cryptoKeys/sign", "nam5", codes.InvalidArgument, "ENCRYPT_DECRYPT"},
		{"projects/p/locations/us/keyRings/r/cryptoKeys/off", "nam5", codes.FailedPrecondition, keyInaccessible},
	}
	for _, c := range cases {
		err := create(a, "bad-db", &adminpb.Database{LocationId: c.loc, Type: adminpb.Database_FIRESTORE_NATIVE,
			CmekConfig: &adminpb.Database_CmekConfig{KmsKeyName: c.key}})
		if status.Code(err) != c.code || !strings.Contains(status.Convert(err).Message(), c.msg) {
			t.Errorf("%s in %s: %v, want %v %q", c.key, c.loc, err, c.code, c.msg)
		}
	}
	if err := create(a, "enc-db", &adminpb.Database{LocationId: "nam5", Type: adminpb.Database_FIRESTORE_NATIVE,
		CmekConfig: &adminpb.Database_CmekConfig{KmsKeyName: usKey}}); err != nil {
		t.Fatal(err)
	}
	db, err := a.GetDatabase(context.Background(), &adminpb.GetDatabaseRequest{Name: "projects/p/databases/enc-db"})
	if err != nil || db.GetCmekConfig().GetKmsKeyName() != usKey || len(db.GetCmekConfig().GetActiveKeyVersion()) != 1 ||
		db.GetCmekConfig().GetActiveKeyVersion()[0] != usKeyV1 {
		t.Fatalf("GetDatabase = %v, %v", db, err)
	}
	// An empty cmek_config, as gcloud sends without --kms-key-name, is
	// Google's default encryption.
	if err := create(a, "plain-db", &adminpb.Database{LocationId: "nam5", Type: adminpb.Database_FIRESTORE_NATIVE,
		CmekConfig: &adminpb.Database_CmekConfig{}}); err != nil {
		t.Errorf("an empty cmek_config: %v", err)
	}
	// Without Cloud KMS, cmek_config is refused.
	b := newWith("")
	if err := create(b, "enc-db", &adminpb.Database{LocationId: "nam5", Type: adminpb.Database_FIRESTORE_NATIVE,
		CmekConfig: &adminpb.Database_CmekConfig{KmsKeyName: usKey}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("without Cloud KMS: %v, want FailedPrecondition", err)
	}
}

func TestAccessFollowsTheKeyAndTheMode(t *testing.T) {
	a, keys, now := cmekAdmin(t)
	ctx := context.Background()
	if err := create(a, "enc-db", &adminpb.Database{LocationId: "nam5", Type: adminpb.Database_FIRESTORE_NATIVE,
		CmekConfig: &adminpb.Database_CmekConfig{KmsKeyName: usKey}}); err != nil {
		t.Fatal(err)
	}
	if err := create(a, "ds-db", &adminpb.Database{LocationId: "nam5", Type: adminpb.Database_DATASTORE_MODE,
		CmekConfig: &adminpb.Database_CmekConfig{KmsKeyName: usKey}}); err != nil {
		t.Fatal(err)
	}
	enc, ds := "projects/p/databases/enc-db", "projects/p/databases/ds-db"
	for _, c := range []struct {
		name, api string
		code      codes.Code
	}{
		{enc, APIFirestore, codes.OK},
		{enc, APIDatastore, codes.FailedPrecondition},
		{ds, APIDatastore, codes.OK},
		{ds, APIFirestore, codes.FailedPrecondition},
		// (default) is Native mode, and the Datastore emulator serves it too.
		{"projects/p/databases/(default)", APIDatastore, codes.OK},
		// A database the API has no record of is the emulators'.
		{"projects/p/databases/unknown", APIDatastore, codes.OK},
	} {
		if err := a.Access(ctx, c.name, c.api); status.Code(err) != c.code {
			t.Errorf("Access(%s, %s) = %v, want %v", c.name, c.api, err, c.code)
		}
	}
	if err := a.Access(ctx, ds, APIFirestore); !strings.Contains(status.Convert(err).Message(), "The Cloud Firestore API is not available for Firestore in Datastore Mode database "+ds) {
		t.Errorf("Firestore API on Datastore mode: %v", err)
	}

	keys.set(usKeyV1, kmspb.CryptoKeyVersion_DISABLED)
	// Within keyCheckInterval the last verdict stands.
	if err := a.Access(ctx, enc, APIFirestore); err != nil {
		t.Errorf("before the key is asked again: %v", err)
	}
	*now = now.Add(2 * keyCheckInterval)
	for _, c := range []struct{ name, api string }{{enc, APIFirestore}, {ds, APIDatastore}} {
		err := a.Access(ctx, c.name, c.api)
		if status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != keyInaccessible {
			t.Errorf("disabled key: Access(%s) = %v", c.name, err)
		}
	}
	keys.set(usKeyV1, kmspb.CryptoKeyVersion_DESTROYED)
	*now = now.Add(2 * keyCheckInterval)
	if err := a.Access(ctx, enc, APIFirestore); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("destroyed key: %v", err)
	}
	keys.set(usKeyV1, kmspb.CryptoKeyVersion_ENABLED)
	*now = now.Add(2 * keyCheckInterval)
	if err := a.Access(ctx, enc, APIFirestore); err != nil {
		t.Errorf("enabled again: %v", err)
	}

	// A rotated key's new primary is in use too.
	keys.mu.Lock()
	keys.keys[usKey].Primary = &kmspb.CryptoKeyVersion{Name: usKey + "/cryptoKeyVersions/2"}
	keys.versions[usKey+"/cryptoKeyVersions/2"] = kmspb.CryptoKeyVersion_ENABLED
	keys.mu.Unlock()
	*now = now.Add(2 * keyCheckInterval)
	if err := a.Access(ctx, enc, APIFirestore); err != nil {
		t.Fatal(err)
	}
	db, _ := a.GetDatabase(ctx, &adminpb.GetDatabaseRequest{Name: enc})
	if got := db.GetCmekConfig().GetActiveKeyVersion(); len(got) != 2 || got[1] != usKey+"/cryptoKeyVersions/2" {
		t.Errorf("active versions after rotation = %v", got)
	}
}

func TestAccessGateAndPath(t *testing.T) {
	a, keys, now := cmekAdmin(t)
	if err := create(a, "ds-db", &adminpb.Database{LocationId: "nam5", Type: adminpb.Database_DATASTORE_MODE}); err != nil {
		t.Fatal(err)
	}
	if err := create(a, "enc-db", &adminpb.Database{LocationId: "nam5", Type: adminpb.Database_FIRESTORE_NATIVE,
		CmekConfig: &adminpb.Database_CmekConfig{KmsKeyName: usKey}}); err != nil {
		t.Fatal(err)
	}
	g := a.AccessGate()
	rest := httptest.NewRequest(http.MethodGet, "/v1/projects/p/databases/ds-db/documents/c/d", nil)
	if !g.Claims(rest) {
		t.Fatal("a REST call on a Datastore mode database was not claimed")
	}
	w := httptest.NewRecorder()
	g.ServeHTTP(w, rest)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "FAILED_PRECONDITION") {
		t.Errorf("REST refusal = %d %s", w.Code, w.Body)
	}
	call := httptest.NewRequest(http.MethodPost, "/google.firestore.v1.Firestore/GetDocument", nil)
	call.Header.Set("Content-Type", "application/grpc")
	call.Header.Set("Google-Cloud-Resource-Prefix", "projects/p/databases/enc-db")
	if g.Claims(call) {
		t.Fatal("a call on an accessible database was claimed")
	}
	keys.set(usKeyV1, kmspb.CryptoKeyVersion_DISABLED)
	*now = now.Add(2 * keyCheckInterval)
	if !g.Claims(call) {
		t.Fatal("a gRPC call on a database whose key is disabled was not claimed")
	}
	w = httptest.NewRecorder()
	g.ServeHTTP(w, call)
	if w.Header().Get("Grpc-Status") != "9" {
		t.Errorf("gRPC refusal: grpc-status %q", w.Header().Get("Grpc-Status"))
	}

	// AccessPath answers the Datastore front.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest(http.MethodGet, AccessPath+"?api=datastore&name=projects/p/databases/enc-db", nil))
	var ans AccessAnswer
	if err := json.Unmarshal(w.Body.Bytes(), &ans); err != nil || ans.Code != codes.FailedPrecondition {
		t.Errorf("AccessPath = %s, %v", w.Body, err)
	}
	w = httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest(http.MethodGet, AccessPath+"?api=datastore&name=projects/p/databases/ds-db", nil))
	if err := json.Unmarshal(w.Body.Bytes(), &ans); err != nil || ans.Code != codes.OK {
		t.Errorf("AccessPath for the Datastore mode database = %s, %v", w.Body, err)
	}
}

func TestDatastoreModeRefusesDocumentCopies(t *testing.T) {
	a, _, _ := cmekAdmin(t)
	if err := create(a, "ds-db", &adminpb.Database{LocationId: "nam5", Type: adminpb.Database_DATASTORE_MODE}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := a.ExportDocuments(ctx, &adminpb.ExportDocumentsRequest{Name: "projects/p/databases/ds-db", OutputUriPrefix: "gs://b/x"}); status.Code(err) != codes.Unimplemented {
		t.Errorf("ExportDocuments: %v, want Unimplemented", err)
	}
	if _, err := a.CreateBackupSchedule(ctx, &adminpb.CreateBackupScheduleRequest{Parent: "projects/p/databases/ds-db",
		BackupSchedule: &adminpb.BackupSchedule{}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("CreateBackupSchedule: %v, want Unimplemented", err)
	}
	// Delete has nothing to clear without the Datastore front.
	if _, err := a.DeleteDatabase(ctx, &adminpb.DeleteDatabaseRequest{Name: "projects/p/databases/ds-db"}); err != nil {
		t.Errorf("DeleteDatabase: %v", err)
	}
}
