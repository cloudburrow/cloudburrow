package kms

import (
	"encoding/base64"
	"net/http/httptest"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestRESTIam (#429): the IAMPolicy mixin's JSON bindings, as Terraform's
// discovery client and the GAPIC REST client send them.
func TestRESTIam(t *testing.T) {
	h := httptest.NewServer(NewRESTHandler(NewServer(store.NewMemory())))
	defer h.Close()
	base := h.URL + "/v1/" + loc
	restDo(t, "POST", base+"/keyRings?keyRingId=r", "")
	restDo(t, "POST", base+"/keyRings/r/cryptoKeys?cryptoKeyId=k", `{"purpose":"ENCRYPT_DECRYPT"}`)
	ring := base + "/keyRings/r"

	code, p, raw := restDo(t, "GET", ring+":getIamPolicy?alt=json&prettyPrint=false&options.requestedPolicyVersion=3", "")
	etag, _ := p["etag"].(string)
	if _, err := base64.StdEncoding.DecodeString(etag); code != 200 || etag == "" || err != nil {
		t.Fatalf("getIamPolicy = %d %s; want 200 with a base64 etag", code, raw)
	}
	// The body's resource is ignored; the path's wins.
	code, set, raw := restDo(t, "POST", ring+"/cryptoKeys/k:setIamPolicy",
		`{"resource":"ignored","policy":{"bindings":[{"role":"roles/cloudkms.viewer","members":["user:a@example.com"]}]}}`)
	if code != 200 || set["bindings"] == nil {
		t.Fatalf("setIamPolicy on the key = %d %s", code, raw)
	}
	if _, rp, _ := restDo(t, "GET", ring+":getIamPolicy", ""); rp["bindings"] != nil {
		t.Errorf("the key's policy leaked onto the ring: %v", rp)
	}
	if code, tp, raw := restDo(t, "POST", ring+":testIamPermissions", `{"permissions":["cloudkms.keyRings.get"]}`); code != 200 ||
		len(tp["permissions"].([]any)) != 1 {
		t.Errorf("testIamPermissions = %d %s", code, raw)
	}
	if code, tp, raw := restDo(t, "POST", base+"/keyRings/absent:testIamPermissions", `{"permissions":["cloudkms.keyRings.get"]}`); code != 200 ||
		tp["permissions"] != nil {
		t.Errorf("testIamPermissions on a missing ring = %d %s; want an empty set", code, raw)
	}
	for _, c := range []struct {
		name, method, url, body string
		code                    int
		status                  string
	}{
		{"POST getIamPolicy", "POST", ring + ":getIamPolicy", "{}", 404, "NOT_FOUND"},
		{"IAM on a version", "POST", ring + "/cryptoKeys/k/cryptoKeyVersions/1:setIamPolicy", "{}", 404, "NOT_FOUND"},
		{"unknown body field", "POST", ring + ":setIamPolicy", `{"policy":{},"bogus":1}`, 400, "INVALID_ARGUMENT"},
		{"stale etag", "POST", ring + ":setIamPolicy", `{"policy":{"etag":"c3RhbGU="}}`, 409, "ABORTED"},
		{"import job", "POST", ring + "/importJobs/j:setIamPolicy", `{"policy":{}}`, 501, "UNIMPLEMENTED"},
		{"unknown parameter", "GET", ring + ":getIamPolicy?bogus=1", "", 400, "INVALID_ARGUMENT"},
	} {
		code, m, raw := restDo(t, c.method, c.url, c.body)
		if e, _ := m["error"].(map[string]any); code != c.code || e["status"] != c.status {
			t.Errorf("%s = %d %s; want %d %s", c.name, code, raw, c.code, c.status)
		}
	}
}
