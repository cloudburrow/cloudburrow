//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/option"
)

// consoleConnectPage is GET /api/connect, as the Connect page (#802) reads it.
type consoleConnectPage struct {
	Variables []struct{ Name, Value string }  `json:"variables"`
	Withheld  []struct{ Name, Reason string } `json:"withheld"`
}

// TestConsoleConnectIsTheInstancesEnv (#802): the Connect page's
// variables, read through the console API of the running instance, are what
// `cloudburrow env --format json` prints for it, less any it withholds as a
// credential; the Cloud Storage endpoint it gives reaches this instance, which
// the official Storage client, pointed at nothing else, shows by reading a
// bucket the suite created; and About is `cloudburrow version`.
func TestConsoleConnectIsTheInstancesEnv(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	flags := cliArgs()

	code, body := consoleDo(t, addr, http.MethodGet, "/api/connect", "")
	if code != http.StatusOK {
		t.Fatalf("GET /api/connect = %d: %s", code, body)
	}
	var page consoleConnectPage
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("the Connect page is not JSON: %v", err)
	}
	out, err := exec.Command(cli, append([]string{"env", "--format", "json"}, flags...)...).Output()
	if err != nil {
		t.Fatalf("cloudburrow env --format json: %v", err)
	}
	var env map[string]string
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("env --format json is not JSON: %v\n%s", err, out)
	}

	shown := map[string]string{}
	for _, v := range page.Variables {
		shown[v.Name] = v.Value
	}
	withheld := map[string]bool{}
	for _, w := range page.Withheld {
		withheld[w.Name] = true
	}
	for name, want := range env {
		switch {
		case withheld[name]:
			if strings.Contains(body, want) {
				t.Errorf("the page withholds %s but carries its value", name)
			}
		case shown[name] != want:
			t.Errorf("the page's %s = %q; env --format json prints %q", name, shown[name], want)
		}
	}
	for name := range shown {
		if _, ok := env[name]; !ok {
			t.Errorf("the page shows %s, which env --format json does not print", name)
		}
	}

	checkAbout(t, addr, cli)
	if os.Getenv(EnvStorage) == "" {
		t.Logf("%s is not set: this instance serves no Cloud Storage to read back through the page's endpoint", EnvStorage)
		return
	}
	endpoint := shown["STORAGE_EMULATOR_HOST"]
	if endpoint == "" {
		t.Fatal("the page gives no STORAGE_EMULATOR_HOST against an instance serving Cloud Storage")
	}
	b := bucket(t, h, storageClient(t, h))
	fromPage, err := storage.NewClient(h.Context(), option.WithoutAuthentication(),
		option.WithEndpoint(endpoint+"/storage/v1/"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fromPage.Close() })
	attrs, err := fromPage.Bucket(b.BucketName()).Attrs(h.Context())
	if err != nil {
		t.Fatalf("the official Storage client at the page's STORAGE_EMULATOR_HOST %s: %v", endpoint, err)
	}
	if attrs.Name != b.BucketName() {
		t.Errorf("read bucket %q, want %q", attrs.Name, b.BucketName())
	}
}

// checkAbout: the console's About is the line `cloudburrow version` prints.
func checkAbout(t *testing.T, addr, cli string) {
	t.Helper()
	code, body := consoleDo(t, addr, http.MethodGet, "/api/about", "")
	if code != http.StatusOK {
		t.Fatalf("GET /api/about = %d: %s", code, body)
	}
	var about struct{ Line string }
	_ = json.Unmarshal([]byte(body), &about)
	line, err := exec.Command(cli, "version").Output()
	if err != nil {
		t.Fatal(err)
	}
	if about.Line != strings.TrimSpace(string(line)) {
		t.Errorf("About = %q; cloudburrow version prints %q", about.Line, strings.TrimSpace(string(line)))
	}
}

// TestConsoleDiagnoseBundleHoldsNoCredentials (#802): the bundle downloaded
// through the console is refused with the admin API's 401 without the
// admin token, and with it passes the same checks as `cloudburrow diagnose`'s
// (TestDiagnoseBundleHoldsNoCredentials): what a bug report needs, and no
// secret payload, ADC private key, kubeconfig credential or admin token.
func TestConsoleDiagnoseBundleHoldsNoCredentials(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	if os.Getenv(EnvCLI) == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	instanceDir := instanceDirFrom(t, cliArgs())
	token := adminToken(t)
	if token == "" {
		t.Fatalf("%s is not set and the token file is not readable; the download needs the admin token", EnvAdminToken)
	}
	forbidden := diagnoseForbidden(t, h, instanceDir)
	forbidden["the admin token"] = token

	download := func(authorization string) (int, []byte) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/api/diagnose", nil)
		if err != nil {
			t.Fatal(err)
		}
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /api/diagnose: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	for _, auth := range []string{"", "Bearer not-the-token"} {
		if code, body := download(auth); code != http.StatusUnauthorized || !strings.Contains(string(body), "admin token") {
			t.Errorf("GET /api/diagnose with %q = %d %s; want the admin API's 401", auth, code, body)
		}
	}
	code, body := download("Bearer " + token)
	if code != http.StatusOK {
		t.Fatalf("GET /api/diagnose with the token = %d: %s", code, body)
	}
	path := filepath.Join(t.TempDir(), "console-bundle.tar.gz")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	checkDiagnoseBundle(t, readBundle(t, path), forbidden)
}
