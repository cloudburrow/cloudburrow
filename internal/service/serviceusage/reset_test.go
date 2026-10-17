package serviceusage

import (
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestResetProjectForgetsOneProject (#1537): a project's disabled APIs and
// the operations that changed them go; another project's stay; "" is every
// project's.
func TestResetProjectForgetsOneProject(t *testing.T) {
	srv := NewServer("", []API{{Name: "run.googleapis.com", Title: "Cloud Run Admin API"}}, nil, store.NewMemory(), anyProject{})
	for _, p := range []string{"reset-one", "reset-two"} {
		if err := srv.SetState(p, "run.googleapis.com", false); err != nil {
			t.Fatal(err)
		}
	}
	state := func(p string) string {
		t.Helper()
		st, err := srv.load(p)
		if err != nil {
			t.Fatal(err)
		}
		return srv.state(st, "run.googleapis.com")
	}
	if err := srv.ResetProject("reset-one"); err != nil {
		t.Fatal(err)
	}
	if got := state("reset-one"); got != "ENABLED" {
		t.Errorf("reset project's API = %s, want ENABLED", got)
	}
	if got := state("reset-two"); got != "DISABLED" {
		t.Errorf("another project's API = %s, want DISABLED", got)
	}
	if n := len(srv.ProjectOperations("reset-one")); n != 0 {
		t.Errorf("reset project kept %d operations", n)
	}
	if n := len(srv.ProjectOperations("reset-two")); n != 1 {
		t.Errorf("another project kept %d operations, want 1", n)
	}
	if err := srv.ResetProject(""); err != nil {
		t.Fatal(err)
	}
	if got := state("reset-two"); got != "ENABLED" || len(srv.Operations()) != 0 {
		t.Errorf("after a full reset: %s, %d operations", got, len(srv.Operations()))
	}
}
