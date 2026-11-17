package metadata

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestIssuedTokens (#1614): every access token handed out is recorded by
// its hash with the account it was minted for, before it is returned, and
// the log outlives the credentials (a restart); a JWT-bearer assertion
// names its account only when this instance's key signed it.
func TestIssuedTokens(t *testing.T) {
	dir := t.TempDir()
	creds, err := LoadOrCreate(dir, "p", "http://127.0.0.1/token")
	if err != nil {
		t.Fatal(err)
	}
	var seen []IssuedToken
	creds.OnIssue(func(it IssuedToken) { seen = append(seen, it) })
	srv := httptest.NewServer(NewServer(creds, "p", "127.0.0.1", 0).Handler())
	defer srv.Close()

	own, err := creds.sign(map[string]any{"alg": "RS256", "typ": "JWT"}, map[string]any{"iss": creds.Email, "exp": time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	other, _ := LoadOrCreate(t.TempDir(), "p", "")
	forged, _ := other.sign(map[string]any{"alg": "RS256"}, map[string]any{"iss": "x@p.iam.gserviceaccount.com"})
	for _, tc := range []struct{ assertion, want string }{{own, creds.Email}, {forged, ""}, {"not-a-jwt", ""}} {
		seen = nil
		resp, err := http.PostForm(srv.URL+"/token", url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {tc.assertion}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || len(seen) != 1 || seen[0].Email != tc.want {
			t.Errorf("token for %.10s: %d, issued %v; want one for %q", tc.assertion, resp.StatusCode, seen, tc.want)
		}
	}
	tok, _ := creds.ServiceAccountAccessToken("sa@p.iam.gserviceaccount.com", "s")
	h := TokenHash(tok)
	found := false
	for _, it := range creds.LiveTokens(time.Now()) {
		if it.Hash == h {
			found = it.Email == "sa@p.iam.gserviceaccount.com"
		}
		if strings.Contains(it.Hash, "cbl_") {
			t.Fatal("the log keeps a token, not its hash")
		}
	}
	if !found {
		t.Error("ServiceAccountAccessToken's token is not in the log under its account")
	}
	again, err := LoadOrCreate(dir, "p", "")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(again.LiveTokens(time.Now())); n != 4 {
		t.Errorf("after a restart the log has %d tokens, want 4", n)
	}
	if n := len(again.LiveTokens(time.Now().Add(2 * time.Hour))); n != 0 {
		t.Errorf("%d tokens live after they expire", n)
	}
}
