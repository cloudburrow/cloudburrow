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

	developerconnect "cloud.google.com/go/developerconnect/apiv1"
	"cloud.google.com/go/developerconnect/apiv1/developerconnectpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	securesourcemanager "cloud.google.com/go/securesourcemanager/apiv1"
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// grantOnTab grants member role on the open Permissions tab.
func (p *tab) grantOnTab(member, role string) {
	p.t.Helper()
	p.clickText("#view #grant-access", "Grant access")
	p.waitFor(`document.querySelector(".modal.is-open #f-members") !== null`)
	p.eval(fmt.Sprintf(`(() => { const q = (s) => document.querySelector(".modal.is-open " + s);
		q("#f-members").value = %q; q("#f-members").dispatchEvent(new Event("input", { bubbles: true }));
		q("#f-role").value = %q; q("#f-role").dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, member, role), nil)
	p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
}

// policyHas is whether pol binds member to role.
func policyHas(pol *iampb.Policy, member, role string) bool {
	for _, b := range pol.GetBindings() {
		if b.GetRole() == role {
			for _, m := range b.GetMembers() {
				if m == member {
					return true
				}
			}
		}
	}
	return false
}

// TestCloudRunSourceManagerPermissionsAndDeveloperConnectThroughThePage
// (#1592), in the run shard, whose Run port serves Secure Source Manager and
// Developer Connect:
//   - Grant access on an instance's Permissions tab binds a principal the
//     official client's GetIamPolicy reads; on a repository's, one
//     GetIamPolicyRepo reads, Test permissions answers Held, and Remove
//     principal, confirmed, unbinds it;
//   - Connect to Developer Connect on the instance's page makes a connection
//     GetConnection reads, Link repository on the repository's page a link
//     GetGitRepositoryLink reads with the repository's clone URL, and Delete
//     link and Delete connection, each confirmed, delete them.
func TestCloudRunSourceManagerPermissionsAndDeveloperConnectThroughThePage(t *testing.T) {
	needService(t, "secure-source-manager")
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envRun)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: every change is read back through Secure Source Manager and Developer Connect themselves", envRun)
	}
	ctx := context.Background()
	opts := []option.ClientOption{option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
	c, err := securesourcemanager.NewClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	dc, err := developerconnect.NewClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dc.Close() })
	project := uniqueProject(t)
	inst := fmt.Sprintf("browser-dc-%d", time.Now().UnixNano()%1e6)
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
	instName, repo := loc+"/instances/"+inst, loc+"/repositories/web"
	ada := "user:ada@example.com"

	p := open(t)
	instPage := func() {
		p.navigate("/secure-source-manager?project=" + project)
		p.clickText("#view tbody a", inst)
		p.waitFor(fmt.Sprintf(`location.pathname === %q`, "/secure-source-manager/"+inst))
	}
	repoPage := func() {
		instPage()
		p.clickText("#view tbody a", "web")
		p.waitFor(`location.pathname.endsWith("/web")`)
	}
	permissions := func() {
		p.clickText("#view .tab-strip .tab", "Permissions")
		p.waitFor(`document.querySelector("#permissions-note") !== null`)
	}

	instPage()
	permissions()
	p.grantOnTab(ada, "roles/securesourcemanager.instanceAccessor")
	if pol, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: instName}); err != nil || !policyHas(pol, ada, "roles/securesourcemanager.instanceAccessor") {
		t.Errorf("GetIamPolicy on the instance after Grant access = %v, %v", pol, err)
	}

	repoPage()
	permissions()
	const reader = "roles/securesourcemanager.repoReader"
	p.grantOnTab(ada, reader)
	if pol, err := c.GetIamPolicyRepo(ctx, &iampb.GetIamPolicyRequest{Resource: repo}); err != nil || !policyHas(pol, ada, reader) {
		t.Errorf("GetIamPolicyRepo after Grant access = %v, %v", pol, err)
	}
	p.clickText("#view #test-permissions", "Test permissions")
	p.waitFor(`document.querySelector(".modal.is-open #f-permissions") !== null`)
	p.eval(`(() => { const f = document.querySelector(".modal.is-open #f-permissions");
		f.value = "securesourcemanager.repos.get"; f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, nil)
	p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelectorAll("#test-permissions-table tbody tr").length === 1`)
	var tested string
	p.eval(`document.querySelector("#test-permissions-table tbody tr").textContent`, &tested)
	if tested != "securesourcemanager.repos.getHeld" {
		t.Errorf("Test permissions on the repository reads %q; want it Held", tested)
	}
	p.clickText(".modal.is-open .modal-actions button", "Close")
	p.waitFor(`document.querySelector(".modal") === null`)
	p.run(chromedp.Click(fmt.Sprintf(`button[aria-label=%q]`, "Remove "+ada+" from "+reader), chromedp.ByQuery))
	p.confirmByName(ada)
	p.waitFor(`document.querySelector(".modal") === null`)
	p.waitFor(`document.querySelectorAll("#permissions-table tbody tr").length === 0`)
	if pol, err := c.GetIamPolicyRepo(ctx, &iampb.GetIamPolicyRequest{Resource: repo}); err != nil || policyHas(pol, ada, reader) {
		t.Errorf("GetIamPolicyRepo after Remove principal = %v, %v", pol, err)
	}

	conn, link := loc+"/connections/conn", loc+"/connections/conn/gitRepositoryLinks/link"
	instPage()
	p.pageAction("Connect to Developer Connect", map[string]string{"id": "conn"}, "")
	if cn, err := dc.GetConnection(ctx, &developerconnectpb.GetConnectionRequest{Name: conn}); err != nil || cn.GetName() != conn {
		t.Fatalf("GetConnection after Connect to Developer Connect = %v, %v", cn, err)
	}
	repoPage()
	p.pageAction("Link repository", map[string]string{"connection": "conn", "id": "link"}, "")
	r, err := c.GetRepository(ctx, &ssmpb.GetRepositoryRequest{Name: repo})
	if err != nil {
		t.Fatal(err)
	}
	l, err := dc.GetGitRepositoryLink(ctx, &developerconnectpb.GetGitRepositoryLinkRequest{Name: link})
	if err != nil || l.GetCloneUri() != r.GetUris().GetGitHttps() {
		t.Fatalf("GetGitRepositoryLink after Link repository = %v, %v; want the clone URL %s", l, err, r.GetUris().GetGitHttps())
	}
	repoPage()
	p.pageAction("Delete link", map[string]string{"link": link}, "web")
	if _, err := dc.GetGitRepositoryLink(ctx, &developerconnectpb.GetGitRepositoryLinkRequest{Name: link}); status.Code(err) != codes.NotFound {
		t.Errorf("GetGitRepositoryLink after Delete link = %v; want NotFound", err)
	}
	instPage()
	p.pageAction("Delete connection", map[string]string{"connection": "conn"}, inst)
	if _, err := dc.GetConnection(ctx, &developerconnectpb.GetConnectionRequest{Name: conn}); status.Code(err) != codes.NotFound {
		t.Errorf("GetConnection after Delete connection = %v; want NotFound", err)
	}
}
