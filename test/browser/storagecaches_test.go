//go:build browser

package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/option"
	storagev1 "google.golang.org/api/storage/v1"

	"github.com/chromedp/chromedp"
)

const envStorage = "CLOUDBURROW_TEST_STORAGE"

// storageSDK is the official Cloud Storage client and Google's generated JSON
// API client (the one gcloud storage's Anywhere Cache and relocate commands
// call) against the instance's Cloud Storage; the test skips when its
// endpoint is not set.
func storageSDK(t *testing.T) (*storage.Client, *storagev1.Service) {
	t.Helper()
	ep := strings.TrimRight(strings.TrimSpace(os.Getenv(envStorage)), "/")
	if ep == "" {
		t.Skipf("%s is not set: every change is read back through the official Cloud Storage clients", envStorage)
	}
	if !strings.Contains(ep, "://") {
		ep = "http://" + ep
	}
	ctx := context.Background()
	t.Setenv("STORAGE_EMULATOR_HOST", ep)
	c, err := storage.NewClient(ctx, option.WithoutAuthentication(), option.WithEndpoint(ep+"/storage/v1/"))
	if err != nil {
		t.Fatalf("storage.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	s, err := storagev1.NewService(ctx, option.WithEndpoint(ep+"/storage/v1/"),
		option.WithoutAuthentication(), option.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatalf("storagev1.NewService: %v", err)
	}
	return c, s
}

// regionalBucket makes a bucket in US-CENTRAL1 through the official client,
// and deletes it afterwards.
func regionalBucket(t *testing.T, c *storage.Client, project, name string) {
	t.Helper()
	ctx := context.Background()
	if err := c.Bucket(name).Create(ctx, project, &storage.BucketAttrs{Location: "US-CENTRAL1"}); err != nil {
		t.Fatalf("create bucket %s: %v", name, err)
	}
	t.Cleanup(func() { _ = c.Bucket(name).Delete(context.Background()) })
}

// storageAction opens one of a bucket page's actions, sets its fields, and
// submits it. An action that starts an operation answers with its rows, in
// the dialog: each must fit it, and the operation's ID, the last segment of
// its name, is returned once the dialog is closed; "" for one that answers
// nothing, whose dialog closes by itself.
func storageAction(p *tab, label string, values map[string]string) string {
	p.t.Helper()
	p.waitFor(`document.querySelector(".modal") === null`)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent === %q)`, label))
	p.clickText("#view .page-actions button", label)
	p.waitFor(`document.querySelector(".modal.is-open form") !== null`)
	for id, v := range values {
		p.eval(fmt.Sprintf(`(() => { const f = document.querySelector(".modal.is-open #f-%s");
			if (f.type === "checkbox") { f.checked = %q === "true"; f.dispatchEvent(new Event("change", { bubbles: true })); return true; }
			f.value = %q; f.dispatchEvent(new Event("input", { bubbles: true })); f.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, id, v, v), nil)
	}
	p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal.is-open") === null || document.querySelector("#action-result-table") !== null`)
	var res struct {
		Operation string
		Overflow  []string
	}
	p.eval(`(() => { const t = document.querySelector("#action-result-table");
		if (!t) return { Operation: "", Overflow: [] };
		const box = t.closest(".modal-body").getBoundingClientRect();
		const rows = [...t.querySelectorAll("tbody tr")];
		const op = rows.find((r) => r.cells[0].textContent === "Operation");
		return { Operation: op ? op.cells[1].textContent : "",
		         Overflow: rows.filter((r) => [...r.cells].some((c) => c.getBoundingClientRect().right > box.right + 0.5))
		                       .map((r) => r.cells[0].textContent) }; })()`, &res)
	if len(res.Overflow) > 0 {
		p.t.Errorf("%s's result rows %v run past the dialog's edge", label, res.Overflow)
	}
	if res.Operation != "" || len(res.Overflow) > 0 {
		p.clickText(".modal.is-open .modal-actions button", "Close")
	}
	p.waitFor(`document.querySelector(".modal") === null`)
	return res.Operation[strings.LastIndex(res.Operation, "/")+1:]
}

// waitStorage polls cond for up to 30 seconds.
func waitStorage(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// TestStorageAnywhereCachesThroughTheBrowser (#1444), in the storage shard.
// On a US-CENTRAL1 bucket's page, Create Anywhere Cache in us-central1-a
// with a TTL and the second-miss admission policy makes the cache the
// generated JSON API client's AnywhereCaches.Get reads, with those settings;
// once running, the Anywhere caches tab lists its zone and says no cache tier
// exists locally. Change Anywhere Cache's pause pauses it and resume runs it
// again, as Get reads.
func TestStorageAnywhereCachesThroughTheBrowser(t *testing.T) {
	needService(t, "storage")
	c, s := storageSDK(t)
	p := open(t)
	project := uniqueProject(t)
	bucket := project + "-caches"
	regionalBucket(t, c, project, bucket)
	get := func() *storagev1.AnywhereCache {
		t.Helper()
		ac, err := s.AnywhereCaches.Get(bucket, "us-central1-a").Do()
		if err != nil {
			t.Fatalf("AnywhereCaches.Get: %v", err)
		}
		return ac
	}

	p.navigate("/storage/browser/" + bucket + "?project=" + project)
	if id := storageAction(p, "Create Anywhere Cache", map[string]string{
		"zone": "us-central1-a", "ttl": "172800s", "admissionPolicy": "admit-on-second-miss"}); id == "" {
		t.Error("Create Anywhere Cache answered no operation")
	}
	if sent := p.sent(http.MethodPost, "/api/actions/storage"); len(sent) != 1 {
		t.Errorf("Create Anywhere Cache sent %d actions, want 1", len(sent))
	}
	ac := get()
	if ac.Ttl != "172800s" || ac.AdmissionPolicy != "admit-on-second-miss" || ac.Bucket != bucket {
		t.Errorf("after Create Anywhere Cache, Get reads %+v; want TTL 172800s and admit-on-second-miss", ac)
	}
	waitStorage(t, "the cache to run", func() bool { return get().State == "running" })

	p.navigate("/storage/browser/" + bucket + "?project=" + project)
	p.clickText(`#view [role="tab"]`, "Anywhere caches")
	p.waitFor(`[...document.querySelectorAll("#view tbody td, #view tbody a")].some((n) => n.textContent.trim() === "us-central1-a")`)
	var note string
	p.eval(`document.querySelector("#view").textContent`, &note)
	if !strings.Contains(note, "CloudBurrow keeps no cache tier") {
		t.Error("the Anywhere caches tab does not say that no cache tier exists locally")
	}

	storageAction(p, "Change Anywhere Cache", map[string]string{"zone": "us-central1-a", "change": "pause"})
	if st := get().State; st != "paused" {
		t.Errorf("after Change Anywhere Cache pause, Get reads state %q, want paused", st)
	}
	storageAction(p, "Change Anywhere Cache", map[string]string{"zone": "us-central1-a", "change": "resume"})
	if st := get().State; st != "running" {
		t.Errorf("after Change Anywhere Cache resume, Get reads state %q, want running", st)
	}
}

// TestStorageRelocateBucketThroughTheBrowser (#1444), in the storage shard.
// On a US-CENTRAL1 bucket's page, Relocate bucket to US-EAST1 as a dry run
// moves nothing: the official client reads the bucket where it was. Without
// the dry run it starts the relocation the JSON API's Operations.List
// returns; once its finalization state is READY, Finalize relocation with its
// operation ID moves the bucket, and BucketAttrs reads US-EAST1.
func TestStorageRelocateBucketThroughTheBrowser(t *testing.T) {
	needService(t, "storage")
	c, s := storageSDK(t)
	p := open(t)
	project := uniqueProject(t)
	bucket := project + "-relocate"
	regionalBucket(t, c, project, bucket)
	ctx := context.Background()
	location := func() string {
		t.Helper()
		a, err := c.Bucket(bucket).Attrs(ctx)
		if err != nil {
			t.Fatalf("bucket attrs: %v", err)
		}
		return a.Location
	}
	relocations := func() []*storagev1.GoogleLongrunningOperation {
		t.Helper()
		ops, err := s.Operations.List(bucket).Do()
		if err != nil {
			t.Fatalf("Operations.List: %v", err)
		}
		var out []*storagev1.GoogleLongrunningOperation
		for _, op := range ops.Operations {
			var md map[string]any
			_ = json.Unmarshal(op.Metadata, &md)
			if md["@type"] == "type.googleapis.com/google.storage.control.v2.RelocateBucketMetadata" && md["validateOnly"] != true {
				out = append(out, op)
			}
		}
		return out
	}

	p.navigate("/storage/browser/" + bucket + "?project=" + project)
	storageAction(p, "Relocate bucket", map[string]string{"location": "US-EAST1", "dryRun": "true"})
	if loc := location(); loc != "US-CENTRAL1" {
		t.Errorf("after a dry-run relocation the bucket is in %s, want US-CENTRAL1", loc)
	}
	if ops := relocations(); len(ops) != 0 {
		t.Errorf("a dry run started %d relocations", len(ops))
	}

	id := storageAction(p, "Relocate bucket", map[string]string{"location": "US-EAST1"})
	ops := relocations()
	if len(ops) != 1 || !strings.HasSuffix(ops[0].Name, "/"+id) {
		t.Fatalf("Relocate bucket answered operation %q and started %d relocations, want that one", id, len(ops))
	}
	waitStorage(t, "the relocation to be ready to finalize", func() bool {
		op, err := s.Operations.Get(bucket, id).Do()
		if err != nil {
			t.Fatalf("Operations.Get: %v", err)
		}
		var md map[string]any
		_ = json.Unmarshal(op.Metadata, &md)
		return md["finalizationState"] == "READY"
	})
	if loc := location(); loc != "US-CENTRAL1" {
		t.Errorf("before finalizing the bucket is in %s, want US-CENTRAL1", loc)
	}

	storageAction(p, "Finalize relocation", map[string]string{"operation": id, "ttl": "7200s"})
	waitStorage(t, "the bucket to move", func() bool { return location() == "US-EAST1" })
}
