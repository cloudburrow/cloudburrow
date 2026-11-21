//go:build browser

package browser

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	orgpolicy "cloud.google.com/go/orgpolicy/apiv2"
	"cloud.google.com/go/orgpolicy/apiv2/orgpolicypb"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"github.com/chromedp/chromedp"
	crmv3 "google.golang.org/api/cloudresourcemanager/v3"
	"google.golang.org/api/option"
)

// pickResource opens the toolbar's Select a resource and chooses the
// folder or organization row whose text includes name.
func (p *tab) pickResource(kind, name string) {
	p.t.Helper()
	p.run(chromedp.Click(chromedp.CSS(`#project-button`)))
	row := fmt.Sprintf(`[...document.querySelectorAll('#project-list li[data-kind=%q] a')].find((a) => a.textContent.includes(%q))`, kind, name)
	p.waitFor(row + ` !== undefined`)
	p.eval(row+`.click(), true`, nil)
	p.waitFor(`document.getElementById("project-panel").hidden`)
}

// TestResourceSelectorScopesIAMOrgPoliciesAndTags (#2023): choosing a
// folder in the toolbar's Select a resource on IAM keeps the page, now the
// folder's (?folder=N, the toolbar naming the folder): it shows a binding
// the Folders API holds, and a grant made from the page is the Folders
// API's SetIamPolicy, not the project's. The drawer's Organization
// Policies keeps the folder and lists the folder's own policy, set with the
// Org Policy v2 client. Choosing the organization on Tags lists the
// organization's keys, not a project's. Choosing a project drops the
// folder, and a page that works per project only drops it too.
func TestResourceSelectorScopesIAMOrgPoliciesAndTags(t *testing.T) {
	needService(t, "projects")
	needService(t, "hierarchy")
	needService(t, "iam-orgpolicies")
	ctx := context.Background()
	opts := rmClientOptions(t)
	folders, err := resourcemanager.NewFoldersClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := resourcemanager.NewTagKeysClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	policies, err := orgpolicy.NewClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = folders.Close(); _ = keys.Close(); _ = policies.Close() })
	name := fmt.Sprintf("Scope %d", time.Now().UnixNano()%1e8)
	fop, err := folders.CreateFolder(ctx, &rmpb.CreateFolderRequest{Folder: &rmpb.Folder{Parent: "organizations/100000000001", DisplayName: name}})
	if err != nil {
		t.Fatal(err)
	}
	folder, err := fop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	folderID := strings.TrimPrefix(folder.GetName(), "folders/")
	constraint := "iam.disableServiceAccountKeyCreation"
	t.Cleanup(func() {
		_ = policies.DeletePolicy(context.Background(), &orgpolicypb.DeletePolicyRequest{Name: folder.GetName() + "/policies/" + constraint})
		if op, err := folders.DeleteFolder(context.Background(), &rmpb.DeleteFolderRequest{Name: folder.GetName()}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})
	pol, err := folders.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: folder.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	pol.Bindings = append(pol.Bindings, &iampb.Binding{Role: "roles/viewer", Members: []string{"user:folder-reader@example.com"}})
	if _, err := folders.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: folder.GetName(), Policy: pol}); err != nil {
		t.Fatal(err)
	}
	if _, err := policies.CreatePolicy(ctx, &orgpolicypb.CreatePolicyRequest{Parent: folder.GetName(), Policy: &orgpolicypb.Policy{
		Name: folder.GetName() + "/policies/" + constraint,
		Spec: &orgpolicypb.PolicySpec{Rules: []*orgpolicypb.PolicySpec_PolicyRule{{Kind: &orgpolicypb.PolicySpec_PolicyRule_Enforce{Enforce: true}}}}}}); err != nil {
		t.Fatalf("CreatePolicy on the folder: %v", err)
	}
	project := uniqueProject(t)
	short := fmt.Sprintf("pk-%d", time.Now().UnixNano()%1e8)
	kop, err := keys.CreateTagKey(ctx, &rmpb.CreateTagKeyRequest{TagKey: &rmpb.TagKey{Parent: "projects/" + project, ShortName: short}})
	if err != nil {
		t.Fatal(err)
	}
	pkey, err := kop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := keys.DeleteTagKey(context.Background(), &rmpb.DeleteTagKeyRequest{Name: pkey.GetName()}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})

	p := open(t)
	p.navigate("/iam-admin/iam?project=" + project)
	p.waitFor(`document.getElementById("iam-scope-title") !== null`)
	p.pickResource("folder", name)
	p.waitFor(`location.pathname === "/iam-admin/iam" && new URLSearchParams(location.search).get("folder") === "` + folderID + `"`)
	p.waitFor(fmt.Sprintf(`document.getElementById("iam-scope-title") && document.getElementById("iam-scope-title").textContent === %q`,
		`Permissions for folder "`+name+`"`))
	var toolbar string
	p.eval(`document.getElementById("project-current").textContent`, &toolbar)
	if toolbar != name {
		t.Errorf("the toolbar names %q; want the folder %q", toolbar, name)
	}
	p.waitFor(`document.getElementById("iam-body").textContent.includes("folder-reader@example.com")`)

	// A grant made from the page, through its own calls.
	p.eval(`(async () => { const d = await iamCall("policy");
		await iamCall("change", { etag: d.etag, members: ["user:granted-here@example.com"], add: [{ role: "roles/browser" }], remove: [] });
		return true; })()`, nil)
	var granted bool
	for deadline := time.Now().Add(15 * time.Second); !granted && time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		got, err := folders.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: folder.GetName()})
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range got.GetBindings() {
			granted = granted || b.GetRole() == "roles/browser" && strings.Contains(strings.Join(b.GetMembers(), ","), "granted-here@example.com")
		}
	}
	if !granted {
		t.Error("the grant made with the folder chosen is not on the folder's policy")
	}

	// The drawer's Organization Policies keeps the folder.
	link := `[...document.querySelectorAll("a[href]")].find((a) => new URL(a.href).pathname === "/iam-admin/orgpolicies")`
	p.waitFor(link + ` !== undefined && ` + link + `.href.includes("folder=` + folderID + `")`)
	p.eval(link+`.click(), true`, nil)
	p.waitFor(`location.pathname === "/iam-admin/orgpolicies"`)
	row := fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes(%q))`, "constraints/"+constraint)
	p.waitFor(row + ` !== undefined && ` + row + `.textContent.includes("Override parent's policy")`)
	p.eval(`document.getElementById("project-current").textContent`, &toolbar)
	if toolbar != name {
		t.Errorf("on Organization Policies the toolbar names %q; want the folder", toolbar)
	}

	// The organization on Tags.
	p.navigate("/iam-admin/tags?project=" + project)
	p.waitFor(fmt.Sprintf(`document.querySelector("#view").textContent.includes(%q)`, short))
	p.pickResource("organization", "")
	p.waitFor(`new URLSearchParams(location.search).get("organizationId") === "100000000001" && location.pathname === "/iam-admin/tags"`)
	p.waitFor(fmt.Sprintf(`document.querySelector("#view table, #view .empty-state") !== null && !document.querySelector("#view").textContent.includes(%q)`, short))

	// A project chosen drops the organization.
	p.run(chromedp.Click(chromedp.CSS(`#project-button`)))
	prow := fmt.Sprintf(`[...document.querySelectorAll('#project-list li[data-kind="project"] button.picker-item')].find((b) => b.textContent.includes(%q))`, project)
	p.waitFor(prow + ` !== undefined`)
	p.eval(prow+`.click(), true`, nil)
	p.waitFor(`!location.search.includes("organizationId") && document.getElementById("project-current").textContent === "` + project + `"`)
	p.waitFor(fmt.Sprintf(`document.querySelector("#view").textContent.includes(%q)`, short))

	// A per-project page drops a folder in the address.
	p.navigate("/projects?project=" + project + "&folder=" + folderID)
	p.waitFor(`!location.search.includes("folder=") && document.getElementById("project-current").textContent === "` + project + `"`)
}

// TestDynamicTagValuesThroughTheBrowser (#2023): Create tag key's Dynamic
// values, Add value constraints and a validation regular expression make a
// key whose allowedValuesRegex the cloudresourcemanager/v3 REST client
// reads; the key's page shows the regex; Manage resources' Tags panel types
// a value for it, refused when it does not match, and attaches one that
// does, which the TagBindings API lists by its namespaced name.
func TestDynamicTagValuesThroughTheBrowser(t *testing.T) {
	needService(t, "projects")
	needService(t, "hierarchy")
	ctx := context.Background()
	opts := rmClientOptions(t)
	keys, err := resourcemanager.NewTagKeysClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := resourcemanager.NewTagBindingsClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = keys.Close(); _ = bindings.Close() })
	v3, err := crmv3.NewService(ctx, option.WithEndpoint("http://"+endpoint(t, envResourceManager)+"/"),
		option.WithoutAuthentication(), option.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}
	project := uniqueProject(t)
	short := fmt.Sprintf("dk-%d", time.Now().UnixNano()%1e8)
	ns := "100000000001/" + short
	target := "//cloudresourcemanager.googleapis.com/projects/" + project

	p := open(t)
	p.navigate("/tags/create?project=" + project)
	p.waitFor(`document.querySelector("#view form.create-page #f-shortName") !== null`)
	p.setField("#f-shortName", short)
	p.eval(`(() => { const r = [...document.querySelectorAll("#view form.create-page input[type=radio]")].find((x) => x.value === "dynamic");
		r.click(); r.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, nil)
	p.waitFor(`document.querySelector("#f-valueConstraints") && !document.querySelector("#f-valueConstraints").closest("[hidden]")`)
	var hidden bool
	p.eval(`!!document.querySelector("#f-allowedValuesRegex").closest("[hidden]")`, &hidden)
	if !hidden {
		t.Error("the validation regular expression shows before Add value constraints is checked")
	}
	p.run(chromedp.Click(chromedp.CSS(`#f-valueConstraints`)))
	p.waitFor(`!document.querySelector("#f-allowedValuesRegex").closest("[hidden]")`)
	p.setField("#f-allowedValuesRegex", "env-[a-z]+")
	p.run(chromedp.Click(chromedp.CSS(`#view form.create-page button[type="submit"]`)))
	p.waitFor(`document.querySelector("#view form.create-page") === null`)
	var key *rmpb.TagKey
	for deadline := time.Now().Add(15 * time.Second); key == nil; time.Sleep(250 * time.Millisecond) {
		key, _ = keys.GetNamespacedTagKey(ctx, &rmpb.GetNamespacedTagKeyRequest{Name: ns})
		if key == nil && time.Now().After(deadline) {
			t.Fatalf("no tag key %s after Create tag key", ns)
		}
	}
	t.Cleanup(func() {
		it := bindings.ListTagBindings(context.Background(), &rmpb.ListTagBindingsRequest{Parent: target})
		for b, err := it.Next(); err == nil; b, err = it.Next() {
			if op, err := bindings.DeleteTagBinding(context.Background(), &rmpb.DeleteTagBindingRequest{Name: b.GetName()}); err == nil {
				_ = op.Wait(context.Background())
			}
		}
		if op, err := keys.DeleteTagKey(context.Background(), &rmpb.DeleteTagKeyRequest{Name: key.GetName()}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})
	rk, err := v3.TagKeys.Get(key.GetName()).Context(ctx).Do()
	if err != nil || rk.AllowedValuesRegex != "env-[a-z]+" {
		t.Fatalf("the REST TagKey = %+v, %v; want allowedValuesRegex env-[a-z]+", rk, err)
	}
	id := strings.TrimPrefix(key.GetName(), "tagKeys/")
	p.navigate("/tags/" + id + "?project=" + project)
	p.waitFor(`document.querySelector("#view").textContent.includes("Validation regular expression") && document.querySelector("#view").textContent.includes("env-[a-z]+")`)

	p.navigate("/cloud-resource-manager?project=" + project)
	p.mrMenu("project", project, project, "Tags")
	p.waitFor(`document.querySelector("#mr-panel-select-scope") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#mr-panel-select-scope`)))
	p.mrSubmitTop("scope", "100000000001")
	p.waitFor(`document.querySelector("#mr-panel-add-tag") !== null && !document.querySelector("#mr-panel-add-tag").disabled`)
	attach := func(value string) {
		p.run(chromedp.Click(chromedp.CSS(`#mr-panel-add-tag`)))
		p.waitFor(`[...document.querySelectorAll("#mr-panel select.mr-tag-key")].length > 0`)
		p.eval(fmt.Sprintf(`(() => { const k = [...document.querySelectorAll("#mr-panel select.mr-tag-key")].pop();
			k.value = %q; k.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, short), nil)
		p.waitFor(`[...document.querySelectorAll("#mr-panel input.mr-tag-dynamic")].length > 0`)
		var help string
		p.eval(`(() => { const v = [...document.querySelectorAll("#mr-panel input.mr-tag-dynamic")].pop();
			return document.getElementById(v.getAttribute("aria-describedby")).textContent; })()`, &help)
		if !strings.Contains(help, "env-[a-z]+") {
			t.Errorf("the typed value's help reads %q; want the key's regex", help)
		}
		p.eval(fmt.Sprintf(`(() => { const v = [...document.querySelectorAll("#mr-panel input.mr-tag-dynamic")].pop();
			v.value = %q; v.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, value), nil)
		p.waitFor(`!document.querySelector("#mr-panel-tags-save").disabled`)
		p.run(chromedp.Click(chromedp.CSS(`#mr-panel-tags-save`)))
		p.waitFor(`document.querySelector("#mr-tag-confirm") !== null`)
		p.run(chromedp.Click(chromedp.CSS(`#mr-tag-confirm`)))
	}
	// The refused value is a 400 from the tag call, which the page shows.
	p.mu.Lock()
	p.expected = append(p.expected, "/api/studio/projects?call=tag")
	p.mu.Unlock()
	attach("Prod")
	p.waitFor(`[...document.querySelectorAll(".modal.is-open .form-error")].some((e) => !e.hidden && e.textContent.includes("does not match"))`)
	p.eval(`(() => { const m = [...document.querySelectorAll(".modal.is-open")].pop(); m.querySelector(".modal-actions button.secondary").click(); return true; })()`, nil)
	p.waitFor(`document.querySelector("#mr-tag-confirm") === null`)
	p.eval(`(() => { for (const b of document.querySelectorAll('#mr-panel button[aria-label^="Delete item"]')) b.click(); return true; })()`, nil)
	attach("env-prod")
	p.waitFor(`document.querySelector("#mr-tag-confirm") === null`)
	var bound string
	for deadline := time.Now().Add(15 * time.Second); bound == "" && time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		if b, err := bindings.ListTagBindings(ctx, &rmpb.ListTagBindingsRequest{Parent: target}).Next(); err == nil {
			bound = b.GetTagValueNamespacedName()
		}
	}
	if bound != ns+"/env-prod" {
		t.Errorf("the project's binding = %q; want %s/env-prod", bound, ns)
	}
}

// TestResourceManagerPagesOf2023MeetWCAG: IAM with a folder chosen in the
// toolbar, Create tag key with Dynamic values and its constraints shown,
// and Manage resources' Tags panel.
func TestResourceManagerPagesOf2023MeetWCAG(t *testing.T) {
	needService(t, "projects")
	needService(t, "hierarchy")
	ctx := context.Background()
	folders, err := resourcemanager.NewFoldersClient(ctx, rmClientOptions(t)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = folders.Close() })
	fop, err := folders.CreateFolder(ctx, &rmpb.CreateFolderRequest{Folder: &rmpb.Folder{Parent: "organizations/100000000001",
		DisplayName: fmt.Sprintf("WCAG %d", time.Now().UnixNano()%1e8)}})
	if err != nil {
		t.Fatal(err)
	}
	folder, err := fop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := folders.DeleteFolder(context.Background(), &rmpb.DeleteFolderRequest{Name: folder.GetName()}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})
	folderID := strings.TrimPrefix(folder.GetName(), "folders/")
	project := uniqueProject(t)
	dynamic := `(() => { const r = [...document.querySelectorAll("#view form.create-page input[type=radio]")].find((x) => x.value === "dynamic");
		r.click(); r.dispatchEvent(new Event("change", { bubbles: true }));
		const c = document.getElementById("f-valueConstraints"); c.click(); })()`
	auditScreens(t, []a11yPage{
		{"IAM for a folder", "/iam-admin/iam?project=" + project + "&folder=" + folderID,
			`document.getElementById("iam-scope-title") !== null && document.getElementById("iam-scope-title").textContent.includes("folder")`, ""},
		{"organization policies for a folder", "/iam-admin/orgpolicies?project=" + project + "&folder=" + folderID,
			`document.querySelector("#view tbody tr") !== null`, ""},
		{"resource selector with a folder chosen", "/iam-admin/iam?project=" + project + "&folder=" + folderID, settled,
			`document.getElementById("project-button").click()`},
		{"create tag key with dynamic values", "/tags/create?project=" + project,
			`document.querySelector("#view form.create-page #f-shortName") !== null`, dynamic},
	})
}
