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

	cloudbuildv2 "cloud.google.com/go/cloudbuild/apiv2"
	cb2pb "cloud.google.com/go/cloudbuild/apiv2/cloudbuildpb"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// TestCloudBuildRepositories2ndGenThroughThePage (#2197), in the run
// shard. Cloud Build > Repositories' 2nd gen tab, as Google's "Connect to
// a GitLab host" documents it: Create host connection with GitLab, a
// region, a name and the two access tokens (which the console stores in
// Secret Manager) makes a connection to this instance's git server that
// the official client reads installed; Link repository links one of the
// server's Secure Source Manager repositories, read back by GetRepository
// with its git URL; both are listed; the linked repository's Delete and
// then the connection's, each confirmed, delete them. Create host
// connection is reached and opened by keyboard, and the tab and the form
// pass the WCAG 2.1 AA audit.
func TestCloudBuildRepositories2ndGenThroughThePage(t *testing.T) {
	needService(t, "cloud-build-repositories")
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envRun)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: every change is read back through Cloud Build itself", envRun)
	}
	ctx := context.Background()
	rm, err := cloudbuildv2.NewRepositoryManagerClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rm.Close() })
	project := uniqueProject(t)
	inst := fmt.Sprintf("browser-cb2-%d", time.Now().UnixNano()%1e6)
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/secure-source-manager?project="+project,
		`{"name":"`+inst+`","region":"us-central1"}`); code != http.StatusOK {
		t.Fatalf("create instance %s through the console API = %d: %s", inst, code, body)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/secure-source-manager?project="+project+"&name="+inst, "")
	})
	b, _ := json.Marshal(map[string]any{"Path": []string{inst}, "Action": "create-repo", "Values": map[string]string{"id": "web"}})
	if code, body := consoleDo(t, http.MethodPost, "/api/actions/secure-source-manager?project="+project, string(b)); code != http.StatusOK {
		t.Fatalf("create a repository through the console API = %d: %s", code, body)
	}
	loc := "projects/" + project + "/locations/us-central1"
	ssmRepo, conn := loc+"/repositories/web", loc+"/connections/gl"
	repo := conn + "/repositories/web"
	t.Cleanup(func() {
		if op, err := rm.DeleteConnection(context.Background(), &cb2pb.DeleteConnectionRequest{Name: conn}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	path := "/cloud-build/repositories?project=" + project

	t.Run("keyboard", func(t *testing.T) {
		p := open(t)
		p.navigate(path)
		p.waitFor(`document.querySelector("#view").textContent.includes("2nd gen")`)
		p.tabTo(`document.activeElement.tagName === "BUTTON" && document.activeElement.textContent.trim() === "Create host connection"`)
		p.run(chromedp.KeyEvent(kb.Enter))
		p.waitFor(`document.querySelector(".modal.is-open #f-provider") !== null`)
		var provider string
		p.eval(`document.querySelector(".modal.is-open #f-provider").value`, &provider)
		if provider != "GitLab" {
			t.Errorf("Create host connection opens on provider %q; want GitLab", provider)
		}
		p.run(chromedp.KeyEvent(kb.Escape))
		p.waitFor(`document.querySelector(".modal.is-open") === null`)
	})

	auditScreens(t, []a11yPage{
		{"cloud build repositories 2nd gen", path, `document.querySelector("#view").textContent.includes("2nd gen")`, ""},
		{"create host connection", path, `document.querySelector("#view").textContent.includes("2nd gen")`,
			`[...document.querySelectorAll("button")].find((b) => b.textContent.trim() === "Create host connection").click()`},
	})

	p := open(t)
	p.navigate(path)
	p.waitFor(`document.querySelector("#view").textContent.includes("2nd gen")`)
	p.pageAction("Create host connection", map[string]string{"provider": "GitLab", "region": "us-central1", "id": "gl",
		"apiToken": "glpat-api-token", "readToken": "glpat-read-token"}, "")
	c, err := rm.GetConnection(ctx, &cb2pb.GetConnectionRequest{Name: conn})
	if err != nil || c.GetInstallationState().GetStage() != cb2pb.InstallationState_COMPLETE || c.GetGitlabConfig() == nil ||
		!strings.HasPrefix(c.GetGitlabConfig().GetAuthorizerCredential().GetUserTokenSecretVersion(), "projects/"+project+"/secrets/") {
		t.Fatalf("GetConnection after Create host connection = %v, %v; want an installed GitLab connection whose tokens are secrets", c, err)
	}
	p.navigate(path)
	p.pageAction("Link repository", map[string]string{"connection": conn, "repository": ssmRepo, "id": "web"}, "")
	r, err := rm.GetRepository(ctx, &cb2pb.GetRepositoryRequest{Name: repo})
	if err != nil || !strings.HasSuffix(r.GetRemoteUri(), "/sourcemanager/git/"+project+"/us-central1/web.git") {
		t.Fatalf("GetRepository after Link repository = %v, %v", r, err)
	}
	p.navigate(path)
	p.waitFor(fmt.Sprintf(`document.querySelector("#view").textContent.includes(%q)`, r.GetRemoteUri()))
	var listed bool
	p.eval(`[...document.querySelectorAll("#view tbody tr")].some((tr) => tr.textContent.includes("Host connection") && tr.textContent.includes("GitLab"))`, &listed)
	if !listed {
		t.Error("the 2nd gen tab does not list the GitLab host connection")
	}

	p.rowAction("2nd gen", "web", "Delete", nil, "web")
	if _, err := rm.GetRepository(ctx, &cb2pb.GetRepositoryRequest{Name: repo}); status.Code(err) != codes.NotFound {
		t.Errorf("GetRepository after Delete = %v; want NotFound", err)
	}
	p.navigate(path)
	p.rowAction("2nd gen", "gl", "Delete", nil, "gl")
	if _, err := rm.GetConnection(ctx, &cb2pb.GetConnectionRequest{Name: conn}); status.Code(err) != codes.NotFound {
		t.Errorf("GetConnection after Delete = %v; want NotFound", err)
	}
}
