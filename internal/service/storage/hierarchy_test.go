package storage

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// TestIntelligenceInheritsThroughFolders (#1532): over a Resource Manager
// that puts a project in folder 222, under folder 111, under the
// organization, the project inheriting takes the nearest configuration
// set: folder 222's, then (222 inheriting) the organization's, past 111;
// folder 111's DISABLED applies to what inherits under it; a project
// Resource Manager does not have inherits the organization's.
func TestIntelligenceInheritsThroughFolders(t *testing.T) {
	num := strconv.FormatInt(resource.ProjectNumber("proj-one"), 10)
	rm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parents := map[string]string{
			"/v3/projects/" + num: "folders/222",
			"/v3/folders/222":     "folders/111",
			"/v3/folders/111":     "organizations/" + localOrganization,
		}
		p, ok := parents[r.URL.Path]
		if !ok {
			http.Error(w, `{"error":{"code":404}}`, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"name":"x","parent":"` + p + `"}`))
	}))
	t.Cleanup(rm.Close)
	s, err := NewServer(Options{Hierarchy: ResourceManagerHierarchy{Base: rm.URL}})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(s)
	t.Cleanup(h.Close)
	name := func(kind, id string) string { return kind + "/" + id + "/locations/global/intelligenceConfig" }
	set := func(kind, id, edition string) {
		t.Helper()
		if code, got := doJSON(t, http.MethodPatch, h.URL+"/v2/"+name(kind, id)+"?updateMask=edition_config", `{"editionConfig":"`+edition+`"}`); code != 200 {
			t.Fatalf("set %s/%s %s = %d %v", kind, id, edition, code, got)
		}
	}
	effective := func(kind, id string) (string, string) {
		t.Helper()
		code, got := doJSON(t, http.MethodGet, h.URL+"/v2/"+name(kind, id), "")
		eff, _ := got["effectiveIntelligenceConfig"].(map[string]any)
		if code != 200 {
			t.Fatalf("get %s/%s = %d %v", kind, id, code, got)
		}
		c, _ := eff["intelligenceConfig"].(string)
		return c, eff["effectiveEdition"].(string)
	}

	if c, e := effective("projects", "proj-one"); c != "" || e != "NONE" {
		t.Errorf("nothing set: %q %s", c, e)
	}
	set("organizations", localOrganization, "STANDARD")
	set("folders", "222", "STANDARD")
	if c, e := effective("projects", "proj-one"); c != name("folders", "222") || e != "STANDARD" {
		t.Errorf("folder 222 set: %q %s; want folder 222's", c, e)
	}
	set("folders", "222", "INHERIT")
	if c, e := effective("projects", "proj-one"); c != name("organizations", localOrganization) || e != "STANDARD" {
		t.Errorf("folder 222 inheriting: %q %s; want the organization's", c, e)
	}
	set("folders", "111", "DISABLED")
	if c, e := effective("projects", "proj-one"); c != name("folders", "111") || e != "NONE" {
		t.Errorf("folder 111 DISABLED: %q %s", c, e)
	}
	if c, e := effective("folders", "222"); c != name("folders", "111") || e != "NONE" {
		t.Errorf("folder 222 under DISABLED 111: %q %s", c, e)
	}
	if c, e := effective("projects", "not-in-rm"); c != name("organizations", localOrganization) || e != "STANDARD" {
		t.Errorf("a project Resource Manager lacks: %q %s; want the organization's", c, e)
	}

	// Resource Manager unreachable: 503, not a silent answer.
	rm.Close()
	if code, got := doJSON(t, http.MethodGet, h.URL+"/v2/"+name("projects", "proj-one"), ""); code != http.StatusServiceUnavailable {
		t.Errorf("Resource Manager down = %d %v; want 503", code, got)
	}
}
