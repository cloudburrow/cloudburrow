package main

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
)

// TestTerraformInterceptServesOnlyBigQueryLocally (#1578): the intercept's
// proxy tunnels CONNECT bigquery.googleapis.com:443 to the instance's
// BigQuery front, path unchanged, under a certificate only its own CA
// signs; it refuses every other host, port, plain proxy request and path
// outside /bigquery/v2/; and when it stops, its CA directory is gone. Only
// the CA certificate is ever on disk.
func TestTerraformInterceptServesOnlyBigQueryLocally(t *testing.T) {
	var gotPath, gotHost string
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotHost = r.URL.Path, r.Host
		_, _ = io.WriteString(w, `{"rowAccessPolicies":[]}`)
	}))
	defer front.Close()
	_, port, _ := net.SplitHostPort(front.Listener.Addr().String())
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Services = []config.Service{config.ServiceStorage, config.ServiceBigQuery}
	cfg.Endpoints.BigQuery, _ = strconv.Atoi(port)

	var log strings.Builder
	ic, err := startIntercept(cfg, &log)
	if err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(ic.dir, "*"))
	if len(files) != 1 || filepath.Base(files[0]) != "ca.pem" {
		t.Fatalf("the intercept directory holds %v, want only ca.pem", files)
	}
	if !strings.HasPrefix(ic.dir, cfg.InstanceDir()) {
		t.Errorf("the CA directory %s is not in the instance's state directory %s", ic.dir, cfg.InstanceDir())
	}
	caPEM, err := os.ReadFile(ic.caFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(caPEM), "PRIVATE KEY") {
		t.Fatal("ca.pem holds a private key")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("ca.pem holds no certificate")
	}
	proxyURL, _ := url.Parse("http://" + ic.listener.Addr().String())
	client := &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{RootCAs: pool},
	}}

	path := "/bigquery/v2/projects/p/datasets/d/tables/t/rowAccessPolicies"
	resp, err := client.Get("https://bigquery.googleapis.com" + path)
	if err != nil {
		t.Fatalf("GET through the intercept: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), "rowAccessPolicies") {
		t.Errorf("GET through the intercept: %d %s", resp.StatusCode, b)
	}
	if gotPath != path || gotHost != front.Listener.Addr().String() {
		t.Errorf("the front got path %q host %q, want %q at %s", gotPath, gotHost, path, front.Listener.Addr())
	}

	for _, u := range []string{
		"https://storage.googleapis.com/storage/v1/b",        // another Google host
		"https://example.com/",                               // the internet
		"http://bigquery.googleapis.com" + path,              // plain proxying
		"https://bigquery.googleapis.com/discovery/v1/apis",  // outside /bigquery/v2/
		"https://bigquery.googleapis.com:8443/bigquery/v2/x", // another port
	} {
		resp, err := client.Get(u)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("%s through the intercept: %d, want 403", u, resp.StatusCode)
			}
		}
		// An error is a refused CONNECT (the transport reports the 403).
	}

	// A client that trusts only the system roots does not trust the leaf:
	// the CA is the intercept's own.
	plain := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	if resp, err := plain.Get("https://bigquery.googleapis.com" + path); err == nil {
		resp.Body.Close()
		t.Error("a client without the intercept's CA trusted its certificate")
	}

	env := strings.Join(ic.env([]string{"NO_PROXY=googleapis.com", "SSL_CERT_FILE=/etc/x", "GODEBUG=a=1", "HOME=/h"}, "127.0.0.1"), "\n")
	for _, want := range []string{"HTTPS_PROXY=" + proxyURL.String(), "NO_PROXY=127.0.0.1,localhost,::1", "SSL_CERT_FILE=" + ic.caFile,
		"SSL_CERT_DIR=" + ic.dir, "GODEBUG=a=1,x509sslcertoverrideplatform=1", "HOME=/h"} {
		if !strings.Contains(env, want+"\n") && !strings.HasSuffix(env, want) {
			t.Errorf("the binary's environment lacks %s:\n%s", want, env)
		}
	}
	if strings.Contains(env, "googleapis.com") || strings.Contains(env, "/etc/x") {
		t.Errorf("the binary's environment keeps the caller's NO_PROXY or SSL_CERT_FILE:\n%s", env)
	}

	ic.stop()
	if _, err := os.Stat(ic.dir); !os.IsNotExist(err) {
		t.Errorf("the CA directory survived stop: %v", err)
	}
	if !strings.Contains(log.String(), "served 1 request(s) to bigquery.googleapis.com locally") ||
		!strings.Contains(log.String(), "storage.googleapis.com:443") {
		t.Errorf("stop reported %q", log.String())
	}
}

// TestTerraformInterceptNeedsAServedHost: with neither BigQuery nor the Run
// port served there is nothing to send a host to, so the intercept refuses
// to start.
func TestTerraformInterceptNeedsAServedHost(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Services = []config.Service{config.ServiceStorage}
	if _, err := startIntercept(cfg, io.Discard); err == nil {
		t.Fatal("the intercept started without BigQuery or Cloud Run")
	}
}

// TestTerraformInterceptServesTheLocationsHosts (#1578): with the Run port
// served, the intercept tunnels run.googleapis.com's and
// artifactregistry.googleapis.com's /v1/projects/ paths to it, Artifact
// Registry's under run.ArtifactRegistryPrefix (the Run port's plain
// /v1/projects/{p}/locations is Cloud Run v1's list), refuses their other
// paths and bigquery.googleapis.com (not served here), and unlocks
// google_cloud_run_locations and google_artifact_registry_locations, but
// not google_iam_testable_permissions.
func TestTerraformInterceptServesTheLocationsHosts(t *testing.T) {
	var gotPaths []string
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path)
		_, _ = io.WriteString(w, `{"locations":[]}`)
	}))
	defer front.Close()
	_, port, _ := net.SplitHostPort(front.Listener.Addr().String())
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Services = []config.Service{config.ServiceStorage, config.ServiceRun}
	cfg.Endpoints.Run, _ = strconv.Atoi(port)

	var log strings.Builder
	ic, err := startIntercept(cfg, &log)
	if err != nil {
		t.Fatal(err)
	}
	caPEM, err := os.ReadFile(ic.caFile)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	proxyURL, _ := url.Parse("http://" + ic.listener.Addr().String())
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: pool}}}
	for _, u := range []string{
		"https://run.googleapis.com/v1/projects/p/locations",
		"https://artifactregistry.googleapis.com/v1/projects/p/locations",
	} {
		resp, err := client.Get(u)
		if err != nil {
			t.Fatalf("GET %s through the intercept: %v", u, err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("GET %s through the intercept: %d", u, resp.StatusCode)
		}
	}
	want := []string{"/v1/projects/p/locations", "/artifactregistry/v1/projects/p/locations"}
	if strings.Join(gotPaths, " ") != strings.Join(want, " ") {
		t.Errorf("the Run port got %q, want %q", gotPaths, want)
	}
	for _, u := range []string{
		"https://run.googleapis.com/apis/serving.knative.dev/v1/namespaces/p/services", // outside /v1/projects/
		"https://artifactregistry.googleapis.com/v1beta2/projects/p/locations",
		"https://bigquery.googleapis.com/bigquery/v2/projects/p/datasets", // BigQuery is not served
		"https://iam.googleapis.com/v1/permissions:queryTestablePermissions",
	} {
		if resp, err := client.Get(u); err == nil {
			resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("%s through the intercept: %d, want 403", u, resp.StatusCode)
			}
		}
	}
	if len(gotPaths) != 2 {
		t.Errorf("a refused request reached the Run port: %q", gotPaths)
	}
	ic.stop()
	for _, w := range []string{"served 1 request(s) to run.googleapis.com locally", "served 1 request(s) to artifactregistry.googleapis.com locally", "iam.googleapis.com:443"} {
		if !strings.Contains(log.String(), w) {
			t.Errorf("stop reported %q, without %q", log.String(), w)
		}
	}
	if strings.Contains(log.String(), "bigquery.googleapis.com locally") {
		t.Errorf("stop names BigQuery, which is not served: %q", log.String())
	}

	got := map[string]bool{}
	for _, f := range interceptUnlocked(cfg, unpointedResources(cfg)) {
		got[f.prefix] = true
	}
	if got["google_cloud_run_locations"] || got["google_artifact_registry_locations"] || !got["google_iam_testable_permissions"] || !got["google_storage_buckets"] {
		t.Errorf("with the intercept the refused families are %v", got)
	}
	noRun := cfg
	noRun.Services = []config.Service{config.ServiceStorage}
	got = map[string]bool{}
	for _, f := range interceptUnlocked(noRun, unpointedResources(noRun)) {
		got[f.prefix] = true
	}
	if !got["google_cloud_run_locations"] || !got["google_artifact_registry_locations"] {
		t.Errorf("without the Run port the intercept unlocked a locations data source: %v", got)
	}
}

func TestGoBefore127(t *testing.T) {
	for v, want := range map[string]bool{"go1.26.8": true, "go1.9": true, "go1.27": false, "go1.27.4": false, "go1.30rc1": false, "devel": false} {
		if got := goBefore127(v); got != want {
			t.Errorf("goBefore127(%q) = %v, want %v", v, got, want)
		}
	}
}
