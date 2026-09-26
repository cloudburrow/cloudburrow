//go:build compat

package compat

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/storage"
)

// HMAC keys (#505), to docs.cloud.google.com/storage/docs/authentication/hmackeys,
// through the official Go SDK.

func deleteHMACKeys(ctx context.Context, c *storage.Client, project, email string) {
	it := c.ListHMACKeys(ctx, project, storage.ForHMACKeyServiceAccountEmail(email))
	for {
		k, err := it.Next()
		if err != nil {
			return
		}
		h := c.HMACKeyHandle(project, k.AccessID)
		_, _ = h.Update(ctx, storage.HMACKeyAttrsToUpdate{State: storage.Inactive})
		_ = h.Delete(ctx)
	}
}

// TestStorageHMACKeyLifecycle: create returns the 40-character secret once,
// a delete while ACTIVE is 400, and a key is deleted after deactivation.
// covers: storage.projects.hmacKeys.create, storage.projects.hmacKeys.get, storage.projects.hmacKeys.update, storage.projects.hmacKeys.delete, storage.projects.hmacKeys.list, storage.projects.serviceAccount.get
func TestStorageHMACKeyLifecycle(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	ctx := h.Context()
	sa := "lifecycle@" + h.Project() + ".iam.gserviceaccount.com"
	t.Cleanup(func() { deleteHMACKeys(context.Background(), c, h.Project(), sa) })
	k, err := c.CreateHMACKey(ctx, h.Project(), sa)
	if err != nil {
		t.Fatal(err)
	}
	if email, err := c.ServiceAccount(ctx, h.Project()); err != nil || !strings.HasSuffix(email, "@gs-project-accounts.iam.gserviceaccount.com") {
		t.Errorf("service account = %q, %v", email, err)
	}
	if len(k.AccessID) != 61 || len(k.Secret) != 40 || k.State != storage.Active {
		t.Errorf("created key: access ID %d characters, secret %d, state %s; want 61, 40, ACTIVE", len(k.AccessID), len(k.Secret), k.State)
	}
	hk := c.HMACKeyHandle(h.Project(), k.AccessID)
	if got, err := hk.Get(ctx); err != nil || got.Secret != "" {
		t.Errorf("get = secret %q, %v; want no secret after create", got.Secret, err)
	}
	if code, _ := apiError(hk.Delete(ctx)); code != http.StatusBadRequest {
		t.Errorf("delete while ACTIVE = %d; want 400", code)
	}
	if _, err := hk.Update(ctx, storage.HMACKeyAttrsToUpdate{State: storage.Inactive}); err != nil {
		t.Fatal(err)
	}
	if err := hk.Delete(ctx); err != nil {
		t.Fatalf("delete after deactivation: %v", err)
	}
	if got, err := hk.Get(ctx); err != nil || got.State != storage.Deleted {
		t.Errorf("after delete = %v, %v; want DELETED", got, err)
	}
}

// TestStorageHMACKeyLimit: a service account's 11th key is refused.
// covers: storage.projects.hmacKeys.create
func TestStorageHMACKeyLimit(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	ctx := h.Context()
	sa := "limit@" + h.Project() + ".iam.gserviceaccount.com"
	t.Cleanup(func() { deleteHMACKeys(context.Background(), c, h.Project(), sa) })
	for i := 0; i < 10; i++ {
		if _, err := c.CreateHMACKey(ctx, h.Project(), sa); err != nil {
			t.Fatalf("key %d: %v", i+1, err)
		}
	}
	_, err := c.CreateHMACKey(ctx, h.Project(), sa)
	if err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Errorf("the 11th key = %v; want it refused", err)
	}
}
