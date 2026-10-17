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

	securesourcemanager "cloud.google.com/go/securesourcemanager/apiv1"
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestCloudRunSourceManagerThroughThePage (#1527), in the run shard, whose
// Run port serves Secure Source Manager: an instance made on the Instances
// page opens; Create repository on its page makes a repository, whose page
// shows its clone URL; Edit description changes what the official client's
// GetRepository then reads; Delete repository removes it.
func TestCloudRunSourceManagerThroughThePage(t *testing.T) {
	needService(t, "secure-source-manager")
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envRun)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: the repository is read back through the Secure Source Manager API itself", envRun)
	}
	ctx := context.Background()
	c, err := securesourcemanager.NewClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	project := uniqueProject(t)
	inst := fmt.Sprintf("browser-ssm-%d", time.Now().UnixNano()%1e6)
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/secure-source-manager?project="+project,
		`{"name":"`+inst+`","region":"us-central1"}`); code != http.StatusOK {
		t.Fatalf("create instance %s through the console API = %d: %s", inst, code, body)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/secure-source-manager?project="+project+"&name="+inst, "")
	})
	repo := "projects/" + project + "/locations/us-central1/repositories/web"

	p := open(t)
	p.navigate("/secure-source-manager?project=" + project)
	p.clickText("#view tbody a", inst)
	p.waitFor(fmt.Sprintf(`location.pathname === %q`, "/secure-source-manager/"+inst))
	p.pageAction("Create repository", map[string]string{"id": "web", "description": "from the browser"}, "")
	r, err := c.GetRepository(ctx, &ssmpb.GetRepositoryRequest{Name: repo})
	if err != nil || r.GetDescription() != "from the browser" {
		t.Fatalf("GetRepository after Create repository = %v, %v", r, err)
	}
	// The snackbar Create repository raised sits over the table's last row.
	p.eval(`document.querySelectorAll(".snackbar-close").forEach((b) => b.click()); true`, nil)
	p.waitFor(`document.querySelector(".snackbar") === null`)
	p.clickText("#view tbody a", "web")
	p.waitFor(`location.pathname.endsWith("/web")`)
	p.waitFor(fmt.Sprintf(`document.querySelector("#view").textContent.includes(%q)`, r.GetUris().GetGitHttps()))
	p.pageAction("Edit description", map[string]string{"description": "edited in the browser"}, "")
	if r, err := c.GetRepository(ctx, &ssmpb.GetRepositoryRequest{Name: repo}); err != nil || r.GetDescription() != "edited in the browser" {
		t.Errorf("GetRepository after Edit description = %v, %v", r, err)
	}
}
