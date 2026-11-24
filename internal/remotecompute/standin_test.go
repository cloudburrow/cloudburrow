package remotecompute

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The tests below drive the real system ssh against a stand-in remote host:
// testdata/remotehost, a Linux container with sshd and Docker Engine
// (scripts/remote-compute-standin.sh starts one). They skip without it.
//
//	CLOUDBURROW_TEST_REMOTE_HOST           ssh://cb@127.0.0.1:23022
//	CLOUDBURROW_TEST_REMOTE_BOOTSTRAP_KEY  a throwaway key the host accepts,
//	                                       standing in for the user's own
//	                                       agent and config
//	CLOUDBURROW_TEST_REMOTE_IMAGE          an image on the host to run (the
//	                                       stand-in's own, which has sh and nc)
const (
	envRemoteHost  = "CLOUDBURROW_TEST_REMOTE_HOST"
	envRemoteKey   = "CLOUDBURROW_TEST_REMOTE_BOOTSTRAP_KEY"
	envRemoteImage = "CLOUDBURROW_TEST_REMOTE_IMAGE"
)

func standIn(t *testing.T) (Target, string) {
	t.Helper()
	raw, key := os.Getenv(envRemoteHost), os.Getenv(envRemoteKey)
	if raw == "" || key == "" {
		t.Skipf("%s and %s name no stand-in remote host", envRemoteHost, envRemoteKey)
	}
	tg, err := ParseTarget(raw)
	if err != nil {
		t.Fatal(err)
	}
	// The user's ssh config, as the stand-in has it: the bootstrap key is
	// what their agent would offer.
	cfg := filepath.Join(t.TempDir(), "ssh_config")
	if err := os.WriteFile(cfg, []byte("Host *\n  IdentityFile "+key+"\n  IdentitiesOnly yes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return tg, cfg
}

func TestStandInConnectInstallsOwnKeyAndForwards(t *testing.T) {
	tg, sshConfig := standIn(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	state := t.TempDir()
	var stages []string
	s, err := Connect(ctx, Options{Target: tg, StateDir: state, Instance: "standin", SSHConfig: sshConfig,
		Progress: func(e Event) { stages = append(stages, string(e.Stage)); t.Logf("%s: %s %s", e.Stage, e.Message, e.Error) }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !strings.Contains(strings.Join(stages, " "), "prerequisites connected") {
		t.Fatalf("stages %v", stages)
	}
	st := s.Status()
	if st.Host.OS != "Linux" || st.Host.CPUs == 0 || st.Host.MemoryBytes == 0 || st.Host.DockerVersion == "" {
		t.Fatalf("host info %+v", st.Host)
	}
	t.Logf("host: %s, Docker %s", st.Host.Size(), st.Host.DockerVersion)

	// From here CloudBurrow's own key signs in, and only it.
	key, err := EnsureKey(state, tg, "")
	if err != nil {
		t.Fatal(err)
	}
	probe := exec.CommandContext(ctx, "ssh", "-F", "/dev/null", "-o", "IdentitiesOnly=yes", "-i", key.Path, "-o", "BatchMode=yes",
		"-o", "UserKnownHostsFile="+filepath.Join(remoteDir(state, tg), "known_hosts"), "-p", fmt.Sprint(tg.Port), tg.destination(), "true")
	if out, err := probe.CombinedOutput(); err != nil {
		t.Fatalf("CloudBurrow's key does not sign in by itself: %v %s", err, out)
	}

	image := os.Getenv(envRemoteImage)
	if image == "" {
		t.Skipf("%s names no image on the host", envRemoteImage)
	}
	docker := func(args ...string) string {
		t.Helper()
		out, err := s.docker(ctx, args...)
		if err != nil {
			t.Fatalf("docker %v: %v %s", args, err, out)
		}
		return out
	}

	// A container that publishes on the host's loopback is reached here at
	// the same port, through the mirror.
	m := s.NewMirror()
	m.Logf = t.Logf
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	name := fmt.Sprintf("cb-rc-pub-%d", time.Now().UnixNano()%1e6)
	docker("run", "-d", "--rm", "--name", name, "-p", "127.0.0.1::8080", "--entrypoint", "sh", image, "-c",
		"while true; do printf 'HTTP/1.0 200 OK\\r\\n\\r\\nfrom the host\\n' | nc -l -p 8080; done")
	defer func() { _, _ = s.docker(context.Background(), "rm", "-f", name) }()
	hostPort := strings.TrimPrefix(strings.TrimSpace(docker("port", name, "8080/tcp")), "127.0.0.1:")
	var body string
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if res, err := http.Get("http://127.0.0.1:" + hostPort + "/"); err == nil {
			b, _ := io.ReadAll(res.Body)
			res.Body.Close()
			body = string(b)
			if strings.Contains(body, "from the host") {
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !strings.Contains(body, "from the host") {
		t.Fatalf("the published port %s did not answer here (%q)", hostPort, body)
	}

	// A listener here is reached by a container on the host, through a
	// reverse forward on the default bridge's gateway, at no inbound port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = io.WriteString(c, "from this machine\n")
			_ = c.Close()
		}
	}()
	gw := strings.Fields(docker("network", "inspect", "bridge", "--format", "{{range .IPAM.Config}}{{.Gateway}} {{end}}"))[0]
	bound, cancelRev, err := s.Reverse(ctx, gw+":0", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer cancelRev()
	reach := func() string {
		out, _ := s.docker(ctx, "run", "--rm", "--entrypoint", "sh", image, "-c", "nc -w 5 "+strings.Replace(bound, ":", " ", 1))
		return out
	}
	if out := reach(); !strings.Contains(out, "from this machine") {
		t.Fatalf("a container on the host did not reach %s (%q)", bound, out)
	}

	// The connection drops (sshd's side is killed); the supervisor
	// reconnects and the reverse forward is back at the same address.
	if err := s.Reconnect(ctx); err != nil {
		t.Fatal(err)
	}
	if out := reach(); !strings.Contains(out, "from this machine") {
		t.Fatalf("after the reconnect a container did not reach %s (%q)", bound, out)
	}
	if st := s.Status(); st.Reconnects != 1 || !st.Connected {
		t.Fatalf("status after reconnect %+v", st)
	}
}
