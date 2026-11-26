package remotecompute

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/hostmount"
	"github.com/cloudburrow/cloudburrow/internal/modelgarden"
)

// TestStandInModelContainerMountsAPathOfThisMachine runs a Model Garden
// model container, through modelgarden's own DockerCLI, on the stand-in
// host: its model directory is this machine's, carried into a volume on the
// host (internal/hostmount), and its published port answers here through
// the mirror. A writable mount's writes come back here. The image
// (CLOUDBURROW_TEST_REMOTE_IMAGE) needs sh, httpd, true and rm: busybox.
func TestStandInModelContainerMountsAPathOfThisMachine(t *testing.T) {
	tg, sshConfig := standIn(t)
	image := os.Getenv(envRemoteImage)
	if image == "" {
		t.Skipf("%s names no image on the host", envRemoteImage)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	state := t.TempDir()
	s, err := Connect(ctx, Options{Target: tg, StateDir: state, Instance: "standin-mounts", SSHConfig: sshConfig})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	t.Setenv("DOCKER_HOST", s.DockerHost())
	t.Setenv("DOCKER_CONTEXT", "")
	hostmount.SetRemote(&hostmount.Config{Instance: "standin-mounts", StateDir: state, Helper: image})
	defer hostmount.SetRemote(nil)
	var volumes []string
	defer func() {
		for _, v := range volumes {
			_, _ = s.docker(context.Background(), "volume", "rm", "-f", v)
		}
	}()
	mirror := s.NewMirror()
	mirror.Logf = t.Logf
	if err := mirror.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer mirror.Stop()

	// The model, on this machine only.
	model := t.TempDir()
	if err := os.WriteFile(filepath.Join(model, "config.json"), []byte(`{"model":"on this machine"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run(ctx, "test -e "+shellQuote(model)); err == nil {
		t.Fatalf("the host has %s already; the test needs a path only this machine has", model)
	}
	docker := modelgarden.DockerCLI{}
	name := "cb-rc-model-" + time.Now().Format("150405")
	if err := docker.Run(ctx, modelgarden.RunSpec{Name: name, Image: image, ModelDir: model, Port: 8080,
		Labels: map[string]string{"cloudburrow.instance": "standin-mounts"},
		Args:   []string{"httpd", "-f", "-p", "8080", "-h", "/model"}}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = docker.Remove(context.Background(), name) }()
	mounts, err := s.docker(ctx, "inspect", "--format", "{{range .Mounts}}{{.Type}} {{.Name}} {{.Destination}} {{.RW}}{{end}}", name)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(mounts, "volume cb-mount-standin-mounts-") || !strings.HasSuffix(mounts, " /model false") {
		t.Fatalf("the model container's mount is %q, not a read-only volume", mounts)
	}
	volumes = append(volumes, strings.Fields(mounts)[1])
	port, err := docker.HostPort(ctx, name, 8080)
	if err != nil {
		t.Fatal(err)
	}
	var body string
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		res, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/config.json")
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if body = string(b); strings.Contains(body, "on this machine") {
			break
		}
	}
	if !strings.Contains(body, "on this machine") {
		t.Fatalf("the model container did not serve the model here: %q", body)
	}

	// A job's writable directory: what the container writes comes back.
	job := t.TempDir()
	if err := os.WriteFile(filepath.Join(job, "in"), []byte("job input"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := hostmount.Scratch(ctx, job, "/cb", hostmount.ReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Release()
	args := append([]string{"run", "--rm", "--network", "none", "--user", "185"}, m.Args()...)
	args = append(args, image, "sh", "-c", "mkdir /cb/out && cat /cb/in > /cb/out/params.json")
	if out, err := s.docker(ctx, args...); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if err := m.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(job, "out", "params.json")); err != nil || string(b) != "job input" {
		t.Fatalf("the job's output came back as %q (%v)", b, err)
	}
}
