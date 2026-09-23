package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
)

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantErr    error
		wantErrStr string // substring of err.Error(), when wantErr is nil
		wantStdout string // substring
		wantStderr string // substring
	}{
		{
			name:       "no arguments prints usage to stderr and fails",
			args:       nil,
			wantErr:    errUsage,
			wantStderr: "Usage:",
		},
		{
			name:       "help prints usage to stdout and succeeds",
			args:       []string{"help"},
			wantStdout: "Usage:",
		},
		{
			name:       "--help is accepted",
			args:       []string{"--help"},
			wantStdout: "Usage:",
		},
		{
			name:       "-h is accepted",
			args:       []string{"-h"},
			wantStdout: "Usage:",
		},
		{
			name:       "version prints a full line",
			args:       []string{"version"},
			wantStdout: "cloudburrow ",
		},
		{
			name:       "version --short prints only the version",
			args:       []string{"version", "--short"},
			wantStdout: "dev",
		},
		{
			name:       "unknown command fails and names the command",
			args:       []string{"nope"},
			wantErr:    errUsage,
			wantStderr: `unknown command "nope"`,
		},
		{
			// `up` without --help starts a real server and blocks until
			// signalled, so its behavior is covered by up_test.go instead.
			name:       "up --help prints flag documentation",
			args:       []string{"up", "--help"},
			wantStdout: "-port-control",
		},
		{
			name:       "status --help prints flag documentation",
			args:       []string{"status", "--help"},
			wantStdout: "-node-image",
		},
		{
			name:       "help distinguishes stop, reset and delete",
			args:       []string{"help"},
			wantStdout: "none implies another",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := run(tt.args, &stdout, &stderr)

			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("run() error = %v, want %v", err, tt.wantErr)
				}
			case tt.wantErrStr != "":
				if err == nil || !strings.Contains(err.Error(), tt.wantErrStr) {
					t.Errorf("run() error = %v, want substring %q", err, tt.wantErrStr)
				}
			default:
				if err != nil {
					t.Errorf("run() unexpected error = %v", err)
				}
			}

			if tt.wantStdout != "" && !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Errorf("stdout = %q, want substring %q", stdout.String(), tt.wantStdout)
			}
			if tt.wantStderr != "" && !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want substring %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}

// The help text must not imply that services work. Issue #1 makes honest
// capability reporting a project-wide rule, so it is enforced by a test rather
// than left to review.
func TestUsageDoesNotOverstateSupport(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run([]string{"help"}, &stdout, &stderr); err != nil {
		t.Fatalf("run(help) = %v", err)
	}
	if !strings.Contains(stdout.String(), "Planned") {
		t.Error("help text should state that all service operations are Planned")
	}
}

// doctor must be discoverable, or a developer hitting the failure it
// diagnoses has no way to learn it exists.
func TestDoctorIsDocumentedAndHasHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run([]string{"help"}, &stdout, &stderr); err != nil {
		t.Fatalf("run(help) = %v", err)
	}
	if !strings.Contains(stdout.String(), "doctor") {
		t.Error("the command list omits doctor")
	}

	stdout.Reset()
	if err := run([]string{"doctor", "--help"}, &stdout, &stderr); err != nil {
		t.Fatalf("run(doctor --help) = %v", err)
	}
	if !strings.Contains(stdout.String(), "cloudburrow doctor") {
		t.Errorf("doctor --help printed no usage:\n%s", stdout.String())
	}
}

// env is the command that keeps a local loop local, so every variable a
// Google client reads must be there. A missing one sends the client to
// Google with the developer's real credentials.
func TestEnvExportsEveryClientVariable(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	args := []string{"env", "--name", "envtest", "--state-dir", dir}
	if err := run(args, &stdout, &stderr); err != nil {
		t.Fatalf("run(env) = %v\n%s", err, stderr.String())
	}

	out := stdout.String()
	for _, want := range []string{
		// Read by the client libraries.
		"STORAGE_EMULATOR_HOST",
		"PUBSUB_EMULATOR_HOST",
		"GOOGLE_APPLICATION_CREDENTIALS",
		"GOOGLE_CLOUD_PROJECT",
		"GCE_METADATA_HOST",
		// Read by gcloud, which ignores the two above.
		"CLOUDSDK_API_ENDPOINT_OVERRIDES_STORAGE",
		"CLOUDSDK_API_ENDPOINT_OVERRIDES_PUBSUB",
		"CLOUDSDK_CORE_PROJECT",
	} {
		if !strings.Contains(out, "export "+want+"=") {
			t.Errorf("env output does not export %s:\n%s", want, out)
		}
	}

	// Nothing may point at Google, or the whole point is lost.
	if strings.Contains(out, "googleapis.com") || strings.Contains(out, "https://accounts.google") {
		t.Errorf("env output points at Google:\n%s", out)
	}
	// The output is evaluated by a shell, so it must say plainly that it
	// authenticates nothing rather than leave that to the docs.
	if !strings.Contains(out, "authenticates") {
		t.Errorf("env output does not say it authenticates nothing:\n%s", out)
	}
}

func TestEnvWritesAUsableCredentialsFixture(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if err := run([]string{"env", "--name", "envtest", "--state-dir", dir, "--format", "plain"},
		&stdout, &stderr); err != nil {
		t.Fatalf("run(env) = %v\n%s", err, stderr.String())
	}

	var adcPath string
	for _, line := range strings.Split(stdout.String(), "\n") {
		if name, value, ok := strings.Cut(line, "="); ok && name == "GOOGLE_APPLICATION_CREDENTIALS" {
			adcPath = value
		}
	}
	if adcPath == "" {
		t.Fatalf("env did not report a credentials path:\n%s", stdout.String())
	}
	body, err := os.ReadFile(adcPath)
	if err != nil {
		t.Fatalf("the reported credentials file does not exist: %v", err)
	}
	var adc map[string]any
	if err := json.Unmarshal(body, &adc); err != nil {
		t.Fatalf("the fixture is not valid JSON: %v", err)
	}
	if adc["type"] != "service_account" {
		t.Errorf("type = %v, want service_account", adc["type"])
	}
}

// --project overrides the instance name, and the other formats must carry the
// same values so a script and a shell agree.
func TestEnvFormatsAgreeAndHonourProject(t *testing.T) {
	dir := t.TempDir()
	for _, format := range []string{"shell", "plain", "json"} {
		var stdout, stderr bytes.Buffer
		if err := run([]string{"env", "--format", format, "--project", "chosen",
			"--name", "envtest", "--state-dir", dir}, &stdout, &stderr); err != nil {
			t.Fatalf("run(env --format %s) = %v\n%s", format, err, stderr.String())
		}
		if !strings.Contains(stdout.String(), "chosen") {
			t.Errorf("--format %s ignored --project:\n%s", format, stdout.String())
		}
	}

	var stdout, stderr bytes.Buffer
	if err := run([]string{"env", "--format", "nonsense", "--state-dir", dir}, &stdout, &stderr); err == nil {
		t.Error("an unknown format was accepted")
	}
}

// TestEnvAndUpNameTheSameProject.
//
// An instance named "demo" used to export project "demo" — the raw name — which
// Cloud Run and Cloud Tasks then refused as malformed (#255). `env` and `up` now
// take the project from the same function, so a shell configured by `env` talks
// to the project the instance actually serves.
func TestEnvAndUpNameTheSameProject(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if err := run([]string{"env", "--name", "demo", "--state-dir", dir, "--format", "plain"},
		&stdout, &stderr); err != nil {
		t.Fatalf("run(env) = %v\n%s", err, stderr.String())
	}
	cfg, err := config.Load(config.Options{
		Args: []string{"--name", "demo", "--state-dir", dir}, Output: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := cfg.DefaultProject()
	if want == "demo" {
		t.Fatal(`"demo" is not a valid project ID, so it must not be the default project`)
	}
	for _, v := range []string{"GOOGLE_CLOUD_PROJECT", "CLOUDSDK_CORE_PROJECT"} {
		if !strings.Contains(stdout.String(), v+"="+want) {
			t.Errorf("env exports %s other than %q, the project up serves:\n%s", v, want, stdout.String())
		}
	}

	// An invalid explicit project is refused before anything is written, rather
	// than exported and left to fail at the first API call.
	stdout.Reset()
	stderr.Reset()
	if err := run([]string{"env", "--name", "demo", "--project", "Not_Valid", "--state-dir", dir},
		&stdout, &stderr); err == nil {
		t.Errorf("env accepted an invalid --project:\n%s", stdout.String())
	}
}
