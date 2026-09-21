package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
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
