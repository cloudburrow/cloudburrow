package run

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestArtifactRegistryLocationsUnderPrefix(t *testing.T) {
	passed := ""
	h := ArtifactRegistryHandlerOr(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { passed = r.URL.Path }))
	get := func(p string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		var m map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return rec.Code, m
	}
	code, m := get("/artifactregistry/v1/projects/p1/locations")
	locs, _ := m["locations"].([]any)
	if code != 200 || len(locs) != len(ArtifactRegistryLocations) {
		t.Fatalf("list: %d %v", code, m)
	}
	if first := locs[0].(map[string]any); first["name"] != "projects/p1/locations/africa-south1" || first["locationId"] != "africa-south1" {
		t.Errorf("first location %v", first)
	}
	if code, m = get("/artifactregistry/v1/projects/p1/locations/us"); code != 200 || m["locationId"] != "us" {
		t.Errorf("get us: %d %v", code, m)
	}
	if code, _ = get("/artifactregistry/v1/projects/p1/locations/mars-north1"); code != 404 {
		t.Errorf("get unknown: %d, want 404", code)
	}
	if code, m = get("/artifactregistry/v1/projects/p1/locations?pageSize=2"); code != 200 || len(m["locations"].([]any)) != 2 || m["nextPageToken"] == "" {
		t.Errorf("paged: %d %v", code, m)
	}
	get("/artifactregistry/v1/projects/p1/locations/us/repositories/r")
	if passed != "/v1/projects/p1/locations/us/repositories/r" {
		t.Errorf("repository call passed as %q", passed)
	}
	get("/v1/projects/p1/locations")
	if passed != "/v1/projects/p1/locations" {
		t.Errorf("Cloud Run v1's list passed as %q", passed)
	}
}
