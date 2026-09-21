//go:build compat

package compat

import (
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"golang.org/x/oauth2/google"
)

// EnvCredentials points at the ADC fixture `cloudburrow up` generated.
const EnvCredentials = "CLOUDBURROW_TEST_CREDENTIALS"

// EnvMetadata points at the local metadata server.
const EnvMetadata = "CLOUDBURROW_TEST_METADATA"

// credentialsFixture returns the ADC path, skipping when no instance exported
// one.
func credentialsFixture(t *testing.T) string {
	t.Helper()
	path := strings.TrimSpace(os.Getenv(EnvCredentials))
	if path == "" {
		t.Skipf("%s is not set; run `cloudburrow env` against a running instance", EnvCredentials)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("%s=%s: %v", EnvCredentials, path, err)
	}
	return path
}

// TestOfficialAuthLibraryUsesTheLocalFixture proves the generated credentials
// are usable by Google's own auth library — the only evidence that counts,
// since the point of the fixture is to satisfy tools we do not control.
//
// It also proves the exchange stays local: the fixture's token_uri is
// CloudBurrow's own endpoint, so a token comes back without any request
// reaching Google.
func TestOfficialAuthLibraryUsesTheLocalFixture(t *testing.T) {
	h := New(t)
	path := credentialsFixture(t)

	// The harness refuses to run with cloud credentials in the environment,
	// so GOOGLE_APPLICATION_CREDENTIALS is set only for this call rather than
	// inherited — which is also what proves the fixture, and not a developer's
	// real gcloud login, is what answered.
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)

	creds, err := google.FindDefaultCredentials(h.Context(),
		"https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		t.Fatalf("FindDefaultCredentials: %v", err)
	}
	if creds.ProjectID == "" {
		t.Error("the fixture carries no project, so clients would have none")
	}

	tok, err := creds.TokenSource.Token()
	if err != nil {
		t.Fatalf("minting a token through the official library: %v", err)
	}
	if !tok.Valid() {
		t.Fatal("the library considers the minted token invalid")
	}
	if tok.TokenType != "Bearer" {
		t.Errorf("token type = %q, want Bearer", tok.TokenType)
	}

	// A Google-issued token starts with "ya29.". If one appears here, the
	// library reached Google with a developer's real credentials instead of
	// using the fixture, and the test would otherwise pass while proving the
	// opposite of what it claims.
	if strings.HasPrefix(tok.AccessToken, "ya29.") {
		t.Fatalf("the token came from Google, not from CloudBurrow: %.8s...", tok.AccessToken)
	}
	if !strings.HasPrefix(tok.AccessToken, "cbl_") {
		t.Errorf("token %.8s... was not minted by CloudBurrow", tok.AccessToken)
	}
	t.Logf("official auth library minted a local token: %.12s...", tok.AccessToken)
}

// TestMetadataServerAnswersTheContract drives the metadata server the way
// Google's libraries probe it.
func TestMetadataServerAnswersTheContract(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvMetadata)

	for _, tc := range []struct {
		name, path string
		flavor     bool
		wantCode   int
		contains   string
	}{
		{"root probe", "/", true, 200, "computeMetadata/"},
		{"header required", "/", false, 403, ""},
		{"project", "/computeMetadata/v1/project/project-id", true, 200, ""},
		{"token", "/computeMetadata/v1/instance/service-accounts/default/token", true, 200, "Bearer"},
		{"email", "/computeMetadata/v1/instance/service-accounts/default/email", true, 200, "iam.gserviceaccount.com"},
		{"identity needs audience", "/computeMetadata/v1/instance/service-accounts/default/identity", true, 400, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := metadataGet(t, addr, tc.path, tc.flavor)
			if code != tc.wantCode {
				t.Fatalf("GET %s = %d, want %d: %s", tc.path, code, tc.wantCode, body)
			}
			if tc.contains != "" && !strings.Contains(body, tc.contains) {
				t.Errorf("GET %s = %q, want it to contain %q", tc.path, body, tc.contains)
			}
		})
	}
}

// metadataGet sends one metadata request, optionally with the flavor header.
func metadataGet(t *testing.T, addr, path string, flavor bool) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if flavor {
		req.Header.Set("Metadata-Flavor", "Google")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// Every response must carry the header, or a probing client concludes the
	// metadata server does not exist rather than that it refused.
	if got := resp.Header.Get("Metadata-Flavor"); got != "Google" {
		t.Errorf("GET %s response carried Metadata-Flavor = %q", path, got)
	}
	return resp.StatusCode, string(body)
}
