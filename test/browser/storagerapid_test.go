//go:build browser

package browser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/storage"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/googleapi"
	storagev1 "google.golang.org/api/storage/v1"
)

// storageRowAction opens one of a listing row's actions from its Actions
// menu, sets its fields, and submits it, as storageAction does a page's. A
// Destructive action is confirmed by typing confirm back.
func storageRowAction(p *tab, row, label string, values map[string]string, confirm string) {
	p.t.Helper()
	p.waitFor(`document.querySelector(".modal") === null`)
	menu := fmt.Sprintf(`button[aria-label=%q]`, "Actions for "+row)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, menu))
	p.run(chromedp.Click(chromedp.CSS(menu)))
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, label)
	if confirm != "" {
		p.waitFor(`document.querySelector(".modal.is-open #confirm-input") !== null`)
		p.run(chromedp.SendKeys(chromedp.CSS(`#confirm-input`), confirm))
		p.clickText(".modal:has(#confirm-input) .modal-actions button", label)
	} else {
		p.waitFor(`document.querySelector(".modal.is-open form") !== null`)
		for id, v := range values {
			p.eval(fmt.Sprintf(`(() => { const f = document.querySelector(".modal.is-open #f-%s");
				if (f.type === "checkbox") { f.checked = %q === "true"; f.dispatchEvent(new Event("change", { bubbles: true })); return true; }
				f.value = %q; f.dispatchEvent(new Event("input", { bubbles: true })); f.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, id, v, v), nil)
		}
		p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	}
	p.waitFor(`document.querySelector(".modal.is-open") === null || document.querySelector("#action-result-table") !== null`)
	var result bool
	p.eval(`document.querySelector("#action-result-table") !== null`, &result)
	if result {
		p.clickText(".modal.is-open .modal-actions button", "Close")
	}
	p.waitFor(`document.querySelector(".modal") === null`)
}

// uniformBucket makes a US-CENTRAL1 bucket with uniform bucket-level access
// through the official client, and deletes it afterwards.
func uniformBucket(t *testing.T, c *storage.Client, project, name string) {
	t.Helper()
	ctx := context.Background()
	if err := c.Bucket(name).Create(ctx, project, &storage.BucketAttrs{Location: "US-CENTRAL1",
		UniformBucketLevelAccess: storage.UniformBucketLevelAccess{Enabled: true}}); err != nil {
		t.Fatalf("create bucket %s: %v", name, err)
	}
	t.Cleanup(func() { _ = c.Bucket(name).Delete(context.Background()) })
}

// TestStorageRapidCachesThroughTheBrowser (#1505), in the storage shard. On
// a US-CENTRAL1 bucket's page, Create Rapid Cache in us-central1-a, of type
// rapid-cache-ultra with a TTL and the second-miss policy, makes the cache
// the generated JSON API client's RapidCaches.Get reads; once it runs, the
// Rapid caches tab lists it, saying no cache tier exists locally. Its row's
// Edit Rapid Cache changes the TTL and admission policy, and Disable Rapid
// Cache, confirmed by its zone, disables it, as Get reads.
func TestStorageRapidCachesThroughTheBrowser(t *testing.T) {
	needService(t, "storage")
	c, s := storageSDK(t)
	p := open(t)
	project := uniqueProject(t)
	bucket := project + "-rapid"
	regionalBucket(t, c, project, bucket)
	get := func() *storagev1.RapidCache {
		t.Helper()
		rc, err := s.RapidCaches.Get(bucket, "us-central1-a").Do()
		if err != nil {
			t.Fatalf("RapidCaches.Get: %v", err)
		}
		return rc
	}
	page := "/storage/browser/" + bucket + "?project=" + project

	p.navigate(page)
	if id := storageAction(p, "Create Rapid Cache", map[string]string{
		"zone": "us-central1-a", "cacheType": "rapid-cache-ultra", "ttl": "172800s", "admissionPolicy": "admit-on-second-miss"}); id == "" {
		t.Error("Create Rapid Cache answered no operation")
	}
	rc := get()
	if rc.CacheType != "rapid-cache-ultra" || rc.Ttl != "172800s" || rc.AdmissionPolicy != "admit-on-second-miss" {
		t.Errorf("after Create Rapid Cache, Get reads %+v", rc)
	}
	waitStorage(t, "the cache to run", func() bool { return get().State == "running" })

	p.navigate(page)
	p.clickText(`#view [role="tab"]`, "Rapid caches")
	p.waitFor(`[...document.querySelectorAll("#view tbody td, #view tbody a")].some((n) => n.textContent.trim() === "us-central1-a")`)
	var text string
	p.eval(`document.querySelector("#view").textContent`, &text)
	if !strings.Contains(text, "CloudBurrow keeps no cache tier") {
		t.Error("the Rapid caches tab does not say that no cache tier exists locally")
	}

	storageRowAction(p, "us-central1-a", "Edit Rapid Cache", map[string]string{"ttl": "7200s", "admissionPolicy": "no_read_admission"}, "")
	if rc := get(); rc.AdmissionPolicy != "admit-on-second-miss" && rc.AdmissionPolicy != "no_read_admission" {
		t.Errorf("after Edit Rapid Cache, Get reads %+v", rc)
	}
	waitStorage(t, "the update to finish", func() bool { rc := get(); return !rc.PendingUpdate && rc.Ttl == "7200s" })
	if rc := get(); rc.AdmissionPolicy != "no_read_admission" {
		t.Errorf("after Edit Rapid Cache, Get reads admission policy %q, want no_read_admission", rc.AdmissionPolicy)
	}

	p.navigate(page)
	p.clickText(`#view [role="tab"]`, "Rapid caches")
	storageRowAction(p, "us-central1-a", "Disable Rapid Cache", nil, "us-central1-a")
	waitStorage(t, "the cache to be disabled", func() bool { return get().State == "disabled" })
}

// TestStorageEditManagedFolderThroughTheBrowser (#1505), in the storage
// shard. With two running Rapid Caches made through the generated JSON API
// client and a managed folder, Edit managed folder on the folder's row sets
// its Rapid cache policies, one ingesting on write, as ManagedFolders.Get
// reads; editing it again with one line takes the other policy off.
func TestStorageEditManagedFolderThroughTheBrowser(t *testing.T) {
	needService(t, "storage")
	c, s := storageSDK(t)
	p := open(t)
	project := uniqueProject(t)
	bucket := project + "-mfrc"
	uniformBucket(t, c, project, bucket)
	for _, z := range []string{"us-central1-a", "us-central1-b"} {
		if _, err := s.RapidCaches.Insert(bucket, &storagev1.RapidCache{Zone: z}).Do(); err != nil {
			t.Fatalf("RapidCaches.Insert %s: %v", z, err)
		}
	}
	waitStorage(t, "the caches to run", func() bool {
		l, err := s.RapidCaches.List(bucket).Do()
		if err != nil {
			t.Fatalf("RapidCaches.List: %v", err)
		}
		for _, rc := range l.Items {
			if rc.State != "running" {
				return false
			}
		}
		return len(l.Items) == 2
	})
	if _, err := s.ManagedFolders.Insert(bucket, &storagev1.ManagedFolder{Name: "hot/"}).Do(); err != nil {
		t.Fatalf("ManagedFolders.Insert: %v", err)
	}
	t.Cleanup(func() { _ = s.ManagedFolders.Delete(bucket, "hot/").Do() })
	policies := func() map[string]storagev1.RapidCachePolicy {
		t.Helper()
		f, err := s.ManagedFolders.Get(bucket, "hot/").Do()
		if err != nil {
			t.Fatalf("ManagedFolders.Get: %v", err)
		}
		if f.RapidCacheConfig == nil {
			return nil
		}
		return f.RapidCacheConfig.Policies
	}
	enabled := func(v string) bool { v = strings.ToLower(v); return v == "enabled" || v == "ingest_on_write_enabled" }

	p.navigate("/storage/browser/" + bucket + "?project=" + project)
	storageRowAction(p, "hot/", "Edit managed folder", map[string]string{"rapidCaches": "us-central1-a ingest-on-write\nus-central1-b"}, "")
	if got := policies(); len(got) != 2 || !enabled(got["us-central1-a"].IngestOnWrite) || enabled(got["us-central1-b"].IngestOnWrite) {
		t.Fatalf("after Edit managed folder, Get reads policies %+v", got)
	}
	if n := len(p.sent(http.MethodPost, "/api/actions/storage")); n != 1 {
		t.Errorf("Edit managed folder sent %d actions, want 1", n)
	}

	p.navigate("/storage/browser/" + bucket + "?project=" + project)
	storageRowAction(p, "hot/", "Edit managed folder", map[string]string{"rapidCaches": "us-central1-b"}, "")
	if got := policies(); len(got) != 1 || got["us-central1-b"].RapidCacheId != "us-central1-b" {
		t.Fatalf("after taking one policy off, Get reads policies %+v", got)
	}
}

// TestStorageIPFilterThroughTheBrowser (#1505), in the storage shard. On a
// bucket's page, Edit IP filtering, enabled with a public range that leaves
// out this machine, makes the IP filter the official client reads, and the
// filter is enforced: the official client's object read is refused 403, and
// the bucket's page still opens, its Objects tab saying why. Editing it to
// admit loopback lets the read through; Not configured removes the filter.
func TestStorageIPFilterThroughTheBrowser(t *testing.T) {
	needService(t, "storage")
	c, s := storageSDK(t)
	p := open(t)
	project := uniqueProject(t)
	bucket := project + "-ipf"
	regionalBucket(t, c, project, bucket)
	ctx := context.Background()
	obj := c.Bucket(bucket).Object("hello.txt")
	w := obj.NewWriter(ctx)
	_, _ = io.WriteString(w, "hello")
	if err := w.Close(); err != nil {
		t.Fatalf("write an object: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.Buckets.Patch(bucket, &storagev1.Bucket{NullFields: []string{"IpFilter"}}).Do()
		_ = obj.Delete(context.Background())
	})
	read := func() error {
		r, err := obj.NewReader(ctx)
		if err != nil {
			return err
		}
		defer r.Close()
		_, err = io.ReadAll(r)
		return err
	}
	page := "/storage/browser/" + bucket + "?project=" + project

	p.navigate(page)
	storageAction(p, "Edit IP filtering", map[string]string{"mode": "Enabled", "publicRanges": "203.0.113.0/24",
		"vpcSources": "projects/" + project + "/global/networks/default 10.0.0.0/8", "allowAllServiceAgentAccess": "true"})
	b, err := s.Buckets.Get(bucket).Do()
	if err != nil {
		t.Fatalf("Buckets.Get: %v", err)
	}
	if f := b.IpFilter; f == nil || f.Mode != "Enabled" || f.PublicNetworkSource == nil ||
		strings.Join(f.PublicNetworkSource.AllowedIpCidrRanges, ",") != "203.0.113.0/24" || len(f.VpcNetworkSources) != 1 || !f.AllowAllServiceAgentAccess {
		t.Fatalf("after Edit IP filtering, Get reads %+v", b.IpFilter)
	}
	var gerr *googleapi.Error
	if err := read(); err == nil {
		t.Error("an enabled filter that leaves out this machine let the official client read an object")
	} else if errors.As(err, &gerr) && gerr.Code != http.StatusForbidden {
		t.Errorf("the refused read = %v, want 403", err)
	}

	p.navigate(page)
	p.waitFor(`document.querySelector("#view").textContent.includes("IP filter is enabled")`)
	p.clickText(`#view [role="tab"]`, "Configuration")
	p.waitFor(`document.querySelector("#view").textContent.includes("203.0.113.0/24")`)

	storageAction(p, "Edit IP filtering", map[string]string{"mode": "Enabled", "publicRanges": "203.0.113.0/24\n127.0.0.0/8\n::1/128"})
	if err := read(); err != nil {
		t.Errorf("a filter admitting loopback refused the official client's read: %v", err)
	}

	p.navigate(page)
	storageAction(p, "Edit IP filtering", map[string]string{"mode": "Not configured"})
	if b, err := s.Buckets.Get(bucket).Do(); err != nil || b.IpFilter != nil {
		t.Errorf("after Not configured, Get reads %+v, %v", b.IpFilter, err)
	}
}
