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

// arClient is the official Artifact Registry client on the Run port, or a
// skip when the shard does not export it.
func arClient(t *testing.T) *artifactregistry.Client {
	t.Helper()
	needService(t, "artifact-registry")
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envRun)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: the repository is read back through the Artifact Registry API itself", envRun)
	}
	c, err := artifactregistry.NewClient(context.Background(), option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// openCreateRepository opens Create Repository from the Repositories page.
func openCreateRepository(p *tab, project string) {
	p.navigate("/artifact-registry?project=" + project)
	p.clickText("#view button", "Create Repository")
	p.waitFor(`location.pathname === "/artifact-registry/create" && document.querySelector("#view #f-id") !== null`)
}

// clickRadio chooses one of a radio field's options by its index.
func clickRadio(p *tab, field string, i int) {
	p.run(chromedp.Click(chromedp.CSS(fmt.Sprintf("#view #f-%s-%d", field, i))))
}

// TestCloudRunArtifactRegistryCreateAndEditRepositoryThroughThePage, in
// the run shard, whose Run port serves Artifact Registry (#1855). The
// product's menu is Repositories and Settings; the Repositories page has
// Google's columns; Create Repository is Google's page, whose Format lists
// Go and Kubeflow Pipelines enabled (#1897), whose Mode reveals each mode's
// settings and whose Location type switches Region for Multi-region, and a
// Docker repository made with its labels, a customer-managed key,
// immutable image tags and a cleanup policy in dry run reads back so
// through the official client's GetRepository. On its page, Setup
// Instructions opens the tab with this instance's docker push address, and
// Edit Repository changes the description and labels and turns the
// immutable tags off.
func TestCloudRunArtifactRegistryCreateAndEditRepositoryThroughThePage(t *testing.T) {
	c := arClient(t)
	ctx := context.Background()
	project := uniqueProject(t)
	id := fmt.Sprintf("browser-repo-%d", time.Now().UnixNano()%1e6)
	name := "projects/" + project + "/locations/us-east1/repositories/" + id
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/artifact-registry?project="+project+"&name="+id, "")
	})

	p := open(t)
	p.navigate("/artifact-registry?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#product-nav a")].map((a) => a.textContent.trim()).join(",").includes("Repositories,Settings")`)

	openCreateRepository(p, project)
	hidden := func(id string) bool {
		var h bool
		p.eval(fmt.Sprintf(`document.querySelector("#view #%s").closest(".form-row").hidden`, id), &h)
		return h
	}
	var off struct{ KFP, Go, Scanning bool }
	p.eval(`(() => ({ KFP: document.querySelector("#view #f-format-6").disabled, Go: document.querySelector("#view #f-format-7").disabled,
		Scanning: document.querySelector("#view #f-vulnerabilityScanning-0").disabled }))()`, &off)
	if off.KFP || off.Go || !off.Scanning {
		t.Errorf("Create Repository draws %+v; want Kubeflow Pipelines and Go enabled (#1897), vulnerability scanning disabled", off)
	}
	if !hidden("f-upstream") || hidden("f-location") || !hidden("f-multiRegion") || !hidden("f-kmsKey") {
		t.Error("Create Repository shows a remote upstream, the multi-regions or the key before their choice is made")
	}
	clickRadio(p, "mode", 1) // Remote
	if hidden("f-upstream") || hidden("f-remoteSource-2") {
		t.Error("Remote does not reveal the remote repository source and its upstream")
	}
	clickRadio(p, "mode", 2) // Virtual
	if !hidden("f-upstream") {
		t.Error("Virtual still shows the remote upstream")
	}
	clickRadio(p, "mode", 0) // Standard
	clickRadio(p, "locationType", 1)
	if !hidden("f-location") || hidden("f-multiRegion") {
		t.Error("Multi-Region does not swap the Region list for the Multi-region list")
	}
	clickRadio(p, "locationType", 0)
	setField(p, "#view", "f-id", id)
	setField(p, "#view", "f-location", "us-east1")
	setField(p, "#view", "f-description", "made in the browser")
	setField(p, "#view", "f-labels", "team=web")
	clickRadio(p, "encryption", 1)
	key := "projects/" + project + "/locations/us-east1/keyRings/ring/cryptoKeys/key"
	setField(p, "#view", "f-kmsKey", key)
	clickRadio(p, "immutableTags", 0) // Enabled
	clickRadio(p, "cleanupMode", 0)   // Dry run
	p.clickText("#view button", "Add a cleanup policy")
	p.eval(`(() => { const row = document.querySelector("#view .group-row");
		const set = (label, v) => { const c = row.querySelector('[aria-label="' + label + ' 1"]'); c.value = v;
			c.dispatchEvent(new Event("input", { bubbles: true })); c.dispatchEvent(new Event("change", { bubbles: true })); };
		set("Name", "untagged"); set("Policy type", "Conditional delete"); set("Tag state", "Untagged"); set("Older than", "30d");
		return true; })()`, nil)
	p.run(chromedp.Click(chromedp.CSS(`#view button[type="submit"]`)))
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, id))
	r, err := c.GetRepository(ctx, &arpb.GetRepositoryRequest{Name: name})
	if err != nil || r.GetFormat() != arpb.Repository_DOCKER || r.GetDescription() != "made in the browser" || r.GetLabels()["team"] != "web" ||
		r.GetKmsKeyName() != key || !r.GetDockerConfig().GetImmutableTags() || !r.GetCleanupPolicyDryRun() ||
		r.GetCleanupPolicies()["untagged"].GetCondition().GetTagState() != arpb.CleanupPolicyCondition_UNTAGGED ||
		r.GetCleanupPolicies()["untagged"].GetCondition().GetOlderThan().GetSeconds() != 30*86400 {
		t.Fatalf("GetRepository after Create Repository = %v, %v; want the page's settings", r, err)
	}
	var heads string
	p.eval(`[...document.querySelectorAll("#view thead th")].map((h) => h.textContent.trim()).filter(Boolean).join(",")`, &heads)
	if !strings.Contains(heads, "Name,Format,Mode,Description,Location,Labels,Encryption,Cleanup policies,Size,Created,Updated") {
		t.Errorf("the Repositories columns are %q; want Google's", heads)
	}

	p.navigate("/artifact-registry/" + id + "?project=" + project)
	p.clickText("#view .page-actions a, #view .page-actions button", "Setup Instructions")
	p.waitFor(`new URLSearchParams(location.search).get("tab") === "setup"`)
	p.waitFor(`document.querySelector("#view").textContent.includes("docker push us-east1-docker.pkg.localhost:")`)
	p.pageAction("Edit Repository", map[string]string{"description": "edited in the browser", "labels": "team=api"}, "")
	r, err = c.GetRepository(ctx, &arpb.GetRepositoryRequest{Name: name})
	if err != nil || r.GetDescription() != "edited in the browser" || r.GetLabels()["team"] != "api" || len(r.GetCleanupPolicies()) != 1 {
		t.Errorf("GetRepository after Edit Repository = %v, %v; want the new description and label, the policy kept", r, err)
	}
}

// TestCloudRunArtifactRegistryEditRuleAndCleanupPoliciesThroughThePage
// (#1491, #1855), in the run shard. On a repository's page, Edit rule
// changes the action the official client's GetRule then reads, and Edit
// Repository's Cleanup policies rows (Add a cleanup policy: Keep most
// recent versions with Keep count) and Dry run set the policies and dry
// run GetRepository reads.
func TestCloudRunArtifactRegistryEditRuleAndCleanupPoliciesThroughThePage(t *testing.T) {
	c := arClient(t)
	ctx := context.Background()
	project := uniqueProject(t)
	id := fmt.Sprintf("browser-rules-%d", time.Now().UnixNano()%1e6)
	name := "projects/" + project + "/locations/us-east1/repositories/" + id
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/artifact-registry?project="+project,
		`{"id":"`+id+`","format":"DOCKER","location":"us-east1"}`); code != http.StatusOK {
		t.Fatalf("create repository %s through the console API = %d: %s", id, code, body)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/artifact-registry?project="+project+"&name="+id, "")
	})
	if _, err := c.CreateRule(ctx, &arpb.CreateRuleRequest{Parent: name, RuleId: "deny-all",
		Rule: &arpb.Rule{Action: arpb.Rule_DENY, Operation: arpb.Rule_DOWNLOAD}}); err != nil {
		t.Fatal(err)
	}

	p := open(t)
	page := "/artifact-registry/" + id + "?project=" + project
	p.navigate(page)
	p.pageAction("Edit rule", map[string]string{"id": "deny-all", "action": "ALLOW"}, "")
	if r, err := c.GetRule(ctx, &arpb.GetRuleRequest{Name: name + "/rules/deny-all"}); err != nil || r.GetAction() != arpb.Rule_ALLOW {
		t.Errorf("GetRule after Edit rule = %v, %v; want ALLOW", r, err)
	}
	p.navigate(page)
	p.waitFor(`document.querySelector(".modal") === null`)
	p.clickText("#view .page-actions button", "Edit Repository")
	p.waitFor(`document.querySelector(".modal.is-open form") !== null`)
	p.run(chromedp.Click(chromedp.CSS(".modal.is-open #f-cleanupMode-0")))
	p.clickText(".modal.is-open button", "Add a cleanup policy")
	p.eval(`(() => { const row = document.querySelector(".modal.is-open .group-row");
		const set = (label, v) => { const c = row.querySelector('[aria-label="' + label + ' 1"]'); c.value = v;
			c.dispatchEvent(new Event("input", { bubbles: true })); c.dispatchEvent(new Event("change", { bubbles: true })); };
		set("Name", "keep-5"); set("Policy type", "Keep most recent versions"); set("Keep count", "5");
		return true; })()`, nil)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	r, err := c.GetRepository(ctx, &arpb.GetRepositoryRequest{Name: name})
	if err != nil || !r.GetCleanupPolicyDryRun() || r.GetCleanupPolicies()["keep-5"].GetMostRecentVersions().GetKeepCount() != 5 {
		t.Errorf("GetRepository after Edit Repository's cleanup policies = %v, %v; want keep-5 in dry run", r, err)
	}
}

// TestCloudRunArtifactRegistrySettingsThroughThePage (#1855), in the run
// shard: Artifact Registry's Settings page lists the project's pull percent
// and, after Set VPC-SC policy, the location's policy, which the official
// client's GetVPCSCConfig reads.
func TestCloudRunArtifactRegistrySettingsThroughThePage(t *testing.T) {
	c := arClient(t)
	ctx := context.Background()
	project := uniqueProject(t)
	p := open(t)
	p.navigate("/artifact-registry-settings?project=" + project)
	p.waitFor(`document.querySelector("#view").textContent.includes("Pull percent")`)
	p.waitFor(`document.querySelector(".modal") === null`)
	p.clickText("#view button", "Set VPC-SC policy")
	p.waitFor(`document.querySelector(".modal.is-open #f-location") !== null`)
	setField(p, ".modal.is-open", "f-location", "europe-west1")
	setField(p, ".modal.is-open", "f-policy", "ALLOW")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	cfg := "projects/" + project + "/locations/europe-west1/vpcscConfig"
	t.Cleanup(func() {
		_, _ = c.UpdateVPCSCConfig(context.Background(), &arpb.UpdateVPCSCConfigRequest{VpcscConfig: &arpb.VPCSCConfig{Name: cfg, VpcscPolicy: arpb.VPCSCConfig_DENY}})
	})
	if v, err := c.GetVPCSCConfig(ctx, &arpb.GetVPCSCConfigRequest{Name: cfg}); err != nil || v.GetVpcscPolicy() != arpb.VPCSCConfig_ALLOW {
		t.Fatalf("GetVPCSCConfig after Set VPC-SC policy = %v, %v; want ALLOW", v, err)
	}
	p.waitFor(`document.querySelector("#view").textContent.includes("VPC-SC policy (europe-west1)")`)
}
