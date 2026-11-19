//go:build browser

package browser

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/iterator"
)

// mrRow is the Manage resources tree's row for a resource, as a selector.
func mrRow(kind, id string) string {
	return fmt.Sprintf(`#mr-tree tr[data-resource=%q]`, kind+":"+id)
}

// mrMenu chooses item from a row's Actions menu.
func (p *tab) mrMenu(kind, id, name, item string) {
	p.t.Helper()
	trigger := fmt.Sprintf(`%s button[aria-label=%q]`, mrRow(kind, id), "Actions for "+name)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, trigger))
	p.eval(fmt.Sprintf(`(() => { const b = document.querySelector(%q); b.scrollIntoView({ block: "center", inline: "center" }); b.click(); return true; })()`, trigger), nil)
	item = strings.TrimSpace(item)
	find := fmt.Sprintf(`[...document.querySelectorAll('[role="menuitem"]')].find((n) => n.textContent.trim() === %q && n.getClientRects().length)`, item)
	p.waitFor(find + ` !== undefined`)
	p.eval(find+`.click(), true`, nil)
}

// mrSubmitTop sets one field of the topmost open dialog, a form opened
// over the info panel, and submits it.
func (p *tab) mrSubmitTop(field, value string) {
	p.t.Helper()
	find := fmt.Sprintf(`[...document.querySelectorAll(".modal.is-open #f-%s")].pop()`, field)
	p.waitFor(find + ` !== undefined`)
	p.eval(fmt.Sprintf(`(() => { const f = %s;
		f.value = %q; f.dispatchEvent(new Event("input", { bubbles: true }));
		f.dispatchEvent(new Event("change", { bubbles: true }));
		f.form.querySelector('button[type="submit"]').click(); return true; })()`, find, value), nil)
	p.waitFor(fmt.Sprintf(`document.querySelector(".modal.is-open #f-%s") === null`, field))
}

// TestManageResourcesTreeThroughThePage (#1865), in the storage shard.
// Google's Manage resources page: the organization over its folders and
// their projects in one tree; Create folder makes a folder the v3 Folders
// API lists; a project made under it is its child row; Move takes the
// project to the organization; the info panel's Add principal is the
// project's SetIamPolicy, its Labels and Tags are the project's labels and
// tag bindings; Delete shuts the project down (DELETE_REQUESTED), and
// Restore under Resources pending deletion brings it back.
func TestManageResourcesTreeThroughThePage(t *testing.T) {
	needService(t, "projects")
	needService(t, "hierarchy")
	needService(t, "iam")
	opts := rmClientOptions(t)
	ctx := context.Background()
	folders, err := resourcemanager.NewFoldersClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	projects, err := resourcemanager.NewProjectsClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := resourcemanager.NewTagKeysClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	values, err := resourcemanager.NewTagValuesClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := resourcemanager.NewTagBindingsClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	orgs, err := resourcemanager.NewOrganizationsClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = folders.Close()
		_ = projects.Close()
		_ = keys.Close()
		_ = values.Close()
		_ = bindings.Close()
		_ = orgs.Close()
	})
	org, err := orgs.SearchOrganizations(ctx, &rmpb.SearchOrganizationsRequest{}).Next()
	if err != nil {
		t.Fatalf("SearchOrganizations: %v", err)
	}
	orgID := strings.TrimPrefix(org.GetName(), "organizations/")

	p := open(t)
	p.navigate("/iam-admin/iam")
	p.clickText("#product-nav a", "Manage resources")
	p.waitFor(`location.pathname === "/cloud-resource-manager"`)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, mrRow("organization", orgID)))
	var cols []string
	p.eval(`[...document.querySelectorAll("#mr-tree thead th")].filter((h) => !h.querySelector(".sr-only")).map((h) => h.textContent.trim())`, &cols)
	if want := []string{"Name", "ID", "Status", "Labels", "Tags"}; !slices.Equal(cols, want) {
		t.Errorf("the tree's columns are %q, want %q", cols, want)
	}

	// Create folder.
	folder := fmt.Sprintf("MR %d", time.Now().UnixNano()%1e8)
	p.run(chromedp.Click(chromedp.CSS(`#mr-create-folder-button`)))
	p.submitForm(map[string]string{"displayName": folder}, "displayName")
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#mr-tree tbody tr")].some((r) => r.textContent.includes(%q))`, folder))
	var made *rmpb.Folder
	for it := folders.ListFolders(ctx, &rmpb.ListFoldersRequest{Parent: org.GetName()}); ; {
		f, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListFolders: %v", err)
		}
		if f.GetDisplayName() == folder {
			made = f
		}
	}
	if made == nil {
		t.Fatalf("the v3 Folders API lists no folder %q under %s after Create folder", folder, org.GetName())
	}
	folderID := strings.TrimPrefix(made.GetName(), "folders/")

	// A project under the folder is its child row, one level down.
	project := fmt.Sprintf("mr-browser-%d", time.Now().UnixNano()%1e8)
	op, err := projects.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: project, Parent: made.GetName()}})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	t.Cleanup(func() {
		if op, err := projects.DeleteProject(context.Background(), &rmpb.DeleteProjectRequest{Name: "projects/" + project}); err == nil {
			_, _ = op.Wait(context.Background())
		}
		if op, err := folders.DeleteFolder(context.Background(), &rmpb.DeleteFolderRequest{Name: made.GetName()}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})
	p.clickText("#view button", "Refresh")
	p.waitFor(fmt.Sprintf(`(document.querySelector(%q) || {}).getAttribute?.("aria-level") === "3"`, mrRow("project", project)))
	var after string
	p.eval(fmt.Sprintf(`(() => { const r = document.querySelector(%q); return r.previousElementSibling ? r.previousElementSibling.dataset.resource : ""; })()`,
		mrRow("project", project)), &after)
	if after != "folder:"+folderID {
		t.Errorf("the project's row follows %q, want its folder's", after)
	}

	// Move the project to the organization.
	p.mrMenu("project", project, project, "Move")
	p.submitForm(map[string]string{"parent": org.GetName()}, "parent")
	parentOf := func() string {
		got, err := projects.GetProject(ctx, &rmpb.GetProjectRequest{Name: "projects/" + project})
		if err != nil {
			t.Fatalf("GetProject: %v", err)
		}
		return got.GetParent()
	}
	p.waitFor(fmt.Sprintf(`(document.querySelector(%q) || {}).getAttribute?.("aria-level") === "2"`, mrRow("project", project)))
	if got := parentOf(); got != org.GetName() {
		t.Errorf("after Move the project's parent is %q, want %s", got, org.GetName())
	}

	// The info panel: Add principal, Labels, Tags.
	p.run(chromedp.Click(chromedp.CSS(mrRow("project", project) + ` input[type=checkbox]`)))
	p.waitFor(`!document.querySelector("#mr-info-panel").disabled`)
	p.run(chromedp.Click(chromedp.CSS(`#mr-info-panel`)))
	p.waitFor(`document.querySelector("#mr-panel-add-principal") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#mr-panel-add-principal`)))
	p.waitFor(`document.querySelector(".modal.is-open #f-members") !== null`)
	const role = "roles/viewer"
	ada := fmt.Sprintf("user:mr-%d@example.com", time.Now().UnixNano()%1e8)
	p.eval(fmt.Sprintf(`(() => { const q = (s) => document.querySelector(".modal.is-open " + s);
		q("#f-members").value = %q; q("#f-members").dispatchEvent(new Event("input", { bubbles: true }));
		q("#f-role").value = %q; q("#f-role").dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, ada, role), nil)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open #f-members`)))
	p.eval(`document.querySelector(".modal.is-open #f-members").form.querySelector('button[type="submit"]').click(), true`, nil)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#mr-panel-permissions tbody tr")].some((r) => r.textContent.includes(%q))`, ada))
	pol, err := projects.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: "projects/" + project})
	if err != nil {
		t.Fatalf("GetIamPolicy: %v", err)
	}
	granted := false
	for _, b := range pol.GetBindings() {
		granted = granted || (b.GetRole() == role && slices.Contains(b.GetMembers(), ada))
	}
	if !granted {
		t.Errorf("after Add principal the project's policy is %v; want %s holding %s", pol.GetBindings(), ada, role)
	}
	p.clickText("#mr-panel .tab", "Labels")
	p.waitFor(`document.querySelector("#mr-panel-edit-labels") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#mr-panel-edit-labels`)))
	p.mrSubmitTop("labels", "env=dev")
	p.waitFor(fmt.Sprintf(`(document.querySelector(%q) || {}).textContent?.includes("env: dev")`, mrRow("project", project)))
	if got, err := projects.GetProject(ctx, &rmpb.GetProjectRequest{Name: "projects/" + project}); err != nil || got.GetLabels()["env"] != "dev" {
		t.Errorf("after Edit labels the project is %v (%v); want env=dev", got, err)
	}

	// A tag value to add, made through the API.
	short := fmt.Sprintf("mr-env-%d", time.Now().UnixNano()%1e8)
	kop, err := keys.CreateTagKey(ctx, &rmpb.CreateTagKeyRequest{TagKey: &rmpb.TagKey{Parent: org.GetName(), ShortName: short}})
	if err != nil {
		t.Fatalf("CreateTagKey: %v", err)
	}
	key, err := kop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateTagKey: %v", err)
	}
	vop, err := values.CreateTagValue(ctx, &rmpb.CreateTagValueRequest{TagValue: &rmpb.TagValue{Parent: key.GetName(), ShortName: "prod"}})
	if err != nil {
		t.Fatalf("CreateTagValue: %v", err)
	}
	value, err := vop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateTagValue: %v", err)
	}
	target := "//cloudresourcemanager.googleapis.com/projects/" + project
	t.Cleanup(func() {
		for it := bindings.ListTagBindings(context.Background(), &rmpb.ListTagBindingsRequest{Parent: target}); ; {
			b, err := it.Next()
			if err != nil {
				break
			}
			if op, err := bindings.DeleteTagBinding(context.Background(), &rmpb.DeleteTagBindingRequest{Name: b.GetName()}); err == nil {
				_ = op.Wait(context.Background())
			}
		}
		if op, err := values.DeleteTagValue(context.Background(), &rmpb.DeleteTagValueRequest{Name: value.GetName()}); err == nil {
			_, _ = op.Wait(context.Background())
		}
		if op, err := keys.DeleteTagKey(context.Background(), &rmpb.DeleteTagKeyRequest{Name: key.GetName()}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})
	// Close the info panel, then Refresh reads the new value.
	p.clickText("#mr-panel button", "Close")
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	p.clickText("#view button", "Refresh")
	p.waitFor(`document.querySelector("#view [aria-busy=true]") === null`)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, mrRow("project", project)))
	p.mrMenu("project", project, project, "Tags")
	p.waitFor(`document.querySelector("#mr-panel-add-tag") !== null && !document.querySelector("#mr-panel-add-tag").disabled`)
	p.eval(`document.querySelector("#mr-panel-add-tag").click(), true`, nil)
	p.mrSubmitTop("value", value.GetNamespacedName())
	p.waitFor(fmt.Sprintf(`(document.querySelector(%q) || {}).textContent?.includes(%q)`, mrRow("project", project), value.GetNamespacedName()))
	b, err := bindings.ListTagBindings(ctx, &rmpb.ListTagBindingsRequest{Parent: target}).Next()
	if err != nil || b.GetTagValue() != value.GetName() {
		t.Errorf("after Add tag the project's binding is %v (%v); want %s", b, err, value.GetName())
	}
	p.clickText("#mr-panel button", "Close")
	p.waitFor(`document.querySelector(".modal.is-open") === null`)

	// Delete shuts the project down; Restore brings it back.
	state := func() rmpb.Project_State {
		got, err := projects.GetProject(ctx, &rmpb.GetProjectRequest{Name: "projects/" + project})
		if err != nil {
			t.Fatalf("GetProject: %v", err)
		}
		return got.GetState()
	}
	p.mrMenu("project", project, project, "Delete")
	p.confirmByName(project)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) === null`, mrRow("project", project)))
	if got := state(); got != rmpb.Project_DELETE_REQUESTED {
		t.Fatalf("after Shut down the project is %v, want DELETE_REQUESTED", got)
	}
	p.clickText("#mr-actions a", "Resources pending deletion")
	restore := fmt.Sprintf(`#mr-pending-table tr[data-resource=%q] button`, "project:"+project)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, restore))
	var deletion string
	p.eval(fmt.Sprintf(`document.querySelector(%q).closest("tr").children[4].textContent`, restore), &deletion)
	if deletion == "" {
		t.Error("Resources pending deletion gives no deletion date")
	}
	p.run(chromedp.Click(chromedp.CSS(restore)))
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) === null`, restore))
	if got := state(); got != rmpb.Project_ACTIVE {
		t.Errorf("after Restore the project is %v, want ACTIVE", got)
	}
	if sent := p.posts("/api/studio/projects"); len(sent) < 6 {
		t.Errorf("the page sent %d Manage resources calls, want its tree reads and each change: %v", len(sent), sent)
	}
}

// TestIAMSettingsShutDown (#1865), in the storage shard: IAM & Admin's
// Settings page has Google's Shut down, confirmed by the project ID, which
// leaves the project DELETE_REQUESTED.
func TestIAMSettingsShutDown(t *testing.T) {
	needService(t, "iam-settings")
	opts := rmClientOptions(t)
	ctx := context.Background()
	projects, err := resourcemanager.NewProjectsClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = projects.Close() })
	project := uniqueProject(t)
	p := open(t)
	p.navigate("/iam-admin/settings/" + project + "?project=" + project)
	p.clickText("#view button", "Shut down")
	p.confirmByName(project)
	deadline := time.Now().Add(15 * time.Second)
	for {
		got, err := projects.GetProject(ctx, &rmpb.GetProjectRequest{Name: "projects/" + project})
		if err == nil && got.GetState() == rmpb.Project_DELETE_REQUESTED {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after Shut down the project is %v (%v), want DELETE_REQUESTED", got, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
