package storage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gcs "cloud.google.com/go/storage"

	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// Create, deactivate, delete; a delete while ACTIVE is 400 (#505).
func TestStorageHMACKeyLifecycleInProcess(t *testing.T) {
	c, _ := sdk(t)
	ctx := context.Background()
	const sa = "app@demo-project.iam.gserviceaccount.com"
	k, err := c.CreateHMACKey(ctx, "demo-project", sa)
	if err != nil {
		t.Fatal(err)
	}
	if len(k.AccessID) != 61 || !strings.HasPrefix(k.AccessID, "GOOG") || len(k.Secret) != 40 || k.State != gcs.Active || k.ServiceAccountEmail != sa {
		t.Errorf("created key = %+v", k)
	}
	h := c.HMACKeyHandle("demo-project", k.AccessID)
	if got, err := h.Get(ctx); err != nil || got.Secret != "" || got.State != gcs.Active {
		t.Errorf("get = %+v, %v; want the metadata without its secret", got, err)
	}
	if code, _ := apiCode(h.Delete(ctx)); code != http.StatusBadRequest {
		t.Errorf("delete while ACTIVE = %d; want 400", code)
	}
	if got, err := h.Update(ctx, gcs.HMACKeyAttrsToUpdate{State: gcs.Inactive}); err != nil || got.State != gcs.Inactive {
		t.Fatalf("deactivate = %+v, %v", got, err)
	}
	if err := h.Delete(ctx); err != nil {
		t.Fatal(err)
	}
	it := c.ListHMACKeys(ctx, "demo-project")
	if _, err := it.Next(); err == nil {
		t.Error("a deleted key is listed without showDeletedKeys")
	}
	if got, err := h.Get(ctx); err != nil || got.State != gcs.Deleted {
		t.Errorf("get after delete = %+v, %v; want DELETED", got, err)
	}
	if _, err := h.Update(ctx, gcs.HMACKeyAttrsToUpdate{State: gcs.Active}); err == nil {
		t.Error("a deleted key was reactivated")
	}
}

// A service account has at most 10 keys; deleted ones do not count.
func TestStorageHMACKeyLimitInProcess(t *testing.T) {
	c, _ := sdk(t)
	ctx := context.Background()
	const sa = "busy@demo-project.iam.gserviceaccount.com"
	var first *gcs.HMACKey
	for i := 0; i < 10; i++ {
		k, err := c.CreateHMACKey(ctx, "demo-project", sa)
		if err != nil {
			t.Fatalf("key %d: %v", i+1, err)
		}
		if first == nil {
			first = k
		}
	}
	if _, err := c.CreateHMACKey(ctx, "demo-project", sa); err == nil {
		t.Fatal("an 11th key was created")
	}
	if _, err := c.CreateHMACKey(ctx, "demo-project", "other@demo-project.iam.gserviceaccount.com"); err != nil {
		t.Errorf("another service account's first key: %v", err)
	}
	h := c.HMACKeyHandle("demo-project", first.AccessID)
	if _, err := h.Update(ctx, gcs.HMACKeyAttrsToUpdate{State: gcs.Inactive}); err != nil {
		t.Fatal(err)
	}
	if err := h.Delete(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateHMACKey(ctx, "demo-project", sa); err != nil {
		t.Errorf("a key after deleting one: %v", err)
	}
}

// The secret appears in the create response and nowhere else: not in the
// events the server's traffic produces, nor in any later response.
func TestHMACSecretNotInEvents(t *testing.T) {
	s, err := NewServer(Options{})
	if err != nil {
		t.Fatal(err)
	}
	var events []rest.Request
	h := httptest.NewServer(rest.Observe(s, func(r rest.Request) { events = append(events, r) }))
	t.Cleanup(h.Close)
	code, body := raw(t, "POST", h.URL+"/storage/v1/projects/p/hmacKeys?serviceAccountEmail=a@p.iam.gserviceaccount.com", "")
	var k struct {
		Metadata struct {
			AccessID string `json:"accessId"`
		} `json:"metadata"`
		Secret string `json:"secret"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &k) != nil || k.Secret == "" {
		t.Fatalf("create = %d %s", code, body)
	}
	base := h.URL + "/storage/v1/projects/p/hmacKeys"
	var later []string
	for _, rq := range []struct{ m, u, b string }{
		{"GET", base + "/" + k.Metadata.AccessID, ""},
		{"GET", base, ""},
		{"PUT", base + "/" + k.Metadata.AccessID, `{"state":"INACTIVE"}`},
		{"DELETE", base + "/" + k.Metadata.AccessID, ""},
		{"GET", base + "?showDeletedKeys=true", ""},
	} {
		_, b := raw(t, rq.m, rq.u, rq.b)
		later = append(later, b)
	}
	ev, _ := json.Marshal(events)
	if len(events) != 6 || strings.Contains(string(ev), k.Secret) {
		t.Errorf("events (%d) = %s; want 6, none holding the secret", len(events), ev)
	}
	for i, b := range later {
		if strings.Contains(b, k.Secret) {
			t.Errorf("response %d after create holds the secret: %s", i, b)
		}
	}
}

// The project's service agent has the documented form.
func TestStorageServiceAccountInProcess(t *testing.T) {
	c, _ := sdk(t)
	email, err := c.ServiceAccount(context.Background(), "demo-project")
	if err != nil || !strings.HasPrefix(email, "service-") || !strings.HasSuffix(email, "@gs-project-accounts.iam.gserviceaccount.com") {
		t.Errorf("service account = %q, %v", email, err)
	}
}
