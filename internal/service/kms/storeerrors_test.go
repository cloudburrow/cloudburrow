package kms

import (
	"context"
	"errors"
	"strings"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// failingStore answers every Get with an error that is not "not found", as a
// timed-out or unreachable backend does.
type failingStore struct{ store.Store }

var errBackend = errors.New("backend unavailable: connection refused")

func (failingStore) Get(string) ([]byte, error) { return nil, errBackend }

// A store failure is INTERNAL, never NOT_FOUND or success (#389). Before, a
// failed read looked like absence, so CreateKeyRing succeeded over a ring it
// could not read.
func TestAStoreFailureIsInternalNotNotFound(t *testing.T) {
	c := clientFor(t, failingStore{store.NewMemory()})
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"CreateKeyRing": func() error {
			_, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "r"})
			return err
		},
		"GetKeyRing": func() error {
			_, err := c.GetKeyRing(ctx, &kmspb.GetKeyRingRequest{Name: loc + "/keyRings/r"})
			return err
		},
		"GetCryptoKey": func() error {
			_, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: loc + "/keyRings/r/cryptoKeys/k"})
			return err
		},
		"GetCryptoKeyVersion": func() error {
			_, err := c.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: loc + "/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1"})
			return err
		},
	} {
		err := call()
		if status.Code(err) != codes.Internal {
			t.Errorf("%s = %v, want INTERNAL", name, err)
		}
		if err != nil && strings.Contains(err.Error(), "connection refused") {
			t.Errorf("%s leaks the backend's error text to the caller: %v", name, err)
		}
	}
}

// A corrupt record is INTERNAL from Get and from every List that includes
// it, for rings, keys and versions: a List must not come back short.
func TestACorruptRecordIsInternalFromGetAndList(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"ring", "key", "version"} {
		db := store.NewMemory()
		c := clientFor(t, db)
		ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "r-" + kind})
		if err != nil {
			t.Fatal(err)
		}
		key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
			CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
		if err != nil {
			t.Fatal(err)
		}
		// The record body holds key material; it must never reach an error.
		const secret = "super-secret-key-material"
		var target, get string
		var list func() error
		switch kind {
		case "ring":
			target, get = dbKey(ringPrefix, ring.GetName()), "ring"
			list = func() error {
				_, err := c.ListKeyRings(ctx, &kmspb.ListKeyRingsRequest{Parent: loc}).Next()
				return err
			}
		case "key":
			target, get = dbKey(keyPrefix, key.GetName()), "key"
			list = func() error {
				_, err := c.ListCryptoKeys(ctx, &kmspb.ListCryptoKeysRequest{Parent: ring.GetName()}).Next()
				return err
			}
		case "version":
			target, get = dbKey(versionPrefix, key.GetName()+"/cryptoKeyVersions/1"), "version"
			list = func() error {
				_, err := c.ListCryptoKeyVersions(ctx, &kmspb.ListCryptoKeyVersionsRequest{Parent: key.GetName()}).Next()
				return err
			}
		}
		if err := db.Put(target, []byte(`{"name": "`+secret+`", broken`)); err != nil {
			t.Fatal(err)
		}
		var gerr error
		switch get {
		case "ring":
			_, gerr = c.GetKeyRing(ctx, &kmspb.GetKeyRingRequest{Name: ring.GetName()})
		case "key":
			_, gerr = c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key.GetName()})
		case "version":
			_, gerr = c.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: key.GetName() + "/cryptoKeyVersions/1"})
		}
		lerr := list()
		for what, err := range map[string]error{"Get": gerr, "List": lerr} {
			if status.Code(err) != codes.Internal || errors.Is(err, iterator.Done) {
				t.Errorf("%s %s over a corrupt record = %v, want INTERNAL", kind, what, err)
			}
			if err != nil && strings.Contains(err.Error(), secret) {
				t.Errorf("%s %s echoes the record body: %v", kind, what, err)
			}
		}
	}
}

// A key whose primary version cannot be read is INTERNAL, not a key with no
// primary.
func TestAKeyWithAnUnreadablePrimaryIsInternal(t *testing.T) {
	ctx := context.Background()
	db := store.NewMemory()
	c := clientFor(t, db)
	ring, _ := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "p"})
	key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Delete(dbKey(versionPrefix, key.GetName()+"/cryptoKeyVersions/1")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key.GetName()}); status.Code(err) != codes.Internal {
		t.Errorf("GetCryptoKey with its primary gone = %v, want INTERNAL", err)
	}
}

// A missing, well-formed resource is still NOT_FOUND.
func TestAMissingResourceIsStillNotFound(t *testing.T) {
	c := client(t)
	if _, err := c.GetKeyRing(context.Background(), &kmspb.GetKeyRingRequest{Name: loc + "/keyRings/none"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetKeyRing of a missing ring = %v, want NOT_FOUND", err)
	}
}

type fakeRunner struct {
	out string
	err error
}

func (f fakeRunner) Run(context.Context, string, ...string) (string, error) { return f.out, f.err }

// KubeStore maps only the API server's NotFound to store.ErrNotFound; a
// connection failure or a missing kubeconfig stays an error.
func TestKubeStoreMapsOnlyNotFoundToAbsence(t *testing.T) {
	for msg, wantNotFound := range map[string]bool{
		`kubectl: exit status 1: Error from server (NotFound): secrets "cb-kms-x" not found`:                            true,
		"kubectl: exit status 1: Unable to connect to the server: dial tcp 127.0.0.1:6443: connect: connection refused": false,
		`kubectl: exit status 1: error: stat /nope/kubeconfig: no such file or directory`:                               false,
		`kubectl: exit status 1: Error from server (Forbidden): secrets "cb-kms-x" is forbidden`:                        false,
		`kubectl: exit status 1: error: context "missing" not found`:                                                    false,
	} {
		_, err := NewKubeStore(fakeRunner{err: errors.New(msg)}, "cloudburrow", "i").Get("key")
		if got := errors.Is(err, store.ErrNotFound); got != wantNotFound {
			t.Errorf("%q: ErrNotFound = %v, want %v (err %v)", msg, got, wantNotFound, err)
		}
		if err == nil {
			t.Errorf("%q: no error", msg)
		}
	}
}
