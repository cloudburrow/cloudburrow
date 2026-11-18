package metadata

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"testing"
)

// The certificate URLs (#1750): x509 (both paths) is key ID to PEM, jwk a
// JWK set, an unknown account 404.
func TestServiceAccountCertURLs(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := testCreds(t)
	c.SetServiceAccounts(&fakeDirectory{key: key, grants: map[string]bool{}})
	base := serve(t, c).URL
	get := func(path string) (int, map[string]any) {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	for _, p := range []string{saX509Prefix + targetEmail, robotX509Prefix + "target%40demo.iam.gserviceaccount.com"} {
		code, out := get(p)
		if code != 200 || out["system-key"] == nil {
			t.Errorf("%s = %d %v", p, code, out)
		}
	}
	code, out := get(saJWKPrefix + targetEmail)
	keys, _ := out["keys"].([]any)
	if code != 200 || len(keys) != 1 || keys[0].(map[string]any)["kid"] != "system-key" || keys[0].(map[string]any)["alg"] != "RS256" {
		t.Errorf("jwk = %d %v", code, out)
	}
	if code, _ := get(saX509Prefix + "nobody@demo.iam.gserviceaccount.com"); code != 404 {
		t.Errorf("unknown account = %d, want 404", code)
	}
	if got := ClientCertURL("http://h:1", targetEmail); got != "http://h:1/robot/v1/metadata/x509/target%40demo.iam.gserviceaccount.com" {
		t.Errorf("ClientCertURL = %s", got)
	}
}
