package modelgarden

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/hostmount"
)

// DockerCLI runs engine containers with the docker CLI, in the Docker the
// instance's kind cluster runs in, beside its nodes.
type DockerCLI struct {
	// Network, when set, is joined too (the kind network), so the cluster
	// can reach the engine by container name.
	Network string
}

func (DockerCLI) out(ctx context.Context, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("docker %s: %s", args[0], msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// Run starts a detached container, its port published on 127.0.0.1 only.
func (d DockerCLI) Run(ctx context.Context, s RunSpec) error {
	args := []string{"run", "-d", "--name", s.Name}
	if s.Platform != "" {
		args = append(args, "--platform", s.Platform)
	}
	if s.ShmSize != "" {
		args = append(args, "--shm-size", s.ShmSize)
	}
	if d.Network != "" {
		args = append(args, "--network", d.Network)
	}
	for _, k := range sortedKeys(s.Labels) {
		args = append(args, "--label", k+"="+s.Labels[k])
	}
	for _, k := range sortedKeys(s.Env) {
		args = append(args, "-e", k+"="+s.Env[k])
	}
	// The model directory is this machine's; on a remote compute host it is
	// carried into a volume there (internal/hostmount).
	model, err := hostmount.Bind(ctx, s.ModelDir, "/model", hostmount.ReadOnly)
	if err != nil {
		return err
	}
	args = append(args, model.Args()...)
	args = append(args, "-p", "127.0.0.1::"+strconv.Itoa(s.Port), s.Image)
	args = append(args, s.Args...)
	_, err = d.out(ctx, args...)
	return err
}

// HostPort is the host port a container port is published on.
func (d DockerCLI) HostPort(ctx context.Context, name string, port int) (int, error) {
	out, err := d.out(ctx, "port", name, strconv.Itoa(port)+"/tcp")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(out, "\n") {
		if _, p, err := net.SplitHostPort(strings.TrimSpace(line)); err == nil {
			if n, err := strconv.Atoi(p); err == nil {
				return n, nil
			}
		}
	}
	return 0, fmt.Errorf("container %s publishes no port %d", name, port)
}

// Running reports whether the container is running.
func (d DockerCLI) Running(ctx context.Context, name string) bool {
	out, err := d.out(ctx, "inspect", "-f", "{{.State.Running}}", name)
	return err == nil && out == "true"
}

// Start starts a stopped container.
func (d DockerCLI) Start(ctx context.Context, name string) error {
	_, err := d.out(ctx, "start", name)
	return err
}

// Stop stops a container.
func (d DockerCLI) Stop(ctx context.Context, name string) error {
	_, err := d.out(ctx, "stop", "-t", "10", name)
	return err
}

// Remove removes a container and its anonymous volumes; one that does not
// exist is already removed.
func (d DockerCLI) Remove(ctx context.Context, name string) error {
	_, err := d.out(ctx, "rm", "-f", "-v", name)
	if err != nil && strings.Contains(err.Error(), "No such container") {
		return nil
	}
	return err
}

// Logs returns the container's last lines.
func (DockerCLI) Logs(ctx context.Context, name string, lines int) string {
	cmd := exec.CommandContext(ctx, "docker", "logs", "--tail", strconv.Itoa(lines), name)
	b, _ := cmd.CombinedOutput()
	s := strings.TrimSpace(string(b))
	if len(s) > 2000 {
		s = s[len(s)-2000:]
	}
	return s
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
