//go:build compat

package compat

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	ak "google.golang.org/api/apikeys/v2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// EnvAPIKeys is the API Keys API v2 endpoint (#1915).
const EnvAPIKeys = "CLOUDBURROW_TEST_APIKEYS"

func apiKeysClient(t *testing.T, h *Harness) *ak.Service {
	t.Helper()
	hc := &http.Client{}
	t.Cleanup(hc.CloseIdleConnections)
	c, err := ak.NewService(h.Context(), option.WithEndpoint("http://"+h.Endpoint(EnvAPIKeys)+"/"),
		option.WithoutAuthentication(), option.WithHTTPClient(hc))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var keyStringForm = regexp.MustCompile(`^AIza[A-Za-z0-9_-]{35}$`)

// covers: apikeys.projects.locations.keys.create, apikeys.projects.locations.keys.get, apikeys.projects.locations.keys.getKeyString, apikeys.projects.locations.keys.list, apikeys.projects.locations.keys.patch, apikeys.projects.locations.keys.delete, apikeys.projects.locations.keys.undelete, apikeys.keys.lookupKey, apikeys.operations.get
//
// TestAPIKeysLifecycle (#1915), with google.golang.org/api/apikeys/v2: a key
// is created with restrictions and a done operation carrying its key string,
// read, listed, its string fetched and looked up, patched under a mask,
// deleted (listed only with showDeleted), undeleted, and two application
// restriction kinds on one key are refused.
func TestAPIKeysLifecycle(t *testing.T) {
	h := New(t)
	c := apiKeysClient(t, h)
	ctx := h.Context()
	parent := "projects/" + h.Project() + "/locations/global"
	id := strings.ToLower(v1ID(h, "key-"))
	op, err := c.Projects.Locations.Keys.Create(parent, &ak.V2Key{DisplayName: "Compat key",
		Restrictions: &ak.V2Restrictions{
			BrowserKeyRestrictions: &ak.V2BrowserKeyRestrictions{AllowedReferrers: []string{"*.example.com/*"}},
			ApiTargets:             []*ak.V2ApiTarget{{Service: "storage.googleapis.com"}},
		}}).KeyId(id).Context(ctx).Do()
	if err != nil {
		t.Fatal(err)
	}
	if !op.Done {
		t.Fatalf("create's operation is not done: %+v", op)
	}
	got, err := c.Operations.Get(op.Name).Context(ctx).Do()
	if err != nil || !got.Done {
		t.Fatalf("operations.get %s = %+v, %v", op.Name, got, err)
	}
	listName := func() []string {
		t.Helper()
		var names []string
		if err := c.Projects.Locations.Keys.List(parent).PageSize(1).Pages(ctx, func(r *ak.V2ListKeysResponse) error {
			for _, k := range r.Keys {
				names = append(names, k.Name)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return names
	}
	k, err := c.Projects.Locations.Keys.Get(parent + "/keys/" + id).Context(ctx).Do()
	if err != nil {
		t.Fatal(err)
	}
	if k.DisplayName != "Compat key" || k.KeyString != "" || k.Restrictions == nil || k.Restrictions.BrowserKeyRestrictions == nil ||
		!strings.HasSuffix(k.Name, "/locations/global/keys/"+id) || k.Uid == "" || k.Etag == "" {
		t.Errorf("get: %+v", k)
	}
	ks, err := c.Projects.Locations.Keys.GetKeyString(k.Name).Context(ctx).Do()
	if err != nil {
		t.Fatal(err)
	}
	if !keyStringForm.MatchString(ks.KeyString) {
		t.Errorf("keyString %q is not of Google's form", ks.KeyString)
	}
	lk, err := c.Keys.LookupKey().KeyString(ks.KeyString).Context(ctx).Do()
	if err != nil || lk.Name != k.Name || !strings.HasSuffix(lk.Parent, "/locations/global") {
		t.Errorf("lookupKey = %+v, %v", lk, err)
	}
	if names := listName(); !contains(names, k.Name) {
		t.Errorf("list lacks %s: %v", k.Name, names)
	}

	if _, err := c.Projects.Locations.Keys.Patch(k.Name, &ak.V2Key{DisplayName: "Renamed",
		Restrictions: &ak.V2Restrictions{}}).UpdateMask("displayName").Context(ctx).Do(); err != nil {
		t.Fatal(err)
	}
	k2, err := c.Projects.Locations.Keys.Get(k.Name).Context(ctx).Do()
	if err != nil {
		t.Fatal(err)
	}
	if k2.DisplayName != "Renamed" || k2.Restrictions == nil || k2.Restrictions.BrowserKeyRestrictions == nil {
		t.Errorf("after a displayName-masked patch: %+v", k2)
	}

	if _, err := c.Projects.Locations.Keys.Delete(k.Name).Context(ctx).Do(); err != nil {
		t.Fatal(err)
	}
	if names := listName(); contains(names, k.Name) {
		t.Errorf("a deleted key is listed without showDeleted")
	}
	del, err := c.Projects.Locations.Keys.List(parent).ShowDeleted(true).Context(ctx).Do()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range del.Keys {
		if d.Name == k.Name && d.DeleteTime != "" {
			found = true
		}
	}
	if !found {
		t.Errorf("showDeleted lacks %s with a deleteTime", k.Name)
	}
	if _, err := c.Projects.Locations.Keys.Undelete(k.Name, &ak.V2UndeleteKeyRequest{}).Context(ctx).Do(); err != nil {
		t.Fatal(err)
	}
	if names := listName(); !contains(names, k.Name) {
		t.Errorf("an undeleted key is not listed")
	}
	t.Cleanup(func() { _, _ = c.Projects.Locations.Keys.Delete(k.Name).Do() })

	_, err = c.Projects.Locations.Keys.Create(parent, &ak.V2Key{Restrictions: &ak.V2Restrictions{
		BrowserKeyRestrictions: &ak.V2BrowserKeyRestrictions{AllowedReferrers: []string{"example.com"}},
		ServerKeyRestrictions:  &ak.V2ServerKeyRestrictions{AllowedIps: []string{"198.51.100.1"}},
	}}).Context(ctx).Do()
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) || gerr.Code != http.StatusBadRequest {
		t.Errorf("two application restriction kinds = %v, want 400", err)
	}
	if _, err := c.Projects.Locations.Keys.Create(parent, &ak.V2Key{}).KeyId(id).Context(ctx).Do(); !errors.As(err, &gerr) || gerr.Code != http.StatusConflict {
		t.Errorf("a second key %s = %v, want 409", id, err)
	}
}

// covers: apikeys.projects.locations.keys.create, apikeys.projects.locations.keys.list, apikeys.projects.locations.keys.get, apikeys.projects.locations.keys.getKeyString, apikeys.projects.locations.keys.patch, apikeys.keys.lookupKey, apikeys.projects.locations.keys.delete, apikeys.projects.locations.keys.undelete
//
// TestGcloudServicesAPIKeys (#1915): gcloud services api-keys create (with a
// website and an API restriction), list, describe, get-key-string, update,
// lookup, delete and undelete, through gcloud-setup's apikeys override.
func TestGcloudServicesAPIKeys(t *testing.T) {
	h := New(t)
	c := apiKeysClient(t, h)
	g := newGcloudSession(t, h)
	project := h.Project()
	id := strings.ToLower(v1ID(h, "gkey-"))
	name := "projects/" + project + "/locations/global/keys/" + id
	g.must("services", "api-keys", "create", "--key-id", id, "--display-name", "gcloud key", "--project", project,
		"--allowed-referrers", "*.example.com/*", "--api-target", "service=storage.googleapis.com")
	t.Cleanup(func() { _, _ = c.Projects.Locations.Keys.Delete(name).Do() })
	listed := func() string {
		return g.must("services", "api-keys", "list", "--project", project, "--format=value(name)")
	}
	if out := listed(); !strings.Contains(out, "/keys/"+id) {
		t.Errorf("api-keys list lacks %s:\n%s", id, out)
	}
	if out := g.must("services", "api-keys", "describe", id, "--project", project, "--format=value(displayName)"); strings.TrimSpace(out) != "gcloud key" {
		t.Errorf("api-keys describe: %q", out)
	}
	ks := strings.TrimSpace(g.must("services", "api-keys", "get-key-string", id, "--project", project, "--format=value(keyString)"))
	if !keyStringForm.MatchString(ks) {
		t.Fatalf("get-key-string is not a key string")
	}
	if out := g.must("services", "api-keys", "lookup", ks, "--format=value(name)"); !strings.Contains(out, "/keys/"+id) {
		t.Errorf("api-keys lookup: %q", out)
	}
	g.must("services", "api-keys", "update", id, "--display-name", "renamed", "--project", project)
	k, err := c.Projects.Locations.Keys.Get(name).Context(h.Context()).Do()
	if err != nil {
		t.Fatal(err)
	}
	if k.DisplayName != "renamed" || k.Restrictions == nil || k.Restrictions.BrowserKeyRestrictions == nil || len(k.Restrictions.ApiTargets) != 1 {
		t.Errorf("after gcloud update: %+v", k)
	}
	g.must("services", "api-keys", "delete", id, "--project", project)
	if out := listed(); strings.Contains(out, "/keys/"+id) {
		t.Errorf("a deleted key is listed:\n%s", out)
	}
	g.must("services", "api-keys", "undelete", id, "--project", project)
	if out := listed(); !strings.Contains(out, "/keys/"+id) {
		t.Errorf("an undeleted key is not listed:\n%s", out)
	}
}

// TestTerraformAPIKeysKey (#1915): google_apikeys_key applies with website
// and API restrictions, exposes its key_string, plans clean, updates its
// display name and restrictions in place, and destroys (a soft delete, so
// the key is then listed only with showDeleted).
func TestTerraformAPIKeysKey(t *testing.T) {
	h := New(t)
	c := apiKeysClient(t, h)
	m := newTFModule(t)
	project := h.Project()
	id := strings.ToLower(v1ID(h, "tfkey-"))
	module := func(display, referrer string) string {
		return fmt.Sprintf(`
resource "google_apikeys_key" "k" {
  project      = %q
  name         = %q
  display_name = %q
  restrictions {
    browser_key_restrictions {
      allowed_referrers = [%q]
    }
    api_targets {
      service = "storage.googleapis.com"
    }
  }
}

output "key" {
  value     = google_apikeys_key.k.key_string
  sensitive = true
}
`, project, id, display, referrer)
	}
	name := "projects/" + project + "/locations/global/keys/" + id
	m.write(module("tf key", "*.example.com/*"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	t.Cleanup(func() { _, _ = c.Projects.Locations.Keys.Delete(name).Do() })
	if out := m.must("output", "-raw", "key"); !keyStringForm.MatchString(strings.TrimSpace(out)) {
		t.Errorf("key_string output is not a key string")
	}
	m.planClean()
	m.write(module("tf key 2", "www.example.org/*"))
	m.apply()
	k, err := c.Projects.Locations.Keys.Get(name).Context(h.Context()).Do()
	if err != nil {
		t.Fatal(err)
	}
	if k.DisplayName != "tf key 2" || k.Restrictions == nil || k.Restrictions.BrowserKeyRestrictions == nil ||
		len(k.Restrictions.BrowserKeyRestrictions.AllowedReferrers) != 1 || k.Restrictions.BrowserKeyRestrictions.AllowedReferrers[0] != "www.example.org/*" {
		t.Errorf("after the in-place update: %+v", k)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	k, err = c.Projects.Locations.Keys.Get(name).Context(h.Context()).Do()
	if err != nil || k.DeleteTime == "" {
		t.Errorf("after destroy: %+v, %v; want the key soft-deleted", k, err)
	}
}
