//go:build browser

package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	backupdr "google.golang.org/api/backupdr/v1"
	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// listAction opens one of a list screen's own actions, sets its fields and
// submits it; an action with a confirmation is confirmed by typing back the
// word it asks for. It returns the text of the rows the action answers with.
func (p *tab) listAction(id string, values map[string]string) string {
	p.t.Helper()
	p.waitFor(`document.querySelector(".modal") === null`)
	btn := "#list-action-" + id
	p.waitFor(fmt.Sprintf(`(() => { const b = document.querySelector(%q); return !!b && b.getClientRects().length > 0; })()`, btn))
	p.run(chromedp.Click(chromedp.CSS(btn)))
	p.waitFor(`document.querySelectorAll(".modal").length === 1 && document.querySelector(".modal.is-open form") !== null`)
	for f, v := range values {
		p.eval(fmt.Sprintf(`(() => { const f = document.querySelector(".modal.is-open #f-%s");
			f.value = %q; f.dispatchEvent(new Event("input", { bubbles: true })); f.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, f, v), nil)
	}
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	// A confirmation, when the action asks for one, opens over the form.
	p.waitFor(`document.querySelector(".modal #confirm-input") !== null || document.querySelector("#action-result-table") !== null ||
		[...document.querySelectorAll(".modal.is-open .form-error")].some((e) => !e.hidden && e.textContent)`)
	var word string
	p.eval(`(() => { const t = document.querySelector("#confirm-title"); if (!document.querySelector(".modal #confirm-input") || !t) return "";
		return t.textContent.replace(/^[^:]*: /, "").replace(/\?$/, ""); })()`, &word)
	if word != "" {
		p.confirmByName(word)
	}
	p.waitFor(`document.querySelector("#action-result-table") !== null ||
		[...document.querySelectorAll(".modal .form-error")].some((e) => !e.hidden && e.textContent)`)
	var res struct{ Rows, Error string }
	p.eval(`(() => { const t = document.querySelector("#action-result-table");
		const e = [...document.querySelectorAll(".modal .form-error")].filter((e) => !e.hidden).map((e) => e.textContent).join(" ");
		return { Rows: t ? t.textContent : "", Error: e }; })()`, &res)
	if res.Error != "" {
		p.t.Fatalf("%s was refused: %s", id, res.Error)
	}
	p.clickText(".modal.is-open .modal-actions button", "Close")
	p.waitFor(`document.querySelector(".modal") === null`)
	return res.Rows
}

// TestBackupDRProtectedTrialAndComplianceThroughThePage (#1536, #1592), in
// the sql shard. A project in a folder, with a Cloud SQL instance. On the
// Backup and DR page:
//   - Find protected resources of the project, of the folder and of the
//     organization each answers the instance, as resourceBackupConfigs.fetch
//     of the project, the folder and the organization reads it;
//   - Show trial answers the ELIGIBLE trial getTrial reads, Subscribe to
//     trial, confirmed, the SUBSCRIBED one, and End trial, confirmed, with
//     MOVE_TO_PAID, the UNSUBSCRIBED one getTrial then reads with that reason;
//   - Check Assured Workloads answers No, as msComplianceMetadata does.
func TestBackupDRProtectedTrialAndComplianceThroughThePage(t *testing.T) {
	needService(t, "backupdr")
	needService(t, "cloudsql-instances")
	needService(t, "hierarchy")
	bdr, err := backupdr.NewService(context.Background(), option.WithEndpoint("http://"+endpoint(t, envBackupDR)+"/"),
		option.WithoutAuthentication(), option.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}
	svc := sqlAdminSDK(t)
	n := time.Now().UnixNano() % 1e6
	code, body := consoleDo(t, http.MethodPost, "/api/resources/hierarchy",
		fmt.Sprintf(`{"displayName":"Backup %d","parent":"organizations/100000000001"}`, n))
	if code != http.StatusOK {
		t.Fatalf("create folder = %d: %s", code, body)
	}
	var created struct{ Name string }
	if err := json.Unmarshal([]byte(body), &created); err != nil || created.Name == "" {
		t.Fatalf("create folder answered %s: %v", body, err)
	}
	folder := "folders/" + strings.TrimPrefix(created.Name, "folders/")
	project := fmt.Sprintf("cb-browser-bdr-%d", n)
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/projects", `{"projectId":"`+project+`","parent":"`+folder+`"}`); code != http.StatusOK {
		t.Fatalf("register project %s in %s = %d: %s", project, folder, code, body)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/projects?name="+project, "")
		consoleDo(t, http.MethodDelete, "/api/resources/hierarchy?name="+url.QueryEscape(strings.TrimPrefix(folder, "folders/")), "")
	})
	inst := fmt.Sprintf("browser-bdr-%d", n)
	createSQL(t, svc, project, &sqladmin.DatabaseInstance{Name: inst, DatabaseVersion: "POSTGRES_17", RootPassword: "Root-pass1",
		Region: "us-central1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}})

	p := open(t)
	p.navigate("/backupdr?project=" + project)
	for _, scope := range []string{"", folder, "organizations/100000000001"} {
		var got *backupdr.FetchResourceBackupConfigsResponse
		switch {
		case scope == "":
			got, err = bdr.Projects.Locations.ResourceBackupConfigs.Fetch("projects/" + project + "/locations/us-central1").Do()
		case strings.HasPrefix(scope, "folders/"):
			got, err = bdr.Folders.Locations.ResourceBackupConfigs.Fetch(scope + "/locations/us-central1").Do()
		default:
			got, err = bdr.Organizations.Locations.ResourceBackupConfigs.Fetch(scope + "/locations/us-central1").Do()
		}
		if err != nil {
			t.Fatalf("resourceBackupConfigs.fetch of %q: %v", scope, err)
		}
		found := false
		for _, c := range got.ResourceBackupConfigs {
			found = found || (c.TargetResourceDisplayName == inst && strings.HasPrefix(c.Name, "projects/"+project+"/"))
		}
		if !found {
			t.Fatalf("resourceBackupConfigs.fetch of %q does not answer %s: %+v", scope, inst, got.ResourceBackupConfigs)
		}
		rows := p.listAction("protected", map[string]string{"location": "us-central1", "scope": scope,
			"filter": `target_resource_display_name="` + inst + `"`})
		if !strings.Contains(rows, inst) || !strings.Contains(rows, project) {
			t.Errorf("Find protected resources of %q answered %q; want %s of %s", scope, rows, inst, project)
		}
	}

	trial := func() *backupdr.Trial {
		t.Helper()
		tr, err := bdr.Projects.Locations.GetTrial("projects/" + project + "/locations/us/trial").Do()
		if err != nil {
			t.Fatalf("getTrial: %v", err)
		}
		return tr
	}
	if tr, rows := trial(), p.listAction("trial", map[string]string{"location": "us"}); tr.State != "ELIGIBLE" || !strings.Contains(rows, "ELIGIBLE") {
		t.Errorf("Show trial answered %q, getTrial %+v; want ELIGIBLE", rows, tr)
	}
	rows := p.listAction("subscribetrial", map[string]string{"location": "us"})
	if tr := trial(); tr.State != "SUBSCRIBED" || tr.EndTime == "" || !strings.Contains(rows, "SUBSCRIBED") || !strings.Contains(rows, tr.EndTime) {
		t.Errorf("after Subscribe to trial the page answered %q, getTrial %+v; want SUBSCRIBED with its end", rows, tr)
	}
	rows = p.listAction("endtrial", map[string]string{"location": "us", "reason": "MOVE_TO_PAID"})
	if tr := trial(); tr.State != "UNSUBSCRIBED" || tr.EndReason != "MOVE_TO_PAID" || !strings.Contains(rows, "UNSUBSCRIBED") || !strings.Contains(rows, "MOVE_TO_PAID") {
		t.Errorf("after End trial the page answered %q, getTrial %+v; want UNSUBSCRIBED, MOVE_TO_PAID", rows, tr)
	}

	md, err := bdr.Projects.Locations.ManagementServers.MsComplianceMetadata("projects/"+project+"/locations/us-central1/managementServers",
		&backupdr.FetchMsComplianceMetadataRequest{ProjectId: project}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if rows := p.listAction("compliance", map[string]string{"location": "us-central1"}); md.IsAssuredWorkload || !strings.Contains(rows, project+"No") {
		t.Errorf("Check Assured Workloads answered %q, msComplianceMetadata %+v; want No", rows, md)
	}
}
