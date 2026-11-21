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
	"github.com/chromedp/chromedp"
	"github.com/google/go-containerregistry/pkg/crane"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
)

// TestCloudRunArtifactRegistrySetupPanelBrowseAndCreatedThroughThePage
// (#2066), in the run shard.
//   - A repository's Setup Instructions, beside its title, open a side
//     panel with this instance's docker push address and a Copy button,
//     not a tab; Close returns focus to the button.
//   - An image version's Created is when the image was built (its
//     config's created), apart from Uploaded.
//   - Create Repository's remote upstream has Browse, which lists the
//     project's standard Docker repositories in the chosen region; the one
//     chosen is filled in by its resource name, and the remote repository
//     made reads it back as its upstream through the official client's
//     GetRepository.
//
// The repository page with its panel open, the versions list and the
// Browse dialog pass the WCAG 2.1 AA audit.
func TestCloudRunArtifactRegistrySetupPanelBrowseAndCreatedThroughThePage(t *testing.T) {
	c := arClient(t)
	ctx := context.Background()
	project := uniqueProject(t)
	id := fmt.Sprintf("browser-base-%d", time.Now().UnixNano()%1e6)
	remote := id + "-remote"
	parent := "projects/" + project + "/locations/us-central1"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/artifact-registry?project="+project,
		`{"id":"`+id+`","format":"DOCKER","location":"us-central1"}`); code != http.StatusOK {
		t.Fatalf("create repository %s through the console API = %d: %s", id, code, body)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/artifact-registry?project="+project+"&name="+remote, "")
		consoleDo(t, http.MethodDelete, "/api/resources/artifact-registry?project="+project+"&name="+id, "")
	})
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envRun)), "http://")
	_, port, _ := net.SplitHostPort(addr)
	tr := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
	t.Cleanup(tr.CloseIdleConnections)
	img, err := crane.Image(map[string][]byte{"app.txt": []byte("built")})
	if err != nil {
		t.Fatal(err)
	}
	built := time.Date(2024, 3, 1, 9, 30, 0, 0, time.UTC)
	if img, err = mutate.CreatedAt(img, v1.Time{Time: built}); err != nil {
		t.Fatal(err)
	}
	if err := crane.Push(img, "us-central1-docker.pkg.localhost:"+port+"/"+project+"/"+id+"/web:v1", crane.Insecure, crane.WithTransport(tr)); err != nil {
		t.Fatal(err)
	}

	p := open(t)
	repoPage := "/artifact-registry/" + id + "?project=" + project
	p.navigate(repoPage)
	p.waitFor(settled)
	var tabs string
	p.eval(`[...document.querySelectorAll("#view [role=tab]")].map((x) => x.textContent.trim()).join(",")`, &tabs)
	if strings.Contains(tabs, "Setup") {
		t.Errorf("the repository's tabs are %q; Setup instructions is a side panel, not a tab", tabs)
	}
	p.clickText("#view button", "Setup Instructions")
	p.waitFor(`document.querySelector("#side-panel") !== null && !document.querySelector("#info-panel").hidden`)
	var panel struct {
		Title, Text string
		Copy        bool
	}
	p.eval(`(() => ({ Title: document.querySelector("#side-panel h2").textContent, Text: document.querySelector("#side-panel pre").textContent,
		Copy: [...document.querySelectorAll("#side-panel button")].some((b) => b.textContent.trim() === "Copy") }))()`, &panel)
	if panel.Title != "Setup instructions" || !strings.Contains(panel.Text, "docker push us-central1-docker.pkg.localhost:"+port+"/"+project+"/"+id) || !panel.Copy {
		t.Errorf("the Setup instructions panel = %+v", panel)
	}
	p.run(chromedp.Click(chromedp.CSS("#side-panel-close")))
	var back bool
	p.eval(`document.querySelector("#info-panel").hidden && document.activeElement && document.activeElement.textContent.trim() === "Setup Instructions"`, &back)
	if !back {
		t.Error("Close does not hide the Setup instructions panel and return focus to Setup Instructions")
	}

	imagePage := "/artifact-registry/" + id + "/web?project=" + project
	p.navigate(imagePage)
	p.waitFor(`document.querySelector("#view tbody tr") !== null`)
	var cells struct{ Created, Uploaded string }
	p.eval(`(() => { const heads = [...document.querySelectorAll("#view thead th")].map((h) => h.textContent.trim());
		const row = [...document.querySelector("#view tbody tr").children].map((c) => c.textContent.trim());
		return { Created: row[heads.indexOf("Created")], Uploaded: row[heads.indexOf("Uploaded")] }; })()`, &cells)
	if !strings.HasPrefix(cells.Created, "2024-03-01") || cells.Uploaded == cells.Created {
		t.Errorf("the version's Created is %q and Uploaded %q; want Created the image's build time, 2024-03-01", cells.Created, cells.Uploaded)
	}

	createPage := "/artifact-registry/create?project=" + project
	p.navigate(createPage)
	p.waitFor(`document.querySelector("#view #f-id") !== null`)
	clickRadio(p, "mode", 1) // Remote
	setField(p, "#view", "f-id", remote)
	p.run(chromedp.Click(chromedp.CSS("#browse-upstream")))
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("[role=dialog] .fs-row-link")].some((b) => b.textContent === %q)`, id))
	p.clickText("[role=dialog] .fs-row-link", id)
	var upstream string
	p.eval(`document.querySelector("#view #f-upstream").value`, &upstream)
	if upstream != parent+"/repositories/"+id {
		t.Errorf("Browse filled the upstream with %q; want %s", upstream, parent+"/repositories/"+id)
	}
	p.run(chromedp.Click(chromedp.CSS(`#view button[type="submit"]`)))
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, remote))
	r, err := c.GetRepository(ctx, &arpb.GetRepositoryRequest{Name: parent + "/repositories/" + remote})
	if err != nil || r.GetMode() != arpb.Repository_REMOTE_REPOSITORY || r.GetRemoteRepositoryConfig().GetCommonRepository().GetUri() != parent+"/repositories/"+id {
		t.Errorf("GetRepository of the remote repository = %v, %v; want the upstream Browse chose", r, err)
	}

	auditScreens(t, []a11yPage{
		{"repository setup instructions panel", repoPage, settled, `document.querySelector("#side-panel-open-0").click()`},
		{"image versions with build time", imagePage, settled, ""},
		{"create repository upstream browse dialog", createPage, `document.querySelector("#view #f-id") !== null`,
			`(() => { document.querySelector("#view #f-mode-1").click(); document.querySelector("#browse-upstream").click(); })()`},
	})
}
