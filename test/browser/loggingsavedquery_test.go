//go:build browser

package browser

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	logging "google.golang.org/api/logging/v2"
	"google.golang.org/api/option"
)

// TestLoggingSavedQueryThroughTheBrowser (#1519), in the storage shard,
// which serves Cloud Logging: Saved queries' Create saved query stores a
// shared Logging query that Google's generated Logging v2 client reads
// back, and the screen lists it.
func TestLoggingSavedQueryThroughTheBrowser(t *testing.T) {
	needService(t, "logging-saved-queries")
	logAddr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envLogging)), "http://")
	if logAddr == "" {
		t.Skipf("%s is not set: the saved query is read back through the Logging API", envLogging)
	}
	svc, err := logging.NewService(context.Background(), option.WithEndpoint("http://"+logAddr+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	p := open(t)
	project := uniqueProject(t)
	name := "projects/" + project + "/locations/global/savedQueries/browser-errors"
	t.Cleanup(func() { _, _ = svc.Projects.Locations.SavedQueries.Delete(name).Do() })
	form := `(document.querySelector(".modal.is-open") || document.querySelector("form.create-page"))`
	setField := func(id, v string) {
		p.eval(fmt.Sprintf(`(() => { const f = %s.querySelector("#f-%s");
			f.value = %q; f.dispatchEvent(new Event("input", { bubbles: true })); f.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, form, id, v), nil)
	}

	p.navigate("/logs/saved-queries?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view button")].some((b) => b.textContent === "Create saved query")`)
	p.clickText(`#view button`, "Create saved query")
	p.waitFor(form + ` !== null && ` + form + `.querySelector("#f-displayName") !== null`)
	setField("id", "browser-errors")
	setField("displayName", "Browser errors")
	setField("query", "severity>=ERROR")
	setField("visibility", "SHARED")
	p.eval(form+`.querySelector('button[type="submit"]').click()`, nil)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, "Browser errors"))

	q, err := svc.Projects.Locations.SavedQueries.Get(name).Do()
	if err != nil {
		t.Fatalf("savedQueries.get of the query made in the browser: %v", err)
	}
	if q.DisplayName != "Browser errors" || q.Visibility != "SHARED" || q.LoggingQuery == nil || q.LoggingQuery.Filter != "severity>=ERROR" {
		t.Errorf("the saved query reads %+v", q)
	}
}
