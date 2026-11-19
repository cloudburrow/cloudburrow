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
	"testing"

	"golang.org/x/oauth2/google"
	"google.golang.org/grpc"
)

// With `up --iam-enforcement=on` (#1879) a request to a Cloud Run service
// needs an ID token of a principal holding run.routes.invoke, as on Google.
// The suites that invoke services do so as the instance's own account, the
// project owner, as their calls to every other service do (no bearer token
// is that account there): ownerInvokeTransport, which TestMain puts in
// http.DefaultTransport when the instance enforces, adds that account's ID
// token, for the service's URL, to a request for a service's host
// (*.cloudburrow.localhost, or any Host through the gateway's port-forward)
// that carries no Authorization. The enforcement
// tests that mean to send none use a transport of their own.
type ownerInvokeTransport struct {
	base http.RoundTripper

	mu     sync.Mutex
	tokens map[string]string
}

const runServiceDomain = ".cloudburrow.localhost"

// ingressAddrs are the gateway port-forwards ingress opened: a request to
// one is for a service whatever its Host (a domain mapping's).
var ingressAddrs sync.Map

// invokeAudiences are the service URLs to mint for a domain mapping's host
// (ownerInvokeAudience): Google's audience for a custom domain is the
// service's own URL.
var invokeAudiences sync.Map

// ownerInvokeAudience makes requests for domain carry the instance's ID
// token for serviceURL when the instance enforces IAM.
func ownerInvokeAudience(domain, serviceURL string) { invokeAudiences.Store(domain, serviceURL) }

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
	_, viaIngress := ingressAddrs.Load(req.URL.Host)
	if req.Header.Get("Authorization") != "" || !strings.HasSuffix(host, runServiceDomain) && !viaIngress {
		return o.base.RoundTrip(req)
	}
	audience := "http://" + host
	if a, ok := invokeAudiences.Load(host); ok {
		audience = a.(string)
	}
	tok, err := o.token(req.Context(), audience)
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

// ownerInvokeDialOptions are a gRPC client's options to call a Cloud Run
// service at host as the instance's own account when the instance enforces
// IAM (none otherwise): its ID token for the service's URL.
func ownerInvokeDialOptions(t *testing.T, host string) []grpc.DialOption {
	t.Helper()
	o, ok := http.DefaultTransport.(*ownerInvokeTransport)
	if !ok {
		return nil
	}
	tok, err := o.token(context.Background(), "http://"+host)
	if err != nil {
		t.Fatalf("the instance's ID token for %s: %v", host, err)
	}
	return []grpc.DialOption{grpc.WithPerRPCCredentials(bearerCreds(tok))}
}
