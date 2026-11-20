//go:build browser

package browser

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"github.com/google/go-containerregistry/pkg/crane"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
)

// TestCloudRunArtifactRegistryVulnerabilitiesTabThroughThePage (#1912), in
// the run shard. With the project's Container Scanning API enabled, an
// Alpine image whose zlib OSV says CVE-2022-37434 affects is pushed to a
// Docker repository; its version's page has Google's Vulnerabilities tab,
// listing the CVE as CRITICAL, CVSS 9.8, fix available, with the package
// and its installed and fixed versions. It skips when the instance's
// scanner is not prefetched, or fails then with CLOUDBURROW_TEST_OSV_REQUIRED
// set, as CI's run shard sets it after prefetching the scanner (#1917).
func TestCloudRunArtifactRegistryVulnerabilitiesTabThroughThePage(t *testing.T) {
	c := arClient(t)
	su := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_RUN_SERVICEUSAGE"))
	if su == "" {
		su = strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_SERVICEUSAGE"))
	}
	if su == "" {
		t.Skip("no Service Usage endpoint: the Container Scanning API is enabled through it")
	}
	ctx := context.Background()
	project := uniqueProject(t)
	id := fmt.Sprintf("browser-scan-%d", time.Now().UnixNano()%1e6)
	parent := "projects/" + project + "/locations/us-central1"
	hc := &http.Client{Timeout: 30 * time.Second}
	t.Cleanup(hc.CloseIdleConnections)
	api := "http://" + strings.TrimPrefix(su, "http://") + "/v1/projects/" + project + "/services/containerscanning.googleapis.com"
	resp, err := hc.Post(api+":enable", "application/json", strings.NewReader("{}"))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("enable the Container Scanning API: %v %v", resp, err)
	}
	resp.Body.Close()
	t.Cleanup(func() {
		if r, err := hc.Post(api+":disable", "application/json", strings.NewReader("{}")); err == nil {
			r.Body.Close()
		}
	})
	op, err := c.CreateRepository(ctx, &arpb.CreateRepositoryRequest{Parent: parent, RepositoryId: id, Repository: &arpb.Repository{Format: arpb.Repository_DOCKER}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteRepository(context.Background(), &arpb.DeleteRepositoryRequest{Name: parent + "/repositories/" + id}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	img, err := crane.Image(map[string][]byte{
		"etc/os-release":       []byte("ID=alpine\nVERSION_ID=3.13.5\nPRETTY_NAME=\"Alpine Linux v3.13\"\n"),
		"etc/alpine-release":   []byte("3.13.5\n"),
		"etc/apk/arch":         []byte("x86_64\n"),
		"etc/apk/repositories": []byte("https://dl-cdn.alpinelinux.org/alpine/v3.13/main\n"),
		"lib/apk/db/installed": []byte("C:Q1\nP:zlib\nV:1.2.11-r3\nA:x86_64\nS:1\nI:1\nT:zlib\nU:https://zlib.net/\nL:Zlib\no:zlib\nm:x\nt:1\n\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	cf, _ := img.ConfigFile()
	cf = cf.DeepCopy()
	cf.OS, cf.Architecture, cf.History = "linux", "amd64", []v1.History{{CreatedBy: "browser test layer"}}
	if img, err = mutate.ConfigFile(img, cf); err != nil {
		t.Fatal(err)
	}
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envRun)), "http://")
	_, port, _ := net.SplitHostPort(addr)
	tr := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
	t.Cleanup(tr.CloseIdleConnections)
	if err := crane.Push(img, "us-central1-docker.pkg.localhost:"+port+"/"+project+"/"+id+"/zlib-app:v1", crane.Insecure, crane.WithTransport(tr)); err != nil {
		t.Fatal(err)
	}
	d, _ := img.Digest()

	p := open(t)
	path := "/artifact-registry/" + id + "/zlib-app/" + d.String() + "?project=" + project + "&tab=vulnerabilities"
	deadline := time.Now().Add(2 * time.Minute)
	var text string
	for {
		p.navigate(path)
		p.waitFor(`document.querySelector("#view [role=tabpanel]") !== null`)
		p.eval(`document.querySelector("#view").textContent`, &text)
		if strings.Contains(text, "CVE-2022-37434") {
			break
		}
		if strings.Contains(text, "not prefetched") {
			if os.Getenv("CLOUDBURROW_TEST_OSV_REQUIRED") != "" {
				t.Fatalf("the instance's scanner is not prefetched, and CLOUDBURROW_TEST_OSV_REQUIRED is set:\n%s", text)
			}
			t.Skip("the instance's scanner is not prefetched (cloudburrow prefetch --vulnerability-scanning)")
		}
		if time.Now().After(deadline) {
			t.Fatalf("the Vulnerabilities tab never listed CVE-2022-37434:\n%s", text)
		}
		time.Sleep(2 * time.Second)
	}
	var tabs, row string
	p.eval(`[...document.querySelectorAll("#view [role=tab]")].map((x) => x.textContent.trim()).join(",")`, &tabs)
	p.eval(`[...document.querySelectorAll("#view [role=tabpanel] tbody tr")].map((r) => r.textContent).find((x) => x.includes("CVE-2022-37434")) || ""`, &row)
	if !strings.Contains(tabs, "Vulnerabilities") {
		t.Errorf("the version's tabs are %q; want a Vulnerabilities tab", tabs)
	}
	for _, want := range []string{"Critical", "9.8", "Yes", "zlib", "OS", "1.2.11-r3", "1.2.12-r2"} {
		if !strings.Contains(row, want) {
			t.Errorf("CVE-2022-37434's row %q lacks %q", row, want)
		}
	}
	// Google's Scan results summary over the table (#1855).
	var summary string
	p.eval(`(document.querySelector("#view [role=tabpanel] .section-summary") || {}).textContent || ""`, &summary)
	for _, want := range []string{"Scan results", "Package types scanned", "OS", "Total vulnerabilities", "Fixable", "Critical"} {
		if !strings.Contains(summary, want) {
			t.Errorf("the Scan results summary %q lacks %q", summary, want)
		}
	}
	// The image's versions list counts it in its Vulnerabilities column,
	// and the Dependencies tab lists zlib (#1855).
	p.navigate("/artifact-registry/" + id + "/zlib-app?project=" + project)
	p.waitFor(`document.querySelector("#view tbody tr") !== null`)
	var vrow string
	p.eval(`document.querySelector("#view tbody tr").textContent`, &vrow)
	if !strings.Contains(vrow, "Critical") {
		t.Errorf("the version row %q has no vulnerability totals", vrow)
	}
	p.navigate(strings.Replace(path, "tab=vulnerabilities", "tab=dependencies", 1))
	p.waitFor(`document.querySelector("#view [role=tabpanel] tbody tr") !== null`)
	var deps string
	p.eval(`document.querySelector("#view [role=tabpanel]").textContent`, &deps)
	for _, want := range []string{"SBOM", "Most common licenses", "zlib", "1.2.11-r3"} {
		if !strings.Contains(deps, want) {
			t.Errorf("the Dependencies tab %q lacks %q", deps, want)
		}
	}
	// The scanner records each package's license (#2066): Most common
	// licenses is a bar graph, and zlib's License type is Zlib.
	var bars, zrow string
	p.eval(`[...document.querySelectorAll("#view [role=tabpanel] .bar-graph .bar-row")].map((r) => r.textContent).join("|")`, &bars)
	p.eval(`[...document.querySelectorAll("#view [role=tabpanel] tbody tr")].map((r) => r.textContent).find((x) => x.includes("zlib")) || ""`, &zrow)
	if bars != "Zlib1" || !strings.Contains(zrow, "Zlib") {
		t.Errorf("Most common licenses bars %q, zlib's row %q; want a Zlib bar of 1 and License type Zlib", bars, zrow)
	}

	// The Vulnerabilities row's View Fixed opens Vulnerability details with
	// its File location(s) and their layer; the Layer bar lists the layer
	// with its build command and count, and choosing it shows its
	// vulnerabilities (#2066).
	p.navigate(path)
	p.waitFor(`document.querySelector("#view [role=tabpanel] tbody tr") !== null`)
	var layerBar struct {
		Label   string
		Options []string
	}
	p.eval(`(() => { const s = document.querySelector("#vulnerabilities-show"); return s ? { Label: s.closest("label").firstChild.textContent.trim(),
		Options: [...s.options].map((o) => o.textContent) } : {}; })()`, &layerBar)
	if layerBar.Label != "Layer" || len(layerBar.Options) != 2 || layerBar.Options[0] != "All layers" ||
		!strings.HasPrefix(layerBar.Options[1], "Layer 0: browser test layer (") || !strings.HasSuffix(layerBar.Options[1], "vulnerabilities)") && !strings.HasSuffix(layerBar.Options[1], "vulnerability)") {
		t.Errorf("the Layer bar = %+v", layerBar)
	}
	p.eval(`(() => { const s = document.querySelector("#vulnerabilities-show"); s.value = s.options[1].value; s.dispatchEvent(new Event("change")); return true; })()`, nil)
	p.waitFor(`document.querySelector("#vulnerabilities-show").selectedIndex === 1 && document.querySelector("#view [role=tabpanel] tbody tr") !== null`)
	var layerRows string
	p.eval(`[...document.querySelectorAll("#view [role=tabpanel] tbody tr")].map((r) => r.textContent).join("|")`, &layerRows)
	if !strings.Contains(layerRows, "CVE-2022-37434") {
		t.Errorf("Layer 0's rows %q lack CVE-2022-37434", layerRows)
	}
	p.clickText("#view [role=tabpanel] tbody .row-panel-link", "View Fixed")
	p.waitFor(`document.querySelector("#side-panel") !== null`)
	var details struct{ Title, Body, Table string }
	p.eval(`(() => ({ Title: document.querySelector("#side-panel h2").textContent, Body: document.querySelector("#side-panel").textContent,
		Table: [...document.querySelectorAll("#side-panel table tr")].map((r) => r.textContent).join("|") }))()`, &details)
	if details.Title != "Vulnerability details" || !strings.Contains(details.Body, "CVE-2022-37434") || !strings.Contains(details.Body, "File location(s)") ||
		!strings.Contains(details.Table, "/lib/apk/db/installed") || !strings.Contains(details.Table, "browser test layer") || !strings.Contains(details.Table, "sha256:") {
		t.Errorf("Vulnerability details = %+v", details)
	}

	auditScreens(t, []a11yPage{
		{"image digest vulnerabilities with the layer bar", path, settled, ""},
		{"vulnerability details panel", path, `document.querySelector("#view [role=tabpanel] tbody .row-panel-link") !== null`,
			`document.querySelector("#view [role=tabpanel] tbody .row-panel-link").click()`},
		{"image digest dependencies with the licenses graph", strings.Replace(path, "tab=vulnerabilities", "tab=dependencies", 1), settled, ""},
	})
}
