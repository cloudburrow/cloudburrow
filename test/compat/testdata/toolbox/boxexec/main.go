// Command boxexec is what gcloud, gsutil, bq, docker-credential-gcloud,
// terraform and tofu are on the compat harness's PATH while the tool box
// runs (test/compat/toolbox_test.go, #2174): it runs the tool its name
// names inside the box, by `docker exec`, with its own arguments, standard
// streams, working directory and environment, and exits with the tool's
// status. It is built for the host by the test, and installed under each
// tool's name beside box.json, which names the box.
//
// It never runs a tool on the host: when the box is gone, or the working
// directory is not one the box mounts, it fails.
//
// Standard library only.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// config is box.json, written by the harness when it starts the box.
type config struct {
	Docker    string            `json:"docker"`
	Container string            `json:"container"`
	User      string            `json:"user"`
	Mounts    []string          `json:"mounts"`
	Tools     map[string]string `json:"tools"` // name -> path in the box
	Path      string            `json:"path"`  // PATH in the box
	Env       []string          `json:"env"`   // forced, after the caller's
}

// dropped are variables of the caller's environment the box never gets:
// host paths and session state that mean nothing there (PATH and HOME are
// the box's own), and those that would point a tool past the box's
// configuration.
var dropped = map[string]bool{
	"PATH": true, "HOME": true, "PWD": true, "OLDPWD": true, "SHLVL": true, "_": true, "USER": true, "LOGNAME": true,
	"SHELL": true, "SSH_AUTH_SOCK": true, "TF_CLI_CONFIG_FILE": true, "TF_PLUGIN_CACHE_DIR": true,
	"TOFU_CLI_CONFIG_FILE": true, "PLUGIN_UNIX_SOCKET_DIR": true, "CLOUDSDK_PYTHON": true, "BOX_TOKEN": true,
	"DOCKER_HOST": true, "DOCKER_CONTEXT": true, "DOCKER_CONFIG": true, "DOCKER_CERT_PATH": true, "DOCKER_TLS_VERIFY": true,
}

func main() {
	os.Exit(run())
}

func run() int {
	name := filepath.Base(os.Args[0])
	self, err := os.Executable()
	if err != nil {
		return fail(name, err)
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(self), "box.json"))
	if err != nil {
		return fail(name, fmt.Errorf("no tool box (%v): run it through the compat harness's toolBox", err))
	}
	var c config
	if err := json.Unmarshal(raw, &c); err != nil {
		return fail(name, err)
	}
	tool, ok := c.Tools[name]
	if !ok {
		return fail(name, errors.New("the tool box has no such tool"))
	}
	wd, err := os.Getwd()
	if err != nil {
		return fail(name, err)
	}
	if !under(wd, c.Mounts) {
		return fail(name, fmt.Errorf("the working directory %s is not one the tool box mounts (%s)", wd, strings.Join(c.Mounts, ", ")))
	}

	args := []string{"exec", "-i", "--user", c.User, "--workdir", wd}
	env := os.Environ()
	var names []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if k == "" || dropped[k] || strings.HasPrefix(k, "__CF") {
			continue
		}
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		args = append(args, "--env", k) // the value comes from docker's environment, below
	}
	args = append(args, "--env", "PATH="+c.Path)
	for _, kv := range c.Env {
		args = append(args, "--env", kv)
	}
	args = append(args, c.Container, tool)
	args = append(args, os.Args[1:]...)

	cmd := exec.Command(c.Docker, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = env
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		return fail(name, err)
	}
	return 0
}

func under(dir string, mounts []string) bool {
	for _, m := range mounts {
		if dir == m || strings.HasPrefix(dir, strings.TrimSuffix(m, "/")+"/") {
			return true
		}
	}
	return false
}

func fail(name string, err error) int {
	fmt.Fprintf(os.Stderr, "%s (compat tool box): %v\n", name, err)
	return 125
}
