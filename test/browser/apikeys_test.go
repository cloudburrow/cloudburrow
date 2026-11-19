//go:build browser

package browser

import (
	"context"
	"net/http"
	"strings"
	"testing"

	ak "google.golang.org/api/apikeys/v2"
	"google.golang.org/api/option"
)

const envAPIKeys = "CLOUDBURROW_TEST_APIKEYS"

// TestAPIKeysThroughTheCredentialsPage (#1915), in the storage shard: Create
// credentials > API key makes a key whose string the dialog shows; its page
// saves a name, a website restriction and an API restriction, which the
// official API Keys client reads back; Show key shows the same string;
// Rotate key makes a second key with the same restrictions, and Delete the
// previous key deletes the first; Restore deleted credentials restores it.
func TestAPIKeysThroughTheCredentialsPage(t *testing.T) {
	needService(t, "serviceusage")
	hc := &http.Client{}
	t.Cleanup(hc.CloseIdleConnections)
	c, err := ak.NewService(context.Background(), option.WithEndpoint("http://"+endpoint(t, envAPIKeys)+"/"),
		option.WithoutAuthentication(), option.WithHTTPClient(hc))
	if err != nil {
		t.Fatal(err)
	}
	project := uniqueProject(t)
	parent := "projects/" + project + "/locations/global"
	p := open(t)
	p.navigate("/apis/credentials?project=" + project)
	p.waitFor(`document.querySelector("#credentials-api-keys-table") !== null`)
	p.clickExpr(`document.querySelector("#credentials-create")`)
	p.clickExpr(`document.querySelector("#credentials-create-api-key")`)
	p.waitFor(`document.querySelector("#api-key-string") !== null`)
	var shown string
	p.eval(`document.querySelector("#api-key-string").value`, &shown)
	if !strings.HasPrefix(shown, "AIza") || len(shown) != 39 {
		t.Fatalf("API key created shows %d characters", len(shown))
	}
	list, err := c.Projects.Locations.Keys.List(parent).Do()
	if err != nil || len(list.Keys) != 1 {
		t.Fatalf("keys after Create = %v, %v", list, err)
	}
	name := list.Keys[0].Name
	id := name[strings.LastIndex(name, "/")+1:]
	t.Cleanup(func() {
		all, _ := c.Projects.Locations.Keys.List(parent).Do()
		if all != nil {
			for _, k := range all.Keys {
				_, _ = c.Projects.Locations.Keys.Delete(k.Name).Do()
			}
		}
	})
	if ks, err := c.Projects.Locations.Keys.GetKeyString(name).Do(); err != nil || ks.KeyString != shown {
		t.Errorf("the dialog's key is not the key's string (%v)", err)
	}
	p.clickExpr(`document.querySelector("#api-key-edit-link")`)
	p.waitFor(`document.querySelector("#api-key-form") !== null`)
	p.setField("#api-key-name", "Browser key")
	p.clickExpr(`document.querySelector("#api-key-app-browser")`)
	p.setField("#api-key-referrers", "*.example.com/*")
	p.clickExpr(`document.querySelector("#api-key-api-restrict")`)
	p.clickExpr(`document.querySelector('#api-key-api-list input[value="storage.googleapis.com"]')`)
	p.clickExpr(`document.querySelector("#api-key-save")`)
	p.waitFor(`location.pathname === "/apis/credentials" && document.querySelector('#credentials-api-keys-table tr[data-key]') !== null`)
	k, err := c.Projects.Locations.Keys.Get(name).Do()
	if err != nil {
		t.Fatal(err)
	}
	if k.DisplayName != "Browser key" || k.Restrictions == nil || k.Restrictions.BrowserKeyRestrictions == nil ||
		strings.Join(k.Restrictions.BrowserKeyRestrictions.AllowedReferrers, ",") != "*.example.com/*" ||
		len(k.Restrictions.ApiTargets) != 1 || k.Restrictions.ApiTargets[0].Service != "storage.googleapis.com" {
		t.Errorf("after Save: %+v", k)
	}
	var summary string
	p.eval(`document.querySelector('#credentials-api-keys-table tr[data-key] td:nth-child(3)').textContent`, &summary)
	if summary != "Websites, 1 API" {
		t.Errorf("Restrictions column = %q", summary)
	}

	p.navigate("/apis/credentials/key/" + id + "?project=" + project)
	p.waitFor(`document.querySelector("#api-key-show") !== null`)
	p.clickExpr(`document.querySelector("#api-key-show")`)
	p.waitFor(`document.querySelector("#api-key-string") !== null`)
	var again string
	p.eval(`document.querySelector("#api-key-string").value`, &again)
	if again != shown {
		t.Errorf("Show key differs from the created key")
	}
	p.clickExpr(`document.querySelector("#api-key-close")`)
	p.clickExpr(`document.querySelector("#api-key-rotate")`)
	p.waitFor(`document.querySelector("#api-key-rotate-form") !== null`)
	p.setField("#f-displayName", "Rotated key")
	p.clickExpr(`document.querySelector('#api-key-rotate-form button[type="submit"]')`)
	p.waitFor(`document.querySelector("#api-key-delete-previous") !== null`)
	list, err = c.Projects.Locations.Keys.List(parent).Do()
	if err != nil || len(list.Keys) != 2 {
		t.Fatalf("keys after Rotate key = %v, %v", list, err)
	}
	for _, nk := range list.Keys {
		if nk.Name != name && (nk.DisplayName != "Rotated key" || nk.Restrictions == nil || nk.Restrictions.BrowserKeyRestrictions == nil) {
			t.Errorf("the rotated key: %+v", nk)
		}
	}
	p.clickExpr(`document.querySelector("#api-key-string") && document.querySelector("#api-key-close")`)
	p.clickExpr(`document.querySelector("#api-key-delete-previous")`)
	p.waitFor(`document.querySelector("#api-key-form") !== null && document.querySelector("#api-key-delete-previous") === null`)
	if k, err := c.Projects.Locations.Keys.Get(name).Do(); err != nil || k.DeleteTime == "" {
		t.Errorf("the previous key after Delete the previous key: %+v, %v", k, err)
	}

	p.navigate("/apis/credentials/restore?project=" + project)
	p.waitFor(`document.querySelector('#api-keys-deleted tr[data-key="` + id + `"]') !== null`)
	p.clickExpr(`document.querySelector('#api-keys-deleted tr[data-key="` + id + `"] [data-action="restore-key"]')`)
	p.waitFor(`document.querySelector('#api-keys-deleted tr[data-key="` + id + `"]') === null`)
	if k, err := c.Projects.Locations.Keys.Get(name).Do(); err != nil || k.DeleteTime != "" {
		t.Errorf("after Restore: %+v, %v", k, err)
	}
}
