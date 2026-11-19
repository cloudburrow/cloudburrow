//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/datastore"
	"cloud.google.com/go/firestore"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// Datastore mode databases (#1880) and customer-managed keys (#1881). The
// Firestore Admin API, in the Firestore pod's call front, records both;
// the Datastore pod's call front serves a Datastore mode database's
// entities by database ID once the Admin API lets it, and both fronts
// refuse a database whose key the host's Cloud KMS says is not enabled.

// uniqueDB is a database ID no other run has used.
func uniqueDB(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()%1e10)
}

func createDB(t *testing.T, h *Harness, id string, db *adminpb.Database) (*adminpb.Database, error) {
	t.Helper()
	c := firestoreAdmin(t, h)
	ctx := h.Context()
	op, err := c.CreateDatabase(ctx, &adminpb.CreateDatabaseRequest{Parent: "projects/" + h.Project(), DatabaseId: id, Database: db})
	if err != nil {
		return nil, err
	}
	out, err := op.Wait(ctx)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() {
		if dop, err := c.DeleteDatabase(context.Background(), &adminpb.DeleteDatabaseRequest{Name: out.GetName()}); err == nil {
			_, _ = dop.Wait(context.Background())
		}
	})
	return out, nil
}

func wantRefusal(t *testing.T, what string, err error, code codes.Code, msg string) {
	t.Helper()
	if status.Code(err) != code || !strings.Contains(status.Convert(err).Message(), msg) {
		t.Errorf("%s: %v, want %v %q", what, err, code, msg)
	}
}

// TestFirestoreDatastoreModeDatabases (#1880): CreateDatabase makes a
// Datastore mode database, which Get and List report with its type; the
// official Datastore client writes and reads it by database ID, in a
// namespace too, apart from (default); the Firestore client is refused
// with Google's FAILED_PRECONDITION, and the Datastore client on a Native
// mode database likewise; DeleteDatabase deletes its entities.
//
// covers: google.firestore.admin.v1.FirestoreAdmin/CreateDatabase, google.firestore.admin.v1.FirestoreAdmin/GetDatabase, google.firestore.admin.v1.FirestoreAdmin/ListDatabases, google.firestore.admin.v1.FirestoreAdmin/DeleteDatabase
func TestFirestoreDatastoreModeDatabases(t *testing.T) {
	h := New(t)
	c := firestoreAdmin(t, h)
	dsAddr := h.Endpoint(EnvDatastore)
	fsAddr := h.Endpoint(EnvFirestore)
	ctx := h.Context()
	project := h.Project()

	if _, err := createDB(t, h, uniqueDB("dsent"), &adminpb.Database{LocationId: "nam5", Type: adminpb.Database_DATASTORE_MODE,
		DatabaseEdition: adminpb.Database_ENTERPRISE}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("an Enterprise edition Datastore mode database: %v, want InvalidArgument", err)
	}
	id := uniqueDB("dsmode")
	db, err := createDB(t, h, id, &adminpb.Database{LocationId: "nam5", Type: adminpb.Database_DATASTORE_MODE})
	if err != nil {
		t.Fatalf("CreateDatabase DATASTORE_MODE: %v", err)
	}
	if db.GetType() != adminpb.Database_DATASTORE_MODE || db.GetDatabaseEdition() != adminpb.Database_STANDARD {
		t.Errorf("created = %v", db)
	}
	if got, err := c.GetDatabase(ctx, &adminpb.GetDatabaseRequest{Name: db.GetName()}); err != nil || got.GetType() != adminpb.Database_DATASTORE_MODE {
		t.Errorf("GetDatabase = %v, %v", got, err)
	}
	list, err := c.ListDatabases(ctx, &adminpb.ListDatabasesRequest{Parent: "projects/" + project})
	if err != nil {
		t.Fatal(err)
	}
	listed := false
	for _, d := range list.GetDatabases() {
		listed = listed || (d.GetName() == db.GetName() && d.GetType() == adminpb.Database_DATASTORE_MODE)
	}
	if !listed {
		t.Errorf("ListDatabases does not list %s as DATASTORE_MODE: %v", db.GetName(), list.GetDatabases())
	}

	t.Setenv("DATASTORE_EMULATOR_HOST", dsAddr)
	t.Setenv("FIRESTORE_EMULATOR_HOST", fsAddr)
	dc, err := datastore.NewClientWithDatabase(ctx, project, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dc.Close() })
	type entity struct{ V string }
	key := datastore.NameKey("Order", "o1", nil)
	nsKey := datastore.NameKey("Order", "o2", nil)
	nsKey.Namespace = "tenant"
	if _, err := dc.PutMulti(ctx, []*datastore.Key{key, nsKey}, []*entity{{"named"}, {"namespaced"}}); err != nil {
		t.Fatalf("Put in the Datastore mode database: %v", err)
	}
	var got entity
	if err := dc.Get(ctx, nsKey, &got); err != nil || got.V != "namespaced" {
		t.Errorf("Get in the Datastore mode database = %v, %v", got, err)
	}
	def, err := datastore.NewClient(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = def.Close() })
	if err := def.Get(ctx, key, &got); !errors.Is(err, datastore.ErrNoSuchEntity) {
		t.Errorf("(default) sees the named database's entity: %v", err)
	}

	fc, err := firestore.NewClientWithDatabase(ctx, project, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fc.Close() })
	_, err = fc.Collection("orders").Doc("o1").Get(ctx)
	wantRefusal(t, "the Firestore client on a Datastore mode database", err, codes.FailedPrecondition,
		"The Cloud Firestore API is not available for Firestore in Datastore Mode database "+db.GetName())
	_, err = fc.Collection("orders").Doc("o1").Set(ctx, map[string]any{"v": 1})
	wantRefusal(t, "a Firestore write on a Datastore mode database", err, codes.FailedPrecondition, "Datastore Mode")

	nativeID := uniqueDB("native")
	native, err := createDB(t, h, nativeID, &adminpb.Database{LocationId: "nam5", Type: adminpb.Database_FIRESTORE_NATIVE})
	if err != nil {
		t.Fatal(err)
	}
	nc, err := datastore.NewClientWithDatabase(ctx, project, nativeID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nc.Close() })
	err = nc.Get(ctx, key, &got)
	wantRefusal(t, "the Datastore client on a Native mode database", err, codes.FailedPrecondition,
		"The Cloud Datastore API is not available for Firestore in Native mode database "+native.GetName())

	// DeleteDatabase deletes the entities, in every namespace.
	dop, err := c.DeleteDatabase(ctx, &adminpb.DeleteDatabaseRequest{Name: db.GetName()})
	if err != nil {
		t.Fatalf("DeleteDatabase: %v", err)
	}
	if _, err := dop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	for _, k := range []*datastore.Key{key, nsKey} {
		if err := dc.Get(ctx, k, &got); !errors.Is(err, datastore.ErrNoSuchEntity) {
			t.Errorf("after DeleteDatabase, Get %v = %v, want no such entity", k, err)
		}
	}
}

// firestoreKMS is the Cloud KMS the shard serves for customer-managed keys,
// and a key ring of the project in location.
func firestoreKMS(t *testing.T, h *Harness, location string) (*kms.KeyManagementClient, string) {
	t.Helper()
	// Cloud KMS as the shard's CMEK_KMS (#1568), so the KMS suite stays in served.
	shardEndpoint(h, envCMEKKMS, EnvKMS)
	kc := kmsClients(t, h)["grpc"]
	ring, err := kc.CreateKeyRing(h.Context(), &kmspb.CreateKeyRingRequest{Parent: "projects/" + h.Project() + "/locations/" + location,
		KeyRingId: fmt.Sprintf("firestore-%d", time.Now().UnixNano()%1e9)})
	if err != nil {
		t.Fatalf("CreateKeyRing: %v", err)
	}
	return kc, ring.GetName()
}

func newKey(t *testing.T, h *Harness, kc *kms.KeyManagementClient, ring, id string, purpose kmspb.CryptoKey_CryptoKeyPurpose) *kmspb.CryptoKey {
	t.Helper()
	k := &kmspb.CryptoKey{Purpose: purpose}
	if purpose == kmspb.CryptoKey_ASYMMETRIC_SIGN {
		k.VersionTemplate = &kmspb.CryptoKeyVersionTemplate{Algorithm: kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256}
	}
	key, err := kc.CreateCryptoKey(h.Context(), &kmspb.CreateCryptoKeyRequest{Parent: ring, CryptoKeyId: id, CryptoKey: k})
	if err != nil {
		t.Fatalf("CreateCryptoKey %s: %v", id, err)
	}
	return key
}

func setVersionState(t *testing.T, h *Harness, kc *kms.KeyManagementClient, version string, st kmspb.CryptoKeyVersion_CryptoKeyVersionState) {
	t.Helper()
	if _, err := kc.UpdateCryptoKeyVersion(h.Context(), &kmspb.UpdateCryptoKeyVersionRequest{
		CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: version, State: st},
		UpdateMask:       &fieldmaskpb.FieldMask{Paths: []string{"state"}}}); err != nil {
		t.Fatalf("UpdateCryptoKeyVersion %s %v: %v", version, st, err)
	}
}

// keyEventually retries f until it is nil or 15 seconds pass: the fronts ask
// Cloud KMS again every second or so.
func keyEventually(f func() error) error {
	deadline := time.Now().Add(15 * time.Second)
	for {
		err := f()
		if err == nil || time.Now().After(deadline) {
			return err
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// TestFirestoreCMEK (#1881): cmek_config.kms_key_name is checked against
// the instance's Cloud KMS (the key must exist, be in the database's
// location, us for nam5, be ENCRYPT_DECRYPT and have its primary
// enabled), the database reports the version in use as active_key_version,
// and disabling or destroying it makes the data calls of a Native mode
// database (the Firestore client) and of a Datastore mode one (the
// Datastore client) fail with Google's FAILED_PRECONDITION until it is
// enabled again.
//
// covers: google.firestore.admin.v1.FirestoreAdmin/CreateDatabase, google.firestore.admin.v1.FirestoreAdmin/GetDatabase
func TestFirestoreCMEK(t *testing.T) {
	h := New(t)
	c := firestoreAdmin(t, h)
	dsAddr := h.Endpoint(EnvDatastore)
	fsAddr := h.Endpoint(EnvFirestore)
	kc, ring := firestoreKMS(t, h, "us")
	_, regionRing := firestoreKMS(t, h, "us-east1")
	ctx := h.Context()
	project := h.Project()

	key := newKey(t, h, kc, ring, "fs", kmspb.CryptoKey_ENCRYPT_DECRYPT)
	sign := newKey(t, h, kc, ring, "sign", kmspb.CryptoKey_ASYMMETRIC_SIGN)
	off := newKey(t, h, kc, ring, "off", kmspb.CryptoKey_ENCRYPT_DECRYPT)
	setVersionState(t, h, kc, off.GetPrimary().GetName(), kmspb.CryptoKeyVersion_DISABLED)
	elsewhere := newKey(t, h, kc, regionRing, "east", kmspb.CryptoKey_ENCRYPT_DECRYPT)

	native := func(k string) *adminpb.Database {
		return &adminpb.Database{LocationId: "nam5", Type: adminpb.Database_FIRESTORE_NATIVE, CmekConfig: &adminpb.Database_CmekConfig{KmsKeyName: k}}
	}
	_, err := createDB(t, h, uniqueDB("cmek-missing"), native(ring+"/cryptoKeys/missing"))
	wantRefusal(t, "a key that does not exist", err, codes.FailedPrecondition, "does not exist")
	_, err = createDB(t, h, uniqueDB("cmek-east"), native(elsewhere.GetName()))
	wantRefusal(t, "a key in another location", err, codes.InvalidArgument, "must use a key in us")
	_, err = createDB(t, h, uniqueDB("cmek-sign"), native(sign.GetName()))
	wantRefusal(t, "a signing key", err, codes.InvalidArgument, "ENCRYPT_DECRYPT")
	_, err = createDB(t, h, uniqueDB("cmek-off"), native(off.GetName()))
	wantRefusal(t, "a disabled key", err, codes.FailedPrecondition, "The customer-managed encryption key required by the requested resource is not accessible")

	id := uniqueDB("cmek")
	db, err := createDB(t, h, id, native(key.GetName()))
	if err != nil {
		t.Fatalf("CreateDatabase with a key: %v", err)
	}
	if got, err := c.GetDatabase(ctx, &adminpb.GetDatabaseRequest{Name: db.GetName()}); err != nil ||
		got.GetCmekConfig().GetKmsKeyName() != key.GetName() || len(got.GetCmekConfig().GetActiveKeyVersion()) != 1 ||
		got.GetCmekConfig().GetActiveKeyVersion()[0] != key.GetPrimary().GetName() {
		t.Fatalf("GetDatabase = %v, %v; want the key and its primary version in use", got, err)
	}
	dsID := uniqueDB("cmek-ds")
	if _, err := createDB(t, h, dsID, &adminpb.Database{LocationId: "nam5", Type: adminpb.Database_DATASTORE_MODE,
		CmekConfig: &adminpb.Database_CmekConfig{KmsKeyName: key.GetName()}}); err != nil {
		t.Fatalf("a Datastore mode database with a key: %v", err)
	}

	t.Setenv("FIRESTORE_EMULATOR_HOST", fsAddr)
	t.Setenv("DATASTORE_EMULATOR_HOST", dsAddr)
	fc, err := firestore.NewClientWithDatabase(ctx, project, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fc.Close() })
	dc, err := datastore.NewClientWithDatabase(ctx, project, dsID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dc.Close() })
	doc := fc.Collection("secrets").Doc("s1")
	dkey := datastore.NameKey("Secret", "s1", nil)
	type entity struct{ V string }
	read := func() (error, error) {
		_, ferr := doc.Get(ctx)
		var e entity
		return ferr, dc.Get(ctx, dkey, &e)
	}
	if _, err := doc.Set(ctx, map[string]any{"v": "kept"}); err != nil {
		t.Fatalf("Firestore write with the key enabled: %v", err)
	}
	if _, err := dc.Put(ctx, dkey, &entity{"kept"}); err != nil {
		t.Fatalf("Datastore write with the key enabled: %v", err)
	}

	version := key.GetPrimary().GetName()
	for _, st := range []kmspb.CryptoKeyVersion_CryptoKeyVersionState{kmspb.CryptoKeyVersion_DISABLED, kmspb.CryptoKeyVersion_DESTROY_SCHEDULED} {
		if st == kmspb.CryptoKeyVersion_DESTROY_SCHEDULED {
			if _, err := kc.DestroyCryptoKeyVersion(ctx, &kmspb.DestroyCryptoKeyVersionRequest{Name: version}); err != nil {
				t.Fatalf("DestroyCryptoKeyVersion: %v", err)
			}
		} else {
			setVersionState(t, h, kc, version, st)
		}
		err := keyEventually(func() error {
			ferr, derr := read()
			if status.Code(ferr) != codes.FailedPrecondition || status.Code(derr) != codes.FailedPrecondition {
				return fmt.Errorf("Firestore %v, Datastore %v", ferr, derr)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("with the key's version %v: %v; want both refused", st, err)
		}
		ferr, derr := read()
		wantRefusal(t, "Firestore read, version "+st.String(), ferr, codes.FailedPrecondition, "The customer-managed encryption key required by the requested resource is not accessible")
		wantRefusal(t, "Datastore read, version "+st.String(), derr, codes.FailedPrecondition, "The customer-managed encryption key required by the requested resource is not accessible")
		_, werr := doc.Set(ctx, map[string]any{"v": "refused"})
		wantRefusal(t, "Firestore write, version "+st.String(), werr, codes.FailedPrecondition, "not accessible")

		// Enabled again (restored, then enabled), the data is served.
		if st == kmspb.CryptoKeyVersion_DESTROY_SCHEDULED {
			if _, err := kc.RestoreCryptoKeyVersion(ctx, &kmspb.RestoreCryptoKeyVersionRequest{Name: version}); err != nil {
				t.Fatalf("RestoreCryptoKeyVersion: %v", err)
			}
		}
		setVersionState(t, h, kc, version, kmspb.CryptoKeyVersion_ENABLED)
		if err := keyEventually(func() error {
			ferr, derr := read()
			return errors.Join(ferr, derr)
		}); err != nil {
			t.Fatalf("with the key enabled again: %v", err)
		}
	}
	snap, err := doc.Get(ctx)
	if err != nil || snap.Data()["v"] != "kept" {
		t.Errorf("the document after the key came back = %v, %v", snap.Data(), err)
	}
}

// TestGcloudFirestoreDatastoreModeAndKMSKey (#1880, #1881), in the
// emulators shard: gcloud firestore, configured by `cloudburrow
// gcloud-setup` alone, creates a Datastore mode database with --type and
// a database with --kms-key-name, and describes both; the official
// Datastore client writes and reads the first.
//
// covers: google.firestore.admin.v1.FirestoreAdmin/CreateDatabase, google.firestore.admin.v1.FirestoreAdmin/GetDatabase
func TestGcloudFirestoreDatastoreModeAndKMSKey(t *testing.T) {
	h := New(t)
	c := firestoreAdmin(t, h) // asks for CLOUDBURROW_TEST_FIRESTORE
	dsAddr := h.Endpoint(EnvDatastore)
	kc, ring := firestoreKMS(t, h, "us")
	key := newKey(t, h, kc, ring, "gcloud", kmspb.CryptoKey_ENCRYPT_DECRYPT)
	gc := gcloudAdminSession(t, h.Project(), "firestore")
	ctx := h.Context()
	project := h.Project()
	cleanup := func(id string) {
		t.Cleanup(func() {
			if op, err := c.DeleteDatabase(context.Background(), &adminpb.DeleteDatabaseRequest{Name: "projects/" + project + "/databases/" + id}); err == nil {
				_, _ = op.Wait(context.Background())
			}
		})
	}

	dsID := uniqueDB("gcloud-ds")
	gc("firestore", "databases", "create", "--database", dsID, "--location", "nam5", "--type", "datastore-mode")
	cleanup(dsID)
	if got := strings.TrimSpace(gc("firestore", "databases", "describe", "--database", dsID, "--format=value(type)")); got != "DATASTORE_MODE" {
		t.Errorf("describe type = %q, want DATASTORE_MODE", got)
	}
	t.Setenv("DATASTORE_EMULATOR_HOST", dsAddr)
	dc, err := datastore.NewClientWithDatabase(ctx, project, dsID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dc.Close() })
	k := datastore.NameKey("Gcloud", "g1", nil)
	type entity struct{ V string }
	if _, err := dc.Put(ctx, k, &entity{"made by gcloud"}); err != nil {
		t.Fatalf("Put in gcloud's Datastore mode database: %v", err)
	}
	var got entity
	if err := dc.Get(ctx, k, &got); err != nil || got.V != "made by gcloud" {
		t.Errorf("Get = %v, %v", got, err)
	}

	encID := uniqueDB("gcloud-cmek")
	gc("firestore", "databases", "create", "--database", encID, "--location", "nam5", "--type", "firestore-native",
		"--kms-key-name", key.GetName())
	cleanup(encID)
	out := gc("firestore", "databases", "describe", "--database", encID,
		"--format=value(cmekConfig.kmsKeyName,cmekConfig.activeKeyVersion)")
	if !strings.Contains(out, key.GetName()) || !strings.Contains(out, key.GetPrimary().GetName()) {
		t.Errorf("describe cmekConfig = %q, want the key and %s", out, key.GetPrimary().GetName())
	}
}
