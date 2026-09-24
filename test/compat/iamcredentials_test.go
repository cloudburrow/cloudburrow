//go:build compat

package compat

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"cloud.google.com/go/storage"
	"golang.org/x/oauth2"
	"google.golang.org/api/impersonate"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// localOnly routes iamcredentials.googleapis.com to CloudBurrow and fails
// every other request that is not to loopback. Go's impersonate package has
// its endpoint compiled in, so this is how Go code points it here — and the
// guard is what proves no Google endpoint was contacted.
type localOnly struct {
	t     *testing.T
	local *url.URL
}

func (l localOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host == "iamcredentials.googleapis.com" {
		r = r.Clone(r.Context())
		r.URL.Scheme, r.URL.Host, r.Host = "http", l.local.Host, l.local.Host
	}
	host := r.URL.Hostname()
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		l.t.Errorf("a request left the machine: %s %s", r.Method, r.URL)
		return nil, fmt.Errorf("blocked non-local request to %s", r.URL.Host)
	}
	return http.DefaultTransport.RoundTrip(r)
}

// TestImpersonatedTokenUsedWithACloudBurrowClient (#303): an impersonated
// token source from google.golang.org/api/impersonate, served by the local
// IAM Credentials endpoint, drives an official Storage client against
// CloudBurrow. The token is not validated — nothing here validates tokens —
// and no request leaves the machine.
func TestImpersonatedTokenUsedWithACloudBurrowClient(t *testing.T) {
	h := New(t)
	meta := h.Endpoint(EnvMetadata)
	guard := localOnly{t: t, local: &url.URL{Host: meta}}
	ctx := h.Context()
	ts, err := impersonate.CredentialsTokenSource(ctx, impersonate.CredentialsConfig{
		TargetPrincipal: "worker@" + h.Project() + ".iam.gserviceaccount.com",
		Scopes:          []string{"https://www.googleapis.com/auth/devstorage.read_write"},
	}, option.WithHTTPClient(&http.Client{Transport: guard}))
	if err != nil {
		t.Fatalf("impersonate.CredentialsTokenSource: %v", err)
	}
	tok, err := ts.Token()
	if err != nil {
		t.Fatalf("Token from the local IAM Credentials endpoint: %v", err)
	}
	if !strings.HasPrefix(tok.AccessToken, "cbl_") {
		t.Errorf("access token %q is not a local one", tok.AccessToken)
	}

	// The impersonated token on an official Storage client, through the
	// same guard.
	sc, err := storage.NewClient(ctx, option.WithEndpoint(h.Endpoint(EnvStorage)+"/storage/v1/"),
		option.WithHTTPClient(&http.Client{Transport: &oauth2.Transport{Source: ts, Base: guard}}))
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	name := h.Project() + "-imp"
	if err := sc.Bucket(name).Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create a bucket with the impersonated token: %v", err)
	}
	t.Cleanup(func() { _ = sc.Bucket(name).Delete(ctx) })
	it := sc.Buckets(ctx, h.Project())
	found := false
	for {
		b, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		found = found || b.Name == name
	}
	if !found {
		t.Error("the bucket made with the impersonated token is not listed")
	}
}
