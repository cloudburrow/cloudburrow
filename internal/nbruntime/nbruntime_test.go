package nbruntime

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

// fakeKernelServer is a runtime's Jupyter Server: one kernel whose
// every cell prints "ran" (or raises when it is "raise"), counting cells,
// and which answers an interrupt.
func fakeKernelServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var interrupts atomic.Int32
	n := 0
	ws := websocket.Handler(func(c *websocket.Conn) {
		for {
			var m map[string]any
			if err := websocket.JSON.Receive(c, &m); err != nil {
				return
			}
			h := m["header"].(map[string]any)
			reply := func(channel, typ string, content any) {
				_ = websocket.JSON.Send(c, map[string]any{"header": map[string]any{"msg_type": typ, "msg_id": "x"},
					"parent_header": h, "content": content, "channel": channel, "metadata": map[string]any{}})
			}
			content := m["content"].(map[string]any)
			switch h["msg_type"] {
			case "kernel_info_request":
				reply("shell", "kernel_info_reply", map[string]any{"language_info": map[string]any{"version": "3.12.0"}})
			case "execute_request":
				code, _ := content["code"].(string)
				reply("iopub", "status", map[string]any{"execution_state": "busy"})
				status := "ok"
				switch {
				case strings.Contains(code, "cloudburrow_colab"):
					reply("iopub", "stream", map[string]any{"name": "stdout", "text": "[]\n"})
				case code == "raise":
					reply("iopub", "error", map[string]any{"ename": "ValueError", "evalue": "no", "traceback": []string{"\x1b[31mValueError\x1b[0m: no"}})
					status = "error"
				case code != "":
					n++
					reply("iopub", "stream", map[string]any{"name": "stdout", "text": "ra"})
					reply("iopub", "stream", map[string]any{"name": "stdout", "text": "n"})
				}
				reply("shell", "execute_reply", map[string]any{"status": status, "execution_count": n})
				reply("iopub", "status", map[string]any{"execution_state": "idle"})
			}
		}
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "token ") {
			http.Error(w, "no token", http.StatusForbidden)
			return
		}
		switch {
		case r.URL.Path == "/api/status":
			_, _ = io.WriteString(w, `{}`)
		case r.URL.Path == "/api/kernels" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id": "k1"}`)
		case r.URL.Path == "/api/kernels":
			_, _ = io.WriteString(w, `[]`)
		case r.URL.Path == "/api/terminals":
			_, _ = io.WriteString(w, `[]`)
		case r.URL.Path == "/api/kernels/k1/channels":
			ws.ServeHTTP(w, r)
		case r.URL.Path == "/api/kernels/k1/interrupt":
			interrupts.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &interrupts
}

// fakeDocker is a docker that logs its calls, has the image once built,
// and whose runtime's Jupyter Server is a fakeKernelServer.
func fakeDocker(t *testing.T, built bool) (bin, calls string) {
	t.Helper()
	srv, _ := fakeKernelServer(t)
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
inspect) echo 172.30.0.3 ;;
port) echo 127.0.0.1:` + srv.URL[strings.LastIndex(srv.URL, ":")+1:] + ` ;;
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
// internal network and the runtime's Jupyter Server, and starts a kernel
// on it; a cell's outputs come back over its channels, a stream's parts
// folded into one output and a traceback without its colours; the kernel
// keeps counting between cells; Disconnect removes all three.
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
	r, err := m.Execute(context.Background(), "p/nb", "raise")
	if err != nil || r.Status != "error" || len(r.Outputs) != 1 || !strings.Contains(string(r.Outputs[0]), `"ValueError: no"`) {
		t.Errorf("a cell that raises: %+v %v", r, err)
	}
	if len(st.Notes) != 0 {
		t.Errorf("notes %v", st.Notes)
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
		"network create --internal --label io.cloudburrow.instance=w1 cb-nb-net-",
		"-p 127.0.0.1::8888 --entrypoint python3 " + Tag() + " -I /opt/cloudburrow/relay.py 8080=host.docker.internal:9050",
		"network connect --alias host.docker.internal cb-nb-net-",
		"--hostname colab --user 1000:1000 -w /home/colab",
		"-e GOOGLE_CLOUD_PROJECT=p -e CB_COLAB=1 -e JUPYTER_CONFIG_PATH=/etc/cloudburrow/colab -e NO_GCE_CHECK=True -e CB_BIGQUERY_ENDPOINT=http://host.docker.internal:8080",
		"--entrypoint jupyter " + Tag() + " lab --ServerApp.root_dir=/home/colab",
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
