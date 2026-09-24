//go:build compat

package compat

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestReadyHooksRanAndCreatedABucket.
//
// The compat instance is started with --hooks-dir test/compat/testdata/hooks
// (#285): two ready.d scripts, the second creating a bucket from the
// STORAGE_EMULATOR_HOST it was given. Both are reported with exit status 0
// by `status --format json`, and the bucket exists.
func TestReadyHooksRanAndCreatedABucket(t *testing.T) {
	h := New(t)
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	out, err := exec.Command(cli, append([]string{"status", "--format", "json"}, strings.Fields(os.Getenv(EnvCLIArgs))...)...).Output()
	var st struct {
		Hooks map[string][]struct {
			Name     string `json:"name"`
			ExitCode int    `json:"exit_code"`
			TimedOut bool   `json:"timed_out"`
			Skipped  string `json:"skipped"`
		} `json:"hooks"`
	}
	if jerr := json.Unmarshal(out, &st); jerr != nil {
		t.Fatalf("status --format json (%v): %v\n%s", err, jerr, out)
	}
	ready := st.Hooks["ready.d"]
	if len(ready) != 2 || ready[0].Name != "10-first.sh" || ready[1].Name != "20-bucket.sh" {
		t.Fatalf("ready hooks reported as %+v, want 10-first.sh then 20-bucket.sh", ready)
	}
	for _, r := range ready {
		if r.ExitCode != 0 || r.TimedOut || r.Skipped != "" {
			t.Errorf("%s reported %+v, want exit status 0", r.Name, r)
		}
	}
	if _, err := storageClient(t, h).Bucket("cloudburrow-hook-bucket").Attrs(h.Context()); err != nil {
		t.Errorf("the bucket the ready hook created is not there: %v", err)
	}
}
