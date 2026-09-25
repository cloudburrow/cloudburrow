package kms

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestRESTLifecycle (#424) replays the update, destroy and restore requests
// gcloud, Terraform and the discovery client send.
func TestRESTLifecycle(t *testing.T) {
	h := httptest.NewServer(NewRESTHandler(NewServer(store.NewMemory())))
	defer h.Close()
	base := h.URL + "/v1/" + loc
	if code, _, raw := restDo(t, "POST", base+"/keyRings?keyRingId=r", ""); code != 200 {
		t.Fatal(raw)
	}
	key := base + "/keyRings/r/cryptoKeys/k"
	if code, _, raw := restDo(t, "POST", base+"/keyRings/r/cryptoKeys?cryptoKeyId=k", `{"purpose":"ENCRYPT_DECRYPT"}`); code != 200 {
		t.Fatal(raw)
	}
	for i := 0; i < 2; i++ {
		if code, _, raw := restDo(t, "POST", key+"/cryptoKeyVersions", "{}"); code != 200 {
			t.Fatal(raw)
		}
	}

	if code, m, raw := restDo(t, "PATCH", key+"?updateMask=labels", `{"labels":{"team":"payments"}}`); code != 200 || m["labels"].(map[string]any)["team"] != "payments" {
		t.Errorf("PATCH labels = %d %s", code, raw)
	}
	// Reaches #405's check in proto form: it names version_template.algorithm.
	if code, m, raw := restDo(t, "PATCH", key+"?updateMask=versionTemplate.algorithm", `{"versionTemplate":{"algorithm":"EC_SIGN_P256_SHA256"}}`); code != 501 ||
		!strings.Contains(m["error"].(map[string]any)["message"].(string), "version_template.algorithm") {
		t.Errorf("PATCH versionTemplate.algorithm = %d %s; want #405's refusal naming version_template.algorithm", code, raw)
	}
	if code, m, raw := restDo(t, "PATCH", key+"/cryptoKeyVersions/2?updateMask=state", `{"state":"DISABLED"}`); code != 200 || m["state"] != "DISABLED" {
		t.Errorf("PATCH state = %d %s", code, raw)
	}
	for _, c := range []struct{ version, body string }{{"2", "{}"}, {"3", ""}} {
		if code, m, raw := restDo(t, "POST", key+"/cryptoKeyVersions/"+c.version+":destroy", c.body); code != 200 || m["state"] != "DESTROY_SCHEDULED" {
			t.Errorf("destroy with body %q = %d %s", c.body, code, raw)
		}
	}
	if code, m, raw := restDo(t, "POST", key+"/cryptoKeyVersions/2:destroy", "{}"); code != 400 || m["error"].(map[string]any)["status"] != "FAILED_PRECONDITION" {
		t.Errorf("destroy twice = %d %s; want 400 FAILED_PRECONDITION", code, raw)
	}
	if code, m, raw := restDo(t, "POST", key+"/cryptoKeyVersions/2:restore", "{}"); code != 200 || m["state"] != "DISABLED" {
		t.Errorf("restore = %d %s", code, raw)
	}

	for _, c := range []struct {
		name, method, url, body string
		code                    int
		status, names           string
	}{
		{"unbound version verb", "POST", key + "/cryptoKeyVersions/1:nope", "{}", 404, "NOT_FOUND", ":nope"},
		{"bound, not transcoded", "POST", key + "/cryptoKeyVersions/1:asymmetricSign", `{"data":"aGk="}`, 501, "UNIMPLEMENTED", "AsymmetricSign"},
		{"DeleteCryptoKey", "DELETE", key, "", 501, "UNIMPLEMENTED", "DeleteCryptoKey"},
		{"DeleteCryptoKeyVersion", "DELETE", key + "/cryptoKeyVersions/1", "", 501, "UNIMPLEMENTED", "DeleteCryptoKeyVersion"},
		{"unknown body field", "PATCH", key + "?updateMask=labels", `{"labels":{},"bogus":1}`, 400, "INVALID_ARGUMENT", "CryptoKey"},
		{"snake_case mask", "PATCH", key + "?updateMask=version_template.algorithm", `{}`, 400, "INVALID_ARGUMENT", "updateMask"},
		{"name conflict", "PATCH", key + "?updateMask=labels", `{"name":"projects/demo-project/locations/global/keyRings/r/cryptoKeys/other"}`, 400, "INVALID_ARGUMENT", "differs"},
		{"destroy name conflict", "POST", key + "/cryptoKeyVersions/1:destroy", `{"name":"projects/demo-project/locations/global/keyRings/r/cryptoKeys/k/cryptoKeyVersions/9"}`, 400, "INVALID_ARGUMENT", "differs"},
	} {
		code, m, raw := restDo(t, c.method, c.url, c.body)
		e, _ := m["error"].(map[string]any)
		if code != c.code || e["status"] != c.status || !strings.Contains(e["message"].(string), c.names) {
			t.Errorf("%s = %d %s; want %d %s naming %q", c.name, code, raw, c.code, c.status, c.names)
		}
	}
}
