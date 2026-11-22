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
	"github.com/chromedp/chromedp/kb"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/chromedp/chromedp"
)

// TestCloudRunArtifactRegistryVersionAttachmentsTab (#2111), in the run
// shard: an image version's Attachments tab, as Google documents it ("Open
// the image version and click the Attachments tab"), lists the artifacts
// referring to the version, here an SPDX SBOM pushed with a subject as
// `oras attach` pushes it, by digest with its type. The digest opens the
// attachment's own version, reached and opened by keyboard, whose Delete
// version deletes it: the tab is empty again and the official client's
// ListAttachments no longer lists it. The tab passes the WCAG 2.1 AA
// audit.
func TestCloudRunArtifactRegistryVersionAttachmentsTab(t *testing.T) {
	c := arClient(t)
	ctx := context.Background()
	project := uniqueProject(t)
	id := fmt.Sprintf("browser-attach-%d", time.Now().UnixNano()%1e6)
	parent := "projects/" + project + "/locations/us-central1"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/artifact-registry?project="+project,
		`{"id":"`+id+`","format":"DOCKER","location":"us-central1"}`); code != http.StatusOK {
		t.Fatalf("create repository %s through the console API = %d: %s", id, code, body)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/artifact-registry?project="+project+"&name="+id, "")
	})
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envRun)), "http://")
	_, port, _ := net.SplitHostPort(addr)
	tr := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
	t.Cleanup(tr.CloseIdleConnections)
	ref := "us-central1-docker.pkg.localhost:" + port + "/" + project + "/" + id + "/web"
	img, err := crane.Image(map[string][]byte{"app.txt": []byte("attached")})
	if err != nil {
		t.Fatal(err)
	}
	if err := crane.Push(img, ref+":v1", crane.Insecure, crane.WithTransport(tr)); err != nil {
		t.Fatal(err)
	}
	d, _ := img.Digest()
	sz, _ := img.Size()
	m, _ := img.Manifest()
	sbom := mutate.MediaType(empty.Image, types.OCIManifestSchema1)
	sbom = mutate.ConfigMediaType(sbom, "application/spdx+json")
	if sbom, err = mutate.Append(sbom, mutate.Addendum{Layer: static.NewLayer([]byte(`{"spdxVersion":"SPDX-2.3"}`), "application/spdx+json")}); err != nil {
		t.Fatal(err)
	}
	sbom = mutate.Subject(sbom, v1.Descriptor{MediaType: m.MediaType, Digest: d, Size: sz}).(v1.Image)
	sd, _ := sbom.Digest()
	dst, err := name.NewDigest(ref+"@"+sd.String(), name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(dst, sbom, remote.WithTransport(tr)); err != nil {
		t.Fatalf("push the SBOM: %v", err)
	}

	path := "/artifact-registry/" + id + "/web/" + d.String() + "?project=" + project + "&tab=attachments"
	p := open(t)
	p.navigate(path)
	p.waitFor(`document.querySelector("#view [role=tabpanel] tbody tr") !== null`)
	var tab struct {
		Selected string
		Heads    []string
		Row      string
	}
	p.eval(`(() => ({ Selected: (document.querySelector("#view [role=tab][aria-selected=true]") || {}).textContent.trim(),
		Heads: [...document.querySelectorAll("#view [role=tabpanel] thead th")].map((h) => h.textContent.trim()).filter((x) => x),
		Row: document.querySelector("#view [role=tabpanel] tbody tr").textContent }))()`, &tab)
	if tab.Selected != "Attachments" || len(tab.Heads) < 2 || tab.Heads[0] != "Digest" ||
		!strings.Contains(tab.Row, sd.String()[:19]) || !strings.Contains(tab.Row, "application/spdx+json") {
		t.Errorf("the Attachments tab = %+v; want the SBOM's digest and type under Digest", tab)
	}

	t.Run("keyboard", func(t *testing.T) {
		p := open(t)
		p.navigate(path)
		p.waitFor(`document.querySelector("#view [role=tabpanel] tbody tr a") !== null`)
		p.tabTo(`document.activeElement.closest("#view [role=tabpanel] tbody tr") !== null && document.activeElement.tagName === "A"`)
		p.run(chromedp.KeyEvent(kb.Enter))
		p.waitFor(fmt.Sprintf(`decodeURIComponent(location.pathname).endsWith(%q)`, "/web/"+sd.String()))
		var text string
		p.eval(`document.querySelector("#view").textContent`, &text)
		if !strings.Contains(text, sd.String()) {
			t.Errorf("the attachment's own version page does not show its digest")
		}
	})

	auditScreens(t, []a11yPage{{"image version attachments tab", path, `document.querySelector("#view [role=tabpanel] tbody tr") !== null`, ""}})

	// Delete version on the attachment's page deletes the attachment.
	p.navigate("/artifact-registry/" + id + "/web/" + sd.String() + "?project=" + project)
	p.waitFor(settled)
	where := p.findAction("Delete version")
	if where == "" {
		t.Fatal("the attachment's version page offers no Delete version")
	}
	p.clickText(where, "Delete version")
	p.confirmByName(p.confirmWord())
	p.waitFor(`document.querySelector(".modal") === null`)
	p.navigate(path)
	p.waitFor(`document.querySelector("#view [role=tabpanel]") !== null && document.querySelector("#view [role=tabpanel] tbody tr") === null`)
	as, err := c.ListAttachments(ctx, &arpb.ListAttachmentsRequest{Parent: parent + "/repositories/" + id}).Next()
	if err == nil {
		t.Errorf("ListAttachments after Delete version lists %v; want none", as)
	}
}
