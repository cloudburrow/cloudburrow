package kms

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// restDo sends a request with a JSON content type, as every client here does.
func restDo(t *testing.T, method, url, body string) (int, map[string]any, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return resp.StatusCode, m, string(raw)
}

// TestRESTCreates (#423) replays the create requests Terraform, gcloud and
// the GAPIC REST client send.
func TestRESTCreates(t *testing.T) {
	h := httptest.NewServer(NewRESTHandler(NewServer(store.NewMemory())))
	defer h.Close()
	base := h.URL + "/v1/" + loc

	// Terraform's key ring create: no body, a JSON content type.
	if code, m, raw := restDo(t, "POST", base+"/keyRings?alt=json&keyRingId=tf", ""); code != 200 || m["name"] != loc+"/keyRings/tf" {
		t.Fatalf("Terraform's CreateKeyRing = %d %s", code, raw)
	}
	if code, m, raw := restDo(t, "POST", base+"/keyRings?keyRingId=tf", "{}"); code != 409 || m["error"].(map[string]any)["status"] != "ALREADY_EXISTS" {
		t.Errorf("duplicate CreateKeyRing = %d %s; want 409 ALREADY_EXISTS", code, raw)
	}
	ring := base + "/keyRings/tf"

	for name, c := range map[string]struct{ query, body string }{
		"terraform": {"cryptoKeyId=tfkey&skipInitialVersionCreation=false&alt=json",
			`{"purpose":"ENCRYPT_DECRYPT","labels":{"goog-terraform-provisioned":"true","env":"dev"}}`},
		"gcloud": {"cryptoKeyId=gckey&alt=json",
			`{"purpose":"ENCRYPT_DECRYPT","versionTemplate":{"protectionLevel":"SOFTWARE","algorithm":"GOOGLE_SYMMETRIC_ENCRYPTION"},"labels":{"env":"dev"},"importOnly":false}`},
		"gapic, enums as numbers": {"crypto_key_id=gapickey&%24alt=json%3Benum-encoding%3Dint", `{"purpose":1,"version_template":{"algorithm":1}}`},
	} {
		code, m, raw := restDo(t, "POST", ring+"/cryptoKeys?"+c.query, c.body)
		if code != 200 || m["primary"] == nil {
			t.Errorf("%s CreateCryptoKey = %d %s", name, code, raw)
			continue
		}
		if strings.Contains(c.body, `"env":"dev"`) && m["labels"].(map[string]any)["env"] != "dev" {
			t.Errorf("%s labels = %v", name, m["labels"])
		}
	}
	if code, m, raw := restDo(t, "POST", ring+"/cryptoKeys?cryptoKeyId=bare&skipInitialVersionCreation=true", `{"purpose":"ENCRYPT_DECRYPT"}`); code != 200 || m["primary"] != nil {
		t.Errorf("skipInitialVersionCreation=true = %d %s; want no primary", code, raw)
	}
	key := ring + "/cryptoKeys/tfkey"
	if code, m, raw := restDo(t, "POST", key+"/cryptoKeyVersions", ""); code != 200 || !strings.HasSuffix(m["name"].(string), "/cryptoKeyVersions/2") {
		t.Errorf("CreateCryptoKeyVersion = %d %s", code, raw)
	}
	if code, m, raw := restDo(t, "POST", key+":updatePrimaryVersion", `{"cryptoKeyVersionId":"2"}`); code != 200 ||
		!strings.HasSuffix(m["primary"].(map[string]any)["name"].(string), "/cryptoKeyVersions/2") {
		t.Errorf("updatePrimaryVersion = %d %s", code, raw)
	}

	for _, c := range []struct {
		name, method, url, body string
		code                    int
		status, names           string
	}{
		{"unknown body field", "POST", ring + "/cryptoKeys?cryptoKeyId=x", `{"purpose":"ENCRYPT_DECRYPT","bogus":1}`, 400, "INVALID_ARGUMENT", "CryptoKey"},
		{"unsupported purpose", "POST", ring + "/cryptoKeys?cryptoKeyId=x", `{"purpose":"ASYMMETRIC_SIGN"}`, 501, "UNIMPLEMENTED", "crypto_key.purpose"},
		{"bad bool", "POST", ring + "/cryptoKeys?cryptoKeyId=x&skipInitialVersionCreation=yes", `{"purpose":"ENCRYPT_DECRYPT"}`, 400, "INVALID_ARGUMENT", "skipInitialVersionCreation"},
		{"unbound verb", "POST", key + ":nope", `{}`, 404, "NOT_FOUND", ":nope"},
		{"name mismatch", "POST", key + ":updatePrimaryVersion", `{"name":"projects/demo-project/locations/global/keyRings/tf/cryptoKeys/other","cryptoKeyVersionId":"1"}`, 400, "INVALID_ARGUMENT", "differs"},
		{"unknown parameter", "POST", key + "/cryptoKeyVersions?bogus=1", ``, 400, "INVALID_ARGUMENT", "bogus"},
	} {
		code, m, raw := restDo(t, c.method, c.url, c.body)
		e, _ := m["error"].(map[string]any)
		if code != c.code || e["status"] != c.status || !strings.Contains(e["message"].(string), c.names) {
			t.Errorf("%s = %d %s; want %d %s naming %q", c.name, code, raw, c.code, c.status, c.names)
		}
		if strings.Contains(raw, "bogus\":1") {
			t.Errorf("%s echoed the body: %s", c.name, raw)
		}
	}
}
