package kms

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

func iamFixture(t *testing.T, db store.Store) (*Server, *iamServer, string, string) {
	t.Helper()
	srv := NewServer(db)
	ctx := context.Background()
	ring, err := srv.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "iam"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := srv.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	return srv, &iamServer{s: srv}, ring.GetName(), key.GetName()
}

func binding(member string) *iampb.Policy {
	return &iampb.Policy{Bindings: []*iampb.Binding{{Role: "roles/cloudkms.cryptoKeyDecrypter", Members: []string{member}}}}
}

// Of two SetIamPolicy calls carrying the same etag, exactly one wins (#428).
func TestConcurrentSetIamPolicyOneWins(t *testing.T) {
	_, iam, _, key := iamFixture(t, store.NewMemory())
	ctx := context.Background()
	cur, err := iam.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: key})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := binding("user:racer@example.com")
			p.Etag = cur.GetEtag()
			_, errs[i] = iam.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: key, Policy: p})
		}(i)
	}
	wg.Wait()
	ok, aborted := 0, 0
	for _, err := range errs {
		switch status.Code(err) {
		case codes.OK:
			ok++
		case codes.Aborted:
			aborted++
		}
	}
	if ok != 1 || aborted != 1 {
		t.Errorf("results %v; want exactly one success and one ABORTED", errs)
	}
}

// A policy survives reopening a durable store, and other updates to the
// record keep it.
func TestIamPolicySurvivesReopenAndOtherUpdates(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "kms")
	db, err := store.OpenDurable(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv, iam, ring, key := iamFixture(t, db)
	ctx := context.Background()
	for _, res := range []string{ring, key} {
		if _, err := iam.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: res, Policy: binding("user:a@example.com")}); err != nil {
			t.Fatal(err)
		}
	}
	// An update, a new version and a primary move rewrite the key record.
	if _, err := srv.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: key}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.UpdateCryptoKeyPrimaryVersion(ctx, &kmspb.UpdateCryptoKeyPrimaryVersionRequest{Name: key, CryptoKeyVersionId: "2"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.OpenDurable(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	iam = &iamServer{s: NewServer(reopened)}
	for _, res := range []string{ring, key} {
		p, err := iam.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: res})
		if err != nil || len(p.GetBindings()) != 1 || p.GetBindings()[0].GetMembers()[0] != "user:a@example.com" {
			t.Errorf("%s after reopening = %v, %v", res, p, err)
		}
	}
}

// A record written before policies were stored reads as the empty policy.
func TestAnOldRingRecordHasTheEmptyPolicy(t *testing.T) {
	mem := store.NewMemory()
	name := loc + "/keyRings/old"
	if err := mem.Put(dbKey(ringPrefix, name), []byte(`{"name":"`+name+`","created":"2026-01-01T00:00:00Z"}`)); err != nil {
		t.Fatal(err)
	}
	p, err := (&iamServer{s: NewServer(mem)}).GetIamPolicy(context.Background(), &iampb.GetIamPolicyRequest{Resource: name})
	if err != nil || len(p.GetBindings()) != 0 || len(p.GetEtag()) == 0 {
		t.Errorf("an old ring's policy = %v, %v; want the empty policy with an etag", p, err)
	}
}

// failingGets fails every read, as an unreachable API server would.
type failingGets struct{ store.Store }

func (failingGets) Get(string) ([]byte, error) { return nil, errors.New("connection refused") }

// A failed read is never "no policy" or NOT_FOUND (#389), and routing is by
// resource type.
func TestIamRouting(t *testing.T) {
	iam := &iamServer{s: NewServer(failingGets{store.NewMemory()})}
	ctx := context.Background()
	ring := loc + "/keyRings/r"
	if _, err := iam.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: ring}); status.Code(err) != codes.Internal {
		t.Errorf("GetIamPolicy over a failing store = %v, want INTERNAL", err)
	}
	if _, err := iam.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: ring, Permissions: []string{"p"}}); status.Code(err) != codes.Internal {
		t.Errorf("TestIamPermissions over a failing store = %v, want INTERNAL", err)
	}
	_, iam, _, _ = iamFixture(t, store.NewMemory())
	for res, want := range map[string]codes.Code{
		"":                        codes.InvalidArgument,
		ring + "/importJobs/j":    codes.Unimplemented,
		loc + "/ekmConfig":        codes.Unimplemented,
		loc + "/ekmConnections/c": codes.Unimplemented,
		ring + "/cryptoKeys/k/cryptoKeyVersions/1": codes.InvalidArgument,
		"projects/p/locations/global/keyRings/r":   codes.InvalidArgument, // project "p" is malformed
		loc + "/keyRings/absent":                   codes.NotFound,
	} {
		if _, err := iam.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: res}); status.Code(err) != want {
			t.Errorf("GetIamPolicy(%q) = %v, want %s", res, err, want)
		}
	}
	r, err := iam.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: loc + "/keyRings/absent", Permissions: []string{"cloudkms.keyRings.get"}})
	if err != nil || len(r.GetPermissions()) != 0 {
		t.Errorf("TestIamPermissions on a missing ring = %v, %v; want an empty set", r, err)
	}
}
