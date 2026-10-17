//go:build browser

package browser

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	artifactregistry "cloud.google.com/go/artifactregistry/apiv1"
	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestCloudRunArtifactRegistryCreateAndEditRepositoryThroughThePage, in
// the run shard, whose Run port serves Artifact Registry. A repository
// could be made only by gcloud run deploy --source and changed only by
// gcloud: Create repository on the Repositories page makes one the
// official client's GetRepository reads with its description and labels,
// and Edit repository on its page changes both.
func TestCloudRunArtifactRegistryCreateAndEditRepositoryThroughThePage(t *testing.T) {
	needService(t, "artifact-registry")
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envRun)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: the repository is read back through the Artifact Registry API itself", envRun)
	}
	ctx := context.Background()
	c, err := artifactregistry.NewClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	project := uniqueProject(t)
	id := fmt.Sprintf("browser-repo-%d", time.Now().UnixNano()%1e6)
	name := "projects/" + project + "/locations/us-east1/repositories/" + id
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/artifact-registry?project="+project+"&name="+id, "")
	})

	p := open(t)
	p.navigate("/artifact-registry?project=" + project)
	p.clickText("#view button", "Create repository")
	p.waitFor(`document.querySelector("#view #f-id, .modal.is-open #f-id") !== null`)
	scope := "#view"
	var inModal bool
	p.eval(`document.querySelector(".modal.is-open #f-id") !== null`, &inModal)
	if inModal {
		scope = ".modal.is-open"
	}
	setField(p, scope, "f-id", id)
	setField(p, scope, "f-location", "us-east1")
	setField(p, scope, "f-description", "made in the browser")
	setField(p, scope, "f-labels", "team=web")
	p.run(chromedp.Click(scope+` button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, id))
	r, err := c.GetRepository(ctx, &arpb.GetRepositoryRequest{Name: name})
	if err != nil || r.GetFormat() != arpb.Repository_DOCKER || r.GetDescription() != "made in the browser" || r.GetLabels()["team"] != "web" {
		t.Fatalf("GetRepository after Create repository = %v, %v; want a DOCKER repository with the form's description and label", r, err)
	}

	p.navigate("/artifact-registry/" + id + "?project=" + project)
	p.pageAction("Edit repository", map[string]string{"description": "edited in the browser", "labels": "team=api"}, "")
	r, err = c.GetRepository(ctx, &arpb.GetRepositoryRequest{Name: name})
	if err != nil || r.GetDescription() != "edited in the browser" || r.GetLabels()["team"] != "api" {
		t.Errorf("GetRepository after Edit repository = %v, %v; want the new description and label", r, err)
	}
}
