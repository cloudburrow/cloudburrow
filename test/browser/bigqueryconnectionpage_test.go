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
)

// TestBigQueryConnectionPageThroughTheBrowser (#1770), in the emulators
// shard. A connection made with Create connection is a link in the
// BigQuery page's External connections (us); its page shows its friendly
// name, description, type and service account (connections.get); its
// Permissions tab's note names the permission the instance checks and
// the caller it takes every request to be, and Grant access there writes
// the connection's own policy (connections.setIamPolicy), which the
// page then lists.
func TestBigQueryConnectionPageThroughTheBrowser(t *testing.T) {
	needService(t, "bigquery")
	p := open(t)
	project := servedProject(t)
	id := fmt.Sprintf("browser_conn_%d", time.Now().UnixNano()%1e12)
	q := "?project=" + url.QueryEscape(project)
	act := func(action string, values map[string]string) (int, string) {
		b, _ := json.Marshal(map[string]any{"Path": []string{}, "Action": action, "Values": values})
		return consoleDo(t, http.MethodPost, "/api/actions/bigquery"+q, string(b))
	}
	if code, body := act("createconnection", map[string]string{"connection": id, "friendlyName": "Browser AI", "description": "made by the browser test"}); code != http.StatusOK {
		t.Fatalf("Create connection through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() { act("deleteconnection", map[string]string{"connection": id}) })

	p.navigate("/bigquery" + q)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view a")].some((a) => a.textContent.includes(%q))`, id))
	p.clickText("#view a", id)
	p.waitFor(fmt.Sprintf(`location.pathname === %q`, "/bigquery/-connections/us/"+id))
	p.waitFor(`document.querySelector("#view").textContent.includes("Browser AI")`)
	var text string
	p.eval(`document.querySelector("#view").textContent`, &text)
	for _, want := range []string{"made by the browser test", "Cloud Resource", "gcp-sa-bigquery-condel.iam.gserviceaccount.com", project + ".us." + id} {
		if !strings.Contains(text, want) {
			t.Errorf("the connection's page does not show %q: %.800s", want, text)
		}
	}

	p.clickText("#view .tab-strip .tab", "Permissions")
	p.waitFor(`document.querySelector("#permissions-note") !== null`)
	var note string
	p.eval(`document.querySelector("#permissions-note").textContent`, &note)
	for _, want := range []string{"bigquery.connections.use", "cloudburrow-local@" + project + ".iam.gserviceaccount.com"} {
		if !strings.Contains(note, want) {
			t.Errorf("the Permissions note does not say %q: %s", want, note)
		}
	}
	if strings.Contains(note, "grants nothing and denies nothing") {
		t.Errorf("the Permissions note says the policy is not enforced: %s", note)
	}
	p.grantOnTab("allUsers", "roles/bigquery.connectionUser")
	p.waitFor(`document.querySelector("#view").textContent.includes("roles/bigquery.connectionUser")`)

	v := url.Values{"project": {project}, "name": {"-connections", "us", id}}
	code, body := consoleDo(t, http.MethodGet, "/api/detail/bigquery?"+v.Encode(), "")
	var d struct {
		Sections []struct {
			ID          string
			Permissions *struct {
				Bindings []struct {
					Role    string
					Members []string
				}
			}
		}
	}
	if code != http.StatusOK || json.Unmarshal([]byte(body), &d) != nil {
		t.Fatalf("the connection's page = %d: %.300s", code, body)
	}
	granted := false
	for _, s := range d.Sections {
		if s.Permissions == nil {
			continue
		}
		for _, b := range s.Permissions.Bindings {
			if b.Role == "roles/bigquery.connectionUser" && strings.Join(b.Members, ",") == "allUsers" {
				granted = true
			}
		}
	}
	if !granted {
		t.Errorf("after Grant access the connection's policy is %.600s", body)
	}
}
