//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestDataformUnitTestsAndSSHThroughThePage (#2105): a workspace with a
// table and a unit test (type: "test"); Start execution with Run unit tests
// only runs the unit test alone, the execution's page showing it as a Unit
// test that Succeeded with its two queries in the Execution log; the Git
// connection's Connect with Git with the SSH authentication method (the
// private key's secret and the host's public key), read back as SSH. Each
// screen meets WCAG 2.1 AA, and the authentication method is chosen by
// keyboard.
func TestDataformUnitTestsAndSSHThroughThePage(t *testing.T) {
	needService(t, "dataform")
	needService(t, "bigquery")
	project := servedProject(t)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1e9)
	repoID := "browser-ut-" + suffix
	dataset := "df_ut_" + suffix
	repo := "projects/" + project + "/locations/us-central1/repositories/" + repoID
	detail := func(path ...string) string {
		t.Helper()
		q := url.Values{"project": {project}, "name": path}
		code, body := consoleDo(t, http.MethodGet, "/api/detail/dataform?"+q.Encode(), "")
		if code != http.StatusOK {
			t.Fatalf("read %v through the console API = %d: %s", path, code, body)
		}
		return body
	}
	t.Cleanup(func() {
		b, _ := json.Marshal(map[string]any{"Path": []string{repo}, "Action": "delete", "Values": map[string]string{}})
		consoleDo(t, http.MethodPost, "/api/actions/dataform?project="+url.QueryEscape(project), string(b))
	})

	p := open(t)
	p.navigate("/bigquery-dataform?project=" + url.QueryEscape(project))
	p.clickText("#view button", "Create repository")
	p.submitForm(map[string]string{"repositoryId": repoID, "region": "us-central1"}, "repositoryId")
	p.waitFor(fmt.Sprintf(`location.pathname === "/bigquery-dataform" &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, repo))
	p.follow(repo)
	var repoPath string
	p.eval(`location.pathname + location.search`, &repoPath)
	p.pageAction("Create development workspace", map[string]string{"workspaceId": "dev"}, "")
	p.follow("dev")
	var wsPath string
	p.eval(`location.pathname + location.search`, &wsPath)
	p.pageAction("Create file", map[string]string{"path": "workflow_settings.yaml", "contents": fmt.Sprintf(
		"defaultProject: %s\ndefaultLocation: US\ndefaultDataset: %s\ndataformCoreVersion: 3.1.6\n", project, dataset)}, "")
	p.pageAction("Create file", map[string]string{"path": "definitions/src.sqlx", "contents": "config { type: \"table\" }\nselect 1 as a\n"}, "")
	p.pageAction("Create file", map[string]string{"path": "definitions/doubled.sqlx", "contents": "config { type: \"view\" }\nselect a * 2 as b from ${ref(\"src\")}\n"}, "")
	p.pageAction("Create file", map[string]string{"path": "definitions/doubled_test.sqlx",
		"contents": "config { type: \"test\", dataset: \"doubled\" }\ninput \"src\" { select 5 as a }\nselect 10 as b\n"}, "")

	// Start execution, Run unit tests only.
	p.navigate(wsPath)
	p.pageActionWithin("Start execution", map[string]string{"unitTests": "true"}, "", 5*time.Minute)
	var inv, state string
	for deadline := time.Now().Add(5 * time.Minute); ; time.Sleep(time.Second) {
		var d struct {
			Sections []struct {
				ID      string
				Listing struct {
					Items []struct{ Name, Status string }
				}
			}
		}
		if err := json.Unmarshal([]byte(detail(repo)), &d); err != nil {
			t.Fatal(err)
		}
		for _, s := range d.Sections {
			if s.ID == "logs" && len(s.Listing.Items) > 0 {
				inv, state = s.Listing.Items[0].Name, s.Listing.Items[0].Status
			}
		}
		if state == "Succeeded" {
			break
		}
		if state == "Failed" || time.Now().After(deadline) {
			t.Fatalf("the unit test execution is %q: %s", state, detail(repo, "invocations", inv))
		}
	}
	if got := detail(repo, "invocations", inv); !strings.Contains(got, "Unit test") || strings.Contains(got, dataset+".src") {
		t.Errorf("the execution reads %s", got)
	}
	p.navigate(repoPath)
	p.clickText(`#view [role="tab"]`, "Workflow Execution Logs")
	p.follow(inv)
	var invPath string
	p.eval(`location.pathname + location.search`, &invPath)
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("doubled_test") && r.textContent.includes("Unit test") && r.textContent.includes("Succeeded"))`)
	p.clickText(`#view [role="tab"]`, "Execution log")
	p.waitFor(`document.querySelector("#view").textContent.includes("-- expected results")`)

	// Connect with Git over SSH.
	hostKey := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKNqSx03FGDwlcmm6O0orPmVNFZVZjHcJPJbugEDbAtM"
	secret := "projects/" + project + "/secrets/df-ssh-key/versions/1"
	p.navigate(repoPath)
	p.clickText(`#view [role="tab"]`, "Git connection")
	// The authentication method is a radio group: SSH is chosen by clicking it.
	p.clickText(p.findAction("Connect with Git"), "Connect with Git")
	p.waitFor(`document.querySelector(".modal.is-open #f-auth-1") !== null`)
	p.eval(`(() => { const r = document.querySelector(".modal.is-open #f-auth-1"); r.checked = true; r.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, nil)
	p.waitFor(`document.querySelector(".modal.is-open #f-auth").value === "ssh"`)
	for id, v := range map[string]string{"url": "ssh://git@127.0.0.1:2222/r.git", "branch": "main", "sshSecret": secret, "hostKey": hostKey} {
		p.setField(".modal.is-open #f-"+id, v)
	}
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	if got := detail(repo); !strings.Contains(got, "SSH public host key value") || !strings.Contains(got, hostKey) || !strings.Contains(got, secret) {
		t.Errorf("the Git connection reads %s", got)
	}

	t.Run("keyboard", func(t *testing.T) {
		parent := p.t
		p.t = t
		defer func() { p.t = parent }()
		p.navigate(repoPath)
		p.clickText(`#view [role="tab"]`, "Git connection")
		p.clickText(p.findAction("Edit Git connection"), "Edit Git connection")
		p.waitFor(`document.querySelector(".modal.is-open #f-auth-1") !== null`)
		// From SSH, the arrow key moves to Personal authentication, whose
		// Account connector field shows and the SSH fields hide.
		p.run(chromedp.Focus(chromedp.CSS(`.modal.is-open #f-auth-1`)), chromedp.KeyEvent(kb.ArrowDown))
		p.waitFor(`document.querySelector(".modal.is-open #f-auth").value === "account" &&
			document.querySelector(".modal.is-open #f-connector").offsetParent !== null &&
			document.querySelector(".modal.is-open #f-sshSecret").offsetParent === null`)
		p.run(chromedp.KeyEvent(kb.Escape))
		p.waitFor(`document.querySelector(".modal.is-open") === null`)
	})

	t.Run("WCAG", func(t *testing.T) {
		auditScreens(t, []a11yPage{
			{"Dataform unit test execution", invPath, `document.querySelector("#view tbody tr") !== null`, ""},
			{"Dataform Connect with Git over SSH", repoPath,
				`[...document.querySelectorAll("#view [role=tab]")].some((b) => b.textContent.trim() === "Git connection")`,
				`(() => { [...document.querySelectorAll("#view [role=tab]")].find((b) => b.textContent.trim() === "Git connection").click();
				const go = () => { const b = [...document.querySelectorAll("#view button")].find((b) => b.textContent.trim() === "Edit Git connection" && b.getClientRects().length);
					if (b) b.click(); else setTimeout(go, 100); }; go(); })()`},
			{"Dataform Start execution", wsPath,
				`[...document.querySelectorAll("#view button")].some((b) => b.textContent.trim() === "Start execution")`,
				`[...document.querySelectorAll("#view button")].find((b) => b.textContent.trim() === "Start execution").click()`},
		})
	})
}
