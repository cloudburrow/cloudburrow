package nbruntime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeJupyterDocker is a docker whose instance's JupyterLab is jupyter, an
// httptest server, at the relay's published port.
func fakeJupyterDocker(t *testing.T, jupyter string) (bin, calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	script := `#!/bin/sh
echo "$*" >> '` + calls + `'
case "$1" in
image) exit 0 ;;
info) echo "Docker Desktop" ;;
logs) echo ready ;;
inspect) echo 172.30.0.3 ;;
port) echo 127.0.0.1:` + jupyter[strings.LastIndex(jupyter, ":")+1:] + `; echo "[::1]:1" ;;
esac
exit 0
`
	bin = filepath.Join(dir, "docker")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, calls
}

// A Workbench instance (#2198) runs JupyterLab on an internal network: the
// relay carries BigQuery, Cloud Storage and the instance's metadata server
// out, and publishes JupyterLab's port on the loopback alone; the data
// disk is a volume at /home/jupyter; the server's token is not on the
// command line. Its idle shutdown follows the kernels' activity.
func TestJupyterInstance(t *testing.T) {
	var busy atomic.Bool
	last := time.Now().Add(-time.Hour)
	jl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "token ") {
			http.Error(w, "no token", http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/api/status":
			_, _ = io.WriteString(w, `{}`)
		case "/api/kernels":
			state := "idle"
			if busy.Load() {
				state = "busy"
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "k", "last_activity": last, "execution_state": state}})
		case "/api/terminals":
			_, _ = io.WriteString(w, `[]`)
		}
	}))
	defer jl.Close()
	bin, calls := fakeJupyterDocker(t, jl.URL)
	defer func(p time.Duration) { IdlePoll = p }(IdlePoll)
	IdlePoll = 20 * time.Millisecond
	m := &Manager{Instance: "w1", Docker: bin, BigQuery: func() string { return "127.0.0.1:9050" }, Storage: func() string { return "127.0.0.1:9023" }}
	defer m.Close()
	key := "workbench/projects/p/locations/us-central1-a/instances/one"
	busy.Store(true)
	m.ConnectWith(key, "p", Options{IdleTimeout: 300 * time.Millisecond, Jupyter: &Jupyter{Volume: "cb-workbench-1", Name: "one", Zone: "us-central1-a", ID: "1"}})
	waitState(t, m, key, StateConnected)
	addr, tok, ok := m.JupyterEndpoint(key)
	if !ok || addr != strings.TrimPrefix(jl.URL, "http://") || len(tok) != 48 {
		t.Fatalf("endpoint = %s %q %v", addr, tok, ok)
	}
	if _, err := m.Execute(context.Background(), key, "1"); err == nil {
		t.Error("Execute on a JupyterLab instance succeeded")
	}
	b, _ := os.ReadFile(calls)
	log := string(b)
	for _, want := range []string{
		"network create --internal --label io.cloudburrow.instance=w1 cb-nb-net-",
		"-p 127.0.0.1::8888 --entrypoint python3 " + Tag() + " -I /opt/cloudburrow/relay.py 8080=host.docker.internal:9050 8082=host.docker.internal:",
		" 8083=host.docker.internal:9023",
		"network connect --alias host.docker.internal cb-nb-net-",
		"volume create --label io.cloudburrow.instance=w1 --label cloudburrow.workbench-disk=1 cb-workbench-1",
		"--hostname one --user 1001:1001 -w /home/jupyter -v cb-workbench-1:/home/jupyter",
		"-e JUPYTER_TOKEN -e GOOGLE_CLOUD_PROJECT=p -e CB_WORKBENCH=1 -e CB_BIGQUERY_ENDPOINT=http://host.docker.internal:8080",
		"-e GCE_METADATA_HOST=host.docker.internal:8082",
		"-e CB_STORAGE_ENDPOINT=http://host.docker.internal:8083",
		"--entrypoint jupyter " + Tag() + " lab",
		"relay.py in 8888=172.30.0.3:8080",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("no docker call with %q in:\n%s", want, log)
		}
	}
	if strings.Contains(log, tok) {
		t.Error("the server's token is on a docker command line")
	}
	// Busy kernels keep it; once idle past the timeout, it shuts down.
	time.Sleep(500 * time.Millisecond)
	if st := m.Status(key); st.State != StateConnected {
		t.Fatalf("a busy instance was shut down: %+v", st)
	}
	busy.Store(false)
	waitState(t, m, key, StateDisconnected)
	b, _ = os.ReadFile(calls)
	if strings.Contains(string(b), "volume rm") {
		t.Error("idle shutdown deleted the data disk")
	}
	if err := m.DeleteDisk("cb-workbench-1"); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(calls)
	if !strings.Contains(string(b), "volume rm -f cb-workbench-1") {
		t.Error("DeleteDisk did not remove the volume")
	}
}

// The instance's metadata server answers as GCE's does: the
// Metadata-Flavor header both ways, the project, zone, name and
// attributes, and the instance's service account's email and tokens.
func TestJupyterMetadata(t *testing.T) {
	m := &Manager{}
	s := &session{project: "p", opts: Options{Jupyter: &Jupyter{Name: "one", Zone: "europe-west1-b", ID: "7",
		Attributes: map[string]string{"idle-timeout-seconds": "3600"},
		Mint:       func(a string) (string, error) { return "tok:" + a, nil }}}}
	srv := httptest.NewServer(m.metadataHandler(s))
	defer srv.Close()
	get := func(path string, flavor bool) (int, string, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if flavor {
			req.Header.Set("Metadata-Flavor", "Google")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b), resp.Header.Get("Metadata-Flavor")
	}
	if code, _, _ := get("/computeMetadata/v1/project/project-id", false); code != http.StatusForbidden {
		t.Errorf("without Metadata-Flavor = %d", code)
	}
	if _, _, f := get("/", true); f != "Google" {
		t.Errorf("the root probe's Metadata-Flavor = %q", f)
	}
	_, email, _ := get("/computeMetadata/v1/instance/service-accounts/default/email", true)
	if !strings.HasSuffix(email, "-compute@developer.gserviceaccount.com") {
		t.Errorf("default account = %q", email)
	}
	for path, want := range map[string]string{
		"/computeMetadata/v1/project/project-id":                            "p",
		"/computeMetadata/v1/instance/name":                                 "one",
		"/computeMetadata/v1/instance/id":                                   "7",
		"/computeMetadata/v1/instance/attributes/idle-timeout-seconds":      "3600",
		"/computeMetadata/v1/universe/universe-domain":                      "googleapis.com",
		"/computeMetadata/v1/instance/service-accounts/" + email + "/email": email,
	} {
		if code, body, _ := get(path, true); code != 200 || body != want {
			t.Errorf("%s = %d %q, want %q", path, code, body, want)
		}
	}
	if _, z, _ := get("/computeMetadata/v1/instance/zone", true); !strings.HasSuffix(z, "/zones/europe-west1-b") {
		t.Errorf("zone = %q", z)
	}
	_, tok, _ := get("/computeMetadata/v1/instance/service-accounts/default/token", true)
	var tr struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	if json.Unmarshal([]byte(tok), &tr) != nil || tr.AccessToken != "tok:"+email || tr.TokenType != "Bearer" {
		t.Errorf("token = %s", tok)
	}
	if code, _, _ := get("/computeMetadata/v1/instance/service-accounts/other@x.iam.gserviceaccount.com/email", true); code != http.StatusNotFound {
		t.Errorf("another account = %d", code)
	}
}
