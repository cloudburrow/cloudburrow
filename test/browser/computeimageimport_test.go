//go:build browser

package browser

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// TestCreateImageFromCloudStorageFileThroughTheBrowser (#2139): Create an
// image's Source offers Cloud Storage file (the virtual disk still drawn
// unavailable with its reason); choosing it shows the Cloud Storage file
// field, whose Browse fills in a bucket of this instance's Cloud Storage;
// an import of a file that is not there is sent (its operation fails as
// Google's does, so the image is never listed).
func TestCreateImageFromCloudStorageFileThroughTheBrowser(t *testing.T) {
	needService(t, "compute-images")
	needService(t, "storage")
	project := uniqueProject(t)
	q := "?project=" + url.QueryEscape(project)
	bucket := strings.ToLower(strings.ReplaceAll(project, "_", "-")) + "-images"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/storage"+q, `{"name":"`+bucket+`"}`); code != http.StatusOK {
		t.Fatalf("creating the bucket = %d: %s", code, body)
	}
	p := open(t)
	p.navigate("/compute/images/create" + q)
	p.waitFor(settled + ` && document.querySelector("#view #f-source") !== null`)
	var opts struct{ CloudStorage, VirtualDisk bool }
	p.eval(`(() => { const o = [...document.querySelector("#f-source").options];
		const by = (t) => o.find((x) => x.textContent.trim() === t);
		return { CloudStorage: !!by("Cloud Storage file") && !by("Cloud Storage file").disabled,
			VirtualDisk: !!by("Virtual disk (VMDK, VHD)") && by("Virtual disk (VMDK, VHD)").disabled }; })()`, &opts)
	if !opts.CloudStorage || !opts.VirtualDisk {
		t.Fatalf("Source's options = %+v; want Cloud Storage file offered and the virtual disk drawn unavailable", opts)
	}
	p.setField("#f-source", "Cloud Storage file")
	p.waitFor(`(() => { const f = document.querySelector("#f-rawSource"); return !!f && f.getClientRects().length > 0; })()`)
	p.run(chromedp.Click(chromedp.CSS(`#browse-rawSource`)))
	p.clickText("#browse-title ~ .browse-list .fs-row-link", bucket)
	p.waitFor(`document.querySelector("#f-rawSource").value === "gs://` + bucket + `/" && document.querySelector("#browse-title") === null`)

	p.submitForm(map[string]string{"name": "browser-imported", "rawSource": "gs://" + bucket + "/missing.tar.gz"}, "name")
	p.waitFor(`location.pathname === "/compute/images"`)
	deadline := time.Now().Add(time.Minute)
	for {
		code, body := consoleDo(t, http.MethodGet, "/api/resources/compute-images"+q, "")
		if code == http.StatusOK && !strings.Contains(body, `"browser-imported"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("an import of a missing file left its image: %d %s", code, body)
		}
		time.Sleep(time.Second)
	}
}

// TestCreateImageFromCloudStorageFileMeetsWCAG (#2139): Create an image
// with Source Cloud Storage file chosen meets WCAG as the compute shard's
// other screens do.
func TestCreateImageFromCloudStorageFileMeetsWCAG(t *testing.T) {
	needService(t, "compute-images")
	project := servedProject(t)
	pg := screen("create image from a Cloud Storage file", "/compute/images/create", project)
	pg.ready += ` && document.querySelector("#view #f-source") !== null`
	pg.then = `(() => { const f = document.querySelector("#f-source"); f.value = "Cloud Storage file";
		f.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`
	auditScreens(t, []a11yPage{pg})
}
