//go:build compat

package compat

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain checks, before any test runs, that CLOUDBURROW_TEST_CLI_ARGS
// names the instance the harness's endpoints do (#927), and fails the whole
// run if not: a CLI- or gcloud-driving test would otherwise create
// resources on whatever instance those flags resolve, such as a developer's
// own on the default ports.
func TestMain(m *testing.M) {
	flag.Parse()
	if f := flag.Lookup("test.list"); f == nil || f.Value.String() == "" {
		if err := verifyCLITarget(); err != nil {
			fmt.Fprintf(os.Stderr, "compat: %v\n", err)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

// cliArgs are the flags naming the harness's instance, for every test that
// runs the CLI. TestMain has checked them against the harness's endpoints.
func cliArgs() []string { return strings.Fields(os.Getenv(EnvCLIArgs)) }

// harnessVars are the harness's variables the target check and the gcloud
// sessions read, by name without the CLOUDBURROW_TEST_ prefix.
func harnessVars() map[string]string {
	out := map[string]string{"CREDENTIALS": os.Getenv("CLOUDBURROW_TEST_CREDENTIALS")}
	for _, e := range targetEndpoints {
		out[e.harness] = os.Getenv("CLOUDBURROW_TEST_" + e.harness)
	}
	return out
}

// verifyCLITarget runs `cloudburrow env` and `status` with
// CLOUDBURROW_TEST_CLI_ARGS and compares what they report with the
// harness's endpoints (checkTarget). Without CLOUDBURROW_TEST_CLI no test
// runs the CLI or gcloud-setup, so there is nothing to check.
func verifyCLITarget() error {
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		return nil
	}
	args := cliArgs()
	envJSON, err := exec.Command(cli, append([]string{"env", "--format", "json"}, args...)...).Output()
	if err != nil {
		return fmt.Errorf("`%s env --format json %s`: %v%s", cli, strings.Join(args, " "), err, stderrOf(err))
	}
	// status exits 3 when the instance is not running and 4 when it is not
	// ready, and prints its report either way.
	statusJSON, err := exec.Command(cli, append([]string{"status", "--format", "json"}, args...)...).Output()
	var ee *exec.ExitError
	if err != nil && !(errors.As(err, &ee) && (ee.ExitCode() == 3 || ee.ExitCode() == 4)) {
		return fmt.Errorf("`%s status --format json %s`: %v%s", cli, strings.Join(args, " "), err, stderrOf(err))
	}
	return checkTarget(harnessVars(), args, envJSON, statusJSON)
}

func stderrOf(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return "\n" + strings.TrimSpace(string(ee.Stderr))
	}
	return ""
}

// gcloudSetup runs `cloudburrow gcloud-setup` for the harness's instance
// into the gcloud directory gdir, then restricts the configuration it wrote
// to the endpoint overrides the harness's endpoints vouch for
// (gcloudRestrictConfig), failing the test on one that names another
// endpoint. It returns the configuration's name.
func gcloudSetup(t *testing.T, cli, gdir string) string {
	t.Helper()
	cmd := exec.Command(cli, append([]string{"gcloud-setup"}, cliArgs()...)...)
	cmd.Env = append(gcloudTargetEnv(os.Environ()), "CLOUDSDK_CONFIG="+gdir)
	b, err := cmd.Output()
	if err != nil {
		t.Fatalf("cloudburrow gcloud-setup: %v%s", err, stderrOf(err))
	}
	name, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "export CLOUDSDK_ACTIVE_CONFIG_NAME=")
	if !ok {
		t.Fatalf("gcloud-setup printed %q", b)
	}
	restrictGcloudConfig(t, gdir, name)
	return name
}

// restrictGcloudConfig applies gcloudRestrictConfig to the configuration
// name in gdir.
func restrictGcloudConfig(t *testing.T, gdir, name string) {
	t.Helper()
	path := filepath.Join(gdir, "configurations", "config_"+name)
	conf, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	kept, problems := gcloudRestrictConfig(string(conf), harnessVars())
	if len(problems) > 0 {
		t.Fatalf("gcloud-setup's configuration names another instance than the harness's; gcloud was not run:\n  %s",
			strings.Join(problems, "\n  "))
	}
	if err := os.WriteFile(path, []byte(kept), 0o644); err != nil {
		t.Fatal(err)
	}
}
