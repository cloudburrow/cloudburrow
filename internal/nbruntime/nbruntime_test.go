package nbruntime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeDocker is a docker that logs its calls, has the image once built,
// and runs a kernel that answers every cell with one stream output.
func fakeDocker(t *testing.T, built bool) (bin, calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	image := filepath.Join(dir, "image")
	if built {
		if err := os.WriteFile(image, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := `#!/bin/sh
echo "$*" >> '` + calls + `'
case "$1" in
image) [ -f '` + image + `' ] || exit 1 ;;
build) cat >/dev/null; : > '` + image + `' ;;
info) echo "Docker Desktop" ;;
logs) echo ready ;;
run)
  case "$*" in
  *" -i "*)
    echo '{"ready": true, "python": "3.12.0", "notes": []}'
    n=0
    while read -r line; do
      n=$((n+1))
      echo '{"status": "ok", "execution_count": '$n', "outputs": [{"output_type": "stream", "name": "stdout", "text": "ran"}]}'
    done ;;
  esac ;;
esac
exit 0
`
	bin = filepath.Join(dir, "docker")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, calls
}

func waitState(t *testing.T, m *Manager, key, want string) Status {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		st := m.Status(key)
		if st.State == want {
			return st
		}
		if st.State == StateFailed || time.Now().After(deadline) {
			t.Fatalf("runtime %s: %+v, want %s", key, st, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Connect builds the image the host lacks, starts the relay on an
// internal network and the kernel, and a cell's outputs come back; the
// kernel keeps counting between cells; Disconnect removes all three.
func TestConnectExecuteDisconnect(t *testing.T) {
	bin, calls := fakeDocker(t, false)
	m := &Manager{Instance: "w1", Docker: bin, BigQuery: func() string { return "127.0.0.1:9050" }}
	if st := m.Status("p/nb"); st.State != StateDisconnected {
		t.Fatalf("before Connect: %+v", st)
	}
	if _, err := m.Execute(context.Background(), "p/nb", "1"); err != ErrNotConnected {
		t.Fatalf("Execute before Connect = %v", err)
	}
	m.Connect("p/nb", "p")
	st := waitState(t, m, "p/nb", StateConnected)
	if st.Python != "3.12.0" || st.Project != "p" || st.Image != Tag() {
		t.Errorf("status %+v", st)
	}
	for i, want := range []int{1, 2} {
		r, err := m.Execute(context.Background(), "p/nb", "print('ran')")
		if err != nil {
			t.Fatal(err)
		}
		if r.Status != "ok" || r.ExecutionCount != want || len(r.Outputs) != 1 || !strings.Contains(string(r.Outputs[0]), `"ran"`) {
			t.Errorf("cell %d: %+v", i, r)
		}
	}
	if err := m.Disconnect("p/nb"); err != nil {
		t.Fatal(err)
	}
	if st := m.Status("p/nb"); st.State != StateDisconnected {
		t.Errorf("after Disconnect: %+v", st)
	}
	b, _ := os.ReadFile(calls)
	log := string(b)
	for _, want := range []string{
		"build --label cloudburrow.notebook-runtime=1 -t " + Tag() + " -",
		"network create --internal --label cloudburrow.instance=w1 cb-nb-net-",
		"--entrypoint python3 " + Tag() + " -I /opt/cloudburrow/relay.py host.docker.internal:9050",
		"network connect --alias host.docker.internal cb-nb-net-",
		"-e GOOGLE_CLOUD_PROJECT=p -e CB_BIGQUERY_ENDPOINT=http://host.docker.internal:8080",
		"rm -f -v cb-nb-p-nb-",
		"rm -f -v cb-nb-relay-p-nb-",
		"network rm cb-nb-net-p-nb-",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("no docker call with %q in:\n%s", want, log)
		}
	}
}

// A host that has the image builds nothing; a runtime with no BigQuery
// address fails with the reason.
func TestConnectWithoutBuildOrBigQuery(t *testing.T) {
	bin, calls := fakeDocker(t, true)
	m := &Manager{Instance: "w1", Docker: bin, BigQuery: func() string { return "127.0.0.1:9050" }}
	m.Connect("k", "p")
	waitState(t, m, "k", StateConnected)
	m.Close()
	if b, _ := os.ReadFile(calls); strings.Contains(string(b), "build") {
		t.Errorf("built an image the host has:\n%s", b)
	}
	m = &Manager{Instance: "w1", Docker: bin, BigQuery: func() string { return "" }}
	m.Connect("k", "p")
	st := waitState(t, m, "k", StateFailed)
	if !strings.Contains(st.Message, "BigQuery is not served") {
		t.Errorf("failed with %q", st.Message)
	}
	// A failed runtime is connected again from the start.
	m.BigQuery = func() string { return "127.0.0.1:9050" }
	m.Connect("k", "p")
	waitState(t, m, "k", StateConnected)
	m.Close()
}
