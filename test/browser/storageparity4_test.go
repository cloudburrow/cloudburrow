//go:build browser

package browser

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	control "cloud.google.com/go/storage/control/apiv2"
	"cloud.google.com/go/storage/control/apiv2/controlpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestStorageGoogleRound4ThroughTheBrowser (#2047), in the storage shard.
// A soft-deleted bucket's Restore, from the Buckets list's Soft deleted
// buckets only, is Google's pane: the tabs Specify bucket generation and
// Specify objects to restore, and Additional options. Restore with Restore
// objects that match criteria and a glob brings the bucket back with only
// the matching object, as the official client reads. Version history's
// rows are checked for its Delete, confirmed by typing delete, which
// deletes the checked generations the official client then no longer
// lists. Every one of those screens and dialogs passes the WCAG 2.1 AA
// audit.
func TestStorageGoogleRound4ThroughTheBrowser(t *testing.T) {
	needService(t, "storage")
	gcs, _ := storageSDK(t)
	ctx := context.Background()
	project := uniqueProject(t)
	q := "?project=" + project
	bucket, kept, hist := project+"-r4", project+"-r4kept", project+"-r4hist"
	softDelete := func(name string, objects ...string) {
		t.Helper()
		if err := gcs.Bucket(name).Create(ctx, project, nil); err != nil {
			t.Fatalf("create %s with the official client: %v", name, err)
		}
		for _, o := range objects {
			w := gcs.Bucket(name).Object(o).NewWriter(ctx)
			_, _ = w.Write([]byte("data-" + o))
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			if err := gcs.Bucket(name).Object(o).Delete(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if err := gcs.Bucket(name).Delete(ctx); err != nil {
			t.Fatalf("delete %s with the official client: %v", name, err)
		}
	}
	softDelete(bucket, "logs/a.txt", "data/b.bin")
	softDelete(kept)
	t.Cleanup(func() {
		for _, o := range []string{"logs/a.txt", "data/b.bin"} {
			_ = gcs.Bucket(bucket).Object(o).Delete(context.Background())
		}
		_ = gcs.Bucket(bucket).Delete(context.Background())
	})

	p := open(t)
	softList := "/storage/browser" + q + "&view=softdeleted"
	p.navigate(softList)
	p.cellMenu(bucket, "Restore")
	p.waitFor(`document.querySelector(".modal.is-open #f-objects-2") !== null`)
	var pane struct{ Tabs, Text, Submit string }
	p.eval(`({ Tabs: [...document.querySelectorAll(".modal.is-open [role=tab]")].map((b) => b.textContent).join("|"),
		Text: document.querySelector(".modal.is-open").textContent,
		Submit: document.querySelector(".modal.is-open button[type=submit]").textContent })`, &pane)
	if pane.Tabs != "Specify bucket generation|Specify objects to restore" || pane.Submit != "Restore" ||
		!strings.Contains(pane.Text, "Additional options") || !strings.Contains(pane.Text, "Don't restore objects") {
		t.Errorf("the restore pane reads %+v", pane)
	}
	p.clickText(".modal.is-open [role=tab]", "Specify objects to restore")
	click := func(sel string) { p.eval(fmt.Sprintf(`document.querySelector(%q).click(), true`, sel), nil) }
	click(".modal.is-open #f-objects-2")
	p.waitFor(`document.querySelector(".modal.is-open #f-filterByGlob")?.closest("[hidden]") === null`)
	click(".modal.is-open #f-filterByGlob")
	p.waitFor(`document.querySelector(".modal.is-open #f-matchGlobs")?.closest("[hidden]") === null`)
	p.run(chromedp.SendKeys(chromedp.CSS(`.modal.is-open #f-matchGlobs`), "logs/**"))
	p.clickText(".modal.is-open button[type=submit]", "Restore")
	p.waitFor(`document.querySelector(".modal") === null`)
	eventually(t, "the restored bucket", func() bool {
		_, err := gcs.Bucket(bucket).Attrs(ctx)
		return err == nil
	})
	eventually(t, "the matching object", func() bool {
		_, err := gcs.Bucket(bucket).Object("logs/a.txt").Attrs(ctx)
		return err == nil
	})
	if _, err := gcs.Bucket(bucket).Object("data/b.bin").Attrs(ctx); !errors.Is(err, storage.ErrObjectNotExist) {
		t.Errorf("an object the glob does not match was restored: %v", err)
	}

	// Version history: the checked versions, Delete, delete typed.
	if err := gcs.Bucket(hist).Create(ctx, project, &storage.BucketAttrs{VersioningEnabled: true}); err != nil {
		t.Fatal(err)
	}
	var gens []int64
	for _, body := range []string{"one", "two", "three"} {
		w := gcs.Bucket(hist).Object("v.txt").NewWriter(ctx)
		_, _ = w.Write([]byte(body))
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		gens = append(gens, w.Attrs().Generation)
	}
	versions := func() []int64 {
		var out []int64
		it := gcs.Bucket(hist).Objects(ctx, &storage.Query{Versions: true, Prefix: "v.txt"})
		for {
			o, err := it.Next()
			if errors.Is(err, iterator.Done) {
				return out
			}
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, o.Generation)
		}
	}
	t.Cleanup(func() {
		for _, g := range versions() {
			_ = gcs.Bucket(hist).Object("v.txt").Generation(g).Delete(context.Background())
		}
		_ = gcs.Bucket(hist).Delete(context.Background())
	})
	historyPage := "/storage/browser/_details/" + hist + "/v.txt" + q + "&tab=versions"
	p.navigate(historyPage)
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].filter((r) => r.textContent.includes("v.txt#")).length === 3`)
	var menu []string
	p.run(chromedp.Click(chromedp.CSS(fmt.Sprintf(`button[aria-label=%q]`, fmt.Sprintf("Actions for v.txt#%d", gens[0])))))
	p.eval(`[...document.querySelectorAll('.overflow-menu:not([hidden]) [role="menuitem"]')].map((b) => b.textContent)`, &menu)
	if contains(menu, "Delete version") {
		t.Errorf("a Version history row's menu still offers Delete version: %v", menu)
	}
	p.run(chromedp.KeyEvent(kb.Escape))
	for _, g := range gens[:2] {
		click(fmt.Sprintf(`#view input[aria-label="Select v.txt#%d"]`, g))
	}
	p.waitFor(`[...document.querySelectorAll("#view .action-bar button")].some((b) => b.textContent === "Delete" && !b.disabled)`)
	p.clickText("#view .action-bar button", "Delete")
	var label string
	p.waitFor(`document.querySelector(".modal.is-open #confirm-input") !== null`)
	p.eval(`document.querySelector(".modal.is-open label[for=confirm-input] strong").textContent`, &label)
	if label != "delete" {
		t.Errorf("Delete asks for %q typed; want delete", label)
	}
	p.confirmByName("delete")
	p.waitFor(`document.querySelector(".modal") === null`)
	eventually(t, "the checked versions to be deleted", func() bool {
		v := versions()
		return len(v) == 1 && v[0] == gens[2]
	})

	ready := `[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(` + fmt.Sprintf("%q", kept) + `))`
	auditScreens(t, []a11yPage{
		{"object Version history with checkboxes", historyPage, settled + ` && document.querySelector("#versions-show") !== null`, ""},
		{"bucket restore pane dialog", softList, ready,
			fmt.Sprintf(`document.querySelector('button[aria-label="Actions for %s"]').click();
			[...document.querySelectorAll(".overflow-menu:not([hidden]) [role=menuitem]")].find((b) => b.textContent === "Restore").click()`, kept)},
	})
}

// TestStorageIntelligenceDialogThroughThePage (#2047), in the storage
// shard. Enable Storage Intelligence opens Google's Configure Storage
// Intelligence dialog: the resource, Continue, then bucket filters, where
// Add regular expression adds a name regex, ending in Enable this
// configuration; the official StorageControl client then reads STANDARD
// with the excluded regex. Disable beside the Overview's Status asks for
// Disable typed, and the client then reads DISABLED. The dialog passes the
// WCAG 2.1 AA audit.
func TestStorageIntelligenceDialogThroughThePage(t *testing.T) {
	needService(t, "storage-intelligence")
	ep := strings.TrimRight(strings.TrimSpace(os.Getenv(envStorage)), "/")
	if ep == "" {
		t.Skipf("%s is not set: every change is read back through the official StorageControl client", envStorage)
	}
	if !strings.Contains(ep, "://") {
		ep = "http://" + ep
	}
	ctx := context.Background()
	c, err := control.NewStorageControlRESTClient(ctx, option.WithEndpoint(ep), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	project := uniqueProject(t)
	name := "projects/" + project + "/locations/global/intelligenceConfig"
	t.Cleanup(func() {
		_, _ = c.UpdateProjectIntelligenceConfig(context.Background(), &controlpb.UpdateProjectIntelligenceConfigRequest{
			IntelligenceConfig: &controlpb.IntelligenceConfig{Name: name, EditionConfig: controlpb.IntelligenceConfig_INHERIT},
			UpdateMask:         &fieldmaskpb.FieldMask{Paths: []string{"edition_config", "filter"}}})
	})
	read := func() *controlpb.IntelligenceConfig {
		t.Helper()
		got, err := c.GetProjectIntelligenceConfig(ctx, &controlpb.GetProjectIntelligenceConfigRequest{Name: name})
		if err != nil {
			t.Fatalf("GetProjectIntelligenceConfig: %v", err)
		}
		return got
	}
	waitEdition := func(want controlpb.IntelligenceConfig_EditionConfig) {
		t.Helper()
		for deadline := time.Now().Add(15 * time.Second); read().GetEditionConfig() != want; time.Sleep(200 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("GetProjectIntelligenceConfig reads edition %v, want %v", read().GetEditionConfig(), want)
			}
		}
	}

	p := open(t)
	page := "/storage/intelligence?project=" + project
	p.navigate(page)
	p.waitFor(`document.querySelector("#list-action-configure") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#list-action-configure`)))
	p.waitFor(`document.querySelector(".modal.is-open #f-resource") !== null`)
	var title string
	p.eval(`document.querySelector(".modal.is-open h2").textContent`, &title)
	if title != "Configure Storage Intelligence" {
		t.Errorf("the dialog is titled %q", title)
	}
	p.clickText(".modal.is-open button", "Continue")
	p.waitFor(`document.querySelector(".modal.is-open #f-bucketFilter")?.closest("[hidden]") === null`)
	p.eval(`(() => { const s = document.querySelector(".modal.is-open #f-bucketFilter"); s.value = "exclude";
		s.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, nil)
	p.clickText(".modal.is-open button", "Add regular expression")
	p.waitFor(`document.querySelector(".modal.is-open .group-row input[type=text]") !== null`)
	p.eval(`(() => { const rows = [...document.querySelectorAll(".modal.is-open .group-row")];
		const i = rows[rows.length - 1].querySelector("input[type=text]");
		i.value = "^tmp-"; i.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, nil)
	p.clickText(".modal.is-open button[type=submit]", "Enable this configuration")
	p.waitFor(`document.querySelector(".modal") === null`)
	waitEdition(controlpb.IntelligenceConfig_STANDARD)
	if f := read().GetFilter(); f == nil || !strings.Contains(fmt.Sprint(f.GetExcludedCloudStorageBuckets()), "^tmp-") {
		t.Errorf("after Enable this configuration the filter is %v; want the excluded regex ^tmp-", f)
	}

	p.navigate(page)
	p.clickText("#view .property-action", "Disable")
	p.waitFor(`document.querySelector(".modal.is-open #confirm-input") !== null`)
	var word string
	p.eval(`document.querySelector(".modal.is-open label[for=confirm-input] strong").textContent`, &word)
	if word != "Disable" {
		t.Errorf("Disable asks for %q typed; want Disable", word)
	}
	p.confirmByName("Disable")
	waitEdition(controlpb.IntelligenceConfig_DISABLED)

	auditScreens(t, []a11yPage{
		{"Configure Storage Intelligence dialog", page, settled + ` && document.querySelector("#list-action-configure") !== null`,
			`document.querySelector("#list-action-configure").click()`},
	})
}

// settingsAccountPage is a service account's HMAC keys page.
func settingsAccountPage(sa, project string) string {
	return "/storage/settings/" + url.PathEscape(sa) + "?project=" + project
}

// has reports whether the page has an element sel matches.
func (p *tab) has(sel string) bool {
	p.t.Helper()
	var found bool
	p.eval(fmt.Sprintf(`document.querySelector(%q) !== null`, sel), &found)
	return found
}
