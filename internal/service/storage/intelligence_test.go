package storage

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// TestIntelligenceConfigREST: an unset configuration inherits and nothing
// is effective; STANDARD with a filter is effective for the project; an
// organization's TRIAL is inherited by a project and a folder, and becomes
// STANDARD when it ends; DISABLED refuses a filter; gcloud's path works.
func TestIntelligenceConfigREST(t *testing.T) {
	h, clock := clockedServer(t)
	num := strconv.FormatInt(resource.ProjectNumber("proj-one"), 10)
	proj := h.URL + "/v2/projects/proj-one/locations/global/intelligenceConfig"
	org := h.URL + "/v2/organizations/" + localOrganization + "/locations/global/intelligenceConfig"

	code, got := doJSON(t, http.MethodGet, proj, "")
	eff, _ := got["effectiveIntelligenceConfig"].(map[string]any)
	if code != 200 || got["editionConfig"] != "INHERIT" || got["name"] != "projects/"+num+"/locations/global/intelligenceConfig" || eff["effectiveEdition"] != "NONE" {
		t.Fatalf("unset = %d %v", code, got)
	}

	code, got = doJSON(t, http.MethodPatch, proj+"?updateMask=edition_config,filter",
		`{"editionConfig":"STANDARD","filter":{"excludedCloudStorageBuckets":{"bucketIdRegexes":["^tmp-"]}}}`)
	eff, _ = got["effectiveIntelligenceConfig"].(map[string]any)
	if code != 200 || got["editionConfig"] != "STANDARD" || got["filter"] == nil || eff["effectiveEdition"] != "STANDARD" ||
		eff["intelligenceConfig"] != got["name"] || got["updateTime"] == nil {
		t.Fatalf("STANDARD = %d %v", code, got)
	}

	code, got = doJSON(t, http.MethodPatch, org+"?updateMask=editionConfig", `{"editionConfig":"TRIAL"}`)
	trial, _ := got["trialConfig"].(map[string]any)
	if code != 200 || got["editionConfig"] != "TRIAL" || trial["expireTime"] != "2026-11-15T12:00:00Z" {
		t.Fatalf("org TRIAL = %d %v", code, got)
	}
	// The project goes back to inheriting, through gcloud's path.
	code, got = doJSON(t, http.MethodPatch, h.URL+"/storage/v1/v2/projects/"+num+"/locations/global/intelligenceConfig?updateMask=edition_config,filter",
		`{"editionConfig":"INHERIT","filter":{}}`)
	eff, _ = got["effectiveIntelligenceConfig"].(map[string]any)
	if code != 200 || got["filter"] != nil || eff["intelligenceConfig"] != "organizations/"+localOrganization+"/locations/global/intelligenceConfig" || eff["effectiveEdition"] != "STANDARD" {
		t.Fatalf("project INHERIT = %d %v", code, got)
	}
	code, got = doJSON(t, http.MethodGet, h.URL+"/v2/folders/123/locations/global/intelligenceConfig", "")
	eff, _ = got["effectiveIntelligenceConfig"].(map[string]any)
	if code != 200 || eff["effectiveEdition"] != "STANDARD" {
		t.Fatalf("folder = %d %v", code, got)
	}

	clock.Add(31 * 24 * time.Hour)
	if code, got = doJSON(t, http.MethodGet, org, ""); code != 200 || got["editionConfig"] != "STANDARD" || got["trialConfig"] != nil {
		t.Errorf("after the trial = %d %v", code, got)
	}
	if code, got = doJSON(t, http.MethodPatch, org, `{"editionConfig":"TRIAL"}`); code != 400 {
		t.Errorf("a second trial = %d %v", code, got)
	}
	if code, got = doJSON(t, http.MethodPatch, proj, `{"editionConfig":"DISABLED","filter":{"includedCloudStorageLocations":{"locations":["us-east1"]}}}`); code != 400 {
		t.Errorf("DISABLED with a filter = %d %v", code, got)
	}
	if code, got = doJSON(t, http.MethodPatch, proj+"?updateMask=labels", `{"editionConfig":"DISABLED"}`); code != 400 {
		t.Errorf("an unknown mask path = %d %v", code, got)
	}
	if code, got = doJSON(t, http.MethodGet, h.URL+"/v2/projects/proj-one/locations/us/intelligenceConfig", ""); code != 400 {
		t.Errorf("a location other than global = %d %v", code, got)
	}
}
