package compat

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
)

// The instance a CLI- or gcloud-driving test reaches must be the one the
// harness's endpoints name (#927). Those tests run `cloudburrow` with
// CLOUDBURROW_TEST_CLI_ARGS, and a mangled or incomplete value (an
// unquoted `compat-env.sh --plain` line sourced by a shell, a missing
// --state-dir) resolves the default instance instead: gcloud then created
// resources on a developer's long-running instance on the default ports
// while the gRPC checks went to the harness's. So before any test runs,
// TestMain asks the CLI, with those flags, which endpoints it resolves, and
// fails the whole run when any differs from the harness's (checkTarget);
// and a gcloud session keeps only the endpoint overrides that match the
// harness's (gcloudRestrictConfig), with nothing inherited from the caller's
// gcloud environment (gcloudTargetEnv).

// targetEndpoint is one harness variable the CLI's own report must agree
// with, and how to read the CLI's value for it.
type targetEndpoint struct {
	// harness is the variable, without the CLOUDBURROW_TEST_ prefix.
	harness string
	// reported reads the CLI's value from `env --format json` and
	// `status --format json`; empty when the CLI reports none.
	reported func(env map[string]string, st targetStatus) string
}

// targetStatus is what `cloudburrow status --format json` reports that the
// check reads.
type targetStatus struct {
	Instance   string `json:"instance"`
	ControlURL string `json:"control_url"`
	ConsoleURL string `json:"console_url"`
}

func envKey(k string) func(map[string]string, targetStatus) string {
	return func(env map[string]string, _ targetStatus) string { return env[k] }
}

func hostPort(h, p string) func(map[string]string, targetStatus) string {
	return func(env map[string]string, _ targetStatus) string {
		if env[h] == "" || env[p] == "" {
			return ""
		}
		return net.JoinHostPort(env[h], env[p])
	}
}

// targetEndpoints are the harness variables naming an address of the
// instance, with where `env` and `status` report the same address. They
// follow scripts/compat-env.sh's sources. RUN_* are left out: a caller may
// point them at in-cluster addresses the CLI does not report.
var targetEndpoints = []targetEndpoint{
	{"CONTROL", func(_ map[string]string, st targetStatus) string { return st.ControlURL }},
	{"CONSOLE", func(_ map[string]string, st targetStatus) string { return st.ConsoleURL }},
	{"METADATA", envKey("GCE_METADATA_HOST")},
	{"STORAGE", envKey("STORAGE_EMULATOR_HOST")},
	{"PUBSUB", envKey("PUBSUB_EMULATOR_HOST")},
	{"TASKS", envKey("CLOUDBURROW_TASKS_ENDPOINT")},
	{"SECRETS", envKey("CLOUDBURROW_SECRETMANAGER_ENDPOINT")},
	{"RUN", envKey("CLOUDBURROW_RUN_ENDPOINT")},
	{"SCHEDULER", envKey("CLOUDBURROW_SCHEDULER_ENDPOINT")},
	{"KMS", envKey("CLOUDBURROW_KMS_ENDPOINT")},
	{"LOGGING", envKey("CLOUDBURROW_LOGGING_ENDPOINT")},
	{"RESOURCEMANAGER", envKey("CLOUDBURROW_RESOURCEMANAGER_ENDPOINT")},
	{"SPANNER", envKey("SPANNER_EMULATOR_HOST")},
	{"DATASTORE", envKey("DATASTORE_EMULATOR_HOST")},
	{"FIRESTORE", envKey("FIRESTORE_EMULATOR_HOST")},
	{"BIGTABLE", envKey("BIGTABLE_EMULATOR_HOST")},
	{"BIGQUERY", envKey("CLOUDBURROW_BIGQUERY_ENDPOINT")},
	{"BIGQUERY_STORAGE", envKey("CLOUDBURROW_BIGQUERY_STORAGE_ENDPOINT")},
	{"MEMORYSTORE", hostPort("REDIS_HOST", "REDIS_PORT")},
	{"MYSQL", hostPort("MYSQL_HOST", "MYSQL_PORT")},
	{"CLOUDSQL", hostPort("PGHOST", "PGPORT")},
}

// normalizeEndpoint reduces an address to host:port, so that
// "http://127.0.0.1:9000/storage/v1/" and "localhost:9000" compare equal.
// Loopback and unspecified hosts are one host: the port is what tells two
// local instances apart.
func normalizeEndpoint(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.Index(s, "/"); i >= 0 {
		s = s[:i]
	}
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return s
	}
	switch host {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0", "::", "":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// targetMismatches compares the harness's endpoints (by variable, without
// the prefix) with the CLI's report for the same flags. It returns how many
// it compared and one line per disagreement. A variable the harness sets
// and the CLI does not report is a disagreement too: that instance does not
// serve what the harness was told it does.
func targetMismatches(harness, env map[string]string, st targetStatus) (compared int, problems []string) {
	for _, e := range targetEndpoints {
		want := strings.TrimSpace(harness[e.harness])
		if want == "" {
			continue
		}
		compared++
		got := e.reported(env, st)
		switch {
		case got == "":
			problems = append(problems, fmt.Sprintf("CLOUDBURROW_TEST_%s=%s, but the CLI's instance reports no such endpoint", e.harness, want))
		case normalizeEndpoint(got) != normalizeEndpoint(want):
			problems = append(problems, fmt.Sprintf("CLOUDBURROW_TEST_%s=%s, but the CLI's instance has it at %s", e.harness, want, got))
		}
	}
	// The ADC fixture lives in the instance directory, so it names the
	// instance whatever its ports.
	if want := strings.TrimSpace(harness["CREDENTIALS"]); want != "" {
		compared++
		if got := env["GOOGLE_APPLICATION_CREDENTIALS"]; filepath.Clean(got) != filepath.Clean(want) {
			problems = append(problems, fmt.Sprintf("CLOUDBURROW_TEST_CREDENTIALS=%s, but the CLI's instance has %s", want, got))
		}
	}
	return compared, problems
}

// checkTarget is the whole check: the CLI's `env --format json` and
// `status --format json` output for CLOUDBURROW_TEST_CLI_ARGS against the
// harness's variables. It fails when anything disagrees, and when nothing
// could be compared, since then nothing ties the CLI to the harness.
func checkTarget(harness map[string]string, cliArgs []string, envJSON, statusJSON []byte) error {
	var env map[string]string
	if err := json.Unmarshal(envJSON, &env); err != nil {
		return fmt.Errorf("`cloudburrow env %s --format json` printed no JSON object (%v): %s", strings.Join(cliArgs, " "), err, envJSON)
	}
	var st targetStatus
	if err := json.Unmarshal(statusJSON, &st); err != nil {
		return fmt.Errorf("`cloudburrow status %s --format json` printed no JSON object (%v): %s", strings.Join(cliArgs, " "), err, statusJSON)
	}
	compared, problems := targetMismatches(harness, env, st)
	var b strings.Builder
	fmt.Fprintf(&b, "CLOUDBURROW_TEST_CLI_ARGS=%q names instance %q", strings.Join(cliArgs, " "), st.Instance)
	switch {
	case len(problems) > 0:
		fmt.Fprintf(&b, ", which is not the instance the harness's endpoints name:\n  %s\n", strings.Join(problems, "\n  "))
	case compared == 0:
		b.WriteString(", but no CLOUDBURROW_TEST_ endpoint is set to check it against (set CLOUDBURROW_TEST_CONTROL)\n")
	default:
		return nil
	}
	b.WriteString("the CLI- and gcloud-driving tests would reach another instance; nothing ran. " +
		"Take CLOUDBURROW_TEST_CLI_ARGS and the endpoints from one `eval \"$(scripts/compat-env.sh <the instance's flags>)\"` (test/compat/README.md)")
	return errors.New(b.String())
}

// gcloudOverrides are the gcloud api_endpoint_overrides properties
// `cloudburrow gcloud-setup` writes, with the harness variable naming the
// same endpoint.
var gcloudOverrides = []struct{ property, harness string }{
	{"storage", "STORAGE"},
	{"pubsub", "PUBSUB"},
	{"cloudkms", "KMS"},
	{"secretmanager", "SECRETS"},
	{"cloudtasks", "TASKS"},
	{"cloudscheduler", "SCHEDULER"},
	{"logging", "LOGGING"},
	{"cloudresourcemanager", "RESOURCEMANAGER"},
}

// gcloudRestrictConfig checks a configuration `cloudburrow gcloud-setup`
// wrote against the harness and keeps only the endpoint overrides the
// harness vouches for. An override naming another endpoint than the
// harness's for its service is a problem, and the test must not run; one
// for a service the harness has no endpoint for is dropped, so a command
// for that service fails at the egress guard rather than reaching an
// address nothing checked. Everything outside [api_endpoint_overrides] is
// kept as written.
func gcloudRestrictConfig(conf string, harness map[string]string) (string, []string) {
	want := map[string]string{}
	for _, o := range gcloudOverrides {
		want[o.property] = strings.TrimSpace(harness[o.harness])
	}
	var out strings.Builder
	var problems []string
	in := false
	sc := bufio.NewScanner(strings.NewReader(conf))
	for sc.Scan() {
		line := sc.Text()
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "[") {
			in = t == "[api_endpoint_overrides]"
		} else if k, v, ok := strings.Cut(t, "="); in && ok {
			k, v = strings.TrimSpace(k), strings.TrimSpace(v)
			h, known := want[k]
			switch {
			case !known || h == "":
				continue
			case normalizeEndpoint(h) != normalizeEndpoint(v):
				problems = append(problems, fmt.Sprintf("gcloud-setup wrote %s = %s, but the harness's endpoint for it is %s", k, v, h))
				continue
			}
		}
		out.WriteString(line + "\n")
	}
	return out.String(), problems
}

// gcloudTargetEnv is the environment a gcloud-driving test runs gcloud and
// `cloudburrow gcloud-setup` in: base without any variable that could point
// gcloud at another instance or project, such as the endpoint overrides and
// project an `eval "$(cloudburrow env)"` for another instance exported, which
// gcloud prefers to any configuration: every CLOUDSDK_*, GOOGLE_*, GCE_* and
// BOTO_* variable and every *_EMULATOR_HOST, as bq's and `env`'s sessions
// already drop. The test adds its own CLOUDSDK_CONFIG and
// CLOUDSDK_ACTIVE_CONFIG_NAME.
func gcloudTargetEnv(base []string) []string {
	var env []string
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "CLOUDSDK_") || strings.HasPrefix(k, "GOOGLE_") || strings.HasPrefix(k, "GCE_") ||
			strings.HasPrefix(k, "BOTO_") || strings.HasSuffix(k, "_EMULATOR_HOST") {
			continue
		}
		env = append(env, kv)
	}
	return env
}
