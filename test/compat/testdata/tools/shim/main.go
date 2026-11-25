// Command shim stands in, on the compat tests' PATH, for gcloud, gsutil, bq,
// docker-credential-gcloud, terraform and tofu (test/compat/
// toolscontainer_test.go, #2174). It runs the tool its name names in the
// tools container, on an internal Docker network with no route out, through
// `docker exec`, in the same working directory, with the caller's
// environment, as the caller's uid. The tool's exit status is its own.
//
// It fails closed: with no shim.json beside it (the test has not started
// the container) it runs nothing, and it refuses a working directory, an
// argument or a variable naming a host path the container does not mount at
// the same path, or any path under the developer's own gcloud
// configuration, rather than run the tool on the host.
//
// Standard library only: the test builds it for the host.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// config is shim.json, which the test writes when the container is up.
type config struct {
	Container string   `json:"container"`
	User      string   `json:"user"`
	Mounts    []string `json:"mounts"`
	Denied    []string `json:"denied"`
}

// dropped are variables of the host's that would mislead a tool in the
// container: its own PATH, home, temporary and plugin directories and
// Terraform configuration come from the image.
var dropped = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true, "TMPDIR": true,
	"PWD": true, "OLDPWD": true, "SHLVL": true, "_": true, "SSH_AUTH_SOCK": true,
	"PLUGIN_UNIX_SOCKET_DIR": true, "TF_CLI_CONFIG_FILE": true, "TOFU_CLI_CONFIG_FILE": true,
	"TF_PLUGIN_CACHE_DIR": true, "CLOUDSDK_PYTHON": true, "CLOUDSDK_ROOT_DIR": true,
}

func main() {
	os.Exit(run())
}

func run() int {
	name := filepath.Base(os.Args[0])
	fail := func(format string, args ...any) int {
		fmt.Fprintf(os.Stderr, "compat tools shim (%s): "+format+"\n", append([]any{name}, args...)...)
		return 127
	}
	self, err := os.Executable()
	if err != nil {
		return fail("%v", err)
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(self), "shim.json"))
	if errors.Is(err, os.ErrNotExist) {
		return fail("the tools container is not running; a test must ask for the tool (toolBinary) first. " +
			"The tool is never run on the host")
	}
	if err != nil {
		return fail("%v", err)
	}
	var cfg config
	if err := json.Unmarshal(raw, &cfg); err != nil || cfg.Container == "" {
		return fail("bad shim.json: %v", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fail("%v", err)
	}
	if p := os.Getenv("PWD"); p != "" && sameDir(p, cwd) {
		cwd = p
	}
	if msg := cfg.check(cwd, true); msg != "" {
		return fail("the working directory %s", msg)
	}
	for _, a := range os.Args[1:] {
		if msg := cfg.check(a, false); msg != "" {
			return fail("the argument %q: %s", a, msg)
		}
	}
	args := []string{"exec", "-i", "-u", cfg.User, "-w", cwd,
		"-e", "HOME=/home/tools", "-e", "USER=tools", "-e", "TMPDIR=/home/tools", "-e", "PLUGIN_UNIX_SOCKET_DIR=/home/tools"}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if dropped[k] || strings.HasPrefix(k, "DOCKER_") || strings.HasPrefix(k, "CLOUDBURROW_TEST_") {
			continue
		}
		if msg := cfg.check(v, false); msg != "" {
			if toolVariable(k) {
				return fail("the variable %s: %s", k, msg)
			}
			continue // the runner's own (GITHUB_ENV, ...): no tool reads it
		}
		args = append(args, "-e", kv)
	}
	args = append(append(args, cfg.Container, name), os.Args[1:]...)
	cmd := exec.Command("docker", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = os.Environ()
	err = cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		return fail("docker exec: %v", err)
	}
	return 0
}

// toolVariable reports whether a tool reads k, so a host path in it that the
// container cannot see must stop the run rather than be dropped.
func toolVariable(k string) bool {
	for _, p := range []string{"CLOUDSDK_", "GOOGLE_", "GCE_", "BOTO", "TF_", "TOFU_", "KUBECONFIG", "SSL_CERT_",
		"REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE", "BIGTABLE_", "CLOUDBURROW_"} {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

func sameDir(a, b string) bool {
	x, err1 := os.Stat(a)
	y, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(x, y)
}

// check returns why the host path in s (a path, flag=path or a
// colon-separated list) cannot be handed to the container, or "". A path
// that does not exist on the host is the container's own (/usr/bin/...) or
// one the tool is to create; dir, the working directory, must be mounted.
func (c *config) check(s string, dir bool) string {
	var cands []string
	if _, v, ok := strings.Cut(s, "="); ok && !dir {
		cands = append(cands, v)
	}
	cands = append(cands, s)
	var out []string
	for _, cand := range cands {
		out = append(out, strings.Split(cand, ":")...)
	}
	for _, p := range out {
		if !strings.HasPrefix(p, "/") || len(p) < 2 {
			continue
		}
		p = filepath.Clean(p)
		for _, d := range c.Denied {
			if within(p, d) {
				return "names " + p + ", under the developer's own gcloud configuration, which the tools never read"
			}
		}
		if _, err := os.Lstat(p); err != nil && !dir {
			continue
		}
		if within(p, "/dev") || within(p, "/proc") {
			continue
		}
		mounted := false
		for _, m := range c.Mounts {
			if within(p, m) {
				mounted = true
				break
			}
		}
		if !mounted {
			return "names " + p + ", which the tools container does not mount (" + strings.Join(c.Mounts, ", ") + ")"
		}
	}
	return ""
}

func within(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
}
