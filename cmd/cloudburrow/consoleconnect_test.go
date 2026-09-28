package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// connectInstance writes a runtime file naming this test process, as a live
// `up` does, for an instance serving services with the ports given, and
// returns its configuration and the flags that name it.
func connectInstance(t *testing.T, name, services string, endpoints map[string]string, control string) (config.Config, []string) {
	t.Helper()
	if !isCloudBurrow(os.Getpid()) {
		t.Skipf("this test binary's name does not read as cloudburrow, so it cannot stand in for up")
	}
	args := []string{"--name", name, "--state-dir", t.TempDir(), "--services", services}
	cfg, err := config.Load(config.Options{Args: args, Output: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.InstanceDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	info := runtimeInfo{PID: os.Getpid(), Control: control, Services: cfg.EnabledServices(), Endpoints: endpoints}
	b, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimePath(cfg), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg, args
}

// runEnvFormat is `cloudburrow env --format <format>` for the instance.
func runEnvFormat(t *testing.T, args []string, format string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if err := run(append([]string{"env", "--format", format}, args...), &stdout, &stderr); err != nil {
		t.Fatalf("env --format %s = %v\n%s", format, err, stderr.String())
	}
	return stdout.String()
}

// TestConnectVariablesAreEnvJSON (#802): the Connect page's variables are
// `cloudburrow env --format json` for the same running instance, name for
// name and value for value, less the one whose value is a credential, which
// is named and withheld; and every format the page offers is that format's
// `env` output, less the withheld line. The page calls the functions `env`
// does, so they cannot drift; this is the test that says so.
func TestConnectVariablesAreEnvJSON(t *testing.T) {
	cfg, args := connectInstance(t, "connect", "storage,pubsub,tasks,cloudsql-mysql", map[string]string{
		"storage": "127.0.0.1:41101", "pubsub": "127.0.0.1:41102", "tasks": "127.0.0.1:41103",
		"cloudsql-mysql": "127.0.0.1:41104", "metadata": "127.0.0.1:41105", "control": "127.0.0.1:41106",
	}, "127.0.0.1:41106")

	var env map[string]string
	if err := json.Unmarshal([]byte(runEnvFormat(t, args, "json")), &env); err != nil {
		t.Fatalf("env --format json is not JSON: %v", err)
	}
	password := env["MYSQL_PASSWORD"]
	if password == "" {
		t.Fatal("env exports no MYSQL_PASSWORD, so this test would check nothing withheld")
	}

	c, err := consoleConnect{cfg: cfg}.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	got := map[string]string{}
	for _, v := range c.Variables {
		got[v.Name] = v.Value
	}
	withheld := map[string]bool{}
	for _, w := range c.Withheld {
		withheld[w.Name] = true
		if w.Reason == "" {
			t.Errorf("%s is withheld without saying why", w.Name)
		}
	}
	if len(withheld) != 1 || !withheld["MYSQL_PASSWORD"] {
		t.Errorf("withheld %v, want MYSQL_PASSWORD only", c.Withheld)
	}
	for name, want := range env {
		if withheld[name] {
			if _, shown := got[name]; shown {
				t.Errorf("%s is both withheld and shown", name)
			}
			continue
		}
		if got[name] != want {
			t.Errorf("the page's %s = %q, env --format json prints %q", name, got[name], want)
		}
	}
	for name := range got {
		if _, ok := env[name]; !ok {
			t.Errorf("the page shows %s, which env --format json does not print", name)
		}
	}
	if got["STORAGE_EMULATOR_HOST"] != "http://127.0.0.1:41101" {
		t.Errorf("STORAGE_EMULATOR_HOST = %q; want the running instance's recorded port", got["STORAGE_EMULATOR_HOST"])
	}

	formats := map[string]string{}
	for _, f := range c.Formats {
		formats[f.ID] = f.Text
	}
	for _, f := range append(append([]string{}, envFormats...), "kubernetes") {
		want := withoutLinesNaming(runEnvFormat(t, args, f), "MYSQL_PASSWORD")
		if formats[f] != want {
			t.Errorf("the page's %s format differs from `env --format %s` less MYSQL_PASSWORD:\npage:\n%s\nenv:\n%s", f, f, formats[f], want)
		}
	}

	page, _ := json.Marshal(c)
	if bytes.Contains(page, []byte(password)) {
		t.Error("the Connect page carries the MySQL password")
	}
	for _, cmd := range []console.Command{c.GcloudSetup, c.GcloudTeardown, c.Terraform, c.Env, c.Diagnose} {
		if !strings.Contains(cmd.Command, "--name connect --state-dir ") {
			t.Errorf("%q does not name this instance", cmd.Command)
		}
		if cmd.Writes == "" {
			t.Errorf("%q does not say what it writes", cmd.Command)
		}
	}
	if !strings.Contains(c.GcloudSetup.Writes, "config_cloudburrow-connect") {
		t.Errorf("gcloud-setup's sentence does not name the file it writes: %s", c.GcloudSetup.Writes)
	}
}

func withoutLinesNaming(text, name string) string {
	var out []string
	for _, line := range strings.SplitAfter(text, "\n") {
		if !strings.Contains(line, name) {
			out = append(out, line)
		}
	}
	return strings.Join(out, "")
}

// An instance that is not running has nothing to export, and the page says
// so rather than showing the configured ports, which are another instance's
// (#630).
func TestConnectRefusesAnInstanceThatIsNotRunning(t *testing.T) {
	cfg := formatsConfig(t, "storage")
	if _, err := (consoleConnect{cfg: cfg}).Connect(context.Background()); err == nil {
		t.Fatal("Connect succeeded for an instance with no runtime file")
	}
}

// TestConsoleDiagnoseNeedsTheAdminToken (#802): the console's diagnose
// download is refused with the admin API's own 401, and builds nothing,
// without the admin token or with a wrong one, whatever the request looks
// like; with the page's token it is the bundle `diagnose` writes, with the
// admin API's events read with that token and the token nowhere in it. A
// cross-origin page is refused even with the token.
func TestConsoleDiagnoseNeedsTheAdminToken(t *testing.T) {
	const token = "connect-admin-token-5d1f0a"
	ready := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(readiness{Ready: true, State: "running", Components: map[string]bool{"forward:storage": true}})
	}))
	t.Cleanup(ready.Close)
	control := strings.TrimPrefix(ready.URL, "http://")
	cfg, _ := connectInstance(t, "diagnose-console", "storage", map[string]string{"storage": "127.0.0.1:41201"}, control)

	rec := admin.NewRecorder(100, nil)
	rec.Record("storage", "request", "storage.buckets.list", nil)
	api := admin.NewAPI(rec)
	api.RequireToken(token)
	src := newConsoleConnect(cfg, api)
	srv := console.New("127.0.0.1:0", nil)
	srv.SetConnect(src)
	h := srv.Handler()

	get := func(header map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9090/api/diagnose", nil)
		req.Host = "127.0.0.1:9090"
		for k, v := range header {
			if k == "Host" {
				req.Host = v
				continue
			}
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	for name, header := range map[string]map[string]string{
		"no token":           {},
		"a wrong token":      {"Authorization": "Bearer not-the-token"},
		"a workload's shape": {"Authorization": "Bearer x", "Host": "host.docker.internal:9090"},
	} {
		w := get(header)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: GET /api/diagnose = %d, want the admin API's 401: %s", name, w.Code, w.Body.String())
			continue
		}
		var body struct {
			Error         string `json:"error"`
			TokenRequired bool   `json:"token_required"`
			TokenFile     string `json:"token_file"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || !body.TokenRequired ||
			!strings.Contains(body.Error, "admin token") || body.TokenFile != adminTokenPath(cfg) {
			t.Errorf("%s: the refusal = %s; want the admin API's message and the token file", name, w.Body.String())
		}
	}
	if w := get(map[string]string{"Authorization": "Bearer " + token, "Sec-Fetch-Site": "cross-site"}); w.Code != http.StatusForbidden {
		t.Errorf("a cross-site request with the token = %d, want 403", w.Code)
	}

	w := get(map[string]string{"Authorization": "Bearer " + token})
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/diagnose with the token = %d: %s", w.Code, w.Body.String())
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "cloudburrow-diagnose-diagnose-console-") {
		t.Errorf("Content-Disposition = %q, want the bundle's name", cd)
	}
	files := untar(t, w.Body.Bytes())
	for _, want := range []string{"manifest.json", "version.txt", "config.json", "doctor.txt", "readyz.json", "admin-events.json"} {
		if _, ok := files[want]; !ok {
			t.Errorf("the console's bundle has no %s", want)
		}
	}
	if !strings.Contains(string(files["admin-events.json"]), "storage.buckets.list") {
		t.Errorf("admin-events.json = %s; want the admin API's events, read with the page's token", files["admin-events.json"])
	}
	for name, data := range files {
		if bytes.Contains(data, []byte(token)) {
			t.Errorf("%s holds the admin token", name)
		}
	}

	_, _, err := src.Diagnose(context.Background(), "")
	var refused *console.AdminRefusal
	if !errors.As(err, &refused) || refused.Status != http.StatusUnauthorized {
		t.Errorf("Diagnose with no token = %v, want the admin API's refusal", err)
	}
}

func untar(t *testing.T, b []byte) map[string][]byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("the download is not gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	out := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr)
		out[hdr.Name] = data
	}
}
