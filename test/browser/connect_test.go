//go:build browser

package browser

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"
)

// TestConnectPageShowsTheEnvironmentAndDownloadsTheBundle (#802): the
// toolbar's Connect control opens /connect without reloading the page; the
// variables table is the console API's /api/connect, which the unit and
// compat suites hold to `cloudburrow env --format json`; the JSON format is
// that JSON; About is /api/about in the page and in Settings and utilities;
// the admin token is nowhere on the page; and Download bundle asks for the
// admin token before sending anything, then saves the redacted bundle, a
// gzipped tar starting with manifest.json that does not hold the token. At
// 360px the page does not scroll sideways.
func TestConnectPageShowsTheEnvironmentAndDownloadsTheBundle(t *testing.T) {
	token := strings.TrimSpace(os.Getenv(envAdminToken))
	if token == "" {
		t.Fatalf("%s is not set; the diagnose download needs the instance's admin token", envAdminToken)
	}
	code, body := consoleDo(t, http.MethodGet, "/api/connect", "")
	if code != http.StatusOK {
		t.Fatalf("GET /api/connect = %d: %s", code, body)
	}
	var api struct {
		Variables []struct{ Name, Value string }
	}
	if err := json.Unmarshal([]byte(body), &api); err != nil {
		t.Fatal(err)
	}
	_, aboutBody := consoleDo(t, http.MethodGet, "/api/about", "")
	var about struct{ Version, Commit string }
	_ = json.Unmarshal([]byte(aboutBody), &about)

	p := open(t)
	downloads := t.TempDir()
	p.run(browser.SetDownloadBehavior(browser.SetDownloadBehaviorBehaviorAllow).WithDownloadPath(downloads))
	p.navigate("/")
	p.waitFor(`document.querySelector("#connect-link") !== null && getComputedStyle(document.querySelector("#connect-link")).display !== "none"`)
	before := p.navigations()
	p.run(chromedp.Click(`#connect-link`, chromedp.ByQuery))
	p.waitFor(`location.pathname === "/connect" && document.querySelectorAll("#connect-vars tbody tr").length > 0`)
	if p.navigations() != before {
		t.Error("the Connect control reloaded the page instead of routing to /connect")
	}

	var rows [][2]string
	p.eval(`[...document.querySelectorAll("#connect-vars tbody tr")].map((r) => [r.cells[0].textContent, r.cells[1].textContent])`, &rows)
	if len(rows) != len(api.Variables) {
		t.Errorf("the page lists %d variables, /api/connect %d", len(rows), len(api.Variables))
	}
	for i, v := range api.Variables {
		if i < len(rows) && (rows[i][0] != v.Name || rows[i][1] != v.Value) {
			t.Errorf("row %d = %v, /api/connect has %s=%s", i, rows[i], v.Name, v.Value)
		}
	}

	// The JSON format is `env --format json`'s output for the same variables.
	p.eval(`(() => { const s = document.querySelector("#connect-format-select");
		s.value = [...s.options].find((o) => o.textContent === "JSON").value;
		s.dispatchEvent(new Event("change")); return true; })()`, nil)
	p.waitFor(`document.querySelector("#connect-format-text").dataset.format === "json"`)
	var text string
	p.eval(`document.querySelector("#connect-format-text").textContent`, &text)
	var shown map[string]string
	if err := json.Unmarshal([]byte(text), &shown); err != nil {
		t.Fatalf("the JSON format is not JSON: %v\n%s", err, text)
	}
	for _, v := range api.Variables {
		if shown[v.Name] != v.Value {
			t.Errorf("the JSON format's %s = %q, /api/connect has %q", v.Name, shown[v.Name], v.Value)
		}
	}

	var version, commit string
	p.eval(`document.querySelector("#connect-version").textContent`, &version)
	p.eval(`document.querySelector("#connect-commit").textContent`, &commit)
	if version != about.Version || commit != about.Commit {
		t.Errorf("About on the page = %s %s, /api/about %s %s", version, commit, about.Version, about.Commit)
	}
	p.run(chromedp.Click(`#settings`, chromedp.ByQuery))
	p.waitFor(`!document.querySelector("#settings-panel").hidden && document.querySelector("#about-version").textContent === ` + jsString(about.Version))
	p.run(chromedp.Click(`#settings`, chromedp.ByQuery))

	var page string
	p.eval(`document.documentElement.outerHTML`, &page)
	if strings.Contains(page, token) {
		t.Fatal("the Connect page carries the admin token")
	}

	// Without a token the form asks for one and sends nothing.
	p.eval(`sessionStorage.removeItem("cb-admin-token")`, nil)
	p.run(chromedp.Click(`#diagnose-download`, chromedp.ByQuery))
	p.waitFor(`!document.querySelector("#diagnose-error").hidden && !document.querySelector("#diagnose-token-row").hidden`)
	if sent := p.sent(http.MethodGet, "/api/diagnose"); len(sent) != 0 {
		t.Fatalf("Download with no token sent %v", sent)
	}

	p.run(chromedp.SendKeys(`#diagnose-token`, token, chromedp.ByQuery), chromedp.Click(`#diagnose-download`, chromedp.ByQuery))
	var done bool
	if err := chromedp.Run(p.ctx, chromedp.Poll(`document.querySelector("#diagnose-status").textContent.startsWith("Downloaded ") || !document.querySelector("#diagnose-error").hidden`,
		&done, chromedp.WithPollingTimeout(3*time.Minute))); err != nil {
		t.Fatalf("waiting for the bundle: %v", err)
	}
	var failure string
	p.eval(`document.querySelector("#diagnose-error").hidden ? "" : document.querySelector("#diagnose-error").textContent`, &failure)
	if failure != "" {
		t.Fatalf("the download failed: %s", failure)
	}
	if sent := p.sent(http.MethodGet, "/api/diagnose"); len(sent) != 1 {
		t.Errorf("the bundle was requested %d times, want once: %v", len(sent), sent)
	}
	// At the narrowest width the page does not scroll sideways: a long
	// state-directory path wraps, and the table scrolls in its own region.
	p.run(emulation.SetDeviceMetricsOverride(360, 900, 1, true))
	p.navigate("/connect")
	p.waitFor(`document.querySelector("#connect-vars") !== null`)
	var width struct{ Scroll, Client float64 }
	p.eval(`({ Scroll: document.documentElement.scrollWidth, Client: document.documentElement.clientWidth })`, &width)
	if width.Scroll > width.Client {
		t.Errorf("at 360px /connect scrolls sideways: scrollWidth %v, clientWidth %v", width.Scroll, width.Client)
	}

	files := savedBundle(t, downloads)
	if _, ok := files["manifest.json"]; !ok {
		t.Errorf("the saved bundle has no manifest.json; it has %d files", len(files))
	}
	for name, data := range files {
		if strings.Contains(string(data), token) {
			t.Errorf("%s in the saved bundle holds the admin token", name)
		}
	}
}

// jsString quotes s as a JavaScript string.
func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// savedBundle waits for Chrome to finish saving the bundle into dir and
// reads it.
func savedBundle(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var path string
	for path == "" {
		matches, _ := filepath.Glob(filepath.Join(dir, "cloudburrow-diagnose-*.tar.gz"))
		if len(matches) > 0 {
			path = matches[0]
			break
		}
		select {
		case <-ctx.Done():
			entries, _ := os.ReadDir(dir)
			t.Fatalf("no bundle was saved in %s: it holds %v", dir, entries)
		case <-time.After(200 * time.Millisecond):
		}
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("the saved bundle is not gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	out := map[string][]byte{}
	first := true
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		if first && hdr.Name != "manifest.json" {
			t.Errorf("the bundle starts with %s, want manifest.json", hdr.Name)
		}
		first = false
		data, _ := io.ReadAll(tr)
		out[hdr.Name] = data
	}
}
