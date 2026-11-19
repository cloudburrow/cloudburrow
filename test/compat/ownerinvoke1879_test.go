//go:build compat

package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"

	"golang.org/x/oauth2/google"
)

// With `up --iam-enforcement=on` (#1879) a request to a Cloud Run service
// needs an ID token of a principal holding run.routes.invoke, as on Google.
// The suites that invoke services do so as the instance's own account, the
// project owner, as their calls to every other service do (no bearer token
// is that account there): ownerInvokeTransport, which TestMain puts in
// http.DefaultTransport when the instance enforces, adds that account's ID
// token, for the service's URL, to a request for a service's host
// (*.cloudburrow.localhost) that carries no Authorization. The enforcement
// tests that mean to send none use a transport of their own.
type ownerInvokeTransport struct {
	base http.RoundTripper

	mu     sync.Mutex
	tokens map[string]string
}

const runServiceDomain = ".cloudburrow.localhost"

func useOwnerInvokeTokens() {
	if v := os.Getenv(envIAMEnforcement); v == "" || v != "on" {
		return
	}
	http.DefaultTransport = &ownerInvokeTransport{base: http.DefaultTransport, tokens: map[string]string{}}
}

func (o *ownerInvokeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if req.Header.Get("Authorization") != "" || !strings.HasSuffix(host, runServiceDomain) {
		return o.base.RoundTrip(req)
	}
	tok, err := o.token(req.Context(), "http://"+host)
	if err != nil {
		return nil, fmt.Errorf("the instance's ID token for %s: %w", host, err)
	}
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+tok)
	return o.base.RoundTrip(r)
}

// token is the instance's own account's ID token for audience, minted
// through IAM Credentials' generateIdToken on the metadata server.
func (o *ownerInvokeTransport) token(ctx context.Context, audience string) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if t, ok := o.tokens[audience]; ok {
		return t, nil
	}
	meta := strings.TrimSpace(os.Getenv(EnvMetadata))
	path := strings.TrimSpace(os.Getenv(EnvCredentials))
	if meta == "" || path == "" {
		return "", fmt.Errorf("%s and %s are needed", EnvMetadata, EnvCredentials)
	}
	if !strings.HasPrefix(meta, "http://") {
		meta = "http://" + meta
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var f struct {
		ClientEmail string `json:"client_email"`
		TokenURI    string `json:"token_uri"`
	}
	if err := json.Unmarshal(raw, &f); err != nil || !strings.HasPrefix(f.TokenURI, "http://127.0.0.1") && !strings.HasPrefix(f.TokenURI, "http://localhost") {
		return "", fmt.Errorf("the credentials fixture's token_uri is not local")
	}
	creds, err := google.CredentialsFromJSON(ctx, raw, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return "", err
	}
	access, err := creds.TokenSource.Token()
	if err != nil {
		return "", err
	}
	body, _ := json.Marshal(map[string]any{"audience": audience, "includeEmail": true})
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, meta+"/v1/projects/-/serviceAccounts/"+f.ClientEmail+":generateIdToken", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Authorization", "Bearer "+access.AccessToken)
	resp, err := o.base.RoundTrip(hr)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out struct {
		Token string `json:"token"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(b, &out) != nil || out.Token == "" {
		return "", fmt.Errorf("generateIdToken: %s", resp.Status)
	}
	o.tokens[audience] = out.Token
	return out.Token, nil
}
