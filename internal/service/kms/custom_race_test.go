package kms

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"cloud.google.com/go/orgpolicy/apiv2/orgpolicypb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/orgpolicy"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// hookCustoms is a custom-constraint source that runs hook, if set, each
// time a check asks it for constraints: while the check runs.
type hookCustoms struct {
	list []orgpolicy.CustomConstraint
	hook func()
}

func (hookCustoms) Effective(context.Context, string, string) (orgpolicy.Effective, error) {
	return orgpolicy.Effective{}, nil
}

func (h hookCustoms) Customs(context.Context, string, string, string) ([]orgpolicy.CustomConstraint, error) {
	if h.hook != nil {
		h.hook()
	}
	return h.list, nil
}

func allowEncrypt() []orgpolicy.CustomConstraint {
	return []orgpolicy.CustomConstraint{{Name: "custom.purpose", Condition: "resource.purpose == 'ENCRYPT_DECRYPT'",
		ActionType: "ALLOW", Description: "Encryption keys only.",
		Effective: orgpolicy.AsResponse(orgpolicy.Effective{Spec: &orgpolicypb.PolicySpec{Rules: []*orgpolicypb.PolicySpec_PolicyRule{
			{Kind: &orgpolicypb.PolicySpec_PolicyRule_Enforce{Enforce: true}}}}})}}
}

// TestCustomCheckOutsideLock (#1902): the custom-constraint check of an
// update runs with the store lock released, so a check that calls back
// into Cloud KMS (here: deleting the key being updated) neither blocks
// nor deadlocks, and the update, seeing the key gone, is NOT_FOUND rather
// than writing the deleted key back.
func TestCustomCheckOutsideLock(t *testing.T) {
	srv := NewServer(store.NewMemory())
	ctx := context.Background()
	if _, err := srv.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "lock"}); err != nil {
		t.Fatal(err)
	}
	ring := loc + "/keyRings/lock"
	k, err := srv.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring, CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}, SkipInitialVersionCreation: true})
	if err != nil {
		t.Fatal(err)
	}
	var deleted atomic.Bool
	orgpolicy.Use(hookCustoms{list: allowEncrypt(), hook: func() {
		if deleted.CompareAndSwap(false, true) {
			if _, err := srv.DeleteCryptoKey(ctx, &kmspb.DeleteCryptoKeyRequest{Name: k.GetName()}); err != nil {
				t.Errorf("DeleteCryptoKey from within the check: %v", err)
			}
		}
	}})
	defer orgpolicy.Use(nil)
	done := make(chan error, 1)
	go func() {
		_, err := srv.UpdateCryptoKey(ctx, &kmspb.UpdateCryptoKeyRequest{
			CryptoKey:  &kmspb.CryptoKey{Name: k.GetName(), Labels: map[string]string{"a": "b"}},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}})
		done <- err
	}()
	select {
	case err := <-done:
		if status.Code(err) != codes.NotFound {
			t.Errorf("an update whose key was deleted during its check = %v, want NOT_FOUND", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("UpdateCryptoKey deadlocked against a delete made during its custom-constraint check")
	}
	if _, err := srv.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: k.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("the deleted key = %v, want NOT_FOUND: the update wrote it back", err)
	}
}

// TestCustomCheckConcurrentUpdateDelete (#1902, run with -race): updates
// and deletes of the same keys, interleaved, each end OK or NOT_FOUND, and
// a key once deleted is never brought back by an update whose check
// passed before the delete.
func TestCustomCheckConcurrentUpdateDelete(t *testing.T) {
	srv := NewServer(store.NewMemory())
	ctx := context.Background()
	if _, err := srv.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "race"}); err != nil {
		t.Fatal(err)
	}
	ring := loc + "/keyRings/race"
	orgpolicy.Use(hookCustoms{list: allowEncrypt(), hook: func() { time.Sleep(time.Millisecond) }})
	defer orgpolicy.Use(nil)
	const keys = 6
	var wg sync.WaitGroup
	for i := range keys {
		id := fmt.Sprintf("k%d", i)
		name := ring + "/cryptoKeys/" + id
		if _, err := srv.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring, CryptoKeyId: id,
			CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}, SkipInitialVersionCreation: true}); err != nil {
			t.Fatal(err)
		}
		for w := range 3 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for n := range 5 {
					_, err := srv.UpdateCryptoKey(ctx, &kmspb.UpdateCryptoKeyRequest{
						CryptoKey:  &kmspb.CryptoKey{Name: name, Labels: map[string]string{"w": fmt.Sprint(w, "-", n)}},
						UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}})
					if c := status.Code(err); c != codes.OK && c != codes.NotFound {
						t.Errorf("UpdateCryptoKey %s = %v, want OK or NOT_FOUND", name, err)
					}
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(2 * time.Millisecond)
			if _, err := srv.DeleteCryptoKey(ctx, &kmspb.DeleteCryptoKeyRequest{Name: name}); err != nil {
				t.Errorf("DeleteCryptoKey %s: %v", name, err)
			}
		}()
	}
	wg.Wait()
	for i := range keys {
		name := fmt.Sprintf("%s/cryptoKeys/k%d", ring, i)
		if _, err := srv.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: name}); status.Code(err) != codes.NotFound {
			t.Errorf("%s after its delete = %v, want NOT_FOUND", name, err)
		}
	}
}
