package kms

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// Status by path (#414): a path Google binds answers 501 UNIMPLEMENTED until
// it is transcoded; a path it does not bind answers 404.
func TestRESTAnswersGoogleBindingsAndNothingElse(t *testing.T) {
	srv := httptest.NewServer(NewRESTHandler(NewServer(store.NewMemory())))
	defer srv.Close()
	const kr = "/v1/projects/p/locations/global/keyRings/r"
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{"POST", kr + "/cryptoKeys/k:encrypt", 501},
		{"POST", kr + "/cryptoKeys/k/cryptoKeyVersions/1:encrypt", 501}, // encrypt binds cryptoKeys/**
		{"POST", kr + "/cryptoKeys/k:decrypt", 501},
		{"GET", "/v1/projects/p/locations/global/keyRings", 501},
		{"GET", kr + ":getIamPolicy", 501},
		{"GET", "/v1/projects/p/locations", 501},
		{"GET", "/v1/projects/p/locations/global", 501},
		{"GET", "/v1/projects/p/locations/global/operations/o", 501},
		{"DELETE", kr, 501},
		{"POST", kr + "/cryptoKeys/k/cryptoKeyVersions/1:decrypt", 404}, // decrypt binds cryptoKeys/* only
		{"POST", kr + ":getIamPolicy", 404},                             // getIamPolicy is GET only
		{"GET", "/v2/whatever", 404},
		{"GET", "/v1/nothing", 404},
	} {
		req, _ := http.NewRequest(c.method, srv.URL+c.path, strings.NewReader("{}"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			Error struct {
				Code    int    `json:"code"`
				Status  string `json:"status"`
				Message string `json:"message"`
			} `json:"error"`
		}
		err = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		wantStatus := map[int]string{501: "UNIMPLEMENTED", 404: "NOT_FOUND"}[c.want]
		if resp.StatusCode != c.want || err != nil || body.Error.Code != c.want || body.Error.Status != wantStatus {
			t.Errorf("%s %s = %d %+v (%v); want %d %s", c.method, c.path, resp.StatusCode, body.Error, err, c.want, wantStatus)
		}
	}
}

// Every service in google.cloud.kms.v1 contributes its bindings.
func TestRoutesCoverEveryKMSService(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range Routes() {
		seen[strings.SplitN(r.RPC, "/", 2)[0]] = true
	}
	for _, svc := range []string{"KeyManagementService", "IAMPolicy", "Locations", "Operations"} {
		if !seen[svc] {
			t.Errorf("no routes for %s: %v", svc, seen)
		}
	}
	if len(Routes()) < 40 {
		t.Errorf("only %d routes; the KMS protos bind more", len(Routes()))
	}
}
