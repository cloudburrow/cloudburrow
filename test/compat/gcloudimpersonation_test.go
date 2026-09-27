//go:build compat

package compat

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// gcloud --impersonate-service-account (#697). Impersonation needs a source
// credential: gcloud loads it, refreshes it, and trades it at the IAM
// Credentials endpoint (api_endpoint_overrides/iamcredentials, else
// iamcredentials.googleapis.com) for the impersonated account's token. Where
// that source comes from decides the outcome, so each configuration is run
// and its outcome asserted, all behind egressGuard: a request to
// iamcredentials.googleapis.com, or to any other host off the machine, fails
// the test.
//
// Measured in CI with Cloud SDK 586.0.0 (#697):
//
//   - auth/disable_credentials = true (what gcloud-setup writes): no
//     credential is loaded before the impersonation branch is reached, so the
//     flag is ignored and requests go out unauthenticated, locally.
//   - No account, no access token and no credential_file_override (a fresh
//     configuration with only `cloudburrow env`): gcloud does not read
//     GOOGLE_APPLICATION_CREDENTIALS for its own calls, so there is no source
//     credential and it fails before sending anything.
//   - A source credential (the fixture as auth/credential_file_override, with
//     credentials enabled): gcloud refreshes it at oauth2.googleapis.com, not
//     at the fixture's local token_uri, before it ever reaches the IAM
//     Credentials override. So local gcloud impersonation does not work in
//     any configuration, and outside the guard that refresh would leave the
//     machine.

// iamRecorder is a reverse proxy in front of the metadata server that
// records the path of every request, so a test can tell whether gcloud
// asked the IAM Credentials endpoint for a token.
type iamRecorder struct {
	url   string
	mu    sync.Mutex
	paths []string
}

func newIAMRecorder(t *testing.T, target string) *iamRecorder {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil || u.Host == "" {
		t.Fatalf("IAM Credentials override %q is not a URL", target)
	}
	rec := &iamRecorder{}
	rp := httputil.NewSingleHostReverseProxy(u)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.paths = append(rec.paths, r.Method+" "+r.URL.Path)
		rec.mu.Unlock()
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	// The exported value's path, on the recorder's host.
	rec.url = srv.URL + u.Path
	return rec
}

func (r *iamRecorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.paths...)
}

// envVarsFromCLI is `cloudburrow env --format plain` as a map.
func envVarsFromCLI(t *testing.T) map[string]string {
	t.Helper()
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	out, err := exec.Command(cli, append([]string{"env", "--format", "plain"}, strings.Fields(os.Getenv(EnvCLIArgs))...)...).Output()
	if err != nil {
		t.Fatalf("cloudburrow env --format plain: %v", err)
	}
	vars := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			vars[k] = v
		}
	}
	return vars
}

// newEnvGcloudSession is gcloud configured by `cloudburrow env` alone: a
// fresh CLOUDSDK_CONFIG, nothing inherited from the caller's CLOUDSDK_* or
// GOOGLE_* variables, the exported variables (the IAM Credentials override
// replaced by rec's address, same path), and egressGuard. The last three
// variables are what gcloud-setup's configuration also turns off; without
// CLOUDSDK_CORE_CHECK_GCE_METADATA=false gcloud may probe
// metadata.google.internal with its proxy handler disabled, which the guard
// would not see.
func newEnvGcloudSession(t *testing.T, vars map[string]string, rec *iamRecorder) *gcloudSession {
	t.Helper()
	bin, err := exec.LookPath("gcloud")
	if err != nil {
		t.Skip("gcloud is not on PATH")
	}
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "CLOUDSDK_") && !strings.HasPrefix(kv, "GOOGLE_") && !strings.HasPrefix(kv, "GCE_") {
			env = append(env, kv)
		}
	}
	for k, v := range vars {
		if k == "CLOUDSDK_API_ENDPOINT_OVERRIDES_IAMCREDENTIALS" {
			v = rec.url
		}
		env = append(env, k+"="+v)
	}
	env = append(env, "CLOUDSDK_CONFIG="+t.TempDir(), "CLOUDSDK_CORE_DISABLE_PROMPTS=1",
		"CLOUDSDK_CORE_DISABLE_USAGE_REPORTING=true", "CLOUDSDK_COMPONENT_MANAGER_DISABLE_UPDATE_CHECK=true",
		"CLOUDSDK_CORE_CHECK_GCE_METADATA=false")
	return &gcloudSession{t: t, bin: bin, env: append(env, egressGuard(t)...), project: vars["CLOUDSDK_CORE_PROJECT"]}
}

// refreshRecorder is a proxy that refuses every request, like egressGuard,
// but records the hosts instead of failing the test, for the cases where
// gcloud is expected to try, and be stopped from, leaving the machine. Its
// variables go last in a command's environment, so they win over the
// session's egressGuard.
func refreshRecorder(t *testing.T) ([]string, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var hosts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hosts = append(hosts, r.Host)
		mu.Unlock()
		http.Error(w, "CloudBurrow compat: egress refused", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	return []string{"HTTP_PROXY=" + srv.URL, "HTTPS_PROXY=" + srv.URL, "http_proxy=" + srv.URL, "https_proxy=" + srv.URL},
		func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), hosts...) }
}

// assertRefreshGoesToGoogle checks the measured outcome of impersonating
// with a source credential: gcloud fails refreshing it, the only request
// off the machine was to oauth2.googleapis.com (refused by the proxy), and
// nothing reached the IAM Credentials override.
func assertRefreshGoesToGoogle(t *testing.T, out string, err error, hosts []string, rec *iamRecorder) {
	t.Helper()
	t.Logf("gcloud storage ls --impersonate-service-account:\n%s", out)
	if err == nil {
		t.Fatalf("gcloud impersonated successfully; docs/credentials.md says it refreshes at oauth2.googleapis.com and fails")
	}
	if !strings.Contains(out, "problem refreshing your current auth tokens") {
		t.Errorf("gcloud failed otherwise than refreshing its source credential")
	}
	if len(hosts) == 0 {
		t.Errorf("gcloud sent nothing off the machine; docs/credentials.md says it refreshes at oauth2.googleapis.com")
	}
	for _, h := range hosts {
		if h != "oauth2.googleapis.com:443" {
			t.Errorf("gcloud tried to reach %s as well", h)
		}
	}
	if got := rec.seen(); len(got) != 0 {
		t.Errorf("gcloud reached the IAM Credentials override: %v", got)
	}
}

const impersonationWarning = "This command is using service account impersonation"

// TestGcloudImpersonationThroughEnv (#697): `gcloud storage ls
// --impersonate-service-account` in a fresh configuration set up by
// `cloudburrow env`. With the exported variables alone gcloud has no source
// credential and fails before any request. With the exported ADC fixture
// also named as auth/credential_file_override, gcloud refreshes it at
// oauth2.googleapis.com, which the proxy refuses, and fails.
func TestGcloudImpersonationThroughEnv(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvStorage)
	vars := envVarsFromCLI(t)
	override, fixture := vars["CLOUDSDK_API_ENDPOINT_OVERRIDES_IAMCREDENTIALS"], vars["GOOGLE_APPLICATION_CREDENTIALS"]
	if override == "" || fixture == "" || vars["CLOUDSDK_CORE_PROJECT"] == "" {
		t.Fatalf("cloudburrow env exported no IAM Credentials override, fixture or project: %v", vars)
	}
	project := vars["CLOUDSDK_CORE_PROJECT"]
	bucket := h.Project() + "-imp"
	sc := storageClient(t, h)
	if err := sc.Bucket(bucket).Create(h.Context(), project, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sc.Bucket(bucket).Delete(h.Context()) })
	sa := "worker@" + project + ".iam.gserviceaccount.com"

	t.Run("exported variables alone", func(t *testing.T) {
		rec := newIAMRecorder(t, override)
		g := newEnvGcloudSession(t, vars, rec)
		out, err := g.run(nil, "storage", "ls", "--impersonate-service-account="+sa)
		t.Logf("gcloud storage ls --impersonate-service-account:\n%s", out)
		if err == nil {
			t.Fatalf("gcloud succeeded with no source credential; docs/credentials.md says it fails")
		}
		if !strings.Contains(out, "You do not currently have an active account selected") {
			t.Errorf("gcloud failed otherwise than for want of an active account")
		}
		if got := rec.seen(); len(got) != 0 {
			t.Errorf("gcloud reached the IAM Credentials override with no source credential: %v", got)
		}
	})

	t.Run("fixture as credential_file_override", func(t *testing.T) {
		rec := newIAMRecorder(t, override)
		g := newEnvGcloudSession(t, vars, rec)
		proxy, hosts := refreshRecorder(t)
		out, err := g.run(append([]string{"CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE=" + fixture}, proxy...),
			"storage", "ls", "--impersonate-service-account="+sa)
		assertRefreshGoesToGoogle(t, out, err, hosts(), rec)
	})
}

// TestGcloudImpersonationUnderGcloudSetup (#697): through the configuration
// `cloudburrow gcloud-setup` writes, --impersonate-service-account is
// ignored. The configuration sets auth/disable_credentials, so gcloud loads
// no credential, never reaches its impersonation code, and lists the
// buckets unauthenticated. Nothing is asked of any IAM Credentials endpoint
// (the configuration names none, so a request would go to
// iamcredentials.googleapis.com and fail the guard). gcloud-setup does not
// support gcloud impersonation; this asserts that the flag stays harmless
// and local rather than that it works.
//
// The second subtest turns credentials back on and adds the exported
// override, as a gcloud-setup that supported impersonation would have to:
// gcloud then refreshes the fixture (the configuration's
// credential_file_override) at oauth2.googleapis.com and fails, so turning
// credentials on would not make impersonation work.
func TestGcloudImpersonationUnderGcloudSetup(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvStorage)
	g := newGcloudSession(t, h)
	bucket := h.Project() + "-impsetup"
	sc := storageClient(t, h)
	if err := sc.Bucket(bucket).Create(h.Context(), g.project, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sc.Bucket(bucket).Delete(h.Context()) })
	sa := "worker@" + g.project + ".iam.gserviceaccount.com"

	t.Run("as written", func(t *testing.T) {
		out, err := g.run(nil, "storage", "ls", "--impersonate-service-account="+sa)
		if err != nil {
			t.Fatalf("gcloud storage ls --impersonate-service-account: %v\n%s", err, out)
		}
		if !strings.Contains(out, "gs://"+bucket+"/") {
			t.Errorf("gcloud storage ls does not show gs://%s/:\n%s", bucket, out)
		}
		if strings.Contains(out, impersonationWarning) {
			t.Errorf("gcloud impersonated under auth/disable_credentials; docs/credentials.md says it does not:\n%s", out)
		}
	})

	t.Run("credentials enabled with the exported override", func(t *testing.T) {
		vars := envVarsFromCLI(t)
		override := vars["CLOUDSDK_API_ENDPOINT_OVERRIDES_IAMCREDENTIALS"]
		if override == "" {
			t.Fatalf("cloudburrow env exported no IAM Credentials override: %v", vars)
		}
		rec := newIAMRecorder(t, override)
		proxy, hosts := refreshRecorder(t)
		out, err := g.run(append([]string{"CLOUDSDK_AUTH_DISABLE_CREDENTIALS=false",
			"CLOUDSDK_API_ENDPOINT_OVERRIDES_IAMCREDENTIALS=" + rec.url}, proxy...),
			"storage", "ls", "--impersonate-service-account="+sa)
		assertRefreshGoesToGoogle(t, out, err, hosts(), rec)
	})
}
