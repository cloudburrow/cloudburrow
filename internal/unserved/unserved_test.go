package unserved

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestTheRefusalNamesTheAPIAndIsNotRetried(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()
	for path, want := range map[string]string{
		URL("", "documentai_custom_endpoint") + "v1/projects/p/locations/us/processors": "documentai_custom_endpoint is not served",
		URL("", "transfer") + "v1/transferJobs":                                         "transfer is not served",
		PathPrefix + "%3Cscript%3E/v1/x":                                                "this API is not served",
	} {
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			Error struct {
				Code    int
				Message string
				Status  string
			}
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		_ = resp.Body.Close()
		// 400, not 5xx: gcloud and the provider retry a 5xx five times.
		if resp.StatusCode != http.StatusBadRequest || body.Error.Status != "FAILED_PRECONDITION" {
			t.Errorf("%s: %d %s, want 400 FAILED_PRECONDITION", path, resp.StatusCode, body.Error.Status)
		}
		if !strings.Contains(body.Error.Message, want) || !strings.Contains(body.Error.Message, "CloudBurrow refused this request locally") {
			t.Errorf("%s: message %q lacks %q", path, body.Error.Message, want)
		}
	}
	if !Match(httptest.NewRequest("GET", URL("http://h:1", "x")+"v1/y", nil)) || Match(httptest.NewRequest("GET", "/v1/projects", nil)) {
		t.Error("Match does not tell refusal paths from others")
	}
}

func TestTheInventoriesAreWhole(t *testing.T) {
	g, b, o := TerraformEndpoints(false), TerraformEndpoints(true), GcloudOverrides()
	// hashicorp/google 8.4.0 has 182 endpoint arguments, google-beta 196;
	// gcloud 586.0.0 has 200 overrides.
	if len(g) != 182 || len(b) != 196 || len(o) != 200 {
		t.Fatalf("inventories: %d google, %d google-beta, %d gcloud", len(g), len(b), len(o))
	}
	for _, a := range append(g, b...) {
		if !strings.HasSuffix(a, "_custom_endpoint") {
			t.Errorf("%q is not an endpoint argument", a)
		}
	}
	for _, need := range []string{"document_ai_custom_endpoint", "storage_custom_endpoint"} {
		if !slices.Contains(g, need) {
			t.Errorf("google lacks %s", need)
		}
	}
	for a := range TerraformNoEnv {
		if !slices.Contains(AllTerraformEndpoints(), a) {
			t.Errorf("TerraformNoEnv names %s, which neither provider has", a)
		}
	}
	for _, api := range GcloudUnoverridable {
		if slices.Contains(o, api) {
			t.Errorf("%s has an override, so it is not unoverridable", api)
		}
	}
	if !slices.Contains(o, "transfer") || slices.Contains(o, "storagetransfer") {
		t.Error("gcloud's Storage Transfer override is `transfer`, and there is no `storagetransfer`")
	}
}
